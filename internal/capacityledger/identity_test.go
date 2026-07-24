package capacityledger_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
)

// Two accounts on one provider with different windows/remaining — reserve must
// bind the exact requested account+window+model+depth and never cross.
func TestReserve_TwoAccountsSameProvider_ExactIdentity(t *testing.T) {
	now := t0()
	path := filepath.Join(t.TempDir(), "cap.json")
	l, err := capacityledger.OpenPath(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	reset := now.Add(time.Hour)
	// Account A: five_hour remaining 90%; Account B: weekly remaining 20%.
	accA := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "codex", AccountRef: "acct-codex-primary", InstallRef: "i-test",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Windows: []capacitysnapshot.Window{{
			Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
			Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 10, Unit: capacitysnapshot.UnitPercentage},
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 90, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
			ResetAt: &reset, CapturedAt: now, Source: "test",
		}},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "gpt-5.5", Present: true, SupportedDepths: []string{"medium"}, DefaultDepth: "medium",
		}},
		Source: "test", CapturedAt: now,
	})
	accB := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "codex", AccountRef: "acct-codex-secondary", InstallRef: "i-test-2",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Windows: []capacitysnapshot.Window{{
			Kind: "weekly", Unit: capacitysnapshot.UnitPercentage,
			Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 80, Unit: capacitysnapshot.UnitPercentage},
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 20, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
			ResetAt: &reset, CapturedAt: now, Source: "test",
		}},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "gpt-5.5", Present: true, SupportedDepths: []string{"medium"}, DefaultDepth: "medium",
		}},
		Source: "test", CapturedAt: now,
	})
	snap, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{accA, accB}, now)
	if err != nil {
		t.Fatal(err)
	}
	// Select secondary account + weekly window + its exact install.
	e, err := l.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "r", AttemptID: "att-sec",
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Provider: "codex", Model: "gpt-5.5", Depth: "medium",
		AccountRef: "acct-codex-secondary", WindowKind: "weekly",
		InstallRef: "i-test-2",
		Snapshot:   &snap, DemandFraction: 0.05,
	})
	if err != nil || e.State != "reserved" {
		t.Fatalf("reserve secondary: %+v %v", e, err)
	}
	// Account is opaque canonical; verify via re-filter reserve of same attempt identity.
	if e.AccountRef == "" || !strings.HasPrefix(e.AccountRef, "acct-") {
		t.Fatalf("account=%q want opaque acct-", e.AccountRef)
	}
	// Distinct from primary: reserve primary under different attempt and compare.
	ePriProbe, err := l.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "r", AttemptID: "att-pri-probe",
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Provider: "codex", Model: "gpt-5.5", Depth: "medium",
		AccountRef: "acct-codex-primary", WindowKind: "five_hour",
		InstallRef: "i-test",
		Snapshot:   &snap, DemandFraction: 0.01,
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.AccountRef == ePriProbe.AccountRef {
		t.Fatalf("secondary and primary accounts must be distinct canonical refs")
	}
	_ = ePriProbe
	if e.WindowKind != "weekly" && !strings.Contains(strings.ToLower(e.WindowKind), "week") {
		t.Fatalf("window=%q want weekly", e.WindowKind)
	}
	if e.Model != "gpt-5.5" || e.Depth != "medium" || e.Provider != "codex" {
		t.Fatalf("model/depth/provider: %+v", e)
	}
	// before should reflect secondary remaining (~0.20), not primary 0.90.
	if e.Before > 0.5 {
		t.Fatalf("before=%.3f — likely selected primary window residual, want ~0.20", e.Before)
	}
	// Explicit primary five_hour.
	e2, err := l.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "r", AttemptID: "att-pri",
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Provider: "codex", Model: "gpt-5.5", Depth: "medium",
		AccountRef: "acct-codex-primary", WindowKind: "five_hour",
		InstallRef: "i-test",
		Snapshot:   &snap, DemandFraction: 0.05,
	})
	if err != nil {
		t.Fatal(err)
	}
	if e2.AccountRef == e.AccountRef {
		t.Fatalf("primary and secondary must differ: %q", e2.AccountRef)
	}
	if e2.Before < 0.5 {
		t.Fatalf("primary before=%.3f want high remaining", e2.Before)
	}
	// Cross mismatch: request secondary account with five_hour (only weekly) → refuse.
	if _, err := l.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "r", AttemptID: "att-cross",
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Provider: "codex", Model: "gpt-5.5",
		AccountRef: "acct-codex-secondary", WindowKind: "five_hour",
		InstallRef: "i-test-2",
		Snapshot:   &snap,
	}); err == nil {
		t.Fatal("want refuse when account has no matching window")
	}
	// Cross-install: same secondary account on primary install must refuse.
	if _, err := l.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "r", AttemptID: "att-cross-install",
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Provider: "codex", Model: "gpt-5.5", Depth: "medium",
		AccountRef: "acct-codex-secondary", WindowKind: "weekly",
		InstallRef: "i-test",
		Snapshot:   &snap,
	}); err == nil {
		t.Fatal("want refuse when install does not match account observation")
	}
}

