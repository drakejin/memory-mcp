//go:build e2e

// P5 — 상태기계 (feature-inventory.md §2 P5): the transition allow-table is
// enforced (illegal moves 409), deprecate demands a reason, purge demands
// confirm + the archived/deprecated buffer, supersede is non-destructive, and
// purge repairs every chain reference. After EACH mutation the hot JSON and
// Neo4j must agree on state/superseded_by — the API said what it did, the two
// stores prove it. All probes run through the direct inspectors, never through
// the server's own read endpoints.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/drakejin/memory-mcp/internal/service/knowledge"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// Frozen public messages this scenario pins byte-for-byte (code-standards
// §2.2: the message is the rule that was broken).
const (
	p05MsgIllegalDeprecatedToArchived = "cannot transition deprecated -> archived"
	p05MsgDeprecateNeedsReasonInput   = "deprecation requires a reason"
	p05MsgDeprecateNeedsReasonDomain  = "deprecating requires a reason — why is it wrong?"
	p05MsgInvalidState                = "state must be one of active|archived|deprecated"
	p05MsgInvalidOp                   = "op must be set_state or deprecate"
	p05MsgNodeNotFound                = "node not found"
	p05MsgPurgeNeedsConfirm           = "purge requires confirm=true (§3 — S3 versioning is the backstop)"
	p05MsgPurgeNotBuffered            = "purge requires state archived or deprecated — archive or deprecate the node first"
	p05MsgSelfSupersede               = "node cannot supersede itself"
)

// Envelope error codes (apierr fixed projection).
const (
	p05CodeInvalid  = "invalid_request"
	p05CodeNotFound = "not_found"
	p05CodeConflict = "conflict"
)

// ---------- P5-local verbs (PATCH/DELETE are not seed verbs) ----------

// p05Patch issues PATCH .../knowledge/nodes/{id} and returns the raw envelope.
func p05Patch(t *testing.T, key projectkey.Key, id string, in knowledge.PatchNodeInput) (int, envelope) {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		failf(t, "marshal patch body: %v", err)
	}
	return h.api(t).do(t, http.MethodPatch, projPath(key)+"/knowledge/nodes/"+id, bytes.NewReader(raw), "application/json")
}

// p05PatchOK runs a PATCH that must succeed and returns the decoded NodeResult.
func p05PatchOK(t *testing.T, key projectkey.Key, id string, in knowledge.PatchNodeInput) knowledge.NodeResult {
	t.Helper()
	status, env := p05Patch(t, key, id, in)
	if status != http.StatusOK || !env.Success {
		failf(t, "PATCH node %s %+v: want 200 success, got HTTP %d error=%q", id, in, status, env.Error)
	}
	var res knowledge.NodeResult
	decodeData(t, env, &res)
	if len(res.Degraded) != 0 {
		failf(t, "PATCH node %s: unexpected degraded notes with every store up: %v", id, res.Degraded)
	}
	return res
}

// p05Purge issues DELETE .../knowledge/nodes/{id} with or without confirm.
func p05Purge(t *testing.T, key projectkey.Key, id string, confirm bool) (int, envelope) {
	t.Helper()
	path := projPath(key) + "/knowledge/nodes/" + id
	if confirm {
		path += "?confirm=true"
	}
	return h.api(t).do(t, http.MethodDelete, path, nil, "")
}

// p05AssertRejected pins a failure envelope: status, code and exact public
// message.
func p05AssertRejected(t *testing.T, what string, status int, env envelope, wantStatus int, wantCode, wantMsg string) {
	t.Helper()
	if status != wantStatus || env.Success || env.Error == nil {
		failf(t, "%s: want HTTP %d failure envelope, got HTTP %d success=%v error=%q", what, wantStatus, status, env.Success, env.Error)
	}
	if env.Error.Code != wantCode || env.Error.Message != wantMsg {
		failf(t, "%s: want code=%q msg=%q, got code=%q msg=%q", what, wantCode, wantMsg, env.Error.Code, env.Error.Message)
	}
}

// ---------- P5-local inspectors ----------

