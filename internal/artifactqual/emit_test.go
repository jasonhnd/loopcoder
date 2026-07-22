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

func TestEmitRefusesWithoutInterruptEvent(t *testing.T) {
	_, err := artifactqual.EmitCanaryEvidence(artifactqual.EmitInput{
		ArchiveDigest: "aa", PreProdSHA: "bb", ProjectID: "disp-x", RunID: "r1",
		Resumed: true, Events: []workflowrun.Event{{Kind: "launch", WorkItemID: "a"}},
	})
	if err == nil || !strings.Contains(err.Error(), "no interrupt") {
		t.Fatalf("want interrupt refuse, got %v", err)
	}
}

func TestEmitRefusesPendingLiveVerifier(t *testing.T) {
	_, err := artifactqual.EmitCanaryEvidence(artifactqual.EmitInput{
		ArchiveDigest: "aa", PreProdSHA: "bb", ProjectID: "disp-x", RunID: "r1",
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

	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	rem := 0.8
	ev, err := artifactqual.EmitCanaryEvidence(artifactqual.EmitInput{
		ArchiveDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		PreProdSHA:    "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		BinaryVersion: "0.9.0-rc.11", BinaryCommit: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		ProjectID: "disp-emit-1", RunID: "run_emit_1",
		EventLogPath: logPath,
		Events: []workflowrun.Event{
			{Kind: "launch", WorkItemID: "a", AttemptID: "att-a-g0"},
			{Kind: "terminal", WorkItemID: "a", AttemptID: "att-a-g0", Terminal: "succeeded"},
			{Kind: "launch", WorkItemID: "b", AttemptID: "att-b-g0"},
			{Kind: "interrupt", WorkItemID: "b", AttemptID: "att-b-g0"},
			{Kind: "reuse", WorkItemID: "a", AttemptID: "att-a-g0"},
			{Kind: "launch", WorkItemID: "b", AttemptID: "att-b-g1"},
		},
		Resumed: true, ReuseCount: 1, ProcessPeak: 2, WorktreePeak: 2,
		Children: []artifactqual.CanaryChild{
			emitChild("c1", "codex", "low"), emitChild("c2", "codex", "high"),
			emitChild("c3", "antigravity", "medium"), emitChild("c4", "antigravity", "medium"),
		},
		ProviderObs: []artifactqual.CanaryProviderObs{
			{Provider: "codex", Source: "cli", Freshness: "fresh", Remaining: &rem, CapturedAt: now},
			{Provider: "antigravity", Source: "cli", Freshness: "fresh", Remaining: &rem, CapturedAt: now},
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
	v := artifactqual.ValidateCanaryEvidence(ev, ev.ArchiveDigest, ev.PreProdSHA, now)
	if !v.Valid {
		t.Fatalf("validation reasons: %v", v.Reasons)
	}
}

func emitChild(id, prov, depth string) artifactqual.CanaryChild {
	b, r, a := 0.9, 0.05, 0.8
	return artifactqual.CanaryChild{
		ChildID: id, AttemptID: "att-" + id, Provider: prov, Model: "m",
		DepthRequired: depth, DepthSelected: depth, DepthInvocation: depth,
		Terminal: "succeeded", CapacityBefore: &b, CapacityReserved: &r, CapacityAfter: &a,
		RealProviderExecuted: true, AfterSource: "cli", AfterFreshness: "fresh",
	}
}
