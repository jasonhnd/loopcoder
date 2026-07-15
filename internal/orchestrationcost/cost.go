// Package orchestrationcost measures LoopCoder-owned coordination work and
// enforces provider-call budgets without treating deterministic waits as model
// activity or inventing usage that a provider did not report.
package orchestrationcost

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/reporter"
)

const (
	SchemaVersion = "loopcoder.orchestration_cost.v1"

	StatusAllowed    = "allowed"
	StatusNeedsHuman = "needs-human"

	UsageExact   = "exact"
	UsageUnknown = "unknown"

	MaxContextPacketBytes = 8 * 1024

	duplicateEventSuppressionID = "waiting:duplicate-event-id-suppressions"
)

type Role string

const (
	RolePlanner  Role = "planner"
	RoleWorker   Role = "worker"
	RoleVerifier Role = "verifier"
	RoleRecovery Role = "recovery"
	RoleDelivery Role = "delivery"
	RoleWaiting  Role = "waiting"
)

type Activity string

const (
	ActivityModelCall            Activity = "model-call"
	ActivityPhase                Activity = "phase"
	ActivityWait                 Activity = "wait"
	ActivityHeartbeat            Activity = "heartbeat"
	ActivityReceipt              Activity = "receipt"
	ActivityCIPoll               Activity = "ci-poll"
	ActivityApprovalPoll         Activity = "approval-poll"
	ActivityQuotaPoll            Activity = "quota-poll"
	ActivityDeliveryRetry        Activity = "delivery-retry"
	ActivityRecoveryRetry        Activity = "recovery-retry"
	ActivityDuplicateRetry       Activity = "duplicate-retry"
	ActivityDuplicateSuppression Activity = "duplicate-suppression"
	ActivityContextPacket        Activity = "context-packet"
)

type Policy struct {
	MaxModelCalls      int   `json:"max_model_calls"`
	MaxTokens          int64 `json:"max_tokens"`
	MaxOverheadPercent int   `json:"max_overhead_percent"`
}

func DefaultPolicy() Policy {
	return Policy{MaxModelCalls: 8, MaxTokens: 500_000, MaxOverheadPercent: 10}
}

type Event struct {
	EventID               string   `json:"event_id"`
	Role                  Role     `json:"role"`
	Activity              Activity `json:"activity"`
	ModelCalls            int      `json:"model_calls"`
	Tokens                *int64   `json:"tokens,omitempty"`
	UsefulExecution       bool     `json:"useful_execution"`
	DurationMS            int64    `json:"duration_ms,omitempty"`
	Retries               int      `json:"retries,omitempty"`
	DuplicateRetries      int      `json:"duplicate_retries,omitempty"`
	DeliveryOnlyRetries   int      `json:"delivery_only_retries,omitempty"`
	DuplicateSuppressions int      `json:"duplicate_suppressions,omitempty"`
	ContextPacketBytes    int      `json:"context_packet_bytes,omitempty"`
	Evidence              []string `json:"evidence"`
}

type RoleSummary struct {
	Role                  Role     `json:"role"`
	ModelCalls            int      `json:"model_calls"`
	Tokens                *int64   `json:"tokens,omitempty"`
	UsageState            string   `json:"usage_state"`
	DurationMS            int64    `json:"duration_ms"`
	Waits                 int      `json:"waits"`
	Retries               int      `json:"retries"`
	DuplicateRetries      int      `json:"duplicate_retries"`
	DeliveryOnlyRetries   int      `json:"delivery_only_retries"`
	DuplicateSuppressions int      `json:"duplicate_suppressions"`
	ContextPacketBytes    int      `json:"context_packet_bytes"`
	Evidence              []string `json:"evidence"`
}