func TestReserveReplay_IdentityMismatchConflicts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cap.json")
	l, err := capacityledger.OpenPath(path, t0)
	if err != nil {
		t.Fatal(err)
	}
	snap := testSnap()
	// Include full identity so replay requires them.
	e1, err := l.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "r", AttemptID: "att-replay",
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Provider: "codex", Model: "gpt-5.5", Depth: "medium",
		AccountRef: "acct-codex", WindowKind: "five_hour",
		InstallRef: "i-test",
		Snapshot:   &snap,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Same identity → idempotent reserved.
	e2, err := l.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "r", AttemptID: "att-replay",
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Provider: "codex", Model: "gpt-5.5", Depth: "medium",
		AccountRef: "acct-codex", WindowKind: "five_hour",
		InstallRef: "i-test",
		Snapshot:   &snap,
	})
	if err != nil || e2.ReservationID != e1.ReservationID {
		t.Fatalf("idempotent: %+v %v", e2, err)
	}
	// Mismatch each dimension + missing account/window/depth → ErrConflict.
	mismatches := []capacityledger.ReserveInput{
		{ProjectID: "p", RunID: "r", AttemptID: "att-replay", PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Provider: "claude", Model: "gpt-5.5", Depth: "medium", AccountRef: "acct-codex", WindowKind: "five_hour", InstallRef: "i-test", Snapshot: &snap},
		{ProjectID: "p", RunID: "r", AttemptID: "att-replay", Provider: "codex", Model: "other-model", Depth: "medium", AccountRef: "acct-codex", WindowKind: "five_hour", InstallRef: "i-test", Snapshot: &snap},
		{ProjectID: "p", RunID: "r", AttemptID: "att-replay", Provider: "codex", Model: "gpt-5.5", Depth: "high", AccountRef: "acct-codex", WindowKind: "five_hour", InstallRef: "i-test", Snapshot: &snap},
		{ProjectID: "p", RunID: "r", AttemptID: "att-replay", Provider: "codex", Model: "gpt-5.5", Depth: "medium", AccountRef: "acct-other", WindowKind: "five_hour", InstallRef: "i-test", Snapshot: &snap},
		{ProjectID: "p", RunID: "r", AttemptID: "att-replay", Provider: "codex", Model: "gpt-5.5", Depth: "medium", AccountRef: "acct-codex", WindowKind: "weekly", InstallRef: "i-test", Snapshot: &snap},
		// Missing fields when prev has them
		{ProjectID: "p", RunID: "r", AttemptID: "att-replay", Provider: "codex", Model: "gpt-5.5", Depth: "medium", WindowKind: "five_hour", InstallRef: "i-test", Snapshot: &snap},
		{ProjectID: "p", RunID: "r", AttemptID: "att-replay", Provider: "codex", Model: "gpt-5.5", Depth: "medium", AccountRef: "acct-codex", InstallRef: "i-test", Snapshot: &snap},
		{ProjectID: "p", RunID: "r", AttemptID: "att-replay", Provider: "codex", Model: "gpt-5.5", AccountRef: "acct-codex", WindowKind: "five_hour", InstallRef: "i-test", Snapshot: &snap},
		// Cross-install same account must conflict
		{ProjectID: "p", RunID: "r", AttemptID: "att-replay", Provider: "codex", Model: "gpt-5.5", Depth: "medium", AccountRef: "acct-codex", WindowKind: "five_hour", InstallRef: "i-other", Snapshot: &snap},
	}
	for i, in := range mismatches {
		if _, err := l.Reserve(in); err == nil {
			t.Fatalf("mismatch %d want error", i)
		}
		// Empty account/window fail as invalid; wrong identity as conflict.
	}
}