// p05AssertNodeBoth asserts one node's state and superseded_by agree between
// the hot JSON file and Neo4j — the P5 store-agreement invariant, checked
// after every mutation.
func p05AssertNodeBoth(t *testing.T, key projectkey.Key, id string, wantState knowledge.State, wantSupBy, step string) {
	t.Helper()
	hot, ok := h.hotKnowledgeNode(t, key, id)
	if !ok {
		failf(t, "%s: hot inspector: node %s missing from %s", step, id, h.hotKnowledgePath(key))
	}
	if hot.State != wantState || hot.SupersededBy != wantSupBy {
		failf(t, "%s: hot inspector: node %s = state %q superseded_by %q, want %q / %q",
			step, id, hot.State, hot.SupersededBy, wantState, wantSupBy)
	}
	neoState, found := h.neoNodeField(t, key, id, "state")
	if !found || neoState != string(wantState) {
		failf(t, "%s: neo inspector: node %s state = %q (found=%v), want %q", step, id, neoState, found, wantState)
	}
	neoSupBy, found := h.neoNodeField(t, key, id, "superseded_by")
	if !found || neoSupBy != wantSupBy {
		failf(t, "%s: neo inspector: node %s superseded_by = %q (found=%v), want %q", step, id, neoSupBy, found, wantSupBy)
	}
	pass(t, "%s: hot and neo4j agree on %s: state=%s superseded_by=%q", step, id, wantState, wantSupBy)
}

// p05NeoNodeExists reports whether the mirrored node exists at all.
func p05NeoNodeExists(t *testing.T, key projectkey.Key, id string) bool {
	t.Helper()
	q := fmt.Sprintf("MATCH (n:%s {id: %s, %s}) RETURN count(n);", neoNodeLabel, cypherLit(id), neoScope(key))
	return h.cypherCount(t, q) > 0
}

// p05NeoGhostRefs counts surviving mirrored nodes that still reference ghost
// in superseded_by or supersedes — must be 0 after a purge (P5 chain repair).
func p05NeoGhostRefs(t *testing.T, key projectkey.Key, ghost string) int {
	t.Helper()
	q := fmt.Sprintf("MATCH (n:%s {%s}) WHERE n.superseded_by = %s OR %s IN n.supersedes RETURN count(n);",
		neoNodeLabel, neoScope(key), cypherLit(ghost), cypherLit(ghost))
	return h.cypherCount(t, q)
}

// p05AssertNoGhost asserts neither store holds any reference to ghost: the
// node itself, superseded_by/supersedes entries, or incident edges.
func p05AssertNoGhost(t *testing.T, key projectkey.Key, ghost, step string) {
	t.Helper()
	// Hot side: full-graph scan straight off the JSON file.
	g := h.hotKnowledge(t, key)
	for _, n := range g.Nodes {
		if n.ID == ghost {
			failf(t, "%s: hot inspector: purged node %s still present", step, ghost)
		}
		if n.SupersededBy == ghost {
			failf(t, "%s: hot inspector: node %s superseded_by still points at purged %s", step, n.ID, ghost)
		}
		if slices.Contains(n.Supersedes, ghost) {
			failf(t, "%s: hot inspector: node %s supersedes still lists purged %s", step, n.ID, ghost)
		}
	}
	for _, e := range g.Edges {
		if e.From == ghost || e.To == ghost {
			failf(t, "%s: hot inspector: edge %s-[%s]->%s still references purged %s", step, e.From, e.Rel, e.To, ghost)
		}
	}
	// Neo4j side: the purged node, ghost property references, and any edge
	// would have to hang off a node — all probed directly via cypher-shell.
	if p05NeoNodeExists(t, key, ghost) {
		failf(t, "%s: neo inspector: purged node %s still mirrored", step, ghost)
	}
	if n := p05NeoGhostRefs(t, key, ghost); n != 0 {
		failf(t, "%s: neo inspector: %d surviving node(s) still reference purged %s in superseded_by/supersedes", step, n, ghost)
	}
	pass(t, "%s: no ghost reference to %s in hot or neo4j", step, ghost)
}

