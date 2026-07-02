package equivalence

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

const (
	GateReportVersion  = 1
	ParallelRunVersion = 1

	StageEquivalence = "equivalence"

	StatusPass       = "pass"
	StatusNeedsHuman = "needs-human"
	StatusBlocked    = "blocked"

	TargetOld = "old"
	TargetNew = "new"

	TargetOriginal  = TargetOld
	TargetCandidate = TargetNew
)

type Executor interface {
	Execute(ctx context.Context, request ExecutionRequest) (Observation, error)
}

type ExecutionRequest struct {
	Target string `json:"target"`
	Case   Case   `json:"case"`
}

type Observation struct {
	Value any `json:"value,omitempty"`
}

type Case struct {
	ID     string `json:"id" yaml:"id"`
	Input  any    `json:"input,omitempty" yaml:"input,omitempty"`
	Golden any    `json:"golden,omitempty" yaml:"golden,omitempty"`
}

type GateOptions struct {
	Contract   Contract
	SliceID    string
	Cases      []Case
	Executor   Executor
	Rebaseline *RebaselineRequest
}

type GateReport struct {
	Version    int                 `json:"version"`
	Stage      string              `json:"stage"`
	SliceID    string              `json:"slice_id"`
	SliceMode  string              `json:"slice_mode"`
	Status     string              `json:"status"`
	Detail     string              `json:"detail,omitempty"`
	Rebaseline *RebaselineDecision `json:"rebaseline,omitempty"`
	Cases      []CaseReport        `json:"cases"`
	Summary    GateSummary         `json:"summary"`
}

type GateSummary struct {
	PassCount       int `json:"pass_count"`
	NeedsHumanCount int `json:"needs_human_count"`
	BlockedCount    int `json:"blocked_count"`
}

type CaseReport struct {
	ID                     string      `json:"id"`
	Status                 string      `json:"status"`
	Detail                 string      `json:"detail,omitempty"`
	NoiseBaseline          Comparison  `json:"noise_baseline"`
	GoldenComparison       *Comparison `json:"golden_comparison,omitempty"`
	DifferentialComparison Comparison  `json:"differential_comparison"`
	OriginalRuns           int         `json:"original_runs"`
	CandidateRuns          int         `json:"candidate_runs"`
}

func RunGate(ctx context.Context, opts GateOptions) (GateReport, error) {
	report := GateReport{
		Version: GateReportVersion,
		Stage:   StageEquivalence,
		SliceID: strings.TrimSpace(opts.SliceID),
		Status:  StatusPass,
		Cases:   []CaseReport{},
	}
	if err := opts.Contract.Validate(); err != nil {
		return GateReport{}, err
	}
	if opts.Rebaseline != nil {
		decision := AuthorizeRebaseline(opts.Contract, *opts.Rebaseline)
		report.Rebaseline = &decision
		if !decision.Allowed {
			report.Status = StatusBlocked
			report.Detail = decision.Detail
			report.Summary.BlockedCount = 1
			return report, nil
		}
	}
	mode, err := opts.Contract.SliceMode(opts.SliceID)
	if err != nil {
		return GateReport{}, err
	}
	report.SliceMode = mode
	if mode == SliceModeSideEffect {
		report.Status = StatusNeedsHuman
		report.Detail = "slice is declared side_effect; live dual-execution is disabled by the equivalence contract"
		report.Summary.NeedsHumanCount = 1
		return report, nil
	}
	if opts.Executor == nil {
		return GateReport{}, fmt.Errorf("equivalence executor is required")
	}
	cases := normalizeCases(opts.Cases)
	if len(cases) == 0 {
		return GateReport{}, fmt.Errorf("at least one equivalence case is required")
	}

	for _, tc := range cases {
		caseReport := runCase(ctx, opts.Contract, opts.Executor, tc)
		report.Cases = append(report.Cases, caseReport)
		switch caseReport.Status {
		case StatusPass:
			report.Summary.PassCount++
		case StatusNeedsHuman:
			report.Summary.NeedsHumanCount++
			report.Status = StatusNeedsHuman
		case StatusBlocked:
			report.Summary.BlockedCount++
			report.Status = StatusBlocked
		}
	}
	return report, nil
}

