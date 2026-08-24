package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/hotstore"
)

const (
	// DefaultSearchSize caps hits when Query.Size is unset.
	DefaultSearchSize = 20
	// bulkBatchSize bounds one _bulk request during rehydration.
	bulkBatchSize = 500
	// excerptMaxRunes bounds the fallback excerpt when no highlight is
	// available (empty query / match_all).
	excerptMaxRunes = 200
)

// esDoc is the indexed document: the record plus project-scope keywords.
type esDoc struct {
	episodic.Record
	Workspace string `json:"workspace"`
	Team      string `json:"team"`
	Project   string `json:"project"`
}

// docID returns the composite _id so identical episode ULIDs in different
// projects can never collide, and re-indexing the same record is an upsert.
func docID(key hotstore.ProjectKey, id string) string {
	return key.String() + "#" + id
}

// rawRequest adapts a plain HTTP call to the opensearch-go Request interface.
type rawRequest struct {
	path        string
	query       url.Values
	body        []byte
	contentType string
}

// GetRequest implements opensearch.Request.
func (r rawRequest) GetRequest(method string) (*http.Request, error) {
	target := r.path
	if len(r.query) > 0 {
		target += "?" + r.query.Encode()
	}
	var body io.Reader
	if r.body != nil {
		body = bytes.NewReader(r.body)
	}
	req, err := http.NewRequest(method, target, body)
	if err != nil {
		return nil, fmt.Errorf("search: build request %s %s: %w", method, r.path, err)
	}
	if r.body != nil {
		ct := r.contentType
		if ct == "" {
			ct = "application/json"
		}
		req.Header.Set("Content-Type", ct)
	}
	return req, nil
}

// do executes one request and returns (status, body). Transport failures and
// 5xx responses wrap ErrUnavailable so callers can degrade (§5); other error
// statuses are returned as (status, body, nil) for per-endpoint handling.
func (c *Client) do(ctx context.Context, method string, req rawRequest) (int, []byte, error) {
	resp, err := c.os.Do(ctx, method, req, nil)
	if resp == nil {
		return 0, nil, fmt.Errorf("%w: %s %s: %w", ErrUnavailable, method, req.path, err)
	}
	var body []byte
	var readErr error
	if resp.Body != nil {
		body, readErr = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	if err != nil {
		return resp.StatusCode, body, fmt.Errorf("%w: %s %s: %w", ErrUnavailable, method, req.path, err)
	}
	if readErr != nil {
		return resp.StatusCode, body, fmt.Errorf("%w: %s %s: read body: %w", ErrUnavailable, method, req.path, readErr)
	}
	if resp.StatusCode >= http.StatusInternalServerError {
		return resp.StatusCode, body, fmt.Errorf("%w: %s %s: status %d: %s", ErrUnavailable, method, req.path, resp.StatusCode, body)
	}
	return resp.StatusCode, body, nil
}

// Ping implements Index.
func (c *Client) Ping(ctx context.Context) error {
	status, body, err := c.do(ctx, http.MethodGet, rawRequest{path: "/"})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("%w: ping status %d: %s", ErrUnavailable, status, body)
	}
	return nil
}

// EnsureIndex implements Index. It creates the nori-mapped index when absent
// and repairs an index that exists without the mapping; an already-correct
// index (including a concurrent-create race) is success.
func (c *Client) EnsureIndex(ctx context.Context) error {
	c.schemaMu.Lock()
	defer c.schemaMu.Unlock()
	return c.ensureIndexLocked(ctx)
}

// ensureSchema converges the mapping exactly once per process before the first
// write. Without it the first _bulk auto-creates the index with a dynamic
// mapping (see Client.schemaMu), so this runs on every write path rather than
// only on the rehydration paths that call EnsureIndex explicitly.
func (c *Client) ensureSchema(ctx context.Context) error {
	c.schemaMu.Lock()
	defer c.schemaMu.Unlock()
	if c.schemaReady {
		return nil
	}
	return c.ensureIndexLocked(ctx)
}

