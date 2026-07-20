package provideroutcome

import (
	"context"
	"errors"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/agent"
)

func TestClassifyTable(t *testing.T) {
	tests := []struct {
		name    string
		result  agent.Result
		err     error
		want    Class
		auto    bool
		human   bool
		trigger Trigger
	}{
		{
			name:    "structured quota",
			result:  agent.Result{FailureClass: string(ClassQuotaExhausted), ExitCode: 1},
			err:     errors.New("ignored text"),
			want:    ClassQuotaExhausted,
			auto:    true,
			human:   false,
			trigger: TriggerQuotaExhausted,
		},
		{
			name:    "hung deadline is timeout",
			result:  agent.Result{Hung: true, HungReason: agent.HungReasonDeadline},
			err:     errors.New("deadline"),
			want:    ClassLocalTimeout,
			auto:    true,
			human:   false,
			trigger: TriggerTimeout,
		},
		{
			name:    "context cancel is local cancellation",
			result:  agent.Result{ExitCode: -1},
			err:     context.Canceled,
			want:    ClassLocalCancellation,
			auto:    false,
			human:   true,
			trigger: TriggerWorkerFailed,
		},
		{
			name:    "provider call refused is permission",
			result:  agent.Result{},
			err:     agent.ProviderCallRefusedError{Err: errors.New("budget")},
			want:    ClassPermissionMismatch,
			auto:    false,
			human:   true,
			trigger: TriggerCandidateFailed,
		},
		{
			name:    "nonzero exit is provider rejection",
			result:  agent.Result{ExitCode: 2},
			err:     errors.New("exit 2"),
			want:    ClassProviderRejection,
			auto:    true,
			human:   false,
			trigger: TriggerWorkerFailed,
		},
		{
			name:    "ambiguous incomplete termination",
			result:  agent.Result{ExitCode: -1, StartedAt: "2026-07-17T00:00:00Z"},
			err:     errors.New("lost"),
			want:    ClassAmbiguousExecution,
			auto:    false,
			human:   true,
			trigger: TriggerWorkerFailed,
		},
		{
			name:    "clean success is terminal product",
			result:  agent.Result{ExitCode: 0},
			err:     nil,
			want:    ClassTerminalProduct,
			auto:    false,
			human:   false,
			trigger: TriggerWorkerFailed,
		},
		{
			name:    "auth structured needs human",
			result:  agent.Result{FailureClass: string(ClassAuthConfigFailure)},
			err:     errors.New("login"),
			want:    ClassAuthConfigFailure,
			auto:    false,
			human:   true,
			trigger: TriggerAuthExpired,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.result, tt.err)
			if got != tt.want {
				t.Fatalf("Classify = %q, want %q", got, tt.want)
			}
			if AllowsAutomaticFallback(got) != tt.auto {
				t.Fatalf("AllowsAutomaticFallback = %v, want %v", AllowsAutomaticFallback(got), tt.auto)
			}
			if NeedsHuman(got) != tt.human {
				t.Fatalf("NeedsHuman = %v, want %v", NeedsHuman(got), tt.human)
			}
			if FallbackTrigger(got) != tt.trigger {
				t.Fatalf("FallbackTrigger = %q, want %q", FallbackTrigger(got), tt.trigger)
			}
		})
	}
}

func TestClassifyIgnoresErrorStringContent(t *testing.T) {
	// Same structured fields must classify identically regardless of message text.
	a := Classify(agent.Result{ExitCode: 1}, errors.New("quota exhausted please upgrade"))
	b := Classify(agent.Result{ExitCode: 1}, errors.New("totally different wording"))
	if a != b || a != ClassProviderRejection {
		t.Fatalf("classification drifted by error text: %q vs %q", a, b)
	}
}
