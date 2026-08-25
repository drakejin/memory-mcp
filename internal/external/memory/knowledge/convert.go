package knowledgemem

// This file converts between driver values and domain values: node/edge
// properties in both directions, record extraction, and the traversal →
// knowledge.Graph fold.

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/dbtype"

	"github.com/drakejin/memory-mcp/internal/service/knowledge"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// Depth bounds for Neighborhood traversal (§7 depth default 1) and a hop cap
// for supersede-chain walks so a cyclic import can never hang a query.
const (
	defaultDepth = 1
	maxDepth     = 10
	maxChainHops = 50
)

// clampDepth normalises a user-supplied depth into [1, maxDepth].
func clampDepth(depth int) int {
	if depth <= 0 {
		return defaultDepth
	}
	if depth > maxDepth {
		return maxDepth
	}
	return depth
}

// nodeProps flattens a knowledge.Node into Neo4j parameters. Times are stored
// as RFC3339Nano strings and aliases additionally as a joined aliases_text
// property, because Neo4j fulltext indexes cover string properties, not lists.
func nodeProps(key projectkey.Key, n knowledge.Node) map[string]any {
	return map[string]any{
		"id":            n.ID,
		"ws":            key.Workspace,
		"team":          key.Team,
		"proj":          key.Project,
		"kind":          string(n.Kind),
		"name":          n.Name,
		"body":          n.Body,
		"aliases":       toAnySlice(n.Aliases),
		"aliases_text":  strings.Join(n.Aliases, " "),
		"state":         string(n.State),
		"trust":         string(n.Trust),
		"supersedes":    toAnySlice(n.Supersedes),
		"superseded_by": n.SupersededBy,
		"provenance":    toAnySlice(n.Provenance),
		"created":       formatTime(n.Created),
		"updated":       formatTime(n.Updated),
		"review_after":  n.ReviewAfter,
	}
}

// edgeProps flattens a knowledge.Edge into Neo4j parameters.
func edgeProps(e knowledge.Edge) map[string]any {
	return map[string]any{
		"from":       e.From,
		"to":         e.To,
		"rel":        string(e.Rel),
		"provenance": toAnySlice(e.Provenance),
		"confidence": e.Confidence,
	}
}

// propsToNode rebuilds a knowledge.Node from stored Neo4j properties. It is
// the inverse of nodeProps; unknown or missing properties yield zero values.
func propsToNode(props map[string]any) knowledge.Node {
	return knowledge.Node{
		ID:           asString(props["id"]),
		Kind:         knowledge.NodeKind(asString(props["kind"])),
		Name:         asString(props["name"]),
		Body:         asString(props["body"]),
		Aliases:      asStringSlice(props["aliases"]),
		State:        knowledge.State(asString(props["state"])),
		Trust:        knowledge.Trust(asString(props["trust"])),
		Supersedes:   asStringSlice(props["supersedes"]),
		SupersededBy: asString(props["superseded_by"]),
		Provenance:   asStringSlice(props["provenance"]),
		Created:      parseTime(asString(props["created"])),
		Updated:      parseTime(asString(props["updated"])),
		ReviewAfter:  asString(props["review_after"]),
	}
}

