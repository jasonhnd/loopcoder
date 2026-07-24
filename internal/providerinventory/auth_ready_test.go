package providerinventory

import "testing"

func TestExactFreshReadyAuthGate(t *testing.T) {
	id := "pinst_x"
	base := AuthReadiness{
		AdapterID:              "grok",
		ReadinessState:         ReadinessReady,
		ReadinessConfidence:    ConfidenceExact,
		Confidence:             ConfidenceExact,
		FreshnessState:         FreshnessFresh,
		ProviderInstallationID: &id,
	}
	if !ExactFreshReadyAuth(base) {
		t.Fatal("exact+fresh Ready must pass")
	}
	stale := base
	stale.FreshnessState = FreshnessStale
	if ExactFreshReadyAuth(stale) {
		t.Fatal("stale Ready must fail")
	}
	est := base
	est.Confidence = ConfidenceEstimated
	if ExactFreshReadyAuth(est) {
		t.Fatal("estimated Confidence must fail")
	}
	estRC := base
	estRC.ReadinessConfidence = ConfidenceEstimated
	if ExactFreshReadyAuth(estRC) {
		t.Fatal("estimated ReadinessConfidence must fail")
	}
	unk := base
	unk.Confidence = ConfidenceUnknown
	if ExactFreshReadyAuth(unk) {
		t.Fatal("unknown Confidence must fail")
	}
	notReady := base
	notReady.ReadinessState = ReadinessNotAuthenticated
	if ExactFreshReadyAuth(notReady) {
		t.Fatal("not-ready must fail")
	}
}

func TestPromoteUsableInstallationsRequiresExactFreshReady(t *testing.T) {
	id := "pinst_promote"
	inst := []ProviderInstallation{{
		AdapterID:              "grok",
		ProviderInstallationID: id,
		InstallationState:      InstallationInstalled,
		UsableForInvocation:    "unknown",
		FreshnessState:         FreshnessFresh,
		Confidence:             ConfidenceExact,
	}}

	// Stale Ready must not promote.
	staleAuth := []AuthReadiness{{
		AdapterID: "grok", ReadinessState: ReadinessReady,
		ReadinessConfidence: ConfidenceExact, Confidence: ConfidenceExact,
		FreshnessState: FreshnessStale, ProviderInstallationID: &id,
	}}
	promoteUsableInstallations(inst, staleAuth)
	if inst[0].UsableForInvocation == "yes" {
		t.Fatal("stale Ready must not set usable=yes")
	}

	// Estimated Ready must not promote.
	inst[0].UsableForInvocation = "unknown"
	estAuth := []AuthReadiness{{
		AdapterID: "grok", ReadinessState: ReadinessReady,
		ReadinessConfidence: ConfidenceEstimated, Confidence: ConfidenceEstimated,
		FreshnessState: FreshnessFresh, ProviderInstallationID: &id,
	}}
	promoteUsableInstallations(inst, estAuth)
	if inst[0].UsableForInvocation == "yes" {
		t.Fatal("estimated Ready must not set usable=yes")
	}

	// Unknown confidence Ready must not promote.
	inst[0].UsableForInvocation = "unknown"
	unkAuth := []AuthReadiness{{
		AdapterID: "grok", ReadinessState: ReadinessReady,
		ReadinessConfidence: ConfidenceUnknown, Confidence: ConfidenceUnknown,
		FreshnessState: FreshnessFresh, ProviderInstallationID: &id,
	}}
	promoteUsableInstallations(inst, unkAuth)
	if inst[0].UsableForInvocation == "yes" {
		t.Fatal("unknown Ready must not set usable=yes")
	}

	// Exact+fresh Ready promotes.
	inst[0].UsableForInvocation = "unknown"
	okAuth := []AuthReadiness{{
		AdapterID: "grok", ReadinessState: ReadinessReady,
		ReadinessConfidence: ConfidenceExact, Confidence: ConfidenceExact,
		FreshnessState: FreshnessFresh, ProviderInstallationID: &id,
	}}
	promoteUsableInstallations(inst, okAuth)
	if inst[0].UsableForInvocation != "yes" {
		t.Fatalf("exact+fresh Ready must set usable=yes, got %q", inst[0].UsableForInvocation)
	}
}
