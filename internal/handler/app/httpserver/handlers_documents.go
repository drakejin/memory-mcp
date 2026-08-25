package httpserver

import (
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/drakejin/memory-mcp/internal/handler/app/httpserver/apierr"
	"github.com/drakejin/memory-mcp/internal/service/episode"
)

// HandleIngestDocument godoc
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
func (s *Handler) HandleIngestDocument(w http.ResponseWriter, r *http.Request) {
	key := projectKey(r)
	if apiErr := validateProjectKey(key); apiErr != nil {
		writeAPIError(w, s.Log, apiErr)
		return
	}
	if s.Documents == nil {
		writeAPIError(w, s.Log, unavailable(msgDocumentsUnavailable))
		return
	}
	if !s.Documents.ColdConfigured() {
		// §6 step 2 is cold-first: without S3 the ingest contract cannot hold.
		// Rejected before the body is read, so a large upload is not parsed
		// just to be refused.
		writeAPIError(w, s.Log, unavailable(degradedCold))
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(multipartMemoryBytes); err != nil {
		writeAPIError(w, s.Log, badRequest("multipart form unreadable or too large"))
		return
	}
	file, header, err := r.FormFile(formFieldFile)
	if err != nil {
		writeAPIError(w, s.Log, badRequest(`multipart field "`+formFieldFile+`" is required`))
		return
	}
	defer file.Close()
	if header.Filename == "" {
		writeAPIError(w, s.Log, badRequest("uploaded file must have a filename"))
		return
	}

	res, err := s.Documents.Ingest(r.Context(), key, header.Filename, file)
	if err != nil {
		writeAPIError(w, s.Log, apierr.From(err))
		return
	}
	writeJSON(w, s.Log, http.StatusCreated, res)
}

// HandleGetDocument godoc
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
func (s *Handler) HandleGetDocument(w http.ResponseWriter, r *http.Request) {
	sha := chi.URLParam(r, paramSHA)
	if apiErr := validateSHA(sha); apiErr != nil {
		writeAPIError(w, s.Log, apiErr)
		return
	}
	if s.Documents == nil {
		writeAPIError(w, s.Log, unavailable(msgDocumentsUnavailable))
		return
	}
	rc, err := s.Documents.Original(r.Context(), sha)
	if err != nil {
		writeAPIError(w, s.Log, apierr.From(err))
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		// Headers already sent; the envelope is no longer available.
		s.Log.Warn("document stream interrupted", "sha", sha, "error", err)
	}
}

// HandleGetDocumentChunks godoc
//
//	@Summary	List document_chunk episodes for a document
//	@Tags		documents
//	@Produce	json
//	@Param		sha	path		string	true	"sha256"
//	@Success	200	{object}	Envelope{data=[]episode.Record}
//	@Failure	400	{object}	Envelope
//	@Failure	404	{object}	Envelope
//	@Router		/v1/documents/{sha}/chunks [get]
func (s *Handler) HandleGetDocumentChunks(w http.ResponseWriter, r *http.Request) {
	sha := chi.URLParam(r, paramSHA)
	if apiErr := validateSHA(sha); apiErr != nil {
		writeAPIError(w, s.Log, apiErr)
		return
	}
	if s.Documents == nil {
		writeAPIError(w, s.Log, unavailable(msgDocumentsUnavailable))
		return
	}
	chunks, err := s.Documents.Chunks(r.Context(), sha)
	if err != nil {
		writeAPIError(w, s.Log, apierr.From(err))
		return
	}
	if chunks == nil {
		chunks = []episode.Record{}
	}
	writeJSON(w, s.Log, http.StatusOK, chunks)
}
