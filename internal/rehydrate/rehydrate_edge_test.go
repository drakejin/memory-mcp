package rehydrate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/graph"
	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/knowledge"
	"github.com/drakejin/memory-mcp/internal/search"
)

var edgeBase = time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)

func TestIndexKeyFor(t *testing.T) {
	tests := []struct {
		name  string
		plane hotstore.Plane
		want  string
	}{
		{"episodic", hotstore.PlaneEpisodic, IndexKeyEpisodic},
		{"knowledge", hotstore.PlaneKnowledge, IndexKeyKnowledge},
		{"unknown planes fall back to the episodic index", hotstore.Plane("other"), IndexKeyEpisodic},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IndexKeyFor(tc.plane); got != tc.want {
				t.Fatalf("IndexKeyFor(%q) = %q, want %q", tc.plane, got, tc.want)
			}
		})
	}
}

func TestCheckDriftManifestReadFailure(t *testing.T) {
	store := newFakeStore()
	store.manifestErr = errors.New("manifest unreadable")
	r := New(store, newFakeIndex(), newFakeGraph(), &fakeClock{now: edgeBase})

	if _, err := r.CheckDrift(context.Background()); err == nil {
		t.Fatal("CheckDrift must fail when the manifest cannot be read")
	}
}

func TestKnowledgeDriftHotReadFailure(t *testing.T) {
	store := newFakeStore()
	store.graphs[testKey.String()] = knowledge.Graph{Nodes: []knowledge.Node{node("01AAAAAAAAAAAAAAAAAAAAAAAA")}}
	store.listErr = errors.New("listing broken")
	r := New(store, newFakeIndex(), newFakeGraph(), &fakeClock{now: edgeBase})

	d := r.knowledgeDrift(context.Background(), hotstore.Manifest{Files: map[string]hotstore.FileState{}})
	if !d.Detected {
		t.Fatalf("drift = %+v, want Detected when hot knowledge is unreadable", d)
	}
	if !strings.Contains(d.Reason, "hot knowledge unreadable") {
		t.Errorf("reason = %q, want it to name the unreadable hot store", d.Reason)
	}
}

func TestDriftWhenDerivedStoresUnreachable(t *testing.T) {
	tests := []struct {
		name       string
		index      search.Index
		graph      graph.Store
		wantEpiUn  bool
		wantKnUn   bool
		epiReason  string
		knowReason string
	}{
		{
			name: "both nil", index: nil, graph: nil,
			wantEpiUn: true, wantKnUn: true,
			epiReason: "opensearch not configured", knowReason: "neo4j not configured",
		},
		{
			name:      "both failing ping",
			index:     &pingFailIndex{fakeIndex: newFakeIndex()},
			graph:     &pingFailGraph{fakeGraph: newFakeGraph()},
			wantEpiUn: true, wantKnUn: true,
			epiReason: "opensearch unreachable", knowReason: "neo4j unreachable",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := New(newFakeStore(), tc.index, tc.graph, &fakeClock{now: edgeBase})
			rep, err := r.CheckDrift(context.Background())
			if err != nil {
				t.Fatalf("CheckDrift: %v", err)
			}
			if rep.Episodic.Unavailable != tc.wantEpiUn || rep.Episodic.Reason != tc.epiReason {
				t.Errorf("episodic drift = %+v, want unavailable=%v reason=%q", rep.Episodic, tc.wantEpiUn, tc.epiReason)
			}
			if rep.Knowledge.Unavailable != tc.wantKnUn || rep.Knowledge.Reason != tc.knowReason {
				t.Errorf("knowledge drift = %+v, want unavailable=%v reason=%q", rep.Knowledge, tc.wantKnUn, tc.knowReason)
			}
			// An unreachable store is not drift — nothing to converge toward.
			if rep.Episodic.Detected || rep.Knowledge.Detected {
				t.Error("unreachable stores must not be reported as drift")
			}
		})
	}
}

// pingFailIndex / pingFailGraph reject Ping but otherwise behave normally.
type pingFailIndex struct{ *fakeIndex }

func (p *pingFailIndex) Ping(context.Context) error { return search.ErrUnavailable }

type pingFailGraph struct{ *fakeGraph }

