package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/x/config"
)

// The composition root is the one function whose mistakes are fatal rather than
// degraded: every package below it is unit-tested against fakes, but only build
// decides which real collaborator each Config literal receives.
//
// What a probe can and cannot prove: a collaborator the server never received
// answers 503 with a fixed message, so its absence is observable over HTTP.
// Index and Graph are the exception — "not wired" and "wired but unreachable"
// answer identically by design (§5), and telling them apart needs the live
// containers of the §10 blackbox suite.

// startupGrace bounds the boot-time drift check. Nothing listens on the derived
// ports, so its probes fail immediately; the deadline only stops a misbehaving
// client from hanging the suite.
const startupGrace = 5 * time.Second

// Bounds for the full-process boot test: how long the server gets to answer,
// how often it is asked, and how long a signalled shutdown may take.
const (
	bootWait     = 10 * time.Second
	bootPoll     = 10 * time.Millisecond
	shutdownWait = 15 * time.Second
)

// testProject is the {ws}/{team}/{proj} path the probes below write under.
const testProject = "/v1/vms/core/memory"

// zeroSHA is sha256("") — a well-formed content address no ingest created, so a
// chunk lookup for it is an empty answer rather than an error.
const zeroSHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// closedPort reserves a loopback port and immediately releases it, yielding an
// address nothing listens on.
//
// The derived endpoints must be dead rather than merely default: a developer
// running `make start` has OpenSearch and Neo4j on the spec addresses, and a
// unit suite pointed there would both write into the live stack and flip its
// own assertions depending on whether the containers happen to be up.
func closedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release loopback port: %v", err)
	}
	return addr
}

// testConfig is a valid runtime configuration pointing at a throwaway home and
// at endpoints nothing is listening on. build performs no I/O (§1 rule 4), so
// the dead endpoints are exactly the point: wiring must succeed without them.
//
// AWSProfile stays empty on purpose: with a profile name cold.New resolves the
// developer's shared AWS config and fails on a machine that has never seen it,
// leaving the archiver legitimately nil and the wiring unassertable. Empty
// selects the SDK default chain, which resolves without credentials because
// cold.New performs no I/O either.
func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Config{
		Home:            t.TempDir(),
		Username:        "blackbox",
		S3Bucket:        config.DefaultS3Bucket,
		S3Region:        config.DefaultS3Region,
		OpenSearchURL:   "http://" + closedPort(t),
		Neo4jURL:        "bolt://" + closedPort(t),
		Neo4jUser:       config.DefaultNeo4jUser,
		Neo4jPassword:   config.DefaultNeo4jPassword,
		EpisodicTTLDays: config.DefaultEpisodicTTLDays,
		ListenAddr:      config.DefaultListenAddr,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config must satisfy the real validator: %v", err)
	}
	return cfg
}

// buildForTest runs the composition root, fails the test on a partial wiring,
// and registers the release func.
func buildForTest(t *testing.T, logs *bytes.Buffer) http.Handler {
	t.Helper()
	srv, release, err := build(testConfig(t), slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatalf("build() = %v, want a fully wired server", err)
	}
	if release == nil {
		t.Fatal("build() returned a nil release func")
	}
	t.Cleanup(release)
	if srv == nil {
		t.Fatal("build() returned a nil server without an error")
	}
	return srv.Router()
}

// TestBuildWiresEveryCollaborator is the regression guard for a whole class of
// boot failures that no package-level test can see: every constructor below
// validates its own Config, so a field omitted from one of the literals in
// build makes the process exit before it ever opens a listener — while
// `go build` and every unit test stay green, because nothing else exercises the
// composition root.
//
// The concrete bug this pins: document.Config was wired without IDs, so
// document.New returned "id generator must not be nil" and memory-mcp died on
// launch with the entire honesty contract (/status, /healthz, degraded and
// truncation reporting) unreachable.
func TestBuildWiresEveryCollaborator(t *testing.T) {
	// Arrange
	logger := slog.New(slog.DiscardHandler)

	// Act
	srv, release, err := build(testConfig(t), logger)

	// Assert
	if err != nil {
		t.Fatalf("build() = %v, want a fully wired server", err)
	}
	if srv == nil {
		t.Fatal("build() returned a nil server without an error")
	}
	if release == nil {
		t.Fatal("build() returned a nil release func")
	}
	release()
}

