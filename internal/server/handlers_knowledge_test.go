package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/graph"
	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/knowledge"
	"github.com/drakejin/memory-mcp/internal/rehydrate"
	"github.com/drakejin/memory-mcp/internal/ulid"
)

const (
	nodesPath = "/v1/ws/team/proj/knowledge/nodes"
	edgesPath = "/v1/ws/team/proj/knowledge/edges"
)

// seedNode returns a stored active node for graph fixtures.
func seedNode(id, name string) knowledge.Node {
	return knowledge.Node{
		ID: id, Kind: knowledge.KindFact, Name: name, Body: "b",
		Aliases: []string{}, State: knowledge.StateActive, Trust: knowledge.TrustUserStated,
		Supersedes: []string{}, Provenance: []string{},
		Created: fixedNow.Add(-time.Hour), Updated: fixedNow.Add(-time.Hour),
	}
}

func TestCreateNodeValidation(t *testing.T) {
	valid := CreateNodeRequest{Kind: knowledge.KindFact, Name: "n", Trust: knowledge.TrustUserStated}

	tests := []struct {
		name   string
		body   any
		status int
	}{
		{"valid", valid, http.StatusCreated},
		{"unknown kind", CreateNodeRequest{Kind: "myth", Name: "n", Trust: knowledge.TrustUserStated}, http.StatusBadRequest},
		{"empty name", CreateNodeRequest{Kind: knowledge.KindFact, Trust: knowledge.TrustUserStated}, http.StatusBadRequest},
		{"unknown trust", CreateNodeRequest{Kind: knowledge.KindFact, Name: "n", Trust: "vibes"}, http.StatusBadRequest},
		{
			name:   "malformed provenance",
			body:   CreateNodeRequest{Kind: knowledge.KindFact, Name: "n", Trust: knowledge.TrustUserStated, Provenance: []string{"nope"}},
			status: http.StatusBadRequest,
		},
		{
			name:   "malformed supersedes",
			body:   CreateNodeRequest{Kind: knowledge.KindFact, Name: "n", Trust: knowledge.TrustUserStated, Supersedes: []string{"nope"}},
			status: http.StatusBadRequest,
		},
		{
			name:   "bad review_after",
			body:   CreateNodeRequest{Kind: knowledge.KindFact, Name: "n", Trust: knowledge.TrustUserStated, ReviewAfter: "someday"},
			status: http.StatusBadRequest,
		},
		{
			name:   "rfc3339 review_after",
			body:   CreateNodeRequest{Kind: knowledge.KindFact, Name: "n", Trust: knowledge.TrustUserStated, ReviewAfter: "2027-01-01T00:00:00Z"},
			status: http.StatusCreated,
		},
		{"empty body", "", http.StatusBadRequest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, h := newTestServer(t, nil)
			rec := do(t, h, http.MethodPost, nodesPath, tc.body)
			assertStatus(t, rec, tc.status)
		})
	}
}

func TestCreateNodeWritesHotAndMirrors(t *testing.T) {
	store := newFakeStore()
	gr := newFakeGraph()
	_, h := newTestServer(t, func(d *Deps) {
		d.Store = store
		d.Graph = gr
	})

	rec := do(t, h, http.MethodPost, nodesPath, CreateNodeRequest{
		Kind: knowledge.KindLesson, Name: "보안 끄기", Body: "single-node dev only",
		Trust: knowledge.TrustUserStated, Aliases: []string{" alias ", ""}, Provenance: []string{ulidA},
	})
	assertStatus(t, rec, http.StatusCreated)

	var got NodeResponse
	decodeEnvelope(t, rec, &got)
	if !ulid.IsULID(got.Node.ID) {
		t.Errorf("id = %q, want ULID", got.Node.ID)
	}
	if got.Node.State != knowledge.StateActive {
		t.Errorf("state = %q, want active", got.Node.State)
	}
	if len(got.Node.Aliases) != 1 || got.Node.Aliases[0] != "alias" {
		t.Errorf("aliases = %v, want [alias]", got.Node.Aliases)
	}
	if len(got.Degraded) != 0 {
		t.Errorf("degraded = %v, want none", got.Degraded)
	}
	if len(store.graphs[testKey.String()].Nodes) != 1 {
		t.Fatal("hot graph must hold the node")
	}
	if len(gr.nodes[testKey.String()]) != 1 {
		t.Fatal("neo4j mirror must receive the node")
	}
}

