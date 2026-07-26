package artifactqual_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/artifactqual"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

// validPR is defined in canary_evidence_test.go (same package).

func baseValidCanary(now time.Time) (artifactqual.CanaryEvidence, string, string) {
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sha := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	rem := 0.9
	after := 0.85
	ev := artifactqual.CanaryEvidence{
		Schema: artifactqual.SchemaCanaryEvidence, ArchiveDigest: digest, PreProdSHA: sha,
		BinaryVersion: "0.9.0-rc.9", BinaryCommit: sha, ProjectID: "disp-cap-1", RunID: "run_cap_1",
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
			ExcludedProvider: "claude", ExcludedReason: "unavailable",
			NoDuplicateClaim: true, NoDuplicateFiles: true, NoDoubleCapacity: true,
			EvidenceRef: "events:hard_exclude:claude",
		},
		Restart: validRestart(4),
		PR: &artifactqual.CanaryPR{
			URL: "https://github.com/jasonhnd/loopcoder/pull/1", Number: 1,
			RequiredChecks: []string{"verify"}, RequiredChecksGreen: true,
			IndependentVerifier: "loopreview", VerifierEvidenceRef: "sha256:v",
			CreatedByLoopCoder: true,
		},
	}
	completeRawCanaryEvidence(&ev, now)
	return ev, digest, sha
}

func TestValidateCanary_StaleCapturedAtLabelledFreshRejected(t *testing.T) {
	now := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	ev, dig, sha := baseValidCanary(now)
	// Stale captured_at labelled fresh.
	ev.ProviderObservations[0].CapturedAt = now.Add(-48 * time.Hour)
	ev.ContentDigest = artifactqual.DigestCanaryBody(ev)
	v := artifactqual.ValidateCanaryEvidence(ev, dig, sha, now)
	if v.CapacityAfterOK && v.Valid {
		// may still set capacity after from children; provider obs must fail
	}
	joined := strings.Join(v.Reasons, ";")
	if !strings.Contains(joined, "provider_obs_captured_at_outside_canary_run") &&
		!strings.Contains(joined, "provider_obs_stale_labelled_fresh") {
		t.Fatalf("want stale/outside-run rejection, got %v", v.Reasons)
	}
	if v.Valid {
		t.Fatal("must be invalid")
	}
}

func TestValidateCanary_MissingAfterMetadataRejected(t *testing.T) {
	now := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	ev, dig, sha := baseValidCanary(now)
	ev.Children[0].AfterSource = ""
	ev.Children[0].AfterFreshness = ""
	ev.Children[0].AfterState = "observed"
	ev.ContentDigest = artifactqual.DigestCanaryBody(ev)
	v := artifactqual.ValidateCanaryEvidence(ev, dig, sha, now)
	joined := strings.Join(v.Reasons, ";")
	if !strings.Contains(joined, "after_source_missing") {
		t.Fatalf("want after_source_missing, got %v", v.Reasons)
	}
	if v.Valid {
		t.Fatal("must be invalid")
	}
}

func TestValidateCanary_RawLedgerBindsExactPriorResumeSnapshot(t *testing.T) {
	now := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	ev, dig, sha := baseValidCanary(now)
	if len(ev.RawLedgerEntries) == 0 {
		t.Fatal("missing raw ledger fixture")
	}
	entry := &ev.RawLedgerEntries[0]
	const priorDigest = "sha256:prior-resume-inventory"
	oldDigest := entry.BeforeInventoryDigest
	entry.BeforeInventoryDigest = priorDigest
	bound := false
	for i := range ev.ProviderObservations {
		observation := &ev.ProviderObservations[i]
		if observation.Provider == entry.Provider &&
			observation.AccountRef == entry.AccountRef &&
			observation.InstallRef == entry.InstallRef &&
			observation.WindowKind == entry.WindowKind &&
			observation.InventoryReportDigest == oldDigest &&
			observation.Source == entry.BeforeSource &&
			observation.CapturedAt.Equal(*entry.BeforeCapturedAt) {
			observation.InventoryReportDigest = priorDigest
			bound = true
			break
		}
	}
	if !bound {
		t.Fatal("failed to locate exact prior observation")
	}
	ev.ContentDigest = artifactqual.DigestCanaryBody(ev)
	v := artifactqual.ValidateCanaryEvidence(ev, dig, sha, now)
	if strings.Contains(strings.Join(v.Reasons, ";"), "raw_ledger_before_inventory_digest_mismatch") {
		t.Fatalf("exact prior snapshot must remain valid across resume: %v", v.Reasons)
	}

	for i := range ev.ProviderObservations {
		if ev.ProviderObservations[i].InventoryReportDigest == priorDigest {
			ev.ProviderObservations[i].InventoryReportDigest = "sha256:tampered"
			break
		}
	}
	ev.ContentDigest = artifactqual.DigestCanaryBody(ev)
	v = artifactqual.ValidateCanaryEvidence(ev, dig, sha, now)
	if !strings.Contains(strings.Join(v.Reasons, ";"), "raw_ledger_before_inventory_digest_mismatch") {
		t.Fatalf("unbound prior snapshot must fail closed: %v", v.Reasons)
	}
}

