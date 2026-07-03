package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	compiler "github.com/jasonhnd/loopcoder/internal/compile"
	"github.com/jasonhnd/loopcoder/internal/equivalence"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/statebranch"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
)

func TestPromoteWholeBatchPromotesPreProdToMainAndSyncs(t *testing.T) {
	repo := t.TempDir()
	runID := "run-test-promote"
	writer := &recordingPromotionWriter{}

	report, err := Promote(context.Background(), PromoteOptions{
		Writer:        writer,
		RepoPath:      repo,
		RunID:         runID,
		PreProdBranch: "pre-prod",
		Gate:          "human-merge",
		Clock:         fixedPromoteClock,
		StatePush:     promoteTestStatePush(t, repo, runID),
	})
	if err != nil {
		t.Fatalf("Promote returned error: %v", err)
	}
	if report.Status != PromoteStatusSucceeded {
		t.Fatalf("status = %s, want succeeded", report.Status)
	}
	if !reflect.DeepEqual(writer.calls, []string{"head:main", "promote:pre-prod", "sync:pre-prod"}) {
		t.Fatalf("calls = %#v", writer.calls)
	}
	if report.Summary.PromotedCount != 1 || report.Summary.KickedBackCount != 0 || report.Summary.FailureCount != 0 {
		t.Fatalf("summary = %#v", report.Summary)
	}
	if report.Promoted.MainBranch != "main" || report.Promoted.PreProdBranch != "pre-prod" || report.Sync.PreProdBranch != "pre-prod" {
		t.Fatalf("report branches = %#v %#v", report.Promoted, report.Sync)
	}
	if report.RunID != runID || report.StatePush == nil || !report.StatePush.Pushed {
		t.Fatalf("ledger state = runID %q statePush %#v", report.RunID, report.StatePush)
	}
	event := readPromoteEvents(t, repo, runID)
	for _, want := range []string{`"event":"promote.attempt"`, `"outcome":"promoted"`, `"sha":"main-sha"`, `"merge_commit":"main-sha"`, `"prior_stable_commit":"main-prior-sha"`} {
		if !strings.Contains(event, want) {
			t.Fatalf("promote ledger missing %q:\n%s", want, event)
		}
	}
}

func TestPromoteKickBackRevertsItemsBeforePromotingRemainder(t *testing.T) {
	repo := t.TempDir()
	runID := "run-test-promote-kick"
	writer := &recordingPromotionWriter{}

	report, err := Promote(context.Background(), PromoteOptions{
		Writer:        writer,
		RepoPath:      repo,
		RunID:         runID,
		PreProdBranch: "pre-prod",
		Gate:          "human-merge",
		KickBackItems: []string{"#101", "merge-sha"},
		Clock:         fixedPromoteClock,
		StatePush:     promoteTestStatePush(t, repo, runID),
	})
	if err != nil {
		t.Fatalf("Promote returned error: %v", err)
	}
	if report.Status != PromoteStatusSucceeded {
		t.Fatalf("status = %s, want succeeded", report.Status)
	}
	wantCalls := []string{"kick:#101:pre-prod", "route:101", "kick:merge-sha:pre-prod", "route:101", "head:main", "promote:pre-prod", "sync:pre-prod"}
	if !reflect.DeepEqual(writer.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", writer.calls, wantCalls)
	}
	if len(report.KickedBack) != 2 || report.KickedBack[0].Item != "#101" || report.KickedBack[1].Item != "merge-sha" {
		t.Fatalf("kicked back = %#v", report.KickedBack)
	}
	if report.Summary.KickedBackCount != 2 || report.Summary.PromotedCount != 1 || report.Summary.FailureCount != 0 {
		t.Fatalf("summary = %#v", report.Summary)
	}
	if len(report.NeedsHuman) != 2 || report.Summary.NeedsHumanCount != 2 {
		t.Fatalf("needs-human routing = %#v summary=%#v", report.NeedsHuman, report.Summary)
	}
	rendered := RenderPromoteText(report)
	if !strings.Contains(rendered, "Needs human") || !strings.Contains(rendered, "applied needs-human label") {
		t.Fatalf("rendered report missing needs-human routing:\n%s", rendered)
	}
}

