package httpserver

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/drakejin/memory-mcp/internal/handler/app/httpserver/apierr"
	"github.com/drakejin/memory-mcp/internal/service/document"
	"github.com/drakejin/memory-mcp/internal/service/episode"
	"github.com/drakejin/memory-mcp/internal/service/knowledge"
	"github.com/drakejin/memory-mcp/internal/x/projectkey"
	"github.com/drakejin/memory-mcp/internal/x/ulid"
)

// Request-size and traversal limits — named constants per coding rules.
const (
	// maxJSONBodyBytes caps JSON request bodies (§7 local API; generous).
	maxJSONBodyBytes = 1 << 20 // 1 MiB
	// maxUploadBytes caps multipart document uploads (§6 originals).
	maxUploadBytes = 128 << 20 // 128 MiB
	// multipartMemoryBytes is the in-memory threshold for multipart parsing.
	multipartMemoryBytes = 32 << 20 // 32 MiB
	// defaultGraphDepth / maxGraphDepth bound §7 neighborhood traversal.
	defaultGraphDepth = 1
	maxGraphDepth     = 10
)

// Path, query and form parameter names, plus the one accepted spelling of a
// true boolean: §7 booleans are an exact "true" comparison, so "1" and "yes"
// read as false.
const (
	paramWorkspace       = "ws"
	paramTeam            = "team"
	paramProject         = "proj"
	paramID              = "id"
	paramSHA             = "sha"
	paramQuery           = "q"
	paramFrom            = "from"
	paramTo              = "to"
	paramKinds           = "kinds"
	paramEntity          = "entity"
	paramDepth           = "depth"
	paramIncludeArchived = "include_archived"
	paramConfirm         = "confirm"
	paramVerify          = "verify"
	formFieldFile        = "file"
	valueTrue            = "true"
)

// keySegmentPattern mirrors the hotstore rule: lowercase [a-z0-9._-], so keys
// embed safely in file paths and S3 keys (§1). The server validates at the
// HTTP boundary; the hotstore validates again on its own boundary.
var keySegmentPattern = regexp.MustCompile(`^[a-z0-9._-]+$`)

// validateProjectKey checks every {ws}/{team}/{proj} segment.
func validateProjectKey(k projectkey.Key) *apierr.Error {
	for _, seg := range []struct{ name, value string }{
		{paramWorkspace, k.Workspace},
		{paramTeam, k.Team},
		{paramProject, k.Project},
	} {
		if seg.value == "" {
			return badRequest(fmt.Sprintf("path segment %q must not be empty", seg.name))
		}
		if seg.value == "." || seg.value == ".." || !keySegmentPattern.MatchString(seg.value) {
			return badRequest(fmt.Sprintf("path segment %q must match [a-z0-9._-]+ and not be a dot path", seg.name))
		}
	}
	return nil
}

// validateSHA checks a sha256 path parameter. The format itself is owned by
// the document domain (P8, §6), which addresses content by it.
func validateSHA(sha string) *apierr.Error {
	if !document.ValidSHA(sha) {
		return badRequest("sha must be a lowercase hex sha256")
	}
	return nil
}

// validateULID checks an episode or node id path parameter.
func validateULID(id string) *apierr.Error {
	if !ulid.Valid(id) {
		return badRequest("id must be a ULID")
	}
	return nil
}

// projectSelectorSegments is the segment count of a "ws/team/proj" selector.
const projectSelectorSegments = 3

// splitProject splits a "ws/team/proj" selector (used by the /v1/consolidate
// body, which addresses projects as strings rather than path params) into its
// three segments. It returns nil when the shape is wrong; segment contents are
// validated separately by validateProjectKey.
func splitProject(raw string) []string {
	parts := strings.Split(raw, "/")
	if len(parts) != projectSelectorSegments {
		return nil
	}
	return parts
}

