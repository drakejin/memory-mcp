package knowledge

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/errs"
)

var testNow = time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

// idSeq backs newID. Fixtures only need ids that satisfy ulid.Valid and differ
// from one another, so a counter beats real entropy: values stay stable across
// runs, which keeps failure output readable. Test-only state.
var idSeq atomic.Int64

// newID mints a unique fixture ULID. It takes no *testing.T because several
// fixtures build ids inside closures that have none.
func newID() string {
	// 4-char head + 22 digits = the 26 Crockford characters ulid.Valid wants.
	return fmt.Sprintf("01JD%022d", idSeq.Add(1))
}

func validNode(t *testing.T) Node {
	t.Helper()
	return Node{
		ID:         newID(),
		Kind:       KindFact,
		Name:       "opensearch nori 플러그인 필수",
		Body:       "episodic 한국어 검색은 nori 분석기가 필요하다",
		Aliases:    []string{"nori"},
		State:      StateActive,
		Trust:      TrustAgentInferred,
		Provenance: []string{newID()},
		Created:    testNow,
		Updated:    testNow,
	}
}

// assertDomainErr checks that err is an *errs.Error of the expected kind and
// op. Kind is compared through the sentinel, never as a string (§2.1).
func assertDomainErr(t *testing.T, err error, sentinel error, wantOp, wantEntity string) *errs.Error {
	t.Helper()
	if !errors.Is(err, sentinel) {
		t.Fatalf("want %v, got %v", sentinel, err)
	}
	var domain *errs.Error
	if !errors.As(err, &domain) {
		t.Fatalf("error is not *errs.Error: %#v", err)
	}
	if domain.Op != wantOp {
		t.Errorf("op = %q, want %q", domain.Op, wantOp)
	}
	if domain.Entity != wantEntity {
		t.Errorf("entity = %q, want %q", domain.Entity, wantEntity)
	}
	if domain.Msg == "" {
		t.Error("msg must carry a client-facing explanation")
	}
	return domain
}

func TestNodeValidate(t *testing.T) {
	base := validNode(t)
	other := newID()

	tests := []struct {
		name    string
		mutate  func(n Node) Node
		wantErr bool
	}{
		{"valid active node", func(n Node) Node { return n }, false},
		{"valid archived superseded node", func(n Node) Node {
			n.State = StateArchived
			n.SupersededBy = other
			return n
		}, false},
		{"valid deprecated without superseded_by", func(n Node) Node {
			n.State = StateDeprecated
			return n
		}, false},
		{"valid review_after RFC3339", func(n Node) Node {
			n.ReviewAfter = "2026-09-01T00:00:00Z"
			return n
		}, false},
		{"non-ULID id", func(n Node) Node { n.ID = "not-a-ulid"; return n }, true},
		{"empty id", func(n Node) Node { n.ID = ""; return n }, true},
		{"unknown kind", func(n Node) Node { n.Kind = "opinion"; return n }, true},
		{"unknown state", func(n Node) Node { n.State = "paused"; return n }, true},
		{"unknown trust", func(n Node) Node { n.Trust = "gospel"; return n }, true},
		{"blank name", func(n Node) Node { n.Name = "   "; return n }, true},
		{"active with superseded_by", func(n Node) Node { n.SupersededBy = other; return n }, true},
		{"non-ULID superseded_by", func(n Node) Node {
			n.State = StateArchived
			n.SupersededBy = "bogus"
			return n
		}, true},
		{"malformed review_after", func(n Node) Node { n.ReviewAfter = "tomorrow"; return n }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.mutate(base).Validate()
			if tt.wantErr {
				assertDomainErr(t, err, errs.ErrInvalid, opValidateNode, EntityNode)
				return
			}
			if err != nil {
				t.Fatalf("want nil, got %v", err)
			}
		})
	}
}

// TestNodeValidateFields locks the structured detail that reaches slog but must
// never reach a response body.
func TestNodeValidateFields(t *testing.T) {
	n := validNode(t)
	n.Kind = "opinion"
	domain := assertDomainErr(t, n.Validate(), errs.ErrInvalid, opValidateNode, EntityNode)
	if domain.Fields["kind"] != "opinion" {
		t.Fatalf("fields = %v, want kind=opinion", domain.Fields)
	}
}

func TestEdgeValidate(t *testing.T) {
	from, to := newID(), newID()
	base := Edge{From: from, To: to, Rel: RelRelatesTo, Confidence: 0.9}

	tests := []struct {
		name    string
		mutate  func(e Edge) Edge
		wantErr bool
	}{
		{"valid edge", func(e Edge) Edge { return e }, false},
		{"confidence bounds low ok", func(e Edge) Edge { e.Confidence = 0; return e }, false},
		{"confidence bounds high ok", func(e Edge) Edge { e.Confidence = 1; return e }, false},
		{"self relates_to allowed by shape", func(e Edge) Edge { e.To = e.From; return e }, false},
		{"non-ULID from", func(e Edge) Edge { e.From = "x"; return e }, true},
		{"non-ULID to", func(e Edge) Edge { e.To = "x"; return e }, true},
		{"unknown rel", func(e Edge) Edge { e.Rel = "blames"; return e }, true},
		{"self supersede forbidden", func(e Edge) Edge {
			e.Rel = RelSupersedes
			e.To = e.From
			return e
		}, true},
		{"confidence below zero", func(e Edge) Edge { e.Confidence = -0.1; return e }, true},
		{"confidence above one", func(e Edge) Edge { e.Confidence = 1.1; return e }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.mutate(base).Validate()
			if tt.wantErr {
				assertDomainErr(t, err, errs.ErrInvalid, opValidateEdge, EntityEdge)
				return
			}
			if err != nil {
				t.Fatalf("want nil, got %v", err)
			}
		})
	}
}
