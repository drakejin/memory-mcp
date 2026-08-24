package hotstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/drakejin/memory-mcp/internal/errs"
	"github.com/drakejin/memory-mcp/internal/knowledge"
)

// writeForeignPlaneFile drops an episodic plane file whose path segments could
// not have come from a valid ProjectKey.
func writeForeignPlaneFile(t *testing.T, home string, key ProjectKey) {
	t.Helper()
	path := filepath.Join(home, string(PlaneEpisodic), key.Workspace, key.Team, key.Project+jsonExt)
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("[]\n"), filePerm); err != nil {
		t.Fatal(err)
	}
}

func TestKnowledgeReadWrite(t *testing.T) {
	store, _ := newTestClient(t)
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
	if err := setKnowledge(ctx, store, key, want); err != nil {
		t.Fatalf("UpdateKnowledge: %v", err)
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
	m, err := store.Manifest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st := m.Files[ManifestFileKey(PlaneKnowledge, key)]
	if st.RecordCount != 2 {
		t.Errorf("knowledge record_count = %d, want 2 (nodes only)", st.RecordCount)
	}
	if st.SHA256 == "" {
		t.Error("knowledge sha256 not recorded")
	}
}

func TestListProjects(t *testing.T) {
	store, _ := newTestClient(t)
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
	if err := setKnowledge(ctx, store, b, knowledge.Graph{}); err != nil {
		t.Fatal(err)
	}
	// Project in BOTH planes must appear once.
	if err := setKnowledge(ctx, store, a, knowledge.Graph{}); err != nil {
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

func TestListProjectsSkipsForeignPaths(t *testing.T) {
	store, home := newTestClient(t)
	ctx := context.Background()

	// A file we could not have written (uppercase segment) is ignored rather
	// than surfaced as a project.
	foreign := ProjectKey{Workspace: "ws", Team: "TEAM", Project: "proj"}
	writeForeignPlaneFile(t, home, foreign)

	keys, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("expected foreign path skipped, got %v", keys)
	}
}

func TestFileInfo(t *testing.T) {
	store, _ := newTestClient(t)
	ctx := context.Background()
	key := testKey()

	_, _, err := store.FileInfo(ctx, key, PlaneEpisodic)
	if !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("FileInfo missing = %v, want ErrNotFound", err)
	}
	var domain *errs.Error
	if errors.As(err, &domain) && domain.ID != ManifestFileKey(PlaneEpisodic, key) {
		t.Errorf("ID = %q, want the manifest file key", domain.ID)
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

// setKnowledge replaces a project's graph outright. UpdateKnowledge is the only
// mutation path, so tests that just need a starting graph express "ignore the
// current one and store this" through it rather than through a second, racy
// write method on the Client.
func setKnowledge(ctx context.Context, store Client, key ProjectKey, g knowledge.Graph) error {
	return store.UpdateKnowledge(ctx, key, func(knowledge.Graph) (knowledge.Graph, error) {
		return g, nil
	})
}
