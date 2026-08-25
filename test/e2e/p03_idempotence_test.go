//go:build e2e

// The TestP03_* scenarios prove feature-inventory.md §2 P3: retry-shaped
// operations — same-sha re-ingest (with its blob re-upload), double reindex,
// and re-aging into the same {yyyy-mm} archive month — produce identical
// state however often they run. Counts and bytes are asserted in all four
// stores directly; S3 no-rewrite claims lean on bucket versioning (a re-put
// mints a new version even for identical bytes, so a stable version count is
// proof the object was not touched).
package e2e

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	cold "github.com/drakejin/memory-mcp/internal/external/thirdparty/cold"
	httpserver "github.com/drakejin/memory-mcp/internal/handler/app/httpserver"
	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/service/knowledge"
)

// Unique fixture tokens (single unambiguous words, see p00).
const (
	p03IngestToken  = "pzerothreeingesttoken"
	p03ReindexToken = "pzerothreereindextoken"
	p03AgeTokenA    = "pzerothreeagerecorda"
	p03AgeTokenB    = "pzerothreeagerecordb"
	p03AgeTokenD    = "pzerothreeagerecordd"
)

// p03JSON renders v as canonical JSON — the comparison idiom shared by these
// scenarios (fixed struct field order; no time.Time DeepEqual traps).
func p03JSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		failf(t, "marshal %T for comparison: %v", v, err)
	}
	return string(raw)
}

// p03VersionCount counts the stored S3 versions of exactly key. The bucket has
// versioning enabled and the e2e prefix is cleaned with delete markers, so
// absolute counts accumulate across runs — callers assert DELTAS only.
func p03VersionCount(t *testing.T, key string) int {
	t.Helper()
	stdout, stderr, err := runCmdStdout("aws", awsArgs(
		"s3api", "list-object-versions", "--bucket", s3Bucket, "--prefix", key, "--output", "json")...)
	if err != nil {
		failf(t, "aws s3api list-object-versions --prefix %s: %v: %s", key, err, strings.TrimSpace(stderr))
	}
	if strings.TrimSpace(stdout) == "" {
		return 0
	}
	var parsed struct {
		Versions []struct {
			Key string `json:"Key"`
		} `json:"Versions"`
	}
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		failf(t, "list-object-versions: decode output: %v: %.300s", err, stdout)
	}
	n := 0
	for _, v := range parsed.Versions {
		if v.Key == key { // --prefix matches prefixes; count the exact key only
			n++
		}
	}
	return n
}

