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
	"github.com/jasonhnd/loopcoder/internal/config"
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

func TestEvaluateAutoGateConjunctiveFailClosed(t *testing.T) {
	truthy := true
	falsey := false
	allTrue := AutoGateInputs{
		CIGreen:         &truthy,
		VerdictPass:     &truthy,
		EvidencePresent: &truthy,
		RedLineClean:    &truthy,
	}
	tests := []struct {
		name       string
		input      AutoGateInputs
		wantAllow  bool
		wantReason string
	}{
		{
			name:      "all-four-true",
			input:     allTrue,
			wantAllow: true,
		},
		{
			name:       "ci-false",
			input:      AutoGateInputs{CIGreen: &falsey, VerdictPass: &truthy, EvidencePresent: &truthy, RedLineClean: &truthy},
			wantReason: "CI",
		},
		{
			name:       "verdict-false",
			input:      AutoGateInputs{CIGreen: &truthy, VerdictPass: &falsey, EvidencePresent: &truthy, RedLineClean: &truthy},
			wantReason: "verdict",
		},
		{
			name:       "evidence-false",
			input:      AutoGateInputs{CIGreen: &truthy, VerdictPass: &truthy, EvidencePresent: &falsey, RedLineClean: &truthy},
			wantReason: "evidence",
		},
		{
			name:       "red-line-false",
			input:      AutoGateInputs{CIGreen: &truthy, VerdictPass: &truthy, EvidencePresent: &truthy, RedLineClean: &falsey},
			wantReason: "red-line floor",
		},
		{
			name:       "ci-missing",
			input:      AutoGateInputs{VerdictPass: &truthy, EvidencePresent: &truthy, RedLineClean: &truthy},
			wantReason: "CI",
		},
		{
			name:       "verdict-missing",
			input:      AutoGateInputs{CIGreen: &truthy, EvidencePresent: &truthy, RedLineClean: &truthy},
			wantReason: "verdict",
		},
		{
			name:       "evidence-missing",
			input:      AutoGateInputs{CIGreen: &truthy, VerdictPass: &truthy, RedLineClean: &truthy},
			wantReason: "evidence",
		},
		{
			name:       "red-line-missing",
			input:      AutoGateInputs{CIGreen: &truthy, VerdictPass: &truthy, EvidencePresent: &truthy},
			wantReason: "red-line floor",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allowed, reason := evaluateAutoGate(tt.input)
			if allowed != tt.wantAllow {
				t.Fatalf("allowed = %t, want %t (reason %q)", allowed, tt.wantAllow, reason)
			}
			if !tt.wantAllow && !strings.Contains(reason, tt.wantReason) {
				t.Fatalf("reason = %q, want it to name %q", reason, tt.wantReason)
			}
		})
	}
}

