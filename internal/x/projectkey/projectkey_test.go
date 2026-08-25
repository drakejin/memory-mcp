package projectkey

import (
	"errors"
	"testing"

	"github.com/drakejin/memory-mcp/internal/x/errs"
)

func TestKeyValidate(t *testing.T) {
	tests := []struct {
		name    string
		key     Key
		wantErr bool
	}{
		{"valid simple", Key{"ws", "team", "proj"}, false},
		{"valid full charset", Key{"my-ws.1", "team_x", "proj-2.0"}, false},
		{"empty workspace", Key{"", "team", "proj"}, true},
		{"empty team", Key{"ws", "", "proj"}, true},
		{"empty project", Key{"ws", "team", ""}, true},
		{"uppercase rejected", Key{"WS", "team", "proj"}, true},
		{"slash rejected", Key{"ws/evil", "team", "proj"}, true},
		{"backslash rejected", Key{"ws", `te\am`, "proj"}, true},
		{"space rejected", Key{"ws", "te am", "proj"}, true},
		{"dot-dot segment rejected", Key{"ws", "..", "proj"}, true},
		{"embedded dot-dot rejected", Key{"ws", "team", "a..b"}, true},
		{"single dot rejected", Key{".", "team", "proj"}, true},
		{"unicode rejected", Key{"ws", "팀", "proj"}, true},
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

func TestKeyString(t *testing.T) {
	key := Key{Workspace: "ws", Team: "team", Project: "proj"}
	if got := key.String(); got != "ws/team/proj" {
		t.Errorf("String() = %q, want %q", got, "ws/team/proj")
	}
}
