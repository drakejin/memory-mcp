package episode

import (
	"errors"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/x/errs"
)

func validRecord() Record {
	return Record{
		ID:         "01JD0000000000000000000000",
		Kind:       KindEvent,
		OccurredAt: time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC),
		Actor:      ActorAgent,
		Text:       "보안을 끄고 배포했다",
		Entities:   []string{"memory-mcp"},
	}
}

func TestRecordValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(r Record) Record
		wantErr bool
	}{
		{
			name:   "valid event",
			mutate: func(r Record) Record { return r },
		},
		{
			name: "valid document_chunk with refs",
			mutate: func(r Record) Record {
				r.Kind = KindDocumentChunk
				r.Refs = &Refs{DocSHA: "abc123", ChunkSeq: 0}
				return r
			},
		},
		{
			name: "valid with recall stats",
			mutate: func(r Record) Record {
				r.RecallCount = 3
				r.LastRecalled = "2026-08-25T02:00:00Z"
				return r
			},
		},
		{
			name:    "empty id",
			mutate:  func(r Record) Record { r.ID = ""; return r },
			wantErr: true,
		},
		{
			name:    "malformed ulid",
			mutate:  func(r Record) Record { r.ID = "not-a-ulid"; return r },
			wantErr: true,
		},
		{
			name:    "lowercase ulid rejected",
			mutate:  func(r Record) Record { r.ID = "01jd0000000000000000000000"; return r },
			wantErr: true,
		},
		{
			name:    "unknown kind",
			mutate:  func(r Record) Record { r.Kind = "note"; return r },
			wantErr: true,
		},
		{
			name:    "empty kind",
			mutate:  func(r Record) Record { r.Kind = ""; return r },
			wantErr: true,
		},
		{
			name:    "unknown actor",
			mutate:  func(r Record) Record { r.Actor = "robot"; return r },
			wantErr: true,
		},
		{
			name:    "empty actor",
			mutate:  func(r Record) Record { r.Actor = ""; return r },
			wantErr: true,
		},
		{
			name:    "empty text",
			mutate:  func(r Record) Record { r.Text = ""; return r },
			wantErr: true,
		},
		{
			name:    "whitespace-only text",
			mutate:  func(r Record) Record { r.Text = "  \n\t "; return r },
			wantErr: true,
		},
		{
			name:    "zero occurred_at",
			mutate:  func(r Record) Record { r.OccurredAt = time.Time{}; return r },
			wantErr: true,
		},
		{
			name: "document_chunk without refs",
			mutate: func(r Record) Record {
				r.Kind = KindDocumentChunk
				return r
			},
			wantErr: true,
		},
		{
			name: "document_chunk with empty doc_sha",
			mutate: func(r Record) Record {
				r.Kind = KindDocumentChunk
				r.Refs = &Refs{DocSHA: "", ChunkSeq: 1}
				return r
			},
			wantErr: true,
		},
		{
			name: "document_chunk with negative chunk_seq",
			mutate: func(r Record) Record {
				r.Kind = KindDocumentChunk
				r.Refs = &Refs{DocSHA: "abc", ChunkSeq: -1}
				return r
			},
			wantErr: true,
		},
		{
			name: "refs on non-chunk kind",
			mutate: func(r Record) Record {
				r.Refs = &Refs{DocSHA: "abc", ChunkSeq: 0}
				return r
			},
			wantErr: true,
		},
		{
			name:    "negative recall_count",
			mutate:  func(r Record) Record { r.RecallCount = -1; return r },
			wantErr: true,
		},
		{
			name:    "malformed last_recalled",
			mutate:  func(r Record) Record { r.LastRecalled = "yesterday"; return r },
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			rec := tt.mutate(validRecord())

			// Act
			err := rec.Validate()

			// Assert
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error")
			}
			if !errors.Is(err, errs.ErrInvalid) {
				t.Fatalf("Validate() error %v is not errs.ErrInvalid", err)
			}
			for _, other := range []error{errs.ErrNotFound, errs.ErrConflict, errs.ErrUnavailable, errs.ErrInternal} {
				if errors.Is(err, other) {
					t.Fatalf("Validate() error %v also matches %v; kinds must be exclusive", err, other)
				}
			}
		})
	}
}

// TestRecordValidateErrorShape pins the semantic envelope the transport layer
// depends on: op, entity and the offending id travel with the error, while the
// public message stays free of a stack or a wrapping prefix.
func TestRecordValidateErrorShape(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(r Record) Record
		wantID  string
		wantMsg string
	}{
		{
			name:    "bad id reports the value",
			mutate:  func(r Record) Record { r.ID = "not-a-ulid"; return r },
			wantID:  "not-a-ulid",
			wantMsg: `id "not-a-ulid" is not a ULID`,
		},
		{
			name:    "empty text keeps the record id",
			mutate:  func(r Record) Record { r.Text = ""; return r },
			wantID:  "01JD0000000000000000000000",
			wantMsg: "text must be non-empty",
		},
		{
			name: "missing refs names the kind",
			mutate: func(r Record) Record {
				r.Kind = KindDocumentChunk
				return r
			},
			wantID:  "01JD0000000000000000000000",
			wantMsg: `kind "document_chunk" requires refs`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			rec := tt.mutate(validRecord())

			// Act
			err := rec.Validate()

			// Assert
			var domain *errs.Error
			if !errors.As(err, &domain) {
				t.Fatalf("Validate() = %v, want *errs.Error", err)
			}
			if domain.Kind != errs.KindInvalid {
				t.Errorf("Kind = %q, want %q", domain.Kind, errs.KindInvalid)
			}
			if domain.Op != opValidate {
				t.Errorf("Op = %q, want %q", domain.Op, opValidate)
			}
			if domain.Entity != entityEpisode {
				t.Errorf("Entity = %q, want %q", domain.Entity, entityEpisode)
			}
			if domain.ID != tt.wantID {
				t.Errorf("ID = %q, want %q", domain.ID, tt.wantID)
			}
			if domain.Msg != tt.wantMsg {
				t.Errorf("Msg = %q, want %q", domain.Msg, tt.wantMsg)
			}
		})
	}
}

// TestVocabularyMatchesSpec guards the §2.1 enums the HTTP boundary validates
// against; a silent addition here would let an unknown kind reach the index.
// It drives ValidKind/ValidActor — the gate itself — so both the accepted set
// and its complement are pinned.
func TestVocabularyMatchesSpec(t *testing.T) {
	kinds := []struct {
		kind Kind
		want bool
	}{
		{KindEvent, true},
		{KindConversation, true},
		{KindDecision, true},
		{KindObservation, true},
		{KindDocumentChunk, true},
		{Kind("rumor"), false},
		{Kind(""), false},
		{Kind("Event"), false},
		{Kind("document-chunk"), false},
	}
	for _, tt := range kinds {
		t.Run("kind/"+string(tt.kind), func(t *testing.T) {
			if got := ValidKind(tt.kind); got != tt.want {
				t.Errorf("ValidKind(%q) = %v, want %v", tt.kind, got, tt.want)
			}
		})
	}

	actors := []struct {
		actor Actor
		want  bool
	}{
		{ActorAgent, true},
		{ActorUser, true},
		{ActorSystem, true},
		{Actor("robot"), false},
		{Actor(""), false},
		{Actor("Agent"), false},
	}
	for _, tt := range actors {
		t.Run("actor/"+string(tt.actor), func(t *testing.T) {
			if got := ValidActor(tt.actor); got != tt.want {
				t.Errorf("ValidActor(%q) = %v, want %v", tt.actor, got, tt.want)
			}
		})
	}
}
