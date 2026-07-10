package reporter

import (
	"strings"
	"testing"
)

func TestNormalizeDecisionUsesBlockingFindingBeforeErrorAndBoundsText(t *testing.T) {
	long := "first line " + strings.Repeat("x", 300) + "\nsecond line"
	receipt := NormalizeDecision(DecisionInput{
		Status: "needs-human",
		Findings: []DecisionFinding{{
			Severity: "info",
			Message:  "positive evidence should not be selected",
		}, {
			Severity: "warning",
			File:     "docs/specs/design.md",
			Message:  long,
			Blocking: true,
		}},
		ConcreteError:      "fallback error",
		FallbackNextAction: "review the missing design doc",
	})

	if !strings.HasPrefix(receipt.Reason, "docs/specs/design.md: first line ") {
		t.Fatalf("reason = %q, want blocking finding", receipt.Reason)
	}
	if strings.Contains(receipt.Reason, "\n") || !strings.Contains(receipt.Reason, "[truncated]") {
		t.Fatalf("reason was not bounded to one truncated line: %q", receipt.Reason)
	}
	if receipt.NextAction != "review the missing design doc" {
		t.Fatalf("next_action = %q", receipt.NextAction)
	}
}
