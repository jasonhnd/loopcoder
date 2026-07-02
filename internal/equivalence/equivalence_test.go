package equivalence

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestParseContractReadsVersionedSnakeCaseSchema(t *testing.T) {
	contract, err := ParseContract([]byte(`
version: 1
partition:
  read_slices: [reader-api]
  side_effect_slices: [writer-api]
tolerance:
  float_precision:
    absolute: 0.01
    relative: 0.001
    paths: ["$.score"]
  null_mappings:
    - path: "$.name"
      old_value: null
      new_value: ""
  ordering_insensitive_paths: ["$.items"]
intentional_divergence_allowlist:
  - id: approved-null-name
    approved: true
    promotion_class: true
    paths: ["$.name"]
    reason: intentional API cleanup
unknown_future_field: tolerated
`))
	if err != nil {
		t.Fatalf("ParseContract returned error: %v", err)
	}
	if contract.Version != ContractVersion {
		t.Fatalf("Version = %d, want %d", contract.Version, ContractVersion)
	}
	if !reflect.DeepEqual(contract.Partition.ReadSlices, []string{"reader-api"}) {
		t.Fatalf("ReadSlices = %#v", contract.Partition.ReadSlices)
	}
	if !reflect.DeepEqual(contract.Partition.SideEffectSlices, []string{"writer-api"}) {
		t.Fatalf("SideEffectSlices = %#v", contract.Partition.SideEffectSlices)
	}
	if contract.Tolerance.FloatPrecision == nil || contract.Tolerance.FloatPrecision.Absolute != 0.01 {
		t.Fatalf("FloatPrecision = %#v", contract.Tolerance.FloatPrecision)
	}
	if len(contract.Tolerance.NullMappings) != 1 || contract.Tolerance.NullMappings[0].OldValue != nil {
		t.Fatalf("NullMappings = %#v", contract.Tolerance.NullMappings)
	}
	if len(contract.IntentionalDivergenceAllowlist) != 1 || !contract.IntentionalDivergenceAllowlist[0].PromotionClass {
		t.Fatalf("Allowlist = %#v", contract.IntentionalDivergenceAllowlist)
	}

	data, err := json.Marshal(contract)
	if err != nil {
		t.Fatalf("marshal contract: %v", err)
	}
	rendered := string(data)
	for _, want := range []string{`"read_slices"`, `"side_effect_slices"`, `"float_precision"`, `"intentional_divergence_allowlist"`} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("contract JSON %s missing %s", rendered, want)
		}
	}
	if strings.Contains(rendered, "ReadSlices") || strings.Contains(rendered, "SideEffectSlices") {
		t.Fatalf("contract JSON uses non-snake-case fields: %s", rendered)
	}
}

func TestRunGateWithinTolerancePasses(t *testing.T) {
	executor := newScriptedExecutor(map[string][]Observation{
		execKey(TargetOriginal, "case-1"): {
			{Value: map[string]any{"score": 1.0}},
			{Value: map[string]any{"score": 1.0}},
		},
		execKey(TargetCandidate, "case-1"): {
			{Value: map[string]any{"score": 1.05}},
		},
	})

	report, err := RunGate(context.Background(), GateOptions{
		Contract: testContract(0.1),
		SliceID:  "reader-api",
		Cases: []Case{{
			ID:     "case-1",
			Golden: map[string]any{"score": 1.0},
		}},
		Executor: executor,
	})
	if err != nil {
		t.Fatalf("RunGate returned error: %v", err)
	}
	if report.Status != StatusPass {
		t.Fatalf("status = %q, want %q; report=%#v", report.Status, StatusPass, report)
	}
	if len(report.Cases) != 1 || report.Cases[0].OriginalRuns != 2 || report.Cases[0].CandidateRuns != 1 {
		t.Fatalf("case report = %#v, want old/old/new execution", report.Cases)
	}
	if len(executor.calls) != 3 {
		t.Fatalf("calls = %d, want 3", len(executor.calls))
	}
}

