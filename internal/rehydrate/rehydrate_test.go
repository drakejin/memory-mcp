package rehydrate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/errs"
	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/knowledge"
)

// --- fakes -----------------------------------------------------------------

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

// fakeStore is an in-memory HotStore.
type fakeStore struct {
	episodes map[string][]episodic.Record
	graphs   map[string]knowledge.Graph
	manifest hotstore.Manifest
	mtimes   map[string]time.Time // manifest file key -> mtime
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

func (s *fakeStore) ListEpisodes(_ context.Context, key hotstore.ProjectKey) ([]episodic.Record, error) {
	if s.episodeListErr != nil {
		return nil, s.episodeListErr
	}
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.episodes[key.String()], nil
}

func (s *fakeStore) ReadKnowledge(_ context.Context, key hotstore.ProjectKey) (knowledge.Graph, error) {
	if s.readKnowledgeErr != nil {
		return knowledge.Graph{}, s.readKnowledgeErr
	}
	return s.graphs[key.String()], nil
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

func (s *fakeStore) FileInfo(_ context.Context, key hotstore.ProjectKey, plane hotstore.Plane) (int64, time.Time, error) {
	fk := hotstore.ManifestFileKey(plane, key)
	mt, ok := s.mtimes[fk]
	if !ok {
		return 0, time.Time{}, errs.NotFound("fake.FileInfo", "hot_file", fk)
	}
	return 1, mt, nil
}

// fakeIndex is an in-memory EpisodeIndex.
type fakeIndex struct {
	docs             map[string]map[string]episodic.Record // project -> id -> rec
	pingErr          error
	indexErr         error
	dropErr          error
	deleteProjectErr error
	countErr         error
	ensureErr        error
	indexCalls       int
	dropCalls        int
	deleteProjectIDs []string // project keys passed to DeleteProject, in order
	ensureCalls      int
}

func newFakeIndex() *fakeIndex {
	return &fakeIndex{docs: map[string]map[string]episodic.Record{}}
}

func (f *fakeIndex) Ping(_ context.Context) error { return f.pingErr }

func (f *fakeIndex) EnsureIndex(_ context.Context) error {
	f.ensureCalls++
	return f.ensureErr
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

// DeleteProject drops one project's documents, like the real delete-by-query.
func (f *fakeIndex) DeleteProject(_ context.Context, key hotstore.ProjectKey) error {
	f.deleteProjectIDs = append(f.deleteProjectIDs, key.String())
	if f.deleteProjectErr != nil {
		return f.deleteProjectErr
	}
	delete(f.docs, key.String())
	return nil
}

func (f *fakeIndex) Drop(_ context.Context) error {
	f.dropCalls++
	if f.dropErr != nil {
		return f.dropErr
	}
	f.docs = map[string]map[string]episodic.Record{}
	return nil
}

// fakeGraph is an in-memory KnowledgeGraph.
type fakeGraph struct {
	nodes            map[string]map[string]knowledge.Node
	edges            map[string][]knowledge.Edge
	pingErr          error
	upsertErr        error
	deleteMissingErr error
	// deleteMissingKeep records the keep-list of each DeleteMissing call.
	deleteMissingKeep [][]string
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

// DeleteMissing drops this project's nodes whose id is not in keep, and the
// edges incident to them — the DETACH DELETE of the real client.
func (f *fakeGraph) DeleteMissing(_ context.Context, key hotstore.ProjectKey, keep []string) error {
	f.deleteMissingKeep = append(f.deleteMissingKeep, keep)
	if f.deleteMissingErr != nil {
		return f.deleteMissingErr
	}
	kept := make(map[string]bool, len(keep))
	for _, id := range keep {
		kept[id] = true
	}
	for id := range f.nodes[key.String()] {
		if !kept[id] {
			delete(f.nodes[key.String()], id)
		}
	}
	edges := f.edges[key.String()][:0:0]
	for _, e := range f.edges[key.String()] {
		if kept[e.From] && kept[e.To] {
			edges = append(edges, e)
		}
	}
	f.edges[key.String()] = edges
	return nil
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

// newService builds the concrete service so tests can reach the unexported
// drift helpers; production callers only ever see the Service interface.
func newService(t *testing.T, store HotStore, index EpisodeIndex, gr KnowledgeGraph, clock Clock) *service {
	t.Helper()
	svc, err := New(Config{Store: store, Index: index, Graph: gr, Clock: clock})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	return svc.(*service)
}

func rec(id string, at time.Time) episodic.Record {
	return episodic.Record{ID: id, Kind: episodic.KindEvent, Actor: episodic.ActorAgent, Text: "t", OccurredAt: at, Entities: []string{}}
}

func node(id string) knowledge.Node {
	return knowledge.Node{ID: id, Kind: knowledge.KindFact, Name: "n", State: knowledge.StateActive, Trust: knowledge.TrustUserStated}
}

// --- construction ----------------------------------------------------------

func TestNewRejectsIncompleteConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "missing store", cfg: Config{Clock: &fakeClock{}}},
		{name: "missing clock", cfg: Config{Store: newFakeStore()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, err := New(tt.cfg)
			if err == nil {
				t.Fatal("New() = nil error, want invalid config")
			}
			if !errors.Is(err, errs.ErrInvalid) {
				t.Errorf("New() error = %v, want kind invalid", err)
			}
			if svc != nil {
				t.Error("New() must not return a service alongside an error")
			}
		})
	}
}

func TestNewAcceptsNilDerivedStores(t *testing.T) {
	// Both derived stores may be down at boot; the server must still build a
	// rehydrator so /status can report the outage (§5).
	if _, err := New(Config{Store: newFakeStore(), Clock: &fakeClock{}}); err != nil {
		t.Fatalf("New() = %v", err)
	}
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
				Episodic:  Drift{Unavailable: true, Reason: reasonIndexNotConfigured},
				Knowledge: Drift{Unavailable: true, Reason: reasonGraphNotConfigured},
			},
		},
		{
			name: "ping failures are unavailable",
			setup: func(s *fakeStore, i *fakeIndex, g *fakeGraph) {
				i.pingErr = errs.Unavailable("fake.Ping", nil)
				g.pingErr = errors.New("down")
			},
			want: DriftReport{
				Episodic:  Drift{Unavailable: true, Reason: reasonIndexUnreachable},
				Knowledge: Drift{Unavailable: true, Reason: reasonGraphUnreachable},
			},
		},
		{
			name: "uncountable index is drift, not unavailability",
			setup: func(s *fakeStore, i *fakeIndex, g *fakeGraph) {
				i.countErr = errors.New("index_not_found_exception")
			},
			want: DriftReport{
				Episodic: Drift{Detected: true, Reason: reasonIndexAbsent},
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
			name: "dirty knowledge file detected",
			setup: func(s *fakeStore, i *fakeIndex, g *fakeGraph) {
				s.manifest.Files["knowledge/ws/team/proj"] = hotstore.FileState{SHA256: "k", RecordCount: 0, Dirty: true}
			},
			want: DriftReport{
				Knowledge: Drift{Detected: true, Reason: "1 dirty knowledge file(s) await rehydration"},
			},
		},
		{
			name: "hydration sha mismatch detected",
			setup: func(s *fakeStore, i *fakeIndex, g *fakeGraph) {
				s.manifest.Files["episodic/ws/team/proj"] = hotstore.FileState{SHA256: "changed", RecordCount: 0}
				s.manifest.Indexes[indexKeyEpisodic] = hotstore.IndexState{LastHydratedSHA: "stale"}
			},
			want: DriftReport{
				Episodic: Drift{Detected: true, Reason: reasonEpisodicChanged},
			},
		},
		{
			name: "knowledge hydration sha mismatch detected",
			setup: func(s *fakeStore, i *fakeIndex, g *fakeGraph) {
				s.manifest.Files["knowledge/ws/team/proj"] = hotstore.FileState{SHA256: "changed"}
				s.manifest.Indexes[indexKeyKnowledge] = hotstore.IndexState{LastHydratedSHA: "stale"}
			},
			want: DriftReport{
				Knowledge: Drift{Detected: true, Reason: reasonKnowledgeChanged},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakeStore()
			idx := newFakeIndex()
			gra := newFakeGraph()
			tt.setup(store, idx, gra)

			var idxDep EpisodeIndex
			if !tt.nilIdx {
				idxDep = idx
			}
			var graDep KnowledgeGraph
			if !tt.nilGra {
				graDep = gra
			}
			r := newService(t, store, idxDep, graDep, &fakeClock{now: base})

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
	r := newService(t, store, idx, gra, &fakeClock{now: base})

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
	if store.manifest.Indexes[indexKeyEpisodic].LastHydratedSHA != PlaneStateSHA(store.manifest, hotstore.PlaneEpisodic) {
		t.Errorf("episodic hydration sha not recorded")
	}
	if store.manifest.Indexes[indexKeyKnowledge].LastHydratedSHA != PlaneStateSHA(store.manifest, hotstore.PlaneKnowledge) {
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
	idx.indexErr = errs.Unavailable("fake.IndexRecords", nil)
	r := newService(t, store, idx, nil, &fakeClock{now: base})

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
	if _, ok := store.manifest.Indexes[indexKeyEpisodic]; ok {
		t.Errorf("hydration sha recorded despite failure")
	}
}

func TestRehydrateAllManifestWriteFailureIsReported(t *testing.T) {
	base := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	store := newFakeStore()
	store.episodes[testKey.String()] = []episodic.Record{rec("01AAAAAAAAAAAAAAAAAAAAAAAA", base)}
	store.failWrite = true
	r := newService(t, store, newFakeIndex(), newFakeGraph(), &fakeClock{now: base})

	rep, err := r.RehydrateAll(context.Background(), false)
	if err != nil {
		t.Fatalf("RehydrateAll: %v", err)
	}
	if !hasFailureContaining(rep.Failures, "manifest update") {
		t.Errorf("failures = %v, want the manifest write failure disclosed", rep.Failures)
	}
}

func TestRehydrateAllVerifyMismatch(t *testing.T) {
	base := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	store := newFakeStore()
	store.episodes[testKey.String()] = []episodic.Record{rec("01AAAAAAAAAAAAAAAAAAAAAAAA", base)}

	idx := newFakeIndex()
	idx.countErr = errors.New("count broken")
	r := newService(t, store, idx, newFakeGraph(), &fakeClock{now: base})

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
	r := newService(t, store, idx, newFakeGraph(), &fakeClock{now: base})

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

// The partial path clears the dirty flag it converged, so it must actually
// converge removals too. IndexRecords alone can only add or update: an episode
// that left hot — aged to cold after §3.1, or dropped by a consolidation whose
// index delete failed — would otherwise stay searchable forever with nothing
// left flagged to fix it (§3.1: search covers the hot range only).
func TestRehydrateProjectDropsEpisodesNoLongerHot(t *testing.T) {
	// Arrange: the index still holds an aged-out record the hot file no longer
	// has, exactly the state consolidate leaves behind when DeleteRecords fails.
	base := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	const survivor, aged = "01AAAAAAAAAAAAAAAAAAAAAAAA", "01AAAAAAAAAAAAAAAAAAAAAAAZ"
	other := hotstore.ProjectKey{Workspace: "ws", Team: "team", Project: "other"}

	store := newFakeStore()
	store.episodes[testKey.String()] = []episodic.Record{rec(survivor, base)}
	store.manifest.Files["episodic/ws/team/proj"] = hotstore.FileState{SHA256: "e1", RecordCount: 1, Dirty: true}

	idx := newFakeIndex()
	idx.docs[testKey.String()] = map[string]episodic.Record{
		survivor: rec(survivor, base),
		aged:     rec(aged, base),
	}
	idx.docs[other.String()] = map[string]episodic.Record{"01BBBBBBBBBBBBBBBBBBBBBBBB": rec("01BBBBBBBBBBBBBBBBBBBBBBBB", base)}
	r := newService(t, store, idx, newFakeGraph(), &fakeClock{now: base})

	// Act
	if _, err := r.RehydrateProject(context.Background(), testKey); err != nil {
		t.Fatalf("RehydrateProject: %v", err)
	}

	// Assert
	if _, still := idx.docs[testKey.String()][aged]; still {
		t.Errorf("episode %s is not in hot but survived rehydration — it stays searchable while the manifest says clean", aged)
	}
	if _, ok := idx.docs[testKey.String()][survivor]; !ok {
		t.Errorf("hot episode %s was not replayed into the index", survivor)
	}
	if len(idx.docs[other.String()]) != 1 {
		t.Errorf("partial rehydration deleted another project's documents: %v", idx.docs[other.String()])
	}
	if store.manifest.Files["episodic/ws/team/proj"].Dirty {
		t.Errorf("dirty flag still set after a converging pass")
	}
}

// A project whose every episode aged out still has stale index documents, so
// "no hot records" must not short-circuit the delete.
func TestRehydrateProjectWithEmptyHotStillClearsIndex(t *testing.T) {
	// Arrange
	base := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	store := newFakeStore()
	store.episodes[testKey.String()] = nil
	store.manifest.Files["episodic/ws/team/proj"] = hotstore.FileState{SHA256: "e1", Dirty: true}

	idx := newFakeIndex()
	idx.docs[testKey.String()] = map[string]episodic.Record{"01AAAAAAAAAAAAAAAAAAAAAAAZ": rec("01AAAAAAAAAAAAAAAAAAAAAAAZ", base)}
	r := newService(t, store, idx, newFakeGraph(), &fakeClock{now: base})

	// Act
	if _, err := r.RehydrateProject(context.Background(), testKey); err != nil {
		t.Fatalf("RehydrateProject: %v", err)
	}

	// Assert
	if n := len(idx.docs[testKey.String()]); n != 0 {
		t.Errorf("index holds %d documents for a project with no hot records, want 0", n)
	}
}

// The knowledge counterpart: MERGE replay cannot remove, so a node purged from
// hot whose live Neo4j delete failed would survive every replay while the dirty
// flag that recorded the failure gets cleared — permanent graph-only content,
// which §0 principle 1 forbids.
func TestRehydrateProjectDropsNodesNoLongerHot(t *testing.T) {
	// Arrange
	base := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	const survivor, purged = "01AAAAAAAAAAAAAAAAAAAAAAAA", "01AAAAAAAAAAAAAAAAAAAAAAAZ"
	other := hotstore.ProjectKey{Workspace: "ws", Team: "team", Project: "other"}

	store := newFakeStore()
	store.graphs[testKey.String()] = knowledge.Graph{Nodes: []knowledge.Node{node(survivor)}}
	store.manifest.Files["knowledge/ws/team/proj"] = hotstore.FileState{SHA256: "k1", RecordCount: 1, Dirty: true}

	gr := newFakeGraph()
	gr.nodes[testKey.String()] = map[string]knowledge.Node{survivor: node(survivor), purged: node(purged)}
	gr.nodes[other.String()] = map[string]knowledge.Node{"01BBBBBBBBBBBBBBBBBBBBBBBB": node("01BBBBBBBBBBBBBBBBBBBBBBBB")}
	r := newService(t, store, newFakeIndex(), gr, &fakeClock{now: base})

	// Act
	if _, err := r.RehydrateProject(context.Background(), testKey); err != nil {
		t.Fatalf("RehydrateProject: %v", err)
	}

	// Assert
	if _, still := gr.nodes[testKey.String()][purged]; still {
		t.Errorf("node %s is not in hot but survived rehydration", purged)
	}
	if _, ok := gr.nodes[testKey.String()][survivor]; !ok {
		t.Errorf("hot node %s was not replayed into the graph", survivor)
	}
	if len(gr.nodes[other.String()]) != 1 {
		t.Errorf("partial rehydration deleted another project's nodes: %v", gr.nodes[other.String()])
	}
	if store.manifest.Files["knowledge/ws/team/proj"].Dirty {
		t.Errorf("dirty flag still set after a converging pass")
	}
}

// A failed reconciliation must leave the dirty flag standing: clearing it would
// throw away the only signal that the plane still has to converge.
func TestRehydrateProjectKeepsDirtyWhenDeleteFails(t *testing.T) {
	tests := []struct {
		name    string
		fileKey string
		arrange func(*fakeStore, *fakeIndex, *fakeGraph)
	}{
		{
			name:    "episodic delete fails",
			fileKey: "episodic/ws/team/proj",
			arrange: func(s *fakeStore, i *fakeIndex, _ *fakeGraph) {
				s.episodes[testKey.String()] = []episodic.Record{rec("01AAAAAAAAAAAAAAAAAAAAAAAA", time.Time{})}
				i.deleteProjectErr = errors.New("opensearch down")
			},
		},
		{
			name:    "knowledge reconcile fails",
			fileKey: "knowledge/ws/team/proj",
			arrange: func(s *fakeStore, _ *fakeIndex, g *fakeGraph) {
				s.graphs[testKey.String()] = knowledge.Graph{Nodes: []knowledge.Node{node("01AAAAAAAAAAAAAAAAAAAAAAAA")}}
				g.deleteMissingErr = errors.New("neo4j down")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			base := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
			store, idx, gr := newFakeStore(), newFakeIndex(), newFakeGraph()
			tt.arrange(store, idx, gr)
			store.manifest.Files[tt.fileKey] = hotstore.FileState{SHA256: "s", RecordCount: 1, Dirty: true}
			r := newService(t, store, idx, gr, &fakeClock{now: base})

			// Act
			rep, err := r.RehydrateProject(context.Background(), testKey)

			// Assert
			if err == nil {
				t.Fatal("RehydrateProject returned nil error after a failed reconciliation")
			}
			if len(rep.Failures) == 0 {
				t.Error("Report.Failures is empty — the failure was not reported")
			}
			if !store.manifest.Files[tt.fileKey].Dirty {
				t.Errorf("%s dirty flag cleared although the pass never converged", tt.fileKey)
			}
		})
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
		r := newService(t, store, idx, newFakeGraph(), clock)

		if err := r.StatGate(context.Background(), testKey); err != nil {
			t.Fatalf("StatGate: %v", err)
		}
		if idx.indexCalls != 1 {
			t.Fatalf("indexCalls = %d, want 1", idx.indexCalls)
		}
		// Mark dirty again; within the debounce window nothing may happen.
		store.manifest.Files["episodic/ws/team/proj"] = hotstore.FileState{SHA256: "e1", RecordCount: 1, Dirty: true}
		clock.now = base.Add(debounceInterval / 2)
		if err := r.StatGate(context.Background(), testKey); err != nil {
			t.Fatalf("StatGate (debounced): %v", err)
		}
		if idx.indexCalls != 1 {
			t.Errorf("debounce failed: indexCalls = %d, want 1", idx.indexCalls)
		}
		// After the window it converges again.
		clock.now = base.Add(debounceInterval + time.Second)
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
		r := newService(t, store, idx, newFakeGraph(), &fakeClock{now: base})
		if err := r.StatGate(context.Background(), testKey); err != nil {
			t.Fatalf("StatGate: %v", err)
		}
		if idx.indexCalls != 1 {
			t.Errorf("indexCalls = %d, want 1", idx.indexCalls)
		}
	})

	t.Run("untracked hot file triggers rehydration", func(t *testing.T) {
		store := newFakeStore()
		store.episodes[testKey.String()] = []episodic.Record{rec("01AAAAAAAAAAAAAAAAAAAAAAAA", base)}
		store.mtimes["episodic/ws/team/proj"] = base // present on disk, absent from the manifest

		idx := newFakeIndex()
		r := newService(t, store, idx, newFakeGraph(), &fakeClock{now: base})
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
		r := newService(t, store, idx, newFakeGraph(), &fakeClock{now: base})
		if err := r.StatGate(context.Background(), testKey); err != nil {
			t.Fatalf("StatGate: %v", err)
		}
		if idx.indexCalls != 0 {
			t.Errorf("indexCalls = %d, want 0 (no drift)", idx.indexCalls)
		}
	})

	t.Run("a failing rehydration surfaces as unavailable", func(t *testing.T) {
		store := newFakeStore()
		store.episodes[testKey.String()] = []episodic.Record{rec("01AAAAAAAAAAAAAAAAAAAAAAAA", base)}
		store.manifest.Files["episodic/ws/team/proj"] = hotstore.FileState{SHA256: "e1", RecordCount: 1, Dirty: true}
		store.mtimes["episodic/ws/team/proj"] = base

		r := newService(t, store, nil, nil, &fakeClock{now: base})
		err := r.StatGate(context.Background(), testKey)
		if err == nil {
			t.Fatal("StatGate must report a rehydration that could not run")
		}
		if !errors.Is(err, errs.ErrUnavailable) {
			t.Errorf("StatGate error = %v, want kind unavailable (degraded signal)", err)
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
