package episodemem

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

const (
	// bulkBatchSize bounds one _bulk request during rehydration.
	bulkBatchSize = 500
	// docIDSep joins the project key and the episode ULID in the composite
	// document _id.
	docIDSep = "#"
	// actionIndex / actionDelete are the _bulk action names.
	actionIndex  = "index"
	actionDelete = "delete"
)

// esDoc is the indexed document: the record plus project-scope keywords.
type esDoc struct {
	episode.Record
	Workspace string `json:"workspace"`
	Team      string `json:"team"`
	Project   string `json:"project"`
}

// docID returns the composite _id so identical episode ULIDs in different
// projects can never collide, and re-indexing the same record is an upsert.
func docID(key projectkey.Key, id string) string {
	return key.String() + docIDSep + id
}

// IndexRecords implements Client.
func (c *client) IndexRecords(ctx context.Context, key projectkey.Key, recs []episode.Record) error {
	if len(recs) == 0 {
		return nil
	}
	// Must precede the first _bulk: otherwise OpenSearch auto-creates the
	// index with a dynamic mapping and Korean recall breaks silently (§2).
	if err := c.ensureSchema(ctx, opIndexRecords); err != nil {
		return err
	}
	for start := 0; start < len(recs); start += bulkBatchSize {
		batch := recs[start:min(start+bulkBatchSize, len(recs))]
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		for _, rec := range batch {
			doc := esDoc{Record: rec, Workspace: key.Workspace, Team: key.Team, Project: key.Project}
			if err := enc.Encode(bulkAction(actionIndex, c.index, docID(key, rec.ID))); err != nil {
				return malformed(opIndexRecords, "encode bulk action", err)
			}
			if err := enc.Encode(doc); err != nil {
				return malformed(opIndexRecords, "encode record "+rec.ID, err)
			}
		}
		if err := c.bulk(ctx, opIndexRecords, buf.Bytes(), false); err != nil {
			return err
		}
	}
	return nil
}

// DeleteRecords implements Client. Missing ids are tolerated so aging retries
// stay idempotent (§4).
func (c *client) DeleteRecords(ctx context.Context, key projectkey.Key, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	// A bulk delete against a missing index also auto-creates it, so the same
	// guard applies to the aging path.
	if err := c.ensureSchema(ctx, opDeleteRecords); err != nil {
		return err
	}
	for start := 0; start < len(ids); start += bulkBatchSize {
		batch := ids[start:min(start+bulkBatchSize, len(ids))]
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		for _, id := range batch {
			if err := enc.Encode(bulkAction(actionDelete, c.index, docID(key, id))); err != nil {
				return malformed(opDeleteRecords, "encode bulk delete", err)
			}
		}
		if err := c.bulk(ctx, opDeleteRecords, buf.Bytes(), true); err != nil {
			return err
		}
	}
	return nil
}

// bulkAction builds one ndjson action line.
func bulkAction(action, index, id string) map[string]map[string]string {
	return map[string]map[string]string{action: {"_index": index, "_id": id}}
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
func (c *client) bulk(ctx context.Context, op string, payload []byte, notFoundOK bool) error {
	req := rawRequest{
		path:        bulkPath,
		query:       url.Values{"refresh": {"true"}},
		body:        payload,
		contentType: contentTypeNDJSON,
	}
	status, body, err := c.do(ctx, op, http.MethodPost, req)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return c.unexpected(op, "bulk", status, body)
	}
	var parsed bulkResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return malformed(op, "decode bulk response", err)
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
			return c.unexpected(op, fmt.Sprintf("bulk %s item", action), result.Status, result.Error)
		}
	}
	return nil
}
