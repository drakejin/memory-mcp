package hotstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/knowledge"
)

type fakeClock struct{ t time.Time }

func (f fakeClock) Now() time.Time { return f.t }

var testNow = time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

func newTestStore(t *testing.T) (*FileStore, string) {
	t.Helper()
	home := t.TempDir()
	return New(home, fakeClock{t: testNow}), home
}

func testKey() ProjectKey {
	return ProjectKey{Workspace: "ws", Team: "team", Project: "proj"}
}

func testRecord(id, text string) episodic.Record {
	return episodic.Record{
		ID:         id,
		Kind:       episodic.KindEvent,
		OccurredAt: testNow,
		Actor:      episodic.ActorAgent,
		Text:       text,
		Entities:   []string{"memory-mcp"},
	}
}

func TestProjectKeyValidate(t *testing.T) {
	tests := []struct {
		name    string
		key     ProjectKey
		wantErr bool
	}{
		{"valid simple", ProjectKey{"ws", "team", "proj"}, false},
		{"valid full charset", ProjectKey{"my-ws.1", "team_x", "proj-2.0"}, false},
		{"empty workspace", ProjectKey{"", "team", "proj"}, true},
		{"empty team", ProjectKey{"ws", "", "proj"}, true},
		{"empty project", ProjectKey{"ws", "team", ""}, true},
		{"uppercase rejected", ProjectKey{"WS", "team", "proj"}, true},
		{"slash rejected", ProjectKey{"ws/evil", "team", "proj"}, true},
		{"backslash rejected", ProjectKey{"ws", `te\am`, "proj"}, true},
		{"space rejected", ProjectKey{"ws", "te am", "proj"}, true},
		{"dot-dot segment rejected", ProjectKey{"ws", "..", "proj"}, true},
		{"embedded dot-dot rejected", ProjectKey{"ws", "team", "a..b"}, true},
		{"single dot rejected", ProjectKey{".", "team", "proj"}, true},
		{"unicode rejected", ProjectKey{"ws", "팀", "proj"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.key.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestManifestFileKey(t *testing.T) {
	got := ManifestFileKey(PlaneEpisodic, testKey())
	if got != "episodic/ws/team/proj" {
		t.Fatalf("ManifestFileKey = %q, want %q", got, "episodic/ws/team/proj")
	}
}

func TestOperationsRejectInvalidKey(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	bad := ProjectKey{Workspace: "../etc", Team: "t", Project: "p"}

	tests := []struct {
		name string
		call func() error
	}{
		{"AppendEpisode", func() error { return store.AppendEpisode(ctx, bad, testRecord("01A", "x")) }},
		{"ListEpisodes", func() error { _, err := store.ListEpisodes(ctx, bad); return err }},
		{"GetEpisode", func() error { _, err := store.GetEpisode(ctx, bad, "01A"); return err }},
		{"RemoveEpisodes", func() error { return store.RemoveEpisodes(ctx, bad, []string{"01A"}) }},
		{"ReadKnowledge", func() error { _, err := store.ReadKnowledge(ctx, bad); return err }},
		{"WriteKnowledge", func() error { return store.WriteKnowledge(ctx, bad, knowledge.Graph{}) }},
		{"MarkDirty", func() error { return store.MarkDirty(ctx, bad, PlaneEpisodic) }},
		{"FileInfo", func() error { _, _, err := store.FileInfo(ctx, bad, PlaneEpisodic); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.call() == nil {
				t.Fatal("expected validation error for path-escaping key, got nil")
			}
		})
	}
}

func TestAppendListGetEpisodes(t *testing.T) {
	store, home := newTestStore(t)
	ctx := context.Background()
	key := testKey()

	// Missing file lists empty, not error.
	recs, err := store.ListEpisodes(ctx, key)
	if err != nil {
		t.Fatalf("ListEpisodes on missing file: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("expected empty list, got %d", len(recs))
	}

	ids := []string{"01AAAA", "01BBBB", "01CCCC"}
	for i, id := range ids {
		if err := store.AppendEpisode(ctx, key, testRecord(id, "text "+id)); err != nil {
			t.Fatalf("AppendEpisode %d: %v", i, err)
		}
	}

	recs, err = store.ListEpisodes(ctx, key)
	if err != nil {
		t.Fatalf("ListEpisodes: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("got %d records, want 3", len(recs))
	}
	for i, id := range ids {
		if recs[i].ID != id {
			t.Errorf("record %d id = %q, want %q (append order)", i, recs[i].ID, id)
		}
	}

	got, err := store.GetEpisode(ctx, key, "01BBBB")
	if err != nil {
		t.Fatalf("GetEpisode: %v", err)
	}
	if got.Text != "text 01BBBB" {
		t.Errorf("GetEpisode text = %q", got.Text)
	}

	if _, err := store.GetEpisode(ctx, key, "01ZZZZ"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetEpisode missing = %v, want ErrNotFound", err)
	}

	// Duplicate id is rejected.
	if err := store.AppendEpisode(ctx, key, testRecord("01AAAA", "dupe")); err == nil {
		t.Error("expected duplicate id append to fail")
	}

	// File lives at the documented layout and no temp litter remains.
	path := filepath.Join(home, "episodic", "ws", "team", "proj.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected episodic file at %s: %v", path, err)
	}
	assertNoTempFiles(t, filepath.Dir(path))
}

func TestUpdateEpisodes(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	key := testKey()
	for _, id := range []string{"01AAAA", "01BBBB"} {
		if err := store.AppendEpisode(ctx, key, testRecord(id, id)); err != nil {
			t.Fatal(err)
		}
	}

	err := store.UpdateEpisodes(ctx, key, []string{"01BBBB"}, func(r episodic.Record) episodic.Record {
		r.Consolidated = true
		return r
	})
	if err != nil {
		t.Fatalf("UpdateEpisodes: %v", err)
	}

	recs, _ := store.ListEpisodes(ctx, key)
	if recs[0].Consolidated {
		t.Error("untouched record was mutated")
	}
	if !recs[1].Consolidated {
		t.Error("targeted record was not updated")
	}

	// Missing id wraps ErrNotFound and does not write.
	err = store.UpdateEpisodes(ctx, key, []string{"01ZZZZ"}, func(r episodic.Record) episodic.Record { return r })
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("missing id = %v, want ErrNotFound", err)
	}

	// Changing an id is rejected.
	err = store.UpdateEpisodes(ctx, key, []string{"01AAAA"}, func(r episodic.Record) episodic.Record {
		r.ID = "01XXXX"
		return r
	})
	if err == nil {
		t.Error("expected id change to be rejected")
	}
	recs, _ = store.ListEpisodes(ctx, key)
	if recs[0].ID != "01AAAA" {
		t.Errorf("id mutated on disk to %q", recs[0].ID)
	}
}

func TestRemoveEpisodes(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	key := testKey()
	for _, id := range []string{"01AAAA", "01BBBB", "01CCCC"} {
		if err := store.AppendEpisode(ctx, key, testRecord(id, id)); err != nil {
			t.Fatal(err)
		}
	}

	// Absent ids are skipped (idempotent aging retry).
	if err := store.RemoveEpisodes(ctx, key, []string{"01BBBB", "01ZZZZ"}); err != nil {
		t.Fatalf("RemoveEpisodes: %v", err)
	}
	recs, _ := store.ListEpisodes(ctx, key)
	if len(recs) != 2 || recs[0].ID != "01AAAA" || recs[1].ID != "01CCCC" {
		t.Fatalf("unexpected survivors: %+v", recs)
	}

	// Retry with the same ids is a no-op.
	if err := store.RemoveEpisodes(ctx, key, []string{"01BBBB"}); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}

	// Removal from a project that never existed is a no-op too.
	other := ProjectKey{Workspace: "ws", Team: "team", Project: "ghost"}
	if err := store.RemoveEpisodes(ctx, other, []string{"01AAAA"}); err != nil {
		t.Fatalf("remove on missing file: %v", err)
	}

	// Manifest record count tracked the removal.
	m, err := store.Manifest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Files[ManifestFileKey(PlaneEpisodic, key)].RecordCount; got != 2 {
		t.Errorf("manifest record_count = %d, want 2", got)
	}
}

func TestKnowledgeReadWrite(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	key := testKey()

	// Missing file reads as empty graph.
	g, err := store.ReadKnowledge(ctx, key)
	if err != nil {
		t.Fatalf("ReadKnowledge missing: %v", err)
	}
	if len(g.Nodes) != 0 || len(g.Edges) != 0 {
		t.Fatalf("expected empty graph, got %+v", g)
	}

	want := knowledge.Graph{
		Nodes: []knowledge.Node{
			{ID: "01NODE1", Kind: knowledge.KindFact, Name: "n1", State: knowledge.StateActive, Trust: knowledge.TrustUserStated, Created: testNow, Updated: testNow},
			{ID: "01NODE2", Kind: knowledge.KindEntity, Name: "n2", State: knowledge.StateActive, Trust: knowledge.TrustAgentInferred, Created: testNow, Updated: testNow},
		},
		Edges: []knowledge.Edge{
			{From: "01NODE1", To: "01NODE2", Rel: knowledge.RelAbout, Confidence: 0.9},
		},
	}
	if err := store.WriteKnowledge(ctx, key, want); err != nil {
		t.Fatalf("WriteKnowledge: %v", err)
	}

	got, err := store.ReadKnowledge(ctx, key)
	if err != nil {
		t.Fatalf("ReadKnowledge: %v", err)
	}
	if len(got.Nodes) != 2 || len(got.Edges) != 1 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.Nodes[0].ID != "01NODE1" || got.Edges[0].Rel != knowledge.RelAbout {
		t.Errorf("round-trip content mismatch: %+v", got)
	}

	// Manifest counts NODES for the knowledge plane (Neo4j node-count parity).
	m, _ := store.Manifest(ctx)
	st := m.Files[ManifestFileKey(PlaneKnowledge, key)]
	if st.RecordCount != 2 {
		t.Errorf("knowledge record_count = %d, want 2 (nodes only)", st.RecordCount)
	}
	if st.SHA256 == "" {
		t.Error("knowledge sha256 not recorded")
	}
}

func TestManifestTracksShaAndCount(t *testing.T) {
	store, home := newTestStore(t)
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
	data, err := os.ReadFile(filepath.Join(home, "episodic", "ws", "team", "proj.json"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if st.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("manifest sha %q != file sha %q", st.SHA256, hex.EncodeToString(sum[:]))
	}
	if !m.UpdatedAt.Equal(testNow) {
		t.Errorf("UpdatedAt = %v, want clock time %v", m.UpdatedAt, testNow)
	}
}

func TestUpdateManifest(t *testing.T) {
	store, _ := newTestStore(t)
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
		m.Indexes["opensearch"] = IndexState{LastHydratedSHA: "abc"}
		return m, nil
	})
	if err != nil {
		t.Fatalf("UpdateManifest: %v", err)
	}
	m, _ = store.Manifest(ctx)
	if m.Indexes["opensearch"].LastHydratedSHA != "abc" {
		t.Errorf("index state not persisted: %+v", m.Indexes)
	}

	// fn error aborts without writing.
	boom := errors.New("boom")
	err = store.UpdateManifest(ctx, func(m Manifest) (Manifest, error) {
		m.Indexes["opensearch"] = IndexState{LastHydratedSHA: "should-not-persist"}
		return m, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("expected fn error surfaced, got %v", err)
	}
	m, _ = store.Manifest(ctx)
	if m.Indexes["opensearch"].LastHydratedSHA != "abc" {
		t.Errorf("aborted update leaked to disk: %+v", m.Indexes)
	}

	// Mutating the returned manifest must not affect the store (copy semantics).
	m.Indexes["opensearch"] = IndexState{LastHydratedSHA: "tampered"}
	m2, _ := store.Manifest(ctx)
	if m2.Indexes["opensearch"].LastHydratedSHA != "abc" {
		t.Error("Manifest() returned a shared map")
	}
}

func TestMarkDirtyAndWritePreservesFlags(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	key := testKey()
	fk := ManifestFileKey(PlaneEpisodic, key)

	if err := store.AppendEpisode(ctx, key, testRecord("01AAAA", "x")); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDirty(ctx, key, PlaneEpisodic); err != nil {
		t.Fatalf("MarkDirty: %v", err)
	}
	m, _ := store.Manifest(ctx)
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
	m, _ = store.Manifest(ctx)
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
	m, _ = store.Manifest(ctx)
	if !m.Files[ManifestFileKey(PlaneKnowledge, key)].Dirty {
		t.Error("dirty mark lost for entry-less plane")
	}
}

func TestListProjects(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	// Empty store: no projects, no error.
	keys, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects empty: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("expected none, got %v", keys)
	}

	a := ProjectKey{Workspace: "ws", Team: "team", Project: "alpha"}
	b := ProjectKey{Workspace: "ws", Team: "team", Project: "beta"}
	if err := store.AppendEpisode(ctx, a, testRecord("01AAAA", "x")); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteKnowledge(ctx, b, knowledge.Graph{}); err != nil {
		t.Fatal(err)
	}
	// Project in BOTH planes must appear once.
	if err := store.WriteKnowledge(ctx, a, knowledge.Graph{}); err != nil {
		t.Fatal(err)
	}

	keys, err = store.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("got %d projects, want 2: %v", len(keys), keys)
	}
	if keys[0] != a || keys[1] != b {
		t.Errorf("unexpected order/content: %v", keys)
	}
}

func TestFileInfo(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	key := testKey()

	if _, _, err := store.FileInfo(ctx, key, PlaneEpisodic); !errors.Is(err, ErrNotFound) {
		t.Errorf("FileInfo missing = %v, want ErrNotFound", err)
	}

	if err := store.AppendEpisode(ctx, key, testRecord("01AAAA", "x")); err != nil {
		t.Fatal(err)
	}
	size, mtime, err := store.FileInfo(ctx, key, PlaneEpisodic)
	if err != nil {
		t.Fatalf("FileInfo: %v", err)
	}
	if size <= 0 {
		t.Errorf("size = %d, want > 0", size)
	}
	if mtime.IsZero() {
		t.Error("mtime is zero")
	}
}

func TestCorruptFileSurfacesError(t *testing.T) {
	store, home := newTestStore(t)
	ctx := context.Background()
	key := testKey()

	path := filepath.Join(home, "episodic", "ws", "team", "proj.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListEpisodes(ctx, key); err == nil {
		t.Error("expected parse error for corrupt episodic file")
	}
	if err := store.AppendEpisode(ctx, key, testRecord("01AAAA", "x")); err == nil {
		t.Error("append over corrupt file must fail, not clobber")
	}
}

func TestNoTempLitterAcrossOperations(t *testing.T) {
	store, home := newTestStore(t)
	ctx := context.Background()
	key := testKey()

	if err := store.AppendEpisode(ctx, key, testRecord("01AAAA", "x")); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteKnowledge(ctx, key, knowledge.Graph{}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDirty(ctx, key, PlaneEpisodic); err != nil {
		t.Fatal(err)
	}
	assertNoTempFiles(t, home)
}

// assertNoTempFiles walks dir and fails on any leftover .tmp-* file.
func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasPrefix(d.Name(), ".tmp-") {
			t.Errorf("temp file litter: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
