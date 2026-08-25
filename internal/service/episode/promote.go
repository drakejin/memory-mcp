package episode

// Promotion closes the §3 consolidation loop. Distillation is the agent's
// job: it reads episodes, writes the knowledge node, and names the episodes
// it distilled in provenance. That naming IS the promotion §3 describes
// ("에이전트가 episode들을 증류해 knowledge 승격 — POST /knowledge, provenance
// 링크"), and §3.1 reads its result, consolidated=true, as the precondition
// for a record ever sinking to cold.
//
// Nothing is judged here (§0 principle 2): the server only records the
// consequence of a statement the agent made. Without it no API path sets the
// flag at all, so aging never fires, hot files grow past the §3.1 pressure
// thresholds with no relief, and /v1/status reports a stale_unconsolidated
// count no legitimate client action can reduce.

import (
	"context"

	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// Promote implements Service.
//
// Ids that name no hot record of this project — already aged to cold, or
// belonging to another project — are skipped rather than failing a node that
// is already written. So is a record already consolidated: re-promoting it
// would rewrite the hot file for nothing. The whole step is best-effort: the
// hot knowledge write has succeeded, so a bookkeeping failure is a degraded
// note (§5), never a failure.
func (s *service) Promote(ctx context.Context, key projectkey.Key, provenance []string) []string {
	if len(provenance) == 0 {
		return nil
	}
	recs, err := s.store.ListEpisodes(ctx, key)
	if err != nil {
		s.log.Warn("provenance promotion: list episodes failed", "project", key.String(), "error", err)
		return []string{DegradedPromotion}
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
	err = s.store.UpdateEpisodes(ctx, key, pending, func(rec Record) Record {
		rec.Consolidated = true
		return rec
	})
	if err != nil {
		s.log.Warn("provenance promotion: mark consolidated failed",
			"project", key.String(), "episodes", len(pending), "error", err)
		return []string{DegradedPromotion}
	}
	// consolidated is an indexed field: a hot-only flip would leave search
	// reporting the record as still undistilled.
	s.convergeEpisodes(ctx, key, pending)
	return nil
}
