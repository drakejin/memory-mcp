//go:build blackbox

// Shared harness for the §10 acceptance scenarios.
//
// This MUST stay a _test.go file: `go test` only honours TestMain when it is
// declared in a test file. Declared in a plain .go file it compiles happily but
// is never invoked, leaving the package-level harness nil and every scenario
// nil-panicking on first use.
package blackbox

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/search"
)

// Harness constants. DJ_MEMORY_USERNAME is set to "jin/blackbox-test" so the
// scaffold's own cold key layout (cold/keys.go) places every S3 object under
// the reserved prefix s3://vms-memory-mcp/jin/blackbox-test/... (§10).
const (
	baseURL       = "http://127.0.0.1:8420"
	opensearchURL = "http://127.0.0.1:9200"
	neo4jBoltURL  = "bolt://127.0.0.1:7687"
	neo4jUser     = "neo4j"
	neo4jPassword = "djmemory-local"

	s3Bucket   = "vms-memory-mcp"
	s3Region   = "ap-northeast-2"
	awsProfile = "vms-holdings"
	s3Username = "jin/blackbox-test"
	s3Prefix   = "jin/blackbox-test/"

	osContainer    = "dj-memory-opensearch"
	neo4jContainer = "dj-memory-neo4j"

	wsName   = "blackbox"
	teamName = "qa"
	projName = "v2"
)

// hotKey addresses the single project every scenario uses.
var hotKey = hotstore.ProjectKey{Workspace: wsName, Team: teamName, Project: projName}

// harness owns process/tooling state shared across the ordered scenarios.
type harness struct {
	repoRoot      string
	home          string // fresh DJ_MEMORY_HOME temp dir per run
	bin           string
	serverLogPath string
	server        *exec.Cmd
	client        *http.Client
	ready         bool // scenario 1 completed; later scenarios depend on it

	// Cross-scenario acceptance state.
	epIDs      []string // scenario 2 episode ids (order = koreanEpisodeTexts)
	noriHitID  string   // id that noriQuery must keep hitting
	chainIDs   []string // scenario 3 supersede chain, oldest first
	fact3ID    string
	docSHA     string
	docNodeID  string
	chunkCount int
	oldID      string // scenario 6 aged consolidated episode
	staleID    string // scenario 6 stale unconsolidated episode
	degradedID string // scenario 7 episode written while OpenSearch was down
}

var h *harness

// TestMain provides idempotent setup/teardown: a fresh DJ_MEMORY_HOME temp
// dir, S3 prefix pre-clean (crash leftovers) and post-clean, and server
// shutdown. Containers are managed inside the scenarios (1, 5, 7) because
// their lifecycle IS the subject under test.
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
	home, err := os.MkdirTemp("", "dj-memory-blackbox-")
	if err != nil {
		logger.Error("create temp DJ_MEMORY_HOME", "error", err)
		os.Exit(2)
	}
	h = &harness{repoRoot: root, home: home, client: &http.Client{Timeout: 60 * time.Second}}
	logger.Info("blackbox harness ready", "home", home, "s3_prefix", s3Prefix)

	// Cold-store preflight. Scenarios 4/6/8 assert against the real bucket, so
	// a missing bucket or bad credentials must fail here with an actionable
	// message rather than as a cryptic mid-scenario aws CLI error.
	if out, err := runCmd("", "aws", awsArgs("s3api", "head-bucket", "--bucket", s3Bucket)...); err != nil {
		logger.Error("cold store unreachable — blackbox cannot verify §10.4/6/8",
			"bucket", s3Bucket, "region", s3Region, "profile", awsProfile,
			"error", err, "output", strings.TrimSpace(out),
			"fix", "aws --profile "+awsProfile+" s3api create-bucket --bucket "+s3Bucket+
				" --region "+s3Region+" --create-bucket-configuration LocationConstraint="+s3Region+
				" && aws --profile "+awsProfile+" s3api put-bucket-versioning --bucket "+s3Bucket+
				" --versioning-configuration Status=Enabled")
		// Nothing ran yet, so drop the temp home rather than leak one per
		// invocation (the harness must stay re-runnable).
		if err := os.RemoveAll(home); err != nil {
			logger.Warn("remove temp home", "error", err)
		}
		os.Exit(2)
	}
	logger.Info("cold store reachable", "bucket", s3Bucket, "region", s3Region)

	if out, err := h.s3CleanPrefix(); err != nil {
		logger.Warn("pre-run S3 prefix clean failed", "error", err, "output", out)
	}

	code := m.Run()

	h.stopServer(logger)
	if out, err := h.s3CleanPrefix(); err != nil {
		logger.Warn("post-run S3 prefix clean failed — clean manually", "prefix", s3Prefix, "error", err, "output", out)
	} else {
		logger.Info("S3 prefix cleaned", "prefix", s3Prefix)
	}
	if code != 0 || os.Getenv("BLACKBOX_KEEP") != "" {
		logger.Info("keeping artifacts for inspection", "home", home, "server_log", h.serverLogPath)
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

// requireReady aborts scenarios that cannot run because startup failed.
func requireReady(t *testing.T) {
	t.Helper()
	if h == nil || !h.ready {
		failf(t, "prerequisite: scenario 1 (startup) did not complete; server log: %s", h.serverLogPath)
	}
}

// waitFor polls fn every 2s until true or timeout (deterministic wait, no
// bare sleeps in assertions). Returns the last evidence string.
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
		time.Sleep(2 * time.Second)
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
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func (h *harness) compose(args ...string) (string, error) {
	full := append([]string{"compose", "-f", "deploy/docker-compose.yml"}, args...)
	return runCmd(h.repoRoot, "docker", full...)
}

func (h *harness) composeFresh(t *testing.T) {
	t.Helper()
	if out, err := h.compose("down", "--remove-orphans"); err != nil {
		failf(t, "docker compose down: %v\n%s", err, out)
	}
	if out, err := h.compose("up", "-d", "--build"); err != nil {
		failf(t, "docker compose up: %v\n%s", err, out)
	}
}

// ---------- container probes ----------

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
	out, err := h.cypher("RETURN 1;")
	if err != nil {
		return false, "cypher-shell: " + strings.TrimSpace(out)
	}
	return true, "cypher RETURN 1 ok"
}