func TestCreateNodeSupersedeArchivesPredecessor(t *testing.T) {
	store := newFakeStore()
	store.graphs[testKey.String()] = knowledge.Graph{Nodes: []knowledge.Node{seedNode(ulidA, "old")}}
	gr := newFakeGraph()
	_, h := newTestServer(t, func(d *Deps) {
		d.Store = store
		d.Graph = gr
	})

	rec := do(t, h, http.MethodPost, nodesPath, CreateNodeRequest{
		Kind: knowledge.KindFact, Name: "new", Trust: knowledge.TrustAgentInferred, Supersedes: []string{ulidA},
	})
	assertStatus(t, rec, http.StatusCreated)

	var got NodeResponse
	decodeEnvelope(t, rec, &got)
	if len(got.Node.Supersedes) != 1 || got.Node.Supersedes[0] != ulidA {
		t.Fatalf("supersedes = %v, want [%s]", got.Node.Supersedes, ulidA)
	}

	stored := store.graphs[testKey.String()]
	var old knowledge.Node
	for _, n := range stored.Nodes {
		if n.ID == ulidA {
			old = n
		}
	}
	if old.State != knowledge.StateArchived {
		t.Errorf("superseded node state = %q, want archived", old.State)
	}
	if old.SupersededBy != got.Node.ID {
		t.Errorf("superseded_by = %q, want %q", old.SupersededBy, got.Node.ID)
	}
	// Both the new node and the archived predecessor must reach the mirror, plus
	// the supersedes edge the state machine added.
	if len(gr.nodes[testKey.String()]) != 2 {
		t.Errorf("mirrored nodes = %d, want 2", len(gr.nodes[testKey.String()]))
	}
	if len(gr.edges[testKey.String()]) != 1 {
		t.Errorf("mirrored edges = %d, want 1 supersedes edge", len(gr.edges[testKey.String()]))
	}
}

func TestCreateNodeSupersedeMissingTargetIs404(t *testing.T) {
	_, h := newTestServer(t, nil)
	rec := do(t, h, http.MethodPost, nodesPath, CreateNodeRequest{
		Kind: knowledge.KindFact, Name: "new", Trust: knowledge.TrustUserStated, Supersedes: []string{ulidA},
	})
	assertStatus(t, rec, http.StatusNotFound)
}

func TestCreateNodeSupersedeInternalErrorIs500(t *testing.T) {
	orig := supersedeGraph
	supersedeGraph = func(knowledge.Graph, knowledge.Node, []string, time.Time) (knowledge.Graph, error) {
		return knowledge.Graph{}, errBoom
	}
	t.Cleanup(func() { supersedeGraph = orig })

	store := newFakeStore()
	store.graphs[testKey.String()] = knowledge.Graph{Nodes: []knowledge.Node{seedNode(ulidA, "old")}}
	_, h := newTestServer(t, func(d *Deps) { d.Store = store })
	rec := do(t, h, http.MethodPost, nodesPath, CreateNodeRequest{
		Kind: knowledge.KindFact, Name: "new", Trust: knowledge.TrustUserStated, Supersedes: []string{ulidA},
	})
	assertStatus(t, rec, http.StatusInternalServerError)
}