func TestCanonicalAccountRef_OpaqueNoLeak(t *testing.T) {
	a := capacityledger.CanonicalAccountRef("user@example.com")
	b := capacityledger.CanonicalAccountRef("other@example.com")
	if a == b {
		t.Fatal("distinct emails must hash distinctly")
	}
	if strings.Contains(a, "@") || strings.Contains(a, "example") {
		t.Fatalf("leak: %q", a)
	}
	if !strings.HasPrefix(a, "acct-") {
		t.Fatalf("want acct- prefix: %q", a)
	}
	// Full SHA-256: acct- + 64 hex (never truncated 16-hex for new hashes).
	if len(a) != 5+64 {
		t.Fatalf("want full sha256 hex len 69 got %d %q", len(a), a)
	}
	// Stable.
	if capacityledger.CanonicalAccountRef("user@example.com") != a {
		t.Fatal("unstable hash")
	}
	// Legacy short opaque is marked insufficient — not exact-routable.
	legacy := "acct-0123456789abcdef"
	got := capacityledger.CanonicalAccountRef(legacy)
	if !strings.HasPrefix(got, capacityledger.AccountRefLegacyInsufficient) {
		t.Fatalf("legacy short must mark insufficient: %q", got)
	}
	if capacityledger.ExactRoutableAccount(got) {
		t.Fatal("legacy must not ExactRoutableAccount")
	}
	if capacityledger.ExactRoutableAccount("") {
		t.Fatal("empty must not ExactRoutableAccount")
	}
	if capacityledger.CanonicalAccountRef("") != "" {
		t.Fatalf("empty must stay empty unknown, got %q", capacityledger.CanonicalAccountRef(""))
	}
}

func TestMapWindowKind_PreservesDailyAndUnknown(t *testing.T) {
	// Reserve with daily window must round-trip exact kind, not five_hour.
	path := filepath.Join(t.TempDir(), "cap.json")
	now := t0()
	l, err := capacityledger.OpenPath(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	reset := now.Add(time.Hour)
	acc := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "codex", AccountRef: "acct-daily", InstallRef: "i-test",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Windows: []capacitysnapshot.Window{{
			Kind: "daily", Unit: capacitysnapshot.UnitPercentage,
			Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 10, Unit: capacitysnapshot.UnitPercentage},
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 90, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
			ResetAt: &reset, CapturedAt: now, Source: "test",
		}},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "gpt-5.5", Present: true, SupportedDepths: []string{"medium"}, DefaultDepth: "medium",
		}},
		Source: "test", CapturedAt: now,
	})
	snap, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{acc}, now)
	if err != nil {
		t.Fatal(err)
	}
	e, err := l.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "r", AttemptID: "att-daily",
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Provider: "codex", Model: "gpt-5.5", Depth: "medium",
		AccountRef: "acct-daily", WindowKind: "daily",
		InstallRef: "i-test",
		Snapshot:   &snap, DemandFraction: 0.05,
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.WindowKind != "daily" && !strings.EqualFold(e.WindowKind, "daily") {
		t.Fatalf("window=%q want daily (not five_hour)", e.WindowKind)
	}
	// Replay exact daily.
	e2, err := l.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "r", AttemptID: "att-daily",
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Provider: "codex", Model: "gpt-5.5", Depth: "medium",
		AccountRef: "acct-daily", WindowKind: "daily",
		InstallRef: "i-test",
		Snapshot:   &snap,
	})
	if err != nil || e2.ReservationID != e.ReservationID {
		t.Fatalf("replay daily: %+v %v", e2, err)
	}
	// five_hour must conflict.
	if _, err := l.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "r", AttemptID: "att-daily",
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Provider: "codex", Model: "gpt-5.5", Depth: "medium",
		AccountRef: "acct-daily", WindowKind: "five_hour",
		InstallRef: "i-test",
		Snapshot:   &snap,
	}); err == nil {
		t.Fatal("daily != five_hour must conflict on replay")
	}
}

