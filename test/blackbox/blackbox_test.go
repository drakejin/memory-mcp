//go:build blackbox

// The §10 acceptance scenarios (architecture-v2.md). They run in declared
// order against real compose containers, a real server subprocess on
// 127.0.0.1:8420 with its own temp DJ_MEMORY_HOME, and the real bucket under
// s3://vms-memory-mcp/jin/blackbox-test/... (pre- and post-cleaned).
// Run via `make blackbox`. Scenario names and order are scaffold-owned.
package blackbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/cold"
	"github.com/drakejin/memory-mcp/internal/consolidate"
	"github.com/drakejin/memory-mcp/internal/document"
	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/graph"
	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/knowledge"
	"github.com/drakejin/memory-mcp/internal/rehydrate"
	"github.com/drakejin/memory-mcp/internal/server"
)

// §10.1 — fresh compose up → /healthz green, swagger doc served.
func TestScenario01_Startup(t *testing.T) {
	h.composeFresh(t)
	pass(t, "compose down+up completed fresh (no volumes — derived stores start empty)")

	h.waitFor(t, "opensearch reachable", 240*time.Second, h.opensearchUp)
	ev := h.waitFor(t, "neo4j reachable via cypher-shell", 240*time.Second, h.neo4jUp)
	pass(t, "containers up: opensearch 200 on %s; neo4j %s", opensearchURL, ev)

	h.ensurePortFree(t)
	h.startServer(t)
	h.waitFor(t, "server /healthz responding", 30*time.Second, func() (bool, string) {
		if h.serverResponding() {
			return true, "healthz responding"
		}
		return false, "no response on " + baseURL
	})

	status, env := h.getJSON(t, "/healthz")
	if status != http.StatusOK || !env.Success {
		failf(t, "/healthz: want 200 success envelope, got HTTP %d success=%v error=%q", status, env.Success, env.Error)
	}
	var hz map[string]string
	decodeData(t, env, &hz)
	if hz["status"] != "ok" {
		failf(t, "/healthz: want data.status=ok, got %v", hz)
	}
	pass(t, "/healthz green: HTTP %d, data=%v", status, hz)

	swStatus, swBody := h.getRawBytes(t, "/swagger/doc.json")
	if swStatus != http.StatusOK {
		failf(t, "/swagger/doc.json: want 200, got %d", swStatus)
	}
	if !bytes.Contains(swBody, []byte(`"swagger"`)) && !bytes.Contains(swBody, []byte(`"openapi"`)) {
		failf(t, "/swagger/doc.json: response is not an OpenAPI document: %.200s", swBody)
	}
	pass(t, "swagger doc served: HTTP %d, %d bytes", swStatus, len(swBody))

	h.ready = true
	pass(t, "Scenario 1 startup complete (server pid %d, home %s)", h.server.Process.Pid, h.home)
}

