package verify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/gitutil"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
)

const (
	StatusPass       = "pass"
	StatusFail       = "fail"
	StatusNeedsHuman = "needs-human"

	commandHardCapDefault = lcdefaults.VerifyCommandHardCap
)

var commandHardCap = commandHardCapDefault

type Options struct {
	RepoPath   string
	PRNumber   int
	Branch     string
	BaseBranch string
}

type Result struct {
	Summary  Summary
	ExitCode int
}

type Summary struct {
	Repo              string        `json:"repo"`
	Worktree          string        `json:"worktree,omitempty"`
	PR                *int          `json:"pr"`
	Branch            *string       `json:"branch"`
	BaseBranch        string        `json:"base_branch"`
	GeneratedAt       string        `json:"generated_at"`
	LocalCommandGates string        `json:"local_command_gates"`
	Verdict           string        `json:"verdict"`
	Error             string        `json:"error,omitempty"`
	Groups            []GroupResult `json:"groups"`
	Cleanup           []string      `json:"cleanup"`
}

type GroupResult struct {
	Group    string          `json:"group"`
	Status   string          `json:"status"`
	Commands []CommandResult `json:"commands"`
}

type CommandResult struct {
	Command  string   `json:"command"`
	ExitCode int      `json:"exit_code"`
	Status   string   `json:"status"`
	Reason   string   `json:"reason"`
	LogTail  []string `json:"log_tail"`
}

type CommandInvocation struct {
	File             string
	Args             []string
	WorkingDirectory string
}

type CommandRunResult struct {
	ExitCode int
	Output   []string
	Err      error
}

type CommandRunner interface {
	Run(ctx context.Context, invocation CommandInvocation) CommandRunResult
}

type GitClient interface {
	FetchOriginBase(ctx context.Context, repoPath, baseBranch string) error
	WorktreeAddDetached(ctx context.Context, repoPath, worktreePath, baseBranch string) error
	WorktreeRemove(ctx context.Context, repoPath, worktreePath string) error
	FetchOriginBranch(ctx context.Context, repoPath, branch string) error
	CheckoutDetached(ctx context.Context, repoPath, rev string) error
	RevParse(ctx context.Context, repoPath, rev string) (string, error)
}

type Deps struct {
	Git       GitClient
	Runner    CommandRunner
	Now       func() time.Time
	MkdirTemp func(dir, pattern string) (string, error)
	RemoveAll func(path string) error
}

type ExecRunner struct{}

type commandGroup struct {
	name     string
	commands []string
}

func DefaultDeps() Deps {
	return Deps{
		Git:       gitutil.New(),
		Runner:    ExecRunner{},
		Now:       time.Now,
		MkdirTemp: os.MkdirTemp,
		RemoveAll: os.RemoveAll,
	}
}

func Run(ctx context.Context, opts Options, deps Deps) Result {
	deps = withDefaults(deps)
	if strings.TrimSpace(opts.BaseBranch) == "" {
		opts.BaseBranch = lcdefaults.BaseBranch
	}

	summary := Summary{
		Repo:        opts.RepoPath,
		PR:          prPointer(opts.PRNumber),
		Branch:      branchPointer(opts.Branch),
		BaseBranch:  opts.BaseBranch,
		GeneratedAt: deps.Now().UTC().Format(time.RFC3339Nano),
		Verdict:     StatusNeedsHuman,
		Groups:      []GroupResult{},
		Cleanup:     []string{},
	}

	repoPath, err := resolveRepo(opts.RepoPath)
	if err != nil {
		summary.LocalCommandGates = "error"
		summary.Error = err.Error()
		return Result{Summary: summary, ExitCode: 2}
	}
	summary.Repo = repoPath

	groups, err := loadCommandGroups(repoPath)
	if err != nil {
		summary.LocalCommandGates = "error"
		summary.Error = err.Error()
		return Result{Summary: summary, ExitCode: 2}
	}
	if len(groups) == 0 {
		summary.LocalCommandGates = "not-configured"
		summary.Verdict = StatusPass
		return Result{Summary: summary, ExitCode: 0}
	}
	summary.LocalCommandGates = "configured"

	var scratchPath string
	if scratchPath, err = deps.MkdirTemp("", "loopcoder-verify-local-*"); err != nil {
		summary.LocalCommandGates = "error"
		summary.Error = fmt.Sprintf("create scratch directory: %v", err)
		return Result{Summary: summary, ExitCode: 2}
	}
	worktreePath := filepath.Join(scratchPath, "wt")
	summary.Worktree = worktreePath

	cleanup := func() {
		if strings.TrimSpace(worktreePath) != "" {
			if err := deps.Git.WorktreeRemove(context.Background(), repoPath, worktreePath); err != nil {
				summary.Cleanup = append(summary.Cleanup, fmt.Sprintf("worktree cleanup failed: %v", err))
			} else {
				summary.Cleanup = append(summary.Cleanup, "removed worktree "+worktreePath)
			}
		}
		if strings.TrimSpace(scratchPath) != "" {
			if err := deps.RemoveAll(scratchPath); err != nil {
				summary.Cleanup = append(summary.Cleanup, fmt.Sprintf("scratch cleanup failed: %v", err))
			} else {
				summary.Cleanup = append(summary.Cleanup, "removed scratch "+scratchPath)
			}
		}
	}

	if err := checkoutIsolated(ctx, deps, repoPath, worktreePath, opts); err != nil {
		cleanup()
		summary.LocalCommandGates = "error"
		summary.Error = err.Error()
		return Result{Summary: summary, ExitCode: 2}
	}

	groupResults := make([]GroupResult, 0, len(groups))
	for _, group := range groups {
		groupResults = append(groupResults, runCommandGroup(ctx, deps.Runner, group, worktreePath))
	}
	summary.Groups = groupResults
	summary.Verdict = verdictForGroups(groupResults)
	cleanup()

	return Result{Summary: summary, ExitCode: exitCodeForVerdict(summary.Verdict)}
}

