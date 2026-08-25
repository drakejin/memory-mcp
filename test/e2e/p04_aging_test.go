//go:build e2e

// P4 — aging order (feature-inventory.md §2 P4): S3 put confirmed → hot
// removal → index delete, never reordered, never skipped; unconsolidated
// records are never aged, whatever the pressure.
//
// Three scenarios, each on its own project key:
//   - TestP04_AgingOrderColdFirst: a real archive run on the main instance —
//     the cold object already carries the full record at the first moment hot
//     absence is observable, and the TTL-old unconsolidated record both
//     survives and stays reported by /v1/status.
//   - TestP04_FilePressureSparesUnconsolidated: a hot file pushed past the
//     §3.1 byte threshold moves ONLY consolidated records; the unconsolidated
//     ballast survives two consecutive runs.
//   - TestP04_BrokenBucketKeepsHot: an extra instance pointed at a nonexistent
//     bucket — the S3 put fails, so the hot file stays byte-identical and the
//     report carries the failure per month batch.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/external/persistence/hotstore"
	cold "github.com/drakejin/memory-mcp/internal/external/thirdparty/cold"
	httpserver "github.com/drakejin/memory-mcp/internal/handler/app/httpserver"
	"github.com/drakejin/memory-mcp/internal/service/consolidate"
	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/service/rehydrate"
	"github.com/drakejin/memory-mcp/internal/x/config"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// p04BrokenBucket must not exist. Even if the name were ever taken by a
// stranger, the put would fail with AccessDenied instead of NoSuchBucket —
// either way the archive step fails, which is the injected condition.
const p04BrokenBucket = "vms-memory-mcp-e2e-p04-no-such-bucket"

// p04ExtraPort hosts the broken-bucket instance (extras use 8431+, one
// distinct port per test across the suite).
const p04ExtraPort = 8434

// p04Days builds a fixture timestamp n days in the past, truncated to the
// second so hot bytes and S3 round-trips compare exactly.
func p04Days(n int) time.Time {
	return time.Now().UTC().AddDate(0, 0, -n).Truncate(time.Second)
}

// p04Record is the shared fixture shape for direct hot writes.
func p04Record(t *testing.T, at time.Time, consolidated bool, text string) episode.Record {
	t.Helper()
	return episode.Record{
		ID:           newULIDAt(t, at),
		Kind:         episode.KindObservation,
		OccurredAt:   at,
		Actor:        episode.ActorSystem,
		Text:         text,
		Entities:     []string{},
		Consolidated: consolidated,
	}
}

// agedRecordByID scans a decoded archive batch (also reused by P9/P11).
func agedRecordByID(recs []episode.Record, id string) (episode.Record, bool) {
	for _, rec := range recs {
		if rec.ID == id {
			return rec, true
		}
	}
	return episode.Record{}, false
}

// decodeArchiveBatch parses one monthly archive object fetched via the aws
// CLI into records (also reused by P9/P11).
func decodeArchiveBatch(t *testing.T, s3Key string, data []byte) []episode.Record {
	t.Helper()
	var recs []episode.Record
	if err := json.Unmarshal(data, &recs); err != nil {
		failf(t, "archive object s3://%s/%s is not a JSON record array: %v: %.300s", s3Bucket, s3Key, err, data)
	}
	return recs
}

// p04RemoveHotEpisodes drops fixture records through the canonical store —
// the polite post-scenario shrink for the pressure ballast, mirroring the
// direct-write pattern of seed_test.go (server idle on this project, derived
// stores never saw the ballast).
func p04RemoveHotEpisodes(t *testing.T, key projectkey.Key, ids []string) {
	t.Helper()
	store, err := hotstore.New(hotstore.Config{Home: h.home, Clock: hotstore.NewSystemClock()})
	if err != nil {
		failf(t, "open canonical hot store at %s: %v", h.home, err)
	}
	if err := store.RemoveEpisodes(context.Background(), key, ids); err != nil {
		failf(t, "remove %d fixture episodes directly: %v", len(ids), err)
	}
}

