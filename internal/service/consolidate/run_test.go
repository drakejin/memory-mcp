package consolidate

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/external/thirdparty/cold"
	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/service/knowledge"
	"github.com/drakejin/memory-mcp/internal/service/rehydrate"
	"github.com/drakejin/memory-mcp/internal/x/errs"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// calls is a shared ordered log so tests can assert the §4 aging order:
// S3 put confirmed → hot remove → index delete.
type calls struct{ log []string }

// fakeStore implements HotStore in memory.
type fakeStore struct {
	c            *calls
	episodes     map[string][]episode.Record
	graphs       map[string]knowledge.Graph
	dirty        map[string]bool
	manifest     rehydrate.Manifest
	removeErr    error
	listErr      error
	listProjErr  error
	manifestErr  error
	readErr      error
	manifestHits int
}

func newFakeStore(c *calls) *fakeStore {
	return &fakeStore{
		c:        c,
		episodes: map[string][]episode.Record{},
		graphs:   map[string]knowledge.Graph{},
		dirty:    map[string]bool{},
	}
}

func (s *fakeStore) append(key projectkey.Key, rec episode.Record) {
	s.episodes[key.String()] = append(s.episodes[key.String()], rec)
}

func (s *fakeStore) ListEpisodes(_ context.Context, key projectkey.Key) ([]episode.Record, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return slices.Clone(s.episodes[key.String()]), nil
}

func (s *fakeStore) RemoveEpisodes(_ context.Context, key projectkey.Key, ids []string) error {
	if s.removeErr != nil {
		return s.removeErr
	}
	s.c.log = append(s.c.log, "remove:"+strings.Join(ids, ","))
	kept := s.episodes[key.String()][:0:0]
	for _, rec := range s.episodes[key.String()] {
		if !slices.Contains(ids, rec.ID) {
			kept = append(kept, rec)
		}
	}
	s.episodes[key.String()] = kept
	return nil
}

func (s *fakeStore) ReadKnowledge(_ context.Context, key projectkey.Key) (knowledge.Graph, error) {
	return s.graphs[key.String()], nil
}

func (s *fakeStore) ListProjects(_ context.Context) ([]projectkey.Key, error) {
	if s.listProjErr != nil {
		return nil, s.listProjErr
	}
	names := make([]string, 0, len(s.episodes)+len(s.graphs))
	for k := range s.episodes {
		names = append(names, k)
	}
	for k := range s.graphs {
		names = append(names, k)
	}
	slices.Sort(names)
	names = slices.Compact(names)
	out := make([]projectkey.Key, 0, len(names))
	for _, name := range names {
		parts := strings.SplitN(name, "/", 3)
		out = append(out, projectkey.Key{Workspace: parts[0], Team: parts[1], Project: parts[2]})
	}
	return out, nil
}

func (s *fakeStore) Manifest(_ context.Context) (rehydrate.Manifest, error) {
	if s.readErr != nil {
		return rehydrate.Manifest{}, s.readErr
	}
	return s.manifest, nil
}

func (s *fakeStore) UpdateManifest(_ context.Context, fn func(rehydrate.Manifest) (rehydrate.Manifest, error)) error {
	s.manifestHits++
	if s.manifestErr != nil {
		return s.manifestErr
	}
	_, err := fn(rehydrate.Manifest{})
	return err
}

func (s *fakeStore) MarkDirty(_ context.Context, key projectkey.Key, plane rehydrate.Plane) error {
	s.dirty[string(plane)+"/"+key.String()] = true
	return nil
}

func (s *fakeStore) FileInfo(_ context.Context, key projectkey.Key, plane rehydrate.Plane) (int64, time.Time, error) {
	return 0, time.Time{}, errs.NotFound("fake.FileInfo", "hot_file", string(plane)+"/"+key.String())
}

// fakeIndexer implements EpisodeIndexer, recording deletions.
type fakeIndexer struct {
	c         *calls
	deleteErr error
	deleted   []string
}

