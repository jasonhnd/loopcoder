package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/detachedrun"
	"github.com/jasonhnd/loopcoder/internal/process"
	"github.com/jasonhnd/loopcoder/internal/progress"
	"github.com/jasonhnd/loopcoder/internal/runtimepath"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/worker"
)

type detachedLaunchRecord struct {
	SchemaVersion  string `json:"schema_version"`
	Detached       bool   `json:"detached"`
	RunID          string `json:"run_id"`
	ProjectID      string `json:"project_id"`
	Owner          string `json:"supervisor_owner"`
	Generation     int64  `json:"supervisor_generation"`
	LeaseExpiresAt string `json:"lease_expires_at"`
	LaunchPhase    string `json:"launch_phase"`
	Status         string `json:"status"`
	PID            int    `json:"pid,omitempty"`
	StatusCommand  string `json:"status_command"`
	AttachCommand  string `json:"attach_command"`
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
	issueBodyFile, err := writeDetachedIssueBody(roots.LogsRoot, opts.RunID, opts.IssueBody)
	if err != nil {
		fmt.Fprintf(stderr, "dispatch: persist detached issue body: %v\n", err)
		return 1
	}
	payload := map[string]any{
		"issue_number": opts.IssueNumber,
		"issue_title":  opts.IssueTitle,
		"attempt":      opts.Attempt,
		"provider":     opts.Provider,
	}
	if issueBodyFile != "" {
		payload["issue_body_path"] = issueBodyFile
	}
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
		Payload: payload,
		Now:     now,
	})
	if err != nil {
		fmt.Fprintf(stderr, "dispatch: claim detached supervisor: %v\n", err)
		return 1
	}

	logPath := filepath.Join(roots.LogsRoot, opts.RunID, "detached-supervisor.log")
	spawned, err := launchDetachedSupervisor(ctx, store, opts, claim.Fence(), logPath, issueBodyFile, stderr, deps)
	if err != nil {
		return 1
	}
	record := detachedLaunchRecord{
		SchemaVersion:  "loopcoder.detached_launch.v1",
		Detached:       true,
		RunID:          spawned.RunID,
		ProjectID:      spawned.ProjectID,
		Owner:          spawned.Owner,
		Generation:     spawned.Generation,
		LeaseExpiresAt: spawned.LeaseExpiresAt,
		LaunchPhase:    spawned.LaunchPhase,
		Status:         spawned.Status,
		PID:            spawned.ProcessPID,
		StatusCommand:  detachedFenceCommand("loopcoder status --receipts", spawned),
		AttachCommand:  detachedFenceCommand("loopcoder attach", spawned),
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
	record, err := detachedrun.Get(ctx, store, fence.RunID)
	if err != nil {
		return 1
	}
	if _, err := persistDetachedReceipt(ctx, store, record, "detached-worker-started", detachedrun.StatusRunning, false, now().UTC()); err != nil {
		_, _ = detachedrun.Complete(ctx, store, fence, detachedrun.StatusNeedsHuman, "", "worker-started-receipt-failed", err.Error(), now().UTC())
		return 1
	}
	if _, err := detachedrun.RenewLease(ctx, store, fence, now().UTC().Add(detachedLeaseDuration(opts)), now().UTC()); err != nil {
		return 1
	}
	dispatchCtx, cancelDispatch := context.WithCancel(ctx)
	cadence, err := startDetachedSupervisorCadence(ctx, store, opts, fence, deps, cancelDispatch)
	if err != nil {
		cancelDispatch()
		_, _ = detachedrun.Complete(ctx, store, fence, detachedrun.StatusNeedsHuman, "", "cadence-start-failed", err.Error(), now().UTC())
		return 1
	}
	opts.Stderr = io.Discard
	result, dispatchErr := deps.Dispatch(dispatchCtx, opts)
	cancelDispatch()
	cadenceErr := cadence.Stop()
	status := strings.TrimSpace(result.Status)
	if status == "" {
		if dispatchErr != nil || cadenceErr != nil {
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
	if cadenceErr != nil {
		status = detachedrun.StatusNeedsHuman
		errorCode = "supervisor-cadence-failed"
		errorMessage = cadenceErr.Error()
	}
	terminalStatus := normalizeDetachedTerminalStatus(status)
	latest, getErr := detachedrun.Get(ctx, store, fence.RunID)
	if getErr != nil {
		return 1
	}
	terminalReceiptID, receiptErr := persistDetachedReceipt(ctx, store, latest, "detached-terminal", terminalStatus, true, now().UTC())
	if receiptErr != nil {
		terminalStatus = detachedrun.StatusNeedsHuman
		errorCode = "terminal-receipt-failed"
		errorMessage = receiptErr.Error()
	}
	_, completeErr := detachedrun.Complete(ctx, store, fence, terminalStatus, terminalReceiptID, errorCode, errorMessage, now().UTC())
	if dispatchErr != nil || cadenceErr != nil || completeErr != nil {
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
	var supervisorOwner string
	var supervisorGeneration int64
	var supervisorLease string
	format := "text"
	fs.StringVar(&repoPath, "repo", ".", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")
	fs.StringVar(&runID, "run", "", "run id")
	fs.StringVar(&runIDAlias, "run-id", "", "run id")
	fs.StringVar(&runIDAlias, "RunId", "", "run id")
	fs.StringVar(&supervisorOwner, "supervisor-owner", "", "expected detached supervisor owner")
	fs.Int64Var(&supervisorGeneration, "supervisor-generation", 0, "expected detached supervisor generation")
	fs.StringVar(&supervisorLease, "supervisor-lease", "", "expected detached supervisor lease expiry")
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
	fence, expectedLease, ok := detachedCommandFence("cancel", runID, supervisorOwner, supervisorGeneration, supervisorLease, stderr)
	if !ok {
		return 2
	}
	current, err := detachedrun.ValidateCurrentFence(context.Background(), store, fence, expectedLease)
	if err != nil {
		fmt.Fprintf(stderr, "cancel: %v\n", err)
		return 1
	}
	record, err := detachedrun.RequestCancelFenced(context.Background(), store, current.Fence(), current.LeaseExpiresAt, now)
	if err != nil {
		fmt.Fprintf(stderr, "cancel: %v\n", err)
		return 1
	}
	if record.ProcessPID > 0 && !detachedTerminal(record.Status) {
		if verifyErr := verifyDetachedProcessAuthority(record, deps); verifyErr != nil {
			record, err = detachedrun.Complete(context.Background(), store, record.Fence(), detachedrun.StatusNeedsHuman, "", "cancel-process-authority-unverified", verifyErr.Error(), now)
			if err != nil {
				fmt.Fprintf(stderr, "cancel: persist process authority ambiguity: %v\n", err)
				return 1
			}
			fmt.Fprintf(stderr, "cancel: process authority for pid %d is not verified: %v\n", record.ProcessPID, verifyErr)
			return 1
		}
		kill := deps.KillProcessTree
		if kill == nil {
			kill = process.KillTree
		}
		if err := kill(record.ProcessPID); err != nil {
			if _, completeErr := detachedrun.Complete(context.Background(), store, record.Fence(), detachedrun.StatusNeedsHuman, "", "cancel-kill-ambiguous", err.Error(), now); completeErr != nil {
				fmt.Fprintf(stderr, "cancel: persist kill ambiguity: %v\n", completeErr)
				return 1
			}
			fmt.Fprintf(stderr, "cancel: kill process tree %d: %v\n", record.ProcessPID, err)
			return 1
		}
		alive := process.Alive
		if deps.ProcessAlive != nil {
			alive = deps.ProcessAlive
		}
		if alive(record.ProcessPID) {
			record, err = detachedrun.Complete(context.Background(), store, record.Fence(), detachedrun.StatusNeedsHuman, "", "cancel-kill-unproven", "process remained observable after cancellation signal", now)
			if err != nil {
				fmt.Fprintf(stderr, "cancel: persist kill ambiguity: %v\n", err)
				return 1
			}
			fmt.Fprintf(stderr, "cancel: process tree %d still appears alive after kill\n", record.ProcessPID)
			return 1
		}
		record, err = detachedrun.Complete(context.Background(), store, record.Fence(), detachedrun.StatusCancelled, "", "cancelled", "detached run cancellation requested", now)
		if err != nil {
			fmt.Fprintf(stderr, "cancel: complete detached cancellation: %v\n", err)
			return 1
		}
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
	var supervisorOwner string
	var supervisorGeneration int64
	var supervisorLease string
	var pollInterval time.Duration
	var followFor time.Duration
	fs.StringVar(&repoPath, "repo", ".", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")
	fs.StringVar(&runID, "run", "", "run id")
	fs.StringVar(&runIDAlias, "run-id", "", "run id")
	fs.StringVar(&runIDAlias, "RunId", "", "run id")
	fs.StringVar(&format, "format", "text", "output format")
	fs.StringVar(&cursor, "cursor", "", "opaque progress receipt cursor")
	fs.StringVar(&supervisorOwner, "supervisor-owner", "", "expected detached supervisor owner")
	fs.Int64Var(&supervisorGeneration, "supervisor-generation", 0, "expected detached supervisor generation")
	fs.StringVar(&supervisorLease, "supervisor-lease", "", "expected detached supervisor lease expiry")
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
	store, _, err := openDetachedStore(context.Background(), resolvedRepo, deps)
	if err != nil {
		fmt.Fprintf(stderr, "attach: %v\n", err)
		return 1
	}
	fence, expectedLease, ok := detachedCommandFence("attach", runID, supervisorOwner, supervisorGeneration, supervisorLease, stderr)
	if !ok {
		_ = store.Close()
		return 2
	}
	if _, err := detachedrun.ValidateCurrentFence(context.Background(), store, fence, expectedLease); err != nil {
		_ = store.Close()
		fmt.Fprintf(stderr, "attach: %v\n", err)
		return 1
	}
	_ = store.Close()
	return runStatusProgressReceipts(statusProgressOptions{
		RepoPath:      resolvedRepo,
		RunID:         runID,
		Format:        format,
		Follow:        true,
		Cursor:        progress.Cursor(cursor),
		Fence:         fence,
		ExpectedLease: expectedLease,
		Limit:         500,
		PollInterval:  pollInterval,
		FollowFor:     followFor,
	}, stdout, stderr, deps)
}

func runDetachedRecover(repoPath, runID, format string, fence detachedrun.Fence, expectedLease string, stdout, stderr io.Writer, deps Deps) int {
	if format != "text" && format != "json" {
		fmt.Fprintf(stderr, "recover: invalid --format %q; want text or json\n", format)
		return 2
	}
	fence, expectedLease, ok := detachedCommandFence("recover", runID, fence.Owner, fence.Generation, expectedLease, stderr)
	if !ok {
		return 2
	}
	ctx := context.Background()
	store, roots, err := openDetachedStore(ctx, repoPath, deps)
	if err != nil {
		fmt.Fprintf(stderr, "recover: %v\n", err)
		return 1
	}
	defer store.Close()
	now := normalizedDepsNow(deps)().UTC()
	if _, err := detachedrun.ValidateCurrentFence(ctx, store, fence, expectedLease); err != nil {
		fmt.Fprintf(stderr, "recover: %v\n", err)
		return 1
	}
	result, err := detachedrun.Reconcile(ctx, store, runID, now)
	if err != nil {
		fmt.Fprintf(stderr, "recover: %v\n", err)
		return 1
	}
	if result.CanRecover {
		if result.Record.ProcessPID > 0 {
			if err := verifyDetachedProcessAuthority(result.Record, deps); err != nil {
				if _, completeErr := detachedrun.Complete(ctx, store, result.Record.Fence(), detachedrun.StatusNeedsHuman, "", "recover-process-authority-unverified", err.Error(), now); completeErr != nil {
					fmt.Fprintf(stderr, "recover: persist process authority ambiguity: %v\n", completeErr)
					return 1
				}
				fmt.Fprintf(stderr, "recover: process authority for pid %d is not verified: %v\n", result.Record.ProcessPID, err)
				return 2
			}
			kill := deps.KillProcessTree
			if kill == nil {
				kill = process.KillTree
			}
			if err := kill(result.Record.ProcessPID); err != nil {
				_, _ = detachedrun.Complete(ctx, store, result.Record.Fence(), detachedrun.StatusNeedsHuman, "", "recover-kill-ambiguous", err.Error(), now)
				fmt.Fprintf(stderr, "recover: kill old process tree %d: %v\n", result.Record.ProcessPID, err)
				return 2
			}
			alive := process.Alive
			if deps.ProcessAlive != nil {
				alive = deps.ProcessAlive
			}
			if alive(result.Record.ProcessPID) {
				_, _ = detachedrun.Complete(ctx, store, result.Record.Fence(), detachedrun.StatusNeedsHuman, "", "recover-kill-unproven", "process remained observable after recovery kill signal", now)
				fmt.Fprintf(stderr, "recover: process tree %d still appears alive after kill\n", result.Record.ProcessPID)
				return 2
			}
		}
		owner := detachedOwner(runID, now)
		acquired, err := detachedrun.AcquireRecovery(ctx, store, detachedrun.RecoveryRequest{
			RunID:                  result.Record.RunID,
			ExpectedOwner:          result.Record.Owner,
			ExpectedGeneration:     result.Record.Generation,
			ExpectedLeaseExpiresAt: result.Record.LeaseExpiresAt,
			Owner:                  owner,
			LeaseExpiresAt:         now.Add(detachedLeaseDuration(optionsFromDetachedRecord(repoPath, result.Record))),
			RecoveryEvidence: []detachedrun.Evidence{{
				Kind:       "durable-recovery-claim",
				ID:         result.Record.RunID,
				Summary:    "expired pre-worker detached supervisor claim acquired by recover",
				Confidence: "exact",
			}},
			Now: now,
		})
		if err != nil {
			fmt.Fprintf(stderr, "recover: acquire detached supervisor: %v\n", err)
			return 1
		}
		result = acquired
		if acquired.Execute {
			opts := optionsFromDetachedRecord(repoPath, acquired.Record)
			issueBodyFile := stringFromPayload(acquired.Record.Payload, "issue_body_path")
			logPath := filepath.Join(roots.LogsRoot, acquired.Record.RunID, "detached-supervisor.log")
			spawned, err := launchDetachedSupervisor(ctx, store, opts, acquired.Record.Fence(), logPath, issueBodyFile, stderr, deps)
			if err != nil {
				return 1
			}
			result.Record = spawned
		}
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
	store, err := storage.Open(ctx, storage.Options{
		Path:         roots.DatabasePath,
		Now:          normalizedDepsNow(deps),
		BusyTimeout:  deps.DetachedStorageBusyTimeout,
		WriteTxRetry: deps.DetachedStorageWriteTxRetry,
	})
	if err != nil {
		return nil, roots, err
	}
	return store, roots, nil
}

func launchDetachedSupervisor(ctx context.Context, store storage.Store, opts worker.Options, fence detachedrun.Fence, logPath, issueBodyFile string, stderr io.Writer, deps Deps) (detachedrun.Record, error) {
	args := detachedDispatchArgs(opts, fence, issueBodyFile)
	start := deps.StartDetachedDispatch
	if start == nil {
		start = startDetachedDispatchProcess
	}
	pid, err := start(ctx, args, logPath)
	if err != nil {
		_, _ = detachedrun.Complete(ctx, store, fence, detachedrun.StatusFailed, "", "spawn-failed", err.Error(), normalizedDepsNow(deps)().UTC())
		fmt.Fprintf(stderr, "dispatch: start detached supervisor: %v\n", err)
		return detachedrun.Record{}, err
	}
	authorityFunc := deps.ProcessAuthority
	if authorityFunc == nil {
		authorityFunc = process.Authority
	}
	observedAt := normalizedDepsNow(deps)().UTC()
	authority, authorityErr := authorityFunc(pid, observedAt)
	if authorityErr != nil {
		kill := deps.KillProcessTree
		if kill == nil {
			kill = process.KillTree
		}
		if killErr := kill(pid); killErr != nil {
			_, _ = detachedrun.Complete(ctx, store, fence, detachedrun.StatusNeedsHuman, "", "spawn-authority-ambiguous", killErr.Error(), observedAt)
		} else {
			_, _ = detachedrun.Complete(ctx, store, fence, detachedrun.StatusNeedsHuman, "", "spawn-authority-unverified", authorityErr.Error(), observedAt)
		}
		fmt.Fprintf(stderr, "dispatch: verify detached spawn authority: %v\n", authorityErr)
		return detachedrun.Record{}, authorityErr
	}
	spawned, err := detachedrun.MarkSpawned(ctx, store, fence, pid, authority, observedAt)
	if err != nil {
		kill := deps.KillProcessTree
		if kill == nil {
			kill = process.KillTree
		}
		if killErr := kill(pid); killErr != nil {
			_, _ = detachedrun.Complete(ctx, store, fence, detachedrun.StatusNeedsHuman, "", "spawn-marker-ambiguous", killErr.Error(), normalizedDepsNow(deps)().UTC())
		} else {
			_, _ = detachedrun.Complete(ctx, store, fence, detachedrun.StatusNeedsHuman, "", "spawn-marker-failed", err.Error(), normalizedDepsNow(deps)().UTC())
		}
		fmt.Fprintf(stderr, "dispatch: persist detached spawn: %v\n", err)
		return detachedrun.Record{}, err
	}
	return spawned, nil
}

type detachedSupervisorCadence struct {
	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
	errMu    sync.Mutex
	err      error
}

func startDetachedSupervisorCadence(ctx context.Context, store storage.Store, opts worker.Options, fence detachedrun.Fence, deps Deps, onError func()) (*detachedSupervisorCadence, error) {
	interval := deps.DetachedSupervisorCadence
	if interval <= 0 {
		interval = progress.DefaultMaxSilenceInterval
	}
	c := &detachedSupervisorCadence{
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	ticker := time.NewTicker(interval)
	go func() {
		defer close(c.doneCh)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				c.setErr(ctx.Err())
				return
			case <-c.stopCh:
				return
			case <-ticker.C:
				if err := detachedSupervisorCadenceTick(ctx, store, opts, fence, deps); err != nil {
					c.setErr(err)
					if onError != nil {
						onError()
					}
					return
				}
			}
		}
	}()
	return c, nil
}

func (c *detachedSupervisorCadence) Stop() error {
	if c == nil {
		return nil
	}
	c.stopOnce.Do(func() { close(c.stopCh) })
	<-c.doneCh
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.err
}

func (c *detachedSupervisorCadence) setErr(err error) {
	if err == nil {
		return
	}
	c.errMu.Lock()
	defer c.errMu.Unlock()
	if c.err == nil {
		c.err = err
	}
}

func detachedSupervisorCadenceTick(ctx context.Context, store storage.Store, opts worker.Options, fence detachedrun.Fence, deps Deps) error {
	now := normalizedDepsNow(deps)().UTC()
	record, err := detachedrun.Get(ctx, store, fence.RunID)
	if err != nil {
		return err
	}
	if detachedTerminal(record.Status) {
		return nil
	}
	if _, err := detachedrun.RenewLease(ctx, store, fence, now.Add(detachedLeaseDuration(opts)), now); err != nil {
		return err
	}
	latest, err := detachedrun.Get(ctx, store, fence.RunID)
	if err != nil {
		return err
	}
	_, err = persistDetachedReceipt(ctx, store, latest, "detached-supervisor-heartbeat", latest.Status, false, now)
	return err
}

func detachedCommandFence(command, runID, owner string, generation int64, lease string, stderr io.Writer) (detachedrun.Fence, string, bool) {
	fence := detachedrun.Fence{RunID: strings.TrimSpace(runID), Owner: strings.TrimSpace(owner), Generation: generation}
	lease = strings.TrimSpace(lease)
	if fence.Owner == "" || fence.Generation <= 0 {
		fmt.Fprintf(stderr, "%s: --supervisor-owner and --supervisor-generation are required for detached run authority\n", command)
		return detachedrun.Fence{}, "", false
	}
	return fence, lease, true
}

func validateDetachedStatusFence(ctx context.Context, store storage.Store, runID string, fence detachedrun.Fence, expectedLease string) error {
	record, err := detachedrun.Get(ctx, store, runID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if strings.TrimSpace(record.RunID) == "" {
		return nil
	}
	if strings.TrimSpace(fence.Owner) == "" && fence.Generation == 0 {
		return fmt.Errorf("--supervisor-owner and --supervisor-generation are required for detached run authority")
	}
	_, err = detachedrun.ValidateCurrentFence(ctx, store, detachedrun.Fence{RunID: runID, Owner: fence.Owner, Generation: fence.Generation}, expectedLease)
	return err
}

func verifyDetachedProcessAuthority(record detachedrun.Record, deps Deps) error {
	if record.ProcessPID <= 0 {
		return nil
	}
	if strings.TrimSpace(record.ProcessAuthority) == "" {
		return fmt.Errorf("missing process authority")
	}
	verify := deps.VerifyProcessAuthority
	if verify == nil {
		verify = process.VerifyAuthority
	}
	return verify(record.ProcessPID, record.ProcessAuthority)
}

func detachedFenceCommand(prefix string, record detachedrun.Record) string {
	return fmt.Sprintf("%s --run %s --supervisor-owner %s --supervisor-generation %d",
		prefix,
		shellQuote(record.RunID),
		shellQuote(record.Owner),
		record.Generation)
}

func detachedDispatchArgs(opts worker.Options, fence detachedrun.Fence, issueBodyFile string) []string {
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
	if strings.TrimSpace(issueBodyFile) != "" {
		args = append(args, "--issue-body-file", issueBodyFile)
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

func writeDetachedIssueBody(logsRoot, runID, issueBody string) (string, error) {
	if strings.TrimSpace(issueBody) == "" {
		return "", nil
	}
	dir := filepath.Join(logsRoot, runID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "issue-body.txt")
	if err := os.WriteFile(path, []byte(issueBody), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func persistDetachedReceipt(ctx context.Context, store storage.Store, record detachedrun.Record, phase, status string, terminal bool, now time.Time) (string, error) {
	counts := progress.TaskCounts{Total: 1}
	switch strings.ToLower(strings.TrimSpace(status)) {
	case detachedrun.StatusSucceeded:
		counts.Succeeded = 1
	case detachedrun.StatusFailed, detachedrun.StatusCancelled:
		counts.Failed = 1
	case detachedrun.StatusNeedsHuman:
		counts.Blocked = 1
	default:
		counts.Running = 1
	}
	known := progress.KnownAliveNoMeaningfulProgress
	next := progress.ActionState{State: "continue", Summary: "detached supervisor is running"}
	if terminal {
		known = progress.KnownTerminal
		next = progress.ActionState{State: "complete", Summary: "detached supervisor reached terminal state"}
	}
	receipt := progress.ProgressReceipt{
		ProjectID:           record.ProjectID,
		DeliveryRunID:       record.RunID,
		RunID:               record.RunID,
		TaskID:              fmt.Sprintf("issue-%d", record.IssueNumber),
		AttemptID:           fmt.Sprintf("%s-generation-%d", record.RunID, record.Generation),
		AttemptOrdinal:      record.Attempt,
		CorrelationID:       record.RunID,
		Phase:               phase,
		Status:              status,
		TaskCounts:          counts,
		Provider:            progress.ProviderIdentity{ProviderID: record.Provider, ModelID: record.Model, ProviderConfidence: progress.Unknown},
		Heartbeat:           progress.AgeEvidence{State: "exact", ObservedAt: now.UTC().Format(time.RFC3339Nano), AgeMillis: 0},
		Progress:            progress.AgeEvidence{State: known, ObservedAt: now.UTC().Format(time.RFC3339Nano), AgeMillis: 0},
		Evidence:            []progress.EvidenceRef{{RecordKind: "detached-run-supervisor", RecordID: record.RunID, Summary: "detached supervisor state changed", Classification: "local-diagnostic", Confidence: "exact"}},
		QuotaBudget:         progress.QuotaBudgetState{State: progress.Unknown, Confidence: progress.Unknown, RemainingQuantity: -1, GapReasons: []string{"not-collected"}},
		NextAction:          next,
		GapReasons:          []string{"host-offline"},
		OccurredAt:          now.UTC().Format(time.RFC3339Nano),
		PersistedAt:         now.UTC().Format(time.RFC3339Nano),
		CorrelationSequence: 0,
	}
	normalized, err := progress.NormalizeReceipt(receipt, now)
	if err != nil {
		return "", err
	}
	adapter, ok := currentHostProgressAdapter()
	sinkID, originID, transport := "detached-run-status", record.RunID, "host-jsonl-v1"
	if ok {
		sinkID, originID, transport = hostSink(adapter, record.ProjectID, record.RunID, record.RunID, "detached-run-status")
	}
	result, err := progress.PersistReceiptWithObligation(ctx, store, normalized, progress.DeliveryObligation{
		OriginKind:        "progress-receipt",
		OriginID:          originID,
		SinkKind:          "host",
		SinkID:            sinkID,
		TransportContract: transport,
		AckPolicy:         progress.DeliveryAckPolicyRequired,
		MaxAttempts:       3,
	})
	if err != nil {
		return "", err
	}
	return result.Receipt.Receipt.ProgressReceiptID, nil
}

func optionsFromDetachedRecord(repoPath string, record detachedrun.Record) worker.Options {
	return worker.Options{
		RepoPath:       repoPath,
		IssueNumber:    record.IssueNumber,
		IssueTitle:     firstNonEmptyCLI(stringFromPayload(record.Payload, "issue_title"), "Detached recovery"),
		BaseBranch:     record.BaseBranch,
		Branch:         record.Branch,
		RunID:          record.RunID,
		Attempt:        record.Attempt,
		Provider:       record.Provider,
		Model:          record.Model,
		Effort:         record.Effort,
		ConfigFromBase: true,
	}
}

func stringFromPayload(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	value, _ := payload[key].(string)
	return strings.TrimSpace(value)
}

func firstNonEmptyCLI(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
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
	switch strings.ToLower(strings.TrimSpace(status)) {
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
