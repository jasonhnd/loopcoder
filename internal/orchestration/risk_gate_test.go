package orchestration

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestEvaluateRiskGateCorePathNeedsHuman(t *testing.T) {
	decision, err := EvaluateRiskGate(context.Background(), RiskGateOptions{
		Reader:         cleanRiskReader(353, "internal/agent/agent.go"),
		PRNumber:       353,
		RequiredChecks: []string{"verify"},
	})
	if err != nil {
		t.Fatalf("EvaluateRiskGate returned error: %v", err)
	}
	if decision.Status != RiskGateStatusNeedsHuman {
		t.Fatalf("status = %q, want %q", decision.Status, RiskGateStatusNeedsHuman)
	}
	if len(decision.RedLines) != 1 || decision.RedLines[0].Category != RiskRedLineCore {
		t.Fatalf("red lines = %#v, want one loopcoder core red line", decision.RedLines)
	}
	detail := decision.RedLines[0].Detail
	for _, want := range []string{"internal/agent/agent.go", "human rebuild", "tick restart"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("core red line detail %q missing %q", detail, want)
		}
	}
}

func TestEvaluateRiskGateNonCorePathStaysClean(t *testing.T) {
	decision, err := EvaluateRiskGate(context.Background(), RiskGateOptions{
		Reader:         cleanRiskReader(354, "README.md"),
		PRNumber:       354,
		RequiredChecks: []string{"verify"},
	})
	if err != nil {
		t.Fatalf("EvaluateRiskGate returned error: %v", err)
	}
	if decision.Status != RiskGateStatusClean || len(decision.RedLines) != 0 {
		t.Fatalf("decision = %#v, want clean non-core path", decision)
	}
}

func TestLoopcoderCorePathsCoverSelfHostingGuardSurface(t *testing.T) {
	corePaths := []string{
		".delivery.yml",
		"AGENTS.md",
		"GEMINI.md",
		"SKILL.md",
		"cmd/loopcoder/main.go",
		"hooks/conductor-attest",
		"internal/agent/agent.go",
		"internal/attestation/attestation.go",
		"internal/compile/compile.go",
		"internal/config/config.go",
		"internal/conductorhooks/attest.go",
		"internal/guardrails/budget.go",
		"internal/loopreview/loopreview.go",
		"internal/orchestration/dispatch_wave.go",
		"internal/orchestration/risk_gate.go",
		"internal/orchestration/tick.go",
		"internal/vcs/github/github.go",
		"internal/verify/verify.go",
		"internal/worker/worker.go",
	}
	for _, path := range corePaths {
		t.Run(path, func(t *testing.T) {
			if !isLoopcoderCorePath(path) {
				t.Fatalf("isLoopcoderCorePath(%q) = false, want true", path)
			}
		})
	}

	nonCorePaths := []string{
		"README.md",
		"docs/specs/0161-autonomous-delivery-loop.md",
		"internal/report/report.go",
	}
	for _, path := range nonCorePaths {
		t.Run(path, func(t *testing.T) {
			if isLoopcoderCorePath(path) {
				t.Fatalf("isLoopcoderCorePath(%q) = true, want false", path)
			}
		})
	}
}

func TestRiskGateOptionsExposeNoCoreBypassSurface(t *testing.T) {
	allowedFields := map[string]bool{
		"Reader":             true,
		"PRNumber":           true,
		"RequiredChecks":     true,
		"AdditionalRedLines": true,
	}
	riskGateOptions := reflect.TypeOf(RiskGateOptions{})
	for i := 0; i < riskGateOptions.NumField(); i++ {
		field := riskGateOptions.Field(i)
		if !allowedFields[field.Name] {
			t.Fatalf("RiskGateOptions exposes %s; core red lines must not accept gate/config/status bypass inputs", field.Name)
		}
	}
}