func (f *fakeIndexer) DeleteRecords(_ context.Context, _ projectkey.Key, ids []string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.c.log = append(f.c.log, "index-delete:"+strings.Join(ids, ","))
	f.deleted = append(f.deleted, ids...)
	return nil
}

// fakeArchiver implements ColdArchiver, recording archive/snapshot calls.
type fakeArchiver struct {
	c           *calls
	archiveErr  error
	snapshotErr error
	archived    map[string][]episode.Record // s3Key -> records
	snapshots   []string
	username    string
}

func newFakeArchiver(c *calls) *fakeArchiver {
	return &fakeArchiver{c: c, archived: map[string][]episode.Record{}, username: "jin"}
}

func (f *fakeArchiver) ArchiveEpisodes(_ context.Context, key projectkey.Key, month string, recs []episode.Record) (string, error) {
	if f.archiveErr != nil {
		return "", f.archiveErr
	}
	s3Key := cold.EpisodeArchiveKey(f.username, key, month)
	f.archived[s3Key] = append(f.archived[s3Key], recs...)
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		ids = append(ids, r.ID)
	}
	f.c.log = append(f.c.log, "archive:"+strings.Join(ids, ","))
	return s3Key, nil
}

func (f *fakeArchiver) SnapshotKnowledge(_ context.Context, key projectkey.Key, _ knowledge.Graph, ts time.Time) (string, string, error) {
	if f.snapshotErr != nil {
		return "", "", f.snapshotErr
	}
	latest := cold.KnowledgeLatestKey(f.username, key)
	snap := cold.KnowledgeSnapshotKey(f.username, key, ts)
	f.c.log = append(f.c.log, "snapshot")
	f.snapshots = append(f.snapshots, latest, snap)
	return latest, snap, nil
}

type fakeClock struct{ t time.Time }

func (c fakeClock) Now() time.Time { return c.t }

// ---- fixtures --------------------------------------------------------------

var (
	runNow = time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	runKey = projectkey.Key{Workspace: "vms", Team: "core", Project: "memory"}
)

func seedProject(store *fakeStore) {
	for _, rec := range []episode.Record{
		crec("01AGEDJUN", runNow.AddDate(0, 0, -70), []string{"neo4j"}, true),      // 2026-06
		crec("01AGEDJUL", runNow.AddDate(0, 0, -40), []string{"opensearch"}, true), // 2026-07
		crec("01RECENT", runNow.AddDate(0, 0, -5), []string{"opensearch"}, true),
		crec("01UNCONS", runNow.AddDate(0, 0, -60), []string{"s3"}, false),
	} {
		store.append(runKey, rec)
	}
	store.graphs[runKey.String()] = knowledge.Graph{
		Nodes: []knowledge.Node{{ID: "01N", Kind: knowledge.KindFact, Name: "fact"}},
	}
}

// newTestService wires a Service the way main does, with nil-able derived deps.
func newTestService(t *testing.T, store HotStore, index EpisodeIndexer, archiver ColdArchiver) Service {
	t.Helper()
	svc, err := New(Config{
		Store:    store,
		Index:    index,
		Archiver: archiver,
		Clock:    fakeClock{t: runNow},
		TTLDays:  30,
	})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	return svc
}

// ---- construction ----------------------------------------------------------

func TestNewRejectsIncompleteConfig(t *testing.T) {
	store := newFakeStore(&calls{})
	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "missing store", cfg: Config{Clock: fakeClock{}, TTLDays: 30}},
		{name: "missing clock", cfg: Config{Store: store, TTLDays: 30}},
		{name: "zero ttl", cfg: Config{Store: store, Clock: fakeClock{}}},
		{name: "negative ttl", cfg: Config{Store: store, Clock: fakeClock{}, TTLDays: -1}},
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
	// A dead OpenSearch or S3 must not stop the pipeline from being built:
	// candidates and entity stats work without any derived store (§5).
	if _, err := New(Config{Store: newFakeStore(&calls{}), Clock: fakeClock{}, TTLDays: 1}); err != nil {
		t.Fatalf("New() = %v", err)
	}
}