func Render(w io.Writer, result Result) error {
	summary := result.Summary
	if summary.LocalCommandGates == "not-configured" {
		if _, err := fmt.Fprintln(w, "no local command gates configured"); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprintln(w, "LOCAL VERIFICATION SUMMARY"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "verdict: %s\n", summary.Verdict); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "local_command_gates: %s\n", summary.LocalCommandGates); err != nil {
		return err
	}
	for _, group := range summary.Groups {
		if _, err := fmt.Fprintf(w, "- %s: %s\n", group.Group, group.Status); err != nil {
			return err
		}
		for _, command := range group.Commands {
			if _, err := fmt.Fprintf(w, "  - %s exit=%d command=%s\n", command.Status, command.ExitCode, command.Command); err != nil {
				return err
			}
		}
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "JSON SUMMARY"); err != nil {
		return err
	}
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	_, err = fmt.Fprintln(w)
	return err
}

func (ExecRunner) Run(ctx context.Context, invocation CommandInvocation) CommandRunResult {
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, invocation.File, invocation.Args...)
	cmd.Dir = invocation.WorkingDirectory

	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output

	result, err := supervisedexec.Run(ctx, cmd, supervisedexec.Options{HardCap: commandHardCap})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return CommandRunResult{ExitCode: -1, Output: splitLines(output.String()), Err: ctxErr}
		}
		lines := splitLines(output.String())
		if len(lines) == 0 {
			lines = []string{err.Error()}
		}
		return CommandRunResult{ExitCode: 127, Output: lines, Err: err}
	}
	if result.Outcome == supervisedexec.OutcomeDeadline {
		lines := splitLines(output.String())
		err := fmt.Errorf("%s timed out after %s", strings.Join(append([]string{invocation.File}, invocation.Args...), " "), commandHardCap)
		if len(lines) == 0 {
			lines = []string{err.Error()}
		}
		return CommandRunResult{ExitCode: -1, Output: lines, Err: err}
	}
	if result.ExitCode != 0 {
		return CommandRunResult{ExitCode: result.ExitCode, Output: splitLines(output.String())}
	}
	return CommandRunResult{ExitCode: 0, Output: splitLines(output.String())}
}

func withDefaults(deps Deps) Deps {
	defaults := DefaultDeps()
	if deps.Git == nil {
		deps.Git = defaults.Git
	}
	if deps.Runner == nil {
		deps.Runner = defaults.Runner
	}
	if deps.Now == nil {
		deps.Now = defaults.Now
	}
	if deps.MkdirTemp == nil {
		deps.MkdirTemp = defaults.MkdirTemp
	}
	if deps.RemoveAll == nil {
		deps.RemoveAll = defaults.RemoveAll
	}
	return deps
}

