package graph

import (
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/dbtype"

	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/knowledge"
)

var (
	testKey = hotstore.ProjectKey{Workspace: "drakejin", Team: "infra", Project: "memory-mcp"}
	testNow = time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
)

// idSeq backs newID. Fixtures only need ids that satisfy ulid.Valid and differ
// from one another, so a counter beats real entropy: values stay stable across
// runs, which keeps failure output readable. Test-only state.
var idSeq atomic.Int64

// newID mints a unique fixture ULID.
func newID() string {
	// 4-char head + 22 digits = the 26 Crockford characters ulid.Valid wants.
	return fmt.Sprintf("01JD%022d", idSeq.Add(1))
}

func TestClampDepth(t *testing.T) {
	tests := []struct {
		name  string
		depth int
		want  int
	}{
		{"zero defaults to 1", 0, defaultDepth},
		{"negative defaults to 1", -3, defaultDepth},
		{"in range passes through", 3, 3},
		{"max passes through", maxDepth, maxDepth},
		{"above max clamps", maxDepth + 7, maxDepth},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clampDepth(tt.depth); got != tt.want {
				t.Fatalf("clampDepth(%d) = %d, want %d", tt.depth, got, tt.want)
			}
		})
	}
}

// TestNodePropsRoundTrip guarantees that what UpsertNodes writes is exactly
// what Search/Neighborhood read back — the MERGE replay convergence property.
func TestNodePropsRoundTrip(t *testing.T) {
	n := knowledge.Node{
		ID:           newID(),
		Kind:         knowledge.KindFact,
		Name:         "보안 플러그인은 로컬에서 끈다",
		Body:         "OpenSearch 로컬 컨테이너는 security off",
		Aliases:      []string{"security-off", "보안"},
		State:        knowledge.StateArchived,
		Trust:        knowledge.TrustUserStated,
		Supersedes:   []string{newID()},
		SupersededBy: newID(),
		Provenance:   []string{newID(), newID()},
		Created:      testNow,
		Updated:      testNow.Add(time.Minute),
		ReviewAfter:  "2026-10-01T00:00:00Z",
	}

	props := nodeProps(testKey, n)
	if props["ws"] != testKey.Workspace || props["team"] != testKey.Team || props["proj"] != testKey.Project {
		t.Fatalf("project scope not embedded: %v", props)
	}
	if props["aliases_text"] != "security-off 보안" {
		t.Fatalf("aliases_text = %q", props["aliases_text"])
	}

	// The driver returns list properties as []any — simulate that before
	// converting back.
	got := propsToNode(props)
	if !reflect.DeepEqual(got, n) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, n)
	}
}

func TestNodePropsZeroTimes(t *testing.T) {
	n := knowledge.Node{ID: newID(), Kind: knowledge.KindEntity, Name: "x", State: knowledge.StateActive, Trust: knowledge.TrustImported}
	props := nodeProps(testKey, n)
	if props["created"] != "" || props["updated"] != "" {
		t.Fatalf("zero times must serialize to empty strings, got %v / %v", props["created"], props["updated"])
	}
	got := propsToNode(props)
	if !got.Created.IsZero() || !got.Updated.IsZero() {
		t.Fatalf("empty strings must parse back to zero times: %+v", got)
	}
}

func TestEdgePropsRoundTrip(t *testing.T) {
	e := knowledge.Edge{
		From:       newID(),
		To:         newID(),
		Rel:        knowledge.RelSupersedes,
		Provenance: []string{newID()},
		Confidence: 0.87,
	}
	props := edgeProps(e)
	if props["from"] != e.From || props["to"] != e.To || props["rel"] != string(e.Rel) {
		t.Fatalf("edge props mismatch: %v", props)
	}
	if props["confidence"] != e.Confidence {
		t.Fatalf("confidence = %v", props["confidence"])
	}
	if !reflect.DeepEqual(props["provenance"], toAnySlice(e.Provenance)) {
		t.Fatalf("provenance = %v", props["provenance"])
	}
}

