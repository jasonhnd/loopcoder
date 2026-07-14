package quotaheadroom

import (
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/usageledger"
)

func TestEstimateWeeklyPoolConstrainsAbundantFiveHourWindow(t *testing.T) {
	now := fixedNow()
	target := targetDimension()
	records := []usageledger.UsageRecord{
		history("usage_a", target, 1000, now.Add(-4*time.Hour), metaFixture()),
		history("usage_b", target, 1000, now.Add(-3*time.Hour), metaFixture()),
		history("usage_c", target, 1000, now.Add(-2*time.Hour), metaFixture()),
	}
	snapshots := []providerinventory.QuotaSnapshot{
		snapshot("qsnap_short", target, providerinventory.WindowRolling, 10000, now.Add(5*time.Hour), nil, nil),
		snapshot("qsnap_weekly", target, providerinventory.WindowFixedWeek, 2500, now.Add(7*24*time.Hour), nil, nil),
	}

	got := Estimate(Request{Now: now, Target: target, Snapshots: snapshots, UsageRecords: records})

	if got.MostConstrainingWindow == nil || got.MostConstrainingWindow.QuotaSnapshotID != "qsnap_weekly" {
		t.Fatalf("most constraining window = %#v, want weekly pool", got.MostConstrainingWindow)
	}
	if got.TaskEquivalentHeadroom != 2 || !got.Feasible {
		t.Fatalf("headroom=%d feasible=%t, want 2 feasible", got.TaskEquivalentHeadroom, got.Feasible)
	}
	if got.CompletionReserveValue != 125 || got.IndependentVerificationReserveValue != 200 {
		t.Fatalf("default reserves = completion %d verification %d", got.CompletionReserveValue, got.IndependentVerificationReserveValue)
	}
}

func TestEstimateIneligibleWeeklyPoolBeatsAbundantFiveHourWindow(t *testing.T) {
	now := fixedNow()
	target := targetDimension()
	records := []usageledger.UsageRecord{
		history("usage_a", target, 1000, now.Add(-4*time.Hour), metaFixture()),
		history("usage_b", target, 1000, now.Add(-3*time.Hour), metaFixture()),
		history("usage_c", target, 1000, now.Add(-2*time.Hour), metaFixture()),
	}
	snapshots := []providerinventory.QuotaSnapshot{
		snapshot("qsnap_five_hour", target, providerinventory.WindowRolling, 100000, now.Add(5*time.Hour), nil, nil),
		snapshot("qsnap_weekly_model_pool", target, providerinventory.WindowFixedWeek, 0, now.Add(7*24*time.Hour), nil, nil),
	}

	got := Estimate(Request{Now: now, Target: target, Snapshots: snapshots, UsageRecords: records})

	if got.Feasible {
		t.Fatalf("feasible = true, want exhausted weekly/model pool to gate feasibility: %#v", got)
	}
	if got.TerminalErrorCode != "ErrBudgetExhausted" {
		t.Fatalf("terminal error = %q, want ErrBudgetExhausted", got.TerminalErrorCode)
	}
	if got.MostConstrainingWindow == nil || got.MostConstrainingWindow.QuotaSnapshotID != "qsnap_weekly_model_pool" {
		t.Fatalf("most constraining window = %#v, want exhausted weekly/model pool", got.MostConstrainingWindow)
	}
	if !contains(got.MostConstrainingWindow.BlockedReasons, "quota-exhausted") {
		t.Fatalf("blocked reasons = %#v, want quota-exhausted", got.MostConstrainingWindow.BlockedReasons)
	}
}

