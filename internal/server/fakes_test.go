package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/cold"
	"github.com/drakejin/memory-mcp/internal/config"
	"github.com/drakejin/memory-mcp/internal/consolidate"
	"github.com/drakejin/memory-mcp/internal/document"
	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/knowledge"
	"github.com/drakejin/memory-mcp/internal/rehydrate"
	"github.com/drakejin/memory-mcp/internal/search"
)

// --- shared fixtures --------------------------------------------------------

var (
	fixedNow = time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC)
	testKey  = hotstore.ProjectKey{Workspace: "ws", Team: "team", Project: "proj"}
)

// Valid 26-char Crockford-base32 ULIDs for request bodies and path params.
const (
	ulidA = "01JD00000000000000000000A0"
	ulidB = "01JD00000000000000000000B0"
	ulidC = "01JD00000000000000000000C0"
)

// testSHA is a well-formed lowercase-hex sha256.
const testSHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func testConfig() config.Config {
	return config.Config{
		Home:            t0Dir,
		Username:        "jin",
		S3Bucket:        "vms-memory-mcp",
		S3Region:        config.DefaultS3Region,
		ListenAddr:      config.DefaultListenAddr,
		EpisodicTTLDays: config.DefaultEpisodicTTLDays,
	}
}

// t0Dir keeps testConfig free of filesystem side effects; no handler touches it.
const t0Dir = "/tmp/memory-mcp-test-home"

// discardLogger keeps test output clean while still exercising the log paths.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- fakes ------------------------------------------------------------------

type fakeClock struct{ now time.Time }

func (c fakeClock) Now() time.Time { return c.now }

// fakeStore is an in-memory hotstore.Store. Error hooks let tests drive the
// failure branches without touching the disk.
type fakeStore struct {
	episodes map[string][]episodic.Record
	graphs   map[string]knowledge.Graph
	manifest hotstore.Manifest
	mtimes   map[string]time.Time

	appendErr    error
	readErr      error
	writeErr     error
	getErr       error
	listProjErr  error
	listEpisErr  error
	manifestErr  error
	updateEpiErr error

	dirtyMarks  []string
	updateCalls int
	writeSeq    int
}

// touchEpisodic mirrors what FileStore does on every episodic hot write: a new
// content hash and mtime land in the manifest. Without this fidelity the fake
// cannot express the drift the hydration sha is designed to catch (§5), which
// is exactly how the recall-bump drift bug stayed invisible to unit tests.
func (s *fakeStore) touchEpisodic(key hotstore.ProjectKey) {
	s.writeSeq++
	fk := rehydrate.FileKey(hotstore.PlaneEpisodic, key)
	fs := s.manifest.Files[fk]
	fs.SHA256 = fmt.Sprintf("sha-%d", s.writeSeq)
	fs.RecordCount = len(s.episodes[key.String()])
	s.manifest.Files[fk] = fs
	s.mtimes[fk] = fixedNow
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
	if s.appendErr != nil {
		return s.appendErr
	}
	s.episodes[key.String()] = append(s.episodes[key.String()], rec)
	s.touchEpisodic(key)
	return nil
}

func (s *fakeStore) ListEpisodes(_ context.Context, key hotstore.ProjectKey) ([]episodic.Record, error) {
	if s.listEpisErr != nil {
		return nil, s.listEpisErr
	}
	return s.episodes[key.String()], nil
}

func (s *fakeStore) GetEpisode(_ context.Context, key hotstore.ProjectKey, id string) (episodic.Record, error) {
	if s.getErr != nil {
		return episodic.Record{}, s.getErr
	}
	for _, r := range s.episodes[key.String()] {
		if r.ID == id {
			return r, nil
		}
	}
	return episodic.Record{}, hotstore.ErrNotFound
}

func (s *fakeStore) UpdateEpisodes(_ context.Context, key hotstore.ProjectKey, ids []string, fn func(episodic.Record) episodic.Record) error {
	s.updateCalls++
	if s.updateEpiErr != nil {
		return s.updateEpiErr
	}
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
	s.touchEpisodic(key)
	return nil
}

func (s *fakeStore) RemoveEpisodes(_ context.Context, _ hotstore.ProjectKey, _ []string) error {
	return nil
}

