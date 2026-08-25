package knowledgemem

// Live tests exercise the real Neo4j container from deploy/docker-compose.yml.
// Guard: they skip unless DJ_MEMORY_LIVE_TEST=1 — the single project-wide live
// gate, shared with internal/external/memory/episode and internal/external/thirdparty/cold — so `go test ./...`
// stays hermetic. Run with:
//
//	make live
//	# or: DJ_MEMORY_LIVE_TEST=1 go test ./internal/external/memory/knowledge/ -run Live -v

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/service/knowledge"
	"github.com/drakejin/memory-mcp/internal/x/errs"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// liveEnvVar is the one opt-in gate for every live test in this repo; the same
// name and the same exact-"1" comparison appear in internal/external/memory/episode and
// internal/external/thirdparty/cold, and `make live` sets it (code-standards §3).
const (
	liveEnvVar   = "DJ_MEMORY_LIVE_TEST"
	liveEnvValue = "1"
)

// Fixed local credentials (architecture-v2.md §8).
const (
	liveDefaultURL = "bolt://127.0.0.1:7687"
	liveUser       = "neo4j"
	livePassword   = "djmemory-local"
)

func liveClient(t *testing.T) (Client, context.Context) {
	t.Helper()
	if os.Getenv(liveEnvVar) != liveEnvValue {
		t.Skip("live Neo4j test: set " + liveEnvVar + "=" + liveEnvValue + " with the compose neo4j container running (make live)")
	}
	url := os.Getenv("DJ_MEMORY_NEO4J_URL")
	if url == "" {
		url = liveDefaultURL
	}
	c, err := New(Config{URL: url, User: liveUser, Password: livePassword})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	t.Cleanup(func() {
		if err := c.Close(context.Background()); err != nil {
			t.Logf("close: %v", err)
		}
	})
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("neo4j unreachable (compose up first): %v", err)
	}
	return c, ctx
}

// liveKey isolates each run under a unique project so parallel/dirty
// containers never interfere; cleanup deletes only this project's nodes.
func liveKey(t *testing.T, c Client) projectkey.Key {
	t.Helper()
	// Real wall-clock nanoseconds, not newID(): fixture ids are a deterministic
	// counter, which would hand every run the same project and break isolation.
	key := projectkey.Key{Workspace: "livetest", Team: "graph", Project: fmt.Sprintf("p%d", time.Now().UnixNano())}
	impl, ok := c.(*client)
	if !ok {
		t.Fatalf("live client is %T, want *client", c)
	}
	t.Cleanup(func() {
		_, err := impl.write(context.Background(), "graph.livetest",
			`MATCH (n:`+nodeLabel+` {ws: $ws, team: $team, proj: $proj}) DETACH DELETE n`,
			withKey(key, nil))
		if err != nil {
			t.Logf("cleanup: %v", err)
		}
	})
	return key
}

func liveNode(name, body string, aliases []string, created time.Time) knowledge.Node {
	return knowledge.Node{
		ID:      newID(),
		Kind:    knowledge.KindFact,
		Name:    name,
		Body:    body,
		Aliases: aliases,
		State:   knowledge.StateActive,
		Trust:   knowledge.TrustAgentInferred,
		Created: created,
		Updated: created,
	}
}

func TestLiveUpsertIdempotentAndCount(t *testing.T) {
	c, ctx := liveClient(t)
	key := liveKey(t, c)

	n1 := liveNode("nori 분석기", "한국어 형태소 분석", []string{"nori"}, time.Now().UTC())
	n2 := liveNode("neo4j merge", "MERGE는 멱등", nil, time.Now().UTC())
	nodes := []knowledge.Node{n1, n2}

	for i := 0; i < 2; i++ { // twice: MERGE must converge, not duplicate
		if err := c.UpsertNodes(ctx, key, nodes); err != nil {
			t.Fatalf("upsert nodes (pass %d): %v", i, err)
		}
	}
	edge := knowledge.Edge{From: n1.ID, To: n2.ID, Rel: knowledge.RelRelatesTo, Provenance: []string{newID()}, Confidence: 0.8}
	for i := 0; i < 2; i++ {
		if err := c.UpsertEdges(ctx, key, []knowledge.Edge{edge}); err != nil {
			t.Fatalf("upsert edges (pass %d): %v", i, err)
		}
	}

	count, err := c.NodeCount(ctx, key)
	if err != nil {
		t.Fatalf("node count: %v", err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2 (idempotent MERGE)", count)
	}

	g, err := c.Neighborhood(ctx, key, "nori 분석기", 1)
	if err != nil {
		t.Fatalf("neighborhood: %v", err)
	}
	if len(g.Nodes) != 2 || len(g.Edges) != 1 {
		t.Fatalf("neighborhood = %d nodes %d edges, want 2/1", len(g.Nodes), len(g.Edges))
	}
	if g.Edges[0].From != n1.ID || g.Edges[0].To != n2.ID || g.Edges[0].Rel != knowledge.RelRelatesTo {
		t.Fatalf("edge = %+v", g.Edges[0])
	}
	// Alias lookup must also resolve the center.
	if _, err := c.Neighborhood(ctx, key, "nori", 1); err != nil {
		t.Fatalf("neighborhood by alias: %v", err)
	}
	if _, err := c.Neighborhood(ctx, key, "없는-엔티티", 1); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("missing entity: want KindNotFound, got %v", err)
	}
}

