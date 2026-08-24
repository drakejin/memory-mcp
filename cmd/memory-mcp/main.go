// Command memory-mcp is the local personal memory server (architecture-v2.md).
// The server itself runs on the host; only the derived stores (OpenSearch,
// Neo4j) run in Docker. Missing derived services degrade the server, they
// never prevent boot (§5).
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/drakejin/memory-mcp/internal/blob"
	"github.com/drakejin/memory-mcp/internal/cold"
	"github.com/drakejin/memory-mcp/internal/config"
	"github.com/drakejin/memory-mcp/internal/consolidate"
	"github.com/drakejin/memory-mcp/internal/document"
	"github.com/drakejin/memory-mcp/internal/graph"
	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/rehydrate"
	"github.com/drakejin/memory-mcp/internal/search"
	"github.com/drakejin/memory-mcp/internal/server"

	_ "github.com/drakejin/memory-mcp/docs" // swag-generated OpenAPI spec
)

//	@title			memory-mcp v2 API
//	@version		2.0
//	@description	Local personal memory server: episodic (OpenSearch) + knowledge (Neo4j) derived from canonical hot JSON, archived to S3.
//	@host			127.0.0.1:8420
//	@BasePath		/

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config load failed", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	clock := hotstore.SystemClock{}
	store := hotstore.New(cfg.Home, clock)
	cache := blob.New(filepath.Join(cfg.Home, "blobs"))

	deps := server.Deps{
		Store:  store,
		Clock:  clock,
		Logger: logger,
	}

	index, err := search.NewClient(cfg.OpenSearchURL)
	if err != nil {
		logger.Warn("opensearch client init failed; episodic search degraded", "error", err)
	} else {
		deps.Index = index
	}

	gr, err := graph.NewClient(cfg.Neo4jURL, cfg.Neo4jUser, cfg.Neo4jPassword)
	if err != nil {
		logger.Warn("neo4j client init failed; knowledge graph degraded", "error", err)
	} else {
		deps.Graph = gr
		// Release the bolt driver's pooled connections on shutdown. ctx is
		// already cancelled at that point, so close on a fresh context.
		defer func() {
			if err := gr.Close(context.Background()); err != nil {
				logger.Warn("neo4j driver close failed", "error", err)
			}
		}()
	}

	s3, err := cold.NewS3(ctx, cfg.AWSProfile, cfg.S3Region, cfg.S3Bucket)
	if err != nil {
		logger.Warn("s3 init failed; cold archive degraded", "error", err)
	} else {
		deps.Archiver = cold.NewArchiver(s3, cfg.Username)
	}

	deps.Documents = document.NewIngestor(document.Deps{
		Store:     store,
		Cache:     cache,
		Archiver:  deps.Archiver,
		Index:     deps.Index,
		Graph:     deps.Graph,
		Extractor: document.DefaultExtractor{},
		Clock:     clock,
	})
	deps.Consolidator = consolidate.New(store, deps.Index, deps.Archiver, clock, cfg.EpisodicTTLDays)
	deps.Rehydrator = rehydrate.New(store, deps.Index, deps.Graph, clock)

	srv := server.New(cfg, deps)
	// §5: compare the manifest against the derived stores before serving, and
	// rebuild them when they drifted (containers boot empty by design). Never
	// fatal — a dead derived store degrades reads, it must not block hot writes.
	srv.Startup(ctx)

	if err := srv.ListenAndServe(ctx); err != nil {
		logger.Error("server exited", "error", err)
		os.Exit(1)
	}
}