type Totals struct {
	ModelCalls            int    `json:"model_calls"`
	Tokens                *int64 `json:"tokens,omitempty"`
	UsageState            string `json:"usage_state"`
	UsefulTokens          *int64 `json:"useful_tokens,omitempty"`
	UsefulUsageState      string `json:"useful_usage_state"`
	OverheadTokens        *int64 `json:"overhead_tokens,omitempty"`
	OverheadUsageState    string `json:"overhead_usage_state"`
	DurationMS            int64  `json:"duration_ms"`
	Waits                 int    `json:"waits"`
	Retries               int    `json:"retries"`
	DuplicateRetries      int    `json:"duplicate_retries"`
	DeliveryOnlyRetries   int    `json:"delivery_only_retries"`
	DuplicateSuppressions int    `json:"duplicate_suppressions"`
	ContextPacketBytes    int    `json:"context_packet_bytes"`
}

type Ratio struct {
	State       string   `json:"state"`
	BasisPoints *big.Int `json:"basis_points,omitempty"`
	Display     string   `json:"display"`
}

type ExternalHostUsage struct {
	State  string `json:"state"`
	Tokens *int64 `json:"tokens,omitempty"`
	Reason string `json:"reason"`
}

type Decision struct {
	Status      string   `json:"status"`
	Allowed     bool     `json:"allowed"`
	Reason      string   `json:"reason"`
	Observed    string   `json:"observed"`
	Limit       string   `json:"limit"`
	PRNumber    int      `json:"pr_number,omitempty"`
	Consumed    bool     `json:"consumed,omitempty"`
	Evidence    []string `json:"evidence"`
	Remediation string   `json:"remediation"`
}

type Report struct {
	SchemaVersion     string            `json:"schema_version"`
	RunID             string            `json:"run_id"`
	Status            string            `json:"status"`
	Reason            string            `json:"reason"`
	NextAction        string            `json:"next_action"`
	Policy            Policy            `json:"policy"`
	Totals            Totals            `json:"totals"`
	OverheadRatio     Ratio             `json:"overhead_ratio"`
	ExternalHostUsage ExternalHostUsage `json:"external_host_usage"`
	Roles             []RoleSummary     `json:"roles"`
	Events            []Event           `json:"events"`
	BudgetDecisions   []Decision        `json:"budget_decisions"`
	ReleaseGate       *Decision         `json:"release_gate,omitempty"`
	Evidence          []string          `json:"evidence"`
	Remediation       []string          `json:"remediation"`
}

func EventFromReport(eventID string, role Role, useful bool, record *reporter.Report, evidence ...string) Event {
	event := Event{EventID: eventID, Role: role, Activity: ActivityModelCall, ModelCalls: 1, UsefulExecution: useful, Evidence: normalizeStrings(evidence)}
	if record == nil {
		return event
	}
	event.DurationMS = record.DurationMS
	event.Tokens = reportedTotal(record.Usage)
	return event
}

func DeterministicEvent(eventID string, role Role, activity Activity, evidence ...string) Event {
	zero := int64(0)
	return Event{EventID: eventID, Role: role, Activity: activity, Tokens: &zero, Evidence: normalizeStrings(evidence)}
}

