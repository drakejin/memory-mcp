package apierr_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/drakejin/memory-mcp/internal/errs"
	"github.com/drakejin/memory-mcp/internal/server/apierr"
)

// errSecret stands in for a cause that must never reach a client: it carries an
// internal host and a credential-shaped token.
var errSecret = errors.New("neo4j://neo4j:hunter2@127.0.0.1:7687: auth failed")

func TestFromMappingTable(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantStatus  int
		wantCode    string
		wantMessage string
	}{
		{
			name:        "invalid",
			err:         errs.Invalid("episodic.Validate", "episode", "kind must be one of event|conversation"),
			wantStatus:  http.StatusBadRequest,
			wantCode:    apierr.CodeInvalidRequest,
			wantMessage: "kind must be one of event|conversation",
		},
		{
			name:        "not found",
			err:         errs.NotFound("hotstore.ReadEpisode", "episode", "01JD"),
			wantStatus:  http.StatusNotFound,
			wantCode:    apierr.CodeNotFound,
			wantMessage: "episode not found",
		},
		{
			name:        "conflict",
			err:         errs.Conflict("knowledge.Supersede", "knowledge_node", "01JX", "node already superseded"),
			wantStatus:  http.StatusConflict,
			wantCode:    apierr.CodeConflict,
			wantMessage: "node already superseded",
		},
		{
			name:        "unavailable",
			err:         errs.Unavailable("search.Search", errSecret),
			wantStatus:  http.StatusServiceUnavailable,
			wantCode:    apierr.CodeUnavailable,
			wantMessage: "service unavailable",
		},
		{
			name:        "internal",
			err:         errs.Internal("cold.PutEpisodeBatch", errSecret),
			wantStatus:  http.StatusInternalServerError,
			wantCode:    apierr.CodeInternal,
			wantMessage: "internal error",
		},
		{
			name:        "unclassified kind falls back to internal",
			err:         &errs.Error{Kind: errs.Kind("something-new"), Op: "future.Op"},
			wantStatus:  http.StatusInternalServerError,
			wantCode:    apierr.CodeInternal,
			wantMessage: "internal error",
		},
		{
			name:        "unknown error is internal and says nothing about itself",
			err:         errSecret,
			wantStatus:  http.StatusInternalServerError,
			wantCode:    apierr.CodeInternal,
			wantMessage: "internal error",
		},
		{
			name:        "domain error behind fmt wrapping",
			err:         fmt.Errorf("collect candidates: %w", errs.NotFound("hotstore.ReadEpisode", "episode", "01JD")),
			wantStatus:  http.StatusNotFound,
			wantCode:    apierr.CodeNotFound,
			wantMessage: "episode not found",
		},
		{
			name:        "errs.Wrap keeps the innermost public message",
			err:         errs.Wrap("server.handleConsolidate", errs.Wrap("consolidate.Run", errs.Conflict("knowledge.Supersede", "knowledge_node", "01JX", "node already superseded"))),
			wantStatus:  http.StatusConflict,
			wantCode:    apierr.CodeConflict,
			wantMessage: "node already superseded",
		},
		{
			name:        "errs.Wrap of a foreign error stays generic",
			err:         errs.Wrap("graph.Upsert", errSecret),
			wantStatus:  http.StatusInternalServerError,
			wantCode:    apierr.CodeInternal,
			wantMessage: "internal error",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := apierr.From(tc.err)
			if got == nil {
				t.Fatal("From returned nil for a non-nil error")
			}
			if got.Status != tc.wantStatus {
				t.Errorf("Status = %d, want %d", got.Status, tc.wantStatus)
			}
			if got.Code != tc.wantCode {
				t.Errorf("Code = %q, want %q", got.Code, tc.wantCode)
			}
			if got.Message != tc.wantMessage {
				t.Errorf("Message = %q, want %q", got.Message, tc.wantMessage)
			}
			if got.Details != nil {
				t.Errorf("Details = %v, want nil unless a handler sets one", got.Details)
			}
			if !errors.Is(got, tc.err) {
				t.Error("the domain error must stay reachable for logging")
			}
		})
	}
}

func TestFromNil(t *testing.T) {
	if got := apierr.From(nil); got != nil {
		t.Fatalf("From(nil) = %#v, want nil", got)
	}
}

