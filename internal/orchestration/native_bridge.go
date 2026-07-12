package orchestration

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/runtimecap"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

const (
	NativeChildResultEnvelopeSchemaV1 = "loopcoder.native_child_result.v1"
	maxNativeChildRawOutputBytes      = 16 * 1024
)

type NativeSubAgentAdapter interface {
	StartNativeChild(context.Context, storage.AgentRegistrationRecord, ChildRunPlan) (NativeChildOutput, error)
}

type NativeChildOutput struct {
	Status          string
	Summary         string
	RawOutput       string
	ProviderReceipt string
}

type NativeChildResultEnvelope struct {
	SchemaVersion string `json:"schema_version"`
	Status        string `json:"status"`
	Summary       string `json:"summary,omitempty"`
	RawOutput     string `json:"raw_output,omitempty"`
	Truncated     bool   `json:"truncated"`
	Trusted       bool   `json:"trusted"`
	Accepted      bool   `json:"accepted"`
}

type NativeChildStartOptions struct {
	Store        storage.Store
	Contract     runtimecap.Contract
	ParentRunID  string
	Child        ChildRunPlan
	Registration storage.AgentRegistrationRequest
	Adapter      NativeSubAgentAdapter
	Clock        func() time.Time
	Lease        time.Duration
}

func StartProviderNativeChild(ctx context.Context, opts NativeChildStartOptions) (ChildRunResult, error) {
	child := opts.Child
	result := childResultFromPlan(child)
	adapterID := strings.TrimSpace(opts.Registration.AdapterID)
	if adapterID == "" {
		adapterID = strings.TrimSpace(child.AdapterID)
	}
	if adapterID == "" {
		result.Status = NestedStatusNeedsHuman
		result.Error = fmt.Sprintf("%v: adapter_id is required for native child %q", storage.ErrUnsupportedNativeSubAgent, child.ChildKey)
		return withNestedDecision(result), fmt.Errorf("%w: adapter_id is required", storage.ErrUnsupportedNativeSubAgent)
	}
	contract := opts.Contract
	if len(contract.Providers) == 0 {
		contract = runtimecap.DefaultContract()
	}
	if err := contract.RequireProviderCapability(adapterID, runtimecap.ProviderNestedSubagents); err != nil {
		result.Status = NestedStatusNeedsHuman
		result.Error = fmt.Sprintf("%v: %v", storage.ErrUnsupportedNativeSubAgent, err)
		return withNestedDecision(result), fmt.Errorf("%w: %v", storage.ErrUnsupportedNativeSubAgent, err)
	}
	if opts.Adapter == nil {
		result.Status = NestedStatusNeedsHuman
		result.Error = fmt.Sprintf("%v: native child adapter %q is not configured", storage.ErrUnsupportedNativeSubAgent, adapterID)
		return withNestedDecision(result), fmt.Errorf("%w: native child adapter %q is not configured", storage.ErrUnsupportedNativeSubAgent, adapterID)
	}
	clock := opts.Clock
	if clock == nil && opts.Store != nil {
		clock = opts.Store.Now
	}
	if clock == nil {
		result.Status = NestedStatusNeedsHuman
		result.Error = fmt.Sprintf("%v: injected clock is required for native child launch", storage.ErrAgentRegistrationRequired)
		return withNestedDecision(result), fmt.Errorf("%w: injected clock is required for native child launch", storage.ErrAgentRegistrationRequired)
	}
	lease := opts.Lease
	if lease <= 0 {
		lease = nestedClaimLeaseDuration
	}
	now := clock().UTC()
	executorID := nestedExecutorID(firstNonEmptyChild(opts.ParentRunID, opts.Registration.ParentRunID))
	regReq := opts.Registration
	regReq.AdapterID = adapterID
	regReq.ParentRunID = firstNonEmptyChild(opts.ParentRunID, regReq.ParentRunID)
	regReq.ChildRunID = child.RunID
	regReq.ChildKey = child.ChildKey
	regReq.Permission = firstNonEmptyChild(regReq.Permission, child.Permission)
	regReq.Now = now
	claim, registration, err := storage.ClaimAndRegisterNativeChild(ctx, opts.Store, regReq.ParentRunID, child.RunID, executorID, now, now.Add(lease), regReq)
	if err != nil {
		result.Status = NestedStatusNeedsHuman
		result.Error = err.Error()
		return withNestedDecision(result), err
	}
	result = applyClaimResult(result, claim)
	result = applyAgentRegistrationResult(result, registration)
	if claim.Outcome != storage.ClaimOutcomeClaimed && claim.Outcome != storage.ClaimOutcomeStaleClaim {
		return result, nil
	}
	if _, err := storage.RequireActiveAgentRegistrationForLaunch(ctx, opts.Store, child.RunID, claim.ExecutorID, claim.ClaimGeneration); err != nil {
		result.Status = NestedStatusNeedsHuman
		result.Error = err.Error()
		return withNestedDecision(result), err
	}
	phaseAt := clock().UTC()
	if err := storage.UpdateChildRunClaimPhase(ctx, opts.Store, regReq.ParentRunID, child.RunID, claim.ExecutorID, claim.ClaimGeneration, storage.ClaimPhaseExecuting, state.FormatTimestamp(phaseAt), ""); err != nil {
		result.Status = NestedStatusNeedsHuman
		result.Error = err.Error()
		return withNestedDecision(result), err
	}
	result.ClaimPhase = storage.ClaimPhaseExecuting
	if result.StartedAt == "" {
		result.StartedAt = state.FormatTimestamp(phaseAt)
	}

	output, err := opts.Adapter.StartNativeChild(ctx, registration, child)
	envelope, envelopeErr := NormalizeNativeChildOutput(output)
	if envelopeErr != nil {
		result.Status = NestedStatusNeedsHuman
		result.Error = envelopeErr.Error()
		return withNestedDecision(result), envelopeErr
	}
	result.Status = normalizeNestedStatus(output.Status)
	if result.Status == "" {
		result.Status = NestedStatusFailed
	}
	result.ProviderReceipt = strings.TrimSpace(output.ProviderReceipt)
	if result.ProviderReceipt == "" {
		result.ProviderReceipt = registration.ProviderSessionRef
	}
	if envelope.Summary != "" {
		result.Reason = envelope.Summary
	}
	if err != nil {
		result.Status = normalizeNestedStatus(state.FailureStatus(err))
		result.Error = err.Error()
	}
	if result.FinishedAt == "" {
		result.FinishedAt = state.FormatTimestamp(clock().UTC())
	}
	completeCtx, cancel := nestedCleanupContext()
	completeErr := storage.CompleteClaimedChildRun(completeCtx, opts.Store, regReq.ParentRunID, child.RunID, claim.ExecutorID, claim.ClaimGeneration, result.Status, result.FinishedAt, "native provider child finished", result.ProviderReceipt)
	cancel()
	if completeErr != nil {
		result.Status = NestedStatusNeedsHuman
		result.Error = completeErr.Error()
		return withNestedDecision(result), completeErr
	}
	return withNestedDecision(result), err
}

