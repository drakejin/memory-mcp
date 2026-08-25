package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/errs"
	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/server/apierr"

	// The swag-generated spec registers itself in an init; without it
	// /swagger/doc.json answers 500 in the test binary.
	_ "github.com/drakejin/memory-mcp/docs"
)

func TestValidateProjectKey(t *testing.T) {
	tests := []struct {
		name    string
		key     hotstore.ProjectKey
		wantErr bool
	}{
		{"plain", hotstore.ProjectKey{Workspace: "ws", Team: "team", Project: "proj"}, false},
		{"punctuation allowed", hotstore.ProjectKey{Workspace: "a-b", Team: "c_d", Project: "e.f"}, false},
		{"digits", hotstore.ProjectKey{Workspace: "w1", Team: "t2", Project: "p3"}, false},
		{"empty workspace", hotstore.ProjectKey{Team: "team", Project: "proj"}, true},
		{"empty team", hotstore.ProjectKey{Workspace: "ws", Project: "proj"}, true},
		{"empty project", hotstore.ProjectKey{Workspace: "ws", Team: "team"}, true},
		{"uppercase", hotstore.ProjectKey{Workspace: "WS", Team: "team", Project: "proj"}, true},
		{"dot", hotstore.ProjectKey{Workspace: ".", Team: "team", Project: "proj"}, true},
		{"dotdot", hotstore.ProjectKey{Workspace: "ws", Team: "..", Project: "proj"}, true},
		{"slash inside segment", hotstore.ProjectKey{Workspace: "ws/x", Team: "team", Project: "proj"}, true},
		{"space", hotstore.ProjectKey{Workspace: "w s", Team: "team", Project: "proj"}, true},
		{"korean", hotstore.ProjectKey{Workspace: "워크", Team: "team", Project: "proj"}, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateProjectKey(tc.key)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateProjectKey(%+v) error = %v, wantErr %v", tc.key, err, tc.wantErr)
			}
		})
	}
}

func TestValidateSHA(t *testing.T) {
	tests := []struct {
		name    string
		sha     string
		wantErr bool
	}{
		{"valid", testSHA, false},
		{"empty", "", true},
		{"too short", strings.Repeat("a", 63), true},
		{"too long", strings.Repeat("a", 65), true},
		{"uppercase", strings.ToUpper(testSHA), true},
		{"non-hex", strings.Repeat("g", 64), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateSHA(tc.sha); (err != nil) != tc.wantErr {
				t.Fatalf("validateSHA(%q) error = %v, wantErr %v", tc.sha, err, tc.wantErr)
			}
		})
	}
}

func TestSplitProject(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"three segments", "ws/team/proj", []string{"ws", "team", "proj"}},
		{"empty middle still splits", "ws//proj", []string{"ws", "", "proj"}},
		{"two segments", "ws/team", nil},
		{"four segments", "ws/team/proj/extra", nil},
		{"trailing slash", "ws/team/proj/", nil},
		{"leading slash", "/ws/team/proj", nil},
		{"empty", "", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := splitProject(tc.raw)
			if len(got) != len(tc.want) {
				t.Fatalf("splitProject(%q) = %v, want %v", tc.raw, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("splitProject(%q) = %v, want %v", tc.raw, got, tc.want)
				}
			}
		})
	}
}

func TestParseProjectString(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{"valid", "ws/team/proj", "ws/team/proj", false},
		{"wrong shape", "ws/team", "", true},
		{"bad charset", "WS/team/proj", "", true},
		{"empty segment", "ws//proj", "", true},
		{"dot segment", "ws/../proj", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseProjectString(tc.raw)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseProjectString(%q) error = %v, wantErr %v", tc.raw, err, tc.wantErr)
			}
			if err != nil {
				if !strings.Contains(err.Error(), tc.raw) {
					t.Errorf("error %q should name the offending selector %q", err, tc.raw)
				}
				return
			}
			if got.String() != tc.want {
				t.Fatalf("parseProjectString(%q) = %q, want %q", tc.raw, got.String(), tc.want)
			}
		})
	}
}

