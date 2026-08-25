//go:build e2e

// P7 — search contract (feature-inventory.md §2 P7): responses carry
// excerpt + metadata + score only (the full body is never injected), Korean
// morphological variants match through the nori analyzer (particles/endings
// reduce to stems — mapping.go), and results are project-scoped: the same
// sentence stored in another project never bleeds into a search.
//
// Store-side proof strategy: raw OpenSearch queries show WHAT the index holds
// (both projects' copies exist — isolation is scoping, not absence), the hot
// files show the canonical body and the recall material staying put, and the
// wire envelope shows what the API refuses to hand back.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	httpserver "github.com/drakejin/memory-mcp/internal/handler/app/httpserver"
	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// Fixture vocabulary. Every token is unique to this file so hit counts can be
// asserted as exact numbers even though the OpenSearch index is shared.
const (
	// p07MorphStored / p07MorphSpecQuery are the spec's own P7 example pair:
	// "보안을 끄고" stored, "보안을 끄는" queried. p07MorphStemQuery shares NO
	// whole surface token with the stored text ("끄는" vs "끄고"), so a hit is
	// possible only through morphological stemming (끄고/끄는 → 끄) — a plain
	// whitespace analyzer would return zero.
	p07MorphStored    = "레이트리밋을 피하려고 보안을 끄고 게이트웨이를 재시작했다"
	p07MorphSpecQuery = "보안을 끄는"
	p07MorphStemQuery = "끄는"

	// p07LongNeedle sits at the head of the long body; p07TailSecret sits
	// thousands of runes past every possible highlight fragment, so its
	// presence anywhere in a search response means the full body leaked.
	//
	// The fixture words are deliberately digit-free fused nonwords: nori
	// splits letter/digit boundaries ("p07x" → p + 07 + x), so numeric
	// prefixes become subtokens shared across fixtures — an OR match on them
	// cross-hits unrelated records and even highlights fragments around the
	// tail secret. A pure-alpha run stays one token.
	p07LongNeedle = "sevenexcerptneedle"
	p07TailSecret = "seventailsecret"

	// p07CrossToken marks the sentence stored identically in two projects
	// (digit-free for the same tokenization reason).
	p07CrossToken = "sevencrossscope"

	// p07ExcerptRuneCap bounds a legal excerpt (2 highlight fragments × 150
	// chars + markup) with a wide margin; the long-body fixture is >3000 runes,
	// so an excerpt under this cap cannot be the body.
	p07ExcerptRuneCap = 1000
	// p07LongBodyMinRunes is the fixture floor that keeps the cap meaningful.
	p07LongBodyMinRunes = 3000
)

// p07SearchWait bounds the deterministic wait for the server-path search to
// answer with the expected hit count.
const p07SearchWait = 30 * time.Second

// p07Search runs the project-scoped server search until it answers 200 with
// exactly wantHits hits, returning the final envelope (raw data bytes for
// wire-level asserts) plus the decoded hits.
func p07Search(t *testing.T, key projectkey.Key, q string, wantHits int) (envelope, []episode.Hit) {
	t.Helper()
	qs := url.Values{"q": {q}}.Encode()
	var env envelope
	var hits []episode.Hit
	h.waitFor(t, fmt.Sprintf("search %s q=%q answers %d hit(s)", key, q, wantHits), p07SearchWait, func() (bool, string) {
		status, e := h.api(t).getJSON(t, projPath(key)+"/episodes/search?"+qs)
		if status != http.StatusOK {
			return false, fmt.Sprintf("HTTP %d error=%q", status, e.Error)
		}
		var hs []episode.Hit
		decodeData(t, e, &hs)
		if len(hs) != wantHits {
			return false, fmt.Sprintf("%d hits, ids=%v", len(hs), hitIDs(hs))
		}
		env, hits = e, hs
		return true, fmt.Sprintf("%d hits", len(hs))
	})
	return env, hits
}

