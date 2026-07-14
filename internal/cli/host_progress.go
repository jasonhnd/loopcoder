package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/hostprofile"
	"github.com/jasonhnd/loopcoder/internal/progress"
	"github.com/jasonhnd/loopcoder/internal/runtimecap"
	"github.com/jasonhnd/loopcoder/internal/runtimepath"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

func codexHostOriginBinding(projectID, deliveryRunID, correlationID string) runtimecap.HostRunOriginBinding {
	req, ok := hostprofile.CodexOriginBindingRequest(hostprofile.OriginOptions{
		ProjectID:     projectID,
		DeliveryRunID: deliveryRunID,
		CorrelationID: correlationID,
		Getenv:        os.Getenv,
	})
	if !ok {
		return runtimecap.HostRunOriginBinding{Bound: false, Code: runtimecap.HostOriginAbsent, Redacted: true}
	}
	return runtimecap.BindHostRunOrigin(req)
}

func codexHostSink(projectID, deliveryRunID, correlationID, fallbackSinkID string) (sinkID, originID, transport string) {
	binding := codexHostOriginBinding(projectID, deliveryRunID, correlationID)
	if binding.Bound && strings.TrimSpace(binding.BindingID) != "" {
		return binding.BindingID, binding.OriginRef, runtimecap.HostProgressKnownOriginReplay
	}
	return fallbackSinkID, correlationID, "host-jsonl-v1"
}

func codexHostStableOriginRef(projectID string) string {
	binding := codexHostOriginBinding(projectID, "codex-host-replay-origin", "codex-host-replay-origin")
	if !binding.Bound {
		return ""
	}
	return strings.TrimSpace(binding.OriginRef)
}

