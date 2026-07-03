package orchestration

import (
	"context"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/equivalence"
)

func TestEvaluateEquivalenceGateDelegatesDistinctStage(t *testing.T) {
	report, err := EvaluateEquivalenceGate(context.Background(), EquivalenceGateOptions{
		Contract: equivalence.Contract{
			Version: equivalence.ContractVersion,
			Partition: equivalence.Partition{
				ReadSlices: []string{"reader-api"},
			},
			Tolerance: equivalence.ToleranceRules{
				FloatPrecision: &equivalence.FloatPrecision{Absolute: 0.1, Paths: []string{"$.score"}},
			},
		},
		SliceID: "reader-api",
		Cases:   []equivalence.Case{{ID: "case-1"}},
		Executor: &orchestrationEquivalenceExecutor{
			observations: []equivalence.Observation{
				{Value: map[string]any{"score": 1.0}},
				{Value: map[string]any{"score": 1.0}},
				{Value: map[string]any{"score": 1.05}},
			},
		},
	})
	if err != nil {
		t.Fatalf("EvaluateEquivalenceGate returned error: %v", err)
	}
	if report.Stage != equivalence.StageEquivalence || report.Status != equivalence.StatusPass {
		t.Fatalf("report = %#v, want equivalence pass", report)
	}
}

type orchestrationEquivalenceExecutor struct {
	observations []equivalence.Observation
}

func (e *orchestrationEquivalenceExecutor) Execute(context.Context, equivalence.ExecutionRequest) (equivalence.Observation, error) {
	if len(e.observations) == 0 {
		return equivalence.Observation{}, nil
	}
	out := e.observations[0]
	e.observations = e.observations[1:]
	return out, nil
}
