package workflowrun

import (
	"testing"

	"github.com/jasonhnd/loopcoder/internal/agent"
	"github.com/jasonhnd/loopcoder/internal/reporter"
)

// Token soft-window estimates must never set ActualCapacity (quota-window unit).
func TestAttachUsage_TokensNeverSetQuotaActual(t *testing.T) {
	in := int64(10000)
	out := int64(5000)
	res := agent.Result{
		Usage: reporter.Usage{
			InputTokens:  &in,
			OutputTokens: &out,
		},
	}
	got := attachUsage(ChildExecResult{}, res)
	if got.InputTokens != in || got.OutputTokens != out {
		t.Fatalf("tokens not preserved: in=%d out=%d", got.InputTokens, got.OutputTokens)
	}
	if got.ActualCapacity != nil {
		t.Fatalf("ActualCapacity must be nil for token usage, got %v", *got.ActualCapacity)
	}
	if got.ActualSource != "unknown" {
		t.Fatalf("ActualSource=%q want unknown", got.ActualSource)
	}
}

func TestAttachUsage_EmptyTokensUnknown(t *testing.T) {
	got := attachUsage(ChildExecResult{}, agent.Result{})
	if got.ActualCapacity != nil || got.ActualSource != "unknown" {
		t.Fatalf("empty usage: capacity=%v source=%q", got.ActualCapacity, got.ActualSource)
	}
}
