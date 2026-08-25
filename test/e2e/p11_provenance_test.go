//go:build e2e

// P11 — provenance survives cold (feature-inventory.md §2 P11): a knowledge
// node keeps naming its source episodes after those episodes age to S3, and
// GET /episodes/{id} keeps resolving them through the cold-archive fallback.
// The episode is consolidated through the only sanctioned path — being named
// in a node's provenance — not by flipping the flag at the store.
package e2e

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	cold "github.com/drakejin/memory-mcp/internal/external/thirdparty/cold"
	httpserver "github.com/drakejin/memory-mcp/internal/handler/app/httpserver"
	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/service/knowledge"
)

// p11Token is a single unambiguous word carried by the episode body, so the
// S3-served copy is provably the original text and not a reconstruction.
const p11Token = "provenancecoldtoken"

func TestP11_ProvenanceSurvivesCold(t *testing.T) {
	h.ensureServer(t)
	key := e2eKey("p11-prov")

	// The source episode: recorded through the API with a 40d-old occurred_at
	// so it becomes TTL-eligible the moment it is consolidated.
	occurred := time.Now().UTC().AddDate(0, 0, -40).Truncate(time.Second)
	ep := h.postEpisode(t, key, httpserver.CreateEpisodeRequest{
		Kind: episode.KindDecision, Actor: episode.ActorAgent,
		OccurredAt: occurred,
		Text:       "p11 결정: 보안 정책을 승인했다 " + p11Token,
		Entities:   []string{"security-policy"},
	})
	if rec, ok := h.hotEpisodeByID(t, key, ep.Record.ID); !ok || rec.Consolidated {
		failf(t, "hot inspector: want a fresh unconsolidated record, got present=%v rec=%+v", ok, rec)
	}

	// Distillation, the sanctioned way: naming the episode in a node's
	// provenance promotes it to consolidated in hot (§3).
	node := h.postNode(t, key, knowledge.CreateNodeInput{
		Kind: knowledge.KindFact, Name: "p11-provenance-fact",
		Body:  "보안 정책 승인 사실 — distilled from " + p11Token,
		Trust: knowledge.TrustAgentInferred, Provenance: []string{ep.Record.ID},
	})
	if rec, ok := h.hotEpisodeByID(t, key, ep.Record.ID); !ok || !rec.Consolidated {
		failf(t, "hot inspector: provenance promotion did not consolidate %s (present=%v rec=%+v)", ep.Record.ID, ok, rec)
	}
	if _, found := h.osDoc(t, key, ep.Record.ID); !found {
		failf(t, "precondition: episode %s not indexed after the API write", ep.Record.ID)
	}
	pass(t, "episode %s consolidated via node %s provenance", ep.Record.ID, node.Node.ID)

	// Age it for real.
	wantKey := cold.EpisodeArchiveKey(s3Username, key, cold.ArchiveMonth(occurred))
	if ok, ev := h.s3Exists(wantKey); ok {
		failf(t, "archive object exists before the run (dirty prefix?): %s", ev)
	}
	rep := h.consolidate(t, httpserver.ConsolidateRequest{Projects: []string{key.String()}})
	if len(rep.Failures) != 0 {
		failf(t, "consolidate: unexpected failures: %v", rep.Failures)
	}
	if rep.MovedEpisodes != 1 || !slices.Contains(rep.ArchiveKeys, wantKey) {
		failf(t, "consolidate: want the provenance episode moved into %s, got moved=%d keys=%v",
			wantKey, rep.MovedEpisodes, rep.ArchiveKeys)
	}

	// Store truth after aging: S3 has it, hot does not, the index dropped it.
	batch := decodeArchiveBatch(t, wantKey, h.s3Cat(t, wantKey))
	archived, found := agedRecordByID(batch, ep.Record.ID)
	if !found || !strings.Contains(archived.Text, p11Token) || !archived.Consolidated || !archived.OccurredAt.Equal(occurred) {
		failf(t, "s3 inspector: archive %s: want the full original record, got found=%v rec=%+v", wantKey, found, archived)
	}
	if _, ok := h.hotEpisodeByID(t, key, ep.Record.ID); ok {
		failf(t, "hot inspector: %s still hot after aging — the fallback below would prove nothing", ep.Record.ID)
	}
	if _, found := h.osDoc(t, key, ep.Record.ID); found {
		failf(t, "os inspector: %s still indexed after aging", ep.Record.ID)
	}
	pass(t, "s3 inspector: %s holds the episode; hot and index no longer do", wantKey)

	// The link itself survives in both knowledge stores.
	hotNode, ok := h.hotKnowledgeNode(t, key, node.Node.ID)
	if !ok || !slices.Contains(hotNode.Provenance, ep.Record.ID) {
		failf(t, "hot inspector: node %s lost provenance %s after aging: present=%v provenance=%v",
			node.Node.ID, ep.Record.ID, ok, hotNode.Provenance)
	}
	if prov, found := h.neoNodeField(t, key, node.Node.ID, "provenance"); !found || !strings.Contains(prov, ep.Record.ID) {
		failf(t, "neo inspector: node %s provenance lacks %s (found=%v value=%q)", node.Node.ID, ep.Record.ID, found, prov)
	}
	pass(t, "provenance link intact in hot knowledge doc and Neo4j after the episode left hot")

	// The P11 moment: the id still resolves — served from the S3 archive.
	status, got := h.getEpisode(t, key, ep.Record.ID)
	if status != http.StatusOK {
		failf(t, "GET episodes/%s: want 200 via cold fallback, got HTTP %d", ep.Record.ID, status)
	}
	if got.ID != ep.Record.ID || !strings.Contains(got.Text, p11Token) ||
		!got.OccurredAt.Equal(occurred) || !got.Consolidated || got.Kind != episode.KindDecision {
		failf(t, "cold fallback served a different record:\n  got  %+v\n  want id=%s text~%q occurred=%s consolidated",
			got, ep.Record.ID, p11Token, occurred)
	}
	// The fallback is read-only: serving from cold must not quietly re-insert
	// the record into hot.
	if _, ok := h.hotEpisodeByID(t, key, ep.Record.ID); ok {
		failf(t, "hot inspector: cold fallback re-inserted %s into the hot file", ep.Record.ID)
	}

	// Negative control: the fallback resolves ids, it is not a wildcard.
	bogus := newULIDAt(t, time.Now().UTC())
	if status, _ := h.getEpisode(t, key, bogus); status != http.StatusNotFound {
		failf(t, "GET episodes/%s (never existed): want 404, got HTTP %d", bogus, status)
	}
	pass(t, "P11 complete: provenance episode aged to cold and GET /episodes/{id} served it from S3")
}