func TestPromoteStopsBeforeMainWhenKickBackFails(t *testing.T) {
	repo := t.TempDir()
	runID := "run-test-promote-fail"
	writer := &recordingPromotionWriter{kickErr: errors.New("revert failed")}

	report, err := Promote(context.Background(), PromoteOptions{
		Writer:        writer,
		RepoPath:      repo,
		RunID:         runID,
		PreProdBranch: "pre-prod",
		Gate:          "human-merge",
		KickBackItems: []string{"#101"},
		Clock:         fixedPromoteClock,
		StatePush:     promoteTestStatePush(t, repo, runID),
	})
	if err != nil {
		t.Fatalf("Promote returned error: %v", err)
	}
	if report.Status != PromoteStatusFailed {
		t.Fatalf("status = %s, want failed", report.Status)
	}
	if !reflect.DeepEqual(writer.calls, []string{"kick:#101:pre-prod"}) {
		t.Fatalf("calls = %#v", writer.calls)
	}
	if report.Summary.FailureCount != 1 || len(report.KickedBack) != 1 || report.KickedBack[0].Status != PromoteStatusFailed {
		t.Fatalf("report = %#v", report)
	}
	event := readPromoteEvents(t, repo, runID)
	for _, want := range []string{`"event":"promote.attempt"`, `"outcome":"failed"`, `"error":"revert failed"`} {
		if !strings.Contains(event, want) {
			t.Fatalf("promote failure ledger missing %q:\n%s", want, event)
		}
	}
}

func TestPromoteAlreadyUpToDateIsLedgeredAsSkipped(t *testing.T) {
	repo := t.TempDir()
	runID := "run-test-promote-skipped"
	writer := &recordingPromotionWriter{alreadyUpToDate: true}

	report, err := Promote(context.Background(), PromoteOptions{
		Writer:        writer,
		RepoPath:      repo,
		RunID:         runID,
		PreProdBranch: "pre-prod",
		Gate:          "human-merge",
		Clock:         fixedPromoteClock,
		StatePush:     promoteTestStatePush(t, repo, runID),
	})
	if err != nil {
		t.Fatalf("Promote returned error: %v", err)
	}
	if report.Status != PromoteStatusSucceeded || !report.Promoted.AlreadyUpToDate {
		t.Fatalf("report = %#v, want already-up-to-date success", report)
	}
	event := readPromoteEvents(t, repo, runID)
	if !strings.Contains(event, `"outcome":"skipped-as-done"`) || !strings.Contains(event, `"already_up_to_date":true`) {
		t.Fatalf("already-up-to-date ledger missing skipped outcome:\n%s", event)
	}
	if !strings.Contains(event, `"prior_stable_commit":"main-prior-sha"`) {
		t.Fatalf("already-up-to-date ledger missing prior stable commit:\n%s", event)
	}
	if strings.Contains(event, `"merge_commit"`) {
		t.Fatalf("already-up-to-date ledger should not record an empty merge commit:\n%s", event)
	}
}

