package document

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/drakejin/memory-mcp/internal/blob"
	"github.com/drakejin/memory-mcp/internal/cold"
	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/knowledge"
)

func TestChunk(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		chunkBytes int
		maxChunks  int
		want       []string
		wantTrunc  Truncation
	}{
		{
			name: "empty text yields nothing",
			text: "", chunkBytes: 4, maxChunks: 10,
			want: nil, wantTrunc: Truncation{},
		},
		{
			name: "text under one chunk",
			text: "abc", chunkBytes: 8, maxChunks: 10,
			want: []string{"abc"}, wantTrunc: Truncation{Total: 1, Indexed: 1},
		},
		{
			name: "splits on byte budget",
			text: "abcde", chunkBytes: 2, maxChunks: 10,
			want: []string{"ab", "cd", "e"}, wantTrunc: Truncation{Total: 3, Indexed: 3},
		},
		{
			name: "exact multiple has no empty tail",
			text: "abcd", chunkBytes: 2, maxChunks: 10,
			want: []string{"ab", "cd"}, wantTrunc: Truncation{Total: 2, Indexed: 2},
		},
		{
			name: "never splits a rune",
			text: "가나다", chunkBytes: 4, maxChunks: 10, // each rune is 3 bytes
			want: []string{"가", "나", "다"}, wantTrunc: Truncation{Total: 3, Indexed: 3},
		},
		{
			name: "rune wider than budget still emitted whole",
			text: "가", chunkBytes: 1, maxChunks: 10,
			want: []string{"가"}, wantTrunc: Truncation{Total: 1, Indexed: 1},
		},
		{
			name: "cap truncates but total stays honest",
			text: "abcdef", chunkBytes: 2, maxChunks: 2,
			want: []string{"ab", "cd"}, wantTrunc: Truncation{Total: 3, Indexed: 2},
		},
		{
			name: "maxChunks zero means uncapped",
			text: "abcdef", chunkBytes: 2, maxChunks: 0,
			want: []string{"ab", "cd", "ef"}, wantTrunc: Truncation{Total: 3, Indexed: 3},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, trunc := Chunk(tt.text, tt.chunkBytes, tt.maxChunks)
			if !slices.Equal(got, tt.want) {
				t.Errorf("chunks = %q, want %q", got, tt.want)
			}
			if trunc != tt.wantTrunc {
				t.Errorf("truncation = %+v, want %+v", trunc, tt.wantTrunc)
			}
			for i, c := range got {
				if !utf8.ValidString(c) {
					t.Errorf("chunk %d is not valid UTF-8: %q", i, c)
				}
			}
		})
	}
}

// ---- fakes -----------------------------------------------------------------

type fakeCache struct {
	blobs map[string][]byte
}

func newFakeCache() *fakeCache { return &fakeCache{blobs: map[string][]byte{}} }

func (c *fakeCache) Put(_ context.Context, data io.Reader) (string, int64, error) {
	raw, err := io.ReadAll(data)
	if err != nil {
		return "", 0, err
	}
	sum := sha256.Sum256(raw)
	sha := hex.EncodeToString(sum[:])
	c.blobs[sha] = raw
	return sha, int64(len(raw)), nil
}

func (c *fakeCache) Get(_ context.Context, sha string) (io.ReadCloser, error) {
	raw, ok := c.blobs[sha]
	if !ok {
		return nil, fmt.Errorf("fake cache: %s: %w", sha, blob.ErrNotCached)
	}
	return io.NopCloser(bytes.NewReader(raw)), nil
}

type fakeBlobArchiver struct {
	blobs      map[string][]byte
	uploads    int
	failUpload bool
}

func newFakeBlobArchiver() *fakeBlobArchiver { return &fakeBlobArchiver{blobs: map[string][]byte{}} }

