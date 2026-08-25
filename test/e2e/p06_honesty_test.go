//go:build e2e

// P6 — 정직성 (feature-inventory.md §2 P6): the system always reports its own
// limits — drift, degraded writes, truncation, stale unconsolidated memory —
// with EXACT numbers, never "nonzero-ish". Every reported figure here is
// cross-checked against an independent inspector computation (manifest sums,
// raw OpenSearch counts, cypher counts, direct hot-file walks): the status
// endpoint claims a comparison, the inspectors perform the same comparison
// themselves.
package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	cold "github.com/drakejin/memory-mcp/internal/external/thirdparty/cold"
	httpserver "github.com/drakejin/memory-mcp/internal/handler/app/httpserver"
	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/service/knowledge"
	"github.com/drakejin/memory-mcp/internal/service/rehydrate"
	"github.com/drakejin/memory-mcp/internal/x/config"
)

// Frozen honesty vocabulary this scenario pins (rehydrate drift reasons and
// the §5 degraded notes; changing any of these is a status-contract break).
const (
	p06ReasonOSUnreachable  = "opensearch unreachable"
	p06ReasonNeoUnreachable = "neo4j unreachable"
	p06DegradedSearch       = "search unavailable"
	p06DegradedGraph        = "graph unavailable"
)

// ---------- independent inspector computations ----------

// p06OSGlobalCount counts every document in the episodic index (all projects),
// mirroring the zero-key DocCount the drift check runs. A best-effort refresh
// runs first so _count never under-reports segments indexed moments ago; its
// status is ignored because during post-restart recovery the refresh may
// briefly refuse. ok=false while the index is absent or not yet answering.
func p06OSGlobalCount(t *testing.T) (int, bool) {
	t.Helper()
	_, _ = h.osRequest(t, http.MethodPost, "/"+osIndex+"/_refresh", "")
	status, raw := h.osRequest(t, http.MethodPost, "/"+osIndex+"/_count", "")
	if status != http.StatusOK {
		return 0, false
	}
	var parsed struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		failf(t, "opensearch global _count: decode: %v: %.300s", err, raw)
	}
	return parsed.Count, true
}

// p06ManifestEpisodicRecords sums RecordCount over every episodic manifest
// entry — the "manifest records" side of the drift comparison, read straight
// from manifest.json.
func p06ManifestEpisodicRecords(t *testing.T) int {
	t.Helper()
	total := 0
	for fk, fs := range h.hotManifest(t).Files {
		if strings.HasPrefix(fk, string(rehydrate.PlaneEpisodic)+"/") {
			total += fs.RecordCount
		}
	}
	return total
}

// p06ManifestDirtyFiles lists every dirty manifest entry, sorted — the
// independent source for the status DirtyFiles claim.
func p06ManifestDirtyFiles(t *testing.T) []string {
	t.Helper()
	dirty := []string{}
	for fk, fs := range h.hotManifest(t).Files {
		if fs.Dirty {
			dirty = append(dirty, fk)
		}
	}
	slices.Sort(dirty)
	return dirty
}

// p06HotKnowledgeNodesGlobal sums knowledge nodes across every hot knowledge
// file in the main home — the "hot nodes" side of the knowledge drift
// comparison.
func p06HotKnowledgeNodesGlobal(t *testing.T) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(h.home, string(rehydrate.PlaneKnowledge), "*", "*", "*.json"))
	if err != nil {
		failf(t, "glob hot knowledge files: %v", err)
	}
	total := 0
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			failf(t, "read hot knowledge file %s: %v", path, err)
		}
		var g knowledge.Graph
		if err := json.Unmarshal(data, &g); err != nil {
			failf(t, "hot knowledge file %s is not a graph document: %v", path, err)
		}
		total += len(g.Nodes)
	}
	return total
}

// p06NeoNodesGlobal counts every mirrored knowledge node across all projects,
// mirroring the zero-key NodeCount the drift check runs.
func p06NeoNodesGlobal(t *testing.T) int {
	t.Helper()
	return h.cypherCount(t, fmt.Sprintf("MATCH (n:%s) RETURN count(n);", neoNodeLabel))
}

