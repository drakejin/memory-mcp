package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/drakejin/memory-mcp/internal/cold"
	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/search"
	"github.com/drakejin/memory-mcp/internal/ulid"
)

// CreateEpisodeRequest is the POST body for episode creation. The server
// assigns the ULID; consolidated always starts false.
type CreateEpisodeRequest struct {
	Kind       episodic.Kind  `json:"kind"`
	OccurredAt time.Time      `json:"occurred_at"`
	Actor      episodic.Actor `json:"actor"`
	Text       string         `json:"text"`
	Entities   []string       `json:"entities"`
	Refs       *episodic.Refs `json:"refs,omitempty"`
}

// CreateEpisodeResponse reports the stored record plus degraded notes when a
// derived upsert failed best-effort (§5 — never a 503 on write).
type CreateEpisodeResponse struct {
	Record   episodic.Record `json:"record"`
	Degraded []string        `json:"degraded,omitempty"`
}

// validate applies §2.1 invariants at the HTTP boundary.
func (req CreateEpisodeRequest) validate() error {
	if !isEpisodicKind(req.Kind) {
		return errors.New("kind must be one of event|conversation|decision|observation|document_chunk")
	}
	if !isActor(req.Actor) {
		return errors.New("actor must be one of agent|user|system")
	}
	if req.Text == "" {
		return errors.New("text must not be empty")
	}
	if req.Kind == episodic.KindDocumentChunk {
		if req.Refs == nil {
			return errors.New("refs is required for kind document_chunk")
		}
		if err := validateSHA(req.Refs.DocSHA); err != nil {
			return errors.New("refs.doc_sha must be a lowercase hex sha256")
		}
		if req.Refs.ChunkSeq < 0 {
			return errors.New("refs.chunk_seq must not be negative")
		}
	} else if req.Refs != nil {
		return errors.New("refs is only valid for kind document_chunk")
	}
	return nil
}

// handleCreateEpisode godoc
//
//	@Summary	Append an episodic record (hot write + best-effort index)
//	@Tags		episodic
//	@Accept		json
//	@Produce	json
//	@Param		ws		path		string					true	"workspace"
//	@Param		team	path		string					true	"team"
//	@Param		proj	path		string					true	"project"
//	@Param		body	body		CreateEpisodeRequest	true	"record"
//	@Success	201		{object}	Envelope{data=CreateEpisodeResponse}
//	@Failure	400		{object}	Envelope
//	@Failure	500		{object}	Envelope
//	@Router		/v1/{ws}/{team}/{proj}/episodes [post]
//
// Contract: validate key+body, assign ULID, hot append (must succeed or 5xx),
// then best-effort index — on index failure mark manifest dirty and report
// degraded, still 201.
func (s *Server) handleCreateEpisode(w http.ResponseWriter, r *http.Request) {
	key := projectKey(r)
	if err := validateProjectKey(key); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.deps.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "hot store unavailable")
		return
	}
	var req CreateEpisodeRequest
	if err := decodeJSON(w, r, &req, false); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	now := s.deps.Clock.Now().UTC()
	occurred := req.OccurredAt
	if occurred.IsZero() {
		occurred = now
	}
	rec := episodic.Record{
		ID:           ulid.At(now.UnixMilli()),
		Kind:         req.Kind,
		OccurredAt:   occurred.UTC(),
		Actor:        req.Actor,
		Text:         req.Text,
		Entities:     normalizeStrings(req.Entities),
		Refs:         req.Refs,
		Consolidated: false,
	}
	if err := s.deps.Store.AppendEpisode(r.Context(), key, rec); err != nil {
		s.deps.Logger.Error("episode hot append failed", "project", key.String(), "error", err)
		writeError(w, http.StatusInternalServerError, "failed to persist episode")
		return
	}

	resp := CreateEpisodeResponse{Record: rec}
	if s.deps.Index == nil {
		resp.Degraded = append(resp.Degraded, degradedSearch)
		s.markDirty(r.Context(), key, hotstore.PlaneEpisodic)
	} else if err := s.deps.Index.IndexRecords(r.Context(), key, []episodic.Record{rec}); err != nil {
		s.deps.Logger.Warn("episode index upsert failed; degraded", "project", key.String(), "error", err)
		resp.Degraded = append(resp.Degraded, degradedSearch)
		s.markDirty(r.Context(), key, hotstore.PlaneEpisodic)
	} else {
		s.markIndexed(r.Context(), key, hotstore.PlaneEpisodic)
	}
	writeJSON(w, http.StatusCreated, resp)
}

