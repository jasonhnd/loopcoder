package reporter

import (
	"strings"
	"testing"
	"time"
)

func TestPrettyVerifiedEmoji(t *testing.T) {
	setPrettyTestLocalTime(t)
	record := validRecord()

	const want = "\u2705 loopcoder report: worker succeeded\n\n" +
		"Target\n" +
		"- work ID: unset\n" +
		"- issue: unset\n\n" +
		"Verdict\n" +
		"- status: succeeded\n" +
		"- blocking defects: 0\n" +
		"- reason: completed without a blocking report signal\n\n" +
		"Review summary\n" +
		"- acceptance criteria: not reviewed\n" +
		"- regressions found: none reported\n" +
		"- findings: none\n\n" +
		"Run\n" +
		"- worker: OpenAI Codex / codex / gpt-5.5 (xhigh) (parsed) / xhigh\n" +
		"- permission: write\n" +
		"- action: \"implement issue #172\"\n" +
		"- exit: 0\n" +
		"- duration: 42.0s\n" +
		"- tokens: input=120  output=34  total=154\n" +
		"- started: 2026-06-28 09:00:00 JST\n" +
		"- ended: 2026-06-28 09:00:42 JST\n" +
		"- verified: true\n\n" +
		"Next\n" +
		"- run verifier review before calling the PR merge-eligible"
	if got := record.Pretty(PrettyOptions{}); got != want {
		t.Fatalf("Pretty() = %q, want %q", got, want)
	}
}

func TestPrettyFailedStatusHasPriority(t *testing.T) {
	setPrettyTestLocalTime(t)
	record := validRecord()
	record.Role = RoleVerifier
	record.Provider = "claude"
	record.Model = "claude-haiku-4-5-20251001"
	record.Effort = ""
	record.Permission = PermissionReadOnly
	record.Action = "review PR #214"
	record.ExitCode = 1
	record.DurationMS = 3200
	record.EndedAt = "2026-06-28T00:00:03.2Z"
	record.Usage = Usage{
		InputTokens:  int64Ptr(2447),
		OutputTokens: int64Ptr(4947),
	}
	record.Verified = true

	got := record.Pretty(PrettyOptions{})
	for _, want := range []string{
		"\u274c loopcoder report: verifier failed",
		"- verifier: Anthropic / claude / claude-haiku-4-5-20251001 (parsed) / unset",
		"- status: failed",
		"- exit: 1",
		"- ended: 2026-06-28 09:00:03 JST",
		"- duration: 3.2s",
		"- tokens: input=2,447  output=4,947  total=7,394",
		"- verified: true",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Pretty() missing %q:\n%s", want, got)
		}
	}
}

func TestPrettySelfReportedPlainTotalOnly(t *testing.T) {
	setPrettyTestLocalTime(t)
	record := validRecord()
	record.Role = RoleConductor
	record.Provider = "codex-cli"
	record.Model = "gpt-5"
	record.ModelSource = ModelSourceSelfReported
	record.Permission = PermissionOrchestrate
	record.Action = "merge PR #214"
	record.DurationMS = 72000
	record.EndedAt = "2026-06-28T00:01:12Z"
	record.Usage = Usage{
		TotalTokens: int64Ptr(18266),
	}
	record.Verified = false

	const want = `loopcoder report: conductor self reported

Target
- work ID: unset
- issue: unset

Verdict
- status: self-reported
- blocking defects: 0
- reason: record was self-reported and not independently verified

Review summary
- acceptance criteria: not reviewed
- regressions found: none reported
- findings: none

Run
- conductor: codex-cli / gpt-5 (xhigh) (self-reported) / xhigh
- permission: orchestrate
- action: "merge PR #214"
- exit: 0
- duration: 1m12.0s
- tokens: total=18,266
- started: 2026-06-28 09:00:00 JST
- ended: 2026-06-28 09:01:12 JST
- verified: false

Next
- inspect the report before continuing`
	if got := record.Pretty(PrettyOptions{Mode: PrettyModePlain}); got != want {
		t.Fatalf("Pretty(plain) = %q, want %q", got, want)
	}
}

