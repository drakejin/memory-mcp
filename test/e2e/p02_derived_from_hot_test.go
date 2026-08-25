//go:build e2e

// TestP02_DerivedFromHot proves feature-inventory.md §2 P2: the derived
// stores are pure functions of hot — OpenSearch and Neo4j can be destroyed
// outright and rebuilt from hot JSON with a field-level diff of zero, and the
// ranking material (the recall bump a search wrote BEFORE the wipe, plus a §3
// consolidated promotion) survives because it lives in hot, not in the store
// that was destroyed.
package e2e

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	httpserver "github.com/drakejin/memory-mcp/internal/handler/app/httpserver"
	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/service/knowledge"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// Unique fixture tokens (single unambiguous words, see p00).
const (
	p02SearchToken = "pzerotwosearchtoken"
	p02OtherToken  = "pzerotwoothertoken"
	p02BodyToken   = "pzerotwoknowledgetoken"
)

// p02JSON renders v as its canonical JSON string — the one comparison idiom of
// this scenario. Struct field order is fixed and map keys marshal sorted, so
// equal strings mean equal values, with none of reflect.DeepEqual's time.Time
// wall/monotonic traps.
func p02JSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		failf(t, "marshal %T for comparison: %v", v, err)
	}
	return string(raw)
}

// p02CanonRaw canonicalizes a raw JSON document (an OpenSearch _source) by
// re-marshalling through map[string]any, which sorts keys.
func p02CanonRaw(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		failf(t, "unmarshal raw source for comparison: %v: %.300s", err, raw)
	}
	return p02JSON(t, m)
}

// p02CanonGraph sorts a traversal subgraph: nodes by id, edges by
// (from,to,rel). The subgraph is a set — Neo4j's collect() order is not part
// of the P2 contract, the contents are.
func p02CanonGraph(g knowledge.Graph) knowledge.Graph {
	nodes := slices.Clone(g.Nodes)
	slices.SortFunc(nodes, func(a, b knowledge.Node) int { return strings.Compare(a.ID, b.ID) })
	edges := slices.Clone(g.Edges)
	slices.SortFunc(edges, func(a, b knowledge.Edge) int {
		if c := strings.Compare(a.From, b.From); c != 0 {
			return c
		}
		if c := strings.Compare(a.To, b.To); c != 0 {
			return c
		}
		return strings.Compare(string(a.Rel), string(b.Rel))
	})
	return knowledge.Graph{Nodes: nodes, Edges: edges}
}

