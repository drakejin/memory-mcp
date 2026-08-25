package knowledge

// Edge orchestration (F7): both endpoints must exist in the hot graph, hot
// commits first, then the best-effort Neo4j MERGE.

import (
	"context"
	"slices"

	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// EdgeResult is an edge write outcome plus degraded notes (§5).
type EdgeResult struct {
	Edge     Edge     `json:"edge"`
	Degraded []string `json:"degraded,omitempty"`
}

// CreateEdge implements Service. Edge.Validate is the gate: it carries the P5
// self-supersede rule (rel == supersedes with from == to is KindInvalid) that
// the transport-side validation historically missed — docs/spec/05 records
// self-loop supersede edges reaching Neo4j through POST /knowledge/edges.
// Routing every edge through the domain validator closes that hole. The
// provenance ids are checked separately because Edge.Validate does not cover
// them.
func (s *service) CreateEdge(ctx context.Context, key projectkey.Key, edge Edge) (EdgeResult, error) {
	if err := edge.Validate(); err != nil {
		return EdgeResult{}, err
	}
	if err := validULIDs(opCreateEdge, EntityEdge, "provenance", edge.Provenance); err != nil {
		return EdgeResult{}, err
	}
	edge.Provenance = normalizeStrings(edge.Provenance)

	// The endpoint check and the insert share one closure so an edge can never
	// be written against a node a concurrent purge has already removed. Hot is
	// canonical (§0 principle 1), so endpoints resolve here, never against the
	// mirror.
	err := s.store.UpdateKnowledge(ctx, key, func(g Graph) (Graph, error) {
		if _, err := FindNode(g, edge.From); err != nil {
			return g, notFoundMsg(opCreateEdge, edge.From, msgEdgeEndpointNotFound)
		}
		if _, err := FindNode(g, edge.To); err != nil {
			return g, notFoundMsg(opCreateEdge, edge.To, msgEdgeEndpointNotFound)
		}
		return upsertEdge(g, edge), nil
	})
	if err != nil {
		return EdgeResult{}, err
	}

	degraded := s.mirror(ctx, key, nil, []Edge{edge})
	return EdgeResult{Edge: edge, Degraded: degraded}, nil
}

// upsertEdge returns g with edge inserted, replacing any existing edge on the
// same (from, to, rel): re-posting an edge is idempotent (P3). g is never
// mutated.
func upsertEdge(g Graph, edge Edge) Graph {
	next := Graph{Nodes: slices.Clone(g.Nodes), Edges: make([]Edge, 0, len(g.Edges)+1)}
	replaced := false
	for _, e := range g.Edges {
		if e.From == edge.From && e.To == edge.To && e.Rel == edge.Rel {
			next.Edges = append(next.Edges, edge)
			replaced = true
			continue
		}
		next.Edges = append(next.Edges, e)
	}
	if !replaced {
		next.Edges = append(next.Edges, edge)
	}
	return next
}
