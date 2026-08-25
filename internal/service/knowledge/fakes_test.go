package knowledge

// Port fakes for the service tests (code-standards §1.1: fakes implement the
// same consumer-side interfaces the production adapters satisfy). A shared
// callLog records the cross-port sequence so tests can prove the write order
// the architecture demands — hot commits before any mirror call (§0
// principle 1, P1).

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// testKey scopes every service test to one project (P10 isolation is the
// stores' concern; the service just threads the key through).
var testKey = projectkey.Key{Workspace: "acme", Team: "core", Project: "memory"}

// errBoom is the injected failure of the fakes.
var errBoom = errors.New("boom")

// Call names recorded by the fakes.
const (
	callUpdateKnowledge = "hot.UpdateKnowledge"
	callMarkDirty       = "hot.MarkDirty"
	callMarkIndexed     = "hot.MarkIndexed"
	callUpsertNodes     = "graph.UpsertNodes"
	callUpsertEdges     = "graph.UpsertEdges"
	callDeleteNode      = "graph.DeleteNode"
)

type callLog struct{ calls []string }

func (l *callLog) add(name string) { l.calls = append(l.calls, name) }

// assertCalls pins the exact cross-port sequence of one operation.
func assertCalls(t *testing.T, log *callLog, want ...string) {
	t.Helper()
	if len(log.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", log.calls, want)
	}
	for i := range want {
		if log.calls[i] != want[i] {
			t.Fatalf("calls = %v, want %v", log.calls, want)
		}
	}
}

// fakeHot is the HotStore port: it applies UpdateKnowledge closures against an
// in-memory graph and counts manifest bookkeeping calls.
type fakeHot struct {
	log   *callLog
	graph Graph

	updateErr      error // returned before fn runs (a store-level failure)
	markDirtyErr   error
	markIndexedErr error

	dirtyCalls   int
	indexedCalls int
}

func (f *fakeHot) UpdateKnowledge(_ context.Context, _ projectkey.Key, fn func(Graph) (Graph, error)) error {
	f.log.add(callUpdateKnowledge)
	if f.updateErr != nil {
		return f.updateErr
	}
	next, err := fn(f.graph)
	if err != nil {
		return err
	}
	f.graph = next
	return nil
}

func (f *fakeHot) MarkDirty(context.Context, projectkey.Key) error {
	f.log.add(callMarkDirty)
	f.dirtyCalls++
	return f.markDirtyErr
}

func (f *fakeHot) MarkIndexed(context.Context, projectkey.Key) error {
	f.log.add(callMarkIndexed)
	f.indexedCalls++
	return f.markIndexedErr
}

// fakeGraph is the GraphIndex port: it records what the mirror sends and can
// fail any single method.
type fakeGraph struct {
	log *callLog

	upsertNodesErr  error
	upsertEdgesErr  error
	deleteErr       error
	searchErr       error
	neighborhoodErr error

	gotNodes   [][]Node
	gotEdges   [][]Edge
	deletedIDs []string

	searchQ        string
	searchArchived bool
	searchResult   []Node

	neighborhoodEntity string
	neighborhoodDepth  int
	neighborhoodResult Graph
}

func (f *fakeGraph) UpsertNodes(_ context.Context, _ projectkey.Key, nodes []Node) error {
	f.log.add(callUpsertNodes)
	if f.upsertNodesErr != nil {
		return f.upsertNodesErr
	}
	f.gotNodes = append(f.gotNodes, nodes)
	return nil
}

func (f *fakeGraph) UpsertEdges(_ context.Context, _ projectkey.Key, edges []Edge) error {
	f.log.add(callUpsertEdges)
	if f.upsertEdgesErr != nil {
		return f.upsertEdgesErr
	}
	f.gotEdges = append(f.gotEdges, edges)
	return nil
}

func (f *fakeGraph) DeleteNode(_ context.Context, _ projectkey.Key, id string) error {
	f.log.add(callDeleteNode)
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deletedIDs = append(f.deletedIDs, id)
	return nil
}

func (f *fakeGraph) Search(_ context.Context, _ projectkey.Key, q string, includeArchived bool) ([]Node, error) {
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	f.searchQ = q
	f.searchArchived = includeArchived
	return f.searchResult, nil
}

func (f *fakeGraph) Neighborhood(_ context.Context, _ projectkey.Key, entity string, depth int) (Graph, error) {
	if f.neighborhoodErr != nil {
		return Graph{}, f.neighborhoodErr
	}
	f.neighborhoodEntity = entity
	f.neighborhoodDepth = depth
	return f.neighborhoodResult, nil
}

// fixedClock returns one instant, so timestamps and minted id times are
// asserted exactly.
type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

// fakeIDs mints fixture ULIDs and records the millis it was asked for, so a
// test can prove the id's time half agrees with the injected clock.
type fakeIDs struct {
	err    error
	millis []int64
	minted []string
}

func (f *fakeIDs) GenerateAt(unixMillis int64) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.millis = append(f.millis, unixMillis)
	id := newID()
	f.minted = append(f.minted, id)
	return id, nil
}

// rig bundles a Service with its fakes.
type rig struct {
	svc   Service
	hot   *fakeHot
	graph *fakeGraph
	ids   *fakeIDs
	log   *callLog
}

// newRig wires a service over fresh fakes seeded with graph g. withGraph=false
// leaves Config.Graph nil to exercise the degraded paths of a missing mirror.
func newRig(t *testing.T, seed Graph, withGraph bool) *rig {
	t.Helper()
	log := &callLog{}
	hot := &fakeHot{log: log, graph: seed}
	ids := &fakeIDs{}
	cfg := Config{
		Store:  hot,
		IDs:    ids,
		Clock:  fixedClock{now: testNow},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	r := &rig{hot: hot, ids: ids, log: log}
	if withGraph {
		r.graph = &fakeGraph{log: log}
		cfg.Graph = r.graph
	}
	svc, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.svc = svc
	return r
}
