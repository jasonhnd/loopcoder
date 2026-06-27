package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// Reader is the GitHub read surface used by orchestration. Tests can inject a
// fake implementation so no real gh credentials are required.
type Reader interface {
	RepoName(ctx context.Context) (string, error)
	ListIssues(ctx context.Context, state string) ([]Issue, error)
	ViewIssue(ctx context.Context, number int) (Issue, error)
	ListOpenPRs(ctx context.Context) ([]PullRequest, error)
	PRChecks(ctx context.Context, number int) ([]Check, error)
}

type Writer interface {
	CreatePR(ctx context.Context, head, base, title, body string) (string, error)
	ListHeadPRs(ctx context.Context, branch string) ([]PullRequestReference, error)
}

type Runner interface {
	Run(ctx context.Context, dir, name string, args ...string) ([]byte, error)
}

type CLI struct {
	repoPath string
	runner   Runner
}

type Label struct {
	Name string `json:"name"`
}

type PullRequestReference struct {
	Number   int    `json:"number,omitempty"`
	Title    string `json:"title,omitempty"`
	URL      string `json:"url,omitempty"`
	State    string `json:"state,omitempty"`
	MergedAt string `json:"mergedAt,omitempty"`
	Merged   bool   `json:"merged,omitempty"`
}

type IssueReference struct {
	Number int    `json:"number"`
	Title  string `json:"title,omitempty"`
	State  string `json:"state,omitempty"`
}

type Issue struct {
	Number                         int                    `json:"number"`
	Title                          string                 `json:"title"`
	Body                           string                 `json:"body"`
	State                          string                 `json:"state"`
	StateReason                    string                 `json:"stateReason"`
	Labels                         []Label                `json:"labels"`
	ClosedByPullRequestsReferences []PullRequestReference `json:"closedByPullRequestsReferences"`
}

type PullRequest struct {
	Number                  int              `json:"number"`
	Title                   string           `json:"title"`
	Body                    string           `json:"body"`
	URL                     string           `json:"url"`
	HeadRefName             string           `json:"headRefName"`
	IsDraft                 bool             `json:"isDraft"`
	ClosingIssuesReferences []IssueReference `json:"closingIssuesReferences"`
}

type Check struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Bucket string `json:"bucket"`
}

// New returns a gh-backed reader rooted at repoPath.
func New(repoPath string) *CLI {
	return NewWithRunner(repoPath, ExecRunner{})
}

// NewWithRunner returns a gh-backed client using runner.
func NewWithRunner(repoPath string, runner Runner) *CLI {
	if runner == nil {
		runner = ExecRunner{}
	}
	return &CLI{repoPath: repoPath, runner: runner}
}

// ExecRunner runs external commands through exec.CommandContext.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if detail != "" {
			return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, detail)
		}
		return nil, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return stdout.Bytes(), nil
}

func (c *CLI) RepoName(ctx context.Context) (string, error) {
	var payload struct {
		NameWithOwner string `json:"nameWithOwner"`
	}
	if err := c.runJSON(ctx, []string{"repo", "view", "--json", "nameWithOwner"}, &payload); err == nil {
		if strings.TrimSpace(payload.NameWithOwner) != "" {
			return payload.NameWithOwner, nil
		}
	}

	remote, err := c.gitRemote(ctx)
	if err == nil {
		if parsed := parseGitHubRemote(remote); parsed != "" {
			return parsed, nil
		}
		if strings.TrimSpace(remote) != "" {
			return strings.TrimSpace(remote), nil
		}
	}

	return c.repoPath, nil
}

func (c *CLI) ListIssues(ctx context.Context, state string) ([]Issue, error) {
	if strings.TrimSpace(state) == "" {
		state = "open"
	}
	var issues []Issue
	err := c.runJSON(ctx, []string{
		"issue", "list",
		"--state", state,
		"--limit", "1000",
		"--json", "number,title,labels,state,stateReason",
	}, &issues)
	return issues, err
}

func (c *CLI) ViewIssue(ctx context.Context, number int) (Issue, error) {
	var issue Issue
	err := c.runJSON(ctx, []string{
		"issue", "view", fmt.Sprintf("%d", number),
		"--json", "number,title,body,state,stateReason,labels,closedByPullRequestsReferences",
	}, &issue)
	return issue, err
}

func (c *CLI) ListOpenPRs(ctx context.Context) ([]PullRequest, error) {
	var prs []PullRequest
	err := c.runJSON(ctx, []string{
		"pr", "list",
		"--state", "open",
		"--limit", "1000",
		"--json", "number,title,url,headRefName,isDraft,closingIssuesReferences",
	}, &prs)
	return prs, err
}

