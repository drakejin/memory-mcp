//go:build e2e

// Four-store inspectors (feature-inventory.md §5): every probe reads its
// store DIRECTLY — hot JSON off the filesystem, OpenSearch over raw HTTP,
// Neo4j through cypher-shell inside the container, S3 through the aws CLI —
// never through the server under test. Verifying the server's self-reporting
// via the server would be circular; persistence is proven at the store.
package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	episodemem "github.com/drakejin/memory-mcp/internal/external/memory/episode"
	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/service/knowledge"
	"github.com/drakejin/memory-mcp/internal/service/rehydrate"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
)

// osIndex is the single episodic index every project shares.
const osIndex = episodemem.IndexName

// Neo4j schema constants, mirrored from the unexported values in
// internal/external/memory/knowledge/graph.go (nodeLabel/relType). The
// duplication is deliberate: this channel must stay independent of the
// adapter under test. If graph.go ever renames them, neoNodeCount hitting 0
// while hot holds nodes is exactly the drift these tests exist to catch.
const (
	neoNodeLabel = "KnowledgeNode"
	neoRelType   = "REL"
)

// ---------- hot store (canonical JSON, parsed directly) ----------

// The *At forms take an explicit home so scenarios can inspect the hot store
// of an extra instance (e.g. the P4 broken-bucket server); the harness
// methods bind the shared main home.

func hotEpisodePathAt(home string, key projectkey.Key) string {
	return filepath.Join(home, "episodic", key.Workspace, key.Team, key.Project+".json")
}

func hotKnowledgePathAt(home string, key projectkey.Key) string {
	return filepath.Join(home, "knowledge", key.Workspace, key.Team, key.Project+".json")
}

func manifestPathAt(home string) string {
	return filepath.Join(home, "manifest.json")
}

func (h *harness) hotEpisodePath(key projectkey.Key) string   { return hotEpisodePathAt(h.home, key) }
func (h *harness) hotKnowledgePath(key projectkey.Key) string { return hotKnowledgePathAt(h.home, key) }
func (h *harness) manifestPath() string                       { return manifestPathAt(h.home) }

// hotEpisodesAt parses the project's episodic hot file. A missing file is an
// empty slice (that is the canonical "no records yet" state); unparseable
// bytes fail the test — the canonical store must never hold garbage.
func hotEpisodesAt(t *testing.T, home string, key projectkey.Key) []episode.Record {
	t.Helper()
	path := hotEpisodePathAt(home, key)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return []episode.Record{}
	}
	if err != nil {
		failf(t, "read hot episodic file %s: %v", path, err)
	}
	var recs []episode.Record
	if err := json.Unmarshal(data, &recs); err != nil {
		failf(t, "hot episodic file %s is not a JSON record array: %v", path, err)
	}
	return recs
}

func (h *harness) hotEpisodes(t *testing.T, key projectkey.Key) []episode.Record {
	t.Helper()
	return hotEpisodesAt(t, h.home, key)
}

// hotEpisodeByID scans the main-home hot file for one record.
func (h *harness) hotEpisodeByID(t *testing.T, key projectkey.Key, id string) (episode.Record, bool) {
	t.Helper()
	for _, rec := range h.hotEpisodes(t, key) {
		if rec.ID == id {
			return rec, true
		}
	}
	return episode.Record{}, false
}

// hotKnowledgeAt parses the project's knowledge hot document. Missing file is
// the zero Graph.
func hotKnowledgeAt(t *testing.T, home string, key projectkey.Key) knowledge.Graph {
	t.Helper()
	path := hotKnowledgePathAt(home, key)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return knowledge.Graph{}
	}
	if err != nil {
		failf(t, "read hot knowledge file %s: %v", path, err)
	}
	var g knowledge.Graph
	if err := json.Unmarshal(data, &g); err != nil {
		failf(t, "hot knowledge file %s is not a graph document: %v", path, err)
	}
	return g
}

func (h *harness) hotKnowledge(t *testing.T, key projectkey.Key) knowledge.Graph {
	t.Helper()
	return hotKnowledgeAt(t, h.home, key)
}

