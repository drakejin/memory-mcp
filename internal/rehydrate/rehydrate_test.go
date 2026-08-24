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

// --- fakes -----------------------------------------------------------------

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

// fakeStore is an in-memory hotstore.Store covering what the Runner uses.
type fakeStore struct {
	episodes map[string][]episodic.Record
	graphs   map[string]knowledge.Graph
	manifest hotstore.Manifest
	mtimes   map[string]time.Time // fileKey -> mtime
	// listErr fails ListProjects (and, transitively, anything that enumerates).
	listErr   error
	failWrite bool
	// Narrower failure hooks for the per-call error branches.
	manifestErr      error
	episodeListErr   error
	readKnowledgeErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		episodes: map[string][]episodic.Record{},
		graphs:   map[string]knowledge.Graph{},
		manifest: hotstore.Manifest{
			Files:   map[string]hotstore.FileState{},
			Indexes: map[string]hotstore.IndexState{},
		},
		mtimes: map[string]time.Time{},
	}
}

func (s *fakeStore) AppendEpisode(_ context.Context, key hotstore.ProjectKey, rec episodic.Record) error {
	s.episodes[key.String()] = append(s.episodes[key.String()], rec)
	return nil
}

func (s *fakeStore) ListEpisodes(_ context.Context, key hotstore.ProjectKey) ([]episodic.Record, error) {
	if s.episodeListErr != nil {
		return nil, s.episodeListErr
	}
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.episodes[key.String()], nil
}

func (s *fakeStore) GetEpisode(_ context.Context, key hotstore.ProjectKey, id string) (episodic.Record, error) {
	for _, r := range s.episodes[key.String()] {
		if r.ID == id {
			return r, nil
		}
	}
	return episodic.Record{}, hotstore.ErrNotFound
}

func (s *fakeStore) UpdateEpisodes(_ context.Context, key hotstore.ProjectKey, ids []string, fn func(episodic.Record) episodic.Record) error {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	recs := s.episodes[key.String()]
	for i, r := range recs {
		if want[r.ID] {
			recs[i] = fn(r)
		}
	}
	return nil
}

func (s *fakeStore) RemoveEpisodes(_ context.Context, _ hotstore.ProjectKey, _ []string) error {
	return nil
}

func (s *fakeStore) ReadKnowledge(_ context.Context, key hotstore.ProjectKey) (knowledge.Graph, error) {
	if s.readKnowledgeErr != nil {
		return knowledge.Graph{}, s.readKnowledgeErr
	}
	return s.graphs[key.String()], nil
}

func (s *fakeStore) WriteKnowledge(_ context.Context, key hotstore.ProjectKey, g knowledge.Graph) error {
	s.graphs[key.String()] = g
	return nil
}

func (s *fakeStore) ListProjects(_ context.Context) ([]hotstore.ProjectKey, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	seen := map[string]hotstore.ProjectKey{}
	for k := range s.episodes {
		seen[k] = splitKey(k)
	}
	for k := range s.graphs {
		seen[k] = splitKey(k)
	}
	out := make([]hotstore.ProjectKey, 0, len(seen))
	for _, v := range seen {
		out = append(out, v)
	}
	return out, nil
}

func splitKey(s string) hotstore.ProjectKey {
	parts := strings.SplitN(s, "/", 3)
	return hotstore.ProjectKey{Workspace: parts[0], Team: parts[1], Project: parts[2]}
}

func (s *fakeStore) Manifest(_ context.Context) (hotstore.Manifest, error) {
	if s.manifestErr != nil {
		return hotstore.Manifest{}, s.manifestErr
	}
	return s.manifest, nil
}

func (s *fakeStore) UpdateManifest(_ context.Context, fn func(hotstore.Manifest) (hotstore.Manifest, error)) error {
	if s.failWrite {
		return errors.New("manifest write failed")
	}
	m, err := fn(s.manifest)
	if err != nil {
		return err
	}
	s.manifest = m
	return nil
}

func (s *fakeStore) MarkDirty(_ context.Context, key hotstore.ProjectKey, plane hotstore.Plane) error {
	fk := string(plane) + "/" + key.String()
	fs := s.manifest.Files[fk]
	fs.Dirty = true
	s.manifest.Files[fk] = fs
	return nil
}

