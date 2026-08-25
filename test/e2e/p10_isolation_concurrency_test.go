//go:build e2e

// P10 — isolation & concurrency (feature-inventory.md §2 P10): the
// {ws}/{team}/{proj} key scopes hot files, the shared OpenSearch index and the
// shared Neo4j graph with zero cross-contamination even when two projects hold
// byte-identical content written in parallel; and N concurrent appends into
// one project lose nothing — counted in the hot file, the manifest and the
// index, not just in HTTP responses.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	httpserver "github.com/drakejin/memory-mcp/internal/handler/app/httpserver"
	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/service/knowledge"
	"github.com/drakejin/memory-mcp/internal/service/rehydrate"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

const (
	// p10IsoToken / p10ConcToken mark this file's corpora so index-wide counts
	// are exact even on the shared index. Digit-free fused nonwords on
	// purpose: nori splits letter/digit boundaries ("p10x" → p + 10 + x) and
	// the resulting subtokens would OR-match other fixtures' records,
	// inflating the exact unfiltered totals below.
	p10IsoToken  = "tenisolation"
	p10ConcToken = "tenconcurrent"
	// p10IsoWrites is the per-project size of the identical-content corpus.
	p10IsoWrites = 5
	// p10ConcWriters is the concurrent-append fan-out.
	p10ConcWriters = 30
	// p10SharedNodeName is the knowledge node name deliberately reused across
	// both projects.
	p10SharedNodeName = "p10-shared-fact"
)

// p10IndexWait bounds the deterministic wait for index convergence.
const p10IndexWait = 30 * time.Second

// p10EdgeConfidence is the confidence of the single A-side edge.
const p10EdgeConfidence = 0.8

