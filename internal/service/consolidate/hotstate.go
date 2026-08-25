package consolidate

// HotState is the /v1/status slice of this pipeline's subject matter (§3.1):
// which hot files still await derived convergence, and how many episodes have
// never been distilled. It lives here rather than in the transport layer
// because the TTL that defines "stale" is this pipeline's aging clock — the
// same number that decides when a consolidated record may sink to cold.

import (
	"context"
	"slices"
	"time"

	"github.com/drakejin/memory-mcp/internal/x/errs"
)

// hoursPerDay converts the configured TTL in days into a duration.
const hoursPerDay = 24 * time.Hour

// Degraded notes HotState raises when a count could not be taken. They are
// data rather than errors: /status stays 200 and discloses the gap (§0
// principle 3).
const (
	// DegradedCountUnavailable reports that no project could be enumerated.
	DegradedCountUnavailable = "unconsolidated count unavailable"
	// DegradedCountIncompletePrefix prefixes the "ws/team/proj" whose episodes
	// could not be listed.
	DegradedCountIncompletePrefix = "unconsolidated count incomplete: "
)

// HotState is the §3.1 bookkeeping /v1/status publishes.
type HotState struct {
	// ManifestUpdatedAt is the manifest's own freshness timestamp.
	ManifestUpdatedAt time.Time
	// DirtyFiles lists hot files whose derived upsert failed and awaits
	// rehydration, sorted. Never nil, so the §7 envelope renders [].
	DirtyFiles []string
	// Unconsolidated counts episodes not yet distilled; StaleUnconsolidated
	// counts those older than the TTL that still refuse to age (§3.1 —
	// surfaced forever, never auto-deleted).
	Unconsolidated      int
	StaleUnconsolidated int
	// Degraded discloses counts that could not be taken.
	Degraded []string
}

// HotState implements Service.
func (s *service) HotState(ctx context.Context) (HotState, error) {
	m, err := s.store.Manifest(ctx)
	if err != nil {
		return HotState{}, errs.Wrap(opHotState, err)
	}
	hs := HotState{
		ManifestUpdatedAt: m.UpdatedAt,
		DirtyFiles:        []string{},
		Degraded:          []string{},
	}
	for fk, fs := range m.Files {
		if fs.Dirty {
			hs.DirtyFiles = append(hs.DirtyFiles, fk)
		}
	}
	slices.Sort(hs.DirtyFiles)

	s.countUnconsolidated(ctx, &hs)
	return hs, nil
}

// countUnconsolidated scans every hot project for undistilled episodes and the
// stale subset older than the TTL (§3.1 — these are surfaced forever, never
// auto-deleted). Failures degrade to a disclosure; the caller must still
// answer.
func (s *service) countUnconsolidated(ctx context.Context, hs *HotState) {
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		s.log.Error("status project listing failed", "error", err)
		hs.Degraded = append(hs.Degraded, DegradedCountUnavailable)
		return
	}
	staleBefore := s.clock.Now().UTC().Add(-time.Duration(s.ttlDays) * hoursPerDay)
	for _, p := range projects {
		recs, err := s.store.ListEpisodes(ctx, p)
		if err != nil {
			s.log.Error("status episode listing failed", "project", p.String(), "error", err)
			hs.Degraded = append(hs.Degraded, DegradedCountIncompletePrefix+p.String())
			continue
		}
		for _, rec := range recs {
			if rec.Consolidated {
				continue
			}
			hs.Unconsolidated++
			if rec.OccurredAt.Before(staleBefore) {
				hs.StaleUnconsolidated++
			}
		}
	}
}