// p02CanonNodes sorts a knowledge-search result by node id (fulltext scores
// tie on identical bodies, so response order is not deterministic; content is).
func p02CanonNodes(nodes []knowledge.Node) []knowledge.Node {
	out := slices.Clone(nodes)
	slices.SortFunc(out, func(a, b knowledge.Node) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// p02Traverse fetches GET .../knowledge/graph?entity=&depth= and decodes the
// subgraph. This scenario only calls it with the graph up, so any non-200 is
// a hard failure.
func p02Traverse(t *testing.T, key projectkey.Key, entity string, depth int) knowledge.Graph {
	t.Helper()
	qs := url.Values{"entity": {entity}, "depth": {strconv.Itoa(depth)}}.Encode()
	st, env := h.api(t).getJSON(t, projPath(key)+"/knowledge/graph?"+qs)
	if st != http.StatusOK || !env.Success {
		failf(t, "GET knowledge/graph entity=%s: want 200 success, got HTTP %d error=%q", entity, st, env.Error)
	}
	var g knowledge.Graph
	decodeData(t, env, &g)
	return g
}

// p02KnowledgeSearch fetches GET .../knowledge/search?q= and decodes the nodes.
func p02KnowledgeSearch(t *testing.T, key projectkey.Key, q string) []knowledge.Node {
	t.Helper()
	qs := url.Values{"q": {q}}.Encode()
	st, env := h.api(t).getJSON(t, projPath(key)+"/knowledge/search?"+qs)
	if st != http.StatusOK || !env.Success {
		failf(t, "GET knowledge/search q=%s: want 200 success, got HTTP %d error=%q", q, st, env.Error)
	}
	var nodes []knowledge.Node
	decodeData(t, env, &nodes)
	return nodes
}

func TestP02_DerivedFromHot(t *testing.T) {
	h.ensureServer(t)
	key := e2eKey("p02-derived")

	// ---- seed: three episodes; the two searchable ones differ sharply in
	// length so their BM25 scores — and therefore hit order — cannot tie ----
	ep1 := h.postEpisode(t, key, httpserver.CreateEpisodeRequest{
		Kind: episode.KindEvent, Actor: episode.ActorAgent,
		Text:     "짧은 기록 " + p02SearchToken,
		Entities: []string{"derived"},
	})
	ep2 := h.postEpisode(t, key, httpserver.CreateEpisodeRequest{
		Kind: episode.KindConversation, Actor: episode.ActorUser,
		Text: "훨씬 더 긴 기록으로 같은 토큰을 담는다: 파생 저장소는 언제든 파괴될 수 있고, " +
			"핫 JSON에서 동일하게 재구성되어야 하며, 검색 랭킹 재료조차 핫에 산다는 것이 이 문장의 존재 이유다 " + p02SearchToken,
		Entities: []string{"derived", "rebuild"},
	})
	ep3 := h.postEpisode(t, key, httpserver.CreateEpisodeRequest{
		Kind: episode.KindObservation, Actor: episode.ActorSystem,
		Text:     "다른 토큰의 기록 " + p02OtherToken,
		Entities: []string{"promotion"},
	})

	// Knowledge: hub -[relates_to]-> spoke -[about]-> leaf. The hub names ep3
	// in its provenance, so the §3 promotion flips ep3 consolidated=true in
	// hot — one more hot-resident fact the rebuild must reproduce.
	n1 := h.postNode(t, key, knowledge.CreateNodeInput{
		Kind: knowledge.KindFact, Name: "p02-hub", Body: "허브 사실 " + p02BodyToken,
		Aliases: []string{"p02hubalias"}, Trust: knowledge.TrustAgentInferred,
		Provenance: []string{ep3.Record.ID},
	})
	n2 := h.postNode(t, key, knowledge.CreateNodeInput{
		Kind: knowledge.KindFact, Name: "p02-spoke", Body: "스포크 사실 " + p02BodyToken,
		Trust: knowledge.TrustAgentInferred,
	})
	n3 := h.postNode(t, key, knowledge.CreateNodeInput{
		Kind: knowledge.KindFact, Name: "p02-leaf", Body: "리프 사실 " + p02BodyToken,
		Trust: knowledge.TrustAgentInferred,
	})
	h.postEdge(t, key, knowledge.Edge{From: n1.Node.ID, To: n2.Node.ID, Rel: knowledge.RelRelatesTo, Confidence: 0.9})
	h.postEdge(t, key, knowledge.Edge{From: n2.Node.ID, To: n3.Node.ID, Rel: knowledge.RelAbout, Confidence: 0.7})
	if rec, _ := h.hotEpisodeByID(t, key, ep3.Record.ID); !rec.Consolidated {
		failf(t, "hot inspector: §3 promotion did not mark %s consolidated before the wipe", ep3.Record.ID)
	}

	// ---- recall bump before the wipe: exactly ONE search ----
	// Single-shot on purpose (no waitFor): every hitting search bumps
	// recall_count, so a poll loop would make the arithmetic nondeterministic.
	// Determinism holds without waiting: postEpisode indexes synchronously and
	// osRefresh makes the docs searchable.
	h.osRefresh(t)
	st, hits := h.searchEpisodes(t, key, p02SearchToken)
	if st != http.StatusOK || len(hits) != 2 {
		failf(t, "pre-wipe search: want 200 with exactly 2 hits, got HTTP %d ids=%v", st, hitIDs(hits))
	}
	order := hitIDs(hits)
	if !slices.Contains(order, ep1.Record.ID) || !slices.Contains(order, ep2.Record.ID) {
		failf(t, "pre-wipe search: want hits {%s,%s}, got %v", ep1.Record.ID, ep2.Record.ID, order)
	}
	if hits[0].Score == hits[1].Score {
		failf(t, "pre-wipe search: scores tie (%v) — hit order would be nondeterministic across the rebuild; fixture lengths must differ more", hits[0].Score)
	}
	// The pre-bump index state answered this search: recall_count still 0.
	for _, hit := range hits {
		if hit.Record.RecallCount != 0 || hit.Record.LastRecalled != "" {
			failf(t, "pre-wipe search: hit %s already carries recall material (%d, %q) — a stray search polluted the fixture",
				hit.Record.ID, hit.Record.RecallCount, hit.Record.LastRecalled)
		}
	}
	// The bump itself landed in HOT — the canonical home of ranking material.
	hot1, _ := h.hotEpisodeByID(t, key, ep1.Record.ID)
	hot2, _ := h.hotEpisodeByID(t, key, ep2.Record.ID)
	hot3, _ := h.hotEpisodeByID(t, key, ep3.Record.ID)
	if hot1.RecallCount != 1 || hot2.RecallCount != 1 || hot1.LastRecalled == "" {
		failf(t, "hot inspector: want recall_count=1 on both hits after one search, got %d/%d (last_recalled %q)",
			hot1.RecallCount, hot2.RecallCount, hot1.LastRecalled)
	}
	if hot3.RecallCount != 0 {
		failf(t, "hot inspector: non-hit %s must stay recall_count=0, got %d", ep3.Record.ID, hot3.RecallCount)
	}
	pass(t, "recall bump recorded in hot: %s/%s at count 1, last_recalled %s", ep1.Record.ID, ep2.Record.ID, hot1.LastRecalled)

	// ---- SNAPSHOT: derived-store contents and API results, field for field ----
	ids := []string{ep1.Record.ID, ep2.Record.ID, ep3.Record.ID}
	srcPre := make(map[string]string, len(ids))
	for _, id := range ids {
		raw, found := h.osDoc(t, key, id)
		if !found {
			failf(t, "os inspector: doc %s missing before the wipe — cannot snapshot", id)
		}
		srcPre[id] = p02CanonRaw(t, raw)
	}
	if !strings.Contains(srcPre[ep1.Record.ID], `"recall_count":1`) {
		failf(t, "os snapshot: converged doc %s does not carry the recall bump: %s", ep1.Record.ID, srcPre[ep1.Record.ID])
	}
	nodesPre, edgesAllPre := h.neoNodeCount(t, key), h.neoEdgeCount(t, key, "")
	relPre, aboutPre := h.neoEdgeCount(t, key, string(knowledge.RelRelatesTo)), h.neoEdgeCount(t, key, string(knowledge.RelAbout))
	hubStatePre, _ := h.neoNodeField(t, key, n1.Node.ID, "state")
	hubNamePre, _ := h.neoNodeField(t, key, n1.Node.ID, "name")
	if nodesPre != 3 || edgesAllPre != 2 || relPre != 1 || aboutPre != 1 {
		failf(t, "neo inspector: pre-wipe shape want 3 nodes / 2 edges (1+1 by rel), got %d/%d (%d,%d)",
			nodesPre, edgesAllPre, relPre, aboutPre)
	}
	graphPre := p02JSON(t, p02CanonGraph(p02Traverse(t, key, "p02-hub", 2)))
	ksearchPre := p02JSON(t, p02CanonNodes(p02KnowledgeSearch(t, key, p02BodyToken)))
	episodicBytesPre := readFileRaw(t, h.hotEpisodePath(key))
	knowledgeBytesPre := readFileRaw(t, h.hotKnowledgePath(key))
	pass(t, "snapshot taken: 3 OS docs, neo shape, traversal + knowledge-search results, raw hot bytes")

	// ---- destroy BOTH derived stores (down -v: data gone, not paused) ----
	h.composeFresh(t)
	h.waitFor(t, "opensearch healthy after recreate", healthTimeout, h.opensearchUp)
	h.waitFor(t, "neo4j healthy after recreate", healthTimeout, h.neo4jUp)
	if h.osIndexExists(t) {
		failf(t, "os inspector: index %s survived compose down -v — the destroy did not destroy, diff would be vacuous", osIndex)
	}
	if n := h.neoNodeCount(t, key); n != 0 {
		failf(t, "neo inspector: %d nodes survived compose down -v — the destroy did not destroy", n)
	}
	// The destruction must not have grazed the canonical store.
	if string(readFileRaw(t, h.hotEpisodePath(key))) != string(episodicBytesPre) ||
		string(readFileRaw(t, h.hotKnowledgePath(key))) != string(knowledgeBytesPre) {
		failf(t, "hot inspector: hot bytes changed across the container wipe — hot is not independent of the derived stores")
	}
	pass(t, "derived stores verifiably empty; hot bytes untouched")

	// ---- rebuild everything from hot ----
	rep := h.reindex(t, true)
	if !rep.Verified || len(rep.Failures) != 0 {
		failf(t, "reindex verify after wipe: want verified with no failures, got verified=%v failures=%v", rep.Verified, rep.Failures)
	}

	// ---- store-level diff == 0, recall material included ----
	for _, id := range ids {
		raw, found := h.osDoc(t, key, id)
		if !found {
			failf(t, "os inspector: doc %s absent after rebuild", id)
		}
		if got := p02CanonRaw(t, raw); got != srcPre[id] {
			failf(t, "os inspector: rebuilt _source for %s differs from snapshot\n pre: %s\npost: %s", id, srcPre[id], got)
		}
	}
	nodesPost, edgesAllPost := h.neoNodeCount(t, key), h.neoEdgeCount(t, key, "")
	relPost, aboutPost := h.neoEdgeCount(t, key, string(knowledge.RelRelatesTo)), h.neoEdgeCount(t, key, string(knowledge.RelAbout))
	if nodesPost != nodesPre || edgesAllPost != edgesAllPre || relPost != relPre || aboutPost != aboutPre {
		failf(t, "neo inspector: rebuilt shape %d/%d (%d,%d) != snapshot %d/%d (%d,%d)",
			nodesPost, edgesAllPost, relPost, aboutPost, nodesPre, edgesAllPre, relPre, aboutPre)
	}
	hubStatePost, foundState := h.neoNodeField(t, key, n1.Node.ID, "state")
	hubNamePost, foundName := h.neoNodeField(t, key, n1.Node.ID, "name")
	if !foundState || !foundName || hubStatePost != hubStatePre || hubNamePost != hubNamePre {
		failf(t, "neo inspector: rebuilt hub fields state=%q name=%q != snapshot state=%q name=%q",
			hubStatePost, hubNamePost, hubStatePre, hubNamePre)
	}
	if string(readFileRaw(t, h.hotEpisodePath(key))) != string(episodicBytesPre) ||
		string(readFileRaw(t, h.hotKnowledgePath(key))) != string(knowledgeBytesPre) {
		failf(t, "hot inspector: reindex mutated the canonical files — rebuild must be a pure read of hot")
	}
	pass(t, "store-level diff = 0: OS _source per id, neo counts and fields, hot bytes byte-identical")

	// ---- API-level diff == 0 on the reads that do not self-mutate ----
	if graphPost := p02JSON(t, p02CanonGraph(p02Traverse(t, key, "p02-hub", 2))); graphPost != graphPre {
		failf(t, "traversal diff after rebuild\n pre: %s\npost: %s", graphPre, graphPost)
	}
	if ksearchPost := p02JSON(t, p02CanonNodes(p02KnowledgeSearch(t, key, p02BodyToken))); ksearchPost != ksearchPre {
		failf(t, "knowledge-search diff after rebuild\n pre: %s\npost: %s", ksearchPre, ksearchPost)
	}
	pass(t, "API-level diff = 0: traversal and knowledge search reproduce field for field")

	// ---- the episodic search response after the rebuild ----
	// The response records must now carry the PRE-WIPE bump (count 1 and the
	// exact hot last_recalled): the only store that held those values was
	// destroyed, so they can only have come through hot. Everything else in
	// the hits must match the pre-wipe search modulo that self-advance; score
	// floats are compared for order and positivity, not equality — Lucene term
	// statistics count update-tombstones until segments merge, so bitwise
	// score stability across a rebuild is not a content invariant.
	h.osRefresh(t)
	st2, hits2 := h.searchEpisodes(t, key, p02SearchToken)
	if st2 != http.StatusOK || len(hits2) != 2 {
		failf(t, "post-rebuild search: want 200 with exactly 2 hits, got HTTP %d ids=%v", st2, hitIDs(hits2))
	}
	if got := hitIDs(hits2); !slices.Equal(got, order) {
		failf(t, "post-rebuild search: hit order %v != pre-wipe order %v", got, order)
	}
	for i := range hits2 {
		pre, post := hits[i], hits2[i]
		if post.Record.RecallCount != 1 || post.Record.LastRecalled != hot1.LastRecalled {
			failf(t, "post-rebuild hit %s: want the surviving bump recall_count=1 last_recalled=%q, got %d %q",
				post.Record.ID, hot1.LastRecalled, post.Record.RecallCount, post.Record.LastRecalled)
		}
		if post.Excerpt != pre.Excerpt {
			failf(t, "post-rebuild hit %s: excerpt %q != pre-wipe %q", post.Record.ID, post.Excerpt, pre.Excerpt)
		}
		if post.Score <= 0 {
			failf(t, "post-rebuild hit %s: non-positive score %v", post.Record.ID, post.Score)
		}
		neutral := post.Record
		neutral.RecallCount, neutral.LastRecalled = pre.Record.RecallCount, pre.Record.LastRecalled
		if p02JSON(t, neutral) != p02JSON(t, pre.Record) {
			failf(t, "post-rebuild hit %s: record differs beyond recall fields\n pre: %s\npost: %s",
				post.Record.ID, p02JSON(t, pre.Record), p02JSON(t, neutral))
		}
	}
	pass(t, "P2 complete: derived stores destroyed and rebuilt as f(hot) with diff=0; recall bump survived via hot")
}
