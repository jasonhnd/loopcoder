package orchestrationcost

import (
	"math"
	"math/big"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/reporter"
)

func TestDeterministicActivitiesRejectProviderUsageAndReportExactZero(t *testing.T) {
	activities := []Activity{ActivityWait, ActivityHeartbeat, ActivityReceipt, ActivityCIPoll, ActivityApprovalPoll, ActivityQuotaPoll, ActivityDeliveryRetry}
	for _, activity := range activities {
		t.Run(string(activity), func(t *testing.T) {
			event := DeterministicEvent("event-"+string(activity), RoleWaiting, activity, "provider-free")
			report, err := Build("run-zero", DefaultPolicy(), []Event{event})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if report.Totals.ModelCalls != 0 || report.Totals.Tokens == nil || *report.Totals.Tokens != 0 || report.Totals.UsageState != UsageExact {
				t.Fatalf("totals = %#v", report.Totals)
			}
			event.ModelCalls = 1
			if _, err := Build("run-invalid", DefaultPolicy(), []Event{event}); err == nil || !strings.Contains(err.Error(), "exactly zero") {
				t.Fatalf("provider-backed deterministic activity error = %v", err)
			}
		})
	}
}

func TestTerminalUnknownUsageStaysUnknownAndFallsBackToCallBudget(t *testing.T) {
	report, err := Build("run-unknown", DefaultPolicy(), []Event{EventFromReport("worker-1", RoleWorker, true, &reporter.Report{})})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if report.Totals.Tokens != nil || report.Totals.UsageState != UsageUnknown || report.ExternalHostUsage.State != UsageUnknown || report.ExternalHostUsage.Tokens != nil {
		t.Fatalf("report = %#v", report)
	}
	decision := CheckBeforeModelCall(report, 1)
	if !decision.Allowed || decision.Reason != "within orchestration call budget; token usage unknown" {
		t.Fatalf("decision = %#v", decision)
	}
	if report.OverheadRatio.State != UsageUnknown || report.OverheadRatio.Display != UsageUnknown {
		t.Fatalf("overhead ratio = %#v, want unknown useful denominator", report.OverheadRatio)
	}
}