func TestEstimateSparseNoStaleAndConflictingHistory(t *testing.T) {
	now := fixedNow()
	target := targetDimension()
	snap := snapshot("qsnap_quota", target, providerinventory.WindowRolling, 5000, now.Add(time.Hour), nil, nil)

	noHistory := Estimate(Request{Now: now, Target: target, Snapshots: []providerinventory.QuotaSnapshot{snap}})
	if noHistory.Feasible || noHistory.TerminalErrorCode == "" || !contains(noHistory.GapReasons, "missing-history") {
		t.Fatalf("no history result = %#v, want fail-closed missing-history", noHistory)
	}

	sparse := Estimate(Request{
		Now:          now,
		Target:       target,
		Snapshots:    []providerinventory.QuotaSnapshot{snap},
		UsageRecords: []usageledger.UsageRecord{history("usage_one", target, 900, now.Add(-time.Hour), metaFixture())},
	})
	if sparse.Estimate.P50Value != 900 || sparse.Estimate.P95Value != 900 || !contains(sparse.Estimate.GapReasons, "sparse-history") {
		t.Fatalf("sparse estimate = %#v, want repeated one-sample quantiles and gap", sparse.Estimate)
	}
	if sparse.Feasible || sparse.TerminalErrorCode != "ErrQuotaConfidenceInsufficient" || !contains(sparse.GapReasons, "insufficient-history-samples") {
		t.Fatalf("sparse result = %#v, want fail-closed insufficient history", sparse)
	}

	stale := Estimate(Request{
		Now:       now,
		Target:    target,
		Policy:    Policy{HistoryFreshAfterMS: int64((30 * time.Minute).Milliseconds())},
		Snapshots: []providerinventory.QuotaSnapshot{snap},
		UsageRecords: []usageledger.UsageRecord{
			history("usage_stale_a", target, 1000, now.Add(-3*time.Hour), metaFixture()),
			history("usage_stale_b", target, 1100, now.Add(-2*time.Hour), metaFixture()),
			history("usage_stale_c", target, 1200, now.Add(-time.Hour), metaFixture()),
		},
	})
	if stale.Estimate.Confidence != providerinventory.ConfidenceStale || !contains(stale.Estimate.GapReasons, "stale-history") {
		t.Fatalf("stale estimate = %#v, want stale-history", stale.Estimate)
	}
	if stale.Feasible || stale.TerminalErrorCode != "ErrQuotaConfidenceInsufficient" || !contains(stale.GapReasons, "stale-history") {
		t.Fatalf("stale result = %#v, want fail-closed stale history", stale)
	}

	futureOnly := Estimate(Request{
		Now:       now,
		Target:    target,
		Snapshots: []providerinventory.QuotaSnapshot{snap},
		UsageRecords: []usageledger.UsageRecord{
			history("usage_future", target, 1000, now.Add(time.Hour), metaFixture()),
		},
	})
	if futureOnly.Feasible || futureOnly.TerminalErrorCode != "ErrQuotaConfidenceInsufficient" || !contains(futureOnly.GapReasons, "clock-skew-history-future") {
		t.Fatalf("future-only result = %#v, want fail-closed clock skew", futureOnly)
	}

	conflictingHistory := history("usage_conflict", target, 1000, now.Add(-time.Hour), metaFixture())
	conflictingHistory.GapReasons = []string{"provider-conflict"}
	conflictingHistoryResult := Estimate(Request{
		Now:          now,
		Target:       target,
		Snapshots:    []providerinventory.QuotaSnapshot{snap},
		UsageRecords: []usageledger.UsageRecord{conflictingHistory},
	})
	if conflictingHistoryResult.Feasible || conflictingHistoryResult.TerminalErrorCode != "ErrQuotaConfidenceInsufficient" || !contains(conflictingHistoryResult.GapReasons, "conflicting-history") {
		t.Fatalf("conflicting history result = %#v, want fail-closed conflicting history", conflictingHistoryResult)
	}

	unknownHistory := history("usage_unknown", target, 1000, now.Add(-time.Hour), metaFixture())
	unknownHistory.Confidence = providerinventory.ConfidenceUnknown
	unknownHistoryResult := Estimate(Request{
		Now:          now,
		Target:       target,
		Snapshots:    []providerinventory.QuotaSnapshot{snap},
		UsageRecords: []usageledger.UsageRecord{unknownHistory},
	})
	if unknownHistoryResult.Feasible || unknownHistoryResult.TerminalErrorCode != "ErrQuotaConfidenceInsufficient" || !contains(unknownHistoryResult.GapReasons, "history-confidence-insufficient") {
		t.Fatalf("unknown history confidence result = %#v, want fail-closed source quality", unknownHistoryResult)
	}

	conflicting := snap
	conflicting.ConflictSet = []string{"qsnap_other"}
	conflict := Estimate(Request{
		Now:          now,
		Target:       target,
		Snapshots:    []providerinventory.QuotaSnapshot{conflicting},
		UsageRecords: []usageledger.UsageRecord{history("usage_ok_a", target, 1000, now.Add(-time.Hour), metaFixture())},
	})
	if conflict.Feasible || !contains(conflict.Windows[0].BlockedReasons, "conflicting-quota-snapshots") {
		t.Fatalf("conflict result = %#v, want blocked conflicting quota", conflict)
	}
}

