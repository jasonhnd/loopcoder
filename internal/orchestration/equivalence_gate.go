package orchestration

import (
	"context"
	"fmt"

	"github.com/jasonhnd/loopcoder/internal/equivalence"
)

type EquivalenceGateFunc func(ctx context.Context, opts EquivalenceGateOptions) (equivalence.GateReport, error)

type EquivalenceGateOptions struct {
	Contract   equivalence.Contract
	SliceID    string
	Cases      []equivalence.Case
	Executor   equivalence.Executor
	Rebaseline *equivalence.RebaselineRequest
}

func EvaluateEquivalenceGate(ctx context.Context, opts EquivalenceGateOptions) (equivalence.GateReport, error) {
	if opts.Executor == nil {
		return equivalence.GateReport{}, fmt.Errorf("equivalence executor is required")
	}
	return equivalence.RunGate(ctx, equivalence.GateOptions{
		Contract:   opts.Contract,
		SliceID:    opts.SliceID,
		Cases:      opts.Cases,
		Executor:   opts.Executor,
		Rebaseline: opts.Rebaseline,
	})
}