func TestPromoteAutoGatePath(t *testing.T) {
	truthy := true
	falsey := false
	allTrue := &AutoGateInputs{
		CIGreen:         &truthy,
		VerdictPass:     &truthy,
		EvidencePresent: &truthy,
		RedLineClean:    &truthy,
	}
	tests := []struct {
		name           string
		gate           string
		autoGate       *AutoGateInputs
		wantErr        string
		wantStatus     string
		wantCalls      []string
		wantNeedsHuman string
	}{
		{
			name:       "human-merge-unchanged",
			gate:       GateHumanMerge,
			wantStatus: PromoteStatusSucceeded,
			wantCalls:  []string{"head:main", "promote:pre-prod", "sync:pre-prod"},
		},
		{
			name:           "auto-inputs-unavailable",
			gate:           GateAuto,
			wantStatus:     PromoteStatusFailed,
			wantNeedsHuman: "auto-gate inputs unavailable",
		},
		{
			name:       "auto-all-true-promotes",
			gate:       GateAuto,
			autoGate:   allTrue,
			wantStatus: PromoteStatusSucceeded,
			wantCalls:  []string{"head:main", "promote:pre-prod", "checks:main", "sync:pre-prod"},
		},
		{
			name: "auto-false-signal-needs-human",
			gate: GateAuto,
			autoGate: &AutoGateInputs{
				CIGreen:         &falsey,
				VerdictPass:     &truthy,
				EvidencePresent: &truthy,
				RedLineClean:    &truthy,
			},
			wantStatus:     PromoteStatusFailed,
			wantNeedsHuman: "CI green is false",
		},
		{
			name: "auto-missing-signal-needs-human",
			gate: GateAuto,
			autoGate: &AutoGateInputs{
				CIGreen:      &truthy,
				VerdictPass:  &truthy,
				RedLineClean: &truthy,
			},
			wantStatus:     PromoteStatusFailed,
			wantNeedsHuman: "evidence present signal missing",
		},
		{
			name:       "bogus-gate-fail-closed",
			gate:       "bogus",
			wantErr:    "allowed values: human-merge, auto",
			wantStatus: PromoteStatusFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := t.TempDir()
			runID := "run-test-promote-auto-" + tt.name
			writer := &recordingPromotionWriter{}

			report, err := Promote(context.Background(), PromoteOptions{
				Writer:         writer,
				RepoPath:       repo,
				RunID:          runID,
				PreProdBranch:  "pre-prod",
				Gate:           tt.gate,
				AutoGate:       tt.autoGate,
				RequiredChecks: []string{"verify"},
				Clock:          fixedPromoteClock,
				StatePush:      promoteTestStatePush(t, repo, runID),
			})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Promote error = %v, want %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("Promote returned error: %v", err)
			}
			if report.Status != tt.wantStatus {
				t.Fatalf("status = %s, want %s", report.Status, tt.wantStatus)
			}
			if !reflect.DeepEqual(writer.calls, tt.wantCalls) {
				t.Fatalf("calls = %#v, want %#v", writer.calls, tt.wantCalls)
			}
			if tt.wantNeedsHuman == "" {
				if len(report.NeedsHuman) != 0 {
					t.Fatalf("needs-human = %#v, want none", report.NeedsHuman)
				}
			} else {
				if len(report.NeedsHuman) != 1 || report.NeedsHuman[0].Step != "auto-gate" || !strings.Contains(report.NeedsHuman[0].Detail, tt.wantNeedsHuman) {
					t.Fatalf("needs-human = %#v, want auto-gate detail containing %q", report.NeedsHuman, tt.wantNeedsHuman)
				}
			}
			if len(tt.wantCalls) == 0 {
				if report.Summary.PromotedCount != 0 || strings.TrimSpace(report.Promoted.SHA) != "" {
					t.Fatalf("promoted = %#v summary=%#v, want no main merge", report.Promoted, report.Summary)
				}
			}
			event := readPromoteEvents(t, repo, runID)
			if !strings.Contains(event, `"event":"promote.attempt"`) {
				t.Fatalf("promote ledger missing event:\n%s", event)
			}
		})
	}
}

