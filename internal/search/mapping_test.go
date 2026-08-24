package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/drakejin/memory-mcp/internal/episodic"
)

// Tests for index mapping and schema convergence: the episodic index must
// always be created from indexBody (nori analyzer, id as keyword) and never
// be left as the dynamic mapping OpenSearch auto-creates on a bare _bulk.

// OpenSearch normalizes the analysis block under settings.index.analysis. A
// correctly created index carries the custom korean analyzer; an index that a
// _bulk write auto-created has no analysis block at all.
const (
	settingsMapped  = `{"` + IndexName + `":{"settings":{"index":{"number_of_shards":"1","analysis":{"analyzer":{"korean":{"type":"custom"}}}}}}}`
	settingsDynamic = `{"` + IndexName + `":{"settings":{"index":{"number_of_shards":"1","number_of_replicas":"1"}}}}`
)

func TestEnsureIndexCreatesWithNoriMapping(t *testing.T) {
	// Arrange
	ft := &fakeTransport{handler: func(call fakeCall) (int, string) {
		if call.Method == http.MethodHead {
			return http.StatusNotFound, `{}`
		}
		return http.StatusOK, `{"acknowledged":true}`
	}}
	c := newTestClient(t, ft)

	// Act
	if err := c.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("EnsureIndex() = %v", err)
	}

	// Assert
	calls := ft.recorded()
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want HEAD then PUT: %+v", len(calls), calls)
	}
	if calls[0].Method != http.MethodHead || calls[0].Path != "/"+IndexName {
		t.Fatalf("first call = %s %s, want HEAD /%s", calls[0].Method, calls[0].Path, IndexName)
	}
	put := calls[1]
	if put.Method != http.MethodPut || put.Path != "/"+IndexName {
		t.Fatalf("second call = %s %s, want PUT /%s", put.Method, put.Path, IndexName)
	}
	var mapping map[string]any
	if err := json.Unmarshal(put.Body, &mapping); err != nil {
		t.Fatalf("index body is not JSON: %v", err)
	}
	for _, needle := range []string{"nori_tokenizer", "nori_part_of_speech", `"korean"`, `"occurred_at"`, `"workspace"`} {
		if !strings.Contains(string(put.Body), needle) {
			t.Errorf("index body missing %s", needle)
		}
	}
}

func TestEnsureIndexIdempotent(t *testing.T) {
	tests := []struct {
		name      string
		handler   func(call fakeCall) (int, string)
		wantCalls []string
	}{
		{
			// An existing index is only accepted once its settings prove the
			// nori analyzer is really there.
			name: "already exists with nori mapping",
			handler: func(call fakeCall) (int, string) {
				if call.Method == http.MethodGet {
					return http.StatusOK, settingsMapped
				}
				return http.StatusOK, `{}`
			},
			wantCalls: []string{"HEAD /" + IndexName, "GET /" + IndexName + "/_settings"},
		},
		{
			name: "concurrent create race",
			handler: func(call fakeCall) (int, string) {
				if call.Method == http.MethodHead {
					return http.StatusNotFound, `{}`
				}
				return http.StatusBadRequest, `{"error":{"type":"resource_already_exists_exception"}}`
			},
			wantCalls: []string{"HEAD /" + IndexName, "PUT /" + IndexName},
		},
		{
			// The regression this guards: an index auto-created by a _bulk
			// write has a dynamic mapping and no analyzer, so Korean recall
			// is silently dead. It must be dropped and recreated.
			name: "auto-created dynamic index is repaired",
			handler: func(call fakeCall) (int, string) {
				if call.Method == http.MethodGet {
					return http.StatusOK, settingsDynamic
				}
				return http.StatusOK, `{"acknowledged":true}`
			},
			wantCalls: []string{
				"HEAD /" + IndexName,
				"GET /" + IndexName + "/_settings",
				"DELETE /" + IndexName,
				"PUT /" + IndexName,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			ft := &fakeTransport{handler: tt.handler}
			c := newTestClient(t, ft)

			// Act / Assert
			if err := c.EnsureIndex(context.Background()); err != nil {
				t.Fatalf("EnsureIndex() = %v, want nil", err)
			}
			got := make([]string, 0, len(ft.recorded()))
			for _, call := range ft.recorded() {
				got = append(got, call.Method+" "+call.Path)
			}
			if strings.Join(got, ", ") != strings.Join(tt.wantCalls, ", ") {
				t.Fatalf("calls = %v, want %v", got, tt.wantCalls)
			}
		})
	}
}