// TestP03_IdempotentIngestAndBlob: ingesting byte-identical content twice
// (same sha) changes nothing anywhere — chunk episodes are not duplicated,
// the document node is found instead of recreated, and the content-addressed
// blob is not re-uploaded to S3.
func TestP03_IdempotentIngestAndBlob(t *testing.T) {
	h.ensureServer(t)
	key := e2eKey("p03-ingest")
	content := []byte("# p03 idempotence fixture\n\n" + p03IngestToken + " same bytes, same sha, one blob.\n")

	// ---- ingest #1: establish the baseline in all four stores ----
	res1 := h.ingestDoc(t, key, "p03-idempotent.md", content)
	if !res1.Extractable || len(res1.ChunkIDs) == 0 || res1.NodeID == "" || len(res1.Degraded) != 0 {
		failf(t, "ingest #1: want extractable with chunks, a node and no degraded notes, got %+v", res1)
	}
	blobKey := cold.BlobKey(s3Username, res1.SHA)
	if res1.BlobKey != blobKey {
		failf(t, "ingest #1: blob_key %q != cold layout %q", res1.BlobKey, blobKey)
	}

	hotEps := h.hotEpisodes(t, key)
	if len(hotEps) != len(res1.ChunkIDs) {
		failf(t, "hot inspector: want exactly the %d chunk episodes, got %d", len(res1.ChunkIDs), len(hotEps))
	}
	if g := h.hotKnowledge(t, key); len(g.Nodes) != 1 || len(g.Edges) != 0 {
		failf(t, "hot inspector: want exactly 1 document node / 0 edges, got %d/%d", len(g.Nodes), len(g.Edges))
	}
	episodicBytes := readFileRaw(t, h.hotEpisodePath(key))
	knowledgeBytes := readFileRaw(t, h.hotKnowledgePath(key))
	h.osRefresh(t)
	osCount, osFound := h.osProjectCount(t, key)
	if !osFound || osCount != len(res1.ChunkIDs) {
		failf(t, "os inspector: want %d indexed chunks, got %d (index present=%v)", len(res1.ChunkIDs), osCount, osFound)
	}
	chunkSrc, found := h.osDoc(t, key, res1.ChunkIDs[0])
	if !found {
		failf(t, "os inspector: chunk %s not indexed after ingest #1", res1.ChunkIDs[0])
	}
	if n := h.neoNodeCount(t, key); n != 1 {
		failf(t, "neo inspector: want 1 document node, got %d", n)
	}
	if got := h.s3Cat(t, blobKey); !bytes.Equal(got, content) {
		failf(t, "s3 inspector: blob bytes differ after ingest #1: got %d bytes, want %d", len(got), len(content))
	}
	blobVersions := p03VersionCount(t, blobKey)
	if blobVersions < 1 {
		failf(t, "s3 inspector: no stored version of %s after ingest #1", blobKey)
	}
	pass(t, "baseline: %d chunks, 1 node, blob at s3://%s/%s (version count %d)", len(res1.ChunkIDs), s3Bucket, blobKey, blobVersions)

	// ---- ingest #2: identical bytes, identical result, zero writes ----
	res2 := h.ingestDoc(t, key, "p03-idempotent.md", content)
	if p03JSON(t, res1) != p03JSON(t, res2) {
		failf(t, "re-ingest result differs from the original\n#1: %s\n#2: %s", p03JSON(t, res1), p03JSON(t, res2))
	}
	if !slices.Equal(res1.ChunkIDs, res2.ChunkIDs) {
		failf(t, "re-ingest minted new chunk ids: %v vs %v", res1.ChunkIDs, res2.ChunkIDs)
	}

	// Hot: not just equal counts — the canonical files were not even rewritten.
	if !bytes.Equal(readFileRaw(t, h.hotEpisodePath(key)), episodicBytes) {
		failf(t, "hot inspector: episodic file rewritten by a same-sha re-ingest")
	}
	if !bytes.Equal(readFileRaw(t, h.hotKnowledgePath(key)), knowledgeBytes) {
		failf(t, "hot inspector: knowledge file rewritten by a same-sha re-ingest")
	}
	h.osRefresh(t)
	if n, _ := h.osProjectCount(t, key); n != osCount {
		failf(t, "os inspector: doc count moved %d -> %d on re-ingest", osCount, n)
	}
	if src2, f := h.osDoc(t, key, res1.ChunkIDs[0]); !f || !bytes.Equal(src2, chunkSrc) {
		failf(t, "os inspector: chunk _source changed on re-ingest (found=%v)", f)
	}
	if n := h.neoNodeCount(t, key); n != 1 {
		failf(t, "neo inspector: node count moved to %d on re-ingest", n)
	}
	// S3: same bytes AND no new version — the blob was never re-uploaded.
	if got := h.s3Cat(t, blobKey); !bytes.Equal(got, content) {
		failf(t, "s3 inspector: blob bytes changed on re-ingest")
	}
	if n := p03VersionCount(t, blobKey); n != blobVersions {
		failf(t, "s3 inspector: blob version count moved %d -> %d — the re-ingest re-uploaded a content-addressed object", blobVersions, n)
	}
	pass(t, "P3 re-ingest: identical result, all four stores byte/count stable, blob upload skipped")
}

