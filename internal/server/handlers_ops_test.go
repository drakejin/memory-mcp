package server

import (
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/consolidate"
	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/errs"
	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/knowledge"
	"github.com/drakejin/memory-mcp/internal/rehydrate"
)

// staleRec is an unconsolidated record older than the TTL — §3.1 says it stays
// hot forever and /status must keep saying so.
func staleRec(id string, age time.Duration, consolidated bool) episodic.Record {
	return episodic.Record{
		ID: id, Kind: episodic.KindEvent, Actor: episodic.ActorAgent, Text: "t",
		OccurredAt: fixedNow.Add(-age), Consolidated: consolidated,
	}
}

func TestStatusReportsUnconsolidatedAndStale(t *testing.T) {
	store := newFakeStore()
	ttl := time.Duration(testTTLDays) * 24 * time.Hour
	store.episodes[testKey.String()] = []episodic.Record{
		staleRec(ulidA, ttl+48*time.Hour, false), // stale + unconsolidated
		staleRec(ulidB, time.Hour, false),        // fresh + unconsolidated
		staleRec(ulidC, ttl+48*time.Hour, true),  // consolidated: not counted
	}
	_, h := newTestServer(t, func(d *Config) { d.Store = store })

	rec := do(t, h, http.MethodGet, "/v1/status", nil)
	assertStatus(t, rec, http.StatusOK)

	var got StatusReport
	decodeEnvelope(t, rec, &got)
	if got.Unconsolidated != 2 {
		t.Errorf("unconsolidated = %d, want 2", got.Unconsolidated)
	}
	if got.StaleUnconsolidated != 1 {
		t.Errorf("stale_unconsolidated = %d, want 1", got.StaleUnconsolidated)
	}
}

func TestStatusReportsDirtyFilesSorted(t *testing.T) {
	store := newFakeStore()
	other := hotstore.ProjectKey{Workspace: "ws", Team: "team", Project: "alpha"}
	store.manifest.Files = map[string]hotstore.FileState{
		hotstore.ManifestFileKey(hotstore.PlaneKnowledge, testKey): {Dirty: true},
		hotstore.ManifestFileKey(hotstore.PlaneEpisodic, other):    {Dirty: true},
		hotstore.ManifestFileKey(hotstore.PlaneEpisodic, testKey):  {Dirty: false},
	}
	store.manifest.UpdatedAt = fixedNow.Add(-time.Minute)
	_, h := newTestServer(t, func(d *Config) { d.Store = store })

	rec := do(t, h, http.MethodGet, "/v1/status", nil)
	assertStatus(t, rec, http.StatusOK)

	var got StatusReport
	decodeEnvelope(t, rec, &got)
	want := []string{
		hotstore.ManifestFileKey(hotstore.PlaneEpisodic, other),
		hotstore.ManifestFileKey(hotstore.PlaneKnowledge, testKey),
	}
	if len(got.DirtyFiles) != len(want) {
		t.Fatalf("dirty_files = %v, want %v", got.DirtyFiles, want)
	}
	for i := range want {
		if got.DirtyFiles[i] != want[i] {
			t.Fatalf("dirty_files = %v, want %v (sorted)", got.DirtyFiles, want)
		}
	}
	if !got.ManifestUpdatedAt.Equal(store.manifest.UpdatedAt) {
		t.Errorf("manifest_updated_at = %v, want %v", got.ManifestUpdatedAt, store.manifest.UpdatedAt)
	}
}

