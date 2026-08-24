package search

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opensearch-project/opensearch-go/v4"

	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/hotstore"
)

// fakeCall records one HTTP request the client sent.
type fakeCall struct {
	Method string
	Path   string
	Query  url.Values
	Body   []byte
}

// fakeTransport implements http.RoundTripper. Each call is recorded and
// answered by the handler; err (when set) simulates a dead container.
type fakeTransport struct {
	mu      sync.Mutex
	calls   []fakeCall
	handler func(call fakeCall) (status int, body string)
	err     error
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body.Close()
	}
	call := fakeCall{Method: req.Method, Path: req.URL.Path, Query: req.URL.Query(), Body: body}

	f.mu.Lock()
	f.calls = append(f.calls, call)
	err := f.err
	handler := f.handler
	f.mu.Unlock()

	if err != nil {
		return nil, err
	}
	status, respBody := http.StatusOK, "{}"
	if handler != nil {
		status, respBody = handler(call)
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(respBody)),
		Request:    req,
	}, nil
}

func (f *fakeTransport) recorded() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// newTestClient wires a Client onto the fake transport. Health checks and
// retries are disabled so every recorded call comes from the code under test.
func newTestClient(t *testing.T, ft *fakeTransport) *Client {
	t.Helper()
	osc, err := opensearch.NewClient(opensearch.Config{
		Addresses:             []string{"http://127.0.0.1:9200"},
		Transport:             ft,
		DisableRetry:          true,
		HealthCheckMaxRetries: -1,
	})
	if err != nil {
		t.Fatalf("opensearch.NewClient: %v", err)
	}
	t.Cleanup(func() { osc.Close() })
	return &Client{os: osc, index: IndexName}
}

// newReadyTestClient is newTestClient with the mapping already converged, so
// write-path tests record only the calls they are asserting on. Tests that
// exercise the lazy ensure itself use newTestClient.
func newReadyTestClient(t *testing.T, ft *fakeTransport) *Client {
	t.Helper()
	c := newTestClient(t, ft)
	c.schemaReady = true
	return c
}

func testKey() hotstore.ProjectKey {
	return hotstore.ProjectKey{Workspace: "vms", Team: "core", Project: "memory-mcp"}
}

func testRecord(id, text string) episodic.Record {
	return episodic.Record{
		ID:         id,
		Kind:       episodic.KindEvent,
		OccurredAt: time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC),
		Actor:      episodic.ActorAgent,
		Text:       text,
		Entities:   []string{"memory-mcp"},
	}
}

// decodeNDJSON splits a bulk payload into parsed JSON lines.
func decodeNDJSON(t *testing.T, payload []byte) []map[string]any {
	t.Helper()
	var lines []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(payload))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var line map[string]any
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("bulk line %q: %v", sc.Text(), err)
		}
		lines = append(lines, line)
	}
	return lines
}

func TestPing(t *testing.T) {
	tests := []struct {
		name            string
		status          int
		transportErr    error
		wantUnavailable bool
	}{
		{name: "green", status: http.StatusOK},
		{name: "server error", status: http.StatusInternalServerError, wantUnavailable: true},
		{name: "connection refused", transportErr: errors.New("dial tcp: connection refused"), wantUnavailable: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			ft := &fakeTransport{
				err:     tt.transportErr,
				handler: func(fakeCall) (int, string) { return tt.status, `{"cluster_name":"x"}` },
			}
			c := newTestClient(t, ft)

			// Act
			err := c.Ping(context.Background())

			// Assert
			if tt.wantUnavailable {
				if !errors.Is(err, ErrUnavailable) {
					t.Fatalf("Ping() = %v, want ErrUnavailable", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Ping() = %v, want nil", err)
			}
		})
	}
}

func TestIndexRecordsBulkPayload(t *testing.T) {
	// Arrange
	ft := &fakeTransport{handler: func(fakeCall) (int, string) {
		return http.StatusOK, `{"took":1,"errors":false,"items":[]}`
	}}
	c := newReadyTestClient(t, ft)
	recs := []episodic.Record{
		testRecord("01JD0000000000000000000001", "보안을 끄고 배포했다"),
		testRecord("01JD0000000000000000000002", "재수화 완료"),
	}

	// Act
	if err := c.IndexRecords(context.Background(), testKey(), recs); err != nil {
		t.Fatalf("IndexRecords() = %v", err)
	}

	// Assert
	calls := ft.recorded()
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1 bulk call", len(calls))
	}
	call := calls[0]
	if call.Method != http.MethodPost || call.Path != "/_bulk" {
		t.Fatalf("call = %s %s, want POST /_bulk", call.Method, call.Path)
	}
	if call.Query.Get("refresh") != "true" {
		t.Errorf("bulk missing refresh=true (realtime upsert): %v", call.Query)
	}
	lines := decodeNDJSON(t, call.Body)
	if len(lines) != 4 {
		t.Fatalf("got %d ndjson lines, want 4 (2 actions + 2 docs)", len(lines))
	}
	action, ok := lines[0]["index"].(map[string]any)
	if !ok {
		t.Fatalf("first line is not an index action: %v", lines[0])
	}
	wantID := "vms/core/memory-mcp#01JD0000000000000000000001"
	if action["_index"] != IndexName || action["_id"] != wantID {
		t.Errorf("action = %v, want _index=%s _id=%s", action, IndexName, wantID)
	}
	doc := lines[1]
	if doc["workspace"] != "vms" || doc["team"] != "core" || doc["project"] != "memory-mcp" {
		t.Errorf("doc missing project scope fields: %v", doc)
	}
	if doc["text"] != "보안을 끄고 배포했다" || doc["id"] != "01JD0000000000000000000001" {
		t.Errorf("doc missing record fields: %v", doc)
	}
}

