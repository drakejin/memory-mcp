package knowledge

import (
	"errors"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/ulid"
)

var testNow = time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

func validNode(t *testing.T) Node {
	t.Helper()
	return Node{
		ID:         ulid.New(),
		Kind:       KindFact,
		Name:       "opensearch nori 플러그인 필수",
		Body:       "episodic 한국어 검색은 nori 분석기가 필요하다",
		Aliases:    []string{"nori"},
		State:      StateActive,
		Trust:      TrustAgentInferred,
		Provenance: []string{ulid.New()},
		Created:    testNow,
		Updated:    testNow,
	}
}

func TestNodeValidate(t *testing.T) {
	base := validNode(t)
	other := ulid.New()

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
				if !errors.Is(err, ErrInvalidNode) {
					t.Fatalf("want ErrInvalidNode, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("want nil, got %v", err)
			}
		})
	}
}

func TestEdgeValidate(t *testing.T) {
	from, to := ulid.New(), ulid.New()
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
				if !errors.Is(err, ErrInvalidEdge) {
					t.Fatalf("want ErrInvalidEdge, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("want nil, got %v", err)
			}
		})
	}
}

// TestTransition encodes the v1 state machine from src/lifecycle.ts:
// active→[archived,deprecated], archived→[active,deprecated],
// deprecated→[active]; same-state is a no-op; deprecation requires a reason.
func TestTransition(t *testing.T) {
	tests := []struct {
		name    string
		from    State
		to      State
		reason  string
		wantErr bool
	}{
		{"active to archived", StateActive, StateArchived, "", false},
		{"active to deprecated with reason", StateActive, StateDeprecated, "판단이 틀렸음", false},
		{"archived to active (revive)", StateArchived, StateActive, "", false},
		{"archived to deprecated with reason", StateArchived, StateDeprecated, "wrong", false},
		{"deprecated to active (revive)", StateDeprecated, StateActive, "", false},
		{"same state no-op", StateArchived, StateArchived, "", false},
		{"deprecated to archived illegal", StateDeprecated, StateArchived, "", true},
		{"deprecate without reason", StateActive, StateDeprecated, "", true},
		{"deprecate with blank reason", StateActive, StateDeprecated, "   ", true},
		{"unknown target", StateActive, State("purged"), "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := validNode(t)
			n.State = tt.from
			if tt.from != StateActive {
				n.SupersededBy = ulid.New()
			}
			got, err := Transition(n, tt.to, tt.reason, testNow.Add(time.Hour))
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidTransition) {
					t.Fatalf("want ErrInvalidTransition, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.State != tt.to {
				t.Fatalf("state = %s, want %s", got.State, tt.to)
			}
			if !got.Updated.Equal(testNow.Add(time.Hour)) {
				t.Fatalf("updated not bumped: %v", got.Updated)
			}
			if tt.to == StateActive && got.SupersededBy != "" {
				t.Fatalf("revive must clear superseded_by, got %q", got.SupersededBy)
			}
		})
	}
}

func TestTransitionDoesNotMutateInput(t *testing.T) {
	n := validNode(t)
	if _, err := Transition(n, StateArchived, "", testNow.Add(time.Hour)); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if n.State != StateActive || !n.Updated.Equal(testNow) {
		t.Fatalf("input node mutated: %+v", n)
	}
}

func TestCanPurge(t *testing.T) {
	tests := []struct {
		state State
		want  bool
	}{
		{StateActive, false},
		{StateArchived, true},
		{StateDeprecated, true},
		{State("bogus"), false},
	}
	for _, tt := range tests {
		if got := CanPurge(tt.state); got != tt.want {
			t.Errorf("CanPurge(%s) = %v, want %v", tt.state, got, tt.want)
		}
	}
}