func TestParseTimeParam(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    time.Time
		wantErr bool
	}{
		{"empty is zero", "", time.Time{}, false},
		{"rfc3339", "2026-08-25T02:00:00Z", time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC), false},
		{"date only", "2026-08-25", time.Time{}, true},
		{"garbage", "yesterday", time.Time{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTimeParam(tc.raw, "from")
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseTimeParam(%q) error = %v, wantErr %v", tc.raw, err, tc.wantErr)
			}
			if err == nil && !got.Equal(tc.want) {
				t.Fatalf("parseTimeParam(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestParseKindsParam(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []episodic.Kind
		wantErr bool
	}{
		{"empty", "", nil, false},
		{"single", "event", []episodic.Kind{episodic.KindEvent}, false},
		{"multiple with spaces", "event, decision ", []episodic.Kind{episodic.KindEvent, episodic.KindDecision}, false},
		{"blank entries skipped", "event,,decision", []episodic.Kind{episodic.KindEvent, episodic.KindDecision}, false},
		{"all kinds", "event,conversation,decision,observation,document_chunk", []episodic.Kind{
			episodic.KindEvent, episodic.KindConversation, episodic.KindDecision,
			episodic.KindObservation, episodic.KindDocumentChunk,
		}, false},
		{"unknown", "event,rumor", nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseKindsParam(tc.raw)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseKindsParam(%q) error = %v, wantErr %v", tc.raw, err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("parseKindsParam(%q) = %v, want %v", tc.raw, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("parseKindsParam(%q) = %v, want %v", tc.raw, got, tc.want)
				}
			}
		})
	}
}

func TestNormalizeStrings(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil becomes empty non-nil", nil, []string{}},
		{"trims", []string{" a ", "b"}, []string{"a", "b"}},
		{"drops blanks", []string{"", "   ", "a"}, []string{"a"}},
		{"keeps order", []string{"z", "a"}, []string{"z", "a"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeStrings(tc.in)
			if got == nil {
				t.Fatal("normalizeStrings must never return nil (hot JSON stores [] not null)")
			}
			if len(got) != len(tc.want) {
				t.Fatalf("normalizeStrings(%v) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("normalizeStrings(%v) = %v, want %v", tc.in, got, tc.want)
				}
			}
		})
	}
}

func TestValidateULIDs(t *testing.T) {
	tests := []struct {
		name    string
		ids     []string
		wantErr bool
	}{
		{"empty", nil, false},
		{"all valid", []string{ulidA, ulidB}, false},
		{"one bad", []string{ulidA, "nope"}, true},
		{"empty string", []string{""}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateULIDs(tc.ids, "provenance")
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateULIDs(%v) error = %v, wantErr %v", tc.ids, err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "provenance") {
				t.Errorf("error %q must name the offending field", err)
			}
		})
	}
}