func TestIndexRecordsEmptyIsNoop(t *testing.T) {
	// Arrange
	ft := &fakeTransport{}
	c := newTestClient(t, ft)

	// Act / Assert
	if err := c.IndexRecords(context.Background(), testKey(), nil); err != nil {
		t.Fatalf("IndexRecords(nil) = %v", err)
	}
	if calls := ft.recorded(); len(calls) != 0 {
		t.Fatalf("expected no HTTP calls, got %+v", calls)
	}
}

func TestIndexRecordsBatchesLargeSets(t *testing.T) {
	// Arrange
	ft := &fakeTransport{handler: func(fakeCall) (int, string) {
		return http.StatusOK, `{"errors":false,"items":[]}`
	}}
	c := newReadyTestClient(t, ft)
	recs := make([]episodic.Record, bulkBatchSize+1)
	for i := range recs {
		recs[i] = testRecord(fmt.Sprintf("01JD%022d", i), "채움")
	}

	// Act
	if err := c.IndexRecords(context.Background(), testKey(), recs); err != nil {
		t.Fatalf("IndexRecords() = %v", err)
	}

	// Assert
	if calls := ft.recorded(); len(calls) != 2 {
		t.Fatalf("got %d bulk calls, want 2", len(calls))
	}
}

func TestIndexRecordsErrors(t *testing.T) {
	tests := []struct {
		name            string
		handler         func(fakeCall) (int, string)
		transportErr    error
		wantUnavailable bool
	}{
		{
			name:         "container down",
			transportErr: errors.New("dial tcp: connection refused"), wantUnavailable: true,
		},
		{
			name: "opensearch 503",
			handler: func(fakeCall) (int, string) {
				return http.StatusServiceUnavailable, `{"error":"unavailable"}`
			},
			wantUnavailable: true,
		},
		{
			name: "item mapping failure",
			handler: func(fakeCall) (int, string) {
				return http.StatusOK, `{"errors":true,"items":[{"index":{"status":400,"error":{"type":"mapper_parsing_exception"}}}]}`
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			ft := &fakeTransport{handler: tt.handler, err: tt.transportErr}
			c := newReadyTestClient(t, ft)

			// Act
			err := c.IndexRecords(context.Background(), testKey(), []episodic.Record{testRecord("01JD0000000000000000000001", "x")})

			// Assert
			if err == nil {
				t.Fatal("IndexRecords() = nil, want error")
			}
			if got := errors.Is(err, ErrUnavailable); got != tt.wantUnavailable {
				t.Fatalf("errors.Is(err, ErrUnavailable) = %v, want %v (err=%v)", got, tt.wantUnavailable, err)
			}
		})
	}
}

func TestDeleteRecords(t *testing.T) {
	// Arrange
	ft := &fakeTransport{handler: func(fakeCall) (int, string) {
		return http.StatusOK, `{"errors":true,"items":[{"delete":{"status":404,"error":{"type":"not_found"}}},{"delete":{"status":200}}]}`
	}}
	c := newReadyTestClient(t, ft)

	// Act — per-item 404 must be tolerated (idempotent aging retries).
	err := c.DeleteRecords(context.Background(), testKey(), []string{"01JD0000000000000000000001", "01JD0000000000000000000002"})

	// Assert
	if err != nil {
		t.Fatalf("DeleteRecords() = %v, want nil", err)
	}
	calls := ft.recorded()
	if len(calls) != 1 || calls[0].Path != "/_bulk" {
		t.Fatalf("expected one POST /_bulk, got %+v", calls)
	}
	lines := decodeNDJSON(t, calls[0].Body)
	if len(lines) != 2 {
		t.Fatalf("got %d delete actions, want 2", len(lines))
	}
	del, ok := lines[0]["delete"].(map[string]any)
	if !ok {
		t.Fatalf("first line is not a delete action: %v", lines[0])
	}
	if del["_id"] != "vms/core/memory-mcp#01JD0000000000000000000001" {
		t.Errorf("delete _id = %v", del["_id"])
	}
}

