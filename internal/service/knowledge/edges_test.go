package knowledge

// Service-level edge orchestration: the domain gate (including the P5
// self-supersede hole this service closes), endpoint existence against hot,
// idempotent re-posts, and the best-effort mirror.

import (
	"context"
	"errors"
	"testing"

	"github.com/drakejin/memory-mcp/internal/x/errs"
)

// TestServiceCreateEdgeClosesSelfSupersedeGap pins the P5 fix: the transport
// validation historically let a rel=supersedes edge with from==to through
// (docs/spec/05), turning the revision chain into a self-loop in Neo4j.
// Service.CreateEdge refuses it as KindInvalid before any store access.
func TestServiceCreateEdgeClosesSelfSupersedeGap(t *testing.T) {
	// Arrange: the node exists, so only the self-supersede rule can refuse.
	ctx := context.Background()
	n := validNode(t)
	r := newRig(t, Graph{Nodes: []Node{n}}, true)

	// Act
	_, err := r.svc.CreateEdge(ctx, testKey, Edge{From: n.ID, To: n.ID, Rel: RelSupersedes, Confidence: 1})

	// Assert
	domain := assertDomainErr(t, err, errs.ErrInvalid, opValidateEdge, EntityEdge)
	if domain.Msg != "node cannot supersede itself" {
		t.Errorf("msg = %q, want the v1 rule wording", domain.Msg)
	}
	assertCalls(t, r.log)
	if len(r.hot.graph.Edges) != 0 {
		t.Errorf("self-supersede edge reached hot: %+v", r.hot.graph.Edges)
	}
}

func TestServiceCreateEdgeValidation(t *testing.T) {
	ctx := context.Background()
	valid := Edge{From: newID(), To: newID(), Rel: RelRelatesTo, Confidence: 0.9}

	tests := []struct {
		name   string
		mutate func(e Edge) Edge
	}{
		{"malformed from", func(e Edge) Edge { e.From = "x"; return e }},
		{"malformed to", func(e Edge) Edge { e.To = "x"; return e }},
		{"unknown rel", func(e Edge) Edge { e.Rel = "blames"; return e }},
		{"confidence below zero", func(e Edge) Edge { e.Confidence = -0.1; return e }},
		{"confidence above one", func(e Edge) Edge { e.Confidence = 1.1; return e }},
		{"malformed provenance id", func(e Edge) Edge { e.Provenance = []string{"zzz"}; return e }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			r := newRig(t, Graph{}, true)

			// Act
			_, err := r.svc.CreateEdge(ctx, testKey, tt.mutate(valid))

			// Assert: invalid input, and no store traffic at all.
			if !errors.Is(err, errs.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
			assertCalls(t, r.log)
		})
	}
}

func TestServiceCreateEdgeEndpointsMustExist(t *testing.T) {
	ctx := context.Background()
	a := validNode(t)

	tests := []struct {
		name string
		edge func() Edge
	}{
		{"missing to", func() Edge { return Edge{From: a.ID, To: newID(), Rel: RelRelatesTo, Confidence: 1} }},
		{"missing from", func() Edge { return Edge{From: newID(), To: a.ID, Rel: RelRelatesTo, Confidence: 1} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			r := newRig(t, Graph{Nodes: []Node{a}}, true)

			// Act
			_, err := r.svc.CreateEdge(ctx, testKey, tt.edge())

			// Assert: not found with the exact transport wording; the closure
			// refused, so nothing was mirrored.
			domain := assertDomainErr(t, err, errs.ErrNotFound, opCreateEdge, EntityNode)
			if domain.Msg != msgEdgeEndpointNotFound {
				t.Errorf("msg = %q, want %q", domain.Msg, msgEdgeEndpointNotFound)
			}
			if len(r.hot.graph.Edges) != 0 {
				t.Errorf("refused edge reached hot: %+v", r.hot.graph.Edges)
			}
			assertCalls(t, r.log, callUpdateKnowledge)
		})
	}
}