func (p *pingFailGraph) Ping(context.Context) error { return graph.ErrUnavailable }

func TestRehydrateAllProjectListingFailure(t *testing.T) {
	store := newFakeStore()
	store.episodes[testKey.String()] = []episodic.Record{rec("01AAAAAAAAAAAAAAAAAAAAAAAA", edgeBase)}
	store.listErr = errors.New("listing broken")
	r := New(store, newFakeIndex(), newFakeGraph(), &fakeClock{now: edgeBase})

	if _, err := r.RehydrateAll(context.Background(), false); err == nil {
		t.Fatal("RehydrateAll must fail when projects cannot be listed")
	}
}

func TestRehydrateAllEnsureIndexFailure(t *testing.T) {
	store := newFakeStore()
	store.episodes[testKey.String()] = []episodic.Record{rec("01AAAAAAAAAAAAAAAAAAAAAAAA", edgeBase)}
	idx := &ensureFailIndex{fakeIndex: newFakeIndex()}
	r := New(store, idx, newFakeGraph(), &fakeClock{now: edgeBase})

	rep, err := r.RehydrateAll(context.Background(), false)
	if err != nil {
		t.Fatalf("RehydrateAll: %v", err)
	}
	if rep.EpisodesIndexed != 0 {
		t.Errorf("EpisodesIndexed = %d, want 0 when the index cannot be created", rep.EpisodesIndexed)
	}
	if !hasFailureContaining(rep.Failures, "ensure index") {
		t.Fatalf("failures = %v, want an ensure-index failure", rep.Failures)
	}
	if _, ok := store.manifest.Indexes[IndexKeyEpisodic]; ok {
		t.Error("hydration sha must not be recorded when the plane never converged")
	}
}

type ensureFailIndex struct{ *fakeIndex }

func (e *ensureFailIndex) EnsureIndex(context.Context) error { return errors.New("mapping rejected") }

func TestRehydrateAllDropFailureIsNonFatal(t *testing.T) {
	store := newFakeStore()
	store.episodes[testKey.String()] = []episodic.Record{rec("01AAAAAAAAAAAAAAAAAAAAAAAA", edgeBase)}
	idx := newFakeIndex()
	idx.dropErr = errors.New("index_not_found")
	r := New(store, idx, newFakeGraph(), &fakeClock{now: edgeBase})

	rep, err := r.RehydrateAll(context.Background(), false)
	if err != nil {
		t.Fatalf("RehydrateAll: %v", err)
	}
	// A missing index is the normal first-hydration case: report and continue.
	if rep.EpisodesIndexed != 1 {
		t.Fatalf("EpisodesIndexed = %d, want 1 despite the drop failure", rep.EpisodesIndexed)
	}
	if !hasFailureContaining(rep.Failures, "drop index") {
		t.Errorf("failures = %v, want the drop failure disclosed", rep.Failures)
	}
}

func TestRehydrateAllEpisodeListingFailureIsPerProject(t *testing.T) {
	store := newFakeStore()
	store.episodes[testKey.String()] = []episodic.Record{rec("01AAAAAAAAAAAAAAAAAAAAAAAA", edgeBase)}
	store.graphs[testKey.String()] = knowledge.Graph{Nodes: []knowledge.Node{node("01BBBBBBBBBBBBBBBBBBBBBBBB")}}
	// Only per-project episode listing breaks; ListProjects still works, so the
	// run must degrade for that plane alone.
	store.episodeListErr = errors.New("episode file corrupt")
	r := New(store, newFakeIndex(), newFakeGraph(), &fakeClock{now: edgeBase})

	rep, err := r.RehydrateAll(context.Background(), false)
	if err != nil {
		t.Fatalf("RehydrateAll: %v", err)
	}
	if !hasFailureContaining(rep.Failures, "list") {
		t.Fatalf("failures = %v, want the per-project listing failure", rep.Failures)
	}
	// Knowledge still converged — one broken plane must not sink the other.
	if rep.NodesUpserted != 1 {
		t.Errorf("NodesUpserted = %d, want 1", rep.NodesUpserted)
	}
}