func loadCommandGroups(repoPath string) ([]commandGroup, error) {
	cfg := config.Default()
	loaded, err := config.Load(filepath.Join(repoPath, ".delivery.yml"))
	if err == nil {
		cfg = loaded
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	groups := []commandGroup{
		{name: "tests", commands: cleanCommands(cfg.CI.Tests)},
		{name: "typecheck", commands: cleanCommands(cfg.CI.Typecheck)},
		{name: "build", commands: cleanCommands(cfg.CI.Build)},
	}
	configured := make([]commandGroup, 0, len(groups))
	for _, group := range groups {
		if len(group.commands) > 0 {
			configured = append(configured, group)
		}
	}
	return configured, nil
}

func cleanCommands(commands []string) []string {
	out := make([]string, 0, len(commands))
	for _, command := range commands {
		if trimmed := strings.TrimSpace(command); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func checkoutIsolated(ctx context.Context, deps Deps, repoPath, worktreePath string, opts Options) error {
	if err := deps.Git.FetchOriginBase(ctx, repoPath, opts.BaseBranch); err != nil {
		return fmt.Errorf("could not fetch base branch %q: %w", opts.BaseBranch, err)
	}
	if err := deps.Git.WorktreeAddDetached(ctx, repoPath, worktreePath, opts.BaseBranch); err != nil {
		return fmt.Errorf("could not create isolated worktree: %w", err)
	}

	if opts.PRNumber > 0 {
		checkout := deps.Runner.Run(ctx, CommandInvocation{
			File:             "gh",
			Args:             []string{"pr", "checkout", strconv.Itoa(opts.PRNumber), "--detach"},
			WorkingDirectory: worktreePath,
		})
		if checkout.Err != nil || checkout.ExitCode != 0 {
			return fmt.Errorf("could not check out PR #%d: %s", opts.PRNumber, strings.Join(checkout.Output, "\n"))
		}
		return nil
	}

	remoteBranch := opts.Branch
	remoteBranch = strings.TrimPrefix(remoteBranch, "origin/")
	if err := deps.Git.FetchOriginBranch(ctx, worktreePath, remoteBranch); err == nil {
		if err := deps.Git.CheckoutDetached(ctx, worktreePath, "FETCH_HEAD"); err != nil {
			return fmt.Errorf("could not check out fetched branch %q: %w", opts.Branch, err)
		}
		return nil
	}

	commit, err := deps.Git.RevParse(ctx, repoPath, opts.Branch+"^{commit}")
	if err != nil {
		return fmt.Errorf("could not resolve branch %q: %w", opts.Branch, err)
	}
	if err := deps.Git.CheckoutDetached(ctx, worktreePath, commit); err != nil {
		return fmt.Errorf("could not check out local branch %q: %w", opts.Branch, err)
	}
	return nil
}

func runCommandGroup(ctx context.Context, runner CommandRunner, group commandGroup, worktreePath string) GroupResult {
	results := make([]CommandResult, 0, len(group.commands))
	for _, command := range group.commands {
		run := runner.Run(ctx, shellInvocation(command, worktreePath))
		status, reason := classifyCommand(command, run)
		results = append(results, CommandResult{
			Command:  command,
			ExitCode: run.ExitCode,
			Status:   status,
			Reason:   reason,
			LogTail:  boundedLogTail(run.Output),
		})
	}

	return GroupResult{
		Group:    group.name,
		Status:   verdictForCommands(results),
		Commands: results,
	}
}

func shellInvocation(command, worktreePath string) CommandInvocation {
	if runtime.GOOS == "windows" {
		return CommandInvocation{
			File:             "cmd",
			Args:             []string{"/c", command},
			WorkingDirectory: worktreePath,
		}
	}
	return CommandInvocation{
		File:             "sh",
		Args:             []string{"-c", command},
		WorkingDirectory: worktreePath,
	}
}

func classifyCommand(command string, run CommandRunResult) (string, string) {
	if run.ExitCode == 0 && run.Err == nil {
		return StatusPass, ""
	}
	text := strings.Join(run.Output, "\n")
	if run.Err != nil {
		if matchesAny(text, missingToolPatterns) || run.ExitCode == 126 || run.ExitCode == 127 {
			return StatusNeedsHuman, "missing-tool"
		}
		return StatusNeedsHuman, "command-runner-error"
	}
	if run.ExitCode == 126 || run.ExitCode == 127 || matchesAny(text, missingToolPatterns) {
		return StatusNeedsHuman, "missing-tool"
	}
	if matchesAny(command, dependencyInstallCommands) || matchesAny(text, dependencyFailurePatterns) {
		return StatusNeedsHuman, "dependency-install-failure"
	}
	if matchesAny(text, shellSyntaxPatterns) {
		return StatusNeedsHuman, "unsupported-shell-syntax"
	}
	return StatusFail, "command-exit-nonzero"
}

func verdictForCommands(commands []CommandResult) string {
	verdict := StatusPass
	for _, command := range commands {
		if command.Status == StatusNeedsHuman {
			return StatusNeedsHuman
		}
		if command.Status == StatusFail {
			verdict = StatusFail
		}
	}
	return verdict
}

func verdictForGroups(groups []GroupResult) string {
	verdict := StatusPass
	for _, group := range groups {
		if group.Status == StatusNeedsHuman {
			return StatusNeedsHuman
		}
		if group.Status == StatusFail {
			verdict = StatusFail
		}
	}
	return verdict
}

func exitCodeForVerdict(verdict string) int {
	switch verdict {
	case StatusPass:
		return 0
	case StatusFail:
		return 1
	default:
		return 2
	}
}

func boundedLogTail(lines []string) []string {
	const (
		maxLines     = 80
		maxChars     = 12000
		maxLineChars = 500
	)

	trimmed := make([]string, 0, len(lines))
	for _, line := range lines {
		if len(line) > maxLineChars {
			line = line[:maxLineChars] + "..."
		}
		trimmed = append(trimmed, line)
	}
	if len(trimmed) > maxLines {
		trimmed = trimmed[len(trimmed)-maxLines:]
	}
	for len(trimmed) > 1 && len(strings.Join(trimmed, "\n")) > maxChars {
		trimmed = trimmed[1:]
	}
	return trimmed
}

func splitLines(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimRight(text, "\n\r")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func resolveRepo(repoPath string) (string, error) {
	if strings.TrimSpace(repoPath) == "" {
		return "", errors.New("repo path is required")
	}
	absolute, err := filepath.Abs(repoPath)
	if err != nil {
		return "", fmt.Errorf("resolve repo path: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve repo path: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("repo path is not a directory: %s", absolute)
	}
	return absolute, nil
}

func prPointer(number int) *int {
	if number <= 0 {
		return nil
	}
	value := number
	return &value
}

func branchPointer(branch string) *string {
	if strings.TrimSpace(branch) == "" {
		return nil
	}
	value := branch
	return &value
}

func matchesAny(text string, patterns []*regexp.Regexp) bool {
	for _, pattern := range patterns {
		if pattern.MatchString(text) {
			return true
		}
	}
	return false
}

var missingToolPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)CommandNotFoundException`),
	regexp.MustCompile(`(?i)The term '.+' is not recognized`),
	regexp.MustCompile(`(?i)is not recognized as (an internal|the name)`),
	regexp.MustCompile(`(?i)command not found`),
	regexp.MustCompile(`(?i)executable file not found`),
	regexp.MustCompile(`(?i)No such file or directory.*(npm|pnpm|yarn|bun|node|python|pip|cargo|go|dotnet|pwsh|bash)`),
	regexp.MustCompile(`(?i)The system cannot find the file specified`),
}

var dependencyInstallCommands = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^\s*(npm|pnpm|yarn|bun)\s+(ci|install)\b`),
	regexp.MustCompile(`(?i)^\s*(pip|pip3)\s+install\b`),
	regexp.MustCompile(`(?i)^\s*poetry\s+install\b`),
	regexp.MustCompile(`(?i)^\s*cargo\s+fetch\b`),
	regexp.MustCompile(`(?i)^\s*go\s+mod\s+download\b`),
	regexp.MustCompile(`(?i)^\s*dotnet\s+restore\b`),
}

var dependencyFailurePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(EAI_AGAIN|ENOTFOUND|ECONNRESET|ETIMEDOUT|CERT_HAS_EXPIRED|SELF_SIGNED_CERT)\b`),
	regexp.MustCompile(`(?i)unable to verify the first certificate`),
	regexp.MustCompile(`(?i)Could not resolve host`),
	regexp.MustCompile(`(?i)failed to (fetch|download|resolve)`),
	regexp.MustCompile(`(?i)network.*(error|unreachable|timeout)`),
	regexp.MustCompile(`(?i)install.*failed`),
}

var shellSyntaxPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)syntax error near unexpected token`),
	regexp.MustCompile(`(?i)Syntax error:`),
	regexp.MustCompile(`(?i)unexpected EOF while looking for matching`),
	regexp.MustCompile(`(?i)unterminated quoted string`),
	regexp.MustCompile(`(?i)The syntax of the command is incorrect`),
}