// TestFromNeverLeaksTheCause is the rule that keeps OpenSearch/Neo4j/S3 replies
// out of the response body: the cause is reachable for logs, never in Message.
func TestFromNeverLeaksTheCause(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"foreign error", errSecret},
		{"wrapped foreign error", fmt.Errorf("upsert nodes: %w", errSecret)},
		{"domain internal", errs.Internal("graph.Upsert", errSecret)},
		{"domain unavailable", errs.Unavailable("graph.Ping", errSecret)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := apierr.From(tc.err)
			if strings.Contains(got.Message, "hunter2") || strings.Contains(got.Message, "7687") {
				t.Errorf("Message = %q leaks the cause", got.Message)
			}
			body, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if strings.Contains(string(body), "hunter2") {
				t.Errorf("response body %s leaks the cause", body)
			}
			if !strings.Contains(got.Error(), "hunter2") {
				t.Errorf("Error() = %q must keep the cause for server-side logs", got.Error())
			}
			if !errors.Is(got, errSecret) {
				t.Error("the cause must stay reachable through errors.Is")
			}
		})
	}
}

func TestNew(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		code       string
		message    string
		wantStatus int
	}{
		{
			name:       "transport failure",
			status:     http.StatusBadRequest,
			code:       apierr.CodeInvalidRequest,
			message:    "malformed json body",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "out-of-range status is normalised to 500",
			status:     0,
			code:       apierr.CodeInternal,
			message:    "internal error",
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:       "absurd status is normalised to 500",
			status:     9000,
			code:       apierr.CodeInternal,
			message:    "internal error",
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := apierr.New(tc.status, tc.code, tc.message)
			if got.Status != tc.wantStatus {
				t.Errorf("Status = %d, want %d", got.Status, tc.wantStatus)
			}
			if got.Code != tc.code {
				t.Errorf("Code = %q, want %q", got.Code, tc.code)
			}
			if got.Message != tc.message {
				t.Errorf("Message = %q, want %q", got.Message, tc.message)
			}
			if got.Unwrap() != nil {
				t.Errorf("Unwrap() = %v, want nil", got.Unwrap())
			}
		})
	}
}

func TestWithCauseAndWithDetailDoNotMutate(t *testing.T) {
	base := apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest, "chunk limit exceeded")
	withDetail := base.WithDetail("total", 620).WithDetail("indexed", 500)
	withCause := withDetail.WithCause(errSecret)

	if base.Details != nil {
		t.Errorf("base.Details = %v, want the original untouched", base.Details)
	}
	if base.Unwrap() != nil {
		t.Error("base must not gain a cause")
	}
	if len(withDetail.Details) != 2 || withDetail.Details["total"] != 620 || withDetail.Details["indexed"] != 500 {
		t.Errorf("Details = %v, want {total:620, indexed:500}", withDetail.Details)
	}
	if withDetail.Unwrap() != nil {
		t.Error("WithDetail must not attach a cause")
	}
	if !errors.Is(withCause, errSecret) {
		t.Error("WithCause must make the cause reachable")
	}
	if withCause.Status != base.Status || withCause.Code != base.Code || withCause.Message != base.Message {
		t.Error("the builders must copy every other field verbatim")
	}
}

// TestJSONShape pins the public projection: status stays in the HTTP status
// line and the cause never appears.
func TestJSONShape(t *testing.T) {
	tests := []struct {
		name string
		err  *apierr.Error
		want string
	}{
		{
			name: "without details",
			err:  apierr.From(errs.NotFound("hotstore.ReadEpisode", "episode", "01JD")),
			want: `{"code":"not_found","message":"episode not found"}`,
		},
		{
			name: "with details",
			err:  apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest, "chunk limit exceeded").WithDetail("total", 620).WithCause(errSecret),
			want: `{"code":"invalid_request","message":"chunk limit exceeded","details":{"total":620}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(tc.err)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(body) != tc.want {
				t.Errorf("json = %s, want %s", body, tc.want)
			}
		})
	}
}

func TestErrorString(t *testing.T) {
	tests := []struct {
		name string
		err  *apierr.Error
		want string
	}{
		{
			name: "without cause",
			err:  apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest, "malformed json body"),
			want: "400 invalid_request: malformed json body",
		},
		{
			name: "with cause",
			err:  apierr.From(errs.Unavailable("search.Search", errors.New("connection refused"))),
			want: "503 unavailable: service unavailable: search.Search: service unavailable: connection refused",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFromIsIdempotentOnTransportErrors keeps a double conversion from turning
// an already-mapped 404 into a 500.
func TestFromIsIdempotentOnTransportErrors(t *testing.T) {
	tests := []struct {
		name string
		err  *apierr.Error
	}{
		{"mapped domain error", apierr.From(errs.NotFound("hotstore.ReadEpisode", "episode", "01JD"))},
		{"transport-only error", apierr.New(http.StatusBadRequest, apierr.CodeInvalidRequest, "malformed json body")},
		{"transport error behind fmt wrapping", apierr.New(http.StatusNotImplemented, "not_implemented", "no adapter")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := apierr.From(fmt.Errorf("handler: %w", tc.err))
			if got.Status != tc.err.Status || got.Code != tc.err.Code || got.Message != tc.err.Message {
				t.Errorf("From(wrapped) = %+v, want %+v", got, tc.err)
			}
		})
	}
}
