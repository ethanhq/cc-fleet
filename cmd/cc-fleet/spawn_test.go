package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/permmode"
	"github.com/ethanhq/cc-fleet/internal/spawn"
)

// TestResolvePermissionOverride covers the manual override flag resolution that
// runs before any spawn side effect.
func TestResolvePermissionOverride(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		danger  bool
		want    string
		wantErr bool
	}{
		{"no flags → infer", "", false, "", false},
		{"danger → bypass", "", true, permmode.BypassPermissions, false},
		{"explicit acceptEdits", "acceptEdits", false, "acceptEdits", false},
		{"explicit auto", "auto", false, "auto", false},
		{"explicit plan", "plan", false, "plan", false},
		{"explicit default", "default", false, "default", false},
		{"explicit bypass", "bypassPermissions", false, "bypassPermissions", false},
		// both flags → error even though they'd agree.
		{"conflict bypass+danger", "bypassPermissions", true, "", true},
		{"conflict acceptEdits+danger", "acceptEdits", true, "", true},
		// unknown mode → error.
		{"invalid mode", "garbage", false, "", true},
		{"invalid mode bypass-typo", "bypass", false, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolvePermissionOverride(tt.mode, tt.danger)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolvePermissionOverride(%q, %v) = (%q, nil), want error", tt.mode, tt.danger, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolvePermissionOverride(%q, %v) unexpected error: %v", tt.mode, tt.danger, err)
			}
			if got != tt.want {
				t.Fatalf("resolvePermissionOverride(%q, %v) = %q, want %q", tt.mode, tt.danger, got, tt.want)
			}
		})
	}
}

// TestSpawnFlags_Registered locks the permission/verify flags onto the command
// surface (the skill drives this CLI and needs them present).
func TestSpawnFlags_Registered(t *testing.T) {
	cmd := newSpawnCmd()
	if cmd.Flags().Lookup("permission-mode") == nil {
		t.Fatal("--permission-mode flag missing")
	}
	if cmd.Flags().Lookup("dangerously-skip-permissions") == nil {
		t.Fatal("--dangerously-skip-permissions flag missing")
	}
	// settle flags: --verify (default true) + --no-verify escape.
	vf := cmd.Flags().Lookup("verify")
	if vf == nil {
		t.Fatal("--verify flag missing")
	}
	if vf.DefValue != "true" {
		t.Fatalf("--verify default = %q, want true (settle default-on, decision 2A)", vf.DefValue)
	}
	if cmd.Flags().Lookup("no-verify") == nil {
		t.Fatal("--no-verify flag missing")
	}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--help"})
	_ = cmd.Help()
	help := buf.String()
	if !strings.Contains(help, "permission-mode") || !strings.Contains(help, "bypassPermissions") {
		t.Fatalf("help text missing permission-mode guidance:\n%s", help)
	}
}

// TestSpawnJSON_PermissionInheritanceField confirms the --json envelope carries
// permission_inheritance and omits it when empty (envelope stays tight).
func TestSpawnJSON_PermissionInheritanceField(t *testing.T) {
	withField := spawn.Result{OK: true, Name: "w", PermissionInheritance: "lead-flag"}
	data, err := json.Marshal(withField)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"permission_inheritance":"lead-flag"`) {
		t.Fatalf("permission_inheritance not in envelope: %s", data)
	}

	empty := spawn.Result{OK: true, Name: "w"}
	data, err = json.Marshal(empty)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "permission_inheritance") {
		t.Fatalf("empty permission_inheritance must be omitted: %s", data)
	}
}