// p06EpisodicTally recomputes the §3.1 unconsolidated bookkeeping exactly the
// way the consolidator does: walk every hot episodic file, count records with
// consolidated=false, and the subset older than the TTL.
func p06EpisodicTally(t *testing.T) (uncons, stale int) {
	t.Helper()
	staleBefore := time.Now().UTC().Add(-time.Duration(config.DefaultEpisodicTTLDays) * 24 * time.Hour)
	matches, err := filepath.Glob(filepath.Join(h.home, string(rehydrate.PlaneEpisodic), "*", "*", "*.json"))
	if err != nil {
		failf(t, "glob hot episodic files: %v", err)
	}
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			failf(t, "read hot episodic file %s: %v", path, err)
		}
		var recs []episode.Record
		if err := json.Unmarshal(data, &recs); err != nil {
			failf(t, "hot episodic file %s is not a record array: %v", path, err)
		}
		for _, rec := range recs {
			if rec.Consolidated {
				continue
			}
			uncons++
			if rec.OccurredAt.Before(staleBefore) {
				stale++
			}
		}
	}
	return uncons, stale
}

// p06AssertCleanStatus pins the fully-converged shape: no drift on either
// plane, nothing dirty, nothing degraded, cold store reachable.
func p06AssertCleanStatus(t *testing.T, st httpserver.StatusReport, step string) {
	t.Helper()
	if st.Drift.Episodic != (rehydrate.Drift{}) || st.Drift.Knowledge != (rehydrate.Drift{}) {
		failf(t, "%s: want no drift, got episodic=%+v knowledge=%+v", step, st.Drift.Episodic, st.Drift.Knowledge)
	}
	if len(st.DirtyFiles) != 0 || len(st.Degraded) != 0 {
		failf(t, "%s: want nothing dirty/degraded, got dirty=%v degraded=%v", step, st.DirtyFiles, st.Degraded)
	}
	if !st.S3.Reachable {
		failf(t, "%s: S3 must be reachable", step)
	}
	pass(t, "%s: status reports a fully converged system", step)
}

// p06Reindex converges the world and requires a verified, failure-free pass.
func p06Reindex(t *testing.T, step string) {
	t.Helper()
	rep := h.reindex(t, true)
	if !rep.Verified || len(rep.Failures) != 0 {
		failf(t, "%s: reindex verify: want verified with no failures, got verified=%v failures=%v", step, rep.Verified, rep.Failures)
	}
}

// ---------- scenarios ----------