func TestEstimateSparseHistoryRequiresExplicitPolicy(t *testing.T) {
	now := fixedNow()
	target := targetDimension()
	snap := snapshot("qsnap_quota", target, providerinventory.WindowRolling, 5000, now.Add(time.Hour), nil, nil)
	record := history("usage_one", target, 900, now.Add(-time.Hour), metaFixture())

	defaultPolicy := Estimate(Request{
		Now:          now,
		Target:       target,
		Snapshots:    []providerinventory.QuotaSnapshot{snap},
		UsageRecords: []usageledger.UsageRecord{record},
	})
	explicitSparsePolicy := Estimate(Request{
		Now:          now,
		Target:       target,
		Policy:       Policy{MinHistorySamples: 1},
		Snapshots:    []providerinventory.QuotaSnapshot{snap},
		UsageRecords: []usageledger.UsageRecord{record},
	})

	if defaultPolicy.Feasible || !contains(defaultPolicy.GapReasons, "insufficient-history-samples") {
		t.Fatalf("default sparse result = %#v, want conservative fail-closed", defaultPolicy)
	}
	if !explicitSparsePolicy.Feasible || explicitSparsePolicy.TerminalErrorCode != "" {
		t.Fatalf("explicit sparse policy result = %#v, want feasible", explicitSparsePolicy)
	}
	if defaultPolicy.InputFingerprint == explicitSparsePolicy.InputFingerprint {
		t.Fatalf("fingerprint did not include min-history policy: %s", defaultPolicy.InputFingerprint)
	}
}

func TestEstimateQuotaFreshnessAndConfidenceGateFeasibility(t *testing.T) {
	now := fixedNow()
	target := targetDimension()
	records := []usageledger.UsageRecord{
		history("usage_a", target, 1000, now.Add(-4*time.Hour), metaFixture()),
		history("usage_b", target, 1000, now.Add(-3*time.Hour), metaFixture()),
		history("usage_c", target, 1000, now.Add(-2*time.Hour), metaFixture()),
	}
	cases := []struct {
		name       string
		confidence providerinventory.Confidence
		freshness  providerinventory.FreshnessState
		wantReason string
	}{
		{name: "unknown-confidence", confidence: providerinventory.ConfidenceUnknown, freshness: providerinventory.FreshnessFresh, wantReason: "quota-confidence-insufficient"},
		{name: "unavailable-confidence", confidence: providerinventory.ConfidenceUnavailable, freshness: providerinventory.FreshnessFresh, wantReason: "quota-confidence-insufficient"},
		{name: "unknown-freshness", confidence: providerinventory.ConfidenceExact, freshness: "", wantReason: "quota-freshness-insufficient"},
		{name: "not-applicable-freshness", confidence: providerinventory.ConfidenceExact, freshness: providerinventory.FreshnessNotApplicable, wantReason: "quota-freshness-insufficient"},
		{name: "stale-freshness", confidence: providerinventory.ConfidenceExact, freshness: providerinventory.FreshnessStale, wantReason: "stale-quota"},
		{name: "expired-freshness", confidence: providerinventory.ConfidenceExact, freshness: providerinventory.FreshnessExpired, wantReason: "expired-quota"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			snap := snapshot("qsnap_"+tt.name, target, providerinventory.WindowRolling, 5000, now.Add(time.Hour), nil, nil)
			snap.Confidence = tt.confidence
			snap.FreshnessState = tt.freshness

			got := Estimate(Request{Now: now, Target: target, Snapshots: []providerinventory.QuotaSnapshot{snap}, UsageRecords: records})

			if got.Feasible || got.TerminalErrorCode != "ErrQuotaConfidenceInsufficient" {
				t.Fatalf("result = %#v, want fail-closed quota source quality", got)
			}
			if got.MostConstrainingWindow == nil || !contains(got.MostConstrainingWindow.BlockedReasons, tt.wantReason) {
				t.Fatalf("most constraining = %#v, want reason %q", got.MostConstrainingWindow, tt.wantReason)
			}
		})
	}
}

