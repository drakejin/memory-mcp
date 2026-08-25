package search

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opensearch-project/opensearch-go/v4"

	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/errs"
	"github.com/drakejin/memory-mcp/internal/hotstore"
)

// ---------------------------------------------------------------- test doubles

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

// stubDoer implements the package's narrow doer interface directly, so tests
// can produce answers no HTTP round trip ever could (a nil response with a nil
// error, a body that fails mid-read).
type stubDoer struct {
	resp *opensearch.Response
	err  error
}

func (s stubDoer) Do(context.Context, string, opensearch.Request, any) (*opensearch.Response, error) {
	return s.resp, s.err
}

// errReader fails on the first Read, simulating a connection dropped while the
// body was streaming.
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }
func (e errReader) Close() error             { return nil }

// newTestClient wires a client onto the fake transport. Health checks and
// retries are disabled so every recorded call comes from the code under test.
func newTestClient(t *testing.T, ft *fakeTransport) *client {
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
	return &client{os: osc, index: IndexName, log: slog.New(slog.DiscardHandler)}
}

// newReadyTestClient is newTestClient with the mapping already converged, so
// write-path tests record only the calls they are asserting on. Tests that
// exercise the lazy ensure itself use newTestClient.
func newReadyTestClient(t *testing.T, ft *fakeTransport) *client {
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

// assertKind fails unless err carries exactly the wanted semantic kind, op and
// entity — the envelope the transport layer maps to a status.
func assertKind(t *testing.T, err error, wantKind errs.Kind, wantOp string) {
	t.Helper()
	var domain *errs.Error
	if !errors.As(err, &domain) {
		t.Fatalf("err = %v (%T), want *errs.Error", err, err)
	}
	if domain.Kind != wantKind {
		t.Fatalf("Kind = %q, want %q (err=%v)", domain.Kind, wantKind, err)
	}
	if domain.Op != wantOp {
		t.Errorf("Op = %q, want %q", domain.Op, wantOp)
	}
}

// ---------------------------------------------------------------- constructor

func TestNew(t *testing.T) {
	tests := []struct {
		name     string
		cfg      Config
		wantKind errs.Kind
		wantOK   bool
	}{
		{name: "valid url", cfg: Config{URL: "http://127.0.0.1:9200"}, wantOK: true},
		{name: "logger injected", cfg: Config{URL: "http://127.0.0.1:9200", Logger: slog.New(slog.DiscardHandler)}, wantOK: true},
		{name: "empty url", cfg: Config{}, wantKind: errs.KindInvalid},
		{name: "blank url", cfg: Config{URL: "   "}, wantKind: errs.KindInvalid},
		{name: "unparseable url", cfg: Config{URL: "http://[::1]:namedport"}, wantKind: errs.KindInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Act
			c, err := New(tt.cfg)

			// Assert
			if tt.wantOK {
				if err != nil {
					t.Fatalf("New() = %v, want nil", err)
				}
				if c == nil {
					t.Fatal("New() returned a nil Client")
				}
				return
			}
			if c != nil {
				t.Fatalf("New() = %v, want nil Client on error", c)
			}
			assertKind(t, err, tt.wantKind, opNew)
		})
	}
}

// TestNewDoesNotDial pins code-standards §1.4: the constructor performs no I/O,
// so an address nobody is listening on still builds a Client.
func TestNewDoesNotDial(t *testing.T) {
	// Arrange / Act
	c, err := New(Config{URL: "http://127.0.0.1:1"})

	// Assert
	if err != nil {
		t.Fatalf("New() = %v, want nil (constructors do not dial)", err)
	}
	if c == nil {
		t.Fatal("New() returned a nil Client")
	}
}

// TestNewUsesProductionIndex keeps the exported constructor pinned to the one
// shared episodic index; only in-package tests may target a scratch index.
func TestNewUsesProductionIndex(t *testing.T) {
	// Arrange / Act
	c, err := newClient(Config{URL: "http://127.0.0.1:9200"}, IndexName)

	// Assert
	if err != nil {
		t.Fatalf("newClient() = %v", err)
	}
	if c.index != IndexName {
		t.Errorf("index = %q, want %q", c.index, IndexName)
	}
	if c.log == nil {
		t.Error("client logger is nil; a nil Config.Logger must fall back to slog.Default()")
	}
}