func TestPromoteAutoGateResolverWiring(t *testing.T) {
	truthy := true
	allTrue := &AutoGateInputs{
		CIGreen:         &truthy,
		VerdictPass:     &truthy,
		EvidencePresent: &truthy,
		RedLineClean:    &truthy,
	}
	tests := []struct {
		name              string
		gate              string
		autoGate          *AutoGateInputs
		resolveAutoGate   func(ctx context.Context) (*AutoGateInputs, error)
		wantResolverCalls int
		wantCalls         []string
		wantNeedsHuman    string
	}{
		{
			name: "auto-resolves-inputs-before-gate",
			gate: GateAuto,
			resolveAutoGate: func(context.Context) (*AutoGateInputs, error) {
				return allTrue, nil
			},
			wantResolverCalls: 1,
			wantCalls:         []string{"head:main", "promote:pre-prod", "checks:main", "sync:pre-prod"},
		},
		{
			name: "auto-resolver-error-fail-closed",
			gate: GateAuto,
			resolveAutoGate: func(context.Context) (*AutoGateInputs, error) {
				return nil, errors.New("resolver unavailable")
			},
			wantResolverCalls: 1,
			wantNeedsHuman:    "resolver unavailable",
		},
		{
			name:     "auto-direct-inputs-skip-resolver",
			gate:     GateAuto,
			autoGate: allTrue,
			resolveAutoGate: func(context.Context) (*AutoGateInputs, error) {
				return nil, errors.New("must not be called")
			},
			wantCalls: []string{"head:main", "promote:pre-prod", "checks:main", "sync:pre-prod"},
		},
		{
			name: "human-merge-skips-resolver",
			gate: GateHumanMerge,
			resolveAutoGate: func(context.Context) (*AutoGateInputs, error) {
				return nil, errors.New("must not be called")
			},
			wantCalls: []string{"head:main", "promote:pre-prod", "sync:pre-prod"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := t.TempDir()
			runID := "run-test-promote-resolver-" + tt.name
			writer := &recordingPromotionWriter{}
			resolverCalls := 0
			resolver := func(ctx context.Context) (*AutoGateInputs, error) {
				resolverCalls++
				return tt.resolveAutoGate(ctx)
			}

			report, err := Promote(context.Background(), PromoteOptions{
				Writer:          writer,
				RepoPath:        repo,
				RunID:           runID,
				PreProdBranch:   "pre-prod",
				Gate:            tt.gate,
				AutoGate:        tt.autoGate,
				ResolveAutoGate: resolver,
				RequiredChecks:  []string{"verify"},
				Clock:           fixedPromoteClock,
				StatePush:       promoteTestStatePush(t, repo, runID),
			})
			if err != nil {
				t.Fatalf("Promote returned error: %v", err)
			}
			if resolverCalls != tt.wantResolverCalls {
				t.Fatalf("resolver calls = %d, want %d", resolverCalls, tt.wantResolverCalls)
			}
			if !reflect.DeepEqual(writer.calls, tt.wantCalls) {
				t.Fatalf("writer calls = %#v, want %#v", writer.calls, tt.wantCalls)
			}
			if tt.wantNeedsHuman == "" {
				if len(report.NeedsHuman) != 0 {
					t.Fatalf("needs-human = %#v, want none", report.NeedsHuman)
				}
				return
			}
			if len(report.NeedsHuman) != 1 || report.NeedsHuman[0].Step != "auto-gate" || !strings.Contains(report.NeedsHuman[0].Detail, tt.wantNeedsHuman) {
				t.Fatalf("needs-human = %#v, want auto-gate detail containing %q", report.NeedsHuman, tt.wantNeedsHuman)
			}
			if report.Summary.PromotedCount != 0 || strings.TrimSpace(report.Promoted.SHA) != "" {
				t.Fatalf("promoted = %#v summary=%#v, want no main merge", report.Promoted, report.Summary)
			}
		})
	}
}