// p05Node creates one active fixture node through the API and immediately
// proves both stores hold it.
func p05Node(t *testing.T, key projectkey.Key, name string) knowledge.Node {
	t.Helper()
	res := h.postNode(t, key, knowledge.CreateNodeInput{
		Kind: knowledge.KindFact, Name: name, Body: "P5 fixture " + name,
		Trust: knowledge.TrustAgentInferred,
	})
	if len(res.Degraded) != 0 {
		failf(t, "postNode %s: unexpected degraded notes with every store up: %v", name, res.Degraded)
	}
	p05AssertNodeBoth(t, key, res.Node.ID, knowledge.StateActive, "", "create "+name)
	return res.Node
}

// ---------- scenarios ----------

// TestP05_StateMachine_TransitionMatrix walks one node through the full v1
// transition table: 6 legal moves (incl. the same-state no-op), the illegal
// deprecated→archived move, the deprecate-reason gate at both layers, and the
// malformed-input rejections. After every call — accepted or rejected — hot
// and Neo4j must agree on the node's state.
func TestP05_StateMachine_TransitionMatrix(t *testing.T) {
	h.ensureServer(t)
	key := e2eKey("p05-matrix")
	node := p05Node(t, key, "p05-matrix-node")

	steps := []struct {
		name       string
		in         knowledge.PatchNodeInput
		wantStatus int
		wantCode   string // "" = success expected
		wantMsg    string
		wantState  knowledge.State // state after the call, in BOTH stores
	}{
		{"legal no-op active->active", knowledge.PatchNodeInput{Op: knowledge.OpSetState, State: knowledge.StateActive}, http.StatusOK, "", "", knowledge.StateActive},
		{"legal active->archived", knowledge.PatchNodeInput{Op: knowledge.OpSetState, State: knowledge.StateArchived}, http.StatusOK, "", "", knowledge.StateArchived},
		{"legal archived->active", knowledge.PatchNodeInput{Op: knowledge.OpSetState, State: knowledge.StateActive}, http.StatusOK, "", "", knowledge.StateActive},
		{"legal active->deprecated (deprecate op)", knowledge.PatchNodeInput{Op: knowledge.OpDeprecate, Reason: "judged wrong: stale claim"}, http.StatusOK, "", "", knowledge.StateDeprecated},
		{"legal deprecated->active", knowledge.PatchNodeInput{Op: knowledge.OpSetState, State: knowledge.StateActive}, http.StatusOK, "", "", knowledge.StateActive},
		{"transit active->archived", knowledge.PatchNodeInput{Op: knowledge.OpSetState, State: knowledge.StateArchived}, http.StatusOK, "", "", knowledge.StateArchived},
		{"legal archived->deprecated (set_state with reason)", knowledge.PatchNodeInput{Op: knowledge.OpSetState, State: knowledge.StateDeprecated, Reason: "set_state deprecation path"}, http.StatusOK, "", "", knowledge.StateDeprecated},
		{"illegal deprecated->archived", knowledge.PatchNodeInput{Op: knowledge.OpSetState, State: knowledge.StateArchived}, http.StatusConflict, p05CodeConflict, p05MsgIllegalDeprecatedToArchived, knowledge.StateDeprecated},
		{"transit deprecated->active", knowledge.PatchNodeInput{Op: knowledge.OpSetState, State: knowledge.StateActive}, http.StatusOK, "", "", knowledge.StateActive},
		{"gate: deprecate with empty reason (transport layer)", knowledge.PatchNodeInput{Op: knowledge.OpDeprecate}, http.StatusBadRequest, p05CodeInvalid, p05MsgDeprecateNeedsReasonInput, knowledge.StateActive},
		{"gate: deprecate with whitespace reason (domain layer)", knowledge.PatchNodeInput{Op: knowledge.OpDeprecate, Reason: "   "}, http.StatusConflict, p05CodeConflict, p05MsgDeprecateNeedsReasonDomain, knowledge.StateActive},
		{"illegal target state", knowledge.PatchNodeInput{Op: knowledge.OpSetState, State: knowledge.State("limbo")}, http.StatusBadRequest, p05CodeInvalid, p05MsgInvalidState, knowledge.StateActive},
		{"illegal op", knowledge.PatchNodeInput{Op: "flip"}, http.StatusBadRequest, p05CodeInvalid, p05MsgInvalidOp, knowledge.StateActive},
	}
	for _, step := range steps {
		status, env := p05Patch(t, key, node.ID, step.in)
		if step.wantCode == "" {
			if status != step.wantStatus || !env.Success {
				failf(t, "%s: want HTTP %d success, got HTTP %d error=%q", step.name, step.wantStatus, status, env.Error)
			}
			var res knowledge.NodeResult
			decodeData(t, env, &res)
			if res.Node.State != step.wantState {
				failf(t, "%s: response state = %q, want %q", step.name, res.Node.State, step.wantState)
			}
		} else {
			p05AssertRejected(t, step.name, status, env, step.wantStatus, step.wantCode, step.wantMsg)
		}
		// The invariant: whatever the answer was, hot and Neo4j agree on the
		// resulting (or preserved) state. This node never enters a supersede
		// chain, so superseded_by stays empty throughout.
		p05AssertNodeBoth(t, key, node.ID, step.wantState, "", step.name)
	}

	// Boundary rejections that never reach the state machine.
	ghostID := newULIDAt(t, node.Created)
	status, env := p05Patch(t, key, ghostID, knowledge.PatchNodeInput{Op: knowledge.OpSetState, State: knowledge.StateArchived})
	p05AssertRejected(t, "patch unknown node", status, env, http.StatusNotFound, p05CodeNotFound, p05MsgNodeNotFound)
	status, env = p05Patch(t, key, "not-a-ulid", knowledge.PatchNodeInput{Op: knowledge.OpSetState, State: knowledge.StateArchived})
	if status != http.StatusBadRequest || env.Success || env.Error == nil || env.Error.Code != p05CodeInvalid {
		failf(t, "patch malformed id: want 400 %s, got HTTP %d error=%q", p05CodeInvalid, status, env.Error)
	}
	pass(t, "matrix complete: 6 legal, 1 illegal move, 2 reason gates, 2 malformed inputs, 1 unknown id")
}

