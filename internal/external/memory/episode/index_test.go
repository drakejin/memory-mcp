package episodemem

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/x/errs"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
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
		{
			// Settings vanished between HEAD and GET (another process dropped
			// the index): treat as unmapped and recreate rather than fail.
			name: "settings disappear mid-check",
			handler: func(call fakeCall) (int, string) {
				if call.Method == http.MethodGet {
					return http.StatusNotFound, `{"error":{"type":"index_not_found_exception"}}`
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
		write func(c *client) error
	}{
		{
			name: "IndexRecords",
			write: func(c *client) error {
				return c.IndexRecords(context.Background(), testKey(),
					[]episode.Record{testRecord("01JD0000000000000000000001", "보안을 끄고 배포했다")})
			},
		},
		{
			name: "DeleteRecords",
			write: func(c *client) error {
				return c.DeleteRecords(context.Background(), testKey(),
					[]string{"01JD0000000000000000000001"})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange: the index does not exist yet, and starts existing once
			// the client creates it — a stateful cluster, so the second write
			// exercises the converged path rather than a permanent 404.
			created := false
			ft := &fakeTransport{handler: func(call fakeCall) (int, string) {
				switch {
				case call.Method == http.MethodHead && !created:
					return http.StatusNotFound, `{}`
				case call.Method == http.MethodHead:
					return http.StatusOK, `{}`
				case call.Path == bulkPath:
					return http.StatusOK, `{"errors":false,"items":[]}`
				case call.Method == http.MethodPut:
					created = true
					return http.StatusOK, `{"acknowledged":true}`
				default:
					return http.StatusOK, `{"acknowledged":true}`
				}
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
				case call.Path == bulkPath:
					bulkAt = i
				}
			}
			if putAt < 0 {
				t.Fatalf("no index creation before bulk: %+v", calls)
			}
			if bulkAt < 0 || bulkAt < putAt {
				t.Fatalf("bulk at %d must follow index creation at %d: %+v", bulkAt, putAt, calls)
			}

			// A second write reuses the converged mapping: it re-confirms the
			// index is still there (one HEAD — the container may have been
			// replaced) but must not re-read the settings or re-PUT.
			before := len(ft.recorded())
			if err := tt.write(c); err != nil {
				t.Fatalf("second write = %v", err)
			}
			second := ft.recorded()[before:]
			got := make([]string, 0, len(second))
			for _, call := range second {
				got = append(got, call.Method+" "+call.Path)
			}
			want := []string{http.MethodHead + " /" + IndexName, http.MethodPost + " " + bulkPath}
			if strings.Join(got, ", ") != strings.Join(want, ", ") {
				t.Fatalf("second write calls = %v, want %v", got, want)
			}
		})
	}
}

// TestWriteAfterContainerReplacementRecreatesMapping pins the honesty hole the
// schemaReady latch used to open. The derived stores run without volumes and may
// be replaced under a live server (§1), but the latch only records what this
// process last saw. A latched client that trusts it sends _bulk straight at an
// empty cluster, OpenSearch auto-creates the index with a dynamic mapping, and
// Korean morphological recall is dead with a 201 and no degraded note anywhere.
func TestWriteAfterContainerReplacementRecreatesMapping(t *testing.T) {
	// Arrange: a client that already converged, then a cluster that lost the
	// index (fresh container) while the process kept running.
	ft := &fakeTransport{handler: func(call fakeCall) (int, string) {
		switch {
		case call.Method == http.MethodHead:
			return http.StatusNotFound, `{}`
		case call.Path == bulkPath:
			return http.StatusOK, `{"errors":false,"items":[]}`
		default:
			return http.StatusOK, `{"acknowledged":true}`
		}
	}}
	c := newReadyTestClient(t, ft)

	// Act
	if err := c.IndexRecords(context.Background(), testKey(),
		[]episode.Record{testRecord("01JD0000000000000000000001", "보안을 끄고 배포했다")}); err != nil {
		t.Fatalf("IndexRecords() = %v", err)
	}

	// Assert: the nori mapping is re-created before anything is bulked.
	calls := ft.recorded()
	putAt, bulkAt := -1, -1
	for i, call := range calls {
		switch {
		case call.Method == http.MethodPut && call.Path == "/"+IndexName:
			putAt = i
			if !bytes.Contains(call.Body, []byte("nori_tokenizer")) {
				t.Error("recreated index does not carry the nori mapping")
			}
		case call.Path == bulkPath:
			bulkAt = i
		}
	}
	if putAt < 0 {
		t.Fatalf("write after container replacement did not recreate the index: %+v", calls)
	}
	if bulkAt < 0 || bulkAt < putAt {
		t.Fatalf("bulk at %d must follow index creation at %d: %+v", bulkAt, putAt, calls)
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
		if call.Path == bulkPath {
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
		[]episode.Record{testRecord("01JD0000000000000000000001", "x")}); err != nil {
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

func TestDrop(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		wantKind errs.Kind
	}{
		{name: "deleted", status: http.StatusOK},
		{name: "already gone", status: http.StatusNotFound},
		{name: "forbidden", status: http.StatusForbidden, wantKind: errs.KindInternal},
		{name: "cluster down", status: http.StatusBadGateway, wantKind: errs.KindUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			ft := &fakeTransport{handler: func(fakeCall) (int, string) { return tt.status, `{}` }}
			c := newTestClient(t, ft)

			// Act
			err := c.Drop(context.Background())

			// Assert
			if tt.wantKind == "" {
				if err != nil {
					t.Fatalf("Drop() = %v, want nil", err)
				}
			} else {
				assertKind(t, err, tt.wantKind, opDrop)
			}
			calls := ft.recorded()
			if len(calls) != 1 || calls[0].Method != http.MethodDelete || calls[0].Path != "/"+IndexName {
				t.Fatalf("expected one DELETE /%s, got %+v", IndexName, calls)
			}
		})
	}
}

// TestEnsureIndexErrors maps every failure of the schema path onto its semantic
// kind: a cluster we cannot reach is unavailable (degraded mode, §5), anything
// else is our own bug and stays internal.
func TestEnsureIndexErrors(t *testing.T) {
	tests := []struct {
		name         string
		handler      func(call fakeCall) (int, string)
		transportErr error
		wantKind     errs.Kind
	}{
		{
			name:         "container down",
			transportErr: errors.New("dial tcp: connection refused"),
			wantKind:     errs.KindUnavailable,
		},
		{
			name: "cluster 503 on head",
			handler: func(fakeCall) (int, string) {
				return http.StatusServiceUnavailable, `{"error":"unavailable"}`
			},
			wantKind: errs.KindUnavailable,
		},
		{
			name: "unexpected head status",
			handler: func(call fakeCall) (int, string) {
				if call.Method == http.MethodHead {
					return http.StatusForbidden, `{"error":"forbidden"}`
				}
				return http.StatusOK, `{}`
			},
			wantKind: errs.KindInternal,
		},
		{
			name: "create rejected",
			handler: func(call fakeCall) (int, string) {
				if call.Method == http.MethodHead {
					return http.StatusNotFound, `{}`
				}
				return http.StatusBadRequest, `{"error":{"type":"mapper_parsing_exception"}}`
			},
			wantKind: errs.KindInternal,
		},
		{
			name: "settings unreadable",
			handler: func(call fakeCall) (int, string) {
				if call.Method == http.MethodGet {
					return http.StatusForbidden, `{"error":"forbidden"}`
				}
				return http.StatusOK, `{}`
			},
			wantKind: errs.KindInternal,
		},
		{
			name: "settings are not json",
			handler: func(call fakeCall) (int, string) {
				if call.Method == http.MethodGet {
					return http.StatusOK, `not json`
				}
				return http.StatusOK, `{}`
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
			err := c.EnsureIndex(context.Background())

			// Assert
			assertKind(t, err, tt.wantKind, opEnsureIndex)
			if c.schemaReady {
				t.Error("a failed EnsureIndex must not latch the mapping as converged")
			}
		})
	}
}

// TestEnsureIndexRepairDropFailurePropagates keeps the repair honest: if the
// drop half of drop-and-recreate fails, the caller hears about it instead of
// getting a silently unmapped index.
func TestEnsureIndexRepairDropFailurePropagates(t *testing.T) {
	// Arrange
	ft := &fakeTransport{handler: func(call fakeCall) (int, string) {
		switch call.Method {
		case http.MethodGet:
			return http.StatusOK, settingsDynamic
		case http.MethodDelete:
			return http.StatusForbidden, `{"error":"forbidden"}`
		default:
			return http.StatusOK, `{}`
		}
	}}
	c := newTestClient(t, ft)

	// Act
	err := c.EnsureIndex(context.Background())

	// Assert
	assertKind(t, err, errs.KindInternal, opEnsureIndex)
	for _, call := range ft.recorded() {
		if call.Method == http.MethodPut {
			t.Fatal("index was recreated even though the repair drop failed")
		}
	}
}

// TestWriteEnsureFailureReportsWriteOp keeps the op of a lazily converged
// schema tied to the write the caller actually made, so the log names the
// failing operation rather than an ensure the caller never called.
func TestWriteEnsureFailureReportsWriteOp(t *testing.T) {
	// Arrange
	ft := &fakeTransport{err: errors.New("dial tcp: connection refused")}
	c := newTestClient(t, ft)

	// Act
	err := c.IndexRecords(context.Background(), testKey(),
		[]episode.Record{testRecord("01JD0000000000000000000001", "x")})

	// Assert
	assertKind(t, err, errs.KindUnavailable, opIndexRecords)
}

// TestDeleteProject pins the project-scoped delete partial rehydration relies
// on: one _delete_by_query filtered to the project, a missing index treated as
// already-empty, and transport failures classified for degraded mode (§5).
func TestDeleteProject(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		wantKind errs.Kind
	}{
		{name: "deleted", status: http.StatusOK},
		{name: "no index yet", status: http.StatusNotFound},
		{name: "forbidden", status: http.StatusForbidden, wantKind: errs.KindInternal},
		{name: "cluster down", status: http.StatusBadGateway, wantKind: errs.KindUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			ft := &fakeTransport{handler: func(fakeCall) (int, string) { return tt.status, `{"deleted":2}` }}
			c := newReadyTestClient(t, ft)

			// Act
			err := c.DeleteProject(context.Background(), testKey())

			// Assert
			if tt.wantKind == "" {
				if err != nil {
					t.Fatalf("DeleteProject() = %v, want nil", err)
				}
			} else {
				assertKind(t, err, tt.wantKind, opDeleteProject)
			}
			calls := ft.recorded()
			if len(calls) != 1 {
				t.Fatalf("expected exactly one call, got %+v", calls)
			}
			call := calls[0]
			if call.Method != http.MethodPost || call.Path != "/"+IndexName+deleteByQuerySuffix {
				t.Fatalf("got %s %s, want POST /%s%s", call.Method, call.Path, IndexName, deleteByQuerySuffix)
			}
			if call.Query.Get("refresh") != "true" {
				t.Errorf("refresh = %q, want true — the delete must be visible to the bulk replay that follows", call.Query.Get("refresh"))
			}
			if call.Query.Get("conflicts") != "proceed" {
				t.Errorf("conflicts = %q, want proceed", call.Query.Get("conflicts"))
			}
			body := string(call.Body)
			for _, want := range []string{`"workspace":"vms"`, `"team":"core"`, `"project":"memory-mcp"`} {
				if !strings.Contains(body, want) {
					t.Errorf("delete query is not scoped to the project: %s missing from %s", want, body)
				}
			}
		})
	}
}

// A zero-value key produces no filters, so sending it would delete every
// project's documents. It must be refused, not interpreted as "all".
func TestDeleteProjectRejectsZeroKey(t *testing.T) {
	// Arrange
	ft := &fakeTransport{handler: func(fakeCall) (int, string) { return http.StatusOK, `{}` }}
	c := newReadyTestClient(t, ft)

	// Act
	err := c.DeleteProject(context.Background(), projectkey.Key{})

	// Assert
	assertKind(t, err, errs.KindInvalid, opDeleteProject)
	if calls := ft.recorded(); len(calls) != 0 {
		t.Fatalf("a zero key reached the cluster: %+v", calls)
	}
}