func TestRunGateOutOfToleranceNeedsHuman(t *testing.T) {
	executor := newScriptedExecutor(map[string][]Observation{
		execKey(TargetOriginal, "case-1"): {
			{Value: map[string]any{"score": 1.0}},
			{Value: map[string]any{"score": 1.0}},
		},
		execKey(TargetCandidate, "case-1"): {
			{Value: map[string]any{"score": 1.25}},
		},
	})

	report, err := RunGate(context.Background(), GateOptions{
		Contract: testContract(0.1),
		SliceID:  "reader-api",
		Cases: []Case{{
			ID:     "case-1",
			Golden: map[string]any{"score": 1.0},
		}},
		Executor: executor,
	})
	if err != nil {
		t.Fatalf("RunGate returned error: %v", err)
	}
	if report.Status != StatusNeedsHuman {
		t.Fatalf("status = %q, want %q", report.Status, StatusNeedsHuman)
	}
	if len(report.Cases) != 1 || report.Cases[0].DifferentialComparison.WithinTolerance {
		t.Fatalf("case report = %#v, want out-of-tolerance differential", report.Cases)
	}
	if !strings.Contains(report.Cases[0].Detail, "outside contract tolerance") {
		t.Fatalf("detail = %q", report.Cases[0].Detail)
	}
}

func TestRunGateNoiseBaselineEstablishesToleranceFloor(t *testing.T) {
	executor := newScriptedExecutor(map[string][]Observation{
		execKey(TargetOriginal, "case-1"): {
			{Value: map[string]any{"score": 10.0}},
			{Value: map[string]any{"score": 10.5}},
		},
		execKey(TargetCandidate, "case-1"): {
			{Value: map[string]any{"score": 10.4}},
		},
	})

	report, err := RunGate(context.Background(), GateOptions{
		Contract: testContract(0),
		SliceID:  "reader-api",
		Cases:    []Case{{ID: "case-1"}},
		Executor: executor,
	})
	if err != nil {
		t.Fatalf("RunGate returned error: %v", err)
	}
	if report.Status != StatusPass {
		t.Fatalf("status = %q, want pass because candidate is inside original noise floor; report=%#v", report.Status, report)
	}
	if len(report.Cases) != 1 || report.Cases[0].NoiseBaseline.WithinTolerance {
		t.Fatalf("noise baseline = %#v, want observed original non-determinism", report.Cases)
	}
	if !report.Cases[0].DifferentialComparison.WithinTolerance {
		t.Fatalf("differential comparison = %#v, want within noise floor", report.Cases[0].DifferentialComparison)
	}
}

func TestRunGateBlocksWorkerRebaselineAttempt(t *testing.T) {
	executor := newScriptedExecutor(nil)
	request := RebaselineRequest{
		RequestedByRole: "worker",
		PromotionClass:  true,
		AllowlistID:     "approved-change",
	}
	report, err := RunGate(context.Background(), GateOptions{
		Contract:   testContract(0.1),
		SliceID:    "reader-api",
		Cases:      []Case{{ID: "case-1"}},
		Executor:   executor,
		Rebaseline: &request,
	})
	if err != nil {
		t.Fatalf("RunGate returned error: %v", err)
	}
	if report.Status != StatusBlocked {
		t.Fatalf("status = %q, want %q", report.Status, StatusBlocked)
	}
	if report.Rebaseline == nil || report.Rebaseline.Allowed {
		t.Fatalf("rebaseline decision = %#v, want blocked", report.Rebaseline)
	}
	if len(executor.calls) != 0 {
		t.Fatalf("executor calls = %d, want no execution after blocked re-baseline", len(executor.calls))
	}
}

