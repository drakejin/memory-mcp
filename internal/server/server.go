// Package server exposes the HTTP API (architecture-v2.md §7) over go-chi with
// swaggo annotations. Binding is loopback-only, no auth. Every response uses the
// {success, data, error} envelope.
//
// Layering (code-standards §2): everything below a handler speaks *errs.Error,
// handlers translate exactly once with apierr.From, and only this package knows
// about HTTP status codes.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	httpSwagger "github.com/swaggo/http-swagger/v2"

	"github.com/drakejin/memory-mcp/internal/errs"
	"github.com/drakejin/memory-mcp/internal/hotstore"
)

// Server lifecycle limits.
const (
	// readHeaderTimeout bounds slow-header clients. There is deliberately no
	// per-request timeout: a large document ingest legitimately runs for
	// seconds (§6).
	readHeaderTimeout = 5 * time.Second
	// shutdownGrace is how long in-flight requests get after ctx is cancelled.
	shutdownGrace = 10 * time.Second
)

// Loopback prefixes accepted by New. §7 makes "no auth" safe by making "not
// reachable from off-host" structurally true, so the server refuses to be
// constructed with an address that would expose it.
const (
	loopbackIPPrefix   = "127.0.0.1:"
	loopbackHostPrefix = "localhost:"
)

// Operation names carried by the errors New returns.
const (
	opNew      = "server.New"
	entityConf = "config"
)

// Config is everything the server needs: the process settings it reports or
// binds to, plus its collaborators. It is the single constructor argument
// (code-standards §1: one New, extra knobs become fields).
//
// The collaborators are split in two groups:
//
//   - Store, Index, Graph, Documents, Consolidator, Rehydrator and Archiver may
//     be nil. A derived store that failed to initialise must not stop the
//     server from booting, so handlers answer such a request in degraded mode
//     (§5): hot writes still succeed and report a degraded note, derived reads
//     answer 503.
//   - Clock, IDs and Logger are required. They are not services that can
//     degrade — a nil one would panic on the first request, and library code
//     must not panic (§3).
type Config struct {
	// ListenAddr is the loopback bind address (§7).
	ListenAddr string
	// S3Bucket names the cold store in /v1/status.
	S3Bucket string
	// EpisodicTTLDays is the age at which a consolidated episode may sink to
	// cold; /v1/status uses it to count stale unconsolidated records (§3.1).
	EpisodicTTLDays int

	Store        HotStore
	Index        EpisodeIndex
	Graph        KnowledgeGraph
	Documents    DocumentIngestor
	Consolidator Consolidator
	Rehydrator   Rehydrator
	Archiver     ColdArchive

	Clock  Clock
	IDs    IDGenerator
	Logger *slog.Logger
}

// validate rejects a configuration the server cannot honestly serve. It does no
// I/O (§1 rule 4).
func (c Config) validate() error {
	if !strings.HasPrefix(c.ListenAddr, loopbackIPPrefix) && !strings.HasPrefix(c.ListenAddr, loopbackHostPrefix) {
		return errs.Invalid(opNew, entityConf, "listen addr must bind loopback only").
			WithField("listen_addr", c.ListenAddr)
	}
	if c.EpisodicTTLDays <= 0 {
		return errs.Invalid(opNew, entityConf, "episodic ttl days must be positive").
			WithField("episodic_ttl_days", c.EpisodicTTLDays)
	}
	if c.Clock == nil {
		return errs.Invalid(opNew, entityConf, "clock is required")
	}
	if c.IDs == nil {
		return errs.Invalid(opNew, entityConf, "id generator is required")
	}
	if c.Logger == nil {
		return errs.Invalid(opNew, entityConf, "logger is required")
	}
	return nil
}

// Server is the HTTP front end. It stays a concrete type rather than an
// interface/impl pair: it reaches no external system of its own — its
// collaborators do, and those are the interfaces — and nothing ever fakes it.
type Server struct {
	listenAddr string
	s3Bucket   string
	ttlDays    int

	store        HotStore
	index        EpisodeIndex
	graph        KnowledgeGraph
	documents    DocumentIngestor
	consolidator Consolidator
	rehydrator   Rehydrator
	archiver     ColdArchive

	clock Clock
	ids   IDGenerator
	log   *slog.Logger

	// statusMu guards lastArchiveAt and lastSnapshotAt: the in-process S3
	// activity timestamps reported by /v1/status, refreshed by successful
	// consolidation runs (§4). Nothing else may read or write them unlocked.
	statusMu       sync.Mutex
	lastArchiveAt  time.Time
	lastSnapshotAt time.Time
}

// New builds a Server from cfg. It performs no I/O; a derived store is verified
// lazily by Startup and by /v1/status.
func New(cfg Config) (*Server, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Server{
		listenAddr:   cfg.ListenAddr,
		s3Bucket:     cfg.S3Bucket,
		ttlDays:      cfg.EpisodicTTLDays,
		store:        cfg.Store,
		index:        cfg.Index,
		graph:        cfg.Graph,
		documents:    cfg.Documents,
		consolidator: cfg.Consolidator,
		rehydrator:   cfg.Rehydrator,
		archiver:     cfg.Archiver,
		clock:        cfg.Clock,
		ids:          cfg.IDs,
		log:          cfg.Logger,
	}, nil
}

// Router mounts every route of §7. Handler bodies live in handlers_*.go; the
// route table itself is the API surface and must not be reshaped. There is no
// middleware by design — observability is the slog calls inside the handlers.
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

// ListenAndServe runs the server on the configured address until ctx is
// cancelled, then shuts down gracefully.
func (s *Server) ListenAndServe(ctx context.Context) error {
	httpServer := &http.Server{
		Addr:              s.listenAddr,
		Handler:           s.Router(),
		ReadHeaderTimeout: readHeaderTimeout,
	}
	errCh := make(chan error, 1)
	go func() {
		s.log.Info("memory-mcp listening", "addr", s.listenAddr)
		errCh <- httpServer.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// projectKey extracts the {ws}/{team}/{proj} chi params. Contents are checked by
// validateProjectKey.
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
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.log, http.StatusOK, map[string]string{"status": "ok"})
}