// TestWritesEnsureMappingBeforeBulk pins the §10-2 regression: OpenSearch
// auto-creates a missing index on the first _bulk with a dynamic mapping (no
// nori analyzer, id as text), which silently breaks Korean morphological
// recall and makes sorting on id a hard 400. Every write path must therefore
// create the index through EnsureIndex before it sends any bulk payload.
func TestWritesEnsureMappingBeforeBulk(t *testing.T) {
	tests := []struct {
		name  string
		write func(c *Client) error
	}{
		{
			name: "IndexRecords",
			write: func(c *Client) error {
				return c.IndexRecords(context.Background(), testKey(),
					[]episodic.Record{testRecord("01JD0000000000000000000001", "보안을 끄고 배포했다")})
			},
		},
		{
			name: "DeleteRecords",
			write: func(c *Client) error {
				return c.DeleteRecords(context.Background(), testKey(),
					[]string{"01JD0000000000000000000001"})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange: the index does not exist yet.
			ft := &fakeTransport{handler: func(call fakeCall) (int, string) {
				if call.Method == http.MethodHead {
					return http.StatusNotFound, `{}`
				}
				if call.Path == "/_bulk" {
					return http.StatusOK, `{"errors":false,"items":[]}`
				}
				return http.StatusOK, `{"acknowledged":true}`
			}}
			c := newTestClient(t, ft)

			// Act
			if err := tt.write(c); err != nil {
				t.Fatalf("write = %v, want nil", err)
			}

			// Assert: the mapping PUT precedes the bulk, and carries nori.
			calls := ft.recorded()
			putAt, bulkAt := -1, -1
			for i, call := range calls {
				switch {
				case call.Method == http.MethodPut && call.Path == "/"+IndexName:
					putAt = i
					if !bytes.Contains(call.Body, []byte("nori_tokenizer")) {
						t.Error("index creation body does not carry the nori mapping")
					}
				case call.Path == "/_bulk":
					bulkAt = i
				}
			}
			if putAt < 0 {
				t.Fatalf("no index creation before bulk: %+v", calls)
			}
			if bulkAt < 0 || bulkAt < putAt {
				t.Fatalf("bulk at %d must follow index creation at %d: %+v", bulkAt, putAt, calls)
			}

			// A second write reuses the converged mapping: no extra probes.
			before := len(ft.recorded())
			if err := tt.write(c); err != nil {
				t.Fatalf("second write = %v", err)
			}
			if extra := len(ft.recorded()) - before; extra != 1 {
				t.Fatalf("second write made %d calls, want 1 bulk only", extra)
			}
		})
	}
}

// TestDropResetsMappingState guards the drop+rebuild path: after Drop the
// index is gone, so the next write must re-create it rather than trust the
// cached "already converged" state and let _bulk auto-create it.
func TestDropResetsMappingState(t *testing.T) {
	// Arrange
	ft := &fakeTransport{handler: func(call fakeCall) (int, string) {
		if call.Method == http.MethodHead {
			return http.StatusNotFound, `{}`
		}
		if call.Path == "/_bulk" {
			return http.StatusOK, `{"errors":false,"items":[]}`
		}
		return http.StatusOK, `{"acknowledged":true}`
	}}
	c := newReadyTestClient(t, ft)

	// Act
	if err := c.Drop(context.Background()); err != nil {
		t.Fatalf("Drop() = %v", err)
	}
	if err := c.IndexRecords(context.Background(), testKey(),
		[]episodic.Record{testRecord("01JD0000000000000000000001", "x")}); err != nil {
		t.Fatalf("IndexRecords() = %v", err)
	}

	// Assert
	var sawPut bool
	for _, call := range ft.recorded() {
		if call.Method == http.MethodPut && call.Path == "/"+IndexName {
			sawPut = true
		}
	}
	if !sawPut {
		t.Fatalf("write after Drop did not recreate the index: %+v", ft.recorded())
	}
}

func TestEnsureIndexUnavailable(t *testing.T) {
	// Arrange
	ft := &fakeTransport{err: errors.New("dial tcp: connection refused")}
	c := newTestClient(t, ft)

	// Act / Assert
	if err := c.EnsureIndex(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("EnsureIndex() = %v, want ErrUnavailable", err)
	}
}
