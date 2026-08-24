package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/hotstore"
	"github.com/drakejin/memory-mcp/internal/knowledge"
	"github.com/drakejin/memory-mcp/internal/ulid"
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

// keySegmentPattern mirrors the hotstore rule: lowercase [a-z0-9._-], so keys
// embed safely in file paths and S3 keys (§1). The server validates at the
// HTTP boundary; the hotstore validates again on its own boundary.
var keySegmentPattern = regexp.MustCompile(`^[a-z0-9._-]+$`)

// shaHexPattern is a full lowercase-hex sha256 path parameter.
var shaHexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// validateProjectKey checks every {ws}/{team}/{proj} segment.
func validateProjectKey(k hotstore.ProjectKey) error {
	for name, seg := range map[string]string{"ws": k.Workspace, "team": k.Team, "proj": k.Project} {
		if seg == "" {
			return fmt.Errorf("path segment %q must not be empty", name)
		}
		if seg == "." || seg == ".." || !keySegmentPattern.MatchString(seg) {
			return fmt.Errorf("path segment %q must match [a-z0-9._-]+ and not be a dot path", name)
		}
	}
	return nil
}

// validateSHA checks a sha256 path parameter.
func validateSHA(sha string) error {
	if !shaHexPattern.MatchString(sha) {
		return errors.New("sha must be a lowercase hex sha256")
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

// errInvalidProject is the single user-facing message for a malformed project
// selector — shape and segment-charset problems read the same to the caller.
func errInvalidProject(raw string) error {
	return fmt.Errorf("project %q must be \"ws/team/proj\" with [a-z0-9._-] segments", raw)
}

// decodeJSON reads a size-capped JSON body into dst. allowEmpty tolerates an
// empty body (used by endpoints whose body is optional).
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, allowEmpty bool) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return errors.New("request body unreadable or too large")
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		if allowEmpty {
			return nil
		}
		return errors.New("request body must not be empty")
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return errors.New("request body must be valid JSON: " + err.Error())
	}
	return nil
}

// parseTimeParam parses an optional RFC3339 query parameter.
func parseTimeParam(raw, name string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be RFC3339", name)
	}
	return t, nil
}

// parseKindsParam parses the comma-separated kinds filter.
func parseKindsParam(raw string) ([]episodic.Kind, error) {
	if raw == "" {
		return nil, nil
	}
	var kinds []episodic.Kind
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k := episodic.Kind(part)
		if !isEpisodicKind(k) {
			return nil, fmt.Errorf("unknown kind %q", part)
		}
		kinds = append(kinds, k)
	}
	return kinds, nil
}

func isEpisodicKind(k episodic.Kind) bool {
	for _, known := range episodic.Kinds {
		if k == known {
			return true
		}
	}
	return false
}

func isActor(a episodic.Actor) bool {
	switch a {
	case episodic.ActorAgent, episodic.ActorUser, episodic.ActorSystem:
		return true
	}
	return false
}

func isNodeKind(k knowledge.NodeKind) bool {
	switch k {
	case knowledge.KindEntity, knowledge.KindFact, knowledge.KindLesson, knowledge.KindPreference, knowledge.KindDocument:
		return true
	}
	return false
}

func isNodeState(s knowledge.State) bool {
	switch s {
	case knowledge.StateActive, knowledge.StateArchived, knowledge.StateDeprecated:
		return true
	}
	return false
}

func isTrust(t knowledge.Trust) bool {
	switch t {
	case knowledge.TrustUserStated, knowledge.TrustAgentInferred, knowledge.TrustImported:
		return true
	}
	return false
}

func isRel(rel knowledge.Rel) bool {
	switch rel {
	case knowledge.RelRelatesTo, knowledge.RelDerivedFrom, knowledge.RelSupersedes, knowledge.RelAbout:
		return true
	}
	return false
}

// validateULIDs checks every id in ids and names the offending field.
func validateULIDs(ids []string, field string) error {
	for _, id := range ids {
		if !ulid.IsULID(id) {
			return fmt.Errorf("%s contains a malformed ULID %q", field, id)
		}
	}
	return nil
}

// normalizeStrings trims entries and drops empties, always returning a
// non-nil slice so hot JSON never stores null arrays.
func normalizeStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
