// Package quotaheadroom estimates task-equivalent quota headroom from local
// immutable quota snapshots and LoopCoder usage history.
//
// The estimator is provider-neutral and deterministic. Quantiles use the
// nearest-rank rule over sorted integer samples: rank=ceil(q*n), clamped to
// [1,n]. With one sample, P50 and P95 are the same observed value and the
// result is marked sparse; sparse history is not feasible unless the caller
// explicitly lowers the minimum-history policy. With no samples, consumption
// stays unknown. The estimator never upgrades local usage history to provider
// truth.
package quotaheadroom

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/usageledger"
)

const (
	SchemaVersion = "loopcoder.quota_headroom.v1"

	DefaultCompletionReserveBasisPoints   = int64(500)
	DefaultVerificationReserveBasisPoints = int64(800)

	defaultMaxHistoryRecords = 5000
	defaultMaxSnapshots      = 500
	defaultMaxGroups         = 256
	defaultMaxDiagnostics    = 128
	defaultMinHistorySamples = 3
	maxHistoryFreshAfterMS   = int64(math.MaxInt64 / int64(time.Millisecond))
)

type Dimension struct {
	ProjectID         string                         `json:"project_id,omitempty"`
	AdapterID         string                         `json:"adapter_id,omitempty"`
	AccountProfileID  string                         `json:"account_profile_id,omitempty"`
	ModelCapabilityID string                         `json:"model_capability_id,omitempty"`
	Role              string                         `json:"role,omitempty"`
	TaskClass         string                         `json:"task_class,omitempty"`
	Effort            string                         `json:"effort,omitempty"`
	ContextBand       string                         `json:"context_band,omitempty"`
	QuantityKind      providerinventory.QuantityKind `json:"quantity_kind"`
	Unit              string                         `json:"unit"`
	ValueScale        int                            `json:"value_scale"`
}

type Policy struct {
	CompletionReserveBasisPoints   int64 `json:"completion_reserve_basis_points"`
	VerificationReserveBasisPoints int64 `json:"verification_reserve_basis_points"`
	HistoryFreshAfterMS            int64 `json:"history_fresh_after_ms,omitempty"`
	MaxHistoryRecords              int   `json:"max_history_records,omitempty"`
	MaxSnapshots                   int   `json:"max_snapshots,omitempty"`
	MaxGroups                      int   `json:"max_groups,omitempty"`
	MaxDiagnostics                 int   `json:"max_diagnostics,omitempty"`
	MinHistorySamples              int   `json:"min_history_samples,omitempty"`
	AllowPaidOverage               bool  `json:"allow_paid_overage,omitempty"`
}

type Request struct {
	Now          time.Time                         `json:"-"`
	Target       Dimension                         `json:"target"`
	Policy       Policy                            `json:"policy"`
	Snapshots    []providerinventory.QuotaSnapshot `json:"quota_snapshots"`
	UsageRecords []usageledger.UsageRecord         `json:"usage_records"`
}

type ConsumptionEstimate struct {
	P50Value                    int64                            `json:"p50_value"`
	P95Value                    int64                            `json:"p95_value"`
	SampleCount                 int                              `json:"sample_count"`
	Confidence                  providerinventory.Confidence     `json:"confidence"`
	FreshnessState              providerinventory.FreshnessState `json:"freshness_state"`
	FreshestSampleAt            string                           `json:"freshest_sample_at,omitempty"`
	CalibrationErrorBasisPoints *int64                           `json:"calibration_error_basis_points,omitempty"`
	UsageRecordIDs              []string                         `json:"usage_record_ids"`
	GapReasons                  []string                         `json:"gap_reasons"`
}

