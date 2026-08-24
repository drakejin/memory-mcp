package server

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/rehydrate"
	"github.com/drakejin/memory-mcp/internal/search"
	"github.com/drakejin/memory-mcp/internal/ulid"
)

const episodesPath = "/v1/ws/team/proj/episodes"

func TestCreateEpisodeValidation(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		body   any
		status int
	}{
		{
			name:   "valid event",
			path:   episodesPath,
			body:   CreateEpisodeRequest{Kind: episodic.KindEvent, Actor: episodic.ActorAgent, Text: "보안을 끄고 재기동"},
			status: http.StatusCreated,
		},
		{
			name:   "unknown kind",
			path:   episodesPath,
			body:   CreateEpisodeRequest{Kind: "rumor", Actor: episodic.ActorAgent, Text: "t"},
			status: http.StatusBadRequest,
		},
		{
			name:   "unknown actor",
			path:   episodesPath,
			body:   CreateEpisodeRequest{Kind: episodic.KindEvent, Actor: "ghost", Text: "t"},
			status: http.StatusBadRequest,
		},
		{
			name:   "empty text",
			path:   episodesPath,
			body:   CreateEpisodeRequest{Kind: episodic.KindEvent, Actor: episodic.ActorAgent},
			status: http.StatusBadRequest,
		},
		{
			name:   "document_chunk without refs",
			path:   episodesPath,
			body:   CreateEpisodeRequest{Kind: episodic.KindDocumentChunk, Actor: episodic.ActorSystem, Text: "t"},
			status: http.StatusBadRequest,
		},
		{
			name: "document_chunk with bad sha",
			path: episodesPath,
			body: CreateEpisodeRequest{
				Kind: episodic.KindDocumentChunk, Actor: episodic.ActorSystem, Text: "t",
				Refs: &episodic.Refs{DocSHA: "NOTASHA", ChunkSeq: 0},
			},
			status: http.StatusBadRequest,
		},
		{
			name: "document_chunk with negative seq",
			path: episodesPath,
			body: CreateEpisodeRequest{
				Kind: episodic.KindDocumentChunk, Actor: episodic.ActorSystem, Text: "t",
				Refs: &episodic.Refs{DocSHA: testSHA, ChunkSeq: -1},
			},
			status: http.StatusBadRequest,
		},
		{
			name: "document_chunk valid",
			path: episodesPath,
			body: CreateEpisodeRequest{
				Kind: episodic.KindDocumentChunk, Actor: episodic.ActorSystem, Text: "t",
				Refs: &episodic.Refs{DocSHA: testSHA, ChunkSeq: 3},
			},
			status: http.StatusCreated,
		},
		{
			name: "refs on non-chunk kind",
			path: episodesPath,
			body: CreateEpisodeRequest{
				Kind: episodic.KindEvent, Actor: episodic.ActorAgent, Text: "t",
				Refs: &episodic.Refs{DocSHA: testSHA},
			},
			status: http.StatusBadRequest,
		},
		{
			name:   "uppercase path segment",
			path:   "/v1/WS/team/proj/episodes",
			body:   CreateEpisodeRequest{Kind: episodic.KindEvent, Actor: episodic.ActorAgent, Text: "t"},
			status: http.StatusBadRequest,
		},
		{
			name:   "dot-path traversal segment",
			path:   "/v1/ws/../proj/episodes",
			body:   CreateEpisodeRequest{Kind: episodic.KindEvent, Actor: episodic.ActorAgent, Text: "t"},
			status: http.StatusBadRequest,
		},
		{
			name:   "empty body",
			path:   episodesPath,
			body:   "",
			status: http.StatusBadRequest,
		},
		{
			name:   "malformed json",
			path:   episodesPath,
			body:   "{not json",
			status: http.StatusBadRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, h := newTestServer(t, nil)
			rec := do(t, h, http.MethodPost, tc.path, tc.body)
			assertStatus(t, rec, tc.status)
			env := decodeEnvelope(t, rec, nil)
			if tc.status == http.StatusCreated && !env.Success {
				t.Fatalf("success envelope expected, got %+v", env)
			}
			if tc.status != http.StatusCreated && env.Error == "" {
				t.Fatal("failure envelope must carry an error message")
			}
		})
	}
}