func dbNode(elementID string, n knowledge.Node) dbtype.Node {
	return dbtype.Node{ElementId: elementID, Labels: []string{nodeLabel}, Props: nodeProps(testKey, n)}
}

func TestGraphFromPaths(t *testing.T) {
	center := knowledge.Node{ID: newID(), Kind: knowledge.KindEntity, Name: "memory-mcp", State: knowledge.StateActive, Trust: knowledge.TrustUserStated}
	mid := knowledge.Node{ID: newID(), Kind: knowledge.KindFact, Name: "fact-1", State: knowledge.StateActive, Trust: knowledge.TrustAgentInferred}
	far := knowledge.Node{ID: newID(), Kind: knowledge.KindLesson, Name: "lesson-1", State: knowledge.StateActive, Trust: knowledge.TrustAgentInferred}

	c, m, f := dbNode("e0", center), dbNode("e1", mid), dbNode("e2", far)
	relCM := dbtype.Relationship{
		ElementId: "r0", StartElementId: "e1", EndElementId: "e0",
		Type: "REL", Props: map[string]any{"rel": "about", "provenance": []any{"01X"}, "confidence": 0.9},
	}
	relMF := dbtype.Relationship{
		ElementId: "r1", StartElementId: "e2", EndElementId: "e1",
		Type: "REL", Props: map[string]any{"rel": "derived_from", "confidence": int64(1)},
	}

	// Two paths share the first hop — nodes and edges must deduplicate.
	paths := []dbtype.Path{
		{Nodes: []dbtype.Node{c, m}, Relationships: []dbtype.Relationship{relCM}},
		{Nodes: []dbtype.Node{c, m, f}, Relationships: []dbtype.Relationship{relCM, relMF}},
	}

	g := graphFromPaths(c, paths)

	if len(g.Nodes) != 3 {
		t.Fatalf("nodes = %d, want 3 (deduped)", len(g.Nodes))
	}
	if g.Nodes[0].ID != center.ID {
		t.Fatalf("center must come first, got %s", g.Nodes[0].ID)
	}
	if len(g.Edges) != 2 {
		t.Fatalf("edges = %d, want 2 (deduped)", len(g.Edges))
	}
	e0 := g.Edges[0]
	if e0.From != mid.ID || e0.To != center.ID || e0.Rel != knowledge.RelAbout || e0.Confidence != 0.9 {
		t.Fatalf("edge0 = %+v", e0)
	}
	if !reflect.DeepEqual(e0.Provenance, []string{"01X"}) {
		t.Fatalf("edge0 provenance = %v", e0.Provenance)
	}
	e1 := g.Edges[1]
	if e1.From != far.ID || e1.To != mid.ID || e1.Rel != knowledge.RelDerivedFrom || e1.Confidence != 1 {
		t.Fatalf("edge1 = %+v", e1)
	}
}

func TestGraphFromPathsCenterOnly(t *testing.T) {
	center := knowledge.Node{ID: newID(), Kind: knowledge.KindEntity, Name: "solo", State: knowledge.StateActive, Trust: knowledge.TrustImported}
	g := graphFromPaths(dbNode("e0", center), nil)
	if len(g.Nodes) != 1 || len(g.Edges) != 0 {
		t.Fatalf("got %d nodes %d edges, want 1/0", len(g.Nodes), len(g.Edges))
	}
	if g.Nodes[0].ID != center.ID {
		t.Fatalf("node = %s, want %s", g.Nodes[0].ID, center.ID)
	}
}