// TestP05_StateMachine_SupersedeNonDestructive proves the F4 revision
// semantics in both stores: loser archived (or kept deprecated) with
// superseded_by set, winner active, the supersedes edge auto-created, revival
// releases superseded_by, and a missing target rejects the whole write
// atomically.
func TestP05_StateMachine_SupersedeNonDestructive(t *testing.T) {
	h.ensureServer(t)
	key := e2eKey("p05-supersede")

	a := p05Node(t, key, "p05-claim-v1")
	bRes := h.postNode(t, key, knowledge.CreateNodeInput{
		Kind: knowledge.KindFact, Name: "p05-claim-v2", Body: "corrected claim",
		Trust: knowledge.TrustAgentInferred, Supersedes: []string{a.ID},
	})
	b := bRes.Node
	if len(b.Supersedes) != 1 || b.Supersedes[0] != a.ID {
		failf(t, "supersede response: winner.supersedes = %v, want [%s]", b.Supersedes, a.ID)
	}
	p05AssertNodeBoth(t, key, a.ID, knowledge.StateArchived, b.ID, "supersede: loser archived")
	p05AssertNodeBoth(t, key, b.ID, knowledge.StateActive, "", "supersede: winner active")

	// The supersedes edge exists in both stores with confidence 1.
	g := h.hotKnowledge(t, key)
	if len(g.Edges) != 1 {
		failf(t, "hot inspector: want exactly 1 edge after supersede, got %d: %+v", len(g.Edges), g.Edges)
	}
	edge := g.Edges[0]
	if edge.From != b.ID || edge.To != a.ID || edge.Rel != knowledge.RelSupersedes || edge.Confidence != 1 {
		failf(t, "hot inspector: supersedes edge = %+v, want %s-[supersedes,conf=1]->%s", edge, b.ID, a.ID)
	}
	if got := h.neoEdgeCount(t, key, string(knowledge.RelSupersedes)); got != 1 {
		failf(t, "neo inspector: supersedes edge count = %d, want 1", got)
	}

	// Revival: archived loser returns to active and superseded_by is released
	// in BOTH stores (F5 "active 복귀 시 superseded_by 해제").
	res := p05PatchOK(t, key, a.ID, knowledge.PatchNodeInput{Op: knowledge.OpSetState, State: knowledge.StateActive})
	if res.Node.SupersededBy != "" {
		failf(t, "revival response: superseded_by = %q, want released", res.Node.SupersededBy)
	}
	p05AssertNodeBoth(t, key, a.ID, knowledge.StateActive, "", "revival releases superseded_by")
	// The winner's supersedes history is untouched by the revival.
	hotB, _ := h.hotKnowledgeNode(t, key, b.ID)
	if len(hotB.Supersedes) != 1 || hotB.Supersedes[0] != a.ID {
		failf(t, "hot inspector: winner.supersedes changed by revival: %v", hotB.Supersedes)
	}

	// A deprecated loser keeps its deprecated state (only active losers move
	// to the archived buffer).
	c := p05Node(t, key, "p05-dep-claim")
	p05PatchOK(t, key, c.ID, knowledge.PatchNodeInput{Op: knowledge.OpDeprecate, Reason: "wrong on purpose"})
	p05AssertNodeBoth(t, key, c.ID, knowledge.StateDeprecated, "", "deprecate fixture")
	dRes := h.postNode(t, key, knowledge.CreateNodeInput{
		Kind: knowledge.KindFact, Name: "p05-dep-claim-v2", Body: "replacement",
		Trust: knowledge.TrustAgentInferred, Supersedes: []string{c.ID},
	})
	p05AssertNodeBoth(t, key, c.ID, knowledge.StateDeprecated, dRes.Node.ID, "supersede keeps deprecated loser deprecated")
	p05AssertNodeBoth(t, key, dRes.Node.ID, knowledge.StateActive, "", "second winner active")

	// Missing supersede target: the whole creation is rejected atomically —
	// no node appears in either store.
	hotNodes, neoNodes := len(h.hotKnowledge(t, key).Nodes), h.neoNodeCount(t, key)
	status, env := h.api(t).postJSON(t, projPath(key)+"/knowledge/nodes", knowledge.CreateNodeInput{
		Kind: knowledge.KindFact, Name: "p05-orphan-winner", Body: "must not land",
		Trust: knowledge.TrustAgentInferred, Supersedes: []string{newULIDAt(t, a.Created)},
	})
	if status != http.StatusNotFound || env.Success || env.Error == nil || env.Error.Code != p05CodeNotFound {
		failf(t, "supersede of missing target: want 404 %s, got HTTP %d error=%q", p05CodeNotFound, status, env.Error)
	}
	if got := len(h.hotKnowledge(t, key).Nodes); got != hotNodes {
		failf(t, "hot inspector: rejected supersede changed node count %d -> %d", hotNodes, got)
	}
	if got := h.neoNodeCount(t, key); got != neoNodes {
		failf(t, "neo inspector: rejected supersede changed node count %d -> %d", neoNodes, got)
	}
	pass(t, "supersede is non-destructive and atomic in both stores")
}

