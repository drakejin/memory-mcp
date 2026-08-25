//go:build e2e

// TestP00_HarnessSmoke proves the shared harness itself before any P-scenario
// trusts it: one episode, two knowledge nodes + one edge, one tiny document,
// one direct-written past record — with every one of the four store
// inspectors (hot JSON, OpenSearch, Neo4j, S3) probed against a live server,
// and every seed verb exercised end to end.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	cold "github.com/drakejin/memory-mcp/internal/external/thirdparty/cold"
	httpserver "github.com/drakejin/memory-mcp/internal/handler/app/httpserver"
	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/service/knowledge"
	"github.com/drakejin/memory-mcp/internal/service/rehydrate"
)

// Unique fixture tokens: single unambiguous words so full-text hits and
// absences cannot be confused with other suites' leftovers.
const (
	smokeEpisodeToken = "smokealphatoken"
	smokeDocToken     = "smokedoctoken"
	smokeTravelToken  = "smoketimetravel"
)

func TestP00_HarnessSmoke(t *testing.T) {
	h.ensureServer(t)
	key := e2eKey("p00-smoke")

	// ---- episode through the API, verified in hot + OpenSearch ----
	ep := h.postEpisode(t, key, httpserver.CreateEpisodeRequest{
		Kind:     episode.KindEvent,
		Actor:    episode.ActorAgent,
		Text:     "e2e 스모크: 보안을 끄고 검증한다 " + smokeEpisodeToken,
		Entities: []string{"opensearch"},
	})
	if len(ep.Degraded) != 0 {
		failf(t, "postEpisode: unexpected degraded notes with every store up: %v", ep.Degraded)
	}

	hotRec, ok := h.hotEpisodeByID(t, key, ep.Record.ID)
	if !ok {
		failf(t, "hot inspector: episode %s missing from %s", ep.Record.ID, h.hotEpisodePath(key))
	}
	if hotRec.Text != ep.Record.Text {
		failf(t, "hot inspector: text mismatch: file %q vs response %q", hotRec.Text, ep.Record.Text)
	}
	if !bytes.Contains(readFileRaw(t, h.hotEpisodePath(key)), []byte(ep.Record.ID)) {
		failf(t, "raw hot bytes do not contain the episode id %s", ep.Record.ID)
	}
	man := h.hotManifest(t)
	fileKey := rehydrate.ManifestFileKey(rehydrate.PlaneEpisodic, key)
	if fs, ok := man.Files[fileKey]; !ok || fs.RecordCount != 1 {
		failf(t, "manifest inspector: want Files[%q].RecordCount=1, got %+v (present=%v)", fileKey, man.Files[fileKey], ok)
	}
	pass(t, "hot inspector: %s holds the record; manifest tracks it", h.hotEpisodePath(key))

	if !h.osIndexExists(t) {
		failf(t, "os inspector: index %s missing while the server just indexed into it", osIndex)
	}
	src, found := h.osDoc(t, key, ep.Record.ID)
	if !found {
		failf(t, "os inspector: _doc %s#%s not found (realtime GET)", key, ep.Record.ID)
	}
	if !bytes.Contains(src, []byte(smokeEpisodeToken)) {
		failf(t, "os inspector: _source lacks token %s: %.300s", smokeEpisodeToken, src)
	}
	h.osRefresh(t)
	res, found := h.osSearchRaw(t, fmt.Sprintf(
		`{"size":10,"query":{"bool":{"must":[{"match":{"text":%q}}],"filter":[{"term":{"workspace":%q}},{"term":{"team":%q}},{"term":{"project":%q}}]}}}`,
		smokeEpisodeToken, key.Workspace, key.Team, key.Project))
	if !found || res.Total != 1 {
		failf(t, "os inspector: raw _search for %s: want exactly 1 hit, found=%v total=%d", smokeEpisodeToken, found, res.Total)
	}
	if wantID := key.String() + "#" + ep.Record.ID; res.Hits[0].ID != wantID {
		failf(t, "os inspector: composite _id %q != %q", res.Hits[0].ID, wantID)
	}
	if n, foundIdx := h.osProjectCount(t, key); !foundIdx || n != 1 {
		failf(t, "os inspector: project _count: want 1, got %d (index present=%v)", n, foundIdx)
	}
	pass(t, "os inspector: _doc, raw _search and _count all see %s directly", ep.Record.ID)

	// Server-path search (seed helper) must converge on the same record.
	h.waitFor(t, "server search hits "+smokeEpisodeToken, 20*time.Second, func() (bool, string) {
		status, hits := h.searchEpisodes(t, key, smokeEpisodeToken)
		if status != http.StatusOK {
			return false, fmt.Sprintf("search HTTP %d", status)
		}
		for _, hit := range hits {
			if hit.Record.ID == ep.Record.ID {
				return true, fmt.Sprintf("hit id=%s score=%.2f", hit.Record.ID, hit.Score)
			}
		}
		return false, fmt.Sprintf("%d hits, ids=%v", len(hits), hitIDs(hits))
	})
	if status, got := h.getEpisode(t, key, ep.Record.ID); status != http.StatusOK || got.ID != ep.Record.ID {
		failf(t, "getEpisode: want 200 with id %s, got HTTP %d record %+v", ep.Record.ID, status, got)
	}

	// ---- knowledge through the API, verified in hot + Neo4j ----
	n1 := h.postNode(t, key, knowledge.CreateNodeInput{
		Kind: knowledge.KindFact, Name: "smoke-fact", Body: "e2e 하네스 스모크 사실",
		Trust: knowledge.TrustAgentInferred, Provenance: []string{ep.Record.ID},
	})
	n2 := h.postNode(t, key, knowledge.CreateNodeInput{
		Kind: knowledge.KindFact, Name: "smoke-fact-2", Body: "이웃 사실",
		Trust: knowledge.TrustAgentInferred, Provenance: []string{ep.Record.ID},
	})
	h.postEdge(t, key, knowledge.Edge{
		From: n2.Node.ID, To: n1.Node.ID, Rel: knowledge.RelRelatesTo,
		Provenance: []string{ep.Record.ID}, Confidence: 0.9,
	})

	graph := h.hotKnowledge(t, key)
	if len(graph.Nodes) != 2 || len(graph.Edges) != 1 {
		failf(t, "hot inspector: knowledge doc: want 2 nodes / 1 edge, got %d / %d", len(graph.Nodes), len(graph.Edges))
	}
	hotNode, ok := h.hotKnowledgeNode(t, key, n1.Node.ID)
	if !ok || hotNode.State != knowledge.StateActive {
		failf(t, "hot inspector: node %s: want state active in hot doc, got %+v (present=%v)", n1.Node.ID, hotNode.State, ok)
	}
	// §3 promotion is a hot mutation the direct parser must see: creating n1
	// named ep in its provenance, so ep is now consolidated in the file.
	if rec, _ := h.hotEpisodeByID(t, key, ep.Record.ID); !rec.Consolidated {
		failf(t, "hot inspector: provenance promotion did not mark %s consolidated in the hot file", ep.Record.ID)
	}

	if got := h.neoNodeCount(t, key); got != 2 {
		failf(t, "neo inspector: node count: want 2, got %d", got)
	}
	if state, found := h.neoNodeField(t, key, n1.Node.ID, "state"); !found || state != string(knowledge.StateActive) {
		failf(t, "neo inspector: node %s state: want %q, got %q (found=%v)", n1.Node.ID, knowledge.StateActive, state, found)
	}
	if got := h.neoEdgeCount(t, key, string(knowledge.RelRelatesTo)); got != 1 {
		failf(t, "neo inspector: relates_to edge count: want 1, got %d", got)
	}
	pass(t, "neo inspector: cypher-shell sees both nodes, the active state and the edge")

	// ---- document through the API, verified in S3 + hot + OS + Neo4j ----
	content := []byte("# e2e harness smoke fixture\n\n" + smokeDocToken + " carries the blob round-trip evidence.\n")
	doc := h.ingestDoc(t, key, "e2e-smoke.md", content)
	if !doc.Extractable || len(doc.ChunkIDs) == 0 || doc.NodeID == "" {
		failf(t, "ingestDoc: want extractable with chunks and a node, got %+v", doc)
	}
	blobKey := cold.BlobKey(s3Username, doc.SHA)
	if doc.BlobKey != blobKey {
		failf(t, "ingestDoc blob_key %q != cold key layout %q", doc.BlobKey, blobKey)
	}
	if ok, ev := h.s3Exists(blobKey); !ok {
		failf(t, "s3 inspector: blob missing (cold-first violated): %s", ev)
	}
	if got := h.s3Cat(t, blobKey); !bytes.Equal(got, content) {
		failf(t, "s3 inspector: blob bytes differ: got %d bytes, want %d", len(got), len(content))
	}
	if keys := h.s3List(t, s3Prefix); !slices.Contains(keys, blobKey) {
		failf(t, "s3 inspector: listing under %s lacks %s: %v", s3Prefix, blobKey, keys)
	}
	pass(t, "s3 inspector: head-object, cp and list-objects-v2 all see s3://%s/%s", s3Bucket, blobKey)

	chunk, ok := h.hotEpisodeByID(t, key, doc.ChunkIDs[0])
	if !ok || chunk.Kind != episode.KindDocumentChunk || chunk.Refs == nil || chunk.Refs.DocSHA != doc.SHA {
		failf(t, "hot inspector: chunk %s: want document_chunk with refs.doc_sha=%s, got %+v", doc.ChunkIDs[0], doc.SHA, chunk)
	}
	if _, found := h.osDoc(t, key, doc.ChunkIDs[0]); !found {
		failf(t, "os inspector: chunk %s not indexed", doc.ChunkIDs[0])
	}
	if kind, found := h.neoNodeField(t, key, doc.NodeID, "kind"); !found || kind != string(knowledge.KindDocument) {
		failf(t, "neo inspector: document node %s kind: want %q, got %q (found=%v)", doc.NodeID, knowledge.KindDocument, kind, found)
	}
	if status, raw := h.api(t).getRawBytes(t, "/v1/documents/"+doc.SHA); status != http.StatusOK || !bytes.Equal(raw, content) {
		failf(t, "GET /v1/documents/%s: want 200 with original %d bytes, got HTTP %d with %d bytes", doc.SHA, len(content), status, len(raw))
	}

	// ---- time travel: direct hot write + reindex, verified everywhere ----
	past := time.Now().UTC().AddDate(0, -2, 0).Truncate(time.Second)
	tid := newULIDAt(t, past)
	h.writeHotEpisodeDirect(t, key, episode.Record{
		ID: tid, Kind: episode.KindObservation, OccurredAt: past, Actor: episode.ActorSystem,
		Text: "direct fixture " + smokeTravelToken, Entities: []string{},
	})
	travel, ok := h.hotEpisodeByID(t, key, tid)
	if !ok || !travel.OccurredAt.Equal(past) {
		failf(t, "direct write: hot record %s occurred_at %s != planted %s (present=%v)", tid, travel.OccurredAt, past, ok)
	}

	wantEpisodes := 2 + len(doc.ChunkIDs) // api episode + chunks + time-travel record
	rep := h.reindex(t, true)
	if !rep.Verified || len(rep.Failures) != 0 {
		failf(t, "reindex verify: want verified with no failures, got verified=%v failures=%v", rep.Verified, rep.Failures)
	}
	if rep.EpisodesIndexed != wantEpisodes || rep.NodesUpserted != 3 || rep.EdgesUpserted != 1 {
		failf(t, "reindex counts: want episodes=%d nodes=3 edges=1, got episodes=%d nodes=%d edges=%d",
			wantEpisodes, rep.EpisodesIndexed, rep.NodesUpserted, rep.EdgesUpserted)
	}
	src, found = h.osDoc(t, key, tid)
	if !found {
		failf(t, "os inspector: time-travel record %s absent after reindex", tid)
	}
	var osRec episode.Record
	if err := json.Unmarshal(src, &osRec); err != nil || !osRec.OccurredAt.Equal(past) {
		failf(t, "os inspector: time-travel _source occurred_at %s != planted %s (err=%v)", osRec.OccurredAt, past, err)
	}
	pass(t, "time-travel tool: planted %s at %s, reindex converged all stores", tid, past.Format(time.RFC3339))

	// ---- consolidate (dry-run) + status decode against known state ----
	crep := h.consolidate(t, httpserver.ConsolidateRequest{Projects: []string{key.String()}, DryRun: true})
	if len(crep.Failures) != 0 {
		failf(t, "consolidate dry-run: unexpected failures: %v", crep.Failures)
	}
	st := h.status(t)
	if !st.S3.Reachable || len(st.Degraded) != 0 || len(st.DirtyFiles) != 0 {
		failf(t, "status: want reachable S3 and nothing degraded/dirty, got s3=%v degraded=%v dirty=%v",
			st.S3.Reachable, st.Degraded, st.DirtyFiles)
	}
	// ep was promoted; chunks and the 2-month-old direct record are not, and
	// only the direct record breaches the 30d TTL.
	if wantUncons := len(doc.ChunkIDs) + 1; st.Unconsolidated != wantUncons || st.StaleUnconsolidated != 1 {
		failf(t, "status honesty: want unconsolidated=%d stale=1, got unconsolidated=%d stale=%d",
			wantUncons, st.Unconsolidated, st.StaleUnconsolidated)
	}
	pass(t, "P00 complete: all four inspectors and every seed verb verified against live stores")
}