func TestEstimateQuantilesAndCalibrationAreDeterministic(t *testing.T) {
	now := fixedNow()
	target := targetDimension()
	records := []usageledger.UsageRecord{
		estimate("usage_est", target, 80, now.Add(-4*time.Hour), "task-cal", metaFixture()),
		historyForTask("usage_100", target, 100, now.Add(-3*time.Hour), "task-cal", metaFixture()),
		history("usage_200", target, 200, now.Add(-2*time.Hour), metaFixture()),
		history("usage_400", target, 400, now.Add(-time.Hour), metaFixture()),
	}
	got := Estimate(Request{
		Now:          now,
		Target:       target,
		Snapshots:    []providerinventory.QuotaSnapshot{snapshot("qsnap_quota", target, providerinventory.WindowRolling, 5000, now.Add(5*time.Hour), nil, nil)},
		UsageRecords: records,
	})

	if got.Estimate.P50Value != 200 || got.Estimate.P95Value != 400 || got.Estimate.SampleCount != 3 {
		t.Fatalf("estimate = %#v, want p50=200 p95=400 n=3", got.Estimate)
	}
	if got.Estimate.CalibrationErrorBasisPoints == nil || *got.Estimate.CalibrationErrorBasisPoints != 2000 {
		t.Fatalf("calibration = %#v, want 2000 bp", got.Estimate.CalibrationErrorBasisPoints)
	}
	first, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(records)
	replayed := Estimate(Request{
		Now:          now,
		Target:       target,
		Snapshots:    []providerinventory.QuotaSnapshot{snapshot("qsnap_quota", target, providerinventory.WindowRolling, 5000, now.Add(5*time.Hour), nil, nil)},
		UsageRecords: records,
	})
	second, err := json.Marshal(replayed)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("replayed result is not byte-stable\nfirst=%s\nsecond=%s", first, second)
	}
}

func TestEstimateWindowPermutationDoesNotChangeBlockerOrResultBytes(t *testing.T) {
	now := fixedNow()
	target := targetDimension()
	records := []usageledger.UsageRecord{
		history("usage_a", target, 1000, now.Add(-4*time.Hour), metaFixture()),
		history("usage_b", target, 1000, now.Add(-3*time.Hour), metaFixture()),
		history("usage_c", target, 1000, now.Add(-2*time.Hour), metaFixture()),
	}
	abundant := snapshot("qsnap_five_hour", target, providerinventory.WindowRolling, 100000, now.Add(5*time.Hour), nil, nil)
	staleWeekly := snapshot("qsnap_weekly", target, providerinventory.WindowFixedWeek, 100000, now.Add(7*24*time.Hour), nil, nil)
	staleWeekly.FreshnessState = providerinventory.FreshnessStale
	exhaustedModel := snapshot("qsnap_model_pool", target, providerinventory.WindowFixedDay, 0, now.Add(24*time.Hour), nil, nil)
	windows := []providerinventory.QuotaSnapshot{abundant, staleWeekly, exhaustedModel}

	var firstBytes []byte
	permutations := [][]int{
		{0, 1, 2},
		{0, 2, 1},
		{1, 0, 2},
		{1, 2, 0},
		{2, 0, 1},
		{2, 1, 0},
	}
	for _, order := range permutations {
		ordered := []providerinventory.QuotaSnapshot{windows[order[0]], windows[order[1]], windows[order[2]]}
		got := Estimate(Request{Now: now, Target: target, Snapshots: ordered, UsageRecords: records})
		if got.Feasible {
			t.Fatalf("order %v result = %#v, want non-feasible", order, got)
		}
		if got.MostConstrainingWindow == nil || got.MostConstrainingWindow.QuotaSnapshotID != "qsnap_model_pool" {
			t.Fatalf("order %v most constraining = %#v, want exhausted model pool", order, got.MostConstrainingWindow)
		}
		data, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if firstBytes == nil {
			firstBytes = data
			continue
		}
		if string(firstBytes) != string(data) {
			t.Fatalf("order %v changed result bytes\nfirst=%s\nthis=%s", order, firstBytes, data)
		}
	}
}