func TestCompareUsesNullMappingAndOrderingRules(t *testing.T) {
	oldValue := map[string]any{
		"name":  nil,
		"items": []any{"a", "b"},
	}
	newValue := map[string]any{
		"name":  "",
		"items": []any{"b", "a"},
	}
	comparison := Compare(testContract(0), oldValue, newValue, nil)
	if !comparison.WithinTolerance {
		t.Fatalf("comparison = %#v, want null mapping and order-insensitive path to pass", comparison)
	}
}

func TestRunGateSideEffectSliceDoesNotLiveExecute(t *testing.T) {
	executor := newScriptedExecutor(nil)
	report, err := RunGate(context.Background(), GateOptions{
		Contract: testContract(0.1),
		SliceID:  "writer-api",
		Cases:    []Case{{ID: "case-1"}},
		Executor: executor,
	})
	if err != nil {
		t.Fatalf("RunGate returned error: %v", err)
	}
	if report.Status != StatusNeedsHuman {
		t.Fatalf("status = %q, want %q", report.Status, StatusNeedsHuman)
	}
	if len(executor.calls) != 0 {
		t.Fatalf("executor calls = %d, want no live dual-execution for side-effect slice", len(executor.calls))
	}
}

func TestReconcileParallelRunSurfacesMatchedAndUnmatchedReport(t *testing.T) {
	report, err := ReconcileParallelRun(testContract(0.1), ParallelRunInput{
		Old: []ParallelRunRecord{
			{Key: "request-1", Value: map[string]any{"score": 1.0}},
			{Key: "request-2", Value: map[string]any{"score": 2.0}},
		},
		New: []ParallelRunRecord{
			{Key: "request-1", Value: map[string]any{"score": 1.05}},
			{Key: "request-3", Value: map[string]any{"score": 3.0}},
		},
	})
	if err != nil {
		t.Fatalf("ReconcileParallelRun returned error: %v", err)
	}
	if report.Status != StatusNeedsHuman {
		t.Fatalf("status = %q, want needs-human for unmatched records", report.Status)
	}
	if report.MatchedCount != 1 || report.OldOnlyCount != 1 || report.NewOnlyCount != 1 || report.MismatchCount != 0 {
		t.Fatalf("parallel report counts = %#v", report)
	}
	if len(report.Matched) != 1 || report.Matched[0].Status != StatusPass {
		t.Fatalf("matched = %#v", report.Matched)
	}
	if len(report.Unmatched) != 2 {
		t.Fatalf("unmatched = %#v", report.Unmatched)
	}
}

type scriptedExecutor struct {
	outputs map[string][]Observation
	calls   []ExecutionRequest
}

func newScriptedExecutor(outputs map[string][]Observation) *scriptedExecutor {
	if outputs == nil {
		outputs = map[string][]Observation{}
	}
	return &scriptedExecutor{outputs: outputs}
}

func (e *scriptedExecutor) Execute(_ context.Context, request ExecutionRequest) (Observation, error) {
	e.calls = append(e.calls, request)
	key := execKey(request.Target, request.Case.ID)
	values := e.outputs[key]
	if len(values) == 0 {
		return Observation{}, nil
	}
	out := values[0]
	e.outputs[key] = values[1:]
	return out, nil
}

func execKey(target, id string) string {
	return target + "/" + id
}

func testContract(floatAbsolute float64) Contract {
	return Contract{
		Version: ContractVersion,
		Partition: Partition{
			ReadSlices:       []string{"reader-api"},
			SideEffectSlices: []string{"writer-api"},
		},
		Tolerance: ToleranceRules{
			FloatPrecision: &FloatPrecision{
				Absolute: floatAbsolute,
				Paths:    []string{"$.score"},
			},
			NullMappings: []NullMapping{{
				Path:     "$.name",
				OldValue: nil,
				NewValue: "",
			}},
			OrderingInsensitivePaths: []string{"$.items"},
		},
		IntentionalDivergenceAllowlist: []IntentionalDivergence{{
			ID:             "approved-change",
			Approved:       true,
			PromotionClass: true,
		}},
	}
}