type WindowEvaluation struct {
	QuotaSnapshotID                   string                           `json:"quota_snapshot_id"`
	AdapterID                         string                           `json:"adapter_id,omitempty"`
	AccountProfileID                  string                           `json:"account_profile_id,omitempty"`
	ModelCapabilityID                 string                           `json:"model_capability_id,omitempty"`
	ScopeKey                          string                           `json:"scope_key"`
	QuantityKind                      providerinventory.QuantityKind   `json:"quantity_kind"`
	Unit                              string                           `json:"unit"`
	ValueScale                        int                              `json:"value_scale"`
	WindowKind                        providerinventory.WindowKind     `json:"window_kind"`
	WindowStart                       string                           `json:"window_start,omitempty"`
	WindowEnd                         string                           `json:"window_end,omitempty"`
	ResetAt                           string                           `json:"reset_at,omitempty"`
	ResetState                        string                           `json:"reset_state"`
	Confidence                        providerinventory.Confidence     `json:"confidence"`
	FreshnessState                    providerinventory.FreshnessState `json:"freshness_state"`
	LimitValue                        *int64                           `json:"limit_value,omitempty"`
	UsedValue                         *int64                           `json:"used_value,omitempty"`
	RemainingValue                    *int64                           `json:"remaining_value,omitempty"`
	ReservedValue                     *int64                           `json:"reserved_value,omitempty"`
	AvailableValue                    int64                            `json:"available_value"`
	UsableAfterReserves               int64                            `json:"usable_after_reserves"`
	TaskEquivalentHeadroom            int64                            `json:"task_equivalent_headroom"`
	ProjectedUnusedCapacityAtReset    int64                            `json:"projected_unused_capacity_at_reset"`
	ExpiryUrgency                     string                           `json:"expiry_urgency"`
	RequiredBurnMultiplierBasisPoints int64                            `json:"required_burn_multiplier_basis_points"`
	CompletionReserveValue            int64                            `json:"completion_reserve_value"`
	VerificationReserveValue          int64                            `json:"verification_reserve_value"`
	Eligible                          bool                             `json:"eligible"`
	BlockedReasons                    []string                         `json:"blocked_reasons"`
	GapReasons                        []string                         `json:"gap_reasons"`
}

type Result struct {
	SchemaVersion                       string              `json:"schema_version"`
	GeneratedAt                         string              `json:"generated_at"`
	InputFingerprint                    string              `json:"input_fingerprint"`
	Target                              Dimension           `json:"target"`
	Estimate                            ConsumptionEstimate `json:"estimate"`
	Windows                             []WindowEvaluation  `json:"windows"`
	MostConstrainingWindow              *WindowEvaluation   `json:"most_constraining_window,omitempty"`
	Feasible                            bool                `json:"feasible"`
	TaskEquivalentHeadroom              int64               `json:"task_equivalent_headroom"`
	ProjectedUnusedCapacityAtReset      int64               `json:"projected_unused_capacity_at_reset"`
	ExpiryUrgency                       string              `json:"expiry_urgency"`
	RequiredBurnMultiplierBasisPoints   int64               `json:"required_burn_multiplier_basis_points"`
	CompletionReserveValue              int64               `json:"completion_reserve_value"`
	IndependentVerificationReserveValue int64               `json:"independent_verification_reserve_value"`
	GapReasons                          []string            `json:"gap_reasons"`
	TerminalErrorCode                   string              `json:"terminal_error_code,omitempty"`
}

func Estimate(req Request) Result {
	now := req.Now.UTC()
	if now.IsZero() {
		now = time.Unix(0, 0).UTC()
	}
	policy, policyGaps := normalizePolicy(req.Policy)
	target := normalizeDimension(req.Target)
	result := Result{
		SchemaVersion:     SchemaVersion,
		GeneratedAt:       formatTime(now),
		Target:            target,
		ExpiryUrgency:     "unknown",
		GapReasons:        append([]string{}, policyGaps...),
		TerminalErrorCode: "",
	}
	result.InputFingerprint = fingerprint(req, now, policy, target)
	if target.QuantityKind == "" || strings.TrimSpace(target.Unit) == "" || target.ValueScale < 0 {
		return fail(result, "ErrInvalidRecord", "invalid-target-quantity")
	}
	if len(policyGaps) > 0 {
		return fail(result, "ErrHeadroomBoundExceeded", "")
	}
	if len(req.UsageRecords) > policy.MaxHistoryRecords {
		return fail(result, "ErrHeadroomBoundExceeded", "history-bound-exceeded")
	}
	if len(req.Snapshots) > policy.MaxSnapshots {
		return fail(result, "ErrHeadroomBoundExceeded", "snapshot-bound-exceeded")
	}

	estimate := estimateConsumption(req.UsageRecords, target, policy, now)
	result.Estimate = estimate
	result.GapReasons = append(result.GapReasons, estimate.GapReasons...)

	windows, windowGaps := evaluateWindows(req.Snapshots, target, policy, estimate, now)
	result.Windows = windows
	result.GapReasons = append(result.GapReasons, windowGaps...)
	best := mostConstraining(windows)
	if best != nil {
		selected := *best
		result.MostConstrainingWindow = &selected
		result.TaskEquivalentHeadroom = selected.TaskEquivalentHeadroom
		result.ProjectedUnusedCapacityAtReset = selected.ProjectedUnusedCapacityAtReset
		result.ExpiryUrgency = selected.ExpiryUrgency
		result.RequiredBurnMultiplierBasisPoints = selected.RequiredBurnMultiplierBasisPoints
		result.CompletionReserveValue = selected.CompletionReserveValue
		result.IndependentVerificationReserveValue = selected.VerificationReserveValue
		result.Feasible = selected.Eligible && estimateSufficient(estimate, policy) && selected.TaskEquivalentHeadroom > 0
	}
	if len(windows) == 0 {
		result.GapReasons = append(result.GapReasons, "missing-quota")
	}
	if !result.Feasible {
		result.TerminalErrorCode = terminalCode(result.MostConstrainingWindow, estimate, policy, len(windows) == 0)
	}
	result.GapReasons = limitStrings(dedupeStrings(result.GapReasons), policy.MaxDiagnostics)
	return result
}