// ensureIndexLocked is the body of EnsureIndex; callers hold schemaMu. On
// success it latches schemaReady so later writes skip the round trip.
func (c *Client) ensureIndexLocked(ctx context.Context) error {
	status, body, err := c.do(ctx, http.MethodHead, rawRequest{path: "/" + c.index})
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK:
		mapped, err := c.hasKoreanAnalyzer(ctx)
		if err != nil {
			return err
		}
		if mapped {
			c.schemaReady = true
			return nil
		}
		// The index exists but was auto-created by a write that bypassed this
		// method: dynamic mapping, no nori. A mapping cannot be changed in
		// place, and the index is a disposable derived store (§1), so the
		// honest repair is to drop it and let rehydration refill it (§5).
		slog.Warn("search: episodic index lacks the nori mapping; dropping and recreating",
			"index", c.index, "analyzer", koreanAnalyzer)
		if err := c.dropIndex(ctx); err != nil {
			return err
		}
	case http.StatusNotFound:
		// fall through to create
	default:
		return fmt.Errorf("search: check index %s: status %d: %s", c.index, status, body)
	}

	status, body, err = c.do(ctx, http.MethodPut, rawRequest{path: "/" + c.index, body: []byte(indexBody)})
	if err != nil {
		return err
	}
	if status == http.StatusOK ||
		(status == http.StatusBadRequest && bytes.Contains(body, []byte("resource_already_exists_exception"))) {
		c.schemaReady = true
		return nil
	}
	return fmt.Errorf("search: create index %s: status %d: %s", c.index, status, body)
}

// settingsResponse is the subset of GET /{index}/_settings we consume. The
// analysis block is normalized by OpenSearch under settings.index.analysis
// regardless of how it was submitted.
type settingsResponse map[string]struct {
	Settings struct {
		Index struct {
			Analysis struct {
				Analyzer map[string]json.RawMessage `json:"analyzer"`
			} `json:"analysis"`
		} `json:"index"`
	} `json:"settings"`
}

// hasKoreanAnalyzer reports whether the live index actually carries the nori
// analyzer. A dynamically auto-created index has no analysis block at all, so
// its absence is the signal that the mapping was never applied.
func (c *Client) hasKoreanAnalyzer(ctx context.Context) (bool, error) {
	status, body, err := c.do(ctx, http.MethodGet, rawRequest{path: "/" + c.index + "/_settings"})
	if err != nil {
		return false, err
	}
	if status == http.StatusNotFound {
		return false, nil
	}
	if status != http.StatusOK {
		return false, fmt.Errorf("search: read settings %s: status %d: %s", c.index, status, body)
	}
	var parsed settingsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false, fmt.Errorf("search: decode settings %s: %w", c.index, err)
	}
	for _, entry := range parsed {
		if _, ok := entry.Settings.Index.Analysis.Analyzer[koreanAnalyzer]; ok {
			return true, nil
		}
	}
	return false, nil
}

// bulkItemResult is one entry of a _bulk response.
type bulkItemResult struct {
	Status int             `json:"status"`
	Error  json.RawMessage `json:"error"`
}

// bulkResponse is the _bulk response envelope.
type bulkResponse struct {
	Errors bool                        `json:"errors"`
	Items  []map[string]bulkItemResult `json:"items"`
}

// bulk sends one ndjson payload with refresh=true so upserts are immediately
// searchable (realtime contract, §2). notFoundOK tolerates per-item 404s
// (idempotent deletes).
func (c *Client) bulk(ctx context.Context, payload []byte, notFoundOK bool) error {
	req := rawRequest{
		path:        "/_bulk",
		query:       url.Values{"refresh": {"true"}},
		body:        payload,
		contentType: "application/x-ndjson",
	}
	status, body, err := c.do(ctx, http.MethodPost, req)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("search: bulk: status %d: %s", status, body)
	}
	var parsed bulkResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("search: bulk: decode response: %w", err)
	}
	if !parsed.Errors {
		return nil
	}
	for _, item := range parsed.Items {
		for action, result := range item {
			if result.Error == nil {
				continue
			}
			if notFoundOK && result.Status == http.StatusNotFound {
				continue
			}
			return fmt.Errorf("search: bulk %s failed: status %d: %s", action, result.Status, result.Error)
		}
	}
	return nil
}

