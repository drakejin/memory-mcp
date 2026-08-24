package episodic

import (
	"errors"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/ulid"
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
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Validate() = nil, want error")
				}
				if !errors.Is(err, ErrInvalidRecord) {
					t.Fatalf("Validate() error %v does not wrap ErrInvalidRecord", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestNewRecordDefaults(t *testing.T) {
	// Arrange
	occurred := time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC)

	// Act
	rec := NewRecord(KindDecision, ActorUser, "결정 사항", nil, occurred)

	// Assert
	if !ulid.IsULID(rec.ID) {
		t.Fatalf("NewRecord id %q is not a ULID", rec.ID)
	}
	if rec.Kind != KindDecision || rec.Actor != ActorUser || rec.Text != "결정 사항" {
		t.Fatalf("NewRecord did not carry inputs: %+v", rec)
	}
	if !rec.OccurredAt.Equal(occurred) {
		t.Fatalf("NewRecord occurred_at = %v, want %v", rec.OccurredAt, occurred)
	}
	if rec.Entities == nil || len(rec.Entities) != 0 {
		t.Fatalf("NewRecord entities = %#v, want empty non-nil slice", rec.Entities)
	}
	if rec.Consolidated {
		t.Fatal("NewRecord must start unconsolidated")
	}
	if rec.RecallCount != 0 || rec.LastRecalled != "" {
		t.Fatalf("NewRecord recall stats must be zero, got %d %q", rec.RecallCount, rec.LastRecalled)
	}
	if rec.Refs != nil {
		t.Fatalf("NewRecord refs = %+v, want nil", rec.Refs)
	}
	if err := rec.Validate(); err != nil {
		t.Fatalf("NewRecord output should validate, got %v", err)
	}
}

func TestNewRecordIDsAreMonotonic(t *testing.T) {
	// Arrange / Act
	a := NewRecord(KindEvent, ActorAgent, "a", nil, time.Now())
	b := NewRecord(KindEvent, ActorAgent, "b", nil, time.Now())

	// Assert
	if a.ID >= b.ID {
		t.Fatalf("expected strictly increasing ULIDs, got %q then %q", a.ID, b.ID)
	}
}

func TestNewRecordCopiesEntitiesReference(t *testing.T) {
	// Arrange
	ents := []string{"opensearch"}

	// Act
	rec := NewRecord(KindObservation, ActorSystem, "관찰", ents, time.Now())

	// Assert
	if len(rec.Entities) != 1 || rec.Entities[0] != "opensearch" {
		t.Fatalf("entities not carried: %#v", rec.Entities)
	}
}