// TestP05_StateMachine_SelfSupersedeEdgeRejected proves the P5 gate the v1
// transport check missed: a from==to supersedes edge must be rejected and
// reach neither store, while an ordinary edge on the same node still works.
func TestP05_StateMachine_SelfSupersedeEdgeRejected(t *testing.T) {
	h.ensureServer(t)
	key := e2eKey("p05-selfedge")

	n := p05Node(t, key, "p05-self-a")
	m := p05Node(t, key, "p05-self-b")

	status, env := h.api(t).postJSON(t, projPath(key)+"/knowledge/edges", knowledge.Edge{
		From: n.ID, To: n.ID, Rel: knowledge.RelSupersedes, Confidence: 1,
	})
	p05AssertRejected(t, "self-supersede edge", status, env, http.StatusBadRequest, p05CodeInvalid, p05MsgSelfSupersede)
	if got := len(h.hotKnowledge(t, key).Edges); got != 0 {
		failf(t, "hot inspector: self-supersede edge landed in hot: %d edge(s)", got)
	}
	if got := h.neoEdgeCount(t, key, ""); got != 0 {
		failf(t, "neo inspector: self-supersede edge landed in neo4j: %d edge(s)", got)
	}

	// Control: the rejection is specific to self-supersede, not edges at large.
	h.postEdge(t, key, knowledge.Edge{From: n.ID, To: m.ID, Rel: knowledge.RelRelatesTo, Confidence: 0.9})
	if got := len(h.hotKnowledge(t, key).Edges); got != 1 {
		failf(t, "hot inspector: control edge missing, got %d edge(s)", got)
	}
	if got := h.neoEdgeCount(t, key, string(knowledge.RelRelatesTo)); got != 1 {
		failf(t, "neo inspector: control edge missing, got %d edge(s)", got)
	}
	pass(t, "self-supersede edge rejected before either store; control edge landed in both")
}