func NormalizeNativeChildOutput(output NativeChildOutput) (NativeChildResultEnvelope, error) {
	status := normalizeNestedStatus(output.Status)
	if status == "" {
		return NativeChildResultEnvelope{}, fmt.Errorf("native child output status is required")
	}
	summary := reporter.BoundDecisionText(strings.TrimSpace(output.Summary))
	raw, truncated := boundNativeChildOutput(redactNativeChildOutput(output.RawOutput))
	envelope := NativeChildResultEnvelope{
		SchemaVersion: NativeChildResultEnvelopeSchemaV1,
		Status:        status,
		Summary:       summary,
		RawOutput:     raw,
		Truncated:     truncated,
		Trusted:       false,
		Accepted:      false,
	}
	if _, err := json.Marshal(envelope); err != nil {
		return NativeChildResultEnvelope{}, fmt.Errorf("schema-validate native child output envelope: %w", err)
	}
	return envelope, nil
}

func boundNativeChildOutput(value string) (string, bool) {
	if len(value) <= maxNativeChildRawOutputBytes {
		return value, false
	}
	return value[:maxNativeChildRawOutputBytes] + "\n[loopcoder] native child output truncated", true
}

func redactNativeChildOutput(value string) string {
	replacements := []struct {
		from string
		to   string
	}{
		{"Authorization:", "Authorization: [redacted]"},
		{"Bearer ", "Bearer [redacted] "},
		{"api_key=", "api_key=[redacted]"},
		{"access_key=", "access_key=[redacted]"},
	}
	for _, replacement := range replacements {
		value = strings.ReplaceAll(value, replacement.from, replacement.to)
	}
	return value
}
