package knowledge

// Service-level read orchestration: fulltext search and neighborhood
// traversal, including the §5 read rule — a dead graph index is unavailable,
// never silently empty.

import (
	"context"
	"errors"
	"testing"

	"github.com/drakejin/memory-mcp/internal/x/errs"
)

func TestServiceSearch(t *testing.T) {
	ctx := context.Background()

	t.Run("empty query is invalid", func(t *testing.T) {
		r := newRig(t, Graph{}, true)
		_, err := r.svc.Search(ctx, testKey, "", false)
		domain := assertDomainErr(t, err, errs.ErrInvalid, opSearch, EntityNode)
		if domain.Msg != "q is required" {
			t.Errorf("msg = %q, want the transport wording", domain.Msg)
		}
	})

	t.Run("missing graph index is unavailable with the degraded wording", func(t *testing.T) {
		r := newRig(t, Graph{}, false)
		_, err := r.svc.Search(ctx, testKey, "nori", false)
		if !errors.Is(err, errs.ErrUnavailable) {
			t.Fatalf("err = %v, want ErrUnavailable", err)
		}
		var domain *errs.Error
		if !errors.As(err, &domain) {
			t.Fatalf("error is not *errs.Error: %#v", err)
		}
		if domain.Msg != degradedGraph {
			t.Errorf("msg = %q, want %q — one degraded vocabulary (§5)", domain.Msg, degradedGraph)
		}
	})

	t.Run("index errors pass through untouched", func(t *testing.T) {
		r := newRig(t, Graph{}, true)
		injected := errs.Unavailable("graph.Search", errBoom)
		r.graph.searchErr = injected
		_, err := r.svc.Search(ctx, testKey, "nori", false)
		if !errors.Is(err, errs.ErrUnavailable) || !errors.Is(err, errBoom) {
			t.Fatalf("err = %v, want the injected unavailable chain", err)
		}
	})

	t.Run("nil result becomes empty non-nil", func(t *testing.T) {
		r := newRig(t, Graph{}, true)
		nodes, err := r.svc.Search(ctx, testKey, "nori", false)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if nodes == nil || len(nodes) != 0 {
			t.Fatalf("nodes = %#v, want empty non-nil ([] not null in the envelope)", nodes)
		}
	})

	t.Run("forwards query and include_archived", func(t *testing.T) {
		r := newRig(t, Graph{}, true)
		hit := validNode(t)
		r.graph.searchResult = []Node{hit}

		nodes, err := r.svc.Search(ctx, testKey, "보안을 끄는", true)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if r.graph.searchQ != "보안을 끄는" || !r.graph.searchArchived {
			t.Errorf("forwarded = (%q, %v), want the caller's query and opt-in", r.graph.searchQ, r.graph.searchArchived)
		}
		if len(nodes) != 1 || nodes[0].ID != hit.ID {
			t.Errorf("nodes = %+v, want the index hit unmodified", nodes)
		}
	})
}

func TestServiceNeighborhood(t *testing.T) {
	ctx := context.Background()

	t.Run("empty entity is invalid", func(t *testing.T) {
		r := newRig(t, Graph{}, true)
		_, err := r.svc.Neighborhood(ctx, testKey, "", 1)
		domain := assertDomainErr(t, err, errs.ErrInvalid, opNeighborhood, EntityNode)
		if domain.Msg != "entity is required" {
			t.Errorf("msg = %q, want the transport wording", domain.Msg)
		}
	})

	t.Run("depth zero selects the default", func(t *testing.T) {
		r := newRig(t, Graph{}, true)
		if _, err := r.svc.Neighborhood(ctx, testKey, "nori", 0); err != nil {
			t.Fatalf("Neighborhood: %v", err)
		}
		if r.graph.neighborhoodDepth != DefaultDepth {
			t.Errorf("depth = %d, want default %d", r.graph.neighborhoodDepth, DefaultDepth)
		}
	})

	t.Run("depth bounds", func(t *testing.T) {
		for _, depth := range []int{-1, MaxDepth + 1} {
			r := newRig(t, Graph{}, true)
			_, err := r.svc.Neighborhood(ctx, testKey, "nori", depth)
			domain := assertDomainErr(t, err, errs.ErrInvalid, opNeighborhood, EntityNode)
			if domain.Msg != "depth must be an integer between 1 and 10" {
				t.Errorf("msg = %q, want the transport wording", domain.Msg)
			}
		}
	})

	t.Run("missing graph index is unavailable with the degraded wording", func(t *testing.T) {
		r := newRig(t, Graph{}, false)
		_, err := r.svc.Neighborhood(ctx, testKey, "nori", 1)
		if !errors.Is(err, errs.ErrUnavailable) {
			t.Fatalf("err = %v, want ErrUnavailable", err)
		}
		var domain *errs.Error
		if !errors.As(err, &domain) {
			t.Fatalf("error is not *errs.Error: %#v", err)
		}
		if domain.Msg != degradedGraph {
			t.Errorf("msg = %q, want %q", domain.Msg, degradedGraph)
		}
	})

	t.Run("index errors pass through untouched", func(t *testing.T) {
		r := newRig(t, Graph{}, true)
		r.graph.neighborhoodErr = errs.Unavailable("graph.Neighborhood", errBoom)
		_, err := r.svc.Neighborhood(ctx, testKey, "nori", 2)
		if !errors.Is(err, errs.ErrUnavailable) || !errors.Is(err, errBoom) {
			t.Fatalf("err = %v, want the injected unavailable chain", err)
		}
	})

	t.Run("forwards entity and depth, returns the subgraph unmodified", func(t *testing.T) {
		r := newRig(t, Graph{}, true)
		n := validNode(t)
		r.graph.neighborhoodResult = Graph{Nodes: []Node{n}}

		sub, err := r.svc.Neighborhood(ctx, testKey, "nori", 3)
		if err != nil {
			t.Fatalf("Neighborhood: %v", err)
		}
		if r.graph.neighborhoodEntity != "nori" || r.graph.neighborhoodDepth != 3 {
			t.Errorf("forwarded = (%q, %d), want (nori, 3)", r.graph.neighborhoodEntity, r.graph.neighborhoodDepth)
		}
		if len(sub.Nodes) != 1 || sub.Nodes[0].ID != n.ID {
			t.Errorf("subgraph = %+v, want the index result unmodified", sub)
		}
	})
}