// §10.2 — 3 Korean episodes → hot file present → "보안을 끄고" stored,
// "보안을 끄는" hits (nori morphology).
func TestScenario02_EpisodicNoriSearch(t *testing.T) {
	requireReady(t)

	for i, text := range koreanEpisodeTexts {
		status, env := h.postJSON(t, h.projPath()+"/episodes", server.CreateEpisodeRequest{
			Kind:       episodic.KindEvent,
			OccurredAt: time.Now().UTC(),
			Actor:      episodic.ActorAgent,
			Text:       text,
			Entities:   []string{"memory-mcp"},
		})
		if status != http.StatusCreated || !env.Success {
			failf(t, "create episode %d: want 201 success, got HTTP %d error=%q", i, status, env.Error)
		}
		var resp server.CreateEpisodeResponse
		decodeData(t, env, &resp)
		if resp.Record.ID == "" {
			failf(t, "create episode %d: no id assigned", i)
		}
		if len(resp.Degraded) != 0 {
			failf(t, "create episode %d: unexpected degraded notes with all services up: %v", i, resp.Degraded)
		}
		h.epIDs = append(h.epIDs, resp.Record.ID)
	}
	pass(t, "3 Korean episodes stored: %v", h.epIDs)

	raw, err := os.ReadFile(h.episodicHotPath())
	if err != nil {
		failf(t, "hot episodic file missing at %s (§1 layout): %v", h.episodicHotPath(), err)
	}
	for i, text := range koreanEpisodeTexts {
		if !bytes.Contains(raw, []byte(text)) {
			failf(t, "hot file lacks episode %d text %q", i, text)
		}
		if !bytes.Contains(raw, []byte(h.epIDs[i])) {
			failf(t, "hot file lacks episode id %s", h.epIDs[i])
		}
	}
	pass(t, "hot file %s holds all 3 texts + ids (%d bytes)", h.episodicHotPath(), len(raw))

	wantID := h.epIDs[0]
	h.waitFor(t, fmt.Sprintf("nori search %q hits the %q episode", noriQuery, "보안을 끄고"), 20*time.Second, func() (bool, string) {
		status, hits := h.searchEpisodes(t, noriQuery)
		if status != http.StatusOK {
			return false, fmt.Sprintf("search HTTP %d", status)
		}
		for _, hit := range hits {
			if hit.Record.ID == wantID {
				return true, fmt.Sprintf("hit id=%s score=%.2f excerpt=%q", hit.Record.ID, hit.Score, hit.Excerpt)
			}
		}
		return false, fmt.Sprintf("%d hits, ids=%v", len(hits), hitIDs(hits))
	})
	h.noriHitID = wantID
	pass(t, "Scenario 2 complete: query %q morphologically matched stored %q (id %s)", noriQuery, "보안을 끄고", wantID)
}