func TestPromoteDarkEpicSlicesDoNotBlockPromotion(t *testing.T) {
	repo := t.TempDir()
	runID := "run-test-promote-dark"
	writeEpicOrderingArtifact(t, repo, compiler.EpicSliceDAGArtifact{
		Version:   compiler.EpicDAGVersion,
		EpicID:    "epic-1",
		EpicTitle: "Rewrite billing engine",
		Nodes: []compiler.EpicSliceNode{
			{
				ID:        "impl",
				Ref:       "rewrite-billing-engine/code-2",
				Issue:     20,
				Completed: true,
				SliceType: compiler.EpicSliceTypeImplementation,
				BuildTagToggle: &compiler.EpicBuildTagToggle{
					Name:         "rewrite-billing-engine/code-2",
					BuildTag:     "lc_rewrite_billing_engine_code_2",
					DefaultState: compiler.EpicToggleStateOff,
					State:        compiler.EpicToggleStateOff,
				},
				Dark:       true,
				DarkReason: "epic is not complete; implementation slice remains toggled off in pre-prod",
			},
		},
	})
	writer := &recordingPromotionWriter{}

	report, err := Promote(context.Background(), PromoteOptions{
		Writer:        writer,
		RepoPath:      repo,
		RunID:         runID,
		PreProdBranch: "pre-prod",
		Gate:          "human-merge",
		Clock:         fixedPromoteClock,
		StatePush:     promoteTestStatePush(t, repo, runID),
	})
	if err != nil {
		t.Fatalf("Promote returned error: %v", err)
	}
	if report.Status != PromoteStatusSucceeded {
		t.Fatalf("status = %s, want succeeded", report.Status)
	}
	if !reflect.DeepEqual(writer.calls, []string{"head:main", "promote:pre-prod", "sync:pre-prod"}) {
		t.Fatalf("calls = %#v, want promotion to continue despite dark slice", writer.calls)
	}
	if len(report.ToggleInventory.LeaveDark) != 1 || report.ToggleInventory.LeaveDark[0].SliceRef != "rewrite-billing-engine/code-2" {
		t.Fatalf("toggle inventory = %#v, want one dark slice", report.ToggleInventory)
	}
	if report.Summary.PromotedCount != 1 || report.Summary.FailureCount != 0 {
		t.Fatalf("summary = %#v", report.Summary)
	}
	rendered := RenderPromoteText(report)
	if !strings.Contains(rendered, "leave_dark rewrite-billing-engine/code-2") {
		t.Fatalf("rendered report missing dark toggle inventory:\n%s", rendered)
	}
}