func TestResolvePromoteAutoGateFromSignals(t *testing.T) {
	evidence := []config.EvidenceArtifact{{
		ProjectType: "cli",
		TestResults: "go test ./...",
	}}
	pending := []TickPendingPromotion{{PRNumber: 418, Branch: "pre-prod", SHA: "merge-sha"}}
	tests := []struct {
		name           string
		writer         recordingPromotionWriter
		requiredChecks []string
		evidence       []config.EvidenceArtifact
		pending        []TickPendingPromotion
		wantCI         *bool
		wantVerdict    *bool
		wantEvidence   *bool
		wantRedLine    *bool
	}{
		{
			name:           "all-healthy",
			requiredChecks: []string{"verify"},
			evidence:       evidence,
			pending:        pending,
			wantCI:         promoteBoolPtr(true),
			wantVerdict:    promoteBoolPtr(true),
			wantEvidence:   promoteBoolPtr(true),
			wantRedLine:    promoteBoolPtr(true),
		},
		{
			name: "ci-red-false",
			writer: recordingPromotionWriter{branchChecks: gh.BranchChecksResult{
				Branch:  "pre-prod",
				HeadSHA: "preprod-sha",
				Checks:  []gh.Check{{Name: "verify", State: "failure", Bucket: "fail"}},
			}},
			requiredChecks: []string{"verify"},
			evidence:       evidence,
			pending:        pending,
			wantCI:         promoteBoolPtr(false),
			wantVerdict:    promoteBoolPtr(true),
			wantEvidence:   promoteBoolPtr(true),
			wantRedLine:    promoteBoolPtr(true),
		},
		{
			name: "ci-pending-nil",
			writer: recordingPromotionWriter{branchChecks: gh.BranchChecksResult{
				Branch:  "pre-prod",
				HeadSHA: "preprod-sha",
				Checks:  []gh.Check{{Name: "verify", State: "queued", Bucket: "pending"}},
			}},
			requiredChecks: []string{"verify"},
			evidence:       evidence,
			pending:        pending,
			wantVerdict:    promoteBoolPtr(true),
			wantEvidence:   promoteBoolPtr(true),
			wantRedLine:    promoteBoolPtr(true),
		},
		{
			name:           "ci-error-nil",
			writer:         recordingPromotionWriter{branchChecksErr: errors.New("checks unavailable")},
			requiredChecks: []string{"verify"},
			evidence:       evidence,
			pending:        pending,
			wantVerdict:    promoteBoolPtr(true),
			wantEvidence:   promoteBoolPtr(true),
			wantRedLine:    promoteBoolPtr(true),
		},
		{
			name:           "empty-ledger-verdict-nil",
			requiredChecks: []string{"verify"},
			evidence:       evidence,
			wantCI:         promoteBoolPtr(true),
			wantEvidence:   promoteBoolPtr(true),
			wantRedLine:    promoteBoolPtr(true),
		},
		{
			name: "core-path-red-line-false",
			writer: recordingPromotionWriter{
				compareFiles: []string{"internal/orchestration/promote.go"},
				compareDiff:  "diff --git a/internal/orchestration/promote.go b/internal/orchestration/promote.go\n",
			},
			requiredChecks: []string{"verify"},
			evidence:       evidence,
			pending:        pending,
			wantCI:         promoteBoolPtr(true),
			wantVerdict:    promoteBoolPtr(true),
			wantEvidence:   promoteBoolPtr(true),
			wantRedLine:    promoteBoolPtr(false),
		},
		{
			name:           "compare-error-red-line-nil",
			writer:         recordingPromotionWriter{compareErr: errors.New("compare unavailable")},
			requiredChecks: []string{"verify"},
			evidence:       evidence,
			pending:        pending,
			wantCI:         promoteBoolPtr(true),
			wantVerdict:    promoteBoolPtr(true),
			wantEvidence:   promoteBoolPtr(true),
		},
		{
			name:           "no-configured-evidence-false",
			requiredChecks: []string{"verify"},
			pending:        pending,
			wantCI:         promoteBoolPtr(true),
			wantVerdict:    promoteBoolPtr(true),
			wantEvidence:   promoteBoolPtr(false),
			wantRedLine:    promoteBoolPtr(true),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writer := tt.writer
			got, err := ResolvePromoteAutoGate(context.Background(), AutoGateResolverOptions{
				Writer:             &writer,
				RepoPath:           t.TempDir(),
				PreProdBranch:      "pre-prod",
				RequiredChecks:     tt.requiredChecks,
				ConfiguredEvidence: tt.evidence,
				LoadPendingPromotionLedger: func(repoPath, preProdBranch string) []TickPendingPromotion {
					if strings.TrimSpace(repoPath) == "" || preProdBranch != "pre-prod" {
						t.Fatalf("ledger loader args = %q %q", repoPath, preProdBranch)
					}
					return tt.pending
				},
			})
			if err != nil {
				t.Fatalf("ResolvePromoteAutoGate returned error: %v", err)
			}
			assertPromoteBoolPtr(t, "CIGreen", got.CIGreen, tt.wantCI)
			assertPromoteBoolPtr(t, "VerdictPass", got.VerdictPass, tt.wantVerdict)
			assertPromoteBoolPtr(t, "EvidencePresent", got.EvidencePresent, tt.wantEvidence)
			assertPromoteBoolPtr(t, "RedLineClean", got.RedLineClean, tt.wantRedLine)
		})
	}
}

