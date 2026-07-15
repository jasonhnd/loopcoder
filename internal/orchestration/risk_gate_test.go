package orchestration

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
	"github.com/jasonhnd/loopcoder/internal/waitstate"
)

func TestEvaluateRiskGateWaitsLocallyForRequiredChecks(t *testing.T) {
	reader := &sequencedRiskReader{
		checks: [][]gh.Check{
			{{Name: "verify", Bucket: "pending"}},
			{{Name: "verify", Bucket: "pending"}},
			{{Name: "verify", Bucket: "pass"}},
		},
	}
	clock := &riskWaitClock{now: time.Date(2026, 7, 16, 6, 0, 0, 0, time.UTC)}
	receipts := 0
	decision, err := EvaluateRiskGate(context.Background(), RiskGateOptions{
		Reader:         reader,
		PRNumber:       967,
		RequiredChecks: []string{"verify"},
		WaitForChecks:  true,
		WaitClock:      clock,
		WaitPolicy: waitstate.Policy{
			MinPollInterval: time.Second,
			MaxPollInterval: time.Second,
			ReceiptCadence:  5 * time.Minute,
			Timeout:         time.Minute,
		},
		WaitReceipt: func(context.Context, waitstate.Receipt) error {
			receipts++
			return nil
		},
	})
	if err != nil {
		t.Fatalf("EvaluateRiskGate: %v", err)
	}
	if decision.Status != RiskGateStatusClean || len(decision.RedLines) != 0 {
		t.Fatalf("decision = %#v, want clean after local wait", decision)
	}
	if decision.Wait == nil || decision.Wait.ProviderInvocations != 0 || decision.Wait.WakeDecisions != 1 {
		t.Fatalf("wait report = %#v", decision.Wait)
	}
	if reader.calls != 3 || len(clock.sleeps) != 1 || receipts != 1 {
		t.Fatalf("calls=%d sleeps=%v receipts=%d, want deterministic initial read plus two probes", reader.calls, clock.sleeps, receipts)
	}
}

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

func TestEvaluateRiskGateAdditionalRedLinePathGlobs(t *testing.T) {
	tests := []struct {
		name        string
		changedFile string
		pathGlobs   []string
		wantStatus  string
		wantCount   int
	}{
		{
			name:        "matches nested domain path",
			changedFile: "disclosure/reports/q4/packet.md",
			pathGlobs:   []string{"disclosure/**"},
			wantStatus:  RiskGateStatusNeedsHuman,
			wantCount:   1,
		},
		{
			name:        "does not match unrelated path",
			changedFile: "README.md",
			pathGlobs:   []string{"disclosure/**"},
			wantStatus:  RiskGateStatusClean,
			wantCount:   0,
		},
		{
			name:        "basename glob matches changed file",
			changedFile: "docs/policy.md",
			pathGlobs:   []string{"*.md"},
			wantStatus:  RiskGateStatusNeedsHuman,
			wantCount:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision, err := EvaluateRiskGate(context.Background(), RiskGateOptions{
				Reader:         cleanRiskReader(390, tt.changedFile),
				PRNumber:       390,
				RequiredChecks: []string{"verify"},
				AdditionalRedLines: []RiskRedLine{{
					Category:  "disclosure-compliance",
					Detail:    "requires disclosure approval",
					PathGlobs: tt.pathGlobs,
				}},
			})
			if err != nil {
				t.Fatalf("EvaluateRiskGate returned error: %v", err)
			}
			if decision.Status != tt.wantStatus || len(decision.RedLines) != tt.wantCount {
				t.Fatalf("decision = %#v, want status %q red line count %d", decision, tt.wantStatus, tt.wantCount)
			}
			if tt.wantCount > 0 {
				if decision.RedLines[0].Category != "disclosure-compliance" || len(decision.RedLines[0].PathGlobs) != 0 {
					t.Fatalf("red lines = %#v, want concrete configured veto without matcher metadata", decision.RedLines)
				}
			}
		})
	}
}

