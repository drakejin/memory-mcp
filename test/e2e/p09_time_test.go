//go:build e2e

// P9 — time contract (feature-inventory.md §2 P9): hot JSON stores every
// timestamp as RFC3339 UTC (zone-carrying input is normalized, never stored
// with an offset), and TTL aging judges the 29-day/31-day boundary correctly
// against the injected past records. The format is proven at byte level on
// the raw file — a decode-then-compare would launder a wrong spelling.
package e2e

import (
	"bytes"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	cold "github.com/drakejin/memory-mcp/internal/external/thirdparty/cold"
	httpserver "github.com/drakejin/memory-mcp/internal/handler/app/httpserver"
	"github.com/drakejin/memory-mcp/internal/service/episode"
)

// Raw byte matchers for the hot JSON time fields. \s* tolerates both the hot
// store's indented encoding (": ") and the compact archive encoding (":").
var (
	p09OccurredAtRe   = regexp.MustCompile(`"occurred_at":\s*"([^"]*)"`)
	p09LastRecalledRe = regexp.MustCompile(`"last_recalled":\s*"([^"]*)"`)
)

// p09AssertRFC3339UTC fails unless every captured occurred_at value in raw is
// RFC3339 with the explicit UTC "Z" spelling, and returns how many it saw.
func p09AssertRFC3339UTC(t *testing.T, what string, raw []byte) int {
	t.Helper()
	matches := p09OccurredAtRe.FindAllSubmatch(raw, -1)
	for _, m := range matches {
		v := string(m[1])
		if _, err := time.Parse(time.RFC3339, v); err != nil {
			failf(t, "%s: occurred_at %q is not RFC3339: %v", what, v, err)
		}
		if !strings.HasSuffix(v, "Z") {
			failf(t, "%s: occurred_at %q is not spelled as UTC (want trailing Z)", what, v)
		}
	}
	return len(matches)
}

