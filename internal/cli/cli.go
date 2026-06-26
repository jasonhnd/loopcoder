// Package cli wires the loopcoder command line surface.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/orchestration"
	"github.com/jasonhnd/loopcoder/internal/process"
	"github.com/jasonhnd/loopcoder/internal/recovery"
	"github.com/jasonhnd/loopcoder/internal/report"
	"github.com/jasonhnd/loopcoder/internal/state"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
	"github.com/jasonhnd/loopcoder/internal/verify"
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
	Stdin           io.Reader
	ComputeReadySet func(ctx context.Context, opts orchestration.Options) (report.ReadySetReport, error)
	Dispatch        func(ctx context.Context, opts worker.Options) (worker.Result, error)
	Recover         func(ctx context.Context, opts recovery.Options) (recovery.Result, error)
	Verify          func(ctx context.Context, opts verify.Options) verify.Result
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
		Stdin:        os.Stdin,
		ComputeReadySet: func(ctx context.Context, opts orchestration.Options) (report.ReadySetReport, error) {
			return orchestration.ComputeReadySet(ctx, opts)
		},
		Dispatch: func(ctx context.Context, opts worker.Options) (worker.Result, error) {
			return worker.Dispatch(ctx, opts, worker.DefaultDeps())
		},
		Verify: func(ctx context.Context, opts verify.Options) verify.Result {
			return verify.Run(ctx, opts, verify.DefaultDeps())
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
	if command.Name == "recover" {
		return runRecover(args[1:], stdout, stderr, deps)
	}
	if command.Name == "verify-local" {
		return runVerifyLocal(args[1:], stdout, stderr, deps)
	}
	if command.Name == "dispatch-wave" {
		return runDispatchWave(args[1:], stdout, stderr, deps)
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
	if command.Name == "recover" {
		fmt.Fprintln(w, "  --repo string                   repository path (required)")
		fmt.Fprintln(w, "  --issue-number int              GitHub issue number (required)")
		fmt.Fprintln(w, "  --issue-title string            GitHub issue title (required)")
		fmt.Fprintln(w, "  --issue-body string             GitHub issue body")
		fmt.Fprintln(w, "  --run-id string                 run id containing attempt history (required)")
		fmt.Fprintln(w, "  --base-branch string            retry base branch (default \"main\")")
		fmt.Fprintln(w, "  --max-attempts int              retry limit (default 3)")
		fmt.Fprintln(w, "  --backoff-seconds string        comma-separated retry backoff schedule (default \"10,30,120\")")
		fmt.Fprintln(w, "  --provider string               worker provider (default \"codex\")")
		fmt.Fprintln(w, "  --model string                  optional Codex model pass-through")
		fmt.Fprintln(w, "  --effort string                 optional Codex reasoning effort pass-through")
	}
	if command.Name == "verify-local" {
		fmt.Fprintln(w, "  --repo string          repository path (required)")
		fmt.Fprintln(w, "  --pr-number int        pull request number to verify (required unless --branch is set)")
		fmt.Fprintln(w, "  --branch string        branch to verify (required unless --pr-number is set)")
		fmt.Fprintln(w, "  --base-branch string   base branch for isolated checkout (default \"main\")")
	}
	if command.Name == "dispatch-wave" {
		fmt.Fprintln(w, "  --repo string              repository path (required)")
		fmt.Fprintln(w, "  --base-branch string       base branch passed to dispatch (default \"main\")")
		fmt.Fprintln(w, "  --run-id string            shared run id for the wave (default generated once)")
		fmt.Fprintln(w, "  --issue-numbers string     comma-separated issue numbers to dispatch")
		fmt.Fprintln(w, "  --from-ready-set           read ready-set JSON from stdin")
		fmt.Fprintln(w, "  --ready-set-path string    read ready-set JSON from a file")
		fmt.Fprintln(w, "  --provider string          optional worker provider pass-through")
		fmt.Fprintln(w, "  --model string             optional Codex model pass-through")
		fmt.Fprintln(w, "  --effort string            optional Codex reasoning effort pass-through")
		fmt.Fprintln(w, "  --throttle-limit int       maximum concurrent dispatches (default 4)")
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
	if deps.ComputeReadySet == nil {
		deps.ComputeReadySet = DefaultDeps().ComputeReadySet
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

	readyReport, err := deps.ComputeReadySet(context.Background(), orchestration.Options{
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

func runDispatchWave(args []string, stdout, stderr io.Writer, deps Deps) int {
	defaults := DefaultDeps()
	if deps.NewGitHubReader == nil {
		deps.NewGitHubReader = defaults.NewGitHubReader
	}
	if deps.ProcessAlive == nil {
		deps.ProcessAlive = defaults.ProcessAlive
	}
	if deps.Now == nil {
		deps.Now = defaults.Now
	}
	if deps.Stdin == nil {
		deps.Stdin = defaults.Stdin
	}
	if deps.ComputeReadySet == nil {
		deps.ComputeReadySet = defaults.ComputeReadySet
	}
	if deps.Dispatch == nil {
		deps.Dispatch = defaults.Dispatch
	}

	fs := flag.NewFlagSet("dispatch-wave", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var repoPath string
	var repoAlias string
	var baseBranch string
	var baseBranchAlias string
	var runID string
	var runIDAlias string
	var issueNumbersValue string
	var issueNumbersAlias string
	var fromReadySet bool
	var fromReadySetAlias bool
	var readySetPath string
	var readySetPathAlias string
	var provider string
	var providerAlias string
	var model string
	var modelAlias string
	var effort string
	var effortAlias string
	var throttleLimit int
	var throttleLimitAlias int

	fs.StringVar(&repoPath, "repo", "", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")
	fs.StringVar(&baseBranch, "base-branch", "main", "base branch")
	fs.StringVar(&baseBranchAlias, "BaseBranch", "", "base branch")
	fs.StringVar(&runID, "run-id", "", "run id")
	fs.StringVar(&runIDAlias, "RunId", "", "run id")
	fs.StringVar(&issueNumbersValue, "issue-numbers", "", "issue numbers")
	fs.StringVar(&issueNumbersAlias, "IssueNumbers", "", "issue numbers")
	fs.BoolVar(&fromReadySet, "from-ready-set", false, "read ready-set JSON from stdin")
	fs.BoolVar(&fromReadySetAlias, "FromReadySet", false, "read ready-set JSON from stdin")
	fs.StringVar(&readySetPath, "ready-set-path", "", "ready-set JSON path")
	fs.StringVar(&readySetPathAlias, "ReadySetPath", "", "ready-set JSON path")
	fs.StringVar(&provider, "provider", "", "provider")
	fs.StringVar(&providerAlias, "Provider", "", "provider")
	fs.StringVar(&model, "model", "", "model")
	fs.StringVar(&modelAlias, "Model", "", "model")
	fs.StringVar(&effort, "effort", "", "effort")
	fs.StringVar(&effortAlias, "Effort", "", "effort")
	fs.IntVar(&throttleLimit, "throttle-limit", 4, "throttle limit")
	fs.IntVar(&throttleLimitAlias, "ThrottleLimit", 0, "throttle limit")

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
	if issueNumbersAlias != "" {
		issueNumbersValue = issueNumbersAlias
	}
	fromReadySet = fromReadySet || fromReadySetAlias
	if readySetPathAlias != "" {
		readySetPath = readySetPathAlias
	}
	if providerAlias != "" {
		provider = providerAlias
	}
	if modelAlias != "" {
		model = modelAlias
	}
	if effortAlias != "" {
		effort = effortAlias
	}
	if throttleLimitAlias != 0 {
		throttleLimit = throttleLimitAlias
	}

	if strings.TrimSpace(repoPath) == "" {
		fmt.Fprintln(stderr, "dispatch-wave: --repo is required")
		return 2
	}
	if throttleLimit <= 0 {
		fmt.Fprintln(stderr, "dispatch-wave: --throttle-limit must be greater than zero")
		return 2
	}

	selectionCount := 0
	if strings.TrimSpace(issueNumbersValue) != "" {
		selectionCount++
	}
	if fromReadySet {
		selectionCount++
	}
	if strings.TrimSpace(readySetPath) != "" {
		selectionCount++
	}
	if selectionCount > 1 {
		fmt.Fprintln(stderr, "dispatch-wave: choose only one of --issue-numbers, --from-ready-set, or --ready-set-path")
		return 2
	}

	var issueNumbers []int
	var readySet *report.ReadySetReport
	var err error
	if strings.TrimSpace(issueNumbersValue) != "" {
		issueNumbers, err = parseIssueNumbers(issueNumbersValue)
		if err != nil {
			fmt.Fprintf(stderr, "dispatch-wave: %v\n", err)
			return 2
		}
	}
	if fromReadySet {
		readySet, err = readReadySetJSON(deps.Stdin)
		if err != nil {
			fmt.Fprintf(stderr, "dispatch-wave: %v\n", err)
			return 2
		}
	}
	if strings.TrimSpace(readySetPath) != "" {
		file, openErr := os.Open(readySetPath)
		if openErr != nil {
			fmt.Fprintf(stderr, "dispatch-wave: read ready-set file: %v\n", openErr)
			return 2
		}
		defer file.Close()
		readySet, err = readReadySetJSON(file)
		if err != nil {
			fmt.Fprintf(stderr, "dispatch-wave: %v\n", err)
			return 2
		}
	}

	resolvedRepo, err := resolveRepo(repoPath)
	if err != nil {
		fmt.Fprintf(stderr, "dispatch-wave: %v\n", err)
		return 2
	}

	cfg := config.Default()
	loaded, err := config.Load(filepath.Join(resolvedRepo, ".delivery.yml"))
	if err == nil {
		cfg = loaded
	} else if !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stderr, "dispatch-wave: %v\n", err)
		return 1
	}

	waveReport, err := orchestration.DispatchWave(context.Background(), orchestration.DispatchWaveOptions{
		Reader:          deps.NewGitHubReader(resolvedRepo),
		RepoPath:        resolvedRepo,
		BaseBranch:      baseBranch,
		RunID:           runID,
		IssueNumbers:    issueNumbers,
		ReadySet:        readySet,
		Provider:        provider,
		Model:           model,
		Effort:          effort,
		ThrottleLimit:   throttleLimit,
		Thresholds:      cfg.Resilience.Worker,
		ProcessAlive:    deps.ProcessAlive,
		Now:             deps.Now(),
		Stderr:          stderr,
		ComputeReadySet: deps.ComputeReadySet,
		Dispatch:        deps.Dispatch,
	})
	if err != nil {
		fmt.Fprintf(stderr, "dispatch-wave: %v\n", err)
		return 1
	}
	if _, err := stdout.Write([]byte(orchestration.RenderDispatchWaveText(waveReport))); err != nil {
		fmt.Fprintf(stderr, "dispatch-wave: write output: %v\n", err)
		return 1
	}
	if orchestration.DispatchWaveHasFailures(waveReport) {
		return 1
	}
	return 0
}

func runRecover(args []string, stdout, stderr io.Writer, deps Deps) int {
	if deps.Dispatch == nil {
		deps.Dispatch = DefaultDeps().Dispatch
	}
	if deps.Recover == nil {
		deps.Recover = recoverWithDispatch(deps.Dispatch)
	}

	fs := flag.NewFlagSet("recover", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var opts recovery.Options
	var repoAlias string
	var issueNumberAlias int
	var issueTitleAlias string
	var issueBodyAlias string
	var runIDAlias string
	var baseBranchAlias string
	var maxAttemptsAlias int
	var backoffSecondsValue string
	var backoffSecondsAlias string
	var providerAlias string
	var modelAlias string
	var effortAlias string

	fs.StringVar(&opts.RepoPath, "repo", "", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")
	fs.IntVar(&opts.IssueNumber, "issue-number", 0, "issue number")
	fs.IntVar(&issueNumberAlias, "IssueNumber", 0, "issue number")
	fs.StringVar(&opts.IssueTitle, "issue-title", "", "issue title")
	fs.StringVar(&issueTitleAlias, "IssueTitle", "", "issue title")
	fs.StringVar(&opts.IssueBody, "issue-body", "", "issue body")
	fs.StringVar(&issueBodyAlias, "IssueBody", "", "issue body")
	fs.StringVar(&opts.RunID, "run-id", "", "run id")
	fs.StringVar(&runIDAlias, "RunId", "", "run id")
	fs.StringVar(&opts.BaseBranch, "base-branch", "main", "base branch")
	fs.StringVar(&baseBranchAlias, "BaseBranch", "", "base branch")
	fs.IntVar(&opts.MaxAttempts, "max-attempts", 3, "max attempts")
	fs.IntVar(&maxAttemptsAlias, "MaxAttempts", 0, "max attempts")
	fs.StringVar(&backoffSecondsValue, "backoff-seconds", "10,30,120", "backoff seconds")
	fs.StringVar(&backoffSecondsAlias, "BackoffSeconds", "", "backoff seconds")
	fs.StringVar(&opts.Provider, "provider", "codex", "provider")
	fs.StringVar(&providerAlias, "Provider", "", "provider")
	fs.StringVar(&opts.Model, "model", "", "model")
	fs.StringVar(&modelAlias, "Model", "", "model")
	fs.StringVar(&opts.Effort, "effort", "", "effort")
	fs.StringVar(&effortAlias, "Effort", "", "effort")

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
	if runIDAlias != "" {
		opts.RunID = runIDAlias
	}
	if baseBranchAlias != "" {
		opts.BaseBranch = baseBranchAlias
	}
	if maxAttemptsAlias != 0 {
		opts.MaxAttempts = maxAttemptsAlias
	}
	if backoffSecondsAlias != "" {
		backoffSecondsValue = backoffSecondsAlias
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
	opts.Stderr = stderr

	backoffSeconds, err := parseBackoffSeconds(backoffSecondsValue)
	if err != nil {
		fmt.Fprintf(stderr, "recover: %v\n", err)
		return 2
	}
	opts.BackoffSeconds = backoffSeconds

	if strings.TrimSpace(opts.RepoPath) == "" {
		fmt.Fprintln(stderr, "recover: --repo is required")
		return 2
	}
	if opts.IssueNumber <= 0 {
		fmt.Fprintln(stderr, "recover: --issue-number is required")
		return 2
	}
	if strings.TrimSpace(opts.IssueTitle) == "" {
		fmt.Fprintln(stderr, "recover: --issue-title is required")
		return 2
	}
	if strings.TrimSpace(opts.RunID) == "" {
		fmt.Fprintln(stderr, "recover: --run-id is required")
		return 2
	}

	result, err := deps.Recover(context.Background(), opts)
	if result.Report != "" {
		if _, writeErr := stdout.Write([]byte(result.Report)); writeErr != nil {
			fmt.Fprintf(stderr, "recover: write output: %v\n", writeErr)
			return 1
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "recover: %v\n", err)
		return 1
	}
	if result.Action == recovery.ActionBlocked {
		return 1
	}
	return 0
}

func runVerifyLocal(args []string, stdout, stderr io.Writer, deps Deps) int {
	if deps.Verify == nil {
		deps.Verify = DefaultDeps().Verify
	}

	fs := flag.NewFlagSet("verify-local", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var opts verify.Options
	var repoAlias string
	var prNumberAlias int
	var branchAlias string
	var baseBranchAlias string

	fs.StringVar(&opts.RepoPath, "repo", "", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")
	fs.IntVar(&opts.PRNumber, "pr-number", 0, "pull request number")
	fs.IntVar(&prNumberAlias, "PrNumber", 0, "pull request number")
	fs.StringVar(&opts.Branch, "branch", "", "branch")
	fs.StringVar(&branchAlias, "Branch", "", "branch")
	fs.StringVar(&opts.BaseBranch, "base-branch", "main", "base branch")
	fs.StringVar(&baseBranchAlias, "BaseBranch", "", "base branch")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if opts.RepoPath == "" {
		opts.RepoPath = repoAlias
	}
	if prNumberAlias != 0 {
		opts.PRNumber = prNumberAlias
	}
	if branchAlias != "" {
		opts.Branch = branchAlias
	}
	if baseBranchAlias != "" {
		opts.BaseBranch = baseBranchAlias
	}

	if strings.TrimSpace(opts.RepoPath) == "" {
		fmt.Fprintln(stderr, "verify-local: --repo is required")
		return 2
	}
	hasPR := opts.PRNumber > 0
	hasBranch := strings.TrimSpace(opts.Branch) != ""
	if hasPR == hasBranch {
		fmt.Fprintln(stderr, "verify-local: exactly one of --pr-number or --branch is required")
		return 2
	}

	result := deps.Verify(context.Background(), opts)
	if err := verify.Render(stdout, result); err != nil {
		fmt.Fprintf(stderr, "verify-local: write output: %v\n", err)
		return 1
	}
	return result.ExitCode
}

func recoverWithDispatch(dispatch func(ctx context.Context, opts worker.Options) (worker.Result, error)) func(context.Context, recovery.Options) (recovery.Result, error) {
	return func(ctx context.Context, opts recovery.Options) (recovery.Result, error) {
		recoverDeps := recovery.DefaultDeps()
		recoverDeps.Dispatch = func(ctx context.Context, dispatchOpts recovery.DispatchOptions) (recovery.DispatchResult, error) {
			result, err := dispatch(ctx, worker.Options{
				RepoPath:        dispatchOpts.RepoPath,
				IssueNumber:     dispatchOpts.IssueNumber,
				IssueTitle:      dispatchOpts.IssueTitle,
				IssueBody:       dispatchOpts.IssueBody,
				BaseBranch:      dispatchOpts.BaseBranch,
				Branch:          dispatchOpts.Branch,
				RunID:           dispatchOpts.RunID,
				Attempt:         dispatchOpts.Attempt,
				RecoveryContext: dispatchOpts.RecoveryContext,
				Provider:        dispatchOpts.Provider,
				Model:           dispatchOpts.Model,
				Effort:          dispatchOpts.Effort,
				Stderr:          dispatchOpts.Stderr,
			})
			if err != nil {
				return recovery.DispatchResult{}, err
			}
			return recovery.DispatchResult{
				OK:          result.OK,
				Issue:       result.Issue,
				Branch:      result.Branch,
				RunID:       result.RunID,
				PR:          result.PR,
				Summary:     result.Summary,
				AttemptPath: result.AttemptPath,
				Status:      result.Status,
				ExitCode:    result.ExitCode,
				LogBytes:    result.LogBytes,
			}, nil
		}
		return recovery.Run(ctx, opts, recoverDeps)
	}
}

func parseBackoffSeconds(value string) ([]int, error) {
	if strings.TrimSpace(value) == "" {
		return []int{}, nil
	}
	parts := strings.Split(value, ",")
	out := make([]int, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			return nil, fmt.Errorf("invalid --backoff-seconds %q", value)
		}
		seconds, err := strconv.Atoi(trimmed)
		if err != nil || seconds < 0 {
			return nil, fmt.Errorf("invalid --backoff-seconds %q", value)
		}
		out = append(out, seconds)
	}
	return out, nil
}

func parseIssueNumbers(value string) ([]int, error) {
	parts := strings.Split(value, ",")
	out := make([]int, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			return nil, fmt.Errorf("invalid --issue-numbers %q", value)
		}
		number, err := strconv.Atoi(trimmed)
		if err != nil || number <= 0 {
			return nil, fmt.Errorf("invalid --issue-numbers %q", value)
		}
		out = append(out, number)
	}
	return out, nil
}

func readReadySetJSON(r io.Reader) (*report.ReadySetReport, error) {
	var readySet report.ReadySetReport
	if err := json.NewDecoder(r).Decode(&readySet); err != nil {
		return nil, fmt.Errorf("read ready-set JSON: %w", err)
	}
	return &readySet, nil
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
