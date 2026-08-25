package knowledge

// Service-level node orchestration: create-with-supersede write order (hot
// first, mirror best-effort), the full P5 transition matrix through PatchNode,
// and the purge gates plus reference repair through PurgeNode.

import (
	"context"
	"errors"
	"testing"

	"github.com/drakejin/memory-mcp/internal/x/errs"
)

// validCreateInput is a creation the §2.2 gate accepts; aliases carry the
// blank entries normalization must drop.
func validCreateInput() CreateNodeInput {
	return CreateNodeInput{
		Kind:       KindFact,
		Name:       "opensearch nori 플러그인 필수",
		Body:       "episodic 한국어 검색은 nori 분석기가 필요하다",
		Aliases:    []string{" nori ", ""},
		Trust:      TrustAgentInferred,
		Provenance: []string{newID()},
	}
}

func TestServiceCreateNodeValidation(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name    string
		mutate  func(in CreateNodeInput) CreateNodeInput
		wantMsg string
	}{
		{
			"unknown kind",
			func(in CreateNodeInput) CreateNodeInput { in.Kind = "opinion"; return in },
			"kind must be one of entity|fact|lesson|preference|document",
		},
		{
			"empty name",
			func(in CreateNodeInput) CreateNodeInput { in.Name = ""; return in },
			"name must not be empty",
		},
		{
			"unknown trust",
			func(in CreateNodeInput) CreateNodeInput { in.Trust = "gospel"; return in },
			"trust must be one of user-stated|agent-inferred|imported",
		},
		{
			"malformed provenance id",
			func(in CreateNodeInput) CreateNodeInput { in.Provenance = []string{"bogus"}; return in },
			`provenance contains a malformed ULID "bogus"`,
		},
		{
			"malformed supersedes id",
			func(in CreateNodeInput) CreateNodeInput { in.Supersedes = []string{"bogus"}; return in },
			`supersedes contains a malformed ULID "bogus"`,
		},
		{
			"malformed review_after",
			func(in CreateNodeInput) CreateNodeInput { in.ReviewAfter = "tomorrow"; return in },
			"review_after must be RFC3339 or empty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			r := newRig(t, Graph{}, true)

			// Act
			_, err := r.svc.CreateNode(ctx, testKey, tt.mutate(validCreateInput()))

			// Assert: the exact transport wording, and no store traffic at all.
			domain := assertDomainErr(t, err, errs.ErrInvalid, opCreateNode, EntityNode)
			if domain.Msg != tt.wantMsg {
				t.Errorf("msg = %q, want %q", domain.Msg, tt.wantMsg)
			}
			assertCalls(t, r.log)
		})
	}
}

func TestServiceCreateNodeAppends(t *testing.T) {
	// Arrange
	ctx := context.Background()
	r := newRig(t, Graph{}, true)
	in := validCreateInput()
	in.ReviewAfter = "2026-09-01T00:00:00Z"

	// Act
	res, err := r.svc.CreateNode(ctx, testKey, in)

	// Assert
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	if len(res.Degraded) != 0 {
		t.Fatalf("degraded = %v, want none", res.Degraded)
	}
	node := res.Node
	if len(r.ids.minted) != 1 || node.ID != r.ids.minted[0] {
		t.Fatalf("id = %q, want the one minted id %v", node.ID, r.ids.minted)
	}
	if len(r.ids.millis) != 1 || r.ids.millis[0] != testNow.UnixMilli() {
		t.Fatalf("id minted at %v, want clock millis %d", r.ids.millis, testNow.UnixMilli())
	}
	if node.State != StateActive || node.SupersededBy != "" {
		t.Errorf("new node = %s/%q, want active with no superseded_by", node.State, node.SupersededBy)
	}
	if !node.Created.Equal(testNow) || !node.Updated.Equal(testNow) {
		t.Errorf("timestamps = %v/%v, want clock now %v", node.Created, node.Updated, testNow)
	}
	if len(node.Aliases) != 1 || node.Aliases[0] != "nori" {
		t.Errorf("aliases = %#v, want normalized [nori]", node.Aliases)
	}
	if node.Supersedes == nil || len(node.Supersedes) != 0 {
		t.Errorf("supersedes = %#v, want empty non-nil", node.Supersedes)
	}
	if node.ReviewAfter != in.ReviewAfter {
		t.Errorf("review_after = %q, want %q", node.ReviewAfter, in.ReviewAfter)
	}

	// Hot is canonical: the node must be in the store the closure committed.
	if _, err := FindNode(r.hot.graph, node.ID); err != nil {
		t.Fatalf("node missing from hot: %v", err)
	}
	// Mirror got exactly the new node, and the write order is hot first.
	if len(r.graph.gotNodes) != 1 || len(r.graph.gotNodes[0]) != 1 || r.graph.gotNodes[0][0].ID != node.ID {
		t.Fatalf("mirrored nodes = %+v, want just %s", r.graph.gotNodes, node.ID)
	}
	assertCalls(t, r.log, callUpdateKnowledge, callUpsertNodes, callMarkIndexed)
}