// TestP03_IdempotentReindex: running the full rehydration twice produces the
// same report and leaves every store byte-for-byte where the first run put it
// — and never touches the canonical hot files at all.
func TestP03_IdempotentReindex(t *testing.T) {
	h.ensureServer(t)
	key := e2eKey("p03-reindex")

	// Own seed so the scenario stands alone: one searchable episode plus a
	// two-node/one-edge graph gives both planes something to replay.
	ep := h.postEpisode(t, key, httpserver.CreateEpisodeRequest{
		Kind: episode.KindEvent, Actor: episode.ActorAgent,
		Text:     "p03 재수화 멱등성 " + p03ReindexToken,
		Entities: []string{"reindex"},
	})
	n1 := h.postNode(t, key, knowledge.CreateNodeInput{
		Kind: knowledge.KindFact, Name: "p03-reindex-fact", Body: "재수화 대상 사실",
		Trust: knowledge.TrustAgentInferred, Provenance: []string{ep.Record.ID},
	})
	n2 := h.postNode(t, key, knowledge.CreateNodeInput{
		Kind: knowledge.KindFact, Name: "p03-reindex-fact-2", Body: "이웃 사실",
		Trust: knowledge.TrustAgentInferred,
	})
	h.postEdge(t, key, knowledge.Edge{From: n1.Node.ID, To: n2.Node.ID, Rel: knowledge.RelRelatesTo, Confidence: 0.8})

	rep1 := h.reindex(t, true)
	if !rep1.Verified || len(rep1.Failures) != 0 {
		failf(t, "reindex #1: want verified with no failures, got verified=%v failures=%v", rep1.Verified, rep1.Failures)
	}
	// Snapshot after pass #1. Counts are per-project (shared-home safe); the
	// report totals are home-global, so they are only compared BETWEEN runs.
	h.osRefresh(t)
	osCount1, _ := h.osProjectCount(t, key)
	src1, found := h.osDoc(t, key, ep.Record.ID)
	if !found {
		failf(t, "os inspector: %s absent after reindex #1", ep.Record.ID)
	}
	neoNodes1, neoEdges1 := h.neoNodeCount(t, key), h.neoEdgeCount(t, key, "")
	episodicBytes := readFileRaw(t, h.hotEpisodePath(key))
	knowledgeBytes := readFileRaw(t, h.hotKnowledgePath(key))

	rep2 := h.reindex(t, true)
	if !rep2.Verified || len(rep2.Failures) != 0 {
		failf(t, "reindex #2: want verified with no failures, got verified=%v failures=%v", rep2.Verified, rep2.Failures)
	}
	if rep1.EpisodesIndexed != rep2.EpisodesIndexed || rep1.NodesUpserted != rep2.NodesUpserted || rep1.EdgesUpserted != rep2.EdgesUpserted {
		failf(t, "reindex reports differ between identical runs: #1 episodes=%d nodes=%d edges=%d, #2 episodes=%d nodes=%d edges=%d",
			rep1.EpisodesIndexed, rep1.NodesUpserted, rep1.EdgesUpserted,
			rep2.EpisodesIndexed, rep2.NodesUpserted, rep2.EdgesUpserted)
	}

	h.osRefresh(t)
	if n, _ := h.osProjectCount(t, key); n != osCount1 {
		failf(t, "os inspector: project doc count moved %d -> %d on the second reindex", osCount1, n)
	}
	if src2, f := h.osDoc(t, key, ep.Record.ID); !f || !bytes.Equal(src2, src1) {
		failf(t, "os inspector: _source of %s changed on the second reindex (found=%v)", ep.Record.ID, f)
	}
	if nn, ne := h.neoNodeCount(t, key), h.neoEdgeCount(t, key, ""); nn != neoNodes1 || ne != neoEdges1 {
		failf(t, "neo inspector: shape moved %d/%d -> %d/%d on the second reindex", neoNodes1, neoEdges1, nn, ne)
	}
	if !bytes.Equal(readFileRaw(t, h.hotEpisodePath(key)), episodicBytes) ||
		!bytes.Equal(readFileRaw(t, h.hotKnowledgePath(key)), knowledgeBytes) {
		failf(t, "hot inspector: reindex rewrote canonical hot files — rehydration must be a pure read of hot")
	}
	pass(t, "P3 double reindex: identical reports (episodes=%d nodes=%d edges=%d), stores and hot bytes stable",
		rep2.EpisodesIndexed, rep2.NodesUpserted, rep2.EdgesUpserted)
}

