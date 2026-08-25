//go:build e2e

// P8 — input containment (feature-inventory.md §2 P8): traversal-shaped
// project keys and malformed content addresses die at the HTTP boundary with
// 400 before any store is touched, and a server configured to bind a routable
// interface refuses to boot entirely.
//
// Store-side proof strategy: for every rejected write the test computes the
// exact file a successful (uncontained) write would have created — including
// the traversal-resolved paths OUTSIDE the hot home — and asserts it does not
// exist, then sweeps the whole home for the fixture marker. The boot-refusal
// leg uses an isolated extra instance whose home must stay untouched, proving
// the refusal happened in config validation, before storage was opened.
package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/handler/app/httpserver/apierr"
)

const (
	// p08Port is this file's dedicated extra-instance port (8431+, one per test).
	p08Port = 8438
	// p08TravMarker names every fixture segment so a filesystem sweep can
	// prove no rejected key ever materialized anywhere under the home.
	p08TravMarker = "p08trav"
	// p08ProbeBody is a fully valid episode body: the only invalid thing about
	// each request below is its project key, so a 400 is attributable to the
	// key alone.
	p08ProbeBody = `{"kind":"event","actor":"agent","text":"p08 traversal probe","entities":[]}`
	// p08BootRefusalMsg is the config validator's refusal (x/config §7).
	p08BootRefusalMsg = "listen addr must bind loopback only"
)

// p08ExitWait bounds the wait for the refused boot to exit on its own.
const p08ExitWait = 30 * time.Second

// p08Request performs one raw request WITHOUT envelope expectations: rejected
// paths may be answered by the router (chi plain-text 404) rather than a
// handler, and the assertion helpers below decide per case.
func p08Request(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	s := h.api(t)
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, s.baseURL+path, rd)
	if err != nil {
		failf(t, "%s %s: build request: %v", method, path, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		failf(t, "%s %s: transport error: %v; server log: %s", method, path, err, s.logPath)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		failf(t, "%s %s: read body: %v", method, path, err)
	}
	return resp.StatusCode, raw
}

// p08MustReject asserts one rejected request: exact status 400 with the
// {success:false, error:{code:invalid_request}} envelope.
func p08MustReject(t *testing.T, what string, status int, raw []byte) {
	t.Helper()
	if status != http.StatusBadRequest {
		failf(t, "%s: want HTTP 400, got %d: %.300s", what, status, raw)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		failf(t, "%s: 400 body is not the {success,data,error} envelope: %v: %.300s", what, err, raw)
	}
	if env.Success || env.Error == nil || env.Error.Code != apierr.CodeInvalidRequest {
		failf(t, "%s: want success=false code=%s, got success=%v error=%q", what, apierr.CodeInvalidRequest, env.Success, env.Error)
	}
}

