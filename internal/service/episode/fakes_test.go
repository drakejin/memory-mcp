package episode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/x/errs"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// --- shared fixtures --------------------------------------------------------

var (
	fixedNow = time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC)
	testKey  = projectkey.Key{Workspace: "ws", Team: "team", Project: "proj"}
)

// Valid 26-char Crockford-base32 ULIDs for stored records and lookups.
const (
	ulidA = "01JD00000000000000000000A0"
	ulidB = "01JD00000000000000000000B0"
	ulidC = "01JD00000000000000000000C0"
)

// errBoom is an unclassified collaborator failure: it carries no errs.Kind, so
// it exercises the internal/unclassified row of the §2.2 mapping table.
var errBoom = errors.New("boom")

// errIndexDown is the degraded-mode signal: KindUnavailable is what a derived
// store returns when it cannot be reached.
var errIndexDown = errs.Unavailable("search.Search", errBoom)

// --- call-order journal -----------------------------------------------------

// journal records collaborator calls in invocation order, so ordering
// invariants (hot write before index upsert, gate before search, bump before
// converge) are asserted deterministically instead of inferred.
type journal struct{ events []string }

func (j *journal) add(event string) { j.events = append(j.events, event) }

// --- fakes ------------------------------------------------------------------
//
// Each fake implements exactly one consumer-side port of service.go, so a test
// double never grows methods the service does not call. Errors they hand back
// are *errs.Error where the real collaborator would produce one.

type fakeClock struct{ now time.Time }

func (c fakeClock) Now() time.Time { return c.now }

// fakeIDs mints predictable, well-formed ULIDs so a test can assert on the id
// the service assigned. lastMillis records the timestamp it was handed, which
// is how tests prove ids come from the injected clock, not the wall clock.
type fakeIDs struct {
	seq        int
	err        error
	lastMillis int64
}

var _ IDGenerator = (*fakeIDs)(nil)

func (f *fakeIDs) GenerateAt(unixMillis int64) (string, error) {
	f.lastMillis = unixMillis
	if f.err != nil {
		return "", f.err
	}
	f.seq++
	// 4-char head + 22 digits = the 26 Crockford characters ulid.Valid wants.
	return fmt.Sprintf("01JD%022d", f.seq), nil
}

// fakeHot is an in-memory HotStore port. Error hooks drive failure branches;
// listHook, when set, decides per-call failures for multi-list flows.
type fakeHot struct {
	j        *journal
	episodes map[string][]Record

	appendErr error
	listErr   error
	listHook  func() error
	getErr    error
	updateErr error

	updateCalls int
}

var _ HotStore = (*fakeHot)(nil)

func (s *fakeHot) AppendEpisode(_ context.Context, key projectkey.Key, rec Record) error {
	s.j.add("store.Append")
	if s.appendErr != nil {
		return s.appendErr
	}
	s.episodes[key.String()] = append(s.episodes[key.String()], rec)
	return nil
}

func (s *fakeHot) ListEpisodes(_ context.Context, key projectkey.Key) ([]Record, error) {
	s.j.add("store.List")
	if s.listErr != nil {
		return nil, s.listErr
	}
	if s.listHook != nil {
		if err := s.listHook(); err != nil {
			return nil, err
		}
	}
	return s.episodes[key.String()], nil
}

func (s *fakeHot) GetEpisode(_ context.Context, key projectkey.Key, id string) (Record, error) {
	s.j.add("store.Get")
	if s.getErr != nil {
		return Record{}, s.getErr
	}
	for _, r := range s.episodes[key.String()] {
		if r.ID == id {
			return r, nil
		}
	}
	return Record{}, errs.NotFound("hotstore.GetEpisode", entityEpisode, id)
}