// TestP06_Honesty_EpisodicDriftAndDegradedWrite kills OpenSearch mid-flight:
// the write must answer 201 with the exact degraded note, reads must answer
// 503 (never a fabricated empty result), and /status must first report the
// outage as UNAVAILABLE, then — container back, before reindex — as DETECTED
// drift whose reason carries the exact indexed-vs-manifest numbers the
// inspectors compute independently.
func TestP06_Honesty_EpisodicDriftAndDegradedWrite(t *testing.T) {
	h.ensureServer(t)
	key := e2eKey("p06-epi")
	t.Cleanup(func() { h.startContainer(t, osContainer) }) // idempotent safety net

	// Converged baseline: status clean, and the two sides of the drift
	// comparison agree per the inspectors themselves.
	p06Reindex(t, "baseline")
	p06AssertCleanStatus(t, h.status(t), "baseline")
	base := p06ManifestEpisodicRecords(t)
	if got, ok := p06OSGlobalCount(t); !ok || got != base {
		failf(t, "baseline: OS global count %d (ok=%v) != manifest records %d", got, ok, base)
	}

	// Outage: stop the container, then write through it.
	h.stopContainer(t, osContainer)
	h.waitFor(t, "opensearch down", 30*time.Second, func() (bool, string) {
		up, ev := h.opensearchUp()
		return !up, ev
	})
	const token = "p06epidegradedtoken"
	res := h.postEpisode(t, key, httpserver.CreateEpisodeRequest{
		Kind: episode.KindEvent, Actor: episode.ActorAgent,
		Text: "P6 degraded write evidence " + token, Entities: []string{},
	})
	if len(res.Degraded) != 1 || res.Degraded[0] != p06DegradedSearch {
		failf(t, "degraded write: notes = %v, want exactly [%q]", res.Degraded, p06DegradedSearch)
	}
	if _, ok := h.hotEpisodeByID(t, key, res.Record.ID); !ok {
		failf(t, "hot inspector: degraded write %s missing from %s", res.Record.ID, h.hotEpisodePath(key))
	}
	pass(t, "write during outage: 201 + %q, record committed to hot", p06DegradedSearch)

	// Read honesty during the outage: 503, not an empty 200.
	if status, _ := h.searchEpisodes(t, key, token); status != http.StatusServiceUnavailable {
		failf(t, "degraded read: want 503, got HTTP %d", status)
	}

	// Status during the outage: unavailable (not "no drift"), the degraded
	// note, and exactly one dirty file — mine — confirmed by the manifest.
	epFK := rehydrate.ManifestFileKey(rehydrate.PlaneEpisodic, key)
	st := h.status(t)
	if want := (rehydrate.Drift{Unavailable: true, Reason: p06ReasonOSUnreachable}); st.Drift.Episodic != want {
		failf(t, "outage status: drift.episodic = %+v, want %+v", st.Drift.Episodic, want)
	}
	if st.Drift.Knowledge != (rehydrate.Drift{}) {
		failf(t, "outage status: drift.knowledge = %+v, want clean", st.Drift.Knowledge)
	}
	if !slices.Equal(st.Degraded, []string{p06DegradedSearch}) {
		failf(t, "outage status: degraded = %v, want exactly [%q]", st.Degraded, p06DegradedSearch)
	}
	if !slices.Equal(st.DirtyFiles, []string{epFK}) {
		failf(t, "outage status: dirty_files = %v, want exactly [%q]", st.DirtyFiles, epFK)
	}
	if manDirty := p06ManifestDirtyFiles(t); !slices.Equal(manDirty, []string{epFK}) {
		failf(t, "manifest inspector: dirty entries = %v, want exactly [%q]", manDirty, epFK)
	}
	if !st.S3.Reachable {
		failf(t, "outage status: S3 must still be reachable during an OpenSearch outage")
	}
	pass(t, "outage status: unavailable + degraded [%q] + dirty [%q], all inspector-confirmed", p06DegradedSearch, epFK)

	// Recovery: the container returns with its pre-outage docs; the degraded
	// write is the exact gap, and status must state both numbers.
	h.startContainer(t, osContainer)
	want := p06ManifestEpisodicRecords(t)
	h.waitFor(t, "opensearch recovered with the pre-outage docs", 60*time.Second, func() (bool, string) {
		got, ok := p06OSGlobalCount(t)
		return ok && got == want-1, fmt.Sprintf("global count=%d ok=%v, want %d", got, ok, want-1)
	})
	st = h.status(t)
	wantReason := fmt.Sprintf("indexed docs %d != manifest records %d", want-1, want)
	if wantDrift := (rehydrate.Drift{Detected: true, Reason: wantReason}); st.Drift.Episodic != wantDrift {
		failf(t, "post-restart status: drift.episodic = %+v, want %+v", st.Drift.Episodic, wantDrift)
	}
	if len(st.Degraded) != 0 {
		failf(t, "post-restart status: degraded = %v, want none — reachable-but-stale is drift, not degraded", st.Degraded)
	}
	if !slices.Equal(st.DirtyFiles, []string{epFK}) {
		failf(t, "post-restart status: dirty_files = %v, want still [%q]", st.DirtyFiles, epFK)
	}
	pass(t, "post-restart status: detected drift with the exact numbers: %q", wantReason)

	// Convergence: reindex, then the missing record is IN the index (proven by
	// the raw _doc inspector) and status is clean again.
	p06Reindex(t, "recovery")
	if _, found := h.osDoc(t, key, res.Record.ID); !found {
		failf(t, "os inspector: degraded record %s still absent after reindex", res.Record.ID)
	}
	if got, ok := p06OSGlobalCount(t); !ok || got != want {
		failf(t, "os inspector: global count %d (ok=%v) != manifest records %d after reindex", got, ok, want)
	}
	p06AssertCleanStatus(t, h.status(t), "after recovery reindex")
	if manDirty := p06ManifestDirtyFiles(t); len(manDirty) != 0 {
		failf(t, "manifest inspector: dirty entries remain after reindex: %v", manDirty)
	}
}

