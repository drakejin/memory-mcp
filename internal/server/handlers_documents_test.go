package server

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/drakejin/memory-mcp/internal/document"
	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/errs"
)

const documentsPath = "/v1/ws/team/proj/documents"

// multipartUpload builds a multipart body with the given field name.
func multipartUpload(t *testing.T, field, filename, content string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile(field, filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := io.WriteString(part, content); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return &buf, w.FormDataContentType()
}

// doUpload posts a multipart form to the router.
func doUpload(t *testing.T, h http.Handler, target, field, filename, content string) *httptest.ResponseRecorder {
	t.Helper()
	body, contentType := multipartUpload(t, field, filename, content)
	req := httptest.NewRequest(http.MethodPost, target, body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestIngestDocument(t *testing.T) {
	docs := &fakeDocuments{result: document.IngestResult{
		SHA: testSHA, BlobKey: "jin/blobs/e3/" + testSHA, NodeID: ulidA,
		Extractable: true, ChunkIDs: []string{ulidB, ulidC},
	}}
	_, h := newTestServer(t, func(d *Config) { d.Documents = docs })

	rec := doUpload(t, h, documentsPath, "file", "spec.pdf", "%PDF-1.4 body")
	assertStatus(t, rec, http.StatusCreated)

	var got document.IngestResult
	decodeEnvelope(t, rec, &got)
	if got.SHA != testSHA || got.NodeID != ulidA || len(got.ChunkIDs) != 2 {
		t.Fatalf("ingest result = %+v", got)
	}
	if docs.lastName != "spec.pdf" {
		t.Errorf("filename = %q, want spec.pdf", docs.lastName)
	}
	if string(docs.lastUpload) != "%PDF-1.4 body" {
		t.Errorf("uploaded bytes = %q, want the posted content", docs.lastUpload)
	}
}

func TestIngestDocumentReportsTruncationHonestly(t *testing.T) {
	docs := &fakeDocuments{result: document.IngestResult{
		SHA: testSHA, Extractable: true,
		Truncated: &document.Truncation{Total: 812, Indexed: 500},
	}}
	_, h := newTestServer(t, func(d *Config) { d.Documents = docs })

	rec := doUpload(t, h, documentsPath, "file", "big.md", "x")
	assertStatus(t, rec, http.StatusCreated)

	var got document.IngestResult
	decodeEnvelope(t, rec, &got)
	if got.Truncated == nil || got.Truncated.Total != 812 || got.Truncated.Indexed != 500 {
		t.Fatalf("truncated = %+v, want {812 500} surfaced to the caller (§6 step 4)", got.Truncated)
	}
}

func TestIngestDocumentUnextractableIsNotAnError(t *testing.T) {
	docs := &fakeDocuments{result: document.IngestResult{SHA: testSHA, Extractable: false, ChunkIDs: []string{}}}
	_, h := newTestServer(t, func(d *Config) { d.Documents = docs })

	rec := doUpload(t, h, documentsPath, "file", "scan.pdf", "no text layer")
	// §6 step 3: honest reporting, not a failure — the agent decides what to do.
	assertStatus(t, rec, http.StatusCreated)
	var got document.IngestResult
	decodeEnvelope(t, rec, &got)
	if got.Extractable {
		t.Fatal("extractable must stay false for a scanned document")
	}
}

func TestIngestDocumentValidation(t *testing.T) {
	tests := []struct {
		name     string
		target   string
		field    string
		filename string
		mutate   func(*Config)
		status   int
	}{
		{"bad project key", "/v1/WS/team/proj/documents", "file", "a.md", nil, http.StatusBadRequest},
		{"wrong field name", documentsPath, "upload", "a.md", nil, http.StatusBadRequest},
		{"empty filename", documentsPath, "file", "", nil, http.StatusBadRequest},
		{
			name: "documents service absent", target: documentsPath, field: "file", filename: "a.md",
			mutate: func(d *Config) { d.Documents = nil }, status: http.StatusServiceUnavailable,
		},
		{
			// §6 step 2 is cold-first: no S3 means the ingest contract cannot hold.
			name: "archiver absent", target: documentsPath, field: "file", filename: "a.md",
			mutate: func(d *Config) { d.Archiver = nil }, status: http.StatusServiceUnavailable,
		},
		{
			name: "ingest fails", target: documentsPath, field: "file", filename: "a.md",
			mutate: func(d *Config) { d.Documents = &fakeDocuments{ingestErr: errBoom} },
			status: http.StatusInternalServerError,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, h := newTestServer(t, tc.mutate)
			rec := doUpload(t, h, tc.target, tc.field, tc.filename, "content")
			assertStatus(t, rec, tc.status)
		})
	}
}

func TestIngestDocumentRejectsNonMultipart(t *testing.T) {
	_, h := newTestServer(t, nil)
	rec := do(t, h, http.MethodPost, documentsPath, `{"file":"nope"}`)
	assertStatus(t, rec, http.StatusBadRequest)
}

func TestGetDocumentStreamsRawBytes(t *testing.T) {
	docs := &fakeDocuments{original: "raw pdf bytes"}
	_, h := newTestServer(t, func(d *Config) { d.Documents = docs })

	rec := do(t, h, http.MethodGet, "/v1/documents/"+testSHA, nil)
	assertStatus(t, rec, http.StatusOK)
	if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("content-type = %q, want application/octet-stream", got)
	}
	if rec.Body.String() != "raw pdf bytes" {
		t.Errorf("body = %q, want the raw bytes (this route bypasses the envelope)", rec.Body.String())
	}
}

func TestGetDocumentFailures(t *testing.T) {
	tests := []struct {
		name   string
		sha    string
		mutate func(*Config)
		status int
	}{
		{"malformed sha", "notasha", nil, http.StatusBadRequest},
		{"uppercase sha", "E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855", nil, http.StatusBadRequest},
		{
			name: "service absent", sha: testSHA,
			mutate: func(d *Config) { d.Documents = nil }, status: http.StatusServiceUnavailable,
		},
		{
			name: "not cached and not in cold", sha: testSHA,
			mutate: func(d *Config) {
				d.Documents = &fakeDocuments{originErr: errs.NotFound("cold.FetchBlob", "blob", testSHA)}
			},
			status: http.StatusNotFound,
		},
		{
			name: "unknown to the hot store", sha: testSHA,
			mutate: func(d *Config) { d.Documents = &fakeDocuments{originErr: errs.NotFound("blob.Open", "blob", testSHA)} },
			status: http.StatusNotFound,
		},
		{
			name: "wrapped not-found still resolves", sha: testSHA,
			mutate: func(d *Config) {
				d.Documents = &fakeDocuments{originErr: fmt.Errorf("s3 get: %w", errs.NotFound("cold.FetchBlob", "blob", testSHA))}
			},
			status: http.StatusNotFound,
		},
		{
			name: "fetch fails", sha: testSHA,
			mutate: func(d *Config) { d.Documents = &fakeDocuments{originErr: errBoom} },
			status: http.StatusInternalServerError,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, h := newTestServer(t, tc.mutate)
			rec := do(t, h, http.MethodGet, "/v1/documents/"+tc.sha, nil)
			assertStatus(t, rec, tc.status)
		})
	}
}

