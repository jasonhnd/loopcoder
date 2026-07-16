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
	terminal := fallbackProgressTerminal(decision.DecisionStatus)
	if terminal {
		known = progress.KnownBlocked
		if decision.Trigger == FallbackTriggerQuotaExhausted || decision.Trigger == FallbackTriggerBudgetRefused {
			known = progress.KnownQuotaBlocked
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
		Terminal:   terminal,
	}
	var err error
	if terminal {
		_, err = emitter.Terminal(ctx, observation)
	} else {
		_, err = emitter.Emit(ctx, observation)
	}
	if err != nil && !errors.Is(err, progress.ErrEmitterClosed) {
		progress.ReportDiagnostic(ctx, emitter, observation, err)
		return
	}
}

func fallbackProgressTerminal(status string) bool {
	switch strings.TrimSpace(status) {
	case FallbackStatusBlocked, FallbackStatusNeedsHuman, FallbackStatusReplanRequired:
		return true
	default:
		return false
	}
}
