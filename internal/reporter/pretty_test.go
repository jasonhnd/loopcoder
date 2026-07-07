package reporter

import (
	"strings"
	"testing"
	"time"
)

func TestPrettyVerifiedEmoji(t *testing.T) {
	setPrettyTestLocalTime(t)
	record := validRecord()

	const want = "\u2705 report verified\n" +
		"who\n" +
		"  role        worker\n" +
		"  provider    OpenAI Codex / codex\n" +
		"  model       gpt-5.5 (xhigh) (parsed)\n" +
		"  permission  write\n" +
		"what\n" +
		"  action      \"implement issue #172\"\n" +
		"result\n" +
		"  exit        0\n" +
		"  duration    42.0s (42.0 s)\n" +
		"  started     2026-06-28 09:00:00 JST\n" +
		"  ended       2026-06-28 09:00:42 JST\n" +
		"  verified    true\n" +
		"cost\n" +
		"  tokens      input=120  output=34  total=154"
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
	if !strings.HasPrefix(got, "\u274c report failed\n") {
		t.Fatalf("Pretty() status = %q, want failed", firstLine(got))
	}
	for _, want := range []string{
		"  provider    Anthropic / claude",
		"  model       claude-haiku-4-5-20251001 (parsed)",
		"  exit        1",
		"  ended       2026-06-28 09:00:03 JST",
		"  duration    3.2s (3.2 s)",
		"  tokens      input=2,447  output=4,947  total=7,394",
		"  verified    true",
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

	const want = `report: self-reported
who
  role        conductor
  provider    codex-cli
  model       gpt-5 (xhigh) (self-reported)
  permission  orchestrate
what
  action      "merge PR #214"
result
  exit        0
  duration    1m12.0s (72.0 s)
  started     2026-06-28 09:00:00 JST
  ended       2026-06-28 09:01:12 JST
  verified    false
cost
  tokens      total=18,266`
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
			for _, want := range []string{
				"  provider    " + tt.display,
			} {
				if !strings.Contains(got, want) {
					t.Fatalf("Pretty() missing %q:\n%s", want, got)
				}
			}
		})
	}
}

func TestPrettyTimestampFallbackKeepsRawValue(t *testing.T) {
	record := validRecord()
	record.StartedAt = "not-rfc3339"

	got := record.Pretty(PrettyOptions{Mode: PrettyModePlain})
	if !strings.Contains(got, "  started     not-rfc3339") {
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
		"  model       Gemini 3.1 Pro (High) (self-reported)",
		"  work_id     run-20260707-issue-567",
		"  issue       #567",
		"  branch      loop/issue-567",
		`  worktree    C:\repo\.worktrees\issue-567`,
		"  round       2",
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
	const wantAction = `  action      "first line\nsecond line\t\"quoted\"\x00done"`
	if !strings.Contains(got, wantAction) {
		t.Fatalf("Pretty() missing escaped action %q:\n%s", wantAction, got)
	}
	if lineCount(got) != 16 {
		t.Fatalf("Pretty() rendered %d lines, want 16:\n%s", lineCount(got), got)
	}
	if strings.Contains(got, "second line\t") {
		t.Fatalf("Pretty() contains an unescaped tab/newline action fragment:\n%s", got)
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
		"report: verified",
		"who",
		"  role        worker",
		"  provider    OpenAI Codex / codex",
		"  model       gpt-5.5 (xhigh) (parsed)",
		"  permission  write",
		"what",
		"  action      \"implement issue #172\"",
		"result",
		"  exit        0",
		"  duration    42.0s (42.0 s)",
		"  started     2026-06-28 09:00:00 JST",
		"  ended       2026-06-28 09:00:42 JST",
		"  verified    true",
		"cost",
		"  tokens      input=120  output=34  total=154",
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

func firstLine(value string) string {
	line, _, _ := strings.Cut(value, "\n")
	return line
}

func lineCount(value string) int {
	if value == "" {
		return 0
	}
	return strings.Count(value, "\n") + 1
}
