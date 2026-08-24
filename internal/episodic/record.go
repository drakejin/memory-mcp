// Package episodic defines the episodic memory plane domain: append-only event
// records indexed in OpenSearch (architecture-v2.md §2, §2.1). Records are
// never edited; corrections are new records. IDs are ULIDs and immutable, so
// knowledge provenance links survive cold archival.
//
// The package is pure domain: no I/O, no clock, no transport. Validation
// failures cross the package boundary as *errs.Error with KindInvalid
// (code-standards §2.1), so callers branch with errors.Is(err, errs.ErrInvalid).
package episodic

import (
	"fmt"
	"strings"
	"time"

	"github.com/drakejin/memory-mcp/internal/errs"
	"github.com/drakejin/memory-mcp/internal/ulid"
)

// Op and entity carried by every error this package returns.
const (
	opValidate    = "episodic.Validate"
	entityEpisode = "episode"
)

// Kind classifies an episodic record (§2.1).
type Kind string

const (
	KindEvent         Kind = "event"
	KindConversation  Kind = "conversation"
	KindDecision      Kind = "decision"
	KindObservation   Kind = "observation"
	KindDocumentChunk Kind = "document_chunk"
)

// Actor identifies who produced the record (§2.1).
type Actor string

const (
	ActorAgent  Actor = "agent"
	ActorUser   Actor = "user"
	ActorSystem Actor = "system"
)

// ValidKind and ValidActor close the record vocabulary of §2.1. They are
// switches rather than package-level slices so the sets cannot be mutated at
// runtime (code-standards §1.1: no mutable package state), matching the
// idiom internal/knowledge uses for its own value sets.
//
// They are exported because this package owns the vocabulary: the HTTP
// boundary validates the same enums before a Record ever reaches Validate, and
// it must not keep a second copy of the sets (code-standards §4).
func ValidKind(k Kind) bool {
	switch k {
	case KindEvent, KindConversation, KindDecision, KindObservation, KindDocumentChunk:
		return true
	default:
		return false
	}
}

func ValidActor(a Actor) bool {
	switch a {
	case ActorAgent, ActorUser, ActorSystem:
		return true
	default:
		return false
	}
}

// Refs links a document_chunk record back to its source blob (§2.1, §6).
type Refs struct {
	DocSHA   string `json:"doc_sha"`
	ChunkSeq int    `json:"chunk_seq"`
}

// Record is the canonical episodic record persisted in hot JSON and mirrored
// into OpenSearch. JSON shape mirrors §2.1 exactly.
type Record struct {
	// ID is a ULID; immutable forever (provenance anchor).
	ID string `json:"id"`
	// Kind is one of the values accepted by ValidKind.
	Kind Kind `json:"kind"`
	// OccurredAt is the event time (drives TTL aging, §3.1).
	OccurredAt time.Time `json:"occurred_at"`
	// Actor is agent|user|system.
	Actor Actor `json:"actor"`
	// Text is the body — the nori full-text indexing target.
	Text string `json:"text"`
	// Entities are normalized entity names mentioned by this record.
	Entities []string `json:"entities"`
	// Refs is set only for kind=document_chunk.
	Refs *Refs `json:"refs,omitempty"`
	// Consolidated marks the record as distilled into knowledge; a
	// precondition for cold archival (§3). Never inferred by the server: it is
	// set only where the agent states the distillation itself, by naming this
	// record in the provenance of a POST .../knowledge/nodes (§3, §0
	// principle 2).
	Consolidated bool `json:"consolidated"`
	// RecallCount / LastRecalled track search hits. LastRecalled is RFC3339
	// or "" when never recalled.
	RecallCount  int    `json:"recall_count"`
	LastRecalled string `json:"last_recalled"`
}

// Validate checks structural invariants: ULID id, known kind and actor,
// non-empty text, non-zero occurred_at, refs present iff kind=document_chunk.
// It returns the first violation as an errs.KindInvalid error.
//
// Kept alongside the transport-layer checks in internal/server on purpose, for
// the same reason as knowledge.Node.Validate: the spec
// (docs/spec/09-code-structure.md §6.1) records the two as duplicated
// validation to be merged handler-side, not as dead code to drop. It is also
// strictly the wider check — the server-minted id, occurred_at, recall_count
// and last_recalled invariants exist only here.
func (r Record) Validate() error {
	if !ulid.Valid(r.ID) {
		return invalid(r.ID, fmt.Sprintf("id %q is not a ULID", r.ID))
	}
	if !ValidKind(r.Kind) {
		return invalid(r.ID, fmt.Sprintf("unknown kind %q", r.Kind))
	}
	if !ValidActor(r.Actor) {
		return invalid(r.ID, fmt.Sprintf("unknown actor %q", r.Actor))
	}
	if strings.TrimSpace(r.Text) == "" {
		return invalid(r.ID, "text must be non-empty")
	}
	if r.OccurredAt.IsZero() {
		return invalid(r.ID, "occurred_at must be set")
	}
	if err := r.validateRefs(); err != nil {
		return err
	}
	if r.RecallCount < 0 {
		return invalid(r.ID, fmt.Sprintf("recall_count must be >= 0, got %d", r.RecallCount))
	}
	if r.LastRecalled != "" {
		if _, err := time.Parse(time.RFC3339, r.LastRecalled); err != nil {
			return invalid(r.ID, fmt.Sprintf("last_recalled %q is not RFC3339", r.LastRecalled))
		}
	}
	return nil
}

// validateRefs enforces the refs-iff-document_chunk rule of §2.1.
func (r Record) validateRefs() error {
	if r.Kind != KindDocumentChunk {
		if r.Refs != nil {
			return invalid(r.ID, fmt.Sprintf("refs are only valid for kind %q, got kind %q", KindDocumentChunk, r.Kind))
		}
		return nil
	}
	if r.Refs == nil {
		return invalid(r.ID, fmt.Sprintf("kind %q requires refs", KindDocumentChunk))
	}
	if r.Refs.DocSHA == "" {
		return invalid(r.ID, "refs.doc_sha must be non-empty")
	}
	if r.Refs.ChunkSeq < 0 {
		return invalid(r.ID, fmt.Sprintf("refs.chunk_seq must be >= 0, got %d", r.Refs.ChunkSeq))
	}
	return nil
}

// invalid is the single error shape this package returns: KindInvalid with the
// offending record id attached for logs. Only msg reaches the client.
func invalid(id, msg string) error {
	e := errs.Invalid(opValidate, entityEpisode, msg)
	e.ID = id
	return e
}