// TestP06_Honesty_KnowledgeDriftAndDegradedWrite is the Neo4j twin: degraded
// node write with the exact note, 503 knowledge reads, unavailable → detected
// drift with the exact graph-vs-hot node numbers, then convergence.
func TestP06_Honesty_KnowledgeDriftAndDegradedWrite(t *testing.T) {
	h.ensureServer(t)
	key := e2eKey("p06-know")
	t.Cleanup(func() { h.startContainer(t, neo4jContainer) })

	p06Reindex(t, "baseline")
	p06AssertCleanStatus(t, h.status(t), "baseline")
	if hot, neo := p06HotKnowledgeNodesGlobal(t), p06NeoNodesGlobal(t); hot != neo {
		failf(t, "baseline: hot nodes %d != neo nodes %d after reindex", hot, neo)
	}

	h.stopContainer(t, neo4jContainer)
	h.waitFor(t, "neo4j down", 30*time.Second, func() (bool, string) {
		up, ev := h.neo4jUp()
		return !up, ev
	})
	res := h.postNode(t, key, knowledge.CreateNodeInput{
		Kind: knowledge.KindFact, Name: "p06-degraded-node", Body: "written during the neo4j outage",
		Trust: knowledge.TrustAgentInferred,
	})
	if len(res.Degraded) != 1 || res.Degraded[0] != p06DegradedGraph {
		failf(t, "degraded node write: notes = %v, want exactly [%q]", res.Degraded, p06DegradedGraph)
	}
	if hot, ok := h.hotKnowledgeNode(t, key, res.Node.ID); !ok || hot.State != knowledge.StateActive {
		failf(t, "hot inspector: degraded node %s missing/not-active in %s (present=%v)", res.Node.ID, h.hotKnowledgePath(key), ok)
	}
	pass(t, "write during outage: 201 + %q, node committed to hot", p06DegradedGraph)

	// Knowledge reads have no honest partial answer: 503 with the same word.
	status, env := h.api(t).getJSON(t, projPath(key)+"/knowledge/search?q=p06-degraded-node")
	if status != http.StatusServiceUnavailable || env.Error == nil || env.Error.Message != p06DegradedGraph {
		failf(t, "degraded knowledge read: want 503 %q, got HTTP %d error=%q", p06DegradedGraph, status, env.Error)
	}

	knFK := rehydrate.ManifestFileKey(rehydrate.PlaneKnowledge, key)
	st := h.status(t)
	if want := (rehydrate.Drift{Unavailable: true, Reason: p06ReasonNeoUnreachable}); st.Drift.Knowledge != want {
		failf(t, "outage status: drift.knowledge = %+v, want %+v", st.Drift.Knowledge, want)
	}
	if st.Drift.Episodic != (rehydrate.Drift{}) {
		failf(t, "outage status: drift.episodic = %+v, want clean", st.Drift.Episodic)
	}
	if !slices.Equal(st.Degraded, []string{p06DegradedGraph}) {
		failf(t, "outage status: degraded = %v, want exactly [%q]", st.Degraded, p06DegradedGraph)
	}
	if !slices.Equal(st.DirtyFiles, []string{knFK}) {
		failf(t, "outage status: dirty_files = %v, want exactly [%q]", st.DirtyFiles, knFK)
	}
	if manDirty := p06ManifestDirtyFiles(t); !slices.Equal(manDirty, []string{knFK}) {
		failf(t, "manifest inspector: dirty entries = %v, want exactly [%q]", manDirty, knFK)
	}
	pass(t, "outage status: unavailable + degraded [%q] + dirty [%q], all inspector-confirmed", p06DegradedGraph, knFK)

	// Recovery: the mirror is short exactly my one node, and status states
	// both numbers of the comparison.
	h.startContainer(t, neo4jContainer)
	wantNodes := p06HotKnowledgeNodesGlobal(t)
	gotNodes := p06NeoNodesGlobal(t)
	if gotNodes != wantNodes-1 {
		failf(t, "neo inspector: global nodes = %d after restart, want %d (hot %d minus the degraded write)", gotNodes, wantNodes-1, wantNodes)
	}
	st = h.status(t)
	wantReason := fmt.Sprintf("graph nodes %d != hot nodes %d", gotNodes, wantNodes)
	if wantDrift := (rehydrate.Drift{Detected: true, Reason: wantReason}); st.Drift.Knowledge != wantDrift {
		failf(t, "post-restart status: drift.knowledge = %+v, want %+v", st.Drift.Knowledge, wantDrift)
	}
	if len(st.Degraded) != 0 {
		failf(t, "post-restart status: degraded = %v, want none", st.Degraded)
	}
	pass(t, "post-restart status: detected drift with the exact numbers: %q", wantReason)

	p06Reindex(t, "recovery")
	if state, found := h.neoNodeField(t, key, res.Node.ID, "state"); !found || state != string(knowledge.StateActive) {
		failf(t, "neo inspector: degraded node %s state = %q (found=%v) after reindex, want active", res.Node.ID, state, found)
	}
	if hot, neo := p06HotKnowledgeNodesGlobal(t), p06NeoNodesGlobal(t); hot != neo {
		failf(t, "convergence: hot nodes %d != neo nodes %d after reindex", hot, neo)
	}
	p06AssertCleanStatus(t, h.status(t), "after recovery reindex")
}

