//go:build e2e

// Seed helpers: plain-HTTP writers against the main instance with full
// response decoding, plus the direct hot-file writer that bypasses the server
// entirely (the P9/P4 time-travel tool — the API lets the server mint the
// ULID and normalize fields, so records with arbitrary pasts, consolidated
// flags or recall stats can only be planted at the store).
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/external/persistence/hotstore"
	httpserver "github.com/drakejin/memory-mcp/internal/handler/app/httpserver"
	"github.com/drakejin/memory-mcp/internal/service/consolidate"
	"github.com/drakejin/memory-mcp/internal/service/document"
	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/service/knowledge"
	"github.com/drakejin/memory-mcp/internal/service/rehydrate"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
	"github.com/drakejin/memory-mcp/internal/x/ulid"
)

// ---------- envelope plumbing ----------

// envelope mirrors the mandatory {success, data, error} response shape.
type envelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   *envelopeError  `json:"error"`
}

// envelopeError is the public projection of apierr.Error.
type envelopeError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// String keeps "error=%q" failure messages readable, nil included.
func (e *envelopeError) String() string {
	if e == nil {
		return ""
	}
	if len(e.Details) == 0 {
		return e.Code + ": " + e.Message
	}
	return fmt.Sprintf("%s: %s %v", e.Code, e.Message, e.Details)
}

// projPath renders the project-scoped route prefix "/v1/{ws}/{team}/{proj}".
func projPath(key projectkey.Key) string {
	return "/v1/" + key.Workspace + "/" + key.Team + "/" + key.Project
}

// do issues one request against this instance and decodes the envelope.
// Failure-asserting scenarios use this (or its wrappers) directly on
// h.api(t) / an extra *server and branch on the returned status + env.Error.
func (s *server) do(t *testing.T, method, path string, body io.Reader, contentType string) (int, envelope) {
	t.Helper()
	req, err := http.NewRequest(method, s.baseURL+path, body)
	if err != nil {
		failf(t, "%s %s: build request: %v", method, path, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		failf(t, "%s %s: transport error (server down?): %v; server log: %s", method, path, err, s.logPath)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		failf(t, "%s %s: read body: %v", method, path, err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		failf(t, "%s %s: HTTP %d is not the {success,data,error} envelope: %.400s", method, path, resp.StatusCode, raw)
	}
	return resp.StatusCode, env
}

func (s *server) getJSON(t *testing.T, path string) (int, envelope) {
	t.Helper()
	return s.do(t, http.MethodGet, path, nil, "")
}

func (s *server) postJSON(t *testing.T, path string, body any) (int, envelope) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		failf(t, "marshal body for %s: %v", path, err)
	}
	return s.do(t, http.MethodPost, path, bytes.NewReader(raw), "application/json")
}

func (s *server) postMultipart(t *testing.T, path, field, filename string, data []byte) (int, envelope) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile(field, filename)
	if err != nil {
		failf(t, "multipart form: %v", err)
	}
	if _, err := fw.Write(data); err != nil {
		failf(t, "multipart write: %v", err)
	}
	if err := w.Close(); err != nil {
		failf(t, "multipart close: %v", err)
	}
	return s.do(t, http.MethodPost, path, &buf, w.FormDataContentType())
}

// getRawBytes fetches a non-envelope endpoint (document original download).
func (s *server) getRawBytes(t *testing.T, path string) (int, []byte) {
	t.Helper()
	resp, err := s.client.Get(s.baseURL + path)
	if err != nil {
		failf(t, "GET %s: transport error: %v; server log: %s", path, err, s.logPath)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		failf(t, "GET %s: read body: %v", path, err)
	}
	return resp.StatusCode, raw
}

// decodeData unmarshals an envelope data payload into out.
func decodeData(t *testing.T, env envelope, out any) {
	t.Helper()
	if err := json.Unmarshal(env.Data, out); err != nil {
		failf(t, "decode envelope data into %T: %v; data: %.400s", out, err, env.Data)
	}
}

// ---------- seed verbs (main instance, happy path, full decode) ----------