// hotKnowledgeNode scans the main-home knowledge document for one node.
func (h *harness) hotKnowledgeNode(t *testing.T, key projectkey.Key, id string) (knowledge.Node, bool) {
	t.Helper()
	for _, n := range h.hotKnowledge(t, key).Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return knowledge.Node{}, false
}

// hotManifestAt parses manifest.json. Missing file is an empty manifest.
func hotManifestAt(t *testing.T, home string) rehydrate.Manifest {
	t.Helper()
	path := manifestPathAt(home)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return rehydrate.Manifest{Files: map[string]rehydrate.FileState{}, Indexes: map[string]rehydrate.IndexState{}}
	}
	if err != nil {
		failf(t, "read manifest %s: %v", path, err)
	}
	var m rehydrate.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		failf(t, "manifest %s is not valid JSON: %v", path, err)
	}
	return m
}

func (h *harness) hotManifest(t *testing.T) rehydrate.Manifest {
	t.Helper()
	return hotManifestAt(t, h.home)
}

// readFileRaw returns a file's exact bytes — for byte-level format assertions
// (P9 RFC3339 spelling) that a decode-then-compare would launder.
func readFileRaw(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		failf(t, "read %s: %v", path, err)
	}
	return data
}

// ---------- OpenSearch (raw HTTP, bypassing the server) ----------

// osHit is one raw _search hit: composite _id ("ws/team/proj#ULID") plus the
// stored source document for field-level diffs (P2).
type osHit struct {
	ID     string
	Source json.RawMessage
}

// osResult is a decoded _search response.
type osResult struct {
	Total int
	Hits  []osHit
}

