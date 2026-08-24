package consolidate

import (
	"slices"
	"testing"
	"time"

	"github.com/drakejin/memory-mcp/internal/episodic"
	"github.com/drakejin/memory-mcp/internal/hotstore"
)

var now = time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

func view(id string, ageDays int, consolidated bool) RecordView {
	return RecordView{
		ID:           id,
		OccurredAt:   now.AddDate(0, 0, -ageDays),
		Consolidated: consolidated,
	}
}

func TestAgeEligible(t *testing.T) {
	const ttl = 30

	tests := []struct {
		name       string
		recs       []RecordView
		fileBytes  int64
		maxBytes   int64
		maxRecords int
		want       []string
	}{
		{
			name: "empty input",
			recs: nil,
			want: []string{},
		},
		{
			name: "old consolidated ages, old unconsolidated never does",
			recs: []RecordView{
				view("01A", 40, true),
				view("01B", 40, false),
				view("01C", 5, true),
			},
			want: []string{"01A"},
		},
		{
			name: "exactly ttl days old is eligible",
			recs: []RecordView{view("01A", ttl, true)},
			want: []string{"01A"},
		},
		{
			name: "one day short of ttl stays hot",
			recs: []RecordView{view("01A", ttl-1, true)},
			want: []string{},
		},
		{
			name: "record-count pressure ages oldest consolidated beyond limit",
			recs: []RecordView{
				view("01A", 10, true), // oldest consolidated
				view("01B", 8, true),
				view("01C", 6, false),
				view("01D", 4, true),
				view("01E", 2, false),
			},
			maxRecords: 3, // 5 records, need 2 removals
			want:       []string{"01A", "01B"},
		},
		{
			name: "byte pressure ages proportionally",
			recs: []RecordView{
				view("01A", 10, true),
				view("01B", 8, true),
				view("01C", 6, true),
				view("01D", 4, true),
			},
			fileBytes: 8 << 20, // 8MB over a 4MB cap: keep 2 of 4 → need 2
			maxBytes:  4 << 20,
			want:      []string{"01A", "01B"},
		},
		{
			name: "pressure never touches unconsolidated records",
			recs: []RecordView{
				view("01A", 10, false),
				view("01B", 8, false),
				view("01C", 6, true),
			},
			maxRecords: 1, // need 2 but only one consolidated exists
			want:       []string{"01C"},
		},
		{
			name: "ttl-aged records count toward the pressure quota",
			recs: []RecordView{
				view("01A", 40, true), // ttl-eligible AND oldest
				view("01B", 5, true),
				view("01C", 4, true),
			},
			maxRecords: 2, // need 1 removal; the ttl-aged one satisfies it
			want:       []string{"01A"},
		},
		{
			name: "results sorted oldest first",
			recs: []RecordView{
				view("01B", 35, true),
				view("01A", 45, true),
			},
			want: []string{"01A", "01B"},
		},
		{
			name: "disabled thresholds never trigger pressure",
			recs: []RecordView{
				view("01A", 5, true),
				view("01B", 4, true),
			},
			fileBytes:  100 << 20,
			maxBytes:   0,
			maxRecords: 0,
			want:       []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AgeEligible(tt.recs, now, ttl, tt.fileBytes, tt.maxBytes, tt.maxRecords)
			if !slices.Equal(got, tt.want) {
				t.Errorf("AgeEligible = %v, want %v", got, tt.want)
			}
		})
	}
}

func crec(id string, occurred time.Time, entities []string, consolidated bool) episodic.Record {
	return episodic.Record{
		ID:           id,
		Kind:         episodic.KindEvent,
		OccurredAt:   occurred,
		Actor:        episodic.ActorAgent,
		Text:         "text " + id,
		Entities:     entities,
		Consolidated: consolidated,
	}
}

var clusterKey = hotstore.ProjectKey{Workspace: "vms", Team: "core", Project: "memory"}