// TestP05_StateMachine_PurgeGates proves the two destructive gates in order:
// confirm=true is demanded before anything else (even an unusable id), and an
// active node is refused with 409 — the node must survive in BOTH stores.
func TestP05_StateMachine_PurgeGates(t *testing.T) {
	h.ensureServer(t)
	key := e2eKey("p05-purgegates")
	n := p05Node(t, key, "p05-gate-node")

	status, env := p05Purge(t, key, n.ID, false)
	p05AssertRejected(t, "purge without confirm", status, env, http.StatusBadRequest, p05CodeInvalid, p05MsgPurgeNeedsConfirm)
	p05AssertNodeBoth(t, key, n.ID, knowledge.StateActive, "", "purge without confirm changed nothing")

	// The confirm gate outranks id validation: a destructive call is rejected
	// for the destructive reason first (documented handler contract).
	status, env = p05Purge(t, key, "not-a-ulid", false)
	p05AssertRejected(t, "purge without confirm, malformed id", status, env, http.StatusBadRequest, p05CodeInvalid, p05MsgPurgeNeedsConfirm)

	status, env = p05Purge(t, key, n.ID, true)
	p05AssertRejected(t, "purge active node", status, env, http.StatusConflict, p05CodeConflict, p05MsgPurgeNotBuffered)
	p05AssertNodeBoth(t, key, n.ID, knowledge.StateActive, "", "refused purge changed nothing")

	// From the archived buffer the purge is allowed and empties both stores.
	p05PatchOK(t, key, n.ID, knowledge.PatchNodeInput{Op: knowledge.OpSetState, State: knowledge.StateArchived})
	status, env = p05Purge(t, key, n.ID, true)
	if status != http.StatusOK || !env.Success {
		failf(t, "purge archived node: want 200, got HTTP %d error=%q", status, env.Error)
	}
	var res knowledge.PurgeResult
	decodeData(t, env, &res)
	if res.PurgedID != n.ID || res.RemovedEdges != 0 || len(res.Degraded) != 0 {
		failf(t, "purge result = %+v, want purged_id=%s removed_edges=0 no degraded", res, n.ID)
	}
	p05AssertNoGhost(t, key, n.ID, "purge from archived")

	// From the deprecated buffer too.
	d := p05Node(t, key, "p05-gate-dep")
	p05PatchOK(t, key, d.ID, knowledge.PatchNodeInput{Op: knowledge.OpDeprecate, Reason: "buffer via deprecated"})
	status, env = p05Purge(t, key, d.ID, true)
	if status != http.StatusOK || !env.Success {
		failf(t, "purge deprecated node: want 200, got HTTP %d error=%q", status, env.Error)
	}
	p05AssertNoGhost(t, key, d.ID, "purge from deprecated")

	// A confirmed purge of an unknown node is 404, not a silent 200.
	status, env = p05Purge(t, key, newULIDAt(t, n.Created), true)
	if status != http.StatusNotFound || env.Success || env.Error == nil || env.Error.Code != p05CodeNotFound {
		failf(t, "purge unknown node: want 404 %s, got HTTP %d error=%q", p05CodeNotFound, status, env.Error)
	}
	pass(t, "purge gates hold: confirm first, buffer enforced, both stores emptied on success")
}