// handleSearchEpisodes godoc
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
//	@Success	200		{object}	Envelope{data=[]search.Hit}
//	@Failure	400		{object}	Envelope
//	@Failure	503		{object}	Envelope	"index unavailable (§5)"
//	@Router		/v1/{ws}/{team}/{proj}/episodes/search [get]
//
// Contract: stat-gate first; hits carry excerpt+meta+score only (§7); bumps
// recall_count/last_recalled best-effort. 503 when OpenSearch is down.
func (s *Server) handleSearchEpisodes(w http.ResponseWriter, r *http.Request) {
	key := projectKey(r)
	if err := validateProjectKey(key); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	qs := r.URL.Query()
	text := qs.Get("q")
	if text == "" {
		writeError(w, http.StatusBadRequest, "q is required")
		return
	}
	from, err := parseTimeParam(qs.Get("from"), "from")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	to, err := parseTimeParam(qs.Get("to"), "to")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	kinds, err := parseKindsParam(qs.Get("kinds"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.deps.Index == nil {
		writeError(w, http.StatusServiceUnavailable, degradedSearch)
		return
	}

	s.statGate(r.Context(), key)

	hits, err := s.deps.Index.Search(r.Context(), key, search.Query{Text: text, From: from, To: to, Kinds: kinds})
	if errors.Is(err, search.ErrUnavailable) {
		writeError(w, http.StatusServiceUnavailable, degradedSearch)
		return
	}
	if err != nil {
		s.deps.Logger.Error("episode search failed", "project", key.String(), "error", err)
		writeError(w, http.StatusInternalServerError, "search failed")
		return
	}
	if hits == nil {
		hits = []search.Hit{}
	}
	s.bumpRecall(r, key, hits)
	writeJSON(w, http.StatusOK, hits)
}

// bumpRecall updates recall_count/last_recalled on hot records best-effort;
// a failure is logged, never surfaced — the search result is already correct.
func (s *Server) bumpRecall(r *http.Request, key hotstore.ProjectKey, hits []search.Hit) {
	if s.deps.Store == nil || len(hits) == 0 {
		return
	}
	ids := make([]string, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.Record.ID)
	}
	now := s.deps.Clock.Now().UTC().Format(time.RFC3339)
	err := s.deps.Store.UpdateEpisodes(r.Context(), key, ids, func(rec episodic.Record) episodic.Record {
		rec.RecallCount++
		rec.LastRecalled = now
		return rec
	})
	if err != nil {
		s.deps.Logger.Warn("recall bump failed", "project", key.String(), "error", err)
		return
	}
	s.convergeRecall(r.Context(), key, ids)
}

// convergeRecall re-indexes the records a recall bump just rewrote and
// refreshes manifest freshness — the same close-the-loop the create path does.
//
// It is not optional bookkeeping. recall_count and last_recalled are mapped,
// indexed fields (search/mapping.go), so a hot-only bump leaves the derived doc
// stale. Worse, the hydration sha covers the whole hot file: without this, the
// first search after any rehydration permanently flips CheckDrift to "episodic
// hot content changed since last hydration" (§5) — and it can never settle,
// because the reindex that would clear it is itself re-dirtied by the next
// search. The stale mtime also made the stat-gate rehydrate the entire project
// on every search more than the 2s debounce apart.
func (s *Server) convergeRecall(ctx context.Context, key hotstore.ProjectKey, ids []string) {
	if s.deps.Index == nil {
		s.markDirty(ctx, key, hotstore.PlaneEpisodic)
		return
	}
	// Re-read from hot rather than mutating the hit copies: hot is canonical
	// (§0 principle 1) and the index copy may lag it.
	recs, err := s.deps.Store.ListEpisodes(ctx, key)
	if err != nil {
		s.deps.Logger.Warn("recall converge: list episodes failed", "project", key.String(), "error", err)
		s.markDirty(ctx, key, hotstore.PlaneEpisodic)
		return
	}
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	bumped := make([]episodic.Record, 0, len(ids))
	for _, rec := range recs {
		if wanted[rec.ID] {
			bumped = append(bumped, rec)
		}
	}
	if len(bumped) == 0 {
		return
	}
	if err := s.deps.Index.IndexRecords(ctx, key, bumped); err != nil {
		s.deps.Logger.Warn("recall converge: index upsert failed; degraded", "project", key.String(), "error", err)
		s.markDirty(ctx, key, hotstore.PlaneEpisodic)
		return
	}
	s.markIndexed(ctx, key, hotstore.PlaneEpisodic)
}

// handleGetEpisode godoc
//
//	@Summary	Fetch one episode by id (hot first, cold archive fallback)
//	@Tags		episodic
//	@Produce	json
//	@Param		ws		path		string	true	"workspace"
//	@Param		team	path		string	true	"team"
//	@Param		proj	path		string	true	"project"
//	@Param		id		path		string	true	"episode ULID"
//	@Success	200		{object}	Envelope{data=episodic.Record}
//	@Failure	400		{object}	Envelope
//	@Failure	404		{object}	Envelope
//	@Router		/v1/{ws}/{team}/{proj}/episodes/{id} [get]
//
// Contract: hot lookup, then Archiver.FetchArchivedEpisode so provenance links
// keep resolving after aging (§2).
func (s *Server) handleGetEpisode(w http.ResponseWriter, r *http.Request) {
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
	rec, err := s.deps.Store.GetEpisode(r.Context(), key, id)
	if err == nil {
		writeJSON(w, http.StatusOK, rec)
		return
	}
	if !errors.Is(err, hotstore.ErrNotFound) {
		s.deps.Logger.Error("episode hot lookup failed", "project", key.String(), "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "episode lookup failed")
		return
	}
	// Aged to cold? Provenance links must keep resolving (§2).
	if s.deps.Archiver == nil {
		writeError(w, http.StatusNotFound, "episode not in hot store; cold archive unavailable")
		return
	}
	rec, err = s.deps.Archiver.FetchArchivedEpisode(r.Context(), key, id)
	if err == nil {
		writeJSON(w, http.StatusOK, rec)
		return
	}
	if errors.Is(err, cold.ErrNotFound) || errors.Is(err, hotstore.ErrNotFound) {
		writeError(w, http.StatusNotFound, "episode not found")
		return
	}
	s.deps.Logger.Error("episode archive lookup failed", "project", key.String(), "id", id, "error", err)
	writeError(w, http.StatusInternalServerError, "archive lookup failed")
}
