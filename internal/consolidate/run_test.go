package consolidate

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/cold"
	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/knowledge"
	"github.com/drakejin/memory-mcp/internal/search"
)

// calls is a shared ordered log so tests can assert the §4 aging order:
// S3 put confirmed → hot remove → index delete.
type calls struct{ log []string }

// fakeRunStore is an in-memory hotstore.Store for Runner tests.
type fakeRunStore struct {
	c            *calls
	episodes     map[string][]episodic.Record
	graphs       map[string]knowledge.Graph
	dirty        map[string]bool
	removeErr    error
	manifestHits int
}

func newFakeRunStore(c *calls) *fakeRunStore {
	return &fakeRunStore{
		c:        c,
		episodes: map[string][]episodic.Record{},
		graphs:   map[string]knowledge.Graph{},
		dirty:    map[string]bool{},
	}
}

func (s *fakeRunStore) AppendEpisode(_ context.Context, key hotstore.ProjectKey, rec episodic.Record) error {
	s.episodes[key.String()] = append(s.episodes[key.String()], rec)
	return nil
}

func (s *fakeRunStore) ListEpisodes(_ context.Context, key hotstore.ProjectKey) ([]episodic.Record, error) {
	return slices.Clone(s.episodes[key.String()]), nil
}

func (s *fakeRunStore) GetEpisode(_ context.Context, _ hotstore.ProjectKey, _ string) (episodic.Record, error) {
	return episodic.Record{}, hotstore.ErrNotFound
}

func (s *fakeRunStore) UpdateEpisodes(_ context.Context, _ hotstore.ProjectKey, _ []string, _ func(episodic.Record) episodic.Record) error {
	return nil
}

