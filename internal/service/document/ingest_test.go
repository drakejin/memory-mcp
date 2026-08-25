package document

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/service/knowledge"
	"github.com/drakejin/memory-mcp/internal/x/config"
	"github.com/drakejin/memory-mcp/internal/x/errs"
)

func TestIngestTextDocument(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)

	content := []byte("메모리 서버는 hot과 cold 계층을 가진다")
	res, err := r.svc.Ingest(ctx, docKey, "design.txt", bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	sum := sha256.Sum256(content)
	wantSHA := hex.EncodeToString(sum[:])
	if res.SHA != wantSHA {
		t.Errorf("sha = %q, want %q", res.SHA, wantSHA)
	}
	if res.BlobKey != blobKeyFor(wantSHA) {
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
	if _, ok := r.archiver.blobs[wantSHA]; !ok {
		t.Error("blob missing from cold storage")
	}

	// Chunk episode landed in hot with correct refs.
	recs := r.store.episodes[docKey.String()]
	if len(recs) != 1 {
		t.Fatalf("hot episodes = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Kind != episode.KindDocumentChunk || rec.Refs == nil || rec.Refs.DocSHA != wantSHA || rec.Refs.ChunkSeq != 0 {
		t.Errorf("chunk record = %+v", rec)
	}
	if rec.Consolidated {
		t.Error("new chunk must start unconsolidated")
	}

	// Best-effort mirrors received the data.
	if len(r.index.indexed) != 1 {
		t.Errorf("indexed records = %d, want 1", len(r.index.indexed))
	}
	if len(r.graph.nodes) != 1 {
		t.Fatalf("graph nodes = %d, want 1", len(r.graph.nodes))
	}

	// Knowledge document node in hot.
	g := r.store.graphs[docKey.String()]
	if len(g.Nodes) != 1 {
		t.Fatalf("knowledge nodes = %d, want 1", len(g.Nodes))
	}
	node := g.Nodes[0]
	if node.ID != res.NodeID || node.Kind != knowledge.KindDocument || node.Name != "design.txt" {
		t.Errorf("document node = %+v", node)
	}
	if node.Body != shaBodyPrefix+wantSHA {
		t.Errorf("node body = %q, want the sha, no summary", node.Body)
	}
	if !slices.Contains(node.Aliases, wantSHA) {
		t.Errorf("node aliases %v missing sha", node.Aliases)
	}
}

func TestIngestSameShaIsIdempotent(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)

	content := []byte("same bytes twice")
	first, err := r.svc.Ingest(ctx, docKey, "a.txt", bytes.NewReader(content))
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	second, err := r.svc.Ingest(ctx, docKey, "a.txt", bytes.NewReader(content))
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}

	if r.archiver.uploads != 1 {
		t.Errorf("cold uploads = %d, want 1", r.archiver.uploads)
	}
	if len(r.store.episodes[docKey.String()]) != 1 {
		t.Errorf("hot episodes = %d, want 1 (no duplicate chunks)", len(r.store.episodes[docKey.String()]))
	}
	if len(r.store.graphs[docKey.String()].Nodes) != 1 {
		t.Errorf("knowledge nodes = %d, want 1 (no duplicate node)", len(r.store.graphs[docKey.String()].Nodes))
	}
	if second.NodeID != first.NodeID {
		t.Errorf("node id changed across re-ingest: %q vs %q", first.NodeID, second.NodeID)
	}
	if !slices.Equal(second.ChunkIDs, first.ChunkIDs) {
		t.Errorf("chunk ids changed across re-ingest: %v vs %v", first.ChunkIDs, second.ChunkIDs)
	}
}