// TestBuiltServerServes proves the wired object graph actually answers HTTP:
// build returning no error is necessary but not sufficient evidence that the
// process would have come up.
//
// Each case names the collaborator it proves reached server.Config: were that
// field nil, the handler would short-circuit with 503 before doing any work.
func TestBuiltServerServes(t *testing.T) {
	// Arrange
	router := buildForTest(t, &bytes.Buffer{})

	tests := []struct {
		name        string
		proves      string
		method      string
		target      string
		contentType string
		body        string
		wantStatus  int
	}{
		{
			name:       "liveness",
			proves:     "the router itself",
			method:     http.MethodGet,
			target:     "/healthz",
			wantStatus: http.StatusOK,
		},
		{
			name:        "episode write",
			proves:      "Store, Clock and IDs",
			method:      http.MethodPost,
			target:      testProject + "/episodes",
			contentType: "application/json",
			body:        `{"kind":"observation","actor":"agent","text":"wiring probe"}`,
			wantStatus:  http.StatusCreated,
		},
		{
			name:       "document chunk lookup",
			proves:     "Documents",
			method:     http.MethodGet,
			target:     "/v1/documents/" + zeroSHA + "/chunks",
			wantStatus: http.StatusOK,
		},
		{
			name:        "consolidation dry run",
			proves:      "Consolidator",
			method:      http.MethodPost,
			target:      "/v1/consolidate",
			contentType: "application/json",
			body:        `{"dry_run":true}`,
			wantStatus:  http.StatusOK,
		},
		{
			// A wired archiver lets the handler reach multipart parsing, which
			// rejects this body as a bad request; a nil one would answer 503
			// "cold storage unavailable" before reading anything (§6 step 2).
			name:        "document ingest reaches multipart parsing",
			proves:      "Archiver",
			method:      http.MethodPost,
			target:      testProject + "/documents",
			contentType: "application/json",
			body:        `{"not":"multipart"}`,
			wantStatus:  http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.target, strings.NewReader(tt.body))
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			rec := httptest.NewRecorder()

			// Act
			router.ServeHTTP(rec, req)

			// Assert
			if rec.Code != tt.wantStatus {
				t.Errorf("%s %s = %d, want %d (%s not wired?); body: %s",
					tt.method, tt.target, rec.Code, tt.wantStatus, tt.proves, rec.Body)
			}
		})
	}
}

