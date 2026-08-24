package server

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/errs"
	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/knowledge"
	"github.com/drakejin/memory-mcp/internal/server/apierr"
)

// PATCH operations of §7. set_state names the target explicitly; deprecate is
// the shorthand that always targets deprecated and demands a reason.
const (
	opSetState  = "set_state"
	opDeprecate = "deprecate"
)

// CreateNodeRequest is the POST body for knowledge node creation. Supersedes
// lists node ids this new node replaces (§7 supersede support); the state
// machine work happens in knowledge.Supersede.
type CreateNodeRequest struct {
	Kind        knowledge.NodeKind `json:"kind"`
	Name        string             `json:"name"`
	Body        string             `json:"body"`
	Aliases     []string           `json:"aliases"`
	Trust       knowledge.Trust    `json:"trust"`
	Provenance  []string           `json:"provenance"`
	Supersedes  []string           `json:"supersedes"`
	ReviewAfter string             `json:"review_after"`
}

// PatchNodeRequest mutates node lifecycle state: op is "set_state" or
// "deprecate" (deprecate requires reason).
type PatchNodeRequest struct {
	Op     string          `json:"op"`
	State  knowledge.State `json:"state,omitempty"`
	Reason string          `json:"reason,omitempty"`
}

// NodeResponse is a node write result plus degraded notes when the best-effort
// Neo4j mirror failed (§5 — hot write already succeeded).
type NodeResponse struct {
	Node     knowledge.Node `json:"node"`
	Degraded []string       `json:"degraded,omitempty"`
}

// EdgeResponse is an edge write result plus degraded notes.
type EdgeResponse struct {
	Edge     knowledge.Edge `json:"edge"`
	Degraded []string       `json:"degraded,omitempty"`
}

// PurgeNodeResponse reports a hot purge; S3 versioning is the backstop (§3).
type PurgeNodeResponse struct {
	PurgedID     string   `json:"purged_id"`
	RemovedEdges int      `json:"removed_edges"`
	Degraded     []string `json:"degraded,omitempty"`
}

// validate applies §2.2 invariants at the HTTP boundary.
func (req CreateNodeRequest) validate() *apierr.Error {
	if !knowledge.ValidNodeKind(req.Kind) {
		return badRequest("kind must be one of entity|fact|lesson|preference|document")
	}
	if req.Name == "" {
		return badRequest("name must not be empty")
	}
	if !knowledge.ValidTrust(req.Trust) {
		return badRequest("trust must be one of user-stated|agent-inferred|imported")
	}
	if apiErr := validateULIDs(req.Provenance, "provenance"); apiErr != nil {
		return apiErr
	}
	if apiErr := validateULIDs(req.Supersedes, "supersedes"); apiErr != nil {
		return apiErr
	}
	if req.ReviewAfter != "" {
		if _, err := time.Parse(time.RFC3339, req.ReviewAfter); err != nil {
			return badRequest("review_after must be RFC3339 or empty")
		}
	}
	return nil
}

