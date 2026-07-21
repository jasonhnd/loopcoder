package cli

import (
	"bytes"
	"encoding/json"
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
		"--provider", "codex",
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
	if acc.Request.Provider != "codex" || acc.Request.Issue != "1124" {
		t.Fatalf("%+v", acc.Request)
	}
}

func TestRunRejectsMissingRouteAndAutoRoute(t *testing.T) {
	deps := DefaultDeps()
	deps.Now = time.Now
	var stdout, stderr bytes.Buffer
	code := runRun([]string{"--repo", "r", "--issue", "1", "--auto-route"}, &stdout, &stderr, deps)
	if code != exitRunUnsupported {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(stderr.String(), "automatic routing") {
		t.Fatalf("stderr=%s", stderr.String())
	}
	code = runRun([]string{"--repo", "r", "--issue", "1", "--provider", "codex"}, &stdout, &stderr, deps)
	if code != exitRunUsage {
		t.Fatalf("missing model code=%d", code)
	}
}

func TestRunHumanJSONLSameSchema(t *testing.T) {
	deps := DefaultDeps()
	deps.Now = func() time.Time { return time.Date(2026, 7, 22, 20, 1, 0, 0, time.UTC) }
	args := []string{"--repo", "o/r", "--issue", "9", "--provider", "p", "--model", "m"}
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
	if acc.Schema != schemaRunAccepted || acc.Status != "accepted" {
		t.Fatalf("%+v", acc)
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
