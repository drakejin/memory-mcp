package knowledge

import (
	"slices"
	"strings"
	"time"

	"github.com/drakejin/memory-mcp/internal/errs"
)

// Op names for the lifecycle operations (§2.1).
const (
	opTransition = "knowledge.Transition"
	opSupersede  = "knowledge.Supersede"
	opPurge      = "knowledge.Purge"
	opFindNode   = "knowledge.FindNode"
)

// msgPurgeNotBuffered is the rule a rejected purge reports, worded as the rule
// itself so the client is told what to do instead (code-standards §2.2).
const msgPurgeNotBuffered = "purge requires state archived or deprecated — archive or deprecate the node first"

// supersedeConfidence is the weight of a supersedes edge: the chain is a fact
// the caller asserted, not an inference.
const supersedeConfidence = 1.0

// allowedTargets is the v1 state machine ported from src/lifecycle.ts: an
// archived buffer sits before any deletion, deprecated preserves a judged-wrong
// node, and both revive back to active. Same-state moves are handled by
// Transition as no-ops.
func allowedTargets(from State) []State {
	switch from {
	case StateActive:
		return []State{StateArchived, StateDeprecated}
	case StateArchived:
		return []State{StateActive, StateDeprecated}
	case StateDeprecated:
		return []State{StateActive}
	default:
		return nil
	}
}

// Transition returns a copy of n moved to target at now. Legal moves follow
// the v1 lifecycle (src/lifecycle.ts): active→{archived,deprecated},
// archived→{active,deprecated}, deprecated→active; same-state is a no-op.
// Deprecation requires a non-empty reason — why is it wrong? — which the
// caller records in the PATCH audit. Reviving to active clears superseded_by
// so the node satisfies the active invariant. n is never mutated.
//
// Every rejection is KindConflict: refusing a move is a state-machine
// violation, and the public message is the rule that was broken (the transport
// layer surfaces it as 409, docs/spec/08-http-api.md).
func Transition(n Node, target State, reason string, now time.Time) (Node, error) {
	if !ValidState(target) {
		return Node{}, errs.Conflict(opTransition, EntityNode, n.ID,
			"target state must be one of active|archived|deprecated").WithField("target", string(target))
	}
	if n.State != target && !slices.Contains(allowedTargets(n.State), target) {
		return Node{}, errs.Conflict(opTransition, EntityNode, n.ID,
			"cannot transition "+string(n.State)+" -> "+string(target))
	}
	if target == StateDeprecated && strings.TrimSpace(reason) == "" {
		return Node{}, errs.Conflict(opTransition, EntityNode, n.ID,
			"deprecating requires a reason — why is it wrong?")
	}
	out := n
	out.State = target
	if target == StateActive {
		out.SupersededBy = ""
	}
	out.Updated = now
	return out, nil
}

// CanPurge reports whether a node in the given state may be purged. Ported
// from v1: purge is only allowed from the archived/deprecated buffer, never
// directly from active. Purge is the gate's only caller.
func CanPurge(s State) bool {
	return s == StateArchived || s == StateDeprecated
}

// Purge returns a copy of g without the node id, without every edge incident to
// it, and with every dangling reference to it repaired — plus the number of
// edges removed. g is never mutated.
//
// The state gate is the point: §3 puts the archived → deprecated buffer in
// front of any deletion ("삭제 대신 상태 전이"), so purging an active node
// directly is a state-machine violation and therefore KindConflict. A missing
// id is KindNotFound.
//
// Repairing the references matters as much as removing the node. A node the
// purged one had superseded still records superseded_by, and a node it had
// been superseded by still lists it in supersedes; left alone, those point at
// an id no node carries any more, which truncates the revision chain and can
// make an otherwise valid node fail Node.Validate.
func Purge(g Graph, id string) (Graph, int, error) {
	target, err := FindNode(g, id)
	if err != nil {
		return Graph{}, 0, errs.NotFound(opPurge, EntityNode, id)
	}
	if !CanPurge(target.State) {
		return Graph{}, 0, errs.Conflict(opPurge, EntityNode, id, msgPurgeNotBuffered).
			WithField("state", string(target.State))
	}

	nodes := make([]Node, 0, len(g.Nodes))
	for _, n := range g.Nodes {
		if n.ID == id {
			continue
		}
		if n.SupersededBy == id {
			n.SupersededBy = ""
		}
		if slices.Contains(n.Supersedes, id) {
			n.Supersedes = withoutID(n.Supersedes, id)
		}
		nodes = append(nodes, n)
	}

	edges := make([]Edge, 0, len(g.Edges))
	removed := 0
	for _, e := range g.Edges {
		if e.From == id || e.To == id {
			removed++
			continue
		}
		edges = append(edges, e)
	}
	return Graph{Nodes: nodes, Edges: edges}, removed, nil
}

