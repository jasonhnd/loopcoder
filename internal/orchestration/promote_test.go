package orchestration

import (
	"context"
	"errors"
	"reflect"
	"testing"

	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
)

func TestPromoteWholeBatchPromotesPreProdToMainAndSyncs(t *testing.T) {
	writer := &recordingPromotionWriter{}

	report, err := Promote(context.Background(), PromoteOptions{
		Writer:        writer,
		RepoPath:      "repo",
		PreProdBranch: "pre-prod",
		Gate:          "human-merge",
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
}

func TestPromoteKickBackRevertsItemsBeforePromotingRemainder(t *testing.T) {
	writer := &recordingPromotionWriter{}

	report, err := Promote(context.Background(), PromoteOptions{
		Writer:        writer,
		RepoPath:      "repo",
		PreProdBranch: "pre-prod",
		Gate:          "human-merge",
		KickBackItems: []string{"#101", "merge-sha"},
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
}

func TestPromoteStopsBeforeMainWhenKickBackFails(t *testing.T) {
	writer := &recordingPromotionWriter{kickErr: errors.New("revert failed")}

	report, err := Promote(context.Background(), PromoteOptions{
		Writer:        writer,
		RepoPath:      "repo",
		PreProdBranch: "pre-prod",
		Gate:          "human-merge",
		KickBackItems: []string{"#101"},
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
}

type recordingPromotionWriter struct {
	calls   []string
	kickErr error
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
	return gh.MainPromotionResult{
		PreProdBranch: preProdBranch,
		MainBranch:    "main",
		Head:          preProdBranch,
		SHA:           "main-sha",
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