func TestStatusReportsDrift(t *testing.T) {
	tests := []struct {
		name         string
		mutate       func(*Config)
		wantEpiUnav  bool
		wantKnUnav   bool
		wantDegraded []string
	}{
		{
			name: "both healthy",
			mutate: func(d *Config) {
				d.Rehydrator = &fakeRehydrator{}
				d.Archiver = reachableArchiver()
			},
			wantDegraded: []string{},
		},
		{
			name: "episodic unavailable",
			mutate: func(d *Config) {
				d.Rehydrator = &fakeRehydrator{drift: rehydrate.DriftReport{
					Episodic: rehydrate.Drift{Unavailable: true, Reason: "opensearch unreachable"},
				}}
				d.Archiver = reachableArchiver()
			},
			wantEpiUnav:  true,
			wantDegraded: []string{degradedSearch},
		},
		{
			name: "rehydrator absent marks both unavailable",
			mutate: func(d *Config) {
				d.Rehydrator = nil
				d.Archiver = reachableArchiver()
			},
			wantEpiUnav:  true,
			wantKnUnav:   true,
			wantDegraded: []string{degradedSearch, degradedGraph},
		},
		{
			name: "drift check failure is reported, not hidden",
			mutate: func(d *Config) {
				d.Rehydrator = &fakeRehydrator{driftErr: errBoom}
				d.Archiver = reachableArchiver()
			},
			wantEpiUnav:  true,
			wantKnUnav:   true,
			wantDegraded: []string{degradedSearch, degradedGraph},
		},
		{
			name: "cold unreachable",
			mutate: func(d *Config) {
				d.Rehydrator = &fakeRehydrator{}
				d.Archiver = nil
			},
			wantDegraded: []string{degradedCold},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, h := newTestServer(t, tc.mutate)
			rec := do(t, h, http.MethodGet, "/v1/status", nil)
			assertStatus(t, rec, http.StatusOK)

			var got StatusReport
			decodeEnvelope(t, rec, &got)
			if got.Drift.Episodic.Unavailable != tc.wantEpiUnav {
				t.Errorf("episodic unavailable = %v, want %v", got.Drift.Episodic.Unavailable, tc.wantEpiUnav)
			}
			if got.Drift.Knowledge.Unavailable != tc.wantKnUnav {
				t.Errorf("knowledge unavailable = %v, want %v", got.Drift.Knowledge.Unavailable, tc.wantKnUnav)
			}
			if len(got.Degraded) != len(tc.wantDegraded) {
				t.Fatalf("degraded = %v, want %v", got.Degraded, tc.wantDegraded)
			}
			for i := range tc.wantDegraded {
				if got.Degraded[i] != tc.wantDegraded[i] {
					t.Fatalf("degraded = %v, want %v", got.Degraded, tc.wantDegraded)
				}
			}
		})
	}
}

// reachableArchiver is a ColdArchive whose probe answers not-found rather than
// erroring — i.e. the bucket is live and simply does not hold the probe key.
func reachableArchiver() *fakeArchiver {
	return newFakeArchiver()
}

func TestStatusS3Sync(t *testing.T) {
	ar := reachableArchiver()
	_, h := newTestServer(t, func(d *Config) { d.Archiver = ar })

	rec := do(t, h, http.MethodGet, "/v1/status", nil)
	assertStatus(t, rec, http.StatusOK)
	var got StatusReport
	decodeEnvelope(t, rec, &got)
	if !got.S3.Reachable {
		t.Error("s3 must report reachable when the probe returns without error")
	}
	if got.S3.Bucket != testS3Bucket {
		t.Errorf("bucket = %q, want %q", got.S3.Bucket, testS3Bucket)
	}
	if !got.S3.LastArchiveAt.IsZero() || !got.S3.LastSnapshotAt.IsZero() {
		t.Error("archive/snapshot timestamps must stay zero until a consolidation writes cold")
	}
}

// TestStatusS3Reachability pins the §0-principle-3 honesty contract for the
// cold store. The probe must be a GET: S3 answers HeadObject against a
// non-existent bucket with a bare 404 that is indistinguishable from a missing
// key, so a HEAD-based probe reported a bucket that does not exist as
// reachable — exactly the state /status is supposed to expose.
func TestStatusS3Reachability(t *testing.T) {
	tests := []struct {
		name          string
		blobErr       error
		wantReachable bool
		wantDegraded  bool
	}{
		{
			name:          "bucket live, probe key absent",
			blobErr:       errs.NotFound("cold.FetchBlob", "blob", emptySHA256),
			wantReachable: true,
		},
		{
			name:          "bucket does not exist",
			blobErr:       errors.New("api error NoSuchBucket: The specified bucket does not exist"),
			wantReachable: false,
			wantDegraded:  true,
		},
		{
			name:          "credentials or network failure",
			blobErr:       errBoom,
			wantReachable: false,
			wantDegraded:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			ar := newFakeArchiver()
			ar.blobErr = tt.blobErr
			_, h := newTestServer(t, func(d *Config) { d.Archiver = ar })

			// Act
			rec := do(t, h, http.MethodGet, "/v1/status", nil)

			// Assert
			assertStatus(t, rec, http.StatusOK)
			var got StatusReport
			decodeEnvelope(t, rec, &got)
			if got.S3.Reachable != tt.wantReachable {
				t.Fatalf("s3.reachable = %v, want %v", got.S3.Reachable, tt.wantReachable)
			}
			if gotDegraded := slices.Contains(got.Degraded, degradedCold); gotDegraded != tt.wantDegraded {
				t.Fatalf("degraded contains %q = %v, want %v", degradedCold, gotDegraded, tt.wantDegraded)
			}
		})
	}
}