// p10PostEpisode is a goroutine-safe episode POST: it returns errors instead
// of failing the test, because t.Fatalf is only legal on the test goroutine.
// Assertions on the outcome happen back on the test goroutine.
func p10PostEpisode(s *server, key projectkey.Key, text string) (episode.AppendResult, error) {
	body, err := json.Marshal(httpserver.CreateEpisodeRequest{
		Kind:     episode.KindEvent,
		Actor:    episode.ActorAgent,
		Text:     text,
		Entities: []string{},
	})
	if err != nil {
		return episode.AppendResult{}, err
	}
	req, err := http.NewRequest(http.MethodPost, s.baseURL+projPath(key)+"/episodes", bytes.NewReader(body))
	if err != nil {
		return episode.AppendResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return episode.AppendResult{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return episode.AppendResult{}, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return episode.AppendResult{}, fmt.Errorf("HTTP %d is not the envelope: %.200s", resp.StatusCode, raw)
	}
	if resp.StatusCode != http.StatusCreated || !env.Success {
		return episode.AppendResult{}, fmt.Errorf("want 201 success, got HTTP %d error=%q", resp.StatusCode, env.Error)
	}
	var res episode.AppendResult
	if err := json.Unmarshal(env.Data, &res); err != nil {
		return episode.AppendResult{}, fmt.Errorf("decode AppendResult: %w", err)
	}
	if res.Record.ID == "" {
		return episode.AppendResult{}, fmt.Errorf("no id assigned: %.200s", env.Data)
	}
	return res, nil
}

// p10CollectIDs asserts every parallel write succeeded non-degraded and
// returns id→text. Duplicate ids fail: ids are the isolation anchor.
func p10CollectIDs(t *testing.T, who string, texts []string, results []episode.AppendResult, errs []error) map[string]string {
	t.Helper()
	ids := make(map[string]string, len(results))
	for i, err := range errs {
		if err != nil {
			failf(t, "%s writer %d: %v", who, i, err)
		}
		res := results[i]
		if len(res.Degraded) != 0 {
			failf(t, "%s writer %d: degraded notes with every store up: %v", who, i, res.Degraded)
		}
		if _, dup := ids[res.Record.ID]; dup {
			failf(t, "%s: duplicate episode id %s across concurrent appends", who, res.Record.ID)
		}
		ids[res.Record.ID] = texts[i]
	}
	return ids
}

// p10AssertHotExactly proves one project's hot file holds exactly the given
// id→text set — the zero-loss / zero-bleed check at the canonical store.
func p10AssertHotExactly(t *testing.T, key projectkey.Key, want map[string]string) {
	t.Helper()
	recs := h.hotEpisodes(t, key)
	if len(recs) != len(want) {
		failf(t, "hot inspector: %s holds %d records, want %d", h.hotEpisodePath(key), len(recs), len(want))
	}
	seen := make(map[string]bool, len(recs))
	for _, rec := range recs {
		wantText, ok := want[rec.ID]
		if !ok {
			failf(t, "hot inspector: %s holds foreign record %s (%q)", h.hotEpisodePath(key), rec.ID, rec.Text)
		}
		if rec.Text != wantText {
			failf(t, "hot inspector: record %s text %q != written %q", rec.ID, rec.Text, wantText)
		}
		if seen[rec.ID] {
			failf(t, "hot inspector: record %s duplicated in %s", rec.ID, h.hotEpisodePath(key))
		}
		seen[rec.ID] = true
	}
}

// TestP10_TwoProjectsSameContentNoCrossContamination writes byte-identical
// corpora into two projects in parallel, then proves zero bleed in all four
// stores: hot files, manifest, OpenSearch scope filters, and the Neo4j graph
// (same-name knowledge nodes included). Provenance promotion — a hot mutation
// — must also stay inside its own project.
func TestP10_TwoProjectsSameContentNoCrossContamination(t *testing.T) {
	h.ensureServer(t)
	s := h.api(t)
	keyA := e2eKey("p10-iso-a")
	keyB := e2eKey("p10-iso-b")

	texts := make([]string, p10IsoWrites)
	for i := range texts {
		texts[i] = fmt.Sprintf("%s 동일 문장 %02d", p10IsoToken, i)
	}

	// Identical content into both projects, all writes in flight together.
	var wg sync.WaitGroup
	resA := make([]episode.AppendResult, p10IsoWrites)
	resB := make([]episode.AppendResult, p10IsoWrites)
	errA := make([]error, p10IsoWrites)
	errB := make([]error, p10IsoWrites)
	for i := range texts {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			resA[i], errA[i] = p10PostEpisode(s, keyA, texts[i])
		}(i)
		go func(i int) {
			defer wg.Done()
			resB[i], errB[i] = p10PostEpisode(s, keyB, texts[i])
		}(i)
	}
	wg.Wait()
	idsA := p10CollectIDs(t, "project A", texts, resA, errA)
	idsB := p10CollectIDs(t, "project B", texts, resB, errB)
	for id := range idsA {
		if _, shared := idsB[id]; shared {
			failf(t, "id %s was minted for both projects", id)
		}
	}

	// Hot: each file holds exactly its own five, none of the other's.
	p10AssertHotExactly(t, keyA, idsA)
	p10AssertHotExactly(t, keyB, idsB)
	man := h.hotManifest(t)
	for _, key := range []projectkey.Key{keyA, keyB} {
		fileKey := rehydrate.ManifestFileKey(rehydrate.PlaneEpisodic, key)
		if fs, ok := man.Files[fileKey]; !ok || fs.RecordCount != p10IsoWrites {
			failf(t, "manifest inspector: Files[%q].RecordCount = %+v (present=%v), want %d", fileKey, fs, ok, p10IsoWrites)
		}
	}
	pass(t, "hot inspector: %d+%d parallel same-content writes landed in exactly their own files", p10IsoWrites, p10IsoWrites)

	// OpenSearch: both corpora are fully indexed; the scope filter is the only
	// separator between them.
	h.osRefresh(t)
	for _, key := range []projectkey.Key{keyA, keyB} {
		if n, ok := h.osProjectCount(t, key); !ok || n != p10IsoWrites {
			failf(t, "os inspector: %s project count = %d (index present=%v), want %d", key, n, ok, p10IsoWrites)
		}
	}
	unf, found := h.osSearchRaw(t, fmt.Sprintf(`{"size":50,"query":{"match":{"text":%q}}}`, p10IsoToken))
	if !found || unf.Total != 2*p10IsoWrites {
		failf(t, "os inspector: unfiltered %q total = %d (found=%v), want %d", p10IsoToken, unf.Total, found, 2*p10IsoWrites)
	}
	for _, tc := range []struct {
		key projectkey.Key
		own map[string]string
	}{{keyA, idsA}, {keyB, idsB}} {
		res, found := h.osSearchRaw(t, fmt.Sprintf(
			`{"size":50,"query":{"bool":{"must":[{"match":{"text":%q}}],"filter":[{"term":{"workspace":%q}},{"term":{"team":%q}},{"term":{"project":%q}}]}}}`,
			p10IsoToken, tc.key.Workspace, tc.key.Team, tc.key.Project))
		if !found || res.Total != p10IsoWrites {
			failf(t, "os inspector: %s-scoped raw search total = %d (found=%v), want %d", tc.key, res.Total, found, p10IsoWrites)
		}
		prefix := tc.key.String() + "#"
		for _, hit := range res.Hits {
			if len(hit.ID) <= len(prefix) || hit.ID[:len(prefix)] != prefix {
				failf(t, "os inspector: %s-scoped search returned foreign _id %s", tc.key, hit.ID)
			}
			if _, own := tc.own[hit.ID[len(prefix):]]; !own {
				failf(t, "os inspector: %s-scoped search returned record %s not written to it", tc.key, hit.ID)
			}
		}
	}
	pass(t, "os inspector: %d docs total, scope filters split them exactly %d/%d", 2*p10IsoWrites, p10IsoWrites, p10IsoWrites)

	// Server-path search: each project sees only its own records.
	for _, tc := range []struct {
		key   projectkey.Key
		own   map[string]string
		other map[string]string
	}{{keyA, idsA, idsB}, {keyB, idsB, idsA}} {
		h.waitFor(t, fmt.Sprintf("server search %s sees exactly its %d records", tc.key, p10IsoWrites), p10IndexWait, func() (bool, string) {
			status, hits := h.searchEpisodes(t, tc.key, p10IsoToken)
			if status != http.StatusOK {
				return false, fmt.Sprintf("HTTP %d", status)
			}
			for _, hit := range hits {
				if _, leaked := tc.other[hit.Record.ID]; leaked {
					failf(t, "server search %s returned the OTHER project's record %s — cross-project contamination", tc.key, hit.Record.ID)
				}
				if _, own := tc.own[hit.Record.ID]; !own {
					failf(t, "server search %s returned unknown record %s", tc.key, hit.Record.ID)
				}
			}
			return len(hits) == p10IsoWrites, fmt.Sprintf("%d hits ids=%v", len(hits), hitIDs(hits))
		})
	}

	// Knowledge: the same node name in both scopes, an edge only in A.
	var firstA, firstB string
	for _, r := range resA {
		if firstA == "" || r.Record.ID < firstA {
			firstA = r.Record.ID
		}
	}
	for _, r := range resB {
		if firstB == "" || r.Record.ID < firstB {
			firstB = r.Record.ID
		}
	}
	nodeA := h.postNode(t, keyA, knowledge.CreateNodeInput{
		Kind: knowledge.KindFact, Name: p10SharedNodeName, Body: "p10 동일 이름 지식",
		Trust: knowledge.TrustAgentInferred, Provenance: []string{firstA},
	})
	neighborA := h.postNode(t, keyA, knowledge.CreateNodeInput{
		Kind: knowledge.KindFact, Name: "p10-neighbor-fact", Body: "A 전용 이웃",
		Trust: knowledge.TrustAgentInferred, Provenance: []string{firstA},
	})
	h.postEdge(t, keyA, knowledge.Edge{
		From: neighborA.Node.ID, To: nodeA.Node.ID, Rel: knowledge.RelRelatesTo,
		Provenance: []string{firstA}, Confidence: p10EdgeConfidence,
	})

	// Provenance promotion is a hot mutation and must stay inside A: firstA is
	// now consolidated, every B record — same content, untouched project — is
	// still false. Asserted BEFORE B gets any node of its own.
	if rec, ok := h.hotEpisodeByID(t, keyA, firstA); !ok || !rec.Consolidated {
		failf(t, "hot inspector: A's provenance episode %s not consolidated after node creation (present=%v)", firstA, ok)
	}
	for _, rec := range h.hotEpisodes(t, keyB) {
		if rec.Consolidated {
			failf(t, "hot inspector: B's record %s got consolidated by A's promotion — cross-project mutation", rec.ID)
		}
	}

	nodeB := h.postNode(t, keyB, knowledge.CreateNodeInput{
		Kind: knowledge.KindFact, Name: p10SharedNodeName, Body: "p10 동일 이름 지식",
		Trust: knowledge.TrustAgentInferred, Provenance: []string{firstB},
	})
	if nodeB.Node.ID == nodeA.Node.ID {
		failf(t, "same-name nodes in two projects share id %s", nodeA.Node.ID)
	}
	if rec, ok := h.hotEpisodeByID(t, keyB, firstB); !ok || !rec.Consolidated {
		failf(t, "hot inspector: B's own promotion did not consolidate %s (present=%v)", firstB, ok)
	}

	// Hot knowledge documents: A has exactly its 2 nodes + 1 edge, B its 1 node.
	gA := h.hotKnowledge(t, keyA)
	if len(gA.Nodes) != 2 || len(gA.Edges) != 1 {
		failf(t, "hot inspector: A knowledge doc: want 2 nodes / 1 edge, got %d / %d", len(gA.Nodes), len(gA.Edges))
	}
	for _, n := range gA.Nodes {
		if n.ID == nodeB.Node.ID {
			failf(t, "hot inspector: A's knowledge doc contains B's node %s", nodeB.Node.ID)
		}
	}
	gB := h.hotKnowledge(t, keyB)
	if len(gB.Nodes) != 1 || len(gB.Edges) != 0 || gB.Nodes[0].ID != nodeB.Node.ID {
		failf(t, "hot inspector: B knowledge doc: want exactly [%s] and no edges, got %d nodes / %d edges", nodeB.Node.ID, len(gB.Nodes), len(gB.Edges))
	}

	// Neo4j: scope properties keep the same-name nodes apart.
	if got := h.neoNodeCount(t, keyA); got != 2 {
		failf(t, "neo inspector: A node count = %d, want 2", got)
	}
	if got := h.neoNodeCount(t, keyB); got != 1 {
		failf(t, "neo inspector: B node count = %d, want 1", got)
	}
	if name, found := h.neoNodeField(t, keyA, nodeA.Node.ID, "name"); !found || name != p10SharedNodeName {
		failf(t, "neo inspector: A's node %s name = %q (found=%v), want %q", nodeA.Node.ID, name, found, p10SharedNodeName)
	}
	if name, found := h.neoNodeField(t, keyB, nodeB.Node.ID, "name"); !found || name != p10SharedNodeName {
		failf(t, "neo inspector: B's node %s name = %q (found=%v), want %q", nodeB.Node.ID, name, found, p10SharedNodeName)
	}
	if _, found := h.neoNodeField(t, keyB, nodeA.Node.ID, "name"); found {
		failf(t, "neo inspector: A's node %s is visible in B's scope", nodeA.Node.ID)
	}
	if _, found := h.neoNodeField(t, keyA, nodeB.Node.ID, "name"); found {
		failf(t, "neo inspector: B's node %s is visible in A's scope", nodeB.Node.ID)
	}
	if got := h.neoEdgeCount(t, keyA, ""); got != 1 {
		failf(t, "neo inspector: A edge count = %d, want 1", got)
	}
	if got := h.neoEdgeCount(t, keyB, ""); got != 0 {
		failf(t, "neo inspector: B edge count = %d, want 0 — the A-side edge leaked", got)
	}
	pass(t, "isolation: identical content in two projects, zero cross-hits in hot, manifest, OpenSearch and Neo4j")
}

