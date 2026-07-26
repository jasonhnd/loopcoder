package artifactqual_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/artifactqual"
	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/workclaim"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

func ptrTimeCanary(t time.Time) *time.Time {
	u := t.UTC()
	return &u
}

func TestValidateCanaryEvidenceRequiresRealRuntime(t *testing.T) {
	now := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sha := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	rem := 0.9
	after := 0.85
	ev := artifactqual.CanaryEvidence{
		Schema:        artifactqual.SchemaCanaryEvidence,
		ArchiveDigest: digest,
		PreProdSHA:    sha,
		BinaryVersion: "0.9.0-rc.9",
		BinaryCommit:  sha,
		ProjectID:     "disp-test123",
		RunID:         "run_test1",
		ProducedAt:    now,
		ProviderObservations: []artifactqual.CanaryProviderObs{
			{Provider: "codex", AccountRef: "acct-codex", InstallRef: "pinst_codex", WindowKind: "five_hour", Source: "codexbar", Freshness: "fresh", Confidence: "exact", Remaining: &rem, CapturedAt: now.Add(-time.Minute), ResetAt: ptrTimeCanary(now.Add(2 * time.Hour))},
			{Provider: "antigravity", AccountRef: "acct-antigravity", InstallRef: "pinst_antigravity", WindowKind: "five_hour", Source: "codexbar", Freshness: "fresh", Confidence: "exact", Remaining: &rem, CapturedAt: now.Add(-time.Minute), ResetAt: ptrTimeCanary(now.Add(2 * time.Hour))},
		},
		Children: []artifactqual.CanaryChild{
			child("wi_research", "att-r-1", "codex", "gpt-5.5", "low", "succeeded", 0.96, 0.05, after),
			child("wi_implement", "att-i-1", "antigravity", "GPT-OSS", "medium", "succeeded", 0.9, 0.05, after),
			child("wi_tests", "att-t-1", "antigravity", "GPT-OSS", "medium", "succeeded", 0.9, 0.05, after),
			child("wi_verify", "att-v-1", "codex", "gpt-5.5", "high", "succeeded", 0.96, 0.05, after),
		},
		UnavailableRetry: &artifactqual.CanaryUnavailableRetry{
			ExcludedProvider: "codex-exhausted", ExcludedReason: "exhausted",
			NoDuplicateClaim: true, NoDuplicateFiles: true, NoDoubleCapacity: true,
			EvidenceRef: "events:hard_exclude:codex-exhausted",
		},
		Restart: validRestart(4),
		PR:      validPR(sha),
	}
	completeRawCanaryEvidence(&ev, now)
	// Live PR observation required — manifest booleans alone cannot green RealPROK.
	live := &artifactqual.PRLiveState{
		Repository: "jasonhnd/loopcoder", Number: 9999,
		URL:     "https://github.com/jasonhnd/loopcoder/pull/9999",
		BaseRef: "main", HeadOID: sha, State: "open",
		RequiredChecksGreen: true, HumanMergeGate: true,
		ChecksAtHead: []artifactqual.PRCheck{
			{Name: "verify", Status: "completed", Conclusion: "success"},
			{Name: "test", Status: "completed", Conclusion: "success"},
		},
	}
	// Bind verifier evidence to wi_verify child OutputEvidence @ disposable/RC head used by PR.
	for i := range ev.Children {
		if ev.Children[i].WorkItemID == "wi_verify" {
			ev.PR.VerifierAttemptID = ev.Children[i].AttemptID
			ev.PR.VerifierProvider = ev.Children[i].Provider
			ev.PR.IndependentVerifier = ev.Children[i].Provider
			ev.PR.VerifierEvidenceRef = ev.Children[i].OutputEvidence + "@head:" + sha
		}
	}
	ev.ContentDigest = artifactqual.DigestCanaryBody(ev)
	v := artifactqual.ValidateCanaryEvidence(ev, digest, sha, now, artifactqual.CanaryValidateOpts{
		ExpectedPRHeadOID: sha, LivePR: live,
	})
	if !v.Valid {
		t.Fatalf("want valid: %v", v.Reasons)
	}
	if !v.MultiDepthOK || !v.MultiProviderOK || !v.CapacityAfterOK ||
		!v.UnavailableRetryOK || !v.RestartOK || !v.RealPROK {
		t.Fatalf("%+v", v)
	}
}