// §10.3 — 2 facts + 1 supersede → Neo4j traversal shows the chain → hot file
// matches.
func TestScenario03_KnowledgeSupersedeChain(t *testing.T) {
	requireReady(t)
	if len(h.epIDs) != 3 {
		failf(t, "prerequisite: scenario 2 episodes missing")
	}

	createNode := func(req server.CreateNodeRequest) knowledge.Node {
		status, env := h.postJSON(t, h.projPath()+"/knowledge/nodes", req)
		if status != http.StatusCreated || !env.Success {
			failf(t, "create node %q: want 201 success, got HTTP %d error=%q", req.Name, status, env.Error)
		}
		var resp server.NodeResponse
		decodeData(t, env, &resp)
		if resp.Node.ID == "" {
			failf(t, "create node %q: no id assigned", req.Name)
		}
		if len(resp.Degraded) != 0 {
			failf(t, "create node %q: unexpected degraded notes with all services up: %v", req.Name, resp.Degraded)
		}
		return resp.Node
	}

	f1 := createNode(server.CreateNodeRequest{Kind: knowledge.KindFact, Name: fact1Name, Body: fact1Body,
		Trust: knowledge.TrustAgentInferred, Provenance: []string{h.epIDs[0]}})
	f2 := createNode(server.CreateNodeRequest{Kind: knowledge.KindFact, Name: fact2Name, Body: fact2Body,
		Trust: knowledge.TrustAgentInferred, Provenance: []string{h.epIDs[1]}})
	f3 := createNode(server.CreateNodeRequest{Kind: knowledge.KindFact, Name: fact3Name, Body: fact3Body,
		Trust: knowledge.TrustUserStated, Provenance: []string{h.epIDs[0]}, Supersedes: []string{f1.ID}})
	pass(t, "facts stored: f1=%s f2=%s f3=%s (f3 supersedes f1)", f1.ID, f2.ID, f3.ID)

	gr, err := graph.NewClient(neo4jBoltURL, neo4jUser, neo4jPassword)
	if err != nil {
		failf(t, "neo4j client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h.waitFor(t, "Neo4j SupersedeChain traversal shows f1→f3, oldest first", 20*time.Second, func() (bool, string) {
		chain, err := gr.SupersedeChain(ctx, hotKey, f3.ID)
		if err != nil {
			return false, "SupersedeChain: " + err.Error()
		}
		ids := make([]string, len(chain))
		for i, n := range chain {
			ids[i] = n.ID
		}
		iOld, iNew := slices.Index(ids, f1.ID), slices.Index(ids, f3.ID)
		if iOld == -1 || iNew == -1 || iOld >= iNew {
			return false, fmt.Sprintf("chain ids=%v (want %s before %s)", ids, f1.ID, f3.ID)
		}
		return true, fmt.Sprintf("chain ids=%v", ids)
	})

	// Independent channel: cypher-shell inside the container.
	if n, ev := h.cypherCount(fmt.Sprintf(
		"MATCH (n) WHERE n.id IN ['%s','%s','%s'] RETURN count(DISTINCT n);", f1.ID, f2.ID, f3.ID)); n != 3 {
		failf(t, "cypher-shell: want 3 fact nodes in Neo4j, got %d (%s)", n, ev)
	}
	if n, ev := h.cypherCount(fmt.Sprintf(
		"MATCH (a)-[r]-(b) WHERE a.id = '%s' AND b.id = '%s' RETURN count(r);", f3.ID, f1.ID)); n < 1 {
		failf(t, "cypher-shell: no relationship between f3 and f1 (%s)", ev)
	}
	pass(t, "cypher-shell confirms 3 nodes and the f3—f1 supersede relationship")

	raw, err := os.ReadFile(h.knowledgeHotPath())
	if err != nil {
		failf(t, "hot knowledge file missing at %s: %v", h.knowledgeHotPath(), err)
	}
	var g knowledge.Graph
	if err := jsonUnmarshal(raw, &g); err != nil {
		failf(t, "hot knowledge file is not a knowledge.Graph document: %v", err)
	}
	hot1, hot3 := findNode(t, g, f1.ID), findNode(t, g, f3.ID)
	if hot1.State != knowledge.StateArchived || hot1.SupersededBy != f3.ID {
		failf(t, "hot f1: want state=archived superseded_by=%s, got state=%s superseded_by=%q", f3.ID, hot1.State, hot1.SupersededBy)
	}
	if !slices.Contains(hot3.Supersedes, f1.ID) || hot3.State != knowledge.StateActive {
		failf(t, "hot f3: want active with supersedes containing %s, got state=%s supersedes=%v", f1.ID, hot3.State, hot3.Supersedes)
	}
	if !hasEdge(g, f3.ID, f1.ID, knowledge.RelSupersedes) {
		failf(t, "hot graph lacks a supersedes edge between %s and %s; edges=%v", f3.ID, f1.ID, g.Edges)
	}
	pass(t, "hot file matches: f1 archived/superseded_by=f3, f3 active, supersedes edge present")

	h.chainIDs = []string{f1.ID, f3.ID}
	h.fact3ID = f3.ID
	pass(t, "Scenario 3 complete: supersede chain %v verified in Neo4j and hot JSON", h.chainIDs)
}