func TestPromoteAutoProductionHealthRollback(t *testing.T) {
	truthy := true
	allTrue := &AutoGateInputs{
		CIGreen:         &truthy,
		VerdictPass:     &truthy,
		EvidencePresent: &truthy,
		RedLineClean:    &truthy,
	}
	tests := []struct {
		name               string
		gate               string
		checks             gh.BranchChecksResult
		mainPriorSHA       string
		revertErr          error
		wantCalls          []string
		wantRollback       bool
		wantRollbackEvent  bool
		wantRollbackLedger []string
		wantRollbackError  string
		wantNeedsHumanStep string
		wantNeedsHumanText string
		wantStatus         string
		wantFailureCount   int
	}{
		{
			name: "auto-green-no-rollback",
			gate: GateAuto,
			checks: gh.BranchChecksResult{
				Branch:  "main",
				HeadSHA: "main-sha",
				Checks:  []gh.Check{{Name: "verify", State: "success", Bucket: "pass"}},
			},
			wantCalls:  []string{"head:main", "promote:pre-prod", "checks:main", "sync:pre-prod"},
			wantStatus: PromoteStatusSucceeded,
		},
		{
			name: "auto-red-at-promoted-sha-rolls-back",
			gate: GateAuto,
			checks: gh.BranchChecksResult{
				Branch:  "main",
				HeadSHA: "main-sha",
				Checks:  []gh.Check{{Name: "verify", State: "failure", Bucket: "fail"}},
			},
			wantCalls:          []string{"head:main", "promote:pre-prod", "checks:main", "revert:main:main-sha:main-prior-sha", "sync:pre-prod"},
			wantRollback:       true,
			wantRollbackEvent:  true,
			wantNeedsHumanStep: "production-rollback",
			wantStatus:         PromoteStatusSucceeded,
		},
		{
			name:      "auto-red-at-promoted-sha-rollback-error-needs-human",
			gate:      GateAuto,
			revertErr: errors.New("rollback command failed"),
			checks: gh.BranchChecksResult{
				Branch:  "main",
				HeadSHA: "main-sha",
				Checks:  []gh.Check{{Name: "verify", State: "failure", Bucket: "fail"}},
			},
			wantCalls:          []string{"head:main", "promote:pre-prod", "checks:main", "revert:main:main-sha:main-prior-sha", "sync:pre-prod"},
			wantRollback:       true,
			wantRollbackEvent:  true,
			wantRollbackLedger: []string{`"outcome":"failed"`, `"merge_commit":"main-sha"`, `"prior_stable_commit":"main-prior-sha"`, `"error":"rollback command failed"`},
			wantRollbackError:  "rollback command failed",
			wantNeedsHumanStep: "production-rollback",
			wantNeedsHumanText: "automatic rollback failed: rollback command failed",
			wantStatus:         PromoteStatusFailed,
			wantFailureCount:   1,
		},
		{
			name:         "auto-red-with-empty-prior-stable-needs-human-no-rollback",
			gate:         GateAuto,
			mainPriorSHA: "",
			checks: gh.BranchChecksResult{
				Branch:  "main",
				HeadSHA: "main-sha",
				Checks:  []gh.Check{{Name: "verify", State: "failure", Bucket: "fail"}},
			},
			wantCalls:          []string{"head:main", "promote:pre-prod", "checks:main", "sync:pre-prod"},
			wantNeedsHumanStep: "production-rollback",
			wantStatus:         PromoteStatusSucceeded,
		},
		{
			name: "auto-pending-leaves-main-alone",
			gate: GateAuto,
			checks: gh.BranchChecksResult{
				Branch:  "main",
				HeadSHA: "main-sha",
				Checks:  []gh.Check{{Name: "verify", State: "pending", Bucket: "pending"}},
			},
			wantCalls:  []string{"head:main", "promote:pre-prod", "checks:main", "sync:pre-prod"},
			wantStatus: PromoteStatusSucceeded,
		},
		{
			name: "human-merge-red-main-does-not-health-check-or-rollback",
			gate: GateHumanMerge,
			checks: gh.BranchChecksResult{
				Branch:  "main",
				HeadSHA: "main-sha",
				Checks:  []gh.Check{{Name: "verify", State: "failure", Bucket: "fail"}},
			},
			wantCalls:  []string{"head:main", "promote:pre-prod", "sync:pre-prod"},
			wantStatus: PromoteStatusSucceeded,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := t.TempDir()
			runID := "run-test-promote-rollback-" + tt.name
			writer := &recordingPromotionWriter{branchChecks: tt.checks, revertErr: tt.revertErr}
			if tt.mainPriorSHA != "" || strings.Contains(tt.name, "empty-prior") {
				writer.mainHeadSHASet = true
				writer.mainHeadSHA = tt.mainPriorSHA
			}

			report, err := Promote(context.Background(), PromoteOptions{
				Writer:         writer,
				RepoPath:       repo,
				RunID:          runID,
				PreProdBranch:  "pre-prod",
				Gate:           tt.gate,
				AutoGate:       allTrue,
				RequiredChecks: []string{"verify"},
				Clock:          fixedPromoteClock,
				StatePush:      promoteTestStatePush(t, repo, runID),
			})
			if err != nil {
				t.Fatalf("Promote returned error: %v", err)
			}
			if report.Status != tt.wantStatus {
				t.Fatalf("status = %s, want %s", report.Status, tt.wantStatus)
			}
			if report.Summary.FailureCount != tt.wantFailureCount {
				t.Fatalf("failure count = %d, want %d; summary=%#v", report.Summary.FailureCount, tt.wantFailureCount, report.Summary)
			}
			if !reflect.DeepEqual(writer.calls, tt.wantCalls) {
				t.Fatalf("calls = %#v, want %#v", writer.calls, tt.wantCalls)
			}
			if (report.Rollback != nil) != tt.wantRollback {
				t.Fatalf("rollback = %#v, want present=%t", report.Rollback, tt.wantRollback)
			}
			if tt.wantRollback {
				if report.Rollback.MergeCommit != "main-sha" || report.Rollback.PriorStableCommit != "main-prior-sha" {
					t.Fatalf("rollback = %#v, want merge/prior SHAs", report.Rollback)
				}
				if tt.wantRollbackError != "" {
					if report.Rollback.Status != PromoteStatusFailed || !strings.Contains(report.Rollback.Error, tt.wantRollbackError) {
						t.Fatalf("rollback = %#v, want failed error containing %q", report.Rollback, tt.wantRollbackError)
					}
				} else if report.Rollback.RevertSHA != "production-revert-sha" {
					t.Fatalf("rollback = %#v, want revert SHA", report.Rollback)
				}
			}
			if tt.wantNeedsHumanStep != "" {
				if !promoteNeedsHumanHasStepDetail(report.NeedsHuman, tt.wantNeedsHumanStep, tt.wantNeedsHumanText) {
					t.Fatalf("needs-human = %#v, want step %q detail containing %q", report.NeedsHuman, tt.wantNeedsHumanStep, tt.wantNeedsHumanText)
				}
			} else if len(report.NeedsHuman) != 0 {
				t.Fatalf("needs-human = %#v, want none", report.NeedsHuman)
			}
			events := readPromoteEvents(t, repo, runID)
			hasRollbackEvent := strings.Contains(events, `"event":"promote.rollback"`)
			if hasRollbackEvent != tt.wantRollbackEvent {
				t.Fatalf("rollback event present = %t, want %t:\n%s", hasRollbackEvent, tt.wantRollbackEvent, events)
			}
			if tt.wantRollbackEvent {
				wantRollbackLedger := tt.wantRollbackLedger
				if len(wantRollbackLedger) == 0 {
					wantRollbackLedger = []string{`"merge_commit":"main-sha"`, `"prior_stable_commit":"main-prior-sha"`, `"revert_sha":"production-revert-sha"`}
				}
				for _, want := range wantRollbackLedger {
					if !strings.Contains(events, want) {
						t.Fatalf("rollback ledger missing %q:\n%s", want, events)
					}
				}
			}
		})
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
			name:          "bogus-gate",
			writer:        &recordingPromotionWriter{},
			preProdBranch: "pre-prod",
			gate:          "bogus",
			wantErr:       "allowed values: human-merge, auto",
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
	mainHeadSHA     string
	mainHeadSHASet  bool
	branchChecks    gh.BranchChecksResult
	branchChecksErr error
	compareFiles    []string
	compareDiff     string
	compareErr      error
	revertErr       error
}