func estimateConsumption(records []usageledger.UsageRecord, target Dimension, policy Policy, now time.Time) ConsumptionEstimate {
	type sample struct {
		value int64
		at    time.Time
		id    string
	}
	var samples []sample
	calibration := map[string]int64{}
	var calibrationErrors []int64
	var gaps []string
	groupSeen := map[string]bool{}
	for _, record := range sortedUsage(records) {
		if !recordMatchesTarget(record, target) {
			continue
		}
		meta := usageMeta(record)
		if !dimensionMetaMatches(meta, target) {
			gaps = appendMissingDimensionGaps(gaps, meta, target)
			continue
		}
		groupKey := strings.Join([]string{record.AdapterID, record.AccountProfileID, record.ModelCapabilityID, meta.Role, meta.TaskClass, meta.Effort, meta.ContextBand}, "\x00")
		groupSeen[groupKey] = true
		if len(groupSeen) > policy.MaxGroups {
			return ConsumptionEstimate{
				Confidence:     providerinventory.ConfidenceUnknown,
				FreshnessState: providerinventory.FreshnessNotApplicable,
				GapReasons:     []string{"grouping-cardinality-bound-exceeded"},
			}
		}
		eventTime, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(record.EventTime))
		if err != nil {
			gaps = append(gaps, "malformed-history-timestamp")
			continue
		}
		if eventTime.UTC().After(now) {
			gaps = append(gaps, "clock-skew-history-future")
			continue
		}
		if historyConflict(record.GapReasons) {
			gaps = append(gaps, "conflicting-history")
			continue
		}
		if record.Confidence != providerinventory.ConfidenceExact && record.Confidence != providerinventory.ConfidenceEstimated {
			gaps = append(gaps, "history-confidence-insufficient")
			continue
		}
		if record.Value <= 0 {
			gaps = append(gaps, "non-positive-history-sample")
			continue
		}
		if record.EventKind == usageledger.EventEstimate {
			calibration[record.TaskID] = record.Value
			continue
		}
		if estimate, ok := calibration[record.TaskID]; ok && record.Value > 0 {
			calibrationErrors = append(calibrationErrors, absBP(record.Value-estimate, record.Value))
		}
		samples = append(samples, sample{value: record.Value, at: eventTime.UTC(), id: record.UsageRecordID})
	}
	out := ConsumptionEstimate{
		Confidence:     providerinventory.ConfidenceUnknown,
		FreshnessState: providerinventory.FreshnessNotApplicable,
		UsageRecordIDs: []string{},
		GapReasons:     dedupeStrings(gaps),
	}
	if len(samples) == 0 {
		out.GapReasons = dedupeStrings(append(out.GapReasons, "missing-history"))
		return out
	}
	values := make([]int64, 0, len(samples))
	var freshest time.Time
	for _, sample := range samples {
		values = append(values, sample.value)
		out.UsageRecordIDs = append(out.UsageRecordIDs, sample.id)
		if sample.at.After(freshest) {
			freshest = sample.at
		}
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	sort.Strings(out.UsageRecordIDs)
	out.P50Value = nearestRank(values, 50)
	out.P95Value = nearestRank(values, 95)
	out.SampleCount = len(values)
	out.Confidence = providerinventory.ConfidenceEstimated
	out.FreshnessState = providerinventory.FreshnessFresh
	out.FreshestSampleAt = formatTime(freshest)
	out.GapReasons = append(out.GapReasons, "loopcoder-local-ledger-not-provider-global")
	if len(values) < 3 {
		out.GapReasons = append(out.GapReasons, "sparse-history")
	}
	if len(values) < policy.MinHistorySamples {
		out.Confidence = providerinventory.ConfidenceUnknown
		out.GapReasons = append(out.GapReasons, "insufficient-history-samples")
	}
	if containsString(out.GapReasons, "conflicting-history") || containsString(out.GapReasons, "history-confidence-insufficient") {
		out.Confidence = providerinventory.ConfidenceUnknown
	}
	if policy.HistoryFreshAfterMS > 0 && freshest.Add(time.Duration(policy.HistoryFreshAfterMS)*time.Millisecond).Before(now) {
		out.FreshnessState = providerinventory.FreshnessStale
		out.Confidence = providerinventory.ConfidenceStale
		out.GapReasons = append(out.GapReasons, "stale-history")
	}
	if len(calibrationErrors) > 0 {
		sort.Slice(calibrationErrors, func(i, j int) bool { return calibrationErrors[i] < calibrationErrors[j] })
		value := nearestRank(calibrationErrors, 50)
		out.CalibrationErrorBasisPoints = &value
	} else {
		out.GapReasons = append(out.GapReasons, "calibration-unavailable")
	}
	out.GapReasons = limitStrings(dedupeStrings(out.GapReasons), policy.MaxDiagnostics)
	return out
}