// TestP04_AgingOrderColdFirst proves the §4 step-3 order on the real bucket:
// after one consolidation the cold object already contains the aged record in
// full at the first moment hot absence is observable (the S3 assert runs
// before the hot assert on purpose), the index copy is gone, and the TTL-old
// unconsolidated neighbour both survives and is still surfaced by /v1/status.
// The contrapositive — no hot removal without a confirmed cold copy — is
// TestP04_BrokenBucketKeepsHot.
func TestP04_AgingOrderColdFirst(t *testing.T) {
	h.ensureServer(t)
	key := e2eKey("p04-order")

	// Baseline for home-global status counters: shared-state rules forbid
	// absolute asserts, so honesty is proven by exact deltas within this test.
	st0 := h.status(t)

	aged := p04Record(t, p04Days(45), true, "p04 aged: consolidated and 45d old, must sink to cold")
	oldUncons := p04Record(t, p04Days(45), false, "p04 stale: unconsolidated and 45d old, must never age")
	youngCons := p04Record(t, p04Days(2), true, "p04 young: consolidated but 2d old, under TTL, must stay hot")
	h.writeHotEpisodeDirect(t, key, aged)
	h.writeHotEpisodeDirect(t, key, oldUncons)
	h.writeHotEpisodeDirect(t, key, youngCons)

	// Converge the derived index so the step-3c delete is observable later.
	// (Report totals are home-global; per the shared-state rules nothing is
	// asserted on them.)
	h.reindex(t, false)
	if _, found := h.osDoc(t, key, aged.ID); !found {
		failf(t, "precondition: aged record %s not in OpenSearch after reindex", aged.ID)
	}

	month := cold.ArchiveMonth(aged.OccurredAt)
	wantKey := cold.EpisodeArchiveKey(s3Username, key, month)
	if ok, ev := h.s3Exists(wantKey); ok {
		failf(t, "archive object exists before the run (dirty prefix?): %s", ev)
	}

	rep := h.consolidate(t, httpserver.ConsolidateRequest{Projects: []string{key.String()}})
	if len(rep.Failures) != 0 {
		failf(t, "consolidate: unexpected failures: %v", rep.Failures)
	}
	if rep.MovedEpisodes != 1 || !slices.Contains(rep.ArchiveKeys, wantKey) {
		failf(t, "consolidate: want exactly the aged record moved into %s, got moved=%d keys=%v",
			wantKey, rep.MovedEpisodes, rep.ArchiveKeys)
	}

	// §4 step-3 order: the cold copy is checked FIRST and must already hold
	// the record byte-fidelity — only then is hot absence even looked at.
	batch := decodeArchiveBatch(t, wantKey, h.s3Cat(t, wantKey))
	got, found := agedRecordByID(batch, aged.ID)
	if !found {
		failf(t, "s3 inspector: archive %s lacks the aged record %s", wantKey, aged.ID)
	}
	if got.Text != aged.Text || got.Kind != aged.Kind || got.Actor != aged.Actor ||
		!got.Consolidated || !got.OccurredAt.Equal(aged.OccurredAt) {
		failf(t, "s3 inspector: archived record differs from the hot original:\n  got  %+v\n  want %+v", got, aged)
	}
	if _, found := agedRecordByID(batch, oldUncons.ID); found {
		failf(t, "s3 inspector: archive %s contains the UNCONSOLIDATED record %s", wantKey, oldUncons.ID)
	}
	if _, found := agedRecordByID(batch, youngCons.ID); found {
		failf(t, "s3 inspector: archive %s contains the under-TTL record %s", wantKey, youngCons.ID)
	}
	pass(t, "s3 inspector: %s holds the full aged record before any hot-side check", wantKey)

	// Hot: only the aged record left; flags on the survivors untouched.
	if _, ok := h.hotEpisodeByID(t, key, aged.ID); ok {
		failf(t, "hot inspector: aged record %s still in %s after a confirmed cold copy", aged.ID, h.hotEpisodePath(key))
	}
	if rec, ok := h.hotEpisodeByID(t, key, oldUncons.ID); !ok || rec.Consolidated {
		failf(t, "hot inspector: stale unconsolidated %s must survive untouched, got present=%v rec=%+v", oldUncons.ID, ok, rec)
	}
	if rec, ok := h.hotEpisodeByID(t, key, youngCons.ID); !ok || !rec.Consolidated {
		failf(t, "hot inspector: under-TTL consolidated %s must stay hot, got present=%v rec=%+v", youngCons.ID, ok, rec)
	}
	man := h.hotManifest(t)
	fileKey := rehydrate.ManifestFileKey(rehydrate.PlaneEpisodic, key)
	if fs := man.Files[fileKey]; fs.RecordCount != 2 {
		failf(t, "manifest inspector: want Files[%q].RecordCount=2 after aging, got %+v", fileKey, fs)
	}

	// Index: step 3c dropped exactly the aged record.
	if _, found := h.osDoc(t, key, aged.ID); found {
		failf(t, "os inspector: aged record %s still indexed after aging", aged.ID)
	}
	if _, found := h.osDoc(t, key, oldUncons.ID); !found {
		failf(t, "os inspector: surviving record %s vanished from the index", oldUncons.ID)
	}
	pass(t, "hot+manifest+os: aged record gone everywhere local, survivors intact")

	// Honesty: the stale unconsolidated record is surfaced forever (§3.1).
	st1 := h.status(t)
	if dU, dS := st1.Unconsolidated-st0.Unconsolidated, st1.StaleUnconsolidated-st0.StaleUnconsolidated; dU != 1 || dS != 1 {
		failf(t, "status honesty: want unconsolidated/stale deltas +1/+1 for the surviving stale record, got +%d/+%d", dU, dS)
	}
	if !st1.S3.Reachable || st1.S3.LastArchiveAt.IsZero() {
		failf(t, "status: want reachable S3 with a recorded archive, got reachable=%v last_archive_at=%s",
			st1.S3.Reachable, st1.S3.LastArchiveAt)
	}
	pass(t, "P4 order: cold confirmed → hot removed → index dropped; stale unconsolidated survives and stays reported")
}

