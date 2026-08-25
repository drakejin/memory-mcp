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

	"github.com/drakejin/memory-mcp/internal/app/httpserver"
	episodemem "github.com/drakejin/memory-mcp/internal/external/memory/episode"
	knowledgemem "github.com/drakejin/memory-mcp/internal/external/memory/knowledge"
	"github.com/drakejin/memory-mcp/internal/external/persistence/blob"
	"github.com/drakejin/memory-mcp/internal/external/persistence/hotstore"
	"github.com/drakejin/memory-mcp/internal/external/thirdparty/cold"
	"github.com/drakejin/memory-mcp/internal/service/consolidate"
	"github.com/drakejin/memory-mcp/internal/service/document"
	"github.com/drakejin/memory-mcp/internal/service/rehydrate"
	"github.com/drakejin/memory-mcp/internal/x/config"
	"github.com/drakejin/memory-mcp/internal/x/ulid"

	_ "github.com/drakejin/memory-mcp/docs" // swag-generated OpenAPI spec
)

//	@title			memory-mcp v2 API
//	@version		2.0
//	@description	Local personal memory server: episodic (OpenSearch) + knowledge (Neo4j) derived from canonical hot JSON, archived to S3.
//	@host			127.0.0.1:8420
//	@BasePath		/

// blobCacheDir is the local content-addressed cache under DJ_MEMORY_HOME (§1).
const blobCacheDir = "blobs"

// exitFailure is the status returned for a boot or serve failure.
const exitFailure = 1

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	// The composition root owns the process-wide default so any dependency
	// that still falls back to slog.Default writes to the same place. Nothing
	// in this repo reads it: every package takes its logger by injection (§3).
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("memory-mcp exited", "error", err)
		os.Exit(exitFailure)
	}
}

// run loads the configuration and serves until the signal context is
// cancelled. It exists so every deferred close still runs before main calls
// os.Exit.
func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, release, err := build(cfg, logger)
	if err != nil {
		return err
	}
	defer release()

	// §5: compare the manifest against the derived stores before serving, and
	// rebuild them when they drifted (containers boot empty by design). Never
	// fatal — a dead derived store degrades reads, it must not block hot writes.
	srv.Startup(ctx)

	return srv.ListenAndServe(ctx)
}

// build wires every collaborator into a Server. It is separated from run so a
// test can construct the whole object graph without binding a port or waiting
// on a signal: a required dependency missing from one of the Config literals
// below is a boot failure that no package-level unit test can see, so this seam
// is what makes it fail in CI instead of at first launch.
//
// It performs no I/O (§1 rule 4) and returns a release func for the resources
// that own connections. release is non-nil exactly when err is nil: a failing
// build releases what it already opened on the way out, so no caller has to
// unwind a half-built object graph.
func build(cfg config.Config, logger *slog.Logger) (*httpserver.Server, func(), error) {
	clock := hotstore.NewSystemClock()
	// One generator per process: its ids are strictly increasing, so records
	// minted anywhere in the server sort by id alone.
	ids, err := ulid.New(ulid.Config{Clock: clock})
	if err != nil {
		return nil, nil, err
	}
	store, err := hotstore.New(hotstore.Config{Home: cfg.Home, Clock: clock})
	if err != nil {
		return nil, nil, err
	}
	cache, err := blob.New(blob.Config{Dir: filepath.Join(cfg.Home, blobCacheDir)})
	if err != nil {
		return nil, nil, err
	}

	// Every derived store below is optional: failing to build one degrades the
	// corresponding plane and is reported by /v1/status (§5). Each is held in
	// an interface-typed variable that stays nil on failure, so a downstream
	// "is it configured?" check reads the truth.
	var index episodemem.Client
	if client, err := episodemem.New(episodemem.Config{URL: cfg.OpenSearchURL, Logger: logger}); err != nil {
		logger.Warn("opensearch client init failed; episodic search degraded", "error", err)
	} else {
		index = client
	}

	// release is what the caller runs on shutdown. It starts as a no-op so a
	// build that never reached a connection-owning client is still safe to
	// release.
	release := func() {}

	var graphClient knowledgemem.Client
	if client, err := knowledgemem.New(knowledgemem.Config{
		URL:      cfg.Neo4jURL,
		User:     cfg.Neo4jUser,
		Password: cfg.Neo4jPassword,
		Logger:   logger,
	}); err != nil {
		logger.Warn("neo4j client init failed; knowledge graph degraded", "error", err)
	} else {
		graphClient = client
		// Release the bolt driver's pooled connections on shutdown. The serve
		// context is already cancelled at that point, so close on a fresh one.
		release = func() {
			if err := client.Close(context.Background()); err != nil {
				logger.Warn("neo4j driver close failed", "error", err)
			}
		}
	}

	var archiver cold.Client
	if client, err := cold.New(cold.Config{
		Bucket:   cfg.S3Bucket,
		Region:   cfg.S3Region,
		Profile:  cfg.AWSProfile,
		Username: cfg.Username,
	}); err != nil {
		logger.Warn("s3 client init failed; cold archive degraded", "error", err)
	} else {
		archiver = client
	}

	documents, err := document.New(document.Config{
		Store:    store,
		Cache:    cache,
		Clock:    clock,
		IDs:      ids,
		Archiver: archiver,
		Index:    index,
		Graph:    graphClient,
		Bucket:   cfg.S3Bucket,
		Logger:   logger,
	})
	if err != nil {
		release()
		return nil, nil, err
	}
	consolidator, err := consolidate.New(consolidate.Config{
		Store:    store,
		Index:    index,
		Archiver: archiver,
		Clock:    clock,
		Logger:   logger,
		TTLDays:  cfg.EpisodicTTLDays,
	})
	if err != nil {
		release()
		return nil, nil, err
	}
	rehydrator, err := rehydrate.New(rehydrate.Config{
		Store:  store,
		Index:  index,
		Graph:  graphClient,
		Clock:  clock,
		Logger: logger,
	})
	if err != nil {
		release()
		return nil, nil, err
	}

	srv, err := httpserver.New(httpserver.Config{
		ListenAddr:      cfg.ListenAddr,
		S3Bucket:        cfg.S3Bucket,
		EpisodicTTLDays: cfg.EpisodicTTLDays,
		Store:           store,
		Index:           index,
		Graph:           graphClient,
		Documents:       documents,
		Consolidator:    consolidator,
		Rehydrator:      rehydrator,
		Archiver:        archiver,
		Clock:           clock,
		IDs:             ids,
		Logger:          logger,
	})
	if err != nil {
		release()
		return nil, nil, err
	}
	return srv, release, nil
}
