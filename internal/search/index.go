package search

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/drakejin/memory-mcp/internal/errs"
	"github.com/drakejin/memory-mcp/internal/hotstore"
)

// alreadyExists is the OpenSearch error type returned when two callers create
// the same index concurrently — success from our side.
const alreadyExists = "resource_already_exists_exception"

// EnsureIndex implements Client. It creates the nori-mapped index when absent
// and repairs an index that exists without the mapping; an already-correct
// index (including a concurrent-create race) is success.
func (c *client) EnsureIndex(ctx context.Context) error {
	c.schemaMu.Lock()
	defer c.schemaMu.Unlock()
	return c.ensureIndexLocked(ctx, opEnsureIndex)
}

// ensureSchema converges the mapping before every write. Without it the first
// _bulk auto-creates the index with a dynamic mapping (see client.schemaMu).
//
// The schemaReady latch records only what THIS process last observed, and the
// derived stores are disposable containers that may be replaced under a running
// server (§1). Trusting the latch alone therefore reopens exactly the hole it
// exists to close: after a container swap the latch still says "converged", the
// next _bulk auto-creates a nori-less index, and Korean recall is silently
// destroyed with no error anywhere. So a latched client still confirms the
// index is really there — one HEAD per write batch — and only the expensive
// settings read stays latched.
func (c *client) ensureSchema(ctx context.Context, op string) error {
	c.schemaMu.Lock()
	defer c.schemaMu.Unlock()
	if !c.schemaReady {
		return c.ensureIndexLocked(ctx, op)
	}
	exists, err := c.indexExists(ctx, op)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	// The index vanished under us: re-create it with the nori mapping rather
	// than letting the pending write auto-create a dynamic one.
	c.schemaReady = false
	return c.createIndexLocked(ctx, op)
}

// ensureIndexLocked is the body of EnsureIndex; callers hold schemaMu. On
// success it latches schemaReady so later writes skip the settings round trip.
func (c *client) ensureIndexLocked(ctx context.Context, op string) error {
	exists, err := c.indexExists(ctx, op)
	if err != nil {
		return err
	}
	if exists {
		mapped, err := c.hasKoreanAnalyzer(ctx, op)
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
		c.log.WarnContext(ctx, "episodic index lacks the nori mapping; dropping and recreating",
			"index", c.index, "analyzer", koreanAnalyzer)
		if err := c.dropIndex(ctx, op); err != nil {
			return err
		}
	}
	return c.createIndexLocked(ctx, op)
}

// indexExists reports whether the index is present on the live cluster. It is
// the cheap probe (one HEAD) that tells a replaced container apart from a
// converged one; callers hold schemaMu.
func (c *client) indexExists(ctx context.Context, op string) (bool, error) {
	status, body, err := c.do(ctx, op, http.MethodHead, rawRequest{path: c.indexPath("")})
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, c.unexpected(op, "check index", status, body)
	}
}

// createIndexLocked PUTs the nori mapping and latches schemaReady. A concurrent
// creator winning the race is success, not a failure; callers hold schemaMu.
func (c *client) createIndexLocked(ctx context.Context, op string) error {
	status, body, err := c.do(ctx, op, http.MethodPut, rawRequest{path: c.indexPath(""), body: []byte(indexBody)})
	if err != nil {
		return err
	}
	if status == http.StatusOK ||
		(status == http.StatusBadRequest && bytes.Contains(body, []byte(alreadyExists))) {
		c.schemaReady = true
		return nil
	}
	return c.unexpected(op, "create index", status, body)
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
func (c *client) hasKoreanAnalyzer(ctx context.Context, op string) (bool, error) {
	status, body, err := c.do(ctx, op, http.MethodGet, rawRequest{path: c.indexPath(settingsSuffix)})
	if err != nil {
		return false, err
	}
	if status == http.StatusNotFound {
		return false, nil
	}
	if status != http.StatusOK {
		return false, c.unexpected(op, "read settings", status, body)
	}
	var parsed settingsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false, malformed(op, "decode settings", err)
	}
	for _, entry := range parsed {
		if _, ok := entry.Settings.Index.Analysis.Analyzer[koreanAnalyzer]; ok {
			return true, nil
		}
	}
	return false, nil
}

// Drop implements Client. A missing index is already-dropped, hence success.
// Dropping invalidates the cached mapping state so the next write re-creates
// the index through EnsureIndex instead of letting _bulk auto-create it.
func (c *client) Drop(ctx context.Context) error {
	c.schemaMu.Lock()
	defer c.schemaMu.Unlock()
	if err := c.dropIndex(ctx, opDrop); err != nil {
		return err
	}
	c.schemaReady = false
	return nil
}

// DeleteProject implements Client. Partial rehydration replays one project by
// delete-then-bulk, which mirrors the drop-then-bulk of the full path: an
// episode that aged to cold (§3.1) must stop being searchable, and IndexRecords
// alone can only add or update. A missing index means nothing is indexed, hence
// nothing to delete. conflicts=proceed keeps a document another writer touched
// mid-scroll from failing the whole pass — the next rehydration converges it.
//
// Unlike Drop this leaves the mapping (and schemaReady) alone: the index itself
// survives, only this project's documents go.
func (c *client) DeleteProject(ctx context.Context, key hotstore.ProjectKey) error {
	filters := projectFilters(key)
	if len(filters) == 0 {
		// A zero-value key yields no filters, which would match — and delete —
		// every project. Refuse rather than silently wipe the index.
		return errs.Invalid(opDeleteProject, entityIndex, "project key must name workspace, team and project")
	}
	body, err := json.Marshal(map[string]any{
		"query": map[string]any{"bool": map[string]any{"filter": filters}},
	})
	if err != nil {
		return malformed(opDeleteProject, "marshal delete query", err)
	}
	status, respBody, err := c.do(ctx, opDeleteProject, http.MethodPost, rawRequest{
		path:  c.indexPath(deleteByQuerySuffix),
		query: url.Values{"refresh": {"true"}, "conflicts": {"proceed"}},
		body:  body,
	})
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return nil
	}
	if status != http.StatusOK {
		return c.unexpected(opDeleteProject, "delete by query", status, respBody)
	}
	return nil
}

// dropIndex is the unlocked delete used by both Drop and the mapping repair
// inside ensureIndexLocked (which already holds schemaMu).
func (c *client) dropIndex(ctx context.Context, op string) error {
	status, body, err := c.do(ctx, op, http.MethodDelete, rawRequest{path: c.indexPath("")})
	if err != nil {
		return err
	}
	if status == http.StatusOK || status == http.StatusNotFound {
		return nil
	}
	return c.unexpected(op, "drop index", status, body)
}
