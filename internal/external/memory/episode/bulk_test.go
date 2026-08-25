package episodemem

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/x/errs"
)

// bulkCalls filters the recorded traffic down to _bulk requests. Every write
// path first confirms the index still exists (index.go: a container may have
// been replaced under us), so the raw call log carries that HEAD as well and
// these payload/batching assertions must look past it.
func bulkCalls(ft *fakeTransport) []fakeCall {
	out := make([]fakeCall, 0, len(ft.recorded()))
	for _, call := range ft.recorded() {
		if call.Path == bulkPath {
			out = append(out, call)
		}
	}
	return out
}

func TestIndexRecordsBulkPayload(t *testing.T) {
	// Arrange
	ft := &fakeTransport{handler: func(fakeCall) (int, string) {
		return http.StatusOK, `{"took":1,"errors":false,"items":[]}`
	}}
	c := newReadyTestClient(t, ft)
	recs := []episode.Record{
		testRecord("01JD0000000000000000000001", "보안을 끄고 배포했다"),
		testRecord("01JD0000000000000000000002", "재수화 완료"),
	}

	// Act
	if err := c.IndexRecords(context.Background(), testKey(), recs); err != nil {
		t.Fatalf("IndexRecords() = %v", err)
	}

	// Assert
	calls := bulkCalls(ft)
	if len(calls) != 1 {
		t.Fatalf("got %d bulk calls, want 1: %+v", len(calls), ft.recorded())
	}
	call := calls[0]
	if call.Method != http.MethodPost {
		t.Fatalf("call = %s %s, want POST %s", call.Method, call.Path, bulkPath)
	}
	if call.Query.Get("refresh") != "true" {
		t.Errorf("bulk missing refresh=true (realtime upsert): %v", call.Query)
	}
	lines := decodeNDJSON(t, call.Body)
	if len(lines) != 4 {
		t.Fatalf("got %d ndjson lines, want 4 (2 actions + 2 docs)", len(lines))
	}
	action, ok := lines[0][actionIndex].(map[string]any)
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

func TestDeleteRecordsEmptyIsNoop(t *testing.T) {
	// Arrange
	ft := &fakeTransport{}
	c := newTestClient(t, ft)

	// Act / Assert
	if err := c.DeleteRecords(context.Background(), testKey(), nil); err != nil {
		t.Fatalf("DeleteRecords(nil) = %v", err)
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
	recs := make([]episode.Record, bulkBatchSize+1)
	for i := range recs {
		recs[i] = testRecord(fmt.Sprintf("01JD%022d", i), "채움")
	}

	// Act
	if err := c.IndexRecords(context.Background(), testKey(), recs); err != nil {
		t.Fatalf("IndexRecords() = %v", err)
	}

	// Assert
	if calls := bulkCalls(ft); len(calls) != 2 {
		t.Fatalf("got %d bulk calls, want 2: %+v", len(calls), ft.recorded())
	}
}

func TestDeleteRecordsBatchesLargeSets(t *testing.T) {
	// Arrange
	ft := &fakeTransport{handler: func(fakeCall) (int, string) {
		return http.StatusOK, `{"errors":false,"items":[]}`
	}}
	c := newReadyTestClient(t, ft)
	ids := make([]string, bulkBatchSize+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("01JD%022d", i)
	}

	// Act
	if err := c.DeleteRecords(context.Background(), testKey(), ids); err != nil {
		t.Fatalf("DeleteRecords() = %v", err)
	}

	// Assert
	if calls := bulkCalls(ft); len(calls) != 2 {
		t.Fatalf("got %d bulk calls, want 2: %+v", len(calls), ft.recorded())
	}
}

func TestIndexRecordsErrors(t *testing.T) {
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
			name: "opensearch 503",
			handler: func(fakeCall) (int, string) {
				return http.StatusServiceUnavailable, `{"error":"unavailable"}`
			},
			wantKind: errs.KindUnavailable,
		},
		{
			name: "bulk rejected outright",
			handler: func(fakeCall) (int, string) {
				return http.StatusBadRequest, `{"error":{"type":"illegal_argument_exception"}}`
			},
			wantKind: errs.KindInternal,
		},
		{
			name: "item mapping failure",
			handler: func(fakeCall) (int, string) {
				return http.StatusOK, `{"errors":true,"items":[{"index":{"status":400,"error":{"type":"mapper_parsing_exception"}}}]}`
			},
			wantKind: errs.KindInternal,
		},
		{
			name: "bulk response is not json",
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
			c := newReadyTestClient(t, ft)

			// Act
			err := c.IndexRecords(context.Background(), testKey(),
				[]episode.Record{testRecord("01JD0000000000000000000001", "x")})

			// Assert
			if err == nil {
				t.Fatal("IndexRecords() = nil, want error")
			}
			assertKind(t, err, tt.wantKind, opIndexRecords)
			// Degraded mode hinges on this: only an unreachable cluster may
			// look unavailable to the write path (§5).
			if got := errors.Is(err, errs.ErrUnavailable); got != (tt.wantKind == errs.KindUnavailable) {
				t.Fatalf("errors.Is(err, errs.ErrUnavailable) = %v, want %v (err=%v)", got, !got, err)
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
	calls := bulkCalls(ft)
	if len(calls) != 1 {
		t.Fatalf("expected one POST %s, got %+v", bulkPath, ft.recorded())
	}
	lines := decodeNDJSON(t, calls[0].Body)
	if len(lines) != 2 {
		t.Fatalf("got %d delete actions, want 2", len(lines))
	}
	del, ok := lines[0][actionDelete].(map[string]any)
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

	// Act
	err := c.DeleteRecords(context.Background(), testKey(), []string{"01JD0000000000000000000001"})

	// Assert
	if err == nil {
		t.Fatal("DeleteRecords() = nil, want error on non-404 item failure")
	}
	assertKind(t, err, errs.KindInternal, opDeleteRecords)
}
