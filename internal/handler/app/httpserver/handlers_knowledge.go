package httpserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/drakejin/memory-mcp/internal/handler/app/httpserver/apierr"
	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/service/knowledge"
	"github.com/drakejin/memory-mcp/internal/x/errs"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// HandleCreateNode godoc
//
//	@Summary	Create a knowledge node (optionally superseding others)
//	@Tags		knowledge
//	@Accept		json
//	@Produce	json
//	@Param		ws		path		string						true	"workspace"
//	@Param		team	path		string						true	"team"
//	@Param		proj	path		string						true	"project"
//	@Param		body	body		knowledge.CreateNodeInput	true	"node"
//	@Success	201		{object}	Envelope{data=knowledge.NodeResult}
//	@Failure	400		{object}	Envelope
//	@Failure	404		{object}	Envelope	"superseded node not found"
//	@Router		/v1/{ws}/{team}/{proj}/knowledge/nodes [post]
//
// Contract: knowledge.CreateNode owns validation, the ULID, the atomic
// supersede write and the best-effort mirror; the handler then runs the §3
// consolidation promotion of the provenance episodes through the episodic
// service and appends its notes — the one cross-plane step, so it lives at
// the boundary that holds both services.
func (s *Handler) HandleCreateNode(w http.ResponseWriter, r *http.Request) {
	key := projectKey(r)
	if apiErr := validateProjectKey(key); apiErr != nil {
		writeAPIError(w, s.Log, apiErr)
		return
	}
	if s.Knowledge == nil {
		writeAPIError(w, s.Log, unavailable(msgHotStoreUnavailable))
		return
	}
	var in knowledge.CreateNodeInput
	if apiErr := decodeJSON(w, r, &in, false); apiErr != nil {
		writeAPIError(w, s.Log, apiErr)
		return
	}

	res, err := s.Knowledge.CreateNode(r.Context(), key, in)
	if err != nil {
		writeAPIError(w, s.Log, apierr.From(err))
		return
	}
	res.Degraded = append(res.Degraded, s.promoteProvenance(r.Context(), key, res.Node.Provenance)...)
	writeJSON(w, s.Log, http.StatusCreated, res)
}

// promoteProvenance marks the distilled episodes consolidated (§3) through
// the episodic service, best-effort. Without an episodic service the
// bookkeeping cannot run at all, which is exactly what the degraded note
// discloses (§5) — the knowledge write has already succeeded either way.
func (s *Handler) promoteProvenance(ctx context.Context, key projectkey.Key, provenance []string) []string {
	if len(provenance) == 0 {
		return nil
	}
	if s.Episodes == nil {
		return []string{episode.DegradedPromotion}
	}
	return s.Episodes.Promote(ctx, key, provenance)
}

// HandleCreateEdge godoc
//
//	@Summary	Create a knowledge edge
//	@Tags		knowledge
//	@Accept		json
//	@Produce	json
//	@Param		ws		path		string			true	"workspace"
//	@Param		team	path		string			true	"team"
//	@Param		proj	path		string			true	"project"
//	@Param		body	body		knowledge.Edge	true	"edge"
//	@Success	201		{object}	Envelope{data=knowledge.EdgeResult}
//	@Failure	400		{object}	Envelope
//	@Failure	404		{object}	Envelope	"endpoint node not found"
//	@Router		/v1/{ws}/{team}/{proj}/knowledge/edges [post]
//
// Contract: transport-shape checks here, then knowledge.CreateEdge — both
// endpoints must exist in the hot graph, hot commits first, mirror follows
// best-effort. The domain validator also carries the P5 self-supersede gate.
func (s *Handler) HandleCreateEdge(w http.ResponseWriter, r *http.Request) {
	key := projectKey(r)
	if apiErr := validateProjectKey(key); apiErr != nil {
		writeAPIError(w, s.Log, apiErr)
		return
	}
	if s.Knowledge == nil {
		writeAPIError(w, s.Log, unavailable(msgHotStoreUnavailable))
		return
	}
	var edge knowledge.Edge
	if apiErr := decodeJSON(w, r, &edge, false); apiErr != nil {
		writeAPIError(w, s.Log, apiErr)
		return
	}
	if apiErr := validateEdge(edge); apiErr != nil {
		writeAPIError(w, s.Log, apiErr)
		return
	}

	res, err := s.Knowledge.CreateEdge(r.Context(), key, edge)
	if err != nil {
		writeAPIError(w, s.Log, apierr.From(err))
		return
	}
	writeJSON(w, s.Log, http.StatusCreated, res)
}

// HandleSearchKnowledge godoc
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
func (s *Handler) HandleSearchKnowledge(w http.ResponseWriter, r *http.Request) {
	key := projectKey(r)
	if apiErr := validateProjectKey(key); apiErr != nil {
		writeAPIError(w, s.Log, apiErr)
		return
	}
	q := r.URL.Query().Get(paramQuery)
	if q == "" {
		writeAPIError(w, s.Log, badRequest(paramQuery+" is required"))
		return
	}
	includeArchived := r.URL.Query().Get(paramIncludeArchived) == valueTrue
	if s.Knowledge == nil {
		writeAPIError(w, s.Log, unavailable(degradedGraph))
		return
	}

	// The stat-gate is the F17 request-entry mechanism, not knowledge
	// orchestration, so it stays at the boundary (the episodic search gates
	// inside its service instead — its recall bump depends on gate order).
	s.statGate(r.Context(), key)

	nodes, err := s.Knowledge.Search(r.Context(), key, q, includeArchived)
	if errors.Is(err, errs.ErrUnavailable) {
		writeAPIError(w, s.Log, unavailable(degradedGraph).WithCause(err))
		return
	}
	if err != nil {
		writeAPIError(w, s.Log, apierr.From(err))
		return
	}
	writeJSON(w, s.Log, http.StatusOK, nodes)
}