func TestSupersede(t *testing.T) {
	old1 := validNode(t)
	old2 := validNode(t)
	old2.State = StateDeprecated
	g := Graph{Nodes: []Node{old1, old2}}

	winner := validNode(t)
	later := testNow.Add(2 * time.Hour)

	got, err := Supersede(g, winner, []string{old1.ID, old2.ID, old1.ID}, later)
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}

	if len(got.Nodes) != 3 {
		t.Fatalf("nodes = %d, want 3", len(got.Nodes))
	}
	w, err := FindNode(got, winner.ID)
	if err != nil {
		t.Fatalf("winner missing: %v", err)
	}
	if w.State != StateActive {
		t.Errorf("winner state = %s, want active", w.State)
	}
	if len(w.Supersedes) != 2 || w.Supersedes[0] != old1.ID || w.Supersedes[1] != old2.ID {
		t.Errorf("winner supersedes = %v, want deduped [%s %s]", w.Supersedes, old1.ID, old2.ID)
	}

	l1, _ := FindNode(got, old1.ID)
	if l1.State != StateArchived || l1.SupersededBy != winner.ID {
		t.Errorf("loser1 = state %s superseded_by %s, want archived/%s", l1.State, l1.SupersededBy, winner.ID)
	}
	if !l1.Updated.Equal(later) {
		t.Errorf("loser1 updated = %v, want %v", l1.Updated, later)
	}
	// v1 applySupersede only archives active losers; deprecated stays put.
	l2, _ := FindNode(got, old2.ID)
	if l2.State != StateDeprecated || l2.SupersededBy != winner.ID {
		t.Errorf("loser2 = state %s superseded_by %s, want deprecated/%s", l2.State, l2.SupersededBy, winner.ID)
	}

	if len(got.Edges) != 2 {
		t.Fatalf("edges = %d, want 2 supersede edges", len(got.Edges))
	}
	for i, target := range []string{old1.ID, old2.ID} {
		e := got.Edges[i]
		if e.From != winner.ID || e.To != target || e.Rel != RelSupersedes {
			t.Errorf("edge[%d] = %+v, want %s -supersedes-> %s", i, e, winner.ID, target)
		}
	}

	// Input graph must be untouched.
	if g.Nodes[0].SupersededBy != "" || g.Nodes[0].State != StateActive {
		t.Errorf("input graph mutated: %+v", g.Nodes[0])
	}
	if len(g.Edges) != 0 {
		t.Errorf("input edges mutated: %v", g.Edges)
	}
}

func TestSupersedeAppendOnlyWhenNoTargets(t *testing.T) {
	n := validNode(t)
	got, err := Supersede(Graph{}, n, nil, testNow)
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if len(got.Nodes) != 1 || len(got.Edges) != 0 {
		t.Fatalf("got %d nodes %d edges, want 1/0", len(got.Nodes), len(got.Edges))
	}
	if got.Nodes[0].State != StateActive {
		t.Fatalf("state = %s, want active", got.Nodes[0].State)
	}
}

func TestSupersedeErrors(t *testing.T) {
	existing := validNode(t)
	g := Graph{Nodes: []Node{existing}}

	t.Run("missing target", func(t *testing.T) {
		_, err := Supersede(g, validNode(t), []string{ulid.New()}, testNow)
		if !errors.Is(err, ErrNodeNotFound) {
			t.Fatalf("want ErrNodeNotFound, got %v", err)
		}
	})
	t.Run("self supersede", func(t *testing.T) {
		n := validNode(t)
		_, err := Supersede(g, n, []string{n.ID}, testNow)
		if !errors.Is(err, ErrInvalidNode) {
			t.Fatalf("want ErrInvalidNode, got %v", err)
		}
	})
	t.Run("duplicate node id", func(t *testing.T) {
		_, err := Supersede(g, existing, nil, testNow)
		if !errors.Is(err, ErrInvalidNode) {
			t.Fatalf("want ErrInvalidNode, got %v", err)
		}
	})
}

func TestSupersedeSetsCreatedWhenZero(t *testing.T) {
	n := validNode(t)
	n.Created = time.Time{}
	got, err := Supersede(Graph{}, n, nil, testNow)
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if !got.Nodes[0].Created.Equal(testNow) {
		t.Fatalf("created = %v, want %v", got.Nodes[0].Created, testNow)
	}
}

func TestFindNode(t *testing.T) {
	n := validNode(t)
	g := Graph{Nodes: []Node{n}}

	got, err := FindNode(g, n.ID)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got.ID != n.ID {
		t.Fatalf("got %s, want %s", got.ID, n.ID)
	}
	if _, err := FindNode(g, ulid.New()); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("want ErrNodeNotFound, got %v", err)
	}
}
