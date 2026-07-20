package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/detachedrun"
	"github.com/jasonhnd/loopcoder/internal/progress"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/waitstate"
)

// approvalProbe observes durable delivery_runs.approval_status with zero
// provider calls. Terminal outcomes cover rejected, expired, and stale rows.
func approvalProbe(store storage.Store, projectID, deliveryRunID string) waitstate.Probe {
	projectID = strings.TrimSpace(projectID)
	deliveryRunID = strings.TrimSpace(deliveryRunID)
	return func(ctx context.Context) (waitstate.Observation, error) {
		if store == nil {
			return waitstate.Observation{}, fmt.Errorf("approval wait requires a store")
		}
		var status, state string
		err := store.WithTx(ctx, func(tx storage.Tx) error {
			return tx.QueryRow(ctx, `SELECT COALESCE(approval_status, ''), COALESCE(state, '')
FROM delivery_runs WHERE project_id = ? AND delivery_run_id = ?`, projectID, deliveryRunID).Scan(&status, &state)
		})
		if err != nil {
			return waitstate.Observation{
				EventID: "approval-missing",
				State:   waitstate.StateTerminal,
				Code:    "approval-record-missing",
				References: []waitstate.Reference{{
					Kind: "delivery-run",
					ID:   deliveryRunID,
				}},
				Consequential: true,
				Terminal:      true,
			}, nil
		}
		status = strings.ToLower(strings.TrimSpace(status))
		refs := []waitstate.Reference{{Kind: "delivery-run", ID: deliveryRunID}, {Kind: "approval-status", ID: status}}
		switch status {
		case "approved":
			return waitstate.Observation{
				EventID: "approval-" + status, State: waitstate.StateReady, Code: "approval-approved",
				References: refs, Consequential: true,
			}, nil
		case "rejected", "expired", "stale":
			return waitstate.Observation{
				EventID: "approval-" + status, State: waitstate.StateTerminal, Code: "approval-" + status,
				References: refs, Consequential: true, Terminal: true,
			}, nil
		case "required", "pending", "not-required", "":
			// not-required is still waiting only if the run itself is blocked on human.
			if status == "not-required" && strings.EqualFold(state, "approved") {
				return waitstate.Observation{
					EventID: "approval-not-required", State: waitstate.StateReady, Code: "approval-not-required",
					References: refs, Consequential: true,
				}, nil
			}
			return waitstate.Observation{
				EventID: "approval-pending", State: waitstate.StateWaiting, Code: "approval-pending",
				References: refs,
			}, nil
		default:
			return waitstate.Observation{
				EventID: "approval-unknown", State: waitstate.StateWaiting, Code: "approval-status-" + status,
				References: refs,
			}, nil
		}
	}
}

// outboxProbe observes durable progress delivery obligations for one run.
func outboxProbe(store storage.Store, projectID, deliveryRunID, obligationID string) waitstate.Probe {
	projectID = strings.TrimSpace(projectID)
	deliveryRunID = strings.TrimSpace(deliveryRunID)
	obligationID = strings.TrimSpace(obligationID)
	return func(ctx context.Context) (waitstate.Observation, error) {
		if store == nil {
			return waitstate.Observation{}, fmt.Errorf("outbox wait requires a store")
		}
		items, err := progress.ListDeliveryObligations(ctx, store, progress.DeliveryObligationFilter{
			ProjectID:     projectID,
			DeliveryRunID: deliveryRunID,
			Limit:         64,
		})
		if err != nil {
			return waitstate.Observation{}, err
		}
		if obligationID != "" {
			filtered := items[:0]
			for _, item := range items {
				if item.ObligationID == obligationID {
					filtered = append(filtered, item)
				}
			}
			items = filtered
		}
		if len(items) == 0 {
			return waitstate.Observation{
				EventID: "outbox-empty",
				State:   waitstate.StateTerminal,
				Code:    "outbox-obligation-missing",
				References: []waitstate.Reference{{
					Kind: "delivery-run",
					ID:   deliveryRunID,
				}},
				Consequential: true,
				Terminal:      true,
			}, nil
		}
		// Prefer an open obligation when multiple exist; otherwise use the first.
		item := items[0]
		for _, candidate := range items {
			switch candidate.Status {
			case progress.DeliveryPending, progress.DeliveryAttempting, progress.DeliveryNeedsReconciliation,
				progress.DeliveryDeliveredUnacknowledged, progress.DeliveryRetryableFailure:
				item = candidate
			}
		}
		refs := []waitstate.Reference{
			{Kind: "delivery-run", ID: deliveryRunID},
			{Kind: "obligation", ID: item.ObligationID},
			{Kind: "outbox-status", ID: item.Status},
		}
		switch item.Status {
		case progress.DeliveryAcknowledged, progress.DeliveryDeliveredUnacknowledged:
			return waitstate.Observation{
				EventID: "outbox-" + item.Status, State: waitstate.StateReady, Code: "outbox-" + item.Status,
				References: refs, Consequential: true,
			}, nil
		case progress.DeliveryTerminalFailure, progress.DeliveryExpired, progress.DeliverySuperseded, progress.DeliveryUnsupported:
			return waitstate.Observation{
				EventID: "outbox-" + item.Status, State: waitstate.StateTerminal, Code: "outbox-" + item.Status,
				References: refs, Consequential: true, Terminal: true,
			}, nil
		case progress.DeliveryRetryableFailure, progress.DeliveryNeedsReconciliation:
			return waitstate.Observation{
				EventID: "outbox-" + item.Status, State: waitstate.StateWaiting, Code: "outbox-" + item.Status,
				References: refs, RetryAfter: 30 * time.Second,
			}, nil
		default:
			return waitstate.Observation{
				EventID: "outbox-" + item.Status, State: waitstate.StateWaiting, Code: "outbox-pending",
				References: refs,
			}, nil
		}
	}
}