// ---- Run -------------------------------------------------------------------

func TestRunHappyPath(t *testing.T) {
	c := &calls{}
	store := newFakeStore(c)
	index := &fakeIndexer{c: c}
	archiver := newFakeArchiver(c)
	seedProject(store)

	report, err := newTestService(t, store, index, archiver).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if report.MovedEpisodes != 2 {
		t.Errorf("moved = %d, want 2", report.MovedEpisodes)
	}
	wantKeys := []string{
		"jin/episodic/vms/core/memory/2026-06.json",
		"jin/episodic/vms/core/memory/2026-07.json",
	}
	if !slices.Equal(report.ArchiveKeys, wantKeys) {
		t.Errorf("archive keys = %v, want %v", report.ArchiveKeys, wantKeys)
	}
	if len(report.SnapshotKeys) != 2 {
		t.Errorf("snapshot keys = %v, want latest+snapshot", report.SnapshotKeys)
	}
	if len(report.Failures) != 0 {
		t.Errorf("failures = %v, want none", report.Failures)
	}

	// Hot retains exactly the recent consolidated and the old unconsolidated.
	var remaining []string
	for _, rec := range store.episodes[runKey.String()] {
		remaining = append(remaining, rec.ID)
	}
	slices.Sort(remaining)
	if !slices.Equal(remaining, []string{"01RECENT", "01UNCONS"}) {
		t.Errorf("remaining hot = %v", remaining)
	}

	// Index deletions match the aged ids.
	slices.Sort(index.deleted)
	if !slices.Equal(index.deleted, []string{"01AGEDJUL", "01AGEDJUN"}) {
		t.Errorf("index deleted = %v", index.deleted)
	}

	// Candidates cover only the unconsolidated episode.
	if len(report.Candidates) != 1 || !slices.Equal(report.Candidates[0].EpisodeIDs, []string{"01UNCONS"}) {
		t.Errorf("candidates = %+v", report.Candidates)
	}

	// Entity stats present and sorted.
	var names []string
	for _, s := range report.Entities {
		names = append(names, s.Entity)
	}
	if !slices.Equal(names, []string{"neo4j", "opensearch", "s3"}) {
		t.Errorf("entities = %v", names)
	}

	if store.manifestHits != 1 {
		t.Errorf("manifest updates = %d, want 1", store.manifestHits)
	}

	// Strict order per month: archive → remove → index-delete.
	for i := 0; i+2 < len(c.log); i += 3 {
		if !strings.HasPrefix(c.log[i], "archive:") {
			break
		}
		if !strings.HasPrefix(c.log[i+1], "remove:") || !strings.HasPrefix(c.log[i+2], "index-delete:") {
			t.Fatalf("aging order violated: %v", c.log)
		}
	}
}

func TestRunS3FailureBlocksLocalDeletion(t *testing.T) {
	c := &calls{}
	store := newFakeStore(c)
	index := &fakeIndexer{c: c}
	archiver := newFakeArchiver(c)
	archiver.archiveErr = errors.New("s3 down")
	seedProject(store)

	report, err := newTestService(t, store, index, archiver).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if report.MovedEpisodes != 0 {
		t.Errorf("moved = %d, want 0", report.MovedEpisodes)
	}
	if len(store.episodes[runKey.String()]) != 4 {
		t.Errorf("hot record count = %d, want all 4 retained", len(store.episodes[runKey.String()]))
	}
	if len(index.deleted) != 0 {
		t.Errorf("index deletions = %v, want none", index.deleted)
	}
	if len(report.Failures) == 0 {
		t.Error("expected archive failures to be reported")
	}
	for _, entry := range c.log {
		if strings.HasPrefix(entry, "remove:") || strings.HasPrefix(entry, "index-delete:") {
			t.Fatalf("local deletion happened despite S3 failure: %v", c.log)
		}
	}
}

