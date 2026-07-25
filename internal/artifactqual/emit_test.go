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

func TestEmitRefusesResumedWithoutInterruptEvent(t *testing.T) {
	// Resumed=true without durable interrupt is hand-written — refuse.
	_, err := artifactqual.EmitCanaryEvidence(artifactqual.EmitInput{
		ArchiveDigest: "aa", PreProdSHA: "bb", ProjectID: "disp-x", RunID: "r1",
		InventoryProvenance: "live_discover", InventoryReportDigest: "sha256:inventory",
		Resumed: true, Events: []workflowrun.Event{{Kind: "launch", WorkItemID: "a"}},
	})
	if err == nil || !strings.Contains(err.Error(), "without interrupt") {
		t.Fatalf("want resumed-without-interrupt refuse, got %v", err)
	}
}

func TestEmitRefusesResumedWhenInterruptAndTerminalClassesDiffer(t *testing.T) {
	interruptPayload := []byte(`{"failure_class":"forced_interrupt","interrupt_class":"service_forced_interrupt","interrupt_id":"iint-mixed","terminal":"cancelled"}`)
	terminalPayload := []byte(`{"failure_class":"hard_kill_recovery","interrupt_class":"hard_kill_recovery","interrupt_id":"iint-mixed","terminal":"cancelled"}`)
	_, err := artifactqual.EmitCanaryEvidence(artifactqual.EmitInput{
		ArchiveDigest: "aa", PreProdSHA: "bb", ProjectID: "disp-x", RunID: "r1",
		InventoryProvenance: "live_discover", InventoryReportDigest: "sha256:inventory",
		Resumed: true,
		Events: []workflowrun.Event{
			{Kind: "launch", WorkItemID: "wi", AttemptID: "att-wi-g0", Generation: 1},
			{Kind: "interrupt", WorkItemID: "wi", AttemptID: "att-wi-g0", Generation: 1, FailureClass: "forced_interrupt", Payload: interruptPayload},
			{Kind: "terminal", WorkItemID: "wi", AttemptID: "att-wi-g0", Generation: 1, Terminal: "cancelled", FailureClass: "hard_kill_recovery", Payload: terminalPayload},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "without interrupt") {
		t.Fatalf("mixed interrupt/terminal classes must not establish durable resume: %v", err)
	}
}

func TestEmitRefusesUnspecifiedInventoryProvenance(t *testing.T) {
	_, err := artifactqual.EmitCanaryEvidence(artifactqual.EmitInput{
		ArchiveDigest: "aa", PreProdSHA: "bb", ProjectID: "disp-x", RunID: "r1",
		InventoryReportDigest: "sha256:inventory",
	})
	if err == nil || !strings.Contains(err.Error(), "live_discover") {
		t.Fatalf("want exact live_discover rejection, got %v", err)
	}
}

func TestEmitInterruptedDoesNotImplyResumed(t *testing.T) {
	repo := t.TempDir()
	payload := []byte(`{"attempt_id":"att-wi-g0","failure_class":"forced_interrupt","generation":"1","interrupt_class":"service_forced_interrupt","interrupt_id":"iint-not-resumed","terminal":"cancelled","work_item_id":"wi"}`)
	ev, err := artifactqual.EmitCanaryEvidence(artifactqual.EmitInput{
		ArchiveDigest: "aa", PreProdSHA: "bb", ProjectID: "disp-x", RunID: "r1",
		InventoryProvenance: "live_discover", InventoryReportDigest: "sha256:inventory",
		RepoPath: repo,
		Events: []workflowrun.Event{
			{Kind: "launch", WorkItemID: "wi", AttemptID: "att-wi-g0", Generation: 1},
			{Kind: "interrupt", WorkItemID: "wi", AttemptID: "att-wi-g0", Generation: 1, FailureClass: "forced_interrupt", Terminal: "cancelled", Payload: payload},
			{Kind: "terminal", WorkItemID: "wi", AttemptID: "att-wi-g0", Generation: 1, FailureClass: "forced_interrupt", Terminal: "cancelled", Payload: payload},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Restart == nil || !ev.Restart.Interrupted || ev.Restart.ResumedFromDurable {
		t.Fatalf("interrupt must remain distinct from resume: %+v", ev.Restart)
	}
}

func TestEmitWithoutInterruptLeavesRestartNil(t *testing.T) {
	// Honest: no interrupt events → Restart=nil. Validate still fail-closed on
	// restart_evidence_missing; qualify keeps required metrics not_run when invalid.
	// Do not score this as green restart or partial qualify.
	repo := t.TempDir()
	ev, err := artifactqual.EmitCanaryEvidence(artifactqual.EmitInput{
		ArchiveDigest: "aa", PreProdSHA: "bb", ProjectID: "disp-no-restart", RunID: "r1",
		InventoryProvenance: "live_discover", InventoryReportDigest: "sha256:inventory",
		BinaryVersion: "v", BinaryCommit: "bb",
		Events:   []workflowrun.Event{{Kind: "launch", WorkItemID: "wi_tests", AttemptID: "a0"}},
		RepoPath: repo,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Restart != nil {
		t.Fatalf("want nil Restart without interrupt, got %+v", ev.Restart)
	}
	v := artifactqual.ValidateCanaryEvidence(ev, "aa", "bb", time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC))
	if v.RestartOK || v.Valid {
		t.Fatalf("want Valid=false RestartOK=false with restart_evidence_missing, got Valid=%v RestartOK=%v reasons=%v",
			v.Valid, v.RestartOK, v.Reasons)
	}
	found := false
	for _, r := range v.Reasons {
		if strings.Contains(r, "restart_evidence_missing") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want restart_evidence_missing in reasons: %v", v.Reasons)
	}
}

func TestEmitRefusesPendingLiveVerifier(t *testing.T) {
	_, err := artifactqual.EmitCanaryEvidence(artifactqual.EmitInput{
		ArchiveDigest: "aa", PreProdSHA: "bb", ProjectID: "disp-x", RunID: "r1",
		InventoryProvenance: "live_discover", InventoryReportDigest: "sha256:inventory",
		Events: []workflowrun.Event{{Kind: "interrupt", WorkItemID: "b"}},
		PRURL:  "https://github.com/o/r/pull/1", PRVerifierEvidenceRef: "sha256:pending-live",
	})
	if err == nil || !strings.Contains(err.Error(), "pending") {
		t.Fatalf("%v", err)
	}
}

func TestEmitAndValidateHappyPath(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "workflow-events.jsonl")
	_ = os.WriteFile(logPath, []byte(`{"kind":"interrupt"}`+"\n"), 0o600)
	// Repo without .loopcoder — measured clean.
	repo := t.TempDir()

	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	rem := 0.8
	ev, err := artifactqual.EmitCanaryEvidence(artifactqual.EmitInput{
		ArchiveDigest:       "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		PreProdSHA:          "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		InventoryProvenance: "live_discover", InventoryReportDigest: "sha256:inventory",
		BinaryVersion: "0.9.0-rc.11", BinaryCommit: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		ProjectID: "disp-emit-1", RunID: "run_emit_1",
		EventLogPath: logPath,
		Events: []workflowrun.Event{
			{Kind: "launch", WorkItemID: "a", AttemptID: "att-a-g0"},
			{Kind: "terminal", WorkItemID: "a", AttemptID: "att-a-g0", Terminal: "succeeded"},
			{Kind: "launch", WorkItemID: "b", AttemptID: "att-b-g0"},
			{Kind: "interrupt", WorkItemID: "b", AttemptID: "att-b-g0", FailureClass: "forced_interrupt",
				Payload: []byte(`{"failure_class":"forced_interrupt","interrupt_class":"service_forced_interrupt","interrupt_id":"iint_1","terminal":"cancelled"}`)},
			{Kind: "terminal", WorkItemID: "b", AttemptID: "att-b-g0", Terminal: "cancelled", FailureClass: "forced_interrupt",
				Payload: []byte(`{"failure_class":"forced_interrupt","interrupt_class":"service_forced_interrupt","interrupt_id":"iint_1","terminal":"cancelled"}`)},
			{Kind: "reuse", WorkItemID: "b", AttemptID: "att-b-g0"},
			{Kind: "launch", WorkItemID: "b", AttemptID: "att-b-g1"},
		},
		Resumed: true, ReuseCount: 1,
		// Sequential ceiling = 1; active zero at return.
		ProcessPeak: 1, WorktreePeak: 1,
		ProcessActive: 0, WorktreeActive: 0, ActiveOccupancyMeasured: true,
		RepoPath: repo,
		Children: []artifactqual.CanaryChild{
			emitChild("c1", "codex", "low"), emitChild("c2", "codex", "high"),
			emitChild("c3", "antigravity", "medium"), emitChild("c4", "antigravity", "medium"),
		},
		ProviderObs: []artifactqual.CanaryProviderObs{
			{Provider: "codex", AccountRef: "acct-codex", InstallRef: "pinst_codex", WindowKind: "five_hour", Source: "codexbar", Freshness: "fresh", Confidence: "exact", Remaining: &rem, CapturedAt: now.Add(-time.Minute), ResetAt: ptrTimeCanary(now.Add(2 * time.Hour))},
			{Provider: "antigravity", AccountRef: "acct-antigravity", InstallRef: "pinst_antigravity", WindowKind: "five_hour", Source: "codexbar", Freshness: "fresh", Confidence: "exact", Remaining: &rem, CapturedAt: now.Add(-time.Minute), ResetAt: ptrTimeCanary(now.Add(2 * time.Hour))},
		},
		PRURL: "https://github.com/o/r/pull/9", PRNumber: 9, PRBranch: "loopcoder/goal-x",
		PRRequiredChecks: []string{"verify", "test"}, PRRequiredChecksGreen: true,
		PRIndependentVerifier: "codex", PRVerifierEvidenceRef: "sha256:abcdef",
		PRCreatedByLoopCoder: true,
		Unavailable: &artifactqual.CanaryUnavailableRetry{
			ExcludedProvider: "claude", ExcludedReason: "unavailable",
			NoDuplicateClaim: true, NoDuplicateFiles: true, NoDoubleCapacity: true,
			EvidenceRef: "events:hard_exclude:claude",
		},
		ProducedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ev.Restart.Interrupted || ev.Restart.EvidenceRef == "" {
		t.Fatalf("%+v", ev.Restart)
	}
	if !ev.Restart.ExactlyOnce || !ev.Restart.ProcessCeilingOK || !ev.Restart.NoLeakedProcesses || !ev.Restart.NoRepoLocalRuntime {
		t.Fatalf("restart flags incomplete: %+v", ev.Restart)
	}
	if ev.ContentDigest == "" {
		t.Fatal("content digest required")
	}
	// Complete PR + live + structured wi_verify/wi_implement for full validation path.
	sha := ev.PreProdSHA
	// Deterministic full sha256: + 64 hex for verify OutputEvidence.
	verEvid := "sha256:" + strings.Repeat("ab", 32)
	// Mutate children to exact structured verifier/implement identity.
	ev.Children[0].ChildID = "wi_verify"
	ev.Children[0].WorkItemID = "wi_verify"
	ev.Children[0].TaskClass = "soul"
	ev.Children[0].Provider = "codex"
	ev.Children[0].AttemptID = "att-v-1"
	ev.Children[0].OutputEvidence = verEvid
	ev.Children[0].Terminal = "succeeded"
	ev.Children[0].RealProviderExecuted = true
	ev.Children[1].ChildID = "wi_implement"
	ev.Children[1].WorkItemID = "wi_implement"
	ev.Children[1].TaskClass = "tera"
	ev.Children[1].Provider = "antigravity"
	ev.Children[1].AttemptID = "att-i-1"
	ev.Children[1].Terminal = "succeeded"
	ev.Children[1].RealProviderExecuted = true
	ev.PR = &artifactqual.CanaryPR{
		URL: "https://github.com/jasonhnd/loopcoder/pull/9", Number: 9,
		Repository: "jasonhnd/loopcoder", Branch: "loopcoder/goal-x", BaseRef: "main", HeadOID: sha,
		RequiredChecks: []string{"verify", "test"}, RequiredChecksGreen: true,
		CreatedByLoopCoder: true, AutoMerge: false, HumanMergeGate: true,
		IndependentVerifier: "codex", VerifierProvider: "codex", VerifierAttemptID: "att-v-1",
		VerifierEvidenceRef: verEvid + "@head:" + sha,
	}
	completeRawCanaryEvidence(&ev, now)
	ev.ContentDigest = artifactqual.DigestCanaryBody(ev)
	live := &artifactqual.PRLiveState{
		Repository: "jasonhnd/loopcoder", Number: 9, URL: ev.PR.URL,
		BaseRef: "main", HeadOID: sha, State: "open",
		RequiredChecksGreen: true, HumanMergeGate: true,
		ChecksAtHead: []artifactqual.PRCheck{
			{Name: "verify", Status: "completed", Conclusion: "success"},
			{Name: "test", Status: "completed", Conclusion: "success"},
		},
	}
	v := artifactqual.ValidateCanaryEvidence(ev, ev.ArchiveDigest, ev.PreProdSHA, now, artifactqual.CanaryValidateOpts{
		ExpectedPRHeadOID: sha, LivePR: live,
	})
	if !v.Valid {
		t.Fatalf("validation reasons: %v", v.Reasons)
	}
}

func TestEmit_NoAbortedAttemptExactlyOnceFalse(t *testing.T) {
	// Interrupt event without any aborted open attempt → ExactlyOnce must stay false.
	repo := t.TempDir()
	ev, err := artifactqual.EmitCanaryEvidence(artifactqual.EmitInput{
		ArchiveDigest: "aa", PreProdSHA: "bb", ProjectID: "disp-x", RunID: "r1",
		InventoryProvenance: "live_discover", InventoryReportDigest: "sha256:inventory",
		BinaryVersion: "v", BinaryCommit: "c",
		Events: []workflowrun.Event{
			{Kind: "launch", WorkItemID: "a", AttemptID: "att-a"},
			{Kind: "terminal", WorkItemID: "a", AttemptID: "att-a", Terminal: "succeeded"},
			{Kind: "interrupt"}, // parent interrupt with no open attempts may yield empty aborted
		},
		Resumed: true, ProcessPeak: 1, WorktreePeak: 1,
		ActiveOccupancyMeasured: true, RepoPath: repo,
	})
	if err != nil {
		// May refuse or emit with ExactlyOnce=false depending on interrupt mapping.
		if !strings.Contains(err.Error(), "interrupt") {
			t.Fatalf("%v", err)
		}
		return
	}
	if ev.Restart.ExactlyOnce {
		t.Fatal("ExactlyOnce must be false without real aborted attempt + reuse")
	}
	if ev.Restart.AbortedAttemptCount == 0 && ev.Restart.ExactlyOnce {
		t.Fatal("impossible")
	}
}

func TestEmit_OverCeilingPeakNotOK(t *testing.T) {
	repo := t.TempDir()
	payload := []byte(`{"failure_class":"forced_interrupt","interrupt_class":"service_forced_interrupt","interrupt_id":"iint-over","terminal":"cancelled"}`)
	ev, err := artifactqual.EmitCanaryEvidence(artifactqual.EmitInput{
		ArchiveDigest: "aa", PreProdSHA: "bb", ProjectID: "disp-x", RunID: "r1",
		InventoryProvenance: "live_discover", InventoryReportDigest: "sha256:inventory",
		BinaryVersion: "v", BinaryCommit: "c",
		Events: []workflowrun.Event{
			{Kind: "launch", WorkItemID: "b", AttemptID: "att-b-g0"},
			{Kind: "interrupt", WorkItemID: "b", AttemptID: "att-b-g0", FailureClass: "forced_interrupt", Payload: payload},
			{Kind: "terminal", WorkItemID: "b", AttemptID: "att-b-g0", Terminal: "cancelled", FailureClass: "forced_interrupt", Payload: payload},
			{Kind: "reuse", WorkItemID: "b", AttemptID: "att-b-g0"},
			{Kind: "launch", WorkItemID: "b", AttemptID: "att-b-g1", Generation: 2},
		},
		Resumed: true, ProcessPeak: 2, WorktreePeak: 2, // over sequential ceiling 1
		ActiveOccupancyMeasured: true, RepoPath: repo,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Restart.ProcessCeilingOK || ev.Restart.WorktreeCeilingOK {
		t.Fatalf("over-ceiling peaks must not be OK: %+v", ev.Restart)
	}
}

func TestEmit_NonzeroActiveNotNoLeaked(t *testing.T) {
	repo := t.TempDir()
	payload := []byte(`{"failure_class":"forced_interrupt","interrupt_class":"service_forced_interrupt","interrupt_id":"iint-active","terminal":"cancelled"}`)
	ev, err := artifactqual.EmitCanaryEvidence(artifactqual.EmitInput{
		ArchiveDigest: "aa", PreProdSHA: "bb", ProjectID: "disp-x", RunID: "r1",
		InventoryProvenance: "live_discover", InventoryReportDigest: "sha256:inventory",
		BinaryVersion: "v", BinaryCommit: "c",
		Events: []workflowrun.Event{
			{Kind: "launch", WorkItemID: "b", AttemptID: "att-b-g0"},
			{Kind: "interrupt", WorkItemID: "b", AttemptID: "att-b-g0", FailureClass: "forced_interrupt", Payload: payload},
			{Kind: "terminal", WorkItemID: "b", AttemptID: "att-b-g0", Terminal: "cancelled", FailureClass: "forced_interrupt", Payload: payload},
			{Kind: "reuse", WorkItemID: "b", AttemptID: "att-b-g0"},
			{Kind: "launch", WorkItemID: "b", AttemptID: "att-b-g1", Generation: 2},
		},
		Resumed: true, ProcessPeak: 1, WorktreePeak: 1,
		ProcessActive: 1, WorktreeActive: 0, ActiveOccupancyMeasured: true,
		RepoPath: repo,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Restart.NoLeakedProcesses {
		t.Fatal("nonzero ProcessActive must not claim NoLeakedProcesses")
	}
}

func TestEmit_RepoLocalLoopcoderNotClean(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".loopcoder"), 0o755); err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"failure_class":"forced_interrupt","interrupt_class":"service_forced_interrupt","interrupt_id":"iint-repo","terminal":"cancelled"}`)
	ev, err := artifactqual.EmitCanaryEvidence(artifactqual.EmitInput{
		ArchiveDigest: "aa", PreProdSHA: "bb", ProjectID: "disp-x", RunID: "r1",
		InventoryProvenance: "live_discover", InventoryReportDigest: "sha256:inventory",
		BinaryVersion: "v", BinaryCommit: "c",
		Events: []workflowrun.Event{
			{Kind: "launch", WorkItemID: "b", AttemptID: "att-b-g0"},
			{Kind: "interrupt", WorkItemID: "b", AttemptID: "att-b-g0", FailureClass: "forced_interrupt", Payload: payload},
			{Kind: "terminal", WorkItemID: "b", AttemptID: "att-b-g0", Terminal: "cancelled", FailureClass: "forced_interrupt", Payload: payload},
			{Kind: "reuse", WorkItemID: "b", AttemptID: "att-b-g0"},
			{Kind: "launch", WorkItemID: "b", AttemptID: "att-b-g1", Generation: 2},
		},
		Resumed: true, ProcessPeak: 1, WorktreePeak: 1,
		ActiveOccupancyMeasured: true, RepoPath: repo,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ev.Restart.RepoLocalRuntimePresent || ev.Restart.NoRepoLocalRuntime {
		t.Fatalf("repo-local .loopcoder must fail closed: %+v", ev.Restart)
	}
}

func TestEmit_DuplicateLaunchNotExactlyOnce(t *testing.T) {
	repo := t.TempDir()
	payload := []byte(`{"failure_class":"forced_interrupt","interrupt_class":"service_forced_interrupt","interrupt_id":"iint-dup","terminal":"cancelled"}`)
	ev, err := artifactqual.EmitCanaryEvidence(artifactqual.EmitInput{
		ArchiveDigest: "aa", PreProdSHA: "bb", ProjectID: "disp-x", RunID: "r1",
		InventoryProvenance: "live_discover", InventoryReportDigest: "sha256:inventory",
		BinaryVersion: "v", BinaryCommit: "c",
		Events: []workflowrun.Event{
			{Kind: "launch", WorkItemID: "b", AttemptID: "att-b-g0"},
			{Kind: "launch", WorkItemID: "b", AttemptID: "att-b-g0"}, // duplicate same attempt
			{Kind: "interrupt", WorkItemID: "b", AttemptID: "att-b-g0", FailureClass: "forced_interrupt", Payload: payload},
			{Kind: "terminal", WorkItemID: "b", AttemptID: "att-b-g0", Terminal: "cancelled", FailureClass: "forced_interrupt", Payload: payload},
			{Kind: "reuse", WorkItemID: "b", AttemptID: "att-b-g0"},
			{Kind: "launch", WorkItemID: "b", AttemptID: "att-b-g1", Generation: 2},
		},
		Resumed: true, ProcessPeak: 1, WorktreePeak: 1,
		ActiveOccupancyMeasured: true, RepoPath: repo,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ev.Restart.DuplicateLaunch || ev.Restart.ExactlyOnce {
		t.Fatalf("dup launch must not be ExactlyOnce: %+v", ev.Restart)
	}
}

func emitChild(id, prov, depth string) artifactqual.CanaryChild {
	b, r, a := 0.9, 0.05, 0.8
	obsAt := time.Date(2026, 7, 23, 7, 55, 0, 0, time.UTC)
	act := 0.1
	return artifactqual.CanaryChild{
		ChildID: id, AttemptID: "att-" + id, Provider: prov, Model: "m",
		DepthRequired: depth, DepthSelected: depth, DepthInvocation: depth,
		Terminal: "succeeded", CapacityBefore: &b, CapacityReserved: &r, CapacityAfter: &a,
		CapacityActual: &act, ActualSource: "estimated_group_delta_reservation_weighted:obs|codexbar|t", ActualConfidence: "estimated",
		RealProviderExecuted: true,
		AfterSource:          "codexbar", AfterFreshness: "fresh", AfterState: "observed",
		AfterConfidence: "exact", AfterObservedAt: obsAt,
		BeforeSource: "codexbar", BeforeFreshness: "fresh", BeforeConfidence: "exact",
		BeforeCapturedAt: obsAt,
		ResetAt:          func() *time.Time { x := obsAt.Add(2 * time.Hour); return &x }(),
		AccountRef:       "acct-" + prov, InstallRef: "pinst_" + prov, WindowKind: "five_hour",
		ActualSources: &artifactqual.CanaryRouteSources{
			Model: "provider_stream", Effort: "accepted_invocation",
			Permission: "accepted_invocation", Account: "auth_binding", Install: "install_binding",
		},
		ArgvDigest: "sha256:emit-argv-" + id,
	}
}
