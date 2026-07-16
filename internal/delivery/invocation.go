package delivery

import (
	"strings"
)

const (
	InvocationContractSchema = "loopcoder.host_invocation_contract.v1"

	OperationPlan     = "delivery.plan"
	OperationDecide   = "delivery.decide"
	OperationContinue = "delivery.continue"

	PermissionReadOnly    = "read-only"
	PermissionWrite       = "write"
	PermissionOrchestrate = "orchestrate"

	CancellationNotApplicable     = "not-applicable"
	CancellationAtomicBeforeWrite = "atomic-before-write"
	CancellationDurableResumable  = "durable-resumable"

	OutcomeAccepted    = "accepted"
	OutcomeDeclined    = "declined"
	OutcomeStale       = "stale"
	OutcomeUnsupported = "unsupported"
	OutcomeInterrupted = "interrupted"
)

type HostOperationContract struct {
	SchemaVersion       string `json:"schema_version"`
	Operation           string `json:"operation"`
	SideEffectClass     string `json:"side_effect_class"`
	Permission          string `json:"permission"`
	ApprovalMutation    bool   `json:"approval_mutation"`
	IdempotencyRequired bool   `json:"idempotency_required"`
	Cancellation        string `json:"cancellation"`
	ProviderLaunch      bool   `json:"provider_launch"`
	MachineJSONOnly     bool   `json:"machine_json_only"`
}

type HostEnforcement struct {
	Provided          bool   `json:"provided"`
	HostProfile       string `json:"host_profile,omitempty"`
	Source            string `json:"source,omitempty"`
	SideEffectClasses bool   `json:"side_effect_classes"`
	Permissions       bool   `json:"permissions"`
	ApprovalMutation  bool   `json:"approval_mutation"`
	Idempotency       bool   `json:"idempotency"`
	Cancellation      bool   `json:"cancellation"`
	StableJSON        bool   `json:"stable_json"`
	Stdout            bool   `json:"stdout"`
	Stderr            bool   `json:"stderr"`
	UnsupportedReason string `json:"unsupported_reason,omitempty"`
}

type InvocationEvidence struct {
	Contract    HostOperationContract `json:"contract"`
	Enforcement HostEnforcement       `json:"enforcement,omitempty"`
	Fingerprint string                `json:"fingerprint,omitempty"`
}

func ContractForOperation(operation string) (HostOperationContract, error) {
	switch strings.TrimSpace(operation) {
	case OperationPlan:
		return HostOperationContract{
			SchemaVersion:       InvocationContractSchema,
			Operation:           OperationPlan,
			SideEffectClass:     "none",
			Permission:          PermissionReadOnly,
			ApprovalMutation:    false,
			IdempotencyRequired: false,
			Cancellation:        CancellationNotApplicable,
			ProviderLaunch:      false,
			MachineJSONOnly:     true,
		}, nil
	case OperationDecide:
		return HostOperationContract{
			SchemaVersion:       InvocationContractSchema,
			Operation:           OperationDecide,
			SideEffectClass:     "local-write",
			Permission:          PermissionOrchestrate,
			ApprovalMutation:    true,
			IdempotencyRequired: true,
			Cancellation:        CancellationAtomicBeforeWrite,
			ProviderLaunch:      false,
			MachineJSONOnly:     true,
		}, nil
	case OperationContinue:
		return HostOperationContract{
			SchemaVersion:       InvocationContractSchema,
			Operation:           OperationContinue,
			SideEffectClass:     "local-write",
			Permission:          PermissionOrchestrate,
			ApprovalMutation:    false,
			IdempotencyRequired: true,
			Cancellation:        CancellationDurableResumable,
			ProviderLaunch:      false,
			MachineJSONOnly:     true,
		}, nil
	default:
		return HostOperationContract{}, typed(ErrInvalidRecordCode, "unknown delivery operation %q", operation)
	}
}

func SupportedHostEnforcement(hostProfile, source string) HostEnforcement {
	return HostEnforcement{
		Provided:          true,
		HostProfile:       strings.TrimSpace(hostProfile),
		Source:            strings.TrimSpace(source),
		SideEffectClasses: true,
		Permissions:       true,
		ApprovalMutation:  true,
		Idempotency:       true,
		Cancellation:      true,
		StableJSON:        true,
		Stdout:            true,
		Stderr:            true,
	}
}

func UnsupportedHostEnforcement(reason string) HostEnforcement {
	enforcement := SupportedHostEnforcement("", "")
	enforcement.UnsupportedReason = strings.TrimSpace(reason)
	return enforcement
}

func enforceHostInvocation(operation string, enforcement HostEnforcement) (InvocationEvidence, error) {
	contract, err := ContractForOperation(operation)
	if err != nil {
		return InvocationEvidence{}, err
	}
	evidence := InvocationEvidence{Contract: contract, Enforcement: enforcement}
	if fingerprint, err := invocationFingerprint(contract, enforcement); err == nil {
		evidence.Fingerprint = fingerprint
	}
	if !enforcement.Provided {
		return evidence, nil
	}
	var missing []string
	if strings.TrimSpace(enforcement.UnsupportedReason) != "" {
		missing = append(missing, enforcement.UnsupportedReason)
	}
	if !enforcement.SideEffectClasses {
		missing = append(missing, "side-effect-class metadata")
	}
	if !enforcement.Permissions {
		missing = append(missing, "permission metadata")
	}
	if !enforcement.StableJSON {
		missing = append(missing, "stable JSON output")
	}
	if !enforcement.Stdout {
		missing = append(missing, "stdout preservation")
	}
	if !enforcement.Stderr {
		missing = append(missing, "stderr preservation")
	}
	if contract.ApprovalMutation && !enforcement.ApprovalMutation {
		missing = append(missing, "approval mutation enforcement")
	}
	if contract.IdempotencyRequired && !enforcement.Idempotency {
		missing = append(missing, "idempotency enforcement")
	}
	if contract.Cancellation == CancellationDurableResumable && !enforcement.Cancellation {
		missing = append(missing, "durable cancellation metadata")
	}
	if len(missing) > 0 {
		return evidence, typed(ErrUnsupportedHostCapabilityCode, "%s cannot be represented by host: %s", operation, strings.Join(missing, ", "))
	}
	return evidence, nil
}

func authorizationInvocationEvidence(enforcement HostEnforcement) (InvocationEvidence, error) {
	return enforceHostInvocation(OperationContinue, enforcement)
}

func invocationFingerprint(contract HostOperationContract, enforcement HostEnforcement) (string, error) {
	if !enforcement.Provided {
		return "", nil
	}
	digest, _, err := DigestCanonicalJSON(map[string]any{
		"schema_version": InvocationContractSchema,
		"contract":       contract,
		"enforcement": map[string]any{
			"host_profile":        enforcement.HostProfile,
			"source":              enforcement.Source,
			"side_effect_classes": enforcement.SideEffectClasses,
			"permissions":         enforcement.Permissions,
			"approval_mutation":   enforcement.ApprovalMutation,
			"idempotency":         enforcement.Idempotency,
			"cancellation":        enforcement.Cancellation,
			"stable_json":         enforcement.StableJSON,
			"stdout":              enforcement.Stdout,
			"stderr":              enforcement.Stderr,
		},
	})
	return digest, err
}