func TestCreateNodeDegradedWhenGraphDown(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Deps)
	}{
		{"graph not configured", func(d *Deps) { d.Graph = nil }},
		{"upsert fails", func(d *Deps) {
			gr := newFakeGraph()
			gr.upsertNodeErr = graph.ErrUnavailable
			d.Graph = gr
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			_, h := newTestServer(t, func(d *Deps) {
				d.Store = store
				tc.mutate(d)
			})
			rec := do(t, h, http.MethodPost, nodesPath, CreateNodeRequest{
				Kind: knowledge.KindFact, Name: "n", Trust: knowledge.TrustUserStated,
			})
			assertStatus(t, rec, http.StatusCreated)

			var got NodeResponse
			decodeEnvelope(t, rec, &got)
			if len(got.Degraded) != 1 || got.Degraded[0] != degradedGraph {
				t.Fatalf("degraded = %v, want [%s]", got.Degraded, degradedGraph)
			}
			if len(store.graphs[testKey.String()].Nodes) != 1 {
				t.Fatal("hot write must succeed while the graph is down (§5)")
			}
			wantDirty := rehydrate.FileKey(hotstore.PlaneKnowledge, testKey)
			if len(store.dirtyMarks) != 1 || store.dirtyMarks[0] != wantDirty {
				t.Fatalf("dirty marks = %v, want [%s]", store.dirtyMarks, wantDirty)
			}
		})
	}
}

func TestCreateNodeStoreFailuresSurface(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*fakeStore)
		status int
	}{
		{"read fails", func(s *fakeStore) { s.readErr = errBoom }, http.StatusInternalServerError},
		{"write fails", func(s *fakeStore) { s.writeErr = errBoom }, http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			tc.mutate(store)
			_, h := newTestServer(t, func(d *Deps) { d.Store = store })
			rec := do(t, h, http.MethodPost, nodesPath, CreateNodeRequest{
				Kind: knowledge.KindFact, Name: "n", Trust: knowledge.TrustUserStated,
			})
			assertStatus(t, rec, tc.status)
		})
	}
}

func TestCreateEdgeValidation(t *testing.T) {
	tests := []struct {
		name   string
		edge   any
		status int
	}{
		{
			name:   "valid",
			edge:   knowledge.Edge{From: ulidA, To: ulidB, Rel: knowledge.RelRelatesTo, Confidence: 0.9},
			status: http.StatusCreated,
		},
		{
			name:   "non-ulid endpoint",
			edge:   knowledge.Edge{From: "x", To: ulidB, Rel: knowledge.RelRelatesTo},
			status: http.StatusBadRequest,
		},
		{
			name:   "unknown rel",
			edge:   knowledge.Edge{From: ulidA, To: ulidB, Rel: "loves"},
			status: http.StatusBadRequest,
		},
		{
			name:   "confidence above range",
			edge:   knowledge.Edge{From: ulidA, To: ulidB, Rel: knowledge.RelAbout, Confidence: 1.5},
			status: http.StatusBadRequest,
		},
		{
			name:   "confidence below range",
			edge:   knowledge.Edge{From: ulidA, To: ulidB, Rel: knowledge.RelAbout, Confidence: -0.1},
			status: http.StatusBadRequest,
		},
		{
			name:   "malformed provenance",
			edge:   knowledge.Edge{From: ulidA, To: ulidB, Rel: knowledge.RelAbout, Provenance: []string{"nope"}},
			status: http.StatusBadRequest,
		},
		{
			name:   "missing endpoint node",
			edge:   knowledge.Edge{From: ulidA, To: ulidC, Rel: knowledge.RelRelatesTo},
			status: http.StatusNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			store.graphs[testKey.String()] = knowledge.Graph{
				Nodes: []knowledge.Node{seedNode(ulidA, "a"), seedNode(ulidB, "b")},
			}
			_, h := newTestServer(t, func(d *Deps) { d.Store = store })
			rec := do(t, h, http.MethodPost, edgesPath, tc.edge)
			assertStatus(t, rec, tc.status)
		})
	}
}