// TestP05_StateMachine_PurgeChainRepair builds the three-generation chain
// A <- B <- C, purges the middle node, and demands that NO ghost reference to
// it survives in either store: hot must repair A.superseded_by and
// C.supersedes, and Neo4j must mirror exactly that repaired truth.
func TestP05_StateMachine_PurgeChainRepair(t *testing.T) {
	h.ensureServer(t)
	key := e2eKey("p05-purgechain")

	a := p05Node(t, key, "p05-chain-v1")
	bRes := h.postNode(t, key, knowledge.CreateNodeInput{
		Kind: knowledge.KindFact, Name: "p05-chain-v2", Body: "second generation",
		Trust: knowledge.TrustAgentInferred, Supersedes: []string{a.ID},
	})
	b := bRes.Node
	cRes := h.postNode(t, key, knowledge.CreateNodeInput{
		Kind: knowledge.KindFact, Name: "p05-chain-v3", Body: "third generation",
		Trust: knowledge.TrustAgentInferred, Supersedes: []string{b.ID},
	})
	c := cRes.Node

	p05AssertNodeBoth(t, key, a.ID, knowledge.StateArchived, b.ID, "chain: v1 archived under v2")
	p05AssertNodeBoth(t, key, b.ID, knowledge.StateArchived, c.ID, "chain: v2 archived under v3")
	p05AssertNodeBoth(t, key, c.ID, knowledge.StateActive, "", "chain: v3 active")
	if got := h.neoEdgeCount(t, key, string(knowledge.RelSupersedes)); got != 2 {
		failf(t, "neo inspector: want 2 supersedes edges before purge, got %d", got)
	}

	// Purge the middle generation (already in the archived buffer).
	status, env := p05Purge(t, key, b.ID, true)
	if status != http.StatusOK || !env.Success {
		failf(t, "purge chain middle: want 200, got HTTP %d error=%q", status, env.Error)
	}
	var res knowledge.PurgeResult
	decodeData(t, env, &res)
	// Both incident supersedes edges (B->A and C->B) must be counted.
	if res.PurgedID != b.ID || res.RemovedEdges != 2 || len(res.Degraded) != 0 {
		failf(t, "purge result = %+v, want purged_id=%s removed_edges=2 no degraded", res, b.ID)
	}

	// No ghost anywhere: node gone, chain references repaired, edges gone —
	// in hot AND in Neo4j.
	p05AssertNoGhost(t, key, b.ID, "chain repair")
	p05AssertNodeBoth(t, key, a.ID, knowledge.StateArchived, "", "chain repair: v1 superseded_by released")
	p05AssertNodeBoth(t, key, c.ID, knowledge.StateActive, "", "chain repair: v3 untouched")
	hotC, _ := h.hotKnowledgeNode(t, key, c.ID)
	if len(hotC.Supersedes) != 0 {
		failf(t, "hot inspector: v3.supersedes = %v, want ghost entry removed", hotC.Supersedes)
	}
	if got := h.cypherCount(t, fmt.Sprintf(
		"MATCH (n:%s {id: %s, %s}) WHERE size(n.supersedes) = 0 RETURN count(n);",
		neoNodeLabel, cypherLit(c.ID), neoScope(key))); got != 1 {
		failf(t, "neo inspector: v3.supersedes not emptied in the mirror")
	}
	if got := h.neoEdgeCount(t, key, ""); got != 0 {
		failf(t, "neo inspector: want 0 edges after purge, got %d", got)
	}
	if got := len(h.hotKnowledge(t, key).Edges); got != 0 {
		failf(t, "hot inspector: want 0 edges after purge, got %d", got)
	}

	// Purge the repaired first generation too: still archived, still purgeable,
	// and the graph ends with the sole active head in both stores.
	status, env = p05Purge(t, key, a.ID, true)
	if status != http.StatusOK || !env.Success {
		failf(t, "purge chain head: want 200, got HTTP %d error=%q", status, env.Error)
	}
	decodeData(t, env, &res)
	if res.PurgedID != a.ID || res.RemovedEdges != 0 {
		failf(t, "second purge result = %+v, want purged_id=%s removed_edges=0", res, a.ID)
	}
	p05AssertNoGhost(t, key, a.ID, "second purge")
	if got := len(h.hotKnowledge(t, key).Nodes); got != 1 {
		failf(t, "hot inspector: want 1 surviving node, got %d", got)
	}
	if got := h.neoNodeCount(t, key); got != 1 {
		failf(t, "neo inspector: want 1 surviving node, got %d", got)
	}
	p05AssertNodeBoth(t, key, c.ID, knowledge.StateActive, "", "final survivor")
	pass(t, "purge repaired the chain and left zero ghost references in either store")
}