func TestValidateCanaryClaimFenceGenerationMayLagAttemptGeneration(t *testing.T) {
	now := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	ev, digest, sha := baseValidCanary(now)
	ev.RawClaims = append(ev.RawClaims, workclaim.Claim{
		Schema: workclaim.SchemaClaim, ClaimID: "wcl-late", ProjectID: ev.ProjectID,
		GraphID: "g", GraphVersion: 1, WorkItemID: "wi_late",
		AttemptID: "att-wi_late-g1", ExecutorID: workflowrun.WorkflowrunExecutorID,
		Generation: 1, State: workclaim.StateClosed, Terminal: "succeeded",
		OutputEvidence: deterministicSHA256Evidence("late"),
	})
	ev.DurableEvidenceDigest = artifactqual.DigestDurableEvidence(ev)
	ev.ContentDigest = artifactqual.DigestCanaryBody(ev)
	v := artifactqual.ValidateCanaryEvidence(ev, digest, sha, now)
	if strings.Contains(strings.Join(v.Reasons, ";"), "raw_claim_envelope_invalid") {
		t.Fatalf("independent claim fence generation must not invalidate raw envelope: %v", v.Reasons)
	}
}

func validPR(head string) *artifactqual.CanaryPR {
	return &artifactqual.CanaryPR{
		URL: "https://github.com/jasonhnd/loopcoder/pull/9999", Number: 9999,
		Repository: "jasonhnd/loopcoder", Branch: "loopcoder/goal-x", BaseRef: "main",
		HeadOID: head, RequiredChecks: []string{"verify", "test"}, RequiredChecksGreen: true,
		IndependentVerifier: "codex", VerifierProvider: "codex", VerifierAttemptID: "att-v-1",
		// Bound in tests to verify child OutputEvidence@head; default uses deterministic wi_verify evidence.
		VerifierEvidenceRef: deterministicSHA256Evidence("wi_verify") + "@head:" + head,
		CreatedByLoopCoder:  true, AutoMerge: false, HumanMergeGate: true,
	}
}

func validRestart(useful int) *artifactqual.CanaryRestart {
	return &artifactqual.CanaryRestart{
		Interrupted: true, ResumedFromDurable: true, ExactlyOnce: true,
		ChildCountUseful: useful, ProcessCeilingOK: true, WorktreeCeilingOK: true,
		NoLeakedProcesses: true, NoRepoLocalRuntime: true,
		EvidenceRef: "/tmp/run/workflow-events.jsonl#sha256:deadbeef",
		ProcessPeak: 1, WorktreePeak: 1,
		ProcessActive: 0, WorktreeActive: 0,
		ProcessLimit:       artifactqual.ProductionSequentialCeiling,
		WorktreeLimit:      artifactqual.ProductionSequentialCeiling,
		ReuseCountMeasured: 1, AbortedAttemptCount: 1,
		ActiveOccupancyMeasured:   true,
		RepoLocalRuntimeChecked:   true,
		RepoLocalRuntimePresent:   false,
		DuplicateLaunch:           false,
		DuplicateSuccessIntegrate: false,
		AbortedAttemptSucceeded:   false,
		LaterGenerationResume:     true,
	}
}

func TestValidateCanaryRejectsLocalProjectAndDigestMismatch(t *testing.T) {
	now := time.Now().UTC()
	ev := artifactqual.CanaryEvidence{
		Schema: artifactqual.SchemaCanaryEvidence, ArchiveDigest: "aa", PreProdSHA: "bb",
		ProjectID: "local-project", RunID: "r1", ProducedAt: now,
		BinaryVersion: "x",
	}
	v := artifactqual.ValidateCanaryEvidence(ev, "cc", "bb", now)
	if v.Valid {
		t.Fatal("must reject")
	}
	joined := ""
	for _, r := range v.Reasons {
		joined += r + ";"
	}
	if !contains(joined, "archive_digest_mismatch") || !contains(joined, "project_id_not_unique") {
		t.Fatalf("reasons=%v", v.Reasons)
	}
}

func TestLoadCanaryEvidenceSchema(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	_ = os.WriteFile(p, []byte(`{"schema":"wrong"}`), 0o600)
	if _, err := artifactqual.LoadCanaryEvidence(p); err == nil {
		t.Fatal("want schema error")
	}
	good := artifactqual.CanaryEvidence{Schema: artifactqual.SchemaCanaryEvidence, ProjectID: "disp-x", RunID: "r"}
	b, _ := json.Marshal(good)
	_ = os.WriteFile(p, b, 0o600)
	if _, err := artifactqual.LoadCanaryEvidence(p); err != nil {
		t.Fatal(err)
	}
}