func TestCreateEdgeIsIdempotentOnFromToRel(t *testing.T) {
	store := newFakeStore()
	store.graphs[testKey.String()] = knowledge.Graph{
		Nodes: []knowledge.Node{seedNode(ulidA, "a"), seedNode(ulidB, "b")},
	}
	_, h := newTestServer(t, func(d *Deps) { d.Store = store })

	edge := knowledge.Edge{From: ulidA, To: ulidB, Rel: knowledge.RelRelatesTo, Confidence: 0.5}
	assertStatus(t, do(t, h, http.MethodPost, edgesPath, edge), http.StatusCreated)
	edge.Confidence = 0.95
	assertStatus(t, do(t, h, http.MethodPost, edgesPath, edge), http.StatusCreated)

	edges := store.graphs[testKey.String()].Edges
	if len(edges) != 1 {
		t.Fatalf("hot edges = %d, want 1 (re-post replaces)", len(edges))
	}
	if edges[0].Confidence != 0.95 {
		t.Fatalf("confidence = %v, want the replacing value 0.95", edges[0].Confidence)
	}
}

func TestCreateEdgeDegradedWhenGraphDown(t *testing.T) {
	store := newFakeStore()
	store.graphs[testKey.String()] = knowledge.Graph{
		Nodes: []knowledge.Node{seedNode(ulidA, "a"), seedNode(ulidB, "b")},
	}
	gr := newFakeGraph()
	gr.upsertEdgeErr = graph.ErrUnavailable
	_, h := newTestServer(t, func(d *Deps) {
		d.Store = store
		d.Graph = gr
	})

	rec := do(t, h, http.MethodPost, edgesPath, knowledge.Edge{From: ulidA, To: ulidB, Rel: knowledge.RelAbout})
	assertStatus(t, rec, http.StatusCreated)
	var got EdgeResponse
	decodeEnvelope(t, rec, &got)
	if len(got.Degraded) != 1 || got.Degraded[0] != degradedGraph {
		t.Fatalf("degraded = %v, want [%s]", got.Degraded, degradedGraph)
	}
	if len(store.graphs[testKey.String()].Edges) != 1 {
		t.Fatal("hot edge write must survive a dead graph")
	}
}

func TestSearchKnowledge(t *testing.T) {
	tests := []struct {
		name   string
		target string
		mutate func(*Deps)
		status int
	}{
		{"missing q", "/v1/ws/team/proj/knowledge/search", nil, http.StatusBadRequest},
		{"bad project", "/v1/WS/team/proj/knowledge/search?q=a", nil, http.StatusBadRequest},
		{"ok", "/v1/ws/team/proj/knowledge/search?q=a", nil, http.StatusOK},
		{"opt-in archived", "/v1/ws/team/proj/knowledge/search?q=a&include_archived=true", nil, http.StatusOK},
		{"graph nil", "/v1/ws/team/proj/knowledge/search?q=a", func(d *Deps) { d.Graph = nil }, http.StatusServiceUnavailable},
		{
			name:   "graph unreachable",
			target: "/v1/ws/team/proj/knowledge/search?q=a",
			mutate: func(d *Deps) {
				gr := newFakeGraph()
				gr.searchErr = graph.ErrUnavailable
				d.Graph = gr
			},
			status: http.StatusServiceUnavailable,
		},
		{
			name:   "graph error",
			target: "/v1/ws/team/proj/knowledge/search?q=a",
			mutate: func(d *Deps) {
				gr := newFakeGraph()
				gr.searchErr = errBoom
				d.Graph = gr
			},
			status: http.StatusInternalServerError,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, h := newTestServer(t, tc.mutate)
			rec := do(t, h, http.MethodGet, tc.target, nil)
			assertStatus(t, rec, tc.status)
		})
	}
}

func TestSearchKnowledgeEmptyIsArrayNotNull(t *testing.T) {
	_, h := newTestServer(t, nil)
	rec := do(t, h, http.MethodGet, "/v1/ws/team/proj/knowledge/search?q=a", nil)
	assertStatus(t, rec, http.StatusOK)
	env := decodeEnvelope(t, rec, nil)
	if env.Data == nil {
		t.Fatal("data must be [] not null")
	}
}

