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
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/attestation"
	compiler "github.com/jasonhnd/loopcoder/internal/compile"
	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/doctor"
	"github.com/jasonhnd/loopcoder/internal/loopreview"
	"github.com/jasonhnd/loopcoder/internal/orchestration"
	"github.com/jasonhnd/loopcoder/internal/perception"
	"github.com/jasonhnd/loopcoder/internal/process"
	"github.com/jasonhnd/loopcoder/internal/recovery"
	"github.com/jasonhnd/loopcoder/internal/relay"
	"github.com/jasonhnd/loopcoder/internal/report"
	"github.com/jasonhnd/loopcoder/internal/runstatus"
	"github.com/jasonhnd/loopcoder/internal/scaffold"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/statebranch"
	"github.com/jasonhnd/loopcoder/internal/upgrade"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
	"github.com/jasonhnd/loopcoder/internal/verify"
	"github.com/jasonhnd/loopcoder/internal/worker"
)

type Command struct {
	Name    string
	Summary string
}

type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

type Deps struct {
	NewGitHubReader  func(repoPath string) orchestration.GitHubReader
	NewIssueWriter   func(repoPath string) compiler.IssueWriter
	NewPreProdWriter func(repoPath string) orchestration.PreProdWriter
	NewPromoteWriter func(repoPath string) orchestration.PromotionWriter
	ProcessAlive     func(pid int) bool
	Now              func() time.Time
	IsTerminal       func(w io.Writer) bool
	Stdin            io.Reader
	BuildInfo        BuildInfo
	ComputeReadySet  func(ctx context.Context, opts orchestration.Options) (report.ReadySetReport, error)
	Tick             func(ctx context.Context, opts orchestration.TickOptions) (orchestration.TickReport, error)
	Discover         func(ctx context.Context, opts perception.Options) (perception.Report, error)
	Compile          func(ctx context.Context, opts compiler.Options) (compiler.Report, error)
	Dispatch         func(ctx context.Context, opts worker.Options) (worker.Result, error)
	Loopreview       func(ctx context.Context, opts loopreview.Options) (loopreview.Result, error)
	Promote          func(ctx context.Context, opts orchestration.PromoteOptions) (orchestration.PromoteReport, error)
	Recover          func(ctx context.Context, opts recovery.Options) (recovery.Result, error)
	Verify           func(ctx context.Context, opts verify.Options) verify.Result
	Doctor           func(ctx context.Context, opts doctor.Options) doctor.Report
	Init             func(ctx context.Context, opts scaffold.Options) (scaffold.Result, error)
	Upgrade          func(ctx context.Context, opts upgrade.Options) (upgrade.Result, error)
	SkillInstall     func(ctx context.Context, opts SkillInstallOptions) (SkillInstallResult, error)
	StatePush        func(ctx context.Context, opts statebranch.PushOptions) (statebranch.PushResult, error)
	StatePull        func(ctx context.Context, opts statebranch.PullOptions) (statebranch.PullResult, error)
	LeaseAcquire     func(ctx context.Context, opts statebranch.LeaseOptions) (statebranch.LeaseResult, error)
	LeaseRelease     func(ctx context.Context, opts statebranch.LeaseOptions) (statebranch.LeaseResult, error)
}

