package consolidate

import (
	"slices"
	"strings"
	"time"

	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/hotstore"
)

// candidateWindow is the maximum gap between an episode and a cluster's latest
// episode for them to be proposed together (§4 step 1: time clusters).
const candidateWindow = 24 * time.Hour

// cluster accumulates one candidate while scanning time-ordered records.
type cluster struct {
	entities map[string]bool
	ids      []string
	from, to time.Time
}

// proposeCandidates groups unconsolidated episodes into distillation
// candidates by shared (normalized) entities and time proximity. Deterministic
// greedy pass over records sorted by (occurred_at, id): a record joins the
// most recent compatible cluster within candidateWindow, else starts its own.
// Entity-less records only cluster with other entity-less records.
func proposeCandidates(key hotstore.ProjectKey, recs []episodic.Record) []Candidate {
	if len(recs) == 0 {
		return nil
	}

	sorted := slices.Clone(recs)
	slices.SortFunc(sorted, func(x, y episodic.Record) int {
		if !x.OccurredAt.Equal(y.OccurredAt) {
			if x.OccurredAt.Before(y.OccurredAt) {
				return -1
			}
			return 1
		}
		return strings.Compare(x.ID, y.ID)
	})

	var clusters []*cluster
	for _, rec := range sorted {
		ents := normalizeEntities(rec.Entities)
		target := findCluster(clusters, ents, rec.OccurredAt)
		if target == nil {
			target = &cluster{entities: map[string]bool{}, from: rec.OccurredAt}
			clusters = append(clusters, target)
		}
		for e := range ents {
			target.entities[e] = true
		}
		target.ids = append(target.ids, rec.ID)
		if rec.OccurredAt.After(target.to) {
			target.to = rec.OccurredAt
		}
	}

	out := make([]Candidate, 0, len(clusters))
	for _, c := range clusters {
		out = append(out, Candidate{
			Project:    key.String(),
			Entities:   sortedKeys(c.entities),
			EpisodeIDs: c.ids,
			From:       c.from,
			To:         c.to,
		})
	}
	return out
}

// findCluster returns the most recently created cluster that is within the
// window of at and compatible entity-wise, or nil.
func findCluster(clusters []*cluster, ents map[string]bool, at time.Time) *cluster {
	for j := len(clusters) - 1; j >= 0; j-- {
		c := clusters[j]
		if at.Sub(c.to) > candidateWindow {
			continue
		}
		if len(ents) == 0 && len(c.entities) == 0 {
			return c
		}
		for e := range ents {
			if c.entities[e] {
				return c
			}
		}
	}
	return nil
}

// normalizeEntities applies the dictionary normalization of §4 step 2:
// trim + lowercase, dropping empties and duplicates.
func normalizeEntities(raw []string) map[string]bool {
	out := map[string]bool{}
	for _, e := range raw {
		n := strings.ToLower(strings.TrimSpace(e))
		if n != "" {
			out[n] = true
		}
	}
	return out
}

// entityAccumulator builds EntityStats across all scanned projects.
type entityAccumulator struct {
	stats map[string]*EntityStat
}

func newEntityAccumulator() *entityAccumulator {
	return &entityAccumulator{stats: map[string]*EntityStat{}}
}

// add counts each record once per distinct normalized entity and increments
// pairwise co-occurrence weights (§4 step 2).
func (a *entityAccumulator) add(recs []episodic.Record) {
	for _, rec := range recs {
		ents := sortedKeys(normalizeEntities(rec.Entities))
		for _, e := range ents {
			stat, ok := a.stats[e]
			if !ok {
				stat = &EntityStat{Entity: e, CoOccurrence: map[string]int{}}
				a.stats[e] = stat
			}
			stat.Count++
			for _, other := range ents {
				if other != e {
					stat.CoOccurrence[other]++
				}
			}
		}
	}
}

// sorted returns the accumulated stats ordered by entity name.
func (a *entityAccumulator) sorted() []EntityStat {
	out := make([]EntityStat, 0, len(a.stats))
	for _, name := range sortedKeys(a.stats) {
		out = append(out, *a.stats[name])
	}
	return out
}

// sortedKeys returns the map's keys in ascending order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
