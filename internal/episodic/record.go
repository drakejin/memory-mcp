// Package episodic defines the episodic memory plane domain: append-only event
// records indexed in OpenSearch (architecture-v2.md §2, §2.1). Records are
// never edited; corrections are new records. IDs are ULIDs and immutable, so
// knowledge provenance links survive cold archival.
package episodic

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/drakejin/memory-mcp/internal/ulid"
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

// Kinds lists every valid Kind, in spec order.
var Kinds = []Kind{KindEvent, KindConversation, KindDecision, KindObservation, KindDocumentChunk}

// Actor identifies who produced the record (§2.1).
type Actor string

const (
	ActorAgent  Actor = "agent"
	ActorUser   Actor = "user"
	ActorSystem Actor = "system"
)

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
	// Kind is one of Kinds.
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
	// precondition for cold archival (§3). Never auto-set by the server.
	Consolidated bool `json:"consolidated"`
	// RecallCount / LastRecalled track search hits. LastRecalled is RFC3339
	// or "" when never recalled.
	RecallCount  int    `json:"recall_count"`
	LastRecalled string `json:"last_recalled"`
}

// ErrInvalidRecord wraps all validation failures from Validate.
var ErrInvalidRecord = errors.New("episodic: invalid record")

// Actors lists every valid Actor.
var Actors = []Actor{ActorAgent, ActorUser, ActorSystem}

// Validate checks structural invariants: ULID id, known kind and actor,
// non-empty text, non-zero occurred_at, refs present iff kind=document_chunk.
// Returns an error wrapping ErrInvalidRecord describing the first violation.
func (r Record) Validate() error {
	if !ulid.IsULID(r.ID) {
		return fmt.Errorf("%w: id %q is not a ULID", ErrInvalidRecord, r.ID)
	}
	if !slices.Contains(Kinds, r.Kind) {
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidRecord, r.Kind)
	}
	if !slices.Contains(Actors, r.Actor) {
		return fmt.Errorf("%w: unknown actor %q", ErrInvalidRecord, r.Actor)
	}
	if strings.TrimSpace(r.Text) == "" {
		return fmt.Errorf("%w: text must be non-empty", ErrInvalidRecord)
	}
	if r.OccurredAt.IsZero() {
		return fmt.Errorf("%w: occurred_at must be set", ErrInvalidRecord)
	}
	if r.Kind == KindDocumentChunk {
		if r.Refs == nil {
			return fmt.Errorf("%w: kind %q requires refs", ErrInvalidRecord, KindDocumentChunk)
		}
		if r.Refs.DocSHA == "" {
			return fmt.Errorf("%w: refs.doc_sha must be non-empty", ErrInvalidRecord)
		}
		if r.Refs.ChunkSeq < 0 {
			return fmt.Errorf("%w: refs.chunk_seq must be >= 0, got %d", ErrInvalidRecord, r.Refs.ChunkSeq)
		}
	} else if r.Refs != nil {
		return fmt.Errorf("%w: refs are only valid for kind %q, got kind %q", ErrInvalidRecord, KindDocumentChunk, r.Kind)
	}
	if r.RecallCount < 0 {
		return fmt.Errorf("%w: recall_count must be >= 0, got %d", ErrInvalidRecord, r.RecallCount)
	}
	if r.LastRecalled != "" {
		if _, err := time.Parse(time.RFC3339, r.LastRecalled); err != nil {
			return fmt.Errorf("%w: last_recalled %q is not RFC3339", ErrInvalidRecord, r.LastRecalled)
		}
	}
	return nil
}

// NewRecord builds an unconsolidated Record with a fresh ULID at now, applying
// defaults (empty entities slice, zero recall stats). It does not validate.
func NewRecord(kind Kind, actor Actor, text string, entities []string, occurredAt time.Time) Record {
	if entities == nil {
		entities = []string{}
	}
	return Record{
		ID:           ulid.New(),
		Kind:         kind,
		Actor:        actor,
		Text:         text,
		Entities:     entities,
		OccurredAt:   occurredAt,
		Consolidated: false,
		RecallCount:  0,
		LastRecalled: "",
	}
}
