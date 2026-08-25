package knowledge

// Node orchestration (F4-F6): input/result value types complete enough that
// the transport layer imports nothing below this package, plus the write
// choreography — hot JSON commits first and alone decides success, the Neo4j
// mirror follows best-effort (§0 principle 1, §5).

import (
	"context"
	"slices"
	"time"

	"github.com/drakejin/memory-mcp/internal/x/errs"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
	"github.com/drakejin/memory-mcp/internal/x/ulid"
)

// PATCH operations of §7. OpSetState names the target state explicitly;
// OpDeprecate is the shorthand that always targets deprecated and demands a
// reason. Exported because the transport layer maps its PATCH body onto them.
const (
	OpSetState  = "set_state"
	OpDeprecate = "deprecate"
)

// CreateNodeInput carries a node creation (F4). Supersedes lists node ids the
// new node replaces; the state-machine work happens in Supersede. JSON tags
// mirror the §7 POST body so the transport can decode into it directly.
type CreateNodeInput struct {
	Kind        NodeKind `json:"kind"`
	Name        string   `json:"name"`
	Body        string   `json:"body"`
	Aliases     []string `json:"aliases"`
	Trust       Trust    `json:"trust"`
	Provenance  []string `json:"provenance"`
	Supersedes  []string `json:"supersedes"`
	ReviewAfter string   `json:"review_after"`
}

