//go:build e2e

// Shared harness for the P1-P12 invariant scenarios (feature-inventory.md §5).
//
// This MUST stay a _test.go file: `go test` only honours TestMain when it is
// declared in a test file. Declared in a plain .go file it compiles happily
// but is never invoked, leaving the package-level harness nil and every
// scenario nil-panicking on first use (same trap documented in
// test/blackbox/harness_test.go).
//
// The suite stands alone from test/blackbox on purpose: blackbox proves "spec
// acceptance" (§10), e2e proves "the P-invariants hold in the four stores
// themselves". Sharing code would couple two suites whose lifecycles differ.
package e2e

import (
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// Fixed endpoints and identities. DJ_MEMORY_USERNAME is "jin/e2e-test" so the
// server's own cold key layout (internal/external/thirdparty/cold/keys.go)
// places every S3 object under the dedicated prefix
// s3://vms-memory-mcp/jin/e2e-test/... which TestMain pre- and post-cleans.
const (
	// mainPort hosts the shared main instance. 8420 is left to `make start`
	// and blackbox; extra e2e instances should use 8431+ (one port per test).
	mainPort      = 8430
	opensearchURL = "http://127.0.0.1:9200"
	neo4jBoltURL  = "bolt://127.0.0.1:7687"
	neo4jUser     = "neo4j"
	neo4jPassword = "djmemory-local"

	s3Bucket   = "vms-memory-mcp"
	s3Region   = "ap-northeast-2"
	awsProfile = "vms-holdings"
	s3Username = "jin/e2e-test"
	s3Prefix   = "jin/e2e-test/"

	osContainer    = "dj-memory-opensearch"
	neo4jContainer = "dj-memory-neo4j"

	// wsName/teamName scope every e2e project key; each scenario picks its own
	// project segment via e2eKey so cross-scenario state cannot collide.
	wsName   = "e2e"
	teamName = "suite"
)

// Deterministic-wait tuning. No bare sleeps in assertions: everything polls
// through waitFor.
const (
	pollInterval  = time.Second
	healthTimeout = 90 * time.Second
	stopTimeout   = 10 * time.Second
)

// e2eKey builds the canonical per-scenario project key: {e2e}/{suite}/{proj}.
// Scenarios needing cross-project isolation (P10) mint several distinct
// project segments.
func e2eKey(project string) projectkey.Key {
	return projectkey.Key{Workspace: wsName, Team: teamName, Project: project}
}

// harness owns the process/tooling state shared by every scenario.
type harness struct {
	repoRoot string
	home     string // fresh DJ_MEMORY_HOME temp dir per run (main instance)
	bin      string // server binary, built once in TestMain

	mu      sync.Mutex // guards main + servers
	main    *server
	servers []*server // every launched instance, reaped in TestMain teardown
}

var h *harness

// TestMain: prerequisites (docker, aws, bucket), a fresh temp DJ_MEMORY_HOME,
// one server binary build, S3 prefix pre/post-clean, and teardown of every
// instance a scenario left running. Containers are NOT recreated here — `make
// e2e` brings them up healthy, and scenarios that kill them own their
// recovery (composeFresh / startContainer).
func TestMain(m *testing.M) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	root, err := filepath.Abs("../..")
	if err != nil {
		logger.Error("resolve repo root", "error", err)
		os.Exit(2)
	}
	for _, tool := range [][]string{{"docker", "version"}, {"docker", "compose", "version"}, {"aws", "--version"}, {"go", "version"}} {
		if out, err := runCmd(root, tool[0], tool[1:]...); err != nil {
			logger.Error("prerequisite missing", "tool", strings.Join(tool, " "), "error", err, "output", out)
			os.Exit(2)
		}
	}
	// Cold-store preflight: P4/P11/P12 assert against the real bucket, so a
	// missing bucket or bad credentials must fail here with an actionable
	// message, not mid-scenario as a cryptic aws CLI error.
	if out, err := runCmd("", "aws", awsArgs("s3api", "head-bucket", "--bucket", s3Bucket)...); err != nil {
		logger.Error("cold store unreachable — e2e cannot verify the S3-side invariants",
			"bucket", s3Bucket, "region", s3Region, "profile", awsProfile,
			"error", err, "output", strings.TrimSpace(out))
		os.Exit(2)
	}

	home, err := os.MkdirTemp("", "dj-memory-e2e-")
	if err != nil {
		logger.Error("create temp DJ_MEMORY_HOME", "error", err)
		os.Exit(2)
	}
	bin := filepath.Join(home, "memory-mcp-e2e")
	if out, err := runCmd(root, "go", "build", "-o", bin, "./cmd/server/http"); err != nil {
		logger.Error("go build ./cmd/server/http", "error", err, "output", out)
		if rmErr := os.RemoveAll(home); rmErr != nil {
			logger.Warn("remove temp home", "error", rmErr)
		}
		os.Exit(2)
	}
	h = &harness{repoRoot: root, home: home, bin: bin}
	logger.Info("e2e harness ready", "home", home, "bin", bin, "s3_prefix", s3Prefix)

	if out, err := h.s3CleanPrefix(); err != nil {
		logger.Warn("pre-run S3 prefix clean failed", "error", err, "output", out)
	}

	code := m.Run()

	h.stopAll()
	if out, err := h.s3CleanPrefix(); err != nil {
		logger.Warn("post-run S3 prefix clean failed — clean manually", "prefix", s3Prefix, "error", err, "output", out)
	} else {
		logger.Info("S3 prefix cleaned", "prefix", s3Prefix)
	}
	if code != 0 || os.Getenv("E2E_KEEP") != "" {
		logger.Info("keeping artifacts for inspection", "home", home)
	} else {
		if err := os.RemoveAll(home); err != nil {
			logger.Warn("remove temp home", "error", err)
		}
	}
	os.Exit(code)
}