func TestServiceCreateEdgeWritesHotThenMirror(t *testing.T) {
	// Arrange
	ctx := context.Background()
	a, b := validNode(t), validNode(t)
	r := newRig(t, Graph{Nodes: []Node{a, b}}, true)
	in := Edge{From: a.ID, To: b.ID, Rel: RelDerivedFrom, Confidence: 0.7}

	// Act
	res, err := r.svc.CreateEdge(ctx, testKey, in)

	// Assert
	if err != nil {
		t.Fatalf("CreateEdge: %v", err)
	}
	if len(res.Degraded) != 0 {
		t.Fatalf("degraded = %v, want none", res.Degraded)
	}
	// nil provenance normalizes to empty non-nil, so hot never stores null.
	if res.Edge.Provenance == nil || len(res.Edge.Provenance) != 0 {
		t.Errorf("provenance = %#v, want empty non-nil", res.Edge.Provenance)
	}
	if len(r.hot.graph.Edges) != 1 || r.hot.graph.Edges[0].Rel != RelDerivedFrom {
		t.Fatalf("hot edges = %+v, want the derived_from edge", r.hot.graph.Edges)
	}
	if len(r.graph.gotEdges) != 1 || len(r.graph.gotEdges[0]) != 1 {
		t.Fatalf("mirrored edges = %+v, want exactly one", r.graph.gotEdges)
	}
	assertCalls(t, r.log, callUpdateKnowledge, callUpsertEdges, callMarkIndexed)
}

// Re-posting the same (from,to,rel) replaces the edge instead of duplicating
// it (P3 idempotency).
func TestServiceCreateEdgeIdempotentRepost(t *testing.T) {
	// Arrange
	ctx := context.Background()
	a, b := validNode(t), validNode(t)
	r := newRig(t, Graph{Nodes: []Node{a, b}}, true)

	// Act
	if _, err := r.svc.CreateEdge(ctx, testKey, Edge{From: a.ID, To: b.ID, Rel: RelAbout, Confidence: 0.4}); err != nil {
		t.Fatalf("first CreateEdge: %v", err)
	}
	res, err := r.svc.CreateEdge(ctx, testKey, Edge{From: a.ID, To: b.ID, Rel: RelAbout, Confidence: 0.9})

	// Assert
	if err != nil {
		t.Fatalf("second CreateEdge: %v", err)
	}
	if len(r.hot.graph.Edges) != 1 {
		t.Fatalf("hot edges = %+v, want one edge after re-post", r.hot.graph.Edges)
	}
	if r.hot.graph.Edges[0].Confidence != 0.9 || res.Edge.Confidence != 0.9 {
		t.Errorf("confidence = %v/%v, want the re-posted 0.9", r.hot.graph.Edges[0].Confidence, res.Edge.Confidence)
	}
}

func TestServiceCreateEdgeDegraded(t *testing.T) {
	// Arrange
	ctx := context.Background()
	a, b := validNode(t), validNode(t)
	r := newRig(t, Graph{Nodes: []Node{a, b}}, true)
	r.graph.upsertEdgesErr = errBoom

	// Act
	res, err := r.svc.CreateEdge(ctx, testKey, Edge{From: a.ID, To: b.ID, Rel: RelRelatesTo, Confidence: 1})

	// Assert: hot keeps the edge, the mirror failure is a degraded note.
	if err != nil {
		t.Fatalf("CreateEdge must succeed hot-side: %v", err)
	}
	if len(res.Degraded) != 1 || res.Degraded[0] != degradedGraph {
		t.Fatalf("degraded = %v, want [%q]", res.Degraded, degradedGraph)
	}
	if len(r.hot.graph.Edges) != 1 {
		t.Errorf("hot edges = %+v, want the edge despite the dead mirror", r.hot.graph.Edges)
	}
	if r.hot.dirtyCalls != 1 {
		t.Errorf("dirty calls = %d, want 1", r.hot.dirtyCalls)
	}
}
