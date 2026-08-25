package document

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/service/knowledge"
	"github.com/drakejin/memory-mcp/internal/service/rehydrate"
	"github.com/drakejin/memory-mcp/internal/x/errs"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

var docKey = projectkey.Key{Workspace: "vms", Team: "core", Project: "memory"}

// testUsername mirrors the cold key prefix so blob keys look production-shaped.
const testUsername = "jin"

// fakeCache is an in-memory BlobCache. A miss is reported the way the real
// cache reports it: KindNotFound from Get, false from Has.
type fakeCache struct {
	blobs   map[string][]byte
	getErr  error // injected read failure (not a miss)
	hasErr  error // injected classification failure
	putErr  error
	putCall int
}

func newFakeCache() *fakeCache { return &fakeCache{blobs: map[string][]byte{}} }

func (c *fakeCache) Put(_ context.Context, data io.Reader) (string, int64, error) {
	c.putCall++
	if c.putErr != nil {
		return "", 0, c.putErr
	}
	raw, err := io.ReadAll(data)
	if err != nil {
		return "", 0, errs.Internal("blob.Put", err)
	}
	sum := sha256.Sum256(raw)
	sha := hex.EncodeToString(sum[:])
	c.blobs[sha] = raw
	return sha, int64(len(raw)), nil
}

func (c *fakeCache) Get(_ context.Context, sha string) (io.ReadCloser, error) {
	if c.getErr != nil {
		return nil, c.getErr
	}
	raw, ok := c.blobs[sha]
	if !ok {
		return nil, errs.NotFound("blob.Get", "blob", sha)
	}
	return io.NopCloser(bytes.NewReader(raw)), nil
}

func (c *fakeCache) Has(_ context.Context, sha string) (bool, error) {
	if c.hasErr != nil {
		return false, c.hasErr
	}
	_, ok := c.blobs[sha]
	return ok, nil
}

// fakeArchiver is an in-memory BlobArchiver counting uploads so idempotency is
// observable.
type fakeArchiver struct {
	blobs         map[string][]byte
	uploads       int
	fetches       int
	failUpload    bool
	failFetch     bool
	failFetchFrom int // 1-based fetch number from which fetches fail; 0 = never
}

func newFakeArchiver() *fakeArchiver { return &fakeArchiver{blobs: map[string][]byte{}} }

func (a *fakeArchiver) UploadBlob(_ context.Context, sha string, r io.Reader) (string, error) {
	if a.failUpload {
		return "", errs.Unavailable("cold.UploadBlob", nil)
	}
	if _, ok := a.blobs[sha]; !ok {
		raw, err := io.ReadAll(r)
		if err != nil {
			return "", errs.Internal("cold.UploadBlob", err)
		}
		a.blobs[sha] = raw
		a.uploads++
	}
	return blobKeyFor(sha), nil
}

func (a *fakeArchiver) FetchBlob(_ context.Context, sha string) (io.ReadCloser, error) {
	a.fetches++
	if a.failFetch || (a.failFetchFrom > 0 && a.fetches >= a.failFetchFrom) {
		return nil, errs.Unavailable("cold.FetchBlob", nil)
	}
	raw, ok := a.blobs[sha]
	if !ok {
		return nil, errs.NotFound("cold.FetchBlob", "blob", sha)
	}
	return io.NopCloser(bytes.NewReader(raw)), nil
}

// blobKeyFor mirrors the cold key layout without importing it: the pipeline
// only passes the key through, so the exact shape is the archiver's contract.
func blobKeyFor(sha string) string {
	shard := sha
	if len(sha) >= 2 {
		shard = sha[:2]
	}
	return strings.Join([]string{testUsername, "blobs", shard, sha}, "/")
}

type fakeIndexer struct {
	indexed []episode.Record
	fail    bool
}

func (f *fakeIndexer) IndexRecords(_ context.Context, _ projectkey.Key, recs []episode.Record) error {
	if f.fail {
		return errs.Unavailable("search.IndexRecords", nil)
	}
	f.indexed = append(f.indexed, recs...)
	return nil
}

type fakeGraph struct {
	nodes []knowledge.Node
	fail  bool
}

func (f *fakeGraph) UpsertNodes(_ context.Context, _ projectkey.Key, nodes []knowledge.Node) error {
	if f.fail {
		return errs.Unavailable("graph.UpsertNodes", nil)
	}
	f.nodes = append(f.nodes, nodes...)
	return nil
}