// TestIngestDerivedFailureIsDegradedNotFatal pins the §1/§5 rule: the hot write
// is authoritative, so a dead derived store degrades the ingest (dirty mark +
// log) instead of failing it.
//
// wantNote is the part that makes the degradation honest rather than merely
// survivable. A dirty mark and a log line are server-side state; the client sees
// neither. Without the note in the response an agent uploads a PDF, gets 201
// with chunk_ids and a node_id, and has no way to learn that none of those
// chunks are searchable and the node never reached the graph (§0 principle 3).
func TestIngestDerivedFailureIsDegradedNotFatal(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name      string
		arrange   func(*rig)
		tweak     func(*Config)
		wantDirty string
		wantNote  string
	}{
		{
			name:      "search index down",
			arrange:   func(r *rig) { r.index.fail = true },
			wantDirty: "episodic/" + docKey.String(),
			wantNote:  degradedSearch,
		},
		{
			name:      "search index not configured",
			tweak:     func(c *Config) { c.Index = nil },
			wantDirty: "episodic/" + docKey.String(),
			wantNote:  degradedSearch,
		},
		{
			name:      "graph down",
			arrange:   func(r *rig) { r.graph.fail = true },
			wantDirty: "knowledge/" + docKey.String(),
			wantNote:  degradedGraph,
		},
		{
			name:      "graph not configured",
			tweak:     func(c *Config) { c.Graph = nil },
			wantDirty: "knowledge/" + docKey.String(),
			wantNote:  degradedGraph,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tweaks []func(*Config)
			if tt.tweak != nil {
				tweaks = append(tweaks, tt.tweak)
			}
			r := newRig(t, tweaks...)
			if tt.arrange != nil {
				tt.arrange(r)
			}

			res, err := r.svc.Ingest(ctx, docKey, "b.txt", strings.NewReader("degraded write"))
			if err != nil {
				t.Fatalf("Ingest must succeed when only a derived store is down: %v", err)
			}
			if len(res.ChunkIDs) != 1 {
				t.Errorf("chunk ids = %v, want 1", res.ChunkIDs)
			}
			if !r.store.dirty[tt.wantDirty] {
				t.Errorf("manifest must be marked dirty at %q, dirty = %v", tt.wantDirty, r.store.dirty)
			}
			if !slices.Contains(res.Degraded, tt.wantNote) {
				t.Errorf("degraded = %v, want it to name %q — the response is the only channel the client can read",
					res.Degraded, tt.wantNote)
			}
			if !strings.Contains(r.logs.String(), "degraded") {
				t.Error("degradation must be logged")
			}
		})
	}
}

// TestIngestFullySuccessfulReportsNoDegradation is the other half of the
// contract: a note that is always present is worth nothing. With every derived
// store healthy the response must claim nothing.
func TestIngestFullySuccessfulReportsNoDegradation(t *testing.T) {
	// Arrange
	r := newRig(t)

	// Act
	res, err := r.svc.Ingest(context.Background(), docKey, "b.txt", strings.NewReader("healthy write"))

	// Assert
	if err != nil {
		t.Fatalf("Ingest() = %v", err)
	}
	if len(res.Degraded) != 0 {
		t.Fatalf("degraded = %v, want empty with every derived store up", res.Degraded)
	}
}

// TestIngestAccumulatesEveryDegradedNote: the two mirrors fail independently, so
// one response must be able to carry both notes. A result that replaced the
// chunk note with the node note would under-report precisely when the most is
// missing — the case where an agent most needs to know to re-index.
func TestIngestAccumulatesEveryDegradedNote(t *testing.T) {
	// Arrange
	r := newRig(t, func(c *Config) { c.Index, c.Graph = nil, nil })

	// Act
	res, err := r.svc.Ingest(context.Background(), docKey, "b.txt", strings.NewReader("both mirrors down"))

	// Assert
	if err != nil {
		t.Fatalf("Ingest() = %v", err)
	}
	want := []string{degradedSearch, degradedGraph}
	for _, note := range want {
		if !slices.Contains(res.Degraded, note) {
			t.Errorf("degraded = %v, want it to name %q", res.Degraded, note)
		}
	}
	if len(res.Degraded) != len(want) {
		t.Errorf("degraded = %v, want exactly %d notes", res.Degraded, len(want))
	}
}

// TestIngestDirtyMarkFailureStillSucceeds: losing the dirty mark is bad but the
// hot write already happened, so the ingest still reports success.
func TestIngestDirtyMarkFailureStillSucceeds(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	r.index.fail = true
	r.store.markDirtyErr = errs.Internal("hotstore.MarkDirty", errors.New("disk full"))

	if _, err := r.svc.Ingest(ctx, docKey, "b.txt", strings.NewReader("x")); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !strings.Contains(r.logs.String(), "mark dirty failed") {
		t.Error("a failed dirty mark must be logged")
	}
}

func TestIngestColdFirstFailures(t *testing.T) {
	ctx := context.Background()

	t.Run("missing archiver rejects ingest as unavailable", func(t *testing.T) {
		r := newRig(t, func(c *Config) { c.Archiver = nil })

		_, err := r.svc.Ingest(ctx, docKey, "d.txt", strings.NewReader("x"))
		if !errors.Is(err, errs.ErrUnavailable) {
			t.Fatalf("err = %v, want unavailable", err)
		}
		if len(r.store.episodes[docKey.String()]) != 0 {
			t.Error("no chunks may be written without a cold copy")
		}
	})

	t.Run("upload failure rejects ingest", func(t *testing.T) {
		r := newRig(t)
		r.archiver.failUpload = true

		_, err := r.svc.Ingest(ctx, docKey, "d.txt", strings.NewReader("x"))
		if !errors.Is(err, errs.ErrUnavailable) {
			t.Fatalf("err = %v, want unavailable (cold-first)", err)
		}
		if len(r.store.episodes[docKey.String()]) != 0 {
			t.Error("no chunks may be written when the cold upload failed")
		}
	})
}

