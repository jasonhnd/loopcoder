package reporter

import (
	"strings"
	"testing"
	"time"
)

func TestPrettyWorkerSuccessReceipt(t *testing.T) {
	setPrettyTestLocalTime(t)
	record := validRecord()
	record.WorkID = "run-issue-172"
	record.Issue = 172
	record.PR = 314
	record.Branch = "loop/issue-172"
	record.Round = 1
	record.Status = "success"
	record.Reason = "worker completed successfully"

	const want = "\u2705 report verified - loopcoder report: worker success\n" +
		"Target\n" +
		"- work id: run-issue-172\n" +
		"- issue: #172\n" +
		"- PR: #314\n" +
		"- branch: loop/issue-172\n" +
		"- round: 1\n" +
		"Verdict\n" +
		"- status: success\n" +
		"- blocking defects: 0\n" +
		"- reason: worker completed successfully\n" +
		"Run\n" +
		"- worker: codex / gpt-5.5 (xhigh) / xhigh\n" +
		"- permission: write\n" +
		"- action: \"implement issue #172\"\n" +
		"- exit: 0\n" +
		"- duration: 42.0s (42.0 s)\n" +
		"- started: 2026-06-28 09:00:00 JST\n" +
		"- ended: 2026-06-28 09:00:42 JST\n" +
		"- verified: true\n" +
		"- tokens: input=120  output=34  total=154\n" +
		"Next\n" +
		"- action: run verifier review before merge consideration\n" +
		"- details: loopcoder report --work-id run-issue-172 --verbose\n" +
		"- raw JSON: loopcoder report --work-id run-issue-172 --format json"
	if got := record.Pretty(PrettyOptions{}); got != want {
		t.Fatalf("Pretty() = %q, want %q", got, want)
	}
}

func TestPrettyVerifierNeedsHumanSummarizesFindings(t *testing.T) {
	setPrettyTestLocalTime(t)
	record := validRecord()
	record.Role = RoleVerifier
	record.Provider = "claude"
	record.Model = "claude-opus-4-8[1m]"
	record.Effort = "max"
	record.Permission = PermissionReadOnly
	record.WorkID = "loopreview-663"
	record.Issue = 644
	record.PR = 663
	record.Branch = "loop/issue-644"
	record.Action = "review PR #663"
	record.Status = "needs-human"
	record.Reason = "merged design/spec evidence was not found"
	record.SpecStatus = "not-applicable"
	record.Findings = []Finding{
		{Severity: "warning", File: "docs/specs/0644.md", Note: "merged design/spec evidence was not found"},
		{Severity: "low", File: "internal/reporter/pretty.go", Note: "minor wording is ambiguous"},
		{Severity: "info", Note: "manual smoke not available"},
	}

	got := record.Pretty(PrettyOptions{Mode: PrettyModePlain})
	for _, want := range []string{
		"report: verified - loopcoder report: verifier needs-human",
		"Target\n- work id: loopreview-663\n- issue: #644\n- PR: #663\n- branch: loop/issue-644",
		"Verdict\n- status: needs-human\n- blocking defects: 0\n- reason: merged design/spec evidence was not found",
		"Review summary\n- acceptance criteria: not applicable\n- findings: 1 warning, 1 low, 1 info",
		"Run\n- verifier: claude / claude-opus-4-8[1m] (max) / max",
		"Next\n- action: human should decide whether the reported reason is acceptable",
		"- raw JSON: loopcoder report --work-id loopreview-663 --format json",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Pretty() missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "minor wording is ambiguous") {
		t.Fatalf("Pretty() default included detailed finding text:\n%s", got)
	}
}

func TestPrettyVerboseFindingDetails(t *testing.T) {
	record := validRecord()
	record.Role = RoleVerifier
	record.Status = "fail"
	record.SpecStatus = "fail"
	record.Findings = []Finding{
		{Severity: "error", File: "internal/cli/cli.go", Note: "json output mixed with human text"},
		{Severity: "warning", Note: "missing smoke evidence"},
	}

	got := record.Pretty(PrettyOptions{Mode: PrettyModePlain, Verbose: true})
	for _, want := range []string{
		"- findings: 1 error, 1 warning",
		"Findings",
		"- error: internal/cli/cli.go: json output mixed with human text",
		"- warning: missing smoke evidence",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Pretty(verbose) missing %q:\n%s", want, got)
		}
	}
}

