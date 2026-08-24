package hotstore

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/drakejin/memory-mcp/internal/errs"
)

// Plane names one of the two memory planes for manifest bookkeeping.
type Plane string

const (
	PlaneEpisodic  Plane = "episodic"
	PlaneKnowledge Plane = "knowledge"
)

// pathSep joins key segments in manifest keys and S3 layouts. It is "/" on
// every platform: these are logical keys, not host paths.
const pathSep = "/"

// dotSegment is the traversal marker rejected inside a key segment.
const dotSegment = ".."

// ProjectKey addresses one project file: {workspace}/{team}/{project}.
type ProjectKey struct {
	Workspace string `json:"workspace"`
	Team      string `json:"team"`
	Project   string `json:"project"`
}

// segmentPattern is the allowed charset for one key segment. The charset
// itself excludes path separators, so containment reduces to rejecting dot
// segments below.
var segmentPattern = regexp.MustCompile(`^[a-z0-9._-]+$`)

// Validate checks each segment is non-empty, lowercase [a-z0-9._-], and free
// of path separators or "..", so keys can be embedded in file and S3 paths.
// A violation is KindInvalid: it is caller input, not a store failure.
func (k ProjectKey) Validate() error {
	segments := []struct {
		name  string
		value string
	}{
		{"workspace", k.Workspace},
		{"team", k.Team},
		{"project", k.Project},
	}
	for _, seg := range segments {
		if seg.value == "" {
			return errs.Invalid(opValidateKey, entityProjectKey,
				fmt.Sprintf("project key %s must be non-empty", seg.name))
		}
		if !segmentPattern.MatchString(seg.value) {
			return errs.Invalid(opValidateKey, entityProjectKey,
				fmt.Sprintf("project key %s %q must match [a-z0-9._-]+", seg.name, seg.value))
		}
		if seg.value == "." || strings.Contains(seg.value, dotSegment) {
			return errs.Invalid(opValidateKey, entityProjectKey,
				fmt.Sprintf("project key %s %q must not contain dot path segments", seg.name, seg.value))
		}
	}
	return nil
}

// String renders "ws/team/proj" for logs, manifest keys, and S3 layouts.
func (k ProjectKey) String() string {
	return k.Workspace + pathSep + k.Team + pathSep + k.Project
}

// ManifestFileKey renders the Manifest.Files key for one project plane file:
// "{plane}/{ws}/{team}/{proj}". Single source of truth for the format.
func ManifestFileKey(plane Plane, key ProjectKey) string {
	return string(plane) + pathSep + key.String()
}