func runCase(ctx context.Context, contract Contract, executor Executor, tc Case) CaseReport {
	report := CaseReport{
		ID:            tc.ID,
		Status:        StatusPass,
		OriginalRuns:  0,
		CandidateRuns: 0,
	}
	oldFirst, err := executor.Execute(ctx, ExecutionRequest{Target: TargetOriginal, Case: tc})
	report.OriginalRuns++
	if err != nil {
		report.Status = StatusNeedsHuman
		report.Detail = "original execution failed: " + err.Error()
		return report
	}
	oldSecond, err := executor.Execute(ctx, ExecutionRequest{Target: TargetOriginal, Case: tc})
	report.OriginalRuns++
	if err != nil {
		report.Status = StatusNeedsHuman
		report.Detail = "original noise-baseline execution failed: " + err.Error()
		return report
	}
	report.NoiseBaseline = Compare(contract, oldFirst.Value, oldSecond.Value, nil)
	noise := NoiseFloorFrom(report.NoiseBaseline)

	golden := oldFirst.Value
	if tc.Golden != nil {
		golden = tc.Golden
		goldenComparison := Compare(contract, tc.Golden, oldFirst.Value, noise)
		report.GoldenComparison = &goldenComparison
		if !goldenComparison.WithinTolerance {
			report.Status = StatusNeedsHuman
			report.Detail = "golden master drift exceeds contract tolerance and noise baseline"
			return report
		}
	}

	newRun, err := executor.Execute(ctx, ExecutionRequest{Target: TargetCandidate, Case: tc})
	report.CandidateRuns++
	if err != nil {
		report.Status = StatusNeedsHuman
		report.Detail = "candidate execution failed: " + err.Error()
		return report
	}
	report.DifferentialComparison = Compare(contract, golden, newRun.Value, noise)
	if !report.DifferentialComparison.WithinTolerance {
		report.Status = StatusNeedsHuman
		report.Detail = "candidate behavior differs outside contract tolerance and noise baseline"
	}
	return report
}

type ParallelRunInput struct {
	Old []ParallelRunRecord `json:"old"`
	New []ParallelRunRecord `json:"new"`
}

type ParallelRunRecord struct {
	Key   string `json:"key"`
	Value any    `json:"value,omitempty"`
}

type ParallelRunReport struct {
	Version       int                    `json:"version"`
	Status        string                 `json:"status"`
	MatchedCount  int                    `json:"matched_count"`
	OldOnlyCount  int                    `json:"old_only_count"`
	NewOnlyCount  int                    `json:"new_only_count"`
	MismatchCount int                    `json:"mismatch_count"`
	Matched       []ParallelRunMatch     `json:"matched"`
	Unmatched     []ParallelRunUnmatched `json:"unmatched"`
}

type ParallelRunMatch struct {
	Key        string     `json:"key"`
	Status     string     `json:"status"`
	Comparison Comparison `json:"comparison"`
}

type ParallelRunUnmatched struct {
	Key    string `json:"key"`
	Side   string `json:"side"`
	Detail string `json:"detail"`
}

func ReconcileParallelRun(contract Contract, input ParallelRunInput) (ParallelRunReport, error) {
	if err := contract.Validate(); err != nil {
		return ParallelRunReport{}, err
	}
	report := ParallelRunReport{
		Version:   ParallelRunVersion,
		Status:    StatusPass,
		Matched:   []ParallelRunMatch{},
		Unmatched: []ParallelRunUnmatched{},
	}
	oldByKey := recordsByKey(input.Old)
	newByKey := recordsByKey(input.New)
	for _, key := range sortedRecordKeys(oldByKey, newByKey) {
		oldRecord, oldOK := oldByKey[key]
		newRecord, newOK := newByKey[key]
		switch {
		case oldOK && newOK:
			comparison := Compare(contract, oldRecord.Value, newRecord.Value, nil)
			status := StatusPass
			if !comparison.WithinTolerance {
				status = StatusNeedsHuman
				report.Status = StatusNeedsHuman
				report.MismatchCount++
			}
			report.Matched = append(report.Matched, ParallelRunMatch{
				Key:        key,
				Status:     status,
				Comparison: comparison,
			})
			report.MatchedCount++
		case oldOK:
			report.Unmatched = append(report.Unmatched, ParallelRunUnmatched{
				Key:    key,
				Side:   "old",
				Detail: "record was emitted only by the old implementation",
			})
			report.OldOnlyCount++
			report.Status = StatusNeedsHuman
		case newOK:
			report.Unmatched = append(report.Unmatched, ParallelRunUnmatched{
				Key:    key,
				Side:   "new",
				Detail: "record was emitted only by the new implementation",
			})
			report.NewOnlyCount++
			report.Status = StatusNeedsHuman
		}
	}
	return report, nil
}

func normalizeCases(cases []Case) []Case {
	out := make([]Case, 0, len(cases))
	seen := map[string]bool{}
	for _, tc := range cases {
		tc.ID = strings.TrimSpace(tc.ID)
		if tc.ID == "" || seen[tc.ID] {
			continue
		}
		seen[tc.ID] = true
		tc.Input = normalizeValue(tc.Input)
		tc.Golden = normalizeValue(tc.Golden)
		out = append(out, tc)
	}
	return out
}

func recordsByKey(records []ParallelRunRecord) map[string]ParallelRunRecord {
	out := map[string]ParallelRunRecord{}
	for _, record := range records {
		key := strings.TrimSpace(record.Key)
		if key == "" {
			continue
		}
		record.Key = key
		record.Value = normalizeValue(record.Value)
		out[key] = record
	}
	return out
}

func sortedRecordKeys(left, right map[string]ParallelRunRecord) []string {
	seen := map[string]bool{}
	for key := range left {
		seen[key] = true
	}
	for key := range right {
		seen[key] = true
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
