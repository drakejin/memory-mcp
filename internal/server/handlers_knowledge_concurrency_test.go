package server

import (
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/drakejin/memory-mcp/internal/knowledge"
)

// The knowledge document is stored as one JSON file per project and replaced
// wholesale on every write. A handler that reads the graph, computes the next
// one and writes it back in three separate store calls therefore loses data
// under concurrency: two writers both read graph G, both write G+1, and one
// node is gone from the canonical store while mirrorKnowledge has already
// MERGEd BOTH into Neo4j. That is content living only in a derived store,
// which architecture-v2.md §0 principle 1 forbids, and the next rehydration
// deletes the survivor from the graph to match hot.
//
// These tests pin the handlers to hotstore.UpdateKnowledge, which holds the
// store lock across the whole read-modify-write. They fail against the
// read-then-write shape.

// concurrentWriters is high enough to interleave reliably on a multi-core
// machine and low enough to keep the suite fast.
const concurrentWriters = 20

// runConcurrently invokes fn(i) for i in [0,n) and waits for all of them. Every
// goroutine blocks on the same barrier before starting, so the requests
// actually overlap instead of trickling out while the previous one finishes —
// without it the read-modify-write window is too narrow to hit reliably.
func runConcurrently(n int, fn func(i int)) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			<-start
			fn(i)
		}()
	}
	close(start)
	wg.Wait()
}

// storedGraph reads the canonical graph back out of the fake store.
func storedGraph(t *testing.T, store *fakeStore) knowledge.Graph {
	t.Helper()
	g, err := store.ReadKnowledge(t.Context(), testKey)
	if err != nil {
		t.Fatalf("ReadKnowledge: %v", err)
	}
	return g
}

func TestConcurrentCreateNodeKeepsEveryNode(t *testing.T) {
	// Arrange
	store := newFakeStore()
	_, h := newTestServer(t, func(c *Config) { c.Store = store })

	// Act: every goroutine creates a distinct node in the same project.
	statuses := make([]int, concurrentWriters)
	runConcurrently(concurrentWriters, func(i int) {
		rec := do(t, h, http.MethodPost, nodesPath, CreateNodeRequest{
			Kind:  knowledge.KindFact,
			Name:  fmt.Sprintf("fact-%02d", i),
			Trust: knowledge.TrustUserStated,
		})
		statuses[i] = rec.Code
	})

	// Assert
	for i, status := range statuses {
		if status != http.StatusCreated {
			t.Fatalf("create node %d: got HTTP %d, want 201", i, status)
		}
	}
	g := storedGraph(t, store)
	if len(g.Nodes) != concurrentWriters {
		t.Fatalf("hot graph holds %d nodes, want %d — a concurrent write was lost from the canonical store",
			len(g.Nodes), concurrentWriters)
	}
	names := make(map[string]bool, len(g.Nodes))
	for _, n := range g.Nodes {
		names[n.Name] = true
	}
	for i := range concurrentWriters {
		if want := fmt.Sprintf("fact-%02d", i); !names[want] {
			t.Errorf("node %q is missing from the hot graph", want)
		}
	}
}

func TestConcurrentCreateEdgeKeepsEveryEdge(t *testing.T) {
	// Arrange: one hub node plus a spoke per writer, so every edge is distinct
	// and both endpoints already exist.
	store := newFakeStore()
	seed := knowledge.Graph{Nodes: []knowledge.Node{seedNode(ulidA, "hub")}}
	spokes := make([]string, concurrentWriters)
	for i := range concurrentWriters {
		spokes[i] = fmt.Sprintf("01JD%022d", i+1)
		seed.Nodes = append(seed.Nodes, seedNode(spokes[i], fmt.Sprintf("spoke-%02d", i)))
	}
	if err := store.UpdateKnowledge(t.Context(), testKey, func(knowledge.Graph) (knowledge.Graph, error) {
		return seed, nil
	}); err != nil {
		t.Fatalf("seed graph: %v", err)
	}
	_, h := newTestServer(t, func(c *Config) { c.Store = store })

	// Act
	statuses := make([]int, concurrentWriters)
	runConcurrently(concurrentWriters, func(i int) {
		rec := do(t, h, http.MethodPost, edgesPath, knowledge.Edge{
			From: ulidA, To: spokes[i], Rel: knowledge.RelRelatesTo, Confidence: 1,
		})
		statuses[i] = rec.Code
	})

	// Assert
	for i, status := range statuses {
		if status != http.StatusCreated {
			t.Fatalf("create edge %d: got HTTP %d, want 201", i, status)
		}
	}
	g := storedGraph(t, store)
	if len(g.Edges) != concurrentWriters {
		t.Fatalf("hot graph holds %d edges, want %d — a concurrent edge write was lost",
			len(g.Edges), concurrentWriters)
	}
	if len(g.Nodes) != concurrentWriters+1 {
		t.Errorf("edge writes changed the node set: got %d nodes, want %d", len(g.Nodes), concurrentWriters+1)
	}
}

func TestConcurrentPatchAndCreateKeepBothEffects(t *testing.T) {
	// Arrange: a patch archiving a seeded node races node creations. The patch
	// must not resurrect the graph it read, and the creations must not undo the
	// state transition.
	store := newFakeStore()
	if err := store.UpdateKnowledge(t.Context(), testKey, func(knowledge.Graph) (knowledge.Graph, error) {
		return knowledge.Graph{Nodes: []knowledge.Node{seedNode(ulidA, "seeded")}}, nil
	}); err != nil {
		t.Fatalf("seed graph: %v", err)
	}
	_, h := newTestServer(t, func(c *Config) { c.Store = store })

	// Act: writer 0 patches, the rest create.
	statuses := make([]int, concurrentWriters)
	runConcurrently(concurrentWriters, func(i int) {
		if i == 0 {
			rec := do(t, h, http.MethodPatch, nodesPath+"/"+ulidA, PatchNodeRequest{
				Op: opSetState, State: knowledge.StateArchived,
			})
			statuses[i] = rec.Code
			return
		}
		rec := do(t, h, http.MethodPost, nodesPath, CreateNodeRequest{
			Kind:  knowledge.KindFact,
			Name:  fmt.Sprintf("fact-%02d", i),
			Trust: knowledge.TrustUserStated,
		})
		statuses[i] = rec.Code
	})

	// Assert
	if statuses[0] != http.StatusOK {
		t.Fatalf("patch: got HTTP %d, want 200", statuses[0])
	}
	for i, status := range statuses[1:] {
		if status != http.StatusCreated {
			t.Fatalf("create node %d: got HTTP %d, want 201", i+1, status)
		}
	}
	g := storedGraph(t, store)
	want := concurrentWriters // 1 seeded + (concurrentWriters-1) created
	if len(g.Nodes) != want {
		t.Fatalf("hot graph holds %d nodes, want %d — a write was lost to the patch/create interleave", len(g.Nodes), want)
	}
	patched, err := knowledge.FindNode(g, ulidA)
	if err != nil {
		t.Fatalf("seeded node vanished: %v", err)
	}
	if patched.State != knowledge.StateArchived {
		t.Errorf("seeded node state = %q, want archived — a concurrent create overwrote the transition", patched.State)
	}
}