func (w *recordingPromotionWriter) BranchHeadSHA(_ context.Context, branch string) (string, error) {
	w.calls = append(w.calls, "head:"+branch)
	if w.mainHeadSHASet {
		return w.mainHeadSHA, nil
	}
	return branch + "-prior-sha", nil
}

func (w *recordingPromotionWriter) BranchChecks(_ context.Context, branch string) (gh.BranchChecksResult, error) {
	w.calls = append(w.calls, "checks:"+branch)
	if w.branchChecksErr != nil {
		return gh.BranchChecksResult{}, w.branchChecksErr
	}
	if strings.TrimSpace(w.branchChecks.Branch) != "" || strings.TrimSpace(w.branchChecks.HeadSHA) != "" || len(w.branchChecks.Checks) > 0 {
		return w.branchChecks, nil
	}
	return gh.BranchChecksResult{
		Branch:  branch,
		HeadSHA: "main-sha",
		Checks:  []gh.Check{{Name: "verify", State: "success", Bucket: "pass"}},
	}, nil
}

func (w *recordingPromotionWriter) CompareBranches(_ context.Context, base, head string) ([]string, string, error) {
	w.calls = append(w.calls, "compare:"+base+"..."+head)
	if w.compareErr != nil {
		return nil, "", w.compareErr
	}
	files := append([]string(nil), w.compareFiles...)
	if len(files) == 0 && strings.TrimSpace(w.compareDiff) == "" {
		files = []string{"README.md"}
	}
	return files, w.compareDiff, nil
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

func (w *recordingPromotionWriter) RevertProductionMerge(_ context.Context, mainBranch, mergeCommit, priorStableCommit string) (gh.ProductionRevertResult, error) {
	w.calls = append(w.calls, "revert:"+mainBranch+":"+mergeCommit+":"+priorStableCommit)
	if w.revertErr != nil {
		return gh.ProductionRevertResult{}, w.revertErr
	}
	return gh.ProductionRevertResult{
		Branch:            mainBranch,
		RevertedSHA:       mergeCommit,
		MergeCommit:       mergeCommit,
		PriorStableCommit: priorStableCommit,
		SHA:               "production-revert-sha",
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

func promoteNeedsHumanHasStepDetail(items []PromoteNeedsHuman, step, detail string) bool {
	for _, item := range items {
		if item.Step != step {
			continue
		}
		if detail == "" || strings.Contains(item.Detail, detail) {
			return true
		}
	}
	return false
}

func assertPromoteBoolPtr(t *testing.T, name string, got, want *bool) {
	t.Helper()
	switch {
	case got == nil && want == nil:
		return
	case got == nil || want == nil:
		t.Fatalf("%s = %v, want %v", name, got, want)
	case *got != *want:
		t.Fatalf("%s = %t, want %t", name, *got, *want)
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
