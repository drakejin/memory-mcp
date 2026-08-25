package httpserver

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/drakejin/memory-mcp/internal/handler/app/httpserver/apierr"
	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/x/errs"
)

// CreateEpisodeRequest is the POST body for episode creation. The server
// assigns the ULID; consolidated always starts false. It mirrors
// episode.AppendRequest field for field — this transport copy exists because
// the §2.1 body invariants below are checked here, before the service runs.
type CreateEpisodeRequest struct {
	Kind       episode.Kind  `json:"kind"`
	OccurredAt time.Time     `json:"occurred_at"`
	Actor      episode.Actor `json:"actor"`
	Text       string        `json:"text"`
	Entities   []string      `json:"entities"`
	Refs       *episode.Refs `json:"refs,omitempty"`
}

// validate applies §2.1 invariants at the HTTP boundary.
func (req CreateEpisodeRequest) validate() *apierr.Error {
	if !episode.ValidKind(req.Kind) {
		return badRequest("kind must be one of event|conversation|decision|observation|document_chunk")
	}
	if !episode.ValidActor(req.Actor) {
		return badRequest("actor must be one of agent|user|system")
	}
	if req.Text == "" {
		return badRequest("text must not be empty")
	}
	if req.Kind != episode.KindDocumentChunk {
		if req.Refs != nil {
			return badRequest("refs is only valid for kind document_chunk")
		}
		return nil
	}
	if req.Refs == nil {
		return badRequest("refs is required for kind document_chunk")
	}
	if err := validateSHA(req.Refs.DocSHA); err != nil {
		return badRequest("refs.doc_sha must be a lowercase hex sha256")
	}
	if req.Refs.ChunkSeq < 0 {
		return badRequest("refs.chunk_seq must not be negative")
	}
	return nil
}

// HandleCreateEpisode godoc
//
//	@Summary	Append an episodic record (hot write + best-effort index)
//	@Tags		episodic
//	@Accept		json
//	@Produce	json
//	@Param		ws		path		string					true	"workspace"
//	@Param		team	path		string					true	"team"
//	@Param		proj	path		string					true	"project"
//	@Param		body	body		CreateEpisodeRequest	true	"record"
//	@Success	201		{object}	Envelope{data=episode.AppendResult}
//	@Failure	400		{object}	Envelope
//	@Failure	500		{object}	Envelope
//	@Router		/v1/{ws}/{team}/{proj}/episodes [post]
//
// Contract: validate key+body, then episode.Append owns the orchestration —
// ULID assignment, hot append (must succeed or 5xx), best-effort index with a
// degraded note on failure, still 201 (P1).
func (s *Handler) HandleCreateEpisode(w http.ResponseWriter, r *http.Request) {
	key := projectKey(r)
	if apiErr := validateProjectKey(key); apiErr != nil {
		writeAPIError(w, s.Log, apiErr)
		return
	}
	if s.Episodes == nil {
		writeAPIError(w, s.Log, unavailable(msgHotStoreUnavailable))
		return
	}
	var req CreateEpisodeRequest
	if apiErr := decodeJSON(w, r, &req, false); apiErr != nil {
		writeAPIError(w, s.Log, apiErr)
		return
	}
	if apiErr := req.validate(); apiErr != nil {
		writeAPIError(w, s.Log, apiErr)
		return
	}

	res, err := s.Episodes.Append(r.Context(), key, episode.AppendRequest{
		Kind:       req.Kind,
		OccurredAt: req.OccurredAt,
		Actor:      req.Actor,
		Text:       req.Text,
		Entities:   req.Entities,
		Refs:       req.Refs,
	})
	if err != nil {
		writeAPIError(w, s.Log, apierr.From(err))
		return
	}
	writeJSON(w, s.Log, http.StatusCreated, res)
}

