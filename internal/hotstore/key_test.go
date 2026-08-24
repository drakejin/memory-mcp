package hotstore

import (
	"errors"
	"testing"

	"github.com/drakejin/memory-mcp/internal/errs"
)

func TestProjectKeyValidate(t *testing.T) {
	tests := []struct {
		name    string
		key     ProjectKey
		wantErr bool
	}{
		{"valid simple", ProjectKey{"ws", "team", "proj"}, false},
		{"valid full charset", ProjectKey{"my-ws.1", "team_x", "proj-2.0"}, false},
		{"empty workspace", ProjectKey{"", "team", "proj"}, true},
		{"empty team", ProjectKey{"ws", "", "proj"}, true},
		{"empty project", ProjectKey{"ws", "team", ""}, true},
		{"uppercase rejected", ProjectKey{"WS", "team", "proj"}, true},
		{"slash rejected", ProjectKey{"ws/evil", "team", "proj"}, true},
		{"backslash rejected", ProjectKey{"ws", `te\am`, "proj"}, true},
		{"space rejected", ProjectKey{"ws", "te am", "proj"}, true},
		{"dot-dot segment rejected", ProjectKey{"ws", "..", "proj"}, true},
		{"embedded dot-dot rejected", ProjectKey{"ws", "team", "a..b"}, true},
		{"single dot rejected", ProjectKey{".", "team", "proj"}, true},
		{"unicode rejected", ProjectKey{"ws", "팀", "proj"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.key.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				return
			}
			// A bad key is caller input: KindInvalid with a client-safe message.
			if !errors.Is(err, errs.ErrInvalid) {
				t.Fatalf("Validate() = %v, want ErrInvalid", err)
			}
			var domain *errs.Error
			if !errors.As(err, &domain) {
				t.Fatalf("Validate() = %v, want a domain error", err)
			}
			if domain.Entity != entityProjectKey {
				t.Errorf("Entity = %q, want %q", domain.Entity, entityProjectKey)
			}
			if domain.Msg == "" {
				t.Error("Msg is empty; the client would see no reason")
			}
		})
	}
}

func TestProjectKeyString(t *testing.T) {
	if got := testKey().String(); got != "ws/team/proj" {
		t.Errorf("String() = %q, want %q", got, "ws/team/proj")
	}
}

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