// fakeStore is an in-memory HotStore — the narrow slice the pipeline consumes,
// nothing more.
type fakeStore struct {
	// mu guards the whole fake, mirroring hotstore.client.mu. UpdateKnowledge
	// must hold it across read-modify-write or a concurrent-ingest test would
	// be asserting against a fake that is atomic only by scheduling luck.
	mu sync.Mutex

	episodes     map[string][]episode.Record
	graphs       map[string]knowledge.Graph
	dirty        map[string]bool
	appendErr    error
	listErr      error
	projectsErr  error
	readErr      error
	writeErr     error
	markDirtyErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		episodes: map[string][]episode.Record{},
		graphs:   map[string]knowledge.Graph{},
		dirty:    map[string]bool{},
	}
}

func (s *fakeStore) AppendEpisode(_ context.Context, key projectkey.Key, rec episode.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.appendErr != nil {
		return s.appendErr
	}
	s.episodes[key.String()] = append(s.episodes[key.String()], rec)
	return nil
}

func (s *fakeStore) ListEpisodes(_ context.Context, key projectkey.Key) ([]episode.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	return slices.Clone(s.episodes[key.String()]), nil
}

func (s *fakeStore) ListProjects(_ context.Context) ([]projectkey.Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.projectsErr != nil {
		return nil, s.projectsErr
	}
	seen := map[string]bool{}
	for k := range s.episodes {
		seen[k] = true
	}
	for k := range s.graphs {
		seen[k] = true
	}
	out := make([]projectkey.Key, 0, len(seen))
	for _, name := range slices.Sorted(mapKeys(seen)) {
		out = append(out, splitKey(name))
	}
	return out, nil
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

func (s *fakeStore) MarkDirty(_ context.Context, key projectkey.Key, plane rehydrate.Plane) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.markDirtyErr != nil {
		return s.markDirtyErr
	}
	s.dirty[string(plane)+"/"+key.String()] = true
	return nil
}

func mapKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func splitKey(s string) projectkey.Key {
	parts := strings.SplitN(s, "/", 3)
	return projectkey.Key{Workspace: parts[0], Team: parts[1], Project: parts[2]}
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// fakeIDs mints strictly increasing ids of the right shape, so chunk order and
// node identity are assertable without pulling in the real generator.
type fakeIDs struct {
	n    int
	err  error
	fail int // 1-based call number that fails; 0 = never
}

func (f *fakeIDs) GenerateAt(unixMillis int64) (string, error) {
	f.n++
	if f.err != nil && (f.fail == 0 || f.fail == f.n) {
		return "", f.err
	}
	return fmt.Sprintf("01T%013dR%09d", unixMillis, f.n), nil
}

// stubExtractor returns a canned extraction result regardless of input.
type stubExtractor struct {
	text string
	err  error
}

func (s stubExtractor) Extract(context.Context, string, io.Reader) (string, bool, error) {
	if s.err != nil {
		return "", false, s.err
	}
	return s.text, s.text != "", nil
}

// rig bundles a Service with the fakes behind it so tests can assert on both
// sides of the pipeline.
type rig struct {
	svc      Service
	store    *fakeStore
	cache    *fakeCache
	archiver *fakeArchiver
	index    *fakeIndexer
	graph    *fakeGraph
	ids      *fakeIDs
	logs     *bytes.Buffer
}

// newRig builds a Service over fresh fakes; tweak adjusts the Config before
// construction (used for the nil-collaborator degraded paths).
func newRig(t *testing.T, tweak ...func(*Config)) *rig {
	t.Helper()

	r := &rig{
		store:    newFakeStore(),
		cache:    newFakeCache(),
		archiver: newFakeArchiver(),
		index:    &fakeIndexer{},
		graph:    &fakeGraph{},
		ids:      &fakeIDs{},
		logs:     &bytes.Buffer{},
	}
	cfg := Config{
		Store:    r.store,
		Cache:    r.cache,
		Archiver: r.archiver,
		Index:    r.index,
		Graph:    r.graph,
		Clock:    fixedClock{t: time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)},
		IDs:      r.ids,
		Logger:   slog.New(slog.NewTextHandler(r.logs, nil)),
	}
	for _, fn := range tweak {
		fn(&cfg)
	}

	svc, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.svc = svc
	return r
}
