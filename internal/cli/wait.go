package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/runtimepath"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/waitstate"
)

func printWaitHelp(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  loopcoder wait quota-reset --until <RFC3339> [--wait-id <id>] [--format text|json]")
	fmt.Fprintln(w, "  loopcoder wait approval --repo <path> --run <delivery-run-id> [--wait-id <id>] [--format text|json]")
	fmt.Fprintln(w, "  loopcoder wait outbox --repo <path> --run <delivery-run-id> [--obligation <id>] [--wait-id <id>] [--format text|json]")
	fmt.Fprintln(w, "  loopcoder wait detached-worker --repo <path> --run <run-id> [--wait-id <id>] [--format text|json]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Provider-free local waits. Every subcommand observes durable local state or the")
	fmt.Fprintln(w, "wall clock only. None launch a provider or poll provider APIs.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags common to most waits:")
	fmt.Fprintln(w, "  --repo string          registered repository path")
	fmt.Fprintln(w, "  --run string           delivery-run id or detached run id")
	fmt.Fprintln(w, "  --wait-id string       durable wait identity")
	fmt.Fprintln(w, "  --timeout duration     hard wait timeout (default 2h)")
	fmt.Fprintln(w, "  --format string        text or json (default \"text\")")
	fmt.Fprintln(w, "  --help                 show help")
}

func runWait(args []string, stdout, stderr io.Writer, deps Deps) int {
	if len(args) == 0 {
		printWaitHelp(stderr)
		return 2
	}
	if deps.Now == nil {
		deps.Now = DefaultDeps().Now
	}
	switch args[0] {
	case "quota-reset":
		return runWaitQuotaReset(args[1:], stdout, stderr, deps)
	case "approval":
		return runWaitApproval(args[1:], stdout, stderr, deps)
	case "outbox":
		return runWaitOutbox(args[1:], stdout, stderr, deps)
	case "detached-worker":
		return runWaitDetachedWorker(args[1:], stdout, stderr, deps)
	case "help", "--help", "-h":
		printWaitHelp(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "wait: unknown subcommand %q\n", args[0])
		return 2
	}
}