// TestP04_FilePressureSparesUnconsolidated pushes one project past the §3.1
// byte threshold (config.MaxProjectFileBytes) with unconsolidated ballast plus
// two young consolidated records. Pressure demands far more evictions than the
// two consolidated records can supply — yet only those two move; a second run
// under identical pressure with zero consolidated candidates moves nothing.
func TestP04_FilePressureSparesUnconsolidated(t *testing.T) {
	h.ensureServer(t)
	key := e2eKey("p04-pressure")

	const ballastCount = 60
	// ~120KB per record; 60 records ≈ 7.2MB of text, comfortably past the 5MB
	// threshold while staying far under the 5,000-record count threshold, so
	// exactly one pressure rule is exercised.
	filler := strings.Repeat("pressure ballast keeps unconsolidated memory alive ", 2400)

	now := time.Now().UTC().Truncate(time.Second)
	ballastIDs := make([]string, 0, ballastCount)
	for i := range ballastCount {
		// Half young, half far beyond TTL: age must not matter — only the
		// consolidated flag may ever admit a record to aging.
		at := now.Add(-time.Duration(i+1) * time.Hour)
		if i%2 == 1 {
			at = now.AddDate(0, 0, -45).Add(-time.Duration(i) * time.Hour)
		}
		rec := p04Record(t, at, false, fmt.Sprintf("p04 ballast %03d ", i)+filler)
		h.writeHotEpisodeDirect(t, key, rec)
		ballastIDs = append(ballastIDs, rec.ID)
	}
	consAt := now.AddDate(0, 0, -10) // under TTL: only pressure can move these
	consA := p04Record(t, consAt, true, "p04 pressure victim A: consolidated, under TTL")
	consB := p04Record(t, consAt, true, "p04 pressure victim B: consolidated, under TTL")
	h.writeHotEpisodeDirect(t, key, consA)
	h.writeHotEpisodeDirect(t, key, consB)

	fi, err := os.Stat(h.hotEpisodePath(key))
	if err != nil {
		failf(t, "stat hot file: %v", err)
	}
	if fi.Size() <= int64(config.MaxProjectFileBytes) {
		failf(t, "fixture too small to exert pressure: %d bytes <= threshold %d", fi.Size(), config.MaxProjectFileBytes)
	}
	if total := ballastCount + 2; total >= config.MaxProjectRecords {
		failf(t, "fixture must stay under the count threshold to isolate byte pressure: %d >= %d", total, config.MaxProjectRecords)
	}
	pass(t, "fixture: %d bytes on disk (> %d threshold), %d records", fi.Size(), config.MaxProjectFileBytes, ballastCount+2)

	wantKey := cold.EpisodeArchiveKey(s3Username, key, cold.ArchiveMonth(consAt))
	rep := h.consolidate(t, httpserver.ConsolidateRequest{Projects: []string{key.String()}})
	if len(rep.Failures) != 0 {
		failf(t, "consolidate under pressure: unexpected failures: %v", rep.Failures)
	}
	if rep.MovedEpisodes != 2 || len(rep.ArchiveKeys) != 1 || rep.ArchiveKeys[0] != wantKey {
		failf(t, "pressure run: want exactly the 2 consolidated records moved into %s, got moved=%d keys=%v",
			wantKey, rep.MovedEpisodes, rep.ArchiveKeys)
	}

	batch := decodeArchiveBatch(t, wantKey, h.s3Cat(t, wantKey))
	if len(batch) != 2 {
		failf(t, "s3 inspector: archive %s: want exactly the 2 consolidated records, got %d", wantKey, len(batch))
	}
	for _, id := range []string{consA.ID, consB.ID} {
		if _, found := agedRecordByID(batch, id); !found {
			failf(t, "s3 inspector: archive %s lacks consolidated record %s", wantKey, id)
		}
	}
	pass(t, "s3 inspector: pressure archived exactly the consolidated pair into %s", wantKey)

	recs := h.hotEpisodes(t, key)
	if len(recs) != ballastCount {
		failf(t, "hot inspector: want all %d unconsolidated survivors, got %d records", ballastCount, len(recs))
	}
	present := make(map[string]bool, len(recs))
	for _, rec := range recs {
		if rec.Consolidated {
			failf(t, "hot inspector: record %s is consolidated after the run — survivors must all be unconsolidated", rec.ID)
		}
		present[rec.ID] = true
	}
	for _, id := range ballastIDs {
		if !present[id] {
			failf(t, "hot inspector: unconsolidated record %s was DELETED under file pressure", id)
		}
	}
	pass(t, "hot inspector: every unconsolidated record survived byte pressure (quota wanted more, got only consolidated)")

	// Second run: identical pressure, zero consolidated candidates → nothing
	// may move. "미통합은 어떤 압박에도 삭제 금지" in its purest form.
	fi2, err := os.Stat(h.hotEpisodePath(key))
	if err != nil {
		failf(t, "stat hot file before second run: %v", err)
	}
	if fi2.Size() <= int64(config.MaxProjectFileBytes) {
		failf(t, "second run precondition lost: %d bytes <= threshold %d", fi2.Size(), config.MaxProjectFileBytes)
	}
	rep2 := h.consolidate(t, httpserver.ConsolidateRequest{Projects: []string{key.String()}})
	if rep2.MovedEpisodes != 0 || len(rep2.ArchiveKeys) != 0 || len(rep2.Failures) != 0 {
		failf(t, "second pressure run must move nothing: moved=%d keys=%v failures=%v",
			rep2.MovedEpisodes, rep2.ArchiveKeys, rep2.Failures)
	}
	if got := len(h.hotEpisodes(t, key)); got != ballastCount {
		failf(t, "hot inspector: second run changed the file: want %d records, got %d", ballastCount, got)
	}
	pass(t, "second run under identical pressure moved nothing")

	// Fixture shrink: drop the ~7MB ballast through the canonical store so
	// later full rehydrations (other scenarios, container rebuilds) stay fast.
	// Every assertion above already ran; this is cleanup, not behaviour.
	p04RemoveHotEpisodes(t, key, ballastIDs)
	if got := len(h.hotEpisodes(t, key)); got != 0 {
		failf(t, "ballast shrink left %d records behind", got)
	}
}

