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

	"github.com/jasonhnd/loopcoder/internal/attestation"
	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/loopreview"
	"github.com/jasonhnd/loopcoder/internal/orchestration"
	"github.com/jasonhnd/loopcoder/internal/process"
	"github.com/jasonhnd/loopcoder/internal/recovery"
	"github.com/jasonhnd/loopcoder/internal/report"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/statebranch"
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
	Loopreview      func(ctx context.Context, opts loopreview.Options) (loopreview.Result, error)
	Recover         func(ctx context.Context, opts recovery.Options) (recovery.Result, error)
	Verify          func(ctx context.Context, opts verify.Options) verify.Result
	StatePush       func(ctx context.Context, opts statebranch.PushOptions) (statebranch.PushResult, error)
	StatePull       func(ctx context.Context, opts statebranch.PullOptions) (statebranch.PullResult, error)
	LeaseAcquire    func(ctx context.Context, opts statebranch.LeaseOptions) (statebranch.LeaseResult, error)
	LeaseRelease    func(ctx context.Context, opts statebranch.LeaseOptions) (statebranch.LeaseResult, error)
}

var commands = []Command{
	{Name: "attest", Summary: "emit conductor self-attestation"},
	{Name: "dispatch", Summary: "dispatch one issue worker"},
	{Name: "ready-set", Summary: "classify ready and blocked work"},
	{Name: "resume", Summary: "reconcile a local run"},
	{Name: "state", Summary: "publish or pull durable run state"},
	{Name: "lease", Summary: "manage the conductor lease"},
	{Name: "recover", Summary: "recover or retry a worker attempt"},
	{Name: "loopreview", Summary: "run an independent read-only PR verifier"},
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
		Loopreview: func(ctx context.Context, opts loopreview.Options) (loopreview.Result, error) {
			return loopreview.Run(ctx, opts, loopreview.DefaultDeps())
		},
		Verify: func(ctx context.Context, opts verify.Options) verify.Result {
			return verify.Run(ctx, opts, verify.DefaultDeps())
		},
		StatePush: func(ctx context.Context, opts statebranch.PushOptions) (statebranch.PushResult, error) {
			return statebranch.Push(ctx, opts, statebranch.DefaultDeps())
		},
		StatePull: func(ctx context.Context, opts statebranch.PullOptions) (statebranch.PullResult, error) {
			return statebranch.Pull(ctx, opts, statebranch.DefaultDeps())
		},
		LeaseAcquire: func(ctx context.Context, opts statebranch.LeaseOptions) (statebranch.LeaseResult, error) {
			return statebranch.Acquire(ctx, opts, statebranch.DefaultDeps())
		},
		LeaseRelease: func(ctx context.Context, opts statebranch.LeaseOptions) (statebranch.LeaseResult, error) {
			return statebranch.Release(ctx, opts, statebranch.DefaultDeps())
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
	if command.Name == "attest" {
		return runAttest(args[1:], stdout, stderr, deps)
	}
	if command.Name == "resume" {
		return runResume(args[1:], stdout, stderr, deps)
	}
	if command.Name == "state" {
		return runState(args[1:], stdout, stderr, deps)
	}
	if command.Name == "lease" {
		return runLease(args[1:], stdout, stderr, deps)
	}
	if command.Name == "dispatch" {
		return runDispatch(args[1:], stdout, stderr, deps)
	}
	if command.Name == "recover" {
		return runRecover(args[1:], stdout, stderr, deps)
	}
	if command.Name == "loopreview" {
		return runLoopreview(args[1:], stdout, stderr, deps)
	}
	if command.Name == "verify-local" {
		return runVerifyLocal(args[1:], stdout, stderr, deps)
	}
	if command.Name == "dispatch-wave" {
		return runDispatchWave(args[1:], stdout, stderr, deps)
	}

	fmt.Fprintf(stderr, "%s: not yet implemented; see docs/specs/0089-go-migration.md\n", command.Name)
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
	if command.Name == "state" {
		printStateHelp(w)
		return
	}
	if command.Name == "lease" {
		printLeaseHelp(w)
		return
	}

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
	if command.Name == "attest" {
		fmt.Fprintln(w, "  --role string            attestation role (default \"conductor\")")
		fmt.Fprintln(w, "  --provider string        conductor host provider (required)")
		fmt.Fprintln(w, "  --model string           conductor host model (required)")
		fmt.Fprintln(w, "  --effort string          optional reasoning effort")
		fmt.Fprintln(w, "  --permission string      conductor permission (default \"orchestrate\")")
		fmt.Fprintln(w, "  --action string          action performed by the conductor (required)")
		fmt.Fprintln(w, "  --exit-code int          conductor action exit code (default 0)")
		fmt.Fprintln(w, "  --started-at string      RFC3339 invocation start timestamp")
		fmt.Fprintln(w, "  --ended-at string        RFC3339 invocation end timestamp")
		fmt.Fprintln(w, "  --duration-ms int        invocation duration in milliseconds")
		fmt.Fprintln(w, "  --input-tokens int       input token count")
		fmt.Fprintln(w, "  --output-tokens int      output token count")
		fmt.Fprintln(w, "  --total-tokens int       total token count")
		fmt.Fprintln(w, "  --model-source string    ignored; forced to self-reported")
		fmt.Fprintln(w, "  --verified               ignored; forced to false")
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
	if command.Name == "loopreview" {
		fmt.Fprintln(w, "  --repo string          repository path (required)")
		fmt.Fprintln(w, "  --pr-number int        pull request number to review (required)")
		fmt.Fprintln(w, "  --provider string      verifier provider (required)")
		fmt.Fprintln(w, "  --base-branch string   base branch for merged spec lookup (default \"main\")")
		fmt.Fprintln(w, "  --timeout duration     verifier timeout (default 10m0s)")
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

func printStateHelp(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  loopcoder state push --repo <path> --run-id <id> [flags]")
	fmt.Fprintln(w, "  loopcoder state pull --repo <path> [flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Publish or pull durable run state on the dedicated state branch.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	fmt.Fprintln(w, "  --repo string      repository path (required)")
	fmt.Fprintln(w, "  --run-id string    run id for push")
	fmt.Fprintf(w, "  --branch string    state branch (default %q)\n", statebranch.DefaultBranch)
	fmt.Fprintf(w, "  --remote string    git remote (default %q)\n", statebranch.DefaultRemote)
	fmt.Fprintln(w, "  --help             show command help")
}

func printLeaseHelp(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  loopcoder lease acquire --repo <path> --run-id <id> [flags]")
	fmt.Fprintln(w, "  loopcoder lease release --repo <path> --run-id <id> [flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Acquire, renew, observe, or release the best-effort conductor lease.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	fmt.Fprintln(w, "  --repo string      repository path (required)")
	fmt.Fprintln(w, "  --run-id string    run id (required)")
	fmt.Fprintf(w, "  --ttl int          lease TTL seconds for acquire (default %d)\n", statebranch.DefaultTTLSeconds)
	fmt.Fprintf(w, "  --branch string    state branch (default %q)\n", statebranch.DefaultBranch)
	fmt.Fprintf(w, "  --remote string    git remote (default %q)\n", statebranch.DefaultRemote)
	fmt.Fprintln(w, "  --help             show command help")
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

func runState(args []string, stdout, stderr io.Writer, deps Deps) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "state: expected push or pull")
		printStateHelp(stderr)
		return 2
	}
	switch args[0] {
	case "push":
		return runStatePush(args[1:], stdout, stderr, deps)
	case "pull":
		return runStatePull(args[1:], stdout, stderr, deps)
	default:
		fmt.Fprintf(stderr, "state: unknown subcommand %q\n", args[0])
		printStateHelp(stderr)
		return 2
	}
}

func runStatePush(args []string, stdout, stderr io.Writer, deps Deps) int {
	if deps.StatePush == nil {
		deps.StatePush = DefaultDeps().StatePush
	}

	fs := flag.NewFlagSet("state push", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var opts statebranch.PushOptions
	var repoAlias string
	var runIDAlias string
	var branchAlias string
	var remoteAlias string

	fs.StringVar(&opts.RepoPath, "repo", "", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")
	fs.StringVar(&opts.RunID, "run-id", "", "run id")
	fs.StringVar(&runIDAlias, "RunId", "", "run id")
	fs.StringVar(&opts.Branch, "branch", statebranch.DefaultBranch, "state branch")
	fs.StringVar(&branchAlias, "Branch", "", "state branch")
	fs.StringVar(&opts.Remote, "remote", statebranch.DefaultRemote, "git remote")
	fs.StringVar(&remoteAlias, "Remote", "", "git remote")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if opts.RepoPath == "" {
		opts.RepoPath = repoAlias
	}
	if runIDAlias != "" {
		opts.RunID = runIDAlias
	}
	if branchAlias != "" {
		opts.Branch = branchAlias
	}
	if remoteAlias != "" {
		opts.Remote = remoteAlias
	}
	if strings.TrimSpace(opts.RepoPath) == "" {
		fmt.Fprintln(stderr, "state push: --repo is required")
		return 2
	}
	if strings.TrimSpace(opts.RunID) == "" {
		fmt.Fprintln(stderr, "state push: --run-id is required")
		return 2
	}

	result, err := deps.StatePush(context.Background(), opts)
	if err != nil {
		fmt.Fprintf(stderr, "state push: %v\n", err)
		return 1
	}
	renderStatePush(stdout, result)
	return 0
}

func runStatePull(args []string, stdout, stderr io.Writer, deps Deps) int {
	if deps.StatePull == nil {
		deps.StatePull = DefaultDeps().StatePull
	}

	fs := flag.NewFlagSet("state pull", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var opts statebranch.PullOptions
	var repoAlias string
	var branchAlias string
	var remoteAlias string

	fs.StringVar(&opts.RepoPath, "repo", "", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")
	fs.StringVar(&opts.Branch, "branch", statebranch.DefaultBranch, "state branch")
	fs.StringVar(&branchAlias, "Branch", "", "state branch")
	fs.StringVar(&opts.Remote, "remote", statebranch.DefaultRemote, "git remote")
	fs.StringVar(&remoteAlias, "Remote", "", "git remote")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if opts.RepoPath == "" {
		opts.RepoPath = repoAlias
	}
	if branchAlias != "" {
		opts.Branch = branchAlias
	}
	if remoteAlias != "" {
		opts.Remote = remoteAlias
	}
	if strings.TrimSpace(opts.RepoPath) == "" {
		fmt.Fprintln(stderr, "state pull: --repo is required")
		return 2
	}

	result, err := deps.StatePull(context.Background(), opts)
	if err != nil {
		fmt.Fprintf(stderr, "state pull: %v\n", err)
		return 1
	}
	renderStatePull(stdout, result)
	return 0
}

func runLease(args []string, stdout, stderr io.Writer, deps Deps) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "lease: expected acquire or release")
		printLeaseHelp(stderr)
		return 2
	}
	switch args[0] {
	case "acquire":
		return runLeaseAcquire(args[1:], stdout, stderr, deps)
	case "release":
		return runLeaseRelease(args[1:], stdout, stderr, deps)
	default:
		fmt.Fprintf(stderr, "lease: unknown subcommand %q\n", args[0])
		printLeaseHelp(stderr)
		return 2
	}
}

func runLeaseAcquire(args []string, stdout, stderr io.Writer, deps Deps) int {
	if deps.LeaseAcquire == nil {
		deps.LeaseAcquire = DefaultDeps().LeaseAcquire
	}

	fs := flag.NewFlagSet("lease acquire", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var opts statebranch.LeaseOptions
	var repoAlias string
	var runIDAlias string
	var branchAlias string
	var remoteAlias string
	var ttlSeconds int
	var ttlAlias int
	var ttlUpperAlias int

	fs.StringVar(&opts.RepoPath, "repo", "", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")
	fs.StringVar(&opts.RunID, "run-id", "", "run id")
	fs.StringVar(&runIDAlias, "RunId", "", "run id")
	fs.StringVar(&opts.Branch, "branch", statebranch.DefaultBranch, "state branch")
	fs.StringVar(&branchAlias, "Branch", "", "state branch")
	fs.StringVar(&opts.Remote, "remote", statebranch.DefaultRemote, "git remote")
	fs.StringVar(&remoteAlias, "Remote", "", "git remote")
	fs.IntVar(&ttlSeconds, "ttl", statebranch.DefaultTTLSeconds, "ttl seconds")
	fs.IntVar(&ttlAlias, "Ttl", 0, "ttl seconds")
	fs.IntVar(&ttlUpperAlias, "TTL", 0, "ttl seconds")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if opts.RepoPath == "" {
		opts.RepoPath = repoAlias
	}
	if runIDAlias != "" {
		opts.RunID = runIDAlias
	}
	if branchAlias != "" {
		opts.Branch = branchAlias
	}
	if remoteAlias != "" {
		opts.Remote = remoteAlias
	}
	if ttlAlias != 0 {
		ttlSeconds = ttlAlias
	}
	if ttlUpperAlias != 0 {
		ttlSeconds = ttlUpperAlias
	}
	if strings.TrimSpace(opts.RepoPath) == "" {
		fmt.Fprintln(stderr, "lease acquire: --repo is required")
		return 2
	}
	if strings.TrimSpace(opts.RunID) == "" {
		fmt.Fprintln(stderr, "lease acquire: --run-id is required")
		return 2
	}
	if ttlSeconds < 0 {
		fmt.Fprintln(stderr, "lease acquire: --ttl must be non-negative")
		return 2
	}
	opts.TTL = time.Duration(ttlSeconds) * time.Second

	result, err := deps.LeaseAcquire(context.Background(), opts)
	if err != nil {
		fmt.Fprintf(stderr, "lease acquire: %v\n", err)
		return 1
	}
	renderLease(stdout, "LEASE ACQUIRE", result)
	return 0
}

func runLeaseRelease(args []string, stdout, stderr io.Writer, deps Deps) int {
	if deps.LeaseRelease == nil {
		deps.LeaseRelease = DefaultDeps().LeaseRelease
	}

	fs := flag.NewFlagSet("lease release", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var opts statebranch.LeaseOptions
	var repoAlias string
	var runIDAlias string
	var branchAlias string
	var remoteAlias string

	fs.StringVar(&opts.RepoPath, "repo", "", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")
	fs.StringVar(&opts.RunID, "run-id", "", "run id")
	fs.StringVar(&runIDAlias, "RunId", "", "run id")
	fs.StringVar(&opts.Branch, "branch", statebranch.DefaultBranch, "state branch")
	fs.StringVar(&branchAlias, "Branch", "", "state branch")
	fs.StringVar(&opts.Remote, "remote", statebranch.DefaultRemote, "git remote")
	fs.StringVar(&remoteAlias, "Remote", "", "git remote")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if opts.RepoPath == "" {
		opts.RepoPath = repoAlias
	}
	if runIDAlias != "" {
		opts.RunID = runIDAlias
	}
	if branchAlias != "" {
		opts.Branch = branchAlias
	}
	if remoteAlias != "" {
		opts.Remote = remoteAlias
	}
	if strings.TrimSpace(opts.RepoPath) == "" {
		fmt.Fprintln(stderr, "lease release: --repo is required")
		return 2
	}
	if strings.TrimSpace(opts.RunID) == "" {
		fmt.Fprintln(stderr, "lease release: --run-id is required")
		return 2
	}

	result, err := deps.LeaseRelease(context.Background(), opts)
	if err != nil {
		fmt.Fprintf(stderr, "lease release: %v\n", err)
		return 1
	}
	renderLease(stdout, "LEASE RELEASE", result)
	return 0
}

func renderStatePush(w io.Writer, result statebranch.PushResult) {
	fmt.Fprintln(w, "STATE PUSH")
	fmt.Fprintf(w, "Repo: %s\n", result.RepoPath)
	fmt.Fprintf(w, "RunId: %s\n", result.RunID)
	fmt.Fprintf(w, "Branch: %s\n", result.Branch)
	fmt.Fprintf(w, "Remote: %s\n", result.Remote)
	fmt.Fprintf(w, "Committed: %t\n", result.Committed)
	if result.Pushed {
		fmt.Fprintln(w, "Push: succeeded")
	} else if result.PushError != "" {
		fmt.Fprintf(w, "Push: failed; local state branch commit retained for retry: %s\n", result.PushError)
	} else {
		fmt.Fprintln(w, "Push: skipped")
	}
	fmt.Fprintf(w, "Files: %d\n", len(result.Files))
}

func renderStatePull(w io.Writer, result statebranch.PullResult) {
	fmt.Fprintln(w, "STATE PULL")
	fmt.Fprintf(w, "Repo: %s\n", result.RepoPath)
	fmt.Fprintf(w, "Branch: %s\n", result.Branch)
	fmt.Fprintf(w, "Remote: %s\n", result.Remote)
	fmt.Fprintf(w, "Mirror: %s\n", result.MirrorPath)
	fmt.Fprintf(w, "Runs: %d\n", len(result.Runs))
	for _, runID := range result.Runs {
		fmt.Fprintf(w, "- %s\n", runID)
	}
}

func renderLease(w io.Writer, title string, result statebranch.LeaseResult) {
	fmt.Fprintln(w, title)
	fmt.Fprintf(w, "Repo: %s\n", result.RepoPath)
	fmt.Fprintf(w, "RunId: %s\n", result.RunID)
	fmt.Fprintf(w, "Branch: %s\n", result.Branch)
	fmt.Fprintf(w, "Status: %s\n", result.Status)
	if result.ObserveOnly {
		fmt.Fprintln(w, "Observe only: true")
	}
	lease := result.Lease
	if lease == nil {
		lease = result.PreviousLease
	}
	if lease != nil {
		fmt.Fprintf(w, "LeaseId: %s\n", lease.LeaseID)
		fmt.Fprintf(w, "Host: %s\n", lease.Host)
		fmt.Fprintf(w, "PID: %d\n", lease.PID)
		if lease.LeaseExpiresAt != "" {
			fmt.Fprintf(w, "ExpiresAt: %s\n", lease.LeaseExpiresAt)
		}
	}
	if result.Pushed {
		fmt.Fprintln(w, "Push: succeeded")
	} else if result.PushError != "" {
		fmt.Fprintf(w, "Push: failed: %s\n", result.PushError)
	}
	if result.Message != "" {
		fmt.Fprintf(w, "Message: %s\n", result.Message)
	}
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

func runAttest(args []string, stdout, stderr io.Writer, deps Deps) int {
	if deps.Now == nil {
		deps.Now = DefaultDeps().Now
	}

	fs := flag.NewFlagSet("attest", flag.ContinueOnError)
	fs.SetOutput(stderr)

	role := string(attestation.RoleConductor)
	var roleAlias string
	var provider string
	var providerAlias string
	var model string
	var modelAlias string
	var effort string
	var effortAlias string
	permission := string(attestation.PermissionOrchestrate)
	var permissionAlias string
	var action string
	var actionAlias string
	var exitCode int
	var exitCodeAlias int
	var startedAt string
	var startedAtAlias string
	var endedAt string
	var endedAtAlias string
	var durationMS int64
	var durationMSAlias int64
	var inputTokens int64
	var inputTokensAlias int64
	var outputTokens int64
	var outputTokensAlias int64
	var totalTokens int64
	var totalTokensAlias int64
	var ignoredModelSource string
	var ignoredModelSourceAlias string
	var ignoredVerified bool
	var ignoredVerifiedAlias bool

	fs.StringVar(&role, "role", role, "role")
	fs.StringVar(&roleAlias, "Role", "", "role")
	fs.StringVar(&provider, "provider", "", "provider")
	fs.StringVar(&providerAlias, "Provider", "", "provider")
	fs.StringVar(&model, "model", "", "model")
	fs.StringVar(&modelAlias, "Model", "", "model")
	fs.StringVar(&effort, "effort", "", "effort")
	fs.StringVar(&effortAlias, "Effort", "", "effort")
	fs.StringVar(&permission, "permission", permission, "permission")
	fs.StringVar(&permissionAlias, "Permission", "", "permission")
	fs.StringVar(&action, "action", "", "action")
	fs.StringVar(&actionAlias, "Action", "", "action")
	fs.IntVar(&exitCode, "exit-code", 0, "exit code")
	fs.IntVar(&exitCodeAlias, "ExitCode", 0, "exit code")
	fs.StringVar(&startedAt, "started-at", "", "started at")
	fs.StringVar(&startedAtAlias, "StartedAt", "", "started at")
	fs.StringVar(&endedAt, "ended-at", "", "ended at")
	fs.StringVar(&endedAtAlias, "EndedAt", "", "ended at")
	fs.Int64Var(&durationMS, "duration-ms", 0, "duration milliseconds")
	fs.Int64Var(&durationMSAlias, "DurationMs", 0, "duration milliseconds")
	fs.Int64Var(&inputTokens, "input-tokens", 0, "input tokens")
	fs.Int64Var(&inputTokensAlias, "InputTokens", 0, "input tokens")
	fs.Int64Var(&outputTokens, "output-tokens", 0, "output tokens")
	fs.Int64Var(&outputTokensAlias, "OutputTokens", 0, "output tokens")
	fs.Int64Var(&totalTokens, "total-tokens", 0, "total tokens")
	fs.Int64Var(&totalTokensAlias, "TotalTokens", 0, "total tokens")
	fs.StringVar(&ignoredModelSource, "model-source", "", "model source")
	fs.StringVar(&ignoredModelSourceAlias, "ModelSource", "", "model source")
	fs.BoolVar(&ignoredVerified, "verified", false, "verified")
	fs.BoolVar(&ignoredVerifiedAlias, "Verified", false, "verified")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if roleAlias != "" {
		role = roleAlias
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
	if permissionAlias != "" {
		permission = permissionAlias
	}
	if actionAlias != "" {
		action = actionAlias
	}
	if flagWasSet(fs, "ExitCode") {
		exitCode = exitCodeAlias
	}
	if startedAtAlias != "" {
		startedAt = startedAtAlias
	}
	if endedAtAlias != "" {
		endedAt = endedAtAlias
	}

	durationSet := flagWasSet(fs, "duration-ms")
	if flagWasSet(fs, "DurationMs") {
		durationMS = durationMSAlias
		durationSet = true
	}
	hasTimestamps := strings.TrimSpace(startedAt) != "" || strings.TrimSpace(endedAt) != ""
	if durationSet && hasTimestamps {
		fmt.Fprintln(stderr, "attest: choose either --started-at/--ended-at or --duration-ms")
		return 2
	}

	payload := map[string]any{
		"role":         role,
		"provider":     provider,
		"model":        model,
		"model_source": string(attestation.ModelSourceSelfReported),
		"effort":       effort,
		"permission":   permission,
		"action":       action,
		"exit_code":    exitCode,
		"verified":     false,
	}

	if durationSet {
		ended := deps.Now().UTC()
		started := ended.Add(-time.Duration(durationMS) * time.Millisecond)
		payload["started_at"] = started.Format(time.RFC3339Nano)
		payload["ended_at"] = ended.Format(time.RFC3339Nano)
		payload["duration_ms"] = durationMS
	} else {
		if strings.TrimSpace(startedAt) != "" {
			payload["started_at"] = startedAt
		}
		if strings.TrimSpace(endedAt) != "" {
			payload["ended_at"] = endedAt
		}
		if strings.TrimSpace(startedAt) != "" && strings.TrimSpace(endedAt) != "" {
			started, startErr := time.Parse(time.RFC3339Nano, startedAt)
			ended, endErr := time.Parse(time.RFC3339Nano, endedAt)
			if startErr == nil && endErr == nil {
				payload["duration_ms"] = ended.Sub(started).Milliseconds()
			}
		}
	}

	usage := map[string]int64{}
	if flagWasSet(fs, "input-tokens") {
		usage["input_tokens"] = inputTokens
	}
	if flagWasSet(fs, "InputTokens") {
		usage["input_tokens"] = inputTokensAlias
	}
	if flagWasSet(fs, "output-tokens") {
		usage["output_tokens"] = outputTokens
	}
	if flagWasSet(fs, "OutputTokens") {
		usage["output_tokens"] = outputTokensAlias
	}
	if flagWasSet(fs, "total-tokens") {
		usage["total_tokens"] = totalTokens
	}
	if flagWasSet(fs, "TotalTokens") {
		usage["total_tokens"] = totalTokensAlias
	}
	if len(usage) > 0 {
		payload["usage"] = usage
	}

	data, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintf(stderr, "attest: %v\n", err)
		return 1
	}
	var record attestation.AttestationRecord
	if err := json.Unmarshal(data, &record); err != nil {
		fmt.Fprintf(stderr, "attest: %v\n", err)
		return 1
	}
	if err := record.Validate(); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	canonical, err := record.CanonicalJSON()
	if err != nil {
		fmt.Fprintf(stderr, "attest: %v\n", err)
		return 1
	}
	if _, err := stdout.Write(append(canonical, '\n')); err != nil {
		fmt.Fprintf(stderr, "attest: write output: %v\n", err)
		return 1
	}
	if _, err := fmt.Fprintln(stdout, record.Header()); err != nil {
		fmt.Fprintf(stderr, "attest: write output: %v\n", err)
		return 1
	}
	return 0
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	wasSet := false
	fs.Visit(func(flag *flag.Flag) {
		if flag.Name == name {
			wasSet = true
		}
	})
	return wasSet
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
		Budget:          cfg.Guardrails.Budget,
		CircuitBreaker:  cfg.Guardrails.CircuitBreaker,
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

	resolvedRepo, err := resolveRepo(opts.RepoPath)
	if err != nil {
		fmt.Fprintf(stderr, "recover: %v\n", err)
		return 2
	}
	opts.RepoPath = resolvedRepo

	cfg := config.Default()
	loaded, err := config.Load(filepath.Join(resolvedRepo, ".delivery.yml"))
	if err == nil {
		cfg = loaded
	} else if !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stderr, "recover: %v\n", err)
		return 1
	}
	opts.Budget = cfg.Guardrails.Budget
	opts.CircuitBreaker = cfg.Guardrails.CircuitBreaker

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

func runLoopreview(args []string, stdout, stderr io.Writer, deps Deps) int {
	if deps.Loopreview == nil {
		deps.Loopreview = DefaultDeps().Loopreview
	}

	fs := flag.NewFlagSet("loopreview", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var opts loopreview.Options
	var repoAlias string
	var prNumberAlias int
	var providerAlias string
	var baseBranchAlias string
	var timeoutAlias time.Duration

	fs.StringVar(&opts.RepoPath, "repo", "", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")
	fs.IntVar(&opts.PRNumber, "pr-number", 0, "pull request number")
	fs.IntVar(&prNumberAlias, "PrNumber", 0, "pull request number")
	fs.StringVar(&opts.Provider, "provider", "", "provider")
	fs.StringVar(&providerAlias, "Provider", "", "provider")
	fs.StringVar(&opts.BaseBranch, "base-branch", "main", "base branch")
	fs.StringVar(&baseBranchAlias, "BaseBranch", "", "base branch")
	fs.DurationVar(&opts.Timeout, "timeout", loopreview.DefaultVerifierTimeout, "verifier timeout")
	fs.DurationVar(&timeoutAlias, "Timeout", 0, "verifier timeout")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if opts.RepoPath == "" {
		opts.RepoPath = repoAlias
	}
	if prNumberAlias != 0 {
		opts.PRNumber = prNumberAlias
	}
	if providerAlias != "" {
		opts.Provider = providerAlias
	}
	if baseBranchAlias != "" {
		opts.BaseBranch = baseBranchAlias
	}
	if timeoutAlias != 0 {
		opts.Timeout = timeoutAlias
	}

	if strings.TrimSpace(opts.RepoPath) == "" {
		fmt.Fprintln(stderr, "loopreview: --repo is required")
		return 2
	}
	if opts.PRNumber <= 0 {
		fmt.Fprintln(stderr, "loopreview: --pr-number is required")
		return 2
	}
	if strings.TrimSpace(opts.Provider) == "" {
		fmt.Fprintln(stderr, "loopreview: --provider is required")
		return 2
	}
	if opts.Timeout <= 0 {
		fmt.Fprintln(stderr, "loopreview: --timeout must be positive")
		return 2
	}

	resolvedRepo, err := resolveRepo(opts.RepoPath)
	if err != nil {
		fmt.Fprintf(stderr, "loopreview: %v\n", err)
		return 2
	}
	opts.RepoPath = resolvedRepo
	opts.Stderr = stderr

	cfg := config.Default()
	loaded, err := config.Load(filepath.Join(resolvedRepo, ".delivery.yml"))
	if err == nil {
		cfg = loaded
	} else if !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stderr, "loopreview: %v\n", err)
		return 1
	}
	if warning := config.ReviewerNotWorkerWarning(config.Adapters{
		Worker:   cfg.Adapters.Worker,
		Verifier: opts.Provider,
	}); warning != "" {
		fmt.Fprintf(stderr, "[loopcoder] warning: %s\n", warning)
	}

	result, err := deps.Loopreview(context.Background(), opts)
	if err != nil {
		fmt.Fprintf(stderr, "loopreview: %v\n", err)
		return 1
	}
	if err := loopreview.Render(stdout, result); err != nil {
		fmt.Fprintf(stderr, "loopreview: write output: %v\n", err)
		return 1
	}
	return result.ExitCode
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