// ---------- evidence helpers ----------

func pass(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Logf("[PASS] "+format, args...)
}

func failf(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Fatalf("[FAIL] "+format, args...)
}

// waitFor polls fn every pollInterval until true or timeout — the only legal
// way to wait in this suite. Returns the last evidence string.
func (h *harness) waitFor(t *testing.T, desc string, timeout time.Duration, fn func() (bool, string)) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		ok, ev := fn()
		last = ev
		if ok {
			return last
		}
		if time.Now().After(deadline) {
			failf(t, "%s: timed out after %s; last evidence: %s", desc, timeout, last)
		}
		time.Sleep(pollInterval)
	}
}

// ---------- subprocess helpers ----------

func runCmd(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runCmdStdout separates stdout (payload) from stderr (noise) for commands
// whose output is consumed, e.g. `aws s3 cp ... -`.
func runCmdStdout(name string, args ...string) (string, string, error) {
	cmd := exec.Command(name, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// execError carries stderr from a failed CLI payload command.
type execError struct{ msg string }

func (e *execError) Error() string { return e.msg }

// ---------- container lifecycle ----------

func (h *harness) compose(args ...string) (string, error) {
	full := append([]string{"compose", "-f", "deploy/docker-compose.yml"}, args...)
	return runCmd(h.repoRoot, "docker", full...)
}

// composeFresh recreates both derived stores with no data — the P2 "destroy
// and re-derive" tool.
//
// -v is load-bearing: the compose file declares no volumes (the derived
// stores are intentionally non-persistent), but the neo4j image declares
// VOLUME /data /logs, so every `up` leaks two anonymous volumes that a plain
// `down` leaves behind until the Docker disk fills (see the identical note in
// test/blackbox/harness_test.go and the Makefile).
func (h *harness) composeFresh(t *testing.T) {
	t.Helper()
	if out, err := h.compose("down", "-v", "--remove-orphans"); err != nil {
		failf(t, "docker compose down: %v\n%s", err, out)
	}
	if out, err := h.compose("up", "-d", "--build"); err != nil {
		failf(t, "docker compose up: %v\n%s", err, out)
	}
}

// stopContainer halts one derived store in place (failure injection for
// P1/P6/P18-style degraded scenarios). name is osContainer or neo4jContainer.
func (h *harness) stopContainer(t *testing.T, name string) {
	t.Helper()
	if out, err := runCmd("", "docker", "stop", name); err != nil {
		failf(t, "docker stop %s: %v\n%s", name, err, out)
	}
}

// startContainer restarts a container previously halted by stopContainer and
// blocks until its own health probe answers again.
func (h *harness) startContainer(t *testing.T, name string) {
	t.Helper()
	if out, err := runCmd("", "docker", "start", name); err != nil {
		failf(t, "docker start %s: %v\n%s", name, err, out)
	}
	probe := h.opensearchUp
	if name == neo4jContainer {
		probe = h.neo4jUp
	}
	h.waitFor(t, name+" healthy after restart", healthTimeout, probe)
}

// opensearchUp / neo4jUp probe the derived stores directly (not through the
// server), so scenarios can distinguish "container down" from "server lying".
func (h *harness) opensearchUp() (bool, string) {
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(opensearchURL)
	if err != nil {
		return false, "opensearch unreachable: " + err.Error()
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK, "opensearch HTTP " + resp.Status
}

func (h *harness) neo4jUp() (bool, string) {
	out, err := h.cypherErr("RETURN 1;")
	if err != nil {
		return false, "cypher-shell: " + strings.TrimSpace(out)
	}
	return true, "cypher RETURN 1 ok"
}

// ---------- server lifecycle ----------

// serverOpts configures one server instance. The zero value is not usable:
// Port is required.
type serverOpts struct {
	// Port is the loopback port to listen on. The main instance owns
	// mainPort; extras must pick a distinct port (8431+, one per test).
	Port int
	// Home overrides DJ_MEMORY_HOME. "" means the harness's shared temp home.
	// Extras that must not share hot state pass t.TempDir().
	Home string
	// Env entries are appended AFTER the standard set, so they override it —
	// including DJ_MEMORY_LISTEN_ADDR if a scenario needs a non-loopback bind
	// attempt (P8) or DJ_MEMORY_S3_BUCKET for a broken bucket (P4).
	Env map[string]string
	// Name stems the log file under the harness home; "" derives from Port.
	Name string
}

// server is one running (or exited) memory-mcp process plus its HTTP client.
type server struct {
	port     int
	baseURL  string
	home     string
	logPath  string
	cmd      *exec.Cmd
	client   *http.Client
	done     chan struct{} // closed when the process exits
	waitErr  error         // cmd.Wait result; valid only after done is closed
	stopOnce sync.Once
}

// launchServer spawns an instance with the given overrides and registers it
// for TestMain teardown. It does NOT wait: callers choose waitHealthy (normal
// boot) or waitExit (boot-failure injection, e.g. P8 non-loopback bind).
// Scenario tests that start extras should also t.Cleanup(s.stop) so a failed
// test cannot leak a listener into the next one.
func (h *harness) launchServer(t *testing.T, opts serverOpts) *server {
	t.Helper()
	if opts.Port == 0 {
		failf(t, "launchServer: opts.Port is required")
	}
	home := opts.Home
	if home == "" {
		home = h.home
	}
	name := opts.Name
	if name == "" {
		name = "server-" + strconv.Itoa(opts.Port)
	}
	logPath := filepath.Join(h.home, name+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		failf(t, "create server log %s: %v", logPath, err)
	}

	cmd := exec.Command(h.bin)
	cmd.Dir = h.repoRoot
	env := append(os.Environ(),
		"DJ_MEMORY_HOME="+home,
		"DJ_MEMORY_USERNAME="+s3Username,
		"DJ_MEMORY_S3_BUCKET="+s3Bucket,
		"DJ_MEMORY_S3_REGION="+s3Region,
		"AWS_PROFILE="+awsProfile,
		"DJ_MEMORY_OPENSEARCH_URL="+opensearchURL,
		"DJ_MEMORY_NEO4J_URL="+neo4jBoltURL,
		"DJ_MEMORY_LISTEN_ADDR=127.0.0.1:"+strconv.Itoa(opts.Port),
	)
	// Appended last: exec.Cmd keeps the LAST duplicate, so these override.
	for k, v := range opts.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		failf(t, "start server (%s): %v", name, err)
	}

	s := &server{
		port:    opts.Port,
		baseURL: "http://127.0.0.1:" + strconv.Itoa(opts.Port),
		home:    home,
		logPath: logPath,
		cmd:     cmd,
		client:  &http.Client{Timeout: 120 * time.Second},
		done:    make(chan struct{}),
	}
	// Single reaper goroutine: cmd.Wait may be called exactly once, so stop()
	// and waitExit() both observe the exit through the done channel instead.
	go func() {
		s.waitErr = cmd.Wait()
		if err := logFile.Close(); err != nil {
			_ = err // log file close failure is not evidence-bearing
		}
		close(s.done)
	}()

	h.mu.Lock()
	h.servers = append(h.servers, s)
	h.mu.Unlock()
	return s
}

// healthy reports whether /healthz answers on this instance.
func (s *server) healthy() bool {
	c := http.Client{Timeout: time.Second}
	resp, err := c.Get(s.baseURL + "/healthz")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// waitHealthy blocks until /healthz answers, failing the test on timeout
// (with the server log path, which holds the actual boot error).
func (s *server) waitHealthy(t *testing.T) {
	t.Helper()
	h.waitFor(t, "server :"+strconv.Itoa(s.port)+" healthy", healthTimeout, func() (bool, string) {
		if s.exited() {
			failf(t, "server :%d exited during boot (%v); log: %s", s.port, s.waitErr, s.logPath)
		}
		if s.healthy() {
			return true, "healthz ok"
		}
		return false, "no healthz answer yet; log: " + s.logPath
	})
}

// exited reports whether the process has terminated (without blocking).
func (s *server) exited() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// waitExit blocks up to timeout for the process to terminate on its own —
// the assertion channel for boot-failure injection (P8: non-loopback bind
// must refuse to start). Returns (exited, waitErr); waitErr is non-nil for a
// non-zero exit.
func (s *server) waitExit(timeout time.Duration) (bool, error) {
	select {
	case <-s.done:
		return true, s.waitErr
	case <-time.After(timeout):
		return false, nil
	}
}

// readLog returns the instance's captured stdout+stderr, for asserting boot
// refusals and degraded logging without guessing.
func (s *server) readLog(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(s.logPath)
	if err != nil {
		failf(t, "read server log %s: %v", s.logPath, err)
	}
	return string(data)
}

// stop terminates the instance: SIGTERM, grace period, then SIGKILL. Safe to
// call multiple times and after the process already exited.
func (s *server) stop() {
	s.stopOnce.Do(func() {
		if s.cmd == nil || s.cmd.Process == nil {
			return
		}
		_ = s.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-s.done:
		case <-time.After(stopTimeout):
			_ = s.cmd.Process.Kill()
			<-s.done
		}
	})
}

// ensureServer returns the shared main instance, starting it on first use (or
// after stopServer). Scenarios call this first; failure-injection tests that
// bounce the main instance call stopServer then ensureServer again.
func (h *harness) ensureServer(t *testing.T) *server {
	t.Helper()
	h.mu.Lock()
	main := h.main
	h.mu.Unlock()
	if main != nil && !main.exited() && main.healthy() {
		return main
	}
	if main != nil {
		main.stop()
	}
	h.ensurePortFree(t, mainPort)
	s := h.launchServer(t, serverOpts{Port: mainPort, Name: "server-main"})
	s.waitHealthy(t)
	h.mu.Lock()
	h.main = s
	h.mu.Unlock()
	return s
}

// stopServer stops the main instance (no-op when not running).
func (h *harness) stopServer() {
	h.mu.Lock()
	main := h.main
	h.main = nil
	h.mu.Unlock()
	if main != nil {
		main.stop()
	}
}

// api returns the running main instance for raw envelope work
// (h.api(t).postJSON(...)); the seed helpers in seed_test.go wrap it.
func (h *harness) api(t *testing.T) *server {
	t.Helper()
	h.mu.Lock()
	main := h.main
	h.mu.Unlock()
	if main == nil || main.exited() {
		failf(t, "main server not running — call h.ensureServer(t) first")
	}
	return main
}

// ensurePortFree makes re-runs idempotent: a listener orphaned by a previous
// crashed run is killed before we bind the same port.
func (h *harness) ensurePortFree(t *testing.T, port int) {
	t.Helper()
	portArg := ":" + strconv.Itoa(port)
	out, _ := runCmd("", "lsof", "-ti", portArg)
	pids := strings.Fields(out)
	if len(pids) == 0 {
		return
	}
	t.Logf("port %d already bound by %v — killing stale listener for an idempotent re-run", port, pids)
	for _, pid := range pids {
		_, _ = runCmd("", "kill", "-9", pid)
	}
	h.waitFor(t, "port "+strconv.Itoa(port)+" released", 15*time.Second, func() (bool, string) {
		out, _ := runCmd("", "lsof", "-ti", portArg)
		return strings.TrimSpace(out) == "", "still bound: " + strings.TrimSpace(out)
	})
}

// stopAll reaps every instance any scenario launched (TestMain teardown).
func (h *harness) stopAll() {
	h.mu.Lock()
	servers := append([]*server(nil), h.servers...)
	h.main = nil
	h.mu.Unlock()
	for _, s := range servers {
		s.stop()
	}
}