func (s *fakeStore) FileInfo(_ context.Context, key hotstore.ProjectKey, plane hotstore.Plane) (int64, time.Time, error) {
	fk := string(plane) + "/" + key.String()
	mt, ok := s.mtimes[fk]
	if !ok {
		return 0, time.Time{}, hotstore.ErrNotFound
	}
	return 1, mt, nil
}

// fakeIndex is an in-memory search.Index.
type fakeIndex struct {
	docs        map[string]map[string]episodic.Record // project -> id -> rec
	pingErr     error
	indexErr    error
	dropErr     error
	countErr    error
	indexCalls  int
	dropCalls   int
	ensureCalls int
}

func newFakeIndex() *fakeIndex {
	return &fakeIndex{docs: map[string]map[string]episodic.Record{}}
}

func (f *fakeIndex) Ping(_ context.Context) error { return f.pingErr }

func (f *fakeIndex) EnsureIndex(_ context.Context) error {
	f.ensureCalls++
	return nil
}

func (f *fakeIndex) IndexRecords(_ context.Context, key hotstore.ProjectKey, recs []episodic.Record) error {
	f.indexCalls++
	if f.indexErr != nil {
		return f.indexErr
	}
	m := f.docs[key.String()]
	if m == nil {
		m = map[string]episodic.Record{}
		f.docs[key.String()] = m
	}
	for _, r := range recs {
		m[r.ID] = r
	}
	return nil
}

func (f *fakeIndex) DeleteRecords(_ context.Context, key hotstore.ProjectKey, ids []string) error {
	for _, id := range ids {
		delete(f.docs[key.String()], id)
	}
	return nil
}

func (f *fakeIndex) Search(_ context.Context, _ hotstore.ProjectKey, _ search.Query) ([]search.Hit, error) {
	return nil, nil
}

func (f *fakeIndex) DocCount(_ context.Context, key hotstore.ProjectKey) (int, error) {
	if f.countErr != nil {
		return 0, f.countErr
	}
	if (key == hotstore.ProjectKey{}) {
		total := 0
		for _, m := range f.docs {
			total += len(m)
		}
		return total, nil
	}
	return len(f.docs[key.String()]), nil
}

func (f *fakeIndex) Drop(_ context.Context) error {
	f.dropCalls++
	if f.dropErr != nil {
		return f.dropErr
	}
	f.docs = map[string]map[string]episodic.Record{}
	return nil
}

// fakeGraph is an in-memory graph.Store.
type fakeGraph struct {
	nodes     map[string]map[string]knowledge.Node
	edges     map[string][]knowledge.Edge
	pingErr   error
	upsertErr error
	// nodeCountOverride (>0) forces a verify mismatch.
	nodeCountOverride int
}

func newFakeGraph() *fakeGraph {
	return &fakeGraph{nodes: map[string]map[string]knowledge.Node{}, edges: map[string][]knowledge.Edge{}}
}

func (f *fakeGraph) Ping(_ context.Context) error { return f.pingErr }

func (f *fakeGraph) UpsertNodes(_ context.Context, key hotstore.ProjectKey, nodes []knowledge.Node) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	m := f.nodes[key.String()]
	if m == nil {
		m = map[string]knowledge.Node{}
		f.nodes[key.String()] = m
	}
	for _, n := range nodes {
		m[n.ID] = n
	}
	return nil
}

func (f *fakeGraph) UpsertEdges(_ context.Context, key hotstore.ProjectKey, edges []knowledge.Edge) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.edges[key.String()] = append(f.edges[key.String()], edges...)
	return nil
}

func (f *fakeGraph) DeleteNode(_ context.Context, key hotstore.ProjectKey, id string) error {
	delete(f.nodes[key.String()], id)
	return nil
}

func (f *fakeGraph) Search(_ context.Context, _ hotstore.ProjectKey, _ string, _ bool) ([]knowledge.Node, error) {
	return nil, nil
}

