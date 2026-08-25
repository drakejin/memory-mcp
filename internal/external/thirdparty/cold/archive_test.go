package cold

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/service/knowledge"
	"github.com/drakejin/memory-mcp/internal/x/errs"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// fakeObjects is an in-memory objectStore recording put counts per key.
type fakeObjects struct {
	objects     map[string][]byte
	puts        map[string]int
	failPut     bool
	failPutOnce map[string]bool // keys whose put fails, everything else succeeds
	failGet     bool
}

func newFakeObjects() *fakeObjects {
	return &fakeObjects{objects: map[string][]byte{}, puts: map[string]int{}}
}

func (f *fakeObjects) Put(_ context.Context, key string, r io.Reader) error {
	if f.failPut || f.failPutOnce[key] {
		return errs.Unavailable(opPut, errors.New("fake: put failed"))
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return errs.Internal(opPut, err)
	}
	f.objects[key] = data
	f.puts[key]++
	return nil
}

func (f *fakeObjects) Get(_ context.Context, key string) (io.ReadCloser, error) {
	if f.failGet {
		return nil, errs.Unavailable(opGet, errors.New("fake: get failed"))
	}
	data, ok := f.objects[key]
	if !ok {
		return nil, errs.NotFound(opGet, entityObject, key)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (f *fakeObjects) Exists(_ context.Context, key string) (bool, error) {
	_, ok := f.objects[key]
	return ok, nil
}

func (f *fakeObjects) List(_ context.Context, prefix string) ([]string, error) {
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys, nil
}

var testKey = projectkey.Key{Workspace: "vms", Team: "core", Project: "memory"}

func rec(id string, occurred time.Time) episode.Record {
	return episode.Record{
		ID:         id,
		Kind:       episode.KindEvent,
		OccurredAt: occurred,
		Actor:      episode.ActorAgent,
		Text:       "text of " + id,
		Entities:   []string{},
	}
}

func batchIDs(t *testing.T, objects *fakeObjects, key string) []string {
	t.Helper()
	data, ok := objects.objects[key]
	if !ok {
		t.Fatalf("expected object at %s", key)
	}
	var recs []episode.Record
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
		objects := newFakeObjects()
		c := newClient(objects, "jin")

		key, err := c.ArchiveEpisodes(ctx, testKey, "2026-07", []episode.Record{rec("01B", t1), rec("01A", t1)})
		if err != nil {
			t.Fatalf("ArchiveEpisodes: %v", err)
		}
		want := "jin/episodic/vms/core/memory/2026-07.json"
		if key != want {
			t.Errorf("key = %q, want %q", key, want)
		}
		if got := batchIDs(t, objects, want); !slices.Equal(got, []string{"01A", "01B"}) {
			t.Errorf("batch ids = %v, want sorted [01A 01B]", got)
		}
	})

	t.Run("merge is idempotent by id", func(t *testing.T) {
		objects := newFakeObjects()
		c := newClient(objects, "jin")

		if _, err := c.ArchiveEpisodes(ctx, testKey, "2026-07", []episode.Record{rec("01A", t1)}); err != nil {
			t.Fatalf("first archive: %v", err)
		}
		key, err := c.ArchiveEpisodes(ctx, testKey, "2026-07", []episode.Record{rec("01A", t1), rec("01C", t1)})
		if err != nil {
			t.Fatalf("second archive: %v", err)
		}
		if got := batchIDs(t, objects, key); !slices.Equal(got, []string{"01A", "01C"}) {
			t.Errorf("batch ids = %v, want [01A 01C] (no duplicate 01A)", got)
		}
	})

	t.Run("empty recs is a no-op", func(t *testing.T) {
		objects := newFakeObjects()
		c := newClient(objects, "jin")

		key, err := c.ArchiveEpisodes(ctx, testKey, "2026-07", nil)
		if err != nil {
			t.Fatalf("ArchiveEpisodes: %v", err)
		}
		if len(objects.objects) != 0 {
			t.Errorf("expected no upload, got objects %v", objects.objects)
		}
		if key == "" {
			t.Error("expected key to be reported even for a no-op")
		}
	})

	t.Run("put failure stays unavailable", func(t *testing.T) {
		objects := newFakeObjects()
		objects.failPut = true
		c := newClient(objects, "jin")

		_, err := c.ArchiveEpisodes(ctx, testKey, "2026-07", []episode.Record{rec("01A", t1)})
		if !errors.Is(err, errs.ErrUnavailable) {
			t.Fatalf("err = %v, want unavailable", err)
		}
		assertOp(t, err, opArchiveEpisodes)
	})

	t.Run("read failure is not mistaken for an empty batch", func(t *testing.T) {
		objects := newFakeObjects()
		objects.failGet = true
		c := newClient(objects, "jin")

		// Overwriting a batch we could not read would drop archived records.
		_, err := c.ArchiveEpisodes(ctx, testKey, "2026-07", []episode.Record{rec("01A", t1)})
		if !errors.Is(err, errs.ErrUnavailable) {
			t.Fatalf("err = %v, want unavailable", err)
		}
		if len(objects.objects) != 0 {
			t.Error("nothing may be written when the existing batch could not be read")
		}
	})

	t.Run("corrupt batch is internal", func(t *testing.T) {
		objects := newFakeObjects()
		objects.objects["jin/episodic/vms/core/memory/2026-07.json"] = []byte("{not json")
		c := newClient(objects, "jin")

		_, err := c.ArchiveEpisodes(ctx, testKey, "2026-07", []episode.Record{rec("01A", t1)})
		if !errors.Is(err, errs.ErrInternal) {
			t.Fatalf("err = %v, want internal", err)
		}
	})
}