// §10.4 — text-layer PDF upload → blob in S3, chunk search hits, document
// node created.
func TestScenario04_DocumentIngest(t *testing.T) {
	requireReady(t)

	pdfBytes := makeMinimalPDF(pdfFixtureLines)
	status, env := h.postMultipart(t, h.projPath()+"/documents", "file", "blackbox-fixture.pdf", pdfBytes)
	if status != http.StatusCreated || !env.Success {
		failf(t, "document ingest: want 201 success, got HTTP %d error=%q", status, env.Error)
	}
	var res document.IngestResult
	decodeData(t, env, &res)
	if len(res.SHA) != 64 {
		failf(t, "ingest: want 64-hex sha256, got %q", res.SHA)
	}
	if !res.Extractable {
		failf(t, "ingest: PDF has a real text layer but extractable=false (honesty inverted)")
	}
	if len(res.ChunkIDs) < 1 || res.NodeID == "" {
		failf(t, "ingest: want >=1 chunk and a document node, got chunks=%d node=%q", len(res.ChunkIDs), res.NodeID)
	}
	if res.Truncated != nil {
		failf(t, "ingest: tiny fixture must not be truncated, got %+v", res.Truncated)
	}
	pass(t, "ingest result: sha=%s node=%s chunks=%d extractable=true", res.SHA, res.NodeID, len(res.ChunkIDs))

	wantBlobKey := cold.BlobKey(s3Username, res.SHA)
	if res.BlobKey != wantBlobKey {
		failf(t, "ingest blob_key %q != key layout %q (cold/keys.go is the single source of truth)", res.BlobKey, wantBlobKey)
	}
	if ok, ev := h.s3Exists(wantBlobKey); !ok {
		failf(t, "blob not in S3 (cold-first, §6 step 2): %s", ev)
	}
	pass(t, "blob cold-first: s3://%s/%s exists (verified via aws CLI)", s3Bucket, wantBlobKey)

	h.waitFor(t, "chunk search hits by token "+pdfQueryToken, 20*time.Second, func() (bool, string) {
		status, hits := h.searchEpisodes(t, pdfQueryToken)
		if status != http.StatusOK {
			return false, fmt.Sprintf("search HTTP %d", status)
		}
		for _, hit := range hits {
			if hit.Record.Kind == episodic.KindDocumentChunk && hit.Record.Refs != nil && hit.Record.Refs.DocSHA == res.SHA {
				return true, fmt.Sprintf("chunk hit id=%s seq=%d excerpt=%q", hit.Record.ID, hit.Record.Refs.ChunkSeq, hit.Excerpt)
			}
		}
		return false, fmt.Sprintf("%d hits, ids=%v", len(hits), hitIDs(hits))
	})
	pass(t, "chunk search hit: document_chunk with refs.doc_sha=%s", res.SHA)

	h.waitFor(t, "document node in Neo4j", 20*time.Second, func() (bool, string) {
		n, ev := h.cypherCount(fmt.Sprintf("MATCH (n) WHERE n.id = '%s' RETURN count(n);", res.NodeID))
		return n == 1, ev
	})
	raw, err := os.ReadFile(h.knowledgeHotPath())
	if err != nil {
		failf(t, "hot knowledge file: %v", err)
	}
	var g knowledge.Graph
	if err := jsonUnmarshal(raw, &g); err != nil {
		failf(t, "hot knowledge file decode: %v", err)
	}
	docNode := findNode(t, g, res.NodeID)
	if docNode.Kind != knowledge.KindDocument {
		failf(t, "hot document node %s: want kind=document, got %s", res.NodeID, docNode.Kind)
	}
	pass(t, "document node %s exists in Neo4j and hot knowledge (kind=document)", res.NodeID)

	dlStatus, dl := h.getRawBytes(t, "/v1/documents/"+res.SHA)
	if dlStatus != http.StatusOK || !bytes.Equal(dl, pdfBytes) {
		failf(t, "original download: want byte-identical PDF (HTTP 200, %d bytes), got HTTP %d, %d bytes", len(pdfBytes), dlStatus, len(dl))
	}
	ckStatus, ckEnv := h.getJSON(t, "/v1/documents/"+res.SHA+"/chunks")
	if ckStatus != http.StatusOK {
		failf(t, "chunks endpoint: want 200, got %d error=%q", ckStatus, ckEnv.Error)
	}
	var chunks []episodic.Record
	decodeData(t, ckEnv, &chunks)
	if len(chunks) != len(res.ChunkIDs) {
		failf(t, "chunks endpoint: want %d chunks, got %d", len(res.ChunkIDs), len(chunks))
	}
	for i := 1; i < len(chunks); i++ {
		if chunks[i].Refs == nil || chunks[i-1].Refs == nil || chunks[i].Refs.ChunkSeq <= chunks[i-1].Refs.ChunkSeq {
			failf(t, "chunks not in chunk_seq order at %d: %+v then %+v", i, chunks[i-1].Refs, chunks[i].Refs)
		}
	}
	pass(t, "original round-trips byte-identical; %d chunk(s) served in chunk_seq order", len(chunks))

	h.docSHA, h.docNodeID, h.chunkCount = res.SHA, res.NodeID, len(res.ChunkIDs)
	pass(t, "Scenario 4 complete: blob+chunks+document node all present and honest")
}