// detachedWorkerProbe observes durable detached_run status with zero provider
// launches. Ambiguous authority becomes needs-human terminal.
func detachedWorkerProbe(store storage.Store, runID string, now func() time.Time) waitstate.Probe {
	runID = strings.TrimSpace(runID)
	if now == nil {
		now = time.Now
	}
	return func(ctx context.Context) (waitstate.Observation, error) {
		if store == nil {
			return waitstate.Observation{}, fmt.Errorf("detached-worker wait requires a store")
		}
		result, err := detachedrun.Reconcile(ctx, store, runID, now().UTC())
		if err != nil {
			// Missing record is terminal; other errors are retriable unavailability.
			if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "missing") {
				return waitstate.Observation{
					EventID: "worker-missing", State: waitstate.StateTerminal, Code: "worker-missing",
					References: []waitstate.Reference{{Kind: "detached-run", ID: runID}},
					Consequential: true, Terminal: true,
				}, nil
			}
			return waitstate.Observation{
				EventID: "worker-unavailable", State: waitstate.StateUnavailable, Code: "worker-probe-error",
				References: []waitstate.Reference{{Kind: "detached-run", ID: runID}},
				RetryAfter: 15 * time.Second,
			}, nil
		}
		status := strings.ToLower(strings.TrimSpace(result.Record.Status))
		if status == "" && result.NeedsHuman {
			status = detachedrun.StatusNeedsHuman
		}
		refs := []waitstate.Reference{
			{Kind: "detached-run", ID: runID},
			{Kind: "worker-status", ID: status},
		}
		switch status {
		case detachedrun.StatusSucceeded, "success", "completed":
			return waitstate.Observation{
				EventID: "worker-succeeded", State: waitstate.StateTerminal, Code: "worker-succeeded",
				References: refs, Consequential: true, Terminal: true,
			}, nil
		case detachedrun.StatusFailed, detachedrun.StatusCancelled:
			return waitstate.Observation{
				EventID: "worker-" + status, State: waitstate.StateTerminal, Code: "worker-" + status,
				References: refs, Consequential: true, Terminal: true,
			}, nil
		case detachedrun.StatusNeedsHuman:
			return waitstate.Observation{
				EventID: "worker-needs-human", State: waitstate.StateTerminal, Code: "worker-needs-human",
				References: refs, Consequential: true, Terminal: true,
			}, nil
		case detachedrun.StatusRetryable:
			return waitstate.Observation{
				EventID: "worker-retryable", State: waitstate.StateWaiting, Code: "worker-retryable",
				References: refs, RetryAfter: 20 * time.Second,
			}, nil
		case detachedrun.StatusRunning, detachedrun.StatusCancelling, detachedrun.StatusNotStarted:
			return waitstate.Observation{
				EventID: "worker-" + status, State: waitstate.StateWaiting, Code: "worker-" + status,
				References: refs,
			}, nil
		default:
			// Ambiguous external authority: fail closed for human review.
			return waitstate.Observation{
				EventID: "worker-ambiguous", State: waitstate.StateTerminal, Code: "worker-ambiguous",
				References: refs, Consequential: true, Terminal: true,
			}, nil
		}
	}
}