func TestHotNodeCountReadFailure(t *testing.T) {
	store := newFakeStore()
	store.graphs[testKey.String()] = knowledge.Graph{Nodes: []knowledge.Node{node("01AAAAAAAAAAAAAAAAAAAAAAAA")}}
	store.readKnowledgeErr = errors.New("knowledge file corrupt")
	r := New(store, newFakeIndex(), newFakeGraph(), &fakeClock{now: edgeBase})

	if _, err := r.hotNodeCount(context.Background()); err == nil {
		t.Fatal("hotNodeCount must fail when a project graph is unreadable")
	} else if !strings.Contains(err.Error(), testKey.String()) {
		t.Errorf("error %q must name the offending project", err)
	}
}

func TestRehydrateKnowledgeFailures(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*fakeGraph)
		verify      bool
		wantFailure string
	}{
		{
			name:        "node upsert fails",
			mutate:      func(g *fakeGraph) { g.upsertErr = graph.ErrUnavailable },
			wantFailure: "upsert nodes",
		},
		{
			name:        "verify mismatch",
			mutate:      func(g *fakeGraph) { g.nodeCountOverride = 99 },
			verify:      true,
			wantFailure: "verify",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			store.graphs[testKey.String()] = knowledge.Graph{
				Nodes: []knowledge.Node{node("01AAAAAAAAAAAAAAAAAAAAAAAA")},
				Edges: []knowledge.Edge{{From: "01AAAAAAAAAAAAAAAAAAAAAAAA", To: "01AAAAAAAAAAAAAAAAAAAAAAAA", Rel: knowledge.RelRelatesTo}},
			}
			gr := newFakeGraph()
			tc.mutate(gr)
			r := New(store, newFakeIndex(), gr, &fakeClock{now: edgeBase})

			rep, err := r.RehydrateAll(context.Background(), tc.verify)
			if err != nil {
				t.Fatalf("RehydrateAll: %v", err)
			}
			if !hasFailureContaining(rep.Failures, tc.wantFailure) {
				t.Fatalf("failures = %v, want one containing %q", rep.Failures, tc.wantFailure)
			}
			if _, ok := store.manifest.Indexes[IndexKeyKnowledge]; ok {
				t.Error("hydration sha recorded despite a failed knowledge plane")
			}
		})
	}
}

func TestRehydrateProjectDegraded(t *testing.T) {
	tests := []struct {
		name     string
		index    search.Index
		graph    graph.Store
		wantErrs []string
	}{
		{
			name: "no derived stores at all", index: nil, graph: nil,
			wantErrs: []string{"opensearch unavailable", "neo4j unavailable"},
		},
		{
			name: "index write fails", index: failingIndex(), graph: newFakeGraph(),
			wantErrs: []string{"bulk index"},
		},
		{
			name: "graph write fails", index: newFakeIndex(), graph: failingGraph(),
			wantErrs: []string{"upsert"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			store.episodes[testKey.String()] = []episodic.Record{rec("01AAAAAAAAAAAAAAAAAAAAAAAA", edgeBase)}
			store.graphs[testKey.String()] = knowledge.Graph{Nodes: []knowledge.Node{node("01BBBBBBBBBBBBBBBBBBBBBBBB")}}
			r := New(store, tc.index, tc.graph, &fakeClock{now: edgeBase})

			_, err := r.RehydrateProject(context.Background(), testKey)
			if err == nil {
				t.Fatal("RehydrateProject must report an error when a plane fails")
			}
			for _, want := range tc.wantErrs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q must mention %q", err, want)
				}
			}
		})
	}
}

func failingIndex() *fakeIndex {
	idx := newFakeIndex()
	idx.indexErr = search.ErrUnavailable
	return idx
}

func failingGraph() *fakeGraph {
	gr := newFakeGraph()
	gr.upsertErr = graph.ErrUnavailable
	return gr
}

func TestRehydrateProjectWithNoHotContent(t *testing.T) {
	store := newFakeStore()
	store.episodes[testKey.String()] = nil
	store.graphs[testKey.String()] = knowledge.Graph{}
	idx := newFakeIndex()
	r := New(store, idx, newFakeGraph(), &fakeClock{now: edgeBase})

	rep, err := r.RehydrateProject(context.Background(), testKey)
	if err != nil {
		t.Fatalf("RehydrateProject: %v", err)
	}
	if rep.EpisodesIndexed != 0 || rep.NodesUpserted != 0 {
		t.Fatalf("report = %+v, want zero counts for an empty project", rep)
	}
	if idx.indexCalls != 0 {
		t.Errorf("indexCalls = %d, want 0 — an empty project needs no bulk request", idx.indexCalls)
	}
}