// IndexRecords implements Index.
func (c *Client) IndexRecords(ctx context.Context, key hotstore.ProjectKey, recs []episodic.Record) error {
	if len(recs) == 0 {
		return nil
	}
	// Must precede the first _bulk: otherwise OpenSearch auto-creates the
	// index with a dynamic mapping and Korean recall breaks silently (§2).
	if err := c.ensureSchema(ctx); err != nil {
		return err
	}
	for start := 0; start < len(recs); start += bulkBatchSize {
		batch := recs[start:min(start+bulkBatchSize, len(recs))]
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		for _, rec := range batch {
			action := map[string]map[string]string{
				"index": {"_index": c.index, "_id": docID(key, rec.ID)},
			}
			doc := esDoc{Record: rec, Workspace: key.Workspace, Team: key.Team, Project: key.Project}
			if err := enc.Encode(action); err != nil {
				return fmt.Errorf("search: encode bulk action: %w", err)
			}
			if err := enc.Encode(doc); err != nil {
				return fmt.Errorf("search: encode record %s: %w", rec.ID, err)
			}
		}
		if err := c.bulk(ctx, buf.Bytes(), false); err != nil {
			return err
		}
	}
	return nil
}

// DeleteRecords implements Index. Missing ids are tolerated so aging retries
// stay idempotent (§4).
func (c *Client) DeleteRecords(ctx context.Context, key hotstore.ProjectKey, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	// A bulk delete against a missing index also auto-creates it, so the same
	// guard applies to the aging path.
	if err := c.ensureSchema(ctx); err != nil {
		return err
	}
	for start := 0; start < len(ids); start += bulkBatchSize {
		batch := ids[start:min(start+bulkBatchSize, len(ids))]
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		for _, id := range batch {
			action := map[string]map[string]string{
				"delete": {"_index": c.index, "_id": docID(key, id)},
			}
			if err := enc.Encode(action); err != nil {
				return fmt.Errorf("search: encode bulk delete: %w", err)
			}
		}
		if err := c.bulk(ctx, buf.Bytes(), true); err != nil {
			return err
		}
	}
	return nil
}

// projectFilters returns term filters scoping a query to one project. A
// zero-value key yields no filters (all projects).
func projectFilters(key hotstore.ProjectKey) []map[string]any {
	if key == (hotstore.ProjectKey{}) {
		return nil
	}
	return []map[string]any{
		{"term": map[string]any{"workspace": key.Workspace}},
		{"term": map[string]any{"team": key.Team}},
		{"term": map[string]any{"project": key.Project}},
	}
}

// buildSearchBody translates a Query into the OpenSearch request body.
func buildSearchBody(key hotstore.ProjectKey, q Query) map[string]any {
	filters := projectFilters(key)
	if !q.From.IsZero() || !q.To.IsZero() {
		bounds := map[string]any{}
		if !q.From.IsZero() {
			bounds["gte"] = q.From.UTC().Format(time.RFC3339Nano)
		}
		if !q.To.IsZero() {
			bounds["lte"] = q.To.UTC().Format(time.RFC3339Nano)
		}
		filters = append(filters, map[string]any{"range": map[string]any{"occurred_at": bounds}})
	}
	if len(q.Kinds) > 0 {
		kinds := make([]string, len(q.Kinds))
		for i, k := range q.Kinds {
			kinds[i] = string(k)
		}
		filters = append(filters, map[string]any{"terms": map[string]any{"kind": kinds}})
	}

	var must any = map[string]any{"match_all": map[string]any{}}
	if strings.TrimSpace(q.Text) != "" {
		must = map[string]any{"match": map[string]any{"text": map[string]any{"query": q.Text}}}
	}

	size := q.Size
	if size <= 0 {
		size = DefaultSearchSize
	}

	boolQuery := map[string]any{"must": []any{must}}
	if len(filters) > 0 {
		boolQuery["filter"] = filters
	}
	return map[string]any{
		"query": map[string]any{"bool": boolQuery},
		"size":  size,
		// Newest first on score ties (Index contract).
		"sort": []any{
			map[string]any{"_score": map[string]any{"order": "desc"}},
			map[string]any{"occurred_at": map[string]any{"order": "desc"}},
			map[string]any{"id": map[string]any{"order": "desc"}},
		},
		"highlight": map[string]any{
			"fields": map[string]any{
				"text": map[string]any{"fragment_size": 150, "number_of_fragments": 2},
			},
		},
	}
}