func (s *fakeStore) ReadKnowledge(_ context.Context, key hotstore.ProjectKey) (knowledge.Graph, error) {
	if s.readErr != nil {
		return knowledge.Graph{}, s.readErr
	}
	return s.graphs[key.String()], nil
}

func (s *fakeStore) WriteKnowledge(_ context.Context, key hotstore.ProjectKey, g knowledge.Graph) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	s.graphs[key.String()] = g
	return nil
}

func (s *fakeStore) ListProjects(_ context.Context) ([]hotstore.ProjectKey, error) {
	if s.listProjErr != nil {
		return nil, s.listProjErr
	}
	seen := map[string]bool{}
	var out []hotstore.ProjectKey
	for _, m := range []map[string]bool{keysOfEpisodes(s), keysOfGraphs(s)} {
		for k := range m {
			if seen[k] {
				continue
			}
			seen[k] = true
			parts := strings.SplitN(k, "/", 3)
			out = append(out, hotstore.ProjectKey{Workspace: parts[0], Team: parts[1], Project: parts[2]})
		}
	}
	return out, nil
}

func keysOfEpisodes(s *fakeStore) map[string]bool {
	out := map[string]bool{}
	for k := range s.episodes {
		out[k] = true
	}
	return out
}

func keysOfGraphs(s *fakeStore) map[string]bool {
	out := map[string]bool{}
	for k := range s.graphs {
		out[k] = true
	}
	return out
}

func (s *fakeStore) Manifest(_ context.Context) (hotstore.Manifest, error) {
	if s.manifestErr != nil {
		return hotstore.Manifest{}, s.manifestErr
	}
	return s.manifest, nil
}

func (s *fakeStore) UpdateManifest(_ context.Context, fn func(hotstore.Manifest) (hotstore.Manifest, error)) error {
	m, err := fn(s.manifest)
	if err != nil {
		return err
	}
	s.manifest = m
	return nil
}

func (s *fakeStore) MarkDirty(_ context.Context, key hotstore.ProjectKey, plane hotstore.Plane) error {
	fk := rehydrate.FileKey(plane, key)
	s.dirtyMarks = append(s.dirtyMarks, fk)
	fs := s.manifest.Files[fk]
	fs.Dirty = true
	s.manifest.Files[fk] = fs
	return nil
}

func (s *fakeStore) FileInfo(_ context.Context, key hotstore.ProjectKey, plane hotstore.Plane) (int64, time.Time, error) {
	mt, ok := s.mtimes[rehydrate.FileKey(plane, key)]
	if !ok {
		return 0, time.Time{}, hotstore.ErrNotFound
	}
	return 1, mt, nil
}

// fakeIndex is an in-memory search.Index.
type fakeIndex struct {
	indexed   map[string][]episodic.Record
	hits      []search.Hit
	indexErr  error
	searchErr error
}

func newFakeIndex() *fakeIndex {
	return &fakeIndex{indexed: map[string][]episodic.Record{}}
}

func (f *fakeIndex) Ping(context.Context) error        { return nil }
func (f *fakeIndex) EnsureIndex(context.Context) error { return nil }

func (f *fakeIndex) IndexRecords(_ context.Context, key hotstore.ProjectKey, recs []episodic.Record) error {
	if f.indexErr != nil {
		return f.indexErr
	}
	f.indexed[key.String()] = append(f.indexed[key.String()], recs...)
	return nil
}

func (f *fakeIndex) DeleteRecords(context.Context, hotstore.ProjectKey, []string) error { return nil }

func (f *fakeIndex) Search(_ context.Context, _ hotstore.ProjectKey, _ search.Query) ([]search.Hit, error) {
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	return f.hits, nil
}

func (f *fakeIndex) DocCount(context.Context, hotstore.ProjectKey) (int, error) { return 0, nil }
func (f *fakeIndex) Drop(context.Context) error                                 { return nil }

// fakeGraph is an in-memory graph.Store.
type fakeGraph struct {
	nodes  map[string][]knowledge.Node
	edges  map[string][]knowledge.Edge
	found  []knowledge.Node
	sub    knowledge.Graph
	purged []string

	upsertNodeErr error
	upsertEdgeErr error
	searchErr     error
	neighborErr   error
	deleteErr     error
}