func (f *fakeGraph) Neighborhood(_ context.Context, _ hotstore.ProjectKey, _ string, _ int) (knowledge.Graph, error) {
	return knowledge.Graph{}, nil
}

func (f *fakeGraph) SupersedeChain(_ context.Context, _ hotstore.ProjectKey, _ string) ([]knowledge.Node, error) {
	return nil, nil
}

func (f *fakeGraph) NodeCount(_ context.Context, key hotstore.ProjectKey) (int, error) {
	if f.nodeCountOverride > 0 {
		return f.nodeCountOverride, nil
	}
	if (key == hotstore.ProjectKey{}) {
		total := 0
		for _, m := range f.nodes {
			total += len(m)
		}
		return total, nil
	}
	return len(f.nodes[key.String()]), nil
}

func (f *fakeGraph) Clear(_ context.Context) error {
	f.nodes = map[string]map[string]knowledge.Node{}
	f.edges = map[string][]knowledge.Edge{}
	return nil
}

// --- helpers ---------------------------------------------------------------

var testKey = hotstore.ProjectKey{Workspace: "ws", Team: "team", Project: "proj"}

func rec(id string, at time.Time) episodic.Record {
	return episodic.Record{ID: id, Kind: episodic.KindEvent, Actor: episodic.ActorAgent, Text: "t", OccurredAt: at, Entities: []string{}}
}

func node(id string) knowledge.Node {
	return knowledge.Node{ID: id, Kind: knowledge.KindFact, Name: "n", State: knowledge.StateActive, Trust: knowledge.TrustUserStated}
}

// --- tests -----------------------------------------------------------------