func Build(runID string, policy Policy, events []Event) (Report, error) {
	if err := validatePolicy(policy); err != nil {
		return Report{}, err
	}
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return Report{}, errors.New("orchestration cost run_id is required")
	}

	byRole := map[Role]*RoleSummary{}
	for _, role := range allRoles() {
		zero := int64(0)
		byRole[role] = &RoleSummary{Role: role, Tokens: &zero, UsageState: UsageExact, Evidence: []string{}}
	}
	seen := map[string]bool{}
	normalized := make([]Event, 0, len(events))
	duplicateEventIDs := 0
	for _, event := range events {
		event.EventID = strings.TrimSpace(event.EventID)
		event.Evidence = normalizeStrings(event.Evidence)
		if err := validateEvent(event); err != nil {
			return Report{}, err
		}
		if seen[event.EventID] {
			duplicateEventIDs++
			continue
		}
		seen[event.EventID] = true
		normalized = append(normalized, event)
		if err := addEvent(byRole[event.Role], event); err != nil {
			return Report{}, err
		}
	}
	if duplicateEventIDs > 0 {
		if err := persistDuplicateSuppressions(&normalized, byRole[RoleWaiting], duplicateEventIDs); err != nil {
			return Report{}, err
		}
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i].EventID < normalized[j].EventID })

	roles := make([]RoleSummary, 0, len(byRole))
	for _, role := range allRoles() {
		summary := *byRole[role]
		summary.Evidence = normalizeStrings(summary.Evidence)
		roles = append(roles, summary)
	}
	report := Report{
		SchemaVersion: SchemaVersion,
		RunID:         runID,
		Status:        StatusAllowed,
		Reason:        "within orchestration cost policy",
		NextAction:    "continue",
		Policy:        policy,
		Roles:         roles,
		Events:        normalized,
		ExternalHostUsage: ExternalHostUsage{
			State:  UsageUnknown,
			Reason: "external host token usage is not observable from LoopCoder-owned reports",
		},
		Evidence:        []string{},
		Remediation:     []string{},
		BudgetDecisions: []Decision{},
	}
	totals, err := sumRoles(roles)
	if err != nil {
		return Report{}, err
	}
	report.Totals = totals
	report.OverheadRatio = overheadRatio(report.Totals)
	report.Evidence = costEvidence(report)
	return report, nil
}

func ApplyBudgetDecision(report Report, decision Decision) Report {
	report.BudgetDecisions = append(report.BudgetDecisions, decision)
	return applyDecision(report, decision)
}

// ReapplyBudgetDecision restores the active effect of an already-recorded
// decision after deterministic accounting events rebuild the report.
func ReapplyBudgetDecision(report Report, decision Decision) Report {
	return applyDecision(report, decision)
}

func ApplyReleaseDecision(report Report, decision Decision) Report {
	report.ReleaseGate = &decision
	return applyDecision(report, decision)
}

func BindReleaseDecision(decision Decision, prNumber int) Decision {
	return BindDecisionToPR(decision, prNumber)
}

func BindDecisionToPR(decision Decision, prNumber int) Decision {
	if prNumber > 0 {
		decision.PRNumber = prNumber
	}
	return decision
}

func MarkBudgetDecisionConsumed(report Report, prNumber int) Report {
	for i := len(report.BudgetDecisions) - 1; i >= 0; i-- {
		decision := &report.BudgetDecisions[i]
		if decision.Allowed || decision.Consumed || decision.PRNumber != prNumber {
			continue
		}
		decision.Consumed = true
	}
	return report
}

func MarkReleaseConsumed(report Report, prNumber int) (Report, error) {
	if report.ReleaseGate == nil {
		return report, nil
	}
	gate := *report.ReleaseGate
	if gate.PRNumber > 0 && prNumber > 0 && gate.PRNumber != prNumber {
		return report, fmt.Errorf("release gate targets PR #%d, cannot consume PR #%d", gate.PRNumber, prNumber)
	}
	if gate.PRNumber == 0 {
		gate.PRNumber = prNumber
	}
	gate.Consumed = true
	report.ReleaseGate = &gate
	return report, nil
}