func TestPromoteGoNoGoPanelCombinesReconciliationToggleNeedsHumanAndFailures(t *testing.T) {
	repo := t.TempDir()
	runID := "run-test-promote-panel"
	writeEpicOrderingArtifact(t, repo, compiler.EpicSliceDAGArtifact{
		Version:   compiler.EpicDAGVersion,
		EpicID:    "epic-1",
		EpicTitle: "Rewrite billing engine",
		Nodes: []compiler.EpicSliceNode{
			{
				ID:        "flip",
				Ref:       "rewrite-billing-engine/flip",
				Completed: true,
				BuildTagToggle: &compiler.EpicBuildTagToggle{
					Name:     "rewrite-billing-engine/flip",
					BuildTag: "lc_rewrite_billing_engine_flip",
					State:    compiler.EpicToggleStateOn,
				},
			},
			{
				ID:        "dark",
				Ref:       "rewrite-billing-engine/dark",
				Completed: true,
				BuildTagToggle: &compiler.EpicBuildTagToggle{
					Name:     "rewrite-billing-engine/dark",
					BuildTag: "lc_rewrite_billing_engine_dark",
					State:    compiler.EpicToggleStateOff,
				},
				Dark:       true,
				DarkReason: "epic is not complete; leave dark",
			},
		},
	})
	writer := &recordingPromotionWriter{routeErr: errors.New("label failed")}

	report, err := Promote(context.Background(), PromoteOptions{
		Writer:        writer,
		RepoPath:      repo,
		RunID:         runID,
		PreProdBranch: "pre-prod",
		Gate:          "human-merge",
		KickBackItems: []string{"#101"},
		Clock:         fixedPromoteClock,
		StatePush:     promoteTestStatePush(t, repo, runID),
		ParallelRun: &PromoteParallelRunConfig{
			Contract: promoteTestParallelRunContract(),
			Input: equivalence.ParallelRunInput{
				Old: []equivalence.ParallelRunRecord{
					{Key: "request-1", Value: map[string]any{"score": 1.0}},
					{Key: "request-2", Value: map[string]any{"score": 2.0}},
				},
				New: []equivalence.ParallelRunRecord{
					{Key: "request-1", Value: map[string]any{"score": 1.05}},
					{Key: "request-3", Value: map[string]any{"score": 3.0}},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Promote returned error: %v", err)
	}
	if report.Status != PromoteStatusFailed {
		t.Fatalf("status = %s, want failed", report.Status)
	}
	if report.GoNoGoPanel == nil {
		t.Fatal("go/no-go panel was not assembled")
	}
	panel := report.GoNoGoPanel
	if panel.Reconciliation == nil || panel.Reconciliation.Status != equivalence.StatusNeedsHuman || panel.Reconciliation.OldOnlyCount != 1 || panel.Reconciliation.NewOnlyCount != 1 {
		t.Fatalf("reconciliation panel = %#v", panel.Reconciliation)
	}
	if panel.ToggleInventory == nil || len(panel.ToggleInventory.FlipOn) != 1 || len(panel.ToggleInventory.LeaveDark) != 1 {
		t.Fatalf("toggle inventory panel = %#v", panel.ToggleInventory)
	}
	if len(panel.NeedsHuman) != 1 || !strings.Contains(panel.NeedsHuman[0].Detail, "routing failed") {
		t.Fatalf("needs-human panel = %#v", panel.NeedsHuman)
	}
	if len(panel.Failed) == 0 || panel.Failed[0].Step != "promote" || !strings.Contains(panel.Failed[0].Detail, "route kick-back PR #101") {
		t.Fatalf("failed panel = %#v", panel.Failed)
	}
	data, err := MarshalPromoteJSON(report)
	if err != nil {
		t.Fatalf("MarshalPromoteJSON: %v", err)
	}
	for _, want := range []string{`"go_no_go_panel"`, `"reconciliation"`, `"toggle_inventory"`, `"needs_human"`, `"failed"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("promote JSON missing %q:\n%s", want, data)
		}
	}
	rendered := RenderPromoteText(report)
	for _, want := range []string{"Go/no-go panel", "reconciliation status=needs-human", "toggle inventory flip_on=1 leave_dark=1", "needs-human items=1", "failed items="} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered report missing %q:\n%s", want, rendered)
		}
	}
}

func TestPromoteGoNoGoPanelOmitsAbsentSections(t *testing.T) {
	repo := t.TempDir()
	runID := "run-test-promote-panel-partial"
	writer := &recordingPromotionWriter{}

	report, err := Promote(context.Background(), PromoteOptions{
		Writer:        writer,
		RepoPath:      repo,
		RunID:         runID,
		PreProdBranch: "pre-prod",
		Gate:          "human-merge",
		Clock:         fixedPromoteClock,
		StatePush:     promoteTestStatePush(t, repo, runID),
		ParallelRun: &PromoteParallelRunConfig{
			Contract: promoteTestParallelRunContract(),
			Input: equivalence.ParallelRunInput{
				Old: []equivalence.ParallelRunRecord{{Key: "request-1", Value: map[string]any{"score": 1.0}}},
				New: []equivalence.ParallelRunRecord{{Key: "request-1", Value: map[string]any{"score": 1.0}}},
			},
		},
	})
	if err != nil {
		t.Fatalf("Promote returned error: %v", err)
	}
	if report.Status != PromoteStatusSucceeded {
		t.Fatalf("status = %s, want succeeded", report.Status)
	}
	if report.GoNoGoPanel == nil || report.GoNoGoPanel.Reconciliation == nil {
		t.Fatalf("go/no-go panel = %#v, want reconciliation only", report.GoNoGoPanel)
	}
	if report.GoNoGoPanel.ToggleInventory != nil || len(report.GoNoGoPanel.NeedsHuman) != 0 || len(report.GoNoGoPanel.Failed) != 0 {
		t.Fatalf("go/no-go panel has absent sections: %#v", report.GoNoGoPanel)
	}
	data, err := MarshalPromoteJSON(report)
	if err != nil {
		t.Fatalf("MarshalPromoteJSON: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("Unmarshal promote JSON: %v\n%s", err, data)
	}
	panel, ok := wire["go_no_go_panel"].(map[string]any)
	if !ok {
		t.Fatalf("go_no_go_panel missing or malformed in JSON:\n%s", data)
	}
	if _, ok := panel["reconciliation"]; !ok {
		t.Fatalf("go_no_go_panel missing reconciliation:\n%s", data)
	}
	if _, ok := wire["toggle_inventory"]; ok {
		t.Fatalf("promote JSON included empty top-level toggle_inventory:\n%s", data)
	}
	for _, absent := range []string{"toggle_inventory", "needs_human", "failed"} {
		if _, ok := panel[absent]; ok {
			t.Fatalf("go_no_go_panel included absent section %q:\n%s", absent, data)
		}
	}
	rendered := RenderPromoteText(report)
	if !strings.Contains(rendered, "Go/no-go panel") || !strings.Contains(rendered, "reconciliation status=pass") {
		t.Fatalf("rendered report missing partial panel:\n%s", rendered)
	}
	for _, absent := range []string{"needs-human items=", "failed items="} {
		if strings.Contains(rendered, absent) {
			t.Fatalf("rendered partial panel included absent section %q:\n%s", absent, rendered)
		}
	}
}

func TestPromoteGoNoGoPanelIncludesReconciliationLoadError(t *testing.T) {
	repo := t.TempDir()
	runID := "run-test-promote-reconciliation-error"
	writer := &recordingPromotionWriter{}

	report, err := Promote(context.Background(), PromoteOptions{
		Writer:        writer,
		RepoPath:      repo,
		RunID:         runID,
		PreProdBranch: "pre-prod",
		Gate:          "human-merge",
		Clock:         fixedPromoteClock,
		StatePush:     promoteTestStatePush(t, repo, runID),
		ParallelRun: &PromoteParallelRunConfig{
			Contract: promoteTestParallelRunContract(),
		},
		ReconcileParallelRun: func(equivalence.Contract, equivalence.ParallelRunInput) (equivalence.ParallelRunReport, error) {
			return equivalence.ParallelRunReport{}, errors.New("missing reconciliation artifact")
		},
	})
	if err != nil {
		t.Fatalf("Promote returned error: %v", err)
	}
	if report.Status != PromoteStatusSucceeded {
		t.Fatalf("status = %s, want succeeded", report.Status)
	}
	if report.GoNoGoPanel == nil || len(report.GoNoGoPanel.NeedsHuman) != 1 {
		t.Fatalf("go/no-go panel = %#v, want reconciliation needs-human", report.GoNoGoPanel)
	}
	item := report.GoNoGoPanel.NeedsHuman[0]
	if item.Step != "parallel-run-reconciliation" || !strings.Contains(item.Detail, "missing reconciliation artifact") {
		t.Fatalf("needs-human panel item = %#v", item)
	}
	if report.GoNoGoPanel.Reconciliation != nil {
		t.Fatalf("reconciliation panel = %#v, want nil after load error", report.GoNoGoPanel.Reconciliation)
	}
}

func TestPromoteGoNoGoPanelIncludesStatePushFailure(t *testing.T) {
	repo := t.TempDir()
	runID := "run-test-promote-state-push-failure"
	writer := &recordingPromotionWriter{}

	report, err := Promote(context.Background(), PromoteOptions{
		Writer:        writer,
		RepoPath:      repo,
		RunID:         runID,
		PreProdBranch: "pre-prod",
		Gate:          "human-merge",
		Clock:         fixedPromoteClock,
		StatePush: func(context.Context, statebranch.PushOptions) (statebranch.PushResult, error) {
			return statebranch.PushResult{
				RepoPath: repo,
				RunID:    runID,
				Branch:   statebranch.DefaultBranch,
				Remote:   statebranch.DefaultRemote,
				Files:    []string{},
			}, errors.New("state branch push failed")
		},
	})
	if err != nil {
		t.Fatalf("Promote returned error: %v", err)
	}
	if report.Status != PromoteStatusFailed || report.StatePush == nil || report.StatePush.Error == "" {
		t.Fatalf("report = %#v, want failed state push report", report)
	}
	if report.GoNoGoPanel == nil || len(report.GoNoGoPanel.Failed) != 1 {
		t.Fatalf("go/no-go panel = %#v, want state-push failure", report.GoNoGoPanel)
	}
	failed := report.GoNoGoPanel.Failed[0]
	if failed.Step != "state-push" || failed.Status != PromoteStatusFailed || !strings.Contains(failed.Detail, "state branch push failed") {
		t.Fatalf("failed panel item = %#v", failed)
	}
}

func TestPromotePreconditionRejectionsAreLedgeredAsFailed(t *testing.T) {
	tests := []struct {
		name          string
		writer        PromotionWriter
		preProdBranch string
		gate          string
		wantErr       string
	}{
		{
			name:          "nil-writer",
			writer:        nil,
			preProdBranch: "pre-prod",
			gate:          "human-merge",
			wantErr:       "promotion writer is required",
		},
		{
			name:          "empty-pre-prod",
			writer:        &recordingPromotionWriter{},
			preProdBranch: "",
			gate:          "human-merge",
			wantErr:       "pre-prod branch is required",
		},
		{
			name:          "reserved-main",
			writer:        &recordingPromotionWriter{},
			preProdBranch: "main",
			gate:          "human-merge",
			wantErr:       "reserved for human promotion",
		},
		{
			name:          "reserved-production",
			writer:        &recordingPromotionWriter{},
			preProdBranch: "production",
			gate:          "human-merge",
			wantErr:       "reserved for human promotion",
		},
		{
			name:          "non-human-gate",
			writer:        &recordingPromotionWriter{},
			preProdBranch: "pre-prod",
			gate:          "auto",
			wantErr:       "requires adapters.gate human-merge",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := t.TempDir()
			runID := "run-test-promote-" + tt.name

			report, err := Promote(context.Background(), PromoteOptions{
				Writer:        tt.writer,
				RepoPath:      repo,
				RunID:         runID,
				PreProdBranch: tt.preProdBranch,
				Gate:          tt.gate,
				Clock:         fixedPromoteClock,
				StatePush:     promoteTestStatePush(t, repo, runID),
			})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Promote error = %v, want %q", err, tt.wantErr)
			}
			if report.Status != PromoteStatusFailed || report.Summary.FailureCount == 0 {
				t.Fatalf("report = %#v, want failed with a failure count", report)
			}
			if report.StatePush == nil || !report.StatePush.Pushed {
				t.Fatalf("state push = %#v, want pushed ledger", report.StatePush)
			}
			if writer, ok := tt.writer.(*recordingPromotionWriter); ok && len(writer.calls) != 0 {
				t.Fatalf("writer calls = %#v, want none", writer.calls)
			}
			event := readPromoteEvents(t, repo, runID)
			for _, want := range []string{`"event":"promote.attempt"`, `"outcome":"failed"`, tt.wantErr} {
				if !strings.Contains(event, want) {
					t.Fatalf("precondition ledger missing %q:\n%s", want, event)
				}
			}
		})
	}
}

func TestNormalizeKickBackItemsCanonicalizesPRFormsAndNumericSHA(t *testing.T) {
	got := normalizeKickBackItems([]string{" #101 ", "101", "pr:101", "PR#101", "pr:#101", "abcdef0", "ABCDEF0", "1234567", "1234567"})
	want := []string{"#101", "abcdef0", "1234567"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeKickBackItems = %#v, want %#v", got, want)
	}
}

type recordingPromotionWriter struct {
	calls           []string
	kickErr         error
	routeErr        error
	alreadyUpToDate bool
}

func (w *recordingPromotionWriter) BranchHeadSHA(_ context.Context, branch string) (string, error) {
	w.calls = append(w.calls, "head:"+branch)
	return branch + "-prior-sha", nil
}

func (w *recordingPromotionWriter) KickBackFromPreProd(_ context.Context, item, preProdBranch string) (gh.PreProdKickBackResult, error) {
	w.calls = append(w.calls, "kick:"+item+":"+preProdBranch)
	if w.kickErr != nil {
		return gh.PreProdKickBackResult{}, w.kickErr
	}
	return gh.PreProdKickBackResult{
		Item:        item,
		PRNumber:    101,
		Branch:      preProdBranch,
		RevertedSHA: "merge-" + item,
		SHA:         "revert-" + item,
	}, nil
}

func (w *recordingPromotionWriter) RouteKickBackToNeedsHuman(_ context.Context, prNumber int) (gh.NeedsHumanRouteResult, error) {
	w.calls = append(w.calls, "route:"+strconv.Itoa(prNumber))
	if w.routeErr != nil {
		return gh.NeedsHumanRouteResult{}, w.routeErr
	}
	return gh.NeedsHumanRouteResult{
		PRNumber:     prNumber,
		IssueNumbers: []int{prNumber},
		Label:        "needs-human",
	}, nil
}

func (w *recordingPromotionWriter) PromotePreProdToMain(_ context.Context, preProdBranch string) (gh.MainPromotionResult, error) {
	w.calls = append(w.calls, "promote:"+preProdBranch)
	sha := "main-sha"
	if w.alreadyUpToDate {
		sha = ""
	}
	return gh.MainPromotionResult{
		PreProdBranch:   preProdBranch,
		MainBranch:      "main",
		Head:            preProdBranch,
		SHA:             sha,
		AlreadyUpToDate: w.alreadyUpToDate,
	}, nil
}

func (w *recordingPromotionWriter) SyncPreProdFromMain(_ context.Context, preProdBranch string) (gh.PreProdSyncResult, error) {
	w.calls = append(w.calls, "sync:"+preProdBranch)
	return gh.PreProdSyncResult{
		PreProdBranch: preProdBranch,
		MainBranch:    "main",
		SHA:           "main-sha",
	}, nil
}

func fixedPromoteClock() time.Time {
	return time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
}

func promoteTestParallelRunContract() equivalence.Contract {
	return equivalence.Contract{
		Version: equivalence.ContractVersion,
		Partition: equivalence.Partition{
			ReadSlices: []string{"reader-api"},
		},
		Tolerance: equivalence.ToleranceRules{
			FloatPrecision: &equivalence.FloatPrecision{
				Absolute: 0.1,
				Paths:    []string{"$.score"},
			},
		},
	}
}

func promoteTestStatePush(t *testing.T, repo, runID string) StatePushFunc {
	t.Helper()
	return func(_ context.Context, opts statebranch.PushOptions) (statebranch.PushResult, error) {
		if opts.RepoPath != repo || opts.RunID != runID {
			t.Fatalf("state push opts = %#v, want repo=%q runID=%q", opts, repo, runID)
		}
		event := readPromoteEvents(t, repo, runID)
		if !strings.Contains(event, `"event":"promote.attempt"`) {
			t.Fatalf("state push happened before promote event was appended:\n%s", event)
		}
		return statebranch.PushResult{
			RepoPath:  repo,
			RunID:     runID,
			Branch:    statebranch.DefaultBranch,
			Remote:    statebranch.DefaultRemote,
			Committed: true,
			Pushed:    true,
			Files:     []string{"runs/" + runID + "/events.jsonl"},
		}, nil
	}
}

func readPromoteEvents(t *testing.T, repo, runID string) string {
	t.Helper()
	data, err := os.ReadFile(state.EventsPath(repo, runID))
	if err != nil {
		t.Fatalf("ReadFile promote events: %v", err)
	}
	return string(data)
}