func runWaitQuotaReset(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("wait quota-reset", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var until, waitID, format string
	var timeout time.Duration
	fs.StringVar(&until, "until", "", "known quota reset time")
	fs.StringVar(&waitID, "wait-id", "", "wait identity")
	fs.DurationVar(&timeout, "timeout", 0, "hard timeout")
	fs.StringVar(&format, "format", "text", "output format")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	until = strings.TrimSpace(until)
	if until == "" {
		fmt.Fprintln(stderr, "wait quota-reset: --until is required")
		return 2
	}
	resetAt, err := parseFlexibleTime(until)
	if err != nil {
		fmt.Fprintf(stderr, "wait quota-reset: parse --until: %v\n", err)
		return 2
	}
	plan := waitstate.PlanQuotaResetWaitFromTimes([]time.Time{resetAt}, nil, deps.Now())
	if !plan.Applicable {
		fmt.Fprintf(stderr, "wait quota-reset: not applicable: %s\n", plan.Reason)
		return 2
	}
	policy := waitstate.DefaultPolicy()
	if timeout > 0 {
		policy.Timeout = timeout
	}
	var receipts []waitstate.Receipt
	report, err := waitstate.RunQuotaResetWait(context.Background(), waitstate.QuotaResetPlan{
		WaitID:  strings.TrimSpace(waitID),
		ResetAt: plan.EarliestReset,
		Policy:  policy,
		Receipt: func(_ context.Context, receipt waitstate.Receipt) error {
			receipts = append(receipts, receipt)
			return nil
		},
		Checkpoint: func(context.Context, waitstate.Snapshot) error { return nil },
	})
	if err != nil {
		fmt.Fprintf(stderr, "wait quota-reset: %v\n", err)
		return 1
	}
	return renderWaitReport(stdout, stderr, format, "quota-reset", report, len(receipts), map[string]any{
		"earliest_reset": plan.EarliestReset.UTC().Format(time.RFC3339Nano),
	})
}

func runWaitApproval(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("wait approval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var repo, runID, waitID, format string
	var timeout time.Duration
	fs.StringVar(&repo, "repo", "", "registered repository")
	fs.StringVar(&runID, "run", "", "delivery run id")
	fs.StringVar(&waitID, "wait-id", "", "wait identity")
	fs.DurationVar(&timeout, "timeout", 0, "hard timeout")
	fs.StringVar(&format, "format", "text", "output format")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	return runStoredWait(stdout, stderr, deps, storedWaitRequest{
		Kind:    waitstate.KindApproval,
		Label:   "approval",
		Repo:    repo,
		RunID:   runID,
		WaitID:  waitID,
		Timeout: timeout,
		Format:  format,
		Watcher: waitstate.WatchApproval,
		Probe: func(store storage.Store, roots runtimepath.Roots) waitstate.Probe {
			return approvalProbe(store, roots.ProjectID, runID)
		},
	})
}

func runWaitOutbox(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("wait outbox", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var repo, runID, obligationID, waitID, format string
	var timeout time.Duration
	fs.StringVar(&repo, "repo", "", "registered repository")
	fs.StringVar(&runID, "run", "", "delivery run id")
	fs.StringVar(&obligationID, "obligation", "", "optional obligation id")
	fs.StringVar(&waitID, "wait-id", "", "wait identity")
	fs.DurationVar(&timeout, "timeout", 0, "hard timeout")
	fs.StringVar(&format, "format", "text", "output format")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	return runStoredWait(stdout, stderr, deps, storedWaitRequest{
		Kind:    waitstate.KindDeliveryOutbox,
		Label:   "outbox",
		Repo:    repo,
		RunID:   runID,
		WaitID:  waitID,
		Timeout: timeout,
		Format:  format,
		Watcher: waitstate.WatchDeliveryOutbox,
		Probe: func(store storage.Store, roots runtimepath.Roots) waitstate.Probe {
			return outboxProbe(store, roots.ProjectID, runID, obligationID)
		},
	})
}

func runWaitDetachedWorker(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("wait detached-worker", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var repo, runID, waitID, format string
	var timeout time.Duration
	fs.StringVar(&repo, "repo", "", "registered repository")
	fs.StringVar(&runID, "run", "", "detached run id")
	fs.StringVar(&waitID, "wait-id", "", "wait identity")
	fs.DurationVar(&timeout, "timeout", 0, "hard timeout")
	fs.StringVar(&format, "format", "text", "output format")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	return runStoredWait(stdout, stderr, deps, storedWaitRequest{
		Kind:    waitstate.KindDetachedWorker,
		Label:   "detached-worker",
		Repo:    repo,
		RunID:   runID,
		WaitID:  waitID,
		Timeout: timeout,
		Format:  format,
		Watcher: waitstate.WatchDetachedWorker,
		Probe: func(store storage.Store, _ runtimepath.Roots) waitstate.Probe {
			return detachedWorkerProbe(store, runID, deps.Now)
		},
	})
}

type storedWaitRequest struct {
	Kind    waitstate.Kind
	Label   string
	Repo    string
	RunID   string
	WaitID  string
	Timeout time.Duration
	Format  string
	Watcher func(context.Context, waitstate.Options) (waitstate.Report, error)
	Probe   func(storage.Store, runtimepath.Roots) waitstate.Probe
}

func runStoredWait(stdout, stderr io.Writer, deps Deps, req storedWaitRequest) int {
	repo := strings.TrimSpace(req.Repo)
	runID := strings.TrimSpace(req.RunID)
	if repo == "" {
		fmt.Fprintf(stderr, "wait %s: --repo is required\n", req.Label)
		return 2
	}
	if runID == "" {
		fmt.Fprintf(stderr, "wait %s: --run is required\n", req.Label)
		return 2
	}
	resolved, err := resolveRepo(repo)
	if err != nil {
		fmt.Fprintf(stderr, "wait %s: %v\n", req.Label, err)
		return 2
	}
	ctx := context.Background()
	roots, err := runtimepath.Resolve(ctx, resolved)
	if err != nil {
		fmt.Fprintf(stderr, "wait %s: resolve project: %v\n", req.Label, err)
		return 1
	}
	if !roots.Registered || strings.TrimSpace(roots.ProjectID) == "" || strings.TrimSpace(roots.DatabasePath) == "" {
		fmt.Fprintf(stderr, "wait %s: requires a registered project; run loopcoder projects register --repo %s\n", req.Label, resolved)
		return 1
	}
	store, err := storage.Open(ctx, storage.Options{Path: roots.DatabasePath, Now: deps.Now})
	if err != nil {
		fmt.Fprintf(stderr, "wait %s: open store: %v\n", req.Label, err)
		return 1
	}
	defer store.Close()

	waitID := strings.TrimSpace(req.WaitID)
	if waitID == "" {
		waitID = string(req.Kind) + ":" + runID
	}
	checkpointDir := filepath.Join(roots.ProjectRoot, "waits")
	if strings.TrimSpace(roots.ProjectRoot) == "" {
		checkpointDir = filepath.Join(filepath.Dir(roots.DatabasePath), "waits")
	}
	checkpoint, load, err := fileWaitCheckpoint(checkpointDir)
	if err != nil {
		fmt.Fprintf(stderr, "wait %s: checkpoint dir: %v\n", req.Label, err)
		return 1
	}
	initial, _, err := load(req.Kind, waitID)
	if err != nil {
		fmt.Fprintf(stderr, "wait %s: load checkpoint: %v\n", req.Label, err)
		return 1
	}
	policy := waitstate.DefaultPolicy()
	if req.Timeout > 0 {
		policy.Timeout = req.Timeout
	}
	// Focused tests inject a short clock via Deps.WaitClock.
	var clock waitstate.Clock
	if deps.WaitClock != nil {
		clock = deps.WaitClock
	}
	var receipts []waitstate.Receipt
	report, err := req.Watcher(ctx, waitstate.Options{
		Kind:       req.Kind,
		WaitID:     waitID,
		Policy:     policy,
		Clock:      clock,
		Probe:      req.Probe(store, roots),
		Initial:    initial,
		Checkpoint: checkpoint,
		Receipt: func(_ context.Context, receipt waitstate.Receipt) error {
			receipts = append(receipts, receipt)
			return nil
		},
	})
	if err != nil {
		fmt.Fprintf(stderr, "wait %s: %v\n", req.Label, err)
		return 1
	}
	return renderWaitReport(stdout, stderr, req.Format, req.Label, report, len(receipts), map[string]any{
		"run_id":          runID,
		"project_id":      roots.ProjectID,
		"next_probe_at":   report.Snapshot.NextProbeAt,
		"last_code":       report.Snapshot.LastCode,
		"checkpoint_dir":  checkpointDir,
		"provider_calls":  report.ProviderInvocations,
	})
}

func renderWaitReport(stdout, stderr io.Writer, format, kind string, report waitstate.Report, receiptCount int, extra map[string]any) int {
	out := map[string]any{
		"kind":                    kind,
		"wait_id":                 report.WaitID,
		"stop_reason":             report.StopReason,
		"provider_calls":          report.ProviderInvocations,
		"receipt_count":           receiptCount,
		"polls":                   report.Polls,
		"wake_decisions":          report.WakeDecisions,
		"wake_delivered":          report.WakeDelivered,
		"duplicate_suppressions":  report.DuplicateSuppressions,
		"duration_ms":             report.DurationMS,
		"last_state":              report.Snapshot.LastState,
		"last_code":               report.Snapshot.LastCode,
		"next_probe_at":           report.Snapshot.NextProbeAt,
	}
	for key, value := range extra {
		out[key] = value
	}
	if format == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			fmt.Fprintf(stderr, "wait %s: write output: %v\n", kind, err)
			return 1
		}
		return waitExitCode(report)
	}
	fmt.Fprintf(stdout, "wait %s complete\n", kind)
	fmt.Fprintf(stdout, "wait_id: %s\n", report.WaitID)
	fmt.Fprintf(stdout, "stop_reason: %s\n", report.StopReason)
	fmt.Fprintf(stdout, "provider_calls: %d\n", report.ProviderInvocations)
	fmt.Fprintf(stdout, "receipt_count: %d\n", receiptCount)
	fmt.Fprintf(stdout, "polls: %d\n", report.Polls)
	if report.Snapshot.LastCode != "" {
		fmt.Fprintf(stdout, "last_code: %s\n", report.Snapshot.LastCode)
	}
	if report.Snapshot.NextProbeAt != "" {
		fmt.Fprintf(stdout, "next_probe_at: %s\n", report.Snapshot.NextProbeAt)
	}
	return waitExitCode(report)
}

func waitExitCode(report waitstate.Report) int {
	switch report.StopReason {
	case waitstate.StopTransition:
		if report.Snapshot.LastState == waitstate.StateTerminal {
			code := report.Snapshot.LastCode
			if strings.Contains(code, "needs-human") || strings.Contains(code, "ambiguous") ||
				strings.Contains(code, "rejected") || strings.Contains(code, "failed") ||
				strings.Contains(code, "failure") || strings.Contains(code, "expired") ||
				strings.Contains(code, "missing") {
				return 2
			}
		}
		return 0
	case waitstate.StopTimeout:
		return 1
	case waitstate.StopCanceled:
		return 1
	default:
		return 0
	}
}

func parseFlexibleTime(value string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if ts, err := time.Parse(layout, value); err == nil {
			return ts.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("want RFC3339 or RFC3339Nano")
}