func child(id, att, prov, model, depth, term string, before, reserved, after float64) artifactqual.CanaryChild {
	b, r, a := before, reserved, after
	// Real observed-after metadata (not derived/default). Captured near canary now.
	obsAt := time.Date(2026, 7, 22, 20, 55, 0, 0, time.UTC)
	act := before - after
	if act < 0 {
		act = 0
	}
	inst := "pinst_" + prov
	taskClass := "luna"
	switch id {
	case "wi_verify":
		taskClass = "soul"
	case "wi_implement", "wi_tests":
		taskClass = "tera"
	}
	// Deterministic valid sha256: + 64 hex from id (pad/hash-like).
	outEvid := deterministicSHA256Evidence(id)
	// Future reset vs observation (finite/fixed window identity).
	resetAt := obsAt.Add(2 * time.Hour)
	return artifactqual.CanaryChild{
		ChildID: id, WorkItemID: id, TaskClass: taskClass, OutputEvidence: outEvid,
		AttemptID: att, Provider: prov, Model: model,
		DepthRequired: depth, DepthSelected: depth, DepthInvocation: depth,
		Terminal: term, CapacityBefore: &b, CapacityReserved: &r, CapacityAfter: &a,
		CapacityActual: &act, ActualSource: "estimated_group_delta_token_weighted:obs|codexbar|t", ActualConfidence: "estimated",
		AfterSource: "codexbar", AfterFreshness: "fresh", AfterState: "observed",
		AfterConfidence: "exact", AfterObservedAt: obsAt,
		BeforeSource: "codexbar", BeforeFreshness: "fresh", BeforeConfidence: "exact",
		BeforeCapturedAt:     obsAt,
		ResetAt:              &resetAt,
		RealProviderExecuted: true, WorktreePath: "/tmp/wt/" + id,
		AccountRef: "acct-" + prov, InstallRef: inst, WindowKind: "five_hour",
		ActualSources: &artifactqual.CanaryRouteSources{
			Model: "provider_stream", Effort: "accepted_invocation",
			Permission: "accepted_invocation", Account: "auth_binding", Install: "install_binding",
		},
		ArgvDigest: "sha256:canary-argv-" + id,
	}
}