// withoutID returns a new slice holding every id except drop. The input is
// never mutated, so the caller's graph keeps its own backing array.
func withoutID(ids []string, drop string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != drop {
			out = append(out, id)
		}
	}
	return out
}

// Supersede returns a new Graph where newNode (state=active) replaces the
// nodes named in supersedes: each replaced node becomes archived (unless
// already deprecated) with superseded_by=newNode.ID, newNode.Supersedes lists
// them, and a supersedes edge is added per replaced node. With an empty
// supersedes list it simply appends newNode. Input graph is never mutated.
//
// Errors: a newNode id already in the graph is KindConflict (duplicate), a
// node superseding itself is KindInvalid, and a missing target is KindNotFound.
func Supersede(g Graph, newNode Node, supersedes []string, now time.Time) (Graph, error) {
	if _, err := FindNode(g, newNode.ID); err == nil {
		return Graph{}, errs.Conflict(opSupersede, EntityNode, newNode.ID, "node already exists")
	}

	targets := dedupe(supersedes)
	for _, id := range targets {
		if id == newNode.ID {
			return Graph{}, errs.Invalid(opSupersede, EntityNode, "node cannot supersede itself")
		}
		if _, err := FindNode(g, id); err != nil {
			return Graph{}, errs.NotFound(opSupersede, EntityNode, id)
		}
	}

	nodes := make([]Node, len(g.Nodes), len(g.Nodes)+1)
	copy(nodes, g.Nodes)
	edges := make([]Edge, len(g.Edges), len(g.Edges)+len(targets))
	copy(edges, g.Edges)

	for i := range nodes {
		if !slices.Contains(targets, nodes[i].ID) {
			continue
		}
		// Non-destructive revision (v1 applySupersede): the loser records
		// superseded_by and, if active, moves to the archived buffer.
		nodes[i].SupersededBy = newNode.ID
		if nodes[i].State == StateActive {
			nodes[i].State = StateArchived
		}
		nodes[i].Updated = now
	}

	winner := newNode
	winner.State = StateActive
	winner.SupersededBy = ""
	winner.Supersedes = targets
	if winner.Created.IsZero() {
		winner.Created = now
	}
	winner.Updated = now
	nodes = append(nodes, winner)

	for _, id := range targets {
		if hasEdge(edges, winner.ID, id, RelSupersedes) {
			continue
		}
		edges = append(edges, Edge{
			From:       winner.ID,
			To:         id,
			Rel:        RelSupersedes,
			Provenance: slices.Clone(winner.Provenance),
			Confidence: supersedeConfidence,
		})
	}

	return Graph{Nodes: nodes, Edges: edges}, nil
}

// FindNode returns the node with the given id. A missing id is KindNotFound.
// It is the single node lookup of this package — Supersede uses it for both
// its duplicate and its target checks.
func FindNode(g Graph, id string) (Node, error) {
	for _, n := range g.Nodes {
		if n.ID == id {
			return n, nil
		}
	}
	return Node{}, errs.NotFound(opFindNode, EntityNode, id)
}

func hasEdge(edges []Edge, from, to string, rel Rel) bool {
	return slices.ContainsFunc(edges, func(e Edge) bool {
		return e.From == from && e.To == to && e.Rel == rel
	})
}

func dedupe(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