// cypher runs a statement through cypher-shell inside the neo4j container —
// the independent Neo4j traversal channel required by §10 scenario 3.
func (h *harness) cypher(query string) (string, error) {
	return runCmd("", "docker", "exec", neo4jContainer,
		"cypher-shell", "-u", neo4jUser, "-p", neo4jPassword, "--format", "plain", query)
}

// cypherCount runs a RETURN count(...) query and parses the numeric result.
func (h *harness) cypherCount(query string) (int, string) {
	out, err := h.cypher(query)
	if err != nil {
		return -1, "cypher error: " + strings.TrimSpace(out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if n, err := strconv.Atoi(strings.TrimSpace(lines[i])); err == nil {
			return n, strings.TrimSpace(out)
		}
	}
	return -1, "no numeric row in: " + strings.TrimSpace(out)
}

// ---------- server lifecycle ----------

func (h *harness) serverResponding() bool {
	c := http.Client{Timeout: time.Second}
	resp, err := c.Get(baseURL + "/healthz")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// ensurePortFree makes re-runs idempotent: a server orphaned by a previous
// crashed run is killed before we start ours.
func (h *harness) ensurePortFree(t *testing.T) {
	t.Helper()
	if !h.serverResponding() {
		return
	}
	t.Logf("port 8420 already serving — killing stale listener for an idempotent re-run")
	out, _ := runCmd("", "lsof", "-ti", ":8420")
	for _, pid := range strings.Fields(out) {
		_, _ = runCmd("", "kill", "-9", pid)
	}
	h.waitFor(t, "port 8420 released", 15*time.Second, func() (bool, string) {
		return !h.serverResponding(), "still responding"
	})
}

func (h *harness) startServer(t *testing.T) {
	t.Helper()
	h.bin = filepath.Join(h.home, "memory-mcp-blackbox")
	if out, err := runCmd(h.repoRoot, "go", "build", "-o", h.bin, "./cmd/memory-mcp"); err != nil {
		failf(t, "go build ./cmd/memory-mcp: %v\n%s", err, out)
	}
	h.serverLogPath = filepath.Join(h.home, "server.log")
	logFile, err := os.Create(h.serverLogPath)
	if err != nil {
		failf(t, "create server log: %v", err)
	}
	cmd := exec.Command(h.bin)
	cmd.Dir = h.repoRoot
	cmd.Env = append(os.Environ(),
		"DJ_MEMORY_HOME="+h.home,
		"DJ_MEMORY_USERNAME="+s3Username,
		"DJ_MEMORY_S3_BUCKET="+s3Bucket,
		"DJ_MEMORY_S3_REGION="+s3Region,
		"AWS_PROFILE="+awsProfile,
		"DJ_MEMORY_OPENSEARCH_URL="+opensearchURL,
		"DJ_MEMORY_NEO4J_URL="+neo4jBoltURL,
	)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		failf(t, "start server: %v", err)
	}
	h.server = cmd
}

func (h *harness) stopServer(logger *slog.Logger) {
	if h.server == nil || h.server.Process == nil {
		return
	}
	_ = h.server.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _, _ = h.server.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		logger.Warn("server did not exit on SIGTERM; killing")
		_ = h.server.Process.Kill()
	}
	h.server = nil
}

// ---------- HTTP helpers ----------

// envelope mirrors the mandatory {success, data, error} response shape (§7).
type envelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   string          `json:"error"`
}

