package hotstore

import "testing"

func TestManifestFileKey(t *testing.T) {
	tests := []struct {
		name  string
		plane Plane
		want  string
	}{
		{"episodic", PlaneEpisodic, "episodic/ws/team/proj"},
		{"knowledge", PlaneKnowledge, "knowledge/ws/team/proj"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ManifestFileKey(tt.plane, testKey()); got != tt.want {
				t.Errorf("ManifestFileKey = %q, want %q", got, tt.want)
			}
		})
	}
}