func TestKnowledgeGraphDepthValidation(t *testing.T) {
	tests := []struct {
		name   string
		target string
		status int
	}{
		{"missing entity", "/v1/ws/team/proj/knowledge/graph", http.StatusBadRequest},
		{"default depth", "/v1/ws/team/proj/knowledge/graph?entity=memory-mcp", http.StatusOK},
		{"explicit depth", "/v1/ws/team/proj/knowledge/graph?entity=x&depth=3", http.StatusOK},
		{"depth zero", "/v1/ws/team/proj/knowledge/graph?entity=x&depth=0", http.StatusBadRequest},
		{"depth too deep", "/v1/ws/team/proj/knowledge/graph?entity=x&depth=11", http.StatusBadRequest},
		{"depth not a number", "/v1/ws/team/proj/knowledge/graph?entity=x&depth=deep", http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, h := newTestServer(t, nil)
			rec := do(t, h, http.MethodGet, tc.target, nil)
			assertStatus(t, rec, tc.status)
		})
	}
}

func TestKnowledgeGraphRunsStatGate(t *testing.T) {
	reh := &fakeRehydrator{}
	gr := newFakeGraph()
	gr.sub = knowledge.Graph{Nodes: []knowledge.Node{seedNode(ulidA, "center")}}
	_, h := newTestServer(t, func(d *Deps) {
		d.Rehydrator = reh
		d.Graph = gr
	})
	rec := do(t, h, http.MethodGet, "/v1/ws/team/proj/knowledge/graph?entity=center", nil)
	assertStatus(t, rec, http.StatusOK)
	if len(reh.gateCalls) != 1 {
		t.Fatalf("stat-gate calls = %v, want exactly one", reh.gateCalls)
	}
	var got knowledge.Graph
	decodeEnvelope(t, rec, &got)
	if len(got.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(got.Nodes))
	}
}

func TestKnowledgeGraphUnavailable(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Deps)
		status int
	}{
		{"graph nil", func(d *Deps) { d.Graph = nil }, http.StatusServiceUnavailable},
		{"unreachable", func(d *Deps) {
			gr := newFakeGraph()
			gr.neighborErr = graph.ErrUnavailable
			d.Graph = gr
		}, http.StatusServiceUnavailable},
		{"error", func(d *Deps) {
			gr := newFakeGraph()
			gr.neighborErr = errBoom
			d.Graph = gr
		}, http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, h := newTestServer(t, tc.mutate)
			rec := do(t, h, http.MethodGet, "/v1/ws/team/proj/knowledge/graph?entity=x", nil)
			assertStatus(t, rec, tc.status)
		})
	}
}