// TestServiceCreateNodeSupersedeChain proves the P5 non-destructive revision
// through the service: both stores receive the chain, hot strictly first.
func TestServiceCreateNodeSupersedeChain(t *testing.T) {
	// Arrange
	ctx := context.Background()
	old := validNode(t)
	r := newRig(t, Graph{Nodes: []Node{old}}, true)
	in := validCreateInput()
	in.Supersedes = []string{old.ID}

	// Act
	res, err := r.svc.CreateNode(ctx, testKey, in)

	// Assert
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	winner := res.Node
	if len(winner.Supersedes) != 1 || winner.Supersedes[0] != old.ID {
		t.Fatalf("winner supersedes = %v, want [%s]", winner.Supersedes, old.ID)
	}

	// Hot: loser archived with superseded_by, supersedes edge present.
	loser, err := FindNode(r.hot.graph, old.ID)
	if err != nil {
		t.Fatalf("loser missing from hot: %v", err)
	}
	if loser.State != StateArchived || loser.SupersededBy != winner.ID {
		t.Errorf("loser = %s/%q, want archived/%s", loser.State, loser.SupersededBy, winner.ID)
	}
	if !loser.Updated.Equal(testNow) {
		t.Errorf("loser updated = %v, want %v", loser.Updated, testNow)
	}
	if len(r.hot.graph.Edges) != 1 {
		t.Fatalf("hot edges = %+v, want the one supersedes edge", r.hot.graph.Edges)
	}
	edge := r.hot.graph.Edges[0]
	if edge.From != winner.ID || edge.To != old.ID || edge.Rel != RelSupersedes {
		t.Errorf("hot edge = %+v, want %s -supersedes-> %s", edge, winner.ID, old.ID)
	}

	// Mirror: winner plus post-transition loser, and the new edge.
	if len(r.graph.gotNodes) != 1 || len(r.graph.gotNodes[0]) != 2 {
		t.Fatalf("mirrored nodes = %+v, want winner+loser", r.graph.gotNodes)
	}
	for _, n := range r.graph.gotNodes[0] {
		if n.ID == old.ID && (n.State != StateArchived || n.SupersededBy != winner.ID) {
			t.Errorf("mirrored loser = %s/%q, want post-transition form", n.State, n.SupersededBy)
		}
	}
	if len(r.graph.gotEdges) != 1 || len(r.graph.gotEdges[0]) != 1 {
		t.Fatalf("mirrored edges = %+v, want exactly the new edge", r.graph.gotEdges)
	}
	mirrored := r.graph.gotEdges[0][0]
	if mirrored.From != edge.From || mirrored.To != edge.To || mirrored.Rel != edge.Rel {
		t.Fatalf("mirrored edge = %+v, want %+v", mirrored, edge)
	}

	// The order the architecture demands: hot commit, then node MERGE, then
	// edge MERGE, then freshness bookkeeping.
	assertCalls(t, r.log, callUpdateKnowledge, callUpsertNodes, callUpsertEdges, callMarkIndexed)
}