func TestStatusListingFailureIsReportedNotFatal(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*fakeStore)
	}{
		{"project listing fails", func(s *fakeStore) { s.listProjErr = errBoom }},
		{"episode listing fails", func(s *fakeStore) {
			s.episodes[testKey.String()] = []episodic.Record{staleRec(ulidA, time.Hour, false)}
			s.listEpisErr = errBoom
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			tc.mutate(store)
			_, h := newTestServer(t, func(d *Config) { d.Store = store })
			rec := do(t, h, http.MethodGet, "/v1/status", nil)
			// /status must always answer; the gap shows up in degraded.
			assertStatus(t, rec, http.StatusOK)
			var got StatusReport
			decodeEnvelope(t, rec, &got)
			if len(got.Degraded) == 0 {
				t.Fatal("an unreadable count must be disclosed in degraded")
			}
		})
	}
}

func TestStatusManifestFailureIs500(t *testing.T) {
	store := newFakeStore()
	store.manifestErr = errBoom
	_, h := newTestServer(t, func(d *Config) { d.Store = store })
	rec := do(t, h, http.MethodGet, "/v1/status", nil)
	assertStatus(t, rec, http.StatusInternalServerError)
}

func TestStatusWithoutHotStoreIs503(t *testing.T) {
	_, h := newTestServer(t, func(d *Config) { d.Store = nil })
	rec := do(t, h, http.MethodGet, "/v1/status", nil)
	assertStatus(t, rec, http.StatusServiceUnavailable)
}

func TestConsolidateProjectSelectors(t *testing.T) {
	tests := []struct {
		name     string
		body     any
		status   int
		wantKeys []string
		wantDry  bool
	}{
		{"empty body means all projects", "", http.StatusOK, nil, false},
		{"explicit projects", ConsolidateRequest{Projects: []string{"ws/team/proj"}}, http.StatusOK, []string{"ws/team/proj"}, false},
		{"dry run", ConsolidateRequest{DryRun: true}, http.StatusOK, nil, true},
		{"too few segments", ConsolidateRequest{Projects: []string{"ws/team"}}, http.StatusBadRequest, nil, false},
		{"too many segments", ConsolidateRequest{Projects: []string{"ws/team/proj/extra"}}, http.StatusBadRequest, nil, false},
		{"bad charset", ConsolidateRequest{Projects: []string{"WS/team/proj"}}, http.StatusBadRequest, nil, false},
		{"empty segment", ConsolidateRequest{Projects: []string{"ws//proj"}}, http.StatusBadRequest, nil, false},
		{"dot segment", ConsolidateRequest{Projects: []string{"ws/../proj"}}, http.StatusBadRequest, nil, false},
		{"malformed json", "{oops", http.StatusBadRequest, nil, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cons := &fakeConsolidator{}
			_, h := newTestServer(t, func(d *Config) { d.Consolidator = cons })
			rec := do(t, h, http.MethodPost, "/v1/consolidate", tc.body)
			assertStatus(t, rec, tc.status)
			if tc.status != http.StatusOK {
				if cons.calls != 0 {
					t.Fatal("a rejected request must never reach the consolidator")
				}
				return
			}
			if cons.lastOpts.DryRun != tc.wantDry {
				t.Errorf("dry_run = %v, want %v", cons.lastOpts.DryRun, tc.wantDry)
			}
			if len(cons.lastOpts.Projects) != len(tc.wantKeys) {
				t.Fatalf("projects = %v, want %v", cons.lastOpts.Projects, tc.wantKeys)
			}
			for i, want := range tc.wantKeys {
				if cons.lastOpts.Projects[i].String() != want {
					t.Fatalf("project[%d] = %q, want %q", i, cons.lastOpts.Projects[i].String(), want)
				}
			}
		})
	}
}