func TestPatchNodeTransitions(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		body    any
		status  int
		wantSt  knowledge.State
		seedSt  knowledge.State
		mirrors bool
	}{
		{
			name: "archive via set_state", id: ulidA,
			body:   PatchNodeRequest{Op: "set_state", State: knowledge.StateArchived},
			status: http.StatusOK, wantSt: knowledge.StateArchived, seedSt: knowledge.StateActive, mirrors: true,
		},
		{
			name: "deprecate with reason", id: ulidA,
			body:   PatchNodeRequest{Op: "deprecate", Reason: "superseded by policy change"},
			status: http.StatusOK, wantSt: knowledge.StateDeprecated, seedSt: knowledge.StateActive, mirrors: true,
		},
		{
			name: "deprecate without reason", id: ulidA,
			body:   PatchNodeRequest{Op: "deprecate"},
			status: http.StatusBadRequest, seedSt: knowledge.StateActive,
		},
		{
			name: "set_state deprecated without reason", id: ulidA,
			body:   PatchNodeRequest{Op: "set_state", State: knowledge.StateDeprecated},
			status: http.StatusBadRequest, seedSt: knowledge.StateActive,
		},
		{
			name: "unknown op", id: ulidA,
			body:   PatchNodeRequest{Op: "yolo"},
			status: http.StatusBadRequest, seedSt: knowledge.StateActive,
		},
		{
			name: "unknown state", id: ulidA,
			body:   PatchNodeRequest{Op: "set_state", State: "sleepy"},
			status: http.StatusBadRequest, seedSt: knowledge.StateActive,
		},
		{
			name: "missing node", id: ulidC,
			body:   PatchNodeRequest{Op: "set_state", State: knowledge.StateArchived},
			status: http.StatusNotFound, seedSt: knowledge.StateActive,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			n := seedNode(ulidA, "n")
			n.State = tc.seedSt
			store.graphs[testKey.String()] = knowledge.Graph{Nodes: []knowledge.Node{n}}
			gr := newFakeGraph()
			_, h := newTestServer(t, func(d *Deps) {
				d.Store = store
				d.Graph = gr
			})

			rec := do(t, h, http.MethodPatch, nodesPath+"/"+tc.id, tc.body)
			assertStatus(t, rec, tc.status)
			if tc.status != http.StatusOK {
				return
			}
			var got NodeResponse
			decodeEnvelope(t, rec, &got)
			if got.Node.State != tc.wantSt {
				t.Fatalf("state = %q, want %q", got.Node.State, tc.wantSt)
			}
			if store.graphs[testKey.String()].Nodes[0].State != tc.wantSt {
				t.Fatal("hot graph must reflect the transition")
			}
			if tc.mirrors && len(gr.nodes[testKey.String()]) != 1 {
				t.Fatal("transition must reach the neo4j mirror")
			}
		})
	}
}

func TestPatchNodeIllegalTransitionIs409(t *testing.T) {
	orig := transitionNode
	transitionNode = func(knowledge.Node, knowledge.State, string, time.Time) (knowledge.Node, error) {
		return knowledge.Node{}, knowledge.ErrInvalidTransition
	}
	t.Cleanup(func() { transitionNode = orig })

	store := newFakeStore()
	store.graphs[testKey.String()] = knowledge.Graph{Nodes: []knowledge.Node{seedNode(ulidA, "n")}}
	_, h := newTestServer(t, func(d *Deps) { d.Store = store })

	rec := do(t, h, http.MethodPatch, nodesPath+"/"+ulidA, PatchNodeRequest{Op: "set_state", State: knowledge.StateArchived})
	assertStatus(t, rec, http.StatusConflict)
}

func TestPatchNodeRejectsNonULID(t *testing.T) {
	_, h := newTestServer(t, nil)
	rec := do(t, h, http.MethodPatch, nodesPath+"/xyz", PatchNodeRequest{Op: "set_state", State: knowledge.StateArchived})
	assertStatus(t, rec, http.StatusBadRequest)
}

func TestPurgeNodeRequiresConfirm(t *testing.T) {
	tests := []struct {
		name   string
		target string
		status int
	}{
		{"no confirm", nodesPath + "/" + ulidA, http.StatusBadRequest},
		{"confirm false", nodesPath + "/" + ulidA + "?confirm=false", http.StatusBadRequest},
		{"confirm true", nodesPath + "/" + ulidA + "?confirm=true", http.StatusOK},
		{"non-ulid", nodesPath + "/nope?confirm=true", http.StatusBadRequest},
		{"missing node", nodesPath + "/" + ulidC + "?confirm=true", http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			store.graphs[testKey.String()] = knowledge.Graph{
				Nodes: []knowledge.Node{seedNode(ulidA, "a"), seedNode(ulidB, "b")},
				Edges: []knowledge.Edge{{From: ulidA, To: ulidB, Rel: knowledge.RelRelatesTo}},
			}
			_, h := newTestServer(t, func(d *Deps) { d.Store = store })
			rec := do(t, h, http.MethodDelete, tc.target, nil)
			assertStatus(t, rec, tc.status)
		})
	}
}