// parseProjectString parses a "ws/team/proj" selector. Shape and segment-charset
// problems read the same to the caller: both mean "this is not a project".
func parseProjectString(raw string) (projectkey.Key, *apierr.Error) {
	invalid := badRequest(fmt.Sprintf("project %q must be \"ws/team/proj\" with [a-z0-9._-] segments", raw))
	parts := splitProject(raw)
	if parts == nil {
		return projectkey.Key{}, invalid
	}
	key := projectkey.Key{Workspace: parts[0], Team: parts[1], Project: parts[2]}
	if err := validateProjectKey(key); err != nil {
		return projectkey.Key{}, invalid
	}
	return key, nil
}

// decodeJSON reads a size-capped JSON body into dst. allowEmpty tolerates an
// empty body (used by endpoints whose body is optional). Content-Type is not
// inspected: a valid JSON body is accepted whatever it claims to be.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, allowEmpty bool) *apierr.Error {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return badRequest("request body unreadable or too large")
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		if allowEmpty {
			return nil
		}
		return badRequest("request body must not be empty")
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		// The parse error describes the caller's own bytes, so echoing it
		// leaks nothing about the server.
		return badRequest("request body must be valid JSON: " + err.Error())
	}
	return nil
}

// parseTimeParam parses an optional RFC3339 query parameter.
func parseTimeParam(raw, name string) (time.Time, *apierr.Error) {
	if raw == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, badRequest(name + " must be RFC3339")
	}
	return t, nil
}

// parseKindsParam parses the comma-separated kinds filter.
func parseKindsParam(raw string) ([]episode.Kind, *apierr.Error) {
	if raw == "" {
		return nil, nil
	}
	var kinds []episode.Kind
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k := episode.Kind(part)
		if !episode.ValidKind(k) {
			return nil, badRequest(fmt.Sprintf("unknown kind %q", part))
		}
		kinds = append(kinds, k)
	}
	return kinds, nil
}

// parseDepthParam bounds the §7 neighborhood traversal depth.
func parseDepthParam(raw string) (int, *apierr.Error) {
	if raw == "" {
		return defaultGraphDepth, nil
	}
	depth, err := strconv.Atoi(raw)
	if err != nil || depth < defaultGraphDepth || depth > maxGraphDepth {
		return 0, badRequest(fmt.Sprintf("depth must be an integer between %d and %d", defaultGraphDepth, maxGraphDepth))
	}
	return depth, nil
}

// validateULIDs checks every id in ids and names the offending field.
func validateULIDs(ids []string, field string) *apierr.Error {
	for _, id := range ids {
		if !ulid.Valid(id) {
			return badRequest(fmt.Sprintf("%s contains a malformed ULID %q", field, id))
		}
	}
	return nil
}

// Edge confidence bounds (§2.2).
const (
	minEdgeConfidence = 0.0
	maxEdgeConfidence = 1.0
)

// validateEdge applies the §2.2 edge invariants at the HTTP boundary. The
// knowledge service checks the same rules (plus the P5 self-supersede gate) on
// its own boundary; this copy keeps the transport wording and ordering.
func validateEdge(edge knowledge.Edge) *apierr.Error {
	if !ulid.Valid(edge.From) || !ulid.Valid(edge.To) {
		return badRequest("from and to must be node ULIDs")
	}
	if !knowledge.ValidRel(edge.Rel) {
		return badRequest("rel must be one of relates_to|derived_from|supersedes|about")
	}
	if edge.Confidence < minEdgeConfidence || edge.Confidence > maxEdgeConfidence {
		return badRequest("confidence must be within [0,1]")
	}
	return validateULIDs(edge.Provenance, "provenance")
}

// projectKey extracts the {ws}/{team}/{proj} chi params. Contents are checked
// by validateProjectKey.
func projectKey(r *http.Request) projectkey.Key {
	return projectkey.Key{
		Workspace: chi.URLParam(r, paramWorkspace),
		Team:      chi.URLParam(r, paramTeam),
		Project:   chi.URLParam(r, paramProject),
	}
}
