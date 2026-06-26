package recovery

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
)

var (
	githubTokenPattern = regexp.MustCompile(`(?i)(ghp|github_pat|gho|ghu|ghs|ghr)_[A-Za-z0-9_]+`)
	apiKeyPattern      = regexp.MustCompile(`(?i)sk-[A-Za-z0-9_-]{20,}`)
	bearerPattern      = regexp.MustCompile(`(?i)(Bearer\s+)[A-Za-z0-9._~+/=-]+`)
	assignmentPattern  = regexp.MustCompile(`(?i)((token|password|secret|api[_-]?key)\s*[=:]\s*)\S+`)
)

type BriefInput struct {
	IssueNumber    int
	IssueTitle     string
	Branch         string
	WorktreePath   string
	LogPath        string
	SummaryPath    string
	AttemptNumber  int
	LastPhase      string
	Status         string
	Error          string
	ChangedFiles   string
	ExistingPRText string
	LogTail        string
}

// Scrub redacts common secret shapes before text is written to recovery state.
func Scrub(text string) string {
	text = githubTokenPattern.ReplaceAllString(text, "[REDACTED_GITHUB_TOKEN]")
	text = apiKeyPattern.ReplaceAllString(text, "[REDACTED_API_KEY]")
	text = bearerPattern.ReplaceAllString(text, "${1}[REDACTED_TOKEN]")
	text = assignmentPattern.ReplaceAllString(text, "${1}[REDACTED_SECRET]")
	return text
}

// RenderBrief renders the Markdown recovery context consumed by retry workers.
func RenderBrief(input BriefInput) string {
	var out bytes.Buffer
	fmt.Fprintf(&out, "# Recovery context for issue #%d\n\n", input.IssueNumber)
	fmt.Fprintf(&out, "- Issue: #%d\n", input.IssueNumber)
	fmt.Fprintf(&out, "- Title: %s\n", input.IssueTitle)
	fmt.Fprintf(&out, "- Branch: %s\n", input.Branch)
	fmt.Fprintf(&out, "- Worktree path: %s\n", input.WorktreePath)
	fmt.Fprintf(&out, "- Log path: %s\n", input.LogPath)
	fmt.Fprintf(&out, "- Summary path: %s\n", input.SummaryPath)
	fmt.Fprintf(&out, "- Attempt: %d\n", input.AttemptNumber)
	fmt.Fprintf(&out, "- Last phase: %s\n", input.LastPhase)
	fmt.Fprintf(&out, "- Status: %s\n", input.Status)
	fmt.Fprintf(&out, "- Error: %s\n", Scrub(input.Error))
	fmt.Fprintln(&out)
	writeSection(&out, "Changed files", input.ChangedFiles)
	writeSection(&out, "Existing PR for branch", input.ExistingPRText)
	writeSection(&out, "Scrubbed log tail (last 50 lines)", Scrub(input.LogTail))
	return out.String()
}

func writeSection(out *bytes.Buffer, title, body string) {
	fmt.Fprintf(out, "## %s\n\n", title)
	fmt.Fprintln(out, "```text")
	fmt.Fprintln(out, strings.TrimRight(body, "\r\n"))
	fmt.Fprintln(out, "```")
	fmt.Fprintln(out)
}