// TestP03_IdempotentAging: aging into the same {yyyy-mm} month is
// download-merge-upload keyed on record id — a re-run with nothing to move
// touches nothing (no new S3 version), and a later record aged into the SAME
// month merges without duplicating or altering the records already archived.
func TestP03_IdempotentAging(t *testing.T) {
	h.ensureServer(t)
	key := e2eKey("p03-age")

	// Fixture: consolidated records ~3 months old (past the 30d TTL), anchored
	// mid-month at day 10 so the +1h/+2h siblings stay inside one {yyyy-mm}
	// batch whatever today's date is.
	raw := time.Now().UTC().AddDate(0, -3, 0)
	base := time.Date(raw.Year(), raw.Month(), 10, 12, 0, 0, 0, time.UTC)
	month := cold.ArchiveMonth(base)
	recA := episode.Record{
		ID: newULIDAt(t, base), Kind: episode.KindEvent, OccurredAt: base,
		Actor: episode.ActorAgent, Text: "p03 aged record A " + p03AgeTokenA,
		Entities: []string{}, Consolidated: true,
	}
	recB := episode.Record{
		ID: newULIDAt(t, base.Add(time.Hour)), Kind: episode.KindObservation, OccurredAt: base.Add(time.Hour),
		Actor: episode.ActorSystem, Text: "p03 aged record B " + p03AgeTokenB,
		Entities: []string{}, Consolidated: true,
	}
	h.writeHotEpisodeDirect(t, key, recA)
	h.writeHotEpisodeDirect(t, key, recB)
	// Converge derived stores after the direct hot writes (harness contract) —
	// which also proves the docs were IN the index before aging deletes them,
	// so the later found=false actually demonstrates the §4 index delete.
	if rep := h.reindex(t, false); len(rep.Failures) != 0 {
		failf(t, "reindex after direct writes: failures %v", rep.Failures)
	}
	if _, f := h.osDoc(t, key, recA.ID); !f {
		failf(t, "os inspector: %s not indexed before aging", recA.ID)
	}
	if _, f := h.osDoc(t, key, recB.ID); !f {
		failf(t, "os inspector: %s not indexed before aging", recB.ID)
	}

	archKey := cold.EpisodeArchiveKey(s3Username, key, month)
	v0 := p03VersionCount(t, archKey) // versions persist across runs under delete markers: deltas only

	// ---- run #1: both records age, in the §4 order ----
	rep1 := h.consolidate(t, httpserver.ConsolidateRequest{Projects: []string{key.String()}})
	if len(rep1.Failures) != 0 || rep1.MovedEpisodes != 2 || !slices.Contains(rep1.ArchiveKeys, archKey) {
		failf(t, "consolidate #1: want 2 moved into %s with no failures, got moved=%d keys=%v failures=%v",
			archKey, rep1.MovedEpisodes, rep1.ArchiveKeys, rep1.Failures)
	}
	wantAB := p03JSON(t, p03SortRecords([]episode.Record{recA, recB}))
	if got := p03JSON(t, p03ArchiveRecords(t, archKey)); got != wantAB {
		failf(t, "s3 inspector: archived batch differs from the planted records\n got: %s\nwant: %s", got, wantAB)
	}
	if left := h.hotEpisodes(t, key); len(left) != 0 {
		failf(t, "hot inspector: %d records still hot after aging (want 0)", len(left))
	}
	if _, f := h.osDoc(t, key, recA.ID); f {
		failf(t, "os inspector: %s still indexed after aging", recA.ID)
	}
	if _, f := h.osDoc(t, key, recB.ID); f {
		failf(t, "os inspector: %s still indexed after aging", recB.ID)
	}
	v1 := p03VersionCount(t, archKey)
	if v1 != v0+1 {
		failf(t, "s3 inspector: want exactly one new version of %s (delta %d -> %d), got %d", archKey, v0, v0+1, v1)
	}
	pass(t, "aging #1: A+B confirmed in s3://%s/%s, hot and index emptied", s3Bucket, archKey)

	// ---- run #2: same request, same month — nothing moves, nothing rewrites ----
	rep2 := h.consolidate(t, httpserver.ConsolidateRequest{Projects: []string{key.String()}})
	if len(rep2.Failures) != 0 || rep2.MovedEpisodes != 0 || len(rep2.ArchiveKeys) != 0 {
		failf(t, "consolidate #2: want a clean no-op, got moved=%d keys=%v failures=%v",
			rep2.MovedEpisodes, rep2.ArchiveKeys, rep2.Failures)
	}
	if n := p03VersionCount(t, archKey); n != v1 {
		failf(t, "s3 inspector: archive version count moved %d -> %d on a no-op re-run — the month object was rewritten", v1, n)
	}
	if got := p03JSON(t, p03ArchiveRecords(t, archKey)); got != wantAB {
		failf(t, "s3 inspector: archive bytes changed on a no-op re-run\n got: %s\nwant: %s", got, wantAB)
	}
	pass(t, "aging #2: no-op — version count and bytes of %s untouched", archKey)

	// ---- run #3: a NEW record in the SAME month merges without duplication ----
	recD := episode.Record{
		ID: newULIDAt(t, base.Add(2*time.Hour)), Kind: episode.KindDecision, OccurredAt: base.Add(2 * time.Hour),
		Actor: episode.ActorAgent, Text: "p03 aged record D " + p03AgeTokenD,
		Entities: []string{}, Consolidated: true,
	}
	h.writeHotEpisodeDirect(t, key, recD)
	if rep := h.reindex(t, false); len(rep.Failures) != 0 {
		failf(t, "reindex after planting D: failures %v", rep.Failures)
	}
	rep3 := h.consolidate(t, httpserver.ConsolidateRequest{Projects: []string{key.String()}})
	if len(rep3.Failures) != 0 || rep3.MovedEpisodes != 1 || !slices.Contains(rep3.ArchiveKeys, archKey) {
		failf(t, "consolidate #3: want exactly D moved into %s, got moved=%d keys=%v failures=%v",
			archKey, rep3.MovedEpisodes, rep3.ArchiveKeys, rep3.Failures)
	}
	wantABD := p03JSON(t, p03SortRecords([]episode.Record{recA, recB, recD}))
	if got := p03JSON(t, p03ArchiveRecords(t, archKey)); got != wantABD {
		failf(t, "s3 inspector: merged batch must be exactly A,B,D with A,B unchanged\n got: %s\nwant: %s", got, wantABD)
	}
	if n := p03VersionCount(t, archKey); n != v1+1 {
		failf(t, "s3 inspector: want exactly one rewrite for the merge (%d -> %d), got %d versions", v1, v1+1, n)
	}
	if left := h.hotEpisodes(t, key); len(left) != 0 {
		failf(t, "hot inspector: %d records still hot after the merge run (want 0)", len(left))
	}
	if _, f := h.osDoc(t, key, recD.ID); f {
		failf(t, "os inspector: %s still indexed after the merge run", recD.ID)
	}
	pass(t, "P3 aging complete: same-month re-age merged D once, duplicated nothing, altered nothing")
}

// p03ArchiveRecords downloads and decodes one monthly archive batch.
func p03ArchiveRecords(t *testing.T, archKey string) []episode.Record {
	t.Helper()
	var recs []episode.Record
	if err := json.Unmarshal(h.s3Cat(t, archKey), &recs); err != nil {
		failf(t, "archive %s is not a JSON record array: %v", archKey, err)
	}
	return recs
}

// p03SortRecords orders records by id — the archive batch contract.
func p03SortRecords(recs []episode.Record) []episode.Record {
	out := slices.Clone(recs)
	slices.SortFunc(out, func(x, y episode.Record) int { return strings.Compare(x.ID, y.ID) })
	return out
}