func TestEvaluateRiskGateInvalidAdditionalRedLineMatcherIsError(t *testing.T) {
	decision, err := EvaluateRiskGate(context.Background(), RiskGateOptions{
		Reader:         cleanRiskReader(391, "README.md"),
		PRNumber:       391,
		RequiredChecks: []string{"verify"},
		AdditionalRedLines: []RiskRedLine{{
			Category:  "bad-config",
			Detail:    "broken matcher must not silently pass",
			PathGlobs: []string{"docs/[broken"},
		}},
	})
	if err == nil {
		t.Fatal("EvaluateRiskGate returned nil error, want invalid matcher error")
	}
	if !strings.Contains(err.Error(), "invalid additional red line 1") ||
		!strings.Contains(err.Error(), "path_globs[0]") {
		t.Fatalf("error = %v, want additional red line path_globs context", err)
	}
	if decision.Status != RiskGateStatusNeedsHuman {
		t.Fatalf("decision status = %q, want %q", decision.Status, RiskGateStatusNeedsHuman)
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

func TestAdditionalRedLinesCannotLowerBuiltInRedLineFloor(t *testing.T) {
	tests := []struct {
		name         string
		reader       fakeReader
		required     []string
		bypass       RiskRedLine
		wantCategory string
		wantDetail   string
	}{
		{
			name:         "destructive cannot be renamed",
			reader:       destructiveRiskReader(392),
			required:     []string{"verify"},
			bypass:       RiskRedLine{Category: "domain-safe", Detail: "pretend destructive changes are safe", PathGlobs: []string{"**"}},
			wantCategory: RiskRedLineDestructive,
			wantDetail:   "mass deletion",
		},
		{
			name: "build not green cannot be suppressed",
			reader: fakeReader{
				checks:    map[int][]gh.Check{392: {{Name: "verify", Bucket: "fail"}}},
				diffFiles: map[int][]string{392: {"README.md"}},
				diffs:     map[int]string{392: modifiedDiff("README.md")},
			},
			required:     []string{"verify"},
			bypass:       RiskRedLine{Category: RiskRedLineBuild, Detail: "checks are acceptable", PathGlobs: []string{"README.md"}},
			wantCategory: RiskRedLineBuild,
			wantDetail:   "required checks not green",
		},
		{
			name:         "loopcoder core cannot be bypassed by nonmatching domain glob",
			reader:       cleanRiskReader(392, "internal/orchestration/risk_gate.go"),
			required:     []string{"verify"},
			bypass:       RiskRedLine{Category: "domain-safe", Detail: "core is safe", PathGlobs: []string{"docs/**"}},
			wantCategory: RiskRedLineCore,
			wantDetail:   "human rebuild and tick restart",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision, err := EvaluateRiskGate(context.Background(), RiskGateOptions{
				Reader:             tt.reader,
				PRNumber:           392,
				RequiredChecks:     tt.required,
				AdditionalRedLines: []RiskRedLine{tt.bypass},
			})
			if err != nil {
				t.Fatalf("EvaluateRiskGate returned error: %v", err)
			}
			if decision.Status != RiskGateStatusNeedsHuman {
				t.Fatalf("status = %q, want %q", decision.Status, RiskGateStatusNeedsHuman)
			}
			line, ok := findRiskRedLine(decision.RedLines, tt.wantCategory)
			if !ok {
				t.Fatalf("red lines = %#v, want built-in category %q", decision.RedLines, tt.wantCategory)
			}
			if !strings.Contains(line.Detail, tt.wantDetail) {
				t.Fatalf("built-in red line detail = %q, want containing %q", line.Detail, tt.wantDetail)
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
		"internal/reporter/reporter.go",
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
		"internal/reporter/",
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

func findRiskRedLine(lines []RiskRedLine, category string) (RiskRedLine, bool) {
	for _, line := range lines {
		if line.Category == category {
			return line, true
		}
	}
	return RiskRedLine{}, false
}

func TestRiskGateOptionsExposeNoCoreBypassSurface(t *testing.T) {
	allowedFields := map[string]bool{
		"Reader":             true,
		"PRNumber":           true,
		"RequiredChecks":     true,
		"AdditionalRedLines": true,
		"WaitForChecks":      true,
		"WaitPolicy":         true,
		"WaitClock":          true,
		"WaitReceipt":        true,
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

type sequencedRiskReader struct {
	checks [][]gh.Check
	calls  int
}

func (r *sequencedRiskReader) PRChecks(context.Context, int) ([]gh.Check, error) {
	index := r.calls
	r.calls++
	if index >= len(r.checks) {
		index = len(r.checks) - 1
	}
	return append([]gh.Check(nil), r.checks[index]...), nil
}

func (*sequencedRiskReader) PRDiff(context.Context, int) (string, error) {
	return modifiedDiff("README.md"), nil
}

func (*sequencedRiskReader) PRDiffNameOnly(context.Context, int) ([]string, error) {
	return []string{"README.md"}, nil
}

type riskWaitClock struct {
	now    time.Time
	sleeps []time.Duration
}

func (c *riskWaitClock) Now() time.Time { return c.now }

func (c *riskWaitClock) Sleep(_ context.Context, delay time.Duration) error {
	c.sleeps = append(c.sleeps, delay)
	c.now = c.now.Add(delay)
	return nil
}