func TestFetchArchivedEpisode(t *testing.T) {
	ctx := context.Background()
	objects := newFakeObjects()
	c := newClient(objects, "jin")

	july := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	june := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if _, err := c.ArchiveEpisodes(ctx, testKey, "2026-06", []episode.Record{rec("01OLD", june)}); err != nil {
		t.Fatalf("archive june: %v", err)
	}
	if _, err := c.ArchiveEpisodes(ctx, testKey, "2026-07", []episode.Record{rec("01NEW", july)}); err != nil {
		t.Fatalf("archive july: %v", err)
	}
	// A non-JSON sibling object must be skipped, not decoded.
	objects.objects["jin/episodic/vms/core/memory/README"] = []byte("not a batch")

	tests := []struct {
		name    string
		id      string
		wantErr error
	}{
		{name: "found in older month", id: "01OLD"},
		{name: "found in newer month", id: "01NEW"},
		{name: "missing id", id: "01NOPE", wantErr: errs.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := c.FetchArchivedEpisode(ctx, testKey, tt.id)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				var domain *errs.Error
				if errors.As(err, &domain); domain.Entity != entityEpisode || domain.ID != tt.id {
					t.Errorf("error = %+v, want entity %q and the requested id", domain, entityEpisode)
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

	t.Run("list failure surfaces unavailable", func(t *testing.T) {
		broken := newClient(&failingObjects{}, "jin")
		_, err := broken.FetchArchivedEpisode(ctx, testKey, "01OLD")
		if !errors.Is(err, errs.ErrUnavailable) {
			t.Fatalf("err = %v, want unavailable", err)
		}
		assertOp(t, err, opFetchArchivedEpisode)
	})
}

// failingObjects fails every read, standing in for an S3 outage.
type failingObjects struct{}

func (failingObjects) Put(context.Context, string, io.Reader) error {
	return errs.Unavailable(opPut, errors.New("fake: down"))
}

func (failingObjects) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, errs.Unavailable(opGet, errors.New("fake: down"))
}

func (failingObjects) Exists(context.Context, string) (bool, error) {
	return false, errs.Unavailable(opExists, errors.New("fake: down"))
}

func (failingObjects) List(context.Context, string) ([]string, error) {
	return nil, errs.Unavailable(opList, errors.New("fake: down"))
}

func TestSnapshotKnowledge(t *testing.T) {
	ctx := context.Background()
	objects := newFakeObjects()
	c := newClient(objects, "jin")

	g := knowledge.Graph{Nodes: []knowledge.Node{{ID: "01N", Kind: knowledge.KindFact, Name: "f"}}}
	ts := time.Date(2026, 8, 25, 12, 30, 45, 0, time.UTC)

	latest, snapshot, err := c.SnapshotKnowledge(ctx, testKey, g, ts)
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
	if !bytes.Equal(objects.objects[wantLatest], objects.objects[wantSnapshot]) {
		t.Error("latest and snapshot payloads differ")
	}
	var round knowledge.Graph
	if err := json.Unmarshal(objects.objects[wantLatest], &round); err != nil {
		t.Fatalf("decode latest: %v", err)
	}
	if len(round.Nodes) != 1 || round.Nodes[0].ID != "01N" {
		t.Errorf("round-tripped graph = %+v", round)
	}

	t.Run("put failure stays unavailable", func(t *testing.T) {
		broken := newClient(&failingObjects{}, "jin")
		if _, _, err := broken.SnapshotKnowledge(ctx, testKey, g, ts); !errors.Is(err, errs.ErrUnavailable) {
			t.Fatalf("err = %v, want unavailable", err)
		}
	})

	t.Run("a half-written snapshot pair is an error, not two keys", func(t *testing.T) {
		partial := newFakeObjects()
		partial.failPutOnce = map[string]bool{wantSnapshot: true}
		c := newClient(partial, "jin")

		latest, snapshot, err := c.SnapshotKnowledge(ctx, testKey, g, ts)
		if !errors.Is(err, errs.ErrUnavailable) {
			t.Fatalf("err = %v, want unavailable", err)
		}
		if latest != "" || snapshot != "" {
			t.Errorf("keys = %q, %q; a failed snapshot must report no keys", latest, snapshot)
		}
	})
}

func TestBlobLifecycle(t *testing.T) {
	ctx := context.Background()
	objects := newFakeObjects()
	c := newClient(objects, "jin")
	sha := "abcdef0123456789"
	wantKey := "jin/blobs/ab/" + sha

	_, err := c.FetchBlob(ctx, sha)
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("FetchBlob before upload err = %v, want not found", err)
	}
	var domain *errs.Error
	if errors.As(err, &domain); domain.Entity != entityBlob || domain.ID != sha {
		t.Errorf("miss = %+v, want it addressed as the blob sha", domain)
	}

	key, err := c.UploadBlob(ctx, sha, strings.NewReader("blob-bytes"))
	if err != nil {
		t.Fatalf("UploadBlob: %v", err)
	}
	if key != wantKey {
		t.Errorf("key = %q, want %q", key, wantKey)
	}

	// Same-sha re-upload skips the put (content-addressed idempotency).
	if _, err := c.UploadBlob(ctx, sha, strings.NewReader("blob-bytes")); err != nil {
		t.Fatalf("re-upload: %v", err)
	}
	if objects.puts[wantKey] != 1 {
		t.Errorf("put count = %d, want 1 (idempotent skip)", objects.puts[wantKey])
	}

	rc, err := c.FetchBlob(ctx, sha)
	if err != nil {
		t.Fatalf("FetchBlob: %v", err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != "blob-bytes" {
		t.Errorf("blob = %q, want %q", data, "blob-bytes")
	}

	if _, ok := objects.objects[wantKey]; !ok {
		t.Errorf("blob missing at %s after upload", wantKey)
	}
}

func TestBlobFailuresStayUnavailable(t *testing.T) {
	ctx := context.Background()
	c := newClient(&failingObjects{}, "jin")

	t.Run("fetch", func(t *testing.T) {
		if _, err := c.FetchBlob(ctx, "abc"); !errors.Is(err, errs.ErrUnavailable) {
			t.Fatalf("err = %v, want unavailable", err)
		}
	})
	t.Run("upload", func(t *testing.T) {
		if _, err := c.UploadBlob(ctx, "abc", strings.NewReader("x")); !errors.Is(err, errs.ErrUnavailable) {
			t.Fatalf("err = %v, want unavailable", err)
		}
		assertOp(t, mustErr(c.UploadBlob(ctx, "abc", strings.NewReader("x"))), opUploadBlob)
	})
}

// assertOp checks that the outermost domain error names the operation, which is
// what makes a log line readable without a stack trace.
func assertOp(t *testing.T, err error, want string) {
	t.Helper()
	var domain *errs.Error
	if !errors.As(err, &domain) {
		t.Fatalf("err = %v, want *errs.Error", err)
	}
	if domain.Op != want {
		t.Errorf("op = %q, want %q", domain.Op, want)
	}
}

func mustErr(_ string, err error) error { return err }
