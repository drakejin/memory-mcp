//go:build e2e

// TestP01_SourceOfTruth proves feature-inventory.md §2 P1: every write commits
// to hot JSON atomically FIRST, and a derived-store failure is never a write
// failure. Injection: stop the OpenSearch container, write through the API,
// and prove 201 + degraded note + record in hot + record ABSENT from the
// surviving index — then the F18 recovery half: after the container returns,
// the server's own request-entry stat-gate converges the miss without any
// manual reindex.
package e2e

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	httpserver "github.com/drakejin/memory-mcp/internal/handler/app/httpserver"
	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/service/rehydrate"
)

// Unique fixture tokens (single unambiguous words, see p00).
const (
	p01BaselineToken = "pzeroonebaseline"
	p01DegradedToken = "pzeroonedegraded"
)

// p01RecoveryTimeout bounds the stat-gate convergence wait: the gate debounce
// is 2s and waitFor polls at 1s, so this is generous, not hopeful.
const p01RecoveryTimeout = 45 * time.Second

func TestP01_SourceOfTruth(t *testing.T) {
	h.ensureServer(t)
	key := e2eKey("p01-sot")
	fileKey := rehydrate.ManifestFileKey(rehydrate.PlaneEpisodic, key)

	// ---- baseline with every store up: hot commits AND the index mirrors ----
	ep0 := h.postEpisode(t, key, httpserver.CreateEpisodeRequest{
		Kind:     episode.KindEvent,
		Actor:    episode.ActorAgent,
		Text:     "p01 기준선: 파생물이 살아있는 동안의 기록 " + p01BaselineToken,
		Entities: []string{},
	})
	if len(ep0.Degraded) != 0 {
		failf(t, "baseline write: unexpected degraded notes with every store up: %v", ep0.Degraded)
	}
	if rec, ok := h.hotEpisodeByID(t, key, ep0.Record.ID); !ok || rec.Text != ep0.Record.Text {
		failf(t, "hot inspector: baseline record %s missing or mismatched in %s (present=%v)",
			ep0.Record.ID, h.hotEpisodePath(key), ok)
	}
	if _, found := h.osDoc(t, key, ep0.Record.ID); !found {
		failf(t, "os inspector: baseline record %s not mirrored while OpenSearch is up", ep0.Record.ID)
	}
	pass(t, "baseline: %s committed to hot and mirrored to the index", ep0.Record.ID)

	// ---- failure injection: stop OpenSearch, then write ----
	// Cleanup restores the container even if an assert below fails, so a red
	// P1 cannot poison every later scenario. docker start on an already
	// running container is a no-op, so the happy path pays nothing.
	t.Cleanup(func() { h.startContainer(t, osContainer) })
	h.stopContainer(t, osContainer)
	if up, ev := h.opensearchUp(); up {
		failf(t, "opensearch still answering after docker stop: %s", ev)
	}

	ep1 := h.postEpisode(t, key, httpserver.CreateEpisodeRequest{
		Kind:     episode.KindDecision,
		Actor:    episode.ActorAgent,
		Text:     "p01 주입: 색인이 죽은 동안의 기록 " + p01DegradedToken,
		Entities: []string{"opensearch"},
	})
	if !slices.Contains(ep1.Degraded, episode.DegradedSearch) {
		failf(t, "degraded write: want 201 with note %q, got degraded=%v", episode.DegradedSearch, ep1.Degraded)
	}
	pass(t, "write with index down: 201 + degraded note %q (id=%s)", episode.DegradedSearch, ep1.Record.ID)

	// Hot has it — proven by parsing the canonical file, not by the response.
	rec, ok := h.hotEpisodeByID(t, key, ep1.Record.ID)
	if !ok || rec.Text != ep1.Record.Text {
		failf(t, "hot inspector: degraded write %s missing or mismatched in %s (present=%v)",
			ep1.Record.ID, h.hotEpisodePath(key), ok)
	}
	// And the manifest both counts the commit and admits the failed mirror.
	fs, ok := h.hotManifest(t).Files[fileKey]
	if !ok || fs.RecordCount != 2 || !fs.Dirty {
		failf(t, "manifest inspector: want Files[%q] RecordCount=2 Dirty=true after the degraded write, got %+v (present=%v)",
			fileKey, fs, ok)
	}
	pass(t, "hot inspector: canonical file holds %s; manifest RecordCount=2 Dirty=true", ep1.Record.ID)

	// Degraded reads are honest 503s in the frozen §5 vocabulary...
	qs := url.Values{"q": {p01DegradedToken}}.Encode()
	st, env := h.api(t).getJSON(t, projPath(key)+"/episodes/search?"+qs)
	if st != http.StatusServiceUnavailable || env.Success {
		failf(t, "degraded read: want HTTP 503 success=false, got HTTP %d success=%v error=%q", st, env.Success, env.Error)
	}
	if env.Error == nil || env.Error.Message != episode.DegradedSearch {
		failf(t, "degraded read: want error message %q, got %q", episode.DegradedSearch, env.Error)
	}
	// ...while the canonical read path keeps answering from hot alone.
	if getSt, got := h.getEpisode(t, key, ep1.Record.ID); getSt != http.StatusOK || got.Text != ep1.Record.Text {
		failf(t, "GET episode during outage: want 200 from hot, got HTTP %d record %+v", getSt, got)
	}
	pass(t, "outage honesty: search=503 %q, get-by-id=200 from hot", episode.DegradedSearch)

	// ---- restart: the index must NOT have gained the record by magic ----
	h.startContainer(t, osContainer)
	// startContainer waits for HTTP, but OpenSearch answers before its shards
	// finish recovering and a realtime GET on an unallocated shard is a 503 —
	// so wait for the baseline doc to be readable again. This poll reads the
	// store directly (the server is never involved), so nothing can converge
	// ep1 behind it: once the shard is up, the ep1 absence check below is
	// deterministic — the bounce preserved data (stop, not down -v), the
	// baseline doc survived, and the degraded write is exactly the one hole.
	baselinePath := "/" + osIndex + "/_doc/" + url.PathEscape(key.String()+"#"+ep0.Record.ID)
	h.waitFor(t, "opensearch shards recovered after restart", healthTimeout, func() (bool, string) {
		status, _ := h.osRequest(t, http.MethodGet, baselinePath, "")
		if status == http.StatusOK {
			return true, "baseline doc readable again"
		}
		return false, fmt.Sprintf("HTTP %d on baseline doc (shard still recovering?)", status)
	})
	if _, found := h.osDoc(t, key, ep1.Record.ID); found {
		failf(t, "os inspector: degraded write %s present in the index before any convergence ran — the 201 secretly depended on the index", ep1.Record.ID)
	}
	pass(t, "after restart: index still holds %s and still lacks %s — hot alone carried the write", ep0.Record.ID, ep1.Record.ID)

	// ---- F18 recovery: the stat-gate converges on its own, no reindex ----
	h.waitFor(t, "degraded write converges via stat-gate after recovery", p01RecoveryTimeout, func() (bool, string) {
		status, hits := h.searchEpisodes(t, key, p01DegradedToken)
		if status != http.StatusOK {
			return false, "search HTTP " + http.StatusText(status)
		}
		for _, hit := range hits {
			if hit.Record.ID == ep1.Record.ID {
				return true, "hit id=" + hit.Record.ID
			}
		}
		return false, "200 but ids=" + strings.Join(hitIDs(hits), ",")
	})
	src, found := h.osDoc(t, key, ep1.Record.ID)
	if !found || !strings.Contains(string(src), p01DegradedToken) {
		failf(t, "os inspector: converged doc %s missing or lacks token after recovery (found=%v): %.300s",
			ep1.Record.ID, found, src)
	}
	if fs := h.hotManifest(t).Files[fileKey]; fs.Dirty {
		failf(t, "manifest inspector: dirty flag still set after successful convergence: %+v", fs)
	}
	pass(t, "P1 complete: hot-first write survived the index outage and auto-converged after recovery")
}