func TestPrettyStableStatuses(t *testing.T) {
	tests := []struct {
		name       string
		role       Role
		status     string
		exitCode   int
		wantLine   string
		wantReason string
	}{
		{name: "fail", role: RoleWorker, status: "fail", exitCode: 1, wantLine: "report: failed - loopcoder report: worker fail", wantReason: "- reason: command exited with code 1"},
		{name: "timeout", role: RoleWorker, status: "timeout", wantLine: "report: failed - loopcoder report: worker timeout", wantReason: "- reason: run timed out"},
		{name: "cancelled", role: RoleWorker, status: "cancelled", wantLine: "report: failed - loopcoder report: worker cancelled", wantReason: "- reason: run was cancelled"},
		{name: "partial child failure", role: RoleConductor, status: "partial-child-failure", wantLine: "report: failed - loopcoder report: conductor partial-child-failure", wantReason: "- reason: one or more child runs failed"},
		{name: "verifier pass", role: RoleVerifier, status: "pass", wantLine: "report: verified - loopcoder report: verifier pass", wantReason: "- reason: verifier passed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record := validRecord()
			record.Role = tt.role
			record.Status = tt.status
			record.ExitCode = tt.exitCode

			got := record.Pretty(PrettyOptions{Mode: PrettyModePlain})
			if !strings.HasPrefix(got, tt.wantLine+"\n") {
				t.Fatalf("Pretty() first line = %q, want %q", firstLine(got), tt.wantLine)
			}
			if !strings.Contains(got, tt.wantReason) {
				t.Fatalf("Pretty() missing reason %q:\n%s", tt.wantReason, got)
			}
		})
	}
}

func TestPrettyTimestampFallbackKeepsRawValue(t *testing.T) {
	record := validRecord()
	record.StartedAt = "not-rfc3339"

	got := record.Pretty(PrettyOptions{Mode: PrettyModePlain})
	if !strings.Contains(got, "- started: not-rfc3339") {
		t.Fatalf("Pretty() missing raw fallback timestamp:\n%s", got)
	}
}

func TestPrettyContextAndModelDepthDisplay(t *testing.T) {
	record := validRecord()
	record.WorkID = "run-20260707-issue-567"
	record.Issue = 567
	record.Branch = "loop/issue-567"
	record.Worktree = `C:\repo\.worktrees\issue-567`
	record.Round = 2
	record.Model = "Gemini 3.1 Pro (High)"
	record.Effort = "High"
	record.ModelSource = ModelSourceSelfReported

	got := record.Pretty(PrettyOptions{Mode: PrettyModePlain})
	for _, want := range []string{
		"- worker: codex / Gemini 3.1 Pro (High) / High",
		"- work id: run-20260707-issue-567",
		"- issue: #567",
		"- branch: loop/issue-567",
		`- worktree: C:\repo\.worktrees\issue-567`,
		"- round: 2",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Pretty() missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Gemini 3.1 Pro (High) (High)") {
		t.Fatalf("Pretty() duplicated model depth:\n%s", got)
	}
}

func TestPrettyEscapesActionControlCharacters(t *testing.T) {
	record := validRecord()
	record.Action = "first line\nsecond line\t\"quoted\"\x00done"

	got := record.Pretty(PrettyOptions{Mode: PrettyModePlain})
	const wantAction = `- action: "first line\nsecond line\t\"quoted\"\x00done"`
	if !strings.Contains(got, wantAction) {
		t.Fatalf("Pretty() missing escaped action %q:\n%s", wantAction, got)
	}
	if strings.Contains(got, "second line\t") {
		t.Fatalf("Pretty() contains an unescaped tab/newline action fragment:\n%s", got)
	}
}

func TestPrettyPlainFallbackHasNoEmojiOrANSI(t *testing.T) {
	record := validRecord()

	got := record.Pretty(PrettyOptions{Mode: PrettyModePlain})
	for _, disallowed := range []string{"\u2705", "\u274c", "\u26a0", "\x1b["} {
		if strings.Contains(got, disallowed) {
			t.Fatalf("Pretty(plain) contains %q:\n%s", disallowed, got)
		}
	}
	if !strings.HasPrefix(got, "report: verified - loopcoder report: worker success\n") {
		t.Fatalf("Pretty(plain) first line = %q", firstLine(got))
	}
}

func TestPrettyDoesNotChangeCanonicalContracts(t *testing.T) {
	record := validRecord()

	data, err := record.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON returned error: %v", err)
	}

	const wantCanonical = `{"role":"worker","provider":"codex","model":"gpt-5.5","model_source":"parsed","effort":"xhigh","permission":"write","action":"implement issue #172","exit_code":0,"started_at":"2026-06-28T00:00:00Z","ended_at":"2026-06-28T00:00:42Z","duration_ms":42000,"usage":{"input_tokens":120,"output_tokens":34,"total_tokens":154},"verified":true}`
	if string(data) != wantCanonical {
		t.Fatalf("CanonicalJSON() = %s, want %s", string(data), wantCanonical)
	}

	const wantHeader = `[reporter] role=worker provider=codex model=gpt-5.5(parsed) effort=xhigh perm=write action="implement issue #172" exit=0 dur=42s tokens=120/34|154 verified=true`
	if got := record.Header(); got != wantHeader {
		t.Fatalf("Header() = %q, want %q", got, wantHeader)
	}
}

func setPrettyTestLocalTime(t *testing.T) {
	t.Helper()
	location, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatalf("LoadLocation(Asia/Tokyo) returned error: %v", err)
	}
	previous := time.Local
	time.Local = location
	t.Cleanup(func() {
		time.Local = previous
	})
}

func firstLine(value string) string {
	line, _, _ := strings.Cut(value, "\n")
	return line
}