// postEpisode records one episode and returns the decoded AppendResult (the
// server-minted Record plus any degraded notes — degraded is data, never a
// failure here; P1 scenarios assert on it).
func (h *harness) postEpisode(t *testing.T, key projectkey.Key, req httpserver.CreateEpisodeRequest) episode.AppendResult {
	t.Helper()
	status, env := h.api(t).postJSON(t, projPath(key)+"/episodes", req)
	if status != http.StatusCreated || !env.Success {
		failf(t, "POST episodes: want 201 success, got HTTP %d error=%q", status, env.Error)
	}
	var res episode.AppendResult
	decodeData(t, env, &res)
	if res.Record.ID == "" {
		failf(t, "POST episodes: no id assigned: %.300s", env.Data)
	}
	return res
}

// postNode creates a knowledge node (optionally superseding) and returns the
// decoded NodeResult.
func (h *harness) postNode(t *testing.T, key projectkey.Key, in knowledge.CreateNodeInput) knowledge.NodeResult {
	t.Helper()
	status, env := h.api(t).postJSON(t, projPath(key)+"/knowledge/nodes", in)
	if status != http.StatusCreated || !env.Success {
		failf(t, "POST knowledge/nodes %q: want 201 success, got HTTP %d error=%q", in.Name, status, env.Error)
	}
	var res knowledge.NodeResult
	decodeData(t, env, &res)
	if res.Node.ID == "" {
		failf(t, "POST knowledge/nodes %q: no id assigned", in.Name)
	}
	return res
}

// postEdge creates a knowledge edge and returns the decoded EdgeResult.
func (h *harness) postEdge(t *testing.T, key projectkey.Key, edge knowledge.Edge) knowledge.EdgeResult {
	t.Helper()
	status, env := h.api(t).postJSON(t, projPath(key)+"/knowledge/edges", edge)
	if status != http.StatusCreated || !env.Success {
		failf(t, "POST knowledge/edges %s-[%s]->%s: want 201 success, got HTTP %d error=%q",
			edge.From, edge.Rel, edge.To, status, env.Error)
	}
	var res knowledge.EdgeResult
	decodeData(t, env, &res)
	return res
}

// ingestDoc uploads one document (multipart field "file") and returns the
// decoded IngestResult.
func (h *harness) ingestDoc(t *testing.T, key projectkey.Key, filename string, data []byte) document.IngestResult {
	t.Helper()
	status, env := h.api(t).postMultipart(t, projPath(key)+"/documents", "file", filename, data)
	if status != http.StatusCreated || !env.Success {
		failf(t, "POST documents %s: want 201 success, got HTTP %d error=%q", filename, status, env.Error)
	}
	var res document.IngestResult
	decodeData(t, env, &res)
	if res.SHA == "" {
		failf(t, "POST documents %s: no sha in result: %.300s", filename, env.Data)
	}
	return res
}

// consolidate runs POST /v1/consolidate and returns the decoded Report.
// Failures inside the report are data (scenarios assert on them); only a
// non-200 fails here.
func (h *harness) consolidate(t *testing.T, req httpserver.ConsolidateRequest) consolidate.Report {
	t.Helper()
	status, env := h.api(t).postJSON(t, "/v1/consolidate", req)
	if status != http.StatusOK || !env.Success {
		failf(t, "POST /v1/consolidate: want 200 success, got HTTP %d error=%q", status, env.Error)
	}
	var rep consolidate.Report
	decodeData(t, env, &rep)
	return rep
}

// reindex runs POST /v1/reindex (full rebuild; verify additionally re-hashes
// everything) and returns the decoded Report.
func (h *harness) reindex(t *testing.T, verify bool) rehydrate.Report {
	t.Helper()
	path := "/v1/reindex"
	if verify {
		path += "?verify=true"
	}
	status, env := h.api(t).postJSON(t, path, struct{}{})
	if status != http.StatusOK || !env.Success {
		failf(t, "POST %s: want 200 success, got HTTP %d error=%q", path, status, env.Error)
	}
	var rep rehydrate.Report
	decodeData(t, env, &rep)
	return rep
}

// status fetches the honesty report GET /v1/status.
func (h *harness) status(t *testing.T) httpserver.StatusReport {
	t.Helper()
	code, env := h.api(t).getJSON(t, "/v1/status")
	if code != http.StatusOK || !env.Success {
		failf(t, "GET /v1/status: want 200 success, got HTTP %d error=%q", code, env.Error)
	}
	var rep httpserver.StatusReport
	decodeData(t, env, &rep)
	return rep
}

