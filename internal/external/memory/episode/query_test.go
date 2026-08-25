package episodemem

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/x/errs"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

func TestSearchRequestBody(t *testing.T) {
	tests := []struct {
		name         string
		query        Query
		wantMatch    bool // false => match_all
		wantRange    bool
		wantKinds    bool
		wantSize     float64
		wantFilterLn int
	}{
		{
			name: "full query",
			query: Query{
				Text:  "보안을 끄는",
				From:  time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
				To:    time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC),
				Kinds: []episode.Kind{episode.KindEvent, episode.KindDecision},
				Size:  5,
			},
			wantMatch: true, wantRange: true, wantKinds: true, wantSize: 5, wantFilterLn: 5,
		},
		{
			name:      "empty text matches all with default size",
			query:     Query{},
			wantMatch: false, wantSize: float64(DefaultSearchSize), wantFilterLn: 3,
		},
		{
			name:      "whitespace-only text matches all",
			query:     Query{Text: "   "},
			wantMatch: false, wantSize: float64(DefaultSearchSize), wantFilterLn: 3,
		},
		{
			name:      "negative size falls back to the default",
			query:     Query{Text: "보안", Size: -3},
			wantMatch: true, wantSize: float64(DefaultSearchSize), wantFilterLn: 3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			ft := &fakeTransport{handler: func(fakeCall) (int, string) {
				return http.StatusOK, `{"hits":{"hits":[]}}`
			}}
			c := newTestClient(t, ft)

			// Act
			if _, err := c.Search(context.Background(), testKey(), tt.query); err != nil {
				t.Fatalf("Search() = %v", err)
			}

			// Assert
			calls := ft.recorded()
			if len(calls) != 1 || calls[0].Path != "/"+IndexName+searchSuffix {
				t.Fatalf("expected one POST /%s%s, got %+v", IndexName, searchSuffix, calls)
			}
			var body map[string]any
			if err := json.Unmarshal(calls[0].Body, &body); err != nil {
				t.Fatalf("search body: %v", err)
			}
			boolQ := body["query"].(map[string]any)["bool"].(map[string]any)
			must := boolQ["must"].([]any)[0].(map[string]any)
			if tt.wantMatch {
				match := must["match"].(map[string]any)["text"].(map[string]any)
				if match["query"] != tt.query.Text {
					t.Errorf("match query = %v, want %q", match["query"], tt.query.Text)
				}
			} else if _, ok := must["match_all"]; !ok {
				t.Errorf("want match_all for empty text, got %v", must)
			}
			filters := boolQ["filter"].([]any)
			if len(filters) != tt.wantFilterLn {
				t.Errorf("got %d filters, want %d: %v", len(filters), tt.wantFilterLn, filters)
			}
			raw := string(calls[0].Body)
			for _, term := range []string{`"workspace":"vms"`, `"team":"core"`, `"project":"memory-mcp"`} {
				if !strings.Contains(raw, term) {
					t.Errorf("search body missing project filter %s", term)
				}
			}
			if tt.wantRange != strings.Contains(raw, `"occurred_at":{"gte"`) {
				t.Errorf("range filter presence = %v, want %v", !tt.wantRange, tt.wantRange)
			}
			if tt.wantKinds != strings.Contains(raw, `"terms":{"kind"`) {
				t.Errorf("kinds filter presence = %v, want %v", !tt.wantKinds, tt.wantKinds)
			}
			if got := body["size"].(float64); got != tt.wantSize {
				t.Errorf("size = %v, want %v", got, tt.wantSize)
			}
			if _, ok := body["highlight"]; !ok {
				t.Error("search body missing highlight block")
			}
			sorts := body["sort"].([]any)
			if len(sorts) < 2 {
				t.Fatalf("sort = %v, want _score then occurred_at (newest first on ties)", sorts)
			}
		})
	}
}

// TestSearchTimeBoundsAreUTC pins §3 at-rest/at-query time handling: bounds go
// out as RFC3339 UTC no matter which zone the caller passed.
func TestSearchTimeBoundsAreUTC(t *testing.T) {
	// Arrange
	seoul := time.FixedZone("KST", 9*60*60)
	ft := &fakeTransport{handler: func(fakeCall) (int, string) {
		return http.StatusOK, `{"hits":{"hits":[]}}`
	}}
	c := newTestClient(t, ft)

	// Act
	_, err := c.Search(context.Background(), testKey(), Query{
		To: time.Date(2026, 8, 25, 11, 0, 0, 0, seoul),
	})

	// Assert
	if err != nil {
		t.Fatalf("Search() = %v", err)
	}
	raw := string(ft.recorded()[0].Body)
	if !strings.Contains(raw, `"lte":"2026-08-25T02:00:00Z"`) {
		t.Errorf("bounds not normalized to UTC RFC3339: %s", raw)
	}
}

