package conductorhooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// mapEnv returns an Options.Env lookup backed by the given map.
func mapEnv(m map[string]string) func(string) string {
	return func(k string) string {
		return m[k]
	}
}

// attestHookEnv mirrors the JS hookEnv(stateDir): scope always + state dir.
func attestHookEnv(stateDir string) map[string]string {
	return map[string]string{
		attestScopeEnv:    "always",
		attestStateDirEnv: stateDir,
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

func TestAttestDeniesStopUntilRecordedThenAllows(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	env := attestHookEnv(stateDir)

	stopInput := map[string]any{
		"session_id":       "session-1",
		"cwd":              root,
		"hook_event_name":  "Stop",
		"stop_hook_active": false,
	}

	firstStop := RunAttest(mustJSON(t, stopInput), Options{Env: mapEnv(env)})
	if firstStop.ExitCode != 2 {
		t.Fatalf("expected first Stop exit 2, got %d", firstStop.ExitCode)
	}
	if !regexp.MustCompile(`loopcoder conductor attestation is required`).MatchString(firstStop.Stderr) {
		t.Fatalf("expected required-attestation message, got %q", firstStop.Stderr)
	}

	postTool := RunAttest(mustJSON(t, map[string]any{
		"session_id":      "session-1",
		"cwd":             root,
		"hook_event_name": "PostToolUse",
		"tool_name":       "Bash",
		"tool_input": map[string]any{
			"command": `loopcoder attest --role conductor --provider claude-code --model opus --permission orchestrate --action "merge PR #10" --duration-ms 1 --total-tokens 2`,
		},
		"tool_response": map[string]any{
			"stdout":      "{\"role\":\"conductor\",\"model_source\":\"self-reported\",\"verified\":false}\n[attestation] role=conductor provider=claude-code model=opus(self-reported) effort=default perm=orchestrate action=\"merge PR #10\" exit=0 dur=1ms tokens=2 verified=false\n",
			"stderr":      "",
			"interrupted": false,
			"isImage":     false,
		},
	}), Options{Env: mapEnv(env)})
	if postTool.ExitCode != 0 {
		t.Fatalf("expected PostToolUse exit 0, got %d (stderr=%q)", postTool.ExitCode, postTool.Stderr)
	}

	secondStop := RunAttest(mustJSON(t, stopInput), Options{Env: mapEnv(env)})
	if secondStop.ExitCode != 0 {
		t.Fatalf("expected second Stop exit 0, got %d", secondStop.ExitCode)
	}
	if secondStop.Stderr != "" {
		t.Fatalf("expected empty stderr, got %q", secondStop.Stderr)
	}
}

func TestAttestAllowsMalformedInput(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	result := RunAttest([]byte("{not-json"), Options{Env: mapEnv(attestHookEnv(stateDir))})
	if result.ExitCode != 0 {
		t.Fatalf("expected exit 0 for malformed input, got %d", result.ExitCode)
	}
}

func TestAttestAutoScopeIgnoresUnrelatedWorkspaces(t *testing.T) {
	root := t.TempDir()
	result := RunAttest(mustJSON(t, map[string]any{
		"session_id":      "session-2",
		"cwd":             root,
		"hook_event_name": "Stop",
	}), Options{Env: mapEnv(map[string]string{})})
	if result.ExitCode != 0 {
		t.Fatalf("expected exit 0 in auto scope for unrelated workspace, got %d", result.ExitCode)
	}
}

func TestIsConductorAttestCommandRecognition(t *testing.T) {
	cases := []struct {
		command string
		want    bool
	}{
		{`loopcoder attest --role conductor --provider codex`, true},
		{`"./loopcoder.exe" attest -Role conductor`, true},
		{`"C:\Tools\loopcoder.exe" attest --role conductor`, true},
		{`$env:LOOPCODER_BIN attest --role=conductor`, true},
		{`loopcoder attest --role worker`, false},
	}
	for _, tc := range cases {
		if got := isConductorAttestCommand(tc.command); got != tc.want {
			t.Errorf("isConductorAttestCommand(%q) = %v, want %v", tc.command, got, tc.want)
		}
	}
}

// TestAttestConductorWorkspaceMarker verifies the NEW marker-file signal in
// auto scope: with <cwd>/.loopcoder/conductor-workspace present the attest hook
// enforces (Stop without prior attest => exit 2); without it (and no other
// signal) it allows.
func TestAttestConductorWorkspaceMarker(t *testing.T) {
	// With the marker: auto scope enforces.
	withMarker := t.TempDir()
	markerDir := filepath.Join(withMarker, ".loopcoder")
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		t.Fatalf("mkdir marker dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(markerDir, "conductor-workspace"), []byte(""), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	// No SCOPE env => auto scope; no STATE_DIR => default under cwd.
	res := RunAttest(mustJSON(t, map[string]any{
		"session_id":      "session-marker",
		"cwd":             withMarker,
		"hook_event_name": "Stop",
	}), Options{Env: mapEnv(map[string]string{})})
	if res.ExitCode != 2 {
		t.Fatalf("expected marker workspace to enforce (exit 2), got %d", res.ExitCode)
	}

	// Without the marker (and no other signal): auto scope allows.
	noMarker := t.TempDir()
	res2 := RunAttest(mustJSON(t, map[string]any{
		"session_id":      "session-nomarker",
		"cwd":             noMarker,
		"hook_event_name": "Stop",
	}), Options{Env: mapEnv(map[string]string{})})
	if res2.ExitCode != 0 {
		t.Fatalf("expected no-marker workspace to allow (exit 0), got %d", res2.ExitCode)
	}
}