// searchEpisodes runs the project-scoped episodic search. Hits decode only on
// 200; other statuses (503 degraded reads) return with nil hits for the
// caller to assert on.
func (h *harness) searchEpisodes(t *testing.T, key projectkey.Key, q string) (int, []episode.Hit) {
	t.Helper()
	qs := url.Values{"q": {q}}.Encode()
	status, env := h.api(t).getJSON(t, projPath(key)+"/episodes/search?"+qs)
	if status != http.StatusOK {
		return status, nil
	}
	var hits []episode.Hit
	decodeData(t, env, &hits)
	return status, hits
}

// getEpisode fetches one episode by id (hot first, S3 archive fallback —
// P11). The handler's data payload is the bare episode.Record; it decodes
// only on 200.
func (h *harness) getEpisode(t *testing.T, key projectkey.Key, id string) (int, episode.Record) {
	t.Helper()
	status, env := h.api(t).getJSON(t, projPath(key)+"/episodes/"+id)
	if status != http.StatusOK {
		return status, episode.Record{}
	}
	var rec episode.Record
	decodeData(t, env, &rec)
	return status, rec
}

// hitIDs projects hit record ids for evidence lines.
func hitIDs(hits []episode.Hit) []string {
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		ids = append(ids, hit.Record.ID)
	}
	return ids
}

// ---------- direct hot-store writes (server bypassed) ----------

// newULIDAt mints a valid ULID whose timestamp half encodes at — so
// direct-written records sort correctly against server-minted ones.
func newULIDAt(t *testing.T, at time.Time) string {
	t.Helper()
	ids, err := ulid.New(ulid.Config{Clock: hotstore.NewSystemClock()})
	if err != nil {
		failf(t, "ulid client: %v", err)
	}
	id, err := ids.GenerateAt(at.UnixMilli())
	if err != nil {
		failf(t, "generate ulid at %s: %v", at, err)
	}
	return id
}

// appendHotEpisodeAt writes one fully caller-controlled record into the hot
// episodic file at home, through the canonical hotstore client — atomic
// write + manifest bookkeeping stay correct by construction, but the SERVER
// is bypassed: id, occurred_at, consolidated and recall fields land exactly
// as given. rec must be valid (rec.Validate) — scenarios that need genuinely
// corrupt bytes write the file with os.WriteFile themselves.
//
// Constraints: do not race concurrent server writes to the same project (two
// processes, no cross-process lock), and reconcile the derived stores
// afterwards — h.reindex(t, ...) is the deterministic way — before asserting
// OpenSearch/Neo4j state.
func appendHotEpisodeAt(t *testing.T, home string, key projectkey.Key, rec episode.Record) {
	t.Helper()
	if err := rec.Validate(); err != nil {
		failf(t, "direct hot episode fixture is invalid (write corrupt bytes with os.WriteFile instead): %v", err)
	}
	store, err := hotstore.New(hotstore.Config{Home: home, Clock: hotstore.NewSystemClock()})
	if err != nil {
		failf(t, "open canonical hot store at %s: %v", home, err)
	}
	if err := store.AppendEpisode(context.Background(), key, rec); err != nil {
		failf(t, "append episode %s directly to hot store: %v", rec.ID, err)
	}
}

// writeHotEpisodeDirect is appendHotEpisodeAt bound to the main home — the
// standard P9/P4 fixture path: plant a past occurred_at (and, when needed,
// consolidated=true), then h.reindex to converge the derived stores.
func (h *harness) writeHotEpisodeDirect(t *testing.T, key projectkey.Key, rec episode.Record) {
	t.Helper()
	appendHotEpisodeAt(t, h.home, key, rec)
}

// markConsolidatedDirect flips consolidated=true on existing hot records
// through the canonical store — for aging fixtures built on server-created
// episodes (the server itself never auto-consolidates; the sanctioned API
// path is naming the episode in a knowledge node's provenance).
func (h *harness) markConsolidatedDirect(t *testing.T, key projectkey.Key, ids ...string) {
	t.Helper()
	store, err := hotstore.New(hotstore.Config{Home: h.home, Clock: hotstore.NewSystemClock()})
	if err != nil {
		failf(t, "open canonical hot store at %s: %v", h.home, err)
	}
	err = store.UpdateEpisodes(context.Background(), key, ids, func(rec episode.Record) episode.Record {
		rec.Consolidated = true
		return rec
	})
	if err != nil {
		failf(t, "mark episodes %v consolidated directly: %v", ids, err)
	}
}
