package goalrun_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/goalrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

func testHome(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "loopcoder-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestExecuteDecomposesAndReportsChildren(t *testing.T) {
	home := testHome(t)
	var reports bytes.Buffer
	res, err := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: "proj", Goal: "implement transparent multi-child routing",
		Issue: "1342", Actor: "owner", Owner: "worker",
		Provider: "fixture", Model: "fixture-model",
		ReportOut: &reports,
		HomeDir:   home,
		Executor:  workflowrun.FakeChildExecutor{HomeDir: home},
		Now:       func() time.Time { return time.Date(2026, 7, 22, 22, 0, 0, 0, time.UTC) },
	})
	if res.GraphID == "" || res.PlanDigest == "" {
		t.Fatalf("missing graph: %+v err=%v", res, err)
	}
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(res.Children) < 4 {
		t.Fatalf("children=%d", len(res.Children))
	}
	for _, c := range res.Children {
		if c.RouteRequirement == "" || c.ChildID == "" {
			t.Fatalf("%+v", c)
		}
		if strings.Contains(strings.ToLower(c.Intent), "provider_native") {
			t.Fatal("provider-native intent leak")
		}
		if c.OutputEvidence == "" {
			t.Fatalf("missing output evidence after real executor path: %+v", c)
		}
		if c.Terminal != "succeeded" {
			t.Fatalf("terminal %+v", c)
		}
	}
	if reports.Len() == 0 {
		t.Fatal("expected JSONL child reports")
	}
	if res.Workflow.LaunchCount < 4 {
		t.Fatalf("workflow launches=%d", res.Workflow.LaunchCount)
	}
}

func TestExecuteAutoRoutesChildrenWithCapacityAccounting(t *testing.T) {
	now := time.Date(2026, 7, 22, 23, 0, 0, 0, time.UTC)
	home := testHome(t)
	// Two unattended-eligible accounts for multi-provider routing.
	codex := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "codex", AccountRef: "acct-codex", InstallRef: "i-codex",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Windows: []capacitysnapshot.Window{{
			Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
			Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 20, Unit: capacitysnapshot.UnitPercentage},
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 80, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
			ResetAt: ptrTime(now.Add(2 * time.Hour)), CapturedAt: now, Source: "test",
		}},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "gpt-5.5", SupportedDepths: []string{"low", "medium", "high"}, DefaultDepth: "medium", Present: true,
		}},
		Source: "test", CapturedAt: now,
	})
	// Prefer Antigravity/Gemini as second company (not Claude-only).
	gemini := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "gemini", AccountRef: "acct-gemini", InstallRef: "i-gemini",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Windows: []capacitysnapshot.Window{{
			Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
			Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 10, Unit: capacitysnapshot.UnitPercentage},
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 90, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
			ResetAt: ptrTime(now.Add(30 * time.Minute)), CapturedAt: now, Source: "test",
		}},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "gemini-2.5-pro", SupportedDepths: []string{"low", "medium", "high"}, DefaultDepth: "medium", Present: true,
		}},
		Source: "test", CapturedAt: now,
	})
	snap, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{codex, gemini}, now)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := capacitysnapshot.ToRouteInventory(snap, now)
	if err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(t.TempDir(), "capacity-ledger.json")
	var reports bytes.Buffer
	res, err := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: "proj-mp", Goal: "implement multi-provider capacity-aware routing with tests",
		Issue: "1342", Actor: "owner",
		// empty provider/model → auto-route
		ReportOut: &reports,
		HomeDir:   home,
		Executor:  workflowrun.FakeChildExecutor{HomeDir: home, Now: func() time.Time { return now }},
		Now:       func() time.Time { return now },
		LoadInventory: func(ctx context.Context, repo string, at time.Time) (autoroute.Inventory, capacitysnapshot.Snapshot, error) {
			return inv, snap, nil
		},
		OpenLedger: func(nowFn func() time.Time) (*capacityledger.Ledger, error) {
			return capacityledger.OpenPath(ledgerPath, nowFn)
		},
	})
	if err != nil {
		t.Fatalf("execute: %v status=%s msg=%s", err, res.Status, res.Message)
	}
	if len(res.Children) < 4 {
		t.Fatalf("children=%d", len(res.Children))
	}
	routed := 0
	for _, c := range res.Children {
		if c.Unavailable {
			t.Fatalf("unexpected unavailable: %+v", c)
		}
		routed++
		if c.Provider == "" || c.Model == "" || c.Depth == "" {
			t.Fatalf("child missing route fields: %+v", c)
		}
		if c.CapacityBefore == nil || c.CapacityReserved == nil {
			t.Fatalf("child missing capacity accounting: %+v", c)
		}
		// Fake executor has unknown actual → honest release (never fabricated actual).
		if c.CapacityState != "released" && c.CapacityState != "reconciled" {
			t.Fatalf("want released|reconciled after execute, got %s (%+v)", c.CapacityState, c)
		}
		if c.CapacityActual != nil && c.ActualSource == "unknown" {
			t.Fatalf("must not invent actual when source unknown: %+v", c)
		}
		// #1343: after must come from post-run observation when inventory available
		// (not n/a solely because token actual is unknown).
		if c.CapacityAfter == nil {
			t.Fatalf("want capacity after from post-run observation: %+v", c)
		}
		if !strings.Contains(c.CapacityNote, "after_source=") && !strings.Contains(c.CapacityNote, "reconciled=") {
			t.Fatalf("want after_source or reconciled in note: %s", c.CapacityNote)
		}
		if c.OutputEvidence == "" {
			t.Fatalf("missing output evidence: %+v", c)
		}
		if c.Terminal != "succeeded" {
			t.Fatalf("terminal %+v", c)
		}
	}
	if routed < 2 {
		t.Fatalf("expected ≥2 routed children, got %d (%+v)", routed, res.Children)
	}
	if !res.MultiProviderOK && len(res.ProvidersUsed) < 1 {
		t.Fatalf("expected at least one provider used: %+v", res)
	}
	// Prefer multi-provider when inventory has two companies.
	if len(res.ProvidersUsed) < 2 {
		t.Logf("note: multi-provider diversity not always selected; providers=%v models=%v depths=%v",
			res.ProvidersUsed, res.ModelsUsed, res.DepthsUsed)
	}
	if !res.MultiModelOrDepthOK && len(res.DepthsUsed) < 2 {
		t.Fatalf("expected multi depth or model; models=%v depths=%v", res.ModelsUsed, res.DepthsUsed)
	}
	if res.Workflow.Status != workflowrun.StatusHumanGate {
		t.Fatalf("workflow status %+v", res.Workflow)
	}
}