func evaluateWindows(snapshots []providerinventory.QuotaSnapshot, target Dimension, policy Policy, estimate ConsumptionEstimate, now time.Time) ([]WindowEvaluation, []string) {
	var out []WindowEvaluation
	var gaps []string
	for _, snapshot := range sortedSnapshots(snapshots) {
		if !snapshotMatchesTarget(snapshot, target) {
			continue
		}
		evaluation := evaluateWindow(snapshot, policy, estimate, now)
		out = append(out, evaluation)
		gaps = append(gaps, evaluation.GapReasons...)
	}
	sort.Slice(out, func(i, j int) bool {
		left := windowSortKey(out[i])
		right := windowSortKey(out[j])
		return left < right
	})
	return out, dedupeStrings(gaps)
}

func evaluateWindow(snapshot providerinventory.QuotaSnapshot, policy Policy, estimate ConsumptionEstimate, now time.Time) WindowEvaluation {
	e := WindowEvaluation{
		QuotaSnapshotID:   strings.TrimSpace(snapshot.QuotaSnapshotID),
		AdapterID:         strings.TrimSpace(snapshot.AdapterID),
		AccountProfileID:  ptrValue(snapshot.AccountProfileID),
		ModelCapabilityID: ptrValue(snapshot.ModelCapabilityID),
		ScopeKey:          strings.TrimSpace(snapshot.ScopeKey),
		QuantityKind:      snapshot.QuantityKind,
		Unit:              strings.TrimSpace(snapshot.Unit),
		ValueScale:        snapshot.ValueScale,
		WindowKind:        snapshot.WindowKind,
		WindowStart:       strings.TrimSpace(snapshot.WindowStart),
		WindowEnd:         strings.TrimSpace(snapshot.WindowEnd),
		ResetAt:           strings.TrimSpace(snapshot.ResetAt),
		ResetState:        "unknown",
		Confidence:        snapshot.Confidence,
		FreshnessState:    snapshot.FreshnessState,
		LimitValue:        cloneInt64(snapshot.LimitValue),
		UsedValue:         cloneInt64(snapshot.UsedValue),
		RemainingValue:    cloneInt64(snapshot.RemainingValue),
		ReservedValue:     cloneInt64(snapshot.ReservedValue),
		ExpiryUrgency:     "unknown",
		Eligible:          true,
		BlockedReasons:    []string{},
		GapReasons:        append([]string{}, snapshot.GapReasons...),
	}
	obsolete := false
	gap := func(reason string) {
		e.GapReasons = append(e.GapReasons, reason)
	}
	block := func(reason string) {
		if obsolete && reason != "already-reset" {
			gap(reason)
			return
		}
		e.Eligible = false
		e.BlockedReasons = append(e.BlockedReasons, reason)
		gap(reason)
	}
	if resetAt, ok := parseOptionalTime(snapshot.ResetAt); ok {
		if !resetAt.After(now) {
			obsolete = true
			e.ResetState = "already-reset"
			block("already-reset")
		} else {
			e.ResetState = "future"
			e.ExpiryUrgency = expiryUrgency(resetAt.Sub(now))
		}
	} else if strings.TrimSpace(snapshot.ResetAt) != "" {
		e.ResetState = "malformed"
		block("malformed-reset-at")
	}
	if len(snapshot.ConflictSet) > 0 {
		block("conflicting-quota-snapshots")
	}
	if snapshot.Confidence != providerinventory.ConfidenceExact && snapshot.Confidence != providerinventory.ConfidenceEstimated {
		block("quota-confidence-insufficient")
	}
	switch snapshot.FreshnessState {
	case providerinventory.FreshnessFresh:
	case providerinventory.FreshnessStale:
		block("stale-quota")
	case providerinventory.FreshnessExpired:
		block("expired-quota")
	default:
		block("quota-freshness-insufficient")
	}
	if validUntil, ok := parseOptionalTime(snapshot.ValidUntil); ok && !validUntil.After(now) {
		block("expired-quota")
	}
	if staleAfter, ok := parseOptionalTime(snapshot.StaleAfter); ok && !staleAfter.After(now) {
		block("stale-quota")
	}
	if containsString(snapshot.GapReasons, "paid-overage") && !policy.AllowPaidOverage {
		block("paid-overage-disabled")
	}
	if snapshot.WindowKind == providerinventory.WindowUnknown || snapshot.ResetSemantics == providerinventory.ResetUnknown {
		block("unknown-reset")
	}
	remaining, hasAvailable := available(snapshot)
	if !hasAvailable {
		if !obsolete {
			block("missing-quota")
		}
	} else {
		e.AvailableValue = remaining
	}
	if !obsolete && hasAvailable && e.AvailableValue <= 0 {
		block("quota-exhausted")
	}
	if !obsolete && estimate.P95Value <= 0 {
		block("missing-history")
	}
	if !obsolete && !estimateSufficient(estimate, policy) {
		block("history-confidence-insufficient")
	}
	if e.AvailableValue > 0 && estimate.P95Value > 0 {
		completionReserve, ok := percentCeil(e.AvailableValue, policy.CompletionReserveBasisPoints)
		if !ok {
			block("arithmetic-overflow")
		}
		verificationReserve, ok := percentCeil(e.AvailableValue, policy.VerificationReserveBasisPoints)
		if !ok {
			block("arithmetic-overflow")
		}
		e.CompletionReserveValue = completionReserve
		e.VerificationReserveValue = verificationReserve
		usable, ok := checkedSub(checkedSubMust(e.AvailableValue, completionReserve), verificationReserve)
		if !ok || usable < 0 {
			usable = 0
		}
		e.UsableAfterReserves = usable
		e.TaskEquivalentHeadroom = usable / estimate.P95Value
		usedByTasks, ok := checkedMul(e.TaskEquivalentHeadroom, estimate.P95Value)
		if !ok {
			block("arithmetic-overflow")
		}
		consumed, ok := checkedAdd(usedByTasks, completionReserve)
		if ok {
			consumed, ok = checkedAdd(consumed, verificationReserve)
		}
		if !ok {
			block("arithmetic-overflow")
		} else if unused, ok := checkedSub(e.AvailableValue, consumed); ok && unused >= 0 {
			e.ProjectedUnusedCapacityAtReset = unused
		}
		e.RequiredBurnMultiplierBasisPoints = multiplierBP(e.AvailableValue, estimate.P95Value)
	}
	if !obsolete && e.TaskEquivalentHeadroom <= 0 {
		block("insufficient-task-equivalent-headroom")
	}
	e.BlockedReasons = limitStrings(dedupeStrings(e.BlockedReasons), policy.MaxDiagnostics)
	e.GapReasons = limitStrings(dedupeStrings(e.GapReasons), policy.MaxDiagnostics)
	return e
}