func TestGetDocumentChunks(t *testing.T) {
	docs := &fakeDocuments{chunks: []episodic.Record{
		{ID: ulidA, Kind: episodic.KindDocumentChunk, Refs: &episodic.Refs{DocSHA: testSHA, ChunkSeq: 0}},
		{ID: ulidB, Kind: episodic.KindDocumentChunk, Refs: &episodic.Refs{DocSHA: testSHA, ChunkSeq: 1}},
	}}
	_, h := newTestServer(t, func(d *Config) { d.Documents = docs })

	rec := do(t, h, http.MethodGet, "/v1/documents/"+testSHA+"/chunks", nil)
	assertStatus(t, rec, http.StatusOK)

	var got []episodic.Record
	decodeEnvelope(t, rec, &got)
	if len(got) != 2 || got[0].Refs.ChunkSeq != 0 || got[1].Refs.ChunkSeq != 1 {
		t.Fatalf("chunks = %+v, want two records in chunk_seq order", got)
	}
}

func TestGetDocumentChunksFailures(t *testing.T) {
	tests := []struct {
		name   string
		sha    string
		mutate func(*Config)
		status int
	}{
		{"malformed sha", "zzz", nil, http.StatusBadRequest},
		{
			name: "service absent", sha: testSHA,
			mutate: func(d *Config) { d.Documents = nil }, status: http.StatusServiceUnavailable,
		},
		{
			name: "unknown document", sha: testSHA,
			mutate: func(d *Config) {
				d.Documents = &fakeDocuments{chunksErr: errs.NotFound("document.Chunks", "document", testSHA)}
			},
			status: http.StatusNotFound,
		},
		{
			name: "listing fails", sha: testSHA,
			mutate: func(d *Config) { d.Documents = &fakeDocuments{chunksErr: errBoom} },
			status: http.StatusInternalServerError,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, h := newTestServer(t, tc.mutate)
			rec := do(t, h, http.MethodGet, "/v1/documents/"+tc.sha+"/chunks", nil)
			assertStatus(t, rec, tc.status)
		})
	}
}

func TestGetDocumentChunksEmptyIsArrayNotNull(t *testing.T) {
	_, h := newTestServer(t, nil)
	rec := do(t, h, http.MethodGet, "/v1/documents/"+testSHA+"/chunks", nil)
	assertStatus(t, rec, http.StatusOK)
	env := decodeEnvelope(t, rec, nil)
	if env.Data == nil {
		t.Fatal("data must be [] not null")
	}
}