// A create only mirrors the edges it added: edges already in hot before the
// write never re-cross to Neo4j (the MERGE payload is the diff, not the file).
func TestServiceCreateNodeMirrorsOnlyNewEdges(t *testing.T) {
	// Arrange: hot already holds an unrelated edge.
	ctx := context.Background()
	a, b := validNode(t), validNode(t)
	existing := Edge{From: a.ID, To: b.ID, Rel: RelRelatesTo, Confidence: 1}
	r := newRig(t, Graph{Nodes: []Node{a, b}, Edges: []Edge{existing}}, true)

	// Act
	if _, err := r.svc.CreateNode(ctx, testKey, validCreateInput()); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	// Assert: node upsert only, no edge traffic, existing edge intact in hot.
	assertCalls(t, r.log, callUpdateKnowledge, callUpsertNodes, callMarkIndexed)
	if len(r.hot.graph.Edges) != 1 {
		t.Errorf("hot edges = %+v, want the pre-existing edge untouched", r.hot.graph.Edges)
	}
}

func TestServiceCreateNodeMissingSupersedeTarget(t *testing.T) {
	// Arrange
	ctx := context.Background()
	old := validNode(t)
	r := newRig(t, Graph{Nodes: []Node{old}}, true)
	in := validCreateInput()
	in.Supersedes = []string{newID()}

	// Act
	_, err := r.svc.CreateNode(ctx, testKey, in)

	// Assert: domain NotFound, hot untouched, no mirror traffic.
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if len(r.hot.graph.Nodes) != 1 || len(r.hot.graph.Edges) != 0 {
		t.Errorf("hot mutated by a refused create: %+v", r.hot.graph)
	}
	assertCalls(t, r.log, callUpdateKnowledge)
}

// TestServiceCreateNodeDegraded is the §5 contract: a dead mirror never fails
// a write that reached hot — the outcome is a degraded note plus a dirty mark.
func TestServiceCreateNodeDegraded(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name string
		rig  func(t *testing.T) *rig
	}{
		{"graph not configured", func(t *testing.T) *rig {
			return newRig(t, Graph{}, false)
		}},
		{"graph node upsert fails", func(t *testing.T) *rig {
			r := newRig(t, Graph{}, true)
			r.graph.upsertNodesErr = errBoom
			return r
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			r := tt.rig(t)

			// Act
			res, err := r.svc.CreateNode(ctx, testKey, validCreateInput())

			// Assert
			if err != nil {
				t.Fatalf("CreateNode must succeed hot-side: %v", err)
			}
			if len(res.Degraded) != 1 || res.Degraded[0] != degradedGraph {
				t.Fatalf("degraded = %v, want [%q]", res.Degraded, degradedGraph)
			}
			if _, err := FindNode(r.hot.graph, res.Node.ID); err != nil {
				t.Fatalf("hot write lost: %v", err)
			}
			if r.hot.dirtyCalls != 1 || r.hot.indexedCalls != 0 {
				t.Errorf("bookkeeping = %d dirty / %d indexed, want 1/0", r.hot.dirtyCalls, r.hot.indexedCalls)
			}
		})
	}
}

// A failing edge MERGE after a successful node MERGE is still one degraded
// mirror, and the nodes already sent stay sent (MERGE is idempotent, P3).
func TestServiceCreateNodeDegradedEdgeUpsert(t *testing.T) {
	// Arrange
	ctx := context.Background()
	old := validNode(t)
	r := newRig(t, Graph{Nodes: []Node{old}}, true)
	r.graph.upsertEdgesErr = errBoom
	in := validCreateInput()
	in.Supersedes = []string{old.ID}

	// Act
	res, err := r.svc.CreateNode(ctx, testKey, in)

	// Assert
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	if len(res.Degraded) != 1 || res.Degraded[0] != degradedGraph {
		t.Fatalf("degraded = %v, want [%q]", res.Degraded, degradedGraph)
	}
	if len(r.graph.gotNodes) != 1 {
		t.Errorf("node upsert should have run before the edge failure: %+v", r.graph.gotNodes)
	}
	if r.hot.dirtyCalls != 1 {
		t.Errorf("dirty calls = %d, want 1", r.hot.dirtyCalls)
	}
}