func TestEstimateResetTimezoneClockSkewAndNestedWindows(t *testing.T) {
	now := time.Date(2026, 3, 8, 9, 30, 0, 0, time.UTC)
	target := targetDimension()
	records := []usageledger.UsageRecord{
		history("usage_a", target, 1000, now.Add(-4*time.Hour), metaFixture()),
		history("usage_b", target, 1000, now.Add(-3*time.Hour), metaFixture()),
		history("usage_c", target, 1000, now.Add(-2*time.Hour), metaFixture()),
	}
	dstReset := "2026-03-08T03:30:00-07:00"
	alreadyReset := "2026-03-08T01:30:00-08:00"
	future := snapshot("qsnap_dst", target, providerinventory.WindowFixedHour, 5000, now.Add(time.Hour), nil, &dstReset)
	past := snapshot("qsnap_past", target, providerinventory.WindowFixedHour, 5000, now.Add(time.Hour), nil, &alreadyReset)
	nested := snapshot("qsnap_nested", target, providerinventory.WindowRolling, 1500, now.Add(2*time.Hour), nil, nil)

	got := Estimate(Request{Now: now, Target: target, Snapshots: []providerinventory.QuotaSnapshot{future, past, nested}, UsageRecords: records})

	var sawPast bool
	for _, window := range got.Windows {
		if window.QuotaSnapshotID == "qsnap_past" {
			sawPast = window.ResetState == "already-reset" && contains(window.BlockedReasons, "already-reset")
		}
	}
	if !sawPast {
		t.Fatalf("windows = %#v, want already-reset state visible", got.Windows)
	}
	if got.MostConstrainingWindow == nil || got.MostConstrainingWindow.QuotaSnapshotID != "qsnap_nested" {
		t.Fatalf("most constraining = %#v, want nested rolling window", got.MostConstrainingWindow)
	}
	if got.ExpiryUrgency != "soon" {
		t.Fatalf("expiry urgency = %q, want soon", got.ExpiryUrgency)
	}
}

