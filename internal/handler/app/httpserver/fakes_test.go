package httpserver_test

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
	"sync"
	"testing"
	"time"

	app "github.com/drakejin/memory-mcp/internal/app/httpserver"
	. "github.com/drakejin/memory-mcp/internal/handler/app/httpserver"

	"github.com/drakejin/memory-mcp/internal/service/consolidate"
	"github.com/drakejin/memory-mcp/internal/service/document"
	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/service/knowledge"
	"github.com/drakejin/memory-mcp/internal/service/rehydrate"
	"github.com/drakejin/memory-mcp/internal/x/errs"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// --- shared fixtures --------------------------------------------------------

var (
	fixedNow = time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC)
	testKey  = projectkey.Key{Workspace: "ws", Team: "team", Project: "proj"}
)

// Valid 26-char Crockford-base32 ULIDs for request bodies and path params.
const (
	ulidA = "01JD00000000000000000000A0"
	ulidB = "01JD00000000000000000000B0"
	ulidC = "01JD00000000000000000000C0"
)

// testSHA is a well-formed lowercase-hex sha256.
const testSHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Settings every test server is built with. They mirror the production
// defaults without importing internal/x/config: the server takes only these
// three values, so the test does too.
const (
	testListenAddr = "127.0.0.1:8420"
	testS3Bucket   = "vms-memory-mcp"
	testTTLDays    = 30
)

// discardLogger keeps test output clean while still exercising the log paths.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- fakes ------------------------------------------------------------------
//
// The storage fakes (store, index, graph, archiver) implement the ports the
// composition root wires into the REAL episodic and knowledge services, so
// these tests keep exercising the full handler→service→storage choreography
// over deterministic storage. The pipeline fakes (documents, consolidator,
// rehydrator) implement the service interfaces themselves: for those the
// handler is a pure transport shell, and their orchestration is pinned by
// their own package suites. Errors are *errs.Error, the same vocabulary the
// real collaborators promise (code-standards §2.1).

type fakeClock struct{ now time.Time }

func (c fakeClock) Now() time.Time { return c.now }

// fakeIDs mints predictable, well-formed ULIDs so a test can assert on the id
// the server assigned. lastMillis records the timestamp it was handed, which is
// how the tests prove ids come from the injected clock and not the wall clock.
type fakeIDs struct {
	// mu guards seq and lastMillis: the concurrency tests mint ids from
	// several goroutines at once, and a fake that races is a fake that
	// reports the race instead of the behaviour under test.
	mu         sync.Mutex
	seq        int
	err        error
	lastMillis int64
}

var _ app.IDGenerator = (*fakeIDs)(nil)

func (f *fakeIDs) GenerateAt(unixMillis int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastMillis = unixMillis
	if f.err != nil {
		return "", f.err
	}
	f.seq++
	// 4-char head + 22 digits = the 26 Crockford characters ulid.Valid wants.
	return fmt.Sprintf("01JD%022d", f.seq), nil
}

// fakeStore is an in-memory hot store satisfying app.HotStore. Error hooks let
// tests drive the failure branches without touching the disk.
type fakeStore struct {
	// mu guards the whole fake, exactly as the real store's mutex guards the
	// whole store. Modelling that is not decoration: UpdateKnowledge must hold
	// it across read-modify-write, or a concurrency test would pass against a
	// fake that is atomic by accident of the Go scheduler.
	mu sync.Mutex

	episodes map[string][]episode.Record
	graphs   map[string]knowledge.Graph
	manifest rehydrate.Manifest

	appendErr    error
	readErr      error
	writeErr     error
	getErr       error
	listEpisErr  error
	updateEpiErr error

	dirtyMarks  []string
	updateCalls int
	writeSeq    int
}

var _ app.HotStore = (*fakeStore)(nil)

// touchEpisodic mirrors what the real store does on every episodic hot write: a
// new content hash and record count land in the manifest. Without this fidelity
// the fake cannot express the drift the hydration sha is designed to catch
// (§5), which is exactly how the recall-bump drift bug stayed invisible.
func (s *fakeStore) touchEpisodic(key projectkey.Key) {
	s.writeSeq++
	fk := rehydrate.ManifestFileKey(rehydrate.PlaneEpisodic, key)
	fs := s.manifest.Files[fk]
	fs.SHA256 = fmt.Sprintf("sha-%d", s.writeSeq)
	fs.RecordCount = len(s.episodes[key.String()])
	s.manifest.Files[fk] = fs
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		episodes: map[string][]episode.Record{},
		graphs:   map[string]knowledge.Graph{},
		manifest: rehydrate.Manifest{
			Files:   map[string]rehydrate.FileState{},
			Indexes: map[string]rehydrate.IndexState{},
		},
	}
}

func (s *fakeStore) AppendEpisode(_ context.Context, key projectkey.Key, rec episode.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.appendErr != nil {
		return s.appendErr
	}
	s.episodes[key.String()] = append(s.episodes[key.String()], rec)
	s.touchEpisodic(key)
	return nil
}