// p07AssertExcerptOnly asserts the §7 recall principle on one hit, at both the
// decoded and the wire level: record.text is present and literally empty, and
// the excerpt is non-empty.
func p07AssertExcerptOnly(t *testing.T, env envelope, hit episode.Hit) {
	t.Helper()
	if hit.Record.Text != "" {
		failf(t, "hit %s carries record text %q — responses must be excerpt-only", hit.Record.ID, hit.Record.Text)
	}
	if hit.Excerpt == "" {
		failf(t, "hit %s has an empty excerpt — the caller would have nothing to recall from", hit.Record.ID)
	}
	// Wire level: the text field must EXIST and be exactly "" — a decode-based
	// check alone could not tell an empty field from an absent one.
	var wire []struct {
		Record map[string]json.RawMessage `json:"record"`
	}
	decodeData(t, env, &wire)
	for i, w := range wire {
		raw, ok := w.Record["text"]
		if !ok || string(raw) != `""` {
			failf(t, "wire hit %d: record.text = %s (present=%v), want the literal empty string", i, raw, ok)
		}
	}
}

// TestP07_KoreanMorphologyHit stores the P7 example sentence and proves the
// inflected query form hits — first at the store (raw _search against the
// nori-analyzed index, including a query sharing no surface token with the
// stored text), then through the server path.
func TestP07_KoreanMorphologyHit(t *testing.T) {
	h.ensureServer(t)
	key := e2eKey("p07-morph")

	ep := h.postEpisode(t, key, httpserver.CreateEpisodeRequest{
		Kind:     episode.KindEvent,
		Actor:    episode.ActorAgent,
		Text:     p07MorphStored,
		Entities: []string{},
	})
	if len(ep.Degraded) != 0 {
		failf(t, "postEpisode: unexpected degraded notes with every store up: %v", ep.Degraded)
	}
	h.osRefresh(t)

	// Store-level proof: both the spec pair and the pure-stem form match the
	// single stored record. The project holds exactly one record, so total
	// must be exactly 1 — not "at least".
	wantID := key.String() + "#" + ep.Record.ID
	for _, q := range []string{p07MorphStemQuery, p07MorphSpecQuery} {
		res, found := h.osSearchRaw(t, fmt.Sprintf(
			`{"size":10,"query":{"bool":{"must":[{"match":{"text":%q}}],"filter":[{"term":{"workspace":%q}},{"term":{"team":%q}},{"term":{"project":%q}}]}}}`,
			q, key.Workspace, key.Team, key.Project))
		if !found {
			failf(t, "os inspector: index absent while the server just indexed into it")
		}
		if res.Total != 1 || res.Hits[0].ID != wantID {
			failf(t, "os inspector: raw _search %q: want exactly 1 hit %s (stored %q), got total=%d hits=%v — nori stemming broken?",
				q, wantID, p07MorphStored, res.Total, res.Hits)
		}
	}
	pass(t, "os inspector: stored %q is hit by %q and by the zero-surface-overlap form %q",
		p07MorphStored, p07MorphSpecQuery, p07MorphStemQuery)

	// Server path with the spec-verbatim query; the answer must be excerpt-only.
	env, hits := p07Search(t, key, p07MorphSpecQuery, 1)
	if hits[0].Record.ID != ep.Record.ID {
		failf(t, "server search: hit id %s != stored %s", hits[0].Record.ID, ep.Record.ID)
	}
	p07AssertExcerptOnly(t, env, hits[0])
	pass(t, "server search: %q hits the record stored as %q, excerpt-only", p07MorphSpecQuery, p07MorphStored)
}

