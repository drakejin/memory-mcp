package graph

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/dbtype"

	"github.com/drakejin/memory-mcp/internal/errs"
	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/knowledge"
)

// fakeRunner implements the same narrow runner interface as boltRunner, so the
// Cypher layer is exercised without a container (code-standards §1.1).
type fakeRunner struct {
	calls     []fakeCall
	results   []fakeResult
	runErr    error
	verifyErr error
	closeErr  error
}

type fakeCall struct {
	write  bool
	query  string
	params map[string]any
}

// fakeResult replays records for every query containing match. Order matters:
// the first match wins, so specific patterns come first.
type fakeResult struct {
	match   string
	records []*neo4j.Record
}

func (f *fakeRunner) Verify(context.Context) error { return f.verifyErr }
func (f *fakeRunner) Close(context.Context) error  { return f.closeErr }

func (f *fakeRunner) Run(_ context.Context, write bool, query string, params map[string]any) ([]*neo4j.Record, error) {
	f.calls = append(f.calls, fakeCall{write: write, query: query, params: params})
	if f.runErr != nil {
		return nil, f.runErr
	}
	for _, res := range f.results {
		if strings.Contains(query, res.match) {
			return res.records, nil
		}
	}
	return nil, nil
}

// queries returns every statement seen so far.
func (f *fakeRunner) queries() []string {
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.query
	}
	return out
}

// lastCall returns the most recent statement.
func (f *fakeRunner) lastCall(t *testing.T) fakeCall {
	t.Helper()
	if len(f.calls) == 0 {
		t.Fatal("no statement was run")
	}
	return f.calls[len(f.calls)-1]
}

func newTestClient(r runner) (*client, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &client{db: r, log: log}, buf
}

func record(keys []string, values []any) *neo4j.Record {
	return &neo4j.Record{Keys: keys, Values: values}
}

// assertKind checks the semantic kind and op of a domain error.
func assertKind(t *testing.T, err error, sentinel error, wantOp string) {
	t.Helper()
	if !errors.Is(err, sentinel) {
		t.Fatalf("want %v, got %v", sentinel, err)
	}
	var domain *errs.Error
	if !errors.As(err, &domain) {
		t.Fatalf("error is not *errs.Error: %#v", err)
	}
	if domain.Op != wantOp {
		t.Errorf("op = %q, want %q", domain.Op, wantOp)
	}
}

var errDriver = errors.New("syntax error at line 1")

func TestNew(t *testing.T) {
	tests := []struct {
		name     string
		cfg      Config
		sentinel error
	}{
		{"valid bolt url", Config{URL: "bolt://127.0.0.1:7687", User: "neo4j", Password: "pw"}, nil},
		{"empty url", Config{}, errs.ErrInvalid},
		{"blank url", Config{URL: "   "}, errs.ErrInvalid},
		{"unsupported scheme", Config{URL: "http://127.0.0.1:7687"}, errs.ErrInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New(tt.cfg)
			if tt.sentinel == nil {
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				if c == nil {
					t.Fatal("New returned a nil Client")
				}
				return
			}
			if c != nil {
				t.Errorf("failed New must return a nil Client, got %#v", c)
			}
			assertKind(t, err, tt.sentinel, opNew)
		})
	}
}