func TestValidateCanary_DerivedAfterOnlyRejected(t *testing.T) {
	now := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	ev, dig, sha := baseValidCanary(now)
	for i := range ev.Children {
		ev.Children[i].AfterState = "derived"
		ev.Children[i].AfterSource = "before_minus_actual"
		ev.Children[i].AfterFreshness = "estimated"
		ev.Children[i].AfterObservedAt = time.Time{}
	}
	ev.ContentDigest = artifactqual.DigestCanaryBody(ev)
	v := artifactqual.ValidateCanaryEvidence(ev, dig, sha, now)
	if v.CapacityAfterOK {
		t.Fatal("derived after must not satisfy CapacityAfterOK")
	}
	joined := strings.Join(v.Reasons, ";")
	if !strings.Contains(joined, "capacity_after_not_observed") {
		t.Fatalf("want not_observed, got %v", v.Reasons)
	}
}

func TestValidateCanary_DefaultProseAbsenceRejected(t *testing.T) {
	now := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	ev, dig, sha := baseValidCanary(now)
	// Simulate old fabrication: capacity_snapshot + fresh with zero ObservedAt.
	for i := range ev.Children {
		ev.Children[i].AfterState = "observed"
		ev.Children[i].AfterSource = "capacity_snapshot"
		ev.Children[i].AfterFreshness = "fresh"
		ev.Children[i].AfterObservedAt = time.Time{}
	}
	ev.ContentDigest = artifactqual.DigestCanaryBody(ev)
	v := artifactqual.ValidateCanaryEvidence(ev, dig, sha, now)
	joined := strings.Join(v.Reasons, ";")
	if !strings.Contains(joined, "after_forbidden_source") && !strings.Contains(joined, "after_observed_at_missing") {
		t.Fatalf("want forbidden/default rejection, got %v", v.Reasons)
	}
}

func TestValidateCanary_PositiveExactFreshObservedAfter(t *testing.T) {
	now := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	ev, dig, sha := baseValidCanary(now)
	// Full PR + live + wi_verify binding for RealPROK.
	ev.PR = validPR(sha)
	for i := range ev.Children {
		if ev.Children[i].WorkItemID == "wi_verify" {
			ev.PR.VerifierAttemptID = ev.Children[i].AttemptID
			ev.PR.VerifierProvider = ev.Children[i].Provider
			ev.PR.IndependentVerifier = ev.Children[i].Provider
			ev.PR.VerifierEvidenceRef = ev.Children[i].OutputEvidence + "@head:" + sha
		}
	}
	ev.ContentDigest = artifactqual.DigestCanaryBody(ev)
	live := &artifactqual.PRLiveState{
		Repository: "jasonhnd/loopcoder", Number: 9999, URL: ev.PR.URL,
		BaseRef: "main", HeadOID: sha, State: "open",
		RequiredChecksGreen: true, HumanMergeGate: true,
		ChecksAtHead: []artifactqual.PRCheck{
			{Name: "verify", Status: "completed", Conclusion: "success"},
			{Name: "test", Status: "completed", Conclusion: "success"},
		},
	}
	v := artifactqual.ValidateCanaryEvidence(ev, dig, sha, now, artifactqual.CanaryValidateOpts{
		ExpectedPRHeadOID: sha, LivePR: live,
	})
	if !v.Valid {
		t.Fatalf("want valid exact fresh after: %v", v.Reasons)
	}
	if !v.CapacityAfterOK {
		t.Fatal("want CapacityAfterOK")
	}
}

func TestValidateCanary_ContentDigestMismatchRejected(t *testing.T) {
	now := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	ev, dig, sha := baseValidCanary(now)
	ev.ContentDigest = "deadbeef"
	v := artifactqual.ValidateCanaryEvidence(ev, dig, sha, now)
	joined := strings.Join(v.Reasons, ";")
	if !strings.Contains(joined, "content_digest_mismatch") {
		t.Fatalf("want content_digest_mismatch, got %v", v.Reasons)
	}
}