// TestP06_Honesty_TruncatedIngest feeds a document that chunks past the §6
// cap and demands the truncation be reported with the true total — while the
// stores hold exactly the indexed prefix, byte-for-byte.
func TestP06_Honesty_TruncatedIngest(t *testing.T) {
	h.ensureServer(t)
	key := e2eKey("p06-trunc")

	// Deterministic fixture: pure ASCII, an exact multiple of the chunk size,
	// ten chunks over the cap — so total and indexed are computable a priori.
	overflow := 10
	total := config.MaxDocumentChunks + overflow
	raw := make([]byte, total*config.DocumentChunkBytes)
	pattern := []byte("p06 truncation honesty fixture ")
	for i := range raw {
		raw[i] = pattern[i%len(pattern)]
	}
	sum := sha256.Sum256(raw)
	wantSHA := hex.EncodeToString(sum[:])

	stBefore := h.status(t)
	doc := h.ingestDoc(t, key, "p06-truncation.txt", raw)

	// Response honesty: the exact cut is stated, nothing rounded or hidden.
	if doc.SHA != wantSHA {
		failf(t, "ingest sha = %s, want locally computed %s", doc.SHA, wantSHA)
	}
	if !doc.Extractable || len(doc.ChunkIDs) != config.MaxDocumentChunks {
		failf(t, "ingest: extractable=%v chunks=%d, want extractable with exactly %d chunks", doc.Extractable, len(doc.ChunkIDs), config.MaxDocumentChunks)
	}
	if doc.Truncated == nil || doc.Truncated.Total != total || doc.Truncated.Indexed != config.MaxDocumentChunks {
		failf(t, "ingest truncation report = %+v, want total=%d indexed=%d", doc.Truncated, total, config.MaxDocumentChunks)
	}
	if len(doc.Degraded) != 0 {
		failf(t, "ingest: unexpected degraded notes with every store up: %v", doc.Degraded)
	}
	pass(t, "ingest reported the exact truncation: total=%d indexed=%d", doc.Truncated.Total, doc.Truncated.Indexed)

	// Hot inspector: exactly the capped chunk set, in sequence, and the kept
	// prefix reassembles byte-for-byte — the tail was cut, the head is intact.
	recs := h.hotEpisodes(t, key)
	if len(recs) != config.MaxDocumentChunks {
		failf(t, "hot inspector: %d records, want exactly %d chunks", len(recs), config.MaxDocumentChunks)
	}
	byID := make(map[string]episode.Record, len(recs))
	for _, rec := range recs {
		byID[rec.ID] = rec
	}
	var reassembled bytes.Buffer
	for i, id := range doc.ChunkIDs {
		rec, ok := byID[id]
		if !ok {
			failf(t, "hot inspector: chunk %d (%s) missing", i, id)
		}
		if rec.Kind != episode.KindDocumentChunk || rec.Refs == nil || rec.Refs.DocSHA != doc.SHA || rec.Refs.ChunkSeq != i {
			failf(t, "hot inspector: chunk %d = kind %q refs %+v, want document_chunk seq=%d sha=%s", i, rec.Kind, rec.Refs, i, doc.SHA)
		}
		if rec.Consolidated {
			failf(t, "hot inspector: chunk %d born consolidated — the server may never infer distillation", i)
		}
		reassembled.WriteString(rec.Text)
	}
	if wantPrefix := raw[:config.MaxDocumentChunks*config.DocumentChunkBytes]; !bytes.Equal(reassembled.Bytes(), wantPrefix) {
		failf(t, "hot inspector: reassembled chunks are %d bytes and differ from the first %d source bytes", reassembled.Len(), len(wantPrefix))
	}
	pass(t, "hot inspector: exactly %d chunks in order, reassembling the untruncated prefix byte-for-byte", config.MaxDocumentChunks)

	// Derived stores: the index holds exactly the cap, the graph exactly the
	// one document node; the blob sits in S3 (cold-first).
	h.osRefresh(t)
	if n, ok := h.osProjectCount(t, key); !ok || n != config.MaxDocumentChunks {
		failf(t, "os inspector: project count = %d (index=%v), want exactly %d", n, ok, config.MaxDocumentChunks)
	}
	if n := h.neoNodeCount(t, key); n != 1 {
		failf(t, "neo inspector: node count = %d, want exactly the 1 document node", n)
	}
	if kind, found := h.neoNodeField(t, key, doc.NodeID, "kind"); !found || kind != string(knowledge.KindDocument) {
		failf(t, "neo inspector: document node %s kind = %q (found=%v), want %q", doc.NodeID, kind, found, knowledge.KindDocument)
	}
	if g := h.hotKnowledge(t, key); len(g.Nodes) != 1 || g.Nodes[0].ID != doc.NodeID {
		failf(t, "hot inspector: knowledge doc holds %d node(s), want exactly %s", len(g.Nodes), doc.NodeID)
	}
	if ok, ev := h.s3Exists(cold.BlobKey(s3Username, doc.SHA)); !ok {
		failf(t, "s3 inspector: original blob missing (cold-first violated): %s", ev)
	}

	// The chunks endpoint reports the same exact set, in order.
	status, env := h.api(t).getJSON(t, "/v1/documents/"+doc.SHA+"/chunks")
	if status != http.StatusOK || !env.Success {
		failf(t, "GET chunks: want 200, got HTTP %d error=%q", status, env.Error)
	}
	var chunks []episode.Record
	decodeData(t, env, &chunks)
	if len(chunks) != config.MaxDocumentChunks {
		failf(t, "GET chunks: %d records, want exactly %d — the response must not invent the truncated tail", len(chunks), config.MaxDocumentChunks)
	}
	for i, rec := range chunks {
		if rec.Refs == nil || rec.Refs.ChunkSeq != i {
			failf(t, "GET chunks: position %d carries seq %+v, want %d", i, rec.Refs, i)
		}
	}

	// Status honesty: the cap's worth of new unconsolidated episodes, exactly.
	st := h.status(t)
	if got, wantDelta := st.Unconsolidated-stBefore.Unconsolidated, config.MaxDocumentChunks; got != wantDelta {
		failf(t, "status: unconsolidated grew by %d, want exactly %d", got, wantDelta)
	}
	if st.StaleUnconsolidated != stBefore.StaleUnconsolidated {
		failf(t, "status: stale_unconsolidated changed %d -> %d on a fresh ingest", stBefore.StaleUnconsolidated, st.StaleUnconsolidated)
	}
	uncons, stale := p06EpisodicTally(t)
	if st.Unconsolidated != uncons || st.StaleUnconsolidated != stale {
		failf(t, "status vs hot walk: unconsolidated %d/%d, stale %d/%d — the report must equal the store",
			st.Unconsolidated, uncons, st.StaleUnconsolidated, stale)
	}
	pass(t, "truncated ingest: every count exact across response, four stores and /status")
}