func available(snapshot providerinventory.QuotaSnapshot) (int64, bool) {
	var value int64
	var ok bool
	if snapshot.RemainingValue != nil {
		value = *snapshot.RemainingValue
		ok = true
	} else if snapshot.LimitValue != nil && snapshot.UsedValue != nil {
		var subOK bool
		value, subOK = checkedSub(*snapshot.LimitValue, *snapshot.UsedValue)
		if !subOK {
			return 0, false
		}
		ok = true
	}
	if ok && snapshot.ReservedValue != nil {
		var subOK bool
		value, subOK = checkedSub(value, *snapshot.ReservedValue)
		if !subOK {
			return 0, false
		}
	}
	return value, ok
}

func mostConstraining(windows []WindowEvaluation) *WindowEvaluation {
	var best *WindowEvaluation
	for i := range windows {
		if best == nil || windowMoreConstraining(windows[i], *best) {
			best = &windows[i]
		}
	}
	return best
}

func windowMoreConstraining(candidate, best WindowEvaluation) bool {
	candidateClass := constraintClass(candidate)
	bestClass := constraintClass(best)
	if candidateClass != bestClass {
		return candidateClass < bestClass
	}
	if !candidate.Eligible {
		candidateRank := blockerRank(candidate)
		bestRank := blockerRank(best)
		if candidateRank != bestRank {
			return candidateRank < bestRank
		}
	}
	if candidate.TaskEquivalentHeadroom != best.TaskEquivalentHeadroom {
		return candidate.TaskEquivalentHeadroom < best.TaskEquivalentHeadroom
	}
	if candidate.AvailableValue != best.AvailableValue {
		return candidate.AvailableValue < best.AvailableValue
	}
	return windowSortKey(candidate) < windowSortKey(best)
}