func (a *fakeBlobArchiver) UploadBlob(_ context.Context, sha string, r io.Reader) (string, error) {
	if a.failUpload {
		return "", errors.New("fake: s3 down")
	}
	if _, ok := a.blobs[sha]; !ok {
		raw, err := io.ReadAll(r)
		if err != nil {
			return "", err
		}
		a.blobs[sha] = raw
		a.uploads++
	}
	return cold.BlobKey("jin", sha), nil
}

func (a *fakeBlobArchiver) FetchBlob(_ context.Context, sha string) (io.ReadCloser, error) {
	raw, ok := a.blobs[sha]
	if !ok {
		return nil, fmt.Errorf("fake archiver: %s: %w", sha, cold.ErrNotFound)
	}
	return io.NopCloser(bytes.NewReader(raw)), nil
}

func (a *fakeBlobArchiver) BlobExists(_ context.Context, sha string) (bool, error) {
	_, ok := a.blobs[sha]
	return ok, nil
}

type fakeIndexer struct {
	indexed []episodic.Record
	fail    bool
}

func (f *fakeIndexer) IndexRecords(_ context.Context, _ hotstore.ProjectKey, recs []episodic.Record) error {
	if f.fail {
		return errors.New("fake: opensearch down")
	}
	f.indexed = append(f.indexed, recs...)
	return nil
}

type fakeGraph struct {
	nodes []knowledge.Node
	fail  bool
}

func (f *fakeGraph) UpsertNodes(_ context.Context, _ hotstore.ProjectKey, nodes []knowledge.Node) error {
	if f.fail {
		return errors.New("fake: neo4j down")
	}
	f.nodes = append(f.nodes, nodes...)
	return nil
}

// fakeStore is an in-memory hotstore.Store; only the methods the document
// pipeline touches carry real behavior.
type fakeStore struct {
	episodes  map[string][]episodic.Record
	graphs    map[string]knowledge.Graph
	dirty     map[string]bool
	appendErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		episodes: map[string][]episodic.Record{},
		graphs:   map[string]knowledge.Graph{},
		dirty:    map[string]bool{},
	}
}

func (s *fakeStore) AppendEpisode(_ context.Context, key hotstore.ProjectKey, rec episodic.Record) error {
	if s.appendErr != nil {
		return s.appendErr
	}
	s.episodes[key.String()] = append(s.episodes[key.String()], rec)
	return nil
}

func (s *fakeStore) ListEpisodes(_ context.Context, key hotstore.ProjectKey) ([]episodic.Record, error) {
	return slices.Clone(s.episodes[key.String()]), nil
}

func (s *fakeStore) GetEpisode(_ context.Context, key hotstore.ProjectKey, id string) (episodic.Record, error) {
	for _, rec := range s.episodes[key.String()] {
		if rec.ID == id {
			return rec, nil
		}
	}
	return episodic.Record{}, hotstore.ErrNotFound
}

func (s *fakeStore) UpdateEpisodes(_ context.Context, _ hotstore.ProjectKey, _ []string, _ func(episodic.Record) episodic.Record) error {
	return nil
}

func (s *fakeStore) RemoveEpisodes(_ context.Context, _ hotstore.ProjectKey, _ []string) error {
	return nil
}

func (s *fakeStore) ReadKnowledge(_ context.Context, key hotstore.ProjectKey) (knowledge.Graph, error) {
	return s.graphs[key.String()], nil
}

func (s *fakeStore) WriteKnowledge(_ context.Context, key hotstore.ProjectKey, g knowledge.Graph) error {
	s.graphs[key.String()] = g
	return nil
}

func (s *fakeStore) ListProjects(_ context.Context) ([]hotstore.ProjectKey, error) {
	seen := map[string]hotstore.ProjectKey{}
	for k := range s.episodes {
		seen[k] = splitKey(k)
	}
	for k := range s.graphs {
		seen[k] = splitKey(k)
	}
	var out []hotstore.ProjectKey
	for _, name := range slices.Sorted(mapsKeysIter(seen)) {
		out = append(out, seen[name])
	}
	return out, nil
}