func (s *fakeRunStore) RemoveEpisodes(_ context.Context, key hotstore.ProjectKey, ids []string) error {
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

func (s *fakeRunStore) ReadKnowledge(_ context.Context, key hotstore.ProjectKey) (knowledge.Graph, error) {
	return s.graphs[key.String()], nil
}

func (s *fakeRunStore) WriteKnowledge(_ context.Context, key hotstore.ProjectKey, g knowledge.Graph) error {
	s.graphs[key.String()] = g
	return nil
}

func (s *fakeRunStore) ListProjects(_ context.Context) ([]hotstore.ProjectKey, error) {
	seen := map[string]bool{}
	var out []hotstore.ProjectKey
	add := func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		parts := strings.SplitN(name, "/", 3)
		out = append(out, hotstore.ProjectKey{Workspace: parts[0], Team: parts[1], Project: parts[2]})
	}
	var names []string
	for k := range s.episodes {
		names = append(names, k)
	}
	for k := range s.graphs {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, n := range names {
		add(n)
	}
	return out, nil
}

func (s *fakeRunStore) Manifest(_ context.Context) (hotstore.Manifest, error) {
	return hotstore.Manifest{}, nil
}

func (s *fakeRunStore) UpdateManifest(_ context.Context, fn func(hotstore.Manifest) (hotstore.Manifest, error)) error {
	s.manifestHits++
	_, err := fn(hotstore.Manifest{})
	return err
}

func (s *fakeRunStore) MarkDirty(_ context.Context, key hotstore.ProjectKey, plane hotstore.Plane) error {
	s.dirty[string(plane)+"/"+key.String()] = true
	return nil
}

func (s *fakeRunStore) FileInfo(_ context.Context, _ hotstore.ProjectKey, _ hotstore.Plane) (int64, time.Time, error) {
	return 0, time.Time{}, hotstore.ErrNotFound
}

// fakeRunIndex is a minimal search.Index recording deletions.
type fakeRunIndex struct {
	c         *calls
	deleteErr error
	deleted   []string
}

func (f *fakeRunIndex) Ping(context.Context) error        { return nil }
func (f *fakeRunIndex) EnsureIndex(context.Context) error { return nil }
func (f *fakeRunIndex) IndexRecords(context.Context, hotstore.ProjectKey, []episodic.Record) error {
	return nil
}

func (f *fakeRunIndex) DeleteRecords(_ context.Context, _ hotstore.ProjectKey, ids []string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.c.log = append(f.c.log, "index-delete:"+strings.Join(ids, ","))
	f.deleted = append(f.deleted, ids...)
	return nil
}

func (f *fakeRunIndex) Search(context.Context, hotstore.ProjectKey, search.Query) ([]search.Hit, error) {
	return nil, nil
}
func (f *fakeRunIndex) DocCount(context.Context, hotstore.ProjectKey) (int, error) { return 0, nil }
func (f *fakeRunIndex) Drop(context.Context) error                                 { return nil }

// fakeRunArchiver is a minimal cold.Archiver recording archive/snapshot calls.
type fakeRunArchiver struct {
	c           *calls
	archiveErr  error
	snapshotErr error
	archived    map[string][]episodic.Record // s3Key -> records
	snapshots   []string
	username    string
}

func newFakeRunArchiver(c *calls) *fakeRunArchiver {
	return &fakeRunArchiver{c: c, archived: map[string][]episodic.Record{}, username: "jin"}
}

func (f *fakeRunArchiver) ArchiveEpisodes(_ context.Context, key hotstore.ProjectKey, month string, recs []episodic.Record) (string, error) {
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

func (f *fakeRunArchiver) FetchArchivedEpisode(context.Context, hotstore.ProjectKey, string) (episodic.Record, error) {
	return episodic.Record{}, cold.ErrNotFound
}

func (f *fakeRunArchiver) SnapshotKnowledge(_ context.Context, key hotstore.ProjectKey, _ knowledge.Graph, ts time.Time) (string, string, error) {
	if f.snapshotErr != nil {
		return "", "", f.snapshotErr
	}
	latest := cold.KnowledgeLatestKey(f.username, key)
	snap := cold.KnowledgeSnapshotKey(f.username, key, ts)
	f.c.log = append(f.c.log, "snapshot")
	f.snapshots = append(f.snapshots, latest, snap)
	return latest, snap, nil
}

func (f *fakeRunArchiver) UploadBlob(context.Context, string, io.Reader) (string, error) {
	return "", errors.New("unused")
}

func (f *fakeRunArchiver) FetchBlob(context.Context, string) (io.ReadCloser, error) {
	return nil, cold.ErrNotFound
}

func (f *fakeRunArchiver) BlobExists(context.Context, string) (bool, error) { return false, nil }

type runClock struct{ t time.Time }

func (c runClock) Now() time.Time { return c.t }

// ---- fixtures --------------------------------------------------------------

var (
	runNow = time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	runKey = hotstore.ProjectKey{Workspace: "vms", Team: "core", Project: "memory"}
)

func seedProject(store *fakeRunStore) (agedOld, agedJune, keptRecent, keptUncons episodic.Record) {
	agedOld = crec("01AGEDJUL", runNow.AddDate(0, 0, -40), []string{"opensearch"}, true) // 2026-07
	agedJune = crec("01AGEDJUN", runNow.AddDate(0, 0, -70), []string{"neo4j"}, true)     // 2026-06
	keptRecent = crec("01RECENT", runNow.AddDate(0, 0, -5), []string{"opensearch"}, true)
	keptUncons = crec("01UNCONS", runNow.AddDate(0, 0, -60), []string{"s3"}, false)
	ctx := context.Background()
	for _, rec := range []episodic.Record{agedJune, agedOld, keptRecent, keptUncons} {
		_ = store.AppendEpisode(ctx, runKey, rec)
	}
	store.graphs[runKey.String()] = knowledge.Graph{Nodes: []knowledge.Node{{ID: "01N", Kind: knowledge.KindFact, Name: "fact"}}}
	return
}

func newRunner(store *fakeRunStore, index search.Index, archiver cold.Archiver) *Runner {
	return New(store, index, archiver, runClock{t: runNow}, 30)
}

// ---- tests -----------------------------------------------------------------

func TestRunHappyPath(t *testing.T) {
	c := &calls{}
	store := newFakeRunStore(c)
	index := &fakeRunIndex{c: c}
	archiver := newFakeRunArchiver(c)
	seedProject(store)

	report, err := newRunner(store, index, archiver).Run(context.Background(), Options{})
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
	store := newFakeRunStore(c)
	index := &fakeRunIndex{c: c}
	archiver := newFakeRunArchiver(c)
	archiver.archiveErr = errors.New("s3 down")
	seedProject(store)

	report, err := newRunner(store, index, archiver).Run(context.Background(), Options{})
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
	store := newFakeRunStore(c)
	store.removeErr = errors.New("disk full")
	index := &fakeRunIndex{c: c}
	archiver := newFakeRunArchiver(c)
	seedProject(store)

	report, err := newRunner(store, index, archiver).Run(context.Background(), Options{})
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
	store := newFakeRunStore(c)
	index := &fakeRunIndex{c: c, deleteErr: search.ErrUnavailable}
	archiver := newFakeRunArchiver(c)
	seedProject(store)

	report, err := newRunner(store, index, archiver).Run(context.Background(), Options{})
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
	store := newFakeRunStore(c)
	seedProject(store)

	report, err := New(store, nil, nil, runClock{t: runNow}, 30).Run(context.Background(), Options{})
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
	// Candidates and stats still work without any derived service.
	if len(report.Candidates) != 1 {
		t.Errorf("candidates = %+v", report.Candidates)
	}
}

func TestRunDryRun(t *testing.T) {
	c := &calls{}
	store := newFakeRunStore(c)
	index := &fakeRunIndex{c: c}
	archiver := newFakeRunArchiver(c)
	seedProject(store)

	report, err := newRunner(store, index, archiver).Run(context.Background(), Options{DryRun: true})
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
	store := newFakeRunStore(c)
	index := &fakeRunIndex{c: c}
	archiver := newFakeRunArchiver(c)
	seedProject(store)

	otherKey := hotstore.ProjectKey{Workspace: "vms", Team: "core", Project: "other"}
	old := crec("01OTHEROLD", runNow.AddDate(0, 0, -40), nil, true)
	_ = store.AppendEpisode(context.Background(), otherKey, old)

	report, err := newRunner(store, index, archiver).Run(context.Background(), Options{
		Projects: []hotstore.ProjectKey{otherKey},
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