func TestDecodeJSONBodyLimits(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		allowEmpty bool
		wantErr    bool
	}{
		{"object", `{"kind":"event"}`, false, false},
		{"empty rejected", "", false, true},
		{"whitespace rejected", "   \n", false, true},
		{"empty allowed", "", true, false},
		{"whitespace allowed", "  ", true, false},
		{"malformed", "{", false, true},
		{"oversized", `{"text":"` + strings.Repeat("x", maxJSONBodyBytes+1) + `"}`, false, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			w := httptest.NewRecorder()
			var dst map[string]any
			err := decodeJSON(w, req, &dst, tc.allowEmpty)
			if (err != nil) != tc.wantErr {
				t.Fatalf("decodeJSON error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestEnvelopeShape(t *testing.T) {
	quiet := slog.New(slog.DiscardHandler)
	// A cause that must stay in the log and out of every response body.
	secret := errs.Unavailable("search.Search", errors.New("opensearch said: index_not_found_exception at /var/lib/opensearch"))

	tests := []struct {
		name        string
		write       func(w http.ResponseWriter)
		wantStatus  int
		wantSuccess bool
		wantCode    string
		wantMessage string
	}{
		{
			name:        "success",
			write:       func(w http.ResponseWriter) { writeJSON(w, quiet, http.StatusOK, map[string]int{"n": 1}) },
			wantStatus:  http.StatusOK,
			wantSuccess: true,
		},
		{
			name:        "a transport-only rejection",
			write:       func(w http.ResponseWriter) { writeAPIError(w, quiet, badRequest("nope")) },
			wantStatus:  http.StatusBadRequest,
			wantCode:    apierr.CodeInvalidRequest,
			wantMessage: "nope",
		},
		{
			name:        "an absent collaborator is 503, not 500",
			write:       func(w http.ResponseWriter) { writeAPIError(w, quiet, unavailable(degradedSearch)) },
			wantStatus:  http.StatusServiceUnavailable,
			wantCode:    apierr.CodeUnavailable,
			wantMessage: degradedSearch,
		},
		{
			name:        "a transport-resolved miss is 404",
			write:       func(w http.ResponseWriter) { writeAPIError(w, quiet, notFound("node not found")) },
			wantStatus:  http.StatusNotFound,
			wantCode:    apierr.CodeNotFound,
			wantMessage: "node not found",
		},
		{
			name: "domain failure crosses the apierr boundary",
			write: func(w http.ResponseWriter) {
				writeAPIError(w, quiet, apierr.From(errs.NotFound("hotstore.ReadEpisode", "episode", "01JD")))
			},
			wantStatus:  http.StatusNotFound,
			wantCode:    apierr.CodeNotFound,
			wantMessage: "episode not found",
		},
		{
			name:        "cause stays out of the body",
			write:       func(w http.ResponseWriter) { writeAPIError(w, quiet, apierr.From(secret)) },
			wantStatus:  http.StatusServiceUnavailable,
			wantCode:    apierr.CodeUnavailable,
			wantMessage: "service unavailable",
		},
		{
			name:        "an unclassified failure never leaks its cause",
			write:       func(w http.ResponseWriter) { writeAPIError(w, quiet, apierr.From(errBoom)) },
			wantStatus:  http.StatusInternalServerError,
			wantCode:    apierr.CodeInternal,
			wantMessage: msgInternal,
		},
		{
			name:        "a nil transport error still answers",
			write:       func(w http.ResponseWriter) { writeAPIError(w, quiet, nil) },
			wantStatus:  http.StatusInternalServerError,
			wantCode:    apierr.CodeInternal,
			wantMessage: msgInternal,
		},
		{
			name:        "a nil logger does not panic on the response path",
			write:       func(w http.ResponseWriter) { writeAPIError(w, nil, badRequest("nope")) },
			wantStatus:  http.StatusBadRequest,
			wantCode:    apierr.CodeInvalidRequest,
			wantMessage: "nope",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			tc.write(w)
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tc.wantStatus)
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("content-type = %q, want application/json", ct)
			}
			if strings.Contains(w.Body.String(), "opensearch said") {
				t.Errorf("body %s leaks the cause", w.Body.String())
			}
			var env Envelope
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("body is not an envelope: %v", err)
			}
			if env.Success != tc.wantSuccess {
				t.Errorf("success = %v, want %v", env.Success, tc.wantSuccess)
			}
			if tc.wantCode == "" {
				if env.Error != nil {
					t.Errorf("error = %+v, want it omitted on success", env.Error)
				}
				return
			}
			if env.Error == nil {
				t.Fatalf("failure envelope carries no error object: %s", w.Body.String())
			}
			if env.Error.Code != tc.wantCode {
				t.Errorf("error.code = %q, want %q", env.Error.Code, tc.wantCode)
			}
			if env.Error.Message != tc.wantMessage {
				t.Errorf("error.message = %q, want %q", env.Error.Message, tc.wantMessage)
			}
		})
	}
}

