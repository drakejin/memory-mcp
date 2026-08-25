package httpserver

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/handler/app/httpserver/apierr"
	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/x/errs"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"

	// The swag-generated spec registers itself in an init; without it
	// /swagger/doc.json answers 500 in the test binary.
	_ "github.com/drakejin/memory-mcp/docs"
)

// testSHA is a well-formed lowercase-hex sha256. The external test package
// (fakes_test.go) declares its own copy; the two test packages cannot share
// unexported helpers.
const testSHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Valid 26-char Crockford-base32 ULIDs (same fixtures as fakes_test.go).
const (
	ulidA = "01JD00000000000000000000A0"
	ulidB = "01JD00000000000000000000B0"
)

// errBoom is an unclassified collaborator failure for the envelope tests.
var errBoom = errors.New("boom")

func TestValidateProjectKey(t *testing.T) {
	tests := []struct {
		name    string
		key     projectkey.Key
		wantErr bool
	}{
		{"plain", projectkey.Key{Workspace: "ws", Team: "team", Project: "proj"}, false},
		{"punctuation allowed", projectkey.Key{Workspace: "a-b", Team: "c_d", Project: "e.f"}, false},
		{"digits", projectkey.Key{Workspace: "w1", Team: "t2", Project: "p3"}, false},
		{"empty workspace", projectkey.Key{Team: "team", Project: "proj"}, true},
		{"empty team", projectkey.Key{Workspace: "ws", Project: "proj"}, true},
		{"empty project", projectkey.Key{Workspace: "ws", Team: "team"}, true},
		{"uppercase", projectkey.Key{Workspace: "WS", Team: "team", Project: "proj"}, true},
		{"dot", projectkey.Key{Workspace: ".", Team: "team", Project: "proj"}, true},
		{"dotdot", projectkey.Key{Workspace: "ws", Team: "..", Project: "proj"}, true},
		{"slash inside segment", projectkey.Key{Workspace: "ws/x", Team: "team", Project: "proj"}, true},
		{"space", projectkey.Key{Workspace: "w s", Team: "team", Project: "proj"}, true},
		{"korean", projectkey.Key{Workspace: "워크", Team: "team", Project: "proj"}, true},
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
		want    []episode.Kind
		wantErr bool
	}{
		{"empty", "", nil, false},
		{"single", "event", []episode.Kind{episode.KindEvent}, false},
		{"multiple with spaces", "event, decision ", []episode.Kind{episode.KindEvent, episode.KindDecision}, false},
		{"blank entries skipped", "event,,decision", []episode.Kind{episode.KindEvent, episode.KindDecision}, false},
		{"all kinds", "event,conversation,decision,observation,document_chunk", []episode.Kind{
			episode.KindEvent, episode.KindConversation, episode.KindDecision,
			episode.KindObservation, episode.KindDocumentChunk,
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
