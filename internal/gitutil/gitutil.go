package gitutil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/execresult"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
)

const gitHardCapDefault = lcdefaults.GitCommandHardCap

var (
	gitCommand   = "git"
	gitHardCap   = gitHardCapDefault
	buildGitArgs = gitArgs
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
	if ctx == nil {
		ctx = context.Background()
	}
	allArgs := buildGitArgs(repoPath, args...)
	cmd := exec.CommandContext(ctx, gitCommand, allArgs...)
	cmd.Env = CleanEnv(os.Environ())

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	result, err := supervisedexec.Run(ctx, cmd, supervisedexec.Options{HardCap: gitHardCap})
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if detail != "" {
			return nil, fmt.Errorf("git %s: %w: %s", strings.Join(allArgs, " "), err, detail)
		}
		return nil, fmt.Errorf("git %s: %w", strings.Join(allArgs, " "), err)
	}
	if result.Outcome == supervisedexec.OutcomeDeadline {
		return nil, fmt.Errorf("git %s timed out after %s", strings.Join(allArgs, " "), gitHardCap)
	}
	if result.ExitCode != 0 {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		err := execresult.CommandExitError(cmd, result.ExitCode)
		if detail != "" {
			return nil, fmt.Errorf("git %s: %w: %s", strings.Join(allArgs, " "), err, detail)
		}
		return nil, fmt.Errorf("git %s: %w", strings.Join(allArgs, " "), err)
	}
	return stdout.Bytes(), nil
}

func gitArgs(repoPath string, args ...string) []string {
	allArgs := append([]string{"-C", repoPath}, args...)
	return allArgs
}

// CleanEnv removes Git repository-scoping environment variables that override
// explicit -C paths and temporary fixture repositories.
func CleanEnv(environ []string) []string {
	blocked := map[string]bool{
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
		"GIT_COMMON_DIR":                   true,
		"GIT_DIR":                          true,
		"GIT_INDEX_FILE":                   true,
		"GIT_NAMESPACE":                    true,
		"GIT_OBJECT_DIRECTORY":             true,
		"GIT_WORK_TREE":                    true,
	}
	cleaned := make([]string, 0, len(environ))
	for _, entry := range environ {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if blocked[key] || strings.HasPrefix(key, "GIT_CONFIG") {
			continue
		}
		cleaned = append(cleaned, entry)
	}
	return cleaned
}

func (c *Client) run(ctx context.Context, repoPath string, args ...string) ([]byte, error) {
	if c == nil || c.runner == nil {
		return nil, fmt.Errorf("git client is not configured")
	}
	return c.runner.RunGit(ctx, repoPath, args...)
}

// FetchOriginBase runs git fetch origin <baseBranch>.
func (c *Client) FetchOriginBase(ctx context.Context, repoPath, baseBranch string) error {
	baseBranch = strings.TrimSpace(baseBranch)
	if baseBranch == "" {
		return fmt.Errorf("base branch is required")
	}
	refspec := "+refs/heads/" + baseBranch + ":refs/remotes/origin/" + baseBranch
	_, err := c.run(ctx, repoPath, "fetch", "-q", "origin", refspec)
	return err
}

// FetchPRHead fetches a pull request head into FETCH_HEAD.
func (c *Client) FetchPRHead(ctx context.Context, repoPath string, prNumber int) error {
	_, err := c.run(ctx, repoPath, "fetch", "-q", "origin", fmt.Sprintf("pull/%d/head", prNumber))
	return err
}

// FetchPRHeadRef fetches a pull request head into destRef.
func (c *Client) FetchPRHeadRef(ctx context.Context, repoPath string, prNumber int, destRef string) error {
	destRef = strings.TrimSpace(destRef)
	if prNumber <= 0 {
		return fmt.Errorf("pull request number is required")
	}
	if destRef == "" {
		return fmt.Errorf("destination ref is required")
	}
	refspec := fmt.Sprintf("+refs/pull/%d/head:%s", prNumber, destRef)
	_, err := c.run(ctx, repoPath, "fetch", "-q", "origin", refspec)
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

// IsPathAbsentOnRef reports whether err is the reliable git show signal for a
// path that is absent from an otherwise valid ref.
func IsPathAbsentOnRef(err error, path string) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	path = strings.ToLower(strings.TrimSpace(path))
	if path == "" {
		return false
	}
	text := strings.ToLower(err.Error())
	quotedPath := "'" + path + "'"
	for _, signal := range []string{
		"path " + quotedPath + " does not exist in '",
		"path " + quotedPath + " exists on disk, but not in '",
		quotedPath + " exists on disk, but not in '",
		path + " exists on disk, but not in '",
	} {
		if strings.Contains(text, signal) {
			return true
		}
	}
	return false
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

// PushUpstreamForceWithLease force-updates a scratch branch and sets upstream.
// Present remote branches pin the lease to their ls-remote SHA; absent remote
// branches intentionally use an empty expected SHA so git requires ref absence.
func (c *Client) PushUpstreamForceWithLease(ctx context.Context, repoPath, branch string) error {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return fmt.Errorf("branch is required")
	}
	remoteRef := "refs/heads/" + branch
	expected, err := c.remoteBranchSHA(ctx, repoPath, remoteRef)
	if err != nil {
		return err
	}
	lease := "--force-with-lease=" + remoteRef + ":" + expected
	_, err = c.run(ctx, repoPath, "push", lease, "-u", "origin", "HEAD:"+remoteRef)
	return err
}

// BranchDelete deletes a local branch forcefully.
func (c *Client) BranchDelete(ctx context.Context, repoPath, branch string) error {
	_, err := c.run(ctx, repoPath, "branch", "-D", branch)
	return err
}

func (c *Client) remoteBranchSHA(ctx context.Context, repoPath, remoteRef string) (string, error) {
	output, err := c.run(ctx, repoPath, "ls-remote", "origin", remoteRef)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(output))
	if line == "" {
		return "", nil
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", nil
	}
	return strings.TrimSpace(fields[0]), nil
}
