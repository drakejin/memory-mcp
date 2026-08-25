// Package httpserver is the composition root of the HTTP API
// (architecture-v2.md §7): it validates the process configuration, wraps the
// storage clients in the domain services, hands the service interfaces to the
// handler set, owns the route table, and runs the listener with graceful
// shutdown. Binding is loopback-only, no auth. Handler bodies live in
// internal/handler/app/httpserver; only that package and this one know about
// HTTP status codes (code-standards §2).
//
// This is the one package allowed to import external adapters, services and
// handlers together (feature-inventory §4.1 rule 1): the plane adapters below
// are the glue that lets the services stay free of the hot store's manifest
// plumbing.
package httpserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	httpSwagger "github.com/swaggo/http-swagger/v2"

	handler "github.com/drakejin/memory-mcp/internal/handler/app/httpserver"
	"github.com/drakejin/memory-mcp/internal/service/consolidate"
	"github.com/drakejin/memory-mcp/internal/service/document"
	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/service/knowledge"
	"github.com/drakejin/memory-mcp/internal/service/rehydrate"
	"github.com/drakejin/memory-mcp/internal/x/errs"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
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

// Clock is the injected time source shared by every service this root builds.
type Clock interface {
	Now() time.Time
}

// IDGenerator mints the ids the server assigns to new episodes and knowledge
// nodes. One generator per process keeps ids strictly increasing.
type IDGenerator interface {
	GenerateAt(unixMillis int64) (string, error)
}

// HotStore is the canonical-store slice this composition root wires into the
// episodic and knowledge services and their plane adapters (code-standards
// §1.1 — the union of what those consumers need, nothing more). The hot store
// client satisfies it structurally.
type HotStore interface {
	AppendEpisode(ctx context.Context, key projectkey.Key, rec episode.Record) error
	ListEpisodes(ctx context.Context, key projectkey.Key) ([]episode.Record, error)
	GetEpisode(ctx context.Context, key projectkey.Key, id string) (episode.Record, error)
	UpdateEpisodes(ctx context.Context, key projectkey.Key, ids []string, fn func(episode.Record) episode.Record) error
	UpdateKnowledge(ctx context.Context, key projectkey.Key, fn func(knowledge.Graph) (knowledge.Graph, error)) error
	MarkDirty(ctx context.Context, key projectkey.Key, plane rehydrate.Plane) error
	UpdateManifest(ctx context.Context, fn func(rehydrate.Manifest) (rehydrate.Manifest, error)) error
}

// Config is everything the server needs: the process settings it reports or
// binds to, plus its collaborators. It is the single constructor argument
// (code-standards §1: one New, extra knobs become fields).
//
// The collaborators are split in two groups:
//
//   - Store, Index, Graph, Archiver, Documents, Consolidator and Rehydrator
//     may be nil. A collaborator that failed to initialise must not stop the
//     server from booting, so handlers answer such a request in degraded mode
//     (§5): hot writes still succeed and report a degraded note, derived reads
//     answer 503. New builds the episodic and knowledge services only when
//     Store is present — they cannot exist without the canonical store.
//   - Clock, IDs and Logger are required. They are not services that can
//     degrade — a nil one would panic on the first request, and library code
//     must not panic (§3).
type Config struct {
	// ListenAddr is the loopback bind address (§7).
	ListenAddr string
	// S3Bucket names the cold store in /v1/status.
	S3Bucket string
	// EpisodicTTLDays is the age at which a consolidated episode may sink to
	// cold; /v1/status reports stale unconsolidated records against it (§3.1).
	EpisodicTTLDays int

	// Store is the canonical hot store client.
	Store HotStore
	// Index is the derived episodic search plane (the OpenSearch client).
	Index episode.Index
	// Graph is the derived knowledge mirror (the Neo4j client).
	Graph knowledge.GraphIndex
	// Archiver is the cold-store read side the episodic service falls back to.
	Archiver episode.Archive

	// Documents, Consolidator and Rehydrator arrive as ready services: their
	// pipelines own further storage clients (blob cache, full cold client)
	// this root has no other use for.
	Documents    document.Service
	Consolidator consolidate.Service
	Rehydrator   rehydrate.Service

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

	handlers *handler.Handler

	rehydrator rehydrate.Service
	log        *slog.Logger
}

