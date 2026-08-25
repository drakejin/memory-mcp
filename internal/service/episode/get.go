package episode

import (
	"context"
	"errors"

	"github.com/drakejin/memory-mcp/internal/x/errs"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// msgNoColdFallback names the gap when a record is not in hot and no cold
// archive is wired to fall back to. The wording is the §7 public message and
// travels to the client as the KindNotFound message.
const msgNoColdFallback = "episode not in hot store; cold archive unavailable"

// GetResult carries the resolved record, wherever it was found. Degraded is
// reserved for interface uniformity: a lookup either resolves the record or
// fails — there is no partial answer to degrade today.
type GetResult struct {
	Record   Record   `json:"record"`
	Degraded []string `json:"degraded,omitempty"`
}

// Get implements Service: hot lookup, then the cold archive, so provenance
// links keep resolving after aging (§2, P11).
func (s *service) Get(ctx context.Context, key projectkey.Key, id string) (GetResult, error) {
	rec, err := s.store.GetEpisode(ctx, key, id)
	if err == nil {
		return GetResult{Record: rec}, nil
	}
	if !errors.Is(err, errs.ErrNotFound) {
		return GetResult{}, errs.Wrap(opGet, err)
	}
	// Aged to cold? Provenance links must keep resolving (§2).
	if s.archive == nil {
		return GetResult{}, noColdFallback(id)
	}
	rec, err = s.archive.FetchArchivedEpisode(ctx, key, id)
	if err != nil {
		return GetResult{}, errs.Wrap(opGet, err)
	}
	return GetResult{Record: rec}, nil
}

// noColdFallback is the KindNotFound error for a hot miss with no archive
// wired: its message names the gap honestly (§5) instead of a bare not-found.
func noColdFallback(id string) error {
	e := errs.NotFound(opGet, entityEpisode, id)
	e.Msg = msgNoColdFallback
	return e
}
