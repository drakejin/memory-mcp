package episode

import (
	"context"
	"log/slog"
	"time"

	"github.com/drakejin/memory-mcp/internal/x/errs"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// Ops carried by this package's orchestration errors. They read as a path
// through the system, which is more useful in a log than a stack
// (code-standards §2.1). opValidate in record.go keeps its historical
// "episodic." spelling; new service ops use the package name.
const (
	opNew            = "episode.New"
	opValidateConfig = "episode.Config.Validate"
	opAppend         = "episode.Append"
	opSearch         = "episode.Search"
	opGet            = "episode.Get"
)

// DegradedPromotion reports that a knowledge node was written but its
// provenance episodes could not be marked consolidated, so those records stay
// ineligible for cold aging until the next promotion (§3.1). Exported for the
// same reason as DegradedSearch: it is part of the Degraded data contract.
const DegradedPromotion = "provenance episodes not marked consolidated"

// entityConfig is the entity addressed by construction errors.
const entityConfig = "config"

// DegradedSearch is the §5 degraded-note vocabulary for the episodic index,
// word for word what the HTTP layer has always published: a write that could
// not refresh the derived index carries it in AppendResult.Degraded, and a
// read that cannot use the index carries it as the message of its
// KindUnavailable error, so a client reads one wording either way. Exported
// because it is part of the Degraded data contract, not an internal detail.
const DegradedSearch = "search unavailable"

// planeEpisodic labels the episodic plane in best-effort failure logs. It
// spells the same value as the hot store's manifest plane vocabulary, so log
// lines read unchanged after the handler extraction — without this package
// importing that vocabulary.
const planeEpisodic = "episodic"

// The interfaces below are the consumer-side ports of this service
// (feature-inventory §4.1 rule 2): each names only the calls this package
// makes, so the service never depends on the whole surface of a provider.
// Go's structural typing lets the production clients satisfy them without
// this package importing internal/external/* — hotstore.Client satisfies
// HotStore, cold.Client satisfies Archive, rehydrate.Service satisfies Gate,
// ulid.Client satisfies IDGenerator, and episodemem.Client satisfies Index
// once its Query/Hit spellings are the domain types below.

// Clock is the injected time source. The service never calls time.Now
// directly, so ULID assignment and recall timestamps stay deterministic in
// tests (code-standards §1.1).
type Clock interface {
	Now() time.Time
}

// IDGenerator mints the ULIDs the server assigns to new records. Only the
// timestamped form is consumed: the id's time half must agree with the
// injected Clock, never with the wall clock.
type IDGenerator interface {
	GenerateAt(unixMillis int64) (string, error)
}

// HotStore is the canonical-store surface this service consumes (§0 principle
// 1: every write commits here first, and the recall ranking material lives
// here, P2).
type HotStore interface {
	AppendEpisode(ctx context.Context, key projectkey.Key, rec Record) error
	ListEpisodes(ctx context.Context, key projectkey.Key) ([]Record, error)
	GetEpisode(ctx context.Context, key projectkey.Key, id string) (Record, error)
	UpdateEpisodes(ctx context.Context, key projectkey.Key, ids []string, fn func(Record) Record) error
}

// Index is the derived episodic search plane. Writes to it are best-effort —
// a failure degrades the response, never fails it (P1) — while a read on an
// unreachable index is an honest KindUnavailable (§5).
type Index interface {
	IndexRecords(ctx context.Context, key projectkey.Key, recs []Record) error
	Search(ctx context.Context, key projectkey.Key, q Query) ([]Hit, error)
}

// Archive is the cold-store read side this service needs: resolving a record
// that already aged out of hot, so provenance links keep resolving (P11).
type Archive interface {
	FetchArchivedEpisode(ctx context.Context, key projectkey.Key, id string) (Record, error)
}

// Gate is the request-entry freshness check (§5) run before a search. Its
// failure never blocks the request; rehydration problems surface in /status.
type Gate interface {
	StatGate(ctx context.Context, key projectkey.Key) error
}

// Bookkeeper records episodic-plane index freshness in the hot manifest. It
// is plane-scoped on purpose: the manifest's Plane vocabulary belongs to the
// hot store, which this package must not import, so the composition root
// wires a thin adapter — MarkDirty forwards to the hot store's dirty flag for
// the episodic plane (§1), and MarkIndexed performs the §5 freshness rewrite
// (dirty cleared, IndexedAt refreshed, plane hydration sha updated) after a
// successful derived upsert. Implementations return the failure and leave the
// best-effort logging to this service.
type Bookkeeper interface {
	MarkDirty(ctx context.Context, key projectkey.Key) error
	MarkIndexed(ctx context.Context, key projectkey.Key) error
}

// Service is the episodic orchestration contract the HTTP layer depends on
// (feature-inventory §4.1 rule 3). Results carry Degraded notes as data, not
// error (code-standards §2.2): a hot write that could not refresh a derived
// store still succeeds. Every error crossing this boundary is a *errs.Error
// for the handler to translate exactly once with apierr.From.
type Service interface {
	// Append assigns the ULID and the occurred_at default from the injected
	// clock, commits the record to hot — the only step that may fail the call
	// — then best-effort indexes it: on an absent or failing index the
	// manifest is marked dirty and AppendResult.Degraded reports it (P1).
	Append(ctx context.Context, key projectkey.Key, req AppendRequest) (AppendResult, error)
	// Search runs the freshness gate, queries the index, and bumps
	// recall_count/last_recalled on the hits — hot first, then a best-effort
	// index refresh of those documents (P2). An absent or unreachable index
	// returns KindUnavailable carrying DegradedSearch as its message, the §5
	// material the handler maps to a 503.
	Search(ctx context.Context, key projectkey.Key, q Query) (SearchResult, error)
	// Get reads one record hot-first and falls back to the cold archive, so
	// provenance links keep resolving after aging (P11). A miss everywhere is
	// KindNotFound.
	Get(ctx context.Context, key projectkey.Key, id string) (GetResult, error)
	// Promote closes the §3 consolidation loop after a knowledge node named
	// provenance episodes: the named, still-unconsolidated records of this
	// project are marked consolidated in hot and their derived documents
	// converged. Wholly best-effort — the returned notes (nil, or
	// [DegradedPromotion]) are degraded data for the caller's response, never
	// an error, because the knowledge write already succeeded.
	Promote(ctx context.Context, key projectkey.Key, provenance []string) []string
}

// Config is the single construction input.
type Config struct {
	// Store is the canonical hot store. Required.
	Store HotStore
	// Index is the derived search plane. Nil means the index is unavailable:
	// appends degrade (§5) and searches return KindUnavailable.
	Index Index
	// Archive is the cold-store read side. Nil means a record absent from hot
	// cannot be resolved; Get then reports KindNotFound naming the gap.
	Archive Archive
	// Gate is the request-entry freshness check. Nil skips the gate.
	Gate Gate
	// Bookkeeper tracks episodic index freshness in the manifest. Required.
	Bookkeeper Bookkeeper
	// Clock stamps new records and recall bookkeeping. Required.
	Clock Clock
	// IDs mints record ULIDs from the Clock's time. Required.
	IDs IDGenerator
	// Logger receives best-effort failures. Nil discards them; degradation
	// still reaches the caller through the result values.
	Logger *slog.Logger
}

// Validate reports whether the config can produce a usable service.
func (c Config) Validate() error {
	if c.Store == nil {
		return errs.Invalid(opValidateConfig, entityConfig, "store must be set")
	}
	if c.Bookkeeper == nil {
		return errs.Invalid(opValidateConfig, entityConfig, "bookkeeper must be set")
	}
	if c.Clock == nil {
		return errs.Invalid(opValidateConfig, entityConfig, "clock must be set")
	}
	if c.IDs == nil {
		return errs.Invalid(opValidateConfig, entityConfig, "id generator must be set")
	}
	return nil
}

// service is the concrete Service.
type service struct {
	store   HotStore
	index   Index
	archive Archive
	gate    Gate
	books   Bookkeeper
	clock   Clock
	ids     IDGenerator
	log     *slog.Logger
}

// Compile-time contract check.
var _ Service = (*service)(nil)

// New returns a Service bound to cfg. It performs no I/O.
func New(cfg Config) (Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, errs.Wrap(opNew, err)
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &service{
		store:   cfg.Store,
		index:   cfg.Index,
		archive: cfg.Archive,
		gate:    cfg.Gate,
		books:   cfg.Bookkeeper,
		clock:   cfg.Clock,
		ids:     cfg.IDs,
		log:     log,
	}, nil
}

// markIndexed records a successful best-effort derived upsert for the
// project's episodic plane: dirty cleared, IndexedAt refreshed, hydration sha
// updated, so CheckDrift stays quiet for converged content (§5). Failures are
// logged only — manifest bookkeeping must never fail a request that already
// wrote hot.
func (s *service) markIndexed(ctx context.Context, key projectkey.Key) {
	if err := s.books.MarkIndexed(ctx, key); err != nil {
		s.log.Error("manifest freshness update failed", "project", key.String(), "plane", planeEpisodic, "error", err)
	}
}

// markDirty flags the project's episodic plane whose best-effort derived
// upsert failed, so the next rehydration pass converges it (§1). Log-only on
// failure.
func (s *service) markDirty(ctx context.Context, key projectkey.Key) {
	if err := s.books.MarkDirty(ctx, key); err != nil {
		s.log.Error("manifest dirty mark failed", "project", key.String(), "plane", planeEpisodic, "error", err)
	}
}