func TestLiveFulltextSearchAndArchivedOptIn(t *testing.T) {
	c, ctx := liveClient(t)
	key := liveKey(t, c)

	active := liveNode("보안 플러그인 설정", "로컬 컨테이너는 보안을 끄고 기동한다", nil, time.Now().UTC())
	archived := liveNode("옛 보안 설정", "예전 보안 방식", nil, time.Now().UTC())
	archived.State = knowledge.StateArchived
	archived.SupersededBy = active.ID
	if err := c.UpsertNodes(ctx, key, []knowledge.Node{active, archived}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Fulltext indexes populate asynchronously; poll briefly.
	var hits []knowledge.Node
	deadline := time.Now().Add(15 * time.Second)
	for {
		var err error
		hits, err = c.Search(ctx, key, "보안", false)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(hits) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(hits) != 1 || hits[0].ID != active.ID {
		t.Fatalf("default search = %v, want only active node %s", ids(hits), active.ID)
	}

	all, err := c.Search(ctx, key, "보안", true)
	if err != nil {
		t.Fatalf("search include_archived: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("include_archived search = %d hits, want 2", len(all))
	}
}

func TestLiveSupersedeChainAndPurge(t *testing.T) {
	c, ctx := liveClient(t)
	key := liveKey(t, c)

	base := time.Now().UTC().Add(-2 * time.Hour)
	oldest := liveNode("v1 사실", "처음 앎", nil, base)
	middle := liveNode("v2 사실", "고쳐 앎", nil, base.Add(time.Hour))
	newest := liveNode("v3 사실", "지금 앎", nil, base.Add(2*time.Hour))

	// Build the chain through the domain function, then replay hot → graph
	// exactly like rehydration does.
	g, err := knowledge.Supersede(knowledge.Graph{Nodes: []knowledge.Node{oldest}}, middle, []string{oldest.ID}, middle.Created)
	if err != nil {
		t.Fatalf("supersede middle: %v", err)
	}
	g, err = knowledge.Supersede(g, newest, []string{middle.ID}, newest.Created)
	if err != nil {
		t.Fatalf("supersede newest: %v", err)
	}
	if err := c.UpsertNodes(ctx, key, g.Nodes); err != nil {
		t.Fatalf("upsert nodes: %v", err)
	}
	if err := c.UpsertEdges(ctx, key, g.Edges); err != nil {
		t.Fatalf("upsert edges: %v", err)
	}

	// From the middle node the chain must cover all three, oldest first.
	chain, err := c.SupersedeChain(ctx, key, middle.ID)
	if err != nil {
		t.Fatalf("chain: %v", err)
	}
	want := []string{oldest.ID, middle.ID, newest.ID}
	if len(chain) != 3 {
		t.Fatalf("chain = %v, want %v", ids(chain), want)
	}
	for i, id := range want {
		if chain[i].ID != id {
			t.Fatalf("chain order = %v, want %v", ids(chain), want)
		}
	}

	if _, err := c.SupersedeChain(ctx, key, newID()); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("missing node: want KindNotFound, got %v", err)
	}

	// Purge the oldest (archived) node; count drops and chain shrinks.
	if err := c.DeleteNode(ctx, key, oldest.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	count, err := c.NodeCount(ctx, key)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("count after purge = %d, want 2", count)
	}
}

func TestLiveUnavailableIsDegradedSignal(t *testing.T) {
	if os.Getenv(liveEnvVar) != liveEnvValue {
		t.Skip("live Neo4j test: set " + liveEnvVar + "=" + liveEnvValue + " (make live)")
	}
	// A closed port must surface KindUnavailable, never a raw driver error.
	c, err := New(Config{URL: "bolt://127.0.0.1:1", User: liveUser, Password: livePassword})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer func() {
		if err := c.Close(context.Background()); err != nil {
			t.Logf("close: %v", err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Ping(ctx); !errors.Is(err, errs.ErrUnavailable) {
		t.Fatalf("ping on dead port: want KindUnavailable, got %v", err)
	}
}
