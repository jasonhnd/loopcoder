// Package cli wires the loopcoder command line surface.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/orchestration"
	"github.com/jasonhnd/loopcoder/internal/process"
	"github.com/jasonhnd/loopcoder/internal/report"
	"github.com/jasonhnd/loopcoder/internal/state"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
	"github.com/jasonhnd/loopcoder/internal/worker"
)

type Command struct {
	Name    string
	Summary string
}

type Deps struct {
	NewGitHubReader func(repoPath string) orchestration.GitHubReader
	ProcessAlive    func(pid int) bool
	Now             func() time.Time
	Dispatch        func(ctx context.Context, opts worker.Options) (worker.Result, error)
}

var commands = []Command{
	{Name: "dispatch", Summary: "dispatch one issue worker"},
	{Name: "ready-set", Summary: "classify ready and blocked work"},
	{Name: "resume", Summary: "reconcile a local run"},
	{Name: "recover", Summary: "recover or retry a worker attempt"},
	{Name: "verify-local", Summary: "run local verification gates"},
	{Name: "dispatch-wave", Summary: "dispatch one ready issue wave"},
}

// Commands returns the registered subcommands in root help order.
func Commands() []Command {
	out := make([]Command, len(commands))
	copy(out, commands)
	return out
}

// Run executes the CLI and returns a process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	return RunWithDeps(args, stdout, stderr, DefaultDeps())
}

func DefaultDeps() Deps {
	return Deps{
		NewGitHubReader: func(repoPath string) orchestration.GitHubReader {
			return gh.New(repoPath)
		},
		ProcessAlive: process.Alive,
		Now:          time.Now,
		Dispatch: func(ctx context.Context, opts worker.Options) (worker.Result, error) {
			return worker.Dispatch(ctx, opts, worker.DefaultDeps())
		},
	}
}

func RunWithDeps(args []string, stdout, stderr io.Writer, deps Deps) int {
	if len(args) == 0 || isHelp(args[0]) {
		PrintRootHelp(stdout)
		return 0
	}

	command, ok := findCommand(args[0])
	if !ok {
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		PrintRootHelp(stderr)
		return 2
	}

	for _, arg := range args[1:] {
		if isHelp(arg) {
			PrintCommandHelp(stdout, command)
			return 0
		}
	}

	if command.Name == "ready-set" {
		return runReadySet(args[1:], stdout, stderr, deps)
	}
	if command.Name == "resume" {
		return runResume(args[1:], stdout, stderr, deps)
	}
	if command.Name == "dispatch" {
		return runDispatch(args[1:], stdout, stderr, deps)
	}

	fmt.Fprintf(stderr, "%s: not yet implemented; see docs/go-migration.md\n", command.Name)
	return 1
}

// PrintRootHelp writes root command help.
func PrintRootHelp(w io.Writer) {
	fmt.Fprintln(w, "loopcoder is the native helper CLI for the loopcoder conductor.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  loopcoder <command> [flags]")
	fmt.Fprintln(w, "  loopcoder --help")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	for _, command := range commands {
		fmt.Fprintf(w, "  %-14s %s\n", command.Name, command.Summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, `Use "loopcoder <command> --help" for command help.`)
}

// PrintCommandHelp writes help for one registered command.
func PrintCommandHelp(w io.Writer, command Command) {
	fmt.Fprintf(w, "Usage:\n  loopcoder %s [flags]\n\n", command.Name)
	fmt.Fprintf(w, "%s\n\n", sentenceCase(command.Summary))
	fmt.Fprintln(w, "Flags:")
	if command.Name == "dispatch" {
		fmt.Fprintln(w, "  --repo string               repository path (required)")
		fmt.Fprintln(w, "  --issue-number int          GitHub issue number (required)")
		fmt.Fprintln(w, "  --issue-title string        GitHub issue title (required)")
		fmt.Fprintln(w, "  --issue-body string         GitHub issue body")
		fmt.Fprintln(w, "  --base-branch string        base branch to fetch and branch from (default \"main\")")
		fmt.Fprintln(w, "  --branch string             worker branch (default loop/issue-<issue-number>)")
		fmt.Fprintln(w, "  --run-id string             run id (default generated)")
		fmt.Fprintln(w, "  --attempt int               attempt number (default 1)")
		fmt.Fprintln(w, "  --recovery-context string   prior recovery context to append to the prompt")
		fmt.Fprintln(w, "  --provider string           worker provider (default \"codex\")")
		fmt.Fprintln(w, "  --model string              optional Codex model pass-through")
		fmt.Fprintln(w, "  --effort string             optional Codex reasoning effort pass-through")
		fmt.Fprintln(w, "  --keep-worktree             preserve the scratch worktree and logs")
	}
	if command.Name == "ready-set" {
		fmt.Fprintln(w, "  --repo string          repository path (required)")
		fmt.Fprintln(w, "  --base-branch string   base branch for dependency reasoning (default \"main\")")
		fmt.Fprintln(w, "  --run-id string        local run id to inspect (default latest local run when present)")
		fmt.Fprintln(w, "  --format string        output format: text, json, or both (default \"text\")")
		fmt.Fprintln(w, "  --include-closed       include closed issues as diagnostic non-ready entries")
	}
	if command.Name == "resume" {
		fmt.Fprintln(w, "  --repo string          repository path (required)")
		fmt.Fprintln(w, "  --base-branch string   base branch for branch and dependency reasoning (default \"main\")")
		fmt.Fprintln(w, "  --run-id string        local run id to inspect (default latest local run when present)")
	}
	fmt.Fprintln(w, "  --help    show command help")
}

func findCommand(name string) (Command, bool) {
	for _, command := range commands {
		if command.Name == name {
			return command, true
		}
	}
	return Command{}, false
}

func isHelp(arg string) bool {
	return arg == "-h" || arg == "--help" || arg == "help"
}

func sentenceCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:] + "."
}