func TestServiceCreateNodeIDGenerationFailure(t *testing.T) {
	// Arrange
	ctx := context.Background()
	r := newRig(t, Graph{}, true)
	r.ids.err = errBoom

	// Act
	_, err := r.svc.CreateNode(ctx, testKey, validCreateInput())

	// Assert: the failure propagates and nothing was written anywhere.
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
	assertCalls(t, r.log)
}

func TestServiceCreateNodeStoreFailure(t *testing.T) {
	// Arrange
	ctx := context.Background()
	r := newRig(t, Graph{}, true)
	r.hot.updateErr = errBoom

	// Act
	_, err := r.svc.CreateNode(ctx, testKey, validCreateInput())

	// Assert: a hot failure is a failed write (§0 principle 1) — no mirror, no
	// bookkeeping.
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
	assertCalls(t, r.log, callUpdateKnowledge)
}

// TestServicePatchNodeMatrix runs the full P5 transition matrix through the
// service: 6 legal moves, the illegal cross move, and every gate — exactly
// the feature-inventory P5 set (legal 6 / illegal / gates).
func TestServicePatchNodeMatrix(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name      string
		from      State
		in        PatchNodeInput
		wantState State
		sentinel  error  // nil for a legal move
		wantMsg   string // asserted when non-empty
	}{
		{"legal 1: active to archived", StateActive, PatchNodeInput{Op: OpSetState, State: StateArchived}, StateArchived, nil, ""},
		{"legal 2: active to deprecated via deprecate", StateActive, PatchNodeInput{Op: OpDeprecate, Reason: "판단이 틀렸음"}, StateDeprecated, nil, ""},
		{"legal 3: archived revives to active", StateArchived, PatchNodeInput{Op: OpSetState, State: StateActive}, StateActive, nil, ""},
		{"legal 4: archived to deprecated via set_state", StateArchived, PatchNodeInput{Op: OpSetState, State: StateDeprecated, Reason: "wrong"}, StateDeprecated, nil, ""},
		{"legal 5: deprecated revives to active", StateDeprecated, PatchNodeInput{Op: OpSetState, State: StateActive}, StateActive, nil, ""},
		{"legal 6: same-state no-op", StateArchived, PatchNodeInput{Op: OpSetState, State: StateArchived}, StateArchived, nil, ""},
		{"illegal: deprecated to archived", StateDeprecated, PatchNodeInput{Op: OpSetState, State: StateArchived}, "", errs.ErrConflict, ""},
		{"gate: blank reason reaches the state machine", StateActive, PatchNodeInput{Op: OpDeprecate, Reason: "   "}, "", errs.ErrConflict, ""},
		{"gate: unknown op", StateActive, PatchNodeInput{Op: "delete"}, "", errs.ErrInvalid, "op must be set_state or deprecate"},
		{"gate: unknown state", StateActive, PatchNodeInput{Op: OpSetState, State: "purged"}, "", errs.ErrInvalid, "state must be one of active|archived|deprecated"},
		{"gate: deprecate without reason", StateActive, PatchNodeInput{Op: OpDeprecate}, "", errs.ErrInvalid, "deprecation requires a reason"},
		{"gate: set_state deprecated without reason", StateActive, PatchNodeInput{Op: OpSetState, State: StateDeprecated}, "", errs.ErrInvalid, "deprecation requires a reason"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			n := validNode(t)
			n.State = tt.from
			if tt.from != StateActive {
				n.SupersededBy = newID()
			}
			r := newRig(t, Graph{Nodes: []Node{n}}, true)

			// Act
			res, err := r.svc.PatchNode(ctx, testKey, n.ID, tt.in)

			// Assert
			if tt.sentinel != nil {
				if !errors.Is(err, tt.sentinel) {
					t.Fatalf("err = %v, want %v", err, tt.sentinel)
				}
				if tt.wantMsg != "" {
					var domain *errs.Error
					if !errors.As(err, &domain) {
						t.Fatalf("error is not *errs.Error: %#v", err)
					}
					if domain.Msg != tt.wantMsg {
						t.Errorf("msg = %q, want %q", domain.Msg, tt.wantMsg)
					}
				}
				got, findErr := FindNode(r.hot.graph, n.ID)
				if findErr != nil || got.State != tt.from {
					t.Errorf("hot state = %v/%v, want untouched %s", got.State, findErr, tt.from)
				}
				return
			}
			if err != nil {
				t.Fatalf("PatchNode: %v", err)
			}
			if res.Node.State != tt.wantState {
				t.Fatalf("state = %s, want %s", res.Node.State, tt.wantState)
			}
			if !res.Node.Updated.Equal(testNow) {
				t.Errorf("updated = %v, want clock now", res.Node.Updated)
			}
			if tt.wantState == StateActive && res.Node.SupersededBy != "" {
				t.Errorf("revive must clear superseded_by, got %q", res.Node.SupersededBy)
			}
			stored, err := FindNode(r.hot.graph, n.ID)
			if err != nil || stored.State != tt.wantState {
				t.Errorf("hot state = %v/%v, want %s", stored.State, err, tt.wantState)
			}
			// Mirror got exactly the updated node, after hot.
			if len(r.graph.gotNodes) != 1 || len(r.graph.gotNodes[0]) != 1 || r.graph.gotNodes[0][0].State != tt.wantState {
				t.Errorf("mirrored nodes = %+v, want the updated node", r.graph.gotNodes)
			}
			assertCalls(t, r.log, callUpdateKnowledge, callUpsertNodes, callMarkIndexed)
		})
	}
}

