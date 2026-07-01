package conductorhooks

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const workerHeader = `[attestation] role=worker provider=codex model=gpt-5(parsed) effort=high perm=write action="implement issue #101" exit=0 dur=42s tokens=120/34|154 verified=true`

var workerPretty = strings.Join([]string{
	"attestation: verified",
	"  role        worker",
	"  provider    OpenAI",
	"  tool        codex",
	"  model       gpt-5 (detected)",
	"  effort      high",
	"  permission  write",
	"  action      \"implement issue #101\"",
	"  exit        0",
	"  started     2026-06-28 00:00:00 UTC",
	"  ended       2026-06-28 00:00:42 UTC",
	"  duration    42s (42.0 s)",
	"  tokens      input=120  output=34  total=154",
	"  verified    true",
}, "\n")

const verifierHeader = `[attestation] role=verifier provider=claude model=claude-opus-4 effort=high perm=read action="review PR #202" exit=0 dur=17s tokens=80/21|101 verified=true`

var verifierPretty = strings.Join([]string{
	"attestation: verified",
	"  role        verifier",
	"  provider    Anthropic",
	"  tool        claude",
	"  model       claude-opus-4",
	"  effort      high",
	"  permission  read",
	"  action      \"review PR #202\"",
	"  exit        0",
	"  started     2026-06-28 00:01:00 UTC",
	"  ended       2026-06-28 00:01:17 UTC",
	"  duration    17s (17.0 s)",
	"  tokens      input=80  output=21  total=101",
	"  verified    true",
}, "\n")

func relayHookEnv(stateDir string) map[string]string {
	return map[string]string{
		relayScopeEnv:    "always",
		relayStateDirEnv: stateDir,
	}
}

func writeWorkerLedger(t *testing.T, root string) (ledgerPath, block string) {
	t.Helper()
	dir := filepath.Join(root, ".loopcoder", "relay", "run-test")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir worker ledger dir: %v", err)
	}
	ledgerPath = filepath.Join(dir, "job-101-1.attest")
	block = workerHeader + "\n" + workerPretty + "\n"
	content := strings.Join([]string{
		"# loopcoder relay attestation",
		"# command=dispatch",
		"# role=worker",
		"# run_id=run-test",
		"# issue=101",
		block,
	}, "\n")
	if err := os.WriteFile(ledgerPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write worker ledger: %v", err)
	}
	return ledgerPath, block
}

func writeVerifierLedger(t *testing.T, root string) (ledgerPath, block string) {
	t.Helper()
	dir := filepath.Join(root, ".loopcoder", "relay", "run-test")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir verifier ledger dir: %v", err)
	}
	ledgerPath = filepath.Join(dir, "pr-202-1.attest")
	block = verifierHeader + "\n" + verifierPretty + "\n"
	content := strings.Join([]string{
		"# loopcoder relay attestation",
		"# command=loopreview",
		"# role=verifier",
		"# run_id=run-test",
		"# pr=202",
		block,
	}, "\n")
	if err := os.WriteFile(ledgerPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write verifier ledger: %v", err)
	}
	return ledgerPath, block
}

func dispatchPostTool(root string, response map[string]any) map[string]any {
	return map[string]any{
		"session_id":      "session-1",
		"cwd":             root,
		"hook_event_name": "PostToolUse",
		"tool_name":       "Bash",
		"tool_input": map[string]any{
			"command": `loopcoder dispatch --repo . --issue-number 101 --issue-title "Implement"`,
		},
		"tool_response": response,
	}
}

func loopreviewPostTool(root string, response map[string]any) map[string]any {
	return map[string]any{
		"session_id":      "session-1",
		"cwd":             root,
		"hook_event_name": "PostToolUse",
		"tool_name":       "Bash",
		"tool_input": map[string]any{
			"command": `loopcoder loopreview --repo . --pr-number 202`,
		},
		"tool_response": response,
	}
}

func relayStopInput(root string) map[string]any {
	return map[string]any{
		"session_id":       "session-1",
		"cwd":              root,
		"hook_event_name":  "Stop",
		"stop_hook_active": false,
	}
}

func TestRelaySurfacedWorkerBlockAllowsStop(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	env := relayHookEnv(stateDir)
	writeWorkerLedger(t, root)

	postTool := RunRelayGuard(mustJSON(t, dispatchPostTool(root, map[string]any{
		"stdout":    workerHeader + "\n{\"role\":\"worker\",\"verified\":true}\n",
		"stderr":    workerPretty + "\n",
		"exit_code": 0,
	})), Options{Env: mapEnv(env)})
	if postTool.ExitCode != 0 {
		t.Fatalf("expected PostToolUse exit 0, got %d", postTool.ExitCode)
	}

	stop := RunRelayGuard(mustJSON(t, relayStopInput(root)), Options{Env: mapEnv(env)})
	if stop.ExitCode != 0 {
		t.Fatalf("expected Stop exit 0, got %d (stderr=%q)", stop.ExitCode, stop.Stderr)
	}
	if stop.Stderr != "" {
		t.Fatalf("expected empty stderr, got %q", stop.Stderr)
	}
}

