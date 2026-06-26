package recovery

import (
	"strings"
	"testing"
)

func TestScrubRedactsSecretPatterns(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "github ghp token",
			in:   "value ghp_abcDEF123_456",
			want: "value [REDACTED_GITHUB_TOKEN]",
		},
		{
			name: "github pat token",
			in:   "value github_pat_abcDEF123_456",
			want: "value [REDACTED_GITHUB_TOKEN]",
		},
		{
			name: "github other prefixes",
			in:   "gho_one ghu_two ghs_three ghr_four",
			want: "[REDACTED_GITHUB_TOKEN] [REDACTED_GITHUB_TOKEN] [REDACTED_GITHUB_TOKEN] [REDACTED_GITHUB_TOKEN]",
		},
		{
			name: "api key",
			in:   "key sk-12345678901234567890",
			want: "key [REDACTED_API_KEY]",
		},
		{
			name: "bearer token",
			in:   "Authorization: Bearer abc.def_ghi~jkl+/=-",
			want: "Authorization: Bearer [REDACTED_TOKEN]",
		},
		{
			name: "token assignment",
			in:   "token=abc123",
			want: "token=[REDACTED_SECRET]",
		},
		{
			name: "password assignment",
			in:   "password: hunter2",
			want: "password: [REDACTED_SECRET]",
		},
		{
			name: "secret assignment",
			in:   "secret=value",
			want: "secret=[REDACTED_SECRET]",
		},
		{
			name: "api key assignment",
			in:   "api_key: value",
			want: "api_key: [REDACTED_SECRET]",
		},
		{
			name: "api-key assignment",
			in:   "api-key=value",
			want: "api-key=[REDACTED_SECRET]",
		},
		{
			name: "normal text",
			in:   "token bucket and password policy are normal words",
			want: "token bucket and password policy are normal words",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Scrub(tt.in); got != tt.want {
				t.Fatalf("Scrub(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRenderBriefIncludesSectionsFencesAndScrubsSecrets(t *testing.T) {
	brief := RenderBrief(BriefInput{
		IssueNumber:    99,
		IssueTitle:     "Cross-platform recovery",
		Branch:         "loop/issue-99",
		WorktreePath:   "C:/tmp/wt",
		LogPath:        "C:/tmp/codex.log",
		SummaryPath:    "C:/tmp/summary.txt",
		AttemptNumber:  2,
		LastPhase:      "codex_started",
		Status:         "failed",
		Error:          "request failed password=hunter2",
		ChangedFiles:   " M internal/recovery/recovery.go",
		ExistingPRText: "#42 https://github.com/owner/repo/pull/42",
		LogTail:        "Authorization: Bearer abc.def\ntoken=plain",
	})

	for _, want := range []string{
		"# Recovery context for issue #99",
		"- Issue: #99",
		"- Title: Cross-platform recovery",
		"- Branch: loop/issue-99",
		"- Worktree path: C:/tmp/wt",
		"- Log path: C:/tmp/codex.log",
		"- Summary path: C:/tmp/summary.txt",
		"- Attempt: 2",
		"- Last phase: codex_started",
		"- Status: failed",
		"## Changed files",
		"```text\n M internal/recovery/recovery.go\n```",
		"## Existing PR for branch",
		"```text\n#42 https://github.com/owner/repo/pull/42\n```",
		"## Scrubbed log tail (last 50 lines)",
		"Bearer [REDACTED_TOKEN]",
		"token=[REDACTED_SECRET]",
	} {
		if !strings.Contains(brief, want) {
			t.Fatalf("RenderBrief output missing %q:\n%s", want, brief)
		}
	}

	if strings.Count(brief, "```text") != 3 {
		t.Fatalf("RenderBrief emitted %d fenced code blocks, want 3:\n%s", strings.Count(brief, "```text"), brief)
	}
	for _, leaked := range []string{"hunter2", "abc.def", "token=plain"} {
		if strings.Contains(brief, leaked) {
			t.Fatalf("RenderBrief leaked %q:\n%s", leaked, brief)
		}
	}
}