// handleCreateNode godoc
//
//	@Summary	Create a knowledge node (optionally superseding others)
//	@Tags		knowledge
//	@Accept		json
//	@Produce	json
//	@Param		ws		path		string				true	"workspace"
//	@Param		team	path		string				true	"team"
//	@Param		proj	path		string				true	"project"
//	@Param		body	body		CreateNodeRequest	true	"node"
//	@Success	201		{object}	Envelope{data=NodeResponse}
//	@Failure	400		{object}	Envelope
//	@Failure	404		{object}	Envelope	"superseded node not found"
//	@Router		/v1/{ws}/{team}/{proj}/knowledge/nodes [post]
//
// Contract: validate, assign ULID, apply knowledge.Supersede against the hot
// graph, atomic UpdateKnowledge, then best-effort Neo4j MERGE and the
// consolidation promotion of the provenance episodes (degraded notes on
// failure). No judgement calls server-side — supersede decisions are the
// agent's (§0 principle 2).
func (s *Server) handleCreateNode(w http.ResponseWriter, r *http.Request) {
	key := projectKey(r)
	if apiErr := validateProjectKey(key); apiErr != nil {
		writeAPIError(w, s.log, apiErr)
		return
	}
	if s.store == nil {
		writeAPIError(w, s.log, unavailable(msgHotStoreUnavailable))
		return
	}
	var req CreateNodeRequest
	if apiErr := decodeJSON(w, r, &req, false); apiErr != nil {
		writeAPIError(w, s.log, apiErr)
		return
	}
	if apiErr := req.validate(); apiErr != nil {
		writeAPIError(w, s.log, apiErr)
		return
	}

	now := s.clock.Now().UTC()
	id, err := s.ids.GenerateAt(now.UnixMilli())
	if err != nil {
		writeAPIError(w, s.log, apierr.From(err))
		return
	}
	node := knowledge.Node{
		ID:          id,
		Kind:        req.Kind,
		Name:        req.Name,
		Body:        req.Body,
		Aliases:     normalizeStrings(req.Aliases),
		State:       knowledge.StateActive,
		Trust:       req.Trust,
		Supersedes:  []string{},
		Provenance:  normalizeStrings(req.Provenance),
		Created:     now,
		Updated:     now,
		ReviewAfter: req.ReviewAfter,
	}

	// Read, decide and write inside one UpdateKnowledge closure: the document
	// is replaced wholesale, so a read-then-write pair would let a concurrent
	// creation drop this node from hot while both reach Neo4j — exactly the
	// derived-only content §0 principle 1 forbids.
	var stored knowledge.Node
	var affected []knowledge.Node
	var newEdges []knowledge.Edge
	err = s.store.UpdateKnowledge(r.Context(), key, func(g knowledge.Graph) (knowledge.Graph, error) {
		var next knowledge.Graph
		if len(req.Supersedes) > 0 {
			// knowledge owns the state machine: a missing target is its
			// NotFound, a self- or duplicate-supersede its Invalid, and apierr
			// maps both.
			var err error
			next, err = knowledge.Supersede(g, node, req.Supersedes, now)
			if err != nil {
				return g, err
			}
		} else {
			next = knowledge.Graph{
				Nodes: append(slices.Clone(g.Nodes), node),
				Edges: slices.Clone(g.Edges),
			}
		}
		stored = findGraphNode(next, node.ID, node)
		affected = affectedNodes(next, node.ID, req.Supersedes)
		newEdges = diffEdges(g, next)
		return next, nil
	})
	if err != nil {
		writeAPIError(w, s.log, apierr.From(err))
		return
	}

	degraded := s.mirrorKnowledge(r.Context(), key, affected, newEdges)
	degraded = append(degraded, s.promoteProvenance(r.Context(), key, stored.Provenance)...)
	writeJSON(w, s.log, http.StatusCreated, NodeResponse{Node: stored, Degraded: degraded})
}