// ---------------------------------------------------------------- ping / transport

func TestPing(t *testing.T) {
	tests := []struct {
		name            string
		status          int
		transportErr    error
		wantUnavailable bool
	}{
		{name: "green", status: http.StatusOK},
		{name: "server error", status: http.StatusInternalServerError, wantUnavailable: true},
		{name: "yellow-but-not-ok status", status: http.StatusForbidden, wantUnavailable: true},
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
			if !tt.wantUnavailable {
				if err != nil {
					t.Fatalf("Ping() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, errs.ErrUnavailable) {
				t.Fatalf("Ping() = %v, want errs.ErrUnavailable", err)
			}
			assertKind(t, err, errs.KindUnavailable, opPing)
		})
	}
}

// TestUnavailableErrorsCarryContext pins the structured detail degraded-mode
// diagnosis relies on: the failed method and path travel in Fields (log-only),
// never in the client-facing message.
func TestUnavailableErrorsCarryContext(t *testing.T) {
	// Arrange
	boom := errors.New("dial tcp 127.0.0.1:9200: connect: connection refused")
	ft := &fakeTransport{err: boom}
	c := newTestClient(t, ft)

	// Act
	err := c.Ping(context.Background())

	// Assert
	var domain *errs.Error
	if !errors.As(err, &domain) {
		t.Fatalf("Ping() = %v, want *errs.Error", err)
	}
	if domain.Fields["method"] != http.MethodGet || domain.Fields["path"] != rootPath {
		t.Errorf("Fields = %v, want method=GET path=%q", domain.Fields, rootPath)
	}
	if domain.Msg != "service unavailable" {
		t.Errorf("Msg = %q, want the generic public text", domain.Msg)
	}
	if strings.Contains(domain.Msg, "127.0.0.1") {
		t.Error("public message leaked the cause")
	}
	if !errors.Is(err, boom) {
		t.Error("cause dropped from the chain; logs lose the reason")
	}
}

// TestTransportEdgesAreUnavailable covers the two answers only a doer can
// produce: no response at all, and a body that fails while being read.
func TestTransportEdgesAreUnavailable(t *testing.T) {
	tests := []struct {
		name string
		stub stubDoer
	}{
		{name: "nil response without error", stub: stubDoer{}},
		{
			name: "body read fails",
			stub: stubDoer{resp: &opensearch.Response{
				StatusCode: http.StatusOK,
				Body:       errReader{err: errors.New("unexpected EOF")},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			c := &client{os: tt.stub, index: IndexName, log: slog.New(slog.DiscardHandler)}

			// Act
			err := c.Ping(context.Background())

			// Assert
			if !errors.Is(err, errs.ErrUnavailable) {
				t.Fatalf("Ping() = %v, want errs.ErrUnavailable", err)
			}
		})
	}
}

// TestClientSatisfiesNarrowConsumerInterfaces guards the duck-typed contracts
// other packages declare against this one (code-standards §1.1): they must
// keep compiling without importing the wide Client.
func TestClientSatisfiesNarrowConsumerInterfaces(t *testing.T) {
	// Arrange
	type episodeIndexer interface {
		IndexRecords(ctx context.Context, key hotstore.ProjectKey, recs []episodic.Record) error
		DeleteRecords(ctx context.Context, key hotstore.ProjectKey, ids []string) error
	}
	type indexProbe interface {
		Ping(ctx context.Context) error
		DocCount(ctx context.Context, key hotstore.ProjectKey) (int, error)
	}
	c := newTestClient(t, &fakeTransport{})

	// Act / Assert — compile-time; the assignments are the assertion.
	var _ episodeIndexer = c
	var _ indexProbe = c
	var _ Client = c
}
