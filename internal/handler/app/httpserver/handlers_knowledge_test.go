package httpserver_test

import (
	"errors"
	app "github.com/drakejin/memory-mcp/internal/app/httpserver"
	. "github.com/drakejin/memory-mcp/internal/handler/app/httpserver"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/handler/app/httpserver/apierr"
	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/service/knowledge"
	"github.com/drakejin/memory-mcp/internal/service/rehydrate"
	"github.com/drakejin/memory-mcp/internal/x/ulid"
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

// seedBufferedNode is a node that has already passed through the archived
// buffer, which is the only state §3 lets a purge start from.
func seedBufferedNode(id, name string) knowledge.Node {
	n := seedNode(id, name)
	n.State = knowledge.StateArchived
	return n
}

func TestCreateNodeValidation(t *testing.T) {
	valid := knowledge.CreateNodeInput{Kind: knowledge.KindFact, Name: "n", Trust: knowledge.TrustUserStated}

	tests := []struct {
		name   string
		body   any
		status int
	}{
		{"valid", valid, http.StatusCreated},
		{"unknown kind", knowledge.CreateNodeInput{Kind: "myth", Name: "n", Trust: knowledge.TrustUserStated}, http.StatusBadRequest},
		{"empty name", knowledge.CreateNodeInput{Kind: knowledge.KindFact, Trust: knowledge.TrustUserStated}, http.StatusBadRequest},
		{"unknown trust", knowledge.CreateNodeInput{Kind: knowledge.KindFact, Name: "n", Trust: "vibes"}, http.StatusBadRequest},
		{
			name:   "malformed provenance",
			body:   knowledge.CreateNodeInput{Kind: knowledge.KindFact, Name: "n", Trust: knowledge.TrustUserStated, Provenance: []string{"nope"}},
			status: http.StatusBadRequest,
		},
		{
			name:   "malformed supersedes",
			body:   knowledge.CreateNodeInput{Kind: knowledge.KindFact, Name: "n", Trust: knowledge.TrustUserStated, Supersedes: []string{"nope"}},
			status: http.StatusBadRequest,
		},
		{
			name:   "bad review_after",
			body:   knowledge.CreateNodeInput{Kind: knowledge.KindFact, Name: "n", Trust: knowledge.TrustUserStated, ReviewAfter: "someday"},
			status: http.StatusBadRequest,
		},
		{
			name:   "rfc3339 review_after",
			body:   knowledge.CreateNodeInput{Kind: knowledge.KindFact, Name: "n", Trust: knowledge.TrustUserStated, ReviewAfter: "2027-01-01T00:00:00Z"},
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
	_, h := newTestServer(t, func(d *app.Config) {
		d.Store = store
		d.Graph = gr
	})

	rec := do(t, h, http.MethodPost, nodesPath, knowledge.CreateNodeInput{
		Kind: knowledge.KindLesson, Name: "보안 끄기", Body: "single-node dev only",
		Trust: knowledge.TrustUserStated, Aliases: []string{" alias ", ""}, Provenance: []string{ulidA},
	})
	assertStatus(t, rec, http.StatusCreated)

	var got knowledge.NodeResult
	decodeEnvelope(t, rec, &got)
	if !ulid.Valid(got.Node.ID) {
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
	_, h := newTestServer(t, func(d *app.Config) {
		d.Store = store
		d.Graph = gr
	})

	rec := do(t, h, http.MethodPost, nodesPath, knowledge.CreateNodeInput{
		Kind: knowledge.KindFact, Name: "new", Trust: knowledge.TrustAgentInferred, Supersedes: []string{ulidA},
	})
	assertStatus(t, rec, http.StatusCreated)

	var got knowledge.NodeResult
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
	rec := do(t, h, http.MethodPost, nodesPath, knowledge.CreateNodeInput{
		Kind: knowledge.KindFact, Name: "new", Trust: knowledge.TrustUserStated, Supersedes: []string{ulidA},
	})
	assertStatus(t, rec, http.StatusNotFound)
}

func TestCreateNodeDegradedWhenGraphDown(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*app.Config)
	}{
		{"graph not configured", func(d *app.Config) { d.Graph = nil }},
		{"upsert fails", func(d *app.Config) {
			gr := newFakeGraph()
			gr.upsertNodeErr = errGraphDown
			d.Graph = gr
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			_, h := newTestServer(t, func(d *app.Config) {
				d.Store = store
				tc.mutate(d)
			})
			rec := do(t, h, http.MethodPost, nodesPath, knowledge.CreateNodeInput{
				Kind: knowledge.KindFact, Name: "n", Trust: knowledge.TrustUserStated,
			})
			assertStatus(t, rec, http.StatusCreated)

			var got knowledge.NodeResult
			decodeEnvelope(t, rec, &got)
			if len(got.Degraded) != 1 || got.Degraded[0] != DegradedGraph {
				t.Fatalf("degraded = %v, want [%s]", got.Degraded, DegradedGraph)
			}
			if len(store.graphs[testKey.String()].Nodes) != 1 {
				t.Fatal("hot write must succeed while the graph is down (§5)")
			}
			wantDirty := rehydrate.ManifestFileKey(rehydrate.PlaneKnowledge, testKey)
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
			_, h := newTestServer(t, func(d *app.Config) { d.Store = store })
			rec := do(t, h, http.MethodPost, nodesPath, knowledge.CreateNodeInput{
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
			_, h := newTestServer(t, func(d *app.Config) { d.Store = store })
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
	_, h := newTestServer(t, func(d *app.Config) { d.Store = store })

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
	gr.upsertEdgeErr = errGraphDown
	_, h := newTestServer(t, func(d *app.Config) {
		d.Store = store
		d.Graph = gr
	})

	rec := do(t, h, http.MethodPost, edgesPath, knowledge.Edge{From: ulidA, To: ulidB, Rel: knowledge.RelAbout})
	assertStatus(t, rec, http.StatusCreated)
	var got knowledge.EdgeResult
	decodeEnvelope(t, rec, &got)
	if len(got.Degraded) != 1 || got.Degraded[0] != DegradedGraph {
		t.Fatalf("degraded = %v, want [%s]", got.Degraded, DegradedGraph)
	}
	if len(store.graphs[testKey.String()].Edges) != 1 {
		t.Fatal("hot edge write must survive a dead graph")
	}
}

func TestSearchKnowledge(t *testing.T) {
	tests := []struct {
		name   string
		target string
		mutate func(*app.Config)
		status int
	}{
		{"missing q", "/v1/ws/team/proj/knowledge/search", nil, http.StatusBadRequest},
		{"bad project", "/v1/WS/team/proj/knowledge/search?q=a", nil, http.StatusBadRequest},
		{"ok", "/v1/ws/team/proj/knowledge/search?q=a", nil, http.StatusOK},
		{"opt-in archived", "/v1/ws/team/proj/knowledge/search?q=a&include_archived=true", nil, http.StatusOK},
		{"graph nil", "/v1/ws/team/proj/knowledge/search?q=a", func(d *app.Config) { d.Graph = nil }, http.StatusServiceUnavailable},
		{
			name:   "graph unreachable",
			target: "/v1/ws/team/proj/knowledge/search?q=a",
			mutate: func(d *app.Config) {
				gr := newFakeGraph()
				gr.searchErr = errGraphDown
				d.Graph = gr
			},
			status: http.StatusServiceUnavailable,
		},
		{
			name:   "graph error",
			target: "/v1/ws/team/proj/knowledge/search?q=a",
			mutate: func(d *app.Config) {
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
	_, h := newTestServer(t, func(d *app.Config) {
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
		mutate func(*app.Config)
		status int
	}{
		{"graph nil", func(d *app.Config) { d.Graph = nil }, http.StatusServiceUnavailable},
		{"unreachable", func(d *app.Config) {
			gr := newFakeGraph()
			gr.neighborErr = errGraphDown
			d.Graph = gr
		}, http.StatusServiceUnavailable},
		{"error", func(d *app.Config) {
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
			body:   knowledge.PatchNodeInput{Op: "set_state", State: knowledge.StateArchived},
			status: http.StatusOK, wantSt: knowledge.StateArchived, seedSt: knowledge.StateActive, mirrors: true,
		},
		{
			name: "deprecate with reason", id: ulidA,
			body:   knowledge.PatchNodeInput{Op: "deprecate", Reason: "superseded by policy change"},
			status: http.StatusOK, wantSt: knowledge.StateDeprecated, seedSt: knowledge.StateActive, mirrors: true,
		},
		{
			name: "deprecate without reason", id: ulidA,
			body:   knowledge.PatchNodeInput{Op: "deprecate"},
			status: http.StatusBadRequest, seedSt: knowledge.StateActive,
		},
		{
			name: "set_state deprecated without reason", id: ulidA,
			body:   knowledge.PatchNodeInput{Op: "set_state", State: knowledge.StateDeprecated},
			status: http.StatusBadRequest, seedSt: knowledge.StateActive,
		},
		{
			name: "unknown op", id: ulidA,
			body:   knowledge.PatchNodeInput{Op: "yolo"},
			status: http.StatusBadRequest, seedSt: knowledge.StateActive,
		},
		{
			name: "unknown state", id: ulidA,
			body:   knowledge.PatchNodeInput{Op: "set_state", State: "sleepy"},
			status: http.StatusBadRequest, seedSt: knowledge.StateActive,
		},
		{
			name: "missing node", id: ulidC,
			body:   knowledge.PatchNodeInput{Op: "set_state", State: knowledge.StateArchived},
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
			_, h := newTestServer(t, func(d *app.Config) {
				d.Store = store
				d.Graph = gr
			})

			rec := do(t, h, http.MethodPatch, nodesPath+"/"+tc.id, tc.body)
			assertStatus(t, rec, tc.status)
			if tc.status != http.StatusOK {
				return
			}
			var got knowledge.NodeResult
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

// TestPatchNodeIllegalTransitionIs409 drives the real state machine: a
// deprecated node may only revive to active, so deprecated -> archived is a
// state-machine violation. knowledge.Transition reports it as KindConflict and
// apierr maps that — and only that — to 409 (§2.2).
func TestPatchNodeIllegalTransitionIs409(t *testing.T) {
	store := newFakeStore()
	deprecated := seedNode(ulidA, "n")
	deprecated.State = knowledge.StateDeprecated
	store.graphs[testKey.String()] = knowledge.Graph{Nodes: []knowledge.Node{deprecated}}
	gr := newFakeGraph()
	_, h := newTestServer(t, func(d *app.Config) {
		d.Store = store
		d.Graph = gr
	})

	rec := do(t, h, http.MethodPatch, nodesPath+"/"+ulidA, knowledge.PatchNodeInput{Op: "set_state", State: knowledge.StateArchived})
	assertStatus(t, rec, http.StatusConflict)

	env := decodeEnvelope(t, rec, nil)
	if env.Error == nil || env.Error.Code != apierr.CodeConflict {
		t.Fatalf("error = %+v, want code %q", env.Error, apierr.CodeConflict)
	}
	if store.graphs[testKey.String()].Nodes[0].State != knowledge.StateDeprecated {
		t.Error("a refused transition must leave the hot graph untouched")
	}
	if len(gr.nodes[testKey.String()]) != 0 {
		t.Error("a refused transition must not reach the neo4j mirror")
	}
}

// TestPatchNodeRevivesDeprecatedToActive is the legal counterpart: the same
// deprecated node may go back to active.
func TestPatchNodeRevivesDeprecatedToActive(t *testing.T) {
	store := newFakeStore()
	deprecated := seedNode(ulidA, "n")
	deprecated.State = knowledge.StateDeprecated
	deprecated.SupersededBy = ulidB
	store.graphs[testKey.String()] = knowledge.Graph{Nodes: []knowledge.Node{deprecated}}
	_, h := newTestServer(t, func(d *app.Config) { d.Store = store })

	rec := do(t, h, http.MethodPatch, nodesPath+"/"+ulidA, knowledge.PatchNodeInput{Op: "set_state", State: knowledge.StateActive})
	assertStatus(t, rec, http.StatusOK)

	var got knowledge.NodeResult
	decodeEnvelope(t, rec, &got)
	if got.Node.State != knowledge.StateActive {
		t.Fatalf("state = %q, want active", got.Node.State)
	}
	if !got.Node.Updated.Equal(fixedNow) {
		t.Errorf("updated = %v, want the injected clock %v", got.Node.Updated, fixedNow)
	}
}

func TestPatchNodeRejectsNonULID(t *testing.T) {
	_, h := newTestServer(t, nil)
	rec := do(t, h, http.MethodPatch, nodesPath+"/xyz", knowledge.PatchNodeInput{Op: "set_state", State: knowledge.StateArchived})
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
				Nodes: []knowledge.Node{seedBufferedNode(ulidA, "a"), seedNode(ulidB, "b")},
				Edges: []knowledge.Edge{{From: ulidA, To: ulidB, Rel: knowledge.RelRelatesTo}},
			}
			_, h := newTestServer(t, func(d *app.Config) { d.Store = store })
			rec := do(t, h, http.MethodDelete, tc.target, nil)
			assertStatus(t, rec, tc.status)
		})
	}
}

func TestPurgeNodeRemovesIncidentEdges(t *testing.T) {
	store := newFakeStore()
	store.graphs[testKey.String()] = knowledge.Graph{
		Nodes: []knowledge.Node{seedBufferedNode(ulidA, "a"), seedNode(ulidB, "b"), seedNode(ulidC, "c")},
		Edges: []knowledge.Edge{
			{From: ulidA, To: ulidB, Rel: knowledge.RelRelatesTo},
			{From: ulidC, To: ulidA, Rel: knowledge.RelAbout},
			{From: ulidB, To: ulidC, Rel: knowledge.RelDerivedFrom},
		},
	}
	gr := newFakeGraph()
	_, h := newTestServer(t, func(d *app.Config) {
		d.Store = store
		d.Graph = gr
	})

	rec := do(t, h, http.MethodDelete, nodesPath+"/"+ulidA+"?confirm=true", nil)
	assertStatus(t, rec, http.StatusOK)

	var got knowledge.PurgeResult
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
		mutate func(*app.Config)
	}{
		{"graph nil", func(d *app.Config) { d.Graph = nil }},
		{"delete fails", func(d *app.Config) {
			gr := newFakeGraph()
			gr.deleteErr = errGraphDown
			d.Graph = gr
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			store.graphs[testKey.String()] = knowledge.Graph{Nodes: []knowledge.Node{seedBufferedNode(ulidA, "a")}}
			_, h := newTestServer(t, func(d *app.Config) {
				d.Store = store
				tc.mutate(d)
			})
			rec := do(t, h, http.MethodDelete, nodesPath+"/"+ulidA+"?confirm=true", nil)
			assertStatus(t, rec, http.StatusOK)

			var got knowledge.PurgeResult
			decodeEnvelope(t, rec, &got)
			if len(got.Degraded) != 1 || got.Degraded[0] != DegradedGraph {
				t.Fatalf("degraded = %v, want [%s]", got.Degraded, DegradedGraph)
			}
			if len(store.graphs[testKey.String()].Nodes) != 0 {
				t.Fatal("hot purge must still happen when the graph is down")
			}
		})
	}
}

// seedEpisode is an unconsolidated hot record — the state every episode starts
// in, and the one §3.1 refuses to age.
func seedEpisode(id string) episode.Record {
	return episode.Record{
		ID: id, Kind: episode.KindEvent, Actor: episode.ActorAgent,
		Text: "관찰", OccurredAt: fixedNow.Add(-time.Hour), Entities: []string{},
	}
}

// consolidatedIDs lists the hot records of testKey that carry consolidated=true.
func consolidatedIDs(t *testing.T, store *fakeStore) []string {
	t.Helper()
	recs, err := store.ListEpisodes(t.Context(), testKey)
	if err != nil {
		t.Fatalf("ListEpisodes: %v", err)
	}
	var out []string
	for _, rec := range recs {
		if rec.Consolidated {
			out = append(out, rec.ID)
		}
	}
	return out
}

// §3 closes the consolidation loop here: the agent distils episodes and says so
// by naming them in provenance, and §3.1 reads the resulting consolidated=true
// as the precondition for cold aging. Without this no API path ever sets the
// flag, so aging can never fire in production and stale_unconsolidated grows
// without bound.
func TestCreateNodeMarksProvenanceEpisodesConsolidated(t *testing.T) {
	// Arrange
	store := newFakeStore()
	store.episodes[testKey.String()] = []episode.Record{seedEpisode(ulidA), seedEpisode(ulidB)}
	index := newFakeIndex()
	_, h := newTestServer(t, func(d *app.Config) {
		d.Store = store
		d.Index = index
	})

	// Act: distil ulidA only.
	rec := do(t, h, http.MethodPost, nodesPath, knowledge.CreateNodeInput{
		Kind: knowledge.KindFact, Name: "distilled", Trust: knowledge.TrustUserStated,
		Provenance: []string{ulidA},
	})

	// Assert
	assertStatus(t, rec, http.StatusCreated)
	var got knowledge.NodeResult
	decodeEnvelope(t, rec, &got)
	if len(got.Degraded) != 0 {
		t.Errorf("degraded = %v, want none", got.Degraded)
	}
	ids := consolidatedIDs(t, store)
	if len(ids) != 1 || ids[0] != ulidA {
		t.Fatalf("consolidated episodes = %v, want [%s] — the promotion §3.1 gates cold aging on never happened", ids, ulidA)
	}
	// The flag is an indexed field, so the derived copy has to be refreshed too.
	var reindexed bool
	for _, r := range index.indexed[testKey.String()] {
		if r.ID == ulidA && r.Consolidated {
			reindexed = true
		}
	}
	if !reindexed {
		t.Errorf("promoted episode was not re-indexed: %+v", index.indexed[testKey.String()])
	}
}

// Provenance may name an episode that already aged to cold or belongs to
// another project. That must not fail a node whose hot write already succeeded.
func TestCreateNodeToleratesProvenanceOutsideHot(t *testing.T) {
	// Arrange: only ulidA is a hot record here; ulidC is not.
	store := newFakeStore()
	store.episodes[testKey.String()] = []episode.Record{seedEpisode(ulidA)}
	_, h := newTestServer(t, func(d *app.Config) { d.Store = store })

	// Act
	rec := do(t, h, http.MethodPost, nodesPath, knowledge.CreateNodeInput{
		Kind: knowledge.KindFact, Name: "distilled", Trust: knowledge.TrustUserStated,
		Provenance: []string{ulidA, ulidC},
	})

	// Assert
	assertStatus(t, rec, http.StatusCreated)
	var got knowledge.NodeResult
	decodeEnvelope(t, rec, &got)
	if len(got.Degraded) != 0 {
		t.Errorf("degraded = %v, want none — an unknown provenance id is not a failure", got.Degraded)
	}
	if ids := consolidatedIDs(t, store); len(ids) != 1 || ids[0] != ulidA {
		t.Fatalf("consolidated episodes = %v, want [%s]", ids, ulidA)
	}
}

// A node with no provenance claims no distillation, so nothing is promoted and
// the episodic file is never rewritten.
func TestCreateNodeWithoutProvenanceTouchesNoEpisode(t *testing.T) {
	// Arrange
	store := newFakeStore()
	store.episodes[testKey.String()] = []episode.Record{seedEpisode(ulidA)}
	_, h := newTestServer(t, func(d *app.Config) { d.Store = store })

	// Act
	rec := do(t, h, http.MethodPost, nodesPath, knowledge.CreateNodeInput{
		Kind: knowledge.KindFact, Name: "standalone", Trust: knowledge.TrustUserStated,
	})

	// Assert
	assertStatus(t, rec, http.StatusCreated)
	if ids := consolidatedIDs(t, store); len(ids) != 0 {
		t.Errorf("consolidated episodes = %v, want none", ids)
	}
	if store.updateCalls != 0 {
		t.Errorf("UpdateEpisodes called %d times for a node with no provenance", store.updateCalls)
	}
}

// The knowledge node is already in hot when the promotion runs, so a failure
// there is degradation, not a failed creation (§5).
func TestCreateNodeReportsDegradedWhenPromotionFails(t *testing.T) {
	// Arrange
	store := newFakeStore()
	store.episodes[testKey.String()] = []episode.Record{seedEpisode(ulidA)}
	store.updateEpiErr = errors.New("episodic file locked")
	_, h := newTestServer(t, func(d *app.Config) { d.Store = store })

	// Act
	rec := do(t, h, http.MethodPost, nodesPath, knowledge.CreateNodeInput{
		Kind: knowledge.KindFact, Name: "distilled", Trust: knowledge.TrustUserStated,
		Provenance: []string{ulidA},
	})

	// Assert
	assertStatus(t, rec, http.StatusCreated)
	var got knowledge.NodeResult
	decodeEnvelope(t, rec, &got)
	if !slices.Contains(got.Degraded, episode.DegradedPromotion) {
		t.Fatalf("degraded = %v, want it to contain %q", got.Degraded, episode.DegradedPromotion)
	}
	if n := len(store.graphs[testKey.String()].Nodes); n != 1 {
		t.Errorf("hot graph holds %d nodes, want 1 — the node write must stand", n)
	}
}

// §3 puts the archived → deprecated buffer in front of every deletion, so
// confirm=true alone must not destroy a live fact. Without the gate a node
// created seconds ago — one no consolidation snapshot has ever covered, so S3
// versioning is no backstop — would be gone in a single call.
func TestPurgeNodeRefusesUnbufferedNode(t *testing.T) {
	// Arrange
	store := newFakeStore()
	store.graphs[testKey.String()] = knowledge.Graph{Nodes: []knowledge.Node{seedNode(ulidA, "a")}}
	gr := newFakeGraph()
	_, h := newTestServer(t, func(d *app.Config) {
		d.Store = store
		d.Graph = gr
	})

	// Act
	rec := do(t, h, http.MethodDelete, nodesPath+"/"+ulidA+"?confirm=true", nil)

	// Assert
	assertStatus(t, rec, http.StatusConflict)
	if n := len(store.graphs[testKey.String()].Nodes); n != 1 {
		t.Fatalf("hot graph holds %d nodes, want 1 — a refused purge must not write", n)
	}
	if len(gr.purged) != 0 {
		t.Errorf("refused purge still reached the graph mirror: %v", gr.purged)
	}
}

// After the archive step the same call succeeds: the gate is a lifecycle
// requirement, not a ban.
func TestPurgeNodeSucceedsAfterDeprecation(t *testing.T) {
	// Arrange
	store := newFakeStore()
	node := seedNode(ulidA, "a")
	node.State = knowledge.StateDeprecated
	store.graphs[testKey.String()] = knowledge.Graph{Nodes: []knowledge.Node{node}}
	_, h := newTestServer(t, func(d *app.Config) { d.Store = store })

	// Act
	rec := do(t, h, http.MethodDelete, nodesPath+"/"+ulidA+"?confirm=true", nil)

	// Assert
	assertStatus(t, rec, http.StatusOK)
	if n := len(store.graphs[testKey.String()].Nodes); n != 0 {
		t.Fatalf("hot graph holds %d nodes, want 0", n)
	}
}

// A purge must not leave the revision chain pointing at an id no node carries
// any more: withoutNode used to drop the node and its edges but keep every
// scalar superseded_by / supersedes reference to it (§3 non-destructive
// revision).
func TestPurgeNodeRepairsDanglingRevisionLinks(t *testing.T) {
	// Arrange: purged (archived) supersedes older, and is superseded by newer.
	older, purged, newer := seedNode(ulidA, "older"), seedBufferedNode(ulidB, "purged"), seedNode(ulidC, "newer")
	older.State = knowledge.StateArchived
	older.SupersededBy = purged.ID
	purged.Supersedes = []string{older.ID}
	purged.SupersededBy = newer.ID
	newer.Supersedes = []string{purged.ID}

	store := newFakeStore()
	store.graphs[testKey.String()] = knowledge.Graph{Nodes: []knowledge.Node{older, purged, newer}}
	_, h := newTestServer(t, func(d *app.Config) { d.Store = store })

	// Act
	rec := do(t, h, http.MethodDelete, nodesPath+"/"+ulidB+"?confirm=true", nil)

	// Assert
	assertStatus(t, rec, http.StatusOK)
	stored := store.graphs[testKey.String()]
	gotOlder, err := knowledge.FindNode(stored, ulidA)
	if err != nil {
		t.Fatalf("older node vanished: %v", err)
	}
	if gotOlder.SupersededBy != "" {
		t.Errorf("older.superseded_by = %q, want cleared — it names a purged node", gotOlder.SupersededBy)
	}
	gotNewer, err := knowledge.FindNode(stored, ulidC)
	if err != nil {
		t.Fatalf("newer node vanished: %v", err)
	}
	if len(gotNewer.Supersedes) != 0 {
		t.Errorf("newer.supersedes = %v, want empty — it names a purged node", gotNewer.Supersedes)
	}
}

func TestKnowledgeHandlersWithoutHotStoreAre503(t *testing.T) {
	tests := []struct {
		name   string
		method string
		target string
		body   any
	}{
		{"create node", http.MethodPost, nodesPath, knowledge.CreateNodeInput{Kind: knowledge.KindFact, Name: "n", Trust: knowledge.TrustUserStated}},
		{"create edge", http.MethodPost, edgesPath, knowledge.Edge{From: ulidA, To: ulidB, Rel: knowledge.RelAbout}},
		{"patch node", http.MethodPatch, nodesPath + "/" + ulidA, knowledge.PatchNodeInput{Op: "set_state", State: knowledge.StateArchived}},
		{"purge node", http.MethodDelete, nodesPath + "/" + ulidA + "?confirm=true", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, h := newTestServer(t, func(d *app.Config) { d.Store = nil })
			rec := do(t, h, tc.method, tc.target, tc.body)
			assertStatus(t, rec, http.StatusServiceUnavailable)
		})
	}
}