func TestRelaySwallowedWorkerBlockBlocksStop(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	env := relayHookEnv(stateDir)
	_, block := writeWorkerLedger(t, root)

	postTool := RunRelayGuard(mustJSON(t, dispatchPostTool(root, map[string]any{
		"stdout":    "dispatch completed without visible attestation\n",
		"stderr":    "",
		"exit_code": 0,
	})), Options{Env: mapEnv(env)})
	if postTool.ExitCode != 0 {
		t.Fatalf("expected PostToolUse exit 0, got %d", postTool.ExitCode)
	}

	firstStop := RunRelayGuard(mustJSON(t, relayStopInput(root)), Options{Env: mapEnv(env)})
	if firstStop.ExitCode != 2 {
		t.Fatalf("expected first Stop exit 2, got %d", firstStop.ExitCode)
	}
	if !regexp.MustCompile(`local verbatim attestation relay was missing`).MatchString(firstStop.Stderr) {
		t.Fatalf("expected missing-relay message, got %q", firstStop.Stderr)
	}
	if !strings.Contains(firstStop.Stderr, strings.TrimRight(block, "\n")) {
		t.Fatalf("expected stderr to include ledger block; stderr=%q", firstStop.Stderr)
	}

	secondStop := RunRelayGuard(mustJSON(t, relayStopInput(root)), Options{Env: mapEnv(env)})
	if secondStop.ExitCode != 0 {
		t.Fatalf("expected second Stop exit 0, got %d", secondStop.ExitCode)
	}
}

func TestRelaySurfacedVerifierBlockAllowsStop(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	env := relayHookEnv(stateDir)
	writeVerifierLedger(t, root)

	postTool := RunRelayGuard(mustJSON(t, loopreviewPostTool(root, map[string]any{
		"stdout":    verifierHeader + "\n{\"role\":\"verifier\",\"verified\":true}\n",
		"stderr":    verifierPretty + "\n",
		"exit_code": 0,
	})), Options{Env: mapEnv(env)})
	if postTool.ExitCode != 0 {
		t.Fatalf("expected PostToolUse exit 0, got %d", postTool.ExitCode)
	}

	stop := RunRelayGuard(mustJSON(t, relayStopInput(root)), Options{Env: mapEnv(env)})
	if stop.ExitCode != 0 {
		t.Fatalf("expected Stop exit 0, got %d (stderr=%q)", stop.ExitCode, stop.Stderr)
	}
	if stop.Stderr != "" {
		t.Fatalf("expected empty stderr, got %q", stop.Stderr)
	}
}

func TestRelaySwallowedVerifierBlockBlocksStop(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	env := relayHookEnv(stateDir)
	_, block := writeVerifierLedger(t, root)

	postTool := RunRelayGuard(mustJSON(t, loopreviewPostTool(root, map[string]any{
		"stdout":    "loopreview completed without visible attestation\n",
		"stderr":    "",
		"exit_code": 0,
	})), Options{Env: mapEnv(env)})
	if postTool.ExitCode != 0 {
		t.Fatalf("expected PostToolUse exit 0, got %d", postTool.ExitCode)
	}

	firstStop := RunRelayGuard(mustJSON(t, relayStopInput(root)), Options{Env: mapEnv(env)})
	if firstStop.ExitCode != 2 {
		t.Fatalf("expected first Stop exit 2, got %d", firstStop.ExitCode)
	}
	if !regexp.MustCompile(`local verbatim attestation relay was missing`).MatchString(firstStop.Stderr) {
		t.Fatalf("expected missing-relay message, got %q", firstStop.Stderr)
	}
	if !strings.Contains(firstStop.Stderr, strings.TrimRight(block, "\n")) {
		t.Fatalf("expected stderr to include ledger block; stderr=%q", firstStop.Stderr)
	}

	secondStop := RunRelayGuard(mustJSON(t, relayStopInput(root)), Options{Env: mapEnv(env)})
	if secondStop.ExitCode != 0 {
		t.Fatalf("expected second Stop exit 0, got %d", secondStop.ExitCode)
	}
}

func TestRelayMalformedInputAllows(t *testing.T) {
	root := t.TempDir()
	result := RunRelayGuard([]byte("{not-json"), Options{Env: mapEnv(relayHookEnv(filepath.Join(root, "state")))})
	if result.ExitCode != 0 {
		t.Fatalf("expected exit 0 for malformed input, got %d", result.ExitCode)
	}
}

