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

type hostProgressAdapter struct {
	Profile           string
	Display           string
	ClaimOwner        string
	ReplayRunID       string
	ReplayCorrelation string
	BindingRequest    func(hostprofile.OriginOptions) (runtimecap.HostRunOriginBindingRequest, bool)
}

var codexProgressAdapter = hostProgressAdapter{
	Profile:           "codex-cli",
	Display:           "Codex",
	ClaimOwner:        "codex-host-replay",
	ReplayRunID:       "codex-host-replay-origin",
	ReplayCorrelation: "codex-host-replay-origin",
	BindingRequest:    hostprofile.CodexOriginBindingRequest,
}

var claudeProgressAdapter = hostProgressAdapter{
	Profile:           "claude-code",
	Display:           "Claude Code",
	ClaimOwner:        "claude-code-host-replay",
	ReplayRunID:       "claude-code-host-replay-origin",
	ReplayCorrelation: "claude-code-host-replay-origin",
	BindingRequest:    hostprofile.ClaudeOriginBindingRequest,
}

func codexHostOriginBinding(projectID, deliveryRunID, correlationID string) runtimecap.HostRunOriginBinding {
	return hostOriginBinding(codexProgressAdapter, projectID, deliveryRunID, correlationID)
}

func claudeHostOriginBinding(projectID, deliveryRunID, correlationID string) runtimecap.HostRunOriginBinding {
	return hostOriginBinding(claudeProgressAdapter, projectID, deliveryRunID, correlationID)
}

