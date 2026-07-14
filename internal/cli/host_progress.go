package cli

import (
	"context"
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

func codexHostSink(projectID, deliveryRunID, correlationID, fallbackSinkID string) (sinkID, transport string) {
	binding := codexHostOriginBinding(projectID, deliveryRunID, correlationID)
	if binding.Bound && strings.TrimSpace(binding.BindingID) != "" {
		return binding.BindingID, runtimecap.HostProgressKnownOriginReplay
	}
	return fallbackSinkID, "host-jsonl-v1"
}

func replayCodexHostProgressBeforeDispatch(ctx context.Context, repoPath, runID string, stderr io.Writer, deps Deps) error {
	if stderr == nil || strings.TrimSpace(runID) == "" {
		return nil
	}
	roots, err := runtimepath.Resolve(ctx, repoPath)
	if err != nil || !roots.Registered || roots.ProjectID == "" || roots.DatabasePath == "" {
		return nil
	}
	binding := codexHostOriginBinding(roots.ProjectID, runID, runID)
	if !binding.Bound {
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
	var views []progress.ReceiptView
	_, err = progress.ReplayPendingForHost(ctx, store, progress.HostReplayOptions{
		ProjectID:     roots.ProjectID,
		DeliveryRunID: runID,
		OriginKind:    "host-run-origin",
		OriginID:      binding.BindingID,
		ClaimOwner:    "codex-host-replay",
		Limit:         progress.DefaultHostReplayLimit,
		Now:           now().UTC(),
	}, func(view progress.ReceiptView) error {
		views = append(views, view)
		return nil
	})
	if err != nil {
		return err
	}
	if len(views) == 0 {
		return nil
	}
	fmt.Fprintf(stderr, "[loopcoder] replaying %d pending progress receipt(s) for Codex origin %s\n", len(views), binding.BindingID)
	return progress.RenderHuman(stderr, views)
}