// RestoreDecisionState keeps the durable decision trail while re-evaluating
// its active effect against the policy currently configured for the run. This
// makes the documented remediation (raising a budget) effective without
// discarding why an earlier invocation stopped.
func RestoreDecisionState(report Report, budgetDecisions []Decision, releaseGate *Decision) Report {
	report.BudgetDecisions = append([]Decision(nil), budgetDecisions...)
	report.ReleaseGate = nil

	// A budget decision is prospective: reaching the configured cap is valid
	// until another provider call is actually proposed. Only durable accounting
	// failures remain active while reconstructing a report; BeforeProviderCall
	// and the release gate re-evaluate their respective policies at the point of
	// action.
	for i := len(budgetDecisions) - 1; i >= 0; i-- {
		if unrecoverableAccountingReason(budgetDecisions[i].Reason) {
			return applyDecision(report, budgetDecisions[i])
		}
	}
	if releaseGate == nil {
		return report
	}
	if releaseGate.Consumed {
		release := *releaseGate
		report.ReleaseGate = &release
		return report
	}
	release := checkReleaseGate(report)
	release.PRNumber = releaseGate.PRNumber
	report.ReleaseGate = &release
	return applyDecision(report, release)
}

func PersistenceFailure(err error) Decision {
	detail := "unknown persistence failure"
	if err != nil {
		detail = err.Error()
	}
	return blocked("orchestration-cost-ledger-write-failed", detail, "durable per-run ledger", "repair .loopcoder run-state storage or continue manually")
}

func AccountingFailure(err error) Decision {
	detail := "unknown accounting failure"
	if err != nil {
		detail = err.Error()
	}
	return blocked("orchestration-cost-accounting-failed", detail, "valid cost event", "repair orchestration cost evidence or continue manually")
}

func applyDecision(report Report, decision Decision) Report {
	if decision.Allowed {
		return report
	}
	report.Status = StatusNeedsHuman
	report.Reason = decision.Reason
	report.NextAction = decision.Remediation
	report.Evidence = normalizeStrings(append(report.Evidence, decision.Evidence...))
	report.Remediation = normalizeStrings(append(report.Remediation, decision.Remediation))
	return report
}

func CheckBeforeModelCall(report Report, proposedCalls int) Decision {
	if report.Status == StatusNeedsHuman {
		return blocked("orchestration-cost-already-blocked", report.Reason, "needs-human", report.NextAction)
	}
	return checkBeforeModelCall(report, proposedCalls)
}

func checkBeforeModelCall(report Report, proposedCalls int) Decision {
	if proposedCalls <= 0 {
		proposedCalls = 1
	}
	if report.Totals.ModelCalls+proposedCalls > report.Policy.MaxModelCalls {
		return blocked("model-call-budget", fmt.Sprintf("%d+%d", report.Totals.ModelCalls, proposedCalls), fmt.Sprint(report.Policy.MaxModelCalls), "increase orchestration.cost_budget.max_model_calls or continue manually")
	}
	if report.Totals.UsageState == UsageUnknown {
		return blocked("token-budget-unknown", "unknown", fmt.Sprint(report.Policy.MaxTokens), "supply exact provider token usage or continue manually")
	}
	if report.Totals.Tokens != nil && *report.Totals.Tokens >= report.Policy.MaxTokens {
		return blocked("token-budget-exhausted", fmt.Sprint(*report.Totals.Tokens), fmt.Sprint(report.Policy.MaxTokens), "increase orchestration.cost_budget.max_tokens or continue manually")
	}
	return Decision{Status: StatusAllowed, Allowed: true, Reason: "within orchestration cost budget", Observed: fmt.Sprintf("calls=%d tokens=%s", report.Totals.ModelCalls, tokenDisplay(report.Totals.Tokens)), Limit: fmt.Sprintf("calls=%d tokens=%d", report.Policy.MaxModelCalls, report.Policy.MaxTokens), Evidence: []string{"loopcoder-owned-usage-only"}, Remediation: "none"}
}

func CheckReleaseGate(report Report) Decision {
	if report.Status == StatusNeedsHuman && unrecoverableAccountingReason(report.Reason) {
		return blocked("orchestration-cost-already-blocked", report.Reason, "needs-human", report.NextAction)
	}
	return checkReleaseGate(report)
}

