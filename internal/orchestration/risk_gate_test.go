package orchestration

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
)

func TestEvaluateRiskGateCorePathNeedsHuman(t *testing.T) {
	decision, err := EvaluateRiskGate(context.Background(), RiskGateOptions{
		Reader:         cleanRiskReader(353, "internal/agent/agent.go"),
		PRNumber:       353,
		RequiredChecks: []string{"verify"},
	})
	if err != nil {
		t.Fatalf("EvaluateRiskGate returned error: %v", err)
	}
	if decision.Status != RiskGateStatusNeedsHuman {
		t.Fatalf("status = %q, want %q", decision.Status, RiskGateStatusNeedsHuman)
	}
	if len(decision.RedLines) != 1 || decision.RedLines[0].Category != RiskRedLineCore {
		t.Fatalf("red lines = %#v, want one loopcoder core red line", decision.RedLines)
	}
	detail := decision.RedLines[0].Detail
	for _, want := range []string{"internal/agent/agent.go", "human rebuild", "tick restart"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("core red line detail %q missing %q", detail, want)
		}
	}
}

func TestEvaluateRiskGateNonCorePathStaysClean(t *testing.T) {
	decision, err := EvaluateRiskGate(context.Background(), RiskGateOptions{
		Reader:         cleanRiskReader(354, "README.md"),
		PRNumber:       354,
		RequiredChecks: []string{"verify"},
	})
	if err != nil {
		t.Fatalf("EvaluateRiskGate returned error: %v", err)
	}
	if decision.Status != RiskGateStatusClean || len(decision.RedLines) != 0 {
		t.Fatalf("decision = %#v, want clean non-core path", decision)
	}
}

func TestEvaluateRiskGateDangerousCommandsNeedHuman(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{name: "rm rf root path", line: "rm -rf /tmp/loopcoder-worktree", want: "rm -rf /"},
		{name: "git reset hard", line: "git reset --hard HEAD~1", want: "git reset --hard"},
		{name: "git push force", line: "git push --force origin main", want: "git push --force"},
		{name: "git filter branch", line: "git filter-branch --tree-filter 'rm -rf secrets' HEAD", want: "git filter-branch"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prNumber := 380
			reader := cleanRiskReader(prNumber, "scripts/release.sh")
			reader.diffs[prNumber] = addedLineDiff("scripts/release.sh", tc.line)
			decision, err := EvaluateRiskGate(context.Background(), RiskGateOptions{
				Reader:         reader,
				PRNumber:       prNumber,
				RequiredChecks: []string{"verify"},
			})
			if err != nil {
				t.Fatalf("EvaluateRiskGate returned error: %v", err)
			}
			if decision.Status != RiskGateStatusNeedsHuman {
				t.Fatalf("status = %q, want %q", decision.Status, RiskGateStatusNeedsHuman)
			}
			if len(decision.RedLines) != 1 || decision.RedLines[0].Category != RiskRedLineDestructive {
				t.Fatalf("red lines = %#v, want one destructive red line", decision.RedLines)
			}
			if !strings.Contains(decision.RedLines[0].Detail, tc.want) {
				t.Fatalf("destructive red line detail %q missing %q", decision.RedLines[0].Detail, tc.want)
			}
		})
	}
}

func TestEvaluateRiskGateAcceptsEitherPassingCheckSignal(t *testing.T) {
	cases := []struct {
		name  string
		check gh.Check
	}{
		{name: "bucket pass with failing state", check: gh.Check{Name: "verify", Bucket: "pass", State: "failure"}},
		{name: "state success with failing bucket", check: gh.Check{Name: "verify", Bucket: "fail", State: "success"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prNumber := 381
			reader := cleanRiskReader(prNumber, "README.md")
			reader.checks = map[int][]gh.Check{prNumber: {tc.check}}
			decision, err := EvaluateRiskGate(context.Background(), RiskGateOptions{
				Reader:         reader,
				PRNumber:       prNumber,
				RequiredChecks: []string{"verify"},
			})
			if err != nil {
				t.Fatalf("EvaluateRiskGate returned error: %v", err)
			}
			if decision.Status != RiskGateStatusClean || len(decision.RedLines) != 0 {
				t.Fatalf("decision = %#v, want clean risk gate when either check signal passes", decision)
			}
		})
	}
}