// searchResponse is the subset of the _search response we consume.
type searchResponse struct {
	Hits struct {
		Hits []struct {
			Score     *float64            `json:"_score"`
			Source    esDoc               `json:"_source"`
			Highlight map[string][]string `json:"highlight"`
		} `json:"hits"`
	} `json:"hits"`
}

// Search implements Index. Per the recall principle (§7) each Hit carries an
// excerpt and metadata only: Record.Text is cleared, Excerpt holds highlighted
// fragments (or a truncated head when nothing was highlighted).
func (c *Client) Search(ctx context.Context, key hotstore.ProjectKey, q Query) ([]Hit, error) {
	body, err := json.Marshal(buildSearchBody(key, q))
	if err != nil {
		return nil, fmt.Errorf("search: marshal query: %w", err)
	}
	status, respBody, err := c.do(ctx, http.MethodPost, rawRequest{path: "/" + c.index + "/_search", body: body})
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		// Index not created yet — nothing indexed, nothing to find.
		return []Hit{}, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("search: query: status %d: %s", status, respBody)
	}
	var parsed searchResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("search: decode response: %w", err)
	}

	hits := make([]Hit, 0, len(parsed.Hits.Hits))
	for _, h := range parsed.Hits.Hits {
		rec := h.Source.Record
		excerpt := strings.Join(h.Highlight["text"], " … ")
		if excerpt == "" {
			excerpt = truncateRunes(rec.Text, excerptMaxRunes)
		}
		rec.Text = "" // excerpt-only responses; never inject the full body (§7)
		var score float64
		if h.Score != nil {
			score = *h.Score
		}
		hits = append(hits, Hit{Record: rec, Score: score, Excerpt: excerpt})
	}
	return hits, nil
}

// DocCount implements Index.
func (c *Client) DocCount(ctx context.Context, key hotstore.ProjectKey) (int, error) {
	query := map[string]any{"match_all": map[string]any{}}
	if filters := projectFilters(key); filters != nil {
		query = map[string]any{"bool": map[string]any{"filter": filters}}
	}
	body, err := json.Marshal(map[string]any{"query": query})
	if err != nil {
		return 0, fmt.Errorf("search: marshal count query: %w", err)
	}
	status, respBody, err := c.do(ctx, http.MethodPost, rawRequest{path: "/" + c.index + "/_count", body: body})
	if err != nil {
		return 0, err
	}
	if status == http.StatusNotFound {
		// No index means zero docs — a legitimate drift signal, not an error.
		return 0, nil
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("search: count: status %d: %s", status, respBody)
	}
	var parsed struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return 0, fmt.Errorf("search: decode count response: %w", err)
	}
	return parsed.Count, nil
}

// Drop implements Index. A missing index is already-dropped, hence success.
// Dropping invalidates the cached mapping state so the next write re-creates
// the index through EnsureIndex instead of letting _bulk auto-create it.
func (c *Client) Drop(ctx context.Context) error {
	c.schemaMu.Lock()
	defer c.schemaMu.Unlock()
	if err := c.dropIndex(ctx); err != nil {
		return err
	}
	c.schemaReady = false
	return nil
}

// dropIndex is the unlocked delete used by both Drop and the mapping repair
// inside ensureIndexLocked (which already holds schemaMu).
func (c *Client) dropIndex(ctx context.Context) error {
	status, body, err := c.do(ctx, http.MethodDelete, rawRequest{path: "/" + c.index})
	if err != nil {
		return err
	}
	if status == http.StatusOK || status == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("search: drop index %s: status %d: %s", c.index, status, body)
}

// truncateRunes returns at most limit runes of s, appending an ellipsis when
// anything was cut.
func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}