func TestRelayAutoScopeIgnoresNonConductorWorkspace(t *testing.T) {
	root := t.TempDir()
	result := RunRelayGuard(mustJSON(t, map[string]any{
		"session_id":      "session-2",
		"cwd":             root,
		"hook_event_name": "Stop",
	}), Options{Env: mapEnv(map[string]string{})})
	if result.ExitCode != 0 {
		t.Fatalf("expected exit 0 in auto scope for non-conductor workspace, got %d", result.ExitCode)
	}
}

func TestRelayScopeOffDisablesGuard(t *testing.T) {
	root := t.TempDir()
	writeWorkerLedger(t, root)
	env := map[string]string{
		relayScopeEnv:    "off",
		relayStateDirEnv: filepath.Join(root, "state"),
	}

	postTool := RunRelayGuard(mustJSON(t, dispatchPostTool(root, map[string]any{
		"stdout":    "dispatch completed without visible attestation\n",
		"stderr":    "",
		"exit_code": 0,
	})), Options{Env: mapEnv(env)})
	if postTool.ExitCode != 0 {
		t.Fatalf("expected PostToolUse exit 0, got %d", postTool.ExitCode)
	}

	stop := RunRelayGuard(mustJSON(t, relayStopInput(root)), Options{Env: mapEnv(env)})
	if stop.ExitCode != 0 {
		t.Fatalf("expected Stop exit 0 with scope off, got %d", stop.ExitCode)
	}
}

func TestRelayCommandRecognition(t *testing.T) {
	if got := relayCommand(`loopcoder dispatch --repo .`); got == nil || got.kind != "dispatch" || got.role != "worker" {
		t.Errorf("relayCommand(dispatch) = %+v, want dispatch/worker", got)
	}
	if got := relayCommand(`"C:\Tools\loopcoder.exe" loopreview --repo .`); got == nil || got.kind != "loopreview" || got.role != "verifier" {
		t.Errorf("relayCommand(quoted loopreview) = %+v, want loopreview/verifier", got)
	}
	if got := relayCommand(`$env:LOOPCODER_BIN dispatch --repo .`); got == nil || got.kind != "dispatch" || got.role != "worker" {
		t.Errorf("relayCommand($env dispatch) = %+v, want dispatch/worker", got)
	}
	if got := relayCommand(`loopcoder attest --role conductor`); got != nil {
		t.Errorf("relayCommand(attest) = %+v, want nil", got)
	}
}

// TestRelayConductorWorkspaceMarker mirrors the attest marker test for the relay
// guard: in auto scope the marker file enables enforcement, its absence does not.
func TestRelayConductorWorkspaceMarker(t *testing.T) {
	// With marker + a swallowed worker ledger: Stop blocks.
	withMarker := t.TempDir()
	markerDir := filepath.Join(withMarker, ".loopcoder")
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		t.Fatalf("mkdir marker dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(markerDir, "conductor-workspace"), []byte(""), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	writeWorkerLedger(t, withMarker)
	// Auto scope (no SCOPE), default state dir under cwd.
	post := RunRelayGuard(mustJSON(t, dispatchPostTool(withMarker, map[string]any{
		"stdout":    "dispatch completed without visible attestation\n",
		"stderr":    "",
		"exit_code": 0,
	})), Options{Env: mapEnv(map[string]string{})})
	if post.ExitCode != 0 {
		t.Fatalf("expected PostToolUse exit 0, got %d", post.ExitCode)
	}
	stop := RunRelayGuard(mustJSON(t, relayStopInput(withMarker)), Options{Env: mapEnv(map[string]string{})})
	if stop.ExitCode != 2 {
		t.Fatalf("expected marker workspace to enforce (exit 2), got %d (stderr=%q)", stop.ExitCode, stop.Stderr)
	}

	// Without marker (and no other signal): even a swallowed ledger is ignored.
	noMarker := t.TempDir()
	writeWorkerLedger(t, noMarker)
	post2 := RunRelayGuard(mustJSON(t, dispatchPostTool(noMarker, map[string]any{
		"stdout":    "dispatch completed without visible attestation\n",
		"stderr":    "",
		"exit_code": 0,
	})), Options{Env: mapEnv(map[string]string{})})
	if post2.ExitCode != 0 {
		t.Fatalf("expected PostToolUse exit 0, got %d", post2.ExitCode)
	}
	stop2 := RunRelayGuard(mustJSON(t, relayStopInput(noMarker)), Options{Env: mapEnv(map[string]string{})})
	if stop2.ExitCode != 0 {
		t.Fatalf("expected no-marker workspace to allow (exit 0), got %d", stop2.ExitCode)
	}
}
