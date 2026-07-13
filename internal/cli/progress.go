package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/progress"
	"github.com/jasonhnd/loopcoder/internal/runtimepath"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

func progressSupervisorForRegisteredRepo(ctx context.Context, repoPath, runID string, now func() time.Time, diagnostics io.Writer) (progress.Recorder, func() error) {
	roots, err := runtimepath.Resolve(ctx, repoPath)
	if err != nil {
		progressDiagnosticWarning(diagnostics, "resolve runtime roots", err)
		return nil, func() error { return nil }
	}
	if !roots.Registered || strings.TrimSpace(roots.ProjectID) == "" || strings.TrimSpace(roots.DatabasePath) == "" || strings.TrimSpace(runID) == "" {
		return nil, func() error { return nil }
	}
	store, err := storage.Open(ctx, storage.Options{Path: roots.DatabasePath, Now: now})
	if err != nil {
		progressDiagnosticWarning(diagnostics, "open progress store", err)
		return nil, func() error { return nil }
	}
	recorder, stop, err := progressSupervisorForStore(store, roots.ProjectID, runID, diagnostics)
	if err != nil {
		_ = store.Close()
		progressDiagnosticWarning(diagnostics, "start progress supervisor", err)
		return nil, func() error { return nil }
	}
	return recorder, func() error {
		stopErr := stop()
		closeErr := store.Close()
		if stopErr != nil {
			return stopErr
		}
		return closeErr
	}
}

func progressSupervisorForStore(store storage.Store, projectID, runID string, diagnostics io.Writer) (progress.Recorder, func() error, error) {
	if diagnostics == nil {
		diagnostics = io.Discard
	}
	supervisor, err := progress.NewSupervisor(progress.SupervisorOptions{
		Store:              store,
		ProjectID:          projectID,
		DeliveryRunID:      runID,
		RunID:              runID,
		MaxSilenceInterval: progress.DefaultMaxSilenceInterval,
		Diagnostic: func(_ context.Context, diagnostic progress.Diagnostic) {
			fmt.Fprintf(diagnostics, "[loopcoder] warning: progress receipt diagnostic code=%s correlation=%s phase=%s status=%s error=%s\n",
				progress.DiagnosticMessage(diagnostic.Code),
				progress.DiagnosticMessage(diagnostic.CorrelationID),
				progress.DiagnosticMessage(diagnostic.Phase),
				progress.DiagnosticMessage(diagnostic.Status),
				progress.DiagnosticMessage(diagnostic.Error))
		},
	})
	if err != nil {
		return nil, nil, err
	}
	stop := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return supervisor.Stop(ctx)
	}
	return supervisor, stop, nil
}

func progressDiagnosticWarning(w io.Writer, operation string, err error) {
	if w == nil || err == nil {
		return
	}
	fmt.Fprintf(w, "[loopcoder] warning: %s: %s\n", progress.DiagnosticMessage(operation), progress.DiagnosticMessage(err.Error()))
}