// TestP07_ExcerptNeverFullBody stores a long body and proves the search
// response carries a bounded excerpt while the deep-tail content never appears
// anywhere in the response — and that hot, the canonical store, still holds
// the full body (the contract trims the response, never the data).
func TestP07_ExcerptNeverFullBody(t *testing.T) {
	h.ensureServer(t)
	key := e2eKey("p07-excerpt")

	head := "incident digest " + p07LongNeedle + " for the excerpt contract."
	filler := strings.Repeat("filler segment alpha beta gamma delta epsilon zeta eta theta. ", 60)
	fullBody := head + " " + filler + p07TailSecret + " end of record."
	if utf8.RuneCountInString(fullBody) <= p07LongBodyMinRunes {
		failf(t, "fixture bug: body is only %d runes, want > %d", utf8.RuneCountInString(fullBody), p07LongBodyMinRunes)
	}
	if idx := strings.Index(fullBody, p07TailSecret); idx < p07LongBodyMinRunes {
		failf(t, "fixture bug: tail secret at byte %d could fall inside a highlight fragment", idx)
	}

	ep := h.postEpisode(t, key, httpserver.CreateEpisodeRequest{
		Kind:     episode.KindObservation,
		Actor:    episode.ActorAgent,
		Text:     fullBody,
		Entities: []string{},
	})
	if len(ep.Degraded) != 0 {
		failf(t, "postEpisode: unexpected degraded notes: %v", ep.Degraded)
	}
	if hotRec, ok := h.hotEpisodeByID(t, key, ep.Record.ID); !ok || hotRec.Text != fullBody {
		failf(t, "hot inspector: canonical store must keep the FULL body (present=%v, %d/%d bytes)",
			ok, len(hotRec.Text), len(fullBody))
	}
	h.osRefresh(t)

	env, hits := p07Search(t, key, p07LongNeedle, 1)
	hit := hits[0]
	if hit.Record.ID != ep.Record.ID {
		failf(t, "server search: hit id %s != stored %s", hit.Record.ID, ep.Record.ID)
	}
	p07AssertExcerptOnly(t, env, hit)
	if !strings.Contains(hit.Excerpt, p07LongNeedle) {
		failf(t, "excerpt %q does not surface the matched needle", hit.Excerpt)
	}
	if got := utf8.RuneCountInString(hit.Excerpt); got >= p07ExcerptRuneCap {
		failf(t, "excerpt is %d runes (cap %d) against a %d-rune body — that is a body, not an excerpt",
			got, p07ExcerptRuneCap, utf8.RuneCountInString(fullBody))
	}
	if bytes.Contains(env.Data, []byte(p07TailSecret)) {
		failf(t, "response data leaks the deep-tail content %q — the full body was injected", p07TailSecret)
	}
	pass(t, "excerpt-only: %d-rune excerpt for a %d-rune body, tail content absent from the wire, hot keeps the body",
		utf8.RuneCountInString(hit.Excerpt), utf8.RuneCountInString(fullBody))
}