// TestBuiltServerReportsDegradedWrite pins the §5 honesty contract at the
// composition root: with OpenSearch unreachable the hot write still returns
// 201, and the response names the plane that went stale instead of implying the
// episode was indexed.
func TestBuiltServerReportsDegradedWrite(t *testing.T) {
	// Arrange
	router := buildForTest(t, &bytes.Buffer{})
	req := httptest.NewRequest(http.MethodPost, testProject+"/episodes",
		strings.NewReader(`{"kind":"observation","actor":"agent","text":"degraded probe"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	// Act
	router.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST episodes = %d, want %d; body: %s", rec.Code, http.StatusCreated, rec.Body)
	}
	var env struct {
		Success bool `json:"success"`
		Data    struct {
			Record   struct{ ID string } `json:"record"`
			Degraded []string            `json:"degraded"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v; body: %s", err, rec.Body)
	}
	if !env.Success {
		t.Error("success = false, want true")
	}
	if env.Data.Record.ID == "" {
		t.Error("stored record carries no id")
	}
	if !slices.Contains(env.Data.Degraded, "search unavailable") {
		t.Errorf("degraded = %v, want it to name the unreachable index", env.Data.Degraded)
	}
}

// TestBuiltServerStartupUsesTheWiredRehydrator covers the one collaborator no
// request path can prove cheaply. Startup announces a missing rehydrator in the
// log and otherwise reports the drift check, so the absence of that warning is
// the evidence that Rehydrator reached server.Config (§5).
func TestBuiltServerStartupUsesTheWiredRehydrator(t *testing.T) {
	// Arrange
	logs := &bytes.Buffer{}
	srv, release, err := build(testConfig(t), slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatalf("build() = %v", err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), startupGrace)
	defer cancel()

	// Act
	srv.Startup(ctx)

	// Assert
	got := logs.String()
	if strings.Contains(got, "rehydrator not configured") {
		t.Errorf("startup ran without a rehydrator; log: %s", got)
	}
	if !strings.Contains(got, "startup drift check") {
		t.Errorf("startup did not report a drift check; log: %s", got)
	}
}

// TestBuildDegradesOnUnbuildableDerivedClients pins §5 at the boot seam: a
// derived client that cannot even be constructed leaves its plane nil and the
// server still comes up. Hot writes are the canonical plane (§0 principle 1),
// so they must keep succeeding while OpenSearch, Neo4j or S3 are unusable —
// with the shortfall disclosed rather than swallowed.
func TestBuildDegradesOnUnbuildableDerivedClients(t *testing.T) {
	tests := []struct {
		name     string
		breakCfg func(*config.Config)
	}{
		{
			name:     "opensearch client unbuildable",
			breakCfg: func(c *config.Config) { c.OpenSearchURL = "" },
		},
		{
			name:     "neo4j client unbuildable",
			breakCfg: func(c *config.Config) { c.Neo4jURL = "" },
		},
		{
			name:     "s3 client unbuildable",
			breakCfg: func(c *config.Config) { c.AWSProfile = "definitely-not-a-real-profile" },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			cfg := testConfig(t)
			tt.breakCfg(&cfg)
			logs := &bytes.Buffer{}

			// Act
			srv, release, err := build(cfg, slog.New(slog.NewTextHandler(logs, nil)))

			// Assert
			if err != nil {
				t.Fatalf("build() = %v, want a boot that degrades instead of failing", err)
			}
			t.Cleanup(release)
			if !strings.Contains(logs.String(), "degraded") {
				t.Errorf("an unbuildable derived client must be disclosed; log: %s", logs)
			}
			req := httptest.NewRequest(http.MethodPost, testProject+"/episodes",
				strings.NewReader(`{"kind":"observation","actor":"agent","text":"hot write survives"}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.Router().ServeHTTP(rec, req)
			if rec.Code != http.StatusCreated {
				t.Errorf("POST episodes = %d, want %d; hot writes must survive a dead derivative; body: %s",
					rec.Code, http.StatusCreated, rec.Body)
			}
		})
	}
}

// TestRunBootsAndShutsDownOnSignal boots the process the way a user does:
// configuration from the environment, a real listener, and shutdown on SIGTERM.
// It is what turns "does the binary come up?" from a manual check into a
// regression test — a Config literal missing a required collaborator makes run
// return before it ever listens, which is how the whole server stayed dead
// while `go build` and every package suite were green.
func TestRunBootsAndShutsDownOnSignal(t *testing.T) {
	// Arrange: a throwaway home and derived endpoints nothing listens on, so
	// the boot exercises the degraded path rather than the developer's stack.
	addr := closedPort(t)
	t.Setenv("DJ_MEMORY_HOME", t.TempDir())
	t.Setenv("DJ_MEMORY_LISTEN_ADDR", addr)
	t.Setenv("DJ_MEMORY_OPENSEARCH_URL", "http://"+closedPort(t))
	t.Setenv("DJ_MEMORY_NEO4J_URL", "bolt://"+closedPort(t))
	t.Setenv("AWS_PROFILE", "")

	done := make(chan error, 1)
	go func() { done <- run(slog.New(slog.NewTextHandler(io.Discard, nil))) }()

	// Act: serve, then ask the process to stop the way a Ctrl-C would.
	waitForHealthz(t, addr)
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("signal self: %v", err)
	}

	// Assert
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run() = %v, want a clean shutdown", err)
		}
	case <-time.After(shutdownWait):
		t.Fatal("run() did not return after SIGTERM")
	}
}

// TestRunRejectsAMalformedEnvironment: a configuration value the loader cannot
// parse aborts the boot instead of quietly substituting a default, so an
// operator learns about the typo at launch rather than after the TTL silently
// reverted to 30 days (§3.1).
func TestRunRejectsAMalformedEnvironment(t *testing.T) {
	// Arrange
	t.Setenv("DJ_MEMORY_HOME", t.TempDir())
	t.Setenv("DJ_MEMORY_EPISODIC_TTL_DAYS", "not-a-number")

	// Act
	err := run(slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Assert
	if err == nil {
		t.Fatal("run() = nil, want the boot to fail on an unparsable TTL")
	}
}

// waitForHealthz blocks until the booting server answers, or fails the test.
func waitForHealthz(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(bootWait)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(bootPoll)
	}
	t.Fatalf("server never answered /healthz on %s within %s", addr, bootWait)
}

// TestBuildRejectsInvalidConfig keeps the seam honest in the other direction: a
// configuration the server cannot serve must fail here, not at first request.
func TestBuildRejectsInvalidConfig(t *testing.T) {
	// Arrange: a routable bind address, which §7 forbids (no auth).
	cfg := testConfig(t)
	cfg.ListenAddr = "0.0.0.0:8420"

	// Act
	srv, release, err := build(cfg, slog.New(slog.DiscardHandler))

	// Assert
	if err == nil {
		release()
		t.Fatal("build() = nil error for a non-loopback listen addr")
	}
	if srv != nil {
		t.Fatalf("build() returned a server alongside an error: %+v", srv)
	}
}
