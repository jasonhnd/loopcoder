package simulation

import (
	"fmt"
	"sort"
	"strings"
)

// HumanDecisionEvidence renders a stable, redacted review surface for simulation
// results. CanonicalResultJSON remains the machine contract; this is the
// human-facing companion used by release regression snapshots.
func HumanDecisionEvidence(result Result) string {
	result = normalizeResult(result)
	var b strings.Builder
	fmt.Fprintf(&b, "schema_version: %s\n", result.SchemaVersion)
	fmt.Fprintf(&b, "scenario_id: %s\n", result.ScenarioID)
	fmt.Fprintf(&b, "seed: %d\n", result.Seed)
	fmt.Fprintf(&b, "clock: %s step_ms=%d\n", result.Clock.Origin, result.Clock.StepMS)
	fmt.Fprintf(&b, "policy: version=%s profile=%s fingerprint=%s plan=%s authorization=%s\n",
		result.PolicyProvenance.PolicyVersion,
		result.PolicyProvenance.RoutingPolicyProfileID,
		result.PolicyProvenance.PolicyFingerprint,
		result.PolicyProvenance.PlanFingerprint,
		result.PolicyProvenance.AuthorizationFingerprint)
	fmt.Fprintf(&b, "sources: project=%s delivery=%s inventory=%s budget=%s routing=%s agent_tree=%s handoff=%s state=%s\n",
		result.DurableSourceIDs.ProjectID,
		result.DurableSourceIDs.DeliveryRunID,
		result.DurableSourceIDs.InventorySourceID,
		result.DurableSourceIDs.BudgetSourceID,
		result.DurableSourceIDs.RoutingSourceID,
		result.DurableSourceIDs.AgentTreeSourceID,
		result.DurableSourceIDs.HandoffSourceID,
		result.DurableSourceIDs.DurableStateID)
	fmt.Fprintf(&b, "truncated: %t", result.Truncated)
	if result.TruncationReason != "" {
		fmt.Fprintf(&b, " reason=%s", result.TruncationReason)
	}
	b.WriteByte('\n')
	b.WriteString("decisions:\n")
	if len(result.Decisions) == 0 {
		b.WriteString("- none\n")
	}
	for _, decision := range result.Decisions {
		fmt.Fprintf(&b, "- %s event=%s task=%s kind=%s accepted=%t chosen=%s budget=%s reason=%s\n",
			decision.DecisionID,
			decision.EventID,
			decision.TaskID,
			decision.DecisionKind,
			decision.Accepted,
			decision.ChosenCandidateID,
			decision.BudgetAuthorityID,
			redact(decision.Reason))
		if len(decision.QuotaSnapshotIDs) > 0 {
			fmt.Fprintf(&b, "  quota_snapshots=%s\n", strings.Join(decision.QuotaSnapshotIDs, ","))
		}
		for _, rejection := range decision.RejectedCandidates {
			fmt.Fprintf(&b, "  rejected=%s code=%s reason=%s\n", rejection.CandidateID, rejection.Code, redact(rejection.Reason))
		}
	}
	b.WriteString("events:\n")
	if len(result.EventLog) == 0 {
		b.WriteString("- none\n")
	}
	for _, event := range result.EventLog {
		fmt.Fprintf(&b, "- %03d %s kind=%s task=%s status=%s decision=%s receipt=%s idem=%s at=%s\n",
			event.Sequence,
			event.EventID,
			event.EventKind,
			event.TaskID,
			event.Status,
			event.DecisionID,
			event.ReceiptID,
			event.IdempotencyKey,
			event.At)
	}
	b.WriteString("durable_state:\n")
	fmt.Fprintf(&b, "- applied_events=%s\n", joinOrNone(result.DurableState.AppliedEventIDs))
	fmt.Fprintf(&b, "- completed_tasks=%s\n", joinOrNone(result.DurableState.CompletedTaskIDs))
	b.WriteString("- provider_receipts:\n")
	if len(result.DurableState.ProviderReceipts) == 0 {
		b.WriteString("  - none\n")
	}
	for _, receipt := range result.DurableState.ProviderReceipts {
		fmt.Fprintf(&b, "  - %s event=%s task=%s model=%s status=%s failure=%s latency_ms=%d cost_microunits=%d\n",
			receipt.ReceiptID,
			receipt.EventID,
			receipt.TaskID,
			receipt.ModelCapabilityID,
			receipt.Status,
			receipt.FailureCode,
			receipt.LatencyMS,
			receipt.CostMicrounits)
	}
	b.WriteString("- handoffs:\n")
	if len(result.DurableState.Handoffs) == 0 {
		b.WriteString("  - none\n")
	}
	for _, handoff := range result.DurableState.Handoffs {
		fmt.Fprintf(&b, "  - %s event=%s source=%s target=%s authorization=%s\n",
			handoff.HandoffID,
			handoff.EventID,
			handoff.SourceTaskID,
			handoff.TargetTaskID,
			handoff.AuthorizationRef)
	}
	b.WriteString("- agent_owners:\n")
	if len(result.DurableState.AgentOwners) == 0 {
		b.WriteString("  - none\n")
	}
	for _, owner := range result.DurableState.AgentOwners {
		fmt.Fprintf(&b, "  - %s event=%s task=%s resource=%s state=%s permission=%s side_effect=%s\n",
			owner.OwnershipID,
			owner.EventID,
			owner.TaskID,
			owner.ResourceKey,
			owner.OwnerState,
			owner.Permission,
			owner.SideEffectClass)
	}
	b.WriteString("invariants:\n")
	if len(result.InvariantResults) == 0 {
		b.WriteString("- none\n")
	}
	for _, invariant := range result.InvariantResults {
		fmt.Fprintf(&b, "- %s kind=%s passed=%t diagnostic=%s\n",
			invariant.InvariantID,
			invariant.Kind,
			invariant.Passed,
			redact(invariant.Diagnostic))
	}
	b.WriteString("diagnostics:\n")
	if len(result.Diagnostics) == 0 {
		b.WriteString("- none\n")
	}
	diagnostics := append([]Diagnostic(nil), result.Diagnostics...)
	sort.Slice(diagnostics, func(i, j int) bool {
		if diagnostics[i].Code != diagnostics[j].Code {
			return diagnostics[i].Code < diagnostics[j].Code
		}
		return diagnostics[i].Message < diagnostics[j].Message
	})
	for _, diagnostic := range diagnostics {
		fmt.Fprintf(&b, "- %s %s\n", diagnostic.Code, redact(diagnostic.Message))
	}
	fmt.Fprintf(&b, "diff: changed=%t before=%s after=%s added_events=%s added_decisions=%s\n",
		result.Diff.Changed,
		result.Diff.BeforeHash,
		result.Diff.AfterHash,
		joinOrNone(result.Diff.AddedEventIDs),
		joinOrNone(result.Diff.AddedDecisionIDs))
	return b.String()
}

func joinOrNone(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ",")
}
