package server

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/drakejin/memory-mcp/internal/graph"
	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/knowledge"
	"github.com/drakejin/memory-mcp/internal/ulid"
)

// Seams to the knowledge package's pure graph operations. Production always
// uses the real functions; unit tests may substitute fakes so server tests
// stay independent of sibling-package progress.
var (
	supersedeGraph = knowledge.Supersede
	transitionNode = knowledge.Transition
)

// CreateNodeRequest is the POST body for knowledge node creation. Supersedes
// lists node ids this new node replaces (§7 supersede support); the state
// machine work happens via knowledge.Supersede.
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
func (req CreateNodeRequest) validate() error {
	if !isNodeKind(req.Kind) {
		return errors.New("kind must be one of entity|fact|lesson|preference|document")
	}
	if req.Name == "" {
		return errors.New("name must not be empty")
	}
	if !isTrust(req.Trust) {
		return errors.New("trust must be one of user-stated|agent-inferred|imported")
	}
	if err := validateULIDs(req.Provenance, "provenance"); err != nil {
		return err
	}
	if err := validateULIDs(req.Supersedes, "supersedes"); err != nil {
		return err
	}
	if req.ReviewAfter != "" {
		if _, err := time.Parse(time.RFC3339, req.ReviewAfter); err != nil {
			return errors.New("review_after must be RFC3339 or empty")
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
// graph, atomic WriteKnowledge, then best-effort Neo4j MERGE (degraded note on
// failure). No judgement calls server-side — supersede decisions are the
// agent's (§0 principle 2).
func (s *Server) handleCreateNode(w http.ResponseWriter, r *http.Request) {
	key := projectKey(r)
	if err := validateProjectKey(key); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.deps.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "hot store unavailable")
		return
	}
	var req CreateNodeRequest
	if err := decodeJSON(w, r, &req, false); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	now := s.deps.Clock.Now().UTC()
	node := knowledge.Node{
		ID:          ulid.At(now.UnixMilli()),
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

	g, err := s.deps.Store.ReadKnowledge(r.Context(), key)
	if err != nil {
		s.deps.Logger.Error("knowledge read failed", "project", key.String(), "error", err)
		writeError(w, http.StatusInternalServerError, "failed to read knowledge graph")
		return
	}

	var next knowledge.Graph
	if len(req.Supersedes) > 0 {
		next, err = supersedeGraph(g, node, req.Supersedes, now)
		if errors.Is(err, knowledge.ErrNodeNotFound) {
			writeError(w, http.StatusNotFound, "superseded node not found")
			return
		}
		if err != nil {
			s.deps.Logger.Error("supersede failed", "project", key.String(), "error", err)
			writeError(w, http.StatusInternalServerError, "supersede failed")
			return
		}
	} else {
		next = knowledge.Graph{
			Nodes: append(slices.Clone(g.Nodes), node),
			Edges: slices.Clone(g.Edges),
		}
	}
	if err := s.deps.Store.WriteKnowledge(r.Context(), key, next); err != nil {
		s.deps.Logger.Error("knowledge hot write failed", "project", key.String(), "error", err)
		writeError(w, http.StatusInternalServerError, "failed to persist knowledge node")
		return
	}

	stored := findGraphNode(next, node.ID, node)
	affected := affectedNodes(next, node.ID, req.Supersedes)
	newEdges := diffEdges(g, next)
	degraded := s.mirrorKnowledge(r, key, affected, newEdges)
	writeJSON(w, http.StatusCreated, NodeResponse{Node: stored, Degraded: degraded})
}

// findGraphNode returns the node with id from g, or fallback when absent.
func findGraphNode(g knowledge.Graph, id string, fallback knowledge.Node) knowledge.Node {
	for _, n := range g.Nodes {
		if n.ID == id {
			return n
		}
	}
	return fallback
}

// affectedNodes collects the new node plus every superseded node in its
// post-transition form, for a minimal best-effort MERGE.
func affectedNodes(g knowledge.Graph, newID string, superseded []string) []knowledge.Node {
	want := map[string]bool{newID: true}
	for _, id := range superseded {
		want[id] = true
	}
	var out []knowledge.Node
	for _, n := range g.Nodes {
		if want[n.ID] {
			out = append(out, n)
		}
	}
	return out
}

// diffEdges returns edges present in next but not in prev (from/to/rel match).
func diffEdges(prev, next knowledge.Graph) []knowledge.Edge {
	seen := make(map[[3]string]bool, len(prev.Edges))
	for _, e := range prev.Edges {
		seen[[3]string{e.From, e.To, string(e.Rel)}] = true
	}
	var out []knowledge.Edge
	for _, e := range next.Edges {
		if !seen[[3]string{e.From, e.To, string(e.Rel)}] {
			out = append(out, e)
		}
	}
	return out
}

// mirrorKnowledge best-effort MERGEs nodes/edges into Neo4j and maintains the
// manifest. Returns degraded notes for the response envelope (§5).
func (s *Server) mirrorKnowledge(r *http.Request, key hotstore.ProjectKey, nodes []knowledge.Node, edges []knowledge.Edge) []string {
	if s.deps.Graph == nil {
		s.markDirty(r.Context(), key, hotstore.PlaneKnowledge)
		return []string{degradedGraph}
	}
	err := error(nil)
	if len(nodes) > 0 {
		err = s.deps.Graph.UpsertNodes(r.Context(), key, nodes)
	}
	if err == nil && len(edges) > 0 {
		err = s.deps.Graph.UpsertEdges(r.Context(), key, edges)
	}
	if err != nil {
		s.deps.Logger.Warn("knowledge graph mirror failed; degraded", "project", key.String(), "error", err)
		s.markDirty(r.Context(), key, hotstore.PlaneKnowledge)
		return []string{degradedGraph}
	}
	s.markIndexed(r.Context(), key, hotstore.PlaneKnowledge)
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
	if err := validateProjectKey(key); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.deps.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "hot store unavailable")
		return
	}
	var edge knowledge.Edge
	if err := decodeJSON(w, r, &edge, false); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !ulid.IsULID(edge.From) || !ulid.IsULID(edge.To) {
		writeError(w, http.StatusBadRequest, "from and to must be node ULIDs")
		return
	}
	if !isRel(edge.Rel) {
		writeError(w, http.StatusBadRequest, "rel must be one of relates_to|derived_from|supersedes|about")
		return
	}
	if edge.Confidence < 0 || edge.Confidence > 1 {
		writeError(w, http.StatusBadRequest, "confidence must be within [0,1]")
		return
	}
	if err := validateULIDs(edge.Provenance, "provenance"); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	edge.Provenance = normalizeStrings(edge.Provenance)

	g, err := s.deps.Store.ReadKnowledge(r.Context(), key)
	if err != nil {
		s.deps.Logger.Error("knowledge read failed", "project", key.String(), "error", err)
		writeError(w, http.StatusInternalServerError, "failed to read knowledge graph")
		return
	}
	if !nodeExists(g, edge.From) || !nodeExists(g, edge.To) {
		writeError(w, http.StatusNotFound, "edge endpoint node not found")
		return
	}

	// Idempotent on (from,to,rel): re-posting replaces the edge.
	next := knowledge.Graph{Nodes: slices.Clone(g.Nodes), Edges: make([]knowledge.Edge, 0, len(g.Edges)+1)}
	replaced := false
	for _, e := range g.Edges {
		if e.From == edge.From && e.To == edge.To && e.Rel == edge.Rel {
			next.Edges = append(next.Edges, edge)
			replaced = true
			continue
		}
		next.Edges = append(next.Edges, e)
	}
	if !replaced {
		next.Edges = append(next.Edges, edge)
	}

	if err := s.deps.Store.WriteKnowledge(r.Context(), key, next); err != nil {
		s.deps.Logger.Error("knowledge hot write failed", "project", key.String(), "error", err)
		writeError(w, http.StatusInternalServerError, "failed to persist knowledge edge")
		return
	}
	degraded := s.mirrorKnowledge(r, key, nil, []knowledge.Edge{edge})
	writeJSON(w, http.StatusCreated, EdgeResponse{Edge: edge, Degraded: degraded})
}

