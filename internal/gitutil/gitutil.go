package gitutil

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Runner is the process surface used by Client. Tests can inject a fake runner
// so git helpers do not shell out.
type Runner interface {
	RunGit(ctx context.Context, repoPath string, args ...string) ([]byte, error)
}

// Client wraps the git commands loopcoder needs for worker dispatch.
type Client struct {
	runner Runner
}

// New returns an exec-backed git client.
func New() *Client {
	return NewWithRunner(ExecRunner{})
}

// NewWithRunner returns a git client using runner.
func NewWithRunner(runner Runner) *Client {
	if runner == nil {
		runner = ExecRunner{}
	}
	return &Client{runner: runner}
}

// ExecRunner runs git through exec.CommandContext.
type ExecRunner struct{}

// RunGit runs git -C <repoPath> <args...> and returns stdout.
func (ExecRunner) RunGit(ctx context.Context, repoPath string, args ...string) ([]byte, error) {
	allArgs := append([]string{"-C", repoPath}, args...)
	cmd := exec.CommandContext(ctx, "git", allArgs...)

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
			return nil, fmt.Errorf("git %s: %w: %s", strings.Join(allArgs, " "), err, detail)
		}
		return nil, fmt.Errorf("git %s: %w", strings.Join(allArgs, " "), err)
	}
	return stdout.Bytes(), nil
}

func (c *Client) run(ctx context.Context, repoPath string, args ...string) ([]byte, error) {
	if c == nil || c.runner == nil {
		return nil, fmt.Errorf("git client is not configured")
	}
	return c.runner.RunGit(ctx, repoPath, args...)
}

// FetchOriginBase runs git fetch origin <baseBranch>.
func (c *Client) FetchOriginBase(ctx context.Context, repoPath, baseBranch string) error {
	_, err := c.run(ctx, repoPath, "fetch", "origin", baseBranch)
	return err
}

// FetchPRHead fetches a pull request head into FETCH_HEAD.
func (c *Client) FetchPRHead(ctx context.Context, repoPath string, prNumber int) error {
	_, err := c.run(ctx, repoPath, "fetch", "-q", "origin", fmt.Sprintf("pull/%d/head", prNumber))
	return err
}

// WorktreeAdd creates a branch worktree from origin/<baseBranch>.
func (c *Client) WorktreeAdd(ctx context.Context, repoPath, branch, worktreePath, baseBranch string) error {
	_, err := c.run(ctx, repoPath, "worktree", "add", "-b", branch, worktreePath, "origin/"+baseBranch)
	return err
}

// WorktreeAddDetached creates a detached worktree from origin/<baseBranch>.
func (c *Client) WorktreeAddDetached(ctx context.Context, repoPath, worktreePath, baseBranch string) error {
	_, err := c.run(ctx, repoPath, "worktree", "add", "--detach", worktreePath, "origin/"+baseBranch)
	return err
}

// WorktreeAddDetachedAt creates a detached worktree at rev.
func (c *Client) WorktreeAddDetachedAt(ctx context.Context, repoPath, worktreePath, rev string) error {
	_, err := c.run(ctx, repoPath, "worktree", "add", "--detach", worktreePath, rev)
	return err
}

// WorktreeRemove removes a worktree forcefully.
func (c *Client) WorktreeRemove(ctx context.Context, repoPath, worktreePath string) error {
	_, err := c.run(ctx, repoPath, "worktree", "remove", "--force", worktreePath)
	return err
}

// FetchOriginBranch fetches one branch from origin into the selected repo.
func (c *Client) FetchOriginBranch(ctx context.Context, repoPath, branch string) error {
	_, err := c.run(ctx, repoPath, "fetch", "-q", "origin", branch)
	return err
}

// CheckoutDetached checks out rev detached in repoPath.
func (c *Client) CheckoutDetached(ctx context.Context, repoPath, rev string) error {
	_, err := c.run(ctx, repoPath, "checkout", "--detach", rev)
	return err
}

// RevParse resolves rev to a commit-ish string.
func (c *Client) RevParse(ctx context.Context, repoPath, rev string) (string, error) {
	output, err := c.run(ctx, repoPath, "rev-parse", "--verify", rev)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

// Show returns the contents of a git object path such as origin/main:file.
func (c *Client) Show(ctx context.Context, repoPath, revPath string) (string, error) {
	output, err := c.run(ctx, repoPath, "show", revPath)
	if err != nil {
		return "", err
	}
	return string(output), nil
}

// StatusPorcelain returns git status --porcelain output.
func (c *Client) StatusPorcelain(ctx context.Context, repoPath string) (string, error) {
	output, err := c.run(ctx, repoPath, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	return string(output), nil
}

// AddAll stages all changes.
func (c *Client) AddAll(ctx context.Context, repoPath string) error {
	_, err := c.run(ctx, repoPath, "add", "-A")
	return err
}

// Commit creates a commit with message.
func (c *Client) Commit(ctx context.Context, repoPath, message string) error {
	_, err := c.run(ctx, repoPath, "commit", "-m", message)
	return err
}

// PushUpstream pushes branch to origin and sets upstream.
func (c *Client) PushUpstream(ctx context.Context, repoPath, branch string) error {
	_, err := c.run(ctx, repoPath, "push", "-u", "origin", branch)
	return err
}

// BranchDelete deletes a local branch forcefully.
func (c *Client) BranchDelete(ctx context.Context, repoPath, branch string) error {
	_, err := c.run(ctx, repoPath, "branch", "-D", branch)
	return err
}
