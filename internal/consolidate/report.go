package consolidate

import (
	"time"

	"github.com/drakejin/memory-mcp/internal/hotstore"
)

// Pipeline steps named in Report.Failures. The strings are part of the honest
// /v1/consolidate response, so they are declared once here (§4 step 5).
const (
	stepList         = "list"
	stepManifest     = "manifest"
	stepAge          = "age"
	stepArchive      = "archive"
	stepHotRemove    = "hot-remove"
	stepIndexDelete  = "index-delete"
	stepSnapshot     = "snapshot"
	stepSnapshotRead = "snapshot-read"
)

const (
	// failureSep joins the segments of a failure line: step, project, cause.
	failureSep = ": "
	// msgColdUnavailable is the degraded note used when S3 is not wired up;
	// it mirrors the server's degraded vocabulary (§5).
	msgColdUnavailable = "cold storage unavailable"
)

// Candidate is one distillation suggestion: unconsolidated episodes grouped by
// shared entities and time proximity (§4 step 1).
type Candidate struct {
	Project    string    `json:"project"` // ws/team/proj
	Entities   []string  `json:"entities"`
	EpisodeIDs []string  `json:"episode_ids"`
	From       time.Time `json:"from"`
	To         time.Time `json:"to"`
}

// EntityStat is a dictionary-normalized entity with its co-occurrence weight
// updates (§4 step 2).
type EntityStat struct {
	Entity       string         `json:"entity"`
	Count        int            `json:"count"`
	CoOccurrence map[string]int `json:"co_occurrence"`
}

// Report is the honest result of one consolidation run (§4 step 5): moved
// counts, snapshot keys, and every failure — nothing silently swallowed.
type Report struct {
	Candidates []Candidate  `json:"candidates"`
	Entities   []EntityStat `json:"entities"`
	// MovedEpisodes counts records shifted to cold; ArchiveKeys lists the
	// {yyyy-mm}.json objects written.
	MovedEpisodes int      `json:"moved_episodes"`
	ArchiveKeys   []string `json:"archive_keys"`
	// SnapshotKeys lists knowledge latest+snapshot objects written.
	SnapshotKeys []string `json:"snapshot_keys"`
	// Failures lists per-step errors; a failure never blocks other steps.
	Failures []string `json:"failures"`
}

// newReport returns a Report whose slices are non-nil, so the JSON envelope
// carries [] rather than null for an uneventful run.
func newReport() Report {
	return Report{
		Candidates:   []Candidate{},
		Entities:     []EntityStat{},
		ArchiveKeys:  []string{},
		SnapshotKeys: []string{},
		Failures:     []string{},
	}
}

// addFailure records "step: ws/team/proj: cause".
func (r *Report) addFailure(step string, key hotstore.ProjectKey, err error) {
	r.Failures = append(r.Failures, step+failureSep+key.String()+failureSep+err.Error())
}

// addStepFailure records "step: cause" for whole-run steps that address no
// single project.
func (r *Report) addStepFailure(step string, err error) {
	r.Failures = append(r.Failures, step+failureSep+err.Error())
}

// addUnavailable records a step skipped because cold storage is not wired up.
// It is a degraded note, not a pipeline error (§5).
func (r *Report) addUnavailable(step string, key hotstore.ProjectKey) {
	r.Failures = append(r.Failures, step+failureSep+key.String()+failureSep+msgColdUnavailable)
}

// Options tunes one run.
type Options struct {
	// Projects limits the run; empty means all projects.
	Projects []hotstore.ProjectKey
	// DryRun computes candidates/stats and reports what WOULD age, without
	// any S3 upload, hot removal, or index deletion.
	DryRun bool
}