func TestReopen_ActiveReservationPressure_NoOversubscribe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cap.json")
	now := t0()
	l, err := capacityledger.OpenPath(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	// Snapshot with only ~0.10 remaining so two 0.06 holds should conflict after reopen.
	reset := now.Add(time.Hour)
	acc := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "codex", AccountRef: "acct-tight", InstallRef: "i-test",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Windows: []capacitysnapshot.Window{{
			Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
			Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 90, Unit: capacitysnapshot.UnitPercentage},
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 10, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
			ResetAt: &reset, CapturedAt: now, Source: "test",
		}},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "gpt-5.5", Present: true, SupportedDepths: []string{"medium"}, DefaultDepth: "medium",
		}},
		Source: "test", CapturedAt: now,
	})
	snap, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{acc}, now)
	if err != nil {
		t.Fatal(err)
	}
	eA, err := l.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "r", AttemptID: "att-A",
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Provider: "codex", Model: "gpt-5.5", Depth: "medium",
		AccountRef: "acct-tight", WindowKind: "five_hour",
		InstallRef: "i-test",
		Snapshot:   &snap, DemandFraction: 0.06,
	})
	if err != nil || eA.State != "reserved" {
		t.Fatalf("A: %+v %v", eA, err)
	}
	// Reopen process — A must still consume pressure.
	l2, err := capacityledger.OpenPath(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	_, err = l2.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "r", AttemptID: "att-B",
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Provider: "codex", Model: "gpt-5.5", Depth: "medium",
		AccountRef: "acct-tight", WindowKind: "five_hour",
		InstallRef: "i-test",
		Snapshot:   &snap, DemandFraction: 0.06,
	})
	if err == nil {
		t.Fatal("B must see A pressure and refuse/oversubscribe fail")
	}
}