// TestP10_ConcurrentAppendsZeroLoss fans out 30 concurrent appends into one
// project and counts zero loss where it matters: the hot file (exact id→text
// set), the manifest record count, and the OpenSearch project count after a
// forced refresh.
func TestP10_ConcurrentAppendsZeroLoss(t *testing.T) {
	h.ensureServer(t)
	s := h.api(t)
	key := e2eKey("p10-conc")

	texts := make([]string, p10ConcWriters)
	for i := range texts {
		texts[i] = fmt.Sprintf("%s writer %02d", p10ConcToken, i)
	}
	results := make([]episode.AppendResult, p10ConcWriters)
	errs := make([]error, p10ConcWriters)
	var wg sync.WaitGroup
	for i := range texts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = p10PostEpisode(s, key, texts[i])
		}(i)
	}
	wg.Wait()
	ids := p10CollectIDs(t, "p10-conc", texts, results, errs)
	if len(ids) != p10ConcWriters {
		failf(t, "want %d unique ids, got %d", p10ConcWriters, len(ids))
	}
	pass(t, "%d concurrent appends all answered 201 non-degraded with unique ids", p10ConcWriters)

	// Hot: zero loss, byte-for-byte texts, no duplicates.
	p10AssertHotExactly(t, key, ids)
	man := h.hotManifest(t)
	fileKey := rehydrate.ManifestFileKey(rehydrate.PlaneEpisodic, key)
	if fs, ok := man.Files[fileKey]; !ok || fs.RecordCount != p10ConcWriters {
		failf(t, "manifest inspector: Files[%q].RecordCount = %+v (present=%v), want %d", fileKey, fs, ok, p10ConcWriters)
	}
	pass(t, "hot inspector: file and manifest both count exactly %d records", p10ConcWriters)

	// Index: every append reported non-degraded, so after a forced refresh the
	// project count must reach exactly the fan-out.
	h.waitFor(t, fmt.Sprintf("opensearch project count reaches %d", p10ConcWriters), p10IndexWait, func() (bool, string) {
		h.osRefresh(t)
		n, ok := h.osProjectCount(t, key)
		return ok && n == p10ConcWriters, fmt.Sprintf("count=%d indexPresent=%v", n, ok)
	})
	// Realtime doc spot-checks on the first and last writers' records.
	for _, i := range []int{0, p10ConcWriters - 1} {
		if _, found := h.osDoc(t, key, results[i].Record.ID); !found {
			failf(t, "os inspector: writer %d's record %s missing from the index", i, results[i].Record.ID)
		}
	}
	pass(t, "os inspector: zero loss — all %d records present in the index", p10ConcWriters)
}