func TestCheckDrift(t *testing.T) {
	base := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		setup   func(*fakeStore, *fakeIndex, *fakeGraph)
		nilIdx  bool
		nilGra  bool
		want    DriftReport
		wantErr bool
	}{
		{
			name:  "clean system reports no drift",
			setup: func(s *fakeStore, i *fakeIndex, g *fakeGraph) {},
			want:  DriftReport{},
		},
		{
			name:   "nil deps are unavailable",
			setup:  func(s *fakeStore, i *fakeIndex, g *fakeGraph) {},
			nilIdx: true,
			nilGra: true,
			want: DriftReport{
				Episodic:  Drift{Unavailable: true, Reason: "opensearch not configured"},
				Knowledge: Drift{Unavailable: true, Reason: "neo4j not configured"},
			},
		},
		{
			name: "ping failures are unavailable",
			setup: func(s *fakeStore, i *fakeIndex, g *fakeGraph) {
				i.pingErr = search.ErrUnavailable
				g.pingErr = errors.New("down")
			},
			want: DriftReport{
				Episodic:  Drift{Unavailable: true, Reason: "opensearch unreachable"},
				Knowledge: Drift{Unavailable: true, Reason: "neo4j unreachable"},
			},
		},
		{
			name: "doc count mismatch detected",
			setup: func(s *fakeStore, i *fakeIndex, g *fakeGraph) {
				s.manifest.Files["episodic/ws/team/proj"] = hotstore.FileState{SHA256: "abc", RecordCount: 3}
				// index empty -> 0 != 3
			},
			want: DriftReport{
				Episodic: Drift{Detected: true, Reason: "indexed docs 0 != manifest records 3"},
			},
		},
		{
			name: "dirty episodic file detected",
			setup: func(s *fakeStore, i *fakeIndex, g *fakeGraph) {
				s.manifest.Files["episodic/ws/team/proj"] = hotstore.FileState{SHA256: "abc", RecordCount: 1, Dirty: true}
				i.docs[testKey.String()] = map[string]episodic.Record{"01AAAAAAAAAAAAAAAAAAAAAAAA": rec("01AAAAAAAAAAAAAAAAAAAAAAAA", base)}
			},
			want: DriftReport{
				Episodic: Drift{Detected: true, Reason: "1 dirty episodic file(s) await rehydration"},
			},
		},
		{
			name: "node count mismatch detected",
			setup: func(s *fakeStore, i *fakeIndex, g *fakeGraph) {
				s.graphs[testKey.String()] = knowledge.Graph{Nodes: []knowledge.Node{node("01AAAAAAAAAAAAAAAAAAAAAAAA")}}
			},
			want: DriftReport{
				Knowledge: Drift{Detected: true, Reason: "graph nodes 0 != hot nodes 1"},
			},
		},
		{
			name: "hydration sha mismatch detected",
			setup: func(s *fakeStore, i *fakeIndex, g *fakeGraph) {
				s.manifest.Files["episodic/ws/team/proj"] = hotstore.FileState{SHA256: "changed", RecordCount: 0}
				s.manifest.Indexes[IndexKeyEpisodic] = hotstore.IndexState{LastHydratedSHA: "stale"}
			},
			want: DriftReport{
				Episodic: Drift{Detected: true, Reason: "episodic hot content changed since last hydration"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakeStore()
			idx := newFakeIndex()
			gra := newFakeGraph()
			tt.setup(store, idx, gra)

			var idxDep search.Index
			if !tt.nilIdx {
				idxDep = idx
			}
			var graDep graph.Store
			if !tt.nilGra {
				graDep = gra
			}
			r := New(store, idxDep, graDep, &fakeClock{now: base})

			got, err := r.CheckDrift(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("CheckDrift error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("CheckDrift = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestRehydrateAll(t *testing.T) {
	base := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	store := newFakeStore()
	store.episodes[testKey.String()] = []episodic.Record{rec("01AAAAAAAAAAAAAAAAAAAAAAAA", base), rec("01AAAAAAAAAAAAAAAAAAAAAAAB", base)}
	store.graphs[testKey.String()] = knowledge.Graph{
		Nodes: []knowledge.Node{node("01AAAAAAAAAAAAAAAAAAAAAAAC")},
		Edges: []knowledge.Edge{{From: "01AAAAAAAAAAAAAAAAAAAAAAAC", To: "01AAAAAAAAAAAAAAAAAAAAAAAC", Rel: knowledge.RelRelatesTo}},
	}
	store.manifest.Files["episodic/ws/team/proj"] = hotstore.FileState{SHA256: "e1", RecordCount: 2, Dirty: true}
	store.manifest.Files["knowledge/ws/team/proj"] = hotstore.FileState{SHA256: "k1", RecordCount: 1, Dirty: true}

	idx := newFakeIndex()
	gra := newFakeGraph()
	r := New(store, idx, gra, &fakeClock{now: base})

	rep, err := r.RehydrateAll(context.Background(), false)
	if err != nil {
		t.Fatalf("RehydrateAll: %v", err)
	}
	if rep.EpisodesIndexed != 2 || rep.NodesUpserted != 1 || rep.EdgesUpserted != 1 {
		t.Errorf("report counts = %+v, want 2/1/1", rep)
	}
	if len(rep.Failures) != 0 {
		t.Errorf("unexpected failures: %v", rep.Failures)
	}
	if idx.dropCalls != 1 || idx.ensureCalls != 1 {
		t.Errorf("drop/ensure calls = %d/%d, want 1/1", idx.dropCalls, idx.ensureCalls)
	}
	if n := len(idx.docs[testKey.String()]); n != 2 {
		t.Errorf("indexed docs = %d, want 2", n)
	}
	// Manifest converged: dirty cleared, IndexedAt set, plane sha recorded.
	for _, fk := range []string{"episodic/ws/team/proj", "knowledge/ws/team/proj"} {
		fs := store.manifest.Files[fk]
		if fs.Dirty {
			t.Errorf("%s still dirty after rehydration", fk)
		}
		if !fs.IndexedAt.Equal(base) {
			t.Errorf("%s IndexedAt = %v, want %v", fk, fs.IndexedAt, base)
		}
	}
	if store.manifest.Indexes[IndexKeyEpisodic].LastHydratedSHA != PlaneStateSHA(store.manifest, hotstore.PlaneEpisodic) {
		t.Errorf("episodic hydration sha not recorded")
	}
	if store.manifest.Indexes[IndexKeyKnowledge].LastHydratedSHA != PlaneStateSHA(store.manifest, hotstore.PlaneKnowledge) {
		t.Errorf("knowledge hydration sha not recorded")
	}
	// And drift must now be clean.
	drift, err := r.CheckDrift(context.Background())
	if err != nil {
		t.Fatalf("CheckDrift after rehydrate: %v", err)
	}
	if drift.Episodic.Detected || drift.Knowledge.Detected {
		t.Errorf("drift after full rehydration: %+v", drift)
	}
}

func TestRehydrateAllDegraded(t *testing.T) {
	base := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	store := newFakeStore()
	store.episodes[testKey.String()] = []episodic.Record{rec("01AAAAAAAAAAAAAAAAAAAAAAAA", base)}
	store.manifest.Files["episodic/ws/team/proj"] = hotstore.FileState{SHA256: "e1", RecordCount: 1, Dirty: true}

	idx := newFakeIndex()
	idx.indexErr = search.ErrUnavailable
	r := New(store, idx, nil, &fakeClock{now: base})

	rep, err := r.RehydrateAll(context.Background(), false)
	if err != nil {
		t.Fatalf("RehydrateAll: %v", err)
	}
	if len(rep.Failures) != 2 {
		t.Fatalf("failures = %v, want index failure + nil graph failure", rep.Failures)
	}
	if !store.manifest.Files["episodic/ws/team/proj"].Dirty {
		t.Errorf("dirty flag cleared despite failed rehydration")
	}
	if _, ok := store.manifest.Indexes[IndexKeyEpisodic]; ok {
		t.Errorf("hydration sha recorded despite failure")
	}
}

func TestRehydrateAllVerifyMismatch(t *testing.T) {
	base := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	store := newFakeStore()
	store.episodes[testKey.String()] = []episodic.Record{rec("01AAAAAAAAAAAAAAAAAAAAAAAA", base)}

	idx := newFakeIndex()
	idx.countErr = errors.New("count broken")
	r := New(store, idx, newFakeGraph(), &fakeClock{now: base})

	rep, err := r.RehydrateAll(context.Background(), true)
	if err != nil {
		t.Fatalf("RehydrateAll: %v", err)
	}
	if !rep.Verified {
		t.Errorf("Verified = false, want true (audit ran)")
	}
	if len(rep.Failures) == 0 || !strings.Contains(rep.Failures[0], "verify") {
		t.Errorf("verify mismatch not reported: %v", rep.Failures)
	}
}

func TestRehydrateProject(t *testing.T) {
	base := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	other := hotstore.ProjectKey{Workspace: "ws", Team: "team", Project: "other"}

	store := newFakeStore()
	store.episodes[testKey.String()] = []episodic.Record{rec("01AAAAAAAAAAAAAAAAAAAAAAAA", base)}
	store.episodes[other.String()] = []episodic.Record{rec("01AAAAAAAAAAAAAAAAAAAAAAAB", base)}
	store.manifest.Files["episodic/ws/team/proj"] = hotstore.FileState{SHA256: "e1", RecordCount: 1, Dirty: true}
	store.manifest.Files["episodic/ws/team/other"] = hotstore.FileState{SHA256: "e2", RecordCount: 1, Dirty: true}

	idx := newFakeIndex()
	r := New(store, idx, newFakeGraph(), &fakeClock{now: base})

	rep, err := r.RehydrateProject(context.Background(), testKey)
	if err != nil {
		t.Fatalf("RehydrateProject: %v", err)
	}
	if rep.EpisodesIndexed != 1 {
		t.Errorf("EpisodesIndexed = %d, want 1", rep.EpisodesIndexed)
	}
	if len(idx.docs[other.String()]) != 0 {
		t.Errorf("partial rehydration touched another project")
	}
	if store.manifest.Files["episodic/ws/team/proj"].Dirty {
		t.Errorf("target project still dirty")
	}
	if !store.manifest.Files["episodic/ws/team/other"].Dirty {
		t.Errorf("other project dirty flag cleared by partial rehydration")
	}
}

func TestStatGate(t *testing.T) {
	base := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)

	t.Run("dirty file triggers partial rehydration once per debounce window", func(t *testing.T) {
		store := newFakeStore()
		store.episodes[testKey.String()] = []episodic.Record{rec("01AAAAAAAAAAAAAAAAAAAAAAAA", base)}
		store.manifest.Files["episodic/ws/team/proj"] = hotstore.FileState{SHA256: "e1", RecordCount: 1, Dirty: true}
		store.mtimes["episodic/ws/team/proj"] = base.Add(-time.Hour)

		clock := &fakeClock{now: base}
		idx := newFakeIndex()
		r := New(store, idx, newFakeGraph(), clock)

		if err := r.StatGate(context.Background(), testKey); err != nil {
			t.Fatalf("StatGate: %v", err)
		}
		if idx.indexCalls != 1 {
			t.Fatalf("indexCalls = %d, want 1", idx.indexCalls)
		}
		// Mark dirty again; within the 2s window nothing may happen.
		store.manifest.Files["episodic/ws/team/proj"] = hotstore.FileState{SHA256: "e1", RecordCount: 1, Dirty: true}
		clock.now = base.Add(time.Second)
		if err := r.StatGate(context.Background(), testKey); err != nil {
			t.Fatalf("StatGate (debounced): %v", err)
		}
		if idx.indexCalls != 1 {
			t.Errorf("debounce failed: indexCalls = %d, want 1", idx.indexCalls)
		}
		// After the window it converges again.
		clock.now = base.Add(3 * time.Second)
		if err := r.StatGate(context.Background(), testKey); err != nil {
			t.Fatalf("StatGate (post-debounce): %v", err)
		}
		if idx.indexCalls != 2 {
			t.Errorf("post-debounce indexCalls = %d, want 2", idx.indexCalls)
		}
	})

	t.Run("mtime newer than IndexedAt triggers rehydration", func(t *testing.T) {
		store := newFakeStore()
		store.episodes[testKey.String()] = []episodic.Record{rec("01AAAAAAAAAAAAAAAAAAAAAAAA", base)}
		store.manifest.Files["episodic/ws/team/proj"] = hotstore.FileState{SHA256: "e1", RecordCount: 1, IndexedAt: base.Add(-time.Minute)}
		store.mtimes["episodic/ws/team/proj"] = base // newer than IndexedAt

		idx := newFakeIndex()
		r := New(store, idx, newFakeGraph(), &fakeClock{now: base})
		if err := r.StatGate(context.Background(), testKey); err != nil {
			t.Fatalf("StatGate: %v", err)
		}
		if idx.indexCalls != 1 {
			t.Errorf("indexCalls = %d, want 1", idx.indexCalls)
		}
	})

	t.Run("fresh project is a no-op", func(t *testing.T) {
		store := newFakeStore()
		store.manifest.Files["episodic/ws/team/proj"] = hotstore.FileState{SHA256: "e1", RecordCount: 1, IndexedAt: base}
		store.mtimes["episodic/ws/team/proj"] = base.Add(-time.Minute)

		idx := newFakeIndex()
		r := New(store, idx, newFakeGraph(), &fakeClock{now: base})
		if err := r.StatGate(context.Background(), testKey); err != nil {
			t.Fatalf("StatGate: %v", err)
		}
		if idx.indexCalls != 0 {
			t.Errorf("indexCalls = %d, want 0 (no drift)", idx.indexCalls)
		}
	})
}

func TestPlaneStateSHA(t *testing.T) {
	m := hotstore.Manifest{Files: map[string]hotstore.FileState{
		"episodic/a/b/c":  {SHA256: "one"},
		"knowledge/a/b/c": {SHA256: "two"},
	}}
	ep := PlaneStateSHA(m, hotstore.PlaneEpisodic)
	kn := PlaneStateSHA(m, hotstore.PlaneKnowledge)
	if ep == kn {
		t.Errorf("plane digests must differ per plane content")
	}
	// Digest is deterministic and ignores the other plane.
	m2 := hotstore.Manifest{Files: map[string]hotstore.FileState{
		"episodic/a/b/c": {SHA256: "one"},
	}}
	if got := PlaneStateSHA(m2, hotstore.PlaneEpisodic); got != ep {
		t.Errorf("digest changed when unrelated plane was removed: %s != %s", got, ep)
	}
}