// TestIngestStoreFailuresPropagate proves hot-store failures are fatal — the
// canonical plane is not best-effort.
func TestIngestStoreFailuresPropagate(t *testing.T) {
	ctx := context.Background()
	boom := errs.Internal("hotstore.op", errors.New("disk gone"))

	tests := []struct {
		name    string
		arrange func(*rig)
	}{
		{name: "cache put", arrange: func(r *rig) { r.cache.putErr = boom }},
		{name: "cache get", arrange: func(r *rig) { r.cache.getErr = boom }},
		{name: "list episodes", arrange: func(r *rig) { r.store.listErr = boom }},
		{name: "append episode", arrange: func(r *rig) { r.store.appendErr = boom }},
		{name: "read knowledge", arrange: func(r *rig) { r.store.readErr = boom }},
		{name: "write knowledge", arrange: func(r *rig) { r.store.writeErr = boom }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t)
			tt.arrange(r)

			_, err := r.svc.Ingest(ctx, docKey, "e.txt", strings.NewReader("payload"))
			if !errors.Is(err, errs.ErrInternal) {
				t.Fatalf("err = %v, want internal", err)
			}
			var domain *errs.Error
			if !errors.As(err, &domain) {
				t.Fatalf("err = %v, want *errs.Error", err)
			}
			if domain.Op != opIngest {
				t.Errorf("op = %q, want %q", domain.Op, opIngest)
			}
		})
	}
}

// TestIngestIDFailuresPropagate: an id we could not mint means a record we
// cannot address forever (ids are the provenance anchor), so the ingest fails
// rather than inventing one.
func TestIngestIDFailuresPropagate(t *testing.T) {
	ctx := context.Background()
	boom := errs.Internal("ulid.GenerateAt", errors.New("entropy source empty"))

	tests := []struct {
		name string
		fail int // 1-based generator call that fails
	}{
		{name: "chunk id", fail: 1},
		{name: "document node id", fail: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t)
			r.ids.err = boom
			r.ids.fail = tt.fail

			_, err := r.svc.Ingest(ctx, docKey, "f.txt", strings.NewReader("one chunk"))
			if !errors.Is(err, errs.ErrInternal) {
				t.Fatalf("err = %v, want internal", err)
			}
			var domain *errs.Error
			if errors.As(err, &domain); domain.Op != opIngest {
				t.Errorf("op = %q, want %q", domain.Op, opIngest)
			}
		})
	}
}

func TestIngestUnextractable(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)

	res, err := r.svc.Ingest(ctx, docKey, "photo.png", bytes.NewReader([]byte{0x89, 'P', 'N', 'G'}))
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
	if len(r.store.episodes[docKey.String()]) != 0 {
		t.Error("no chunk episodes for unextractable input")
	}
}

func TestIngestTruncatesAtChunkCap(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)

	// One chunk past the cap: (MaxDocumentChunks+1) * DocumentChunkBytes ascii.
	overflow := config.MaxDocumentChunks + 1
	big := strings.Repeat("a", overflow*config.DocumentChunkBytes)
	res, err := r.svc.Ingest(ctx, docKey, "big.txt", strings.NewReader(big))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.Truncated == nil {
		t.Fatal("expected truncation report")
	}
	if res.Truncated.Total != overflow || res.Truncated.Indexed != config.MaxDocumentChunks {
		t.Errorf("truncation = %+v, want {%d %d}", res.Truncated, overflow, config.MaxDocumentChunks)
	}
	if len(res.ChunkIDs) != config.MaxDocumentChunks {
		t.Errorf("chunk ids = %d, want %d", len(res.ChunkIDs), config.MaxDocumentChunks)
	}

	// Re-ingesting the same document must keep reporting the true total.
	again, err := r.svc.Ingest(ctx, docKey, "big.txt", strings.NewReader(big))
	if err != nil {
		t.Fatalf("re-ingest: %v", err)
	}
	if again.Truncated == nil || again.Truncated.Total != overflow {
		t.Errorf("re-ingest truncation = %+v, want the same honest total", again.Truncated)
	}
}