// TestNewRejectsUnservableConfig pins the constructor contract: the intrinsic
// dependencies must be present, and the §7 trust boundary ("no auth is safe
// because it is not reachable") is enforced here too, not only in config.Load.
// A derived store is deliberately not required — §5 says a dead one degrades
// the server rather than stopping it.
func TestNewRejectsUnservableConfig(t *testing.T) {
	base := func() Config {
		return Config{
			ListenAddr:      testListenAddr,
			S3Bucket:        testS3Bucket,
			EpisodicTTLDays: testTTLDays,
			Clock:           fakeClock{now: fixedNow},
			IDs:             &fakeIDs{},
			Logger:          discardLogger(),
		}
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"loopback ip", nil, false},
		{"localhost name", func(c *Config) { c.ListenAddr = "localhost:8420" }, false},
		{"every derived store absent is legal", func(c *Config) { c.Store, c.Index, c.Graph = nil, nil, nil }, false},
		{"all interfaces bound", func(c *Config) {
			c.Store, c.Index, c.Graph = newFakeStore(), newFakeIndex(), newFakeGraph()
		}, false},
		{"empty listen addr", func(c *Config) { c.ListenAddr = "" }, true},
		{"wildcard bind", func(c *Config) { c.ListenAddr = "0.0.0.0:8420" }, true},
		{"routable bind", func(c *Config) { c.ListenAddr = "192.168.0.10:8420" }, true},
		{"zero ttl", func(c *Config) { c.EpisodicTTLDays = 0 }, true},
		{"negative ttl", func(c *Config) { c.EpisodicTTLDays = -1 }, true},
		{"no clock", func(c *Config) { c.Clock = nil }, true},
		{"no id generator", func(c *Config) { c.IDs = nil }, true},
		{"no logger", func(c *Config) { c.Logger = nil }, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			if tc.mutate != nil {
				tc.mutate(&cfg)
			}
			srv, err := New(cfg)
			if (err != nil) != tc.wantErr {
				t.Fatalf("New error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				if srv != nil {
					t.Error("a rejected config must not yield a server")
				}
				// A constructor is below the transport boundary, so it speaks
				// the domain vocabulary (§2.1), not HTTP.
				if !errors.Is(err, errs.ErrInvalid) {
					t.Errorf("error = %v, want errs.ErrInvalid", err)
				}
				return
			}
			if srv == nil {
				t.Fatal("New returned no server and no error")
			}
		})
	}
}

// specPaths fetches the served OpenAPI document and returns its paths object.
func specPaths(t *testing.T) map[string]map[string]any {
	t.Helper()
	_, h := newTestServer(t, nil)
	rec := do(t, h, http.MethodGet, "/swagger/doc.json", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("swagger doc.json status = %d, want 200 (run `make swagger`)", rec.Code)
	}
	var spec struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
		t.Fatalf("swagger doc.json is not JSON: %v", err)
	}
	if len(spec.Paths) == 0 {
		t.Fatal("swagger spec declares no paths")
	}
	return spec.Paths
}

// TestEveryRouteIsAnnotated pins the §7 endpoint table to the generated spec:
// a route added to Router without a swaggo block fails here.
func TestEveryRouteIsAnnotated(t *testing.T) {
	want := []struct{ path, method string }{
		{"/healthz", "get"},
		{"/v1/status", "get"},
		{"/v1/consolidate", "post"},
		{"/v1/reindex", "post"},
		{"/v1/documents/{sha}", "get"},
		{"/v1/documents/{sha}/chunks", "get"},
		{"/v1/{ws}/{team}/{proj}/episodes", "post"},
		{"/v1/{ws}/{team}/{proj}/episodes/search", "get"},
		{"/v1/{ws}/{team}/{proj}/episodes/{id}", "get"},
		{"/v1/{ws}/{team}/{proj}/knowledge/nodes", "post"},
		{"/v1/{ws}/{team}/{proj}/knowledge/nodes/{id}", "patch"},
		{"/v1/{ws}/{team}/{proj}/knowledge/nodes/{id}", "delete"},
		{"/v1/{ws}/{team}/{proj}/knowledge/edges", "post"},
		{"/v1/{ws}/{team}/{proj}/knowledge/search", "get"},
		{"/v1/{ws}/{team}/{proj}/knowledge/graph", "get"},
		{"/v1/{ws}/{team}/{proj}/documents", "post"},
	}

	paths := specPaths(t)
	for _, tc := range want {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			ops, ok := paths[tc.path]
			if !ok {
				t.Fatalf("%s is missing from the OpenAPI spec", tc.path)
			}
			if _, ok := ops[tc.method]; !ok {
				t.Fatalf("%s %s is missing from the OpenAPI spec", tc.method, tc.path)
			}
		})
	}
	if len(paths) != 15 {
		t.Errorf("spec declares %d paths, want the 15 of §7 — update this test when the API changes", len(paths))
	}
}

func TestUnknownRouteIs404(t *testing.T) {
	_, h := newTestServer(t, nil)
	rec := do(t, h, http.MethodGet, "/v1/nope", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
