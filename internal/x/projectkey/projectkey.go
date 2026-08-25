// Package projectkey holds the {workspace}/{team}/{project} identifier every
// layer addresses memory by. It was promoted out of the hot store because it is
// not a storage detail: handlers validate it at the HTTP boundary, the stores
// embed it in file and S3 paths, and the derived planes scope indexes by it.
package projectkey

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/drakejin/memory-mcp/internal/x/errs"
)

// Op and entity carried by the errors this package returns. The op keeps its
// pre-promotion value so error output is unchanged by the relocation.
const (
	opValidateKey    = "hotstore.ProjectKey.Validate"
	entityProjectKey = "project_key"
)

// pathSep joins key segments in manifest keys and S3 layouts. It is "/" on
// every platform: these are logical keys, not host paths.
const pathSep = "/"

// dotSegment is the traversal marker rejected inside a key segment.
const dotSegment = ".."

// Key addresses one project file: {workspace}/{team}/{project}.
type Key struct {
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
func (k Key) Validate() error {
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
func (k Key) String() string {
	return k.Workspace + pathSep + k.Team + pathSep + k.Project
}