func checkReleaseGate(report Report) Decision {
	if report.Totals.ModelCalls > report.Policy.MaxModelCalls {
		return blocked("model-call-budget-exceeded", fmt.Sprint(report.Totals.ModelCalls), fmt.Sprint(report.Policy.MaxModelCalls), "increase orchestration.cost_budget.max_model_calls or require human release review")
	}
	if report.Totals.UsageState == UsageUnknown || report.Totals.Tokens == nil {
		return blocked("token-budget-unknown", UsageUnknown, fmt.Sprint(report.Policy.MaxTokens), "supply exact provider token usage or require human release review")
	}
	if *report.Totals.Tokens > report.Policy.MaxTokens {
		return blocked("token-budget-exceeded", fmt.Sprint(*report.Totals.Tokens), fmt.Sprint(report.Policy.MaxTokens), "increase orchestration.cost_budget.max_tokens or require human release review")
	}
	if report.OverheadRatio.State == UsageUnknown {
		return blocked("overhead-ratio-unknown", report.OverheadRatio.Display, fmt.Sprintf("%d%%", report.Policy.MaxOverheadPercent), "supply exact token usage for coordination calls or require human release review")
	}
	if overheadExceedsPercent(report.Totals, report.Policy.MaxOverheadPercent) {
		return blocked("overhead-ratio-exceeded", report.OverheadRatio.Display, fmt.Sprintf("%d%%", report.Policy.MaxOverheadPercent), "reduce recovery or coordination model calls before release")
	}
	return Decision{Status: StatusAllowed, Allowed: true, Reason: "orchestration overhead ratio is within policy", Observed: report.OverheadRatio.Display, Limit: fmt.Sprintf("%d%%", report.Policy.MaxOverheadPercent), Evidence: []string{"loopcoder-owned-token-ratio"}, Remediation: "none"}
}

func overheadExceedsPercent(totals Totals, limitPercent int) bool {
	if totals.OverheadTokens == nil || totals.UsefulTokens == nil || *totals.UsefulTokens <= 0 {
		return false
	}
	left := new(big.Int).Mul(big.NewInt(*totals.OverheadTokens), big.NewInt(100))
	right := new(big.Int).Mul(big.NewInt(*totals.UsefulTokens), big.NewInt(int64(limitPercent)))
	return left.Cmp(right) > 0
}

func unrecoverableAccountingReason(reason string) bool {
	switch strings.TrimSpace(reason) {
	case "orchestration-cost-ledger-write-failed", "orchestration-cost-accounting-failed":
		return true
	default:
		return false
	}
}

func validatePolicy(policy Policy) error {
	if policy.MaxModelCalls <= 0 || policy.MaxTokens <= 0 || policy.MaxOverheadPercent <= 0 || policy.MaxOverheadPercent > 100 {
		return errors.New("invalid orchestration cost policy")
	}
	return nil
}

func validateEvent(event Event) error {
	if event.EventID == "" {
		return errors.New("orchestration cost event_id is required")
	}
	if !validRole(event.Role) {
		return fmt.Errorf("unsupported orchestration cost role %q", event.Role)
	}
	if !validActivity(event.Activity) {
		return fmt.Errorf("unsupported orchestration cost activity %q", event.Activity)
	}
	if event.ModelCalls < 0 || event.DurationMS < 0 || event.Retries < 0 || event.DuplicateRetries < 0 || event.DeliveryOnlyRetries < 0 || event.DuplicateSuppressions < 0 || event.ContextPacketBytes < 0 {
		return errors.New("orchestration cost counters must be non-negative")
	}
	if event.Tokens != nil && *event.Tokens < 0 {
		return errors.New("orchestration cost tokens must be non-negative")
	}
	if zeroModelActivity(event.Activity) && (event.ModelCalls != 0 || event.Tokens == nil || *event.Tokens != 0) {
		return fmt.Errorf("%s must report exactly zero model calls and tokens", event.Activity)
	}
	if event.Activity == ActivityModelCall && event.ModelCalls == 0 {
		return errors.New("model-call event must report at least one model call")
	}
	if event.UsefulExecution && event.Role != RoleWorker && event.Role != RoleVerifier {
		return errors.New("only worker and verifier events may be useful execution")
	}
	if event.ContextPacketBytes > MaxContextPacketBytes {
		return fmt.Errorf("context packet exceeds %d bytes", MaxContextPacketBytes)
	}
	return nil
}