func TestSortChainOldestFirst(t *testing.T) {
	a := knowledge.Node{ID: "01AAAAAAAAAAAAAAAAAAAAAAAA", Created: testNow.Add(2 * time.Hour)}
	b := knowledge.Node{ID: "01BBBBBBBBBBBBBBBBBBBBBBBB", Created: testNow}
	c := knowledge.Node{ID: "01CCCCCCCCCCCCCCCCCCCCCCCC", Created: testNow}

	in := []knowledge.Node{a, c, b}
	got := sortChainOldestFirst(in)

	wantOrder := []string{b.ID, c.ID, a.ID}
	for i, w := range wantOrder {
		if got[i].ID != w {
			t.Fatalf("order[%d] = %s, want %s (full: %v)", i, got[i].ID, w, ids(got))
		}
	}
	// Input slice must be untouched.
	if in[0].ID != a.ID || in[1].ID != c.ID || in[2].ID != b.ID {
		t.Fatalf("input mutated: %v", ids(in))
	}
}

func ids(nodes []knowledge.Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.ID
	}
	return out
}

func TestAsStringSlice(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want []string
	}{
		{"any slice", []any{"a", "b"}, []string{"a", "b"}},
		{"string slice", []string{"x"}, []string{"x"}},
		{"nil", nil, []string{}},
		{"wrong type", 42, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := asStringSlice(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("asStringSlice(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestSingleInt(t *testing.T) {
	rec := &neo4j.Record{Keys: []string{"c"}, Values: []any{int64(7)}}
	tests := []struct {
		name    string
		records []*neo4j.Record
		key     string
		want    int
	}{
		{"present", []*neo4j.Record{rec}, "c", 7},
		{"missing key", []*neo4j.Record{rec}, "x", 0},
		{"no records", nil, "c", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := singleInt(tt.records, tt.key); got != tt.want {
				t.Fatalf("singleInt = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestWithKey(t *testing.T) {
	params := withKey(testKey, nil)
	if params["ws"] != "drakejin" || params["team"] != "infra" || params["proj"] != "memory-mcp" {
		t.Fatalf("withKey = %v", params)
	}
	merged := withKey(testKey, map[string]any{"q": "검색"})
	if merged["q"] != "검색" || merged["ws"] != "drakejin" {
		t.Fatalf("withKey merge = %v", merged)
	}
}

func TestRecordListHelpers(t *testing.T) {
	n := dbNode("e0", knowledge.Node{ID: newID(), Kind: knowledge.KindEntity, Name: "x"})
	p := dbtype.Path{Nodes: []dbtype.Node{n}}

	tests := []struct {
		name      string
		rec       *neo4j.Record
		key       string
		wantNodes int
		wantPaths int
	}{
		{"nodes and paths present", &neo4j.Record{
			Keys:   []string{"list"},
			Values: []any{[]any{n, p}},
		}, "list", 1, 1},
		{"missing key", &neo4j.Record{Keys: []string{"list"}, Values: []any{[]any{n}}}, "other", 0, 0},
		{"column is not a list", &neo4j.Record{Keys: []string{"list"}, Values: []any{"scalar"}}, "list", 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := len(recordNodes(tt.rec, tt.key)); got != tt.wantNodes {
				t.Errorf("recordNodes = %d, want %d", got, tt.wantNodes)
			}
			if got := len(recordPaths(tt.rec, tt.key)); got != tt.wantPaths {
				t.Errorf("recordPaths = %d, want %d", got, tt.wantPaths)
			}
		})
	}
}

func TestRecordNode(t *testing.T) {
	n := dbNode("e0", knowledge.Node{ID: newID(), Kind: knowledge.KindEntity, Name: "x"})
	tests := []struct {
		name string
		rec  *neo4j.Record
		key  string
		want bool
	}{
		{"present", &neo4j.Record{Keys: []string{"n"}, Values: []any{n}}, "n", true},
		{"missing key", &neo4j.Record{Keys: []string{"n"}, Values: []any{n}}, "x", false},
		{"wrong type", &neo4j.Record{Keys: []string{"n"}, Values: []any{"scalar"}}, "n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := recordNode(tt.rec, tt.key); ok != tt.want {
				t.Fatalf("recordNode ok = %v, want %v", ok, tt.want)
			}
		})
	}
}
