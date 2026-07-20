package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/provideroutcome"
	"github.com/jasonhnd/loopcoder/internal/routing"
	"github.com/jasonhnd/loopcoder/internal/runtimepath"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/worker"
)

// applyTypedFallbackAfterDispatch connects a classified Worker failure to
// routing.ApplyTypedProviderFailure. Kept in CLI to avoid worker→routing import
// cycles. Does not relaunch a provider in-process.
func applyTypedFallbackAfterDispatch(ctx context.Context, repoPath string, opts worker.Options, result worker.Result, now func() time.Time, stderr io.Writer) worker.Result {
	if result.OK || strings.TrimSpace(result.FailureClass) == "" {
		return result
	}
	if opts.RoutePinned {
		result.FallbackNeedsHuman = true
		if result.NextAction == "" {
			result.NextAction = "needs-human: explicit pin forbids automatic fallback without owner authorization"
		}
		return result
	}
	if !result.AutoFallbackAllowed {
		return result
	}
	decisionID := strings.TrimSpace(opts.RoutingDecisionID)
	if decisionID == "" {
		return result
	}
	class := provideroutcome.Class(strings.TrimSpace(result.FailureClass))
	if class == "" {
		return result
	}
	if now == nil {
		now = time.Now
	}
	if ctx == nil {
		ctx = context.Background()
	}
	roots, err := runtimepath.Resolve(ctx, repoPath)
	if err != nil || !roots.Registered || strings.TrimSpace(roots.DatabasePath) == "" {
		result.Evidence = append(result.Evidence, "fallback_store=unavailable")
		return result
	}
	store, err := storage.Open(ctx, storage.Options{Path: roots.DatabasePath, Now: now})
	if err != nil {
		result.Evidence = append(result.Evidence, "fallback_store_open_error")
		result.NextAction = "typed failure recorded; open store for fallback failed: " + err.Error()
		return result
	}
	defer store.Close()

	original, err := routing.LoadRoutingDecision(ctx, store, decisionID)
	if err != nil {
		result.Evidence = append(result.Evidence, "fallback_load_decision_error")
		result.NextAction = "typed failure recorded; load routing decision failed: " + err.Error()
		return result
	}
	prior := strings.TrimSpace(original.ChosenCandidateID)
	if prior == "" {
		result.Evidence = append(result.Evidence, "fallback_prior_candidate_missing")
		result.NextAction = "typed failure recorded; routing decision has no chosen candidate for fallback"
		return result
	}
	out, applyErr := routing.ApplyTypedProviderFailure(ctx, store, routing.TypedFallbackRequest{
		RoutingDecisionID: decisionID,
		PriorCandidateID:  prior,
		Class:             class,
		IdempotencyKey:    fmt.Sprintf("cli-typed-fallback:%s:%s:%d", decisionID, class, opts.Attempt),
		Pinned:            opts.RoutePinned,
		DecidedBy: delivery.Actor{
			ActorKind:         "system",
			ActorID:           "loopcoder-dispatch",
			Display:           "loopcoder dispatch",
			DecisionAuthority: "cli-typed-fallback",
			Source:            "cli.applyTypedFallbackAfterDispatch",
		},
		Host: delivery.Host{
			HostKind:         "cli",
			HostID:           "loopcoder-cli",
			SessionID:        opts.RunID,
			LoopcoderVersion: "loopcoder",
		},
	})
	result.FallbackApplied = out.Applied
	result.FallbackNeedsHuman = out.NeedsHuman
	if out.Applied {
		result.FallbackDecisionID = strings.TrimSpace(out.Decision.FallbackDecisionID)
		result.FallbackCandidateID = firstNonEmpty(
			out.Decision.FallbackCandidateID,
			out.Decision.SelectedCandidateID,
		)
	}
	if out.NeedsHuman {
		result.Outcome = string(worker.OutcomeNeedsHuman)
	}
	if strings.TrimSpace(out.NextAction) != "" {
		result.NextAction = out.NextAction
	}
	result.Evidence = append(result.Evidence,
		"fallback_prior_candidate="+prior,
		fmt.Sprintf("fallback_applied=%t", out.Applied),
		fmt.Sprintf("fallback_needs_human=%t", out.NeedsHuman),
	)
	if out.Decision.FallbackDecisionID != "" {
		result.Evidence = append(result.Evidence, "fallback_decision_id="+out.Decision.FallbackDecisionID)
	}
	if out.Decision.FallbackCandidateID != "" {
		result.Evidence = append(result.Evidence, "fallback_candidate_id="+out.Decision.FallbackCandidateID)
	}
	if applyErr != nil && !out.Applied {
		result.Evidence = append(result.Evidence, "fallback_error="+applyErr.Error())
	}
	if stderr != nil && (out.Applied || out.NeedsHuman || applyErr != nil) {
		fmt.Fprintf(stderr, "[loopcoder] typed fallback: class=%s applied=%t needs_human=%t next=%s\n",
			class, out.Applied, out.NeedsHuman, result.NextAction)
	}
	return result
}