func runReadySet(args []string, stdout, stderr io.Writer, deps Deps) int {
	if deps.NewGitHubReader == nil {
		deps.NewGitHubReader = DefaultDeps().NewGitHubReader
	}
	if deps.ProcessAlive == nil {
		deps.ProcessAlive = DefaultDeps().ProcessAlive
	}
	if deps.Now == nil {
		deps.Now = DefaultDeps().Now
	}

	fs := flag.NewFlagSet("ready-set", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var repoPath string
	var repoAlias string
	var baseBranch string
	var baseBranchAlias string
	var runID string
	var runIDAlias string
	var outputFormat string
	var outputFormatAlias string
	var includeClosed bool
	var includeClosedAlias bool

	fs.StringVar(&repoPath, "repo", "", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")
	fs.StringVar(&baseBranch, "base-branch", "main", "base branch")
	fs.StringVar(&baseBranchAlias, "BaseBranch", "", "base branch")
	fs.StringVar(&runID, "run-id", "", "run id")
	fs.StringVar(&runIDAlias, "RunId", "", "run id")
	fs.StringVar(&outputFormat, "format", "text", "output format")
	fs.StringVar(&outputFormatAlias, "Format", "", "output format")
	fs.BoolVar(&includeClosed, "include-closed", false, "include closed issues")
	fs.BoolVar(&includeClosedAlias, "IncludeClosed", false, "include closed issues")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if repoPath == "" {
		repoPath = repoAlias
	}
	if baseBranchAlias != "" {
		baseBranch = baseBranchAlias
	}
	if runIDAlias != "" {
		runID = runIDAlias
	}
	if outputFormatAlias != "" {
		outputFormat = outputFormatAlias
	}
	includeClosed = includeClosed || includeClosedAlias

	if strings.TrimSpace(repoPath) == "" {
		fmt.Fprintln(stderr, "ready-set: --repo is required")
		return 2
	}
	switch outputFormat {
	case "text", "json", "both":
	default:
		fmt.Fprintf(stderr, "ready-set: invalid --format %q; want text, json, or both\n", outputFormat)
		return 2
	}

	resolvedRepo, err := resolveRepo(repoPath)
	if err != nil {
		fmt.Fprintf(stderr, "ready-set: %v\n", err)
		return 2
	}

	cfg := config.Default()
	loaded, err := config.Load(filepath.Join(resolvedRepo, ".delivery.yml"))
	if err == nil {
		cfg = loaded
	} else if !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stderr, "ready-set: %v\n", err)
		return 1
	}

	if strings.TrimSpace(runID) == "" {
		latestRunID, err := state.LatestRunID(resolvedRepo)
		if err != nil {
			fmt.Fprintf(stderr, "ready-set: %v\n", err)
			return 1
		}
		runID = latestRunID
	}

	attempts, err := state.LoadAttempts(resolvedRepo, runID)
	if err != nil {
		fmt.Fprintf(stderr, "ready-set: %v\n", err)
		return 1
	}

	readyReport, err := orchestration.ComputeReadySet(context.Background(), orchestration.Options{
		Reader:        deps.NewGitHubReader(resolvedRepo),
		RepoPath:      resolvedRepo,
		BaseBranch:    baseBranch,
		RunID:         runID,
		IncludeClosed: includeClosed,
		Attempts:      attempts,
		Thresholds:    cfg.Resilience.Worker,
		ProcessAlive:  deps.ProcessAlive,
		Now:           deps.Now(),
	})
	if err != nil {
		fmt.Fprintf(stderr, "ready-set: %v\n", err)
		return 1
	}

	if outputFormat == "text" || outputFormat == "both" {
		fmt.Fprint(stdout, report.RenderReadySetText(readyReport))
	}
	if outputFormat == "both" {
		fmt.Fprintln(stdout)
	}
	if outputFormat == "json" || outputFormat == "both" {
		data, err := report.MarshalReadySetJSON(readyReport)
		if err != nil {
			fmt.Fprintf(stderr, "ready-set: %v\n", err)
			return 1
		}
		if _, err := stdout.Write(data); err != nil {
			fmt.Fprintf(stderr, "ready-set: write output: %v\n", err)
			return 1
		}
	}
	return 0
}