func TestCreateEpisodeAssignsServerControlledFields(t *testing.T) {
	store := newFakeStore()
	_, h := newTestServer(t, func(d *Deps) { d.Store = store })

	// consolidated must never be settable by the caller, and the id/occurred_at
	// defaults come from the server clock.
	rec := do(t, h, http.MethodPost, episodesPath, map[string]any{
		"kind": "decision", "actor": "user", "text": "결정", "consolidated": true,
		"entities": []string{" memory-mcp ", "", "opensearch"},
	})
	assertStatus(t, rec, http.StatusCreated)

	var got CreateEpisodeResponse
	decodeEnvelope(t, rec, &got)
	if got.Record.Consolidated {
		t.Error("consolidated must always start false (§3: server never auto-sets it)")
	}
	if !ulid.IsULID(got.Record.ID) {
		t.Errorf("id = %q, want a ULID", got.Record.ID)
	}
	if !got.Record.OccurredAt.Equal(fixedNow) {
		t.Errorf("occurred_at = %v, want clock now %v", got.Record.OccurredAt, fixedNow)
	}
	if len(got.Record.Entities) != 2 || got.Record.Entities[0] != "memory-mcp" {
		t.Errorf("entities = %v, want trimmed non-empty entries", got.Record.Entities)
	}
	if len(store.episodes[testKey.String()]) != 1 {
		t.Fatalf("hot store holds %d records, want 1", len(store.episodes[testKey.String()]))
	}
}

func TestCreateEpisodeExplicitOccurredAtIsPreserved(t *testing.T) {
	_, h := newTestServer(t, nil)
	when := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	rec := do(t, h, http.MethodPost, episodesPath, CreateEpisodeRequest{
		Kind: episodic.KindObservation, Actor: episodic.ActorSystem, Text: "t", OccurredAt: when,
	})
	assertStatus(t, rec, http.StatusCreated)
	var got CreateEpisodeResponse
	decodeEnvelope(t, rec, &got)
	if !got.Record.OccurredAt.Equal(when) {
		t.Fatalf("occurred_at = %v, want %v", got.Record.OccurredAt, when)
	}
}

func TestCreateEpisodeDegradedWhenIndexDown(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*Deps)
		wantNote string
	}{
		{
			name:     "index not configured",
			mutate:   func(d *Deps) { d.Index = nil },
			wantNote: degradedSearch,
		},
		{
			name: "index upsert fails",
			mutate: func(d *Deps) {
				idx := newFakeIndex()
				idx.indexErr = search.ErrUnavailable
				d.Index = idx
			},
			wantNote: degradedSearch,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			_, h := newTestServer(t, func(d *Deps) {
				d.Store = store
				tc.mutate(d)
			})

			rec := do(t, h, http.MethodPost, episodesPath, CreateEpisodeRequest{
				Kind: episodic.KindEvent, Actor: episodic.ActorAgent, Text: "hot write must survive",
			})

			// §5: the hot write succeeds; degradation is reported, not 503'd.
			assertStatus(t, rec, http.StatusCreated)
			var got CreateEpisodeResponse
			decodeEnvelope(t, rec, &got)
			if len(got.Degraded) != 1 || got.Degraded[0] != tc.wantNote {
				t.Fatalf("degraded = %v, want [%s]", got.Degraded, tc.wantNote)
			}
			if len(store.episodes[testKey.String()]) != 1 {
				t.Fatal("hot store must hold the record even when the index is down")
			}
			wantDirty := rehydrate.FileKey(hotstore.PlaneEpisodic, testKey)
			if len(store.dirtyMarks) != 1 || store.dirtyMarks[0] != wantDirty {
				t.Fatalf("dirty marks = %v, want [%s]", store.dirtyMarks, wantDirty)
			}
		})
	}
}

func TestCreateEpisodeIndexSuccessClearsDirty(t *testing.T) {
	store := newFakeStore()
	fk := rehydrate.FileKey(hotstore.PlaneEpisodic, testKey)
	store.manifest.Files[fk] = hotstore.FileState{SHA256: "abc", RecordCount: 1, Dirty: true}
	index := newFakeIndex()
	_, h := newTestServer(t, func(d *Deps) {
		d.Store = store
		d.Index = index
	})

	rec := do(t, h, http.MethodPost, episodesPath, CreateEpisodeRequest{
		Kind: episodic.KindEvent, Actor: episodic.ActorAgent, Text: "t",
	})
	assertStatus(t, rec, http.StatusCreated)

	if store.manifest.Files[fk].Dirty {
		t.Error("successful index upsert must clear the dirty flag")
	}
	if !store.manifest.Files[fk].IndexedAt.Equal(fixedNow) {
		t.Errorf("indexed_at = %v, want %v", store.manifest.Files[fk].IndexedAt, fixedNow)
	}
	if got := store.manifest.Indexes[rehydrate.IndexKeyEpisodic].LastHydratedSHA; got == "" {
		t.Error("plane hydration sha must be recorded after a successful upsert")
	}
	if len(index.indexed[testKey.String()]) != 1 {
		t.Fatalf("index holds %d records, want 1", len(index.indexed[testKey.String()]))
	}
}