func completeRawCanaryEvidence(ev *artifactqual.CanaryEvidence, now time.Time) {
	const (
		inventoryDigest = "sha256:live-inventory-before"
		afterDigest     = "sha256:live-inventory-after"
	)
	ev.InventoryProvenance = "live_discover"
	ev.InventoryReportDigest = inventoryDigest
	var ledger []capacityledger.Entry
	for i := range ev.Children {
		child := &ev.Children[i]
		if !child.RealProviderExecuted || child.Terminal != "succeeded" {
			continue
		}
		child.InstallRef += "-" + child.ChildID
		child.FilesTouched = []string{"product/" + child.ChildID + ".md"}
		beforeAt := child.BeforeCapturedAt.UTC()
		afterAt := child.AfterObservedAt.UTC()
		reset := child.ResetAt
		ledger = append(ledger, capacityledger.Entry{
			Schema: capacityledger.SchemaEntry, ProjectID: ev.ProjectID, RunID: ev.RunID,
			AttemptID: child.AttemptID, Provider: child.Provider, Model: child.Model,
			Depth: child.DepthInvocation, AccountRef: child.AccountRef, InstallRef: child.InstallRef,
			WindowKind: child.WindowKind, Confidence: "exact", Freshness: "fresh",
			ResetAt: reset, Before: *child.CapacityBefore, Reserved: *child.CapacityReserved,
			Actual: child.CapacityActual, ActualSource: child.ActualSource,
			ActualConfidence: "estimated", BeforeSource: child.BeforeSource,
			BeforeCapturedAt: &beforeAt, BeforeInventoryDigest: inventoryDigest,
			After: child.CapacityAfter, AfterState: "observed", AfterSource: child.AfterSource,
			AfterObservedAt: &afterAt, AfterFreshness: "fresh", AfterConfidence: "exact",
			AfterInventoryDigest: afterDigest, ReservationID: "res-" + child.AttemptID,
			State: "reconciled", IdempotencyKey: ev.ProjectID + "|" + ev.RunID + "|" + child.AttemptID,
			CreatedAt: now.Add(-10 * time.Minute), UpdatedAt: now,
		})
	}
	// Claimed model_unavailable followed by one exact higher-generation retry.
	failedEvidence := deterministicSHA256Evidence("wi_retry_failed")
	retry := child("wi_retry", "att-wi_retry-g1", "codex", "gpt-5.5", "medium", "succeeded", 0.90, 0.05, 0.85)
	retry.BeforeCapturedAt = now.Add(-5 * time.Minute)
	retry.AfterObservedAt = now.Add(-4 * time.Minute)
	retry.ResetAt = ptrTimeCanary(now.Add(2 * time.Hour))
	retry.InstallRef += "-wi_retry"
	retry.FilesTouched = []string{"product/wi_retry.md"}
	failed := retry
	failed.AttemptID = "att-wi_retry-g0"
	failed.Model = "gpt-unavailable"
	failed.Terminal = "failed"
	failed.OutputEvidence = failedEvidence
	failed.RealProviderExecuted = false
	failed.FilesTouched = nil
	failed.CapacityActual = nil
	failed.CapacityAfter = nil
	ev.Children = append(ev.Children, failed, retry)
	beforeAt := retry.BeforeCapturedAt.UTC()
	afterAt := retry.AfterObservedAt.UTC()
	ledger = append(ledger,
		capacityledger.Entry{
			Schema: capacityledger.SchemaEntry, ProjectID: ev.ProjectID, RunID: ev.RunID,
			AttemptID: failed.AttemptID, Provider: failed.Provider, Model: failed.Model,
			Depth: failed.DepthInvocation, AccountRef: failed.AccountRef, InstallRef: failed.InstallRef,
			WindowKind: failed.WindowKind, Confidence: "exact", Freshness: "fresh",
			ResetAt: failed.ResetAt, Before: *failed.CapacityBefore, Reserved: *failed.CapacityReserved,
			BeforeSource: failed.BeforeSource, BeforeCapturedAt: &beforeAt,
			BeforeInventoryDigest: inventoryDigest, ReservationID: "res-" + failed.AttemptID,
			State: "released", IdempotencyKey: ev.ProjectID + "|" + ev.RunID + "|" + failed.AttemptID,
			CreatedAt: now.Add(-10 * time.Minute), UpdatedAt: now,
		},
		capacityledger.Entry{
			Schema: capacityledger.SchemaEntry, ProjectID: ev.ProjectID, RunID: ev.RunID,
			AttemptID: retry.AttemptID, Provider: retry.Provider, Model: retry.Model,
			Depth: retry.DepthInvocation, AccountRef: retry.AccountRef, InstallRef: retry.InstallRef,
			WindowKind: retry.WindowKind, Confidence: "exact", Freshness: "fresh",
			ResetAt: retry.ResetAt, Before: *retry.CapacityBefore, Reserved: *retry.CapacityReserved,
			Actual: retry.CapacityActual, ActualSource: retry.ActualSource, ActualConfidence: "estimated",
			BeforeSource: retry.BeforeSource, BeforeCapturedAt: &beforeAt,
			BeforeInventoryDigest: inventoryDigest, After: retry.CapacityAfter,
			AfterState: "observed", AfterSource: retry.AfterSource, AfterObservedAt: &afterAt,
			AfterFreshness: "fresh", AfterConfidence: "exact", AfterInventoryDigest: afterDigest,
			ReservationID: "res-" + retry.AttemptID, State: "reconciled",
			IdempotencyKey: ev.ProjectID + "|" + ev.RunID + "|" + retry.AttemptID,
			CreatedAt:      now.Add(-10 * time.Minute), UpdatedAt: now,
		},
	)
	event := func(id, kind, wi, att string, generation int) workflowrun.Event {
		return workflowrun.Event{
			Schema: workflowrun.EventSchema, EventID: id, At: now.Add(-time.Minute),
			ProjectID: ev.ProjectID, RunID: ev.RunID, Kind: kind,
			WorkItemID: wi, AttemptID: att, Generation: generation,
		}
	}
	failedClaim := event("wev-u-1", "claim", "wi_retry", failed.AttemptID, 1)
	failedLaunch := event("wev-u-2", "launch", "wi_retry", failed.AttemptID, 1)
	modelUnavailable := event("wev-u-3", "model_unavailable", "wi_retry", failed.AttemptID, 1)
	modelUnavailable.FailureClass, modelUnavailable.Terminal, modelUnavailable.Evidence =
		"model_unavailable", "failed", failedEvidence
	modelUnavailable.Payload = []byte(`{"attempt_id":"att-wi_retry-g0","failure_class":"model_unavailable","model":"gpt-unavailable","provider":"codex","work_item_id":"wi_retry"}`)
	failedTerminal := event("wev-u-4", "terminal", "wi_retry", failed.AttemptID, 1)
	failedTerminal.FailureClass, failedTerminal.Terminal, failedTerminal.Evidence =
		"model_unavailable", "failed", failedEvidence
	retryClaim := event("wev-u-5", "claim", "wi_retry", retry.AttemptID, 2)
	retryClaim.Payload = []byte(`{"retry_attempt_id":"att-wi_retry-g1","supersedes_attempt_id":"att-wi_retry-g0"}`)
	reroute := event("wev-u-6", "reroute", "wi_retry", retry.AttemptID, 2)
	retryLaunch := event("wev-u-7", "launch", "wi_retry", retry.AttemptID, 2)
	retryTerminal := event("wev-u-8", "terminal", "wi_retry", retry.AttemptID, 2)
	retryTerminal.Terminal, retryTerminal.Evidence = "succeeded", retry.OutputEvidence
	retryIntegrate := event("wev-u-9", "integrate", "wi_retry", retry.AttemptID, 2)
	interrupt := event("wev-r-2", "interrupt", "wi_restart", "att-wi_restart-g0", 1)
	interrupt.FailureClass, interrupt.Terminal = "forced_interrupt", "cancelled"
	interrupt.Payload = []byte(`{"attempt_id":"att-wi_restart-g0","failure_class":"forced_interrupt","generation":"1","interrupt_class":"service_forced_interrupt","interrupt_id":"iint-exact","terminal":"cancelled","work_item_id":"wi_restart"}`)
	cancelled := event("wev-r-3", "terminal", "wi_restart", "att-wi_restart-g0", 1)
	cancelled.FailureClass, cancelled.Terminal = "forced_interrupt", "cancelled"
	cancelled.Payload = interrupt.Payload
	reusedEvidence := deterministicSHA256Evidence("wi_reused")
	reused := event("wev-r-4", "reuse", "wi_reused", "att-wi_reused-g0", 1)
	reused.Terminal, reused.Evidence = "succeeded", reusedEvidence
	ev.RawEvents = []workflowrun.Event{
		event("wev-r-1", "launch", "wi_restart", "att-wi_restart-g0", 1),
		interrupt, cancelled,
		reused,
		event("wev-r-5", "launch", "wi_restart", "att-wi_restart-g1", 2),
		failedClaim, failedLaunch, modelUnavailable, failedTerminal,
		retryClaim, reroute, retryLaunch, retryTerminal, retryIntegrate,
	}
	ev.RawClaims = []workclaim.Claim{
		{Schema: workclaim.SchemaClaim, ClaimID: "wcl-r0", ProjectID: ev.ProjectID,
			GraphID: "g", GraphVersion: 1, WorkItemID: "wi_restart",
			AttemptID: "att-wi_restart-g0", ExecutorID: "workflowrun", Generation: 1,
			State: workclaim.StateClosed, Terminal: "cancelled"},
		{Schema: workclaim.SchemaClaim, ClaimID: "wcl-r1", ProjectID: ev.ProjectID,
			GraphID: "g", GraphVersion: 1, WorkItemID: "wi_restart",
			AttemptID: "att-wi_restart-g1", ExecutorID: "workflowrun", Generation: 2,
			State: workclaim.StateClosed, Terminal: "succeeded", OutputEvidence: deterministicSHA256Evidence("restart")},
		{Schema: workclaim.SchemaClaim, ClaimID: "wcl-reuse", ProjectID: ev.ProjectID,
			GraphID: "g", GraphVersion: 1, WorkItemID: "wi_reused",
			AttemptID: "att-wi_reused-g0", ExecutorID: "workflowrun", Generation: 1,
			State: workclaim.StateClosed, Terminal: "succeeded", OutputEvidence: reusedEvidence},
		{Schema: workclaim.SchemaClaim, ClaimID: "wcl-u0", ProjectID: ev.ProjectID,
			GraphID: "g", GraphVersion: 1, WorkItemID: "wi_retry",
			AttemptID: failed.AttemptID, ExecutorID: "workflowrun", Generation: 1,
			State: workclaim.StateClosed, Terminal: "failed", OutputEvidence: failedEvidence},
		{Schema: workclaim.SchemaClaim, ClaimID: "wcl-u1", ProjectID: ev.ProjectID,
			GraphID: "g", GraphVersion: 1, WorkItemID: "wi_retry",
			AttemptID: retry.AttemptID, ExecutorID: "workflowrun", Generation: 2,
			State: workclaim.StateClosed, Terminal: "succeeded", OutputEvidence: retry.OutputEvidence},
	}
	ev.RawLedgerEntries = ledger
	ev.UnavailableRetry = &artifactqual.CanaryUnavailableRetry{
		ExcludedProvider: "codex", ExcludedReason: "model_unavailable",
		RetryAttemptID: retry.AttemptID, NoDuplicateClaim: true,
		NoDuplicateFiles: true, NoDoubleCapacity: true,
	}
	if ev.Restart == nil {
		ev.Restart = validRestart(5)
	}
	ev.Restart.ChildCountUseful = 5
	ev.ProviderObservations = nil
	for i := range ledger {
		entry := ledger[i]
		before := entry.Before
		ev.ProviderObservations = append(ev.ProviderObservations, artifactqual.CanaryProviderObs{
			Provider: entry.Provider, AccountRef: entry.AccountRef,
			InstallRef: entry.InstallRef, WindowKind: entry.WindowKind,
			Source: entry.BeforeSource, Freshness: entry.Freshness,
			Confidence: string(entry.Confidence), Remaining: &before,
			CapturedAt: timeValueForTest(entry.BeforeCapturedAt), ResetAt: entry.ResetAt,
			InventoryReportDigest: entry.BeforeInventoryDigest,
		})
		if entry.After != nil {
			after := *entry.After
			ev.ProviderObservations = append(ev.ProviderObservations, artifactqual.CanaryProviderObs{
				Provider: entry.Provider, AccountRef: entry.AccountRef,
				InstallRef: entry.InstallRef, WindowKind: entry.WindowKind,
				Source: entry.AfterSource, Freshness: entry.AfterFreshness,
				Confidence: string(entry.AfterConfidence), Remaining: &after,
				CapturedAt: timeValueForTest(entry.AfterObservedAt), ResetAt: entry.ResetAt,
				InventoryReportDigest: entry.AfterInventoryDigest,
			})
		}
	}
	ev.DurableEvidenceDigest = artifactqual.DigestDurableEvidence(*ev)
	ev.ContentDigest = artifactqual.DigestCanaryBody(*ev)
}

