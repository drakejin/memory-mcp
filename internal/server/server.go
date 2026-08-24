// Package server exposes the HTTP API (architecture-v2.md §7) over go-chi
// with swaggo annotations. Binding is loopback-only, no auth. Every response
// uses the {success, data, error} envelope.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	httpSwagger "github.com/swaggo/http-swagger/v2"

	"github.com/drakejin/memory-mcp/internal/cold"
	"github.com/drakejin/memory-mcp/internal/config"
	"github.com/drakejin/memory-mcp/internal/consolidate"
	"github.com/drakejin/memory-mcp/internal/document"
	"github.com/drakejin/memory-mcp/internal/graph"
	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/rehydrate"
	"github.com/drakejin/memory-mcp/internal/search"
)

// Deps are the server's collaborators — all interfaces so handler tests use
// fakes. Any field may be nil when the backing service failed to initialize;
// handlers must then answer in degraded mode per §5 (hot writes succeed with a
// degraded note; derived reads return 503).
type Deps struct {
	Store        hotstore.Store
	Index        search.Index
	Graph        graph.Store
	Documents    document.Service
	Consolidator consolidate.Consolidator
	Rehydrator   rehydrate.Rehydrator
	Archiver     cold.Archiver
	Clock        hotstore.Clock
	Logger       *slog.Logger
}

// Server is the HTTP front end.
type Server struct {
	cfg  config.Config
	deps Deps

	// statusMu guards the S3 activity timestamps reported by /v1/status;
	// they are refreshed by successful consolidation runs (§4).
	statusMu       sync.Mutex
	lastArchiveAt  time.Time
	lastSnapshotAt time.Time
}

// New builds a Server. Logger and Clock fall back to defaults when nil.
func New(cfg config.Config, deps Deps) *Server {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Clock == nil {
		deps.Clock = hotstore.SystemClock{}
	}
	return &Server{cfg: cfg, deps: deps}
}

// Router mounts every route of §7. Handler bodies live in handlers_*.go; the
// route table itself is scaffold-owned and must not be reshaped.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()

	r.Get("/healthz", s.handleHealthz)
	r.Get("/swagger/*", httpSwagger.WrapHandler)

	r.Route("/v1", func(r chi.Router) {
		r.Get("/status", s.handleStatus)
		r.Post("/consolidate", s.handleConsolidate)
		r.Post("/reindex", s.handleReindex)

		r.Get("/documents/{sha}", s.handleGetDocument)
		r.Get("/documents/{sha}/chunks", s.handleGetDocumentChunks)

		r.Route("/{ws}/{team}/{proj}", func(r chi.Router) {
			r.Post("/episodes", s.handleCreateEpisode)
			r.Get("/episodes/search", s.handleSearchEpisodes)
			r.Get("/episodes/{id}", s.handleGetEpisode)

			r.Post("/knowledge/nodes", s.handleCreateNode)
			r.Patch("/knowledge/nodes/{id}", s.handlePatchNode)
			r.Delete("/knowledge/nodes/{id}", s.handlePurgeNode)
			r.Post("/knowledge/edges", s.handleCreateEdge)
			r.Get("/knowledge/search", s.handleSearchKnowledge)
			r.Get("/knowledge/graph", s.handleKnowledgeGraph)

			r.Post("/documents", s.handleIngestDocument)
		})
	})
	return r
}

// ListenAndServe runs the server on cfg.ListenAddr until ctx is cancelled,
// then shuts down gracefully. Fully implemented by the scaffold.
func (s *Server) ListenAndServe(ctx context.Context) error {
	httpServer := &http.Server{
		Addr:              s.cfg.ListenAddr,
		Handler:           s.Router(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		s.deps.Logger.Info("memory-mcp listening", "addr", s.cfg.ListenAddr)
		errCh <- httpServer.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// projectKey extracts and returns the {ws}/{team}/{proj} chi params.
func projectKey(r *http.Request) hotstore.ProjectKey {
	return hotstore.ProjectKey{
		Workspace: chi.URLParam(r, "ws"),
		Team:      chi.URLParam(r, "team"),
		Project:   chi.URLParam(r, "proj"),
	}
}

// handleHealthz godoc
//
//	@Summary	Liveness probe
//	@Tags		ops
//	@Produce	json
//	@Success	200	{object}	Envelope
//	@Router		/healthz [get]
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
