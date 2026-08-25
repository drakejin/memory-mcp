package hotstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/x/errs"
)

const testIndexName = "opensearch"

func TestManifestTracksShaAndCount(t *testing.T) {
	store, home := newTestClient(t)
	ctx := context.Background()
	key := testKey()

	if err := store.AppendEpisode(ctx, key, testRecord("01AAAA", "hello")); err != nil {
		t.Fatal(err)
	}

	m, err := store.Manifest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := m.Files[ManifestFileKey(PlaneEpisodic, key)]
	if !ok {
		t.Fatalf("manifest missing file entry; have %v", m.Files)
	}
	if st.RecordCount != 1 {
		t.Errorf("record_count = %d, want 1", st.RecordCount)
	}
	// The recorded sha must equal the sha of the actual file bytes.
	data, err := os.ReadFile(episodicPath(home, key))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if st.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("manifest sha %q != file sha %q", st.SHA256, hex.EncodeToString(sum[:]))
	}
	// The injected clock stamps the manifest — never time.Now().
	if !m.UpdatedAt.Equal(testNow) {
		t.Errorf("UpdatedAt = %v, want clock time %v", m.UpdatedAt, testNow)
	}
}

func TestUpdateManifest(t *testing.T) {
	store, _ := newTestClient(t)
	ctx := context.Background()

	// Fresh manifest has non-nil empty maps.
	m, err := store.Manifest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m.Files == nil || m.Indexes == nil {
		t.Fatal("expected non-nil maps on fresh manifest")
	}

	err = store.UpdateManifest(ctx, func(m Manifest) (Manifest, error) {
		m.Indexes[testIndexName] = IndexState{LastHydratedSHA: "abc"}
		return m, nil
	})
	if err != nil {
		t.Fatalf("UpdateManifest: %v", err)
	}
	m, err = store.Manifest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m.Indexes[testIndexName].LastHydratedSHA != "abc" {
		t.Errorf("index state not persisted: %+v", m.Indexes)
	}
}

func TestUpdateManifestAbortsOnFnError(t *testing.T) {
	store, _ := newTestClient(t)
	ctx := context.Background()

	if err := store.UpdateManifest(ctx, func(m Manifest) (Manifest, error) {
		m.Indexes[testIndexName] = IndexState{LastHydratedSHA: "abc"}
		return m, nil
	}); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("boom")
	err := store.UpdateManifest(ctx, func(m Manifest) (Manifest, error) {
		m.Indexes[testIndexName] = IndexState{LastHydratedSHA: "should-not-persist"}
		return m, boom
	})
	// The cause survives wrapping so callers can still match their own error.
	if !errors.Is(err, boom) {
		t.Fatalf("expected fn error surfaced, got %v", err)
	}
	if !errors.Is(err, errs.ErrInternal) {
		t.Errorf("fn error = %v, want KindInternal at the boundary", err)
	}
	m, err := store.Manifest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m.Indexes[testIndexName].LastHydratedSHA != "abc" {
		t.Errorf("aborted update leaked to disk: %+v", m.Indexes)
	}
}

func TestManifestReturnsCopy(t *testing.T) {
	store, _ := newTestClient(t)
	ctx := context.Background()

	if err := store.UpdateManifest(ctx, func(m Manifest) (Manifest, error) {
		m.Indexes[testIndexName] = IndexState{LastHydratedSHA: "abc"}
		return m, nil
	}); err != nil {
		t.Fatal(err)
	}

	m, err := store.Manifest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	m.Indexes[testIndexName] = IndexState{LastHydratedSHA: "tampered"}

	again, err := store.Manifest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again.Indexes[testIndexName].LastHydratedSHA != "abc" {
		t.Error("Manifest() returned a shared map")
	}
}

func TestMarkDirtyAndWritePreservesFlags(t *testing.T) {
	store, _ := newTestClient(t)
	ctx := context.Background()
	key := testKey()
	fk := ManifestFileKey(PlaneEpisodic, key)

	if err := store.AppendEpisode(ctx, key, testRecord("01AAAA", "x")); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDirty(ctx, key, PlaneEpisodic); err != nil {
		t.Fatalf("MarkDirty: %v", err)
	}
	m, err := store.Manifest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Files[fk].Dirty {
		t.Fatal("dirty flag not set")
	}

	// A later hot write refreshes sha/count but preserves Dirty and IndexedAt
	// (they belong to the rehydration flow).
	indexed := testNow.Add(-time.Hour)
	if err := store.UpdateManifest(ctx, func(m Manifest) (Manifest, error) {
		st := m.Files[fk]
		st.IndexedAt = indexed
		m.Files[fk] = st
		return m, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEpisode(ctx, key, testRecord("01BBBB", "y")); err != nil {
		t.Fatal(err)
	}
	m, err = store.Manifest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st := m.Files[fk]
	if !st.Dirty {
		t.Error("hot write cleared dirty flag")
	}
	if !st.IndexedAt.Equal(indexed) {
		t.Errorf("hot write clobbered IndexedAt: %v", st.IndexedAt)
	}
	if st.RecordCount != 2 {
		t.Errorf("record_count = %d, want 2", st.RecordCount)
	}

	// MarkDirty on a plane with no prior entry still records the mark.
	if err := store.MarkDirty(ctx, key, PlaneKnowledge); err != nil {
		t.Fatal(err)
	}
	m, err = store.Manifest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Files[ManifestFileKey(PlaneKnowledge, key)].Dirty {
		t.Error("dirty mark lost for entry-less plane")
	}
}
