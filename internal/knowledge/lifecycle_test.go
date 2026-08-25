package knowledge

// State-machine and non-destructive-revision behaviour: Transition, CanPurge,
// Supersede and the node lookup they share.

import (
	"errors"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/errs"
)

// TestTransition encodes the v1 state machine from src/lifecycle.ts:
// active→[archived,deprecated], archived→[active,deprecated],
// deprecated→[active]; same-state is a no-op; deprecation requires a reason.
// Every rejection is a conflict (409 at the transport boundary).
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
				n.SupersededBy = newID()
			}
			got, err := Transition(n, tt.to, tt.reason, testNow.Add(time.Hour))
			if tt.wantErr {
				domain := assertDomainErr(t, err, errs.ErrConflict, opTransition, EntityNode)
				if domain.ID != n.ID {
					t.Errorf("id = %q, want %q", domain.ID, n.ID)
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

// TestTransitionIsNotInvalid guards the mapping: a refused transition must not
// look like bad input, or the transport layer would answer 400 instead of 409.
func TestTransitionIsNotInvalid(t *testing.T) {
	n := validNode(t)
	n.State = StateDeprecated
	_, err := Transition(n, StateArchived, "", testNow)
	if errors.Is(err, errs.ErrInvalid) || errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("transition refusal must be a conflict only, got %v", err)
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

func TestAllowedTargetsUnknownState(t *testing.T) {
	if got := allowedTargets(State("bogus")); got != nil {
		t.Fatalf("allowedTargets(bogus) = %v, want nil", got)
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
		if e.Confidence != supersedeConfidence {
			t.Errorf("edge[%d] confidence = %v, want %v", i, e.Confidence, supersedeConfidence)
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

func TestSupersedeKeepsExistingEdge(t *testing.T) {
	old := validNode(t)
	winner := validNode(t)
	existing := Edge{From: winner.ID, To: old.ID, Rel: RelSupersedes, Confidence: 0.5}
	g := Graph{Nodes: []Node{old}, Edges: []Edge{existing}}

	got, err := Supersede(g, winner, []string{old.ID}, testNow)
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if len(got.Edges) != 1 {
		t.Fatalf("edges = %d, want 1 (no duplicate)", len(got.Edges))
	}
	if got.Edges[0].Confidence != 0.5 {
		t.Fatalf("existing edge overwritten: %+v", got.Edges[0])
	}
}

// TestSupersedeErrors pins the Kind of each refusal — the transport layer maps
// them to 404 / 409 / 400 respectively.
func TestSupersedeErrors(t *testing.T) {
	existing := validNode(t)
	g := Graph{Nodes: []Node{existing}}

	tests := []struct {
		name     string
		newNode  func(t *testing.T) Node
		targets  func(n Node) []string
		sentinel error
	}{
		{
			name:     "missing target is not found",
			newNode:  validNode,
			targets:  func(Node) []string { return []string{newID()} },
			sentinel: errs.ErrNotFound,
		},
		{
			name:     "self supersede is invalid",
			newNode:  validNode,
			targets:  func(n Node) []string { return []string{n.ID} },
			sentinel: errs.ErrInvalid,
		},
		{
			name:     "duplicate node id is a conflict",
			newNode:  func(*testing.T) Node { return existing },
			targets:  func(Node) []string { return nil },
			sentinel: errs.ErrConflict,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := tt.newNode(t)
			_, err := Supersede(g, n, tt.targets(n), testNow)
			assertDomainErr(t, err, tt.sentinel, opSupersede, EntityNode)
		})
	}
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

	missing := newID()
	_, err = FindNode(g, missing)
	domain := assertDomainErr(t, err, errs.ErrNotFound, opFindNode, EntityNode)
	if domain.ID != missing {
		t.Fatalf("id = %q, want %q", domain.ID, missing)
	}
}

// TestPurgeRequiresBuffer is the lifecycle gate CanPurge describes: §3 places
// the archived/deprecated buffer in front of every deletion, so an active node
// cannot be destroyed directly no matter how the caller confirms.
func TestPurgeRequiresBuffer(t *testing.T) {
	tests := []struct {
		state   State
		wantErr error
	}{
		{StateActive, errs.ErrConflict},
		{StateArchived, nil},
		{StateDeprecated, nil},
	}
	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			// Arrange
			n := validNode(t)
			n.State = tt.state
			g := Graph{Nodes: []Node{n}}

			// Act
			got, removed, err := Purge(g, n.ID)

			// Assert
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Purge(%s) error = %v, want %v", tt.state, err, tt.wantErr)
				}
				if len(g.Nodes) != 1 {
					t.Errorf("input graph mutated by a refused purge: %+v", g.Nodes)
				}
				return
			}
			if err != nil {
				t.Fatalf("Purge(%s) = %v, want nil", tt.state, err)
			}
			if len(got.Nodes) != 0 {
				t.Errorf("nodes = %d, want 0", len(got.Nodes))
			}
			if removed != 0 {
				t.Errorf("removed edges = %d, want 0", removed)
			}
		})
	}
}

func TestPurgeMissingNode(t *testing.T) {
	if _, _, err := Purge(Graph{}, newID()); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("Purge(absent) = %v, want ErrNotFound", err)
	}
}

// A purge drops incident edges and repairs every scalar reference, so no node
// is left pointing at an id the graph no longer holds.
func TestPurgeDropsEdgesAndRepairsReferences(t *testing.T) {
	// Arrange: older <- purged <- newer, plus one unrelated edge that must stay.
	older, purged, newer, other := validNode(t), validNode(t), validNode(t), validNode(t)
	purged.State = StateArchived
	older.State = StateArchived
	older.SupersededBy = purged.ID
	purged.Supersedes = []string{older.ID}
	purged.SupersededBy = newer.ID
	newer.Supersedes = []string{purged.ID, other.ID}

	g := Graph{
		Nodes: []Node{older, purged, newer, other},
		Edges: []Edge{
			{From: purged.ID, To: older.ID, Rel: RelSupersedes, Confidence: 1},
			{From: newer.ID, To: purged.ID, Rel: RelSupersedes, Confidence: 1},
			{From: newer.ID, To: other.ID, Rel: RelRelatesTo, Confidence: 1},
		},
	}

	// Act
	got, removed, err := Purge(g, purged.ID)

	// Assert
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed edges = %d, want 2", removed)
	}
	if len(got.Edges) != 1 || got.Edges[0].Rel != RelRelatesTo {
		t.Errorf("edges = %+v, want only the unrelated relates_to", got.Edges)
	}
	gotOlder, err := FindNode(got, older.ID)
	if err != nil {
		t.Fatalf("older node vanished: %v", err)
	}
	if gotOlder.SupersededBy != "" {
		t.Errorf("older.SupersededBy = %q, want cleared", gotOlder.SupersededBy)
	}
	gotNewer, err := FindNode(got, newer.ID)
	if err != nil {
		t.Fatalf("newer node vanished: %v", err)
	}
	if len(gotNewer.Supersedes) != 1 || gotNewer.Supersedes[0] != other.ID {
		t.Errorf("newer.Supersedes = %v, want only %s", gotNewer.Supersedes, other.ID)
	}

	// The input graph is never touched.
	if g.Nodes[0].SupersededBy != purged.ID || len(g.Nodes[2].Supersedes) != 2 || len(g.Edges) != 3 {
		t.Errorf("Purge mutated its input graph: %+v", g)
	}
}