var commands = []Command{
	{Name: "attest", Summary: "emit conductor self-attestation"},
	{Name: "version", Summary: "print version and build information"},
	{Name: "doctor", Summary: "run read-only preflight checks"},
	{Name: "init", Summary: "scaffold loopcoder files in the current repository"},
	{Name: "discover", Summary: "discover CI failures and file GitHub issues"},
	{Name: "compile", Summary: "compile ROADMAP.md into GitHub issues"},
	{Name: "tick", Summary: "run one unattended delivery pass"},
	{Name: "promote", Summary: "promote pre-prod to main"},
	{Name: "upgrade", Summary: "self-update from GitHub Releases"},
	{Name: "skill", Summary: "install bundled playbook skill files"},
	{Name: "dispatch", Summary: "dispatch one issue worker"},
	{Name: "ready-set", Summary: "classify ready and blocked work"},
	{Name: "status", Summary: "render local delivery run status"},
	{Name: "resume", Summary: "reconcile a local run"},
	{Name: "state", Summary: "publish or pull durable run state"},
	{Name: "lease", Summary: "manage the conductor lease"},
	{Name: "recover", Summary: "recover or retry a worker attempt"},
	{Name: "loopreview", Summary: "run an independent read-only PR verifier"},
	{Name: "verify-local", Summary: "run local verification gates"},
	{Name: "dispatch-wave", Summary: "dispatch one ready issue wave"},
	{Name: "hook", Summary: "run an embedded loopcoder conductor hook (used by Claude Code hook settings)"},
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

func RunWithBuildInfo(args []string, stdout, stderr io.Writer, build BuildInfo) int {
	deps := DefaultDeps()
	deps.BuildInfo = build
	return RunWithDeps(args, stdout, stderr, deps)
}

func DefaultDeps() Deps {
	return Deps{
		NewGitHubReader: func(repoPath string) orchestration.GitHubReader {
			return gh.New(repoPath)
		},
		NewIssueWriter: func(repoPath string) compiler.IssueWriter {
			return gh.New(repoPath)
		},
		NewPreProdWriter: func(repoPath string) orchestration.PreProdWriter {
			return gh.New(repoPath)
		},
		NewPromoteWriter: func(repoPath string) orchestration.PromotionWriter {
			return gh.New(repoPath)
		},
		ProcessAlive: process.Alive,
		Now:          time.Now,
		IsTerminal:   isTerminalWriter,
		Stdin:        os.Stdin,
		ComputeReadySet: func(ctx context.Context, opts orchestration.Options) (report.ReadySetReport, error) {
			return orchestration.ComputeReadySet(ctx, opts)
		},
		Tick: func(ctx context.Context, opts orchestration.TickOptions) (orchestration.TickReport, error) {
			return orchestration.Tick(ctx, opts)
		},
		Discover: func(ctx context.Context, opts perception.Options) (perception.Report, error) {
			return perception.Run(ctx, opts)
		},
		Compile: func(ctx context.Context, opts compiler.Options) (compiler.Report, error) {
			return compiler.Run(ctx, opts, compiler.DefaultDeps())
		},
		Dispatch: func(ctx context.Context, opts worker.Options) (worker.Result, error) {
			return worker.Dispatch(ctx, opts, worker.DefaultDeps())
		},
		Loopreview: func(ctx context.Context, opts loopreview.Options) (loopreview.Result, error) {
			return loopreview.Run(ctx, opts, loopreview.DefaultDeps())
		},
		Promote: func(ctx context.Context, opts orchestration.PromoteOptions) (orchestration.PromoteReport, error) {
			return orchestration.Promote(ctx, opts)
		},
		Verify: func(ctx context.Context, opts verify.Options) verify.Result {
			return verify.Run(ctx, opts, verify.DefaultDeps())
		},
		Doctor: func(ctx context.Context, opts doctor.Options) doctor.Report {
			return doctor.Run(ctx, opts, doctor.DefaultDeps())
		},
		Init: func(ctx context.Context, opts scaffold.Options) (scaffold.Result, error) {
			return scaffold.Init(ctx, opts, scaffold.DefaultDeps())
		},
		Upgrade: func(ctx context.Context, opts upgrade.Options) (upgrade.Result, error) {
			return upgrade.Run(ctx, opts, upgrade.DefaultDeps())
		},
		SkillInstall: func(ctx context.Context, opts SkillInstallOptions) (SkillInstallResult, error) {
			return InstallSkill(ctx, opts, DefaultSkillInstallDeps())
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
	if isRootVersion(args[0]) {
		if len(args) > 1 {
			fmt.Fprintf(stderr, "version: unexpected argument %q\n", args[1])
			return 2
		}
		printVersion(stdout, deps.BuildInfo)
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
	if command.Name == "status" {
		return runStatus(args[1:], stdout, stderr, deps)
	}
	if command.Name == "attest" {
		return runAttest(args[1:], stdout, stderr, deps)
	}
	if command.Name == "version" {
		return runVersion(args[1:], stdout, stderr, deps)
	}
	if command.Name == "doctor" {
		return runDoctor(args[1:], stdout, stderr, deps)
	}
	if command.Name == "init" {
		return runInit(args[1:], stdout, stderr, deps)
	}
	if command.Name == "compile" {
		return runCompile(args[1:], stdout, stderr, deps)
	}
	if command.Name == "discover" {
		return runDiscover(args[1:], stdout, stderr, deps)
	}
	if command.Name == "tick" {
		return runTick(args[1:], stdout, stderr, deps)
	}
	if command.Name == "promote" {
		return runPromote(args[1:], stdout, stderr, deps)
	}
	if command.Name == "upgrade" {
		return runUpgrade(args[1:], stdout, stderr, deps)
	}
	if command.Name == "skill" {
		return runSkill(args[1:], stdout, stderr, deps)
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
	if command.Name == "hook" {
		return runHook(args[1:], stdout, stderr, deps)
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
	fmt.Fprintln(w, "  loopcoder --version")
	fmt.Fprintln(w, "  loopcoder -v")
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
	if command.Name == "skill" {
		printSkillHelp(w)
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
		fmt.Fprintln(w, "  --model string              optional worker model override for this run")
		fmt.Fprintln(w, "  --effort string             optional worker reasoning effort override for this run")
		fmt.Fprintln(w, "  --keep-worktree             preserve the scratch worktree and logs")
		fmt.Fprintln(w, "  --pretty                    force emoji pretty attestation on stderr (LOOPCODER_PRETTY; default is stderr, plain on non-TTY)")
		fmt.Fprintln(w, "  --no-pretty                 suppress pretty attestation on stderr (LOOPCODER_NO_PRETTY)")
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
		fmt.Fprintln(w, "  --pretty                 render human-readable attestation instead of durable output")
	}
	if command.Name == "doctor" {
		fmt.Fprintln(w, "  --repo string   repository path (default \".\")")
	}
	if command.Name == "init" {
		fmt.Fprintln(w, "  --force                     overwrite existing .delivery.yml and ROADMAP.md")
		fmt.Fprintln(w, "  --worker-model string       optional first-run worker model to persist")
		fmt.Fprintln(w, "  --worker-effort string      optional first-run worker reasoning effort to persist")
		fmt.Fprintln(w, "  --verifier-model string     optional first-run verifier model to persist")
		fmt.Fprintln(w, "  --verifier-effort string    optional first-run verifier reasoning effort to persist")
	}
	if command.Name == "compile" {
		fmt.Fprintln(w, "  --repo string   repository path (required)")
	}
	if command.Name == "discover" {
		fmt.Fprintln(w, "  --repo string   repository path (required)")
	}
	if command.Name == "tick" {
		fmt.Fprintln(w, "  --repo string                    repository path (required)")
		fmt.Fprintln(w, "  --base-branch string             base branch for ready, dispatch, and review (default worker.base_branch or \"main\")")
		fmt.Fprintln(w, "  --pre-prod-branch string         pre-prod branch for clean unattended integrations (default environment.pre_prod_branch or \"pre-prod\")")
		fmt.Fprintln(w, "  --run-id string                  shared run id for this pass (default generated once)")
		fmt.Fprintln(w, "  --worker-provider string         optional worker provider override for this pass")
		fmt.Fprintln(w, "  --verifier-provider string       optional verifier provider override for this pass")
		fmt.Fprintln(w, "  --worker-model string            optional worker model override for this pass")
		fmt.Fprintln(w, "  --worker-effort string           optional worker reasoning effort override for this pass")
		fmt.Fprintln(w, "  --verifier-model string          optional verifier model override for this pass")
		fmt.Fprintln(w, "  --verifier-effort string         optional verifier reasoning effort override for this pass")
		fmt.Fprintln(w, "  --verifier-timeout duration      verifier timeout (default 10m0s)")
		fmt.Fprintln(w, "  --throttle-limit int             maximum concurrent dispatches (default 4)")
		fmt.Fprintln(w, "  --pretty                         force emoji pretty attestations on stderr (LOOPCODER_PRETTY; default is stderr, plain on non-TTY)")
		fmt.Fprintln(w, "  --no-pretty                      suppress pretty attestations on stderr (LOOPCODER_NO_PRETTY)")
	}
	if command.Name == "promote" {
		fmt.Fprintln(w, "  --repo string              repository path (required)")
		fmt.Fprintln(w, "  --pre-prod-branch string   pre-prod branch to promote (default environment.pre_prod_branch or \"pre-prod\")")
		fmt.Fprintln(w, "  --run-id string            run id for the promote ledger (default generated)")
		fmt.Fprintln(w, "  --kick-back string         item to revert out of pre-prod before promoting; repeatable")
	}
	if command.Name == "upgrade" {
		fmt.Fprintln(w, "  --version string   release version to install (default latest stable)")
	}
	if command.Name == "ready-set" {
		fmt.Fprintln(w, "  --repo string          repository path (required)")
		fmt.Fprintln(w, "  --base-branch string   base branch for dependency reasoning (default \"main\")")
		fmt.Fprintln(w, "  --run-id string        local run id to inspect (default latest local run when present)")
		fmt.Fprintln(w, "  --format string        output format: text, json, or both (default \"text\")")
		fmt.Fprintln(w, "  --include-closed       include closed issues as diagnostic non-ready entries")
	}
	if command.Name == "status" {
		fmt.Fprintln(w, "  --repo string   repository path (default \".\")")
		fmt.Fprintln(w, "  --run string    local run id to inspect (default latest modified local run)")
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
		fmt.Fprintln(w, "  --model string                  optional worker model override for this run")
		fmt.Fprintln(w, "  --effort string                 optional worker reasoning effort override for this run")
	}
	if command.Name == "loopreview" {
		fmt.Fprintln(w, "  --repo string          repository path (required)")
		fmt.Fprintln(w, "  --pr-number int        pull request number to review (required)")
		fmt.Fprintln(w, "  --provider string      verifier provider (required)")
		fmt.Fprintln(w, "  --base-branch string   base branch for merged spec lookup (default \"main\")")
		fmt.Fprintln(w, "  --model string         optional verifier model override for this run")
		fmt.Fprintln(w, "  --effort string        optional verifier reasoning effort override for this run")
		fmt.Fprintln(w, "  --timeout duration     verifier timeout (default 10m0s)")
		fmt.Fprintln(w, "  --pretty               force emoji pretty attestation on stderr (LOOPCODER_PRETTY; default is stderr, plain on non-TTY)")
		fmt.Fprintln(w, "  --no-pretty            suppress pretty attestation on stderr (LOOPCODER_NO_PRETTY)")
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
		fmt.Fprintln(w, "  --model string             optional worker model override for this wave")
		fmt.Fprintln(w, "  --effort string            optional worker reasoning effort override for this wave")
		fmt.Fprintln(w, "  --throttle-limit int       maximum concurrent dispatches (default 4)")
		fmt.Fprintln(w, "  --pretty                   force emoji pretty attestations on stderr (LOOPCODER_PRETTY; default is stderr, plain on non-TTY)")
		fmt.Fprintln(w, "  --no-pretty                suppress pretty attestations on stderr (LOOPCODER_NO_PRETTY)")
	}
	if command.Name == "hook" {
		fmt.Fprintln(w, "  <name>    hook to run: conductor-attest or conductor-relay-guard")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Reads the hook payload from stdin and runs the named embedded conductor hook.")
		fmt.Fprintln(w, "Wired into Claude Code settings; unknown or missing names fail open (exit 0).")
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

func isRootVersion(arg string) bool {
	return arg == "-v" || arg == "--version"
}

func sentenceCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:] + "."
}

func runVersion(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(stderr)

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "version: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	printVersion(stdout, deps.BuildInfo)
	return 0
}

func printVersion(w io.Writer, build BuildInfo) {
	build = normalizeBuildInfo(build)
	fmt.Fprintf(
		w,
		"loopcoder version=%s commit=%s date=%s go=%s platform=%s/%s\n",
		build.Version,
		build.Commit,
		build.Date,
		runtime.Version(),
		runtime.GOOS,
		runtime.GOARCH,
	)
}

func runDoctor(args []string, stdout, stderr io.Writer, deps Deps) int {
	if deps.Doctor == nil {
		deps.Doctor = DefaultDeps().Doctor
	}

	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var repoPath string
	var repoAlias string
	fs.StringVar(&repoPath, "repo", ".", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if repoAlias != "" {
		repoPath = repoAlias
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "doctor: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	resolvedRepo, err := resolveRepo(repoPath)
	if err != nil {
		fmt.Fprintf(stderr, "doctor: %v\n", err)
		return 2
	}

	report := deps.Doctor(context.Background(), doctor.Options{
		RepoPath: resolvedRepo,
		BuildInfo: doctor.BuildInfo{
			Version: deps.BuildInfo.Version,
			Commit:  deps.BuildInfo.Commit,
			Date:    deps.BuildInfo.Date,
		},
	})
	if err := doctor.Render(stdout, report); err != nil {
		fmt.Fprintf(stderr, "doctor: write output: %v\n", err)
		return 1
	}
	return report.ExitCode()
}

func runInit(args []string, stdout, stderr io.Writer, deps Deps) int {
	if deps.Init == nil {
		deps.Init = DefaultDeps().Init
	}

	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var opts scaffold.Options
	var forceAlias bool
	var workerModelAlias string
	var workerEffortAlias string
	var verifierModelAlias string
	var verifierEffortAlias string

	fs.BoolVar(&opts.Force, "force", false, "overwrite existing files")
	fs.BoolVar(&forceAlias, "Force", false, "overwrite existing files")
	fs.StringVar(&opts.WorkerModel, "worker-model", "", "worker model")
	fs.StringVar(&workerModelAlias, "WorkerModel", "", "worker model")
	fs.StringVar(&opts.WorkerEffort, "worker-effort", "", "worker reasoning effort")
	fs.StringVar(&workerEffortAlias, "WorkerEffort", "", "worker reasoning effort")
	fs.StringVar(&opts.VerifierModel, "verifier-model", "", "verifier model")
	fs.StringVar(&verifierModelAlias, "VerifierModel", "", "verifier model")
	fs.StringVar(&opts.VerifierEffort, "verifier-effort", "", "verifier reasoning effort")
	fs.StringVar(&verifierEffortAlias, "VerifierEffort", "", "verifier reasoning effort")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if forceAlias {
		opts.Force = true
	}
	if workerModelAlias != "" {
		opts.WorkerModel = workerModelAlias
	}
	if workerEffortAlias != "" {
		opts.WorkerEffort = workerEffortAlias
	}
	if verifierModelAlias != "" {
		opts.VerifierModel = verifierModelAlias
	}
	if verifierEffortAlias != "" {
		opts.VerifierEffort = verifierEffortAlias
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "init: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	resolvedRepo, err := resolveRepo(".")
	if err != nil {
		fmt.Fprintf(stderr, "init: %v\n", err)
		return 2
	}
	opts.RepoPath = resolvedRepo

	result, err := deps.Init(context.Background(), opts)
	if err != nil {
		fmt.Fprintf(stderr, "init: %v\n", err)
		return 1
	}
	renderInitResult(stdout, stderr, result)
	return 0
}

func runCompile(args []string, stdout, stderr io.Writer, deps Deps) int {
	defaults := DefaultDeps()
	if deps.NewIssueWriter == nil {
		deps.NewIssueWriter = defaults.NewIssueWriter
	}
	if deps.Compile == nil {
		deps.Compile = defaults.Compile
	}
	if deps.Now == nil {
		deps.Now = defaults.Now
	}

	fs := flag.NewFlagSet("compile", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var repoPath string
	var repoAlias string
	fs.StringVar(&repoPath, "repo", "", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if repoPath == "" {
		repoPath = repoAlias
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "compile: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if strings.TrimSpace(repoPath) == "" {
		fmt.Fprintln(stderr, "compile: --repo is required")
		return 2
	}

	resolvedRepo, err := resolveRepo(repoPath)
	if err != nil {
		fmt.Fprintf(stderr, "compile: %v\n", err)
		return 2
	}
	report, err := deps.Compile(context.Background(), compiler.Options{
		RepoPath: resolvedRepo,
		Writer:   deps.NewIssueWriter(resolvedRepo),
		Now:      deps.Now(),
	})
	if err != nil {
		fmt.Fprintf(stderr, "compile: %v\n", err)
		return 1
	}
	data, err := compiler.MarshalReportJSON(report)
	if err != nil {
		fmt.Fprintf(stderr, "compile: %v\n", err)
		return 1
	}
	if _, err := stdout.Write(data); err != nil {
		fmt.Fprintf(stderr, "compile: write output: %v\n", err)
		return 1
	}
	if _, err := stderr.Write([]byte(compiler.RenderText(report))); err != nil {
		fmt.Fprintf(stderr, "compile: write summary: %v\n", err)
		return 1
	}
	return 0
}

func runDiscover(args []string, stdout, stderr io.Writer, deps Deps) int {
	defaults := DefaultDeps()
	if deps.NewGitHubReader == nil {
		deps.NewGitHubReader = defaults.NewGitHubReader
	}
	if deps.NewIssueWriter == nil {
		deps.NewIssueWriter = defaults.NewIssueWriter
	}
	if deps.Discover == nil {
		deps.Discover = defaults.Discover
	}
	if deps.Now == nil {
		deps.Now = defaults.Now
	}

	fs := flag.NewFlagSet("discover", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var repoPath string
	var repoAlias string
	fs.StringVar(&repoPath, "repo", "", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if repoPath == "" {
		repoPath = repoAlias
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "discover: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if strings.TrimSpace(repoPath) == "" {
		fmt.Fprintln(stderr, "discover: --repo is required")
		return 2
	}

	resolvedRepo, err := resolveRepo(repoPath)
	if err != nil {
		fmt.Fprintf(stderr, "discover: %v\n", err)
		return 2
	}
	report, err := deps.Discover(context.Background(), perception.Options{
		RepoPath: resolvedRepo,
		CI:       deps.NewGitHubReader(resolvedRepo),
		Writer:   deps.NewIssueWriter(resolvedRepo),
		Now:      deps.Now(),
	})
	if err != nil {
		fmt.Fprintf(stderr, "discover: %v\n", err)
		return 1
	}
	data, err := perception.MarshalReportJSON(report)
	if err != nil {
		fmt.Fprintf(stderr, "discover: %v\n", err)
		return 1
	}
	if _, err := stdout.Write(data); err != nil {
		fmt.Fprintf(stderr, "discover: write output: %v\n", err)
		return 1
	}
	if _, err := stderr.Write([]byte(perception.RenderText(report))); err != nil {
		fmt.Fprintf(stderr, "discover: write summary: %v\n", err)
		return 1
	}
	return 0
}

func runTick(args []string, stdout, stderr io.Writer, deps Deps) int {
	defaults := DefaultDeps()
	if deps.NewGitHubReader == nil {
		deps.NewGitHubReader = defaults.NewGitHubReader
	}
	if deps.NewIssueWriter == nil {
		deps.NewIssueWriter = defaults.NewIssueWriter
	}
	if deps.NewPreProdWriter == nil {
		deps.NewPreProdWriter = defaults.NewPreProdWriter
	}
	if deps.ProcessAlive == nil {
		deps.ProcessAlive = defaults.ProcessAlive
	}
	if deps.Now == nil {
		deps.Now = defaults.Now
	}
	if deps.IsTerminal == nil {
		deps.IsTerminal = defaults.IsTerminal
	}
	if deps.Tick == nil {
		deps.Tick = defaults.Tick
	}
	if deps.Compile == nil {
		deps.Compile = defaults.Compile
	}
	if deps.ComputeReadySet == nil {
		deps.ComputeReadySet = defaults.ComputeReadySet
	}
	if deps.Dispatch == nil {
		deps.Dispatch = defaults.Dispatch
	}
	if deps.Loopreview == nil {
		deps.Loopreview = defaults.Loopreview
	}
	if deps.StatePush == nil {
		deps.StatePush = defaults.StatePush
	}

	fs := flag.NewFlagSet("tick", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var repoPath string
	var repoAlias string
	var baseBranch string
	var baseBranchAlias string
	var preProdBranch string
	var preProdBranchAlias string
	var runID string
	var runIDAlias string
	var workerProvider string
	var workerProviderAlias string
	var verifierProvider string
	var verifierProviderAlias string
	var workerModel string
	var workerModelAlias string
	var workerEffort string
	var workerEffortAlias string
	var verifierModel string
	var verifierModelAlias string
	var verifierEffort string
	var verifierEffortAlias string
	var verifierTimeout time.Duration
	var verifierTimeoutAlias time.Duration
	var throttleLimit int
	var throttleLimitAlias int
	var pretty bool
	var prettyAlias bool
	var noPretty bool
	var noPrettyAlias bool

	fs.StringVar(&repoPath, "repo", "", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")
	fs.StringVar(&baseBranch, "base-branch", "", "base branch")
	fs.StringVar(&baseBranchAlias, "BaseBranch", "", "base branch")
	fs.StringVar(&preProdBranch, "pre-prod-branch", "", "pre-prod branch for clean unattended integrations")
	fs.StringVar(&preProdBranchAlias, "PreProdBranch", "", "pre-prod branch for clean unattended integrations")
	fs.StringVar(&runID, "run-id", "", "run id")
	fs.StringVar(&runIDAlias, "RunId", "", "run id")
	fs.StringVar(&workerProvider, "worker-provider", "", "worker provider")
	fs.StringVar(&workerProviderAlias, "WorkerProvider", "", "worker provider")
	fs.StringVar(&verifierProvider, "verifier-provider", "", "verifier provider")
	fs.StringVar(&verifierProviderAlias, "VerifierProvider", "", "verifier provider")
	fs.StringVar(&workerModel, "worker-model", "", "worker model")
	fs.StringVar(&workerModelAlias, "WorkerModel", "", "worker model")
	fs.StringVar(&workerEffort, "worker-effort", "", "worker effort")
	fs.StringVar(&workerEffortAlias, "WorkerEffort", "", "worker effort")
	fs.StringVar(&verifierModel, "verifier-model", "", "verifier model")
	fs.StringVar(&verifierModelAlias, "VerifierModel", "", "verifier model")
	fs.StringVar(&verifierEffort, "verifier-effort", "", "verifier effort")
	fs.StringVar(&verifierEffortAlias, "VerifierEffort", "", "verifier effort")
	fs.DurationVar(&verifierTimeout, "verifier-timeout", loopreview.DefaultVerifierTimeout, "verifier timeout")
	fs.DurationVar(&verifierTimeoutAlias, "VerifierTimeout", 0, "verifier timeout")
	fs.IntVar(&throttleLimit, "throttle-limit", 4, "throttle limit")
	fs.IntVar(&throttleLimitAlias, "ThrottleLimit", 0, "throttle limit")
	fs.BoolVar(&pretty, "pretty", false, "render human-readable attestations on stderr")
	fs.BoolVar(&prettyAlias, "Pretty", false, "render human-readable attestations on stderr")
	fs.BoolVar(&noPretty, "no-pretty", false, "suppress human-readable attestations on stderr")
	fs.BoolVar(&noPrettyAlias, "NoPretty", false, "suppress human-readable attestations on stderr")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	baseBranchFlagSet := flagWasSet(fs, "base-branch") || flagWasSet(fs, "BaseBranch")
	preProdBranchFlagSet := flagWasSet(fs, "pre-prod-branch") || flagWasSet(fs, "PreProdBranch")
	workerModelFlagSet := flagWasSet(fs, "worker-model") || flagWasSet(fs, "WorkerModel")
	workerEffortFlagSet := flagWasSet(fs, "worker-effort") || flagWasSet(fs, "WorkerEffort")
	verifierModelFlagSet := flagWasSet(fs, "verifier-model") || flagWasSet(fs, "VerifierModel")
	verifierEffortFlagSet := flagWasSet(fs, "verifier-effort") || flagWasSet(fs, "VerifierEffort")
	if repoPath == "" {
		repoPath = repoAlias
	}
	if baseBranchAlias != "" {
		baseBranch = baseBranchAlias
	}
	if preProdBranchAlias != "" {
		preProdBranch = preProdBranchAlias
	}
	if runIDAlias != "" {
		runID = runIDAlias
	}
	if workerProviderAlias != "" {
		workerProvider = workerProviderAlias
	}
	if verifierProviderAlias != "" {
		verifierProvider = verifierProviderAlias
	}
	if workerModelAlias != "" {
		workerModel = workerModelAlias
	}
	if workerEffortAlias != "" {
		workerEffort = workerEffortAlias
	}
	if verifierModelAlias != "" {
		verifierModel = verifierModelAlias
	}
	if verifierEffortAlias != "" {
		verifierEffort = verifierEffortAlias
	}
	if verifierTimeoutAlias != 0 {
		verifierTimeout = verifierTimeoutAlias
	}
	if throttleLimitAlias != 0 {
		throttleLimit = throttleLimitAlias
	}
	pretty = pretty || prettyAlias
	noPretty = noPretty || noPrettyAlias

	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "tick: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if strings.TrimSpace(repoPath) == "" {
		fmt.Fprintln(stderr, "tick: --repo is required")
		return 2
	}
	if throttleLimit <= 0 {
		fmt.Fprintln(stderr, "tick: --throttle-limit must be greater than zero")
		return 2
	}
	if verifierTimeout <= 0 {
		fmt.Fprintln(stderr, "tick: --verifier-timeout must be positive")
		return 2
	}

	resolvedRepo, err := resolveRepo(repoPath)
	if err != nil {
		fmt.Fprintf(stderr, "tick: %v\n", err)
		return 2
	}
	cfg, err := loadDeliveryConfig(resolvedRepo)
	if err != nil {
		fmt.Fprintf(stderr, "tick: %v\n", err)
		return 1
	}
	if !baseBranchFlagSet && strings.TrimSpace(baseBranch) == "" {
		baseBranch = strings.TrimSpace(cfg.Worker.BaseBranch)
	}
	if strings.TrimSpace(baseBranch) == "" {
		baseBranch = "main"
	}
	if !preProdBranchFlagSet && strings.TrimSpace(preProdBranch) == "" {
		preProdBranch = strings.TrimSpace(cfg.Environment.PreProdBranch)
	}
	if strings.TrimSpace(preProdBranch) == "" {
		preProdBranch = "pre-prod"
	}
	if strings.TrimSpace(workerProvider) == "" {
		workerProvider = strings.TrimSpace(cfg.Adapters.Worker)
	}
	if strings.TrimSpace(verifierProvider) == "" {
		verifierProvider = strings.TrimSpace(cfg.Adapters.Verifier)
	}
	workerModel, workerEffort = applyRoleModelEffort(
		workerModel,
		workerEffort,
		workerModelFlagSet,
		workerEffortFlagSet,
		cfg.Worker.Model,
		cfg.Worker.ReasoningEffort,
	)
	verifierModel, verifierEffort = applyRoleModelEffort(
		verifierModel,
		verifierEffort,
		verifierModelFlagSet,
		verifierEffortFlagSet,
		cfg.Verifier.Model,
		cfg.Verifier.ReasoningEffort,
	)
	if warning := config.ReviewerNotWorkerWarning(config.Adapters{
		Worker:   workerProvider,
		Verifier: verifierProvider,
	}); warning != "" {
		fmt.Fprintf(stderr, "[loopcoder] warning: %s\n", warning)
	}

	tickReport, err := deps.Tick(context.Background(), orchestration.TickOptions{
		Reader:             deps.NewGitHubReader(resolvedRepo),
		IssueWriter:        deps.NewIssueWriter(resolvedRepo),
		RepoPath:           resolvedRepo,
		BaseBranch:         baseBranch,
		PreProdBranch:      preProdBranch,
		RunID:              runID,
		WorkerProvider:     workerProvider,
		WorkerModel:        workerModel,
		WorkerEffort:       workerEffort,
		VerifierProvider:   verifierProvider,
		VerifierModel:      verifierModel,
		VerifierEffort:     verifierEffort,
		VerifierTimeout:    verifierTimeout,
		ThrottleLimit:      throttleLimit,
		RequiredChecks:     cfg.CI.Checks,
		ConfiguredEvidence: cfg.Evidence.Artifacts(),
		Thresholds:         cfg.Resilience.Worker,
		Budget:             cfg.Guardrails.Budget,
		CircuitBreaker:     cfg.Guardrails.CircuitBreaker,
		ProcessAlive:       deps.ProcessAlive,
		Clock:              deps.Now,
		Stderr:             stderr,
		Compile:            deps.Compile,
		ComputeReadySet:    deps.ComputeReadySet,
		Dispatch:           deps.Dispatch,
		Loopreview:         deps.Loopreview,
		PreProdWriter:      deps.NewPreProdWriter(resolvedRepo),
		StatePush:          deps.StatePush,
	})
	if err != nil {
		fmt.Fprintf(stderr, "tick: %v\n", err)
		return 1
	}
	data, err := orchestration.MarshalTickJSON(tickReport)
	if err != nil {
		fmt.Fprintf(stderr, "tick: %v\n", err)
		return 1
	}
	if _, err := stdout.Write(data); err != nil {
		fmt.Fprintf(stderr, "tick: write output: %v\n", err)
		return 1
	}
	if _, err := stderr.Write([]byte(orchestration.RenderTickText(tickReport))); err != nil {
		fmt.Fprintf(stderr, "tick: write summary: %v\n", err)
		return 1
	}
	if shouldRenderPretty(noPretty) {
		mode := prettyModeForTarget(stderr, deps, pretty)
		if err := renderTickPrettyAttestations(stderr, tickReport, mode); err != nil {
			fmt.Fprintf(stderr, "tick: write pretty attestation: %v\n", err)
			return 1
		}
	}
	return orchestration.TickExitCode(tickReport)
}

func runPromote(args []string, stdout, stderr io.Writer, deps Deps) int {
	defaults := DefaultDeps()
	if deps.NewPromoteWriter == nil {
		deps.NewPromoteWriter = defaults.NewPromoteWriter
	}
	if deps.Promote == nil {
		deps.Promote = defaults.Promote
	}
	if deps.StatePush == nil {
		deps.StatePush = defaults.StatePush
	}
	if deps.Now == nil {
		deps.Now = defaults.Now
	}

	fs := flag.NewFlagSet("promote", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var repoPath string
	var repoAlias string
	var preProdBranch string
	var preProdBranchAlias string
	var runID string
	var runIDAlias string
	var kickBack repeatStringFlag
	var kickBackAlias repeatStringFlag

	fs.StringVar(&repoPath, "repo", "", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")
	fs.StringVar(&preProdBranch, "pre-prod-branch", "", "pre-prod branch")
	fs.StringVar(&preProdBranchAlias, "PreProdBranch", "", "pre-prod branch")
	fs.StringVar(&runID, "run-id", "", "run id")
	fs.StringVar(&runIDAlias, "RunId", "", "run id")
	fs.Var(&kickBack, "kick-back", "kick-back item")
	fs.Var(&kickBackAlias, "KickBack", "kick-back item")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	preProdBranchFlagSet := flagWasSet(fs, "pre-prod-branch") || flagWasSet(fs, "PreProdBranch")
	if repoPath == "" {
		repoPath = repoAlias
	}
	if preProdBranchAlias != "" {
		preProdBranch = preProdBranchAlias
	}
	if runIDAlias != "" {
		runID = runIDAlias
	}
	kickBack = append(kickBack, kickBackAlias...)

	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "promote: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if strings.TrimSpace(repoPath) == "" {
		fmt.Fprintln(stderr, "promote: --repo is required")
		return 2
	}

	resolvedRepo, err := resolveRepo(repoPath)
	if err != nil {
		fmt.Fprintf(stderr, "promote: %v\n", err)
		return 2
	}
	cfg, err := loadDeliveryConfig(resolvedRepo)
	if err != nil {
		fmt.Fprintf(stderr, "promote: %v\n", err)
		return 1
	}
	if !preProdBranchFlagSet && strings.TrimSpace(preProdBranch) == "" {
		preProdBranch = strings.TrimSpace(cfg.Environment.PreProdBranch)
	}
	if strings.TrimSpace(preProdBranch) == "" {
		preProdBranch = "pre-prod"
	}

	report, err := deps.Promote(context.Background(), orchestration.PromoteOptions{
		Writer:        deps.NewPromoteWriter(resolvedRepo),
		RepoPath:      resolvedRepo,
		RunID:         runID,
		PreProdBranch: preProdBranch,
		Gate:          cfg.Adapters.Gate,
		KickBackItems: []string(kickBack),
		Clock:         deps.Now,
		StatePush:     deps.StatePush,
	})
	if err != nil {
		fmt.Fprintf(stderr, "promote: %v\n", err)
		return 1
	}
	data, err := orchestration.MarshalPromoteJSON(report)
	if err != nil {
		fmt.Fprintf(stderr, "promote: %v\n", err)
		return 1
	}
	if _, err := stdout.Write(data); err != nil {
		fmt.Fprintf(stderr, "promote: write output: %v\n", err)
		return 1
	}
	if _, err := stderr.Write([]byte(orchestration.RenderPromoteText(report))); err != nil {
		fmt.Fprintf(stderr, "promote: write summary: %v\n", err)
		return 1
	}
	return orchestration.PromoteExitCode(report)
}

func renderTickPrettyAttestations(w io.Writer, report orchestration.TickReport, mode attestation.PrettyMode) error {
	if report.DispatchWave != nil {
		for _, result := range report.DispatchWave.Results {
			if result.Attestation == nil {
				continue
			}
			if err := renderPrettyAttestation(w, *result.Attestation, mode); err != nil {
				return err
			}
		}
	}
	for _, review := range report.Reviews {
		if review.Attestation == nil {
			continue
		}
		if err := renderPrettyAttestation(w, *review.Attestation, mode); err != nil {
			return err
		}
	}
	return nil
}

func renderInitResult(stdout, stderr io.Writer, result scaffold.Result) {
	fmt.Fprintln(stdout, "loopcoder init complete")
	for _, file := range result.Files {
		switch file.Status {
		case scaffold.FileCreated:
			fmt.Fprintf(stdout, "  created %s\n", file.Path)
		case scaffold.FileOverwritten:
			fmt.Fprintf(stdout, "  overwritten %s\n", file.Path)
		case scaffold.FileExists:
			fmt.Fprintf(stdout, "  exists %s\n", file.Path)
		default:
			fmt.Fprintf(stdout, "  %s %s\n", file.Status, file.Path)
		}
	}
	for _, label := range result.Labels {
		switch label.Status {
		case scaffold.LabelCreated:
			fmt.Fprintf(stdout, "  created label %s\n", label.Name)
		case scaffold.LabelExists:
			fmt.Fprintf(stdout, "  exists label %s\n", label.Name)
		default:
			fmt.Fprintf(stdout, "  %s label %s\n", label.Status, label.Name)
		}
	}
	for _, warning := range result.Warnings {
		fmt.Fprintf(stderr, "[loopcoder] warning: %s\n", warning)
	}
}

func runUpgrade(args []string, stdout, stderr io.Writer, deps Deps) int {
	if deps.Upgrade == nil {
		deps.Upgrade = DefaultDeps().Upgrade
	}

	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var requestedVersion string
	var requestedVersionAlias string
	fs.StringVar(&requestedVersion, "version", "", "release version")
	fs.StringVar(&requestedVersionAlias, "Version", "", "release version")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if requestedVersionAlias != "" {
		requestedVersion = requestedVersionAlias
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "upgrade: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	build := normalizeBuildInfo(deps.BuildInfo)
	result, err := deps.Upgrade(context.Background(), upgrade.Options{
		RequestedVersion: requestedVersion,
		CurrentVersion:   build.Version,
	})
	if result.CurrentPath != "" || result.CurrentVersion != "" {
		renderUpgradeCurrent(stdout, result)
	}
	if err != nil {
		fmt.Fprintf(stderr, "upgrade: %v\n", err)
		return 1
	}
	renderUpgradeSuccess(stdout, result)
	if result.SkillRefresh.Warning != "" {
		fmt.Fprintf(stderr, "[loopcoder] warning: skill refresh failed after upgrade: %s; run: loopcoder skill install\n", result.SkillRefresh.Warning)
	}
	return 0
}

func renderUpgradeCurrent(w io.Writer, result upgrade.Result) {
	fmt.Fprintf(w, "Current selected binary: path=%s version=%s\n", result.CurrentPath, result.CurrentVersion)
}

func renderUpgradeSuccess(w io.Writer, result upgrade.Result) {
	fmt.Fprintf(w, "Resolved target version: %s\n", result.TargetVersion)
	if result.AlreadyLatest {
		fmt.Fprintln(w, "Already latest; no download needed.")
		return
	}
	fmt.Fprintf(w, "Platform asset: %s (%s)\n", result.AssetName, result.Platform)
	fmt.Fprintf(w, "Installed versioned binary: %s\n", result.VersionBinaryPath)
	if result.Deferred {
		fmt.Fprintf(w, "Stable selected binary: %s (deferred Windows swap from %s)\n", result.StableBinaryPath, result.PendingPath)
	} else {
		fmt.Fprintf(w, "Stable selected binary: %s\n", result.StableBinaryPath)
	}
	fmt.Fprintf(w, "Before: path=%s version=%s\n", result.CurrentPath, result.CurrentVersion)
	fmt.Fprintf(w, "After: path=%s version=%s\n", result.StableBinaryPath, result.TargetVersion)
	renderUpgradeSkillRefresh(w, result.SkillRefresh)
	fmt.Fprintln(w, "Run: loopcoder doctor")
}

func renderUpgradeSkillRefresh(w io.Writer, result upgrade.SkillRefreshResult) {
	if strings.TrimSpace(result.BinaryPath) == "" {
		return
	}
	fmt.Fprintf(w, "Skill refresh: %s skill install\n", result.BinaryPath)
	if result.Warning != "" {
		return
	}
	if result.Dir != "" {
		fmt.Fprintf(w, "  directory %s\n", result.Dir)
	}
	for _, file := range result.Files {
		fmt.Fprintf(w, "  %s %s\n", file.Status, file.Path)
	}
}

func normalizeBuildInfo(build BuildInfo) BuildInfo {
	if strings.TrimSpace(build.Version) == "" {
		build.Version = "dev"
	}
	if strings.TrimSpace(build.Commit) == "" {
		build.Commit = "unknown"
	}
	if strings.TrimSpace(build.Date) == "" {
		build.Date = "unknown"
	}
	return build
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
	if deps.Now == nil {
		deps.Now = DefaultDeps().Now
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
	var pretty bool
	var prettyAlias bool
	var noPretty bool
	var noPrettyAlias bool

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
	fs.BoolVar(&pretty, "pretty", false, "render human-readable attestation on stderr")
	fs.BoolVar(&prettyAlias, "Pretty", false, "render human-readable attestation on stderr")
	fs.BoolVar(&noPretty, "no-pretty", false, "suppress human-readable attestation on stderr")
	fs.BoolVar(&noPrettyAlias, "NoPretty", false, "suppress human-readable attestation on stderr")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	modelFlagSet := flagWasSet(fs, "model") || flagWasSet(fs, "Model")
	effortFlagSet := flagWasSet(fs, "effort") || flagWasSet(fs, "Effort")
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
	pretty = pretty || prettyAlias
	noPretty = noPretty || noPrettyAlias
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

	resolvedRepo, err := resolveRepo(opts.RepoPath)
	if err != nil {
		fmt.Fprintf(stderr, "dispatch: %v\n", err)
		return 2
	}
	opts.RepoPath = resolvedRepo

	cfg, err := loadDeliveryConfig(resolvedRepo)
	if err != nil {
		fmt.Fprintf(stderr, "dispatch: %v\n", err)
		return 1
	}
	opts.Model, opts.Effort = applyRoleModelEffort(
		opts.Model,
		opts.Effort,
		modelFlagSet,
		effortFlagSet,
		cfg.Worker.Model,
		cfg.Worker.ReasoningEffort,
	)

	result, err := deps.Dispatch(context.Background(), opts)
	if err != nil {
		fmt.Fprintf(stderr, "dispatch: %v\n", err)
		return 1
	}
	if err := renderDispatch(stdout, result); err != nil {
		fmt.Fprintf(stderr, "dispatch: %v\n", err)
		return 1
	}
	if result.Attestation != nil {
		mode := prettyModeForTarget(stderr, deps, pretty)
		if err := writeDispatchRelayLedger(opts, result, *result.Attestation, mode, deps.Now()); err != nil {
			fmt.Fprintf(stderr, "dispatch: write relay ledger: %v\n", err)
			return 1
		}
		if shouldRenderPretty(noPretty) {
			if err := renderPrettyAttestation(stderr, *result.Attestation, mode); err != nil {
				fmt.Fprintf(stderr, "dispatch: write pretty attestation: %v\n", err)
				return 1
			}
		}
	}
	return 0
}

func writeDispatchRelayLedger(opts worker.Options, result worker.Result, record attestation.AttestationRecord, mode attestation.PrettyMode, now time.Time) error {
	invocationID := relayInvocationIDFromAttemptPath(result.AttemptPath)
	if invocationID == "" {
		invocationID = fmt.Sprintf("dispatch-issue-%d-%d", result.Issue, now.UTC().UnixNano())
	}
	_, err := relay.Write(relay.Entry{
		RepoPath:     opts.RepoPath,
		RunID:        result.RunID,
		InvocationID: invocationID,
		Command:      "dispatch",
		Role:         attestation.RoleWorker,
		Issue:        result.Issue,
		PR:           result.PR,
		CreatedAt:    now,
		Header:       record.Header(),
		Pretty:       record.Pretty(attestation.PrettyOptions{Mode: mode}),
	})
	return err
}

func relayInvocationIDFromAttemptPath(attemptPath string) string {
	base := filepath.Base(strings.TrimSpace(attemptPath))
	if base == "." || base == string(filepath.Separator) {
		return ""
	}
	return strings.TrimSuffix(base, ".attempt.json")
}

func writeLoopreviewRelayLedger(opts loopreview.Options, record attestation.AttestationRecord, mode attestation.PrettyMode, now time.Time) error {
	_, err := relay.Write(relay.Entry{
		RepoPath:     opts.RepoPath,
		RunID:        fmt.Sprintf("loopreview-pr-%d", opts.PRNumber),
		InvocationID: fmt.Sprintf("loopreview-pr-%d-%d", opts.PRNumber, now.UTC().UnixNano()),
		Command:      "loopreview",
		Role:         attestation.RoleVerifier,
		PRNumber:     opts.PRNumber,
		CreatedAt:    now,
		Header:       record.Header(),
		Pretty:       record.Pretty(attestation.PrettyOptions{Mode: mode}),
	})
	return err
}

func renderDispatch(w io.Writer, result worker.Result) error {
	if result.Attestation == nil {
		return errors.New("worker attestation is missing")
	}
	if err := result.Attestation.Validate(); err != nil {
		return fmt.Errorf("validate worker attestation: %w", err)
	}
	canonical, err := result.Attestation.CanonicalJSON()
	if err != nil {
		return fmt.Errorf("render worker attestation JSON: %w", err)
	}
	data, err := worker.MarshalResult(result)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, result.Attestation.Header()); err != nil {
		return err
	}
	if _, err := w.Write(append(canonical, '\n')); err != nil {
		return err
	}
	if _, err := w.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
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
	var pretty bool
	var prettyAlias bool

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
	fs.BoolVar(&pretty, "pretty", false, "render human-readable attestation")
	fs.BoolVar(&prettyAlias, "Pretty", false, "render human-readable attestation")

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
	pretty = pretty || prettyAlias

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
	if pretty {
		if err := renderPrettyAttestation(stdout, record, prettyModeForTarget(stdout, deps, false)); err != nil {
			fmt.Fprintf(stderr, "attest: write output: %v\n", err)
			return 1
		}
		return 0
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

type repeatStringFlag []string

func (f *repeatStringFlag) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(*f, ",")
}

func (f *repeatStringFlag) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("value must not be empty")
	}
	*f = append(*f, value)
	return nil
}

func loadDeliveryConfig(repoPath string) (config.Config, error) {
	cfg := config.Default()
	loaded, err := config.Load(filepath.Join(repoPath, ".delivery.yml"))
	if err == nil {
		return loaded, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	return cfg, err
}

func applyRoleModelEffort(model, effort string, modelFlagSet, effortFlagSet bool, roleModel, roleEffort string) (string, string) {
	if !modelFlagSet && strings.TrimSpace(model) == "" {
		model = strings.TrimSpace(roleModel)
	}
	if !effortFlagSet && strings.TrimSpace(effort) == "" {
		effort = strings.TrimSpace(roleEffort)
	}
	return model, effort
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
	var pretty bool
	var prettyAlias bool
	var noPretty bool
	var noPrettyAlias bool

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
	fs.BoolVar(&pretty, "pretty", false, "render human-readable attestation on stderr")
	fs.BoolVar(&prettyAlias, "Pretty", false, "render human-readable attestation on stderr")
	fs.BoolVar(&noPretty, "no-pretty", false, "suppress human-readable attestation on stderr")
	fs.BoolVar(&noPrettyAlias, "NoPretty", false, "suppress human-readable attestation on stderr")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	modelFlagSet := flagWasSet(fs, "model") || flagWasSet(fs, "Model")
	effortFlagSet := flagWasSet(fs, "effort") || flagWasSet(fs, "Effort")
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
	pretty = pretty || prettyAlias
	noPretty = noPretty || noPrettyAlias

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

	cfg, err := loadDeliveryConfig(resolvedRepo)
	if err != nil {
		fmt.Fprintf(stderr, "dispatch-wave: %v\n", err)
		return 1
	}
	model, effort = applyRoleModelEffort(
		model,
		effort,
		modelFlagSet,
		effortFlagSet,
		cfg.Worker.Model,
		cfg.Worker.ReasoningEffort,
	)

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
	if shouldRenderPretty(noPretty) {
		mode := prettyModeForTarget(stderr, deps, pretty)
		for _, result := range waveReport.Results {
			if result.Attestation == nil {
				continue
			}
			if err := renderPrettyAttestation(stderr, *result.Attestation, mode); err != nil {
				fmt.Fprintf(stderr, "dispatch-wave: write pretty attestation: %v\n", err)
				return 1
			}
		}
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
	modelFlagSet := flagWasSet(fs, "model") || flagWasSet(fs, "Model")
	effortFlagSet := flagWasSet(fs, "effort") || flagWasSet(fs, "Effort")
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

	cfg, err := loadDeliveryConfig(resolvedRepo)
	if err != nil {
		fmt.Fprintf(stderr, "recover: %v\n", err)
		return 1
	}
	opts.Model, opts.Effort = applyRoleModelEffort(
		opts.Model,
		opts.Effort,
		modelFlagSet,
		effortFlagSet,
		cfg.Worker.Model,
		cfg.Worker.ReasoningEffort,
	)
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
	if deps.Now == nil {
		deps.Now = DefaultDeps().Now
	}

	fs := flag.NewFlagSet("loopreview", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var opts loopreview.Options
	var repoAlias string
	var prNumberAlias int
	var providerAlias string
	var modelAlias string
	var effortAlias string
	var baseBranchAlias string
	var timeoutAlias time.Duration
	var pretty bool
	var prettyAlias bool
	var noPretty bool
	var noPrettyAlias bool

	fs.StringVar(&opts.RepoPath, "repo", "", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")
	fs.IntVar(&opts.PRNumber, "pr-number", 0, "pull request number")
	fs.IntVar(&prNumberAlias, "PrNumber", 0, "pull request number")
	fs.StringVar(&opts.Provider, "provider", "", "provider")
	fs.StringVar(&providerAlias, "Provider", "", "provider")
	fs.StringVar(&opts.Model, "model", "", "model")
	fs.StringVar(&modelAlias, "Model", "", "model")
	fs.StringVar(&opts.Effort, "effort", "", "effort")
	fs.StringVar(&effortAlias, "Effort", "", "effort")
	fs.StringVar(&opts.BaseBranch, "base-branch", "main", "base branch")
	fs.StringVar(&baseBranchAlias, "BaseBranch", "", "base branch")
	fs.DurationVar(&opts.Timeout, "timeout", loopreview.DefaultVerifierTimeout, "verifier timeout")
	fs.DurationVar(&timeoutAlias, "Timeout", 0, "verifier timeout")
	fs.BoolVar(&pretty, "pretty", false, "render human-readable attestation on stderr")
	fs.BoolVar(&prettyAlias, "Pretty", false, "render human-readable attestation on stderr")
	fs.BoolVar(&noPretty, "no-pretty", false, "suppress human-readable attestation on stderr")
	fs.BoolVar(&noPrettyAlias, "NoPretty", false, "suppress human-readable attestation on stderr")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	modelFlagSet := flagWasSet(fs, "model") || flagWasSet(fs, "Model")
	effortFlagSet := flagWasSet(fs, "effort") || flagWasSet(fs, "Effort")
	if opts.RepoPath == "" {
		opts.RepoPath = repoAlias
	}
	if prNumberAlias != 0 {
		opts.PRNumber = prNumberAlias
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
	if baseBranchAlias != "" {
		opts.BaseBranch = baseBranchAlias
	}
	if timeoutAlias != 0 {
		opts.Timeout = timeoutAlias
	}
	pretty = pretty || prettyAlias
	noPretty = noPretty || noPrettyAlias

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

	cfg, err := loadDeliveryConfig(resolvedRepo)
	if err != nil {
		fmt.Fprintf(stderr, "loopreview: %v\n", err)
		return 1
	}
	opts.Model, opts.Effort = applyRoleModelEffort(
		opts.Model,
		opts.Effort,
		modelFlagSet,
		effortFlagSet,
		cfg.Verifier.Model,
		cfg.Verifier.ReasoningEffort,
	)
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
	if result.Verdict.Attestation != nil {
		mode := prettyModeForTarget(stderr, deps, pretty)
		if err := writeLoopreviewRelayLedger(opts, *result.Verdict.Attestation, mode, deps.Now()); err != nil {
			fmt.Fprintf(stderr, "loopreview: write relay ledger: %v\n", err)
			return 1
		}
		if shouldRenderPretty(noPretty) {
			if err := renderPrettyAttestation(stderr, *result.Verdict.Attestation, mode); err != nil {
				fmt.Fprintf(stderr, "loopreview: write pretty attestation: %v\n", err)
				return 1
			}
		}
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

func runStatus(args []string, stdout, stderr io.Writer, _ Deps) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)

	repoPath := "."
	var repoAlias string
	var runID string
	var runIDAlias string

	fs.StringVar(&repoPath, "repo", ".", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")
	fs.StringVar(&runID, "run", "", "run id")
	fs.StringVar(&runIDAlias, "Run", "", "run id")
	fs.StringVar(&runIDAlias, "run-id", "", "run id")
	fs.StringVar(&runIDAlias, "RunId", "", "run id")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if repoAlias != "" {
		repoPath = repoAlias
	}
	if runIDAlias != "" {
		runID = runIDAlias
	}

	resolvedRepo, err := resolveRepo(repoPath)
	if err != nil {
		fmt.Fprintf(stderr, "status: %v\n", err)
		return 2
	}

	report, err := runstatus.Load(runstatus.Options{
		RepoPath: resolvedRepo,
		RunID:    runID,
	})
	if err != nil {
		fmt.Fprintf(stderr, "status: %v\n", err)
		return 1
	}
	if _, err := stdout.Write([]byte(runstatus.Render(report))); err != nil {
		fmt.Fprintf(stderr, "status: write output: %v\n", err)
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