func addEvent(summary *RoleSummary, event Event) error {
	summary.ModelCalls += event.ModelCalls
	summary.DurationMS += event.DurationMS
	summary.Retries += event.Retries
	summary.DuplicateRetries += event.DuplicateRetries
	summary.DeliveryOnlyRetries += event.DeliveryOnlyRetries
	summary.DuplicateSuppressions += event.DuplicateSuppressions
	summary.ContextPacketBytes += event.ContextPacketBytes
	if event.Activity == ActivityWait || event.Activity == ActivityCIPoll || event.Activity == ActivityApprovalPoll || event.Activity == ActivityQuotaPoll {
		summary.Waits++
	}
	if event.ModelCalls > 0 && event.Tokens == nil {
		summary.Tokens = nil
		summary.UsageState = UsageUnknown
	} else if event.Tokens != nil && summary.UsageState == UsageExact {
		value := *event.Tokens
		if summary.Tokens != nil {
			if *summary.Tokens > math.MaxInt64-value {
				return errors.New("orchestration cost token total overflow")
			}
			value += *summary.Tokens
		}
		summary.Tokens = &value
	}
	summary.Evidence = append(summary.Evidence, event.Evidence...)
	return nil
}

func sumRoles(roles []RoleSummary) (Totals, error) {
	zero, usefulZero, overheadZero := int64(0), int64(0), int64(0)
	totals := Totals{Tokens: &zero, UsageState: UsageExact, UsefulTokens: &usefulZero, UsefulUsageState: UsageExact, OverheadTokens: &overheadZero, OverheadUsageState: UsageExact}
	for _, role := range roles {
		totals.ModelCalls += role.ModelCalls
		totals.DurationMS += role.DurationMS
		totals.Waits += role.Waits
		totals.Retries += role.Retries
		totals.DuplicateRetries += role.DuplicateRetries
		totals.DeliveryOnlyRetries += role.DeliveryOnlyRetries
		totals.DuplicateSuppressions += role.DuplicateSuppressions
		totals.ContextPacketBytes += role.ContextPacketBytes
		if err := addUsage(&totals.Tokens, &totals.UsageState, role.Tokens, role.UsageState, role.ModelCalls); err != nil {
			return Totals{}, err
		}
		if role.Role == RoleWorker || role.Role == RoleVerifier {
			if err := addUsage(&totals.UsefulTokens, &totals.UsefulUsageState, role.Tokens, role.UsageState, role.ModelCalls); err != nil {
				return Totals{}, err
			}
		} else {
			if err := addUsage(&totals.OverheadTokens, &totals.OverheadUsageState, role.Tokens, role.UsageState, role.ModelCalls); err != nil {
				return Totals{}, err
			}
		}
	}
	return totals, nil
}

func addUsage(total **int64, state *string, value *int64, valueState string, calls int) error {
	if calls > 0 && (valueState == UsageUnknown || value == nil) {
		*total = nil
		*state = UsageUnknown
		return nil
	}
	if *state == UsageUnknown || value == nil {
		return nil
	}
	if **total > math.MaxInt64-*value {
		return errors.New("orchestration cost token total overflow")
	}
	next := **total + *value
	*total = &next
	return nil
}