func (s *fakeStore) ListEpisodes(_ context.Context, key projectkey.Key) ([]episode.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listEpisErr != nil {
		return nil, s.listEpisErr
	}
	return s.episodes[key.String()], nil
}

func (s *fakeStore) GetEpisode(_ context.Context, key projectkey.Key, id string) (episode.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return episode.Record{}, s.getErr
	}
	for _, r := range s.episodes[key.String()] {
		if r.ID == id {
			return r, nil
		}
	}
	return episode.Record{}, errs.NotFound("hotstore.GetEpisode", "episode", id)
}

func (s *fakeStore) UpdateEpisodes(_ context.Context, key projectkey.Key, ids []string, fn func(episode.Record) episode.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
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

// ReadKnowledge is not part of app.HotStore — the handlers no longer read the
// graph wholesale — but the concurrency suite uses it to inspect the canonical
// outcome, so the fake keeps it as a test-side accessor.
func (s *fakeStore) ReadKnowledge(_ context.Context, key projectkey.Key) (knowledge.Graph, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readErr != nil {
		return knowledge.Graph{}, s.readErr
	}
	return s.graphs[key.String()], nil
}

func (s *fakeStore) UpdateKnowledge(_ context.Context, key projectkey.Key, fn func(knowledge.Graph) (knowledge.Graph, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readErr != nil {
		return s.readErr
	}
	next, err := fn(s.graphs[key.String()])
	if err != nil {
		return err
	}
	if s.writeErr != nil {
		return s.writeErr
	}
	s.graphs[key.String()] = next
	return nil
}

func (s *fakeStore) UpdateManifest(_ context.Context, fn func(rehydrate.Manifest) (rehydrate.Manifest, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := fn(s.manifest)
	if err != nil {
		return err
	}
	s.manifest = m
	return nil
}

func (s *fakeStore) MarkDirty(_ context.Context, key projectkey.Key, plane rehydrate.Plane) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fk := rehydrate.ManifestFileKey(plane, key)
	s.dirtyMarks = append(s.dirtyMarks, fk)
	fs := s.manifest.Files[fk]
	fs.Dirty = true
	s.manifest.Files[fk] = fs
	return nil
}

// fakeIndex is an in-memory episodic index satisfying episode.Index.
type fakeIndex struct {
	indexed   map[string][]episode.Record
	hits      []episode.Hit
	indexErr  error
	searchErr error
}

var _ episode.Index = (*fakeIndex)(nil)

func newFakeIndex() *fakeIndex {
	return &fakeIndex{indexed: map[string][]episode.Record{}}
}

func (f *fakeIndex) IndexRecords(_ context.Context, key projectkey.Key, recs []episode.Record) error {
	if f.indexErr != nil {
		return f.indexErr
	}
	f.indexed[key.String()] = append(f.indexed[key.String()], recs...)
	return nil
}

func (f *fakeIndex) Search(_ context.Context, _ projectkey.Key, _ episode.Query) ([]episode.Hit, error) {
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	return f.hits, nil
}

// fakeGraph is an in-memory knowledge mirror satisfying knowledge.GraphIndex.
type fakeGraph struct {
	// mu guards the mirrored collections for the same reason as fakeIDs.mu.
	mu sync.Mutex

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

var _ knowledge.GraphIndex = (*fakeGraph)(nil)

func newFakeGraph() *fakeGraph {
	return &fakeGraph{nodes: map[string][]knowledge.Node{}, edges: map[string][]knowledge.Edge{}}
}

func (f *fakeGraph) UpsertNodes(_ context.Context, key projectkey.Key, nodes []knowledge.Node) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertNodeErr != nil {
		return f.upsertNodeErr
	}
	f.nodes[key.String()] = append(f.nodes[key.String()], nodes...)
	return nil
}

func (f *fakeGraph) UpsertEdges(_ context.Context, key projectkey.Key, edges []knowledge.Edge) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertEdgeErr != nil {
		return f.upsertEdgeErr
	}
	f.edges[key.String()] = append(f.edges[key.String()], edges...)
	return nil
}

func (f *fakeGraph) DeleteNode(_ context.Context, _ projectkey.Key, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.purged = append(f.purged, id)
	return nil
}

func (f *fakeGraph) Search(_ context.Context, _ projectkey.Key, _ string, _ bool) ([]knowledge.Node, error) {
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	return f.found, nil
}

func (f *fakeGraph) Neighborhood(_ context.Context, _ projectkey.Key, _ string, _ int) (knowledge.Graph, error) {
	if f.neighborErr != nil {
		return knowledge.Graph{}, f.neighborErr
	}
	return f.sub, nil
}

// fakeDocuments is a document.Service double. The zero value reports cold
// storage configured and reachable, mirroring a healthy pipeline; tests flip
// coldUnconfigured / coldUnreachable to model the §6/§0-honesty gaps.
type fakeDocuments struct {
	result     document.IngestResult
	ingestErr  error
	original   string
	originErr  error
	chunks     []episode.Record
	chunksErr  error
	lastName   string
	lastUpload []byte

	coldUnconfigured bool
	coldUnreachable  bool
	probeCalls       int
}

var _ document.Service = (*fakeDocuments)(nil)

func (f *fakeDocuments) Ingest(_ context.Context, _ projectkey.Key, filename string, data io.Reader) (document.IngestResult, error) {
	f.lastName = filename
	body, err := io.ReadAll(data)
	if err != nil {
		return document.IngestResult{}, err
	}
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

func (f *fakeDocuments) Chunks(context.Context, string) ([]episode.Record, error) {
	if f.chunksErr != nil {
		return nil, f.chunksErr
	}
	return f.chunks, nil
}

func (f *fakeDocuments) ColdConfigured() bool { return !f.coldUnconfigured }

func (f *fakeDocuments) ColdReachable(context.Context) bool {
	f.probeCalls++
	return !f.coldUnconfigured && !f.coldUnreachable
}

// fakeConsolidator is a consolidate.Service double.
type fakeConsolidator struct {
	report   consolidate.Report
	err      error
	lastOpts consolidate.Options
	calls    int

	hotState    consolidate.HotState
	hotStateErr error
}

var _ consolidate.Service = (*fakeConsolidator)(nil)

func (f *fakeConsolidator) Run(_ context.Context, opts consolidate.Options) (consolidate.Report, error) {
	f.calls++
	f.lastOpts = opts
	return f.report, f.err
}

func (f *fakeConsolidator) HotState(context.Context) (consolidate.HotState, error) {
	return f.hotState, f.hotStateErr
}

// fakeRehydrator is a rehydrate.Service double.
type fakeRehydrator struct {
	drift      rehydrate.DriftReport
	driftErr   error
	report     rehydrate.Report
	allErr     error
	gateErr    error
	gateCalls  []string
	allCalls   int
	lastVerify bool
}

var _ rehydrate.Service = (*fakeRehydrator)(nil)

func (f *fakeRehydrator) CheckDrift(context.Context) (rehydrate.DriftReport, error) {
	return f.drift, f.driftErr
}

func (f *fakeRehydrator) RehydrateAll(_ context.Context, verify bool) (rehydrate.Report, error) {
	f.allCalls++
	f.lastVerify = verify
	return f.report, f.allErr
}

func (f *fakeRehydrator) RehydrateProject(context.Context, projectkey.Key) (rehydrate.Report, error) {
	return rehydrate.Report{Failures: []string{}}, nil
}

func (f *fakeRehydrator) StatGate(_ context.Context, key projectkey.Key) error {
	f.gateCalls = append(f.gateCalls, key.String())
	return f.gateErr
}

// fakeArchiver is the episode.Archive double: the cold read side the episodic
// service falls back to for aged records (P11).
type fakeArchiver struct {
	archived map[string]episode.Record
	fetchErr error
}

var _ episode.Archive = (*fakeArchiver)(nil)

func newFakeArchiver() *fakeArchiver {
	return &fakeArchiver{archived: map[string]episode.Record{}}
}

func (f *fakeArchiver) FetchArchivedEpisode(_ context.Context, _ projectkey.Key, id string) (episode.Record, error) {
	if f.fetchErr != nil {
		return episode.Record{}, f.fetchErr
	}
	rec, ok := f.archived[id]
	if !ok {
		return episode.Record{}, errs.NotFound("cold.FetchArchivedEpisode", "episode", id)
	}
	return rec, nil
}

// --- harness ----------------------------------------------------------------

// newTestServer builds a Server whose collaborators default to working fakes;
// mutate the Config before construction when a nil or failing one is wanted.
// The episodic and knowledge services are the REAL ones, constructed by
// app.New over the fake storage — exactly the production wiring.
func newTestServer(t *testing.T, mutate func(*app.Config)) (*app.Server, http.Handler) {
	t.Helper()
	cfg := app.Config{
		ListenAddr:      testListenAddr,
		S3Bucket:        testS3Bucket,
		EpisodicTTLDays: testTTLDays,
		Store:           newFakeStore(),
		Index:           newFakeIndex(),
		Graph:           newFakeGraph(),
		Documents:       &fakeDocuments{},
		Consolidator:    &fakeConsolidator{},
		Rehydrator:      &fakeRehydrator{},
		Archiver:        newFakeArchiver(),
		Clock:           fakeClock{now: fixedNow},
		IDs:             &fakeIDs{},
		Logger:          discardLogger(),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	srv, err := app.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
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

// errBoom is an unclassified collaborator failure: it carries no errs.Kind, so
// it exercises the "internal / unclassified" row of the §2.2 mapping table.
var errBoom = errors.New("boom")

// errIndexDown / errGraphDown are the degraded-mode signal: KindUnavailable is
// what a derived store returns when it cannot be reached.
var (
	errIndexDown = errs.Unavailable("search.Search", errBoom)
	errGraphDown = errs.Unavailable("graph.Search", errBoom)
)