func TestOpenPath_CorruptAndDuplicateFailClosed(t *testing.T) {
	dir := t.TempDir()
	// Corrupt JSON
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := capacityledger.OpenPath(bad, t0); err == nil {
		t.Fatal("corrupt JSON must fail closed")
	}
	// Duplicate entry keys
	dup := filepath.Join(dir, "dup.json")
	body := `{
  "schema": "loopcoder.capacity_ledger.file.v1",
  "entries": [
    {"schema":"loopcoder.capacity_ledger.entry.v1","idempotency_key":"p|r|a1","project_id":"p","run_id":"r","attempt_id":"a1","state":"released","provider":"codex","model":"m","account_ref":"acct-aaaaaaaaaaaaaaaa","window_kind":"five_hour","reservation_id":"res1","reserved":0.1},
    {"schema":"loopcoder.capacity_ledger.entry.v1","idempotency_key":"p|r|a1","project_id":"p","run_id":"r","attempt_id":"a1","state":"released","provider":"codex","model":"m","account_ref":"acct-aaaaaaaaaaaaaaaa","window_kind":"five_hour","reservation_id":"res2","reserved":0.1}
  ]
}`
	if err := os.WriteFile(dup, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := capacityledger.OpenPath(dup, t0); err == nil {
		t.Fatal("duplicate key must fail closed")
	}
	// Invalid active identity (missing SoftExpiresAt)
	inv := filepath.Join(dir, "inv.json")
	invBody := `{
  "schema": "loopcoder.capacity_ledger.file.v1",
  "entries": [
    {"schema":"loopcoder.capacity_ledger.entry.v1","idempotency_key":"p|r|a1","project_id":"p","run_id":"r","attempt_id":"a1","state":"reserved","provider":"codex","model":"m","depth":"medium","account_ref":"acct-aaaaaaaaaaaaaaaa","window_kind":"five_hour","reservation_id":"res1","reserved":0.1}
  ]
}`
	if err := os.WriteFile(inv, []byte(invBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := capacityledger.OpenPath(inv, t0); err == nil {
		t.Fatal("reserved without expiry must fail closed")
	}
}

func TestReopen_AfterExpiryDoesNotBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cap.json")
	now := t0()
	l, err := capacityledger.OpenPath(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	// Enough remaining for one hold while active and another after soft-expiry release.
	reset := now.Add(30 * time.Minute)
	acc := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "codex", AccountRef: "acct-exp", InstallRef: "i-test",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Windows: []capacitysnapshot.Window{{
			Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
			Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 40, Unit: capacitysnapshot.UnitPercentage},
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 60, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
			ResetAt: &reset, CapturedAt: now, Source: "test",
		}},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "gpt-5.5", Present: true, SupportedDepths: []string{"medium"}, DefaultDepth: "medium",
		}},
		Source: "test", CapturedAt: now,
	})
	snap, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{acc}, now)
	if err != nil {
		t.Fatal(err)
	}
	eA, err := l.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "r", AttemptID: "att-A",
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Provider: "codex", Model: "gpt-5.5", Depth: "medium",
		AccountRef: "acct-exp", WindowKind: "five_hour",
		InstallRef: "i-test",
		Snapshot:   &snap, DemandFraction: 0.40,
	})
	if err != nil || eA.State != "reserved" {
		t.Fatalf("A: %+v %v", eA, err)
	}
	if eA.SoftExpiresAt == nil {
		t.Fatal("SoftExpiresAt must be persisted")
	}
	// While still within SoftExpiresAt: second large hold should fail closed (pressure).
	lMid, err := capacityledger.OpenPath(path, func() time.Time { return now.Add(time.Minute) })
	if err != nil {
		t.Fatal(err)
	}
	_, err = lMid.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "r", AttemptID: "att-B-mid",
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Provider: "codex", Model: "gpt-5.5", Depth: "medium",
		AccountRef: "acct-exp", WindowKind: "five_hour",
		InstallRef: "i-test",
		Snapshot:   &snap, DemandFraction: 0.40,
	})
	if err == nil {
		t.Fatal("while active, second large hold must refuse")
	}
	// After original SoftExpiresAt: released on load; second hold must succeed.
	past := eA.SoftExpiresAt.Add(time.Minute)
	lPast, err := capacityledger.OpenPath(path, func() time.Time { return past })
	if err != nil {
		t.Fatal(err)
	}
	eB, err := lPast.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "r", AttemptID: "att-B-past",
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Provider: "codex", Model: "gpt-5.5", Depth: "medium",
		AccountRef: "acct-exp", WindowKind: "five_hour",
		InstallRef: "i-test",
		Snapshot:   &snap, DemandFraction: 0.40,
	})
	if err != nil || eB.State != "reserved" {
		t.Fatalf("after original expiry, hold must not block: %+v %v", eB, err)
	}
}
