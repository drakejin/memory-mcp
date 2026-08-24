package cold

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/knowledge"
)

// fakeStorage is an in-memory Storage recording put counts per key.
type fakeStorage struct {
	objects map[string][]byte
	puts    map[string]int
	failPut bool
}

func newFakeStorage() *fakeStorage {
	return &fakeStorage{objects: map[string][]byte{}, puts: map[string]int{}}
}

func (f *fakeStorage) Put(_ context.Context, key string, r io.Reader) error {
	if f.failPut {
		return errors.New("fake: put failed")
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.objects[key] = data
	f.puts[key]++
	return nil
}

func (f *fakeStorage) Get(_ context.Context, key string) (io.ReadCloser, error) {
	data, ok := f.objects[key]
	if !ok {
		return nil, fmt.Errorf("fake: %s: %w", key, ErrNotFound)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (f *fakeStorage) Exists(_ context.Context, key string) (bool, error) {
	_, ok := f.objects[key]
	return ok, nil
}

func (f *fakeStorage) List(_ context.Context, prefix string) ([]string, error) {
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys, nil
}

var testKey = hotstore.ProjectKey{Workspace: "vms", Team: "core", Project: "memory"}

func rec(id string, occurred time.Time) episodic.Record {
	return episodic.Record{
		ID:         id,
		Kind:       episodic.KindEvent,
		OccurredAt: occurred,
		Actor:      episodic.ActorAgent,
		Text:       "text of " + id,
		Entities:   []string{},
	}
}

func batchIDs(t *testing.T, storage *fakeStorage, key string) []string {
	t.Helper()
	data, ok := storage.objects[key]
	if !ok {
		t.Fatalf("expected object at %s", key)
	}
	var recs []episodic.Record
	if err := json.Unmarshal(data, &recs); err != nil {
		t.Fatalf("decode batch %s: %v", key, err)
	}
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		ids = append(ids, r.ID)
	}
	return ids
}

func TestArchiveEpisodes(t *testing.T) {
	ctx := context.Background()
	t1 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	t.Run("creates new batch", func(t *testing.T) {
		storage := newFakeStorage()
		a := NewArchiver(storage, "jin")

		key, err := a.ArchiveEpisodes(ctx, testKey, "2026-07", []episodic.Record{rec("01B", t1), rec("01A", t1)})
		if err != nil {
			t.Fatalf("ArchiveEpisodes: %v", err)
		}
		want := "jin/episodic/vms/core/memory/2026-07.json"
		if key != want {
			t.Errorf("key = %q, want %q", key, want)
		}
		if got := batchIDs(t, storage, want); !slices.Equal(got, []string{"01A", "01B"}) {
			t.Errorf("batch ids = %v, want sorted [01A 01B]", got)
		}
	})

	t.Run("merge is idempotent by id", func(t *testing.T) {
		storage := newFakeStorage()
		a := NewArchiver(storage, "jin")

		if _, err := a.ArchiveEpisodes(ctx, testKey, "2026-07", []episodic.Record{rec("01A", t1)}); err != nil {
			t.Fatalf("first archive: %v", err)
		}
		key, err := a.ArchiveEpisodes(ctx, testKey, "2026-07", []episodic.Record{rec("01A", t1), rec("01C", t1)})
		if err != nil {
			t.Fatalf("second archive: %v", err)
		}
		if got := batchIDs(t, storage, key); !slices.Equal(got, []string{"01A", "01C"}) {
			t.Errorf("batch ids = %v, want [01A 01C] (no duplicate 01A)", got)
		}
	})

	t.Run("empty recs is a no-op", func(t *testing.T) {
		storage := newFakeStorage()
		a := NewArchiver(storage, "jin")

		key, err := a.ArchiveEpisodes(ctx, testKey, "2026-07", nil)
		if err != nil {
			t.Fatalf("ArchiveEpisodes: %v", err)
		}
		if len(storage.objects) != 0 {
			t.Errorf("expected no upload, got objects %v", storage.objects)
		}
		if key == "" {
			t.Error("expected key to be reported even for a no-op")
		}
	})

	t.Run("put failure surfaces error", func(t *testing.T) {
		storage := newFakeStorage()
		storage.failPut = true
		a := NewArchiver(storage, "jin")

		if _, err := a.ArchiveEpisodes(ctx, testKey, "2026-07", []episodic.Record{rec("01A", t1)}); err == nil {
			t.Fatal("expected error when storage put fails")
		}
	})
}

func TestFetchArchivedEpisode(t *testing.T) {
	ctx := context.Background()
	storage := newFakeStorage()
	a := NewArchiver(storage, "jin")

	july := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	june := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if _, err := a.ArchiveEpisodes(ctx, testKey, "2026-06", []episodic.Record{rec("01OLD", june)}); err != nil {
		t.Fatalf("archive june: %v", err)
	}
	if _, err := a.ArchiveEpisodes(ctx, testKey, "2026-07", []episodic.Record{rec("01NEW", july)}); err != nil {
		t.Fatalf("archive july: %v", err)
	}

	tests := []struct {
		name    string
		id      string
		wantErr error
	}{
		{name: "found in older month", id: "01OLD"},
		{name: "found in newer month", id: "01NEW"},
		{name: "missing id", id: "01NOPE", wantErr: ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := a.FetchArchivedEpisode(ctx, testKey, tt.id)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("FetchArchivedEpisode: %v", err)
			}
			if got.ID != tt.id {
				t.Errorf("record id = %q, want %q", got.ID, tt.id)
			}
		})
	}
}