func TestConsolidateRecordsS3Activity(t *testing.T) {
	cons := &fakeConsolidator{report: consolidate.Report{
		MovedEpisodes: 3,
		ArchiveKeys:   []string{"jin/episodic/ws/team/proj/2026-07.json"},
		SnapshotKeys:  []string{"jin/knowledge/ws/team/proj/latest.json"},
		Failures:      []string{},
	}}
	ar := reachableArchiver()
	_, h := newTestServer(t, func(d *Config) {
		d.Consolidator = cons
		d.Archiver = ar
	})

	rec := do(t, h, http.MethodPost, "/v1/consolidate", ConsolidateRequest{})
	assertStatus(t, rec, http.StatusOK)
	var report consolidate.Report
	decodeEnvelope(t, rec, &report)
	if report.MovedEpisodes != 3 {
		t.Fatalf("moved_episodes = %d, want 3", report.MovedEpisodes)
	}

	// /status must now disclose when cold last received data.
	statusRec := do(t, h, http.MethodGet, "/v1/status", nil)
	assertStatus(t, statusRec, http.StatusOK)
	var st StatusReport
	decodeEnvelope(t, statusRec, &st)
	if !st.S3.LastArchiveAt.Equal(fixedNow) {
		t.Errorf("last_archive_at = %v, want %v", st.S3.LastArchiveAt, fixedNow)
	}
	if !st.S3.LastSnapshotAt.Equal(fixedNow) {
		t.Errorf("last_snapshot_at = %v, want %v", st.S3.LastSnapshotAt, fixedNow)
	}
}

func TestConsolidateFailures(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		status int
	}{
		{"consolidator absent", func(d *Config) { d.Consolidator = nil }, http.StatusServiceUnavailable},
		{"run fails", func(d *Config) { d.Consolidator = &fakeConsolidator{err: errBoom} }, http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, h := newTestServer(t, tc.mutate)
			rec := do(t, h, http.MethodPost, "/v1/consolidate", ConsolidateRequest{})
			assertStatus(t, rec, tc.status)
		})
	}
}

func TestReindex(t *testing.T) {
	tests := []struct {
		name       string
		target     string
		wantVerify bool
	}{
		{"default", "/v1/reindex", false},
		{"verify true", "/v1/reindex?verify=true", true},
		{"verify anything else", "/v1/reindex?verify=1", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reh := &fakeRehydrator{report: rehydrate.Report{EpisodesIndexed: 7, NodesUpserted: 2, Failures: []string{}}}
			_, h := newTestServer(t, func(d *Config) { d.Rehydrator = reh })
			rec := do(t, h, http.MethodPost, tc.target, nil)
			assertStatus(t, rec, http.StatusOK)

			var got rehydrate.Report
			decodeEnvelope(t, rec, &got)
			if got.EpisodesIndexed != 7 {
				t.Errorf("episodes_indexed = %d, want 7", got.EpisodesIndexed)
			}
			if reh.allCalls != 1 {
				t.Errorf("RehydrateAll calls = %d, want 1", reh.allCalls)
			}
			if reh.lastVerify != tc.wantVerify {
				t.Errorf("verify = %v, want %v", reh.lastVerify, tc.wantVerify)
			}
		})
	}
}

func TestReindexFailures(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		status int
	}{
		{"rehydrator absent", func(d *Config) { d.Rehydrator = nil }, http.StatusServiceUnavailable},
		{"rehydration fails", func(d *Config) { d.Rehydrator = &fakeRehydrator{allErr: errBoom} }, http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, h := newTestServer(t, tc.mutate)
			rec := do(t, h, http.MethodPost, "/v1/reindex", nil)
			assertStatus(t, rec, tc.status)
		})
	}
}