func TestValidateCanary_MissingContentDigestRejected(t *testing.T) {
	now := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	ev, dig, sha := baseValidCanary(now)
	ev.ContentDigest = ""
	v := artifactqual.ValidateCanaryEvidence(ev, dig, sha, now)
	joined := strings.Join(v.Reasons, ";")
	if !strings.Contains(joined, "content_digest_missing") {
		t.Fatalf("want content_digest_missing, got %v", v.Reasons)
	}
}

func TestValidateCanary_RawCapacityArithmeticMutationRejected(t *testing.T) {
	now := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	ev, dig, sha := baseValidCanary(now)
	if len(ev.RawLedgerEntries) == 0 || ev.RawLedgerEntries[0].Actual == nil {
		t.Fatal("positive fixture must carry raw actual")
	}
	mutated := *ev.RawLedgerEntries[0].Actual + 0.01
	ev.RawLedgerEntries[0].Actual = &mutated
	ev.DurableEvidenceDigest = artifactqual.DigestDurableEvidence(ev)
	ev.ContentDigest = artifactqual.DigestCanaryBody(ev)
	v := artifactqual.ValidateCanaryEvidence(ev, dig, sha, now)
	if v.CapacityAfterOK {
		t.Fatal("one-field raw actual mutation must fail capacity arithmetic")
	}
	if !strings.Contains(strings.Join(v.Reasons, ";"), "raw_ledger") {
		t.Fatalf("want raw ledger rejection: %v", v.Reasons)
	}
}

func TestValidateCanary_UnavailableSummaryCannotReplaceRawEvidence(t *testing.T) {
	now := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	ev, dig, sha := baseValidCanary(now)
	ev.RawEvents = append([]workflowrun.Event(nil), ev.RawEvents...)
	for i, event := range ev.RawEvents {
		if event.Kind == "model_unavailable" {
			ev.RawEvents = append(ev.RawEvents[:i], ev.RawEvents[i+1:]...)
			break
		}
	}
	// Leave all three hand-written summary booleans true.
	ev.DurableEvidenceDigest = artifactqual.DigestDurableEvidence(ev)
	ev.ContentDigest = artifactqual.DigestCanaryBody(ev)
	v := artifactqual.ValidateCanaryEvidence(ev, dig, sha, now)
	if v.UnavailableRetryOK {
		t.Fatal("summary booleans must not green missing raw model_unavailable")
	}
}

func TestValidateCanary_ForcedInterruptClassIsByteExact(t *testing.T) {
	now := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	ev, dig, sha := baseValidCanary(now)
	for i := range ev.RawEvents {
		if ev.RawEvents[i].Kind == "interrupt" {
			ev.RawEvents[i].Payload = []byte(`{"attempt_id":"att-wi_restart-g0","failure_class":"forced_interrupt","generation":"1","interrupt_class":" service_forced_interrupt","interrupt_id":"iint-exact","terminal":"cancelled","work_item_id":"wi_restart"}`)
			break
		}
	}
	ev.DurableEvidenceDigest = artifactqual.DigestDurableEvidence(ev)
	ev.ContentDigest = artifactqual.DigestCanaryBody(ev)
	v := artifactqual.ValidateCanaryEvidence(ev, dig, sha, now)
	if v.RestartOK {
		t.Fatal("padded interrupt_class must not qualify")
	}
}

func TestValidateCanary_ProviderAliasesCountCanonicalCompanies(t *testing.T) {
	now := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	ev, dig, sha := baseValidCanary(now)
	for i := range ev.Children {
		if ev.Children[i].Provider == "antigravity" {
			ev.Children[i].Provider = "openai"
		}
	}
	for i := range ev.ProviderObservations {
		if ev.ProviderObservations[i].Provider == "antigravity" {
			ev.ProviderObservations[i].Provider = "openai"
		}
	}
	for i := range ev.RawLedgerEntries {
		if ev.RawLedgerEntries[i].Provider == "antigravity" {
			ev.RawLedgerEntries[i].Provider = "openai"
		}
	}
	ev.DurableEvidenceDigest = artifactqual.DigestDurableEvidence(ev)
	ev.ContentDigest = artifactqual.DigestCanaryBody(ev)
	v := artifactqual.ValidateCanaryEvidence(ev, dig, sha, now)
	if v.MultiProviderOK {
		t.Fatalf("codex+openai aliases are one company: %v", v.Providers)
	}
	if !strings.Contains(strings.Join(v.Reasons, ";"), "executed_provider_companies_lt_2") {
		t.Fatalf("want canonical company rejection: %v", v.Reasons)
	}
}