// promoteProvenance closes the §3 consolidation loop. Distillation is the
// agent's job: it reads episodes, writes the knowledge node, and names the
// episodes it distilled in provenance. That naming IS the promotion §3
// describes ("에이전트가 episode들을 증류해 knowledge 승격 — POST /knowledge,
// provenance 링크"), and §3.1 reads its result, consolidated=true, as the
// precondition for a record ever sinking to cold.
//
// Nothing is judged here (§0 principle 2): the server only records the
// consequence of a statement the agent made. Without it no API path sets the
// flag at all, so aging never fires, hot files grow past the §3.1 pressure
// thresholds with no relief, and /v1/status reports a stale_unconsolidated
// count no legitimate client action can reduce.
//
// Ids that name no hot record of this project — already aged to cold, or
// belonging to another project — are skipped rather than failing a node that is
// already written. So is a record already consolidated: re-promoting it would
// rewrite the hot file for nothing. The whole step is best-effort: the hot
// knowledge write has succeeded, so a bookkeeping failure is a degraded note
// (§5), never a failed creation.
func (s *Server) promoteProvenance(ctx context.Context, key hotstore.ProjectKey, provenance []string) []string {
	if len(provenance) == 0 {
		return nil
	}
	recs, err := s.store.ListEpisodes(ctx, key)
	if err != nil {
		s.log.Warn("provenance promotion: list episodes failed", "project", key.String(), "error", err)
		return []string{degradedPromotion}
	}
	named := make(map[string]bool, len(provenance))
	for _, id := range provenance {
		named[id] = true
	}
	pending := make([]string, 0, len(provenance))
	for _, rec := range recs {
		if named[rec.ID] && !rec.Consolidated {
			pending = append(pending, rec.ID)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	err = s.store.UpdateEpisodes(ctx, key, pending, func(rec episodic.Record) episodic.Record {
		rec.Consolidated = true
		return rec
	})
	if err != nil {
		s.log.Warn("provenance promotion: mark consolidated failed",
			"project", key.String(), "episodes", len(pending), "error", err)
		return []string{degradedPromotion}
	}
	// consolidated is an indexed field: a hot-only flip would leave search
	// reporting the record as still undistilled.
	s.convergeEpisodes(ctx, key, pending)
	return nil
}

// mirrorKnowledge best-effort MERGEs nodes/edges into Neo4j and maintains the
// manifest. Returns degraded notes for the response envelope (§5).
func (s *Server) mirrorKnowledge(ctx context.Context, key hotstore.ProjectKey, nodes []knowledge.Node, edges []knowledge.Edge) []string {
	if s.graph == nil {
		s.markDirty(ctx, key, hotstore.PlaneKnowledge)
		return []string{degradedGraph}
	}
	var err error
	if len(nodes) > 0 {
		err = s.graph.UpsertNodes(ctx, key, nodes)
	}
	if err == nil && len(edges) > 0 {
		err = s.graph.UpsertEdges(ctx, key, edges)
	}
	if err != nil {
		s.log.Warn("knowledge graph mirror failed; degraded", "project", key.String(), "error", err)
		s.markDirty(ctx, key, hotstore.PlaneKnowledge)
		return []string{degradedGraph}
	}
	s.markIndexed(ctx, key, hotstore.PlaneKnowledge)
	return nil
}

// handleCreateEdge godoc
//
//	@Summary	Create a knowledge edge
//	@Tags		knowledge
//	@Accept		json
//	@Produce	json
//	@Param		ws		path		string			true	"workspace"
//	@Param		team	path		string			true	"team"
//	@Param		proj	path		string			true	"project"
//	@Param		body	body		knowledge.Edge	true	"edge"
//	@Success	201		{object}	Envelope{data=EdgeResponse}
//	@Failure	400		{object}	Envelope
//	@Failure	404		{object}	Envelope	"endpoint node not found"
//	@Router		/v1/{ws}/{team}/{proj}/knowledge/edges [post]
//
// Contract: both endpoints must exist in the hot graph; write hot first, then
// best-effort MERGE.
func (s *Server) handleCreateEdge(w http.ResponseWriter, r *http.Request) {
	key := projectKey(r)
	if apiErr := validateProjectKey(key); apiErr != nil {
		writeAPIError(w, s.log, apiErr)
		return
	}
	if s.store == nil {
		writeAPIError(w, s.log, unavailable(msgHotStoreUnavailable))
		return
	}
	var edge knowledge.Edge
	if apiErr := decodeJSON(w, r, &edge, false); apiErr != nil {
		writeAPIError(w, s.log, apiErr)
		return
	}
	if apiErr := validateEdge(edge); apiErr != nil {
		writeAPIError(w, s.log, apiErr)
		return
	}
	edge.Provenance = normalizeStrings(edge.Provenance)

	// The endpoint check and the insert share one closure so an edge can never
	// be written against a node a concurrent purge has already removed. The
	// transport error returned from here survives apierr.From unchanged.
	err := s.store.UpdateKnowledge(r.Context(), key, func(g knowledge.Graph) (knowledge.Graph, error) {
		if !nodeExists(g, edge.From) || !nodeExists(g, edge.To) {
			return g, notFound("edge endpoint node not found")
		}
		return upsertEdge(g, edge), nil
	})
	if err != nil {
		writeAPIError(w, s.log, apierr.From(err))
		return
	}
	degraded := s.mirrorKnowledge(r.Context(), key, nil, []knowledge.Edge{edge})
	writeJSON(w, s.log, http.StatusCreated, EdgeResponse{Edge: edge, Degraded: degraded})
}

// handleSearchKnowledge godoc
//
//	@Summary	Full-text search over knowledge nodes
//	@Tags		knowledge
//	@Produce	json
//	@Param		ws					path		string	true	"workspace"
//	@Param		team				path		string	true	"team"
//	@Param		proj				path		string	true	"project"
//	@Param		q					query		string	true	"query text"
//	@Param		include_archived	query		bool	false	"opt-in archived/deprecated (§7)"
//	@Success	200					{object}	Envelope{data=[]knowledge.Node}
//	@Failure	400					{object}	Envelope
//	@Failure	503					{object}	Envelope	"graph unavailable (§5)"
//	@Router		/v1/{ws}/{team}/{proj}/knowledge/search [get]
func (s *Server) handleSearchKnowledge(w http.ResponseWriter, r *http.Request) {
	key := projectKey(r)
	if apiErr := validateProjectKey(key); apiErr != nil {
		writeAPIError(w, s.log, apiErr)
		return
	}
	q := r.URL.Query().Get(paramQuery)
	if q == "" {
		writeAPIError(w, s.log, badRequest(paramQuery+" is required"))
		return
	}
	includeArchived := r.URL.Query().Get(paramIncludeArchived) == valueTrue
	if s.graph == nil {
		writeAPIError(w, s.log, unavailable(degradedGraph))
		return
	}

	s.statGate(r.Context(), key)

	nodes, err := s.graph.Search(r.Context(), key, q, includeArchived)
	if errors.Is(err, errs.ErrUnavailable) {
		writeAPIError(w, s.log, unavailable(degradedGraph).WithCause(err))
		return
	}
	if err != nil {
		writeAPIError(w, s.log, apierr.From(err))
		return
	}
	if nodes == nil {
		nodes = []knowledge.Node{}
	}
	writeJSON(w, s.log, http.StatusOK, nodes)
}

// handleKnowledgeGraph godoc
//
//	@Summary	Neighborhood traversal around an entity
//	@Tags		knowledge
//	@Produce	json
//	@Param		ws		path		string	true	"workspace"
//	@Param		team	path		string	true	"team"
//	@Param		proj	path		string	true	"project"
//	@Param		entity	query		string	true	"entity name or alias"
//	@Param		depth	query		int		false	"hop depth (default 1, max 10)"
//	@Success	200		{object}	Envelope{data=knowledge.Graph}
//	@Failure	400		{object}	Envelope
//	@Failure	503		{object}	Envelope	"graph unavailable (§5)"
//	@Router		/v1/{ws}/{team}/{proj}/knowledge/graph [get]
func (s *Server) handleKnowledgeGraph(w http.ResponseWriter, r *http.Request) {
	key := projectKey(r)
	if apiErr := validateProjectKey(key); apiErr != nil {
		writeAPIError(w, s.log, apiErr)
		return
	}
	entity := r.URL.Query().Get(paramEntity)
	if entity == "" {
		writeAPIError(w, s.log, badRequest(paramEntity+" is required"))
		return
	}
	depth, apiErr := parseDepthParam(r.URL.Query().Get(paramDepth))
	if apiErr != nil {
		writeAPIError(w, s.log, apiErr)
		return
	}
	if s.graph == nil {
		writeAPIError(w, s.log, unavailable(degradedGraph))
		return
	}

	s.statGate(r.Context(), key)

	sub, err := s.graph.Neighborhood(r.Context(), key, entity, depth)
	if errors.Is(err, errs.ErrUnavailable) {
		writeAPIError(w, s.log, unavailable(degradedGraph).WithCause(err))
		return
	}
	if err != nil {
		writeAPIError(w, s.log, apierr.From(err))
		return
	}
	writeJSON(w, s.log, http.StatusOK, sub)
}

// handlePatchNode godoc
//
//	@Summary	Transition node state (set_state / deprecate)
//	@Tags		knowledge
//	@Accept		json
//	@Produce	json
//	@Param		ws		path		string				true	"workspace"
//	@Param		team	path		string				true	"team"
//	@Param		proj	path		string				true	"project"
//	@Param		id		path		string				true	"node ULID"
//	@Param		body	body		PatchNodeRequest	true	"operation"
//	@Success	200		{object}	Envelope{data=NodeResponse}
//	@Failure	400		{object}	Envelope
//	@Failure	404		{object}	Envelope
//	@Failure	409		{object}	Envelope	"illegal state transition"
//	@Router		/v1/{ws}/{team}/{proj}/knowledge/nodes/{id} [patch]
//
// Contract: knowledge.Transition enforces legality (an illegal move is its
// Conflict, hence 409); hot write first, then best-effort MERGE.
func (s *Server) handlePatchNode(w http.ResponseWriter, r *http.Request) {
	key := projectKey(r)
	if apiErr := validateProjectKey(key); apiErr != nil {
		writeAPIError(w, s.log, apiErr)
		return
	}
	id := chi.URLParam(r, paramID)
	if apiErr := validateULID(id); apiErr != nil {
		writeAPIError(w, s.log, apiErr)
		return
	}
	if s.store == nil {
		writeAPIError(w, s.log, unavailable(msgHotStoreUnavailable))
		return
	}
	var req PatchNodeRequest
	if apiErr := decodeJSON(w, r, &req, false); apiErr != nil {
		writeAPIError(w, s.log, apiErr)
		return
	}
	target, apiErr := req.targetState()
	if apiErr != nil {
		writeAPIError(w, s.log, apiErr)
		return
	}

	// Lookup, transition and write share one closure: a patch computed against
	// a stale snapshot would resurrect every node a concurrent write added
	// after that snapshot was taken.
	var updated knowledge.Node
	err := s.store.UpdateKnowledge(r.Context(), key, func(g knowledge.Graph) (knowledge.Graph, error) {
		idx := slices.IndexFunc(g.Nodes, func(n knowledge.Node) bool { return n.ID == id })
		if idx < 0 {
			return g, notFound("node not found")
		}
		var err error
		updated, err = knowledge.Transition(g.Nodes[idx], target, req.Reason, s.clock.Now().UTC())
		if err != nil {
			return g, err
		}
		next := knowledge.Graph{Nodes: slices.Clone(g.Nodes), Edges: slices.Clone(g.Edges)}
		next.Nodes[idx] = updated
		return next, nil
	})
	if err != nil {
		writeAPIError(w, s.log, apierr.From(err))
		return
	}
	degraded := s.mirrorKnowledge(r.Context(), key, []knowledge.Node{updated}, nil)
	writeJSON(w, s.log, http.StatusOK, NodeResponse{Node: updated, Degraded: degraded})
}

// targetState resolves the requested operation to the state to transition into.
func (req PatchNodeRequest) targetState() (knowledge.State, *apierr.Error) {
	var target knowledge.State
	switch req.Op {
	case opSetState:
		if !knowledge.ValidState(req.State) {
			return "", badRequest("state must be one of active|archived|deprecated")
		}
		target = req.State
	case opDeprecate:
		target = knowledge.StateDeprecated
	default:
		return "", badRequest("op must be " + opSetState + " or " + opDeprecate)
	}
	if target == knowledge.StateDeprecated && req.Reason == "" {
		return "", badRequest("deprecation requires a reason")
	}
	return target, nil
}

// handlePurgeNode godoc
//
//	@Summary	Purge a node from hot (S3 versioning is the backstop)
//	@Tags		knowledge
//	@Produce	json
//	@Param		ws		path		string	true	"workspace"
//	@Param		team	path		string	true	"team"
//	@Param		proj	path		string	true	"project"
//	@Param		id		path		string	true	"node ULID"
//	@Param		confirm	query		bool	true	"must be true (§3)"
//	@Success	200		{object}	Envelope{data=PurgeNodeResponse}
//	@Failure	400		{object}	Envelope	"confirm missing"
//	@Failure	404		{object}	Envelope
//	@Failure	409		{object}	Envelope	"node is still active — archive or deprecate it first (§3)"
//	@Router		/v1/{ws}/{team}/{proj}/knowledge/nodes/{id} [delete]
//
// Contract: refuse without confirm=true — that check runs before id validation,
// so a destructive call is rejected for the destructive reason first. The
// lifecycle gate is knowledge.Purge's: §3 requires the archived/deprecated
// buffer before any deletion, so an active node is 409, not a silent wipe.
// Then best-effort delete it in Neo4j.
func (s *Server) handlePurgeNode(w http.ResponseWriter, r *http.Request) {
	key := projectKey(r)
	if apiErr := validateProjectKey(key); apiErr != nil {
		writeAPIError(w, s.log, apiErr)
		return
	}
	if r.URL.Query().Get(paramConfirm) != valueTrue {
		writeAPIError(w, s.log, badRequest("purge requires confirm=true (§3 — S3 versioning is the backstop)"))
		return
	}
	id := chi.URLParam(r, paramID)
	if apiErr := validateULID(id); apiErr != nil {
		writeAPIError(w, s.log, apiErr)
		return
	}
	if s.store == nil {
		writeAPIError(w, s.log, unavailable(msgHotStoreUnavailable))
		return
	}

	// Lookup, lifecycle gate and removal share one closure so a purge cannot
	// rewrite the graph from a snapshot taken before a concurrent node creation,
	// nor pass a gate against a state a concurrent PATCH has since changed.
	// knowledge owns both rules: a missing id is its NotFound, an unbuffered
	// node its Conflict, and apierr maps them to 404 and 409.
	var removedEdges int
	err := s.store.UpdateKnowledge(r.Context(), key, func(g knowledge.Graph) (knowledge.Graph, error) {
		next, removed, err := knowledge.Purge(g, id)
		if err != nil {
			return g, err
		}
		removedEdges = removed
		return next, nil
	})
	if err != nil {
		writeAPIError(w, s.log, apierr.From(err))
		return
	}

	var degraded []string
	if s.graph == nil {
		degraded = []string{degradedGraph}
		s.markDirty(r.Context(), key, hotstore.PlaneKnowledge)
	} else if err := s.graph.DeleteNode(r.Context(), key, id); err != nil {
		s.log.Warn("graph node delete failed; degraded", "project", key.String(), "id", id, "error", err)
		degraded = []string{degradedGraph}
		s.markDirty(r.Context(), key, hotstore.PlaneKnowledge)
	} else {
		s.markIndexed(r.Context(), key, hotstore.PlaneKnowledge)
	}
	writeJSON(w, s.log, http.StatusOK, PurgeNodeResponse{PurgedID: id, RemovedEdges: removedEdges, Degraded: degraded})
}