func hostOriginBinding(adapter hostProgressAdapter, projectID, deliveryRunID, correlationID string) runtimecap.HostRunOriginBinding {
	if adapter.BindingRequest == nil {
		return runtimecap.HostRunOriginBinding{Bound: false, Code: runtimecap.HostOriginAbsent, Redacted: true}
	}
	req, ok := adapter.BindingRequest(hostprofile.OriginOptions{
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

func hostSink(adapter hostProgressAdapter, projectID, deliveryRunID, correlationID, fallbackSinkID string) (sinkID, originID, transport string) {
	binding := hostOriginBinding(adapter, projectID, deliveryRunID, correlationID)
	if binding.Bound && strings.TrimSpace(binding.BindingID) != "" {
		return binding.BindingID, binding.OriginRef, runtimecap.HostProgressKnownOriginReplay
	}
	return fallbackSinkID, correlationID, "host-jsonl-v1"
}

func codexHostStableOriginRef(projectID string) string {
	return hostStableOriginRef(codexProgressAdapter, projectID)
}

func hostStableOriginRef(adapter hostProgressAdapter, projectID string) string {
	binding := hostOriginBinding(adapter, projectID, adapter.ReplayRunID, adapter.ReplayCorrelation)
	if !binding.Bound {
		return ""
	}
	return strings.TrimSpace(binding.OriginRef)
}

func replayCurrentHostProgressBeforeDispatch(ctx context.Context, repoPath, runID string, stderr io.Writer, deps Deps) error {
	adapter, ok := currentHostProgressAdapter()
	if !ok {
		return nil
	}
	return replayHostProgressBeforeDispatch(ctx, repoPath, runID, stderr, deps, adapter)
}

func replayHostProgressBeforeDispatch(ctx context.Context, repoPath, runID string, stderr io.Writer, deps Deps, adapter hostProgressAdapter) error {
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
		return fmt.Errorf("open %s host progress replay store: %w", adapter.Display, err)
	}
	defer store.Close()
	if strings.TrimSpace(runID) != "" {
		_, err := replayHostProgressForRun(ctx, store, roots.ProjectID, runID, runID, stderr, now, progress.DefaultHostReplayLimit, adapter)
		return err
	}
	stableOriginRef := hostStableOriginRef(adapter, roots.ProjectID)
	if stableOriginRef == "" {
		return nil
	}
	candidates, err := hostReplayCandidates(ctx, store, roots.ProjectID, stableOriginRef, now().UTC(), progress.DefaultHostReplayLimit, adapter)
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
		count, err := replayHostProgressForBinding(ctx, store, roots.ProjectID, candidate.deliveryRunID, candidate.sinkID, stderr, now, progress.DefaultHostReplayLimit-replayed, adapter)
		if err != nil {
			if errors.Is(err, progress.ErrMissingReference) {
				fmt.Fprintf(stderr, "[loopcoder] skipped %s progress replay for run %s: %v. Use `loopcoder status --repo %s --run %s --receipts` or `loopcoder attach --repo %s --run %s` to inspect durable receipts.\n", adapter.Display, candidate.deliveryRunID, err, repoPath, candidate.deliveryRunID, repoPath, candidate.deliveryRunID)
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

func replayHostProgressForRun(ctx context.Context, store storage.Store, projectID, runID, correlationID string, stderr io.Writer, now func() time.Time, limit int, adapter hostProgressAdapter) (int, error) {
	binding := hostOriginBinding(adapter, projectID, runID, correlationID)
	if !binding.Bound {
		return 0, nil
	}
	return replayHostProgressForBinding(ctx, store, projectID, runID, binding.BindingID, stderr, now, limit, adapter)
}

func replayCodexHostProgressForBinding(ctx context.Context, store storage.Store, projectID, runID, bindingID string, stderr io.Writer, now func() time.Time, limit int) (int, error) {
	return replayHostProgressForBinding(ctx, store, projectID, runID, bindingID, stderr, now, limit, codexProgressAdapter)
}

func replayHostProgressForBinding(ctx context.Context, store storage.Store, projectID, runID, bindingID string, stderr io.Writer, now func() time.Time, limit int, adapter hostProgressAdapter) (int, error) {
	bindingID = strings.TrimSpace(bindingID)
	if bindingID == "" {
		return 0, nil
	}
	if limit <= 0 {
		return 0, nil
	}
	result, err := progress.ReplayPendingForHost(ctx, store, progress.HostReplayOptions{
		ProjectID:     projectID,
		DeliveryRunID: runID,
		OriginKind:    "host-run-origin",
		OriginID:      bindingID,
		ClaimOwner:    adapter.ClaimOwner,
		Limit:         limit,
		Now:           now().UTC(),
	}, func(view progress.ReceiptView) error {
		if _, err := fmt.Fprintf(stderr, "[loopcoder] replaying 1 pending progress receipt for %s origin %s\n", adapter.Display, bindingID); err != nil {
			return err
		}
		return progress.RenderHuman(stderr, []progress.ReceiptView{view})
	})
	if err != nil {
		return 0, err
	}
	return result.Replayed, nil
}

func codexHostReplayCandidates(ctx context.Context, store storage.Store, projectID, stableOriginRef string, now time.Time, limit int) ([]codexHostReplayCandidate, error) {
	return hostReplayCandidates(ctx, store, projectID, stableOriginRef, now, limit, codexProgressAdapter)
}

func hostReplayCandidates(ctx context.Context, store storage.Store, projectID, stableOriginRef string, now time.Time, limit int, adapter hostProgressAdapter) ([]codexHostReplayCandidate, error) {
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
			return fmt.Errorf("list %s host replay candidates: %w", adapter.Display, err)
		}
		defer rows.Close()
		for rows.Next() {
			var candidate codexHostReplayCandidate
			var firstCreatedAt string
			if err := rows.Scan(&candidate.deliveryRunID, &candidate.originID, &candidate.sinkID, &firstCreatedAt); err != nil {
				return fmt.Errorf("list %s host replay candidates scan: %w", adapter.Display, err)
			}
			out = append(out, candidate)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("list %s host replay candidates rows: %w", adapter.Display, err)
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

func currentHostProgressAdapter() (hostProgressAdapter, bool) {
	if raw := strings.TrimSpace(os.Getenv(hostprofile.EnvName)); raw != "" {
		explicit, ok := hostprofile.NormalizeName(raw)
		if !ok {
			return hostProgressAdapter{}, false
		}
		switch explicit {
		case codexProgressAdapter.Profile:
			return codexProgressAdapter, true
		case claudeProgressAdapter.Profile:
			return claudeProgressAdapter, true
		default:
			return hostProgressAdapter{}, false
		}
	}
	if _, ok := hostprofile.CodexOriginBindingRequest(hostprofile.OriginOptions{Getenv: os.Getenv}); ok {
		return codexProgressAdapter, true
	}
	if _, ok := hostprofile.ClaudeOriginBindingRequest(hostprofile.OriginOptions{Getenv: os.Getenv}); ok {
		return claudeProgressAdapter, true
	}
	return hostProgressAdapter{}, false
}
