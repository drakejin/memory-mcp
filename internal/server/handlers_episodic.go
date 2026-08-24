package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/errs"
	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/search"
	"github.com/drakejin/memory-mcp/internal/server/apierr"
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
func (req CreateEpisodeRequest) validate() *apierr.Error {
	if !episodic.ValidKind(req.Kind) {
		return badRequest("kind must be one of event|conversation|decision|observation|document_chunk")
	}
	if !episodic.ValidActor(req.Actor) {
		return badRequest("actor must be one of agent|user|system")
	}
	if req.Text == "" {
		return badRequest("text must not be empty")
	}
	if req.Kind != episodic.KindDocumentChunk {
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
	if apiErr := validateProjectKey(key); apiErr != nil {
		writeAPIError(w, s.log, apiErr)
		return
	}
	if s.store == nil {
		writeAPIError(w, s.log, unavailable(msgHotStoreUnavailable))
		return
	}
	var req CreateEpisodeRequest
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
	occurred := req.OccurredAt
	if occurred.IsZero() {
		occurred = now
	}
	rec := episodic.Record{
		ID:           id,
		Kind:         req.Kind,
		OccurredAt:   occurred.UTC(),
		Actor:        req.Actor,
		Text:         req.Text,
		Entities:     normalizeStrings(req.Entities),
		Refs:         req.Refs,
		Consolidated: false,
	}
	if err = s.store.AppendEpisode(r.Context(), key, rec); err != nil {
		writeAPIError(w, s.log, apierr.From(err))
		return
	}

	resp := CreateEpisodeResponse{Record: rec}
	if s.index == nil {
		resp.Degraded = append(resp.Degraded, degradedSearch)
		s.markDirty(r.Context(), key, hotstore.PlaneEpisodic)
	} else if err := s.index.IndexRecords(r.Context(), key, []episodic.Record{rec}); err != nil {
		s.log.Warn("episode index upsert failed; degraded", "project", key.String(), "error", err)
		resp.Degraded = append(resp.Degraded, degradedSearch)
		s.markDirty(r.Context(), key, hotstore.PlaneEpisodic)
	} else {
		s.markIndexed(r.Context(), key, hotstore.PlaneEpisodic)
	}
	writeJSON(w, s.log, http.StatusCreated, resp)
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
	if apiErr := validateProjectKey(key); apiErr != nil {
		writeAPIError(w, s.log, apiErr)
		return
	}
	qs := r.URL.Query()
	text := qs.Get(paramQuery)
	if text == "" {
		writeAPIError(w, s.log, badRequest(paramQuery+" is required"))
		return
	}
	from, apiErr := parseTimeParam(qs.Get(paramFrom), paramFrom)
	if apiErr != nil {
		writeAPIError(w, s.log, apiErr)
		return
	}
	to, apiErr := parseTimeParam(qs.Get(paramTo), paramTo)
	if apiErr != nil {
		writeAPIError(w, s.log, apiErr)
		return
	}
	kinds, apiErr := parseKindsParam(qs.Get(paramKinds))
	if apiErr != nil {
		writeAPIError(w, s.log, apiErr)
		return
	}
	if s.index == nil {
		writeAPIError(w, s.log, unavailable(degradedSearch))
		return
	}

	s.statGate(r.Context(), key)

	hits, err := s.index.Search(r.Context(), key, search.Query{Text: text, From: from, To: to, Kinds: kinds})
	if errors.Is(err, errs.ErrUnavailable) {
		// §5: a derived read on a dead store is an honest 503, worded exactly
		// like the degraded note a write would have carried.
		writeAPIError(w, s.log, unavailable(degradedSearch).WithCause(err))
		return
	}
	if err != nil {
		writeAPIError(w, s.log, apierr.From(err))
		return
	}
	if hits == nil {
		hits = []search.Hit{}
	}
	s.bumpRecall(r.Context(), key, hits)
	writeJSON(w, s.log, http.StatusOK, hits)
}

// bumpRecall updates recall_count/last_recalled on hot records best-effort;
// a failure is logged, never surfaced — the search result is already correct.
func (s *Server) bumpRecall(ctx context.Context, key hotstore.ProjectKey, hits []search.Hit) {
	if s.store == nil || len(hits) == 0 {
		return
	}
	ids := make([]string, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.Record.ID)
	}
	now := s.clock.Now().UTC().Format(time.RFC3339)
	err := s.store.UpdateEpisodes(ctx, key, ids, func(rec episodic.Record) episodic.Record {
		rec.RecallCount++
		rec.LastRecalled = now
		return rec
	})
	if err != nil {
		s.log.Warn("recall bump failed", "project", key.String(), "error", err)
		return
	}
	s.convergeEpisodes(ctx, key, ids)
}

// convergeEpisodes re-indexes records a handler just rewrote in hot and
// refreshes manifest freshness — the same close-the-loop the create path does.
// Both in-place episodic mutations use it: the recall bump above and the
// consolidation promotion of handlers_knowledge.go.
//
// It is not optional bookkeeping. recall_count, last_recalled and consolidated
// are mapped, indexed fields (search/mapping.go), so a hot-only rewrite leaves
// the derived doc stale. Worse, the hydration sha covers the whole hot file:
// without this, the first search after any rehydration permanently flips
// CheckDrift to "episodic hot content changed since last hydration" (§5) — and
// it can never settle, because the reindex that would clear it is itself
// re-dirtied by the next search. The stale mtime also made the stat-gate
// rehydrate the entire project on every search more than the 2s debounce apart.
func (s *Server) convergeEpisodes(ctx context.Context, key hotstore.ProjectKey, ids []string) {
	if s.index == nil {
		s.markDirty(ctx, key, hotstore.PlaneEpisodic)
		return
	}
	// Re-read from hot rather than mutating the caller's copies: hot is
	// canonical (§0 principle 1) and the index copy may lag it.
	recs, err := s.store.ListEpisodes(ctx, key)
	if err != nil {
		s.log.Warn("episode converge: list episodes failed", "project", key.String(), "error", err)
		s.markDirty(ctx, key, hotstore.PlaneEpisodic)
		return
	}
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	changed := make([]episodic.Record, 0, len(ids))
	for _, rec := range recs {
		if wanted[rec.ID] {
			changed = append(changed, rec)
		}
	}
	if len(changed) == 0 {
		return
	}
	if err := s.index.IndexRecords(ctx, key, changed); err != nil {
		s.log.Warn("episode converge: index upsert failed; degraded", "project", key.String(), "error", err)
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
// Contract: hot lookup, then the cold archive so provenance links keep
// resolving after aging (§2).
func (s *Server) handleGetEpisode(w http.ResponseWriter, r *http.Request) {
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
	rec, err := s.store.GetEpisode(r.Context(), key, id)
	if err == nil {
		writeJSON(w, s.log, http.StatusOK, rec)
		return
	}
	if !errors.Is(err, errs.ErrNotFound) {
		writeAPIError(w, s.log, apierr.From(err))
		return
	}
	// Aged to cold? Provenance links must keep resolving (§2).
	if s.archiver == nil {
		writeAPIError(w, s.log, notFound("episode not in hot store; cold archive unavailable"))
		return
	}
	rec, err = s.archiver.FetchArchivedEpisode(r.Context(), key, id)
	if err != nil {
		writeAPIError(w, s.log, apierr.From(err))
		return
	}
	writeJSON(w, s.log, http.StatusOK, rec)
}