func TestEstimateDoesNotCrossContaminateAccountsModelsOrProviders(t *testing.T) {
	now := fixedNow()
	target := targetDimension()
	otherAccount := target
	otherAccount.AccountProfileID = "acct_other"
	otherModel := target
	otherModel.ModelCapabilityID = "mcap_other"
	otherProvider := target
	otherProvider.AdapterID = "other-provider"
	records := []usageledger.UsageRecord{
		history("usage_target_a", target, 1000, now.Add(-3*time.Hour), metaFixture()),
		history("usage_target_b", target, 1000, now.Add(-2*time.Hour), metaFixture()),
		history("usage_target_c", target, 1000, now.Add(-time.Hour), metaFixture()),
		history("usage_other_account", otherAccount, 999999, now.Add(-time.Hour), metaFixture()),
		history("usage_other_model", otherModel, 999999, now.Add(-time.Hour), metaFixture()),
		history("usage_other_provider", otherProvider, 999999, now.Add(-time.Hour), metaFixture()),
	}
	got := Estimate(Request{
		Now:    now,
		Target: target,
		Snapshots: []providerinventory.QuotaSnapshot{
			snapshot("qsnap_target", target, providerinventory.WindowRolling, 5000, now.Add(time.Hour), nil, nil),
			snapshot("qsnap_other_account", otherAccount, providerinventory.WindowRolling, 1, now.Add(time.Hour), nil, nil),
			snapshot("qsnap_other_model", otherModel, providerinventory.WindowRolling, 1, now.Add(time.Hour), nil, nil),
			snapshot("qsnap_other_provider", otherProvider, providerinventory.WindowRolling, 1, now.Add(time.Hour), nil, nil),
		},
		UsageRecords: records,
	})

	if got.Estimate.P95Value != 1000 || len(got.Windows) != 1 || got.Windows[0].QuotaSnapshotID != "qsnap_target" {
		t.Fatalf("cross-contaminated result = %#v", got)
	}
}

func TestEstimateReservesOverflowAndUnknownUnitsFailClosed(t *testing.T) {
	now := fixedNow()
	target := targetDimension()
	records := []usageledger.UsageRecord{
		history("usage_a", target, 1000, now.Add(-3*time.Hour), metaFixture()),
		history("usage_b", target, 1000, now.Add(-2*time.Hour), metaFixture()),
		history("usage_c", target, 1000, now.Add(-time.Hour), metaFixture()),
	}
	invalidPolicy := Estimate(Request{
		Now:          now,
		Target:       target,
		Policy:       Policy{CompletionReserveBasisPoints: 10001},
		Snapshots:    []providerinventory.QuotaSnapshot{snapshot("qsnap_quota", target, providerinventory.WindowRolling, 5000, now.Add(time.Hour), nil, nil)},
		UsageRecords: records,
	})
	if invalidPolicy.Feasible || !contains(invalidPolicy.GapReasons, "invalid-reserve-policy") {
		t.Fatalf("invalid reserve policy = %#v, want fail-closed", invalidPolicy)
	}

	huge := snapshot("qsnap_huge", target, providerinventory.WindowRolling, math.MaxInt64, now.Add(time.Hour), nil, nil)
	overflow := Estimate(Request{Now: now, Target: target, Snapshots: []providerinventory.QuotaSnapshot{huge}, UsageRecords: records})
	if overflow.Feasible || !contains(overflow.Windows[0].BlockedReasons, "arithmetic-overflow") {
		t.Fatalf("overflow = %#v, want arithmetic-overflow", overflow)
	}

	badTarget := target
	badTarget.Unit = ""
	unknown := Estimate(Request{Now: now, Target: badTarget, UsageRecords: records})
	if unknown.TerminalErrorCode != "ErrInvalidRecord" || !contains(unknown.GapReasons, "invalid-target-quantity") {
		t.Fatalf("unknown unit = %#v, want invalid target", unknown)
	}
}

func TestEstimateProviderNeutralityAndSecretShapedCanary(t *testing.T) {
	now := fixedNow()
	target := targetDimension()
	target.AdapterID = "provider-" + strings.ToLower("AKIA"+strings.Repeat("X", 12))
	records := []usageledger.UsageRecord{
		history("usage_neutral_a", target, 700, now.Add(-3*time.Hour), metaFixture()),
		history("usage_neutral_b", target, 900, now.Add(-2*time.Hour), metaFixture()),
		history("usage_neutral_c", target, 1100, now.Add(-time.Hour), metaFixture()),
	}
	got := Estimate(Request{
		Now:          now,
		Target:       target,
		Snapshots:    []providerinventory.QuotaSnapshot{snapshot("qsnap_neutral", target, providerinventory.WindowRolling, 4000, now.Add(time.Hour), nil, nil)},
		UsageRecords: records,
	})
	if got.Estimate.P95Value != 1100 || got.MostConstrainingWindow == nil || got.MostConstrainingWindow.AdapterID != target.AdapterID {
		t.Fatalf("provider-neutral result = %#v", got)
	}
}

