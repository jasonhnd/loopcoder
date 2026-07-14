package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/detachedrun"
	"github.com/jasonhnd/loopcoder/internal/process"
	"github.com/jasonhnd/loopcoder/internal/progress"
	"github.com/jasonhnd/loopcoder/internal/runtimepath"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/worker"
)

type detachedLaunchRecord struct {
	SchemaVersion string `json:"schema_version"`
	Detached      bool   `json:"detached"`
	RunID         string `json:"run_id"`
	ProjectID     string `json:"project_id"`
	Owner         string `json:"supervisor_owner"`
	Generation    int64  `json:"supervisor_generation"`
	LaunchPhase   string `json:"launch_phase"`
	Status        string `json:"status"`
	PID           int    `json:"pid,omitempty"`
	StatusCommand string `json:"status_command"`
	AttachCommand string `json:"attach_command"`
}

func runDetachedDispatch(opts worker.Options, format string, stdout, stderr io.Writer, deps Deps) int {
	ctx := context.Background()
	now := normalizedDepsNow(deps)().UTC()
	store, roots, err := openDetachedStore(ctx, opts.RepoPath, deps)
	if err != nil {
		fmt.Fprintf(stderr, "dispatch: %v\n", err)
		return 1
	}
	defer store.Close()

	owner := detachedOwner(opts.RunID, now)
	claim, err := detachedrun.Claim(ctx, store, detachedrun.ClaimRequest{
		ProjectID:           roots.ProjectID,
		RunID:               opts.RunID,
		Owner:               owner,
		LeaseExpiresAt:      now.Add(detachedLeaseDuration(opts)),
		IssueNumber:         opts.IssueNumber,
		Attempt:             opts.Attempt,
		BaseBranch:          opts.BaseBranch,
		Branch:              opts.Branch,
		Provider:            opts.Provider,
		Model:               opts.Model,
		Effort:              opts.Effort,
		DeliverySinks:       []string{"progress_receipts", "progress_delivery_outbox", "local_relay_ledger"},
		CancellationChannel: "detached-run:" + opts.RunID,
		WorkerLease: map[string]any{
			"authority": "agent_ownership_locks",
			"scope":     "worker dispatch",
		},
		RecoveryEvidence: []detachedrun.Evidence{{
			Kind:       "durable-claim",
			ID:         opts.RunID,
			Summary:    "detached supervisor claim persisted before launch success",
			Confidence: "exact",
		}},
		Payload: map[string]any{
			"issue_number": opts.IssueNumber,
			"attempt":      opts.Attempt,
			"provider":     opts.Provider,
		},
		Now: now,
	})
	if err != nil {
		fmt.Fprintf(stderr, "dispatch: claim detached supervisor: %v\n", err)
		return 1
	}

	logPath := filepath.Join(roots.LogsRoot, opts.RunID, "detached-supervisor.log")
	args := detachedDispatchArgs(opts, claim.Fence())
	start := deps.StartDetachedDispatch
	if start == nil {
		start = startDetachedDispatchProcess
	}
	pid, err := start(ctx, args, logPath)
	if err != nil {
		_, _ = detachedrun.Complete(ctx, store, claim.Fence(), detachedrun.StatusFailed, "", "spawn-failed", err.Error(), now)
		fmt.Fprintf(stderr, "dispatch: start detached supervisor: %v\n", err)
		return 1
	}
	spawned, err := detachedrun.MarkSpawned(ctx, store, claim.Fence(), pid, "process-tree", normalizedDepsNow(deps)().UTC())
	if err != nil {
		fmt.Fprintf(stderr, "dispatch: persist detached spawn: %v\n", err)
		return 1
	}
	record := detachedLaunchRecord{
		SchemaVersion: "loopcoder.detached_launch.v1",
		Detached:      true,
		RunID:         spawned.RunID,
		ProjectID:     spawned.ProjectID,
		Owner:         spawned.Owner,
		Generation:    spawned.Generation,
		LaunchPhase:   spawned.LaunchPhase,
		Status:        spawned.Status,
		PID:           spawned.ProcessPID,
		StatusCommand: fmt.Sprintf("loopcoder status --repo %s --run %s --receipts", shellQuote(opts.RepoPath), spawned.RunID),
		AttachCommand: fmt.Sprintf("loopcoder attach --repo %s --run %s", shellQuote(opts.RepoPath), spawned.RunID),
	}
	if format == "json" {
		if err := writeJSONLine(stdout, record); err != nil {
			fmt.Fprintf(stderr, "dispatch: write detached output: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "detached run: %s\n", record.RunID)
	fmt.Fprintf(stdout, "supervisor: %s generation %d pid %d\n", record.Owner, record.Generation, record.PID)
	fmt.Fprintf(stdout, "status: %s\n", record.StatusCommand)
	fmt.Fprintf(stdout, "attach: %s\n", record.AttachCommand)
	return 0
}

func runDispatchSupervisor(opts worker.Options, fence detachedrun.Fence, deps Deps) int {
	ctx := context.Background()
	now := normalizedDepsNow(deps)
	store, _, err := openDetachedStore(ctx, opts.RepoPath, deps)
	if err != nil {
		return 1
	}
	defer store.Close()
	if _, err := detachedrun.MarkWorkerStarted(ctx, store, fence, now().UTC()); err != nil {
		return 1
	}
	opts.Stderr = io.Discard
	result, dispatchErr := deps.Dispatch(ctx, opts)
	status := strings.TrimSpace(result.Status)
	if status == "" {
		if dispatchErr != nil {
			status = detachedrun.StatusFailed
		} else {
			status = detachedrun.StatusSucceeded
		}
	}
	errorCode := ""
	errorMessage := ""
	if dispatchErr != nil {
		errorCode = "dispatch-error"
		errorMessage = dispatchErr.Error()
	}
	_, completeErr := detachedrun.Complete(ctx, store, fence, normalizeDetachedTerminalStatus(status), "", errorCode, errorMessage, now().UTC())
	if dispatchErr != nil || completeErr != nil {
		return 1
	}
	return dispatchResultExitCode(result)
}

func runCancel(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("cancel", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repoPath := "."
	var repoAlias string
	var runID string
	var runIDAlias string
	format := "text"
	fs.StringVar(&repoPath, "repo", ".", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")
	fs.StringVar(&runID, "run", "", "run id")
	fs.StringVar(&runIDAlias, "run-id", "", "run id")
	fs.StringVar(&runIDAlias, "RunId", "", "run id")
	fs.StringVar(&format, "format", "text", "output format")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if repoAlias != "" {
		repoPath = repoAlias
	}
	if runIDAlias != "" {
		runID = runIDAlias
	}
	if strings.TrimSpace(runID) == "" {
		fmt.Fprintln(stderr, "cancel: --run is required")
		return 2
	}
	resolvedRepo, err := resolveRepo(repoPath)
	if err != nil {
		fmt.Fprintf(stderr, "cancel: %v\n", err)
		return 2
	}
	store, _, err := openDetachedStore(context.Background(), resolvedRepo, deps)
	if err != nil {
		fmt.Fprintf(stderr, "cancel: %v\n", err)
		return 1
	}
	defer store.Close()
	now := normalizedDepsNow(deps)().UTC()
	record, err := detachedrun.RequestCancel(context.Background(), store, runID, now)
	if err != nil {
		fmt.Fprintf(stderr, "cancel: %v\n", err)
		return 1
	}
	if record.ProcessPID > 0 && !detachedTerminal(record.Status) {
		kill := deps.KillProcessTree
		if kill == nil {
			kill = process.KillTree
		}
		if err := kill(record.ProcessPID); err != nil {
			fmt.Fprintf(stderr, "cancel: kill process tree %d: %v\n", record.ProcessPID, err)
			return 1
		}
		record, _ = detachedrun.Complete(context.Background(), store, record.Fence(), detachedrun.StatusCancelled, "", "cancelled", "detached run cancellation requested", now)
	}
	if format == "json" {
		if err := writeJSONLine(stdout, record); err != nil {
			fmt.Fprintf(stderr, "cancel: write output: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "cancelled detached run %s status=%s pid=%d\n", record.RunID, record.Status, record.ProcessPID)
	return 0
}

func runAttach(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repoPath := "."
	var repoAlias string
	var runID string
	var runIDAlias string
	format := "text"
	var cursor string
	var pollInterval time.Duration
	var followFor time.Duration
	fs.StringVar(&repoPath, "repo", ".", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")
	fs.StringVar(&runID, "run", "", "run id")
	fs.StringVar(&runIDAlias, "run-id", "", "run id")
	fs.StringVar(&runIDAlias, "RunId", "", "run id")
	fs.StringVar(&format, "format", "text", "output format")
	fs.StringVar(&cursor, "cursor", "", "opaque progress receipt cursor")
	fs.DurationVar(&pollInterval, "poll", progress.DefaultFollowPollInterval, "follow poll interval")
	fs.DurationVar(&followFor, "follow-for", 0, "optional bounded follow duration")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if repoAlias != "" {
		repoPath = repoAlias
	}
	if runIDAlias != "" {
		runID = runIDAlias
	}
	if strings.TrimSpace(runID) == "" {
		fmt.Fprintln(stderr, "attach: --run is required")
		return 2
	}
	if format != "text" && format != "jsonl" {
		fmt.Fprintf(stderr, "attach: invalid --format %q; want text or jsonl\n", format)
		return 2
	}
	resolvedRepo, err := resolveRepo(repoPath)
	if err != nil {
		fmt.Fprintf(stderr, "attach: %v\n", err)
		return 2
	}
	return runStatusProgressReceipts(statusProgressOptions{
		RepoPath:     resolvedRepo,
		RunID:        runID,
		Format:       format,
		Follow:       true,
		Cursor:       progress.Cursor(cursor),
		Limit:        500,
		PollInterval: pollInterval,
		FollowFor:    followFor,
	}, stdout, stderr, deps)
}

func runDetachedRecover(repoPath, runID, format string, stdout, stderr io.Writer, deps Deps) int {
	if format != "text" && format != "json" {
		fmt.Fprintf(stderr, "recover: invalid --format %q; want text or json\n", format)
		return 2
	}
	store, _, err := openDetachedStore(context.Background(), repoPath, deps)
	if err != nil {
		fmt.Fprintf(stderr, "recover: %v\n", err)
		return 1
	}
	defer store.Close()
	result, err := detachedrun.Reconcile(context.Background(), store, runID, normalizedDepsNow(deps)().UTC())
	if err != nil {
		fmt.Fprintf(stderr, "recover: %v\n", err)
		return 1
	}
	if format == "json" {
		if err := writeJSONLine(stdout, result); err != nil {
			fmt.Fprintf(stderr, "recover: write output: %v\n", err)
			return 1
		}
	} else {
		fmt.Fprintf(stdout, "detached run: %s\n", result.Record.RunID)
		fmt.Fprintf(stdout, "state: %s phase=%s action=%s\n", result.Record.Status, result.Record.LaunchPhase, result.ReplayAction)
		if result.Reason != "" {
			fmt.Fprintf(stdout, "reason: %s\n", result.Reason)
		}
	}
	if result.NeedsHuman {
		return 2
	}
	return 0
}

func openDetachedStore(ctx context.Context, repoPath string, deps Deps) (storage.Store, runtimepath.Roots, error) {
	roots, err := runtimepath.Resolve(ctx, repoPath)
	if err != nil {
		return nil, runtimepath.Roots{}, err
	}
	if !roots.Registered || strings.TrimSpace(roots.ProjectID) == "" {
		return nil, roots, fmt.Errorf("%w: detached runs require a registered project", detachedrun.ErrUnsupported)
	}
	store, err := storage.Open(ctx, storage.Options{Path: roots.DatabasePath, Now: normalizedDepsNow(deps)})
	if err != nil {
		return nil, roots, err
	}
	return store, roots, nil
}

func detachedDispatchArgs(opts worker.Options, fence detachedrun.Fence) []string {
	args := []string{
		"dispatch",
		"--repo", opts.RepoPath,
		"--issue-number", fmt.Sprint(opts.IssueNumber),
		"--issue-title", opts.IssueTitle,
		"--base-branch", opts.BaseBranch,
		"--branch", opts.Branch,
		"--run-id", opts.RunID,
		"--attempt", fmt.Sprint(opts.Attempt),
		"--provider", opts.Provider,
		"--format", "json",
		"--supervisor-run",
		"--supervisor-owner", fence.Owner,
		"--supervisor-generation", fmt.Sprint(fence.Generation),
	}
	if strings.TrimSpace(opts.IssueBody) != "" {
		args = append(args, "--issue-body", opts.IssueBody)
	}
	if strings.TrimSpace(opts.Model) != "" {
		args = append(args, "--model", opts.Model)
	}
	if strings.TrimSpace(opts.Effort) != "" {
		args = append(args, "--effort", opts.Effort)
	}
	if opts.Timeout > 0 {
		args = append(args, "--timeout", opts.Timeout.String())
	}
	if opts.ConfigFromBase {
		args = append(args, "--config-from-base")
	}
	if opts.KeepWorktree {
		args = append(args, "--keep-worktree")
	}
	return args
}

func detachedOwner(runID string, now time.Time) string {
	return "detached-supervisor-" + strings.TrimSpace(runID) + "-" + fmt.Sprint(now.UTC().UnixNano())
}

func detachedLeaseDuration(opts worker.Options) time.Duration {
	if opts.Timeout > 0 {
		return opts.Timeout + 30*time.Minute
	}
	return worker.WorkerHardCap + 30*time.Minute
}

func normalizeDetachedTerminalStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "success", "succeeded":
		return detachedrun.StatusSucceeded
	case "cancelled", "canceled":
		return detachedrun.StatusCancelled
	case "needs-human":
		return detachedrun.StatusNeedsHuman
	default:
		return detachedrun.StatusFailed
	}
}

func detachedTerminal(status string) bool {
	switch normalizeDetachedTerminalStatus(status) {
	case detachedrun.StatusSucceeded, detachedrun.StatusFailed, detachedrun.StatusCancelled, detachedrun.StatusNeedsHuman:
		return true
	default:
		return false
	}
}

func normalizedDepsNow(deps Deps) func() time.Time {
	if deps.Now != nil {
		return deps.Now
	}
	return time.Now
}

func shellQuote(value string) string {
	data, err := json.Marshal(value)
	if err != nil {
		return value
	}
	return string(data)
}

func startDetachedDispatchProcess(ctx context.Context, args []string, logPath string) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	if !filepath.IsAbs(logPath) {
		return 0, fmt.Errorf("detached supervisor log path must be absolute: %s", logPath)
	}
	logDir := filepath.Dir(logPath)
	logName := filepath.Base(logPath)
	if logName != "detached-supervisor.log" || logDir == "." || logDir == string(filepath.Separator) {
		return 0, fmt.Errorf("invalid detached supervisor log path: %s", logPath)
	}
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return 0, err
	}
	logRoot, err := os.OpenRoot(logDir)
	if err != nil {
		return 0, err
	}
	defer logRoot.Close()
	logFile, err := logRoot.OpenFile(logName, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer logFile.Close()
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return 0, err
	}
	defer devNull.Close()
	// #nosec G204 -- exe is the current loopcoder binary and args are reconstructed from typed dispatch options.
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	detachProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	if cmd.Process == nil {
		return 0, errors.New("detached process did not expose a pid")
	}
	return cmd.Process.Pid, nil
}