// TestP04_BrokenBucketKeepsHot injects a cold-store failure: an extra
// instance in an isolated home with DJ_MEMORY_S3_BUCKET pointing at a bucket
// that does not exist. Consolidation must fail at the S3 put and therefore
// touch NOTHING local: the hot file stays byte-identical, the report carries
// one archive failure per month batch, and the real bucket receives no
// objects. This is the contrapositive half of the §4 step-3 order proof.
func TestP04_BrokenBucketKeepsHot(t *testing.T) {
	key := e2eKey("p04-broken")
	extraHome := t.TempDir()

	// Fixtures land BEFORE boot, through the canonical client: two TTL-old
	// consolidated records in (usually) two distinct month batches, plus one
	// TTL-old unconsolidated record that is never eligible anyway.
	cons40 := p04Record(t, p04Days(40), true, "p04 broken-bucket: consolidated 40d, would age on a healthy bucket")
	cons70 := p04Record(t, p04Days(70), true, "p04 broken-bucket: consolidated 70d, would age on a healthy bucket")
	uncons70 := p04Record(t, p04Days(70), false, "p04 broken-bucket: unconsolidated 70d, never eligible")
	for _, rec := range []episode.Record{cons40, cons70, uncons70} {
		appendHotEpisodeAt(t, extraHome, key, rec)
	}
	months := []string{cold.ArchiveMonth(cons40.OccurredAt)}
	if m := cold.ArchiveMonth(cons70.OccurredAt); m != months[0] {
		months = append(months, m)
	}

	extra := h.launchServer(t, serverOpts{
		Port: p04ExtraPort,
		Home: extraHome,
		Env:  map[string]string{"DJ_MEMORY_S3_BUCKET": p04BrokenBucket},
		Name: "p04-broken-bucket",
	})
	t.Cleanup(func() {
		extra.stop()
		// Booting the extra rehydrated the SHARED derived stores from its own
		// home (§5 startup drift check), wiping the main home's derived docs.
		// Converge them back before any later scenario reads them.
		h.ensureServer(t)
		h.reindex(t, false)
	})
	extra.waitHealthy(t)

	// Honesty preflight on the instance itself: the injected bucket must be
	// reported unreachable before we even attempt to age.
	code, env := extra.getJSON(t, "/v1/status")
	if code != http.StatusOK || !env.Success {
		failf(t, "extra /v1/status: want 200 success, got HTTP %d error=%q", code, env.Error)
	}
	var st httpserver.StatusReport
	decodeData(t, env, &st)
	if st.S3.Reachable || st.S3.Bucket != p04BrokenBucket {
		failf(t, "extra /v1/status: want unreachable bucket %q, got reachable=%v bucket=%q",
			p04BrokenBucket, st.S3.Reachable, st.S3.Bucket)
	}
	pass(t, "extra instance honestly reports cold store unreachable (bucket %s)", p04BrokenBucket)

	before := readFileRaw(t, hotEpisodePathAt(extraHome, key))

	code, env = extra.postJSON(t, "/v1/consolidate", httpserver.ConsolidateRequest{Projects: []string{key.String()}})
	if code != http.StatusOK || !env.Success {
		failf(t, "extra /v1/consolidate: want 200 success (failures live in the report), got HTTP %d error=%q", code, env.Error)
	}
	var rep consolidate.Report
	decodeData(t, env, &rep)

	if rep.MovedEpisodes != 0 || len(rep.ArchiveKeys) != 0 {
		failf(t, "broken bucket: nothing may report as moved: moved=%d keys=%v", rep.MovedEpisodes, rep.ArchiveKeys)
	}
	for _, month := range months {
		wantPrefix := "archive " + month + ": " + key.String() + ": "
		if !slices.ContainsFunc(rep.Failures, func(f string) bool { return strings.HasPrefix(f, wantPrefix) }) {
			failf(t, "report must carry the archive failure for month %s (prefix %q), got failures: %v",
				month, wantPrefix, rep.Failures)
		}
	}
	pass(t, "report carries an archive failure per month batch %v and zero moves", months)

	// Hot no-loss, at byte level: the canonical file is untouched.
	after := readFileRaw(t, hotEpisodePathAt(extraHome, key))
	if !bytes.Equal(before, after) {
		failf(t, "hot file changed during a failed archive run: %d bytes -> %d bytes", len(before), len(after))
	}
	recs := hotEpisodesAt(t, extraHome, key)
	if len(recs) != 3 {
		failf(t, "hot inspector: want all 3 records after the failed run, got %d", len(recs))
	}
	for _, want := range []episode.Record{cons40, cons70, uncons70} {
		got, ok := agedRecordByID(recs, want.ID)
		if !ok || got.Consolidated != want.Consolidated || !got.OccurredAt.Equal(want.OccurredAt) {
			failf(t, "hot inspector: record %s altered by the failed run: present=%v got=%+v", want.ID, ok, got)
		}
	}
	pass(t, "hot inspector: file byte-identical, every record and flag intact")

	// The REAL bucket must not have been written either: same username and
	// key layout, so a silently ignored bucket override would land here.
	for _, month := range months {
		realKey := cold.EpisodeArchiveKey(s3Username, key, month)
		if ok, ev := h.s3Exists(realKey); ok {
			failf(t, "s3 inspector: real bucket received %s despite the broken-bucket override: %s", realKey, ev)
		}
	}
	pass(t, "P4 contrapositive: without a confirmed cold copy, nothing local moves")
}
