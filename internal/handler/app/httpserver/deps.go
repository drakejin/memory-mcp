// Package httpserver holds the HTTP handlers of the §7 API: validation,
// response envelope, degraded-mode notes and the apierr translation boundary.
// The route table and process lifecycle live in the composition root,
// internal/app/httpserver, which mounts the exported Handle* methods below.
//
// Layering (code-standards §2, feature-inventory §4.1 rule 3): handlers
// depend on the service interfaces alone — episode.Service, knowledge.Service,
// document.Service, consolidate.Service, rehydrate.Service. No storage client
// appears in a handler signature, everything below this layer speaks
// *errs.Error, handlers translate exactly once with apierr.From, and only
// this layer knows about HTTP status codes.
package httpserver

import (
	"log/slog"
	"sync"
	"time"

	"github.com/drakejin/memory-mcp/internal/service/consolidate"
	"github.com/drakejin/memory-mcp/internal/service/document"
	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/service/knowledge"
	"github.com/drakejin/memory-mcp/internal/service/rehydrate"
)

// Clock is the injected time source for the one timestamp this layer still
// owns: the in-process S3 activity times /v1/status reports. Everything else
// (ULIDs, record stamps, TTL arithmetic) moved into the services.
type Clock interface {
	Now() time.Time
}

// Handler carries the service set the §7 handlers call. The composition root
// (internal/app/httpserver) fills the exported fields once at boot and mounts
// the Handle* methods; nothing mutates them afterwards.
//
// Every service field may be nil: a collaborator the process could not build
// degrades the endpoints that need it to an honest 503 (§5) instead of
// preventing boot. Clock and Log are required — they cannot degrade, only
// panic.
type Handler struct {
	// S3Bucket names the cold store in /v1/status.
	S3Bucket string

	// Episodes is the episodic plane orchestration (F1-F3).
	Episodes episode.Service
	// Knowledge is the knowledge plane orchestration (F4-F9).
	Knowledge knowledge.Service
	// Documents is the §6 document pipeline plus the cold-store ops surface
	// /v1/status and ingest gate on (F10-F12).
	Documents document.Service
	// Consolidator runs the deterministic §4 pipeline and answers the §3.1
	// bookkeeping slice of /v1/status.
	Consolidator consolidate.Service
	// Rehydrator answers drift for /v1/status, rebuilds on /v1/reindex, and
	// runs the request-entry stat-gate for the knowledge read paths (F17; the
	// episodic search gates inside episode.Service).
	Rehydrator rehydrate.Service

	Clock Clock
	Log   *slog.Logger

	// statusMu guards lastArchiveAt and lastSnapshotAt: the in-process S3
	// activity timestamps reported by /v1/status, refreshed by successful
	// consolidation runs (§4). Nothing else may read or write them unlocked.
	statusMu       sync.Mutex
	lastArchiveAt  time.Time
	lastSnapshotAt time.Time
}