func newFakeGraph() *fakeGraph {
	return &fakeGraph{nodes: map[string][]knowledge.Node{}, edges: map[string][]knowledge.Edge{}}
}

func (f *fakeGraph) Ping(context.Context) error { return nil }

func (f *fakeGraph) UpsertNodes(_ context.Context, key hotstore.ProjectKey, nodes []knowledge.Node) error {
	if f.upsertNodeErr != nil {
		return f.upsertNodeErr
	}
	f.nodes[key.String()] = append(f.nodes[key.String()], nodes...)
	return nil
}

func (f *fakeGraph) UpsertEdges(_ context.Context, key hotstore.ProjectKey, edges []knowledge.Edge) error {
	if f.upsertEdgeErr != nil {
		return f.upsertEdgeErr
	}
	f.edges[key.String()] = append(f.edges[key.String()], edges...)
	return nil
}

func (f *fakeGraph) DeleteNode(_ context.Context, _ hotstore.ProjectKey, id string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.purged = append(f.purged, id)
	return nil
}

func (f *fakeGraph) Search(_ context.Context, _ hotstore.ProjectKey, _ string, _ bool) ([]knowledge.Node, error) {
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	return f.found, nil
}

func (f *fakeGraph) Neighborhood(_ context.Context, _ hotstore.ProjectKey, _ string, _ int) (knowledge.Graph, error) {
	if f.neighborErr != nil {
		return knowledge.Graph{}, f.neighborErr
	}
	return f.sub, nil
}

func (f *fakeGraph) SupersedeChain(context.Context, hotstore.ProjectKey, string) ([]knowledge.Node, error) {
	return nil, nil
}

func (f *fakeGraph) NodeCount(context.Context, hotstore.ProjectKey) (int, error) { return 0, nil }
func (f *fakeGraph) Clear(context.Context) error                                 { return nil }

// fakeDocuments is a document.Service double.
type fakeDocuments struct {
	result     document.IngestResult
	ingestErr  error
	original   string
	originErr  error
	chunks     []episodic.Record
	chunksErr  error
	lastName   string
	lastUpload []byte
}

func (f *fakeDocuments) Ingest(_ context.Context, _ hotstore.ProjectKey, filename string, data io.Reader) (document.IngestResult, error) {
	f.lastName = filename
	body, _ := io.ReadAll(data)
	f.lastUpload = body
	if f.ingestErr != nil {
		return document.IngestResult{}, f.ingestErr
	}
	return f.result, nil
}

func (f *fakeDocuments) Original(context.Context, string) (io.ReadCloser, error) {
	if f.originErr != nil {
		return nil, f.originErr
	}
	return io.NopCloser(strings.NewReader(f.original)), nil
}

func (f *fakeDocuments) Chunks(context.Context, string) ([]episodic.Record, error) {
	if f.chunksErr != nil {
		return nil, f.chunksErr
	}
	return f.chunks, nil
}

// fakeConsolidator is a consolidate.Consolidator double.
type fakeConsolidator struct {
	report   consolidate.Report
	err      error
	lastOpts consolidate.Options
	calls    int
}

func (f *fakeConsolidator) Run(_ context.Context, opts consolidate.Options) (consolidate.Report, error) {
	f.calls++
	f.lastOpts = opts
	return f.report, f.err
}

// fakeRehydrator is a rehydrate.Rehydrator double.
type fakeRehydrator struct {
	drift        rehydrate.DriftReport
	driftErr     error
	report       rehydrate.Report
	allErr       error
	projErr      error
	gateErr      error
	gateCalls    []string
	allCalls     int
	lastVerify   bool
	projectCalls []string
}

func (f *fakeRehydrator) CheckDrift(context.Context) (rehydrate.DriftReport, error) {
	return f.drift, f.driftErr
}

func (f *fakeRehydrator) RehydrateAll(_ context.Context, verify bool) (rehydrate.Report, error) {
	f.allCalls++
	f.lastVerify = verify
	return f.report, f.allErr
}