func TestHealthz(t *testing.T) {
	_, h := newTestServer(t, nil)
	rec := do(t, h, http.MethodGet, "/healthz", nil)
	assertStatus(t, rec, http.StatusOK)
	env := decodeEnvelope(t, rec, nil)
	if !env.Success {
		t.Fatal("healthz must answer with a success envelope")
	}
}

func TestStartupRehydratesOnlyOnDrift(t *testing.T) {
	tests := []struct {
		name     string
		reh      *fakeRehydrator
		wantCall int
	}{
		{
			name:     "no drift",
			reh:      &fakeRehydrator{},
			wantCall: 0,
		},
		{
			name:     "episodic drift",
			reh:      &fakeRehydrator{drift: rehydrate.DriftReport{Episodic: rehydrate.Drift{Detected: true, Reason: "docs 0 != 5"}}},
			wantCall: 1,
		},
		{
			name:     "knowledge drift",
			reh:      &fakeRehydrator{drift: rehydrate.DriftReport{Knowledge: rehydrate.Drift{Detected: true, Reason: "nodes 0 != 3"}}},
			wantCall: 1,
		},
		{
			name:     "unavailable is not drift",
			reh:      &fakeRehydrator{drift: rehydrate.DriftReport{Episodic: rehydrate.Drift{Unavailable: true}}},
			wantCall: 0,
		},
		{
			name:     "drift check failure does not rehydrate blindly",
			reh:      &fakeRehydrator{driftErr: errBoom},
			wantCall: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newTestServer(t, func(d *Config) { d.Rehydrator = tc.reh })
			srv.Startup(t.Context())
			if tc.reh.allCalls != tc.wantCall {
				t.Fatalf("RehydrateAll calls = %d, want %d", tc.reh.allCalls, tc.wantCall)
			}
			if tc.wantCall > 0 && tc.reh.lastVerify {
				t.Error("startup rehydration must not run the expensive verify audit")
			}
		})
	}
}

func TestStartupToleratesFailures(t *testing.T) {
	// A boot with dead derived stores must not panic or block; degradation is
	// reported by /status instead (§5).
	srv, _ := newTestServer(t, func(d *Config) {
		d.Rehydrator = &fakeRehydrator{
			drift:  rehydrate.DriftReport{Episodic: rehydrate.Drift{Detected: true}},
			allErr: errBoom,
		}
	})
	srv.Startup(t.Context())

	srv2, _ := newTestServer(t, func(d *Config) { d.Rehydrator = nil })
	srv2.Startup(t.Context())
}

func TestStartupLogsPartialFailures(t *testing.T) {
	reh := &fakeRehydrator{
		drift:  rehydrate.DriftReport{Knowledge: rehydrate.Drift{Detected: true}},
		report: rehydrate.Report{NodesUpserted: 1, Failures: []string{"knowledge ws/team/proj: upsert: boom"}},
	}
	srv, _ := newTestServer(t, func(d *Config) { d.Rehydrator = reh })
	srv.Startup(t.Context())
	if reh.allCalls != 1 {
		t.Fatalf("RehydrateAll calls = %d, want 1", reh.allCalls)
	}
}

func TestStatusCountsAcrossProjects(t *testing.T) {
	store := newFakeStore()
	other := hotstore.ProjectKey{Workspace: "ws", Team: "team", Project: "alpha"}
	store.episodes[testKey.String()] = []episodic.Record{staleRec(ulidA, time.Hour, false)}
	store.episodes[other.String()] = []episodic.Record{staleRec(ulidB, time.Hour, false)}
	store.graphs[other.String()] = knowledge.Graph{}
	_, h := newTestServer(t, func(d *Config) { d.Store = store })

	rec := do(t, h, http.MethodGet, "/v1/status", nil)
	assertStatus(t, rec, http.StatusOK)
	var got StatusReport
	decodeEnvelope(t, rec, &got)
	if got.Unconsolidated != 2 {
		t.Fatalf("unconsolidated = %d, want 2 across both projects", got.Unconsolidated)
	}
}