// TestSearchWithoutProjectKey covers the all-projects query shape: a zero key
// drops the scope filters instead of matching an empty workspace.
func TestSearchWithoutProjectKey(t *testing.T) {
	// Arrange
	ft := &fakeTransport{handler: func(fakeCall) (int, string) {
		return http.StatusOK, `{"hits":{"hits":[]}}`
	}}
	c := newTestClient(t, ft)

	// Act
	if _, err := c.Search(context.Background(), projectkey.Key{}, Query{Text: "보안"}); err != nil {
		t.Fatalf("Search() = %v", err)
	}

	// Assert
	var body map[string]any
	if err := json.Unmarshal(ft.recorded()[0].Body, &body); err != nil {
		t.Fatalf("search body: %v", err)
	}
	boolQ := body["query"].(map[string]any)["bool"].(map[string]any)
	if _, ok := boolQ["filter"]; ok {
		t.Errorf("zero key must not produce scope filters: %v", boolQ)
	}
}

func TestSearchHitsAreExcerptOnly(t *testing.T) {
	// Arrange
	resp := `{"hits":{"hits":[
		{"_score":2.5,
		 "_source":{"id":"01JD0000000000000000000001","kind":"event","occurred_at":"2026-08-25T02:00:00Z","actor":"agent","text":"보안을 끄고 배포했다","entities":["memory-mcp"],"consolidated":false,"recall_count":1,"last_recalled":"","workspace":"vms","team":"core","project":"memory-mcp"},
		 "highlight":{"text":["<em>보안</em>을 <em>끄</em>고 배포했다"]}},
		{"_score":1.0,
		 "_source":{"id":"01JD0000000000000000000002","kind":"decision","occurred_at":"2026-08-24T02:00:00Z","actor":"user","text":"하이라이트 없는 결과 본문","entities":[],"consolidated":true,"recall_count":0,"last_recalled":"","workspace":"vms","team":"core","project":"memory-mcp"}}
	]}}`
	ft := &fakeTransport{handler: func(fakeCall) (int, string) { return http.StatusOK, resp }}
	c := newTestClient(t, ft)

	// Act
	hits, err := c.Search(context.Background(), testKey(), Query{Text: "보안"})

	// Assert
	if err != nil {
		t.Fatalf("Search() = %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}
	first := hits[0]
	if first.Score != 2.5 {
		t.Errorf("score = %v, want 2.5", first.Score)
	}
	if first.Excerpt != "<em>보안</em>을 <em>끄</em>고 배포했다" {
		t.Errorf("excerpt = %q", first.Excerpt)
	}
	if first.Record.Text != "" {
		t.Errorf("Record.Text = %q, want empty (excerpt-only responses, §7)", first.Record.Text)
	}
	if first.Record.ID != "01JD0000000000000000000001" || first.Record.Kind != episode.KindEvent {
		t.Errorf("record metadata lost: %+v", first.Record)
	}
	second := hits[1]
	if second.Excerpt != "하이라이트 없는 결과 본문" {
		t.Errorf("fallback excerpt = %q, want text head", second.Excerpt)
	}
	if second.Record.Text != "" {
		t.Errorf("second Record.Text = %q, want empty", second.Record.Text)
	}
	if !second.Record.Consolidated {
		t.Error("consolidated flag lost in hit metadata")
	}
}

// TestSearchJoinsHighlightFragments pins the multi-fragment excerpt shape: the
// caller gets one string with an ellipsis between fragments, never the body.
func TestSearchJoinsHighlightFragments(t *testing.T) {
	// Arrange
	resp := `{"hits":{"hits":[
		{"_source":{"id":"01JD0000000000000000000001","kind":"event","occurred_at":"2026-08-25T02:00:00Z","actor":"agent","text":"본문","entities":[],"consolidated":false,"recall_count":0,"last_recalled":"","workspace":"vms","team":"core","project":"memory-mcp"},
		 "highlight":{"text":["앞 조각","뒤 조각"]}}
	]}}`
	ft := &fakeTransport{handler: func(fakeCall) (int, string) { return http.StatusOK, resp }}
	c := newTestClient(t, ft)

	// Act
	hits, err := c.Search(context.Background(), testKey(), Query{Text: "조각"})

	// Assert
	if err != nil {
		t.Fatalf("Search() = %v", err)
	}
	if want := "앞 조각" + excerptJoiner + "뒤 조각"; hits[0].Excerpt != want {
		t.Errorf("excerpt = %q, want %q", hits[0].Excerpt, want)
	}
	// A hit with no _score must still be a valid hit, scored zero.
	if hits[0].Score != 0 {
		t.Errorf("score = %v, want 0 for a missing _score", hits[0].Score)
	}
}

// TestSearchMissingIndexIsUnavailable pins the honesty rule of §5: a query
// against an index that does not exist must NOT come back as "0 hits". The
// caller cannot tell that apart from "nothing matched", so a lost container
// would read as an empty memory — the repo's own acceptance spec calls a
// 200 + empty array here a false success and worse than a 503.
func TestSearchMissingIndexIsUnavailable(t *testing.T) {
	// Arrange
	ft := &fakeTransport{handler: func(fakeCall) (int, string) {
		return http.StatusNotFound, `{"error":{"type":"index_not_found_exception"}}`
	}}
	c := newTestClient(t, ft)

	// Act
	hits, err := c.Search(context.Background(), testKey(), Query{Text: "x"})

	// Assert
	assertKind(t, err, errs.KindUnavailable, opSearch)
	if hits != nil {
		t.Fatalf("hits = %v, want nil alongside the error", hits)
	}
}

func TestSearchErrors(t *testing.T) {
	tests := []struct {
		name         string
		handler      func(fakeCall) (int, string)
		transportErr error
		wantKind     errs.Kind
	}{
		{
			// Reads surface unavailable so the server can answer 503 (§5).
			name:         "container down",
			transportErr: errors.New("dial tcp: connection refused"),
			wantKind:     errs.KindUnavailable,
		},
		{
			name: "cluster 500",
			handler: func(fakeCall) (int, string) {
				return http.StatusInternalServerError, `{"error":"boom"}`
			},
			wantKind: errs.KindUnavailable,
		},
		{
			name: "malformed query rejected",
			handler: func(fakeCall) (int, string) {
				return http.StatusBadRequest, `{"error":{"type":"parsing_exception"}}`
			},
			wantKind: errs.KindInternal,
		},
		{
			name: "response is not json",
			handler: func(fakeCall) (int, string) {
				return http.StatusOK, `not json`
			},
			wantKind: errs.KindInternal,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			ft := &fakeTransport{handler: tt.handler, err: tt.transportErr}
			c := newTestClient(t, ft)

			// Act
			hits, err := c.Search(context.Background(), testKey(), Query{Text: "x"})

			// Assert
			if hits != nil {
				t.Errorf("hits = %v, want nil on error", hits)
			}
			assertKind(t, err, tt.wantKind, opSearch)
		})
	}
}

func TestDocCount(t *testing.T) {
	tests := []struct {
		name       string
		key        projectkey.Key
		status     int
		resp       string
		want       int
		wantFilter bool
	}{
		{
			name: "project scoped", key: testKey(),
			status: http.StatusOK, resp: `{"count":7}`, want: 7, wantFilter: true,
		},
		{
			name: "zero key counts all projects", key: projectkey.Key{},
			status: http.StatusOK, resp: `{"count":42}`, want: 42,
		},
		{
			name: "missing index counts zero", key: testKey(),
			status: http.StatusNotFound, resp: `{"error":{"type":"index_not_found_exception"}}`, want: 0, wantFilter: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			ft := &fakeTransport{handler: func(fakeCall) (int, string) { return tt.status, tt.resp }}
			c := newTestClient(t, ft)

			// Act
			got, err := c.DocCount(context.Background(), tt.key)

			// Assert
			if err != nil {
				t.Fatalf("DocCount() = %v", err)
			}
			if got != tt.want {
				t.Fatalf("DocCount() = %d, want %d", got, tt.want)
			}
			calls := ft.recorded()
			if len(calls) != 1 || calls[0].Path != "/"+IndexName+countSuffix {
				t.Fatalf("expected one POST /%s%s, got %+v", IndexName, countSuffix, calls)
			}
			hasFilter := strings.Contains(string(calls[0].Body), `"workspace":"vms"`)
			if hasFilter != tt.wantFilter {
				t.Errorf("project filter presence = %v, want %v: %s", hasFilter, tt.wantFilter, calls[0].Body)
			}
		})
	}
}

