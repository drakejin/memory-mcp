// Package knowledge defines the knowledge memory plane domain: permanent
// nodes/edges with non-destructive revision (architecture-v2.md §2, §2.2).
// Nothing is ever deleted in the normal flow — nodes transition
// active → archived (supersede) → deprecated, and purge requires confirm with
// S3 versioning as the backstop (§3).
package knowledge

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/drakejin/memory-mcp/internal/ulid"
)

// NodeKind classifies a knowledge node (§2.2).
type NodeKind string

const (
	KindEntity     NodeKind = "entity"
	KindFact       NodeKind = "fact"
	KindLesson     NodeKind = "lesson"
	KindPreference NodeKind = "preference"
	KindDocument   NodeKind = "document"
)

// State is the node lifecycle state (v1 state machine inherited, §3).
type State string

const (
	StateActive     State = "active"
	StateArchived   State = "archived"
	StateDeprecated State = "deprecated"
)

// Trust records where a node's claim came from (§2.2).
type Trust string

const (
	TrustUserStated    Trust = "user-stated"
	TrustAgentInferred Trust = "agent-inferred"
	TrustImported      Trust = "imported"
)

// Rel is the edge relation type (§2.2).
type Rel string

const (
	RelRelatesTo   Rel = "relates_to"
	RelDerivedFrom Rel = "derived_from"
	RelSupersedes  Rel = "supersedes"
	RelAbout       Rel = "about"
)

// Node is a canonical knowledge node. JSON shape mirrors §2.2 exactly.
type Node struct {
	// ID is a ULID; immutable.
	ID   string   `json:"id"`
	Kind NodeKind `json:"kind"`
	// Name is the headline; Body the prose statement.
	Name    string   `json:"name"`
	Body    string   `json:"body"`
	Aliases []string `json:"aliases"`
	State   State    `json:"state"`
	Trust   Trust    `json:"trust"`
	// Supersedes lists node ids this node replaced; SupersededBy is the
	// replacing node id or "" while active.
	Supersedes   []string `json:"supersedes"`
	SupersededBy string   `json:"superseded_by"`
	// Provenance lists originating episode ids; links stay valid after the
	// episodes sink to cold because episode ids are immutable (§2).
	Provenance []string  `json:"provenance"`
	Created    time.Time `json:"created"`
	Updated    time.Time `json:"updated"`
	// ReviewAfter is RFC3339 or "" when no review is scheduled.
	ReviewAfter string `json:"review_after"`
}

// Edge is a canonical knowledge edge. JSON shape mirrors §2.2 exactly.
type Edge struct {
	From       string   `json:"from"`
	To         string   `json:"to"`
	Rel        Rel      `json:"rel"`
	Provenance []string `json:"provenance"`
	Confidence float64  `json:"confidence"`
}

// Graph is the per-project knowledge document persisted at
// knowledge/{ws}/{team}/{proj}.json in the hot store.
type Graph struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// Validation and state-machine errors.
var (
	ErrInvalidNode       = errors.New("knowledge: invalid node")
	ErrInvalidEdge       = errors.New("knowledge: invalid edge")
	ErrInvalidTransition = errors.New("knowledge: invalid state transition")
	ErrNodeNotFound      = errors.New("knowledge: node not found")
)

// nodeKinds, states, trusts and rels are the closed value sets of §2.2.
var (
	nodeKinds = map[NodeKind]bool{
		KindEntity: true, KindFact: true, KindLesson: true,
		KindPreference: true, KindDocument: true,
	}
	states = map[State]bool{
		StateActive: true, StateArchived: true, StateDeprecated: true,
	}
	trusts = map[Trust]bool{
		TrustUserStated: true, TrustAgentInferred: true, TrustImported: true,
	}
	rels = map[Rel]bool{
		RelRelatesTo: true, RelDerivedFrom: true, RelSupersedes: true, RelAbout: true,
	}
)

// transitions is the v1 state machine ported from src/lifecycle.ts: an
// archived buffer sits before any deletion, deprecated preserves a judged-wrong
// node, and both revive back to active. Same-state moves are no-ops.
var transitions = map[State][]State{
	StateActive:     {StateArchived, StateDeprecated},
	StateArchived:   {StateActive, StateDeprecated},
	StateDeprecated: {StateActive},
}

// Validate checks node invariants: ULID id, known kind/state/trust, non-empty
// name, active nodes carry no superseded_by, a set superseded_by is a ULID,
// and review_after is RFC3339 or empty. Wraps ErrInvalidNode.
func (n Node) Validate() error {
	if !ulid.IsULID(n.ID) {
		return fmt.Errorf("%w: id %q is not a ULID", ErrInvalidNode, n.ID)
	}
	if !nodeKinds[n.Kind] {
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidNode, n.Kind)
	}
	if !states[n.State] {
		return fmt.Errorf("%w: unknown state %q", ErrInvalidNode, n.State)
	}
	if !trusts[n.Trust] {
		return fmt.Errorf("%w: unknown trust %q", ErrInvalidNode, n.Trust)
	}
	if strings.TrimSpace(n.Name) == "" {
		return fmt.Errorf("%w: name must be non-empty", ErrInvalidNode)
	}
	if n.State == StateActive && n.SupersededBy != "" {
		return fmt.Errorf("%w: active node cannot have superseded_by", ErrInvalidNode)
	}
	if n.SupersededBy != "" && !ulid.IsULID(n.SupersededBy) {
		return fmt.Errorf("%w: superseded_by %q is not a ULID", ErrInvalidNode, n.SupersededBy)
	}
	if n.ReviewAfter != "" {
		if _, err := time.Parse(time.RFC3339, n.ReviewAfter); err != nil {
			return fmt.Errorf("%w: review_after %q is not RFC3339: %v", ErrInvalidNode, n.ReviewAfter, err)
		}
	}
	return nil
}