func TestSnapshotKnowledge(t *testing.T) {
	ctx := context.Background()
	storage := newFakeStorage()
	a := NewArchiver(storage, "jin")

	g := knowledge.Graph{Nodes: []knowledge.Node{{ID: "01N", Kind: knowledge.KindFact, Name: "f"}}}
	ts := time.Date(2026, 8, 25, 12, 30, 45, 0, time.UTC)

	latest, snapshot, err := a.SnapshotKnowledge(ctx, testKey, g, ts)
	if err != nil {
		t.Fatalf("SnapshotKnowledge: %v", err)
	}
	wantLatest := "jin/knowledge/vms/core/memory/latest.json"
	wantSnapshot := "jin/knowledge/vms/core/memory/snapshots/20260825T123045Z.json"
	if latest != wantLatest {
		t.Errorf("latest = %q, want %q", latest, wantLatest)
	}
	if snapshot != wantSnapshot {
		t.Errorf("snapshot = %q, want %q", snapshot, wantSnapshot)
	}
	if !bytes.Equal(storage.objects[wantLatest], storage.objects[wantSnapshot]) {
		t.Error("latest and snapshot payloads differ")
	}
	var round knowledge.Graph
	if err := json.Unmarshal(storage.objects[wantLatest], &round); err != nil {
		t.Fatalf("decode latest: %v", err)
	}
	if len(round.Nodes) != 1 || round.Nodes[0].ID != "01N" {
		t.Errorf("round-tripped graph = %+v", round)
	}
}

func TestBlobLifecycle(t *testing.T) {
	ctx := context.Background()
	storage := newFakeStorage()
	a := NewArchiver(storage, "jin")
	sha := "abcdef0123456789"
	wantKey := "jin/blobs/ab/" + sha

	exists, err := a.BlobExists(ctx, sha)
	if err != nil || exists {
		t.Fatalf("BlobExists before upload = %v, %v; want false, nil", exists, err)
	}
	if _, err := a.FetchBlob(ctx, sha); !errors.Is(err, ErrNotFound) {
		t.Fatalf("FetchBlob before upload err = %v, want ErrNotFound", err)
	}

	key, err := a.UploadBlob(ctx, sha, strings.NewReader("blob-bytes"))
	if err != nil {
		t.Fatalf("UploadBlob: %v", err)
	}
	if key != wantKey {
		t.Errorf("key = %q, want %q", key, wantKey)
	}

	// Same-sha re-upload skips the put (content-addressed idempotency).
	if _, err := a.UploadBlob(ctx, sha, strings.NewReader("blob-bytes")); err != nil {
		t.Fatalf("re-upload: %v", err)
	}
	if storage.puts[wantKey] != 1 {
		t.Errorf("put count = %d, want 1 (idempotent skip)", storage.puts[wantKey])
	}

	rc, err := a.FetchBlob(ctx, sha)
	if err != nil {
		t.Fatalf("FetchBlob: %v", err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != "blob-bytes" {
		t.Errorf("blob = %q, want %q", data, "blob-bytes")
	}

	exists, err = a.BlobExists(ctx, sha)
	if err != nil || !exists {
		t.Errorf("BlobExists after upload = %v, %v; want true, nil", exists, err)
	}
}
