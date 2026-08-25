package episode

import (
	"context"
	"errors"
	"time"

	"github.com/drakejin/memory-mcp/internal/x/errs"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// Query is the episodic search request (§7). It is a domain value type so the
// handler and the Index port speak it without either importing the index
// client (feature-inventory §4.1 rule 4).
type Query struct {
	// Text is the full-text (nori) match query; empty matches all.
	Text string
	// From/To bound occurred_at; zero values mean unbounded.
	From time.Time
	To   time.Time
	// Kinds filters record kinds; empty means all kinds.
	Kinds []Kind
	// Size caps hits; <= 0 lets the index apply its default.
	Size int
}

// Hit is one search result. Per the recall principle (§7) responses carry
// excerpt + metadata + score only — never a full-body injection.
type Hit struct {
	Record Record  `json:"record"`
	Score  float64 `json:"score"`
	// Excerpt is a highlighted fragment of Text, not the full body.
	Excerpt string `json:"excerpt"`
}

// SearchResult carries the hits plus degraded notes, the uniform result shape
// of this service. Hits is never nil, so a §7 response body renders [] rather
// than null. Degraded is reserved: today an unusable index fails the read as
// KindUnavailable (§5) instead of degrading it, and recall-bump problems are
// deliberately log-only.
type SearchResult struct {
	Hits     []Hit    `json:"hits"`
	Degraded []string `json:"degraded,omitempty"`
}

// Search implements Service: stat-gate first; hits carry excerpt+meta+score
// only (§7); bumps recall_count/last_recalled best-effort. KindUnavailable —
// carrying the §5 degraded wording — when the index is absent or down.
func (s *service) Search(ctx context.Context, key projectkey.Key, q Query) (SearchResult, error) {
	if s.index == nil {
		return SearchResult{}, unavailableSearch(nil)
	}

	s.statGate(ctx, key)

	hits, err := s.index.Search(ctx, key, q)
	if errors.Is(err, errs.ErrUnavailable) {
		// §5: a derived read on a dead store is an honest failure, worded
		// exactly like the degraded note a write would have carried.
		return SearchResult{}, unavailableSearch(err)
	}
	if err != nil {
		return SearchResult{}, errs.Wrap(opSearch, err)
	}
	if hits == nil {
		hits = []Hit{}
	}
	s.bumpRecall(ctx, key, hits)
	return SearchResult{Hits: hits}, nil
}

// unavailableSearch builds the KindUnavailable error a dead index produces.
// Its message is the §5 degraded vocabulary word for word, so apierr.From
// renders the same body the handler used to build by hand, and the handler's
// errors.Is(err, errs.ErrUnavailable) branch keeps matching.
func unavailableSearch(cause error) error {
	e := errs.Unavailable(opSearch, cause)
	e.Msg = DegradedSearch
	return e
}

// statGate runs the request-entry freshness check (§5) best-effort; a failing
// gate never blocks the request — rehydration problems surface in /status.
func (s *service) statGate(ctx context.Context, key projectkey.Key) {
	if s.gate == nil {
		return
	}
	if err := s.gate.StatGate(ctx, key); err != nil {
		s.log.Warn("stat-gate rehydration incomplete", "project", key.String(), "error", err)
	}
}

// bumpRecall updates recall_count/last_recalled on hot records best-effort; a
// failure is logged, never surfaced — the search result is already correct.
// Hot is written first, then the index refresh follows (P2): the ranking
// material lives in hot and the derived document is rebuilt from it.
func (s *service) bumpRecall(ctx context.Context, key projectkey.Key, hits []Hit) {
	if len(hits) == 0 {
		return
	}
	ids := make([]string, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.Record.ID)
	}
	now := s.clock.Now().UTC().Format(time.RFC3339)
	err := s.store.UpdateEpisodes(ctx, key, ids, func(rec Record) Record {
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

// convergeEpisodes re-indexes records this service just rewrote in hot and
// refreshes manifest freshness — the same close-the-loop Append performs. Both
// in-place episodic mutations use it: the recall bump above and the §3
// provenance promotion of promote.go.
//
// It is not optional bookkeeping. recall_count, last_recalled and consolidated
// are mapped, indexed fields, so a hot-only rewrite leaves the derived
// document stale. Worse, the hydration sha covers the whole hot file: without
// this, the first search after any rehydration permanently flips CheckDrift to
// "episodic hot content changed since last hydration" (§5) — and it can never
// settle, because the reindex that would clear it is itself re-dirtied by the
// next search. The stale mtime also made the stat-gate rehydrate the entire
// project on every search more than the debounce apart.
func (s *service) convergeEpisodes(ctx context.Context, key projectkey.Key, ids []string) {
	if s.index == nil {
		// The promotion path reaches here with no index wired; the hot rewrite
		// stands and the dirty mark hands convergence to rehydration (§1).
		s.markDirty(ctx, key)
		return
	}
	// Re-read from hot rather than mutating the caller's copies: hot is
	// canonical (§0 principle 1) and the index copy may lag it.
	recs, err := s.store.ListEpisodes(ctx, key)
	if err != nil {
		s.log.Warn("episode converge: list episodes failed", "project", key.String(), "error", err)
		s.markDirty(ctx, key)
		return
	}
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	changed := make([]Record, 0, len(ids))
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
		s.markDirty(ctx, key)
		return
	}
	s.markIndexed(ctx, key)
}