func constraintClass(window WindowEvaluation) int {
	if !window.Eligible && gatesFeasibility(window) {
		return 0
	}
	if window.Eligible {
		return 1
	}
	return 2
}

func gatesFeasibility(window WindowEvaluation) bool {
	if len(window.BlockedReasons) == 0 {
		return !window.Eligible
	}
	for _, reason := range window.BlockedReasons {
		if reason != "already-reset" {
			return true
		}
	}
	return false
}

func terminalCode(window *WindowEvaluation, estimate ConsumptionEstimate, policy Policy, missingQuota bool) string {
	if window != nil && !window.Eligible {
		return windowTerminalCode(*window)
	}
	if !estimateSufficient(estimate, policy) || missingQuota {
		return "ErrQuotaConfidenceInsufficient"
	}
	return "ErrBudgetExhausted"
}

func windowTerminalCode(window WindowEvaluation) string {
	reason := strongestBlockerReason(window)
	switch reason {
	case "conflicting-quota-snapshots", "quota-confidence-insufficient", "stale-quota", "expired-quota", "quota-freshness-insufficient", "missing-quota", "malformed-reset-at", "unknown-reset", "already-reset", "missing-history", "history-confidence-insufficient":
		return "ErrQuotaConfidenceInsufficient"
	case "arithmetic-overflow":
		return "ErrHeadroomBoundExceeded"
	}
	return "ErrBudgetExhausted"
}

func strongestBlockerReason(window WindowEvaluation) string {
	bestReason := ""
	bestRank := 1000
	for _, reason := range window.BlockedReasons {
		rank := blockerReasonRank(reason)
		if rank < bestRank || (rank == bestRank && reason < bestReason) {
			bestRank = rank
			bestReason = reason
		}
	}
	return bestReason
}

func blockerRank(window WindowEvaluation) int {
	rank := 1000
	for _, reason := range window.BlockedReasons {
		if value := blockerReasonRank(reason); value < rank {
			rank = value
		}
	}
	return rank
}

func blockerReasonRank(reason string) int {
	switch reason {
	case "arithmetic-overflow":
		return 10
	case "conflicting-quota-snapshots":
		return 20
	case "quota-exhausted":
		return 30
	case "missing-quota":
		return 40
	case "quota-confidence-insufficient", "quota-freshness-insufficient", "expired-quota", "stale-quota":
		return 50
	case "already-reset", "malformed-reset-at", "unknown-reset":
		return 60
	case "paid-overage-disabled":
		return 70
	case "history-confidence-insufficient", "missing-history":
		return 80
	case "insufficient-task-equivalent-headroom":
		return 90
	default:
		return 900
	}
}

func normalizePolicy(policy Policy) (Policy, []string) {
	var gaps []string
	if policy.CompletionReserveBasisPoints == 0 {
		policy.CompletionReserveBasisPoints = DefaultCompletionReserveBasisPoints
	}
	if policy.VerificationReserveBasisPoints == 0 {
		policy.VerificationReserveBasisPoints = DefaultVerificationReserveBasisPoints
	}
	if policy.MaxHistoryRecords == 0 {
		policy.MaxHistoryRecords = defaultMaxHistoryRecords
	}
	if policy.MaxSnapshots == 0 {
		policy.MaxSnapshots = defaultMaxSnapshots
	}
	if policy.MaxGroups == 0 {
		policy.MaxGroups = defaultMaxGroups
	}
	if policy.MaxDiagnostics == 0 {
		policy.MaxDiagnostics = defaultMaxDiagnostics
	}
	if policy.MinHistorySamples == 0 {
		policy.MinHistorySamples = defaultMinHistorySamples
	}
	if policy.CompletionReserveBasisPoints < 0 || policy.CompletionReserveBasisPoints > 10000 ||
		policy.VerificationReserveBasisPoints < 0 || policy.VerificationReserveBasisPoints > 10000 {
		gaps = append(gaps, "invalid-reserve-policy")
	}
	if policy.MaxHistoryRecords < 0 || policy.MaxSnapshots < 0 || policy.MaxGroups < 0 || policy.MaxDiagnostics < 0 ||
		policy.HistoryFreshAfterMS < 0 || policy.HistoryFreshAfterMS > maxHistoryFreshAfterMS ||
		policy.MinHistorySamples < 1 || policy.MinHistorySamples > policy.MaxHistoryRecords {
		gaps = append(gaps, "invalid-bound-policy")
	}
	return policy, gaps
}