func TestPendingProviderReservationBlocksNextProvider(t *testing.T) {
	event := EventFromReport("worker-1", RoleWorker, true, nil, "provider-call-reserved")
	report, err := Build("run-pending", DefaultPolicy(), []Event{event})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	decision := CheckBeforeModelCall(report, 1)
	if decision.Allowed || decision.Reason != "provider-call-usage-pending" {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestBudgetExhaustionBlocksOnlyNextCall(t *testing.T) {
	tokens := int64(100)
	policy := Policy{MaxModelCalls: 1, MaxTokens: 100, MaxOverheadPercent: 10}
	report, err := Build("run-budget", policy, []Event{EventFromReport("worker-1", RoleWorker, true, &reporter.Report{Usage: reporter.Usage{TotalTokens: &tokens}})})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	decision := CheckBeforeModelCall(report, 1)
	if decision.Allowed || decision.Status != StatusNeedsHuman || len(report.Events) != 1 {
		t.Fatalf("decision=%#v report=%#v", decision, report)
	}
}

func TestBuildRejectsTokenAggregationOverflow(t *testing.T) {
	maximum, one := int64(math.MaxInt64), int64(1)
	_, err := Build("run-overflow", Policy{MaxModelCalls: 2, MaxTokens: math.MaxInt64, MaxOverheadPercent: 10}, []Event{
		EventFromReport("worker-1", RoleWorker, true, &reporter.Report{Usage: reporter.Usage{TotalTokens: &maximum}}),
		EventFromReport("worker-2", RoleWorker, true, &reporter.Report{Usage: reporter.Usage{TotalTokens: &one}}),
	})
	if err == nil || !strings.Contains(err.Error(), "token total overflow") {
		t.Fatalf("Build overflow error = %v", err)
	}
}

func TestConsumedReleaseGateRestoresAsHistorical(t *testing.T) {
	tokens := int64(100)
	report, err := Build("run-consumed-release", DefaultPolicy(), []Event{
		EventFromReport("worker-1", RoleWorker, true, &reporter.Report{Usage: reporter.Usage{TotalTokens: &tokens}}),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	report = ApplyReleaseDecision(report, BindReleaseDecision(CheckReleaseGate(report), 98))
	report, err = MarkReleaseConsumed(report, 98)
	if err != nil {
		t.Fatalf("MarkReleaseConsumed: %v", err)
	}
	restored, err := Build(report.RunID, report.Policy, report.Events)
	if err != nil {
		t.Fatalf("Build restored: %v", err)
	}
	restored = RestoreDecisionState(restored, report.BudgetDecisions, report.ReleaseGate)
	if restored.Status != StatusAllowed || restored.ReleaseGate == nil || restored.ReleaseGate.PRNumber != 98 || !restored.ReleaseGate.Consumed {
		t.Fatalf("restored = %#v", restored)
	}
}

func TestRestoreDecisionStateReevaluatesHistoricalDenialWithRaisedPolicy(t *testing.T) {
	tokens := int64(100)
	oldPolicy := Policy{MaxModelCalls: 1, MaxTokens: 100, MaxOverheadPercent: 10}
	events := []Event{EventFromReport("worker-1", RoleWorker, true, &reporter.Report{Usage: reporter.Usage{TotalTokens: &tokens}})}
	oldReport, err := Build("run-raised-budget", oldPolicy, events)
	if err != nil {
		t.Fatalf("Build old report: %v", err)
	}
	oldReport = ApplyBudgetDecision(oldReport, CheckBeforeModelCall(oldReport, 1))
	if oldReport.Status != StatusNeedsHuman {
		t.Fatalf("old report = %#v, want needs-human", oldReport)
	}

	newPolicy := Policy{MaxModelCalls: 2, MaxTokens: 200, MaxOverheadPercent: 10}
	resumed, err := Build("run-raised-budget", newPolicy, events)
	if err != nil {
		t.Fatalf("Build resumed report: %v", err)
	}
	resumed = RestoreDecisionState(resumed, oldReport.BudgetDecisions, oldReport.ReleaseGate)
	if resumed.Status != StatusAllowed || len(resumed.BudgetDecisions) != 1 {
		t.Fatalf("resumed report = %#v, want allowed with historical decision", resumed)
	}
	if decision := CheckBeforeModelCall(resumed, 1); !decision.Allowed {
		t.Fatalf("raised budget still blocked next provider: %#v", decision)
	}
}

func TestRestoreDecisionStateKeepsExactCallCapAllowedUntilNextProposal(t *testing.T) {
	tokens := int64(10)
	policy := Policy{MaxModelCalls: 1, MaxTokens: 100, MaxOverheadPercent: 10}
	events := []Event{EventFromReport("worker-1", RoleWorker, true, &reporter.Report{Usage: reporter.Usage{TotalTokens: &tokens}})}
	report, err := Build("run-exact-call-cap", policy, events)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	historical := CheckBeforeModelCall(report, 1)
	if historical.Allowed {
		t.Fatalf("next provider decision = %#v, want blocked", historical)
	}
	restored := RestoreDecisionState(report, []Decision{historical}, nil)
	if restored.Status != StatusAllowed || len(restored.BudgetDecisions) != 1 {
		t.Fatalf("restored report = %#v, want allowed with historical denial", restored)
	}
	if decision := CheckBeforeModelCall(restored, 1); decision.Allowed {
		t.Fatalf("actual next provider proposal = %#v, want blocked", decision)
	}
}

func TestOverheadRatioAndRetryClassesAreStable(t *testing.T) {
	useful := int64(1000)
	overhead := int64(101)
	events := []Event{
		EventFromReport("worker-1", RoleWorker, true, &reporter.Report{Usage: reporter.Usage{TotalTokens: &useful}}),
		EventFromReport("recovery-1", RoleRecovery, false, &reporter.Report{Usage: reporter.Usage{TotalTokens: &overhead}}),
		{EventID: "retries", Role: RoleRecovery, Activity: ActivityRecoveryRetry, Tokens: int64Ptr(0), Retries: 2, DuplicateRetries: 1, DeliveryOnlyRetries: 1, Evidence: []string{"retry-ledger"}},
	}
	report, err := Build("run-ratio", DefaultPolicy(), events)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if report.OverheadRatio.Display != "10.10%" || report.Totals.DuplicateRetries != 1 || report.Totals.DeliveryOnlyRetries != 1 {
		t.Fatalf("report = %#v", report)
	}
	decision := CheckReleaseGate(report)
	if decision.Allowed || decision.Reason != "overhead-ratio-exceeded" {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestReleaseGateRejectsFractionAboveConfiguredPercent(t *testing.T) {
	useful := int64(100_000)
	overhead := int64(10_001)
	report, err := Build("run-ratio-boundary", DefaultPolicy(), []Event{
		EventFromReport("worker-1", RoleWorker, true, &reporter.Report{Usage: reporter.Usage{TotalTokens: &useful}}),
		EventFromReport("recovery-1", RoleRecovery, false, &reporter.Report{Usage: reporter.Usage{TotalTokens: &overhead}}),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if report.OverheadRatio.Display != "10.00%" {
		t.Fatalf("display = %q, want stable two-decimal display", report.OverheadRatio.Display)
	}
	if decision := CheckReleaseGate(report); decision.Allowed || decision.Reason != "overhead-ratio-exceeded" {
		t.Fatalf("fractional overrun decision = %#v", decision)
	}
}

func TestOverheadRatioUsesArbitraryPrecisionBasisPoints(t *testing.T) {
	useful, overhead := int64(1), int64(math.MaxInt64-1)
	report, err := Build("run-ratio-large", Policy{MaxModelCalls: 2, MaxTokens: math.MaxInt64, MaxOverheadPercent: 10}, []Event{
		EventFromReport("worker-1", RoleWorker, true, &reporter.Report{Usage: reporter.Usage{TotalTokens: &useful}}),
		EventFromReport("recovery-1", RoleRecovery, false, &reporter.Report{Usage: reporter.Usage{TotalTokens: &overhead}}),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := new(big.Int).Mul(big.NewInt(overhead), big.NewInt(10_000))
	if report.OverheadRatio.BasisPoints == nil || report.OverheadRatio.BasisPoints.Cmp(want) != 0 || strings.HasPrefix(report.OverheadRatio.Display, "-") {
		t.Fatalf("ratio = %#v, want basis points %s", report.OverheadRatio, want)
	}
}

func TestReleaseGateRejectsUnknownUsageAndTokenOverrun(t *testing.T) {
	unknown, err := Build("run-release-unknown", DefaultPolicy(), []Event{
		EventFromReport("verifier-1", RoleVerifier, true, &reporter.Report{}),
	})
	if err != nil {
		t.Fatalf("Build unknown: %v", err)
	}
	if decision := CheckReleaseGate(unknown); decision.Allowed || decision.Reason != "token-budget-unknown" {
		t.Fatalf("unknown decision = %#v", decision)
	}

	tokens := int64(101)
	policy := Policy{MaxModelCalls: 2, MaxTokens: 100, MaxOverheadPercent: 10}
	overrun, err := Build("run-release-overrun", policy, []Event{
		EventFromReport("worker-1", RoleWorker, true, &reporter.Report{Usage: reporter.Usage{TotalTokens: &tokens}}),
	})
	if err != nil {
		t.Fatalf("Build overrun: %v", err)
	}
	if decision := CheckReleaseGate(overrun); decision.Allowed || decision.Reason != "token-budget-exceeded" {
		t.Fatalf("overrun decision = %#v", decision)
	}
}

func TestDuplicateEventIDsAreSuppressedAndPacketIsBounded(t *testing.T) {
	event := DeterministicEvent("same", RoleWaiting, ActivityContextPacket, "packet")
	event.ContextPacketBytes = MaxContextPacketBytes
	report, err := Build("run-dedupe", DefaultPolicy(), []Event{event, event})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(report.Events) != 2 || report.Totals.DuplicateSuppressions != 1 || report.Totals.ContextPacketBytes != MaxContextPacketBytes {
		t.Fatalf("report = %#v", report)
	}
	event.EventID = "too-large"
	event.ContextPacketBytes++
	if _, err := Build("run-large", DefaultPolicy(), []Event{event}); err == nil {
		t.Fatal("Build accepted oversized context packet")
	}
}

func int64Ptr(value int64) *int64 { return &value }
