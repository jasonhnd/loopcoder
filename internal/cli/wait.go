package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/routing"
	"github.com/jasonhnd/loopcoder/internal/waitstate"
)

func printWaitHelp(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  loopcoder wait quota-reset --until <RFC3339> [--wait-id <id>] [--format text|json]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Provider-free local waits. Quota-reset waits only observe the wall clock and")
	fmt.Fprintln(w, "emit five-minute receipts; they never launch a provider or poll provider APIs.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	fmt.Fprintln(w, "  --until string     known quota reset time (RFC3339 or RFC3339Nano) (required)")
	fmt.Fprintln(w, "  --wait-id string   durable wait identity (default derived from reset time)")
	fmt.Fprintln(w, "  --timeout duration hard wait timeout (default: until reset + 5m)")
	fmt.Fprintln(w, "  --format string    text or json (default \"text\")")
	fmt.Fprintln(w, "  --help             show help")
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
	plan := routing.PlanQuotaResetWaitFromTimes([]time.Time{resetAt}, nil, deps.Now())
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
	out := map[string]any{
		"kind":           "quota-reset",
		"wait_id":        report.WaitID,
		"stop_reason":    report.StopReason,
		"earliest_reset": plan.EarliestReset.UTC().Format(time.RFC3339Nano),
		"provider_calls": 0,
		"receipt_count":  len(receipts),
		"wake_decisions": report.WakeDecisions,
		"wake_delivered": report.WakeDelivered,
		"polls":          report.Polls,
	}
	if format == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			fmt.Fprintf(stderr, "wait quota-reset: write output: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "wait quota-reset complete\n")
	fmt.Fprintf(stdout, "wait_id: %s\n", report.WaitID)
	fmt.Fprintf(stdout, "earliest_reset: %s\n", plan.EarliestReset.UTC().Format(time.RFC3339Nano))
	fmt.Fprintf(stdout, "stop_reason: %s\n", report.StopReason)
	fmt.Fprintf(stdout, "provider_calls: 0\n")
	fmt.Fprintf(stdout, "receipt_count: %d\n", len(receipts))
	return 0
}

func parseFlexibleTime(value string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if ts, err := time.Parse(layout, value); err == nil {
			return ts.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("want RFC3339 or RFC3339Nano")
}