func (h *harness) do(t *testing.T, method, path string, body io.Reader, contentType string) (int, envelope) {
	t.Helper()
	req, err := http.NewRequest(method, baseURL+path, body)
	if err != nil {
		failf(t, "%s %s: build request: %v", method, path, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		failf(t, "%s %s: transport error (server down?): %v; server log: %s", method, path, err, h.serverLogPath)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		failf(t, "%s %s: read body: %v", method, path, err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		failf(t, "%s %s: HTTP %d is not the {success,data,error} envelope (§7): %.400s", method, path, resp.StatusCode, raw)
	}
	return resp.StatusCode, env
}

func (h *harness) getJSON(t *testing.T, path string) (int, envelope) {
	t.Helper()
	return h.do(t, http.MethodGet, path, nil, "")
}

func (h *harness) postJSON(t *testing.T, path string, body any) (int, envelope) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		failf(t, "marshal body for %s: %v", path, err)
	}
	return h.do(t, http.MethodPost, path, bytes.NewReader(raw), "application/json")
}

func (h *harness) postMultipart(t *testing.T, path, field, filename string, data []byte) (int, envelope) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile(field, filename)
	if err != nil {
		failf(t, "multipart form: %v", err)
	}
	if _, err := fw.Write(data); err != nil {
		failf(t, "multipart write: %v", err)
	}
	if err := w.Close(); err != nil {
		failf(t, "multipart close: %v", err)
	}
	return h.do(t, http.MethodPost, path, &buf, w.FormDataContentType())
}

// getRawBytes fetches a non-envelope endpoint (document original download).
func (h *harness) getRawBytes(t *testing.T, path string) (int, []byte) {
	t.Helper()
	resp, err := h.client.Get(baseURL + path)
	if err != nil {
		failf(t, "GET %s: transport error: %v", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		failf(t, "GET %s: read body: %v", path, err)
	}
	return resp.StatusCode, raw
}

// decodeData unmarshals the envelope data payload into out.
func decodeData(t *testing.T, env envelope, out any) {
	t.Helper()
	if err := json.Unmarshal(env.Data, out); err != nil {
		failf(t, "decode envelope data into %T: %v; data: %.400s", out, err, env.Data)
	}
}

func (h *harness) projPath() string {
	return "/v1/" + wsName + "/" + teamName + "/" + projName
}

// searchEpisodes runs the project-scoped episodic search and decodes hits
// when the index answered 200.
func (h *harness) searchEpisodes(t *testing.T, q string) (int, []search.Hit) {
	t.Helper()
	qs := url.Values{"q": {q}}.Encode()
	status, env := h.getJSON(t, h.projPath()+"/episodes/search?"+qs)
	if status != http.StatusOK {
		return status, nil
	}
	var hits []search.Hit
	decodeData(t, env, &hits)
	return status, hits
}

// hitIDs projects hit record ids for evidence lines.
func hitIDs(hits []search.Hit) []string {
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		ids = append(ids, hit.Record.ID)
	}
	return ids
}

// ---------- hot store paths / fixture access ----------

func (h *harness) episodicHotPath() string {
	return filepath.Join(h.home, "episodic", wsName, teamName, projName+".json")
}

func (h *harness) knowledgeHotPath() string {
	return filepath.Join(h.home, "knowledge", wsName, teamName, projName+".json")
}

// store opens the canonical hot store rooted at the harness home. Used only
// for fixture injection (marking an episode consolidated, §10.6) and for
// structured hot-file assertions; the format stays correct by construction.
func (h *harness) store() hotstore.Store {
	return hotstore.New(h.home, hotstore.SystemClock{})
}

// ---------- S3 via aws CLI (independent verification channel) ----------

func awsArgs(args ...string) []string {
	return append([]string{"--profile", awsProfile, "--region", s3Region}, args...)
}

func (h *harness) s3Exists(key string) (bool, string) {
	out, err := runCmd("", "aws", awsArgs("s3api", "head-object", "--bucket", s3Bucket, "--key", key)...)
	if err != nil {
		return false, strings.TrimSpace(out)
	}
	return true, "head-object ok: s3://" + s3Bucket + "/" + key
}

func (h *harness) s3Cat(key string) (string, error) {
	stdout, stderr, err := runCmdStdout("aws", awsArgs("s3", "cp", "s3://"+s3Bucket+"/"+key, "-")...)
	if err != nil {
		return "", &execError{msg: strings.TrimSpace(stderr)}
	}
	return stdout, nil
}

func (h *harness) s3CleanPrefix() (string, error) {
	return runCmd("", "aws", awsArgs("s3", "rm", "--recursive", "s3://"+s3Bucket+"/"+s3Prefix)...)
}

// execError carries stderr from a failed CLI payload command.
type execError struct{ msg string }

func (e *execError) Error() string { return e.msg }