func TestCreateEpisodeHotWriteFailureIs500(t *testing.T) {
	store := newFakeStore()
	store.appendErr = errBoom
	_, h := newTestServer(t, func(d *Deps) { d.Store = store })

	rec := do(t, h, http.MethodPost, episodesPath, CreateEpisodeRequest{
		Kind: episodic.KindEvent, Actor: episodic.ActorAgent, Text: "t",
	})
	assertStatus(t, rec, http.StatusInternalServerError)
	env := decodeEnvelope(t, rec, nil)
	if env.Success {
		t.Fatal("hot write failure must not report success")
	}
}

func TestCreateEpisodeWithoutHotStoreIs503(t *testing.T) {
	_, h := newTestServer(t, func(d *Deps) { d.Store = nil })
	rec := do(t, h, http.MethodPost, episodesPath, CreateEpisodeRequest{
		Kind: episodic.KindEvent, Actor: episodic.ActorAgent, Text: "t",
	})
	assertStatus(t, rec, http.StatusServiceUnavailable)
}

func TestSearchEpisodesQueryValidation(t *testing.T) {
	tests := []struct {
		name   string
		target string
		status int
	}{
		{"missing q", "/v1/ws/team/proj/episodes/search", http.StatusBadRequest},
		{"valid q", "/v1/ws/team/proj/episodes/search?q=%EB%B3%B4%EC%95%88", http.StatusOK},
		{"bad from", "/v1/ws/team/proj/episodes/search?q=a&from=yesterday", http.StatusBadRequest},
		{"bad to", "/v1/ws/team/proj/episodes/search?q=a&to=tomorrow", http.StatusBadRequest},
		{"good range", "/v1/ws/team/proj/episodes/search?q=a&from=2026-01-01T00:00:00Z&to=2026-12-31T00:00:00Z", http.StatusOK},
		{"unknown kind", "/v1/ws/team/proj/episodes/search?q=a&kinds=event,rumor", http.StatusBadRequest},
		{"known kinds", "/v1/ws/team/proj/episodes/search?q=a&kinds=event,decision", http.StatusOK},
		{"bad project key", "/v1/WS/team/proj/episodes/search?q=a", http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, h := newTestServer(t, nil)
			rec := do(t, h, http.MethodGet, tc.target, nil)
			assertStatus(t, rec, tc.status)
		})
	}
}

func TestSearchEpisodesReturns503WhenIndexUnavailable(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Deps)
	}{
		{"index not configured", func(d *Deps) { d.Index = nil }},
		{"index unreachable", func(d *Deps) {
			idx := newFakeIndex()
			idx.searchErr = search.ErrUnavailable
			d.Index = idx
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, h := newTestServer(t, tc.mutate)
			rec := do(t, h, http.MethodGet, "/v1/ws/team/proj/episodes/search?q=a", nil)
			// §5: reads on a dead derived store are honest 503s, not empty 200s.
			assertStatus(t, rec, http.StatusServiceUnavailable)
			env := decodeEnvelope(t, rec, nil)
			if env.Error != degradedSearch {
				t.Fatalf("error = %q, want %q", env.Error, degradedSearch)
			}
		})
	}
}