func timeValueForTest(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return value.UTC()
}

// deterministicSHA256Evidence returns sha256: + 64 hex derived from seed (not a real hash).
func deterministicSHA256Evidence(seed string) string {
	const hex = "0123456789abcdef"
	var b strings.Builder
	b.WriteString("sha256:")
	for i := 0; i < 64; i++ {
		if i < len(seed) {
			b.WriteByte(hex[int(seed[i])%16])
		} else {
			b.WriteByte(hex[i%16])
		}
	}
	return b.String()
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		(len(s) > 0 && (func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		})()))
}

// Four successes on one provider + failed second provider must not qualify multi-provider.
func TestValidateCanary_FailedSecondProviderNotMultiProvider(t *testing.T) {
	now := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sha := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	rem := 0.9
	after := 0.85
	failed := child("wi_fail_grok", "att-f-1", "grok", "grok-4.5", "medium", "failed", 0.9, 0.05, after)
	failed.RealProviderExecuted = false
	failed.ArgvDigest = ""
	failed.ActualSources = &artifactqual.CanaryRouteSources{
		Account: "auth_binding", Install: "install_binding",
	}
	ev := artifactqual.CanaryEvidence{
		Schema: artifactqual.SchemaCanaryEvidence, ArchiveDigest: digest, PreProdSHA: sha,
		BinaryVersion: "0.9.0-rc.9", BinaryCommit: sha, ProjectID: "disp-test123", RunID: "run_fail_prov",
		ProducedAt: now,
		ProviderObservations: []artifactqual.CanaryProviderObs{
			{Provider: "codex", AccountRef: "acct-codex", InstallRef: "pinst_codex", WindowKind: "five_hour", Source: "codexbar", Freshness: "fresh", Confidence: "exact", Remaining: &rem, CapturedAt: now.Add(-time.Minute), ResetAt: ptrTimeCanary(now.Add(2 * time.Hour))},
			{Provider: "grok", AccountRef: "acct-grok", InstallRef: "pinst_grok", WindowKind: "five_hour", Source: "codexbar", Freshness: "fresh", Confidence: "exact", Remaining: &rem, CapturedAt: now.Add(-time.Minute), ResetAt: ptrTimeCanary(now.Add(2 * time.Hour))},
		},
		Children: []artifactqual.CanaryChild{
			child("wi_research", "att-r-1", "codex", "gpt-5.5", "low", "succeeded", 0.96, 0.05, after),
			child("wi_implement", "att-i-1", "codex", "gpt-5.5", "medium", "succeeded", 0.96, 0.05, after),
			child("wi_tests", "att-t-1", "codex", "gpt-5.5", "medium", "succeeded", 0.96, 0.05, after),
			child("wi_verify", "att-v-1", "codex", "gpt-5.5", "high", "succeeded", 0.96, 0.05, after),
			failed,
		},
		UnavailableRetry: &artifactqual.CanaryUnavailableRetry{
			ExcludedProvider: "codex-exhausted", ExcludedReason: "exhausted",
			NoDuplicateClaim: true, NoDuplicateFiles: true, NoDoubleCapacity: true,
			EvidenceRef: "events:hard_exclude:codex-exhausted",
		},
		Restart: validRestart(4),
		PR: &artifactqual.CanaryPR{
			URL: "https://github.com/jasonhnd/loopcoder/pull/9999", Number: 9999,
			RequiredChecks: []string{"verify", "test"}, RequiredChecksGreen: true,
			IndependentVerifier: "loopreview", VerifierEvidenceRef: "sha256:verifdeadbeef",
			CreatedByLoopCoder: true,
		},
	}
	v := artifactqual.ValidateCanaryEvidence(ev, digest, sha, now)
	if v.MultiProviderOK {
		t.Fatalf("failed second provider must not green multi-provider; providers=%v useful=%d", v.Providers, v.UsefulChildren)
	}
	if v.UsefulChildren != 4 {
		t.Fatalf("useful=%d want 4 succeeded codex only", v.UsefulChildren)
	}
	if len(v.Providers) != 1 || v.Providers[0] != "openai" {
		t.Fatalf("providers=%v want only canonical openai", v.Providers)
	}
}