func TestDeleteRecordsRealFailure(t *testing.T) {
	// Arrange
	ft := &fakeTransport{handler: func(fakeCall) (int, string) {
		return http.StatusOK, `{"errors":true,"items":[{"delete":{"status":400,"error":{"type":"validation_exception"}}}]}`
	}}
	c := newReadyTestClient(t, ft)

	// Act / Assert
	if err := c.DeleteRecords(context.Background(), testKey(), []string{"01JD0000000000000000000001"}); err == nil {
		t.Fatal("DeleteRecords() = nil, want error on non-404 item failure")
	}
}

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
				Kinds: []episodic.Kind{episodic.KindEvent, episodic.KindDecision},
				Size:  5,
			},
			wantMatch: true, wantRange: true, wantKinds: true, wantSize: 5, wantFilterLn: 5,
		},
		{
			name:      "empty text matches all with default size",
			query:     Query{},
			wantMatch: false, wantSize: float64(DefaultSearchSize), wantFilterLn: 3,
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
			if len(calls) != 1 || calls[0].Path != "/"+IndexName+"/_search" {
				t.Fatalf("expected one POST /%s/_search, got %+v", IndexName, calls)
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
	if first.Record.ID != "01JD0000000000000000000001" || first.Record.Kind != episodic.KindEvent {
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

func TestSearchMissingIndexReturnsEmpty(t *testing.T) {
	// Arrange
	ft := &fakeTransport{handler: func(fakeCall) (int, string) {
		return http.StatusNotFound, `{"error":{"type":"index_not_found_exception"}}`
	}}
	c := newTestClient(t, ft)

	// Act
	hits, err := c.Search(context.Background(), testKey(), Query{Text: "x"})

	// Assert
	if err != nil {
		t.Fatalf("Search() = %v, want nil on missing index", err)
	}
	if len(hits) != 0 {
		t.Fatalf("got %d hits, want 0", len(hits))
	}
}

func TestSearchUnavailable(t *testing.T) {
	// Arrange
	ft := &fakeTransport{err: errors.New("dial tcp: connection refused")}
	c := newTestClient(t, ft)

	// Act / Assert — reads surface ErrUnavailable so the server can 503 (§5).
	if _, err := c.Search(context.Background(), testKey(), Query{Text: "x"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Search() = %v, want ErrUnavailable", err)
	}
}

func TestDocCount(t *testing.T) {
	tests := []struct {
		name       string
		key        hotstore.ProjectKey
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
			name: "zero key counts all projects", key: hotstore.ProjectKey{},
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
			if len(calls) != 1 || calls[0].Path != "/"+IndexName+"/_count" {
				t.Fatalf("expected one POST /%s/_count, got %+v", IndexName, calls)
			}
			hasFilter := strings.Contains(string(calls[0].Body), `"workspace":"vms"`)
			if hasFilter != tt.wantFilter {
				t.Errorf("project filter presence = %v, want %v: %s", hasFilter, tt.wantFilter, calls[0].Body)
			}
		})
	}
}

func TestDrop(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "deleted", status: http.StatusOK},
		{name: "already gone", status: http.StatusNotFound},
		{name: "forbidden", status: http.StatusForbidden, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			ft := &fakeTransport{handler: func(fakeCall) (int, string) { return tt.status, `{}` }}
			c := newTestClient(t, ft)

			// Act
			err := c.Drop(context.Background())

			// Assert
			if tt.wantErr != (err != nil) {
				t.Fatalf("Drop() = %v, wantErr %v", err, tt.wantErr)
			}
			calls := ft.recorded()
			if len(calls) != 1 || calls[0].Method != http.MethodDelete || calls[0].Path != "/"+IndexName {
				t.Fatalf("expected one DELETE /%s, got %+v", IndexName, calls)
			}
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := truncateRunes(tt.in, tt.limit); got != tt.want {
				t.Fatalf("truncateRunes(%q, %d) = %q, want %q", tt.in, tt.limit, got, tt.want)
			}
		})
	}
}
