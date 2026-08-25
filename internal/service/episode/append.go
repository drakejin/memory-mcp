package episode

import (
	"context"
	"strings"
	"time"

	"github.com/drakejin/memory-mcp/internal/x/errs"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// AppendRequest is the caller's half of a new record (§2.1): everything the
// server does not control. The service assigns the id and the occurred_at
// default, and Consolidated always starts false — it is set only where the
// agent states the distillation itself (§0 principle 2). JSON tags mirror the
// §7 POST body so the transport layer can decode straight into it.
type AppendRequest struct {
	Kind       Kind      `json:"kind"`
	OccurredAt time.Time `json:"occurred_at"`
	Actor      Actor     `json:"actor"`
	Text       string    `json:"text"`
	Entities   []string  `json:"entities"`
	Refs       *Refs     `json:"refs,omitempty"`
}

// AppendResult reports the stored record plus degraded notes when a derived
// upsert failed best-effort (§5 — never a failure on write). JSON tags mirror
// the §7 response body, so the handler serializes it as is.
type AppendResult struct {
	Record   Record   `json:"record"`
	Degraded []string `json:"degraded,omitempty"`
}

// Append implements Service: assign the ULID from the injected clock, commit
// to hot (must succeed or the call fails), then best-effort index — on index
// failure mark the manifest dirty and report degraded, still a success (P1).
func (s *service) Append(ctx context.Context, key projectkey.Key, req AppendRequest) (AppendResult, error) {
	now := s.clock.Now().UTC()
	id, err := s.ids.GenerateAt(now.UnixMilli())
	if err != nil {
		return AppendResult{}, errs.Wrap(opAppend, err)
	}
	occurred := req.OccurredAt
	if occurred.IsZero() {
		occurred = now
	}
	rec := Record{
		ID:           id,
		Kind:         req.Kind,
		OccurredAt:   occurred.UTC(),
		Actor:        req.Actor,
		Text:         req.Text,
		Entities:     normalizeEntities(req.Entities),
		Refs:         req.Refs,
		Consolidated: false,
	}
	if err := s.store.AppendEpisode(ctx, key, rec); err != nil {
		return AppendResult{}, errs.Wrap(opAppend, err)
	}

	res := AppendResult{Record: rec}
	if s.index == nil {
		res.Degraded = append(res.Degraded, DegradedSearch)
		s.markDirty(ctx, key)
		return res, nil
	}
	if err := s.index.IndexRecords(ctx, key, []Record{rec}); err != nil {
		s.log.Warn("episode index upsert failed; degraded", "project", key.String(), "error", err)
		res.Degraded = append(res.Degraded, DegradedSearch)
		s.markDirty(ctx, key)
		return res, nil
	}
	s.markIndexed(ctx, key)
	return res, nil
}

// normalizeEntities trims entries and drops empties, always returning a
// non-nil slice so hot JSON never stores null arrays (§2.1).
func normalizeEntities(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
