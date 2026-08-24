package document

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/drakejin/memory-mcp/internal/config"
	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/errs"
	"github.com/drakejin/memory-mcp/internal/knowledge"
)

// unknownSHA is a well-formed sha256 that was never ingested.
const unknownSHA = "0000000000000000000000000000000000000000000000000000000000000000"

func TestOriginal(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)

	content := []byte("original bytes")
	res, err := r.svc.Ingest(ctx, docKey, "orig.txt", bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	t.Run("served from cache", func(t *testing.T) {
		rc, err := r.svc.Original(ctx, res.SHA)
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
		delete(r.cache.blobs, res.SHA) // evict
		rc, err := r.svc.Original(ctx, res.SHA)
		if err != nil {
			t.Fatalf("Original after eviction: %v", err)
		}
		defer rc.Close()
		got, _ := io.ReadAll(rc)
		if !bytes.Equal(got, content) {
			t.Errorf("bytes = %q", got)
		}
		if _, ok := r.cache.blobs[res.SHA]; !ok {
			t.Error("blob must be re-cached after cold fetch")
		}
	})
}

// TestOriginalErrors pins which failures become not-found (the client asked for
// something that is not here) and which stay unavailable or internal (we failed
// the client). Confusing the two is what makes a degraded store look empty.
func TestOriginalErrors(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name    string
		sha     string
		arrange func(*rig)
		tweak   func(*Config)
		want    error
	}{
		{
			name:    "unknown sha is not found",
			sha:     unknownSHA,
			arrange: func(*rig) {},
			want:    errs.ErrNotFound,
		},
		{
			name:    "not cached and no archiver is not found",
			sha:     unknownSHA,
			arrange: func(*rig) {},
			tweak:   func(c *Config) { c.Archiver = nil },
			want:    errs.ErrNotFound,
		},
		{
			name: "cold outage is unavailable, never not found",
			sha:  unknownSHA,
			arrange: func(r *rig) {
				r.archiver.failFetch = true
			},
			want: errs.ErrUnavailable,
		},
		{
			name: "cache read failure is not mistaken for a miss",
			sha:  unknownSHA,
			arrange: func(r *rig) {
				// Get fails, but the cache still claims to hold the blob:
				// falling back to cold would hide a broken local cache.
				r.cache.blobs[unknownSHA] = []byte("unreadable")
				r.cache.getErr = errs.Internal("blob.Get", errors.New("permission denied"))
			},
			want: errs.ErrInternal,
		},
		{
			name: "corrupt cold object is internal",
			sha:  unknownSHA,
			arrange: func(r *rig) {
				// Cold answers with bytes whose sha is not the requested one.
				r.archiver.blobs[unknownSHA] = []byte("not the addressed content")
			},
			want: errs.ErrInternal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tweaks []func(*Config)
			if tt.tweak != nil {
				tweaks = append(tweaks, tt.tweak)
			}
			r := newRig(t, tweaks...)
			tt.arrange(r)

			rc, err := r.svc.Original(ctx, tt.sha)
			if rc != nil {
				rc.Close()
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			var domain *errs.Error
			if !errors.As(err, &domain) {
				t.Fatalf("err = %v, want *errs.Error", err)
			}
			if domain.Op != opOriginal {
				t.Errorf("op = %q, want %q", domain.Op, opOriginal)
			}
		})
	}
}

// TestOriginalStreamsFromColdWhenRecacheFails: cold is authoritative, so a
// broken local cache must not deny a document that exists in cold.
func TestOriginalStreamsFromColdWhenRecacheFails(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)

	content := []byte("cold only bytes")
	res, err := r.svc.Ingest(ctx, docKey, "orig.txt", bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	delete(r.cache.blobs, res.SHA)
	r.cache.putErr = errs.Internal("blob.Put", errors.New("read-only filesystem"))

	rc, err := r.svc.Original(ctx, res.SHA)
	if err != nil {
		t.Fatalf("Original: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, content) {
		t.Errorf("bytes = %q, want %q", got, content)
	}
	if !strings.Contains(r.logs.String(), "re-cache failed") {
		t.Error("the fallback must be logged, not silent")
	}
}

// TestOriginalColdLostAfterRecacheFailure covers the second cold read: if cold
// goes away between the first fetch and the re-stream, the caller must see the
// outage rather than a nil reader.
func TestOriginalColdLostAfterRecacheFailure(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)

	content := []byte("cold only bytes")
	res, err := r.svc.Ingest(ctx, docKey, "orig.txt", bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	delete(r.cache.blobs, res.SHA)
	r.cache.putErr = errs.Internal("blob.Put", errors.New("read-only filesystem"))
	r.archiver.failFetchFrom = 2 // the re-stream, not the first read

	rc, err := r.svc.Original(ctx, res.SHA)
	if rc != nil {
		rc.Close()
		t.Error("no reader may be returned alongside an error")
	}
	if !errors.Is(err, errs.ErrUnavailable) {
		t.Fatalf("err = %v, want unavailable", err)
	}
}

func TestChunksOrderedBySeq(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)

	// Force two chunks by ingesting more than one chunk budget.
	content := strings.Repeat("x", config.DocumentChunkBytes+10)
	res, err := r.svc.Ingest(ctx, docKey, "two.txt", strings.NewReader(content))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(res.ChunkIDs) != 2 {
		t.Fatalf("chunk ids = %d, want 2", len(res.ChunkIDs))
	}

	// Unrelated records must not appear: a plain event, and a chunk of another
	// document in another project.
	other := episodic.Record{ID: "01OTHER", Kind: episodic.KindEvent, Text: "no refs"}
	if err := r.store.AppendEpisode(ctx, docKey, other); err != nil {
		t.Fatal(err)
	}
	elsewhere := docKey
	elsewhere.Project = "other"
	foreign := episodic.Record{
		ID:   "01FOREIGN",
		Kind: episodic.KindDocumentChunk,
		Refs: &episodic.Refs{DocSHA: unknownSHA, ChunkSeq: 0},
	}
	if err := r.store.AppendEpisode(ctx, elsewhere, foreign); err != nil {
		t.Fatal(err)
	}

	recs, err := r.svc.Chunks(ctx, res.SHA)
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

func TestChunksUnknownShaIsEmptyNotAnError(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)

	recs, err := r.svc.Chunks(ctx, unknownSHA)
	if err != nil {
		t.Fatalf("Chunks: %v", err)
	}
	if recs == nil {
		t.Fatal("Chunks must return an empty slice, never nil (json shape)")
	}
	if len(recs) != 0 {
		t.Errorf("chunks = %v, want none", recs)
	}
}

func TestChunksStoreFailuresPropagate(t *testing.T) {
	ctx := context.Background()
	boom := errs.Internal("hotstore.op", errors.New("disk gone"))

	tests := []struct {
		name    string
		arrange func(*rig)
	}{
		{name: "list projects", arrange: func(r *rig) { r.store.projectsErr = boom }},
		{name: "list episodes", arrange: func(r *rig) { r.store.listErr = boom }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t)
			// A project must exist for the per-project listing to be reached.
			r.store.graphs[docKey.String()] = knowledge.Graph{}
			tt.arrange(r)

			_, err := r.svc.Chunks(ctx, unknownSHA)
			if !errors.Is(err, errs.ErrInternal) {
				t.Fatalf("err = %v, want internal", err)
			}
			var domain *errs.Error
			if errors.As(err, &domain); domain.Op != opChunks {
				t.Errorf("op = %q, want %q", domain.Op, opChunks)
			}
		})
	}
}