func TestSearchEpisodesRunsStatGateAndBumpsRecall(t *testing.T) {
	store := newFakeStore()
	store.episodes[testKey.String()] = []episodic.Record{{
		ID: ulidA, Kind: episodic.KindEvent, Actor: episodic.ActorAgent, Text: "보안", OccurredAt: fixedNow,
	}}
	index := newFakeIndex()
	index.hits = []search.Hit{{
		Record:  episodic.Record{ID: ulidA, Kind: episodic.KindEvent},
		Score:   1.5,
		Excerpt: "…<em>보안</em>…",
	}}
	reh := &fakeRehydrator{}
	_, h := newTestServer(t, func(d *Deps) {
		d.Store = store
		d.Index = index
		d.Rehydrator = reh
	})

	rec := do(t, h, http.MethodGet, "/v1/ws/team/proj/episodes/search?q=a", nil)
	assertStatus(t, rec, http.StatusOK)

	var hits []search.Hit
	decodeEnvelope(t, rec, &hits)
	if len(hits) != 1 || hits[0].Excerpt == "" {
		t.Fatalf("hits = %+v, want one excerpt-bearing hit", hits)
	}
	if len(reh.gateCalls) != 1 || reh.gateCalls[0] != testKey.String() {
		t.Fatalf("stat-gate calls = %v, want [%s]", reh.gateCalls, testKey.String())
	}
	got := store.episodes[testKey.String()][0]
	if got.RecallCount != 1 || got.LastRecalled != fixedNow.Format(time.RFC3339) {
		t.Fatalf("recall bookkeeping = (%d, %q), want (1, %q)", got.RecallCount, got.LastRecalled, fixedNow.Format(time.RFC3339))
	}
}

// A recall bump rewrites the hot file, so recall_count/last_recalled — both
// mapped, indexed fields — go stale in OpenSearch and the plane hydration sha
// moves. Left unconverged, CheckDrift reports "episodic hot content changed
// since last hydration" forever: the reindex that would clear it is re-dirtied
// by the very next search (§5). Each case asserts the loop is closed.
func TestSearchEpisodesConvergesIndexAfterRecallBump(t *testing.T) {
	tests := []struct {
		name string
		// setup injects the failure under test; nil means the happy path.
		setup func(*fakeStore, *fakeIndex)
		// dropIndex models OpenSearch being absent entirely.
		dropIndex bool
		// wantIndexedIDs are the ids re-indexed by the converge step.
		wantIndexedIDs []string
		wantDirty      bool
		// wantHydrated requires the manifest hydration sha to match the post-bump
		// plane state, i.e. CheckDrift would stay quiet.
		wantHydrated bool
	}{
		{
			name:           "happy path re-indexes the bumped record and refreshes hydration sha",
			wantIndexedIDs: []string{ulidA},
			wantHydrated:   true,
		},
		{
			name:      "index upsert failure marks the file dirty for the next rehydration",
			setup:     func(_ *fakeStore, i *fakeIndex) { i.indexErr = errBoom },
			wantDirty: true,
		},
		{
			name:           "hot re-read failure marks dirty rather than indexing stale copies",
			setup:          func(s *fakeStore, _ *fakeIndex) { s.listEpisErr = errBoom },
			wantDirty:      true,
			wantIndexedIDs: nil,
		},
		{
			name:      "recall bump failure leaves hot unchanged so nothing needs converging",
			setup:     func(s *fakeStore, _ *fakeIndex) { s.updateEpiErr = errBoom },
			wantDirty: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			store.episodes[testKey.String()] = []episodic.Record{{
				ID: ulidA, Kind: episodic.KindEvent, Actor: episodic.ActorAgent, Text: "보안", OccurredAt: fixedNow,
			}}
			store.touchEpisodic(testKey)
			index := newFakeIndex()
			index.hits = []search.Hit{{Record: episodic.Record{ID: ulidA}, Score: 1.5, Excerpt: "…보안…"}}
			if tc.setup != nil {
				tc.setup(store, index)
			}
			// Index-only records written before the search are noise; reset so
			// indexed[] holds exactly what converging produced.
			index.indexed = map[string][]episodic.Record{}

			_, h := newTestServer(t, func(d *Deps) {
				d.Store = store
				d.Index = index
			})
			rec := do(t, h, http.MethodGet, "/v1/ws/team/proj/episodes/search?q=a", nil)
			assertStatus(t, rec, http.StatusOK)

			var gotIDs []string
			for _, r := range index.indexed[testKey.String()] {
				gotIDs = append(gotIDs, r.ID)
				if r.RecallCount != 1 {
					t.Errorf("re-indexed record %s has recall_count %d, want the bumped value 1", r.ID, r.RecallCount)
				}
			}
			if !slices.Equal(gotIDs, tc.wantIndexedIDs) {
				t.Errorf("re-indexed ids = %v, want %v", gotIDs, tc.wantIndexedIDs)
			}

			fk := rehydrate.FileKey(hotstore.PlaneEpisodic, testKey)
			if gotDirty := store.manifest.Files[fk].Dirty; gotDirty != tc.wantDirty {
				t.Errorf("manifest dirty = %v, want %v", gotDirty, tc.wantDirty)
			}
			if !tc.wantHydrated {
				return
			}
			want := rehydrate.PlaneStateSHA(store.manifest, hotstore.PlaneEpisodic)
			if got := store.manifest.Indexes[rehydrate.IndexKeyEpisodic].LastHydratedSHA; got != want {
				t.Errorf("hydration sha = %q, want %q — CheckDrift would report permanent drift", got, want)
			}
		})
	}
}

