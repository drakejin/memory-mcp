package server

import (
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/drakejin/memory-mcp/internal/cold"
	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/hotstore"
)

// handleIngestDocument godoc
//
//	@Summary	Ingest a document (multipart) — blob + chunks + document node
//	@Tags		documents
//	@Accept		multipart/form-data
//	@Produce	json
//	@Param		ws		path		string	true	"workspace"
//	@Param		team	path		string	true	"team"
//	@Param		proj	path		string	true	"project"
//	@Param		file	formData	file	true	"document file"
//	@Success	201		{object}	Envelope{data=document.IngestResult}
//	@Failure	400		{object}	Envelope
//	@Failure	503		{object}	Envelope	"document pipeline unavailable"
//	@Router		/v1/{ws}/{team}/{proj}/documents [post]
//
// Contract: §6 pipeline via document.Service.Ingest. S3 blob upload is
// cold-first and must succeed; extraction/chunking failures are reported
// honestly (extractable=false, truncated), never guessed around.
func (s *Server) handleIngestDocument(w http.ResponseWriter, r *http.Request) {
	key := projectKey(r)
	if err := validateProjectKey(key); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.deps.Documents == nil {
		writeError(w, http.StatusServiceUnavailable, "document pipeline unavailable")
		return
	}
	if s.deps.Archiver == nil {
		// §6 step 2 is cold-first: without S3 the ingest contract cannot hold.
		writeError(w, http.StatusServiceUnavailable, degradedCold)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(multipartMemoryBytes); err != nil {
		writeError(w, http.StatusBadRequest, "multipart form unreadable or too large")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, `multipart field "file" is required`)
		return
	}
	defer file.Close()
	if header.Filename == "" {
		writeError(w, http.StatusBadRequest, "uploaded file must have a filename")
		return
	}

	res, err := s.deps.Documents.Ingest(r.Context(), key, header.Filename, file)
	if err != nil {
		s.deps.Logger.Error("document ingest failed", "project", key.String(), "filename", header.Filename, "error", err)
		writeError(w, http.StatusInternalServerError, "document ingest failed")
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

// handleGetDocument godoc
//
//	@Summary	Download original document bytes by sha256
//	@Tags		documents
//	@Produce	application/octet-stream
//	@Param		sha	path		string	true	"sha256"
//	@Success	200	{file}		binary
//	@Failure	400	{object}	Envelope
//	@Failure	404	{object}	Envelope
//	@Router		/v1/documents/{sha} [get]
//
// Contract: local blob cache first, S3 rehydrate on miss (§6 step 6). This is
// the one endpoint that streams raw bytes instead of the JSON envelope.
func (s *Server) handleGetDocument(w http.ResponseWriter, r *http.Request) {
	sha := chi.URLParam(r, "sha")
	if err := validateSHA(sha); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.deps.Documents == nil {
		writeError(w, http.StatusServiceUnavailable, "document pipeline unavailable")
		return
	}
	rc, err := s.deps.Documents.Original(r.Context(), sha)
	if errors.Is(err, hotstore.ErrNotFound) || errors.Is(err, cold.ErrNotFound) {
		writeError(w, http.StatusNotFound, "document not found")
		return
	}
	if err != nil {
		s.deps.Logger.Error("document fetch failed", "sha", sha, "error", err)
		writeError(w, http.StatusInternalServerError, "document fetch failed")
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		// Headers already sent; log only.
		s.deps.Logger.Warn("document stream interrupted", "sha", sha, "error", err)
	}
}

// handleGetDocumentChunks godoc
//
//	@Summary	List document_chunk episodes for a document
//	@Tags		documents
//	@Produce	json
//	@Param		sha	path		string	true	"sha256"
//	@Success	200	{object}	Envelope{data=[]episodic.Record}
//	@Failure	400	{object}	Envelope
//	@Failure	404	{object}	Envelope
//	@Router		/v1/documents/{sha}/chunks [get]
func (s *Server) handleGetDocumentChunks(w http.ResponseWriter, r *http.Request) {
	sha := chi.URLParam(r, "sha")
	if err := validateSHA(sha); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.deps.Documents == nil {
		writeError(w, http.StatusServiceUnavailable, "document pipeline unavailable")
		return
	}
	chunks, err := s.deps.Documents.Chunks(r.Context(), sha)
	if errors.Is(err, hotstore.ErrNotFound) {
		writeError(w, http.StatusNotFound, "document not found")
		return
	}
	if err != nil {
		s.deps.Logger.Error("document chunk listing failed", "sha", sha, "error", err)
		writeError(w, http.StatusInternalServerError, "document chunk listing failed")
		return
	}
	if chunks == nil {
		chunks = []episodic.Record{}
	}
	writeJSON(w, http.StatusOK, chunks)
}