func TestServicePatchNodeInputGates(t *testing.T) {
	ctx := context.Background()

	t.Run("malformed id", func(t *testing.T) {
		r := newRig(t, Graph{}, true)
		_, err := r.svc.PatchNode(ctx, testKey, "not-a-ulid", PatchNodeInput{Op: OpSetState, State: StateArchived})
		domain := assertDomainErr(t, err, errs.ErrInvalid, opPatchNode, EntityNode)
		if domain.Msg != msgIDMustBeULID {
			t.Errorf("msg = %q, want %q", domain.Msg, msgIDMustBeULID)
		}
		assertCalls(t, r.log)
	})

	t.Run("missing node keeps the transport wording", func(t *testing.T) {
		r := newRig(t, Graph{}, true)
		missing := newID()
		_, err := r.svc.PatchNode(ctx, testKey, missing, PatchNodeInput{Op: OpSetState, State: StateArchived})
		domain := assertDomainErr(t, err, errs.ErrNotFound, opPatchNode, EntityNode)
		if domain.Msg != msgNodeNotFound {
			t.Errorf("msg = %q, want %q", domain.Msg, msgNodeNotFound)
		}
		if domain.ID != missing {
			t.Errorf("id = %q, want %q", domain.ID, missing)
		}
		assertCalls(t, r.log, callUpdateKnowledge)
	})
}