// TestP06_Honesty_StaleUnconsolidated plants unconsolidated records straddling
// the TTL and demands /status carry the exact global counts (cross-checked by
// walking the hot files), then proves a real consolidation pass ages ONLY the
// consolidated record — stale unconsolidated memory survives and stays
// reported, forever.
func TestP06_Honesty_StaleUnconsolidated(t *testing.T) {
	h.ensureServer(t)
	key := e2eKey("p06-stale")
	now := time.Now().UTC().Truncate(time.Second)

	plant := func(daysAgo int, consolidated bool, text string) episode.Record {
		at := now.AddDate(0, 0, -daysAgo)
		rec := episode.Record{
			ID: newULIDAt(t, at), Kind: episode.KindObservation, OccurredAt: at,
			Actor: episode.ActorSystem, Text: text, Entities: []string{}, Consolidated: consolidated,
		}
		h.writeHotEpisodeDirect(t, key, rec)
		return rec
	}

	stBase := h.status(t)
	r45 := plant(45, false, "p06 stale unconsolidated 45d")
	r31 := plant(31, false, "p06 stale unconsolidated 31d")
	r05 := plant(5, false, "p06 young unconsolidated 5d")
	r35c := plant(35, true, "p06 consolidated 35d — aging fodder")
	fresh := h.postEpisode(t, key, httpserver.CreateEpisodeRequest{
		Kind: episode.KindEvent, Actor: episode.ActorAgent,
		Text: "p06 fresh unconsolidated via API", Entities: []string{},
	})
	p06Reindex(t, "after direct plants") // converge derived stores deterministically

	// Exact deltas: 4 new unconsolidated (45d, 31d, 5d, fresh), 2 stale.
	st := h.status(t)
	if got, want := st.Unconsolidated-stBase.Unconsolidated, 4; got != want {
		failf(t, "status: unconsolidated grew by %d, want exactly %d", got, want)
	}
	if got, want := st.StaleUnconsolidated-stBase.StaleUnconsolidated, 2; got != want {
		failf(t, "status: stale_unconsolidated grew by %d, want exactly %d", got, want)
	}
	// Exact globals: the report equals an independent walk of every hot file.
	uncons, stale := p06EpisodicTally(t)
	if st.Unconsolidated != uncons || st.StaleUnconsolidated != stale {
		failf(t, "status vs hot walk: unconsolidated %d/%d, stale %d/%d",
			st.Unconsolidated, uncons, st.StaleUnconsolidated, stale)
	}
	pass(t, "status reports the exact counts: unconsolidated=%d stale=%d, both matching the hot walk", st.Unconsolidated, st.StaleUnconsolidated)

	// A real consolidation pass over this project: the consolidated 35d record
	// ages to cold; every unconsolidated record — however stale — survives.
	crep := h.consolidate(t, httpserver.ConsolidateRequest{Projects: []string{key.String()}})
	if len(crep.Failures) != 0 {
		failf(t, "consolidate: unexpected failures: %v", crep.Failures)
	}
	if crep.MovedEpisodes != 1 {
		failf(t, "consolidate: moved %d episode(s), want exactly the 1 consolidated record", crep.MovedEpisodes)
	}
	archKey := cold.EpisodeArchiveKey(s3Username, key, cold.ArchiveMonth(r35c.OccurredAt))
	if !slices.Equal(crep.ArchiveKeys, []string{archKey}) {
		failf(t, "consolidate: archive_keys = %v, want exactly [%q]", crep.ArchiveKeys, archKey)
	}
	if ok, ev := h.s3Exists(archKey); !ok {
		failf(t, "s3 inspector: archive object missing after aging: %s", ev)
	}
	body := h.s3Cat(t, archKey)
	if !bytes.Contains(body, []byte(r35c.ID)) || bytes.Contains(body, []byte(r45.ID)) {
		failf(t, "s3 inspector: archive must hold %s and never the unconsolidated %s", r35c.ID, r45.ID)
	}

	// Hot inspector: the aged record is gone, all four unconsolidated remain.
	if _, ok := h.hotEpisodeByID(t, key, r35c.ID); ok {
		failf(t, "hot inspector: consolidated record %s still in hot after aging", r35c.ID)
	}
	for _, id := range []string{r45.ID, r31.ID, r05.ID, fresh.Record.ID} {
		if _, ok := h.hotEpisodeByID(t, key, id); !ok {
			failf(t, "hot inspector: unconsolidated record %s was removed by consolidation — §3.1 forbids this", id)
		}
	}

	// Status honesty after the pass: the unconsolidated numbers are unchanged
	// (still inspector-exact), and the cold sync state discloses the archive.
	st2 := h.status(t)
	if st2.Unconsolidated != st.Unconsolidated || st2.StaleUnconsolidated != st.StaleUnconsolidated {
		failf(t, "status after aging: unconsolidated %d->%d stale %d->%d, want unchanged",
			st.Unconsolidated, st2.Unconsolidated, st.StaleUnconsolidated, st2.StaleUnconsolidated)
	}
	uncons2, stale2 := p06EpisodicTally(t)
	if st2.Unconsolidated != uncons2 || st2.StaleUnconsolidated != stale2 {
		failf(t, "status vs hot walk after aging: unconsolidated %d/%d, stale %d/%d",
			st2.Unconsolidated, uncons2, st2.StaleUnconsolidated, stale2)
	}
	if !st2.S3.Reachable || st2.S3.LastArchiveAt.IsZero() || !st2.S3.LastArchiveAt.After(stBase.S3.LastArchiveAt) {
		failf(t, "status after aging: s3 = %+v, want reachable with last_archive_at advanced past %s",
			st2.S3, stBase.S3.LastArchiveAt)
	}
	pass(t, "stale unconsolidated memory survived consolidation and stays reported: unconsolidated=%d stale=%d", st2.Unconsolidated, st2.StaleUnconsolidated)
}