// HandleKnowledgeGraph godoc
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
func (s *Handler) HandleKnowledgeGraph(w http.ResponseWriter, r *http.Request) {
	key := projectKey(r)
	if apiErr := validateProjectKey(key); apiErr != nil {
		writeAPIError(w, s.Log, apiErr)
		return
	}
	entity := r.URL.Query().Get(paramEntity)
	if entity == "" {
		writeAPIError(w, s.Log, badRequest(paramEntity+" is required"))
		return
	}
	depth, apiErr := parseDepthParam(r.URL.Query().Get(paramDepth))
	if apiErr != nil {
		writeAPIError(w, s.Log, apiErr)
		return
	}
	if s.Knowledge == nil {
		writeAPIError(w, s.Log, unavailable(degradedGraph))
		return
	}

	s.statGate(r.Context(), key)

	sub, err := s.Knowledge.Neighborhood(r.Context(), key, entity, depth)
	if errors.Is(err, errs.ErrUnavailable) {
		writeAPIError(w, s.Log, unavailable(degradedGraph).WithCause(err))
		return
	}
	if err != nil {
		writeAPIError(w, s.Log, apierr.From(err))
		return
	}
	writeJSON(w, s.Log, http.StatusOK, sub)
}

// HandlePatchNode godoc
//
//	@Summary	Transition node state (set_state / deprecate)
//	@Tags		knowledge
//	@Accept		json
//	@Produce	json
//	@Param		ws		path		string						true	"workspace"
//	@Param		team	path		string						true	"team"
//	@Param		proj	path		string						true	"project"
//	@Param		id		path		string						true	"node ULID"
//	@Param		body	body		knowledge.PatchNodeInput	true	"operation"
//	@Success	200		{object}	Envelope{data=knowledge.NodeResult}
//	@Failure	400		{object}	Envelope
//	@Failure	404		{object}	Envelope
//	@Failure	409		{object}	Envelope	"illegal state transition"
//	@Router		/v1/{ws}/{team}/{proj}/knowledge/nodes/{id} [patch]
//
// Contract: knowledge.PatchNode resolves the op and enforces legality (an
// illegal move is the state machine's Conflict, hence 409); hot write first,
// then best-effort mirror.
func (s *Handler) HandlePatchNode(w http.ResponseWriter, r *http.Request) {
	key := projectKey(r)
	if apiErr := validateProjectKey(key); apiErr != nil {
		writeAPIError(w, s.Log, apiErr)
		return
	}
	id := chi.URLParam(r, paramID)
	if apiErr := validateULID(id); apiErr != nil {
		writeAPIError(w, s.Log, apiErr)
		return
	}
	if s.Knowledge == nil {
		writeAPIError(w, s.Log, unavailable(msgHotStoreUnavailable))
		return
	}
	var in knowledge.PatchNodeInput
	if apiErr := decodeJSON(w, r, &in, false); apiErr != nil {
		writeAPIError(w, s.Log, apiErr)
		return
	}

	res, err := s.Knowledge.PatchNode(r.Context(), key, id, in)
	if err != nil {
		writeAPIError(w, s.Log, apierr.From(err))
		return
	}
	writeJSON(w, s.Log, http.StatusOK, res)
}

// HandlePurgeNode godoc
//
//	@Summary	Purge a node from hot (S3 versioning is the backstop)
//	@Tags		knowledge
//	@Produce	json
//	@Param		ws		path		string	true	"workspace"
//	@Param		team	path		string	true	"team"
//	@Param		proj	path		string	true	"project"
//	@Param		id		path		string	true	"node ULID"
//	@Param		confirm	query		bool	true	"must be true (§3)"
//	@Success	200		{object}	Envelope{data=knowledge.PurgeResult}
//	@Failure	400		{object}	Envelope	"confirm missing"
//	@Failure	404		{object}	Envelope
//	@Failure	409		{object}	Envelope	"node is still active — archive or deprecate it first (§3)"
//	@Router		/v1/{ws}/{team}/{proj}/knowledge/nodes/{id} [delete]
//
// Contract: knowledge.PurgeNode refuses without confirm=true — that gate runs
// before id validation, so a destructive call is rejected for the destructive
// reason first (P5). §3 requires the archived/deprecated buffer before any
// deletion, so an active node is 409, not a silent wipe. Then the node is
// best-effort deleted from the mirror.
func (s *Handler) HandlePurgeNode(w http.ResponseWriter, r *http.Request) {
	key := projectKey(r)
	if apiErr := validateProjectKey(key); apiErr != nil {
		writeAPIError(w, s.Log, apiErr)
		return
	}
	// The confirm and id gates run before the service-availability check, in
	// this order, so a destructive call is rejected for the destructive reason
	// first (P5) — the wording matches the service's own gates byte for byte.
	if r.URL.Query().Get(paramConfirm) != valueTrue {
		writeAPIError(w, s.Log, badRequest("purge requires confirm=true (§3 — S3 versioning is the backstop)"))
		return
	}
	id := chi.URLParam(r, paramID)
	if apiErr := validateULID(id); apiErr != nil {
		writeAPIError(w, s.Log, apiErr)
		return
	}
	if s.Knowledge == nil {
		writeAPIError(w, s.Log, unavailable(msgHotStoreUnavailable))
		return
	}

	res, err := s.Knowledge.PurgeNode(r.Context(), key, id, true)
	if err != nil {
		writeAPIError(w, s.Log, apierr.From(err))
		return
	}
	writeJSON(w, s.Log, http.StatusOK, res)
}