func TestServicePatchNodeDegraded(t *testing.T) {
	// Arrange
	ctx := context.Background()
	n := validNode(t)
	r := newRig(t, Graph{Nodes: []Node{n}}, true)
	r.graph.upsertNodesErr = errBoom

	// Act
	res, err := r.svc.PatchNode(ctx, testKey, n.ID, PatchNodeInput{Op: OpSetState, State: StateArchived})

	// Assert: hot transitioned, mirror degraded.
	if err != nil {
		t.Fatalf("PatchNode: %v", err)
	}
	if len(res.Degraded) != 1 || res.Degraded[0] != degradedGraph {
		t.Fatalf("degraded = %v, want [%q]", res.Degraded, degradedGraph)
	}
	stored, _ := FindNode(r.hot.graph, n.ID)
	if stored.State != StateArchived {
		t.Errorf("hot state = %s, want archived", stored.State)
	}
	if r.hot.dirtyCalls != 1 {
		t.Errorf("dirty calls = %d, want 1", r.hot.dirtyCalls)
	}
}

func TestServicePurgeNodeGates(t *testing.T) {
	ctx := context.Background()

	t.Run("confirm gate runs first", func(t *testing.T) {
		// Even a malformed id is refused for the destructive reason first.
		r := newRig(t, Graph{}, true)
		_, err := r.svc.PurgeNode(ctx, testKey, "not-a-ulid", false)
		domain := assertDomainErr(t, err, errs.ErrInvalid, opPurgeNode, EntityNode)
		if domain.Msg != msgPurgeNeedsConfirm {
			t.Errorf("msg = %q, want %q", domain.Msg, msgPurgeNeedsConfirm)
		}
		assertCalls(t, r.log)
	})

	t.Run("malformed id", func(t *testing.T) {
		r := newRig(t, Graph{}, true)
		_, err := r.svc.PurgeNode(ctx, testKey, "not-a-ulid", true)
		domain := assertDomainErr(t, err, errs.ErrInvalid, opPurgeNode, EntityNode)
		if domain.Msg != msgIDMustBeULID {
			t.Errorf("msg = %q, want %q", domain.Msg, msgIDMustBeULID)
		}
		assertCalls(t, r.log)
	})

	t.Run("active node stays behind the buffer", func(t *testing.T) {
		n := validNode(t)
		r := newRig(t, Graph{Nodes: []Node{n}}, true)
		_, err := r.svc.PurgeNode(ctx, testKey, n.ID, true)
		if !errors.Is(err, errs.ErrConflict) {
			t.Fatalf("err = %v, want ErrConflict (P5 buffer gate)", err)
		}
		if _, findErr := FindNode(r.hot.graph, n.ID); findErr != nil {
			t.Errorf("refused purge must leave the node in hot: %v", findErr)
		}
		// No graph delete for a refused purge.
		assertCalls(t, r.log, callUpdateKnowledge)
	})

	t.Run("missing node", func(t *testing.T) {
		r := newRig(t, Graph{}, true)
		_, err := r.svc.PurgeNode(ctx, testKey, newID(), true)
		if !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
}

// TestServicePurgeNodeRepairsAndDeletes: the purge repairs chain references in
// hot AND issues the graph delete — the two halves of F6.
func TestServicePurgeNodeRepairsAndDeletes(t *testing.T) {
	// Arrange: older <- purged <- newer, plus one unrelated edge that stays.
	ctx := context.Background()
	older, purged, newer, other := validNode(t), validNode(t), validNode(t), validNode(t)
	purged.State = StateArchived
	older.State = StateArchived
	older.SupersededBy = purged.ID
	purged.Supersedes = []string{older.ID}
	purged.SupersededBy = newer.ID
	newer.Supersedes = []string{purged.ID, other.ID}
	seed := Graph{
		Nodes: []Node{older, purged, newer, other},
		Edges: []Edge{
			{From: purged.ID, To: older.ID, Rel: RelSupersedes, Confidence: 1},
			{From: newer.ID, To: purged.ID, Rel: RelSupersedes, Confidence: 1},
			{From: newer.ID, To: other.ID, Rel: RelRelatesTo, Confidence: 1},
		},
	}
	r := newRig(t, seed, true)

	// Act
	res, err := r.svc.PurgeNode(ctx, testKey, purged.ID, true)

	// Assert
	if err != nil {
		t.Fatalf("PurgeNode: %v", err)
	}
	if res.PurgedID != purged.ID || res.RemovedEdges != 2 || len(res.Degraded) != 0 {
		t.Fatalf("result = %+v, want purged=%s removed=2 no degraded", res, purged.ID)
	}
	if _, err := FindNode(r.hot.graph, purged.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("purged node still in hot: %v", err)
	}
	gotOlder, _ := FindNode(r.hot.graph, older.ID)
	if gotOlder.SupersededBy != "" {
		t.Errorf("older.SupersededBy = %q, want repaired to empty", gotOlder.SupersededBy)
	}
	gotNewer, _ := FindNode(r.hot.graph, newer.ID)
	if len(gotNewer.Supersedes) != 1 || gotNewer.Supersedes[0] != other.ID {
		t.Errorf("newer.Supersedes = %v, want ghost reference removed", gotNewer.Supersedes)
	}
	if len(r.hot.graph.Edges) != 1 || r.hot.graph.Edges[0].Rel != RelRelatesTo {
		t.Errorf("hot edges = %+v, want only the unrelated relates_to", r.hot.graph.Edges)
	}
	if len(r.graph.deletedIDs) != 1 || r.graph.deletedIDs[0] != purged.ID {
		t.Errorf("graph deletes = %v, want [%s]", r.graph.deletedIDs, purged.ID)
	}
	// The repair must reach the mirror too (P5: no ghost reference in EITHER
	// store): after the graph delete, the survivors whose superseded_by /
	// supersedes the purge repaired are re-MERGEd in their repaired form.
	if len(r.graph.gotNodes) != 1 || len(r.graph.gotNodes[0]) != 2 {
		t.Fatalf("mirror upserts = %v, want one batch with the 2 repaired survivors", r.graph.gotNodes)
	}
	mirrored := map[string]Node{}
	for _, n := range r.graph.gotNodes[0] {
		mirrored[n.ID] = n
	}
	if n, ok := mirrored[older.ID]; !ok || n.SupersededBy != "" {
		t.Errorf("mirrored older = %+v (present=%v), want superseded_by repaired to empty", n, ok)
	}
	if n, ok := mirrored[newer.ID]; !ok || len(n.Supersedes) != 1 || n.Supersedes[0] != other.ID {
		t.Errorf("mirrored newer = %+v (present=%v), want supersedes repaired to [%s]", n, ok, other.ID)
	}
	assertCalls(t, r.log, callUpdateKnowledge, callDeleteNode, callMarkIndexed, callUpsertNodes, callMarkIndexed)
}

func TestServicePurgeNodeDegraded(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name string
		rig  func(t *testing.T, seed Graph) *rig
	}{
		{"graph not configured", func(t *testing.T, seed Graph) *rig {
			return newRig(t, seed, false)
		}},
		{"graph delete fails", func(t *testing.T, seed Graph) *rig {
			r := newRig(t, seed, true)
			r.graph.deleteErr = errBoom
			return r
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			n := validNode(t)
			n.State = StateDeprecated
			r := tt.rig(t, Graph{Nodes: []Node{n}})

			// Act
			res, err := r.svc.PurgeNode(ctx, testKey, n.ID, true)

			// Assert: hot purge holds, mirror failure is a degraded note.
			if err != nil {
				t.Fatalf("PurgeNode must succeed hot-side: %v", err)
			}
			if len(res.Degraded) != 1 || res.Degraded[0] != degradedGraph {
				t.Fatalf("degraded = %v, want [%q]", res.Degraded, degradedGraph)
			}
			if _, findErr := FindNode(r.hot.graph, n.ID); !errors.Is(findErr, errs.ErrNotFound) {
				t.Errorf("node still in hot after purge: %v", findErr)
			}
			if r.hot.dirtyCalls != 1 || r.hot.indexedCalls != 0 {
				t.Errorf("bookkeeping = %d dirty / %d indexed, want 1/0", r.hot.dirtyCalls, r.hot.indexedCalls)
			}
		})
	}
}