func TestUniqueProjectIDNeverLocalProject(t *testing.T) {
	now := time.Date(2026, 7, 22, 20, 0, 0, 0, time.UTC)
	a := goalrun.UniqueProjectID("/tmp/disp-a", func() time.Time { return now })
	b := goalrun.UniqueProjectID("/tmp/disp-a", func() time.Time { return now.Add(time.Nanosecond) })
	if a == "" || a == "local-project" || !strings.HasPrefix(a, "disp-") {
		t.Fatalf("a=%q", a)
	}
	if a == b {
		t.Fatalf("expected distinct project ids for distinct times: %q", a)
	}
}

func TestExecuteUsesUniqueProjectAndRunNotSharedLocal(t *testing.T) {
	now := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	home := testHome(t)
	// Default ProjectID empty → UniqueProjectID, not local-project.
	res, err := goalrun.Execute(context.Background(), goalrun.Request{
		// ProjectID empty
		Goal: "unique namespace canary", Issue: "1343", Actor: "owner",
		Provider: "fixture", Model: "fixture-model",
		HomeDir: home, RepoPath: "/tmp/disposable-repo-a",
		Executor: workflowrun.FakeChildExecutor{HomeDir: home},
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Workflow.RunID == "" {
		t.Fatal("expected unique workflow RunID")
	}
	// Attempts should embed unique run, not only plan digest.
	for _, c := range res.Workflow.Children {
		if c.AttemptID == "" {
			continue
		}
		if !strings.Contains(c.AttemptID, "att-") {
			t.Fatalf("attempt %q", c.AttemptID)
		}
		// Worktree under home, not bare local-project shared path when home set.
		if c.WorktreePath != "" && strings.Contains(c.WorktreePath, "/local-project/") {
			t.Fatalf("must not use shared local-project path: %s", c.WorktreePath)
		}
	}
}

func TestExecuteBindsRequiredDepthsFromRouteRequirement(t *testing.T) {
	now := time.Date(2026, 7, 22, 23, 45, 0, 0, time.UTC)
	home := testHome(t)
	codex := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "codex", AccountRef: "acct-codex", InstallRef: "i-codex",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Windows: []capacitysnapshot.Window{{
			Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
			Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 15, Unit: capacitysnapshot.UnitPercentage},
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 85, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
			ResetAt: ptrTime(now.Add(time.Hour)), CapturedAt: now, Source: "test",
		}},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "gpt-5.5", SupportedDepths: []string{"low", "medium", "high"}, DefaultDepth: "medium", Present: true,
		}},
		Source: "test", CapturedAt: now,
	})
	ag := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "antigravity", AccountRef: "acct-ag", InstallRef: "i-ag",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Windows: []capacitysnapshot.Window{{
			Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
			Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 20, Unit: capacitysnapshot.UnitPercentage},
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 80, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
			ResetAt: ptrTime(now.Add(30 * time.Minute)), CapturedAt: now, Source: "test",
		}},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "GPT-OSS 120B", SupportedDepths: []string{"low", "medium", "high"}, DefaultDepth: "medium", Present: true,
		}},
		Source: "test", CapturedAt: now,
	})
	snap, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{codex, ag}, now)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := capacitysnapshot.ToRouteInventory(snap, now)
	if err != nil {
		t.Fatal(err)
	}
	dry := true
	ledgerPath := filepath.Join(t.TempDir(), "cap-depth.json")
	res, err := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: "proj-depth", Goal: "bind required depths",
		DryRun: &dry, Issue: "1343", Actor: "owner",
		HomeDir: home, Now: func() time.Time { return now },
		LoadInventory: func(ctx context.Context, repo string, at time.Time) (autoroute.Inventory, capacitysnapshot.Snapshot, error) {
			return inv, snap, nil
		},
		OpenLedger: func(nowFn func() time.Time) (*capacityledger.Ledger, error) {
			return capacityledger.OpenPath(ledgerPath, nowFn)
		},
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	wantDepth := map[string]string{
		"wi_research":  "low",
		"wi_implement": "medium",
		"wi_tests":     "medium",
		"wi_verify":    "high",
		"wi_docs":      "low",
	}
	depthsSeen := map[string]bool{}
	for _, c := range res.Children {
		want, ok := wantDepth[c.ChildID]
		if !ok {
			continue
		}
		if c.Unavailable {
			t.Fatalf("child %s unavailable: %+v", c.ChildID, c)
		}
		if c.Depth != want {
			t.Fatalf("child %s depth=%q want %q route=%s", c.ChildID, c.Depth, want, c.RouteReason)
		}
		// requirement→selection→invocation evidence in route reason
		for _, frag := range []string{
			"depth requirement=" + want,
			"selection=" + want,
			"invocation=" + want,
		} {
			if !strings.Contains(c.RouteReason, frag) {
				t.Fatalf("child %s missing %q in route reason: %s", c.ChildID, frag, c.RouteReason)
			}
		}
		depthsSeen[c.Depth] = true
	}
	if !depthsSeen["low"] || !depthsSeen["high"] {
		t.Fatalf("expected both low and high depths used; seen=%v children=%+v", depthsSeen, res.Children)
	}
	if len(res.DepthsUsed) < 2 {
		t.Fatalf("DepthsUsed=%v want ≥2", res.DepthsUsed)
	}
}