// TestP08_TraversalProjectKeys400 fires a battery of traversal-shaped and
// charset-violating project keys at the write path (plus one read path) and
// proves 400 + zero filesystem effect, including at the traversal-resolved
// paths outside the episodic tree.
func TestP08_TraversalProjectKeys400(t *testing.T) {
	h.ensureServer(t)

	// Each case names the escape it attempts. rawWs/rawTeam/rawProj build the
	// URL (percent-encoding included); decoded* is what the segment means to a
	// filesystem — the material for the containment targets below.
	cases := []struct {
		name                              string
		rawWs, rawTeam, rawProj           string
		decodedWs, decodedTeam, decodedPj string
	}{
		{
			name:  "workspace is dot-dot",
			rawWs: "..", rawTeam: p08TravMarker, rawProj: "esc1",
			decodedWs: "..", decodedTeam: p08TravMarker, decodedPj: "esc1",
		},
		{
			name:  "team is dot-dot",
			rawWs: "e2e", rawTeam: "..", rawProj: p08TravMarker + "-esc2",
			decodedWs: "e2e", decodedTeam: "..", decodedPj: p08TravMarker + "-esc2",
		},
		{
			name:  "project is dot-dot",
			rawWs: "e2e", rawTeam: p08TravMarker, rawProj: "..",
			decodedWs: "e2e", decodedTeam: p08TravMarker, decodedPj: "..",
		},
		{
			// Passes the handler's charset gate ([a-z0-9._-]) but must die on
			// the domain's projectkey.Validate dot-segment rule — proving the
			// deeper layer holds even where the transport gate is looser.
			name:  "embedded dot-dot",
			rawWs: "e2e", rawTeam: p08TravMarker, rawProj: p08TravMarker + "..esc4",
			decodedWs: "e2e", decodedTeam: p08TravMarker, decodedPj: p08TravMarker + "..esc4",
		},
		{
			name:  "uppercase charset violation",
			rawWs: "e2e", rawTeam: p08TravMarker, rawProj: "P08TRAV-ESC5",
			decodedWs: "e2e", decodedTeam: p08TravMarker, decodedPj: "P08TRAV-ESC5",
		},
		{
			name:  "percent-encoded dot-dot",
			rawWs: "%2e%2e", rawTeam: p08TravMarker, rawProj: "esc6",
			decodedWs: "..", decodedTeam: p08TravMarker, decodedPj: "esc6",
		},
		{
			name:  "percent-encoded slash smuggled into one segment",
			rawWs: "e2e", rawTeam: p08TravMarker, rawProj: p08TravMarker + "%2fesc7",
			decodedWs: "e2e", decodedTeam: p08TravMarker, decodedPj: p08TravMarker + "/esc7",
		},
		{
			// The only shape that could actually leave the home: multi-level
			// encoded traversal in a single segment.
			name:  "percent-encoded multi-level escape",
			rawWs: "..%2f..%2f..%2ftmp%2f" + p08TravMarker + "-out", rawTeam: p08TravMarker, rawProj: "esc8",
			decodedWs: "../../../tmp/" + p08TravMarker + "-out", decodedTeam: p08TravMarker, decodedPj: "esc8",
		},
	}

	targets := make([]string, 0, len(cases))
	for _, tc := range cases {
		path := "/v1/" + tc.rawWs + "/" + tc.rawTeam + "/" + tc.rawProj + "/episodes"
		status, raw := p08Request(t, http.MethodPost, path, p08ProbeBody)
		p08MustReject(t, tc.name+" (POST "+path+")", status, raw)
		// The exact file an uncontained write would have created. filepath.Join
		// resolves the dot segments, i.e. it computes the escape destination.
		targets = append(targets, filepath.Join(h.home, "episodic", tc.decodedWs, tc.decodedTeam, tc.decodedPj+".json"))
	}
	pass(t, "all %d traversal/charset keys answered exactly 400 %s", len(cases), apierr.CodeInvalidRequest)

	// The read path validates the same gate before touching any store.
	searchPath := "/v1/../" + p08TravMarker + "/esc9/episodes/search?q=" + p08TravMarker
	status, raw := p08Request(t, http.MethodGet, searchPath, "")
	p08MustReject(t, "traversal key on GET search", status, raw)

	// Containment, part 1: none of the would-be files exist — including the
	// ones that resolve OUTSIDE the home.
	for _, target := range targets {
		if _, err := os.Stat(target); !errors.Is(err, fs.ErrNotExist) {
			failf(t, "containment breach: %s exists (stat err=%v) after a rejected write", target, err)
		}
	}
	// Containment, part 2: sweep the whole home — no file or directory NAME
	// anywhere carries the fixture marker. (Log contents may mention it; names
	// are what a materialized key would have produced.)
	var leaked []string
	err := filepath.WalkDir(h.home, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.Contains(strings.ToLower(d.Name()), p08TravMarker) {
			leaked = append(leaked, path)
		}
		return nil
	})
	if err != nil {
		failf(t, "walk %s: %v", h.home, err)
	}
	if len(leaked) != 0 {
		failf(t, "containment breach: rejected keys materialized on disk: %v", leaked)
	}
	pass(t, "store containment: no traversal target exists, home sweep found no %q entries", p08TravMarker)
}

