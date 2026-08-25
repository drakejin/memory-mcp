package server

import (
	"slices"

	"github.com/drakejin/memory-mcp/internal/knowledge"
	"github.com/drakejin/memory-mcp/internal/server/apierr"
	"github.com/drakejin/memory-mcp/internal/ulid"
)

// This file holds the pure graph algebra the knowledge handlers apply to a hot
// graph before persisting it, plus the edge validation that belongs to the HTTP
// boundary. Nothing here touches a Server, a request or a response: the
// functions take a graph and return a new one, so the handlers stay a thin
// read-decide-write sequence and these rules can be read in one place.
//
// The lifecycle rules themselves (supersede, state transitions) are not here —
// they belong to the knowledge package, which owns the state machine.

// findGraphNode returns the node with id from g, or fallback when absent.
func findGraphNode(g knowledge.Graph, id string, fallback knowledge.Node) knowledge.Node {
	for _, n := range g.Nodes {
		if n.ID == id {
			return n
		}
	}
	return fallback
}

// affectedNodes collects the new node plus every superseded node in its
// post-transition form, for a minimal best-effort MERGE.
func affectedNodes(g knowledge.Graph, newID string, superseded []string) []knowledge.Node {
	want := map[string]bool{newID: true}
	for _, id := range superseded {
		want[id] = true
	}
	var out []knowledge.Node
	for _, n := range g.Nodes {
		if want[n.ID] {
			out = append(out, n)
		}
	}
	return out
}

// diffEdges returns edges present in next but not in prev (from/to/rel match).
func diffEdges(prev, next knowledge.Graph) []knowledge.Edge {
	seen := make(map[[3]string]bool, len(prev.Edges))
	for _, e := range prev.Edges {
		seen[[3]string{e.From, e.To, string(e.Rel)}] = true
	}
	var out []knowledge.Edge
	for _, e := range next.Edges {
		if !seen[[3]string{e.From, e.To, string(e.Rel)}] {
			out = append(out, e)
		}
	}
	return out
}

// Edge confidence bounds (§2.2).
const (
	minEdgeConfidence = 0.0
	maxEdgeConfidence = 1.0
)

// validateEdge applies the §2.2 edge invariants at the HTTP boundary. The
// knowledge package checks the same rules on its own boundary.
func validateEdge(edge knowledge.Edge) *apierr.Error {
	if !ulid.Valid(edge.From) || !ulid.Valid(edge.To) {
		return badRequest("from and to must be node ULIDs")
	}
	if !knowledge.ValidRel(edge.Rel) {
		return badRequest("rel must be one of relates_to|derived_from|supersedes|about")
	}
	if edge.Confidence < minEdgeConfidence || edge.Confidence > maxEdgeConfidence {
		return badRequest("confidence must be within [0,1]")
	}
	return validateULIDs(edge.Provenance, "provenance")
}

// upsertEdge returns g with edge inserted, replacing any existing edge on the
// same (from, to, rel): re-posting an edge is idempotent. g is never mutated.
func upsertEdge(g knowledge.Graph, edge knowledge.Edge) knowledge.Graph {
	next := knowledge.Graph{Nodes: slices.Clone(g.Nodes), Edges: make([]knowledge.Edge, 0, len(g.Edges)+1)}
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

// nodeExists reports whether id is a node of g. Edge creation resolves its
// endpoints here rather than asking the mirror, because hot is canonical (§0
// principle 1). Node removal does not go through here: it is a lifecycle
// operation, so knowledge.Purge owns both the lookup and the state gate.
func nodeExists(g knowledge.Graph, id string) bool {
	for _, n := range g.Nodes {
		if n.ID == id {
			return true
		}
	}
	return false
}