func TestPrettyProviderDisplay(t *testing.T) {
	setPrettyTestLocalTime(t)
	tests := []struct {
		provider string
		display  string
	}{
		{provider: "codex", display: "OpenAI Codex / codex"},
		{provider: "claude", display: "Anthropic / claude"},
		{provider: "gemini", display: "Google / gemini"},
		{provider: "antigravity", display: "Google Antigravity / antigravity"},
		{provider: "custom-cli", display: "custom-cli"},
	}

	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			record := validRecord()
			record.Provider = tt.provider

			got := record.Pretty(PrettyOptions{Mode: PrettyModePlain})
			if !strings.Contains(got, "- worker: "+tt.display+" /") {
				t.Fatalf("Pretty() missing provider display %q:\n%s", tt.display, got)
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
		"- worker: OpenAI Codex / codex / Gemini 3.1 Pro (High) (self-reported) / High",
		"- work ID: run-20260707-issue-567",
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
	setPrettyTestLocalTime(t)
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

func TestPrettyFindingsAreSummarizedBeforeDetails(t *testing.T) {
	record := validRecord()
	blocking := 1

	got := record.Pretty(PrettyOptions{
		Mode:               PrettyModePlain,
		Status:             "needs-human",
		BlockingDefects:    &blocking,
		Reason:             "merged design/spec evidence was not found\nextra detail",
		SpecConformance:    "not-applicable",
		ShowFindingDetails: true,
		Findings: []PrettyFinding{
			{Severity: "warning", File: "docs/specs/design.md", Note: "missing evidence"},
			{Severity: "low", File: "README.md", Note: "minor ambiguity"},
			{Severity: "error", File: "main.go", Note: "blocking defect"},
		},
	})

	assertOrder(t, got,
		"- findings: 1 error, 1 warning, 1 low",
		"- finding details:",
		"  - warning: docs/specs/design.md - missing evidence",
	)
	for _, want := range []string{
		"loopcoder report: worker needs human",
		"- status: needs-human",
		"- blocking defects: 1",
		"- reason: merged design/spec evidence was not found",
		"- acceptance criteria: not applicable",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Pretty() missing %q:\n%s", want, got)
		}
	}
}

func TestPrettyBoundsLongMultilineReason(t *testing.T) {
	record := validRecord()
	longReason := strings.Repeat("missing merged spec evidence ", 20) + "\nsecond line must not render"

	got := record.Pretty(PrettyOptions{
		Mode:   PrettyModePlain,
		Status: "needs-human",
		Reason: longReason,
	})
	if !strings.Contains(got, "- reason: missing merged spec evidence") {
		t.Fatalf("Pretty() missing bounded reason:\n%s", got)
	}
	if strings.Contains(got, "second line must not render") {
		t.Fatalf("Pretty() leaked multiline reason detail:\n%s", got)
	}
	if !strings.Contains(got, "[truncated]") {
		t.Fatalf("Pretty() did not mark long reason as truncated:\n%s", got)
	}
}

func TestPrettyStableStatusFixtures(t *testing.T) {
	tests := []struct {
		name   string
		status string
		reason string
		want   []string
	}{
		{
			name:   "timeout",
			status: "timed_out",
			want:   []string{"loopcoder report: worker timed out", "- status: timed_out", "- reason: command timed out"},
		},
		{
			name:   "cancelled",
			status: "cancelled",
			want:   []string{"loopcoder report: worker cancelled", "- status: cancelled", "- reason: command was cancelled"},
		},
		{
			name:   "partial child failure",
			status: "needs-human",
			reason: "child run failed after partial completion",
			want:   []string{"loopcoder report: worker needs human", "- status: needs-human", "- reason: child run failed after partial completion"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validRecord().Pretty(PrettyOptions{
				Mode:   PrettyModePlain,
				Status: tt.status,
				Reason: tt.reason,
			})
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Fatalf("Pretty() missing %q:\n%s", want, got)
				}
			}
		})
	}
}

func TestPrettyPlainFallbackHasNoEmojiOrANSIAndPreservesFields(t *testing.T) {
	setPrettyTestLocalTime(t)
	record := validRecord()

	got := record.Pretty(PrettyOptions{Mode: PrettyModePlain})
	for _, disallowed := range []string{"\u2705", "\u274c", "\u26a0", "\x1b["} {
		if strings.Contains(got, disallowed) {
			t.Fatalf("Pretty(plain) contains %q:\n%s", disallowed, got)
		}
	}
	for _, want := range []string{
		"loopcoder report: worker succeeded",
		"Target",
		"- work ID: unset",
		"Verdict",
		"- status: succeeded",
		"Review summary",
		"- findings: none",
		"Run",
		"- worker: OpenAI Codex / codex / gpt-5.5 (xhigh) (parsed) / xhigh",
		"- permission: write",
		"- action: \"implement issue #172\"",
		"- exit: 0",
		"- duration: 42.0s",
		"- tokens: input=120  output=34  total=154",
		"- started: 2026-06-28 09:00:00 JST",
		"- ended: 2026-06-28 09:00:42 JST",
		"- verified: true",
		"Next",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Pretty(plain) missing %q:\n%s", want, got)
		}
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

func assertOrder(t *testing.T, text string, values ...string) {
	t.Helper()
	previous := -1
	for _, value := range values {
		index := strings.Index(text, value)
		if index < 0 {
			t.Fatalf("missing %q:\n%s", value, text)
		}
		if index <= previous {
			t.Fatalf("%q appeared out of order:\n%s", value, text)
		}
		previous = index
	}
}