func (c *CLI) ViewPR(ctx context.Context, number int) (PullRequest, error) {
	var pr PullRequest
	err := c.runJSON(ctx, []string{
		"pr", "view", fmt.Sprintf("%d", number),
		"--json", "number,title,body,url,headRefName,isDraft,closingIssuesReferences",
	}, &pr)
	return pr, err
}

func (c *CLI) PRDiff(ctx context.Context, number int) (string, error) {
	output, err := c.run(ctx, "gh", "pr", "diff", fmt.Sprintf("%d", number))
	if err != nil {
		return "", err
	}
	return string(output), nil
}

func (c *CLI) PRDiffNameOnly(ctx context.Context, number int) ([]string, error) {
	output, err := c.run(ctx, "gh", "pr", "diff", fmt.Sprintf("%d", number), "--name-only")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.ReplaceAll(string(output), "\r\n", "\n"), "\n")
	files := make([]string, 0, len(lines))
	for _, line := range lines {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			files = append(files, trimmed)
		}
	}
	return files, nil
}

func (c *CLI) PRChecks(ctx context.Context, number int) ([]Check, error) {
	var checks []Check
	err := c.runJSON(ctx, []string{
		"pr", "checks", fmt.Sprintf("%d", number),
		"--json", "name,state,bucket",
	}, &checks)
	if err != nil {
		return nil, err
	}
	return checks, nil
}

func (c *CLI) CreatePR(ctx context.Context, head, base, title, body string) (string, error) {
	output, err := c.run(ctx, "gh", "pr", "create", "--head", head, "--base", base, "--title", title, "--body", body)
	if err != nil {
		return "", err
	}
	url := strings.TrimSpace(string(output))
	if url == "" {
		return "", fmt.Errorf("gh pr create returned an empty URL")
	}
	return url, nil
}

func (c *CLI) ListHeadPRs(ctx context.Context, branch string) ([]PullRequestReference, error) {
	var prs []PullRequestReference
	err := c.runJSON(ctx, []string{
		"pr", "list",
		"--head", branch,
		"--json", "number,url",
	}, &prs)
	return prs, err
}

func (c *CLI) runJSON(ctx context.Context, args []string, target any) error {
	output, err := c.run(ctx, "gh", args...)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(output)) == 0 {
		return nil
	}
	return parseJSONOutput(output, target)
}

func (c *CLI) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if c == nil || c.runner == nil {
		return nil, fmt.Errorf("github client is not configured")
	}
	return c.runner.Run(ctx, c.repoPath, name, args...)
}

func (c *CLI) gitRemote(ctx context.Context) (string, error) {
	output, err := c.run(ctx, "git", "remote", "get-url", "origin")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func parseJSONOutput(output []byte, target any) error {
	cleaned := trimToJSON(output)
	if len(cleaned) == 0 {
		return fmt.Errorf("no JSON object or array found")
	}
	if err := json.Unmarshal(cleaned, target); err != nil {
		return fmt.Errorf("parse gh JSON: %w", err)
	}
	return nil
}

func trimToJSON(output []byte) []byte {
	text := strings.TrimSpace(string(output))
	if text == "" {
		return nil
	}

	arrayStart := strings.Index(text, "[")
	objectStart := strings.Index(text, "{")
	start := -1
	switch {
	case arrayStart >= 0 && objectStart >= 0 && arrayStart < objectStart:
		start = arrayStart
	case arrayStart >= 0 && objectStart >= 0:
		start = objectStart
	case arrayStart >= 0:
		start = arrayStart
	case objectStart >= 0:
		start = objectStart
	}
	if start < 0 {
		return nil
	}

	text = strings.TrimSpace(text[start:])
	if strings.HasPrefix(text, "[") {
		if end := strings.LastIndex(text, "]"); end >= 0 {
			text = text[:end+1]
		}
	} else if strings.HasPrefix(text, "{") {
		if end := strings.LastIndex(text, "}"); end >= 0 {
			text = text[:end+1]
		}
	}
	return []byte(strings.TrimSpace(text))
}

var githubRemotePattern = regexp.MustCompile(`github\.com[:/]([^/]+)/(.+?)(?:\.git)?$`)

func parseGitHubRemote(remote string) string {
	matches := githubRemotePattern.FindStringSubmatch(strings.TrimSpace(remote))
	if len(matches) != 3 {
		return ""
	}
	return matches[1] + "/" + matches[2]
}