func runDispatch(args []string, stdout, stderr io.Writer, deps Deps) int {
	if deps.Dispatch == nil {
		deps.Dispatch = DefaultDeps().Dispatch
	}

	fs := flag.NewFlagSet("dispatch", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var opts worker.Options
	var repoAlias string
	var issueNumberAlias int
	var issueTitleAlias string
	var issueBodyAlias string
	var baseBranchAlias string
	var branchAlias string
	var runIDAlias string
	var attemptAlias int
	var recoveryContextAlias string
	var providerAlias string
	var modelAlias string
	var effortAlias string
	var keepWorktreeAlias bool

	fs.StringVar(&opts.RepoPath, "repo", "", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")
	fs.IntVar(&opts.IssueNumber, "issue-number", 0, "issue number")
	fs.IntVar(&issueNumberAlias, "IssueNumber", 0, "issue number")
	fs.StringVar(&opts.IssueTitle, "issue-title", "", "issue title")
	fs.StringVar(&issueTitleAlias, "IssueTitle", "", "issue title")
	fs.StringVar(&opts.IssueBody, "issue-body", "", "issue body")
	fs.StringVar(&issueBodyAlias, "IssueBody", "", "issue body")
	fs.StringVar(&opts.BaseBranch, "base-branch", "main", "base branch")
	fs.StringVar(&baseBranchAlias, "BaseBranch", "", "base branch")
	fs.StringVar(&opts.Branch, "branch", "", "branch")
	fs.StringVar(&branchAlias, "Branch", "", "branch")
	fs.StringVar(&opts.RunID, "run-id", "", "run id")
	fs.StringVar(&runIDAlias, "RunId", "", "run id")
	fs.IntVar(&opts.Attempt, "attempt", 1, "attempt")
	fs.IntVar(&attemptAlias, "Attempt", 0, "attempt")
	fs.StringVar(&opts.RecoveryContext, "recovery-context", "", "recovery context")
	fs.StringVar(&recoveryContextAlias, "RecoveryContext", "", "recovery context")
	fs.StringVar(&opts.Provider, "provider", "codex", "provider")
	fs.StringVar(&providerAlias, "Provider", "", "provider")
	fs.StringVar(&opts.Model, "model", "", "model")
	fs.StringVar(&modelAlias, "Model", "", "model")
	fs.StringVar(&opts.Effort, "effort", "", "effort")
	fs.StringVar(&effortAlias, "Effort", "", "effort")
	fs.BoolVar(&opts.KeepWorktree, "keep-worktree", false, "keep worktree")
	fs.BoolVar(&keepWorktreeAlias, "KeepWorktree", false, "keep worktree")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if opts.RepoPath == "" {
		opts.RepoPath = repoAlias
	}
	if issueNumberAlias != 0 {
		opts.IssueNumber = issueNumberAlias
	}
	if issueTitleAlias != "" {
		opts.IssueTitle = issueTitleAlias
	}
	if issueBodyAlias != "" {
		opts.IssueBody = issueBodyAlias
	}
	if baseBranchAlias != "" {
		opts.BaseBranch = baseBranchAlias
	}
	if branchAlias != "" {
		opts.Branch = branchAlias
	}
	if runIDAlias != "" {
		opts.RunID = runIDAlias
	}
	if attemptAlias != 0 {
		opts.Attempt = attemptAlias
	}
	if recoveryContextAlias != "" {
		opts.RecoveryContext = recoveryContextAlias
	}
	if providerAlias != "" {
		opts.Provider = providerAlias
	}
	if modelAlias != "" {
		opts.Model = modelAlias
	}
	if effortAlias != "" {
		opts.Effort = effortAlias
	}
	opts.KeepWorktree = opts.KeepWorktree || keepWorktreeAlias
	opts.Stderr = stderr

	if strings.TrimSpace(opts.RepoPath) == "" {
		fmt.Fprintln(stderr, "dispatch: --repo is required")
		return 2
	}
	if opts.IssueNumber <= 0 {
		fmt.Fprintln(stderr, "dispatch: --issue-number is required")
		return 2
	}
	if strings.TrimSpace(opts.IssueTitle) == "" {
		fmt.Fprintln(stderr, "dispatch: --issue-title is required")
		return 2
	}

	result, err := deps.Dispatch(context.Background(), opts)
	if err != nil {
		fmt.Fprintf(stderr, "dispatch: %v\n", err)
		return 1
	}
	data, err := worker.MarshalResult(result)
	if err != nil {
		fmt.Fprintf(stderr, "dispatch: %v\n", err)
		return 1
	}
	if _, err := stdout.Write(append(data, '\n')); err != nil {
		fmt.Fprintf(stderr, "dispatch: write output: %v\n", err)
		return 1
	}
	return 0
}

func runResume(args []string, stdout, stderr io.Writer, deps Deps) int {
	if deps.NewGitHubReader == nil {
		deps.NewGitHubReader = DefaultDeps().NewGitHubReader
	}
	if deps.ProcessAlive == nil {
		deps.ProcessAlive = DefaultDeps().ProcessAlive
	}
	if deps.Now == nil {
		deps.Now = DefaultDeps().Now
	}

	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var repoPath string
	var repoAlias string
	var baseBranch string
	var baseBranchAlias string
	var runID string
	var runIDAlias string

	fs.StringVar(&repoPath, "repo", "", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")
	fs.StringVar(&baseBranch, "base-branch", "main", "base branch")
	fs.StringVar(&baseBranchAlias, "BaseBranch", "", "base branch")
	fs.StringVar(&runID, "run-id", "", "run id")
	fs.StringVar(&runIDAlias, "RunId", "", "run id")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if repoPath == "" {
		repoPath = repoAlias
	}
	if baseBranchAlias != "" {
		baseBranch = baseBranchAlias
	}
	if runIDAlias != "" {
		runID = runIDAlias
	}

	if strings.TrimSpace(repoPath) == "" {
		fmt.Fprintln(stderr, "resume: --repo is required")
		return 2
	}

	resolvedRepo, err := resolveRepo(repoPath)
	if err != nil {
		fmt.Fprintf(stderr, "resume: %v\n", err)
		return 2
	}

	cfg := config.Default()
	loaded, err := config.Load(filepath.Join(resolvedRepo, ".delivery.yml"))
	if err == nil {
		cfg = loaded
	} else if !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stderr, "resume: %v\n", err)
		return 1
	}

	runNote := "requested run"
	if strings.TrimSpace(runID) == "" {
		runID, runNote, err = latestRunIDWithNote(resolvedRepo)
		if err != nil {
			fmt.Fprintf(stderr, "resume: %v\n", err)
			return 1
		}
	}

	attempts, err := state.LoadAttempts(resolvedRepo, runID)
	if err != nil {
		fmt.Fprintf(stderr, "resume: %v\n", err)
		return 1
	}
	eventCount, err := state.CountEvents(resolvedRepo, runID)
	if err != nil {
		fmt.Fprintf(stderr, "resume: %v\n", err)
		return 1
	}

	resumeReport, err := orchestration.ComputeResume(context.Background(), orchestration.ResumeOptions{
		Reader:       deps.NewGitHubReader(resolvedRepo),
		RepoPath:     resolvedRepo,
		BaseBranch:   baseBranch,
		RunID:        runID,
		RunNote:      runNote,
		Attempts:     attempts,
		EventCount:   eventCount,
		Thresholds:   cfg.Resilience.Worker,
		ProcessAlive: deps.ProcessAlive,
		Now:          deps.Now(),
	})
	if err != nil {
		fmt.Fprintf(stderr, "resume: %v\n", err)
		return 1
	}

	fmt.Fprint(stdout, report.RenderResumeText(resumeReport))
	return 0
}

func latestRunIDWithNote(repoPath string) (string, string, error) {
	runsRoot := state.RunsRoot(repoPath)
	info, err := os.Stat(runsRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ".loopcoder/runs not found", nil
		}
		return "", "", fmt.Errorf("read runs directory: %w", err)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("runs path is not a directory: %s", runsRoot)
	}

	runID, err := state.LatestRunID(repoPath)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(runID) == "" {
		return "", "no run directories found", nil
	}
	return runID, "latest modified run selected", nil
}

func resolveRepo(repoPath string) (string, error) {
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
