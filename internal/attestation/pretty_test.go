package attestation

import (
	"strings"
	"testing"
)

func TestPrettyVerifiedEmoji(t *testing.T) {
	record := validRecord()

	const want = `✅ attestation verified
   role        worker
   provider    codex
   model       gpt-5.5 (source=parsed)
   effort      xhigh
   permission  write
   action      "implement issue #172"
   exit        0
   duration    42s (42000 ms)
   started     2026-06-28T00:00:00Z
   ended       2026-06-28T00:00:42Z
   tokens      input=120 output=34 total=154
   verified    true`
	if got := record.Pretty(PrettyOptions{}); got != want {
		t.Fatalf("Pretty() = %q, want %q", got, want)
	}
}

func TestPrettyFailedStatusHasPriority(t *testing.T) {
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
	if !strings.HasPrefix(got, "❌ attestation failed\n") {
		t.Fatalf("Pretty() status = %q, want failed", firstLine(got))
	}
	for _, want := range []string{
		"   effort      unset",
		"   exit        1",
		"   duration    3.2s (3200 ms)",
		"   tokens      input=2447 output=4947 total=unset",
		"   verified    true",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Pretty() missing %q:\n%s", want, got)
		}
	}
}

func TestPrettySelfReportedPlainTotalOnly(t *testing.T) {
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

	const want = `attestation: self-reported
  role        conductor
  provider    codex-cli
  model       gpt-5 (source=self-reported)
  effort      xhigh
  permission  orchestrate
  action      "merge PR #214"
  exit        0
  duration    1m12s (72000 ms)
  started     2026-06-28T00:00:00Z
  ended       2026-06-28T00:01:12Z
  tokens      total=18266
  verified    false`
	if got := record.Pretty(PrettyOptions{Mode: PrettyModePlain}); got != want {
		t.Fatalf("Pretty(plain) = %q, want %q", got, want)
	}
}

func TestPrettyEscapesActionControlCharacters(t *testing.T) {
	record := validRecord()
	record.Action = "first line\nsecond line\t\"quoted\"\x00done"

	got := record.Pretty(PrettyOptions{Mode: PrettyModePlain})
	const wantAction = `  action      "first line\nsecond line\t\"quoted\"\x00done"`
	if !strings.Contains(got, wantAction) {
		t.Fatalf("Pretty() missing escaped action %q:\n%s", wantAction, got)
	}
	if lineCount(got) != 13 {
		t.Fatalf("Pretty() rendered %d lines, want 13:\n%s", lineCount(got), got)
	}
	if strings.Contains(got, "second line\t") {
		t.Fatalf("Pretty() contains an unescaped tab/newline action fragment:\n%s", got)
	}
}

func TestPrettyPlainFallbackHasNoEmojiOrANSIAndPreservesFields(t *testing.T) {
	record := validRecord()

	got := record.Pretty(PrettyOptions{Mode: PrettyModePlain})
	for _, disallowed := range []string{"✅", "❌", "⚠", "\x1b["} {
		if strings.Contains(got, disallowed) {
			t.Fatalf("Pretty(plain) contains %q:\n%s", disallowed, got)
		}
	}
	for _, want := range []string{
		"attestation: verified",
		"  role        worker",
		"  provider    codex",
		"  model       gpt-5.5 (source=parsed)",
		"  effort      xhigh",
		"  permission  write",
		"  action      \"implement issue #172\"",
		"  exit        0",
		"  duration    42s (42000 ms)",
		"  started     2026-06-28T00:00:00Z",
		"  ended       2026-06-28T00:00:42Z",
		"  tokens      input=120 output=34 total=154",
		"  verified    true",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Pretty(plain) missing %q:\n%s", want, got)
		}
	}
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