func mapsKeysIter[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func splitKey(s string) hotstore.ProjectKey {
	parts := strings.SplitN(s, "/", 3)
	return hotstore.ProjectKey{Workspace: parts[0], Team: parts[1], Project: parts[2]}
}

func (s *fakeStore) Manifest(_ context.Context) (hotstore.Manifest, error) {
	return hotstore.Manifest{}, nil
}

func (s *fakeStore) UpdateManifest(_ context.Context, _ func(hotstore.Manifest) (hotstore.Manifest, error)) error {
	return nil
}

func (s *fakeStore) MarkDirty(_ context.Context, key hotstore.ProjectKey, plane hotstore.Plane) error {
	s.dirty[string(plane)+"/"+key.String()] = true
	return nil
}

func (s *fakeStore) FileInfo(_ context.Context, _ hotstore.ProjectKey, _ hotstore.Plane) (int64, time.Time, error) {
	return 0, time.Time{}, hotstore.ErrNotFound
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// ---- ingest tests ----------------------------------------------------------

var docKey = hotstore.ProjectKey{Workspace: "vms", Team: "core", Project: "memory"}

func newTestIngestor() (*Ingestor, *fakeStore, *fakeCache, *fakeBlobArchiver, *fakeIndexer, *fakeGraph) {
	store := newFakeStore()
	cache := newFakeCache()
	archiver := newFakeBlobArchiver()
	index := &fakeIndexer{}
	graph := &fakeGraph{}
	ing := NewIngestor(Deps{
		Store:     store,
		Cache:     cache,
		Archiver:  archiver,
		Index:     index,
		Graph:     graph,
		Extractor: DefaultExtractor{},
		Clock:     fixedClock{t: time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)},
	})
	return ing, store, cache, archiver, index, graph
}

func TestIngestTextDocument(t *testing.T) {
	ctx := context.Background()
	ing, store, _, archiver, index, graph := newTestIngestor()

	content := []byte("메모리 서버는 hot과 cold 계층을 가진다")
	res, err := ing.Ingest(ctx, docKey, "design.txt", bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	sum := sha256.Sum256(content)
	wantSHA := hex.EncodeToString(sum[:])
	if res.SHA != wantSHA {
		t.Errorf("sha = %q, want %q", res.SHA, wantSHA)
	}
	if res.BlobKey != cold.BlobKey("jin", wantSHA) {
		t.Errorf("blob key = %q", res.BlobKey)
	}
	if !res.Extractable {
		t.Error("expected extractable")
	}
	if res.Truncated != nil {
		t.Errorf("unexpected truncation %+v", res.Truncated)
	}
	if len(res.ChunkIDs) != 1 {
		t.Fatalf("chunk ids = %v, want exactly 1", res.ChunkIDs)
	}

	// Blob reached cold.
	if _, ok := archiver.blobs[wantSHA]; !ok {
		t.Error("blob missing from cold storage")
	}

	// Chunk episode landed in hot with correct refs.
	recs := store.episodes[docKey.String()]
	if len(recs) != 1 {
		t.Fatalf("hot episodes = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Kind != episodic.KindDocumentChunk || rec.Refs == nil || rec.Refs.DocSHA != wantSHA || rec.Refs.ChunkSeq != 0 {
		t.Errorf("chunk record = %+v", rec)
	}
	if rec.Consolidated {
		t.Error("new chunk must start unconsolidated")
	}

	// Best-effort mirrors received the data.
	if len(index.indexed) != 1 {
		t.Errorf("indexed records = %d, want 1", len(index.indexed))
	}
	if len(graph.nodes) != 1 {
		t.Fatalf("graph nodes = %d, want 1", len(graph.nodes))
	}

	// Knowledge document node in hot.
	g := store.graphs[docKey.String()]
	if len(g.Nodes) != 1 {
		t.Fatalf("knowledge nodes = %d, want 1", len(g.Nodes))
	}
	node := g.Nodes[0]
	if node.ID != res.NodeID || node.Kind != knowledge.KindDocument || node.Name != "design.txt" {
		t.Errorf("document node = %+v", node)
	}
	if !slices.Contains(node.Aliases, wantSHA) {
		t.Errorf("node aliases %v missing sha", node.Aliases)
	}
}

func TestIngestSameShaIsIdempotent(t *testing.T) {
	ctx := context.Background()
	ing, store, _, archiver, _, _ := newTestIngestor()

	content := []byte("same bytes twice")
	first, err := ing.Ingest(ctx, docKey, "a.txt", bytes.NewReader(content))
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	second, err := ing.Ingest(ctx, docKey, "a.txt", bytes.NewReader(content))
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}

	if archiver.uploads != 1 {
		t.Errorf("cold uploads = %d, want 1", archiver.uploads)
	}
	if len(store.episodes[docKey.String()]) != 1 {
		t.Errorf("hot episodes = %d, want 1 (no duplicate chunks)", len(store.episodes[docKey.String()]))
	}
	if len(store.graphs[docKey.String()].Nodes) != 1 {
		t.Errorf("knowledge nodes = %d, want 1 (no duplicate node)", len(store.graphs[docKey.String()].Nodes))
	}
	if second.NodeID != first.NodeID {
		t.Errorf("node id changed across re-ingest: %q vs %q", first.NodeID, second.NodeID)
	}
	if !slices.Equal(second.ChunkIDs, first.ChunkIDs) {
		t.Errorf("chunk ids changed across re-ingest: %v vs %v", first.ChunkIDs, second.ChunkIDs)
	}
}

func TestIngestIndexFailureIsDegradedNotFatal(t *testing.T) {
	ctx := context.Background()
	ing, store, _, _, index, _ := newTestIngestor()
	index.fail = true

	res, err := ing.Ingest(ctx, docKey, "b.txt", strings.NewReader("degraded write"))
	if err != nil {
		t.Fatalf("Ingest must succeed when only the index is down: %v", err)
	}
	if len(res.ChunkIDs) != 1 {
		t.Errorf("chunk ids = %v", res.ChunkIDs)
	}
	if !store.dirty["episodic/"+docKey.String()] {
		t.Error("manifest must be marked dirty for the episodic plane")
	}
}

func TestIngestGraphFailureIsDegradedNotFatal(t *testing.T) {
	ctx := context.Background()
	ing, store, _, _, _, graph := newTestIngestor()
	graph.fail = true

	if _, err := ing.Ingest(ctx, docKey, "c.txt", strings.NewReader("graph down")); err != nil {
		t.Fatalf("Ingest must succeed when only the graph is down: %v", err)
	}
	if !store.dirty["knowledge/"+docKey.String()] {
		t.Error("manifest must be marked dirty for the knowledge plane")
	}
}

func TestIngestColdFirstFailures(t *testing.T) {
	ctx := context.Background()

	t.Run("nil archiver rejects ingest", func(t *testing.T) {
		ing, _, _, _, _, _ := newTestIngestor()
		ing.deps.Archiver = nil
		if _, err := ing.Ingest(ctx, docKey, "d.txt", strings.NewReader("x")); !errors.Is(err, ErrColdUnavailable) {
			t.Fatalf("err = %v, want ErrColdUnavailable", err)
		}
	})

	t.Run("upload failure rejects ingest", func(t *testing.T) {
		ing, store, _, archiver, _, _ := newTestIngestor()
		archiver.failUpload = true
		if _, err := ing.Ingest(ctx, docKey, "d.txt", strings.NewReader("x")); err == nil {
			t.Fatal("expected error when blob upload fails (cold-first)")
		}
		if len(store.episodes[docKey.String()]) != 0 {
			t.Error("no chunks may be written when the cold upload failed")
		}
	})
}

func TestIngestUnextractable(t *testing.T) {
	ctx := context.Background()
	ing, store, _, _, _, _ := newTestIngestor()

	res, err := ing.Ingest(ctx, docKey, "photo.png", bytes.NewReader([]byte{0x89, 'P', 'N', 'G'}))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.Extractable {
		t.Error("png must be reported unextractable")
	}
	if len(res.ChunkIDs) != 0 {
		t.Errorf("chunk ids = %v, want none", res.ChunkIDs)
	}
	if res.NodeID == "" {
		t.Error("document node must still be created")
	}
	if len(store.episodes[docKey.String()]) != 0 {
		t.Error("no chunk episodes for unextractable input")
	}
}

func TestIngestTruncatesAtChunkCap(t *testing.T) {
	ctx := context.Background()
	ing, _, _, _, _, _ := newTestIngestor()

	// > 500 chunks * 2048 bytes: 501 * 2048 single-byte chars.
	big := strings.Repeat("a", 501*2048)
	res, err := ing.Ingest(ctx, docKey, "big.txt", strings.NewReader(big))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.Truncated == nil {
		t.Fatal("expected truncation report")
	}
	if res.Truncated.Total != 501 || res.Truncated.Indexed != 500 {
		t.Errorf("truncation = %+v, want {501 500}", res.Truncated)
	}
	if len(res.ChunkIDs) != 500 {
		t.Errorf("chunk ids = %d, want 500", len(res.ChunkIDs))
	}
}

func TestOriginal(t *testing.T) {
	ctx := context.Background()
	ing, _, cache, archiver, _, _ := newTestIngestor()

	content := []byte("original bytes")
	res, err := ing.Ingest(ctx, docKey, "orig.txt", bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	t.Run("served from cache", func(t *testing.T) {
		rc, err := ing.Original(ctx, res.SHA)
		if err != nil {
			t.Fatalf("Original: %v", err)
		}
		defer rc.Close()
		got, _ := io.ReadAll(rc)
		if !bytes.Equal(got, content) {
			t.Errorf("bytes = %q", got)
		}
	})

	t.Run("rehydrated from cold after eviction", func(t *testing.T) {
		delete(cache.blobs, res.SHA) // evict
		rc, err := ing.Original(ctx, res.SHA)
		if err != nil {
			t.Fatalf("Original after eviction: %v", err)
		}
		defer rc.Close()
		got, _ := io.ReadAll(rc)
		if !bytes.Equal(got, content) {
			t.Errorf("bytes = %q", got)
		}
		if _, ok := cache.blobs[res.SHA]; !ok {
			t.Error("blob must be re-cached after cold fetch")
		}
	})

	t.Run("unknown sha maps to hotstore.ErrNotFound", func(t *testing.T) {
		if _, err := ing.Original(ctx, strings.Repeat("0", 64)); !errors.Is(err, hotstore.ErrNotFound) {
			t.Fatalf("err = %v, want hotstore.ErrNotFound", err)
		}
	})

	_ = archiver
}

func TestChunksOrderedBySeq(t *testing.T) {
	ctx := context.Background()
	ing, store, _, _, _, _ := newTestIngestor()

	// Two chunks worth of ascii under a small file: force multiple chunks by
	// ingesting > 2048 bytes.
	content := strings.Repeat("x", 2048+10)
	res, err := ing.Ingest(ctx, docKey, "two.txt", strings.NewReader(content))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(res.ChunkIDs) != 2 {
		t.Fatalf("chunk ids = %d, want 2", len(res.ChunkIDs))
	}

	// Unrelated record must not appear.
	other := episodic.Record{ID: "01OTHER", Kind: episodic.KindEvent, Text: "no refs"}
	if err := store.AppendEpisode(ctx, docKey, other); err != nil {
		t.Fatal(err)
	}

	recs, err := ing.Chunks(ctx, res.SHA)
	if err != nil {
		t.Fatalf("Chunks: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("chunks = %d, want 2", len(recs))
	}
	for i, rec := range recs {
		if rec.Refs.ChunkSeq != i {
			t.Errorf("chunk %d has seq %d", i, rec.Refs.ChunkSeq)
		}
	}
	if recs[0].Text+recs[1].Text != content {
		t.Error("concatenated chunks must reproduce the extracted text")
	}
}