func TestDocCountErrors(t *testing.T) {
	tests := []struct {
		name         string
		handler      func(fakeCall) (int, string)
		transportErr error
		wantKind     errs.Kind
	}{
		{
			name:         "container down",
			transportErr: errors.New("dial tcp: connection refused"),
			wantKind:     errs.KindUnavailable,
		},
		{
			name: "unexpected status",
			handler: func(fakeCall) (int, string) {
				return http.StatusForbidden, `{"error":"forbidden"}`
			},
			wantKind: errs.KindInternal,
		},
		{
			name: "response is not json",
			handler: func(fakeCall) (int, string) {
				return http.StatusOK, `not json`
			},
			wantKind: errs.KindInternal,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			ft := &fakeTransport{handler: tt.handler, err: tt.transportErr}
			c := newTestClient(t, ft)

			// Act
			got, err := c.DocCount(context.Background(), testKey())

			// Assert
			if got != 0 {
				t.Errorf("DocCount() = %d, want 0 on error", got)
			}
			assertKind(t, err, tt.wantKind, opDocCount)
		})
	}
}

func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		limit int
		want  string
	}{
		{name: "short passes through", in: "짧다", limit: 10, want: "짧다"},
		{name: "exact limit passes through", in: "abcde", limit: 5, want: "abcde"},
		{name: "long korean is cut on rune boundary", in: "가나다라마바사", limit: 3, want: "가나다…"},
		{name: "empty stays empty", in: "", limit: 3, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := truncateRunes(tt.in, tt.limit); got != tt.want {
				t.Fatalf("truncateRunes(%q, %d) = %q, want %q", tt.in, tt.limit, got, tt.want)
			}
		})
	}
}
