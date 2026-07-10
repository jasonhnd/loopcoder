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

	"github.com/jasonhnd/loopcoder/internal/audit"
	compiler "github.com/jasonhnd/loopcoder/internal/compile"
	"github.com/jasonhnd/loopcoder/internal/config"
	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/doctor"
	"github.com/jasonhnd/loopcoder/internal/loopreview"
	localmigrate "github.com/jasonhnd/loopcoder/internal/migrate"
	"github.com/jasonhnd/loopcoder/internal/migration"
	"github.com/jasonhnd/loopcoder/internal/models"
	"github.com/jasonhnd/loopcoder/internal/orchestration"
	"github.com/jasonhnd/loopcoder/internal/perception"
	"github.com/jasonhnd/loopcoder/internal/process"
	"github.com/jasonhnd/loopcoder/internal/recovery"
	"github.com/jasonhnd/loopcoder/internal/registry"
	"github.com/jasonhnd/loopcoder/internal/relay"
	"github.com/jasonhnd/loopcoder/internal/relaygate"
	"github.com/jasonhnd/loopcoder/internal/report"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/reportquery"
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
	NewGitHubReader   func(repoPath string) orchestration.GitHubReader
	NewIssueWriter    func(repoPath string) compiler.IssueWriter
	NewPreProdWriter  func(repoPath string) orchestration.PreProdWriter
	NewPromoteWriter  func(repoPath string) orchestration.PromotionWriter
	ProcessAlive      func(pid int) bool
	Now               func() time.Time
	IsTerminal        func(w io.Writer) bool
	Stdin             io.Reader
	BuildInfo         BuildInfo
	ComputeReadySet   func(ctx context.Context, opts orchestration.Options) (report.ReadySetReport, error)
	Tick              func(ctx context.Context, opts orchestration.TickOptions) (orchestration.TickReport, error)
	Discover          func(ctx context.Context, opts perception.Options) (perception.Report, error)
	Compile           func(ctx context.Context, opts compiler.Options) (compiler.Report, error)
	Dispatch          func(ctx context.Context, opts worker.Options) (worker.Result, error)
	Loopreview        func(ctx context.Context, opts loopreview.Options) (loopreview.Result, error)
	Promote           func(ctx context.Context, opts orchestration.PromoteOptions) (orchestration.PromoteReport, error)
	Recover           func(ctx context.Context, opts recovery.Options) (recovery.Result, error)
	Verify            func(ctx context.Context, opts verify.Options) verify.Result
	Audit             func(ctx context.Context, opts audit.Options) (audit.Result, error)
	Doctor            func(ctx context.Context, opts doctor.Options) doctor.Report
	Init              func(ctx context.Context, opts scaffold.Options) (scaffold.Result, error)
	Upgrade           func(ctx context.Context, opts upgrade.Options) (upgrade.Result, error)
	MigrateLocalState func(ctx context.Context, opts localmigrate.Options) (localmigrate.Result, error)
	SkillInstall      func(ctx context.Context, opts SkillInstallOptions) (SkillInstallResult, error)
	StatePush         func(ctx context.Context, opts statebranch.PushOptions) (statebranch.PushResult, error)
	StatePull         func(ctx context.Context, opts statebranch.PullOptions) (statebranch.PullResult, error)
	LeaseAcquire      func(ctx context.Context, opts statebranch.LeaseOptions) (statebranch.LeaseResult, error)
	LeaseRelease      func(ctx context.Context, opts statebranch.LeaseOptions) (statebranch.LeaseResult, error)
}

var commands = []Command{
	{Name: "attest", Summary: "emit conductor self-report"},
	{Name: "version", Summary: "print version and build information"},
	{Name: "models", Summary: "list static provider model and depth registry entries"},
	{Name: "projects", Summary: "manage the machine-local project registry"},
	{Name: "audit", Summary: "run a read-only repository security audit"},
	{Name: "doctor", Summary: "run read-only preflight checks"},
	{Name: "init", Summary: "scaffold loopcoder files in the current repository"},
	{Name: "discover", Summary: "discover CI failures and file GitHub issues"},
	{Name: "compile", Summary: "compile ROADMAP.md into GitHub issues"},
	{Name: "tick", Summary: "run one unattended delivery pass"},
	{Name: "trigger", Summary: "run automation triggers for tick"},
	{Name: "promote", Summary: "promote pre-prod to main"},
	{Name: "upgrade", Summary: "self-update from GitHub Releases"},
	{Name: "migrate", Summary: "import legacy repo-local state into local storage"},
	{Name: "skill", Summary: "install bundled playbook skill files"},
	{Name: "dispatch", Summary: "dispatch one issue worker"},
	{Name: "nested", Summary: "submit and execute a nested child plan"},
	{Name: "relay", Summary: "flush or list pending local report relay blocks"},
	{Name: "report", Summary: "list local reporter records"},
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
	{Name: "ps", Summary: "list loopcoder-managed worker processes"},
	{Name: "kill", Summary: "terminate loopcoder-managed processes (never by bare name)"},
}

const (
	loopreviewCommandFailureExitCode = 3
	relayGateExitCode                = 4
)

// Commands returns the registered subcommands in root help order.
func Commands() []Command {
	out := make([]Command, len(commands))
	copy(out, commands)
	return out
}