func overheadRatio(totals Totals) Ratio {
	zero := big.NewInt(0)
	if totals.ModelCalls == 0 {
		return Ratio{State: UsageExact, BasisPoints: zero, Display: "0.00%"}
	}
	if totals.OverheadUsageState != UsageExact || totals.UsefulUsageState != UsageExact || totals.OverheadTokens == nil || totals.UsefulTokens == nil || *totals.UsefulTokens <= 0 {
		return Ratio{State: UsageUnknown, Display: UsageUnknown}
	}
	if *totals.OverheadTokens == 0 {
		return Ratio{State: UsageExact, BasisPoints: zero, Display: "0.00%"}
	}
	bps := new(big.Int).Mul(big.NewInt(*totals.OverheadTokens), big.NewInt(10_000))
	bps.Quo(bps, big.NewInt(*totals.UsefulTokens))
	whole, fraction := new(big.Int), new(big.Int)
	whole.QuoRem(bps, big.NewInt(100), fraction)
	return Ratio{State: UsageExact, BasisPoints: bps, Display: fmt.Sprintf("%s.%02d%%", whole.String(), fraction.Int64())}
}

func persistDuplicateSuppressions(events *[]Event, waiting *RoleSummary, count int) error {
	evidence := fmt.Sprintf("duplicate-event-ids-suppressed:%d", count)
	for i := range *events {
		if (*events)[i].EventID != duplicateEventSuppressionID {
			continue
		}
		(*events)[i].DuplicateSuppressions += count
		(*events)[i].Evidence = normalizeStrings(append((*events)[i].Evidence, evidence))
		waiting.DuplicateSuppressions += count
		waiting.Evidence = append(waiting.Evidence, evidence)
		return nil
	}
	event := DeterministicEvent(duplicateEventSuppressionID, RoleWaiting, ActivityDuplicateSuppression, evidence)
	event.DuplicateSuppressions = count
	*events = append(*events, event)
	return addEvent(waiting, event)
}

func reportedTotal(usage reporter.Usage) *int64 {
	if usage.TotalTokens != nil {
		value := *usage.TotalTokens
		return &value
	}
	if usage.InputTokens != nil && usage.OutputTokens != nil {
		value := *usage.InputTokens + *usage.OutputTokens
		return &value
	}
	return nil
}

func costEvidence(report Report) []string {
	return []string{
		fmt.Sprintf("roles=%d", len(report.Roles)),
		fmt.Sprintf("model_calls=%d", report.Totals.ModelCalls),
		"external_host_usage=unknown",
	}
}

func blocked(reason, observed, limit, remediation string) Decision {
	return Decision{Status: StatusNeedsHuman, Allowed: false, Reason: reason, Observed: observed, Limit: limit, Evidence: []string{"loopcoder-owned-usage-only", "no-provider-fallback-invoked"}, Remediation: remediation}
}

func tokenDisplay(value *int64) string {
	if value == nil {
		return UsageUnknown
	}
	return fmt.Sprint(*value)
}

func zeroModelActivity(activity Activity) bool {
	switch activity {
	case ActivityWait, ActivityHeartbeat, ActivityReceipt, ActivityCIPoll, ActivityApprovalPoll, ActivityQuotaPoll, ActivityDeliveryRetry, ActivityDuplicateSuppression, ActivityContextPacket, ActivityPhase:
		return true
	default:
		return false
	}
}

func validRole(role Role) bool {
	for _, candidate := range allRoles() {
		if role == candidate {
			return true
		}
	}
	return false
}

func allRoles() []Role {
	return []Role{RolePlanner, RoleWorker, RoleVerifier, RoleRecovery, RoleDelivery, RoleWaiting}
}

func validActivity(activity Activity) bool {
	switch activity {
	case ActivityModelCall, ActivityPhase, ActivityWait, ActivityHeartbeat, ActivityReceipt, ActivityCIPoll, ActivityApprovalPoll, ActivityQuotaPoll, ActivityDeliveryRetry, ActivityRecoveryRetry, ActivityDuplicateRetry, ActivityDuplicateSuppression, ActivityContextPacket:
		return true
	default:
		return false
	}
}

func normalizeStrings(values []string) []string {
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