// HandleSearchEpisodes godoc
//
//	@Summary	Full-text (nori) search over hot episodes
//	@Tags		episodic
//	@Produce	json
//	@Param		ws		path		string	true	"workspace"
//	@Param		team	path		string	true	"team"
//	@Param		proj	path		string	true	"project"
//	@Param		q		query		string	true	"query text"
//	@Param		from	query		string	false	"RFC3339 lower bound"
//	@Param		to		query		string	false	"RFC3339 upper bound"
//	@Param		kinds	query		string	false	"comma-separated kinds"
//	@Success	200		{object}	Envelope{data=[]episode.Hit}
//	@Failure	400		{object}	Envelope
//	@Failure	503		{object}	Envelope	"index unavailable (§5)"
//	@Router		/v1/{ws}/{team}/{proj}/episodes/search [get]
//
// Contract: episode.Search stat-gates first, answers excerpt+meta+score only
// (§7), and bumps recall best-effort. The handler must NOT gate again — the
// service already does, and a double gate would defeat the debounce.
func (s *Handler) HandleSearchEpisodes(w http.ResponseWriter, r *http.Request) {
	key := projectKey(r)
	if apiErr := validateProjectKey(key); apiErr != nil {
		writeAPIError(w, s.Log, apiErr)
		return
	}
	qs := r.URL.Query()
	text := qs.Get(paramQuery)
	if text == "" {
		writeAPIError(w, s.Log, badRequest(paramQuery+" is required"))
		return
	}
	from, apiErr := parseTimeParam(qs.Get(paramFrom), paramFrom)
	if apiErr != nil {
		writeAPIError(w, s.Log, apiErr)
		return
	}
	to, apiErr := parseTimeParam(qs.Get(paramTo), paramTo)
	if apiErr != nil {
		writeAPIError(w, s.Log, apiErr)
		return
	}
	kinds, apiErr := parseKindsParam(qs.Get(paramKinds))
	if apiErr != nil {
		writeAPIError(w, s.Log, apiErr)
		return
	}
	if s.Episodes == nil {
		writeAPIError(w, s.Log, unavailable(degradedSearch))
		return
	}

	res, err := s.Episodes.Search(r.Context(), key, episode.Query{Text: text, From: from, To: to, Kinds: kinds})
	if errors.Is(err, errs.ErrUnavailable) {
		// §5: a derived read on a dead store is an honest 503, worded exactly
		// like the degraded note a write would have carried.
		writeAPIError(w, s.Log, unavailable(degradedSearch).WithCause(err))
		return
	}
	if err != nil {
		writeAPIError(w, s.Log, apierr.From(err))
		return
	}
	// SearchResult.Hits is never nil, so the body stays a bare array.
	writeJSON(w, s.Log, http.StatusOK, res.Hits)
}

// HandleGetEpisode godoc
//
//	@Summary	Fetch one episode by id (hot first, cold archive fallback)
//	@Tags		episodic
//	@Produce	json
//	@Param		ws		path		string	true	"workspace"
//	@Param		team	path		string	true	"team"
//	@Param		proj	path		string	true	"project"
//	@Param		id		path		string	true	"episode ULID"
//	@Success	200		{object}	Envelope{data=episode.Record}
//	@Failure	400		{object}	Envelope
//	@Failure	404		{object}	Envelope
//	@Router		/v1/{ws}/{team}/{proj}/episodes/{id} [get]
//
// Contract: episode.Get looks hot up first, then the cold archive, so
// provenance links keep resolving after aging (§2, P11).
func (s *Handler) HandleGetEpisode(w http.ResponseWriter, r *http.Request) {
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
	if s.Episodes == nil {
		writeAPIError(w, s.Log, unavailable(msgHotStoreUnavailable))
		return
	}
	res, err := s.Episodes.Get(r.Context(), key, id)
	if err != nil {
		writeAPIError(w, s.Log, apierr.From(err))
		return
	}
	writeJSON(w, s.Log, http.StatusOK, res.Record)
}