// TestP07_CrossProjectContaminationZero stores the identical sentence in two
// projects and proves zero bleed: raw OpenSearch shows BOTH copies indexed
// (so isolation is the scope filter working, not data happening to be absent),
// each scoped search returns only its own record, and the recall bump lands
// only on the searched project's hot file.
func TestP07_CrossProjectContaminationZero(t *testing.T) {
	h.ensureServer(t)
	keyA := e2eKey("p07-scope-a")
	keyB := e2eKey("p07-scope-b")
	sentence := "교차 오염 검증 문장 " + p07CrossToken + " 그대로 기록"

	req := httpserver.CreateEpisodeRequest{
		Kind:     episode.KindEvent,
		Actor:    episode.ActorAgent,
		Text:     sentence,
		Entities: []string{},
	}
	epA := h.postEpisode(t, keyA, req)
	epB := h.postEpisode(t, keyB, req)
	if epA.Record.ID == epB.Record.ID {
		failf(t, "two projects minted the same episode id %s", epA.Record.ID)
	}

	// Hot isolation: each project file holds exactly its own record.
	recsA := h.hotEpisodes(t, keyA)
	recsB := h.hotEpisodes(t, keyB)
	if len(recsA) != 1 || recsA[0].ID != epA.Record.ID {
		failf(t, "hot inspector: %s: want exactly [%s], got %v", h.hotEpisodePath(keyA), epA.Record.ID, recsA)
	}
	if len(recsB) != 1 || recsB[0].ID != epB.Record.ID {
		failf(t, "hot inspector: %s: want exactly [%s], got %v", h.hotEpisodePath(keyB), epB.Record.ID, recsB)
	}
	if _, leaked := h.hotEpisodeByID(t, keyA, epB.Record.ID); leaked {
		failf(t, "hot inspector: project A's file contains project B's record %s", epB.Record.ID)
	}
	if _, leaked := h.hotEpisodeByID(t, keyB, epA.Record.ID); leaked {
		failf(t, "hot inspector: project B's file contains project A's record %s", epA.Record.ID)
	}
	pass(t, "hot inspector: one record per project file, no cross-writes")

	// Raw OS: both copies ARE indexed — the isolation below is scoping.
	h.osRefresh(t)
	unf, found := h.osSearchRaw(t, fmt.Sprintf(`{"size":50,"query":{"match":{"text":%q}}}`, p07CrossToken))
	if !found || unf.Total != 2 {
		failf(t, "os inspector: unfiltered %q: want both copies (total=2), found=%v total=%d — isolation cannot be proven against missing data",
			p07CrossToken, found, unf.Total)
	}
	for _, want := range []string{keyA.String() + "#" + epA.Record.ID, keyB.String() + "#" + epB.Record.ID} {
		seen := false
		for _, hit := range unf.Hits {
			if hit.ID == want {
				seen = true
			}
		}
		if !seen {
			failf(t, "os inspector: unfiltered hits lack %s: %v", want, unf.Hits)
		}
	}
	for _, tc := range []struct {
		key projectkey.Key
		id  string
	}{{keyA, epA.Record.ID}, {keyB, epB.Record.ID}} {
		res, found := h.osSearchRaw(t, fmt.Sprintf(
			`{"size":50,"query":{"bool":{"must":[{"match":{"text":%q}}],"filter":[{"term":{"workspace":%q}},{"term":{"team":%q}},{"term":{"project":%q}}]}}}`,
			p07CrossToken, tc.key.Workspace, tc.key.Team, tc.key.Project))
		if !found || res.Total != 1 || res.Hits[0].ID != tc.key.String()+"#"+tc.id {
			failf(t, "os inspector: %s-scoped raw search: want exactly its own hit, got found=%v total=%d hits=%v",
				tc.key, found, res.Total, res.Hits)
		}
	}
	pass(t, "os inspector: both copies indexed; scope filters return exactly one each")

	// Server search in A: only A's record; B's id appears nowhere in the wire.
	envA, hitsA := p07Search(t, keyA, p07CrossToken, 1)
	if hitsA[0].Record.ID != epA.Record.ID {
		failf(t, "server search A: hit %s != own record %s", hitsA[0].Record.ID, epA.Record.ID)
	}
	if bytes.Contains(envA.Data, []byte(epB.Record.ID)) {
		failf(t, "server search A: response data contains project B's id %s — cross-project contamination", epB.Record.ID)
	}

	// Recall bump isolation: A's record was recalled, B's stays untouched.
	recA, _ := h.hotEpisodeByID(t, keyA, epA.Record.ID)
	if recA.RecallCount < 1 || recA.LastRecalled == "" {
		failf(t, "hot inspector: A's record recall material not bumped after its own search: %+v", recA)
	}
	recB, _ := h.hotEpisodeByID(t, keyB, epB.Record.ID)
	if recB.RecallCount != 0 || recB.LastRecalled != "" {
		failf(t, "hot inspector: B's record was recall-bumped by A's search: count=%d last=%q — cross-project side effect",
			recB.RecallCount, recB.LastRecalled)
	}

	// And B's own search sees only B.
	envB, hitsB := p07Search(t, keyB, p07CrossToken, 1)
	if hitsB[0].Record.ID != epB.Record.ID {
		failf(t, "server search B: hit %s != own record %s", hitsB[0].Record.ID, epB.Record.ID)
	}
	if bytes.Contains(envB.Data, []byte(epA.Record.ID)) {
		failf(t, "server search B: response data contains project A's id %s", epA.Record.ID)
	}
	pass(t, "cross-project contamination zero: scoped searches, wire bytes and recall bumps all stay inside their project")
}