func TestStatGateManifestFailure(t *testing.T) {
	store := newFakeStore()
	store.manifestErr = errors.New("manifest unreadable")
	r := New(store, newFakeIndex(), newFakeGraph(), &fakeClock{now: edgeBase})

	if err := r.StatGate(context.Background(), testKey); err == nil {
		t.Fatal("StatGate must surface a manifest read failure")
	}
}

// TestRehydrateAllClearsStaleGraphNodes pins the §5 convergence contract for
// the knowledge plane. MERGE replay can only add or update, so a node that
// left hot — purged, or left behind by a previous hot state on a container
// that outlived it — used to survive every rehydration. CheckDrift then
// reported "graph nodes N != hot nodes M" forever and no reindex could clear
// it, which also made POST /v1/reindex unable to do its stated job.
func TestRehydrateAllClearsStaleGraphNodes(t *testing.T) {
	// Arrange: hot holds one node; the graph additionally holds two strays.
	store := newFakeStore()
	store.graphs[testKey.String()] = knowledge.Graph{Nodes: []knowledge.Node{node("01HOT0000000000000000000001")}}
	gra := newFakeGraph()
	gra.nodes[testKey.String()] = map[string]knowledge.Node{
		"01HOT0000000000000000000001": node("01HOT0000000000000000000001"),
		"01STALE000000000000000000A":  node("01STALE000000000000000000A"),
	}
	gra.nodes["other/other/other"] = map[string]knowledge.Node{
		"01STALE000000000000000000B": node("01STALE000000000000000000B"),
	}
	r := New(store, newFakeIndex(), gra, &fakeClock{now: edgeBase})

	// Act
	rep, err := r.RehydrateAll(context.Background(), false)
	if err != nil {
		t.Fatalf("RehydrateAll() = %v", err)
	}

	// Assert: the graph now mirrors hot exactly, and drift is clear.
	if len(rep.Failures) != 0 {
		t.Fatalf("unexpected failures: %v", rep.Failures)
	}
	got, err := gra.NodeCount(context.Background(), hotstore.ProjectKey{})
	if err != nil {
		t.Fatalf("NodeCount() = %v", err)
	}
	if got != 1 {
		t.Fatalf("graph holds %d nodes after full rehydration, want 1 (stale nodes must not survive)", got)
	}
	drift, err := r.CheckDrift(context.Background())
	if err != nil {
		t.Fatalf("CheckDrift() = %v", err)
	}
	if drift.Knowledge.Detected {
		t.Fatalf("knowledge drift must converge after a full rehydration, got %q", drift.Knowledge.Reason)
	}
}

// TestRehydrateProjectDoesNotClearOtherProjects is the other half of the
// contract: the request-time partial path converges one project and must never
// wipe the rest of the graph.
func TestRehydrateProjectDoesNotClearOtherProjects(t *testing.T) {
	// Arrange
	other := hotstore.ProjectKey{Workspace: "ws", Team: "team", Project: "other"}
	store := newFakeStore()
	store.graphs[testKey.String()] = knowledge.Graph{Nodes: []knowledge.Node{node("01HOT0000000000000000000001")}}
	gra := newFakeGraph()
	gra.nodes[other.String()] = map[string]knowledge.Node{
		"01OTHER00000000000000000A": node("01OTHER00000000000000000A"),
	}
	r := New(store, newFakeIndex(), gra, &fakeClock{now: edgeBase})

	// Act
	if _, err := r.RehydrateProject(context.Background(), testKey); err != nil {
		t.Fatalf("RehydrateProject() = %v", err)
	}

	// Assert
	if n := len(gra.nodes[other.String()]); n != 1 {
		t.Fatalf("other project holds %d nodes, want 1 — partial rehydration must not clear it", n)
	}
}

// hasFailureContaining reports whether any failure mentions want.
func hasFailureContaining(failures []string, want string) bool {
	for _, f := range failures {
		if strings.Contains(f, want) {
			return true
		}
	}
	return false
}