func TestProposeCandidates(t *testing.T) {
	base := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)

	t.Run("shared entity within window clusters together", func(t *testing.T) {
		recs := []episodic.Record{
			crec("01A", base, []string{"opensearch"}, false),
			crec("01B", base.Add(2*time.Hour), []string{"OpenSearch", "neo4j"}, false),
		}
		got := proposeCandidates(clusterKey, recs)
		if len(got) != 1 {
			t.Fatalf("candidates = %d, want 1: %+v", len(got), got)
		}
		c := got[0]
		if c.Project != "vms/core/memory" {
			t.Errorf("project = %q", c.Project)
		}
		if !slices.Equal(c.EpisodeIDs, []string{"01A", "01B"}) {
			t.Errorf("ids = %v", c.EpisodeIDs)
		}
		if !slices.Equal(c.Entities, []string{"neo4j", "opensearch"}) {
			t.Errorf("entities = %v, want normalized sorted union", c.Entities)
		}
		if !c.From.Equal(base) || !c.To.Equal(base.Add(2*time.Hour)) {
			t.Errorf("window = %v..%v", c.From, c.To)
		}
	})

	t.Run("disjoint entities split clusters", func(t *testing.T) {
		recs := []episodic.Record{
			crec("01A", base, []string{"redis"}, false),
			crec("01B", base.Add(time.Hour), []string{"kafka"}, false),
		}
		if got := proposeCandidates(clusterKey, recs); len(got) != 2 {
			t.Fatalf("candidates = %d, want 2: %+v", len(got), got)
		}
	})

	t.Run("same entity beyond 24h splits clusters", func(t *testing.T) {
		recs := []episodic.Record{
			crec("01A", base, []string{"opensearch"}, false),
			crec("01B", base.Add(25*time.Hour), []string{"opensearch"}, false),
		}
		if got := proposeCandidates(clusterKey, recs); len(got) != 2 {
			t.Fatalf("candidates = %d, want 2: %+v", len(got), got)
		}
	})

	t.Run("entity-less records cluster by time only", func(t *testing.T) {
		recs := []episodic.Record{
			crec("01A", base, nil, false),
			crec("01B", base.Add(time.Hour), []string{}, false),
			crec("01C", base.Add(time.Hour), []string{"tagged"}, false),
		}
		got := proposeCandidates(clusterKey, recs)
		if len(got) != 2 {
			t.Fatalf("candidates = %d, want 2 (bare pair + tagged): %+v", len(got), got)
		}
	})

	t.Run("empty input yields no candidates", func(t *testing.T) {
		if got := proposeCandidates(clusterKey, nil); got != nil {
			t.Errorf("candidates = %+v, want nil", got)
		}
	})
}

func TestEntityAccumulator(t *testing.T) {
	base := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	acc := newEntityAccumulator()
	acc.add([]episodic.Record{
		crec("01A", base, []string{"OpenSearch", "neo4j"}, false),
		crec("01B", base, []string{"opensearch"}, true),
		crec("01C", base, []string{" opensearch ", "s3"}, false),
	})

	stats := acc.sorted()
	names := make([]string, 0, len(stats))
	byName := map[string]EntityStat{}
	for _, s := range stats {
		names = append(names, s.Entity)
		byName[s.Entity] = s
	}
	if !slices.Equal(names, []string{"neo4j", "opensearch", "s3"}) {
		t.Fatalf("entities = %v", names)
	}
	if byName["opensearch"].Count != 3 {
		t.Errorf("opensearch count = %d, want 3 (normalized merge)", byName["opensearch"].Count)
	}
	if byName["opensearch"].CoOccurrence["neo4j"] != 1 || byName["opensearch"].CoOccurrence["s3"] != 1 {
		t.Errorf("opensearch co-occurrence = %v", byName["opensearch"].CoOccurrence)
	}
	if byName["neo4j"].CoOccurrence["opensearch"] != 1 {
		t.Errorf("neo4j co-occurrence = %v", byName["neo4j"].CoOccurrence)
	}
}