// Four successes at one depth + failed second depth must not qualify multi-depth.
func TestValidateCanary_FailedSecondDepthNotMultiDepth(t *testing.T) {
	now := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sha := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	rem := 0.9
	after := 0.85
	// Four succeeded medium + one failed high on second provider.
	failedHigh := child("wi_fail_high", "att-fh-1", "antigravity", "GPT-OSS", "high", "failed", 0.9, 0.05, after)
	failedHigh.RealProviderExecuted = false
	failedHigh.ArgvDigest = ""
	// Succeeded rows all medium (single depth).
	ev := artifactqual.CanaryEvidence{
		Schema: artifactqual.SchemaCanaryEvidence, ArchiveDigest: digest, PreProdSHA: sha,
		BinaryVersion: "0.9.0-rc.9", BinaryCommit: sha, ProjectID: "disp-test123", RunID: "run_fail_depth",
		ProducedAt: now,
		ProviderObservations: []artifactqual.CanaryProviderObs{
			{Provider: "codex", AccountRef: "acct-codex", InstallRef: "pinst_codex", WindowKind: "five_hour", Source: "codexbar", Freshness: "fresh", Confidence: "exact", Remaining: &rem, CapturedAt: now.Add(-time.Minute), ResetAt: ptrTimeCanary(now.Add(2 * time.Hour))},
			{Provider: "antigravity", AccountRef: "acct-antigravity", InstallRef: "pinst_antigravity", WindowKind: "five_hour", Source: "codexbar", Freshness: "fresh", Confidence: "exact", Remaining: &rem, CapturedAt: now.Add(-time.Minute), ResetAt: ptrTimeCanary(now.Add(2 * time.Hour))},
		},
		Children: []artifactqual.CanaryChild{
			child("wi_a", "att-a-1", "codex", "gpt-5.5", "medium", "succeeded", 0.96, 0.05, after),
			child("wi_b", "att-b-1", "antigravity", "GPT-OSS", "medium", "succeeded", 0.9, 0.05, after),
			child("wi_c", "att-c-1", "codex", "gpt-5.5", "medium", "succeeded", 0.96, 0.05, after),
			child("wi_d", "att-d-1", "antigravity", "GPT-OSS", "medium", "succeeded", 0.9, 0.05, after),
			failedHigh,
		},
		UnavailableRetry: &artifactqual.CanaryUnavailableRetry{
			ExcludedProvider: "codex-exhausted", ExcludedReason: "exhausted",
			NoDuplicateClaim: true, NoDuplicateFiles: true, NoDoubleCapacity: true,
			EvidenceRef: "events:hard_exclude:codex-exhausted",
		},
		Restart: validRestart(4),
		PR: &artifactqual.CanaryPR{
			URL: "https://github.com/jasonhnd/loopcoder/pull/9999", Number: 9999,
			RequiredChecks: []string{"verify", "test"}, RequiredChecksGreen: true,
			IndependentVerifier: "loopreview", VerifierEvidenceRef: "sha256:verifdeadbeef",
			CreatedByLoopCoder: true,
		},
	}
	v := artifactqual.ValidateCanaryEvidence(ev, digest, sha, now)
	if v.MultiDepthOK {
		t.Fatalf("failed second depth must not green multi-depth; depths=%v", v.Depths)
	}
	if len(v.Depths) != 1 || v.Depths[0] != "medium" {
		t.Fatalf("depths=%v want only medium", v.Depths)
	}
}

