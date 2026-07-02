package orchestration

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

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
	if !reflect.DeepEqual(writer.calls, []string{"promote:pre-prod", "sync:pre-prod"}) {
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
	for _, want := range []string{`"event":"promote.attempt"`, `"outcome":"promoted"`, `"sha":"main-sha"`} {
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
	wantCalls := []string{"kick:#101:pre-prod", "kick:merge-sha:pre-prod", "promote:pre-prod", "sync:pre-prod"}
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
	if !strings.Contains(rendered, "Needs human") || !strings.Contains(rendered, "return item to needs-human") {
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
}

type recordingPromotionWriter struct {
	calls           []string
	kickErr         error
	alreadyUpToDate bool
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