// graphFromPaths builds a deduplicated knowledge.Graph from the center node
// plus every traversal path. Node identity is the ULID id; edge identity is
// (from, to, rel). Relationship endpoints arrive as element ids, so a mapping
// from element id to knowledge id is built from all path nodes first.
func graphFromPaths(center dbtype.Node, paths []dbtype.Path) knowledge.Graph {
	elementToID := map[string]string{center.GetElementId(): asString(center.Props["id"])}
	nodeByID := map[string]knowledge.Node{}
	order := []string{}

	addNode := func(n dbtype.Node) {
		kn := propsToNode(n.Props)
		elementToID[n.GetElementId()] = kn.ID
		if _, ok := nodeByID[kn.ID]; !ok {
			nodeByID[kn.ID] = kn
			order = append(order, kn.ID)
		}
	}
	addNode(center)
	for _, p := range paths {
		for _, n := range p.Nodes {
			addNode(n)
		}
	}

	edgeSeen := map[string]bool{}
	edges := []knowledge.Edge{}
	for _, p := range paths {
		for _, r := range p.Relationships {
			e := knowledge.Edge{
				From:       elementToID[r.StartElementId],
				To:         elementToID[r.EndElementId],
				Rel:        knowledge.Rel(asString(r.Props["rel"])),
				Provenance: asStringSlice(r.Props["provenance"]),
				Confidence: asFloat(r.Props["confidence"]),
			}
			sig := e.From + "\x00" + e.To + "\x00" + string(e.Rel)
			if edgeSeen[sig] {
				continue
			}
			edgeSeen[sig] = true
			edges = append(edges, e)
		}
	}

	nodes := make([]knowledge.Node, 0, len(order))
	for _, id := range order {
		nodes = append(nodes, nodeByID[id])
	}
	return knowledge.Graph{Nodes: nodes, Edges: edges}
}

// sortChainOldestFirst returns a new slice ordered by created time ascending,
// with the ULID id as a deterministic tie-break (blackbox scenario 3 expects
// oldest first).
func sortChainOldestFirst(nodes []knowledge.Node) []knowledge.Node {
	out := append([]knowledge.Node(nil), nodes...)
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.Before(out[j].Created)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// withKey merges the project-scope parameters into params and returns it.
func withKey(key projectkey.Key, params map[string]any) map[string]any {
	if params == nil {
		params = map[string]any{}
	}
	params["ws"] = key.Workspace
	params["team"] = key.Team
	params["proj"] = key.Project
	return params
}

// recordNode extracts a dbtype.Node value from a record by key.
func recordNode(rec *neo4j.Record, key string) (dbtype.Node, bool) {
	v, ok := rec.Get(key)
	if !ok {
		return dbtype.Node{}, false
	}
	n, ok := v.(dbtype.Node)
	return n, ok
}

// recordNodes extracts a list of dbtype.Node values from a record by key.
func recordNodes(rec *neo4j.Record, key string) []dbtype.Node {
	list, ok := recordList(rec, key)
	if !ok {
		return nil
	}
	out := make([]dbtype.Node, 0, len(list))
	for _, item := range list {
		if n, ok := item.(dbtype.Node); ok {
			out = append(out, n)
		}
	}
	return out
}

// recordPaths extracts a list of dbtype.Path values from a record by key.
func recordPaths(rec *neo4j.Record, key string) []dbtype.Path {
	list, ok := recordList(rec, key)
	if !ok {
		return nil
	}
	out := make([]dbtype.Path, 0, len(list))
	for _, item := range list {
		if p, ok := item.(dbtype.Path); ok {
			out = append(out, p)
		}
	}
	return out
}

// recordList extracts an untyped list column from a record by key.
func recordList(rec *neo4j.Record, key string) ([]any, bool) {
	v, ok := rec.Get(key)
	if !ok {
		return nil, false
	}
	list, ok := v.([]any)
	return list, ok
}

// singleInt reads an integer column from the first record, defaulting to 0.
func singleInt(records []*neo4j.Record, key string) int {
	if len(records) == 0 {
		return 0
	}
	v, ok := records[0].Get(key)
	if !ok {
		return 0
	}
	n, ok := v.(int64)
	if !ok {
		return 0
	}
	return int(n)
}

func toAnySlice(in []string) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asFloat(v any) float64 {
	switch f := v.(type) {
	case float64:
		return f
	case int64:
		return float64(f)
	default:
		return 0
	}
}

func asStringSlice(v any) []string {
	switch list := v.(type) {
	case []string:
		return append([]string(nil), list...)
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			out = append(out, fmt.Sprint(item))
		}
		return out
	default:
		return []string{}
	}
}
