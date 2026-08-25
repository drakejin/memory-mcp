package knowledge

// Read orchestration (F8, F9). Reads hit the derived graph index, and a dead
// index has no honest partial answer: unavailability is an error here, where a
// write would have degraded (§5). The request-entry stat-gate stays with the
// transport layer — it is a cross-cutting freshness mechanism (F17), not
// knowledge orchestration.

import (
	"context"
	"fmt"

	"github.com/drakejin/memory-mcp/internal/x/errs"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// DefaultDepth and MaxDepth bound the §7 neighborhood traversal. Exported so
// the transport layer parses its depth parameter against the same numbers
// instead of keeping a second copy.
const (
	DefaultDepth = 1
	MaxDepth     = 10
)

// graphUnavailable reports the graph index missing or down. The public
// message is the §5 degraded vocabulary ("graph unavailable"), not the errs
// default, so the client reads the same wording a write's degraded note
// carries. Built as a literal so no error value is mutated after construction.
func graphUnavailable(op string) *errs.Error {
	return &errs.Error{Kind: errs.KindUnavailable, Op: op, Msg: degradedGraph}
}

// Search implements Service: fulltext over the graph index, archived and
// deprecated nodes only on opt-in (F8). The result is never nil — an empty
// match must serialize as [], not null (§7 envelope).
func (s *service) Search(ctx context.Context, key projectkey.Key, q string, includeArchived bool) ([]Node, error) {
	if q == "" {
		return nil, errs.Invalid(opSearch, EntityNode, "q is required")
	}
	if s.graph == nil {
		return nil, graphUnavailable(opSearch)
	}
	nodes, err := s.graph.Search(ctx, key, q, includeArchived)
	if err != nil {
		return nil, err
	}
	if nodes == nil {
		nodes = []Node{}
	}
	return nodes, nil
}

// Neighborhood implements Service: traversal up to depth hops around an
// entity name or alias (F9). depth 0 selects DefaultDepth so callers may omit
// it; anything outside [DefaultDepth, MaxDepth] is rejected with the exact
// transport wording.
func (s *service) Neighborhood(ctx context.Context, key projectkey.Key, entity string, depth int) (Graph, error) {
	if entity == "" {
		return Graph{}, errs.Invalid(opNeighborhood, EntityNode, "entity is required")
	}
	if depth == 0 {
		depth = DefaultDepth
	}
	if depth < DefaultDepth || depth > MaxDepth {
		return Graph{}, errs.Invalid(opNeighborhood, EntityNode,
			fmt.Sprintf("depth must be an integer between %d and %d", DefaultDepth, MaxDepth))
	}
	if s.graph == nil {
		return Graph{}, graphUnavailable(opNeighborhood)
	}
	return s.graph.Neighborhood(ctx, key, entity, depth)
}