func TestLoopcoderCorePathsCoverSelfHostingGuardSurface(t *testing.T) {
	corePaths := []string{
		".delivery.yml",
		"AGENTS.md",
		"GEMINI.md",
		"SKILL.md",
		"cmd/loopcoder/main.go",
		"hooks/conductor-attest",
		"internal/agent/agent.go",
		"internal/attestation/attestation.go",
		"internal/compile/compile.go",
		"internal/config/config.go",
		"internal/conductorhooks/attest.go",
		"internal/guardrails/budget.go",
		"internal/loopreview/loopreview.go",
		"internal/orchestration/dispatch_wave.go",
		"internal/orchestration/risk_gate.go",
		"internal/orchestration/tick.go",
		"internal/vcs/github/github.go",
		"internal/verify/verify.go",
		"internal/worker/worker.go",
	}
	for _, path := range corePaths {
		t.Run(path, func(t *testing.T) {
			if !isLoopcoderCorePath(path) {
				t.Fatalf("isLoopcoderCorePath(%q) = false, want true", path)
			}
		})
	}

	nonCorePaths := []string{
		"README.md",
		"docs/specs/0161-autonomous-delivery-loop.md",
		"internal/report/report.go",
	}
	for _, path := range nonCorePaths {
		t.Run(path, func(t *testing.T) {
			if isLoopcoderCorePath(path) {
				t.Fatalf("isLoopcoderCorePath(%q) = true, want false", path)
			}
		})
	}
}

func TestLoopcoderCorePathPrefixesCoverArbitraryDescendants(t *testing.T) {
	prefixes := []string{
		"cmd/loopcoder/",
		"hooks/",
		"internal/agent/",
		"internal/attestation/",
		"internal/compile/",
		"internal/config/",
		"internal/conductorhooks/",
		"internal/guardrails/",
		"internal/loopreview/",
		"internal/vcs/",
		"internal/verify/",
		"internal/worker/",
	}

	for _, prefix := range prefixes {
		path := prefix + "future_surface.go"
		t.Run(path, func(t *testing.T) {
			if !isLoopcoderCorePath(path) {
				t.Fatalf("isLoopcoderCorePath(%q) = false, want true", path)
			}
		})
	}
}

func TestLoopcoderCoreOrchestrationGoFilesAreBlanketGuarded(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob orchestration Go files: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("glob orchestration Go files returned no files")
	}
	for _, file := range files {
		repoPath := filepath.ToSlash(filepath.Join("internal", "orchestration", file))
		want := filepath.Base(file) != "doc.go"
		t.Run(repoPath, func(t *testing.T) {
			if got := isLoopcoderCorePath(repoPath); got != want {
				t.Fatalf("isLoopcoderCorePath(%q) = %v, want %v", repoPath, got, want)
			}
		})
	}

	for _, path := range []string{
		"internal/orchestration/future_core.go",
		"internal\\orchestration\\future_windows_path.go",
	} {
		t.Run(path, func(t *testing.T) {
			if !isLoopcoderCorePath(path) {
				t.Fatalf("isLoopcoderCorePath(%q) = false, want true", path)
			}
		})
	}

	for _, path := range []string{
		"internal/orchestration/doc.go",
		"internal/orchestration/README.md",
	} {
		t.Run(path, func(t *testing.T) {
			if isLoopcoderCorePath(path) {
				t.Fatalf("isLoopcoderCorePath(%q) = true, want false", path)
			}
		})
	}
}

func TestRiskGateOptionsExposeNoCoreBypassSurface(t *testing.T) {
	allowedFields := map[string]bool{
		"Reader":             true,
		"PRNumber":           true,
		"RequiredChecks":     true,
		"AdditionalRedLines": true,
	}
	riskGateOptions := reflect.TypeOf(RiskGateOptions{})
	for i := 0; i < riskGateOptions.NumField(); i++ {
		field := riskGateOptions.Field(i)
		if !allowedFields[field.Name] {
			t.Fatalf("RiskGateOptions exposes %s; core red lines must not accept gate/config/status bypass inputs", field.Name)
		}
	}
}

func addedLineDiff(file, line string) string {
	return "diff --git a/" + file + " b/" + file + "\n" +
		"--- a/" + file + "\n" +
		"+++ b/" + file + "\n" +
		"@@ -0,0 +1 @@\n" +
		"+" + line + "\n"
}