func nodeExists(g knowledge.Graph, id string) bool {
	for _, n := range g.Nodes {
		if n.ID == id {
			return true
		}
	}
	return false
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
	if err := validateProjectKey(key); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	q := r.URL.Query().Get("q")
	if q == "" {
		writeError(w, http.StatusBadRequest, "q is required")
		return
	}
	includeArchived := r.URL.Query().Get("include_archived") == "true"
	if s.deps.Graph == nil {
		writeError(w, http.StatusServiceUnavailable, degradedGraph)
		return
	}

	s.statGate(r.Context(), key)

	nodes, err := s.deps.Graph.Search(r.Context(), key, q, includeArchived)
	if errors.Is(err, graph.ErrUnavailable) {
		writeError(w, http.StatusServiceUnavailable, degradedGraph)
		return
	}
	if err != nil {
		s.deps.Logger.Error("knowledge search failed", "project", key.String(), "error", err)
		writeError(w, http.StatusInternalServerError, "knowledge search failed")
		return
	}
	if nodes == nil {
		nodes = []knowledge.Node{}
	}
	writeJSON(w, http.StatusOK, nodes)
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
	if err := validateProjectKey(key); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	entity := r.URL.Query().Get("entity")
	if entity == "" {
		writeError(w, http.StatusBadRequest, "entity is required")
		return
	}
	depth := defaultGraphDepth
	if raw := r.URL.Query().Get("depth"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > maxGraphDepth {
			writeError(w, http.StatusBadRequest, "depth must be an integer between 1 and 10")
			return
		}
		depth = parsed
	}
	if s.deps.Graph == nil {
		writeError(w, http.StatusServiceUnavailable, degradedGraph)
		return
	}

	s.statGate(r.Context(), key)

	sub, err := s.deps.Graph.Neighborhood(r.Context(), key, entity, depth)
	if errors.Is(err, graph.ErrUnavailable) {
		writeError(w, http.StatusServiceUnavailable, degradedGraph)
		return
	}
	if err != nil {
		s.deps.Logger.Error("knowledge traversal failed", "project", key.String(), "error", err)
		writeError(w, http.StatusInternalServerError, "knowledge traversal failed")
		return
	}
	writeJSON(w, http.StatusOK, sub)
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
// Contract: knowledge.Transition enforces legality; hot write first, then
// best-effort MERGE.
func (s *Server) handlePatchNode(w http.ResponseWriter, r *http.Request) {
	key := projectKey(r)
	if err := validateProjectKey(key); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id := chi.URLParam(r, "id")
	if !ulid.IsULID(id) {
		writeError(w, http.StatusBadRequest, "id must be a ULID")
		return
	}
	if s.deps.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "hot store unavailable")
		return
	}
	var req PatchNodeRequest
	if err := decodeJSON(w, r, &req, false); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var target knowledge.State
	switch req.Op {
	case "set_state":
		if !isNodeState(req.State) {
			writeError(w, http.StatusBadRequest, "state must be one of active|archived|deprecated")
			return
		}
		target = req.State
	case "deprecate":
		target = knowledge.StateDeprecated
	default:
		writeError(w, http.StatusBadRequest, "op must be set_state or deprecate")
		return
	}
	if target == knowledge.StateDeprecated && req.Reason == "" {
		writeError(w, http.StatusBadRequest, "deprecation requires a reason")
		return
	}

	g, err := s.deps.Store.ReadKnowledge(r.Context(), key)
	if err != nil {
		s.deps.Logger.Error("knowledge read failed", "project", key.String(), "error", err)
		writeError(w, http.StatusInternalServerError, "failed to read knowledge graph")
		return
	}
	idx := -1
	for i, n := range g.Nodes {
		if n.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeError(w, http.StatusNotFound, "node not found")
		return
	}

	now := s.deps.Clock.Now().UTC()
	updated, err := transitionNode(g.Nodes[idx], target, req.Reason, now)
	if errors.Is(err, knowledge.ErrInvalidTransition) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	next := knowledge.Graph{Nodes: slices.Clone(g.Nodes), Edges: slices.Clone(g.Edges)}
	next.Nodes[idx] = updated
	if err := s.deps.Store.WriteKnowledge(r.Context(), key, next); err != nil {
		s.deps.Logger.Error("knowledge hot write failed", "project", key.String(), "error", err)
		writeError(w, http.StatusInternalServerError, "failed to persist state transition")
		return
	}
	degraded := s.mirrorKnowledge(r, key, []knowledge.Node{updated}, nil)
	writeJSON(w, http.StatusOK, NodeResponse{Node: updated, Degraded: degraded})
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
//	@Router		/v1/{ws}/{team}/{proj}/knowledge/nodes/{id} [delete]
//
// Contract: refuse without confirm=true; remove node + incident edges from hot
// and best-effort DeleteNode in Neo4j.
func (s *Server) handlePurgeNode(w http.ResponseWriter, r *http.Request) {
	key := projectKey(r)
	if err := validateProjectKey(key); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if r.URL.Query().Get("confirm") != "true" {
		writeError(w, http.StatusBadRequest, "purge requires confirm=true (§3 — S3 versioning is the backstop)")
		return
	}
	id := chi.URLParam(r, "id")
	if !ulid.IsULID(id) {
		writeError(w, http.StatusBadRequest, "id must be a ULID")
		return
	}
	if s.deps.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "hot store unavailable")
		return
	}

	g, err := s.deps.Store.ReadKnowledge(r.Context(), key)
	if err != nil {
		s.deps.Logger.Error("knowledge read failed", "project", key.String(), "error", err)
		writeError(w, http.StatusInternalServerError, "failed to read knowledge graph")
		return
	}
	if !nodeExists(g, id) {
		writeError(w, http.StatusNotFound, "node not found")
		return
	}

	next := knowledge.Graph{Nodes: make([]knowledge.Node, 0, len(g.Nodes)-1), Edges: make([]knowledge.Edge, 0, len(g.Edges))}
	for _, n := range g.Nodes {
		if n.ID != id {
			next.Nodes = append(next.Nodes, n)
		}
	}
	removedEdges := 0
	for _, e := range g.Edges {
		if e.From == id || e.To == id {
			removedEdges++
			continue
		}
		next.Edges = append(next.Edges, e)
	}
	if err := s.deps.Store.WriteKnowledge(r.Context(), key, next); err != nil {
		s.deps.Logger.Error("knowledge hot write failed", "project", key.String(), "error", err)
		writeError(w, http.StatusInternalServerError, "failed to persist purge")
		return
	}

	var degraded []string
	if s.deps.Graph == nil {
		degraded = []string{degradedGraph}
		s.markDirty(r.Context(), key, hotstore.PlaneKnowledge)
	} else if err := s.deps.Graph.DeleteNode(r.Context(), key, id); err != nil {
		s.deps.Logger.Warn("graph node delete failed; degraded", "project", key.String(), "id", id, "error", err)
		degraded = []string{degradedGraph}
		s.markDirty(r.Context(), key, hotstore.PlaneKnowledge)
	} else {
		s.markIndexed(r.Context(), key, hotstore.PlaneKnowledge)
	}
	writeJSON(w, http.StatusOK, PurgeNodeResponse{PurgedID: id, RemovedEdges: removedEdges, Degraded: degraded})
}