func TestPurgeNodeRemovesIncidentEdges(t *testing.T) {
	store := newFakeStore()
	store.graphs[testKey.String()] = knowledge.Graph{
		Nodes: []knowledge.Node{seedNode(ulidA, "a"), seedNode(ulidB, "b"), seedNode(ulidC, "c")},
		Edges: []knowledge.Edge{
			{From: ulidA, To: ulidB, Rel: knowledge.RelRelatesTo},
			{From: ulidC, To: ulidA, Rel: knowledge.RelAbout},
			{From: ulidB, To: ulidC, Rel: knowledge.RelDerivedFrom},
		},
	}
	gr := newFakeGraph()
	_, h := newTestServer(t, func(d *Deps) {
		d.Store = store
		d.Graph = gr
	})

	rec := do(t, h, http.MethodDelete, nodesPath+"/"+ulidA+"?confirm=true", nil)
	assertStatus(t, rec, http.StatusOK)

	var got PurgeNodeResponse
	decodeEnvelope(t, rec, &got)
	if got.PurgedID != ulidA || got.RemovedEdges != 2 {
		t.Fatalf("purge report = %+v, want id=%s removed_edges=2", got, ulidA)
	}
	stored := store.graphs[testKey.String()]
	if len(stored.Nodes) != 2 || len(stored.Edges) != 1 {
		t.Fatalf("hot graph = %d nodes / %d edges, want 2/1", len(stored.Nodes), len(stored.Edges))
	}
	if len(gr.purged) != 1 || gr.purged[0] != ulidA {
		t.Fatalf("graph purges = %v, want [%s]", gr.purged, ulidA)
	}
}

func TestPurgeNodeDegradedWhenGraphDown(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Deps)
	}{
		{"graph nil", func(d *Deps) { d.Graph = nil }},
		{"delete fails", func(d *Deps) {
			gr := newFakeGraph()
			gr.deleteErr = graph.ErrUnavailable
			d.Graph = gr
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			store.graphs[testKey.String()] = knowledge.Graph{Nodes: []knowledge.Node{seedNode(ulidA, "a")}}
			_, h := newTestServer(t, func(d *Deps) {
				d.Store = store
				tc.mutate(d)
			})
			rec := do(t, h, http.MethodDelete, nodesPath+"/"+ulidA+"?confirm=true", nil)
			assertStatus(t, rec, http.StatusOK)

			var got PurgeNodeResponse
			decodeEnvelope(t, rec, &got)
			if len(got.Degraded) != 1 || got.Degraded[0] != degradedGraph {
				t.Fatalf("degraded = %v, want [%s]", got.Degraded, degradedGraph)
			}
			if len(store.graphs[testKey.String()].Nodes) != 0 {
				t.Fatal("hot purge must still happen when the graph is down")
			}
		})
	}
}

func TestKnowledgeHandlersWithoutHotStoreAre503(t *testing.T) {
	tests := []struct {
		name   string
		method string
		target string
		body   any
	}{
		{"create node", http.MethodPost, nodesPath, CreateNodeRequest{Kind: knowledge.KindFact, Name: "n", Trust: knowledge.TrustUserStated}},
		{"create edge", http.MethodPost, edgesPath, knowledge.Edge{From: ulidA, To: ulidB, Rel: knowledge.RelAbout}},
		{"patch node", http.MethodPatch, nodesPath + "/" + ulidA, PatchNodeRequest{Op: "set_state", State: knowledge.StateArchived}},
		{"purge node", http.MethodDelete, nodesPath + "/" + ulidA + "?confirm=true", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, h := newTestServer(t, func(d *Deps) { d.Store = nil })
			rec := do(t, h, tc.method, tc.target, tc.body)
			assertStatus(t, rec, http.StatusServiceUnavailable)
		})
	}
}