// PatchNodeInput mutates node lifecycle state (F5): Op is OpSetState or
// OpDeprecate (deprecate requires Reason).
type PatchNodeInput struct {
	Op     string `json:"op"`
	State  State  `json:"state,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// NodeResult is a node write outcome plus degraded notes when the best-effort
// Neo4j mirror failed (§5 — the hot write already succeeded). Degraded is
// data, not an error. JSON shape matches the §7 response envelope data.
type NodeResult struct {
	Node     Node     `json:"node"`
	Degraded []string `json:"degraded,omitempty"`
}

// PurgeResult reports a hot purge; S3 versioning is the backstop (§3).
type PurgeResult struct {
	PurgedID     string   `json:"purged_id"`
	RemovedEdges int      `json:"removed_edges"`
	Degraded     []string `json:"degraded,omitempty"`
}

// validate applies the §2.2 invariants an incoming creation must satisfy.
// Checks and messages are byte-identical to the transport-side validation the
// orchestration was extracted from, so the gate holds whichever layer runs
// first. Failures are KindInvalid.
func (in CreateNodeInput) validate() error {
	if !ValidNodeKind(in.Kind) {
		return errs.Invalid(opCreateNode, EntityNode, "kind must be one of entity|fact|lesson|preference|document")
	}
	if in.Name == "" {
		return errs.Invalid(opCreateNode, EntityNode, "name must not be empty")
	}
	if !ValidTrust(in.Trust) {
		return errs.Invalid(opCreateNode, EntityNode, "trust must be one of user-stated|agent-inferred|imported")
	}
	if err := validULIDs(opCreateNode, EntityNode, "provenance", in.Provenance); err != nil {
		return err
	}
	if err := validULIDs(opCreateNode, EntityNode, "supersedes", in.Supersedes); err != nil {
		return err
	}
	if in.ReviewAfter != "" {
		if _, err := time.Parse(time.RFC3339, in.ReviewAfter); err != nil {
			return errs.Invalid(opCreateNode, EntityNode, "review_after must be RFC3339 or empty")
		}
	}
	return nil
}

// CreateNode implements Service. Contract preserved from the extracted
// handler: validate, assign ULID, apply Supersede against the hot graph inside
// one atomic UpdateKnowledge, then best-effort Neo4j MERGE with degraded notes
// on failure. No judgement calls server-side — supersede decisions are the
// agent's (§0 principle 2).
//
// Read, decide and write share one UpdateKnowledge closure: the document is
// replaced wholesale, so a read-then-write pair would let a concurrent
// creation drop this node from hot while both reach Neo4j — exactly the
// derived-only content §0 principle 1 forbids.
func (s *service) CreateNode(ctx context.Context, key projectkey.Key, in CreateNodeInput) (NodeResult, error) {
	if err := in.validate(); err != nil {
		return NodeResult{}, err
	}

	now := s.clock.Now().UTC()
	id, err := s.ids.GenerateAt(now.UnixMilli())
	if err != nil {
		return NodeResult{}, err
	}
	node := Node{
		ID:          id,
		Kind:        in.Kind,
		Name:        in.Name,
		Body:        in.Body,
		Aliases:     normalizeStrings(in.Aliases),
		State:       StateActive,
		Trust:       in.Trust,
		Supersedes:  []string{},
		Provenance:  normalizeStrings(in.Provenance),
		Created:     now,
		Updated:     now,
		ReviewAfter: in.ReviewAfter,
	}

	var stored Node
	var affected []Node
	var newEdges []Edge
	err = s.store.UpdateKnowledge(ctx, key, func(g Graph) (Graph, error) {
		var next Graph
		if len(in.Supersedes) > 0 {
			// The domain owns the state machine: a missing target is its
			// NotFound, a self- or duplicate-supersede its Invalid/Conflict.
			var err error
			next, err = Supersede(g, node, in.Supersedes, now)
			if err != nil {
				return g, err
			}
		} else {
			next = Graph{
				Nodes: append(slices.Clone(g.Nodes), node),
				Edges: slices.Clone(g.Edges),
			}
		}
		stored = node
		if found, err := FindNode(next, node.ID); err == nil {
			stored = found
		}
		affected = affectedNodes(next, node.ID, in.Supersedes)
		newEdges = diffEdges(g, next)
		return next, nil
	})
	if err != nil {
		return NodeResult{}, err
	}

	degraded := s.mirror(ctx, key, affected, newEdges)
	return NodeResult{Node: stored, Degraded: degraded}, nil
}

// targetState resolves the requested operation to the state to transition
// into. The empty-reason gate answers KindInvalid — bad input, 400 — before
// any store access, exactly as the transport gate did; a blank-but-non-empty
// reason still reaches Transition, whose refusal is the state machine's
// KindConflict (P5: deprecate needs a reason).
func (in PatchNodeInput) targetState() (State, error) {
	var target State
	switch in.Op {
	case OpSetState:
		if !ValidState(in.State) {
			return "", errs.Invalid(opPatchNode, EntityNode, "state must be one of active|archived|deprecated")
		}
		target = in.State
	case OpDeprecate:
		target = StateDeprecated
	default:
		return "", errs.Invalid(opPatchNode, EntityNode, "op must be "+OpSetState+" or "+OpDeprecate)
	}
	if target == StateDeprecated && in.Reason == "" {
		return "", errs.Invalid(opPatchNode, EntityNode, "deprecation requires a reason")
	}
	return target, nil
}

// PatchNode implements Service. Transition enforces legality (an illegal move
// is its Conflict, hence 409 at the boundary); hot write first, then
// best-effort MERGE of the updated node.
//
// Lookup, transition and write share one closure: a patch computed against a
// stale snapshot would resurrect every node a concurrent write added after
// that snapshot was taken.
func (s *service) PatchNode(ctx context.Context, key projectkey.Key, id string, in PatchNodeInput) (NodeResult, error) {
	if !ulid.Valid(id) {
		return NodeResult{}, errs.Invalid(opPatchNode, EntityNode, msgIDMustBeULID)
	}
	target, err := in.targetState()
	if err != nil {
		return NodeResult{}, err
	}

	var updated Node
	err = s.store.UpdateKnowledge(ctx, key, func(g Graph) (Graph, error) {
		idx := slices.IndexFunc(g.Nodes, func(n Node) bool { return n.ID == id })
		if idx < 0 {
			return g, notFoundMsg(opPatchNode, id, msgNodeNotFound)
		}
		var err error
		updated, err = Transition(g.Nodes[idx], target, in.Reason, s.clock.Now().UTC())
		if err != nil {
			return g, err
		}
		next := Graph{Nodes: slices.Clone(g.Nodes), Edges: slices.Clone(g.Edges)}
		next.Nodes[idx] = updated
		return next, nil
	})
	if err != nil {
		return NodeResult{}, err
	}

	degraded := s.mirror(ctx, key, []Node{updated}, nil)
	return NodeResult{Node: updated, Degraded: degraded}, nil
}

// PurgeNode implements Service. The confirm gate runs before id validation so
// a destructive call is rejected for the destructive reason first (P5). The
// lifecycle gate is Purge's: §3 requires the archived/deprecated buffer before
// any deletion, so an active node is a Conflict, not a silent wipe. Then the
// node is best-effort deleted from Neo4j, and the survivors whose chain
// references the purge repaired are best-effort re-MERGEd — the mirror must
// carry the repaired truth, not a ghost superseded_by/supersedes (P5).
//
// Lookup, lifecycle gate and removal share one closure so a purge cannot
// rewrite the graph from a snapshot taken before a concurrent node creation,
// nor pass a gate against a state a concurrent patch has since changed.
func (s *service) PurgeNode(ctx context.Context, key projectkey.Key, id string, confirm bool) (PurgeResult, error) {
	if !confirm {
		return PurgeResult{}, errs.Invalid(opPurgeNode, EntityNode, msgPurgeNeedsConfirm)
	}
	if !ulid.Valid(id) {
		return PurgeResult{}, errs.Invalid(opPurgeNode, EntityNode, msgIDMustBeULID)
	}

	var removedEdges int
	var repaired []Node
	err := s.store.UpdateKnowledge(ctx, key, func(g Graph) (Graph, error) {
		next, removed, err := Purge(g, id)
		if err != nil {
			return g, err
		}
		removedEdges = removed
		repaired = repairedSurvivors(g, next, id)
		return next, nil
	})
	if err != nil {
		return PurgeResult{}, err
	}

	degraded := s.deleteMirror(ctx, key, id)
	// With the delete already degraded the graph is down (and the plane marked
	// dirty); a second attempt would only duplicate the note.
	if len(degraded) == 0 && len(repaired) > 0 {
		degraded = s.mirror(ctx, key, repaired, nil)
	}
	return PurgeResult{PurgedID: id, RemovedEdges: removedEdges, Degraded: degraded}, nil
}