func TestRunHotRemoveFailureSkipsIndexDelete(t *testing.T) {
	c := &calls{}
	store := newFakeStore(c)
	store.removeErr = errors.New("disk full")
	index := &fakeIndexer{c: c}
	archiver := newFakeArchiver(c)
	seedProject(store)

	report, err := newTestService(t, store, index, archiver).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.MovedEpisodes != 0 {
		t.Errorf("moved = %d, want 0 (removal failed)", report.MovedEpisodes)
	}
	if len(index.deleted) != 0 {
		t.Errorf("index deletions = %v, want none when hot removal failed", index.deleted)
	}
	if len(report.Failures) != 2 { // one per month batch
		t.Errorf("failures = %v, want 2 hot-remove failures", report.Failures)
	}
}

func TestRunIndexFailureIsDegraded(t *testing.T) {
	c := &calls{}
	store := newFakeStore(c)
	index := &fakeIndexer{c: c, deleteErr: errs.Unavailable("fake.DeleteRecords", nil)}
	archiver := newFakeArchiver(c)
	seedProject(store)

	report, err := newTestService(t, store, index, archiver).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.MovedEpisodes != 2 {
		t.Errorf("moved = %d, want 2 (aging proceeds; index is derived)", report.MovedEpisodes)
	}
	if len(report.Failures) != 2 {
		t.Errorf("failures = %v, want 2 index-delete failures", report.Failures)
	}
	if !store.dirty["episodic/"+runKey.String()] {
		t.Error("manifest must be marked dirty when index delete fails")
	}
}

func TestRunNilDerivedDeps(t *testing.T) {
	c := &calls{}
	store := newFakeStore(c)
	seedProject(store)

	report, err := newTestService(t, store, nil, nil).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.MovedEpisodes != 0 {
		t.Errorf("moved = %d, want 0 (no archiver, nothing may leave hot)", report.MovedEpisodes)
	}
	if len(store.episodes[runKey.String()]) != 4 {
		t.Errorf("hot record count = %d, want all retained", len(store.episodes[runKey.String()]))
	}
	if len(report.Failures) != 2 { // age + snapshot unavailable
		t.Errorf("failures = %v, want age+snapshot unavailability", report.Failures)
	}
	for _, f := range report.Failures {
		if !strings.Contains(f, msgColdUnavailable) {
			t.Errorf("failure %q must name the missing cold store", f)
		}
	}
	// Candidates and stats still work without any derived service.
	if len(report.Candidates) != 1 {
		t.Errorf("candidates = %+v", report.Candidates)
	}
}

