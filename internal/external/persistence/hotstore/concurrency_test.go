package hotstore

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/drakejin/memory-mcp/internal/service/knowledge"
)

// concurrentWriters is high enough to interleave reliably and low enough to
// keep the suite fast.
const concurrentWriters = 20

// runConcurrently invokes fn(i) for i in [0,n) behind a shared start barrier so
// the calls genuinely overlap, then waits for all of them.
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

// TestConcurrentUpdateKnowledgeKeepsEveryNode is the store-level guarantee the
// knowledge handlers rest on: the graph document is replaced wholesale, so
// UpdateKnowledge must serialise read, fn and write. If the lock were released
// between the read and the write, writers would overwrite each other and the
// canonical file would end up with fewer nodes than were accepted.
func TestConcurrentUpdateKnowledgeKeepsEveryNode(t *testing.T) {
	// Arrange
	store, _ := newTestClient(t)
	ctx := context.Background()
	key := testKey()

	// Act
	errsOut := make([]error, concurrentWriters)
	runConcurrently(concurrentWriters, func(i int) {
		errsOut[i] = store.UpdateKnowledge(ctx, key, func(g knowledge.Graph) (knowledge.Graph, error) {
			n := knowledge.Node{
				ID: fmt.Sprintf("01NODE%014d", i), Kind: knowledge.KindFact,
				Name: fmt.Sprintf("n%02d", i), State: knowledge.StateActive,
				Trust: knowledge.TrustUserStated, Created: testNow, Updated: testNow,
			}
			return knowledge.Graph{Nodes: append(append([]knowledge.Node{}, g.Nodes...), n), Edges: g.Edges}, nil
		})
	})

	// Assert
	for i, err := range errsOut {
		if err != nil {
			t.Fatalf("UpdateKnowledge %d: %v", i, err)
		}
	}
	got, err := store.ReadKnowledge(ctx, key)
	if err != nil {
		t.Fatalf("ReadKnowledge: %v", err)
	}
	if len(got.Nodes) != concurrentWriters {
		t.Fatalf("graph holds %d nodes, want %d — a concurrent update was lost", len(got.Nodes), concurrentWriters)
	}

	// The manifest must agree with the file it describes, or rehydrate reads
	// drift where there is none (§5).
	m, err := store.Manifest(ctx)
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if st := m.Files[ManifestFileKey(PlaneKnowledge, key)]; st.RecordCount != concurrentWriters {
		t.Errorf("manifest record_count = %d, want %d", st.RecordCount, concurrentWriters)
	}
}

// TestConcurrentAppendEpisodeKeepsEveryRecord pins the same guarantee on the
// episodic plane, whose file is rewritten in full on every append.
func TestConcurrentAppendEpisodeKeepsEveryRecord(t *testing.T) {
	// Arrange
	store, _ := newTestClient(t)
	ctx := context.Background()
	key := testKey()

	// Act
	errsOut := make([]error, concurrentWriters)
	runConcurrently(concurrentWriters, func(i int) {
		errsOut[i] = store.AppendEpisode(ctx, key, testRecord(fmt.Sprintf("01REC%015d", i), fmt.Sprintf("t%02d", i)))
	})

	// Assert
	for i, err := range errsOut {
		if err != nil {
			t.Fatalf("AppendEpisode %d: %v", i, err)
		}
	}
	recs, err := store.ListEpisodes(ctx, key)
	if err != nil {
		t.Fatalf("ListEpisodes: %v", err)
	}
	if len(recs) != concurrentWriters {
		t.Fatalf("hot file holds %d records, want %d — a concurrent append was lost", len(recs), concurrentWriters)
	}
	seen := make(map[string]bool, len(recs))
	for _, r := range recs {
		seen[r.ID] = true
	}
	for i := range concurrentWriters {
		if id := fmt.Sprintf("01REC%015d", i); !seen[id] {
			t.Errorf("record %s missing from the hot file", id)
		}
	}
}

// TestConcurrentUpdateKnowledgeAbortDoesNotWrite proves the abort path is also
// serialised: a failing closure must leave the graph exactly as the successful
// writers left it, never a half-applied one.
func TestConcurrentUpdateKnowledgeAbortDoesNotWrite(t *testing.T) {
	// Arrange
	store, _ := newTestClient(t)
	ctx := context.Background()
	key := testKey()
	boom := fmt.Errorf("closure refused")

	// Act: odd writers abort, even writers append.
	runConcurrently(concurrentWriters, func(i int) {
		err := store.UpdateKnowledge(ctx, key, func(g knowledge.Graph) (knowledge.Graph, error) {
			if i%2 == 1 {
				return knowledge.Graph{}, boom
			}
			n := knowledge.Node{
				ID: fmt.Sprintf("01NODE%014d", i), Kind: knowledge.KindFact,
				Name: fmt.Sprintf("n%02d", i), State: knowledge.StateActive,
				Trust: knowledge.TrustUserStated, Created: testNow, Updated: testNow,
			}
			return knowledge.Graph{Nodes: append(append([]knowledge.Node{}, g.Nodes...), n), Edges: g.Edges}, nil
		})
		if i%2 == 1 && err == nil {
			t.Errorf("writer %d: aborting closure returned nil error", i)
		}
		if i%2 == 0 && err != nil {
			t.Errorf("writer %d: %v", i, err)
		}
	})

	// Assert
	got, err := store.ReadKnowledge(ctx, key)
	if err != nil {
		t.Fatalf("ReadKnowledge: %v", err)
	}
	if want := concurrentWriters / 2; len(got.Nodes) != want {
		t.Fatalf("graph holds %d nodes, want %d (aborted closures must write nothing)", len(got.Nodes), want)
	}
}