func normalizeDimension(d Dimension) Dimension {
	d.ProjectID = strings.TrimSpace(d.ProjectID)
	d.AdapterID = strings.TrimSpace(d.AdapterID)
	d.AccountProfileID = strings.TrimSpace(d.AccountProfileID)
	d.ModelCapabilityID = strings.TrimSpace(d.ModelCapabilityID)
	d.Role = strings.TrimSpace(d.Role)
	d.TaskClass = strings.TrimSpace(d.TaskClass)
	d.Effort = strings.TrimSpace(d.Effort)
	d.ContextBand = strings.TrimSpace(d.ContextBand)
	d.Unit = strings.TrimSpace(d.Unit)
	return d
}

type meta struct {
	Role        string `json:"role"`
	TaskClass   string `json:"task_class"`
	Effort      string `json:"effort"`
	ContextBand string `json:"context_band"`
}

func usageMeta(record usageledger.UsageRecord) meta {
	var out meta
	if len(record.OriginalQuantityJSON) > 0 {
		_ = json.Unmarshal(record.OriginalQuantityJSON, &out)
	}
	return meta{
		Role:        strings.TrimSpace(out.Role),
		TaskClass:   strings.TrimSpace(out.TaskClass),
		Effort:      strings.TrimSpace(out.Effort),
		ContextBand: strings.TrimSpace(out.ContextBand),
	}
}

func recordMatchesTarget(record usageledger.UsageRecord, target Dimension) bool {
	return strings.TrimSpace(record.ProjectID) == target.ProjectID &&
		strings.TrimSpace(record.AdapterID) == target.AdapterID &&
		strings.TrimSpace(record.AccountProfileID) == target.AccountProfileID &&
		strings.TrimSpace(record.ModelCapabilityID) == target.ModelCapabilityID &&
		record.QuantityKind == target.QuantityKind &&
		strings.TrimSpace(record.Unit) == target.Unit &&
		record.ValueScale == target.ValueScale
}

func dimensionMetaMatches(m meta, target Dimension) bool {
	return m.Role == target.Role && m.TaskClass == target.TaskClass && m.Effort == target.Effort && m.ContextBand == target.ContextBand
}

func appendMissingDimensionGaps(gaps []string, m meta, target Dimension) []string {
	if target.Role != "" && m.Role == "" {
		gaps = append(gaps, "missing-role-history")
	}
	if target.TaskClass != "" && m.TaskClass == "" {
		gaps = append(gaps, "missing-task-class-history")
	}
	if target.Effort != "" && m.Effort == "" {
		gaps = append(gaps, "missing-effort-history")
	}
	if target.ContextBand != "" && m.ContextBand == "" {
		gaps = append(gaps, "missing-context-band-history")
	}
	return gaps
}

func snapshotMatchesTarget(snapshot providerinventory.QuotaSnapshot, target Dimension) bool {
	if strings.TrimSpace(snapshot.AdapterID) != target.AdapterID {
		return false
	}
	if snapshot.QuantityKind != target.QuantityKind || strings.TrimSpace(snapshot.Unit) != target.Unit || snapshot.ValueScale != target.ValueScale {
		return false
	}
	if snapshot.AccountProfileID != nil && strings.TrimSpace(*snapshot.AccountProfileID) != target.AccountProfileID {
		return false
	}
	if snapshot.ModelCapabilityID != nil && strings.TrimSpace(*snapshot.ModelCapabilityID) != target.ModelCapabilityID {
		return false
	}
	return true
}

func nearestRank(sortedValues []int64, quantile int) int64 {
	if len(sortedValues) == 0 {
		return 0
	}
	rank := (quantile*len(sortedValues) + 99) / 100
	if rank < 1 {
		rank = 1
	}
	if rank > len(sortedValues) {
		rank = len(sortedValues)
	}
	return sortedValues[rank-1]
}

func sortedUsage(records []usageledger.UsageRecord) []usageledger.UsageRecord {
	out := append([]usageledger.UsageRecord(nil), records...)
	sort.Slice(out, func(i, j int) bool {
		left := strings.Join([]string{out[i].EventTime, out[i].UsageRecordID}, "\x00")
		right := strings.Join([]string{out[j].EventTime, out[j].UsageRecordID}, "\x00")
		return left < right
	})
	return out
}

func sortedSnapshots(snapshots []providerinventory.QuotaSnapshot) []providerinventory.QuotaSnapshot {
	out := append([]providerinventory.QuotaSnapshot(nil), snapshots...)
	sort.Slice(out, func(i, j int) bool {
		return snapshotSortKey(out[i]) < snapshotSortKey(out[j])
	})
	return out
}