func TestRunDryRun(t *testing.T) {
	c := &calls{}
	store := newFakeStore(c)
	index := &fakeIndexer{c: c}
	archiver := newFakeArchiver(c)
	seedProject(store)

	report, err := newTestService(t, store, index, archiver).Run(context.Background(), Options{DryRun: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.MovedEpisodes != 2 {
		t.Errorf("dry-run moved = %d, want 2 (what WOULD age)", report.MovedEpisodes)
	}
	if len(c.log) != 0 {
		t.Errorf("dry run must not touch storage/index, log = %v", c.log)
	}
	if len(store.episodes[runKey.String()]) != 4 {
		t.Error("dry run must not remove hot records")
	}
	if store.manifestHits != 0 {
		t.Error("dry run must not rewrite the manifest")
	}
	if len(report.ArchiveKeys) != 0 || len(report.SnapshotKeys) != 0 {
		t.Errorf("dry run wrote keys: %v %v", report.ArchiveKeys, report.SnapshotKeys)
	}
}

func TestRunScopedToRequestedProjects(t *testing.T) {
	c := &calls{}
	store := newFakeStore(c)
	index := &fakeIndexer{c: c}
	archiver := newFakeArchiver(c)
	seedProject(store)

	otherKey := projectkey.Key{Workspace: "vms", Team: "core", Project: "other"}
	store.append(otherKey, crec("01OTHEROLD", runNow.AddDate(0, 0, -40), nil, true))

	report, err := newTestService(t, store, index, archiver).Run(context.Background(), Options{
		Projects: []projectkey.Key{otherKey},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.MovedEpisodes != 1 {
		t.Errorf("moved = %d, want 1 (only the scoped project)", report.MovedEpisodes)
	}
	if len(store.episodes[runKey.String()]) != 4 {
		t.Error("unscoped project must be untouched")
	}
	if len(store.episodes[otherKey.String()]) != 0 {
		t.Error("scoped project's aged record must be removed from hot")
	}
}

func TestRunProjectListingFailureAborts(t *testing.T) {
	c := &calls{}
	store := newFakeStore(c)
	seedProject(store)
	store.listProjErr = errs.Internal("fake.ListProjects", errors.New("disk gone"))

	_, err := newTestService(t, store, &fakeIndexer{c: c}, newFakeArchiver(c)).Run(context.Background(), Options{})
	if err == nil {
		t.Fatal("Run must fail when projects cannot be listed at all")
	}
	if !errors.Is(err, errs.ErrInternal) {
		t.Errorf("Run error = %v, want the store's kind preserved", err)
	}
}

func TestRunPerProjectListingFailureIsReported(t *testing.T) {
	c := &calls{}
	store := newFakeStore(c)
	seedProject(store)
	store.listErr = errs.Unavailable("fake.ListEpisodes", nil)

	report, err := newTestService(t, store, &fakeIndexer{c: c}, newFakeArchiver(c)).
		Run(context.Background(), Options{Projects: []projectkey.Key{runKey}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Failures) != 1 || !strings.HasPrefix(report.Failures[0], stepList+": "+runKey.String()) {
		t.Fatalf("failures = %v, want one list failure naming the project", report.Failures)
	}
}

func TestRunManifestFailureIsReported(t *testing.T) {
	c := &calls{}
	store := newFakeStore(c)
	store.manifestErr = errors.New("manifest write failed")
	seedProject(store)

	report, err := newTestService(t, store, &fakeIndexer{c: c}, newFakeArchiver(c)).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !slices.ContainsFunc(report.Failures, func(f string) bool { return strings.HasPrefix(f, stepManifest+": ") }) {
		t.Errorf("failures = %v, want the manifest failure disclosed", report.Failures)
	}
}

func TestRunSnapshotFailureIsReported(t *testing.T) {
	c := &calls{}
	store := newFakeStore(c)
	archiver := newFakeArchiver(c)
	archiver.snapshotErr = errors.New("s3 snapshot rejected")
	seedProject(store)

	report, err := newTestService(t, store, &fakeIndexer{c: c}, archiver).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.SnapshotKeys) != 0 {
		t.Errorf("snapshot keys = %v, want none when the upload failed", report.SnapshotKeys)
	}
	if !slices.ContainsFunc(report.Failures, func(f string) bool { return strings.HasPrefix(f, stepSnapshot+": ") }) {
		t.Errorf("failures = %v, want the snapshot failure disclosed", report.Failures)
	}
}

func TestRunEmptyStoreProducesEmptyReport(t *testing.T) {
	c := &calls{}
	report, err := newTestService(t, newFakeStore(c), &fakeIndexer{c: c}, newFakeArchiver(c)).
		Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Non-nil slices keep the JSON envelope honest: [] not null.
	if report.Candidates == nil || report.Entities == nil ||
		report.ArchiveKeys == nil || report.SnapshotKeys == nil || report.Failures == nil {
		t.Errorf("report has nil slices: %+v", report)
	}
}