func replayCodexHostProgressBeforeDispatch(ctx context.Context, repoPath, runID string, stderr io.Writer, deps Deps) error {
	if stderr == nil {
		return nil
	}
	roots, err := runtimepath.Resolve(ctx, repoPath)
	if err != nil || !roots.Registered || roots.ProjectID == "" || roots.DatabasePath == "" {
		return nil
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	store, err := storage.Open(ctx, storage.Options{
		Path:         roots.DatabasePath,
		Now:          now,
		BusyTimeout:  deps.DetachedStorageBusyTimeout,
		WriteTxRetry: deps.DetachedStorageWriteTxRetry,
	})
	if err != nil {
		return fmt.Errorf("open Codex host progress replay store: %w", err)
	}
	defer store.Close()
	if strings.TrimSpace(runID) != "" {
		_, err := replayCodexHostProgressForRun(ctx, store, roots.ProjectID, runID, runID, stderr, now, progress.DefaultHostReplayLimit)
		return err
	}
	stableOriginRef := codexHostStableOriginRef(roots.ProjectID)
	if stableOriginRef == "" {
		return nil
	}
	candidates, err := codexHostReplayCandidates(ctx, store, roots.ProjectID, stableOriginRef, now().UTC(), progress.DefaultHostReplayLimit)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	replayed := 0
	for _, candidate := range candidates {
		if replayed >= progress.DefaultHostReplayLimit {
			return nil
		}
		key := candidate.deliveryRunID + "\x00" + candidate.sinkID
		if seen[key] {
			continue
		}
		seen[key] = true
		count, err := replayCodexHostProgressForBinding(ctx, store, roots.ProjectID, candidate.deliveryRunID, candidate.sinkID, stderr, now, progress.DefaultHostReplayLimit-replayed)
		if err != nil {
			if errors.Is(err, progress.ErrMissingReference) {
				fmt.Fprintf(stderr, "[loopcoder] skipped Codex progress replay for run %s: %v. Use `loopcoder status --repo %s --run %s --receipts` or `loopcoder attach --repo %s --run %s` to inspect durable receipts.\n", candidate.deliveryRunID, err, repoPath, candidate.deliveryRunID, repoPath, candidate.deliveryRunID)
				continue
			}
			return err
		}
		replayed += count
	}
	return nil
}

type codexHostReplayCandidate struct {
	deliveryRunID string
	originID      string
	sinkID        string
}

func replayCodexHostProgressForRun(ctx context.Context, store storage.Store, projectID, runID, correlationID string, stderr io.Writer, now func() time.Time, limit int) (int, error) {
	binding := codexHostOriginBinding(projectID, runID, correlationID)
	if !binding.Bound {
		return 0, nil
	}
	return replayCodexHostProgressForBinding(ctx, store, projectID, runID, binding.BindingID, stderr, now, limit)
}

func replayCodexHostProgressForBinding(ctx context.Context, store storage.Store, projectID, runID, bindingID string, stderr io.Writer, now func() time.Time, limit int) (int, error) {
	bindingID = strings.TrimSpace(bindingID)
	if bindingID == "" {
		return 0, nil
	}
	if limit <= 0 {
		return 0, nil
	}
	var views []progress.ReceiptView
	_, err := progress.ReplayPendingForHost(ctx, store, progress.HostReplayOptions{
		ProjectID:     projectID,
		DeliveryRunID: runID,
		OriginKind:    "host-run-origin",
		OriginID:      bindingID,
		ClaimOwner:    "codex-host-replay",
		Limit:         limit,
		Now:           now().UTC(),
	}, func(view progress.ReceiptView) error {
		views = append(views, view)
		return nil
	})
	if err != nil {
		return 0, err
	}
	if len(views) == 0 {
		return 0, nil
	}
	fmt.Fprintf(stderr, "[loopcoder] replaying %d pending progress receipt(s) for Codex origin %s\n", len(views), bindingID)
	return len(views), progress.RenderHuman(stderr, views)
}

func codexHostReplayCandidates(ctx context.Context, store storage.Store, projectID, stableOriginRef string, now time.Time, limit int) ([]codexHostReplayCandidate, error) {
	limit = codexHostReplayCandidateLimit(limit)
	stableOriginRef = strings.TrimSpace(stableOriginRef)
	if stableOriginRef == "" {
		return nil, nil
	}
	query := `SELECT o.delivery_run_id, o.origin_id, o.sink_id, MIN(o.created_at) AS first_created_at
		FROM progress_delivery_obligations o
		JOIN progress_receipts r ON r.progress_receipt_id = o.progress_receipt_id
		LEFT JOIN progress_delivery_replay_cursors c
			ON c.project_id = o.project_id
			AND c.delivery_run_id = o.delivery_run_id
			AND c.origin_kind = 'host-run-origin'
			AND c.origin_id = o.sink_id
		LEFT JOIN progress_delivery_obligations anchor
			ON anchor.obligation_id = COALESCE(json_extract(c.payload_json, '$.obligation_id'), c.obligation_id)
		WHERE o.project_id = ? AND o.sink_kind = 'host' AND o.transport_contract = ?
			AND o.origin_id = ?
			AND (o.status = ? OR (o.status = ? AND (o.next_attempt_at = '' OR o.next_attempt_at <= ?)))
			AND (
				COALESCE(json_extract(r.progress_json, '$.state'), '') = 'terminal'
				OR r.phase = 'detached-terminal'
				OR r.status IN ('succeeded', 'failed', 'cancelled', 'needs-human', 'timed-out', 'abandoned')
				OR COALESCE(json_extract(r.blocker_json, '$.state'), '') NOT IN ('', 'none', 'unknown')
			)
			AND (
				c.obligation_id IS NULL
				OR anchor.obligation_id IS NULL
				OR o.created_at > anchor.created_at
				OR (o.created_at = anchor.created_at AND o.obligation_id > anchor.obligation_id)
			)
		GROUP BY o.delivery_run_id, o.origin_id, o.sink_id
		ORDER BY first_created_at, o.delivery_run_id, o.origin_id, o.sink_id
		LIMIT ?`
	args := []any{strings.TrimSpace(projectID), runtimecap.HostProgressKnownOriginReplay, stableOriginRef, progress.DeliveryPending, progress.DeliveryRetryableFailure, now.UTC().Format(time.RFC3339Nano), limit}
	var out []codexHostReplayCandidate
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("list Codex host replay candidates: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var candidate codexHostReplayCandidate
			var firstCreatedAt string
			if err := rows.Scan(&candidate.deliveryRunID, &candidate.originID, &candidate.sinkID, &firstCreatedAt); err != nil {
				return fmt.Errorf("list Codex host replay candidates scan: %w", err)
			}
			out = append(out, candidate)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("list Codex host replay candidates rows: %w", err)
		}
		return nil
	})
	return out, err
}

func codexHostReplayCandidateLimit(limit int) int {
	if limit <= 0 {
		return progress.DefaultHostReplayLimit
	}
	if limit > progress.MaxHostReplayLimit {
		return progress.MaxHostReplayLimit
	}
	return limit
}