// Validate checks edge invariants: ULID endpoints, known rel, confidence in
// [0,1], and no self-supersede (v1: "cannot supersede itself"). Wraps
// ErrInvalidEdge.
func (e Edge) Validate() error {
	if !ulid.IsULID(e.From) {
		return fmt.Errorf("%w: from %q is not a ULID", ErrInvalidEdge, e.From)
	}
	if !ulid.IsULID(e.To) {
		return fmt.Errorf("%w: to %q is not a ULID", ErrInvalidEdge, e.To)
	}
	if !rels[e.Rel] {
		return fmt.Errorf("%w: unknown rel %q", ErrInvalidEdge, e.Rel)
	}
	if e.Rel == RelSupersedes && e.From == e.To {
		return fmt.Errorf("%w: node cannot supersede itself", ErrInvalidEdge)
	}
	if e.Confidence < 0 || e.Confidence > 1 {
		return fmt.Errorf("%w: confidence %v outside [0,1]", ErrInvalidEdge, e.Confidence)
	}
	return nil
}

// Transition returns a copy of n moved to target at now. Legal moves follow
// the v1 lifecycle (src/lifecycle.ts): active→{archived,deprecated},
// archived→{active,deprecated}, deprecated→active; same-state is a no-op.
// Deprecation requires a non-empty reason — why is it wrong? — which the
// caller records in the PATCH audit. Reviving to active clears superseded_by
// so the node satisfies the active invariant. Illegal moves wrap
// ErrInvalidTransition. n is never mutated.
func Transition(n Node, target State, reason string, now time.Time) (Node, error) {
	if !states[target] {
		return Node{}, fmt.Errorf("%w: unknown target state %q", ErrInvalidTransition, target)
	}
	if n.State != target && !containsState(transitions[n.State], target) {
		return Node{}, fmt.Errorf("%w: cannot transition %s -> %s", ErrInvalidTransition, n.State, target)
	}
	if target == StateDeprecated && strings.TrimSpace(reason) == "" {
		return Node{}, fmt.Errorf("%w: deprecating requires a reason — why is it wrong?", ErrInvalidTransition)
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
// directly from active.
func CanPurge(s State) bool {
	return s == StateArchived || s == StateDeprecated
}

// Supersede returns a new Graph where newNode (state=active) replaces the
// nodes named in supersedes: each replaced node becomes archived (unless
// already deprecated) with superseded_by=newNode.ID, newNode.Supersedes lists
// them, and a supersedes edge is added per replaced node. With an empty
// supersedes list it simply appends newNode. Input graph is never mutated.
// Missing ids wrap ErrNodeNotFound; a duplicate or self-referencing newNode
// wraps ErrInvalidNode.
func Supersede(g Graph, newNode Node, supersedes []string, now time.Time) (Graph, error) {
	if _, err := FindNode(g, newNode.ID); err == nil {
		return Graph{}, fmt.Errorf("%w: node %s already exists", ErrInvalidNode, newNode.ID)
	}

	targets := dedupe(supersedes)
	for _, id := range targets {
		if id == newNode.ID {
			return Graph{}, fmt.Errorf("%w: node %s cannot supersede itself", ErrInvalidNode, id)
		}
		if _, err := FindNode(g, id); err != nil {
			return Graph{}, fmt.Errorf("supersedes target %s: %w", id, err)
		}
	}

	nodes := make([]Node, len(g.Nodes), len(g.Nodes)+1)
	copy(nodes, g.Nodes)
	edges := make([]Edge, len(g.Edges), len(g.Edges)+len(targets))
	copy(edges, g.Edges)

	for i := range nodes {
		if !containsString(targets, nodes[i].ID) {
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
			Provenance: append([]string(nil), winner.Provenance...),
			Confidence: 1.0,
		})
	}

	return Graph{Nodes: nodes, Edges: edges}, nil
}

// FindNode returns the node with the given id, or ErrNodeNotFound.
func FindNode(g Graph, id string) (Node, error) {
	for _, n := range g.Nodes {
		if n.ID == id {
			return n, nil
		}
	}
	return Node{}, fmt.Errorf("%w: %s", ErrNodeNotFound, id)
}

func containsState(list []State, s State) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func hasEdge(edges []Edge, from, to string, rel Rel) bool {
	for _, e := range edges {
		if e.From == from && e.To == to && e.Rel == rel {
			return true
		}
	}
	return false
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
