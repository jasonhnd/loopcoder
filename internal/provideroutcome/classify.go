// Package provideroutcome normalizes provider adapter outcomes into stable
// classes for orchestration. Classification uses structured agent fields and
// typed errors — never user-facing error-string substrings.
package provideroutcome

import (
	"context"
	"errors"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/agent"
)

// Class is a stable provider-outcome class at the adapter/worker boundary.
type Class string

const (
	ClassAuthConfigFailure  Class = "authentication_configuration_failure"
	ClassQuotaExhausted     Class = "quota_exhausted_known_reset"
	ClassQuotaUnknown       Class = "quota_usage_unknown"
	ClassModelUnavailable   Class = "model_unavailable_or_unsupported"
	ClassTransientTransport Class = "transient_transport_service_failure"
	ClassPermissionMismatch Class = "permission_capability_mismatch"
	ClassProviderRejection  Class = "provider_rejection"
	ClassLocalCancellation  Class = "local_cancellation"
	ClassLocalTimeout       Class = "local_timeout"
	ClassAmbiguousExecution Class = "ambiguous_execution_state"
	ClassTerminalProduct    Class = "terminal_product_verdict"
	ClassUnknown            Class = "unknown"
)

// Structured is optional structured failure evidence set by adapters.
// Orchestration must prefer these fields over error message text.
type Structured struct {
	Class      Class
	KnownReset bool
	Ambiguous  bool
}

// Classify maps a finished agent attempt into a stable class without reading
// user-facing error strings in orchestration policy code.
func Classify(result agent.Result, runErr error) Class {
	if result.FailureClass != "" {
		if c := Class(strings.TrimSpace(result.FailureClass)); validClass(c) {
			return c
		}
	}
	if result.Hung {
		switch strings.TrimSpace(result.HungReason) {
		case agent.HungReasonDeadline:
			return ClassLocalTimeout
		case agent.HungReasonStall:
			return ClassLocalTimeout
		default:
			return ClassAmbiguousExecution
		}
	}
	if runErr != nil {
		if errors.Is(runErr, context.Canceled) {
			return ClassLocalCancellation
		}
		var refused agent.ProviderCallRefusedError
		if errors.As(runErr, &refused) {
			return ClassPermissionMismatch
		}
		// Started process with incomplete termination evidence is fail-closed.
		if result.ExitCode < 0 && result.StartedAt != "" && result.EndedAt == "" {
			return ClassAmbiguousExecution
		}
	}
	if runErr == nil && result.ExitCode == 0 {
		return ClassTerminalProduct
	}
	if result.ExitCode != 0 {
		// Non-zero exit without structured class is a terminal provider rejection,
		// not an automatic transient retry signal.
		return ClassProviderRejection
	}
	if runErr != nil {
		return ClassUnknown
	}
	return ClassTerminalProduct
}

// AllowsAutomaticFallback reports whether policy may select a successor route
// without human intervention. Ambiguous execution never auto-falls back.
func AllowsAutomaticFallback(class Class) bool {
	switch class {
	case ClassQuotaExhausted, ClassModelUnavailable, ClassTransientTransport, ClassProviderRejection, ClassLocalTimeout:
		return true
	case ClassAuthConfigFailure, ClassQuotaUnknown, ClassPermissionMismatch, ClassLocalCancellation, ClassAmbiguousExecution, ClassTerminalProduct, ClassUnknown:
		return false
	default:
		return false
	}
}

// Trigger is the routing fallback trigger name (string form) for a class.
// Kept as plain string here to avoid an import cycle with package routing.
type Trigger string

const (
	TriggerQuotaExhausted  Trigger = "quota-exhausted"
	TriggerAuthExpired     Trigger = "auth-expired"
	TriggerModelRemoved    Trigger = "model-removed"
	TriggerTimeout         Trigger = "timeout"
	TriggerWorkerFailed    Trigger = "worker-failed"
	TriggerCandidateFailed Trigger = "candidate-failed"
)

// FallbackTrigger maps a class onto the routing fallback trigger vocabulary.
// Classes that must not auto-fallback still return a trigger for audit/needs-human.
func FallbackTrigger(class Class) Trigger {
	switch class {
	case ClassQuotaExhausted:
		return TriggerQuotaExhausted
	case ClassAuthConfigFailure:
		return TriggerAuthExpired
	case ClassModelUnavailable:
		return TriggerModelRemoved
	case ClassLocalTimeout:
		return TriggerTimeout
	case ClassTransientTransport, ClassProviderRejection, ClassUnknown:
		return TriggerWorkerFailed
	case ClassPermissionMismatch, ClassQuotaUnknown:
		return TriggerCandidateFailed
	case ClassAmbiguousExecution:
		return TriggerWorkerFailed
	case ClassLocalCancellation:
		return TriggerWorkerFailed
	default:
		return TriggerWorkerFailed
	}
}

// NeedsHuman reports whether the class requires human resolution before any
// successor provider launch.
func NeedsHuman(class Class) bool {
	switch class {
	case ClassAmbiguousExecution, ClassAuthConfigFailure, ClassQuotaUnknown, ClassPermissionMismatch, ClassLocalCancellation:
		return true
	default:
		return false
	}
}

func validClass(class Class) bool {
	switch class {
	case ClassAuthConfigFailure, ClassQuotaExhausted, ClassQuotaUnknown, ClassModelUnavailable,
		ClassTransientTransport, ClassPermissionMismatch, ClassProviderRejection,
		ClassLocalCancellation, ClassLocalTimeout, ClassAmbiguousExecution,
		ClassTerminalProduct, ClassUnknown:
		return true
	default:
		return false
	}
}
