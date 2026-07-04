package conductorhooks

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
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
	return dispatchPostToolForTool(root, "Bash", response)
}

func dispatchPostToolForTool(root, toolName string, response map[string]any) map[string]any {
	return map[string]any{
		"session_id":      "session-1",
		"cwd":             root,
		"hook_event_name": "PostToolUse",
		"tool_name":       toolName,
		"tool_input": map[string]any{
			"command": `loopcoder dispatch --repo . --issue-number 101 --issue-title "Implement"`,
		},
		"tool_response": response,
	}
}

func loopreviewPostTool(root string, response map[string]any) map[string]any {
	return loopreviewPostToolForTool(root, "Bash", response)
}

func loopreviewPostToolForTool(root, toolName string, response map[string]any) map[string]any {
	return map[string]any{
		"session_id":      "session-1",
		"cwd":             root,
		"hook_event_name": "PostToolUse",
		"tool_name":       toolName,
		"tool_input": map[string]any{
			"command": `loopcoder loopreview --repo . --pr-number 202`,
		},
		"tool_response": response,
	}
}

func requireRelayRecordStatus(t *testing.T, root, stateDir, ledgerPath, wantStatus string) *relayRecord {
	t.Helper()
	statePath, err := stateFilePath("session-1", root, stateDir, relayStateSub)
	if err != nil {
		t.Fatalf("stateFilePath: %v", err)
	}
	state, err := readRelayState(statePath, "session-1")
	if err != nil {
		t.Fatalf("read relay state: %v", err)
	}
	cleanLedger := filepath.Clean(ledgerPath)
	for _, rec := range state.Records {
		if rec == nil || filepath.Clean(rec.LedgerPath) != cleanLedger {
			continue
		}
		if rec.Status != wantStatus {
			t.Fatalf("record status = %q, want %q (record=%#v)", rec.Status, wantStatus, rec)
		}
		return rec
	}
	t.Fatalf("ledger record %s not found in state: %#v", ledgerPath, state.Records)
	return nil
}

func TestIsShellToolRecognizesSupportedToolNames(t *testing.T) {
	cases := []struct {
		toolName string
		want     bool
	}{
		{"Bash", true},
		{"PowerShell", true},
		{"pwsh", true},
		{"run_shell_command", true},
		{"shell_command", true},
		{"cmd", false},
	}
	for _, tc := range cases {
		if got := isShellTool(tc.toolName); got != tc.want {
			t.Errorf("isShellTool(%q) = %v, want %v", tc.toolName, got, tc.want)
		}
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

func TestRelayPowerShellToolRecordsDispatchAndLoopreview(t *testing.T) {
	tests := []struct {
		name        string
		writeLedger func(*testing.T, string) (string, string)
		input       func(string, map[string]any) map[string]any
		wantRole    string
		wantCommand string
	}{
		{
			name:        "dispatch worker",
			writeLedger: writeWorkerLedger,
			input: func(root string, response map[string]any) map[string]any {
				return dispatchPostToolForTool(root, "PowerShell", response)
			},
			wantRole:    "worker",
			wantCommand: "dispatch",
		},
		{
			name:        "loopreview verifier",
			writeLedger: writeVerifierLedger,
			input: func(root string, response map[string]any) map[string]any {
				return loopreviewPostToolForTool(root, "PowerShell", response)
			},
			wantRole:    "verifier",
			wantCommand: "loopreview",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			stateDir := filepath.Join(root, "state")
			env := relayHookEnv(stateDir)
			ledgerPath, _ := tt.writeLedger(t, root)

			postTool := RunRelayGuard(mustJSON(t, tt.input(root, map[string]any{
				"stdout":    "completed without visible attestation\n",
				"stderr":    "",
				"exit_code": 0,
			})), Options{Env: mapEnv(env)})
			if postTool.ExitCode != 0 {
				t.Fatalf("expected PostToolUse exit 0, got %d", postTool.ExitCode)
			}

			rec := requireRelayRecordStatus(t, root, stateDir, ledgerPath, "pending")
			if rec.Role != tt.wantRole || rec.Command != tt.wantCommand {
				t.Fatalf("record role/command = %s/%s, want %s/%s", rec.Role, rec.Command, tt.wantRole, tt.wantCommand)
			}
		})
	}
}

func TestRelayBackgroundRunRecordsPendingAndStopBlocks(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	env := relayHookEnv(stateDir)
	ledgerPath, block := writeWorkerLedger(t, root)

	postTool := RunRelayGuard(mustJSON(t, dispatchPostToolForTool(root, "PowerShell", map[string]any{
		"stdout":    "running in background\n",
		"stderr":    "",
		"exit_code": 0,
	})), Options{Env: mapEnv(env)})
	if postTool.ExitCode != 0 {
		t.Fatalf("expected PostToolUse exit 0, got %d", postTool.ExitCode)
	}
	requireRelayRecordStatus(t, root, stateDir, ledgerPath, "pending")

	stop := RunRelayGuard(mustJSON(t, relayStopInput(root)), Options{Env: mapEnv(env)})
	if stop.ExitCode != 2 {
		t.Fatalf("expected Stop exit 2, got %d", stop.ExitCode)
	}
	if !strings.Contains(stop.Stderr, strings.TrimRight(block, "\n")) {
		t.Fatalf("expected Stop stderr to include pending ledger block; stderr=%q", stop.Stderr)
	}
}

func TestRelayPendingBackgroundRunCanBeSurfacedByLaterEvent(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	env := relayHookEnv(stateDir)
	ledgerPath, _ := writeWorkerLedger(t, root)
	firstSeen := time.Now()

	postTool := RunRelayGuard(mustJSON(t, dispatchPostToolForTool(root, "PowerShell", map[string]any{
		"stdout":    "running in background\n",
		"stderr":    "",
		"exit_code": 0,
	})), Options{
		Env: mapEnv(env),
		Now: func() time.Time { return firstSeen },
	})
	if postTool.ExitCode != 0 {
		t.Fatalf("expected background PostToolUse exit 0, got %d", postTool.ExitCode)
	}
	requireRelayRecordStatus(t, root, stateDir, ledgerPath, "pending")

	later := firstSeen.Add(time.Duration(recentLedgerGraceMs)*time.Millisecond + time.Minute)
	surfaced := RunRelayGuard(mustJSON(t, dispatchPostToolForTool(root, "PowerShell", map[string]any{
		"stdout":    workerHeader + "\n",
		"stderr":    workerPretty + "\n",
		"exit_code": 0,
	})), Options{
		Env: mapEnv(env),
		Now: func() time.Time { return later },
	})
	if surfaced.ExitCode != 0 {
		t.Fatalf("expected surfaced PostToolUse exit 0, got %d", surfaced.ExitCode)
	}
	requireRelayRecordStatus(t, root, stateDir, ledgerPath, "surfaced")

	stop := RunRelayGuard(mustJSON(t, relayStopInput(root)), Options{Env: mapEnv(env)})
	if stop.ExitCode != 0 {
		t.Fatalf("expected Stop after later surfacing to allow, got %d (stderr=%q)", stop.ExitCode, stop.Stderr)
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
