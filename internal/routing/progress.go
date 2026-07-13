package routing

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/progress"
)

func emitFallbackProgress(ctx context.Context, emitter progress.Recorder, decision FallbackDecision, now time.Time) {
	if emitter == nil {
		return
	}
	known := progress.KnownFallbackInProgress
	if decision.Trigger == FallbackTriggerQuotaExhausted || decision.Trigger == FallbackTriggerBudgetRefused {
		known = progress.KnownQuotaBlocked
	}
	if decision.DecisionStatus == FallbackStatusBlocked || decision.DecisionStatus == FallbackStatusNeedsHuman || decision.DecisionStatus == FallbackStatusReplanRequired {
		if known != progress.KnownQuotaBlocked {
			known = progress.KnownBlocked
		}
	}
	observation := progress.Observation{
		ProjectID:     decision.ProjectID,
		DeliveryRunID: decision.DeliveryRunID,
		RunID:         decision.DeliveryRunID,
		TaskID:        decision.TaskID,
		CorrelationID: "fallback:" + decision.RoutingDecisionID,
		Phase:         "fallback",
		Status:        strings.TrimSpace(decision.DecisionStatus),
		KnownState:    known,
		Reason:        progress.ReasonStateChange,
		Evidence: []progress.EvidenceRef{{
			RecordKind:     "fallback-decision",
			RecordID:       decision.FallbackDecisionID,
			Summary:        "fallback decision persisted from " + string(decision.Trigger),
			Classification: "local-diagnostic",
			Confidence:     "exact",
		}},
		OccurredAt: now,
	}
	_, err := emitter.Emit(ctx, observation)
	if err != nil && !errors.Is(err, progress.ErrEmitterClosed) {
		progress.ReportDiagnostic(ctx, emitter, observation, err)
		return
	}
}
