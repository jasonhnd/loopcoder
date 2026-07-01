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

func attestStop(root string, stopHookActive bool) map[string]any {
	return map[string]any{
		"session_id":       "session-1",
		"cwd":              root,
		"hook_event_name":  "Stop",
		"stop_hook_active": stopHookActive,
	}
}

func attestDispatchPost(root string) map[string]any {
	return map[string]any{
		"session_id":      "session-1",
		"cwd":             root,
		"hook_event_name": "PostToolUse",
		"tool_name":       "Bash",
		"tool_input":      map[string]any{"command": `loopcoder dispatch --repo . --issue-number 10`},
		"tool_response":   map[string]any{"stdout": "dispatch completed\n", "exit_code": 0},
	}
}

func attestAttestPost(root string) map[string]any {
	return map[string]any{
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
	}
}

// A planning / chat turn (no delivery command observed) must never block, even
// in a conductor workspace. This is the core fix for the v0.3.8 over-blocking.
func TestAttestNoDeliveryDoesNotBlock(t *testing.T) {
	root := t.TempDir()
	opts := Options{Env: mapEnv(attestHookEnv(filepath.Join(root, "state")))}
	res := RunAttest(mustJSON(t, attestStop(root, false)), opts)
	if res.ExitCode != 0 {
		t.Fatalf("expected no-delivery Stop to allow (exit 0), got %d (stderr=%q)", res.ExitCode, res.Stderr)
	}
}

// A delivery turn blocks exactly once (reminder) and then self-clears, so the
// gate can never loop even if the Conductor never attests.
func TestAttestDeliveryBlocksOnceThenSelfClears(t *testing.T) {
	root := t.TempDir()
	opts := Options{Env: mapEnv(attestHookEnv(filepath.Join(root, "state")))}

	if r := RunAttest(mustJSON(t, attestDispatchPost(root)), opts); r.ExitCode != 0 {
		t.Fatalf("dispatch PostToolUse exit = %d", r.ExitCode)
	}
	first := RunAttest(mustJSON(t, attestStop(root, false)), opts)
	if first.ExitCode != 2 {
		t.Fatalf("expected first Stop after delivery to block (exit 2), got %d", first.ExitCode)
	}
	if !regexp.MustCompile(`conductor attestation is required`).MatchString(first.Stderr) {
		t.Fatalf("expected reminder message, got %q", first.Stderr)
	}
	second := RunAttest(mustJSON(t, attestStop(root, false)), opts)
	if second.ExitCode != 0 {
		t.Fatalf("expected second Stop to self-clear (exit 0), got %d", second.ExitCode)
	}
}

// A delivery turn followed by a Conductor attestation clears the gate.
func TestAttestDeliveryThenAttestAllows(t *testing.T) {
	root := t.TempDir()
	opts := Options{Env: mapEnv(attestHookEnv(filepath.Join(root, "state")))}

	RunAttest(mustJSON(t, attestDispatchPost(root)), opts)
	if r := RunAttest(mustJSON(t, attestAttestPost(root)), opts); r.ExitCode != 0 {
		t.Fatalf("attest PostToolUse exit = %d (stderr=%q)", r.ExitCode, r.Stderr)
	}
	stop := RunAttest(mustJSON(t, attestStop(root, false)), opts)
	if stop.ExitCode != 0 {
		t.Fatalf("expected Stop after attest to allow (exit 0), got %d (stderr=%q)", stop.ExitCode, stop.Stderr)
	}
}

