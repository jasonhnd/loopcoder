package artifactqual_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/artifactqual"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

// Production sequence: launch → forced interrupt → cancelled terminal (same interrupt_id)
// → reuse → later generation launch. Must count aborted attempt (not open-attempt map).
func TestEmit_ProductionForcedInterruptCancelledTerminal(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "workflow-events.jsonl")
	_ = os.WriteFile(logPath, []byte("{}\n"), 0o600)
	repo := t.TempDir()
	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	rem := 0.8
	payload := []byte(`{"failure_class":"forced_interrupt","interrupt_class":"service_forced_interrupt","interrupt_id":"iint_prod1","terminal":"cancelled"}`)
	ev, err := artifactqual.EmitCanaryEvidence(artifactqual.EmitInput{
		ArchiveDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		PreProdSHA:    "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		BinaryVersion: "0.9.0", BinaryCommit: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		ProjectID: "disp-forced-1", RunID: "run_forced_1", EventLogPath: logPath,
		Events: []workflowrun.Event{
			{Kind: "launch", WorkItemID: "wi_impl", AttemptID: "att-wi_impl-g0", Generation: 1},
			{Kind: "interrupt", WorkItemID: "wi_impl", AttemptID: "att-wi_impl-g0", FailureClass: "forced_interrupt", Payload: payload},
			{Kind: "terminal", WorkItemID: "wi_impl", AttemptID: "att-wi_impl-g0", Terminal: "cancelled", FailureClass: "forced_interrupt", Payload: payload},
			{Kind: "reuse", WorkItemID: "wi_impl", AttemptID: "att-wi_impl-g0"},
			{Kind: "launch", WorkItemID: "wi_impl", AttemptID: "att-wi_impl-g1", Generation: 2},
		},
		Resumed: true, ProcessPeak: 1, WorktreePeak: 1,
		ActiveOccupancyMeasured: true, ProcessActive: 0, WorktreeActive: 0, RepoPath: repo,
		Children: []artifactqual.CanaryChild{
			emitChild("c1", "codex", "low"), emitChild("c2", "codex", "high"),
			emitChild("c3", "antigravity", "medium"), emitChild("c4", "antigravity", "medium"),
		},
		ProviderObs: []artifactqual.CanaryProviderObs{
			{Provider: "codex", AccountRef: "acct-codex", InstallRef: "pinst_codex", WindowKind: "five_hour", Source: "codexbar", Freshness: "fresh", Confidence: "exact", Remaining: &rem, CapturedAt: now.Add(-time.Minute), ResetAt: ptrTimeCanary(now.Add(2 * time.Hour))},
			{Provider: "antigravity", AccountRef: "acct-antigravity", InstallRef: "pinst_antigravity", WindowKind: "five_hour", Source: "codexbar", Freshness: "fresh", Confidence: "exact", Remaining: &rem, CapturedAt: now.Add(-time.Minute), ResetAt: ptrTimeCanary(now.Add(2 * time.Hour))},
		},
		PRURL: "https://github.com/o/r/pull/1", PRNumber: 1,
		PRRequiredChecks: []string{"verify"}, PRRequiredChecksGreen: true,
		PRIndependentVerifier: "x", PRVerifierEvidenceRef: "sha256:a", PRCreatedByLoopCoder: true,
		Unavailable: &artifactqual.CanaryUnavailableRetry{
			ExcludedProvider: "claude", ExcludedReason: "unavailable",
			NoDuplicateClaim: true, NoDuplicateFiles: true, NoDoubleCapacity: true,
			EvidenceRef: "events:x",
		},
		ProducedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Restart.AbortedAttemptCount < 1 {
		t.Fatalf("want aborted attempt from cancelled terminal pair, got %+v", ev.Restart)
	}
	if !ev.Restart.LaterGenerationResume {
		t.Fatal("want later generation resume")
	}
	if !ev.Restart.ExactlyOnce {
		t.Fatalf("want ExactlyOnce true: %+v", ev.Restart)
	}
	// Full PR+live+structured wi_verify/wi_implement for complete validation.
	sha := ev.PreProdSHA
	verEvid := "sha256:" + strings.Repeat("cd", 32)
	ev.Children[0].ChildID = "wi_verify"
	ev.Children[0].WorkItemID = "wi_verify"
	ev.Children[0].TaskClass = "soul"
	ev.Children[0].Provider = "codex"
	ev.Children[0].AttemptID = "att-v"
	ev.Children[0].OutputEvidence = verEvid
	ev.Children[0].Terminal = "succeeded"
	ev.Children[0].RealProviderExecuted = true
	ev.Children[1].ChildID = "wi_implement"
	ev.Children[1].WorkItemID = "wi_implement"
	ev.Children[1].TaskClass = "tera"
	ev.Children[1].Provider = "antigravity"
	ev.Children[1].AttemptID = "att-i"
	ev.Children[1].Terminal = "succeeded"
	ev.Children[1].RealProviderExecuted = true
	ev.PR = &artifactqual.CanaryPR{
		URL: "https://github.com/jasonhnd/loopcoder/pull/1", Number: 1,
		Repository: "jasonhnd/loopcoder", BaseRef: "main", HeadOID: sha, Branch: "b",
		RequiredChecks: []string{"verify"}, RequiredChecksGreen: true,
		CreatedByLoopCoder: true, AutoMerge: false, HumanMergeGate: true,
		IndependentVerifier: "codex", VerifierProvider: "codex", VerifierAttemptID: "att-v",
		VerifierEvidenceRef: verEvid + "@head:" + sha,
	}
	ev.ContentDigest = artifactqual.DigestCanaryBody(ev)
	live := &artifactqual.PRLiveState{
		Repository: "jasonhnd/loopcoder", Number: 1, URL: ev.PR.URL,
		BaseRef: "main", HeadOID: sha, State: "open",
		RequiredChecksGreen: true, HumanMergeGate: true,
		ChecksAtHead: []artifactqual.PRCheck{{Name: "verify", Status: "completed", Conclusion: "success"}},
	}
	v := artifactqual.ValidateCanaryEvidence(ev, ev.ArchiveDigest, ev.PreProdSHA, now, artifactqual.CanaryValidateOpts{
		ExpectedPRHeadOID: sha, LivePR: live,
	})
	if !v.Valid {
		t.Fatalf("want valid: %v", v.Reasons)
	}
}

func TestEmit_AbortedSucceededRejected(t *testing.T) {
	repo := t.TempDir()
	payload := []byte(`{"failure_class":"forced_interrupt","interrupt_class":"service_forced_interrupt","interrupt_id":"iint_x","terminal":"cancelled"}`)
	// Succeeded terminal on same attempt as interrupt — must not ExactlyOnce.
	ev, err := artifactqual.EmitCanaryEvidence(artifactqual.EmitInput{
		ArchiveDigest: "aa", PreProdSHA: "bb", ProjectID: "disp-x", RunID: "r1",
		BinaryVersion: "v", BinaryCommit: "bb",
		Events: []workflowrun.Event{
			{Kind: "launch", WorkItemID: "wi", AttemptID: "att-wi-g0"},
			{Kind: "interrupt", WorkItemID: "wi", AttemptID: "att-wi-g0", FailureClass: "forced_interrupt", Payload: payload},
			{Kind: "terminal", WorkItemID: "wi", AttemptID: "att-wi-g0", Terminal: "succeeded"}, // impossible success after interrupt
			{Kind: "reuse", WorkItemID: "wi", AttemptID: "att-wi-g0"},
			{Kind: "launch", WorkItemID: "wi", AttemptID: "att-wi-g1"},
		},
		Resumed: true, ProcessPeak: 1, WorktreePeak: 1,
		ActiveOccupancyMeasured: true, RepoPath: repo,
	})
	if err != nil {
		// may still emit with ExactlyOnce false
		t.Log(err)
		return
	}
	if ev.Restart.ExactlyOnce {
		t.Fatal("succeeded aborted attempt must not be ExactlyOnce")
	}
}
