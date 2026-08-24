package search

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/drakejin/memory-mcp/internal/hotstore"
)

const (
	// excerptMaxRunes bounds the fallback excerpt when no highlight is
	// available (empty query / match_all).
	excerptMaxRunes = 200
	// excerptEllipsis marks a truncated fallback excerpt; excerptJoiner joins
	// several highlighted fragments into one excerpt.
	excerptEllipsis = "…"
	excerptJoiner   = " … "
	// highlightField is the only field we highlight — the record body.
	highlightField = "text"
	// highlightFragmentSize / highlightFragments bound the returned excerpt so
	// a hit never carries the full body (§7).
	highlightFragmentSize = 150
	highlightFragments    = 2
)

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
		must = map[string]any{"match": map[string]any{highlightField: map[string]any{"query": q.Text}}}
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
		// Newest first on score ties (Client contract).
		"sort": []any{
			map[string]any{"_score": map[string]any{"order": "desc"}},
			map[string]any{"occurred_at": map[string]any{"order": "desc"}},
			map[string]any{"id": map[string]any{"order": "desc"}},
		},
		"highlight": map[string]any{
			"fields": map[string]any{
				highlightField: map[string]any{
					"fragment_size":       highlightFragmentSize,
					"number_of_fragments": highlightFragments,
				},
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

// Search implements Client. Per the recall principle (§7) each Hit carries an
// excerpt and metadata only: Record.Text is cleared, Excerpt holds highlighted
// fragments (or a truncated head when nothing was highlighted).
func (c *client) Search(ctx context.Context, key hotstore.ProjectKey, q Query) ([]Hit, error) {
	body, err := json.Marshal(buildSearchBody(key, q))
	if err != nil {
		return nil, malformed(opSearch, "marshal query", err)
	}
	status, respBody, err := c.do(ctx, opSearch, http.MethodPost, rawRequest{path: c.indexPath(searchSuffix), body: body})
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		// The index is absent. Answering "0 hits" here would be a false
		// success: the caller cannot tell "nothing matched" from "the derived
		// store is gone", and a container is allowed to disappear at any time
		// (§1). Reporting unavailable is the honest answer — the handler turns
		// it into 503 and the stat gate rebuilds the index (§5).
		return nil, c.indexAbsent(opSearch)
	}
	if status != http.StatusOK {
		return nil, c.unexpected(opSearch, "query", status, respBody)
	}
	var parsed searchResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, malformed(opSearch, "decode search response", err)
	}

	hits := make([]Hit, 0, len(parsed.Hits.Hits))
	for _, h := range parsed.Hits.Hits {
		rec := h.Source.Record
		excerpt := strings.Join(h.Highlight[highlightField], excerptJoiner)
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

// DocCount implements Client.
func (c *client) DocCount(ctx context.Context, key hotstore.ProjectKey) (int, error) {
	query := map[string]any{"match_all": map[string]any{}}
	if filters := projectFilters(key); filters != nil {
		query = map[string]any{"bool": map[string]any{"filter": filters}}
	}
	body, err := json.Marshal(map[string]any{"query": query})
	if err != nil {
		return 0, malformed(opDocCount, "marshal count query", err)
	}
	status, respBody, err := c.do(ctx, opDocCount, http.MethodPost, rawRequest{path: c.indexPath(countSuffix), body: body})
	if err != nil {
		return 0, err
	}
	if status == http.StatusNotFound {
		// No index means zero docs — a legitimate drift signal, not an error.
		return 0, nil
	}
	if status != http.StatusOK {
		return 0, c.unexpected(opDocCount, "count", status, respBody)
	}
	var parsed struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return 0, malformed(opDocCount, "decode count response", err)
	}
	return parsed.Count, nil
}

// truncateRunes returns at most limit runes of s, appending an ellipsis when
// anything was cut.
func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + excerptEllipsis
}