func (s *fakeHot) UpdateEpisodes(_ context.Context, key projectkey.Key, ids []string, fn func(Record) Record) error {
	s.j.add("store.Update")
	s.updateCalls++
	if s.updateErr != nil {
		return s.updateErr
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	recs := s.episodes[key.String()]
	for i, r := range recs {
		if want[r.ID] {
			recs[i] = fn(r)
		}
	}
	return nil
}

// fakeIndex is an in-memory Index port.
type fakeIndex struct {
	j       *journal
	indexed map[string][]Record
	hits    []Hit

	indexErr  error
	searchErr error

	lastQuery Query
}

var _ Index = (*fakeIndex)(nil)

func (f *fakeIndex) IndexRecords(_ context.Context, key projectkey.Key, recs []Record) error {
	f.j.add("index.Index")
	if f.indexErr != nil {
		return f.indexErr
	}
	f.indexed[key.String()] = append(f.indexed[key.String()], recs...)
	return nil
}

func (f *fakeIndex) Search(_ context.Context, _ projectkey.Key, q Query) ([]Hit, error) {
	f.j.add("index.Search")
	f.lastQuery = q
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	return f.hits, nil
}

// fakeArchive is an Archive port double.
type fakeArchive struct {
	j        *journal
	archived map[string]Record
	fetchErr error

	fetchCalls int
}

var _ Archive = (*fakeArchive)(nil)

func (f *fakeArchive) FetchArchivedEpisode(_ context.Context, _ projectkey.Key, id string) (Record, error) {
	f.j.add("archive.Fetch")
	f.fetchCalls++
	if f.fetchErr != nil {
		return Record{}, f.fetchErr
	}
	rec, ok := f.archived[id]
	if !ok {
		return Record{}, errs.NotFound("cold.FetchArchivedEpisode", entityEpisode, id)
	}
	return rec, nil
}

// fakeGate is a Gate port double.
type fakeGate struct {
	j     *journal
	calls []string
	err   error
}

var _ Gate = (*fakeGate)(nil)

func (f *fakeGate) StatGate(_ context.Context, key projectkey.Key) error {
	f.j.add("gate.StatGate")
	f.calls = append(f.calls, key.String())
	return f.err
}

// fakeBooks is a Bookkeeper port double recording which projects were marked.
type fakeBooks struct {
	j       *journal
	dirty   []string
	indexed []string

	dirtyErr   error
	indexedErr error
}

var _ Bookkeeper = (*fakeBooks)(nil)

func (f *fakeBooks) MarkDirty(_ context.Context, key projectkey.Key) error {
	f.j.add("books.MarkDirty")
	if f.dirtyErr != nil {
		return f.dirtyErr
	}
	f.dirty = append(f.dirty, key.String())
	return nil
}

func (f *fakeBooks) MarkIndexed(_ context.Context, key projectkey.Key) error {
	f.j.add("books.MarkIndexed")
	if f.indexedErr != nil {
		return f.indexedErr
	}
	f.indexed = append(f.indexed, key.String())
	return nil
}

// --- harness ----------------------------------------------------------------

// fixture bundles one journal-sharing set of fakes plus the log sink, so a
// test reads every observable side effect from one place.
type fixture struct {
	j       *journal
	store   *fakeHot
	index   *fakeIndex
	archive *fakeArchive
	gate    *fakeGate
	books   *fakeBooks
	ids     *fakeIDs
	logBuf  *bytes.Buffer
}

func newFixture() *fixture {
	j := &journal{}
	return &fixture{
		j:       j,
		store:   &fakeHot{j: j, episodes: map[string][]Record{}},
		index:   &fakeIndex{j: j, indexed: map[string][]Record{}},
		archive: &fakeArchive{j: j, archived: map[string]Record{}},
		gate:    &fakeGate{j: j},
		books:   &fakeBooks{j: j},
		ids:     &fakeIDs{},
		logBuf:  &bytes.Buffer{},
	}
}

// config returns a fully-wired Config; tests mutate it for nil or failing
// collaborators.
func (f *fixture) config() Config {
	return Config{
		Store:      f.store,
		Index:      f.index,
		Archive:    f.archive,
		Gate:       f.gate,
		Bookkeeper: f.books,
		Clock:      fakeClock{now: fixedNow},
		IDs:        f.ids,
		Logger:     slog.New(slog.NewTextHandler(f.logBuf, nil)),
	}
}

// service builds the Service under test, failing the test on a config error.
func (f *fixture) service(t *testing.T, mutate func(*Config)) Service {
	t.Helper()
	cfg := f.config()
	if mutate != nil {
		mutate(&cfg)
	}
	svc, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc
}

// asDomain asserts err carries the package's semantic error type and returns
// the outermost *errs.Error for shape assertions.
func asDomain(t *testing.T, err error) *errs.Error {
	t.Helper()
	var domain *errs.Error
	if !errors.As(err, &domain) {
		t.Fatalf("error %v is not a *errs.Error", err)
	}
	return domain
}
