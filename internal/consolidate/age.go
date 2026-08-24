package consolidate

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/drakejin/memory-mcp/internal/config"
	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/errs"
	"github.com/drakejin/memory-mcp/internal/hotstore"
)

// noFilePressure is the size reported for a project whose hot file cannot be
// stat'ed: a file that is not there exerts no pressure.
const noFilePressure = 0

// ageProject archives eligible records for one project, in the only order §4
// step 3 permits: S3 put confirmed → hot removal → index delete. A dry run
// only counts what would move.
func (s *service) ageProject(ctx context.Context, key hotstore.ProjectKey, recs []episodic.Record, now time.Time, dryRun bool, report *Report) {
	fileBytes, _, err := s.store.FileInfo(ctx, key, hotstore.PlaneEpisodic)
	if err != nil {
		fileBytes = noFilePressure
	}

	views := make([]recordView, 0, len(recs))
	byID := make(map[string]episodic.Record, len(recs))
	for _, rec := range recs {
		views = append(views, recordView{ID: rec.ID, OccurredAt: rec.OccurredAt, Consolidated: rec.Consolidated})
		byID[rec.ID] = rec
	}
	ids := ageEligible(views, now, s.ttlDays, fileBytes, config.MaxProjectFileBytes, config.MaxProjectRecords)
	if len(ids) == 0 {
		return
	}

	if dryRun {
		report.MovedEpisodes += len(ids)
		return
	}
	if s.archiver == nil {
		report.addUnavailable(stepAge, key)
		return
	}

	aging := make([]episodic.Record, 0, len(ids))
	for _, id := range ids {
		aging = append(aging, byID[id])
	}
	months, byMonth := batchByMonth(aging)

	for _, month := range months {
		batch := byMonth[month]
		batchIDs := make([]string, 0, len(batch))
		for _, rec := range batch {
			batchIDs = append(batchIDs, rec.ID)
		}

		// 3a: S3 put — must succeed before anything local is touched.
		s3Key, err := s.archiver.ArchiveEpisodes(ctx, key, month, batch)
		if err != nil {
			report.addFailure(stepArchive+" "+month, key, err)
			continue
		}
		report.ArchiveKeys = append(report.ArchiveKeys, s3Key)

		// 3b: hot removal, only after the cold copy is confirmed.
		if err := s.store.RemoveEpisodes(ctx, key, batchIDs); err != nil {
			// Cold copy exists but hot still holds the records — safe
			// direction; the next run will re-archive idempotently.
			report.addFailure(stepHotRemove+" "+month, key, err)
			continue
		}
		report.MovedEpisodes += len(batchIDs)

		// 3c: index delete, best-effort; drift converges via rehydration.
		if err := s.deleteFromIndex(ctx, key, batchIDs); err != nil {
			report.addFailure(stepIndexDelete+" "+month, key, err)
			if derr := s.store.MarkDirty(ctx, key, hotstore.PlaneEpisodic); derr != nil {
				s.log.WarnContext(ctx, "mark dirty failed", "project", key.String(), "error", derr)
			}
		}
	}
}

// deleteFromIndex drops the aged ids from the derived index. A missing index
// is reported as unavailable rather than silently skipped (§0 principle 3).
func (s *service) deleteFromIndex(ctx context.Context, key hotstore.ProjectKey, ids []string) error {
	if s.index == nil {
		return errs.Unavailable(opDeleteIndexed, nil)
	}
	return errs.Wrap(opDeleteIndexed, s.index.DeleteRecords(ctx, key, ids))
}

// recordView is the minimal projection ageEligible needs; constructed from
// episodic.Record.
type recordView struct {
	ID           string
	OccurredAt   time.Time
	Consolidated bool
}

// ageEligible returns the ids of records that qualify for cold aging at now
// per §3.1: consolidated AND at least ttlDays old; plus, when the file exceeds
// the size/count pressure thresholds, the oldest consolidated records beyond
// the limit. Unconsolidated records are never eligible, regardless of age or
// pressure. maxBytes/maxRecords <= 0 disables that pressure check. Returned
// ids are sorted oldest-first (OccurredAt, then ID). Pure function.
func ageEligible(recs []recordView, now time.Time, ttlDays int, fileBytes int64, maxBytes int64, maxRecords int) []string {
	cutoff := now.AddDate(0, 0, -ttlDays)

	consolidated := make([]recordView, 0, len(recs))
	for _, rec := range recs {
		if rec.Consolidated {
			consolidated = append(consolidated, rec)
		}
	}
	slices.SortFunc(consolidated, byOccurrenceThenID)

	eligible := make(map[string]bool, len(consolidated))
	for _, rec := range consolidated {
		if !rec.OccurredAt.After(cutoff) {
			eligible[rec.ID] = true
		}
	}

	need := pressureQuota(len(recs), fileBytes, maxBytes, maxRecords)
	for _, rec := range consolidated {
		if len(eligible) >= need {
			break
		}
		eligible[rec.ID] = true
	}

	ids := make([]string, 0, len(eligible))
	for _, rec := range consolidated { // already oldest-first
		if eligible[rec.ID] {
			ids = append(ids, rec.ID)
		}
	}
	return ids
}

// pressureQuota is how many records the §3.1 pressure thresholds demand be
// removed. Zero when neither threshold is exceeded or both are disabled.
func pressureQuota(recordCount int, fileBytes, maxBytes int64, maxRecords int) int {
	need := 0
	if maxRecords > 0 && recordCount > maxRecords {
		need = recordCount - maxRecords
	}
	if maxBytes > 0 && fileBytes > maxBytes && recordCount > 0 {
		// Estimate per-record size from the file average; keep enough of the
		// newest records to fit under maxBytes.
		keep := int(maxBytes * int64(recordCount) / fileBytes)
		if over := recordCount - keep; over > need {
			need = over
		}
	}
	return need
}

// byOccurrenceThenID orders records oldest-first, breaking ties on the ULID so
// aging is deterministic across runs.
func byOccurrenceThenID(x, y recordView) int {
	if !x.OccurredAt.Equal(y.OccurredAt) {
		if x.OccurredAt.Before(y.OccurredAt) {
			return -1
		}
		return 1
	}
	return strings.Compare(x.ID, y.ID)
}
