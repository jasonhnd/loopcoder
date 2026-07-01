package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/claudehooks"
)

// TestConductorHookInstallEndToEnd is the regression test for the bug where
// `loopcoder skill install` wired `node hooks/conductor-attest.js` into a
// consumer repo's .claude/settings.json but never installed the script, so the
// hook failed to resolve ("Cannot find module") in every repo other than
// loopcoder's own — while doctor's string-only check still reported it healthy.
//
// It installs into a REAL temp project (not a fake filesystem) and then proves
// the EXACT command string written into settings resolves to a working
// loopcoder subcommand, and that the installed workspace marker makes the hook
// actually enforce. This is the seam that had no test before.
func TestConductorHookInstallEndToEnd(t *testing.T) {
	// Force the auto-detection path deterministically regardless of ambient env.
	t.Setenv("LOOPCODER_CONDUCTOR_ATTEST_SCOPE", "auto")

	project := t.TempDir()
	skillDir := t.TempDir()

	if _, err := InstallSkill(context.Background(), SkillInstallOptions{
		Dir:        skillDir,
		ProjectDir: project,
	}, DefaultSkillInstallDeps()); err != nil {
		t.Fatalf("InstallSkill: %v", err)
	}

	// 1. The gitignored conductor-workspace marker must exist so the hook's
	// auto-detection recognizes this repo as a loopcoder conductor workspace.
	markerPath := filepath.Join(project, ".loopcoder", "conductor-workspace")
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("conductor-workspace marker not written: %v", err)
	}

	// 2. The installed settings must reference the binary subcommand, not a
	// relative node script path that would never resolve in a consumer repo.
	settings, err := os.ReadFile(claudehooks.SettingsPath(project))
	if err != nil {
		t.Fatalf("read installed settings: %v", err)
	}
	if bytes.Contains(settings, []byte("node hooks/")) {
		t.Fatalf("settings still reference a node hooks/* script:\n%s", settings)
	}
	command := extractHookCommand(t, settings, "conductor-attest")
	if command != "loopcoder hook conductor-attest" {
		t.Fatalf("installed command = %q, want %q", command, "loopcoder hook conductor-attest")
	}

	// 3. THE REGRESSION CHECK: the exact installed command must resolve to a
	// real, working subcommand. Strip the leading binary token and drive the
	// rest through the CLI with a Stop payload. With the marker present and no
	// prior attestation, the attest hook must BLOCK (exit 2) — proving the
	// installed command is wired to live logic, not a dangling file reference.
	fields := strings.Fields(command)
	if len(fields) < 2 || fields[0] != "loopcoder" {
		t.Fatalf("installed command %q is not a loopcoder subcommand invocation", command)
	}
	args := fields[1:] // ["hook", "conductor-attest"]

	stopPayload := mustJSONBytes(t, map[string]any{
		"session_id":      "e2e-session",
		"cwd":             project,
		"hook_event_name": "Stop",
	})

	var stdout, stderr bytes.Buffer
	deps := DefaultDeps()
	deps.Stdin = bytes.NewReader(stopPayload)
	if code := RunWithDeps(args, &stdout, &stderr, deps); code != 2 {
		t.Fatalf("installed hook command exit = %d, want 2 (blocking Stop); stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "conductor attestation is required") {
		t.Fatalf("hook stderr = %q, want the conductor attestation prompt", stderr.String())
	}
}

// TestConductorHookCommandFailsOpenOutsideWorkspace proves the same installed
// command does NOT enforce (exits 0) in a plain directory with no marker, so
// the hook never blocks unrelated, non-loopcoder work.
func TestConductorHookCommandFailsOpenOutsideWorkspace(t *testing.T) {
	t.Setenv("LOOPCODER_CONDUCTOR_ATTEST_SCOPE", "auto")

	bare := t.TempDir() // no marker, no SKILL.md/AGENTS.md/.delivery.yml
	stopPayload := mustJSONBytes(t, map[string]any{
		"session_id":      "e2e-session",
		"cwd":             bare,
		"hook_event_name": "Stop",
	})

	var stdout, stderr bytes.Buffer
	deps := DefaultDeps()
	deps.Stdin = bytes.NewReader(stopPayload)
	if code := RunWithDeps([]string{"hook", "conductor-attest"}, &stdout, &stderr, deps); code != 0 {
		t.Fatalf("hook exit = %d in non-workspace dir, want 0 (fail open); stderr=%q", code, stderr.String())
	}
}

// extractHookCommand returns the first hook command string in the settings JSON
// that contains substr, walking the hooks.<event>[].hooks[].command shape.
func extractHookCommand(t *testing.T, settings []byte, substr string) string {
	t.Helper()
	var parsed struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(settings, &parsed); err != nil {
		t.Fatalf("parse settings JSON: %v\n%s", err, settings)
	}
	for _, entries := range parsed.Hooks {
		for _, entry := range entries {
			for _, h := range entry.Hooks {
				if strings.Contains(h.Command, substr) {
					return h.Command
				}
			}
		}
	}
	t.Fatalf("no hook command containing %q in settings:\n%s", substr, settings)
	return ""
}

func mustJSONBytes(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return data
}
