// Package knowledge defines the knowledge memory plane domain: permanent
// nodes/edges with non-destructive revision (architecture-v2.md §2, §2.2).
// Nothing is ever deleted in the normal flow — nodes transition
// active → archived (supersede) → deprecated, and purge requires confirm with
// S3 versioning as the backstop (§3).
//
// The package is pure: no I/O, no ambient clock (callers pass now), no HTTP.
// Every error that crosses the package boundary is an *errs.Error carrying the
// meaning in its Kind (code-standards §2.1): KindInvalid for a shape violation,
// KindNotFound for a missing node, KindConflict for a state-machine violation
// or a duplicate. Callers compare with errors.Is(err, errs.ErrInvalid) and
// friends, never by string.
package knowledge

import (
	"strings"
	"time"

	"github.com/drakejin/memory-mcp/internal/errs"
	"github.com/drakejin/memory-mcp/internal/ulid"
)

// Entity names carried by every error this package produces. Exported because
// adapters (internal/graph) raise not-found errors about the very same
// entities and must not re-spell the string.
const (
	EntityNode = "knowledge_node"
	EntityEdge = "knowledge_edge"
)

// Op names — the readable call path recorded on each error (§2.1).
const (
	opValidateNode = "knowledge.Node.Validate"
	opValidateEdge = "knowledge.Edge.Validate"
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
	Provenance []string `json:"provenance"`
	// Created/Updated marshal as RFC3339 and are stored UTC by the writers.
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
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

// Confidence bounds for an edge (§2.2).
const (
	minConfidence = 0.0
	maxConfidence = 1.0
)

// ValidNodeKind, ValidState, ValidTrust and ValidRel close the value sets of
// §2.2. They are switches rather than package-level maps so the sets cannot be
// mutated at runtime (code-standards §1.1: no mutable package state).
//
// They are exported because this package owns the vocabulary: the HTTP
// boundary validates the same enums before a Node ever reaches Validate, and
// it must not keep a second copy of the sets (code-standards §4).
func ValidNodeKind(k NodeKind) bool {
	switch k {
	case KindEntity, KindFact, KindLesson, KindPreference, KindDocument:
		return true
	default:
		return false
	}
}

func ValidState(s State) bool {
	switch s {
	case StateActive, StateArchived, StateDeprecated:
		return true
	default:
		return false
	}
}

func ValidTrust(t Trust) bool {
	switch t {
	case TrustUserStated, TrustAgentInferred, TrustImported:
		return true
	default:
		return false
	}
}

func ValidRel(r Rel) bool {
	switch r {
	case RelRelatesTo, RelDerivedFrom, RelSupersedes, RelAbout:
		return true
	default:
		return false
	}
}

// Validate checks node invariants: ULID id, known kind/state/trust, non-empty
// name, active nodes carry no superseded_by, a set superseded_by is a ULID,
// and review_after is RFC3339 or empty. Failures are KindInvalid.
//
// Kept alongside the transport-layer checks in internal/server on purpose: the
// spec (docs/spec/09-code-structure.md §6.1) records the two as duplicated
// validation to be merged handler-side, not as dead code to drop.
func (n Node) Validate() error {
	if !ulid.Valid(n.ID) {
		return errs.Invalid(opValidateNode, EntityNode, "id must be a ULID")
	}
	if !ValidNodeKind(n.Kind) {
		return errs.Invalid(opValidateNode, EntityNode, "kind must be one of entity|fact|lesson|preference|document").WithField("kind", string(n.Kind))
	}
	if !ValidState(n.State) {
		return errs.Invalid(opValidateNode, EntityNode, "state must be one of active|archived|deprecated").WithField("state", string(n.State))
	}
	if !ValidTrust(n.Trust) {
		return errs.Invalid(opValidateNode, EntityNode, "trust must be one of user-stated|agent-inferred|imported").WithField("trust", string(n.Trust))
	}
	if strings.TrimSpace(n.Name) == "" {
		return errs.Invalid(opValidateNode, EntityNode, "name must be non-empty")
	}
	if n.State == StateActive && n.SupersededBy != "" {
		return errs.Invalid(opValidateNode, EntityNode, "active node cannot have superseded_by")
	}
	if n.SupersededBy != "" && !ulid.Valid(n.SupersededBy) {
		return errs.Invalid(opValidateNode, EntityNode, "superseded_by must be a ULID")
	}
	if n.ReviewAfter != "" {
		if _, err := time.Parse(time.RFC3339, n.ReviewAfter); err != nil {
			return errs.Invalid(opValidateNode, EntityNode, "review_after must be RFC3339 or empty").WithField("review_after", n.ReviewAfter)
		}
	}
	return nil
}

// Validate checks edge invariants: ULID endpoints, known rel, confidence in
// [0,1], and no self-supersede (v1: "cannot supersede itself"). Failures are
// KindInvalid.
//
// Duplicated transport-side today for the same reason as Node.Validate.
func (e Edge) Validate() error {
	if !ulid.Valid(e.From) {
		return errs.Invalid(opValidateEdge, EntityEdge, "from must be a node ULID")
	}
	if !ulid.Valid(e.To) {
		return errs.Invalid(opValidateEdge, EntityEdge, "to must be a node ULID")
	}
	if !ValidRel(e.Rel) {
		return errs.Invalid(opValidateEdge, EntityEdge, "rel must be one of relates_to|derived_from|supersedes|about").WithField("rel", string(e.Rel))
	}
	if e.Rel == RelSupersedes && e.From == e.To {
		return errs.Invalid(opValidateEdge, EntityEdge, "node cannot supersede itself")
	}
	if e.Confidence < minConfidence || e.Confidence > maxConfidence {
		return errs.Invalid(opValidateEdge, EntityEdge, "confidence must be within [0,1]").WithField("confidence", e.Confidence)
	}
	return nil
}
