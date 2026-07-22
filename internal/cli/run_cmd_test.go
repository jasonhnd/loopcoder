package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunDryRunAndAcceptedIdentity(t *testing.T) {
	deps := DefaultDeps()
	deps.Now = func() time.Time { return time.Date(2026, 7, 22, 20, 0, 0, 0, time.UTC) }
	var stdout, stderr bytes.Buffer
	code := runRun([]string{
		"--repo", "jasonhnd/loopcoder",
		"--issue", "1124",
		"--provider", "fixture",
		"--model", "m",
		"--dry-run",
		"--format", "json",
	}, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var acc RunAccepted
	if err := json.Unmarshal(stdout.Bytes(), &acc); err != nil {
		t.Fatal(err)
	}
	if acc.Status != "dry_run" || acc.RunID == "" || !strings.HasPrefix(acc.RunID, "run_") {
		t.Fatalf("%+v", acc)
	}
	if acc.Request.Provider != "fixture" || acc.Request.Issue != "1124" {
		t.Fatalf("%+v", acc.Request)
	}
}

func TestRunAutoRouteSelectsAndPartialPinFails(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPCODER_HOME", home)
	deps := DefaultDeps()
	deps.Now = func() time.Time { return time.Date(2026, 7, 22, 22, 0, 0, 0, time.UTC) }
	var stdout, stderr bytes.Buffer
	// --auto-route must work (P4 enabled)
	code := runRun([]string{
		"--repo", "acme/demo", "--issue", "1", "--auto-route", "--format", "json",
	}, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("auto-route code=%d stderr=%s", code, stderr.String())
	}
	var acc RunAccepted
	if err := json.Unmarshal(stdout.Bytes(), &acc); err != nil {
		t.Fatal(err)
	}
	if acc.Request.Provider == "" || acc.Request.Model == "" {
		t.Fatalf("auto-route did not fill route: %+v", acc.Request)
	}
	if acc.Status != "human_gate" && acc.Status != "dry_run" {
		// full path should reach human_gate
		if acc.Status != "human_gate" {
			t.Fatalf("status=%s msg=%s", acc.Status, acc.Message)
		}
	}
	if !strings.Contains(stderr.String(), "auto-route selected") {
		t.Fatalf("expected auto-route log on stderr: %q", stderr.String())
	}
	// partial pin still usage error
	stdout.Reset()
	stderr.Reset()
	code = runRun([]string{"--repo", "r", "--issue", "1", "--provider", "fixture"}, &stdout, &stderr, deps)
	if code != exitRunUsage && code != exitRunPrecondition {
		t.Fatalf("missing model code=%d stderr=%s", code, stderr.String())
	}
}

func TestRunOmittedRouteUsesAutoPolicy(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPCODER_HOME", home)
	deps := DefaultDeps()
	deps.Now = func() time.Time { return time.Date(2026, 7, 22, 22, 5, 0, 0, time.UTC) }
	var stdout, stderr bytes.Buffer
	// omit provider+model without --auto-route flag: still auto policy
	code := runRun([]string{
		"--repo", "acme/demo", "--issue", "2", "--format", "json", "--dry-run",
	}, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var acc RunAccepted
	if err := json.Unmarshal(stdout.Bytes(), &acc); err != nil {
		t.Fatal(err)
	}
	if acc.Status != "dry_run" {
		t.Fatalf("%+v", acc)
	}
	if acc.Request.Provider == "" || acc.Request.Model == "" {
		t.Fatalf("omitted route not filled: %+v", acc.Request)
	}
}

func TestRunHumanJSONLSameSchema(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPCODER_HOME", home)
	deps := DefaultDeps()
	deps.Now = func() time.Time { return time.Date(2026, 7, 22, 20, 1, 0, 0, time.UTC) }
	args := []string{"--repo", "o/r", "--issue", "9", "--provider", "fixture", "--model", "m"}
	var hOut, jOut bytes.Buffer
	if runRun(append(args, "--format", "human"), &hOut, ioDiscard{}, deps) != 0 {
		t.Fatal(hOut.String())
	}
	if runRun(append(args, "--format", "jsonl"), &jOut, ioDiscard{}, deps) != 0 {
		t.Fatal(jOut.String())
	}
	var acc RunAccepted
	if err := json.Unmarshal(jOut.Bytes(), &acc); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hOut.String(), acc.RunID) {
		t.Fatalf("human missing run_id %s in %s", acc.RunID, hOut.String())
	}
	if acc.Schema != schemaRunAccepted || acc.Status != "human_gate" {
		t.Fatalf("%+v", acc)
	}
	if strings.Contains(acc.Message, "execution ports not yet attached") {
		t.Fatal("old stub message still present")
	}
	if !strings.Contains(acc.Message, "human merge gate") && !strings.Contains(acc.Message, "pr=") {
		t.Fatalf("expected delivery human-gate message: %+v", acc)
	}
}

func TestRunRemovesPortsNotAttachedStub(t *testing.T) {
	src, err := os.ReadFile("run_cmd.go")
	if err != nil {
		src, err = os.ReadFile(filepath.Join("internal", "cli", "run_cmd.go"))
	}
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "execution ports not yet attached") {
		t.Fatal("stub message still in source")
	}
	if !strings.Contains(string(src), "directrun.Service") {
		t.Fatal("cmd path does not reach directrun.Service")
	}
	if !strings.Contains(string(src), "directdelivery.Service") {
		t.Fatal("cmd path does not reach directdelivery.Service")
	}
	if !strings.Contains(string(src), "autoroute.Resolve") {
		t.Fatal("cmd path does not reach autoroute.Resolve")
	}
	if strings.Contains(string(src), "unsupported until P4") {
		t.Fatal("P4 rejection stub still present")
	}
}

func TestRunBlackBoxReachesCleanupTerminal(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPCODER_HOME", home)
	deps := DefaultDeps()
	deps.Now = func() time.Time { return time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC) }
	var stdout, stderr bytes.Buffer
	code := runRun([]string{
		"--repo", "acme/demo", "--issue", "7", "--provider", "fixture", "--model", "m",
		"--format", "json",
	}, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var acc RunAccepted
	if err := json.Unmarshal(stdout.Bytes(), &acc); err != nil {
		t.Fatal(err)
	}
	if acc.Status != "human_gate" {
		t.Fatalf("%+v", acc)
	}
	// start report should have been written to stderr report stream
	if !strings.Contains(stderr.String(), "start") && !strings.Contains(stderr.String(), "stage=") {
		t.Fatalf("expected start report on stderr: %q", stderr.String())
	}
	// delivery evidence on stderr
	if !strings.Contains(stderr.String(), "delivery pr=") {
		t.Fatalf("expected delivery pr line on stderr: %q", stderr.String())
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }

func TestRootHelpLeadsWithRun(t *testing.T) {
	cmds := Commands()
	if len(cmds) == 0 || cmds[0].Name != "run" {
		t.Fatalf("first command=%v", cmds[0])
	}
	// primary set present
	want := map[string]bool{"run": false, "status": false, "events": false, "cancel": false, "doctor": false, "providers": false}
	for _, c := range cmds {
		if _, ok := want[c.Name]; ok {
			want[c.Name] = true
		}
	}
	for k, v := range want {
		if !v {
			t.Fatalf("missing %s", k)
		}
	}
}