func TestP09_TimeContract(t *testing.T) {
	h.ensureServer(t)
	key := e2eKey("p09-time")

	// ---- storage format: zone-carrying input is normalized to UTC ----
	kst := time.FixedZone("KST", 9*60*60)
	zoned := time.Now().In(kst).Add(-72 * time.Hour).Truncate(time.Second)
	epZoned := h.postEpisode(t, key, httpserver.CreateEpisodeRequest{
		Kind: episode.KindEvent, Actor: episode.ActorUser,
		// The text must not spell the offset itself: the raw-byte scan below
		// asserts the offset string appears nowhere in the hot file.
		OccurredAt: zoned,
		Text:       "p09 zoned input: occurred_at carried a KST offset on the wire",
	})
	if !epZoned.Record.OccurredAt.Equal(zoned) {
		failf(t, "zoned input: instant changed: sent %s, stored %s", zoned, epZoned.Record.OccurredAt)
	}
	if _, offset := epZoned.Record.OccurredAt.Zone(); offset != 0 {
		failf(t, "zoned input: response occurred_at is not UTC (offset %ds)", offset)
	}
	epMinted := h.postEpisode(t, key, httpserver.CreateEpisodeRequest{
		Kind: episode.KindEvent, Actor: episode.ActorAgent,
		Text: "p09 minted: server assigns occurred_at when the caller omits it",
	})

	// ---- TTL boundary fixtures: 29d (TTL-1d) and 31d (TTL+1d), both
	// consolidated, planted at the store so the pasts are exact ----
	base := time.Now().UTC().Truncate(time.Second)
	at29 := base.AddDate(0, 0, -29)
	at31 := base.AddDate(0, 0, -31)
	edge29 := episode.Record{
		ID: newULIDAt(t, at29), Kind: episode.KindDecision, OccurredAt: at29,
		Actor: episode.ActorAgent, Text: "p09 edge: consolidated at TTL-1d, must stay hot",
		Entities: []string{}, Consolidated: true,
	}
	edge31 := episode.Record{
		ID: newULIDAt(t, at31), Kind: episode.KindDecision, OccurredAt: at31,
		Actor: episode.ActorAgent, Text: "p09 edge: consolidated at TTL+1d, must age",
		Entities: []string{}, Consolidated: true,
	}
	h.writeHotEpisodeDirect(t, key, edge29)
	h.writeHotEpisodeDirect(t, key, edge31)

	// ---- byte-level audit of the whole hot file ----
	raw := readFileRaw(t, h.hotEpisodePath(key))
	wantRecords := len(h.hotEpisodes(t, key))
	if wantRecords != 4 {
		failf(t, "fixture drift: want 4 hot records, got %d", wantRecords)
	}
	if got := p09AssertRFC3339UTC(t, "hot file", raw); got != wantRecords {
		failf(t, "hot file: matcher saw %d occurred_at fields for %d records — format audit incomplete", got, wantRecords)
	}
	if bytes.Contains(raw, []byte("+09:00")) {
		failf(t, "hot file stores a zone offset — RFC3339 UTC normalization violated (%s)", h.hotEpisodePath(key))
	}
	recalled := p09LastRecalledRe.FindAllSubmatch(raw, -1)
	if len(recalled) != wantRecords {
		failf(t, "hot file: matcher saw %d last_recalled fields for %d records", len(recalled), wantRecords)
	}
	for _, m := range recalled {
		// Never-recalled records store the documented empty string, not null,
		// not a zero time.
		if v := string(m[1]); v != "" {
			failf(t, "hot file: last_recalled %q on a never-recalled record (want \"\")", v)
		}
	}
	pass(t, "hot bytes: all %d occurred_at values are RFC3339 with Z; +09:00 input normalized; last_recalled empty", wantRecords)

	// Converge the index so the aged record's index-side delete is observable.
	h.reindex(t, false)
	if _, found := h.osDoc(t, key, edge31.ID); !found {
		failf(t, "precondition: edge31 %s not indexed after reindex", edge31.ID)
	}

	// ---- the boundary judgment: exactly one of the two edges ages ----
	wantKey := cold.EpisodeArchiveKey(s3Username, key, cold.ArchiveMonth(at31))
	rep := h.consolidate(t, httpserver.ConsolidateRequest{Projects: []string{key.String()}})
	if len(rep.Failures) != 0 {
		failf(t, "consolidate: unexpected failures: %v", rep.Failures)
	}
	if rep.MovedEpisodes != 1 || len(rep.ArchiveKeys) != 1 || rep.ArchiveKeys[0] != wantKey {
		failf(t, "TTL boundary: want exactly the 31d record moved into %s, got moved=%d keys=%v",
			wantKey, rep.MovedEpisodes, rep.ArchiveKeys)
	}

	if _, ok := h.hotEpisodeByID(t, key, edge31.ID); ok {
		failf(t, "hot inspector: TTL+1d record %s still hot after aging", edge31.ID)
	}
	rec29, ok := h.hotEpisodeByID(t, key, edge29.ID)
	if !ok || !rec29.Consolidated || !rec29.OccurredAt.Equal(at29) {
		failf(t, "hot inspector: TTL-1d record %s must stay hot untouched, got present=%v rec=%+v", edge29.ID, ok, rec29)
	}
	for _, id := range []string{epZoned.Record.ID, epMinted.Record.ID} {
		if _, ok := h.hotEpisodeByID(t, key, id); !ok {
			failf(t, "hot inspector: unconsolidated record %s vanished during aging", id)
		}
	}
	pass(t, "hot inspector: 31d aged, 29d stayed — the boundary judged correctly")

	// Cold side: the batch holds the 31d record — and its bytes keep the same
	// RFC3339 UTC spelling, so the time contract survives archival.
	archiveRaw := h.s3Cat(t, wantKey)
	batch := decodeArchiveBatch(t, wantKey, archiveRaw)
	got31, found := agedRecordByID(batch, edge31.ID)
	if !found || !got31.OccurredAt.Equal(at31) || !got31.Consolidated {
		failf(t, "s3 inspector: archive %s: want the 31d record with its exact instant, got found=%v rec=%+v", wantKey, found, got31)
	}
	if _, found := agedRecordByID(batch, edge29.ID); found {
		failf(t, "s3 inspector: archive %s contains the TTL-1d record %s", wantKey, edge29.ID)
	}
	if got := p09AssertRFC3339UTC(t, "archive object", archiveRaw); got != len(batch) {
		failf(t, "archive object: matcher saw %d occurred_at fields for %d records", got, len(batch))
	}

	// Index side: only the aged record dropped.
	if _, found := h.osDoc(t, key, edge31.ID); found {
		failf(t, "os inspector: aged record %s still indexed", edge31.ID)
	}
	if _, found := h.osDoc(t, key, edge29.ID); !found {
		failf(t, "os inspector: surviving record %s vanished from the index", edge29.ID)
	}
	if !slices.Contains(hotIDsOf(h.hotEpisodes(t, key)), edge29.ID) {
		failf(t, "hot inspector: survivor listing lost %s", edge29.ID)
	}
	pass(t, "P9 complete: RFC3339 UTC at rest (hot and cold), TTL 29d/31d boundary aged exactly one record")
}

// hotIDsOf projects record ids for evidence and membership checks.
func hotIDsOf(recs []episode.Record) []string {
	ids := make([]string, 0, len(recs))
	for _, rec := range recs {
		ids = append(ids, rec.ID)
	}
	return ids
}