func snapshotSortKey(s providerinventory.QuotaSnapshot) string {
	return strings.Join([]string{
		strings.TrimSpace(s.AdapterID),
		ptrValue(s.AccountProfileID),
		ptrValue(s.ModelCapabilityID),
		string(s.QuantityKind),
		strings.TrimSpace(s.Unit),
		strconv.Itoa(s.ValueScale),
		string(s.WindowKind),
		strings.TrimSpace(s.WindowStart),
		strings.TrimSpace(s.WindowEnd),
		strings.TrimSpace(s.ResetAt),
		strings.TrimSpace(s.QuotaSnapshotID),
	}, "\x00")
}

func windowSortKey(w WindowEvaluation) string {
	return strings.Join([]string{
		w.AdapterID,
		w.AccountProfileID,
		w.ModelCapabilityID,
		string(w.QuantityKind),
		w.Unit,
		strconv.Itoa(w.ValueScale),
		string(w.WindowKind),
		w.WindowStart,
		w.WindowEnd,
		w.ResetAt,
		w.QuotaSnapshotID,
	}, "\x00")
}

func fingerprint(req Request, now time.Time, policy Policy, target Dimension) string {
	payload := struct {
		GeneratedAt  string                            `json:"generated_at"`
		Target       Dimension                         `json:"target"`
		Policy       Policy                            `json:"policy"`
		Snapshots    []providerinventory.QuotaSnapshot `json:"quota_snapshots"`
		UsageRecords []usageledger.UsageRecord         `json:"usage_records"`
	}{
		GeneratedAt:  formatTime(now),
		Target:       target,
		Policy:       policy,
		Snapshots:    sortedSnapshots(req.Snapshots),
		UsageRecords: sortedUsage(req.UsageRecords),
	}
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func fail(result Result, code, gap string) Result {
	result.Feasible = false
	result.TerminalErrorCode = code
	result.GapReasons = dedupeStrings(append(result.GapReasons, gap))
	return result
}

func parseOptionalTime(value string) (time.Time, bool) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func ptrValue(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func estimateSufficient(estimate ConsumptionEstimate, policy Policy) bool {
	if policy.MinHistorySamples < 1 {
		return false
	}
	if estimate.P95Value <= 0 || estimate.SampleCount < policy.MinHistorySamples {
		return false
	}
	if estimate.FreshnessState != providerinventory.FreshnessFresh {
		return false
	}
	if estimate.Confidence != providerinventory.ConfidenceExact && estimate.Confidence != providerinventory.ConfidenceEstimated {
		return false
	}
	if containsString(estimate.GapReasons, "conflicting-history") || containsString(estimate.GapReasons, "history-confidence-insufficient") {
		return false
	}
	return true
}

func historyConflict(gaps []string) bool {
	for _, gap := range gaps {
		normalized := strings.ToLower(strings.TrimSpace(gap))
		if strings.Contains(normalized, "conflict") {
			return true
		}
	}
	return false
}

func dedupeStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func limitStrings(values []string, limit int) []string {
	if limit > 0 && len(values) > limit {
		return values[:limit]
	}
	return values
}

func checkedAdd(a, b int64) (int64, bool) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, false
	}
	return a + b, true
}

func checkedSub(a, b int64) (int64, bool) {
	if b == math.MinInt64 {
		return 0, false
	}
	return checkedAdd(a, -b)
}

func checkedSubMust(a, b int64) int64 {
	out, ok := checkedSub(a, b)
	if !ok {
		return 0
	}
	return out
}

func checkedMul(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if a > math.MaxInt64/b {
		return 0, false
	}
	return a * b, true
}

func percentCeil(value, basisPoints int64) (int64, bool) {
	if value < 0 || basisPoints < 0 || basisPoints > 10000 {
		return 0, false
	}
	product, ok := checkedMul(value, basisPoints)
	if !ok {
		return 0, false
	}
	out, ok := checkedAdd(product, 9999)
	if !ok {
		return 0, false
	}
	return out / 10000, true
}

func multiplierBP(available, p95 int64) int64 {
	if p95 <= 0 || available <= 0 {
		return 0
	}
	product, ok := checkedMul(available, 10000)
	if !ok {
		return math.MaxInt64
	}
	return product / p95
}

func absBP(delta, denominator int64) int64 {
	if denominator <= 0 {
		return 0
	}
	if delta < 0 {
		delta = -delta
	}
	product, ok := checkedMul(delta, 10000)
	if !ok {
		return math.MaxInt64
	}
	return product / denominator
}

func expiryUrgency(remaining time.Duration) string {
	switch {
	case remaining <= 0:
		return "expired"
	case remaining <= time.Hour:
		return "critical"
	case remaining <= 5*time.Hour:
		return "soon"
	case remaining <= 24*time.Hour:
		return "today"
	default:
		return "later"
	}
}
