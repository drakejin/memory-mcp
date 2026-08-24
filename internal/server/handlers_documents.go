package server

import (
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/server/apierr"
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
// Contract: the §6 pipeline. S3 blob upload is cold-first and must succeed;
// extraction/chunking shortfalls are reported honestly (extractable=false,
// truncated), never guessed around.
func (s *Server) handleIngestDocument(w http.ResponseWriter, r *http.Request) {
	key := projectKey(r)
	if apiErr := validateProjectKey(key); apiErr != nil {
		writeAPIError(w, s.log, apiErr)
		return
	}
	if s.documents == nil {
		writeAPIError(w, s.log, unavailable(msgDocumentsUnavailable))
		return
	}
	if s.archiver == nil {
		// §6 step 2 is cold-first: without S3 the ingest contract cannot hold.
		writeAPIError(w, s.log, unavailable(degradedCold))
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(multipartMemoryBytes); err != nil {
		writeAPIError(w, s.log, badRequest("multipart form unreadable or too large"))
		return
	}
	file, header, err := r.FormFile(formFieldFile)
	if err != nil {
		writeAPIError(w, s.log, badRequest(`multipart field "`+formFieldFile+`" is required`))
		return
	}
	defer file.Close()
	if header.Filename == "" {
		writeAPIError(w, s.log, badRequest("uploaded file must have a filename"))
		return
	}

	res, err := s.documents.Ingest(r.Context(), key, header.Filename, file)
	if err != nil {
		writeAPIError(w, s.log, apierr.From(err))
		return
	}
	writeJSON(w, s.log, http.StatusCreated, res)
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
	sha := chi.URLParam(r, paramSHA)
	if apiErr := validateSHA(sha); apiErr != nil {
		writeAPIError(w, s.log, apiErr)
		return
	}
	if s.documents == nil {
		writeAPIError(w, s.log, unavailable(msgDocumentsUnavailable))
		return
	}
	rc, err := s.documents.Original(r.Context(), sha)
	if err != nil {
		writeAPIError(w, s.log, apierr.From(err))
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		// Headers already sent; the envelope is no longer available.
		s.log.Warn("document stream interrupted", "sha", sha, "error", err)
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
	sha := chi.URLParam(r, paramSHA)
	if apiErr := validateSHA(sha); apiErr != nil {
		writeAPIError(w, s.log, apiErr)
		return
	}
	if s.documents == nil {
		writeAPIError(w, s.log, unavailable(msgDocumentsUnavailable))
		return
	}
	chunks, err := s.documents.Chunks(r.Context(), sha)
	if err != nil {
		writeAPIError(w, s.log, apierr.From(err))
		return
	}
	if chunks == nil {
		chunks = []episodic.Record{}
	}
	writeJSON(w, s.log, http.StatusOK, chunks)
}
