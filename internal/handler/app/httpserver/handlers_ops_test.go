package httpserver_test

import (
	"net/http"
	"slices"
	"testing"
	"time"

	app "github.com/drakejin/memory-mcp/internal/app/httpserver"
	. "github.com/drakejin/memory-mcp/internal/handler/app/httpserver"

	"github.com/drakejin/memory-mcp/internal/service/consolidate"
	"github.com/drakejin/memory-mcp/internal/service/rehydrate"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// The /v1/status handler is an assembler now: counts and manifest freshness
// come from consolidate.HotState (whose arithmetic is pinned by that package's
// suite), drift from the rehydrator, cold reachability from the document
// pipeline. These tests pin the assembly — section order, degraded note
// order, and the availability gates — not the arithmetic.

func TestStatusCarriesHotStateThrough(t *testing.T) {
	other := projectkey.Key{Workspace: "ws", Team: "team", Project: "alpha"}
	dirty := []string{
		rehydrate.ManifestFileKey(rehydrate.PlaneEpisodic, other),
		rehydrate.ManifestFileKey(rehydrate.PlaneKnowledge, testKey),
	}
	cons := &fakeConsolidator{hotState: consolidate.HotState{
		ManifestUpdatedAt:   fixedNow.Add(-time.Minute),
		DirtyFiles:          dirty,
		Unconsolidated:      2,
		StaleUnconsolidated: 1,
		Degraded:            []string{},
	}}
	_, h := newTestServer(t, func(d *app.Config) { d.Consolidator = cons })

	rec := do(t, h, http.MethodGet, "/v1/status", nil)
	assertStatus(t, rec, http.StatusOK)

	var got StatusReport
	decodeEnvelope(t, rec, &got)
	if got.Unconsolidated != 2 || got.StaleUnconsolidated != 1 {
		t.Errorf("counts = (%d, %d), want (2, 1)", got.Unconsolidated, got.StaleUnconsolidated)
	}
	if !slices.Equal(got.DirtyFiles, dirty) {
		t.Errorf("dirty_files = %v, want %v", got.DirtyFiles, dirty)
	}
	if !got.ManifestUpdatedAt.Equal(fixedNow.Add(-time.Minute)) {
		t.Errorf("manifest_updated_at = %v, want %v", got.ManifestUpdatedAt, fixedNow.Add(-time.Minute))
	}
}

func TestStatusEmptyHotStateSerializesEmptyArrays(t *testing.T) {
	// A zero-value HotState must not surface null: dirty_files and degraded
	// stay [] (§7 envelope).
	_, h := newTestServer(t, func(d *app.Config) { d.Consolidator = &fakeConsolidator{} })
	rec := do(t, h, http.MethodGet, "/v1/status", nil)
	assertStatus(t, rec, http.StatusOK)
	body := rec.Body.String()
	if !slices.Contains([]bool{true}, true) || body == "" {
		t.Fatal("empty body")
	}
	var got StatusReport
	decodeEnvelope(t, rec, &got)
	if got.DirtyFiles == nil {
		t.Error("dirty_files must decode as [], not null")
	}
	if got.Degraded == nil {
		t.Error("degraded must decode as [], not null")
	}
}

func TestStatusReportsDrift(t *testing.T) {
	tests := []struct {
		name         string
		mutate       func(*app.Config)
		wantEpiUnav  bool
		wantKnUnav   bool
		wantDegraded []string
	}{
		{
			name: "both healthy",
			mutate: func(d *app.Config) {
				d.Rehydrator = &fakeRehydrator{}
			},
			wantDegraded: []string{},
		},
		{
			name: "episodic unavailable",
			mutate: func(d *app.Config) {
				d.Rehydrator = &fakeRehydrator{drift: rehydrate.DriftReport{
					Episodic: rehydrate.Drift{Unavailable: true, Reason: "opensearch unreachable"},
				}}
			},
			wantEpiUnav:  true,
			wantDegraded: []string{DegradedSearch},
		},
		{
			name: "rehydrator absent marks both unavailable",
			mutate: func(d *app.Config) {
				d.Rehydrator = nil
			},
			wantEpiUnav:  true,
			wantKnUnav:   true,
			wantDegraded: []string{DegradedSearch, DegradedGraph},
		},
		{
			name: "drift check failure is reported, not hidden",
			mutate: func(d *app.Config) {
				d.Rehydrator = &fakeRehydrator{driftErr: errBoom}
			},
			wantEpiUnav:  true,
			wantKnUnav:   true,
			wantDegraded: []string{DegradedSearch, DegradedGraph},
		},
		{
			name: "cold unreachable",
			mutate: func(d *app.Config) {
				d.Rehydrator = &fakeRehydrator{}
				d.Documents = &fakeDocuments{coldUnreachable: true}
			},
			wantDegraded: []string{DegradedCold},
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

// TestStatusDegradedNoteOrder pins the section order of the degraded list:
// drift notes first, then count disclosures, then cold — the §5 vocabulary in
// the order the sections are assembled.
func TestStatusDegradedNoteOrder(t *testing.T) {
	_, h := newTestServer(t, func(d *app.Config) {
		d.Rehydrator = &fakeRehydrator{drift: rehydrate.DriftReport{
			Episodic: rehydrate.Drift{Unavailable: true, Reason: "opensearch unreachable"},
		}}
		d.Consolidator = &fakeConsolidator{hotState: consolidate.HotState{
			DirtyFiles: []string{},
			Degraded:   []string{consolidate.DegradedCountUnavailable},
		}}
		d.Documents = &fakeDocuments{coldUnreachable: true}
	})

	rec := do(t, h, http.MethodGet, "/v1/status", nil)
	assertStatus(t, rec, http.StatusOK)
	var got StatusReport
	decodeEnvelope(t, rec, &got)
	want := []string{DegradedSearch, consolidate.DegradedCountUnavailable, DegradedCold}
	if !slices.Equal(got.Degraded, want) {
		t.Fatalf("degraded = %v, want %v", got.Degraded, want)
	}
}

func TestStatusS3Sync(t *testing.T) {
	docs := &fakeDocuments{}
	_, h := newTestServer(t, func(d *app.Config) { d.Documents = docs })

	rec := do(t, h, http.MethodGet, "/v1/status", nil)
	assertStatus(t, rec, http.StatusOK)
	var got StatusReport
	decodeEnvelope(t, rec, &got)
	if !got.S3.Reachable {
		t.Error("s3 must report reachable when the pipeline's probe succeeds")
	}
	if docs.probeCalls != 1 {
		t.Errorf("probe calls = %d, want exactly 1", docs.probeCalls)
	}
	if got.S3.Bucket != testS3Bucket {
		t.Errorf("bucket = %q, want %q", got.S3.Bucket, testS3Bucket)
	}
	if !got.S3.LastArchiveAt.IsZero() || !got.S3.LastSnapshotAt.IsZero() {
		t.Error("archive/snapshot timestamps must stay zero until a consolidation writes cold")
	}
}

// TestStatusS3Reachability: the probe mechanics (GET vs HEAD, not-found means
// reachable) are pinned by the document package's ColdReachable suite; here the
// contract is that /status publishes the probe's verdict and degrades honestly.
func TestStatusS3Reachability(t *testing.T) {
	tests := []struct {
		name          string
		docs          *fakeDocuments
		wantReachable bool
		wantDegraded  bool
	}{
		{
			name:          "bucket reachable",
			docs:          &fakeDocuments{},
			wantReachable: true,
		},
		{
			name:          "bucket unreachable",
			docs:          &fakeDocuments{coldUnreachable: true},
			wantReachable: false,
			wantDegraded:  true,
		},
		{
			name:          "cold storage not configured",
			docs:          &fakeDocuments{coldUnconfigured: true},
			wantReachable: false,
			wantDegraded:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			_, h := newTestServer(t, func(d *app.Config) { d.Documents = tt.docs })

			// Act
			rec := do(t, h, http.MethodGet, "/v1/status", nil)

			// Assert
			assertStatus(t, rec, http.StatusOK)
			var got StatusReport
			decodeEnvelope(t, rec, &got)
			if got.S3.Reachable != tt.wantReachable {
				t.Fatalf("s3.reachable = %v, want %v", got.S3.Reachable, tt.wantReachable)
			}
			if gotDegraded := slices.Contains(got.Degraded, DegradedCold); gotDegraded != tt.wantDegraded {
				t.Fatalf("degraded contains %q = %v, want %v", DegradedCold, gotDegraded, tt.wantDegraded)
			}
		})
	}
}

func TestStatusWithoutDocumentPipelineReportsColdDown(t *testing.T) {
	_, h := newTestServer(t, func(d *app.Config) { d.Documents = nil })
	rec := do(t, h, http.MethodGet, "/v1/status", nil)
	assertStatus(t, rec, http.StatusOK)
	var got StatusReport
	decodeEnvelope(t, rec, &got)
	if got.S3.Reachable {
		t.Error("no pipeline means no probe; reachability must not be claimed")
	}
	if !slices.Contains(got.Degraded, DegradedCold) {
		t.Errorf("degraded = %v, want it to name cold storage", got.Degraded)
	}
}

func TestStatusCountFailureIsDisclosedNotFatal(t *testing.T) {
	cons := &fakeConsolidator{hotState: consolidate.HotState{
		DirtyFiles: []string{},
		Degraded:   []string{consolidate.DegradedCountIncompletePrefix + testKey.String()},
	}}
	_, h := newTestServer(t, func(d *app.Config) { d.Consolidator = cons })
	rec := do(t, h, http.MethodGet, "/v1/status", nil)
	// /status must always answer; the gap shows up in degraded.
	assertStatus(t, rec, http.StatusOK)
	var got StatusReport
	decodeEnvelope(t, rec, &got)
	if len(got.Degraded) == 0 {
		t.Fatal("an unreadable count must be disclosed in degraded")
	}
}

func TestStatusHotStateFailureIs500(t *testing.T) {
	cons := &fakeConsolidator{hotStateErr: errBoom}
	_, h := newTestServer(t, func(d *app.Config) { d.Consolidator = cons })
	rec := do(t, h, http.MethodGet, "/v1/status", nil)
	assertStatus(t, rec, http.StatusInternalServerError)
}

// TestStatusWithoutHotStoreIs503: without the canonical store the process has
// no consolidator either (it cannot be built storeless), and that absence is
// what /status reads as "hot store unavailable" — the same coupling production
// wiring has.
func TestStatusWithoutHotStoreIs503(t *testing.T) {
	_, h := newTestServer(t, func(d *app.Config) {
		d.Store = nil
		d.Consolidator = nil
	})
	rec := do(t, h, http.MethodGet, "/v1/status", nil)
	assertStatus(t, rec, http.StatusServiceUnavailable)
	env := decodeEnvelope(t, rec, nil)
	if env.Error == nil || env.Error.Message != "hot store unavailable" {
		t.Fatalf("error = %+v, want the hot-store message", env.Error)
	}
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
			_, h := newTestServer(t, func(d *app.Config) { d.Consolidator = cons })
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
	_, h := newTestServer(t, func(d *app.Config) {
		d.Consolidator = cons
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
		mutate func(*app.Config)
		status int
	}{
		{"consolidator absent", func(d *app.Config) { d.Consolidator = nil }, http.StatusServiceUnavailable},
		{"run fails", func(d *app.Config) { d.Consolidator = &fakeConsolidator{err: errBoom} }, http.StatusInternalServerError},
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
			_, h := newTestServer(t, func(d *app.Config) { d.Rehydrator = reh })
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
		mutate func(*app.Config)
		status int
	}{
		{"rehydrator absent", func(d *app.Config) { d.Rehydrator = nil }, http.StatusServiceUnavailable},
		{"rehydration fails", func(d *app.Config) { d.Rehydrator = &fakeRehydrator{allErr: errBoom} }, http.StatusInternalServerError},
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
			srv, _ := newTestServer(t, func(d *app.Config) { d.Rehydrator = tc.reh })
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
	srv, _ := newTestServer(t, func(d *app.Config) {
		d.Rehydrator = &fakeRehydrator{
			drift:  rehydrate.DriftReport{Episodic: rehydrate.Drift{Detected: true}},
			allErr: errBoom,
		}
	})
	srv.Startup(t.Context())

	srv2, _ := newTestServer(t, func(d *app.Config) { d.Rehydrator = nil })
	srv2.Startup(t.Context())
}

func TestStartupLogsPartialFailures(t *testing.T) {
	reh := &fakeRehydrator{
		drift:  rehydrate.DriftReport{Knowledge: rehydrate.Drift{Detected: true}},
		report: rehydrate.Report{NodesUpserted: 1, Failures: []string{"knowledge ws/team/proj: upsert: boom"}},
	}
	srv, _ := newTestServer(t, func(d *app.Config) { d.Rehydrator = reh })
	srv.Startup(t.Context())
	if reh.allCalls != 1 {
		t.Fatalf("RehydrateAll calls = %d, want 1", reh.allCalls)
	}
}
