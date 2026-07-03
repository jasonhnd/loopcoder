package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
)

const (
	HungReasonStall    = "stall"
	HungReasonDeadline = "deadline"
)

var runSupervised = supervisedexec.Run

func runProviderCommand(ctx context.Context, cmd *exec.Cmd, inv Invocation, provider string) (supervisedexec.Result, error) {
	opts := supervisedexec.Options{
		HardCap:      inv.HardCap,
		StallTimeout: inv.StallTimeout,
		LogPath:      inv.LogPath,
		RunID:        inv.RunID,
		Role:         inv.Role,
	}
	if inv.StallTimeout > 0 {
		opts.OnStall = appendStallLine(inv.LogPath, provider)
	}
	return runSupervised(ctx, cmd, opts)
}

func resultWithSupervision(exitCode int, summary string, metadata invocationMetadata, startedAt, endedAt time.Time, supervision supervisedexec.Result, runErr error, ctx context.Context) Result {
	result := resultWithTiming(exitCode, summary, metadata, startedAt, endedAt)
	if isHungOutcome(supervision, runErr, ctx) {
		result.Hung = true
		result.HungReason = hungReason(supervision.Outcome)
	}
	return result
}

func supervisedExitCode(result supervisedexec.Result, runErr error) int {
	if result.Outcome == supervisedexec.OutcomeCompleted && runErr == nil {
		return result.ExitCode
	}
	return -1
}

func isHungOutcome(result supervisedexec.Result, runErr error, ctx context.Context) bool {
	switch result.Outcome {
	case supervisedexec.OutcomeStalled, supervisedexec.OutcomeDeadline:
	default:
		return false
	}
	return !isParentContextCancellation(ctx, runErr)
}

func isParentContextCancellation(ctx context.Context, err error) bool {
	if ctx == nil || err == nil {
		return false
	}
	ctxErr := ctx.Err()
	return ctxErr != nil && errors.Is(err, ctxErr)
}

func hungReason(outcome supervisedexec.Outcome) string {
	switch outcome {
	case supervisedexec.OutcomeStalled:
		return HungReasonStall
	case supervisedexec.OutcomeDeadline:
		return HungReasonDeadline
	default:
		return ""
	}
}

func appendStallLine(logPath, provider string) func(time.Duration) {
	return func(silentFor time.Duration) {
		file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return
		}
		defer file.Close()
		fmt.Fprintf(file, "\n[loopcoder] %s provider silent for %s\n", provider, silentFor.Round(time.Second))
	}
}