func TestSearchEpisodesRecallBumpFailureDoesNotFailRequest(t *testing.T) {
	store := newFakeStore()
	store.updateEpiErr = errBoom
	index := newFakeIndex()
	index.hits = []search.Hit{{Record: episodic.Record{ID: ulidA}}}
	_, h := newTestServer(t, func(d *Deps) {
		d.Store = store
		d.Index = index
	})
	rec := do(t, h, http.MethodGet, "/v1/ws/team/proj/episodes/search?q=a", nil)
	assertStatus(t, rec, http.StatusOK)
}

func TestSearchEpisodesEmptyResultIsEmptyArray(t *testing.T) {
	store := newFakeStore()
	_, h := newTestServer(t, func(d *Deps) { d.Store = store })
	rec := do(t, h, http.MethodGet, "/v1/ws/team/proj/episodes/search?q=a", nil)
	assertStatus(t, rec, http.StatusOK)
	// A nil slice would serialize as null; clients get [] instead.
	env := decodeEnvelope(t, rec, nil)
	if env.Data == nil {
		t.Fatal("data must be [] not null when no hits match")
	}
	if store.updateCalls != 0 {
		t.Fatal("a zero-hit search must not touch hot records for recall bookkeeping")
	}
}

func TestSearchEpisodesInternalErrorIs500(t *testing.T) {
	index := newFakeIndex()
	index.searchErr = errBoom
	_, h := newTestServer(t, func(d *Deps) { d.Index = index })
	rec := do(t, h, http.MethodGet, "/v1/ws/team/proj/episodes/search?q=a", nil)
	assertStatus(t, rec, http.StatusInternalServerError)
}

func TestGetEpisodeHotThenColdFallback(t *testing.T) {
	hotRec := episodic.Record{ID: ulidA, Kind: episodic.KindEvent, Text: "hot"}
	coldRec := episodic.Record{ID: ulidB, Kind: episodic.KindEvent, Text: "aged to S3"}

	tests := []struct {
		name     string
		id       string
		mutate   func(*Deps)
		status   int
		wantText string
	}{
		{
			name: "hot hit",
			id:   ulidA,
			mutate: func(d *Deps) {
				st := newFakeStore()
				st.episodes[testKey.String()] = []episodic.Record{hotRec}
				d.Store = st
			},
			status:   http.StatusOK,
			wantText: "hot",
		},
		{
			name: "cold fallback keeps provenance resolvable",
			id:   ulidB,
			mutate: func(d *Deps) {
				ar := newFakeArchiver()
				ar.archived[ulidB] = coldRec
				d.Archiver = ar
			},
			status:   http.StatusOK,
			wantText: "aged to S3",
		},
		{
			name:   "absent everywhere",
			id:     ulidC,
			mutate: nil,
			status: http.StatusNotFound,
		},
		{
			name:   "no archiver configured",
			id:     ulidC,
			mutate: func(d *Deps) { d.Archiver = nil },
			status: http.StatusNotFound,
		},
		{
			name: "archive lookup error",
			id:   ulidC,
			mutate: func(d *Deps) {
				ar := newFakeArchiver()
				ar.fetchErr = errBoom
				d.Archiver = ar
			},
			status: http.StatusInternalServerError,
		},
		{
			name: "hot lookup error",
			id:   ulidA,
			mutate: func(d *Deps) {
				st := newFakeStore()
				st.getErr = errBoom
				d.Store = st
			},
			status: http.StatusInternalServerError,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, h := newTestServer(t, tc.mutate)
			rec := do(t, h, http.MethodGet, episodesPath+"/"+tc.id, nil)
			assertStatus(t, rec, tc.status)
			if tc.wantText == "" {
				return
			}
			var got episodic.Record
			decodeEnvelope(t, rec, &got)
			if got.Text != tc.wantText {
				t.Fatalf("text = %q, want %q", got.Text, tc.wantText)
			}
		})
	}
}

func TestGetEpisodeRejectsNonULID(t *testing.T) {
	_, h := newTestServer(t, nil)
	rec := do(t, h, http.MethodGet, episodesPath+"/not-a-ulid", nil)
	assertStatus(t, rec, http.StatusBadRequest)
}