// §10.5 — compose down+up → zero-data containers → /status detects drift →
// after rehydrate, scenario 2+3 results identical.
func TestScenario05_Rehydration(t *testing.T) {
	requireReady(t)
	if h.noriHitID == "" || len(h.chainIDs) != 2 {
		failf(t, "prerequisite: scenarios 2/3 baselines missing")
	}

	h.composeFresh(t)
	h.waitFor(t, "opensearch back", 240*time.Second, h.opensearchUp)
	h.waitFor(t, "neo4j back", 240*time.Second, h.neo4jUp)
	if n, ev := h.cypherCount("MATCH (n) RETURN count(n);"); n != 0 {
		failf(t, "fresh neo4j should hold zero data (no volumes), got %d nodes (%s)", n, ev)
	}
	pass(t, "containers recreated with zero data (neo4j node count 0)")

	stStatus, stEnv := h.getJSON(t, "/v1/status")
	if stStatus != http.StatusOK {
		failf(t, "/status: want 200, got %d error=%q", stStatus, stEnv.Error)
	}
	var st server.StatusReport
	decodeData(t, stEnv, &st)
	if !st.Drift.Episodic.Detected || !st.Drift.Knowledge.Detected {
		failf(t, "/status must detect drift on empty derived stores; got episodic=%+v knowledge=%+v", st.Drift.Episodic, st.Drift.Knowledge)
	}
	pass(t, "/status detected drift: episodic=%q knowledge=%q", st.Drift.Episodic.Reason, st.Drift.Knowledge.Reason)

	riStatus, riEnv := h.postJSON(t, "/v1/reindex?verify=true", struct{}{})
	if riStatus != http.StatusOK || !riEnv.Success {
		failf(t, "/reindex: want 200 success, got %d error=%q", riStatus, riEnv.Error)
	}
	var rep rehydrate.Report
	decodeData(t, riEnv, &rep)
	if rep.EpisodesIndexed < 3+h.chunkCount || rep.NodesUpserted < 4 {
		failf(t, "/reindex counts too low: episodes=%d (want >=%d) nodes=%d (want >=4) failures=%v",
			rep.EpisodesIndexed, 3+h.chunkCount, rep.NodesUpserted, rep.Failures)
	}
	if len(rep.Failures) != 0 {
		failf(t, "/reindex reported failures: %v", rep.Failures)
	}
	pass(t, "full rehydration: %d episodes bulk-indexed, %d nodes / %d edges MERGEd, verified=%v",
		rep.EpisodesIndexed, rep.NodesUpserted, rep.EdgesUpserted, rep.Verified)

	h.waitFor(t, "nori search result identical after rehydration", 30*time.Second, func() (bool, string) {
		status, hits := h.searchEpisodes(t, noriQuery)
		if status != http.StatusOK {
			return false, fmt.Sprintf("search HTTP %d", status)
		}
		if slices.Contains(hitIDs(hits), h.noriHitID) {
			return true, fmt.Sprintf("hit ids=%v", hitIDs(hits))
		}
		return false, fmt.Sprintf("hit ids=%v (want %s)", hitIDs(hits), h.noriHitID)
	})

	gr, err := graph.NewClient(neo4jBoltURL, neo4jUser, neo4jPassword)
	if err != nil {
		failf(t, "neo4j client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	chain, err := gr.SupersedeChain(ctx, hotKey, h.fact3ID)
	if err != nil {
		failf(t, "SupersedeChain after rehydration: %v", err)
	}
	ids := make([]string, len(chain))
	for i, n := range chain {
		ids[i] = n.ID
	}
	iOld, iNew := slices.Index(ids, h.chainIDs[0]), slices.Index(ids, h.chainIDs[1])
	if iOld == -1 || iNew == -1 || iOld >= iNew {
		failf(t, "chain after rehydration differs: got %v, want %v in order", ids, h.chainIDs)
	}
	pass(t, "Scenario 5 complete: search hit %s and chain %v identical after container loss + rehydration", h.noriHitID, h.chainIDs)
}

// §10.6 — aged consolidated fixture → /consolidate → S3 {yyyy-mm}.json,
// removed from hot, unconsolidated remains, knowledge snapshot in S3.
func TestScenario06_Consolidation(t *testing.T) {
	requireReady(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	oldAt := time.Now().UTC().AddDate(0, 0, -40)
	postAged := func(text string) string {
		status, env := h.postJSON(t, h.projPath()+"/episodes", server.CreateEpisodeRequest{
			Kind: episodic.KindEvent, OccurredAt: oldAt, Actor: episodic.ActorAgent,
			Text: text, Entities: []string{"consolidation"},
		})
		if status != http.StatusCreated {
			failf(t, "create aged episode: want 201, got %d error=%q", status, env.Error)
		}
		var resp server.CreateEpisodeResponse
		decodeData(t, env, &resp)
		return resp.Record.ID
	}
	h.oldID, h.staleID = postAged(agedText), postAged(staleText)

	// Fixture: flip consolidated=true through the canonical store — the
	// server never auto-consolidates (§0 principle 2), distillation is the
	// agent's job, and this harness plays the agent.
	if err := h.store().UpdateEpisodes(ctx, hotKey, []string{h.oldID}, func(r episodic.Record) episodic.Record {
		r.Consolidated = true
		return r
	}); err != nil {
		failf(t, "mark fixture consolidated via hotstore: %v", err)
	}
	got, err := h.store().GetEpisode(ctx, hotKey, h.oldID)
	if err != nil || !got.Consolidated {
		failf(t, "fixture readback: want consolidated=true, got %+v err=%v", got, err)
	}
	pass(t, "fixtures ready: aged consolidated %s and stale unconsolidated %s (occurred %s)", h.oldID, h.staleID, oldAt.Format(time.RFC3339))

	cStatus, cEnv := h.postJSON(t, "/v1/consolidate", server.ConsolidateRequest{Projects: []string{hotKey.String()}})
	if cStatus != http.StatusOK || !cEnv.Success {
		failf(t, "/consolidate: want 200 success, got %d error=%q", cStatus, cEnv.Error)
	}
	var rep consolidate.Report
	decodeData(t, cEnv, &rep)
	if len(rep.Failures) != 0 {
		failf(t, "/consolidate reported failures: %v", rep.Failures)
	}

	wantArchiveKey := cold.EpisodeArchiveKey(s3Username, hotKey, cold.ArchiveMonth(oldAt))
	if rep.MovedEpisodes < 1 || !slices.Contains(rep.ArchiveKeys, wantArchiveKey) {
		failf(t, "/consolidate: want >=1 moved with archive key %q, got moved=%d keys=%v", wantArchiveKey, rep.MovedEpisodes, rep.ArchiveKeys)
	}
	if ok, ev := h.s3Exists(wantArchiveKey); !ok {
		failf(t, "monthly archive missing in S3: %s", ev)
	}
	body, err := h.s3Cat(wantArchiveKey)
	if err != nil {
		failf(t, "fetch archive from S3: %v", err)
	}
	if !strings.Contains(body, h.oldID) || !strings.Contains(body, "agedmarkerx") {
		failf(t, "archive %s lacks aged episode %s: %.400s", wantArchiveKey, h.oldID, body)
	}
	pass(t, "S3 archive s3://%s/%s exists and contains aged episode %s (%d bytes)", s3Bucket, wantArchiveKey, h.oldID, len(body))

	raw, err := os.ReadFile(h.episodicHotPath())
	if err != nil {
		failf(t, "hot episodic file: %v", err)
	}
	if bytes.Contains(raw, []byte(h.oldID)) {
		failf(t, "aged consolidated episode %s still in hot after S3-confirmed aging", h.oldID)
	}
	if !bytes.Contains(raw, []byte(h.staleID)) {
		failf(t, "stale unconsolidated episode %s was removed from hot — §3.1 forbids aging undistilled records", h.staleID)
	}
	if _, err := h.store().GetEpisode(ctx, hotKey, h.oldID); !errors.Is(err, hotstore.ErrNotFound) {
		failf(t, "hot GetEpisode(%s): want ErrNotFound after aging, got err=%v", h.oldID, err)
	}
	pass(t, "hot state honest: aged %s removed, stale unconsolidated %s remains", h.oldID, h.staleID)

	wantLatest := cold.KnowledgeLatestKey(s3Username, hotKey)
	if len(rep.SnapshotKeys) < 1 {
		failf(t, "/consolidate: no knowledge snapshot keys reported")
	}
	for _, k := range append([]string{wantLatest}, rep.SnapshotKeys...) {
		if !strings.HasPrefix(k, s3Prefix) {
			failf(t, "snapshot key %q escapes the blackbox prefix %q", k, s3Prefix)
		}
		if ok, ev := h.s3Exists(k); !ok {
			failf(t, "knowledge snapshot missing in S3: %s", ev)
		}
	}
	pass(t, "knowledge snapshots in S3: latest=%s plus %v", wantLatest, rep.SnapshotKeys)

	h.waitFor(t, "aged episode purged from index, stale still searchable", 30*time.Second, func() (bool, string) {
		_, agedHits := h.searchEpisodes(t, "agedmarkerx")
		_, staleHits := h.searchEpisodes(t, "stalemarkerx")
		if len(agedHits) == 0 && len(staleHits) >= 1 {
			return true, fmt.Sprintf("aged=0 hits, stale ids=%v", hitIDs(staleHits))
		}
		return false, fmt.Sprintf("aged ids=%v stale ids=%v", hitIDs(agedHits), hitIDs(staleHits))
	})
	pass(t, "Scenario 6 complete: aging order S3-put → hot remove → index delete held; unconsolidated survived")
}

// §10.7 — OpenSearch down → episode write succeeds + degraded report →
// container back → rehydrate → searchable.
func TestScenario07_DegradedMode(t *testing.T) {
	requireReady(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if out, err := runCmd("", "docker", "stop", osContainer); err != nil {
		failf(t, "docker stop %s: %v\n%s", osContainer, err, out)
	}
	h.waitFor(t, "opensearch down", 60*time.Second, func() (bool, string) {
		up, ev := h.opensearchUp()
		return !up, ev
	})
	pass(t, "OpenSearch container stopped; server keeps running host-side")

	status, env := h.postJSON(t, h.projPath()+"/episodes", server.CreateEpisodeRequest{
		Kind: episodic.KindObservation, OccurredAt: time.Now().UTC(), Actor: episodic.ActorAgent,
		Text: degradedText, Entities: []string{"degraded"},
	})
	if status != http.StatusCreated || !env.Success {
		failf(t, "write during outage: hot write must succeed with 201 (§5, never 503 on write), got %d error=%q", status, env.Error)
	}
	var resp server.CreateEpisodeResponse
	decodeData(t, env, &resp)
	if len(resp.Degraded) == 0 {
		failf(t, "write during outage: response must carry a degraded note (§5 honesty), got none")
	}
	h.degradedID = resp.Record.ID
	pass(t, "episode %s written during outage: HTTP 201 with degraded=%v", h.degradedID, resp.Degraded)

	if rec, err := h.store().GetEpisode(ctx, hotKey, h.degradedID); err != nil || rec.Text != degradedText {
		failf(t, "hot store must own the degraded-mode write: %+v err=%v", rec, err)
	}
	sStatus, _ := h.searchEpisodes(t, "degradedmarkerx")
	if sStatus != http.StatusServiceUnavailable {
		failf(t, "search during outage: want 503 (§5 reads), got %d", sStatus)
	}
	pass(t, "during outage: hot holds the record; search read honestly answers 503")

	if out, err := runCmd("", "docker", "start", osContainer); err != nil {
		failf(t, "docker start %s: %v\n%s", osContainer, err, out)
	}
	h.waitFor(t, "opensearch back", 240*time.Second, h.opensearchUp)
	riStatus, riEnv := h.postJSON(t, "/v1/reindex", struct{}{})
	if riStatus != http.StatusOK {
		failf(t, "/reindex after recovery: want 200, got %d error=%q", riStatus, riEnv.Error)
	}
	h.waitFor(t, "degraded-mode episode searchable after recovery", 30*time.Second, func() (bool, string) {
		status, hits := h.searchEpisodes(t, "degradedmarkerx")
		if status != http.StatusOK {
			return false, fmt.Sprintf("search HTTP %d", status)
		}
		if slices.Contains(hitIDs(hits), h.degradedID) {
			return true, fmt.Sprintf("hit ids=%v", hitIDs(hits))
		}
		return false, fmt.Sprintf("hit ids=%v (want %s)", hitIDs(hits), h.degradedID)
	})
	pass(t, "Scenario 7 complete: degraded write converged into the index after recovery")
}

// §10.8 — /status reports freshness, unconsolidated and drift accurately
// across everything the previous scenarios left behind.
func TestScenario08_StatusHonesty(t *testing.T) {
	requireReady(t)
	if h.chunkCount == 0 || h.staleID == "" || h.degradedID == "" {
		failf(t, "prerequisite: scenarios 4/6/7 state missing — honesty totals cannot be computed")
	}

	status, env := h.getJSON(t, "/v1/status")
	if status != http.StatusOK || !env.Success {
		failf(t, "/status: want 200 success, got %d error=%q", status, env.Error)
	}
	var st server.StatusReport
	decodeData(t, env, &st)
	t.Logf("status report: %s", env.Data)

	if st.Drift.Episodic.Detected || st.Drift.Episodic.Unavailable ||
		st.Drift.Knowledge.Detected || st.Drift.Knowledge.Unavailable {
		failf(t, "drift must be clean after final rehydration: %+v", st.Drift)
	}
	pass(t, "drift clean: episodic=%+v knowledge=%+v", st.Drift.Episodic, st.Drift.Knowledge)

	// Exact honesty accounting: 3 Korean episodes + PDF chunks + stale aged
	// fixture + degraded-mode episode. The aged consolidated one sank to cold.
	wantUnconsolidated := len(koreanEpisodeTexts) + h.chunkCount + 1 + 1
	if st.Unconsolidated != wantUnconsolidated {
		failf(t, "unconsolidated: want exactly %d (3 korean + %d chunks + 1 stale + 1 degraded), got %d",
			wantUnconsolidated, h.chunkCount, st.Unconsolidated)
	}
	if st.StaleUnconsolidated != 1 {
		failf(t, "stale_unconsolidated: want exactly 1 (the 40-day fixture %s), got %d", h.staleID, st.StaleUnconsolidated)
	}
	pass(t, "counts honest: unconsolidated=%d, stale_unconsolidated=%d (never auto-deleted, §3.1)", st.Unconsolidated, st.StaleUnconsolidated)

	if st.ManifestUpdatedAt.IsZero() {
		failf(t, "manifest_updated_at is zero — freshness not reported")
	}
	if len(st.DirtyFiles) != 0 {
		failf(t, "dirty_files should be empty after rehydration cleared the outage marks, got %v", st.DirtyFiles)
	}
	if !st.S3.Reachable || st.S3.Bucket != s3Bucket {
		failf(t, "s3 sync status: want reachable=true bucket=%s, got %+v", s3Bucket, st.S3)
	}
	if st.S3.LastArchiveAt.IsZero() || st.S3.LastSnapshotAt.IsZero() {
		failf(t, "s3 sync status must reflect scenario 6 activity (archive+snapshot), got %+v", st.S3)
	}
	if len(st.Degraded) != 0 {
		failf(t, "degraded must be empty with every service up, got %v", st.Degraded)
	}
	pass(t, "Scenario 8 complete: manifest fresh (%s), no dirty files, S3 %s reachable with archive/snapshot times, nothing degraded",
		st.ManifestUpdatedAt.Format(time.RFC3339), st.S3.Bucket)
}

// ---------- small shared assertions ----------

func findNode(t *testing.T, g knowledge.Graph, id string) knowledge.Node {
	t.Helper()
	for _, n := range g.Nodes {
		if n.ID == id {
			return n
		}
	}
	failf(t, "node %s not found in hot knowledge graph (%d nodes)", id, len(g.Nodes))
	return knowledge.Node{}
}

func hasEdge(g knowledge.Graph, a, b string, rel knowledge.Rel) bool {
	for _, e := range g.Edges {
		if e.Rel != rel {
			continue
		}
		if (e.From == a && e.To == b) || (e.From == b && e.To == a) {
			return true
		}
	}
	return false
}

// jsonUnmarshal isolates the encoding/json dependency for hot-file decoding.
func jsonUnmarshal(raw []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	return dec.Decode(out)
}
