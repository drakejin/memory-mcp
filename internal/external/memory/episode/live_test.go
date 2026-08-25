package episodemem

// Opt-in integration test against a live OpenSearch container (with the
// analysis-nori plugin, deploy/docker-compose.yml). Guarded by
// DJ_MEMORY_LIVE_TEST=1 — the single project-wide live gate, shared with
// internal/external/memory/knowledge and internal/external/thirdparty/cold — so `make test` stays container-free:
//
//	make live
//	# or: DJ_MEMORY_LIVE_TEST=1 go test ./internal/external/memory/episode/ -run TestLive -v
//
// It writes to a dedicated scratch index and drops it afterwards; the
// production index dj-memory-episodic is never touched.

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

const liveIndexName = "dj-memory-episodic-livetest"

// liveEnvVar is the one opt-in gate for every live test in this repo; the same
// name and the same exact-"1" comparison appear in
// internal/external/memory/knowledge and internal/external/thirdparty/cold,
// and `make live` sets it (code-standards §3: no magic strings, one uniform
// pattern).
const (
	liveEnvVar   = "DJ_MEMORY_LIVE_TEST"
	liveEnvValue = "1"
)

func newLiveClient(t *testing.T) *client {
	t.Helper()
	if os.Getenv(liveEnvVar) != liveEnvValue {
		t.Skip("live OpenSearch test: set " + liveEnvVar + "=" + liveEnvValue + " with the compose opensearch container running (make live)")
	}
	url := os.Getenv("DJ_MEMORY_OPENSEARCH_URL")
	if url == "" {
		url = "http://127.0.0.1:9200"
	}
	// newClient, not New: the scratch index is a test concern, so it never
	// becomes a production Config knob.
	c, err := newClient(Config{URL: url, Logger: slog.New(slog.DiscardHandler)}, liveIndexName)
	if err != nil {
		t.Fatalf("newClient(%s): %v", url, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("live OpenSearch at %s not reachable: %v (compose-up first)", url, err)
	}
	return c
}

func TestLiveEpisodicIndexLifecycle(t *testing.T) {
	// Arrange
	c := newLiveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if err := c.Drop(cleanupCtx); err != nil {
			t.Errorf("cleanup Drop() = %v", err)
		}
	})
	if err := c.Drop(ctx); err != nil { // fresh start even after a failed prior run
		t.Fatalf("pre-test Drop() = %v", err)
	}

	if err := c.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex() = %v", err)
	}
	if err := c.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex() second call must be idempotent, got %v", err)
	}

	key := projectkey.Key{Workspace: "blackbox", Team: "search", Project: "livetest"}
	otherKey := projectkey.Key{Workspace: "blackbox", Team: "search", Project: "other"}
	base := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	recs := []episode.Record{
		{ID: "01JD000000000000000000000A", Kind: episode.KindEvent, OccurredAt: base,
			Actor: episode.ActorAgent, Text: "보안을 끄고 오픈서치를 배포했다", Entities: []string{"opensearch"}},
		{ID: "01JD000000000000000000000B", Kind: episode.KindDecision, OccurredAt: base.Add(24 * time.Hour),
			Actor: episode.ActorUser, Text: "노리 형태소 분석기를 사용하기로 결정했다", Entities: []string{"nori"}},
		{ID: "01JD000000000000000000000C", Kind: episode.KindObservation, OccurredAt: base.Add(48 * time.Hour),
			Actor: episode.ActorSystem, Text: "재수화 후 문서 수가 일치함을 관찰했다", Entities: []string{"rehydrate"}},
	}

	// Act: realtime upsert (refresh=true) must be immediately searchable.
	if err := c.IndexRecords(ctx, key, recs); err != nil {
		t.Fatalf("IndexRecords() = %v", err)
	}
	if err := c.IndexRecords(ctx, otherKey, []episode.Record{
		{ID: "01JD000000000000000000000D", Kind: episode.KindEvent, OccurredAt: base,
			Actor: episode.ActorAgent, Text: "다른 프로젝트의 보안 기록", Entities: nil},
	}); err != nil {
		t.Fatalf("IndexRecords(otherKey) = %v", err)
	}

	// Assert: nori morphological match — stored "보안을 끄고", queried "보안을 끄는" (§10-2).
	hits, err := c.Search(ctx, key, Query{Text: "보안을 끄는"})
	if err != nil {
		t.Fatalf("Search() = %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("nori search: no hits for 보안을 끄는 against stored 보안을 끄고")
	}
	if hits[0].Record.ID != recs[0].ID {
		t.Errorf("top hit = %s, want %s", hits[0].Record.ID, recs[0].ID)
	}
	if hits[0].Record.Text != "" {
		t.Errorf("hit carries full body %q, want excerpt-only", hits[0].Record.Text)
	}
	if !strings.Contains(hits[0].Excerpt, "보안") {
		t.Errorf("excerpt %q does not highlight the query stem", hits[0].Excerpt)
	}
	for _, h := range hits {
		if h.Record.ID == "01JD000000000000000000000D" {
			t.Error("project scoping leaked a hit from another project")
		}
	}

	// Assert: kinds filter.
	hits, err = c.Search(ctx, key, Query{Kinds: []episode.Kind{episode.KindDecision}})
	if err != nil {
		t.Fatalf("Search(kinds) = %v", err)
	}
	if len(hits) != 1 || hits[0].Record.Kind != episode.KindDecision {
		t.Errorf("kinds filter: got %+v, want the single decision record", hits)
	}

	// Assert: time range filter keeps only the last two records.
	hits, err = c.Search(ctx, key, Query{From: base.Add(12 * time.Hour)})
	if err != nil {
		t.Fatalf("Search(range) = %v", err)
	}
	if len(hits) != 2 {
		t.Errorf("time range filter: got %d hits, want 2", len(hits))
	}

	// Assert: match_all ties sort newest first.
	hits, err = c.Search(ctx, key, Query{})
	if err != nil {
		t.Fatalf("Search(all) = %v", err)
	}
	if len(hits) != 3 || hits[0].Record.ID != recs[2].ID {
		t.Errorf("newest-first tie ordering violated: %+v", hits)
	}

	// Assert: counts, project-scoped and global.
	if n, err := c.DocCount(ctx, key); err != nil || n != 3 {
		t.Errorf("DocCount(key) = %d, %v; want 3, nil", n, err)
	}
	if n, err := c.DocCount(ctx, projectkey.Key{}); err != nil || n != 4 {
		t.Errorf("DocCount(all) = %d, %v; want 4, nil", n, err)
	}

	// Act: idempotent re-index must not duplicate docs (upsert semantics).
	if err := c.IndexRecords(ctx, key, recs[:1]); err != nil {
		t.Fatalf("re-IndexRecords() = %v", err)
	}
	if n, err := c.DocCount(ctx, key); err != nil || n != 3 {
		t.Errorf("DocCount after re-index = %d, %v; want 3, nil", n, err)
	}

	// Act: delete-by-id (aging), tolerant of repeats.
	ids := []string{recs[0].ID}
	if err := c.DeleteRecords(ctx, key, ids); err != nil {
		t.Fatalf("DeleteRecords() = %v", err)
	}
	if err := c.DeleteRecords(ctx, key, ids); err != nil {
		t.Fatalf("DeleteRecords() repeat must be idempotent, got %v", err)
	}
	if n, err := c.DocCount(ctx, key); err != nil || n != 2 {
		t.Errorf("DocCount after delete = %d, %v; want 2, nil", n, err)
	}

	// Act: drop + rebuild = bulk rehydration path (§5).
	if err := c.Drop(ctx); err != nil {
		t.Fatalf("Drop() = %v", err)
	}
	if n, err := c.DocCount(ctx, key); err != nil || n != 0 {
		t.Errorf("DocCount after drop = %d, %v; want 0, nil", n, err)
	}
	if err := c.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex(rebuild) = %v", err)
	}
	if err := c.IndexRecords(ctx, key, recs); err != nil {
		t.Fatalf("IndexRecords(rehydrate) = %v", err)
	}
	hits, err = c.Search(ctx, key, Query{Text: "보안을 끄는"})
	if err != nil || len(hits) == 0 {
		t.Fatalf("post-rehydration search: hits=%d err=%v, want same hit as before", len(hits), err)
	}
}