// New builds a Server from cfg: it wires the plane adapters, constructs the
// episodic and knowledge services over them, and fills the handler set with
// service interfaces only (feature-inventory §4.1 rule 3). It performs no
// I/O; a derived store is verified lazily by Startup and by /v1/status.
func New(cfg Config) (*Server, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	var episodes episode.Service
	var knowledgeSvc knowledge.Service
	if cfg.Store != nil {
		var err error
		episodes, err = episode.New(episode.Config{
			Store:      cfg.Store,
			Index:      cfg.Index,
			Archive:    cfg.Archiver,
			Gate:       cfg.Rehydrator,
			Bookkeeper: planeBookkeeper{store: cfg.Store, clock: cfg.Clock, plane: rehydrate.PlaneEpisodic},
			Clock:      cfg.Clock,
			IDs:        cfg.IDs,
			Logger:     cfg.Logger,
		})
		if err != nil {
			return nil, err
		}
		knowledgeSvc, err = knowledge.New(knowledge.Config{
			Store: knowledgeStore{
				store: cfg.Store,
				books: planeBookkeeper{store: cfg.Store, clock: cfg.Clock, plane: rehydrate.PlaneKnowledge},
			},
			Graph:  cfg.Graph,
			IDs:    cfg.IDs,
			Clock:  cfg.Clock,
			Logger: cfg.Logger,
		})
		if err != nil {
			return nil, err
		}
	}

	return &Server{
		listenAddr: cfg.ListenAddr,
		handlers: &handler.Handler{
			S3Bucket:     cfg.S3Bucket,
			Episodes:     episodes,
			Knowledge:    knowledgeSvc,
			Documents:    cfg.Documents,
			Consolidator: cfg.Consolidator,
			Rehydrator:   cfg.Rehydrator,
			Clock:        cfg.Clock,
			Log:          cfg.Logger,
		},
		rehydrator: cfg.Rehydrator,
		log:        cfg.Logger,
	}, nil
}

// Router mounts every route of §7. Handler bodies live in
// internal/handler/app/httpserver; the route table itself is the API surface
// and must not be reshaped. There is no middleware by design — observability is
// the slog calls inside the handlers.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()

	r.Get("/healthz", s.handlers.HandleHealthz)
	r.Get("/swagger/*", httpSwagger.WrapHandler)

	r.Route("/v1", func(r chi.Router) {
		r.Get("/status", s.handlers.HandleStatus)
		r.Post("/consolidate", s.handlers.HandleConsolidate)
		r.Post("/reindex", s.handlers.HandleReindex)

		r.Get("/documents/{sha}", s.handlers.HandleGetDocument)
		r.Get("/documents/{sha}/chunks", s.handlers.HandleGetDocumentChunks)

		r.Route("/{ws}/{team}/{proj}", func(r chi.Router) {
			r.Post("/episodes", s.handlers.HandleCreateEpisode)
			r.Get("/episodes/search", s.handlers.HandleSearchEpisodes)
			r.Get("/episodes/{id}", s.handlers.HandleGetEpisode)

			r.Post("/knowledge/nodes", s.handlers.HandleCreateNode)
			r.Patch("/knowledge/nodes/{id}", s.handlers.HandlePatchNode)
			r.Delete("/knowledge/nodes/{id}", s.handlers.HandlePurgeNode)
			r.Post("/knowledge/edges", s.handlers.HandleCreateEdge)
			r.Get("/knowledge/search", s.handlers.HandleSearchKnowledge)
			r.Get("/knowledge/graph", s.handlers.HandleKnowledgeGraph)

			r.Post("/documents", s.handlers.HandleIngestDocument)
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
