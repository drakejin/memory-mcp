package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/hotstore"

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
		{"all kinds", "event,conversation,decision,observation,document_chunk", episodic.Kinds, false},
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
	tests := []struct {
		name        string
		write       func(w http.ResponseWriter)
		wantStatus  int
		wantSuccess bool
		wantError   string
	}{
		{
			name:        "success",
			write:       func(w http.ResponseWriter) { writeJSON(w, http.StatusOK, map[string]int{"n": 1}) },
			wantStatus:  http.StatusOK,
			wantSuccess: true,
		},
		{
			name:       "failure",
			write:      func(w http.ResponseWriter) { writeError(w, http.StatusBadRequest, "nope") },
			wantStatus: http.StatusBadRequest,
			wantError:  "nope",
		},
		{
			name:       "not implemented",
			write:      func(w http.ResponseWriter) { notImplemented(w) },
			wantStatus: http.StatusNotImplemented,
			wantError:  "not implemented",
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
			var env Envelope
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("body is not an envelope: %v", err)
			}
			if env.Success != tc.wantSuccess {
				t.Errorf("success = %v, want %v", env.Success, tc.wantSuccess)
			}
			if env.Error != tc.wantError {
				t.Errorf("error = %q, want %q", env.Error, tc.wantError)
			}
		})
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	srv := New(testConfig(), Deps{Store: newFakeStore()})
	if srv.deps.Logger == nil {
		t.Error("Logger must default to slog.Default()")
	}
	if srv.deps.Clock == nil {
		t.Error("Clock must default to SystemClock")
	}
	if _, ok := srv.deps.Clock.(hotstore.SystemClock); !ok {
		t.Errorf("Clock = %T, want hotstore.SystemClock", srv.deps.Clock)
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
