package artifactqual_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/artifactqual"
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
	ev.ContentDigest = artifactqual.DigestCanaryBody(ev)
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
	if len(v.Providers) != 1 || v.Providers[0] != "codex" {
		t.Fatalf("providers=%v want only codex", v.Providers)
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