// TestNewDoesNotDial guards §1 rule 4: the constructor performs no I/O, so an
// endpoint that is not listening still yields a usable Client.
func TestNewDoesNotDial(t *testing.T) {
	c, err := New(Config{URL: "bolt://127.0.0.1:1", User: "neo4j", Password: "pw"})
	if err != nil {
		t.Fatalf("New must not dial: %v", err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestNewDefaultsLogger(t *testing.T) {
	c, err := New(Config{URL: "bolt://127.0.0.1:7687"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	impl, ok := c.(*client)
	if !ok {
		t.Fatalf("New returned %T, want *client", c)
	}
	if impl.log != slog.Default() {
		t.Fatal("nil Config.Logger must fall back to slog.Default")
	}
}

func TestPing(t *testing.T) {
	tests := []struct {
		name      string
		verifyErr error
		sentinel  error
	}{
		{"reachable", nil, nil},
		{"connectivity failure", &neo4j.ConnectivityError{Inner: errors.New("dial refused")}, errs.ErrUnavailable},
		// Every probe failure is the degraded signal, even an auth error.
		{"any failure is unavailable", errDriver, errs.ErrUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestClient(&fakeRunner{verifyErr: tt.verifyErr})
			err := c.Ping(context.Background())
			if tt.sentinel == nil {
				if err != nil {
					t.Fatalf("ping: %v", err)
				}
				return
			}
			assertKind(t, err, tt.sentinel, opPing)
		})
	}
}

func TestClose(t *testing.T) {
	c, _ := newTestClient(&fakeRunner{})
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	failing, _ := newTestClient(&fakeRunner{closeErr: errDriver})
	assertKind(t, failing.Close(context.Background()), errs.ErrInternal, opClose)
}

func testNode(name string) knowledge.Node {
	return knowledge.Node{
		ID:      newID(),
		Kind:    knowledge.KindFact,
		Name:    name,
		State:   knowledge.StateActive,
		Trust:   knowledge.TrustUserStated,
		Created: testNow,
		Updated: testNow,
	}
}

func TestUpsertNodes(t *testing.T) {
	n := testNode("nori 분석기")
	r := &fakeRunner{}
	c, _ := newTestClient(r)

	if err := c.UpsertNodes(context.Background(), testKey, nil); err != nil {
		t.Fatalf("empty upsert: %v", err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("empty upsert must not touch the driver: %v", r.queries())
	}

	if err := c.UpsertNodes(context.Background(), testKey, []knowledge.Node{n}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Schema DDL converges once, then the MERGE runs.
	if len(r.calls) != 3 {
		t.Fatalf("statements = %v, want 2 schema + 1 merge", r.queries())
	}
	if !strings.Contains(r.calls[0].query, "CREATE INDEX") || !strings.Contains(r.calls[1].query, "FULLTEXT INDEX") {
		t.Fatalf("schema statements = %v", r.queries()[:2])
	}
	merge := r.lastCall(t)
	if !merge.write {
		t.Error("upsert must run as a write")
	}
	if !strings.Contains(merge.query, "MERGE (k:"+nodeLabel) {
		t.Errorf("merge query = %q", merge.query)
	}
	rows, ok := merge.params["nodes"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("params[nodes] = %#v", merge.params["nodes"])
	}
	props, ok := rows[0].(map[string]any)
	if !ok || props["id"] != n.ID || props["ws"] != testKey.Workspace {
		t.Fatalf("row = %#v", rows[0])
	}

	// Second pass must not repeat the DDL.
	if err := c.UpsertNodes(context.Background(), testKey, []knowledge.Node{n}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if len(r.calls) != 4 {
		t.Fatalf("schema converged more than once: %v", r.queries())
	}
}

func TestUpsertNodesErrors(t *testing.T) {
	tests := []struct {
		name     string
		runErr   error
		sentinel error
		wantOp   string
	}{
		{"schema failure", errDriver, errs.ErrInternal, opSchema},
		{"connectivity failure", &neo4j.ConnectivityError{Inner: errors.New("reset")}, errs.ErrUnavailable, opSchema},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &fakeRunner{runErr: tt.runErr}
			c, _ := newTestClient(r)
			err := c.UpsertNodes(context.Background(), testKey, []knowledge.Node{testNode("x")})
			assertKind(t, err, tt.sentinel, tt.wantOp)
			if c.schemaReady {
				t.Error("failed schema must stay unconverged so the next call retries")
			}
		})
	}
}

// TestUpsertNodesMergeError isolates the MERGE failure from the schema pass.
func TestUpsertNodesMergeError(t *testing.T) {
	r := &fakeRunner{}
	c, _ := newTestClient(r)
	if err := c.UpsertNodes(context.Background(), testKey, []knowledge.Node{testNode("x")}); err != nil {
		t.Fatalf("warm-up upsert: %v", err)
	}
	r.runErr = errDriver
	err := c.UpsertNodes(context.Background(), testKey, []knowledge.Node{testNode("y")})
	assertKind(t, err, errs.ErrInternal, opUpsertNodes)
}

func TestUpsertEdges(t *testing.T) {
	from, to := newID(), newID()
	edge := knowledge.Edge{From: from, To: to, Rel: knowledge.RelRelatesTo, Confidence: 0.8}

	t.Run("empty is a no-op", func(t *testing.T) {
		r := &fakeRunner{}
		c, _ := newTestClient(r)
		if err := c.UpsertEdges(context.Background(), testKey, nil); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if len(r.calls) != 0 {
			t.Fatalf("driver touched: %v", r.queries())
		}
	})

	t.Run("all merged stays quiet", func(t *testing.T) {
		r := &fakeRunner{results: []fakeResult{{match: "MERGE (a)-[r:", records: []*neo4j.Record{record([]string{"merged"}, []any{int64(1)})}}}}
		c, logs := newTestClient(r)
		if err := c.UpsertEdges(context.Background(), testKey, []knowledge.Edge{edge}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		call := r.lastCall(t)
		if call.params["ws"] != testKey.Workspace {
			t.Errorf("project scope missing: %v", call.params)
		}
		rows, ok := call.params["edges"].([]any)
		if !ok || len(rows) != 1 {
			t.Fatalf("params[edges] = %#v", call.params["edges"])
		}
		if strings.Contains(logs.String(), "skipped") {
			t.Errorf("unexpected warning: %s", logs.String())
		}
	})

	t.Run("missing endpoint warns", func(t *testing.T) {
		// No merged column at all: count reads as 0 < 1 requested.
		r := &fakeRunner{}
		c, logs := newTestClient(r)
		if err := c.UpsertEdges(context.Background(), testKey, []knowledge.Edge{edge}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if !strings.Contains(logs.String(), "endpoint missing") {
			t.Errorf("want a degraded warning, logs = %s", logs.String())
		}
	})

	t.Run("driver failure", func(t *testing.T) {
		r := &fakeRunner{runErr: &neo4j.ConnectivityError{Inner: errors.New("reset")}}
		c, _ := newTestClient(r)
		err := c.UpsertEdges(context.Background(), testKey, []knowledge.Edge{edge})
		assertKind(t, err, errs.ErrUnavailable, opUpsertEdges)
	})
}

func TestDeleteNode(t *testing.T) {
	id := newID()
	r := &fakeRunner{}
	c, _ := newTestClient(r)
	if err := c.DeleteNode(context.Background(), testKey, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	call := r.lastCall(t)
	if !call.write || !strings.Contains(call.query, "DETACH DELETE n") {
		t.Fatalf("delete query = %q (write=%v)", call.query, call.write)
	}
	if call.params["id"] != id || call.params["proj"] != testKey.Project {
		t.Fatalf("params = %v", call.params)
	}

	failing, _ := newTestClient(&fakeRunner{runErr: errDriver})
	assertKind(t, failing.DeleteNode(context.Background(), testKey, id), errs.ErrInternal, opDeleteNode)
}

func TestSearch(t *testing.T) {
	active := testNode("보안 플러그인 설정")
	hit := record([]string{"node"}, []any{dbNode("e0", active)})

	t.Run("returns parsed nodes", func(t *testing.T) {
		r := &fakeRunner{results: []fakeResult{{match: "db.index.fulltext", records: []*neo4j.Record{hit}}}}
		c, _ := newTestClient(r)
		nodes, err := c.Search(context.Background(), testKey, "보안", true)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(nodes) != 1 || nodes[0].ID != active.ID {
			t.Fatalf("hits = %v", ids(nodes))
		}
		call := r.lastCall(t)
		if call.write {
			t.Error("search must run as a read")
		}
		if call.params["q"] != "보안" || call.params["includeArchived"] != true || call.params["index"] != fulltextIndex {
			t.Fatalf("params = %v", call.params)
		}
	})

	t.Run("skips non-node rows", func(t *testing.T) {
		junk := record([]string{"node"}, []any{"not-a-node"})
		r := &fakeRunner{results: []fakeResult{{match: "db.index.fulltext", records: []*neo4j.Record{junk}}}}
		c, _ := newTestClient(r)
		nodes, err := c.Search(context.Background(), testKey, "보안", false)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(nodes) != 0 {
			t.Fatalf("nodes = %v, want none", ids(nodes))
		}
	})

	t.Run("driver failure", func(t *testing.T) {
		c, _ := newTestClient(&fakeRunner{runErr: errDriver})
		_, err := c.Search(context.Background(), testKey, "보안", false)
		// The schema pass runs first, so its op is the one reported.
		assertKind(t, err, errs.ErrInternal, opSchema)
	})

	t.Run("query failure after schema", func(t *testing.T) {
		r := &fakeRunner{}
		c, _ := newTestClient(r)
		if err := c.UpsertNodes(context.Background(), testKey, []knowledge.Node{active}); err != nil {
			t.Fatalf("warm-up: %v", err)
		}
		r.runErr = context.DeadlineExceeded
		_, err := c.Search(context.Background(), testKey, "보안", false)
		assertKind(t, err, errs.ErrUnavailable, opSearch)
	})
}

func TestNeighborhood(t *testing.T) {
	center := testNode("memory-mcp")
	near := testNode("fact-1")
	centerDB, nearDB := dbNode("e0", center), dbNode("e1", near)
	path := dbtype.Path{
		Nodes: []dbtype.Node{centerDB, nearDB},
		Relationships: []dbtype.Relationship{{
			ElementId: "r0", StartElementId: "e1", EndElementId: "e0",
			Type: relType, Props: map[string]any{"rel": string(knowledge.RelAbout), "confidence": 0.9},
		}},
	}
	found := record([]string{"c", "paths"}, []any{centerDB, []any{path}})

	t.Run("folds paths into a subgraph", func(t *testing.T) {
		r := &fakeRunner{results: []fakeResult{{match: "OPTIONAL MATCH p =", records: []*neo4j.Record{found}}}}
		c, _ := newTestClient(r)
		g, err := c.Neighborhood(context.Background(), testKey, "memory-mcp", 3)
		if err != nil {
			t.Fatalf("neighborhood: %v", err)
		}
		if len(g.Nodes) != 2 || len(g.Edges) != 1 {
			t.Fatalf("subgraph = %d nodes %d edges, want 2/1", len(g.Nodes), len(g.Edges))
		}
		call := r.lastCall(t)
		if !strings.Contains(call.query, "*1..3") {
			t.Errorf("depth not interpolated: %q", call.query)
		}
		if call.params["entity"] != "memory-mcp" {
			t.Errorf("params = %v", call.params)
		}
	})

	t.Run("depth clamped", func(t *testing.T) {
		r := &fakeRunner{results: []fakeResult{{match: "OPTIONAL MATCH p =", records: []*neo4j.Record{found}}}}
		c, _ := newTestClient(r)
		if _, err := c.Neighborhood(context.Background(), testKey, "memory-mcp", 999); err != nil {
			t.Fatalf("neighborhood: %v", err)
		}
		if !strings.Contains(r.lastCall(t).query, "*1..10") {
			t.Errorf("depth not clamped: %q", r.lastCall(t).query)
		}
	})

	t.Run("unknown entity is not found", func(t *testing.T) {
		c, _ := newTestClient(&fakeRunner{})
		_, err := c.Neighborhood(context.Background(), testKey, "없는-엔티티", 1)
		assertKind(t, err, errs.ErrNotFound, opNeighborhood)
	})

	t.Run("row without a center is not found", func(t *testing.T) {
		blank := record([]string{"c", "paths"}, []any{nil, nil})
		r := &fakeRunner{results: []fakeResult{{match: "OPTIONAL MATCH p =", records: []*neo4j.Record{blank}}}}
		c, _ := newTestClient(r)
		_, err := c.Neighborhood(context.Background(), testKey, "memory-mcp", 1)
		assertKind(t, err, errs.ErrNotFound, opNeighborhood)
	})

	t.Run("driver failure", func(t *testing.T) {
		c, _ := newTestClient(&fakeRunner{runErr: errDriver})
		_, err := c.Neighborhood(context.Background(), testKey, "memory-mcp", 1)
		assertKind(t, err, errs.ErrInternal, opNeighborhood)
	})
}

func TestSupersedeChain(t *testing.T) {
	base := testNow.Add(-2 * time.Hour)
	oldest, middle, newest := testNode("v1"), testNode("v2"), testNode("v3")
	oldest.Created, middle.Created, newest.Created = base, base.Add(time.Hour), base.Add(2*time.Hour)

	row := record(
		[]string{"n", "olders", "newers"},
		[]any{dbNode("e1", middle), []any{dbNode("e0", oldest)}, []any{dbNode("e2", newest)}},
	)

	t.Run("oldest first", func(t *testing.T) {
		r := &fakeRunner{results: []fakeResult{{match: "collect(DISTINCT newer)", records: []*neo4j.Record{row}}}}
		c, _ := newTestClient(r)
		chain, err := c.SupersedeChain(context.Background(), testKey, middle.ID)
		if err != nil {
			t.Fatalf("chain: %v", err)
		}
		want := []string{oldest.ID, middle.ID, newest.ID}
		if got := ids(chain); len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Fatalf("chain = %v, want %v", got, want)
		}
		if !strings.Contains(r.lastCall(t).query, string(knowledge.RelSupersedes)) {
			t.Errorf("query must filter on the supersedes rel: %q", r.lastCall(t).query)
		}
	})

	t.Run("missing node is not found", func(t *testing.T) {
		c, _ := newTestClient(&fakeRunner{})
		_, err := c.SupersedeChain(context.Background(), testKey, newID())
		assertKind(t, err, errs.ErrNotFound, opSupersedeChain)
	})

	t.Run("driver failure", func(t *testing.T) {
		c, _ := newTestClient(&fakeRunner{runErr: errDriver})
		_, err := c.SupersedeChain(context.Background(), testKey, newID())
		assertKind(t, err, errs.ErrInternal, opSupersedeChain)
	})
}

func TestNodeCount(t *testing.T) {
	counted := []*neo4j.Record{record([]string{"c"}, []any{int64(7)})}

	tests := []struct {
		name       string
		key        hotstore.ProjectKey
		wantScoped bool
	}{
		{"zero key counts every project", hotstore.ProjectKey{}, false},
		{"project key scopes the count", testKey, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &fakeRunner{results: []fakeResult{{match: "count(n)", records: counted}}}
			c, _ := newTestClient(r)
			got, err := c.NodeCount(context.Background(), tt.key)
			if err != nil {
				t.Fatalf("count: %v", err)
			}
			if got != 7 {
				t.Fatalf("count = %d, want 7", got)
			}
			call := r.lastCall(t)
			if scoped := strings.Contains(call.query, "ws: $ws"); scoped != tt.wantScoped {
				t.Fatalf("scoped = %v, want %v (query %q)", scoped, tt.wantScoped, call.query)
			}
			if _, ok := call.params["ws"]; ok != tt.wantScoped {
				t.Fatalf("params = %v, want scoped=%v", call.params, tt.wantScoped)
			}
		})
	}

	t.Run("driver failure", func(t *testing.T) {
		c, _ := newTestClient(&fakeRunner{runErr: errDriver})
		got, err := c.NodeCount(context.Background(), testKey)
		assertKind(t, err, errs.ErrInternal, opNodeCount)
		if got != 0 {
			t.Fatalf("count = %d, want 0 on failure", got)
		}
	})
}

func TestClear(t *testing.T) {
	r := &fakeRunner{}
	c, _ := newTestClient(r)
	if err := c.Clear(context.Background()); err != nil {
		t.Fatalf("clear: %v", err)
	}
	call := r.lastCall(t)
	if !call.write || !strings.Contains(call.query, "MATCH (n:"+nodeLabel+") DETACH DELETE n") {
		t.Fatalf("clear query = %q (write=%v)", call.query, call.write)
	}

	failing, _ := newTestClient(&fakeRunner{runErr: errDriver})
	assertKind(t, failing.Clear(context.Background()), errs.ErrInternal, opClear)
}

func TestMapErr(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		sentinel error
		wantOp   string
	}{
		{"nil stays nil", nil, nil, ""},
		{"connectivity is unavailable", &neo4j.ConnectivityError{Inner: errors.New("dial")}, errs.ErrUnavailable, opSearch},
		{"deadline is unavailable", context.DeadlineExceeded, errs.ErrUnavailable, opSearch},
		{"query error is internal", errDriver, errs.ErrInternal, opSearch},
		{"domain kind is preserved", errs.NotFound("inner.Op", knowledge.EntityNode, "01JX"), errs.ErrNotFound, opSearch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapErr(opSearch, tt.err)
			if tt.sentinel == nil {
				if got != nil {
					t.Fatalf("want nil, got %v", got)
				}
				return
			}
			assertKind(t, got, tt.sentinel, tt.wantOp)
			if !errors.Is(got, tt.err) {
				t.Errorf("cause must stay in the chain: %v", got)
			}
		})
	}
}

// TestNoHTTPConcepts is a guard for code-standards §2: nothing below the
// handler may reference transport concepts.
func TestErrorsCarryPublicMessages(t *testing.T) {
	c, _ := newTestClient(&fakeRunner{runErr: errDriver})
	err := c.Clear(context.Background())
	var domain *errs.Error
	if !errors.As(err, &domain) {
		t.Fatalf("not a domain error: %v", err)
	}
	if strings.Contains(domain.Msg, errDriver.Error()) {
		t.Fatalf("driver detail leaked into the public message: %q", domain.Msg)
	}
}

// TestDeleteMissing pins the project-scoped reconciliation the partial
// rehydration path uses: everything in this project that is not in the keep
// list goes, and nothing outside the project is ever matched (§5).
func TestDeleteMissing(t *testing.T) {
	keep := []string{newID(), newID()}
	r := &fakeRunner{}
	c, _ := newTestClient(r)

	if err := c.DeleteMissing(context.Background(), testKey, keep); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
	call := r.lastCall(t)
	if !call.write {
		t.Errorf("DeleteMissing ran as a read transaction")
	}
	for _, want := range []string{"DETACH DELETE n", "WHERE NOT n.id IN $keep", "ws: $ws", "team: $team", "proj: $proj"} {
		if !strings.Contains(call.query, want) {
			t.Errorf("query is missing %q: %s", want, call.query)
		}
	}
	got, ok := call.params["keep"].([]string)
	if !ok || len(got) != len(keep) || got[0] != keep[0] || got[1] != keep[1] {
		t.Errorf("keep param = %v, want %v", call.params["keep"], keep)
	}
	if call.params["proj"] != testKey.Project {
		t.Errorf("params not scoped to the project: %v", call.params)
	}

	failing, _ := newTestClient(&fakeRunner{runErr: errDriver})
	assertKind(t, failing.DeleteMissing(context.Background(), testKey, keep), errs.ErrInternal, opDeleteMissing)
}

// A nil keep list means hot holds no nodes at all, so every node of the project
// must go. Sent as Cypher null, `n.id IN $keep` is never false and nothing would
// be deleted — so nil has to reach the driver as an empty list.
func TestDeleteMissingNilKeepDeletesEverything(t *testing.T) {
	r := &fakeRunner{}
	c, _ := newTestClient(r)

	if err := c.DeleteMissing(context.Background(), testKey, nil); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
	got, ok := r.lastCall(t).params["keep"].([]string)
	if !ok || got == nil {
		t.Fatalf("keep param = %#v, want a non-nil empty []string", r.lastCall(t).params["keep"])
	}
	if len(got) != 0 {
		t.Errorf("keep param = %v, want empty", got)
	}
}