// stop_hook_active is an escape valve: even an unattested delivery turn must not
// block when Claude Code signals we are already in a stop-loop.
func TestAttestStopHookActiveEscape(t *testing.T) {
	root := t.TempDir()
	opts := Options{Env: mapEnv(attestHookEnv(filepath.Join(root, "state")))}
	RunAttest(mustJSON(t, attestDispatchPost(root)), opts)
	res := RunAttest(mustJSON(t, attestStop(root, true)), opts)
	if res.ExitCode != 0 {
		t.Fatalf("expected stop_hook_active to escape (exit 0), got %d", res.ExitCode)
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

func TestDeliveryOrMergeCommandRecognition(t *testing.T) {
	cases := []struct {
		command string
		want    bool
	}{
		{`loopcoder dispatch --repo .`, true},
		{`loopcoder dispatch-wave --repo .`, true},
		{`loopcoder loopreview --pr-number 12`, true},
		{`"C:\Tools\loopcoder.exe" dispatch --repo .`, true},
		{`$env:LOOPCODER_BIN loopreview --pr-number 9`, true},
		{`gh pr merge 330 --squash`, true},
		{`gh pr view 330`, false},
		{`loopcoder attest --role conductor`, false},
		{`loopcoder status`, false},
		{`git commit -m x`, false},
		{`echo run loopcoder dispatch later`, false},
		{`git status && loopcoder dispatch --repo .`, true},
		{`gh --repo o/r pr merge 5`, true},
	}
	for _, tc := range cases {
		if got := deliveryOrMergeCommand(tc.command); got != tc.want {
			t.Errorf("deliveryOrMergeCommand(%q) = %v, want %v", tc.command, got, tc.want)
		}
	}
}

// TestAttestConductorWorkspaceMarker verifies the marker-file signal activates
// the gate in auto scope: with the marker AND a delivery, Stop blocks once;
// with the marker but no delivery it allows; without the marker it is not a
// conductor workspace so it allows.
func TestAttestConductorWorkspaceMarker(t *testing.T) {
	writeMarker := func(dir string) {
		markerDir := filepath.Join(dir, ".loopcoder")
		if err := os.MkdirAll(markerDir, 0o755); err != nil {
			t.Fatalf("mkdir marker dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(markerDir, "conductor-workspace"), []byte(""), 0o644); err != nil {
			t.Fatalf("write marker: %v", err)
		}
	}
	autoScope := Options{Env: mapEnv(map[string]string{})} // no SCOPE => auto; state under cwd/.loopcoder

	// Marker + delivery => auto scope enforces (blocks once).
	withMarker := t.TempDir()
	writeMarker(withMarker)
	RunAttest(mustJSON(t, map[string]any{
		"session_id":      "s-marker",
		"cwd":             withMarker,
		"hook_event_name": "PostToolUse",
		"tool_name":       "Bash",
		"tool_input":      map[string]any{"command": "loopcoder dispatch --repo ."},
		"tool_response":   map[string]any{"exit_code": 0},
	}), autoScope)
	blocked := RunAttest(mustJSON(t, map[string]any{
		"session_id":      "s-marker",
		"cwd":             withMarker,
		"hook_event_name": "Stop",
	}), autoScope)
	if blocked.ExitCode != 2 {
		t.Fatalf("expected marker+delivery to enforce (exit 2), got %d", blocked.ExitCode)
	}

	// Marker but no delivery => allows.
	markerNoDelivery := t.TempDir()
	writeMarker(markerNoDelivery)
	res := RunAttest(mustJSON(t, map[string]any{
		"session_id":      "s-marker-nodelivery",
		"cwd":             markerNoDelivery,
		"hook_event_name": "Stop",
	}), autoScope)
	if res.ExitCode != 0 {
		t.Fatalf("expected marker without delivery to allow (exit 0), got %d", res.ExitCode)
	}

	// No marker (not a conductor workspace) => allows.
	noMarker := t.TempDir()
	res2 := RunAttest(mustJSON(t, map[string]any{
		"session_id":      "s-nomarker",
		"cwd":             noMarker,
		"hook_event_name": "Stop",
	}), autoScope)
	if res2.ExitCode != 0 {
		t.Fatalf("expected no-marker workspace to allow (exit 0), got %d", res2.ExitCode)
	}
}