func (f *fakeRehydrator) RehydrateProject(_ context.Context, key hotstore.ProjectKey) (rehydrate.Report, error) {
	f.projectCalls = append(f.projectCalls, key.String())
	return f.report, f.projErr
}

func (f *fakeRehydrator) StatGate(_ context.Context, key hotstore.ProjectKey) error {
	f.gateCalls = append(f.gateCalls, key.String())
	return f.gateErr
}

// fakeArchiver is a cold.Archiver double.
type fakeArchiver struct {
	archived  map[string]episodic.Record
	fetchErr  error
	blobErr   error
	exists    bool
	existsErr error
}

func newFakeArchiver() *fakeArchiver {
	return &fakeArchiver{archived: map[string]episodic.Record{}}
}

func (f *fakeArchiver) ArchiveEpisodes(context.Context, hotstore.ProjectKey, string, []episodic.Record) (string, error) {
	return "", nil
}

func (f *fakeArchiver) FetchArchivedEpisode(_ context.Context, _ hotstore.ProjectKey, id string) (episodic.Record, error) {
	if f.fetchErr != nil {
		return episodic.Record{}, f.fetchErr
	}
	rec, ok := f.archived[id]
	if !ok {
		return episodic.Record{}, cold.ErrNotFound
	}
	return rec, nil
}

func (f *fakeArchiver) SnapshotKnowledge(context.Context, hotstore.ProjectKey, knowledge.Graph, time.Time) (string, string, error) {
	return "", "", nil
}

func (f *fakeArchiver) UploadBlob(context.Context, string, io.Reader) (string, error) {
	return "", nil
}

// FetchBlob doubles as the /status cold-reachability probe. blobErr models a
// bucket-level failure (NoSuchBucket); the default ErrNotFound models a live
// bucket that simply does not hold the probe key.
func (f *fakeArchiver) FetchBlob(context.Context, string) (io.ReadCloser, error) {
	if f.blobErr != nil {
		return nil, f.blobErr
	}
	return nil, cold.ErrNotFound
}

func (f *fakeArchiver) BlobExists(context.Context, string) (bool, error) {
	if f.existsErr != nil {
		return false, f.existsErr
	}
	return f.exists, nil
}

// --- harness ----------------------------------------------------------------

// newTestServer builds a Server whose deps default to working fakes; mutate the
// returned Deps copy before calling when a nil/failing collaborator is wanted.
func newTestServer(t *testing.T, mutate func(*Deps)) (*Server, http.Handler) {
	t.Helper()
	deps := Deps{
		Store:        newFakeStore(),
		Index:        newFakeIndex(),
		Graph:        newFakeGraph(),
		Documents:    &fakeDocuments{},
		Consolidator: &fakeConsolidator{},
		Rehydrator:   &fakeRehydrator{},
		Archiver:     newFakeArchiver(),
		Clock:        fakeClock{now: fixedNow},
		Logger:       discardLogger(),
	}
	if mutate != nil {
		mutate(&deps)
	}
	srv := New(testConfig(), deps)
	return srv, srv.Router()
}

// do issues a request against the router and returns the recorder.
func do(t *testing.T, h http.Handler, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		switch v := body.(type) {
		case string:
			reader = strings.NewReader(v)
		default:
			raw, err := json.Marshal(v)
			if err != nil {
				t.Fatalf("marshal body: %v", err)
			}
			reader = bytes.NewReader(raw)
		}
	}
	req := httptest.NewRequest(method, target, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// decodeEnvelope asserts the {success,data,error} shape and returns it with the
// data re-decoded into out (when out is non-nil).
func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder, out any) Envelope {
	t.Helper()
	var env Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("response is not an envelope: %v (body=%s)", err, rec.Body.String())
	}
	if out != nil {
		raw, err := json.Marshal(env.Data)
		if err != nil {
			t.Fatalf("re-marshal data: %v", err)
		}
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("decode data into %T: %v (data=%s)", out, err, raw)
		}
	}
	return env
}

// assertStatus fails when the recorder status differs from want.
func assertStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d (body=%s)", rec.Code, want, rec.Body.String())
	}
}

// errBoom is the generic collaborator failure used across tests.
var errBoom = errors.New("boom")