// osRequest performs one raw OpenSearch call. found=false means the index
// does not exist (HTTP 404) — a distinct, assertable state (P1/P2), not an
// error.
func (h *harness) osRequest(t *testing.T, method, path, body string) (status int, raw []byte) {
	t.Helper()
	req, err := http.NewRequest(method, opensearchURL+path, strings.NewReader(body))
	if err != nil {
		failf(t, "opensearch %s %s: build request: %v", method, path, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	c := http.Client{Timeout: 15 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		failf(t, "opensearch %s %s: transport error (container down? use opensearchUp to probe): %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err = io.ReadAll(resp.Body)
	if err != nil {
		failf(t, "opensearch %s %s: read body: %v", method, path, err)
	}
	return resp.StatusCode, raw
}

// osIndexExists checks the episodic index's existence directly.
func (h *harness) osIndexExists(t *testing.T) bool {
	t.Helper()
	status, _ := h.osRequest(t, http.MethodHead, "/"+osIndex, "")
	return status == http.StatusOK
}

// osRefresh forces an index refresh so _search sees every prior write without
// waiting out the refresh interval. A missing index is tolerated.
func (h *harness) osRefresh(t *testing.T) {
	t.Helper()
	status, raw := h.osRequest(t, http.MethodPost, "/"+osIndex+"/_refresh", "")
	if status != http.StatusOK && status != http.StatusNotFound {
		failf(t, "opensearch _refresh: HTTP %d: %.300s", status, raw)
	}
}

// osSearchRaw runs a raw query-DSL body against the episodic index. Callers
// own the body (including "size"); found=false means the index is absent.
func (h *harness) osSearchRaw(t *testing.T, body string) (osResult, bool) {
	t.Helper()
	status, raw := h.osRequest(t, http.MethodPost, "/"+osIndex+"/_search", body)
	if status == http.StatusNotFound {
		return osResult{}, false
	}
	if status != http.StatusOK {
		failf(t, "opensearch _search: HTTP %d: %.300s", status, raw)
	}
	var parsed struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
			Hits []struct {
				ID     string          `json:"_id"`
				Source json.RawMessage `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		failf(t, "opensearch _search: decode response: %v: %.300s", err, raw)
	}
	res := osResult{Total: parsed.Hits.Total.Value, Hits: make([]osHit, 0, len(parsed.Hits.Hits))}
	for _, hit := range parsed.Hits.Hits {
		res.Hits = append(res.Hits, osHit{ID: hit.ID, Source: hit.Source})
	}
	return res, true
}

// osProjectCount counts the project's indexed documents (term filters on the
// scope keywords). found=false means the index is absent.
func (h *harness) osProjectCount(t *testing.T, key projectkey.Key) (int, bool) {
	t.Helper()
	body := fmt.Sprintf(`{"query":{"bool":{"filter":[{"term":{"workspace":%q}},{"term":{"team":%q}},{"term":{"project":%q}}]}}}`,
		key.Workspace, key.Team, key.Project)
	status, raw := h.osRequest(t, http.MethodPost, "/"+osIndex+"/_count", body)
	if status == http.StatusNotFound {
		return 0, false
	}
	if status != http.StatusOK {
		failf(t, "opensearch _count: HTTP %d: %.300s", status, raw)
	}
	var parsed struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		failf(t, "opensearch _count: decode response: %v: %.300s", err, raw)
	}
	return parsed.Count, true
}

// osDoc fetches one indexed episode by its composite _id (realtime GET — no
// refresh needed). found=false covers both a missing doc and a missing index.
func (h *harness) osDoc(t *testing.T, key projectkey.Key, id string) (json.RawMessage, bool) {
	t.Helper()
	docID := url.PathEscape(key.String() + "#" + id)
	status, raw := h.osRequest(t, http.MethodGet, "/"+osIndex+"/_doc/"+docID, "")
	if status == http.StatusNotFound {
		return nil, false
	}
	if status != http.StatusOK {
		failf(t, "opensearch _doc %s#%s: HTTP %d: %.300s", key, id, status, raw)
	}
	var parsed struct {
		Found  bool            `json:"found"`
		Source json.RawMessage `json:"_source"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		failf(t, "opensearch _doc: decode response: %v: %.300s", err, raw)
	}
	if !parsed.Found {
		return nil, false
	}
	return parsed.Source, true
}

// ---------- Neo4j (cypher-shell inside the container) ----------

// cypherErr runs a statement through cypher-shell, returning combined output
// and any error — the probe form (container may be down).
func (h *harness) cypherErr(query string) (string, error) {
	return runCmd("", "docker", "exec", neo4jContainer,
		"cypher-shell", "-u", neo4jUser, "-p", neo4jPassword, "--format", "plain", query)
}

// cypher runs a statement and fails the test on error.
func (h *harness) cypher(t *testing.T, query string) string {
	t.Helper()
	out, err := h.cypherErr(query)
	if err != nil {
		failf(t, "cypher-shell %q: %v\n%s", query, err, out)
	}
	return out
}

// cypherCount runs a RETURN count(...) query and parses the numeric result.
func (h *harness) cypherCount(t *testing.T, query string) int {
	t.Helper()
	out := h.cypher(t, query)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if n, err := strconv.Atoi(strings.TrimSpace(lines[i])); err == nil {
			return n
		}
	}
	failf(t, "cypher %q: no numeric row in output:\n%s", query, out)
	return -1 // unreachable
}

// cypherLit quotes s as a single-quoted Cypher string literal.
func cypherLit(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return "'" + s + "'"
}

// neoScope renders the project-scope property map fragment used by every
// mirrored node: {ws, team, proj} (see knowledgemem's MERGE key).
func neoScope(key projectkey.Key) string {
	return fmt.Sprintf("ws: %s, team: %s, proj: %s",
		cypherLit(key.Workspace), cypherLit(key.Team), cypherLit(key.Project))
}

// neoNodeCount counts the project's mirrored knowledge nodes.
func (h *harness) neoNodeCount(t *testing.T, key projectkey.Key) int {
	t.Helper()
	return h.cypherCount(t, fmt.Sprintf("MATCH (n:%s {%s}) RETURN count(n);", neoNodeLabel, neoScope(key)))
}

// neoEdgeCount counts the project's mirrored edges; rel "" counts all.
func (h *harness) neoEdgeCount(t *testing.T, key projectkey.Key, rel string) int {
	t.Helper()
	where := ""
	if rel != "" {
		where = " WHERE r.rel = " + cypherLit(rel)
	}
	return h.cypherCount(t, fmt.Sprintf("MATCH (:%s {%s})-[r:%s]->(:%s {%s})%s RETURN count(r);",
		neoNodeLabel, neoScope(key), neoRelType, neoNodeLabel, neoScope(key), where))
}

// neoNodeField returns one property of one mirrored node as its plain-format
// string (quotes stripped; a null property comes back as the literal "NULL").
// found=false means no such node.
func (h *harness) neoNodeField(t *testing.T, key projectkey.Key, id, prop string) (string, bool) {
	t.Helper()
	query := fmt.Sprintf("MATCH (n:%s {id: %s, %s}) RETURN n.%s LIMIT 1;",
		neoNodeLabel, cypherLit(id), neoScope(key), prop)
	out := strings.TrimSpace(h.cypher(t, query))
	lines := strings.Split(out, "\n")
	// --format plain prints a header line, then one line per row. Header-only
	// output means the MATCH found nothing.
	if len(lines) < 2 {
		return "", false
	}
	val := strings.TrimSpace(lines[len(lines)-1])
	if len(val) >= 2 && strings.HasPrefix(val, `"`) && strings.HasSuffix(val, `"`) {
		val = val[1 : len(val)-1]
	}
	return val, true
}

// ---------- S3 (aws CLI — independent of the server's SDK client) ----------

func awsArgs(args ...string) []string {
	return append([]string{"--profile", awsProfile, "--region", s3Region}, args...)
}

// s3Exists heads one object. The evidence string carries the CLI output.
func (h *harness) s3Exists(key string) (bool, string) {
	out, err := runCmd("", "aws", awsArgs("s3api", "head-object", "--bucket", s3Bucket, "--key", key)...)
	if err != nil {
		return false, strings.TrimSpace(out)
	}
	return true, "head-object ok: s3://" + s3Bucket + "/" + key
}

// s3CatErr streams one object's bytes; the error carries stderr.
func (h *harness) s3CatErr(key string) ([]byte, error) {
	stdout, stderr, err := runCmdStdout("aws", awsArgs("s3", "cp", "s3://"+s3Bucket+"/"+key, "-")...)
	if err != nil {
		return nil, &execError{msg: strings.TrimSpace(stderr)}
	}
	return []byte(stdout), nil
}

// s3Cat streams one object's bytes and fails the test on error.
func (h *harness) s3Cat(t *testing.T, key string) []byte {
	t.Helper()
	data, err := h.s3CatErr(key)
	if err != nil {
		failf(t, "aws s3 cp s3://%s/%s: %v", s3Bucket, key, err)
	}
	return data
}

// s3List returns every object key under prefix (list-objects-v2), empty slice
// when none. Scenario code builds expected keys via the cold package
// (cold.EpisodeArchiveKey / KnowledgeLatestKey / BlobKey with s3Username) —
// never by hand.
func (h *harness) s3List(t *testing.T, prefix string) []string {
	t.Helper()
	stdout, stderr, err := runCmdStdout("aws", awsArgs(
		"s3api", "list-objects-v2", "--bucket", s3Bucket, "--prefix", prefix, "--output", "json")...)
	if err != nil {
		failf(t, "aws s3api list-objects-v2 --prefix %s: %v: %s", prefix, err, strings.TrimSpace(stderr))
	}
	if strings.TrimSpace(stdout) == "" {
		return []string{}
	}
	var parsed struct {
		Contents []struct {
			Key string `json:"Key"`
		} `json:"Contents"`
	}
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		failf(t, "list-objects-v2: decode output: %v: %.300s", err, stdout)
	}
	keys := make([]string, 0, len(parsed.Contents))
	for _, obj := range parsed.Contents {
		keys = append(keys, obj.Key)
	}
	return keys
}

// s3CleanPrefix removes everything under the dedicated e2e prefix. Called by
// TestMain pre/post; scenarios needing a mid-run clean slate may call it too.
func (h *harness) s3CleanPrefix() (string, error) {
	return runCmd("", "aws", awsArgs("s3", "rm", "--recursive", "s3://"+s3Bucket+"/"+s3Prefix)...)
}