// TestP08_MalformedSHA400 proves the content-address gate: every malformed sha
// is 400 invalid_request on both document read routes, a WELL-formed but
// unknown sha is 404 not_found (the 400 is about form, not existence), and the
// blob cache directory gains nothing from any of it.
func TestP08_MalformedSHA400(t *testing.T) {
	h.ensureServer(t)

	badSHAs := []struct{ name, sha string }{
		{"too short", "deadbeef"},
		{"one hex digit short", strings.Repeat("a", 63)},
		{"uppercase hex", strings.Repeat("A", 64)},
		{"right length, not hex", strings.Repeat("g", 64)},
		{"traversal-shaped", "..%2f..%2fetc%2fpasswd"},
	}
	for _, tc := range badSHAs {
		for _, route := range []string{"/v1/documents/" + tc.sha, "/v1/documents/" + tc.sha + "/chunks"} {
			status, raw := p08Request(t, http.MethodGet, route, "")
			p08MustReject(t, tc.name+" (GET "+route+")", status, raw)
		}
	}
	pass(t, "all %d malformed shas answered exactly 400 on both document routes", len(badSHAs))

	// Contrast case: valid form, absent content — must be 404 not_found, both
	// locally and in cold (the S3 probe is what makes this deterministic).
	sum := sha256.Sum256([]byte("p08 absent contrast fixture"))
	absentSHA := hex.EncodeToString(sum[:])
	status, raw := p08Request(t, http.MethodGet, "/v1/documents/"+absentSHA, "")
	if status != http.StatusNotFound {
		failf(t, "well-formed absent sha: want HTTP 404, got %d: %.300s", status, raw)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		failf(t, "absent sha 404 body is not the envelope: %v: %.300s", err, raw)
	}
	if env.Success || env.Error == nil || env.Error.Code != apierr.CodeNotFound {
		failf(t, "absent sha: want success=false code=%s, got %q", apierr.CodeNotFound, env.Error)
	}
	pass(t, "well-formed unknown sha is 404 %s — the 400s above reject form, not existence", apierr.CodeNotFound)

	// Store side: reads must write nothing. The cache directory may hold other
	// scenarios' blobs, so assert none of THIS test's probe names appear.
	blobDir := filepath.Join(h.home, "blobs")
	entries, err := os.ReadDir(blobDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		failf(t, "read blob cache dir %s: %v", blobDir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == absentSHA || strings.Contains(name, "passwd") {
			failf(t, "blob cache gained %q from a read-only probe", name)
		}
		for _, tc := range badSHAs {
			if name == tc.sha {
				failf(t, "blob cache gained the malformed address %q", name)
			}
		}
	}
	pass(t, "blob cache untouched by every rejected or missing read")
}

// TestP08_NonLoopbackBindRefusesBoot launches an extra instance configured to
// bind 0.0.0.0 and proves the process refuses to start: non-zero exit, the
// config validator's message in the log, nothing listening, and an untouched
// home (the refusal happened before storage was opened — which also means this
// instance never got far enough to rehydrate the shared containers from its
// empty home).
func TestP08_NonLoopbackBindRefusesBoot(t *testing.T) {
	home := t.TempDir()
	s := h.launchServer(t, serverOpts{
		Port: p08Port,
		Home: home,
		Name: "p08-nonloopback",
		Env: map[string]string{
			"DJ_MEMORY_LISTEN_ADDR": "0.0.0.0:" + strconv.Itoa(p08Port),
		},
	})
	t.Cleanup(s.stop)

	exited, waitErr := s.waitExit(p08ExitWait)
	if !exited {
		failf(t, "server with a non-loopback bind is still running after %s — it must refuse to boot; log: %s", p08ExitWait, s.logPath)
	}
	if waitErr == nil {
		failf(t, "non-loopback boot exited 0 — a refusal must be a non-zero exit; log: %s", s.logPath)
	}
	log := s.readLog(t)
	if !strings.Contains(log, p08BootRefusalMsg) {
		failf(t, "boot log does not state the refusal %q:\n%s", p08BootRefusalMsg, log)
	}
	if s.healthy() {
		failf(t, "a refused boot is answering /healthz on port %d", p08Port)
	}

	// The refusal happened in config validation: the isolated home was never
	// initialized — no manifest, no episodic tree, no blob cache.
	entries, err := os.ReadDir(home)
	if err != nil {
		failf(t, "read refused instance home %s: %v", home, err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		failf(t, "refused boot touched its home before dying: %v", names)
	}
	pass(t, "non-loopback bind refused: exit=%v, log states %q, nothing listening, home untouched", waitErr, p08BootRefusalMsg)
}
