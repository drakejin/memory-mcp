package knowledge

// Best-effort mirroring into the Neo4j graph index and the §5 manifest
// bookkeeping it entails, plus the pure graph algebra that computes the
// minimal MERGE payload. A mirror failure is never an error: the hot write
// already succeeded, so the outcome is a degraded note plus a dirty mark the
// next rehydration pass converges (§1, §5).

import (
	"context"
	"slices"
	"strings"

	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// mirror best-effort MERGEs nodes and edges into the graph index and
// maintains manifest freshness. It returns the degraded notes for the result.
func (s *service) mirror(ctx context.Context, key projectkey.Key, nodes []Node, edges []Edge) []string {
	if s.graph == nil {
		s.markDirty(ctx, key)
		return []string{degradedGraph}
	}
	var err error
	if len(nodes) > 0 {
		err = s.graph.UpsertNodes(ctx, key, nodes)
	}
	if err == nil && len(edges) > 0 {
		err = s.graph.UpsertEdges(ctx, key, edges)
	}
	if err != nil {
		s.log.Warn("knowledge graph mirror failed; degraded", "project", key.String(), "error", err)
		s.markDirty(ctx, key)
		return []string{degradedGraph}
	}
	s.markIndexed(ctx, key)
	return nil
}

// deleteMirror best-effort removes a purged node from the graph index, with
// the same bookkeeping as mirror.
func (s *service) deleteMirror(ctx context.Context, key projectkey.Key, id string) []string {
	if s.graph == nil {
		s.markDirty(ctx, key)
		return []string{degradedGraph}
	}
	if err := s.graph.DeleteNode(ctx, key, id); err != nil {
		s.log.Warn("graph node delete failed; degraded", "project", key.String(), "id", id, "error", err)
		s.markDirty(ctx, key)
		return []string{degradedGraph}
	}
	s.markIndexed(ctx, key)
	return nil
}

// markDirty flags the project's knowledge plane after a failed mirror so the
// next rehydration converges it (§1). Log-only on failure — manifest
// bookkeeping must never fail a request that already wrote hot.
func (s *service) markDirty(ctx context.Context, key projectkey.Key) {
	if err := s.store.MarkDirty(ctx, key); err != nil {
		s.log.Error("manifest dirty mark failed", "project", key.String(), "error", err)
	}
}

// markIndexed records a successful mirror: dirty cleared and freshness
// refreshed so drift detection stays quiet for converged content (§5).
// Log-only on failure, same rule as markDirty.
func (s *service) markIndexed(ctx context.Context, key projectkey.Key) {
	if err := s.store.MarkIndexed(ctx, key); err != nil {
		s.log.Error("manifest freshness update failed", "project", key.String(), "error", err)
	}
}

// repairedSurvivors returns the post-purge form of every remaining node whose
// chain references Purge repaired (superseded_by cleared or a supersedes entry
// dropped). They must be re-MERGEd best-effort, because a DETACH DELETE of the
// purged node alone would leave the mirror's survivors pointing at a ghost id
// hot no longer knows — a divergence node-count drift checks can never see
// (P5: after a purge, no ghost reference in EITHER store).
func repairedSurvivors(prev, next Graph, purgedID string) []Node {
	touched := make(map[string]bool, len(prev.Nodes))
	for _, n := range prev.Nodes {
		if n.ID == purgedID {
			continue
		}
		if n.SupersededBy == purgedID || slices.Contains(n.Supersedes, purgedID) {
			touched[n.ID] = true
		}
	}
	var out []Node
	for _, n := range next.Nodes {
		if touched[n.ID] {
			out = append(out, n)
		}
	}
	return out
}

// affectedNodes collects the new node plus every superseded node in its
// post-transition form, for a minimal best-effort MERGE.
func affectedNodes(g Graph, newID string, superseded []string) []Node {
	want := map[string]bool{newID: true}
	for _, id := range superseded {
		want[id] = true
	}
	var out []Node
	for _, n := range g.Nodes {
		if want[n.ID] {
			out = append(out, n)
		}
	}
	return out
}

// diffEdges returns edges present in next but not in prev (from/to/rel match).
func diffEdges(prev, next Graph) []Edge {
	seen := make(map[[3]string]bool, len(prev.Edges))
	for _, e := range prev.Edges {
		seen[[3]string{e.From, e.To, string(e.Rel)}] = true
	}
	var out []Edge
	for _, e := range next.Edges {
		if !seen[[3]string{e.From, e.To, string(e.Rel)}] {
			out = append(out, e)
		}
	}
	return out
}

// normalizeStrings trims entries and drops empties, always returning a
// non-nil slice so hot JSON never stores null arrays.
func normalizeStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
