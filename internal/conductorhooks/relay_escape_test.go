package conductorhooks

import (
	"os"
	"path/filepath"
	"testing"
)

func escWriteWorkerLedger(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, ".loopcoder", "relay", "run-esc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir ledger dir: %v", err)
	}
	header := `[attestation] role=worker provider=codex model=gpt-5(parsed) effort=high perm=write action="implement issue #7" exit=0 dur=1s tokens=1/1|2 verified=true`
	content := "# loopcoder relay report\n# command=dispatch\n# role=worker\n# run_id=run-esc\n# issue=7\n" + header + "\nreport: verified\n  role        worker\n"
	if err := os.WriteFile(filepath.Join(dir, "job-7-1.attest"), []byte(content), 0o644); err != nil {
		t.Fatalf("write ledger: %v", err)
	}
}

func escRelayEnv(stateDir string) map[string]string {
	return map[string]string{
		relayScopeEnv:    "always",
		relayStateDirEnv: stateDir,
	}
}

func escSwallowedDispatch(root, session string) map[string]any {
	return map[string]any{
		"session_id":      session,
		"cwd":             root,
		"hook_event_name": "PostToolUse",
		"tool_name":       "Bash",
		"tool_input":      map[string]any{"command": "loopcoder dispatch --repo ."},
		"tool_response":   map[string]any{"stdout": "dispatch completed without visible report\n", "exit_code": 0},
	}
}

// TestRelayGuardStopHookActiveEscape proves the stop_hook_active escape valve:
// an identical pending-relay setup blocks a normal Stop (control) but is allowed
// through when stop_hook_active is set.
func TestRelayGuardStopHookActiveEscape(t *testing.T) {
	// Control: pending relay + normal Stop => block.
	ctrl := t.TempDir()
	ctrlOpts := Options{Env: mapEnv(escRelayEnv(filepath.Join(ctrl, "state")))}
	escWriteWorkerLedger(t, ctrl)
	RunRelayGuard(mustJSON(t, escSwallowedDispatch(ctrl, "ctrl")), ctrlOpts)
	control := RunRelayGuard(mustJSON(t, map[string]any{
		"session_id": "ctrl", "cwd": ctrl, "hook_event_name": "Stop", "stop_hook_active": false,
	}), ctrlOpts)
	if control.ExitCode != 2 {
		t.Fatalf("control: expected pending relay to block (exit 2), got %d", control.ExitCode)
	}

	// Escape: identical setup + stop_hook_active => allow.
	esc := t.TempDir()
	escOpts := Options{Env: mapEnv(escRelayEnv(filepath.Join(esc, "state")))}
	escWriteWorkerLedger(t, esc)
	RunRelayGuard(mustJSON(t, escSwallowedDispatch(esc, "esc")), escOpts)
	escaped := RunRelayGuard(mustJSON(t, map[string]any{
		"session_id": "esc", "cwd": esc, "hook_event_name": "Stop", "stop_hook_active": true,
	}), escOpts)
	if escaped.ExitCode != 0 {
		t.Fatalf("escape: expected stop_hook_active to allow (exit 0), got %d (stderr=%q)", escaped.ExitCode, escaped.Stderr)
	}
}