// Run executes the CLI and returns a process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	installShutdownOnSignal(stderr)
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
		Audit: func(ctx context.Context, opts audit.Options) (audit.Result, error) {
			return audit.Run(ctx, opts, audit.DefaultDeps())
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
		MigrateLocalState: func(ctx context.Context, opts localmigrate.Options) (localmigrate.Result, error) {
			return localmigrate.LocalState(ctx, opts, localmigrate.DefaultDeps())
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
	if command.Name == "report" {
		return runReport(args[1:], stdout, stderr)
	}
	if command.Name == "attest" {
		return runAttest(args[1:], stdout, stderr, deps)
	}
	if command.Name == "version" {
		return runVersion(args[1:], stdout, stderr, deps)
	}
	if command.Name == "models" {
		return runModels(args[1:], stdout, stderr)
	}
	if command.Name == "projects" {
		return runProjects(args[1:], stdout, stderr, deps)
	}
	if command.Name == "audit" {
		return runAudit(args[1:], stdout, stderr, deps)
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
	if command.Name == "trigger" {
		return runTrigger(args[1:], stdout, stderr, deps)
	}
	if command.Name == "promote" {
		return runPromote(args[1:], stdout, stderr, deps)
	}
	if command.Name == "upgrade" {
		return runUpgrade(args[1:], stdout, stderr, deps)
	}
	if command.Name == "migrate" {
		return runMigrate(args[1:], stdout, stderr, deps)
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
	if command.Name == "nested" {
		return runNested(args[1:], stdout, stderr, deps)
	}
	if command.Name == "relay" {
		return runRelay(args[1:], stdout, stderr)
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
	if command.Name == "ps" {
		return runPs(args[1:], stdout, stderr, deps)
	}
	if command.Name == "kill" {
		return runKill(args[1:], stdout, stderr, deps)
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
	if command.Name == "relay" {
		printRelayHelp(w)
		return
	}
	if command.Name == "projects" {
		printProjectsHelp(w)
		return
	}
	if command.Name == "nested" {
		printNestedHelp(w)
		return
	}
	if command.Name == "migrate" {
		printMigrateHelp(w)
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
		fmt.Fprintln(w, "  --strict                    reject invalid model/depth selections instead of warning")
		fmt.Fprintln(w, "  --config-from-base          read .delivery.yml from base branch when absent from working tree")
		fmt.Fprintln(w, "  --keep-worktree             preserve the scratch worktree and logs")
		fmt.Fprintln(w, "  --pretty                    force emoji pretty report on stderr (LOOPCODER_PRETTY; default is stderr, plain on non-TTY)")
		fmt.Fprintln(w, "  --no-pretty                 suppress pretty report on stderr (LOOPCODER_NO_PRETTY)")
	}
	if command.Name == "attest" {
		fmt.Fprintln(w, "  --role string            report role (default \"conductor\")")
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
		fmt.Fprintln(w, "  --pretty                 render human-readable report instead of durable output")
	}
	if command.Name == "doctor" {
		fmt.Fprintln(w, "  --repo string          repository path (default \".\")")
		fmt.Fprintln(w, "  --base-branch string   base branch to check for .delivery.yml mismatch (default \"main\")")
		fmt.Fprintln(w, "  --format string        output format: text or json (default \"text\")")
		fmt.Fprintln(w, "  --fix                  apply explicit storage permission repair, migrations, and stale local state cleanup")
	}
	if command.Name == "status" {
		fmt.Fprintln(w, "  --repo string     repository path (default \".\")")
		fmt.Fprintln(w, "  --run string      run id; omit to inspect the latest local run")
		fmt.Fprintln(w, "  --run-id string   alias for --run")
		fmt.Fprintln(w, "  --format string   output format: text or json (default \"text\")")
	}
	if command.Name == "report" {
		fmt.Fprintln(w, "  --repo string      repository path (default \".\")")
		fmt.Fprintln(w, "  --work-id string   filter by report work id")
		fmt.Fprintln(w, "  --run string       include run tree for a run id in JSON output")
		fmt.Fprintln(w, "  --issue int        filter by issue number")
		fmt.Fprintln(w, "  --role string      filter by role: worker, verifier, or conductor")
		fmt.Fprintln(w, "  --limit int        maximum reports to list (default 20)")
		fmt.Fprintln(w, "  --format string    output format: text or json (default \"text\")")
		fmt.Fprintln(w, "  --verbose          include raw canonical records in text output")
	}
	if command.Name == "models" {
		fmt.Fprintln(w, "  --provider string   registry provider key to render")
	}
	if command.Name == "audit" {
		fmt.Fprintln(w, "  --repo string                 repository path (default \".\")")
		fmt.Fprintln(w, "  --format string               output format: text, json, or both (default \"text\")")
		fmt.Fprintln(w, "  --layer string                audit layer to run; repeatable or comma-separated (default \"sast\")")
		fmt.Fprintln(w, "  --layers string               alias for --layer")
		fmt.Fprintln(w, "  --severity-threshold string   one-run severity threshold override")
		fmt.Fprintln(w, "  --threshold string            alias for --severity-threshold")
		fmt.Fprintf(w, "  --base-branch string          base branch for config lookup (default %q)\n", lcdefaults.BaseBranch)
		fmt.Fprintln(w, "  --config-from-base            read .delivery.yml from base branch when absent from working tree")
		fmt.Fprintln(w, "  --strict                      reject invalid LLM model/depth selections instead of warning")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Exit codes:")
		fmt.Fprintln(w, "  0   clean audit verdict")
		fmt.Fprintln(w, "  1   findings at or above the configured threshold")
		fmt.Fprintln(w, "  2   needs-human audit verdict")
		fmt.Fprintln(w, "  3   command/runtime failure")
		fmt.Fprintln(w, "  4   pending local relay block; run `loopcoder relay flush` before mechanical progress")
	}
	if command.Name == "init" {
		fmt.Fprintln(w, "  --force                     overwrite existing .delivery.yml and ROADMAP.md")
		fmt.Fprintln(w, "  --repo string               repository path (default \".\")")
		fmt.Fprintln(w, "  --gate string               generated promotion gate: human-merge or auto (default \"human-merge\")")
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
		fmt.Fprintf(w, "  --base-branch string             base branch for ready, dispatch, and review (default worker.base_branch or %q)\n", lcdefaults.BaseBranch)
		fmt.Fprintf(w, "  --pre-prod-branch string         pre-prod branch for clean unattended integrations (default environment.pre_prod_branch or %q)\n", lcdefaults.PreProdBranch)
		fmt.Fprintln(w, "  --run-id string                  shared run id for this pass (default generated once)")
		fmt.Fprintln(w, "  --worker-provider string         optional worker provider override for this pass")
		fmt.Fprintln(w, "  --verifier-provider string       optional verifier provider override for this pass")
		fmt.Fprintln(w, "  --worker-model string            optional worker model override for this pass")
		fmt.Fprintln(w, "  --worker-effort string           optional worker reasoning effort override for this pass")
		fmt.Fprintln(w, "  --verifier-model string          optional verifier model override for this pass")
		fmt.Fprintln(w, "  --verifier-effort string         optional verifier reasoning effort override for this pass")
		fmt.Fprintln(w, "  --verifier-timeout duration      verifier timeout (default 10m0s)")
		fmt.Fprintln(w, "  --strict                         reject invalid model/depth selections instead of warning")
		fmt.Fprintf(w, "  --throttle-limit int             maximum concurrent dispatches (default %d)\n", lcdefaults.DispatchWaveThrottleLimit)
		fmt.Fprintln(w, "  --config-from-base               read .delivery.yml from base branch when absent from working tree")
		fmt.Fprintln(w, "  --pretty                         force emoji pretty reports on stderr (LOOPCODER_PRETTY; default is stderr, plain on non-TTY)")
		fmt.Fprintln(w, "  --no-pretty                      suppress pretty reports on stderr (LOOPCODER_NO_PRETTY)")
	}
	if command.Name == "trigger" {
		fmt.Fprintln(w, "  <kind>                           trigger kind: cron, goal-loop, or hook")
		fmt.Fprintln(w, "  --repo string                    repository path (required)")
		fmt.Fprintf(w, "  --base-branch string             base branch for config checks and tick (default %q)\n", lcdefaults.BaseBranch)
		fmt.Fprintln(w, "  --schedule string                cron schedule metadata (cron)")
		fmt.Fprintln(w, "  --event string                   event name (hook)")
		fmt.Fprintln(w, "  --goal string                    goal predicate: roadmap-exhausted or no-ready-work (goal-loop)")
		fmt.Fprintln(w, "  --max-iterations int             maximum tick firings before needs-human (goal-loop)")
		fmt.Fprintln(w, "  --max_iterations int             alias for --max-iterations")
		fmt.Fprintln(w, "  --strict                         reject invalid model/depth selections instead of warning")
		fmt.Fprintln(w, "  --config-from-base               read .delivery.yml from base branch when absent from working tree")
		fmt.Fprintln(w, "  --pretty                         force emoji pretty reports on stderr (LOOPCODER_PRETTY; default is stderr, plain on non-TTY)")
		fmt.Fprintln(w, "  --no-pretty                      suppress pretty reports on stderr (LOOPCODER_NO_PRETTY)")
	}
	if command.Name == "promote" {
		fmt.Fprintln(w, "  --repo string              repository path (required)")
		fmt.Fprintf(w, "  --pre-prod-branch string   pre-prod branch to promote (default environment.pre_prod_branch or %q)\n", lcdefaults.PreProdBranch)
		fmt.Fprintln(w, "  --run-id string            run id for the promote ledger (default generated)")
		fmt.Fprintln(w, "  --kick-back string         item to revert out of pre-prod before promoting; repeatable")
		fmt.Fprintln(w, "  --config-from-base         read .delivery.yml from base branch when absent from working tree")
	}
	if command.Name == "upgrade" {
		fmt.Fprintln(w, "  --version string   release version to install (default latest stable)")
	}
	if command.Name == "ready-set" {
		fmt.Fprintln(w, "  --repo string          repository path (required)")
		fmt.Fprintf(w, "  --base-branch string   base branch for dependency reasoning (default %q)\n", lcdefaults.BaseBranch)
		fmt.Fprintln(w, "  --run-id string        local run id to inspect (default latest local run when present)")
		fmt.Fprintln(w, "  --format string        output format: text, json, or both (default \"text\")")
		fmt.Fprintln(w, "  --include-closed       include closed issues as diagnostic non-ready entries")
		fmt.Fprintln(w, "  --config-from-base     read .delivery.yml from base branch when absent from working tree")
	}
	if command.Name == "status" {
		fmt.Fprintln(w, "  --repo string   repository path (default \".\")")
		fmt.Fprintln(w, "  --run string    local run id to inspect (default latest modified local run)")
	}
	if command.Name == "report" {
		fmt.Fprintln(w, "  --repo string      repository path (default \".\")")
		fmt.Fprintln(w, "  --work-id string   filter by report work_id")
		fmt.Fprintln(w, "  --issue int        filter by GitHub issue number")
		fmt.Fprintln(w, "  --role string      filter by role: worker, verifier, or conductor")
		fmt.Fprintln(w, "  --limit int        maximum reports to render (default 20)")
		fmt.Fprintln(w, "  --format string    output format: text or json (default \"text\")")
	}
	if command.Name == "resume" {
		fmt.Fprintln(w, "  --repo string          repository path (required)")
		fmt.Fprintf(w, "  --base-branch string   base branch for branch and dependency reasoning (default %q)\n", lcdefaults.BaseBranch)
		fmt.Fprintln(w, "  --run-id string        local run id to inspect (default latest local run when present)")
		fmt.Fprintln(w, "  --format string        output format: text, json, or both (default \"text\")")
		fmt.Fprintln(w, "  --config-from-base     read .delivery.yml from base branch when absent from working tree")
	}
	if command.Name == "recover" {
		fmt.Fprintln(w, "  --repo string                   repository path (required)")
		fmt.Fprintln(w, "  --issue-number int              GitHub issue number (required)")
		fmt.Fprintln(w, "  --issue-title string            GitHub issue title (required)")
		fmt.Fprintln(w, "  --issue-body string             GitHub issue body")
		fmt.Fprintln(w, "  --run-id string                 run id containing attempt history (required)")
		fmt.Fprintf(w, "  --base-branch string            retry base branch (default %q)\n", lcdefaults.BaseBranch)
		fmt.Fprintf(w, "  --max-attempts int              retry limit (default %d)\n", lcdefaults.WorkerMaxAttempts)
		fmt.Fprintf(w, "  --backoff-seconds string        comma-separated retry backoff schedule (default %q)\n", csvInts(lcdefaults.WorkerRetryBackoffSeconds()))
		fmt.Fprintln(w, "  --provider string               worker provider (default \"codex\")")
		fmt.Fprintln(w, "  --model string                  optional worker model override for this run")
		fmt.Fprintln(w, "  --effort string                 optional worker reasoning effort override for this run")
		fmt.Fprintln(w, "  --upgraded-model string         optional upgraded retry worker model override")
		fmt.Fprintln(w, "  --upgraded-effort string        optional upgraded retry worker effort override (default \"xhigh\" when needed)")
		fmt.Fprintln(w, "  --verifier-provider string      optional verifier provider for recovered PRs")
		fmt.Fprintln(w, "  --verifier-model string         optional verifier model override for recovered PRs")
		fmt.Fprintln(w, "  --verifier-effort string        optional verifier effort override for recovered PRs")
		fmt.Fprintln(w, "  --verifier-timeout duration     verifier timeout for recovered PRs (default 10m0s)")
		fmt.Fprintln(w, "  --strict                        reject invalid model/depth selections instead of warning")
		fmt.Fprintln(w, "  --config-from-base              read .delivery.yml from base branch when absent from working tree")
	}
	if command.Name == "loopreview" {
		fmt.Fprintln(w, "  --repo string          repository path (required)")
		fmt.Fprintln(w, "  --pr-number int        pull request number to review (required)")
		fmt.Fprintln(w, "  --provider string      optional verifier provider override for this run")
		fmt.Fprintf(w, "  --base-branch string   base branch for merged spec lookup (default %q)\n", lcdefaults.BaseBranch)
		fmt.Fprintln(w, "  --model string         optional verifier model override for this run")
		fmt.Fprintln(w, "  --effort string        optional verifier reasoning effort override for this run")
		fmt.Fprintln(w, "  --strict               reject invalid model/depth selections instead of warning")
		fmt.Fprintln(w, "  --config-from-base     read .delivery.yml from base branch when absent from working tree")
		fmt.Fprintln(w, "  --timeout duration     verifier timeout (default 10m0s)")
		fmt.Fprintln(w, "  --pretty               force emoji pretty report on stderr (LOOPCODER_PRETTY; default is stderr, plain on non-TTY)")
		fmt.Fprintln(w, "  --no-pretty            suppress pretty report on stderr (LOOPCODER_NO_PRETTY)")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Exit codes:")
		fmt.Fprintln(w, "  0   clean verifier verdict: pass")
		fmt.Fprintln(w, "  1   clean verifier verdict: fail")
		fmt.Fprintln(w, "  2   clean verifier verdict: needs-human")
		fmt.Fprintln(w, "  3   command failure before/after a clean verdict (flags, repo, config, provider/git, or output error)")
		fmt.Fprintln(w, "  4   pending local relay block; run `loopcoder relay flush` before mechanical progress")
	}
	if command.Name == "verify-local" {
		fmt.Fprintln(w, "  --repo string          repository path (required)")
		fmt.Fprintln(w, "  --pr-number int        pull request number to verify (required unless --branch is set)")
		fmt.Fprintln(w, "  --branch string        branch to verify (required unless --pr-number is set)")
		fmt.Fprintf(w, "  --base-branch string   base branch for isolated checkout (default %q)\n", lcdefaults.BaseBranch)
	}
	if command.Name == "dispatch-wave" {
		fmt.Fprintln(w, "  --repo string              repository path (required)")
		fmt.Fprintf(w, "  --base-branch string       base branch passed to dispatch (default %q)\n", lcdefaults.BaseBranch)
		fmt.Fprintln(w, "  --run-id string            shared run id for the wave (default generated once)")
		fmt.Fprintln(w, "  --issue-numbers string     comma-separated issue numbers to dispatch")
		fmt.Fprintln(w, "  --from-ready-set           read ready-set JSON from stdin")
		fmt.Fprintln(w, "  --ready-set-path string    read ready-set JSON from a file")
		fmt.Fprintln(w, "  --provider string          optional worker provider pass-through")
		fmt.Fprintln(w, "  --model string             optional worker model override for this wave")
		fmt.Fprintln(w, "  --effort string            optional worker reasoning effort override for this wave")
		fmt.Fprintln(w, "  --strict                   reject invalid model/depth selections instead of warning")
		fmt.Fprintln(w, "  --config-from-base         read .delivery.yml from base branch when absent from working tree")
		fmt.Fprintf(w, "  --throttle-limit int       maximum concurrent dispatches (default %d)\n", lcdefaults.DispatchWaveThrottleLimit)
		fmt.Fprintln(w, "  --pretty                   force emoji pretty reports on stdout (LOOPCODER_PRETTY; default is stdout, plain on non-TTY)")
		fmt.Fprintln(w, "  --no-pretty                suppress pretty reports on stdout (LOOPCODER_NO_PRETTY)")
	}
	if command.Name == "hook" {
		fmt.Fprintf(w, "  <name>    hook to run: %s, conductor-relay-guard, or legacy %s\n", migration.ReporterHookName, migration.LegacyReporterHookName)
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

func printRelayHelp(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  loopcoder relay flush --repo <path>")
	fmt.Fprintln(w, "  loopcoder relay list --repo <path>")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flush or list pending local-only Worker/Verifier report relay blocks.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	fmt.Fprintln(w, "  --repo string   repository path (default \".\")")
	fmt.Fprintln(w, "  --help          show command help")
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

func runModels(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("models", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var provider string
	var providerAlias string
	fs.StringVar(&provider, "provider", "", "registry provider key")
	fs.StringVar(&providerAlias, "Provider", "", "registry provider key")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if providerAlias != "" {
		provider = providerAlias
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "models: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	registry := models.DefaultRegistry()
	providers := registry.Providers
	provider = strings.TrimSpace(provider)
	if provider != "" {
		selected, ok := registry.LookupProvider(provider)
		if !ok {
			fmt.Fprintf(stderr, "models: unknown provider %q (supported providers: %s)\n", provider, strings.Join(registry.ProviderNames(), ", "))
			if provider == "agy" {
				fmt.Fprintln(stderr, "models: hint: use --provider antigravity; agy is the CLI executable")
			}
			return 2
		}
		providers = []models.Provider{selected}
	}

	if err := renderModelProviders(stdout, providers); err != nil {
		fmt.Fprintf(stderr, "models: write output: %v\n", err)
		return 1
	}
	return 0
}

func renderModelProviders(w io.Writer, providers []models.Provider) error {
	for providerIndex, provider := range providers {
		if providerIndex > 0 {
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "provider: %s\n", provider.Name); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "vendor: %s\n", provider.Vendor); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "cli: %s\n", provider.CLI); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "default: %s / %s\n", provider.DefaultModel, renderedDefaultDepth(provider.DefaultDepth)); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w, "models:"); err != nil {
			return err
		}
		for _, model := range provider.Models {
			if _, err := fmt.Fprintf(w, "  - %s\n", model.Name); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "    depths: %s\n", renderedDepths(model)); err != nil {
				return err
			}
		}
	}
	return nil
}

func runProjects(args []string, stdout, stderr io.Writer, deps Deps) int {
	if len(args) == 0 || isHelp(args[0]) {
		printProjectsHelp(stdout)
		return 0
	}
	action := args[0]
	switch action {
	case "register":
		return runProjectsRegister(args[1:], stdout, stderr, deps)
	case "list":
		return runProjectsList(args[1:], stdout, stderr, deps)
	case "show":
		return runProjectsShow(args[1:], stdout, stderr, deps)
	case "remove":
		return runProjectsRemove(args[1:], stdout, stderr, deps)
	default:
		fmt.Fprintf(stderr, "projects: unknown subcommand %q\n\n", action)
		printProjectsHelp(stderr)
		return 2
	}
}

func printProjectsHelp(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  loopcoder projects register --repo <path> [--format text|json]")
	fmt.Fprintln(w, "  loopcoder projects list [--format text|json]")
	fmt.Fprintln(w, "  loopcoder projects show --repo <path> [--format text|json]")
	fmt.Fprintln(w, "  loopcoder projects remove --repo <path> [--format text|json]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Register, inspect, list active projects, and detach projects from the machine-local registry.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	fmt.Fprintln(w, "  --repo string     repository path (default \".\" where applicable)")
	fmt.Fprintln(w, "  --format string   output format: text or json (default \"text\")")
	fmt.Fprintln(w, "  --help            show command help")
}

func runProjectsRegister(args []string, stdout, stderr io.Writer, deps Deps) int {
	repoPath, format, ok := parseProjectsRepoFormat("projects register", args, stderr, true)
	if !ok {
		return 2
	}
	result, err := registry.Register(context.Background(), registry.Options{RepoPath: repoPath, Now: deps.Now}, registry.DefaultDeps())
	if err != nil {
		fmt.Fprintf(stderr, "projects register: %v\n", err)
		return 1
	}
	if format == "json" {
		return writeProjectJSON(stdout, stderr, "projects register", result)
	}
	action := "registered"
	if result.Updated {
		action = "updated"
	}
	fmt.Fprintf(stdout, "%s project %s (%s)\n", action, result.Project.ProjectID, result.Project.DisplayName)
	fmt.Fprintf(stdout, "path: %s\n", result.Project.LocalPath)
	fmt.Fprintf(stdout, "identity: %s\n", result.Project.IdentitySource)
	if result.Project.RemoteURLNormalized != "" {
		fmt.Fprintf(stdout, "remote: %s\n", result.Project.RemoteURLNormalized)
	}
	return 0
}

func runProjectsList(args []string, stdout, stderr io.Writer, deps Deps) int {
	_, format, ok := parseProjectsRepoFormat("projects list", args, stderr, false)
	if !ok {
		return 2
	}
	projects, err := registry.List(context.Background(), registry.Options{Now: deps.Now}, registry.DefaultDeps())
	if err != nil {
		fmt.Fprintf(stderr, "projects list: %v\n", err)
		return 1
	}
	if projects == nil {
		projects = []registry.Project{}
	}
	payload := struct {
		Projects []registry.Project `json:"projects"`
	}{Projects: projects}
	if format == "json" {
		return writeProjectJSON(stdout, stderr, "projects list", payload)
	}
	if len(projects) == 0 {
		fmt.Fprintln(stdout, "no registered projects")
		return 0
	}
	for _, project := range projects {
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", project.ProjectID, project.DisplayName, project.IdentitySource, project.LocalPath)
	}
	return 0
}

func runProjectsShow(args []string, stdout, stderr io.Writer, deps Deps) int {
	repoPath, format, ok := parseProjectsRepoFormat("projects show", args, stderr, true)
	if !ok {
		return 2
	}
	result, err := registry.Show(context.Background(), registry.Options{RepoPath: repoPath, Now: deps.Now}, registry.DefaultDeps())
	if err != nil {
		fmt.Fprintf(stderr, "projects show: %v\n", err)
		return 1
	}
	if format == "json" {
		return writeProjectJSON(stdout, stderr, "projects show", result)
	}
	status := "not registered"
	if result.Registered {
		status = "registered"
	} else if result.Detached {
		status = "detached"
	}
	fmt.Fprintf(stdout, "status: %s\n", status)
	fmt.Fprintf(stdout, "project_id: %s\n", result.Project.ProjectID)
	fmt.Fprintf(stdout, "display_name: %s\n", result.Project.DisplayName)
	fmt.Fprintf(stdout, "path: %s\n", result.Project.LocalPath)
	fmt.Fprintf(stdout, "identity: %s\n", result.Project.IdentitySource)
	if result.Project.RemoteURLNormalized != "" {
		fmt.Fprintf(stdout, "remote: %s\n", result.Project.RemoteURLNormalized)
	}
	if len(result.Conflicts) > 0 {
		fmt.Fprintln(stdout, "conflicts:")
		for _, conflict := range result.Conflicts {
			fmt.Fprintf(stdout, "  - %s %s %s\n", conflict.ProjectID, conflict.IdentitySource, conflict.RemoteURLNormalized)
		}
	}
	return 0
}

func runProjectsRemove(args []string, stdout, stderr io.Writer, deps Deps) int {
	repoPath, format, ok := parseProjectsRepoFormat("projects remove", args, stderr, true)
	if !ok {
		return 2
	}
	result, err := registry.Remove(context.Background(), registry.Options{RepoPath: repoPath, Now: deps.Now}, registry.DefaultDeps())
	if err != nil {
		fmt.Fprintf(stderr, "projects remove: %v\n", err)
		return 1
	}
	if format == "json" {
		return writeProjectJSON(stdout, stderr, "projects remove", result)
	}
	if result.Removed {
		fmt.Fprintf(stdout, "detached project %s (%s)\n", result.Project.ProjectID, result.Project.DisplayName)
	} else if result.Detached {
		fmt.Fprintf(stdout, "project %s is already detached\n", result.Project.ProjectID)
	} else {
		fmt.Fprintf(stdout, "project %s is not registered\n", result.Project.ProjectID)
	}
	fmt.Fprintln(stdout, "run_history_deleted: false")
	fmt.Fprintln(stdout, "project_deleted: false")
	return 0
}

func parseProjectsRepoFormat(name string, args []string, stderr io.Writer, includeRepo bool) (string, string, bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)

	repoPath := "."
	var repoAlias string
	format := "text"
	var formatAlias string
	if includeRepo {
		fs.StringVar(&repoPath, "repo", ".", "repository path")
		fs.StringVar(&repoAlias, "Repo", "", "repository path")
	}
	fs.StringVar(&format, "format", "text", "output format")
	fs.StringVar(&formatAlias, "Format", "", "output format")
	if err := fs.Parse(args); err != nil {
		return "", "", false
	}
	if includeRepo && repoAlias != "" {
		repoPath = repoAlias
	}
	if formatAlias != "" {
		format = formatAlias
	}
	format = strings.ToLower(strings.TrimSpace(format))
	if format == "" {
		format = "text"
	}
	if format != "text" && format != "json" {
		fmt.Fprintf(stderr, "%s: invalid --format %q; want text or json\n", name, format)
		return "", "", false
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "%s: unexpected argument %q\n", name, fs.Arg(0))
		return "", "", false
	}
	return repoPath, format, true
}

func writeProjectJSON(stdout, stderr io.Writer, prefix string, payload any) int {
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(payload); err != nil {
		fmt.Fprintf(stderr, "%s: write output: %v\n", prefix, err)
		return 1
	}
	return 0
}

func runMigrate(args []string, stdout, stderr io.Writer, deps Deps) int {
	if len(args) == 0 || isHelp(args[0]) {
		printMigrateHelp(stdout)
		return 0
	}
	switch args[0] {
	case "local-state":
		return runMigrateLocalState(args[1:], stdout, stderr, deps)
	default:
		fmt.Fprintf(stderr, "migrate: unknown subcommand %q\n\n", args[0])
		printMigrateHelp(stderr)
		return 2
	}
}

func printMigrateHelp(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  loopcoder migrate local-state --repo <path> [--dry-run] [--format text|json]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Import legacy repo-local .loopcoder records into machine-local storage.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Subcommands:")
	fmt.Fprintln(w, "  local-state   import v0.6.x repo-local run, relay, recovery, and report records")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	fmt.Fprintln(w, "  --repo        repository path for local-state (default \".\")")
	fmt.Fprintln(w, "  --dry-run     scan without writing machine-local storage")
	fmt.Fprintln(w, "  --format      output format for local-state: text or json (default \"text\")")
	fmt.Fprintln(w, "  --help        show command help")
}

func runMigrateLocalState(args []string, stdout, stderr io.Writer, deps Deps) int {
	if deps.MigrateLocalState == nil {
		deps.MigrateLocalState = DefaultDeps().MigrateLocalState
	}
	fs := flag.NewFlagSet("migrate local-state", flag.ContinueOnError)
	fs.SetOutput(stderr)

	repoPath := "."
	var repoAlias string
	format := "text"
	var formatAlias string
	dryRun := false
	var dryRunAlias bool

	fs.StringVar(&repoPath, "repo", ".", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")
	fs.StringVar(&format, "format", "text", "output format")
	fs.StringVar(&formatAlias, "Format", "", "output format")
	fs.BoolVar(&dryRun, "dry-run", false, "scan legacy records without writing machine-local storage")
	fs.BoolVar(&dryRunAlias, "DryRun", false, "scan legacy records without writing machine-local storage")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if repoAlias != "" {
		repoPath = repoAlias
	}
	if formatAlias != "" {
		format = formatAlias
	}
	dryRun = dryRun || dryRunAlias
	format = strings.ToLower(strings.TrimSpace(format))
	switch format {
	case "text", "json":
	default:
		fmt.Fprintf(stderr, "migrate local-state: invalid --format %q; want text or json\n", format)
		return 2
	}
	resolvedRepo, err := resolveRepo(repoPath)
	if err != nil {
		fmt.Fprintf(stderr, "migrate local-state: %v\n", err)
		return 2
	}
	result, err := deps.MigrateLocalState(context.Background(), localmigrate.Options{
		RepoPath: resolvedRepo,
		DryRun:   dryRun,
		Now:      deps.Now,
	})
	if err != nil {
		fmt.Fprintf(stderr, "migrate local-state: %v\n", err)
		return 1
	}
	if format == "json" {
		data, err := json.Marshal(result)
		if err != nil {
			fmt.Fprintf(stderr, "migrate local-state: %v\n", err)
			return 1
		}
		if _, err := stdout.Write(append(data, '\n')); err != nil {
			fmt.Fprintf(stderr, "migrate local-state: write output: %v\n", err)
			return 1
		}
		return 0
	}
	if _, err := stdout.Write([]byte(renderMigrateLocalStateText(result))); err != nil {
		fmt.Fprintf(stderr, "migrate local-state: write output: %v\n", err)
		return 1
	}
	return 0
}

func renderMigrateLocalStateText(result localmigrate.Result) string {
	var out strings.Builder
	fmt.Fprintln(&out, "LOCAL STATE MIGRATION")
	fmt.Fprintf(&out, "status: %s\n", result.Status)
	fmt.Fprintf(&out, "project_id: %s\n", displayText(result.ProjectID))
	fmt.Fprintf(&out, "database: %s\n", displayText(result.DatabasePath))
	if result.DryRun {
		fmt.Fprintln(&out, "dry_run: true")
	}
	fmt.Fprintf(&out, "scanned: %d\n", result.ScannedCount)
	fmt.Fprintf(&out, "imported: %d\n", result.ImportedCount)
	fmt.Fprintf(&out, "skipped: %d\n", result.SkippedCount)
	fmt.Fprintf(&out, "reports: %d\n", result.ReportCount)
	fmt.Fprintf(&out, "malformed: %d\n", result.MalformedCount)
	if len(result.Diagnostics) > 0 {
		fmt.Fprintln(&out, "diagnostics:")
		for _, diagnostic := range result.Diagnostics {
			if diagnostic.Line > 0 {
				fmt.Fprintf(&out, "  - %s:%d: %s\n", diagnostic.SourcePath, diagnostic.Line, diagnostic.Message)
			} else {
				fmt.Fprintf(&out, "  - %s: %s\n", diagnostic.SourcePath, diagnostic.Message)
			}
		}
	}
	return out.String()
}

func displayText(value string) string {
	if strings.TrimSpace(value) == "" {
		return "not reported"
	}
	return value
}

func renderedDefaultDepth(depth string) string {
	if depth == "" {
		return "(none)"
	}
	return depth
}

func renderedDepths(model models.Model) string {
	if len(model.Depths) == 0 {
		return "(none)"
	}
	depths := make([]string, 0, len(model.Depths))
	for _, depth := range model.Depths {
		token := depth.Token
		if token == model.DefaultDepth {
			token += "*"
		}
		depths = append(depths, token)
	}
	return strings.Join(depths, ", ")
}

func runRelay(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "relay: expected flush or list")
		printRelayHelp(stderr)
		return 2
	}
	if isHelp(args[0]) {
		printRelayHelp(stdout)
		return 0
	}

	switch args[0] {
	case "flush":
		return runRelayFlush(args[1:], stdout, stderr)
	case "list":
		return runRelayList(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "relay: unknown subcommand %q\n", args[0])
		printRelayHelp(stderr)
		return 2
	}
}

func runRelayFlush(args []string, stdout, stderr io.Writer) int {
	repoPath, ok := parseRelayRepo("relay flush", args, stderr)
	if !ok {
		return 2
	}
	resolvedRepo, err := resolveRepo(repoPath)
	if err != nil {
		fmt.Fprintf(stderr, "relay flush: %v\n", err)
		return 2
	}
	if err := relaygate.Flush(resolvedRepo, stdout); err != nil {
		fmt.Fprintf(stderr, "relay flush: %v\n", err)
		return 1
	}
	return 0
}

func runRelayList(args []string, stdout, stderr io.Writer) int {
	repoPath, ok := parseRelayRepo("relay list", args, stderr)
	if !ok {
		return 2
	}
	resolvedRepo, err := resolveRepo(repoPath)
	if err != nil {
		fmt.Fprintf(stderr, "relay list: %v\n", err)
		return 2
	}
	for _, rec := range relaygate.List(resolvedRepo) {
		fmt.Fprintf(stdout, "role=%s pr=%d nonce=%s\n", rec.Role, rec.PRNumber, rec.Nonce)
	}
	return 0
}

func parseRelayRepo(name string, args []string, stderr io.Writer) (string, bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)

	repoPath := "."
	var repoAlias string
	fs.StringVar(&repoPath, "repo", ".", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")

	if err := fs.Parse(args); err != nil {
		return "", false
	}
	if repoAlias != "" {
		repoPath = repoAlias
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "%s: unexpected argument %q\n", name, fs.Arg(0))
		return "", false
	}
	return repoPath, true
}

func runDoctor(args []string, stdout, stderr io.Writer, deps Deps) int {
	if deps.Doctor == nil {
		deps.Doctor = DefaultDeps().Doctor
	}

	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var repoPath string
	var repoAlias string
	var baseBranch string
	var baseBranchAlias string
	var outputFormat string
	var fix bool
	fs.StringVar(&repoPath, "repo", ".", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")
	fs.StringVar(&baseBranch, "base-branch", lcdefaults.BaseBranch, "base branch")
	fs.StringVar(&baseBranchAlias, "BaseBranch", "", "base branch")
	fs.StringVar(&outputFormat, "format", "text", "output format: text or json")
	fs.BoolVar(&fix, "fix", false, "apply explicit storage permission repair, upgrade migrations, and stale local state cleanup")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	outputFormat = strings.ToLower(strings.TrimSpace(outputFormat))
	if outputFormat == "" {
		outputFormat = "text"
	}
	if outputFormat != "text" && outputFormat != "json" {
		fmt.Fprintf(stderr, "doctor: unsupported --format %q (want text or json)\n", outputFormat)
		return 2
	}
	if repoAlias != "" {
		repoPath = repoAlias
	}
	if baseBranchAlias != "" {
		baseBranch = baseBranchAlias
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

	build := doctor.BuildInfo{
		Version: deps.BuildInfo.Version,
		Commit:  deps.BuildInfo.Commit,
		Date:    deps.BuildInfo.Date,
	}
	report := deps.Doctor(context.Background(), doctor.Options{
		RepoPath:   resolvedRepo,
		BaseBranch: baseBranch,
		Fix:        fix,
		BuildInfo:  build,
	})
	report = doctor.WithMetadata(report, resolvedRepo, build)
	var renderErr error
	if outputFormat == "json" {
		renderErr = doctor.RenderJSON(stdout, report)
	} else {
		renderErr = doctor.Render(stdout, report)
	}
	if renderErr != nil {
		fmt.Fprintf(stderr, "doctor: write output: %v\n", renderErr)
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
	var repoPathAlias string
	var gateAlias string
	var workerModelAlias string
	var workerEffortAlias string
	var verifierModelAlias string
	var verifierEffortAlias string

	fs.BoolVar(&opts.Force, "force", false, "overwrite existing files")
	fs.BoolVar(&forceAlias, "Force", false, "overwrite existing files")
	fs.StringVar(&opts.RepoPath, "repo", ".", "repository path")
	fs.StringVar(&repoPathAlias, "Repo", "", "repository path")
	fs.StringVar(&opts.Gate, "gate", "", "generated promotion gate")
	fs.StringVar(&gateAlias, "Gate", "", "generated promotion gate")
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
	if repoPathAlias != "" {
		opts.RepoPath = repoPathAlias
	}
	if gateAlias != "" {
		opts.Gate = gateAlias
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

	resolvedRepo, err := resolveRepo(opts.RepoPath)
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
	var configFromBase bool
	var configFromBaseAlias bool
	var strict bool
	var strictAlias bool
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
	fs.IntVar(&throttleLimit, "throttle-limit", lcdefaults.DispatchWaveThrottleLimit, "throttle limit")
	fs.IntVar(&throttleLimitAlias, "ThrottleLimit", 0, "throttle limit")
	fs.BoolVar(&strict, "strict", false, "reject invalid model/depth selections instead of warning")
	fs.BoolVar(&strictAlias, "Strict", false, "reject invalid model/depth selections instead of warning")
	fs.BoolVar(&configFromBase, "config-from-base", false, "read .delivery.yml from base branch when absent from working tree")
	fs.BoolVar(&configFromBaseAlias, "ConfigFromBase", false, "read .delivery.yml from base branch when absent from working tree")
	fs.BoolVar(&pretty, "pretty", false, "render human-readable reports on stderr")
	fs.BoolVar(&prettyAlias, "Pretty", false, "render human-readable reports on stderr")
	fs.BoolVar(&noPretty, "no-pretty", false, "suppress human-readable reports on stderr")
	fs.BoolVar(&noPrettyAlias, "NoPretty", false, "suppress human-readable reports on stderr")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	baseBranchFlagSet := flagWasSet(fs, "base-branch") || flagWasSet(fs, "BaseBranch")
	preProdBranchFlagSet := flagWasSet(fs, "pre-prod-branch") || flagWasSet(fs, "PreProdBranch")
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
	configFromBase = configFromBase || configFromBaseAlias
	strict = strict || strictAlias
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
	preExistingRelayNonces := relayRecordNonces(relaygate.Check(resolvedRepo))
	cfg, err := loadDeliveryConfig(resolvedRepo, baseBranch, configFromBase)
	if err != nil {
		fmt.Fprintf(stderr, "tick: %v\n", err)
		return 1
	}
	if !baseBranchFlagSet && strings.TrimSpace(baseBranch) == "" {
		baseBranch = strings.TrimSpace(cfg.Worker.BaseBranch)
	}
	if strings.TrimSpace(baseBranch) == "" {
		baseBranch = lcdefaults.BaseBranch
	}
	if !preProdBranchFlagSet && strings.TrimSpace(preProdBranch) == "" {
		preProdBranch = strings.TrimSpace(cfg.Environment.PreProdBranch)
	}
	if strings.TrimSpace(preProdBranch) == "" {
		preProdBranch = lcdefaults.PreProdBranch
	}
	if strings.TrimSpace(workerProvider) == "" {
		workerProvider = strings.TrimSpace(cfg.Adapters.Worker)
	}
	if strings.TrimSpace(verifierProvider) == "" {
		verifierProvider = strings.TrimSpace(cfg.Adapters.Verifier)
	}
	workerSelection, ok := resolveAndValidateRoleSelection(roleSelectionInput{
		Role:           "worker",
		Provider:       workerProvider,
		Model:          workerModel,
		Effort:         workerEffort,
		ConfigProvider: cfg.Adapters.Worker,
		ConfigModel:    cfg.Worker.Model,
		ConfigEffort:   cfg.Worker.ReasoningEffort,
		Strict:         cfg.Models.Strict || strict,
		Warnings:       stderr,
	})
	if !ok {
		return 1
	}
	workerProvider = workerSelection.Provider
	workerModel = workerSelection.Model
	workerEffort = workerSelection.Effort
	verifierSelection, ok := resolveAndValidateRoleSelection(roleSelectionInput{
		Role:           "verifier",
		Provider:       verifierProvider,
		Model:          verifierModel,
		Effort:         verifierEffort,
		ConfigProvider: cfg.Adapters.Verifier,
		ConfigModel:    cfg.Verifier.Model,
		ConfigEffort:   cfg.Verifier.ReasoningEffort,
		Strict:         cfg.Models.Strict || strict,
		Warnings:       stderr,
	})
	if !ok {
		return 1
	}
	verifierProvider = verifierSelection.Provider
	verifierModel = verifierSelection.Model
	verifierEffort = verifierSelection.Effort
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
		ConfigFromBase:     configFromBase,
		VerifierProvider:   verifierProvider,
		VerifierModel:      verifierModel,
		VerifierEffort:     verifierEffort,
		VerifierTimeout:    verifierTimeout,
		ThrottleLimit:      throttleLimit,
		RequiredChecks:     cfg.CI.Checks,
		ConfiguredEvidence: cfg.Evidence.Artifacts(),
		AdditionalRiskRedLines: orchestration.DomainRedLines(
			cfg.Domain.RedLines,
		),
		Thresholds:      cfg.Resilience.Worker,
		Budget:          cfg.Guardrails.Budget,
		CircuitBreaker:  cfg.Guardrails.CircuitBreaker,
		ProcessAlive:    deps.ProcessAlive,
		Clock:           deps.Now,
		Stderr:          stderr,
		Compile:         deps.Compile,
		ComputeReadySet: deps.ComputeReadySet,
		Dispatch:        deps.Dispatch,
		Loopreview:      deps.Loopreview,
		Recover:         deps.Recover,
		PreProdWriter:   deps.NewPreProdWriter(resolvedRepo),
		StatePush:       deps.StatePush,
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
	var ownRelayRecords []relaygate.Record
	prettyMode := reporter.PrettyModePlain
	renderPretty := shouldRenderPretty(noPretty)
	if renderPretty {
		prettyMode = prettyModeForTarget(stderr, deps, pretty)
		ownRelayRecords, err = writeTickRelayRecords(resolvedRepo, tickReport, prettyMode, preExistingRelayNonces)
		if err != nil {
			fmt.Fprintf(stderr, "tick: write relay records: %v\n", err)
			return 1
		}
	}
	if _, err := stdout.Write(data); err != nil {
		fmt.Fprintf(stderr, "tick: write output: %v\n", err)
		return 1
	}
	if _, err := stderr.Write([]byte(orchestration.RenderTickText(tickReport))); err != nil {
		fmt.Fprintf(stderr, "tick: write summary: %v\n", err)
		return 1
	}
	if renderPretty {
		if err := renderTickPrettyReports(stderr, tickReport, prettyMode); err != nil {
			fmt.Fprintf(stderr, "tick: write pretty report: %v\n", err)
			return 1
		}
		if err := relaygate.Ack(resolvedRepo, ownRelayRecords); err != nil {
			fmt.Fprintf(stderr, "tick: acknowledge relay records: %v\n", err)
			return 1
		}
	}
	return orchestration.TickExitCode(tickReport)
}

func runTrigger(args []string, stdout, stderr io.Writer, deps Deps) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "trigger: expected trigger kind: cron, goal-loop, or hook")
		return 2
	}
	kind := strings.TrimSpace(args[0])
	if kind == "" {
		fmt.Fprintln(stderr, "trigger: expected trigger kind: cron, goal-loop, or hook")
		return 2
	}

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

	fs := flag.NewFlagSet("trigger "+kind, flag.ContinueOnError)
	fs.SetOutput(stderr)

	var repoPath string
	var repoAlias string
	var baseBranch string
	var baseBranchAlias string
	var schedule string
	var scheduleAlias string
	var event string
	var eventAlias string
	var goal string
	var goalAlias string
	var maxIterations int
	var maxIterationsAlias int
	var maxIterationsSnakeAlias int
	var configFromBase bool
	var configFromBaseAlias bool
	var strict bool
	var strictAlias bool
	var pretty bool
	var prettyAlias bool
	var noPretty bool
	var noPrettyAlias bool

	fs.StringVar(&repoPath, "repo", "", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")
	fs.StringVar(&baseBranch, "base-branch", lcdefaults.BaseBranch, "base branch")
	fs.StringVar(&baseBranchAlias, "BaseBranch", "", "base branch")
	fs.StringVar(&schedule, "schedule", "", "cron schedule")
	fs.StringVar(&scheduleAlias, "Schedule", "", "cron schedule")
	fs.StringVar(&event, "event", "", "hook event")
	fs.StringVar(&eventAlias, "Event", "", "hook event")
	fs.StringVar(&goal, "goal", "roadmap-exhausted", "goal predicate")
	fs.StringVar(&goalAlias, "Goal", "", "goal predicate")
	fs.IntVar(&maxIterations, "max-iterations", 0, "max iterations")
	fs.IntVar(&maxIterationsSnakeAlias, "max_iterations", 0, "max iterations")
	fs.IntVar(&maxIterationsAlias, "MaxIterations", 0, "max iterations")
	fs.BoolVar(&strict, "strict", false, "reject invalid model/depth selections instead of warning")
	fs.BoolVar(&strictAlias, "Strict", false, "reject invalid model/depth selections instead of warning")
	fs.BoolVar(&configFromBase, "config-from-base", false, "read .delivery.yml from base branch when absent from working tree")
	fs.BoolVar(&configFromBaseAlias, "ConfigFromBase", false, "read .delivery.yml from base branch when absent from working tree")
	fs.BoolVar(&pretty, "pretty", false, "render human-readable reports on stderr")
	fs.BoolVar(&prettyAlias, "Pretty", false, "render human-readable reports on stderr")
	fs.BoolVar(&noPretty, "no-pretty", false, "suppress human-readable reports on stderr")
	fs.BoolVar(&noPrettyAlias, "NoPretty", false, "suppress human-readable reports on stderr")

	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if repoPath == "" {
		repoPath = repoAlias
	}
	baseBranchFlagSet := flagWasSet(fs, "base-branch") || flagWasSet(fs, "BaseBranch")
	if baseBranchAlias != "" {
		baseBranch = baseBranchAlias
	}
	if scheduleAlias != "" {
		schedule = scheduleAlias
	}
	if eventAlias != "" {
		event = eventAlias
	}
	if goalAlias != "" {
		goal = goalAlias
	}
	if maxIterationsSnakeAlias != 0 {
		maxIterations = maxIterationsSnakeAlias
	}
	if maxIterationsAlias != 0 {
		maxIterations = maxIterationsAlias
	}
	strict = strict || strictAlias
	configFromBase = configFromBase || configFromBaseAlias
	pretty = pretty || prettyAlias
	noPretty = noPretty || noPrettyAlias

	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "trigger %s: unexpected argument %q\n", kind, fs.Arg(0))
		return 2
	}
	if strings.TrimSpace(repoPath) == "" {
		fmt.Fprintf(stderr, "trigger %s: --repo is required\n", kind)
		return 2
	}
	if kind == orchestration.TriggerKindCron && strings.TrimSpace(schedule) == "" {
		fmt.Fprintln(stderr, "trigger cron: --schedule is required")
		return 2
	}
	if kind == orchestration.TriggerKindHook && strings.TrimSpace(event) == "" {
		fmt.Fprintln(stderr, "trigger hook: --event is required")
		return 2
	}
	if kind == orchestration.TriggerKindGoalLoop && maxIterations <= 0 {
		fmt.Fprintln(stderr, "trigger goal-loop: --max-iterations is required and must be greater than zero")
		return 2
	}
	if kind == orchestration.TriggerKindGoalLoop {
		normalizedGoal := strings.TrimSpace(goal)
		if normalizedGoal != "" && normalizedGoal != "roadmap-exhausted" && normalizedGoal != "no-ready-work" {
			fmt.Fprintf(stderr, "trigger goal-loop: unsupported --goal %q\n", goal)
			return 2
		}
	}
	if kind != orchestration.TriggerKindCron && kind != orchestration.TriggerKindGoalLoop && kind != orchestration.TriggerKindHook {
		fmt.Fprintf(stderr, "trigger: unknown trigger kind %q\n", kind)
		return 2
	}

	resolvedRepo, err := resolveRepo(repoPath)
	if err != nil {
		fmt.Fprintf(stderr, "trigger %s: %v\n", kind, err)
		return 2
	}
	preExistingRelayNonces := relayRecordNonces(relaygate.Check(resolvedRepo))
	cfg, err := loadDeliveryConfig(resolvedRepo, baseBranch, configFromBase)
	if err != nil {
		fmt.Fprintf(stderr, "trigger %s: %v\n", kind, err)
		return 1
	}
	tickBaseBranch := ""
	if baseBranchFlagSet {
		tickBaseBranch = baseBranch
	}
	tickOptions, ok := tickOptionsFromConfig(resolvedRepo, stderr, deps, cfg, configFromBase, tickBaseBranch, strict)
	if !ok {
		return 1
	}
	if warning := config.ReviewerNotWorkerWarning(config.Adapters{
		Worker:   tickOptions.WorkerProvider,
		Verifier: tickOptions.VerifierProvider,
	}); warning != "" {
		fmt.Fprintf(stderr, "[loopcoder] warning: %s\n", warning)
	}

	triggerReport, err := orchestration.RunTrigger(context.Background(), orchestration.TriggerOptions{
		Kind:          kind,
		RepoPath:      resolvedRepo,
		Schedule:      schedule,
		Event:         event,
		Goal:          goal,
		MaxIterations: maxIterations,
		TickOptions:   tickOptions,
		Tick:          deps.Tick,
		Clock:         deps.Now,
	})
	if err != nil {
		fmt.Fprintf(stderr, "trigger %s: %v\n", kind, err)
		return 1
	}
	data, err := orchestration.MarshalTriggerJSON(triggerReport)
	if err != nil {
		fmt.Fprintf(stderr, "trigger %s: %v\n", kind, err)
		return 1
	}
	var ownRelayRecords []relaygate.Record
	prettyMode := reporter.PrettyModePlain
	renderPretty := shouldRenderPretty(noPretty)
	if renderPretty {
		prettyMode = prettyModeForTarget(stderr, deps, pretty)
		ownRelayRecords, err = writeTriggerRelayRecords(resolvedRepo, triggerReport, prettyMode, preExistingRelayNonces)
		if err != nil {
			fmt.Fprintf(stderr, "trigger %s: write relay records: %v\n", kind, err)
			return 1
		}
	}
	if _, err := stdout.Write(data); err != nil {
		fmt.Fprintf(stderr, "trigger %s: write output: %v\n", kind, err)
		return 1
	}
	if _, err := stderr.Write([]byte(orchestration.RenderTriggerText(triggerReport))); err != nil {
		fmt.Fprintf(stderr, "trigger %s: write summary: %v\n", kind, err)
		return 1
	}
	if renderPretty {
		if err := renderTriggerPrettyReports(stderr, triggerReport, prettyMode); err != nil {
			fmt.Fprintf(stderr, "trigger %s: write pretty report: %v\n", kind, err)
			return 1
		}
		if err := relaygate.Ack(resolvedRepo, ownRelayRecords); err != nil {
			fmt.Fprintf(stderr, "trigger %s: acknowledge relay records: %v\n", kind, err)
			return 1
		}
	}
	return orchestration.TriggerExitCode(triggerReport)
}

func tickOptionsFromConfig(repoPath string, stderr io.Writer, deps Deps, cfg config.Config, configFromBase bool, explicitBaseBranch string, strictOverride bool) (orchestration.TickOptions, bool) {
	baseBranch := strings.TrimSpace(explicitBaseBranch)
	if baseBranch == "" {
		baseBranch = strings.TrimSpace(cfg.Worker.BaseBranch)
	}
	if baseBranch == "" {
		baseBranch = lcdefaults.BaseBranch
	}
	preProdBranch := strings.TrimSpace(cfg.Environment.PreProdBranch)
	if preProdBranch == "" {
		preProdBranch = lcdefaults.PreProdBranch
	}
	workerSelection, ok := resolveAndValidateRoleSelection(roleSelectionInput{
		Role:           "worker",
		ConfigProvider: cfg.Adapters.Worker,
		ConfigModel:    cfg.Worker.Model,
		ConfigEffort:   cfg.Worker.ReasoningEffort,
		Strict:         cfg.Models.Strict || strictOverride,
		Warnings:       stderr,
	})
	if !ok {
		return orchestration.TickOptions{}, false
	}
	verifierSelection, ok := resolveAndValidateRoleSelection(roleSelectionInput{
		Role:           "verifier",
		ConfigProvider: cfg.Adapters.Verifier,
		ConfigModel:    cfg.Verifier.Model,
		ConfigEffort:   cfg.Verifier.ReasoningEffort,
		Strict:         cfg.Models.Strict || strictOverride,
		Warnings:       stderr,
	})
	if !ok {
		return orchestration.TickOptions{}, false
	}
	return orchestration.TickOptions{
		Reader:             deps.NewGitHubReader(repoPath),
		IssueWriter:        deps.NewIssueWriter(repoPath),
		RepoPath:           repoPath,
		BaseBranch:         baseBranch,
		PreProdBranch:      preProdBranch,
		WorkerProvider:     workerSelection.Provider,
		WorkerModel:        workerSelection.Model,
		WorkerEffort:       workerSelection.Effort,
		ConfigFromBase:     configFromBase,
		VerifierProvider:   verifierSelection.Provider,
		VerifierModel:      verifierSelection.Model,
		VerifierEffort:     verifierSelection.Effort,
		VerifierTimeout:    loopreview.DefaultVerifierTimeout,
		ThrottleLimit:      lcdefaults.DispatchWaveThrottleLimit,
		RequiredChecks:     cfg.CI.Checks,
		ConfiguredEvidence: cfg.Evidence.Artifacts(),
		AdditionalRiskRedLines: orchestration.DomainRedLines(
			cfg.Domain.RedLines,
		),
		Thresholds:      cfg.Resilience.Worker,
		Budget:          cfg.Guardrails.Budget,
		CircuitBreaker:  cfg.Guardrails.CircuitBreaker,
		ProcessAlive:    deps.ProcessAlive,
		Clock:           deps.Now,
		Stderr:          stderr,
		Compile:         deps.Compile,
		ComputeReadySet: deps.ComputeReadySet,
		Dispatch:        deps.Dispatch,
		Loopreview:      deps.Loopreview,
		Recover:         deps.Recover,
		PreProdWriter:   deps.NewPreProdWriter(repoPath),
		StatePush:       deps.StatePush,
	}, true
}

func renderTriggerPrettyReports(w io.Writer, report orchestration.TriggerReport, mode reporter.PrettyMode) error {
	for _, tick := range report.Ticks {
		if err := renderTickPrettyReports(w, tick, mode); err != nil {
			return err
		}
	}
	return nil
}

func relayRecordNonces(records []relaygate.Record) map[string]bool {
	nonces := make(map[string]bool, len(records))
	for _, rec := range records {
		nonce := strings.TrimSpace(rec.Nonce)
		if nonce != "" {
			nonces[nonce] = true
		}
	}
	return nonces
}

func writeTriggerRelayRecords(repoPath string, report orchestration.TriggerReport, mode reporter.PrettyMode, preExisting map[string]bool) ([]relaygate.Record, error) {
	var records []relaygate.Record
	for _, tick := range report.Ticks {
		tickRecords, err := writeTickRelayRecords(repoPath, tick, mode, preExisting)
		if err != nil {
			return records, err
		}
		records = append(records, tickRecords...)
	}
	return records, nil
}

func writeTickRelayRecords(repoPath string, report orchestration.TickReport, mode reporter.PrettyMode, preExisting map[string]bool) ([]relaygate.Record, error) {
	var records []relaygate.Record
	if report.DispatchWave != nil {
		runID := strings.TrimSpace(report.DispatchWave.RunID)
		if runID == "" {
			runID = strings.TrimSpace(report.RunID)
		}
		for _, result := range report.DispatchWave.Results {
			if result.Report == nil {
				continue
			}
			rec, ok, err := writeAutonomousRelayRecord(repoPath, runID, string(result.Report.Role), prNumberFromPR(result.PR), *result.Report, mode, preExisting)
			if err != nil {
				return records, err
			}
			if ok {
				records = append(records, rec)
			}
		}
	}
	for _, review := range report.Reviews {
		if review.Report == nil {
			continue
		}
		prNumber := review.PRNumber
		if prNumber == 0 {
			prNumber = prNumberFromPR(review.PR)
		}
		runID := fmt.Sprintf("loopreview-pr-%d", prNumber)
		rec, ok, err := writeAutonomousRelayRecord(repoPath, runID, string(review.Report.Role), prNumber, *review.Report, mode, preExisting)
		if err != nil {
			return records, err
		}
		if ok {
			records = append(records, rec)
		}
	}
	return records, nil
}

func writeAutonomousRelayRecord(repoPath, runID, role string, prNumber int, record reporter.Report, mode reporter.PrettyMode, preExisting map[string]bool) (relaygate.Record, bool, error) {
	pretty := prettyReport(record, reporter.PrettyOptions{Mode: mode, PR: formatPRNumber(prNumber)})
	nonce := relaygate.Nonce(runID, prNumber, role)
	if _, err := relaygate.Write(relaygate.WriteOptions{
		RepoPath: repoPath,
		RunID:    runID,
		Role:     role,
		PRNumber: prNumber,
		Block:    pretty,
		Report:   &record,
	}); err != nil {
		return relaygate.Record{}, false, err
	}
	if preExisting[nonce] {
		return relaygate.Record{}, false, nil
	}
	return relaygate.Record{Nonce: nonce}, true, nil
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
	var configFromBase bool
	var configFromBaseAlias bool

	fs.StringVar(&repoPath, "repo", "", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")
	fs.StringVar(&preProdBranch, "pre-prod-branch", "", "pre-prod branch")
	fs.StringVar(&preProdBranchAlias, "PreProdBranch", "", "pre-prod branch")
	fs.StringVar(&runID, "run-id", "", "run id")
	fs.StringVar(&runIDAlias, "RunId", "", "run id")
	fs.Var(&kickBack, "kick-back", "kick-back item")
	fs.Var(&kickBackAlias, "KickBack", "kick-back item")
	fs.BoolVar(&configFromBase, "config-from-base", false, "read .delivery.yml from base branch when absent from working tree")
	fs.BoolVar(&configFromBaseAlias, "ConfigFromBase", false, "read .delivery.yml from base branch when absent from working tree")

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
	configFromBase = configFromBase || configFromBaseAlias

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
	if exitCode, blocked := checkRelayGate(resolvedRepo, stdout, stderr); blocked {
		return exitCode
	}
	cfg, err := loadDeliveryConfig(resolvedRepo, lcdefaults.BaseBranch, configFromBase)
	if err != nil {
		fmt.Fprintf(stderr, "promote: %v\n", err)
		return 1
	}
	if !preProdBranchFlagSet && strings.TrimSpace(preProdBranch) == "" {
		preProdBranch = strings.TrimSpace(cfg.Environment.PreProdBranch)
	}
	if strings.TrimSpace(preProdBranch) == "" {
		preProdBranch = lcdefaults.PreProdBranch
	}

	writer := deps.NewPromoteWriter(resolvedRepo)
	resolveAutoGate := func(ctx context.Context) (*orchestration.AutoGateInputs, error) {
		return orchestration.ResolvePromoteAutoGate(ctx, orchestration.AutoGateResolverOptions{
			Writer:             writer,
			RepoPath:           resolvedRepo,
			PreProdBranch:      preProdBranch,
			RequiredChecks:     cfg.CI.Checks,
			ConfiguredEvidence: cfg.Evidence.Artifacts(),
		})
	}

	report, err := deps.Promote(context.Background(), orchestration.PromoteOptions{
		Writer:          writer,
		RepoPath:        resolvedRepo,
		RunID:           runID,
		PreProdBranch:   preProdBranch,
		Gate:            cfg.Adapters.Gate,
		KickBackItems:   []string(kickBack),
		ResolveAutoGate: resolveAutoGate,
		RequiredChecks:  cfg.CI.Checks,
		Clock:           deps.Now,
		StatePush:       deps.StatePush,
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

func renderTickPrettyReports(w io.Writer, report orchestration.TickReport, mode reporter.PrettyMode) error {
	if report.DispatchWave != nil {
		for _, result := range report.DispatchWave.Results {
			if result.Report == nil {
				continue
			}
			if err := renderPrettyReportWithOptions(w, *result.Report, reporter.PrettyOptions{
				Mode:   mode,
				Status: result.Status,
				PR:     result.PR,
				Reason: result.Error,
			}); err != nil {
				return err
			}
		}
	}
	for _, review := range report.Reviews {
		if review.Report == nil {
			continue
		}
		blocking := blockingFindingCount(review.Findings)
		if err := renderPrettyReportWithOptions(w, *review.Report, reporter.PrettyOptions{
			Mode:            mode,
			Status:          review.Verdict,
			PR:              firstNonEmptyString(formatPRNumber(review.PRNumber), review.PR),
			BlockingDefects: &blocking,
			Reason:          firstNonEmptyString(firstReceiptLine(review.Evidence), firstReceiptLine(review.Error)),
			SpecConformance: review.SpecConformance,
			Findings:        prettyFindings(review.Findings),
		}); err != nil {
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
	if result.LocalStateExclude != nil {
		fmt.Fprintf(stdout, "  local-state %s %s\n", result.LocalStateExclude.Status, result.LocalStateExclude.Path)
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
		CurrentCommit:    build.Commit,
		CurrentDate:      build.Date,
	})
	result = fillUpgradeRenderDefaults(result, requestedVersion, build)
	if result.CurrentPath != "" || result.CurrentVersion != "" {
		renderUpgradeCurrent(stdout, result)
	}
	if err != nil {
		fmt.Fprintf(stderr, "upgrade: %v\n", err)
		return 1
	}
	renderUpgradeSuccess(stdout, result)
	if result.SkillRefresh.Warning != "" {
		fmt.Fprintf(stderr, "[loopcoder] warning: skill refresh failed after upgrade: %s; run: loopcoder skill install --global-only\n", result.SkillRefresh.Warning)
	}
	return 0
}

func fillUpgradeRenderDefaults(result upgrade.Result, requestedVersion string, build BuildInfo) upgrade.Result {
	if strings.TrimSpace(result.CurrentVersion) == "" {
		result.CurrentVersion = build.Version
	}
	if strings.TrimSpace(result.CurrentCommit) == "" {
		result.CurrentCommit = build.Commit
	}
	if strings.TrimSpace(result.CurrentDate) == "" {
		result.CurrentDate = build.Date
	}
	if strings.TrimSpace(result.RequestedVersion) == "" {
		result.RequestedVersion = requestedVersion
	}
	return result
}

func renderUpgradeCurrent(w io.Writer, result upgrade.Result) {
	line := fmt.Sprintf("Current selected binary: path=%s version=%s", result.CurrentPath, result.CurrentVersion)
	if strings.TrimSpace(result.CurrentCommit) != "" {
		line += " commit=" + result.CurrentCommit
	}
	if strings.TrimSpace(result.CurrentDate) != "" {
		line += " date=" + result.CurrentDate
	}
	fmt.Fprintln(w, line)
	requested := strings.TrimSpace(result.RequestedVersion)
	if requested == "" {
		requested = "latest stable"
	}
	fmt.Fprintf(w, "Requested target: %s\n", requested)
}

func renderUpgradeSuccess(w io.Writer, result upgrade.Result) {
	fmt.Fprintf(w, "Resolved target version: %s\n", result.TargetVersion)
	if result.AlreadyLatest {
		fmt.Fprintln(w, "Already latest; no download needed.")
		renderUpgradeVersionStatus(w, result)
		renderUpgradeMigrationStatus(w, result.MigrationStatus)
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
	renderUpgradeVersionStatus(w, result)
	renderUpgradeSkillRefresh(w, result.SkillRefresh)
	renderUpgradeMigrationStatus(w, result.MigrationStatus)
	fmt.Fprintln(w, "Run: loopcoder doctor --repo .")
}

func renderUpgradeVersionStatus(w io.Writer, result upgrade.Result) {
	status := result.VersionStatus
	if status.CurrentClassification == "" && status.TargetClassification == "" {
		return
	}
	if status.CurrentClassification == upgrade.VersionUnknown && status.TargetClassification != upgrade.VersionBreakingTransition {
		return
	}
	fmt.Fprintf(w, "Upgrade version status: current=%s (%s) target=%s (%s)\n",
		result.CurrentVersion,
		status.CurrentClassification,
		result.TargetVersion,
		status.TargetClassification,
	)
	if status.BreakingBoundary {
		fmt.Fprintln(w, "0.5.x -> 0.6.0 boundary detected; compatibility aliases are active for this transition and new output uses report/reporter names.")
	} else if status.CompatibilityAliasesActive {
		fmt.Fprintln(w, "0.6.0 transition selected; compatibility aliases are active and old reporter names remain accepted for this release.")
	}
}

func renderUpgradeSkillRefresh(w io.Writer, result upgrade.SkillRefreshResult) {
	if strings.TrimSpace(result.BinaryPath) == "" {
		return
	}
	fmt.Fprintf(w, "Skill refresh: %s skill install --global-only\n", result.BinaryPath)
	if result.Warning != "" {
		return
	}
	if result.Dir != "" {
		fmt.Fprintf(w, "  directory %s\n", result.Dir)
	}
	for _, file := range result.Files {
		fmt.Fprintf(w, "  %s %s\n", file.Status, file.Path)
	}
	if len(result.Files) > 0 {
		fmt.Fprintln(w, "  verified managed files: SKILL.md, AGENTS.md")
	}
}

func renderUpgradeMigrationStatus(w io.Writer, status upgrade.MigrationStatus) {
	if status.RepoPath == "" && !status.RepoAvailable && len(status.EnvDiagnostics) == 0 && status.ScanWarning == "" {
		return
	}
	fmt.Fprintln(w, "Migration status:")
	if status.RepoPath == "" {
		fmt.Fprintln(w, "  repo: unavailable; repo migration scan deferred to loopcoder doctor --repo <repo>")
	} else if !status.RepoAvailable {
		fmt.Fprintf(w, "  repo: %s is not a detected loopcoder repository; repo migration scan deferred to loopcoder doctor --repo <repo>\n", status.RepoPath)
	} else {
		fmt.Fprintf(w, "  repo: %s\n", status.RepoPath)
		if status.DeliveryVersion != "" || status.MinLoopcoderVersion != "" {
			fmt.Fprintf(w, "  delivery version: schema=%s min_loopcoder_version=%s\n", displayUpgradeValue(status.DeliveryVersion), displayUpgradeValue(status.MinLoopcoderVersion))
		}
		renderMigrationDiagnostics(w, "config", status.ConfigPresent, status.ConfigError, status.ConfigDiagnostics)
		renderMigrationDiagnosticList(w, "env", status.EnvDiagnostics)
		renderMigrationDiagnosticList(w, "hook", status.HookDiagnostics)
		renderOldSurfaceDiagnostics(w, status.OldSurfaceDiagnostics)
	}
	if !status.RepoAvailable {
		renderMigrationDiagnosticList(w, "env", status.EnvDiagnostics)
	}
	if status.RepoAvailable {
		if len(status.EnvDiagnostics) == 0 {
			fmt.Fprintln(w, "  env: ok (no legacy reporter env vars found)")
		}
		if len(status.HookDiagnostics) == 0 {
			fmt.Fprintf(w, "  hook: ok (no legacy %s hook command found)\n", migration.LegacyReporterHookName)
		}
		if len(status.OldSurfaceDiagnostics) == 0 {
			fmt.Fprintln(w, "  old local state: ok (no legacy report state keys or hook-state labels found)")
		}
	}
	if status.ScanWarning != "" {
		fmt.Fprintf(w, "  warning: %s\n", status.ScanWarning)
	}
}

func renderMigrationDiagnostics(w io.Writer, label string, present bool, configError string, diagnostics []migration.Diagnostic) {
	if configError != "" {
		fmt.Fprintf(w, "  %s: warning: %s; run: loopcoder doctor --repo .\n", label, configError)
		return
	}
	if !present {
		fmt.Fprintf(w, "  %s: .delivery.yml not found; repo config migration deferred to loopcoder doctor --repo <repo>\n", label)
		return
	}
	if len(diagnostics) == 0 {
		fmt.Fprintf(w, "  %s: ok (no legacy reporter config keys found)\n", label)
		return
	}
	renderMigrationDiagnosticList(w, label, diagnostics)
}

func renderMigrationDiagnosticList(w io.Writer, label string, diagnostics []migration.Diagnostic) {
	for _, diagnostic := range diagnostics {
		fmt.Fprintf(w, "  %s: %s\n", label, diagnostic.String())
	}
}

func renderOldSurfaceDiagnostics(w io.Writer, diagnostics []upgrade.OldSurfaceDiagnostic) {
	for _, diagnostic := range diagnostics {
		detail := strings.TrimSpace(diagnostic.Detail)
		if detail != "" {
			detail += "; "
		}
		fmt.Fprintf(w, "  old local state: legacy %s %q accepted as %q; %slocation: %s; fix: %s\n",
			diagnostic.Surface,
			diagnostic.Legacy,
			diagnostic.Current,
			detail,
			diagnostic.Location,
			diagnostic.FixCommand,
		)
	}
}

func displayUpgradeValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "(none)"
	}
	return value
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
	var configFromBase bool
	var configFromBaseAlias bool

	fs.StringVar(&repoPath, "repo", "", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")
	fs.StringVar(&baseBranch, "base-branch", lcdefaults.BaseBranch, "base branch")
	fs.StringVar(&baseBranchAlias, "BaseBranch", "", "base branch")
	fs.StringVar(&runID, "run-id", "", "run id")
	fs.StringVar(&runIDAlias, "RunId", "", "run id")
	fs.StringVar(&outputFormat, "format", "text", "output format")
	fs.StringVar(&outputFormatAlias, "Format", "", "output format")
	fs.BoolVar(&includeClosed, "include-closed", false, "include closed issues")
	fs.BoolVar(&includeClosedAlias, "IncludeClosed", false, "include closed issues")
	fs.BoolVar(&configFromBase, "config-from-base", false, "read .delivery.yml from base branch when absent from working tree")
	fs.BoolVar(&configFromBaseAlias, "ConfigFromBase", false, "read .delivery.yml from base branch when absent from working tree")

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
	configFromBase = configFromBase || configFromBaseAlias

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
	if exitCode, blocked := checkRelayGate(resolvedRepo, stdout, stderr); blocked {
		return exitCode
	}

	cfg, err := loadDeliveryConfig(resolvedRepo, baseBranch, configFromBase)
	if err != nil {
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
	var configFromBaseAlias bool
	var keepWorktreeAlias bool
	var strict bool
	var strictAlias bool
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
	fs.StringVar(&opts.BaseBranch, "base-branch", lcdefaults.BaseBranch, "base branch")
	fs.StringVar(&baseBranchAlias, "BaseBranch", "", "base branch")
	fs.StringVar(&opts.Branch, "branch", "", "branch")
	fs.StringVar(&branchAlias, "Branch", "", "branch")
	fs.StringVar(&opts.RunID, "run-id", "", "run id")
	fs.StringVar(&runIDAlias, "RunId", "", "run id")
	fs.IntVar(&opts.Attempt, "attempt", 1, "attempt")
	fs.IntVar(&attemptAlias, "Attempt", 0, "attempt")
	fs.StringVar(&opts.RecoveryContext, "recovery-context", "", "recovery context")
	fs.StringVar(&recoveryContextAlias, "RecoveryContext", "", "recovery context")
	fs.StringVar(&opts.Provider, "provider", "", "provider")
	fs.StringVar(&providerAlias, "Provider", "", "provider")
	fs.StringVar(&opts.Model, "model", "", "model")
	fs.StringVar(&modelAlias, "Model", "", "model")
	fs.StringVar(&opts.Effort, "effort", "", "effort")
	fs.StringVar(&effortAlias, "Effort", "", "effort")
	fs.BoolVar(&strict, "strict", false, "reject invalid model/depth selections instead of warning")
	fs.BoolVar(&strictAlias, "Strict", false, "reject invalid model/depth selections instead of warning")
	fs.BoolVar(&opts.ConfigFromBase, "config-from-base", false, "read .delivery.yml from base branch when absent from working tree")
	fs.BoolVar(&configFromBaseAlias, "ConfigFromBase", false, "read .delivery.yml from base branch when absent from working tree")
	fs.BoolVar(&opts.KeepWorktree, "keep-worktree", false, "keep worktree")
	fs.BoolVar(&keepWorktreeAlias, "KeepWorktree", false, "keep worktree")
	fs.BoolVar(&pretty, "pretty", false, "render human-readable report on stderr")
	fs.BoolVar(&prettyAlias, "Pretty", false, "render human-readable report on stderr")
	fs.BoolVar(&noPretty, "no-pretty", false, "suppress human-readable report on stderr")
	fs.BoolVar(&noPrettyAlias, "NoPretty", false, "suppress human-readable report on stderr")

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
	opts.ConfigFromBase = opts.ConfigFromBase || configFromBaseAlias
	opts.KeepWorktree = opts.KeepWorktree || keepWorktreeAlias
	strict = strict || strictAlias
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
	if exitCode, blocked := checkRelayGate(resolvedRepo, stdout, stderr); blocked {
		return exitCode
	}

	cfg, err := loadDeliveryConfig(resolvedRepo, opts.BaseBranch, opts.ConfigFromBase)
	if err != nil {
		fmt.Fprintf(stderr, "dispatch: %v\n", err)
		return 1
	}
	selection, ok := resolveAndValidateRoleSelection(roleSelectionInput{
		Role:           "worker",
		Provider:       opts.Provider,
		Model:          opts.Model,
		Effort:         opts.Effort,
		ConfigProvider: cfg.Adapters.Worker,
		ConfigModel:    cfg.Worker.Model,
		ConfigEffort:   cfg.Worker.ReasoningEffort,
		Strict:         cfg.Models.Strict || strict,
		Warnings:       stderr,
	})
	if !ok {
		return 1
	}
	opts.Provider = selection.Provider
	opts.Model = selection.Model
	opts.Effort = selection.Effort

	result, err := deps.Dispatch(context.Background(), opts)
	if err != nil {
		fmt.Fprintf(stderr, "dispatch: %v\n", err)
		return 1
	}
	if err := renderDispatch(stdout, result); err != nil {
		fmt.Fprintf(stderr, "dispatch: %v\n", err)
		return 1
	}
	if result.Report != nil {
		mode := prettyModeForTarget(stderr, deps, pretty)
		if err := writeDispatchRelayLedger(opts, result, *result.Report, mode, deps.Now()); err != nil {
			fmt.Fprintf(stderr, "dispatch: write relay ledger: %v\n", err)
			return 1
		}
		if shouldRenderPretty(noPretty) {
			if err := renderPrettyReportWithOptions(stderr, *result.Report, reporter.PrettyOptions{
				Mode:   mode,
				Status: result.Status,
				PR:     result.PR,
			}); err != nil {
				fmt.Fprintf(stderr, "dispatch: write pretty report: %v\n", err)
				return 1
			}
		}
	}
	return 0
}

func writeDispatchRelayLedger(opts worker.Options, result worker.Result, record reporter.Report, mode reporter.PrettyMode, now time.Time) error {
	invocationID := relayInvocationIDFromAttemptPath(result.AttemptPath)
	if invocationID == "" {
		invocationID = fmt.Sprintf("dispatch-issue-%d-%d", result.Issue, now.UTC().UnixNano())
	}
	pretty := dispatchPrettyBlock(record, result.Status, result.PR, "", mode)
	_, err := relay.Write(relay.Entry{
		RepoPath:     opts.RepoPath,
		RunID:        result.RunID,
		InvocationID: invocationID,
		Command:      "dispatch",
		Role:         record.Role,
		Issue:        result.Issue,
		PR:           result.PR,
		CreatedAt:    now,
		Header:       record.Header(),
		Pretty:       pretty,
		Report:       &record,
	})
	if err != nil {
		return err
	}
	_, err = relaygate.Write(relaygate.WriteOptions{
		RepoPath: opts.RepoPath,
		RunID:    result.RunID,
		Role:     string(record.Role),
		PRNumber: prNumberFromPR(result.PR),
		Block:    pretty,
		Report:   &record,
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

func writeLoopreviewRelayLedger(opts loopreview.Options, verdict loopreview.Verdict, mode reporter.PrettyMode, now time.Time) error {
	if verdict.Report == nil {
		return nil
	}
	record := *verdict.Report
	pretty := loopreviewPrettyBlock(verdict, mode)
	runID := fmt.Sprintf("loopreview-pr-%d", opts.PRNumber)
	_, err := relay.Write(relay.Entry{
		RepoPath:     opts.RepoPath,
		RunID:        runID,
		InvocationID: fmt.Sprintf("loopreview-pr-%d-%d", opts.PRNumber, now.UTC().UnixNano()),
		Command:      "loopreview",
		Role:         reporter.RoleVerifier,
		PRNumber:     opts.PRNumber,
		CreatedAt:    now,
		Header:       record.Header(),
		Pretty:       pretty,
		Report:       &record,
	})
	if err != nil {
		return err
	}
	_, err = relaygate.Write(relaygate.WriteOptions{
		RepoPath: opts.RepoPath,
		RunID:    runID,
		Role:     string(reporter.RoleVerifier),
		PRNumber: opts.PRNumber,
		Block:    pretty,
		Report:   &record,
	})
	return err
}

func prNumberFromPR(pr string) int {
	value := strings.TrimSpace(pr)
	if value == "" {
		return 0
	}
	if idx := strings.LastIndex(value, "/pull/"); idx >= 0 {
		value = value[idx+len("/pull/"):]
	}
	value = strings.TrimPrefix(value, "#")
	var digits strings.Builder
	for _, r := range value {
		if r < '0' || r > '9' {
			break
		}
		digits.WriteRune(r)
	}
	if digits.Len() == 0 {
		return 0
	}
	number, err := strconv.Atoi(digits.String())
	if err != nil {
		return 0
	}
	return number
}

func renderDispatch(w io.Writer, result worker.Result) error {
	if result.Report == nil {
		return errors.New("dispatch report is missing")
	}
	if err := result.Report.Validate(); err != nil {
		return fmt.Errorf("validate dispatch report: %w", err)
	}
	canonical, err := result.Report.CanonicalJSON()
	if err != nil {
		return fmt.Errorf("render dispatch report JSON: %w", err)
	}
	data, err := worker.MarshalResult(result)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, result.Report.Header()); err != nil {
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

	role := string(reporter.RoleConductor)
	var roleAlias string
	var provider string
	var providerAlias string
	var model string
	var modelAlias string
	var effort string
	var effortAlias string
	permission := string(reporter.PermissionOrchestrate)
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
	fs.BoolVar(&pretty, "pretty", false, "render human-readable report")
	fs.BoolVar(&prettyAlias, "Pretty", false, "render human-readable report")

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
		"model_source": string(reporter.ModelSourceSelfReported),
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
	var record reporter.Report
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
		if err := renderPrettyReport(stdout, record, prettyModeForTarget(stdout, deps, false)); err != nil {
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

func loadDeliveryConfig(repoPath string, baseBranch string, configFromBase bool) (config.Config, error) {
	return loadDeliveryConfigWithOptions(repoPath, config.LoadOptions{
		BaseBranch:     baseBranch,
		ConfigFromBase: configFromBase,
	})
}

func loadDeliveryConfigWithOptions(repoPath string, opts config.LoadOptions) (config.Config, error) {
	return config.LoadForRepo(context.Background(), repoPath, opts)
}

func applyRoleModelEffort(model, effort string, roleModel, roleEffort string) (string, string) {
	if strings.TrimSpace(model) == "" {
		model = strings.TrimSpace(roleModel)
	}
	if strings.TrimSpace(effort) == "" {
		effort = strings.TrimSpace(roleEffort)
	}
	return model, effort
}

type roleSelectionInput struct {
	Role           string
	Provider       string
	Model          string
	Effort         string
	ConfigProvider string
	ConfigModel    string
	ConfigEffort   string
	Strict         bool
	Warnings       io.Writer
}

type roleSelection struct {
	Provider string
	Model    string
	Effort   string
}

func resolveAndValidateRoleSelection(input roleSelectionInput) (roleSelection, bool) {
	provider := strings.TrimSpace(input.Provider)
	if provider == "" {
		provider = strings.TrimSpace(input.ConfigProvider)
	}
	if provider == "" {
		provider = defaultProviderForRole(input.Role)
	}
	model, effort := applyRoleModelEffort(
		input.Model,
		input.Effort,
		input.ConfigModel,
		input.ConfigEffort,
	)
	result := models.ValidateSelection(models.Selection{
		Role:     input.Role,
		Provider: provider,
		Model:    model,
		Depth:    effort,
	}, models.ValidationOptions{Strict: input.Strict})
	if !writeSelectionDiagnostics(input.Warnings, result.Diagnostics) {
		return roleSelection{
			Provider: result.Selection.Provider,
			Model:    result.Selection.Model,
			Effort:   result.Selection.Depth,
		}, false
	}
	return roleSelection{
		Provider: result.Selection.Provider,
		Model:    result.Selection.Model,
		Effort:   result.Selection.Depth,
	}, true
}

func writeSelectionDiagnostics(w io.Writer, diagnostics []models.Diagnostic) bool {
	if w == nil {
		w = io.Discard
	}
	ok := true
	for _, diagnostic := range diagnostics {
		fmt.Fprintln(w, diagnostic.Message)
		if diagnostic.Severity == models.SeverityReject {
			ok = false
		}
	}
	return ok
}

func defaultProviderForRole(role string) string {
	switch strings.TrimSpace(role) {
	case "verifier":
		return "claude"
	default:
		return "codex"
	}
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
	var configFromBase bool
	var configFromBaseAlias bool
	var strict bool
	var strictAlias bool
	var throttleLimit int
	var throttleLimitAlias int
	var pretty bool
	var prettyAlias bool
	var noPretty bool
	var noPrettyAlias bool

	fs.StringVar(&repoPath, "repo", "", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")
	fs.StringVar(&baseBranch, "base-branch", lcdefaults.BaseBranch, "base branch")
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
	fs.BoolVar(&strict, "strict", false, "reject invalid model/depth selections instead of warning")
	fs.BoolVar(&strictAlias, "Strict", false, "reject invalid model/depth selections instead of warning")
	fs.BoolVar(&configFromBase, "config-from-base", false, "read .delivery.yml from base branch when absent from working tree")
	fs.BoolVar(&configFromBaseAlias, "ConfigFromBase", false, "read .delivery.yml from base branch when absent from working tree")
	fs.IntVar(&throttleLimit, "throttle-limit", lcdefaults.DispatchWaveThrottleLimit, "throttle limit")
	fs.IntVar(&throttleLimitAlias, "ThrottleLimit", 0, "throttle limit")
	fs.BoolVar(&pretty, "pretty", false, "render human-readable report on stderr")
	fs.BoolVar(&prettyAlias, "Pretty", false, "render human-readable report on stderr")
	fs.BoolVar(&noPretty, "no-pretty", false, "suppress human-readable report on stderr")
	fs.BoolVar(&noPrettyAlias, "NoPretty", false, "suppress human-readable report on stderr")

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
	configFromBase = configFromBase || configFromBaseAlias
	strict = strict || strictAlias
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
	if exitCode, blocked := checkRelayGate(resolvedRepo, stdout, stderr); blocked {
		return exitCode
	}

	cfg, err := loadDeliveryConfig(resolvedRepo, baseBranch, configFromBase)
	if err != nil {
		fmt.Fprintf(stderr, "dispatch-wave: %v\n", err)
		return 1
	}
	selection, ok := resolveAndValidateRoleSelection(roleSelectionInput{
		Role:           "worker",
		Provider:       provider,
		Model:          model,
		Effort:         effort,
		ConfigProvider: cfg.Adapters.Worker,
		ConfigModel:    cfg.Worker.Model,
		ConfigEffort:   cfg.Worker.ReasoningEffort,
		Strict:         cfg.Models.Strict || strict,
		Warnings:       stderr,
	})
	if !ok {
		return 1
	}
	provider = selection.Provider
	model = selection.Model
	effort = selection.Effort

	prettyMode := prettyModeForTarget(stdout, deps, pretty)
	renderPretty := shouldRenderPretty(noPretty)
	writeWaveRelayRecord := func(runID string, result orchestration.DispatchWaveIssueResult) error {
		if result.Report == nil {
			return nil
		}
		invocationID := relayInvocationIDFromAttemptPath(result.AttemptPath)
		if invocationID == "" {
			invocationID = fmt.Sprintf("dispatch-wave-issue-%d-%d", result.Issue, deps.Now().UTC().UnixNano())
		}
		prettyBlock := dispatchPrettyBlock(*result.Report, result.Status, result.PR, result.Error, prettyMode)
		if _, err := relay.Write(relay.Entry{
			RepoPath:     resolvedRepo,
			RunID:        runID,
			InvocationID: invocationID,
			Command:      "dispatch-wave",
			Role:         result.Report.Role,
			Issue:        result.Issue,
			PR:           result.PR,
			CreatedAt:    deps.Now(),
			Header:       result.Report.Header(),
			Pretty:       prettyBlock,
		}); err != nil {
			return err
		}
		prNumber := prNumberFromPR(result.PR)
		if prNumber == 0 {
			prNumber = result.Issue
		}
		_, err := relaygate.Write(relaygate.WriteOptions{
			RepoPath: resolvedRepo,
			RunID:    runID,
			Role:     string(result.Report.Role),
			PRNumber: prNumber,
			Block:    prettyBlock,
		})
		return err
	}
	streamWaveCompletion := func(completion orchestration.DispatchWaveIssueComplete) error {
		result := completion.Result
		if result.Report == nil {
			return nil
		}
		if err := writeWaveRelayRecord(completion.RunID, result); err != nil {
			return fmt.Errorf("write relay record for worker #%d: %w", result.Issue, err)
		}
		if !renderPretty {
			return nil
		}
		prettyBlock := dispatchPrettyBlock(*result.Report, result.Status, result.PR, result.Error, prettyMode)
		text := orchestration.RenderDispatchWaveIssueCompletion(result, prettyBlock)
		if _, err := stdout.Write([]byte(text)); err != nil {
			return fmt.Errorf("write worker #%d completion: %w", result.Issue, err)
		}
		return nil
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
		ConfigFromBase:  configFromBase,
		ThrottleLimit:   throttleLimit,
		Thresholds:      cfg.Resilience.Worker,
		Budget:          cfg.Guardrails.Budget,
		CircuitBreaker:  cfg.Guardrails.CircuitBreaker,
		ProcessAlive:    deps.ProcessAlive,
		Now:             deps.Now(),
		Stderr:          stderr,
		ComputeReadySet: deps.ComputeReadySet,
		Dispatch:        deps.Dispatch,
		OnIssueComplete: streamWaveCompletion,
	})
	if dispatchWaveReportHasContent(waveReport) {
		if _, writeErr := stdout.Write([]byte(orchestration.RenderDispatchWaveText(waveReport))); writeErr != nil {
			fmt.Fprintf(stderr, "dispatch-wave: write output: %v\n", writeErr)
			return 1
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "dispatch-wave: %v\n", err)
		if dispatchWaveReportHasContent(waveReport) {
			fmt.Fprintf(stderr, "dispatch-wave: pending relay records may remain; run `loopcoder relay list --repo %s` or `loopcoder relay flush --repo %s`.\n", resolvedRepo, resolvedRepo)
		}
		return 1
	}
	if orchestration.DispatchWaveHasFailures(waveReport) {
		return 1
	}
	return 0
}

func dispatchWaveReportHasContent(report orchestration.DispatchWaveReport) bool {
	return strings.TrimSpace(report.RunID) != "" ||
		len(report.IssuesRequested) > 0 ||
		len(report.Results) > 0
}

func runRecover(args []string, stdout, stderr io.Writer, deps Deps) int {
	if deps.Dispatch == nil {
		deps.Dispatch = DefaultDeps().Dispatch
	}
	if deps.Loopreview == nil {
		deps.Loopreview = DefaultDeps().Loopreview
	}
	if deps.Recover == nil {
		deps.Recover = recoverWithDispatch(deps.Dispatch, deps.Loopreview)
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
	var configFromBaseAlias bool
	var upgradedModelAlias string
	var upgradedEffortAlias string
	var verifierProviderAlias string
	var verifierModelAlias string
	var verifierEffortAlias string
	var verifierTimeoutAlias time.Duration
	var strict bool
	var strictAlias bool

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
	fs.StringVar(&opts.BaseBranch, "base-branch", lcdefaults.BaseBranch, "base branch")
	fs.StringVar(&baseBranchAlias, "BaseBranch", "", "base branch")
	fs.IntVar(&opts.MaxAttempts, "max-attempts", lcdefaults.WorkerMaxAttempts, "max attempts")
	fs.IntVar(&maxAttemptsAlias, "MaxAttempts", 0, "max attempts")
	fs.StringVar(&backoffSecondsValue, "backoff-seconds", csvInts(lcdefaults.WorkerRetryBackoffSeconds()), "backoff seconds")
	fs.StringVar(&backoffSecondsAlias, "BackoffSeconds", "", "backoff seconds")
	fs.StringVar(&opts.Provider, "provider", "", "provider")
	fs.StringVar(&providerAlias, "Provider", "", "provider")
	fs.StringVar(&opts.Model, "model", "", "model")
	fs.StringVar(&modelAlias, "Model", "", "model")
	fs.StringVar(&opts.Effort, "effort", "", "effort")
	fs.StringVar(&effortAlias, "Effort", "", "effort")
	fs.BoolVar(&strict, "strict", false, "reject invalid model/depth selections instead of warning")
	fs.BoolVar(&strictAlias, "Strict", false, "reject invalid model/depth selections instead of warning")
	fs.BoolVar(&opts.ConfigFromBase, "config-from-base", false, "read .delivery.yml from base branch when absent from working tree")
	fs.BoolVar(&configFromBaseAlias, "ConfigFromBase", false, "read .delivery.yml from base branch when absent from working tree")
	fs.StringVar(&opts.UpgradedModel, "upgraded-model", "", "upgraded model")
	fs.StringVar(&upgradedModelAlias, "UpgradedModel", "", "upgraded model")
	fs.StringVar(&opts.UpgradedEffort, "upgraded-effort", "", "upgraded effort")
	fs.StringVar(&upgradedEffortAlias, "UpgradedEffort", "", "upgraded effort")
	fs.StringVar(&opts.VerifierProvider, "verifier-provider", "", "verifier provider")
	fs.StringVar(&verifierProviderAlias, "VerifierProvider", "", "verifier provider")
	fs.StringVar(&opts.VerifierModel, "verifier-model", "", "verifier model")
	fs.StringVar(&verifierModelAlias, "VerifierModel", "", "verifier model")
	fs.StringVar(&opts.VerifierEffort, "verifier-effort", "", "verifier effort")
	fs.StringVar(&verifierEffortAlias, "VerifierEffort", "", "verifier effort")
	fs.DurationVar(&opts.VerifierTimeout, "verifier-timeout", loopreview.DefaultVerifierTimeout, "verifier timeout")
	fs.DurationVar(&verifierTimeoutAlias, "VerifierTimeout", 0, "verifier timeout")

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
	opts.ConfigFromBase = opts.ConfigFromBase || configFromBaseAlias
	if upgradedModelAlias != "" {
		opts.UpgradedModel = upgradedModelAlias
	}
	if upgradedEffortAlias != "" {
		opts.UpgradedEffort = upgradedEffortAlias
	}
	if verifierProviderAlias != "" {
		opts.VerifierProvider = verifierProviderAlias
	}
	if verifierModelAlias != "" {
		opts.VerifierModel = verifierModelAlias
	}
	if verifierEffortAlias != "" {
		opts.VerifierEffort = verifierEffortAlias
	}
	if verifierTimeoutAlias != 0 {
		opts.VerifierTimeout = verifierTimeoutAlias
	}
	strict = strict || strictAlias
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
	if exitCode, blocked := checkRelayGate(resolvedRepo, stdout, stderr); blocked {
		return exitCode
	}

	cfg, err := loadDeliveryConfig(resolvedRepo, opts.BaseBranch, opts.ConfigFromBase)
	if err != nil {
		fmt.Fprintf(stderr, "recover: %v\n", err)
		return 1
	}
	workerSelection, ok := resolveAndValidateRoleSelection(roleSelectionInput{
		Role:           "worker",
		Provider:       opts.Provider,
		Model:          opts.Model,
		Effort:         opts.Effort,
		ConfigProvider: cfg.Adapters.Worker,
		ConfigModel:    cfg.Worker.Model,
		ConfigEffort:   cfg.Worker.ReasoningEffort,
		Strict:         cfg.Models.Strict || strict,
		Warnings:       stderr,
	})
	if !ok {
		return 1
	}
	opts.Provider = workerSelection.Provider
	opts.Model = workerSelection.Model
	opts.Effort = workerSelection.Effort
	verifierSelection, ok := resolveAndValidateRoleSelection(roleSelectionInput{
		Role:           "verifier",
		Provider:       opts.VerifierProvider,
		Model:          opts.VerifierModel,
		Effort:         opts.VerifierEffort,
		ConfigProvider: cfg.Adapters.Verifier,
		ConfigModel:    cfg.Verifier.Model,
		ConfigEffort:   cfg.Verifier.ReasoningEffort,
		Strict:         cfg.Models.Strict || strict,
		Warnings:       stderr,
	})
	if !ok {
		return 1
	}
	opts.VerifierProvider = verifierSelection.Provider
	opts.VerifierModel = verifierSelection.Model
	opts.VerifierEffort = verifierSelection.Effort
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
	var configFromBaseAlias bool
	var timeoutAlias time.Duration
	var strict bool
	var strictAlias bool
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
	fs.BoolVar(&strict, "strict", false, "reject invalid model/depth selections instead of warning")
	fs.BoolVar(&strictAlias, "Strict", false, "reject invalid model/depth selections instead of warning")
	fs.StringVar(&opts.BaseBranch, "base-branch", lcdefaults.BaseBranch, "base branch")
	fs.StringVar(&baseBranchAlias, "BaseBranch", "", "base branch")
	fs.BoolVar(&opts.ConfigFromBase, "config-from-base", false, "read .delivery.yml from base branch when absent from working tree")
	fs.BoolVar(&configFromBaseAlias, "ConfigFromBase", false, "read .delivery.yml from base branch when absent from working tree")
	fs.DurationVar(&opts.Timeout, "timeout", loopreview.DefaultVerifierTimeout, "verifier timeout")
	fs.DurationVar(&timeoutAlias, "Timeout", 0, "verifier timeout")
	fs.BoolVar(&pretty, "pretty", false, "render human-readable report on stderr")
	fs.BoolVar(&prettyAlias, "Pretty", false, "render human-readable report on stderr")
	fs.BoolVar(&noPretty, "no-pretty", false, "suppress human-readable report on stderr")
	fs.BoolVar(&noPrettyAlias, "NoPretty", false, "suppress human-readable report on stderr")

	if err := fs.Parse(args); err != nil {
		return loopreviewCommandFailureExitCode
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
	if modelAlias != "" {
		opts.Model = modelAlias
	}
	if effortAlias != "" {
		opts.Effort = effortAlias
	}
	if baseBranchAlias != "" {
		opts.BaseBranch = baseBranchAlias
	}
	opts.ConfigFromBase = opts.ConfigFromBase || configFromBaseAlias
	if timeoutAlias != 0 {
		opts.Timeout = timeoutAlias
	}
	strict = strict || strictAlias
	pretty = pretty || prettyAlias
	noPretty = noPretty || noPrettyAlias

	if strings.TrimSpace(opts.RepoPath) == "" {
		fmt.Fprintln(stderr, "loopreview: --repo is required")
		return loopreviewCommandFailureExitCode
	}
	if opts.PRNumber <= 0 {
		fmt.Fprintln(stderr, "loopreview: --pr-number is required")
		return loopreviewCommandFailureExitCode
	}
	if opts.Timeout <= 0 {
		fmt.Fprintln(stderr, "loopreview: --timeout must be positive")
		return loopreviewCommandFailureExitCode
	}

	resolvedRepo, err := resolveRepo(opts.RepoPath)
	if err != nil {
		fmt.Fprintf(stderr, "loopreview: %v\n", err)
		return loopreviewCommandFailureExitCode
	}
	opts.RepoPath = resolvedRepo
	opts.Stderr = stderr
	if exitCode, blocked := checkRelayGate(resolvedRepo, stdout, stderr); blocked {
		return exitCode
	}

	cfg, err := loadDeliveryConfig(resolvedRepo, opts.BaseBranch, opts.ConfigFromBase)
	if err != nil {
		fmt.Fprintf(stderr, "loopreview: %v\n", err)
		return loopreviewCommandFailureExitCode
	}
	selection, ok := resolveAndValidateRoleSelection(roleSelectionInput{
		Role:           "verifier",
		Provider:       opts.Provider,
		Model:          opts.Model,
		Effort:         opts.Effort,
		ConfigProvider: cfg.Adapters.Verifier,
		ConfigModel:    cfg.Verifier.Model,
		ConfigEffort:   cfg.Verifier.ReasoningEffort,
		Strict:         cfg.Models.Strict || strict,
		Warnings:       stderr,
	})
	if !ok {
		return loopreviewCommandFailureExitCode
	}
	opts.Provider = selection.Provider
	opts.Model = selection.Model
	opts.Effort = selection.Effort
	workerProvider := strings.TrimSpace(cfg.Adapters.Worker)
	if warning := config.ReviewerNotWorkerWarning(config.Adapters{
		Worker:   workerProvider,
		Verifier: opts.Provider,
	}); warning != "" {
		fmt.Fprintf(stderr, "[loopcoder] warning: %s\n", warning)
	}

	result, err := deps.Loopreview(context.Background(), opts)
	if err != nil {
		fmt.Fprintf(stderr, "loopreview: %v\n", err)
		return loopreviewCommandFailureExitCode
	}
	if err := loopreview.Render(stdout, result); err != nil {
		fmt.Fprintf(stderr, "loopreview: write output: %v\n", err)
		return loopreviewCommandFailureExitCode
	}
	if result.Verdict.Report != nil {
		mode := prettyModeForTarget(stderr, deps, pretty)
		if err := writeLoopreviewRelayLedger(opts, result.Verdict, mode, deps.Now()); err != nil {
			fmt.Fprintf(stderr, "loopreview: write relay ledger: %v\n", err)
			return loopreviewCommandFailureExitCode
		}
		if shouldRenderPretty(noPretty) {
			if err := renderLoopreviewPrettyReport(stderr, result.Verdict, mode); err != nil {
				fmt.Fprintf(stderr, "loopreview: write pretty report: %v\n", err)
				return loopreviewCommandFailureExitCode
			}
		}
	}
	return loopreview.ExitCodeForVerdict(result.Verdict.Verdict)
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
	fs.StringVar(&opts.BaseBranch, "base-branch", lcdefaults.BaseBranch, "base branch")
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

	resolvedRepo, err := resolveRepo(opts.RepoPath)
	if err != nil {
		fmt.Fprintf(stderr, "verify-local: %v\n", err)
		return 2
	}
	opts.RepoPath = resolvedRepo
	if exitCode, blocked := checkRelayGate(resolvedRepo, stdout, stderr); blocked {
		return exitCode
	}

	result := deps.Verify(context.Background(), opts)
	if err := verify.Render(stdout, result); err != nil {
		fmt.Fprintf(stderr, "verify-local: write output: %v\n", err)
		return 1
	}
	return result.ExitCode
}

func recoverWithDispatch(dispatch func(ctx context.Context, opts worker.Options) (worker.Result, error), review func(ctx context.Context, opts loopreview.Options) (loopreview.Result, error)) func(context.Context, recovery.Options) (recovery.Result, error) {
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
				ConfigFromBase:  dispatchOpts.ConfigFromBase,
				Stderr:          dispatchOpts.Stderr,
			})
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
				Report:      result.Report,
			}, err
		}
		recoverDeps.Review = review
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

func csvInts(values []int) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, strconv.Itoa(value))
	}
	return strings.Join(parts, ",")
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
	format := "text"
	var formatAlias string

	fs.StringVar(&repoPath, "repo", ".", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")
	fs.StringVar(&runID, "run", "", "run id")
	fs.StringVar(&runIDAlias, "Run", "", "run id")
	fs.StringVar(&runIDAlias, "run-id", "", "run id")
	fs.StringVar(&runIDAlias, "RunId", "", "run id")
	fs.StringVar(&format, "format", "text", "output format")
	fs.StringVar(&formatAlias, "Format", "", "output format")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if repoAlias != "" {
		repoPath = repoAlias
	}
	if runIDAlias != "" {
		runID = runIDAlias
	}
	if formatAlias != "" {
		format = formatAlias
	}
	switch format {
	case "text", "json":
	default:
		fmt.Fprintf(stderr, "status: invalid --format %q; want text or json\n", format)
		return 2
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
	if format == "json" {
		data, err := runstatus.MarshalJSON(report)
		if err != nil {
			fmt.Fprintf(stderr, "status: %v\n", err)
			return 1
		}
		if _, err := stdout.Write(data); err != nil {
			fmt.Fprintf(stderr, "status: write output: %v\n", err)
			return 1
		}
		return 0
	}
	if _, err := stdout.Write([]byte(runstatus.Render(report))); err != nil {
		fmt.Fprintf(stderr, "status: write output: %v\n", err)
		return 1
	}
	return 0
}

func runReport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(stderr)

	repoPath := "."
	var repoAlias string
	var workID string
	var workIDAlias string
	var runID string
	var runIDAlias string
	var issue int
	var issueAlias int
	var role string
	var roleAlias string
	limit := reportquery.DefaultLimit
	var limitAlias int
	format := "text"
	var formatAlias string
	var verbose bool
	var verboseAlias bool

	fs.StringVar(&repoPath, "repo", ".", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")
	fs.StringVar(&workID, "work-id", "", "work id")
	fs.StringVar(&workIDAlias, "WorkId", "", "work id")
	fs.StringVar(&runID, "run", "", "run id")
	fs.StringVar(&runIDAlias, "Run", "", "run id")
	fs.StringVar(&runIDAlias, "run-id", "", "run id")
	fs.StringVar(&runIDAlias, "RunId", "", "run id")
	fs.IntVar(&issue, "issue", 0, "issue number")
	fs.IntVar(&issueAlias, "Issue", 0, "issue number")
	fs.StringVar(&role, "role", "", "role")
	fs.StringVar(&roleAlias, "Role", "", "role")
	fs.IntVar(&limit, "limit", reportquery.DefaultLimit, "limit")
	fs.IntVar(&limitAlias, "Limit", 0, "limit")
	fs.StringVar(&format, "format", "text", "output format")
	fs.StringVar(&formatAlias, "Format", "", "output format")
	fs.BoolVar(&verbose, "verbose", false, "include raw canonical records in text output")
	fs.BoolVar(&verboseAlias, "Verbose", false, "include raw canonical records in text output")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if repoAlias != "" {
		repoPath = repoAlias
	}
	if workIDAlias != "" {
		workID = workIDAlias
	}
	if runIDAlias != "" {
		runID = runIDAlias
	}
	if strings.TrimSpace(runID) != "" && strings.TrimSpace(workID) == "" {
		workID = runID
	}
	if issueAlias != 0 {
		issue = issueAlias
	}
	if roleAlias != "" {
		role = roleAlias
	}
	if limitAlias != 0 {
		limit = limitAlias
	}
	if formatAlias != "" {
		format = formatAlias
	}
	verbose = verbose || verboseAlias
	switch format {
	case "text", "json":
	default:
		fmt.Fprintf(stderr, "report: invalid --format %q; want text or json\n", format)
		return 2
	}
	reportRole := reporter.Role(strings.TrimSpace(role))
	if reportRole != "" && !validReportRole(reportRole) {
		fmt.Fprintf(stderr, "report: invalid --role %q; want worker, verifier, or conductor\n", role)
		return 2
	}
	if issue < 0 {
		fmt.Fprintln(stderr, "report: --issue must be non-negative")
		return 2
	}
	if limit <= 0 {
		fmt.Fprintln(stderr, "report: --limit must be positive")
		return 2
	}

	resolvedRepo, err := resolveRepo(repoPath)
	if err != nil {
		fmt.Fprintf(stderr, "report: %v\n", err)
		return 2
	}
	records, err := reportquery.List(reportquery.Options{
		RepoPath: resolvedRepo,
		WorkID:   workID,
		Issue:    issue,
		Role:     reportRole,
		Limit:    limit,
	})
	if err != nil {
		fmt.Fprintf(stderr, "report: %v\n", err)
		return 1
	}
	if format == "json" {
		var runTree any
		if strings.TrimSpace(runID) != "" {
			statusReport, err := runstatus.Load(runstatus.Options{
				RepoPath: resolvedRepo,
				RunID:    runID,
			})
			if err != nil {
				fmt.Fprintf(stderr, "report: %v\n", err)
				return 1
			}
			runTree = statusReport.RunTree
		}
		data, err := reportquery.MarshalJSONWithRunTree(records, runTree)
		if err != nil {
			fmt.Fprintf(stderr, "report: %v\n", err)
			return 1
		}
		if _, err := stdout.Write(append(data, '\n')); err != nil {
			fmt.Fprintf(stderr, "report: write output: %v\n", err)
			return 1
		}
		return 0
	}
	if _, err := stdout.Write([]byte(reportquery.RenderTextWithOptions(records, reportquery.RenderOptions{Verbose: verbose}))); err != nil {
		fmt.Fprintf(stderr, "report: write output: %v\n", err)
		return 1
	}
	return 0
}

func validReportRole(role reporter.Role) bool {
	switch role {
	case reporter.RoleWorker, reporter.RoleVerifier, reporter.RoleConductor:
		return true
	default:
		return false
	}
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
	var outputFormat string
	var outputFormatAlias string
	var configFromBase bool
	var configFromBaseAlias bool

	fs.StringVar(&repoPath, "repo", "", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")
	fs.StringVar(&baseBranch, "base-branch", lcdefaults.BaseBranch, "base branch")
	fs.StringVar(&baseBranchAlias, "BaseBranch", "", "base branch")
	fs.StringVar(&runID, "run-id", "", "run id")
	fs.StringVar(&runIDAlias, "RunId", "", "run id")
	fs.StringVar(&outputFormat, "format", "text", "output format")
	fs.StringVar(&outputFormatAlias, "Format", "", "output format")
	fs.BoolVar(&configFromBase, "config-from-base", false, "read .delivery.yml from base branch when absent from working tree")
	fs.BoolVar(&configFromBaseAlias, "ConfigFromBase", false, "read .delivery.yml from base branch when absent from working tree")

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
	configFromBase = configFromBase || configFromBaseAlias

	if strings.TrimSpace(repoPath) == "" {
		fmt.Fprintln(stderr, "resume: --repo is required")
		return 2
	}
	switch outputFormat {
	case "text", "json", "both":
	default:
		fmt.Fprintf(stderr, "resume: invalid --format %q; want text, json, or both\n", outputFormat)
		return 2
	}

	resolvedRepo, err := resolveRepo(repoPath)
	if err != nil {
		fmt.Fprintf(stderr, "resume: %v\n", err)
		return 2
	}

	cfg, err := loadDeliveryConfig(resolvedRepo, baseBranch, configFromBase)
	if err != nil {
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

	if outputFormat == "text" || outputFormat == "both" {
		fmt.Fprint(stdout, report.RenderResumeText(resumeReport))
	}
	if outputFormat == "both" {
		fmt.Fprintln(stdout)
	}
	if outputFormat == "json" || outputFormat == "both" {
		data, err := report.MarshalResumeJSON(resumeReport)
		if err != nil {
			fmt.Fprintf(stderr, "resume: %v\n", err)
			return 1
		}
		if _, err := stdout.Write(data); err != nil {
			fmt.Fprintf(stderr, "resume: write output: %v\n", err)
			return 1
		}
	}
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

func checkRelayGate(repoPath string, stdout, stderr io.Writer) (int, bool) {
	records, err := relaygate.CheckWithError(repoPath)
	if err != nil {
		fmt.Fprintf(stderr, "relay gate: could not read pending records: %v; proceeding\n", err)
		return 0, false
	}
	if len(records) == 0 {
		return 0, false
	}
	if err := renderRelayGate(stdout, repoPath, records); err != nil {
		fmt.Fprintf(stderr, "relay gate: write output: %v\n", err)
		return 1, true
	}
	return relayGateExitCode, true
}

func renderRelayGate(w io.Writer, repoPath string, records []relaygate.Record) error {
	if _, err := fmt.Fprintln(w, "loopcoder relay gate: pending local-only Worker/Verifier report block(s) must be relayed before this command can run."); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Run `loopcoder relay flush --repo %s` to print and acknowledge the pending block(s).\n\n", repoPath); err != nil {
		return err
	}
	for _, rec := range records {
		if _, err := io.WriteString(w, rec.Block); err != nil {
			return err
		}
	}
	return nil
}