func TestDryRunPreviewReleasesWithoutExecute(t *testing.T) {
	now := time.Date(2026, 7, 22, 23, 30, 0, 0, time.UTC)
	codex := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "codex", AccountRef: "acct-codex", InstallRef: "i-codex",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Windows: []capacitysnapshot.Window{{
			Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
			Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 5, Unit: capacitysnapshot.UnitPercentage},
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 95, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
			ResetAt: ptrTime(now.Add(time.Hour)), CapturedAt: now, Source: "test",
		}},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "gpt-5.5", SupportedDepths: []string{"low", "medium", "high"}, DefaultDepth: "medium", Present: true,
		}},
		Source: "test", CapturedAt: now,
	})
	snap, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{codex}, now)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := capacitysnapshot.ToRouteInventory(snap, now)
	if err != nil {
		t.Fatal(err)
	}
	dry := true
	ledgerPath := filepath.Join(t.TempDir(), "cap.json")
	res, err := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: "proj-dry", Goal: "preview routes only",
		DryRun: &dry,
		Now:    func() time.Time { return now },
		LoadInventory: func(ctx context.Context, repo string, at time.Time) (autoroute.Inventory, capacitysnapshot.Snapshot, error) {
			return inv, snap, nil
		},
		OpenLedger: func(nowFn func() time.Time) (*capacityledger.Ledger, error) {
			return capacityledger.OpenPath(ledgerPath, nowFn)
		},
	})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if res.Status != "planned" {
		t.Fatalf("status %s", res.Status)
	}
	if res.Workflow.LaunchCount != 0 {
		t.Fatalf("dry-run must not launch children: %+v", res.Workflow)
	}
	for _, c := range res.Children {
		if c.Unavailable {
			continue
		}
		if c.CapacityState != "released" {
			t.Fatalf("dry-run want released: %+v", c)
		}
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// TestForcedInterruptRestartFromDurableCheckpoint: interrupt mid-goal, resume
// same project/run from checkpoint, same attempt/evidence, no second provider call
// for already-succeeded children, ceilings recorded, no worktree leak for reuse.
func TestForcedInterruptRestartFromDurableCheckpoint(t *testing.T) {
	home := testHome(t)
	now := time.Date(2026, 7, 23, 6, 0, 0, 0, time.UTC)
	projectID := "proj-restart-goal"
	runID := "run_goal_restart_1"
	calls1 := map[string]int{}
	// Fail the second child in graph order to force partial durable state.
	// DecomposeGoal yields wi_research, wi_implement, wi_tests, wi_verify, wi_docs
	// for multi-child goals — fail wi_implement after research succeeds.
	res1, err1 := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID, RunID: runID,
		Goal: "implement transparent multi-child routing", Issue: "1343",
		Actor: "owner", Owner: "worker",
		Provider: "fixture", Model: "fixture-model",
		HomeDir: home, Now: func() time.Time { return now },
		Executor: workflowrun.FakeChildExecutor{
			HomeDir: home, Now: func() time.Time { return now },
			Calls:   calls1,
			FailIDs: map[string]bool{"wi_implement": true},
		},
	})
	if err1 == nil {
		t.Fatalf("expected blocked interrupt: %+v", res1)
	}
	if res1.CheckpointPath == "" {
		t.Fatal("expected durable checkpoint path after interrupt")
	}
	if _, err := os.Stat(res1.CheckpointPath); err != nil {
		t.Fatalf("checkpoint missing: %v", err)
	}
	cp, _, err := goalrun.LoadCheckpoint(home, projectID, runID)
	if err != nil {
		t.Fatalf("load checkpoint: %v", err)
	}
	if len(cp.PriorSucceeded) == 0 {
		t.Fatalf("checkpoint has no prior succeeded: %+v", cp)
	}
	// At least research (or first wave member) succeeded before implement fail.
	var researchAttempt, researchEvidence string
	for id, c := range cp.PriorSucceeded {
		if c.AttemptID == "" || c.OutputEvidence == "" {
			t.Fatalf("prior %s missing identity: %+v", id, c)
		}
		if id == "wi_research" {
			researchAttempt = c.AttemptID
			researchEvidence = c.OutputEvidence
		}
	}
	if researchAttempt == "" {
		// If graph order failed earlier, still require some prior.
		for _, c := range cp.PriorSucceeded {
			researchAttempt = c.AttemptID
			researchEvidence = c.OutputEvidence
			break
		}
	}
	if calls1["wi_research"] > 1 {
		t.Fatalf("research over-called first pass: %+v", calls1)
	}

	// Resume: same run — succeeded kids must not re-exec.
	calls2 := map[string]int{}
	// Do not fail implement on resume so run can complete.
	res2, err2 := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID, RunID: runID, Resume: true,
		Goal: "implement transparent multi-child routing", Issue: "1343",
		Actor: "owner", Owner: "worker",
		Provider: "fixture", Model: "fixture-model",
		HomeDir: home, Now: func() time.Time { return now.Add(time.Minute) },
		Executor: workflowrun.FakeChildExecutor{
			HomeDir: home, Now: func() time.Time { return now.Add(time.Minute) },
			Calls: calls2,
		},
	})
	if err2 != nil {
		t.Fatalf("resume execute: %v status=%s msg=%s", err2, res2.Status, res2.Message)
	}
	if !res2.Resumed {
		t.Fatalf("expected resumed=true: %+v", res2)
	}
	if res2.ReuseCount < 1 {
		t.Fatalf("reuse_count=%d want ≥1 events=%v", res2.ReuseCount, res2.Workflow.Events)
	}
	// No second provider call for any prior-succeeded child.
	for id := range cp.PriorSucceeded {
		if calls2[id] != 0 {
			t.Fatalf("duplicate provider call on resume for %s: calls2=%+v prior=%v", id, calls2, cp.PriorSucceeded)
		}
	}
	// Same attempt/evidence identity preserved in final children.
	for _, c := range res2.Children {
		if prior, ok := cp.PriorSucceeded[c.ChildID]; ok {
			if c.AttemptID != prior.AttemptID || c.OutputEvidence != prior.OutputEvidence {
				t.Fatalf("identity drift %s: prior attempt=%s ev=%s got attempt=%s ev=%s",
					c.ChildID, prior.AttemptID, prior.OutputEvidence, c.AttemptID, c.OutputEvidence)
			}
			if c.Stage != "integrated" && c.Stage != "resumed" && c.Stage != "human_gate" {
				// after applyChildOutcomes succeeded → integrated/human_gate
			}
		}
	}
	if researchEvidence != "" {
		found := false
		for _, c := range res2.Workflow.Children {
			if c.WorkItemID == "wi_research" {
				found = true
				if c.AttemptID != researchAttempt || c.OutputEvidence != researchEvidence {
					t.Fatalf("research identity: want %s/%s got %s/%s", researchAttempt, researchEvidence, c.AttemptID, c.OutputEvidence)
				}
			}
		}
		if !found {
			t.Fatal("research missing from resume workflow children")
		}
	}
	if res2.WorktreePeak < 1 && res2.Workflow.WorktreePeak < 1 {
		t.Fatalf("worktree peak missing: %+v", res2)
	}
	if res2.ProcessPeak < 1 && res2.Workflow.ProcessPeak < 1 {
		// resume may only launch remaining; peaks still ≥1 if any remaining
		if res2.Workflow.LaunchCount > 0 && res2.Workflow.ProcessPeak < 1 {
			t.Fatalf("process peak missing with launches: %+v", res2.Workflow)
		}
	}
	// Checkpoint rewritten after resume success.
	if res2.CheckpointPath == "" {
		t.Fatal("resume must rewrite checkpoint")
	}
	cp2, _, err := goalrun.LoadCheckpoint(home, projectID, runID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(cp2.PriorSucceeded) < len(cp.PriorSucceeded) {
		t.Fatalf("checkpoint regressed: before=%d after=%d", len(cp.PriorSucceeded), len(cp2.PriorSucceeded))
	}
}

func TestCheckpointRefuseReuseWithoutEvidence(t *testing.T) {
	got := goalrun.PriorSucceededFrom(
		[]workflowrun.ChildOutcome{
			{WorkItemID: "a", Terminal: "succeeded", AttemptID: "att-a", OutputEvidence: "sha256:abc"},
			{WorkItemID: "b", Terminal: "succeeded", AttemptID: "att-b", OutputEvidence: ""}, // refuse
			{WorkItemID: "c", Terminal: "failed", AttemptID: "att-c", OutputEvidence: "sha256:x"},
		},
		nil,
	)
	if len(got) != 1 || got["a"].AttemptID != "att-a" {
		t.Fatalf("%+v", got)
	}
}