func fixedNow() time.Time {
	return time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
}

func targetDimension() Dimension {
	return Dimension{
		ProjectID:         "proj",
		AdapterID:         "provider-a",
		AccountProfileID:  "acct-a",
		ModelCapabilityID: "mcap-a",
		Role:              "worker",
		TaskClass:         "code",
		Effort:            "high",
		ContextBand:       "medium",
		QuantityKind:      providerinventory.QuantityTotalTokens,
		Unit:              "token",
		ValueScale:        0,
	}
}

func metaFixture() meta {
	return meta{Role: "worker", TaskClass: "code", Effort: "high", ContextBand: "medium"}
}

func history(id string, target Dimension, value int64, at time.Time, m meta) usageledger.UsageRecord {
	return historyForTask(id, target, value, at, id+"-task", m)
}

func historyForTask(id string, target Dimension, value int64, at time.Time, taskID string, m meta) usageledger.UsageRecord {
	return usageRecord(id, usageledger.EventProviderReported, target, value, at, taskID, m)
}

func estimate(id string, target Dimension, value int64, at time.Time, taskID string, m meta) usageledger.UsageRecord {
	return usageRecord(id, usageledger.EventEstimate, target, value, at, taskID, m)
}

func usageRecord(id string, event usageledger.EventKind, target Dimension, value int64, at time.Time, taskID string, m meta) usageledger.UsageRecord {
	raw, _ := json.Marshal(m)
	return usageledger.UsageRecord{
		SchemaVersion:        usageledger.UsageRecordSchema,
		UsageRecordID:        id,
		EventKind:            event,
		EventTime:            at.UTC().Format(time.RFC3339Nano),
		ProjectID:            target.ProjectID,
		TaskID:               taskID,
		AdapterID:            target.AdapterID,
		AccountProfileID:     target.AccountProfileID,
		ModelCapabilityID:    target.ModelCapabilityID,
		QuantityKind:         target.QuantityKind,
		Value:                value,
		Unit:                 target.Unit,
		ValueScale:           target.ValueScale,
		OriginalQuantityJSON: raw,
		Confidence:           providerinventory.ConfidenceExact,
		SourceRecordIDs:      []string{"source_" + id},
		IdempotencyKey:       "idem_" + id,
		GapReasons:           []string{},
	}
}

func snapshot(id string, target Dimension, window providerinventory.WindowKind, remaining int64, reset time.Time, reserved *int64, resetOverride *string) providerinventory.QuotaSnapshot {
	account := target.AccountProfileID
	model := target.ModelCapabilityID
	resetAt := reset.UTC().Format(time.RFC3339Nano)
	if resetOverride != nil {
		resetAt = *resetOverride
	}
	start := reset.Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	return providerinventory.QuotaSnapshot{
		SchemaVersion:     providerinventory.QuotaSnapshotSchema,
		RecordVersion:     1,
		QuotaSnapshotID:   id,
		QuotaSourceID:     "qsrc_test",
		SourceKind:        providerinventory.QuotaSourceFixture,
		AdapterID:         target.AdapterID,
		AccountProfileID:  &account,
		ModelCapabilityID: &model,
		ScopeKey:          "scope/" + id,
		QuantityKind:      target.QuantityKind,
		Unit:              target.Unit,
		WindowKind:        window,
		WindowStart:       start,
		WindowEnd:         resetAt,
		ResetAt:           resetAt,
		ResetSemantics:    providerinventory.ResetWindowBoundary,
		RemainingValue:    &remaining,
		ReservedValue:     reserved,
		ValueScale:        target.ValueScale,
		Confidence:        providerinventory.ConfidenceExact,
		FreshnessState:    providerinventory.FreshnessFresh,
		CapturedAt:        fixedNow().UTC().Format(time.RFC3339Nano),
		ConflictSet:       []string{},
		GapReasons:        []string{},
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