func TestValidateCanaryRejectsEligibleNotChosenUnavailableRetry(t *testing.T) {
	now := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sha := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	rem := 0.9
	after := 0.85
	base := func(reason string) artifactqual.CanaryEvidence {
		return artifactqual.CanaryEvidence{
			Schema: artifactqual.SchemaCanaryEvidence, ArchiveDigest: digest, PreProdSHA: sha,
			BinaryVersion: "0.9.0-rc.9", BinaryCommit: sha, ProjectID: "disp-test123", RunID: "run_test1",
			ProducedAt: now,
			ProviderObservations: []artifactqual.CanaryProviderObs{
				{Provider: "codex", AccountRef: "acct-codex", InstallRef: "pinst_codex", WindowKind: "five_hour", Source: "codexbar", Freshness: "fresh", Confidence: "exact", Remaining: &rem, CapturedAt: now.Add(-time.Minute), ResetAt: ptrTimeCanary(now.Add(2 * time.Hour))},
				{Provider: "antigravity", AccountRef: "acct-antigravity", InstallRef: "pinst_antigravity", WindowKind: "five_hour", Source: "codexbar", Freshness: "fresh", Confidence: "exact", Remaining: &rem, CapturedAt: now.Add(-time.Minute), ResetAt: ptrTimeCanary(now.Add(2 * time.Hour))},
			},
			Children: []artifactqual.CanaryChild{
				child("wi_research", "att-r-1", "codex", "gpt-5.5", "low", "succeeded", 0.96, 0.05, after),
				child("wi_implement", "att-i-1", "antigravity", "GPT-OSS", "medium", "succeeded", 0.9, 0.05, after),
				child("wi_tests", "att-t-1", "antigravity", "GPT-OSS", "medium", "succeeded", 0.9, 0.05, after),
				child("wi_verify", "att-v-1", "codex", "gpt-5.5", "high", "succeeded", 0.96, 0.05, after),
			},
			UnavailableRetry: &artifactqual.CanaryUnavailableRetry{
				ExcludedProvider: "grok", ExcludedReason: reason,
				NoDuplicateClaim: true, NoDuplicateFiles: true, NoDoubleCapacity: true,
				EvidenceRef: "events:fake",
			},
			Restart: validRestart(4),
			PR: &artifactqual.CanaryPR{
				URL: "https://github.com/jasonhnd/loopcoder/pull/9999", Number: 9999,
				RequiredChecks: []string{"verify", "test"}, RequiredChecksGreen: true,
				IndependentVerifier: "loopreview", VerifierEvidenceRef: "sha256:verifdeadbeef",
				CreatedByLoopCoder: true,
			},
		}
	}
	for _, reason := range []string{"eligible_not_chosen", "not_chosen", "soft_excluded", "stale"} {
		v := artifactqual.ValidateCanaryEvidence(base(reason), digest, sha, now)
		if v.UnavailableRetryOK {
			t.Fatalf("reason %q must not satisfy unavailable_retry", reason)
		}
		joined := ""
		for _, r := range v.Reasons {
			joined += r + ";"
		}
		if !contains(joined, "unavailable_retry_reason_not_unavailable") && !contains(joined, "unavailable_retry") {
			t.Fatalf("reason %q: want unavailable rejection, got %v", reason, v.Reasons)
		}
	}
}
