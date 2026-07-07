package guardrails

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/state"
)

func TestEvaluateBudgetDisabledIsNoOp(t *testing.T) {
	repo := t.TempDir()
	decision := EvaluateBudget(BudgetOptions{
		RepoPath: repo,
		RunID:    "run-test",
		Issue:    103,
	})
	if !decision.Allowed || decision.Enabled {
		t.Fatalf("decision = %#v, want allowed disabled no-op", decision)
	}
	path, err := RecordDecision(repo, decision)
	if err != nil {
		t.Fatalf("RecordDecision returned error: %v", err)
	}
	if path != "" {
		t.Fatalf("RecordDecision path = %q, want empty for disabled budget", path)
	}
}

func TestEvaluateBudgetBlocksWhenAttemptCapWouldBeExceeded(t *testing.T) {
	repo := t.TempDir()
	writeAllowedLedger(t, repo, "run-old", 103, "main:103", []int{103})
	writeAttempt(t, repo, "run-old", 103, "job-103-1", 1, usageTotalOnly(20), nil)

	maxAttempts := 1
	decision := EvaluateBudget(BudgetOptions{
		RepoPath:         repo,
		RunID:            "run-new",
		BaseBranch:       "main",
		Issue:            103,
		ScopeIssues:      []int{103},
		Budget:           config.GuardrailBudget{MaxTotalAttempts: &maxAttempts},
		ProposedAttempts: 1,
		Now:              fixedBudgetTime(),
	})

	if decision.Allowed || decision.Status != StatusNeedsHuman {
		t.Fatalf("decision = %#v, want needs-human block", decision)
	}
	for _, want := range []string{
		"guardrails.budget.max_total_attempts",
		"observed_attempts=1",
		"proposed_increment=1",
		"issue=#103",
		"run_id=run-new",
	} {
		if !strings.Contains(decision.Message, want) {
			t.Fatalf("message missing %q:\n%s", want, decision.Message)
		}
	}

	path, err := RecordDecision(repo, decision)
	if err != nil {
		t.Fatalf("RecordDecision returned error: %v", err)
	}
	if path != filepath.Join(repo, ".loopcoder", "runs", "run-new", "guardrails", "103.json") {
		t.Fatalf("ledger path = %q", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile ledger: %v", err)
	}
	for _, want := range []string{
		`"status": "needs-human"`,
		`"reason": "guardrails.budget.max_total_attempts"`,
		`"delivery_scope_id": "main:103"`,
	} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("ledger missing %q:\n%s", want, string(data))
		}
	}
}

func TestEvaluateBudgetUsesReportTokenUsage(t *testing.T) {
	repo := t.TempDir()
	input := int64(7)
	output := int64(8)
	writeAttempt(t, repo, "run-test", 103, "job-103-1", 1, &reporter.Usage{
		InputTokens:  &input,
		OutputTokens: &output,
	}, nil)

	maxTokens := int64(15)
	decision := EvaluateBudget(BudgetOptions{
		RepoPath: repo,
		RunID:    "run-test",
		Issue:    103,
		Budget:   config.GuardrailBudget{MaxTotalTokens: &maxTokens},
		Now:      fixedBudgetTime(),
	})

	if decision.Allowed || decision.Reason != "guardrails.budget.max_total_tokens" {
		t.Fatalf("decision = %#v, want token budget block", decision)
	}
	if decision.Observed.TotalTokens != 15 {
		t.Fatalf("TotalTokens = %d, want 15", decision.Observed.TotalTokens)
	}
	if !strings.Contains(decision.Message, "observed_tokens=15") {
		t.Fatalf("message missing observed tokens:\n%s", decision.Message)
	}
}

func TestEvaluateBudgetCostCapRequiresExactEvidence(t *testing.T) {
	repo := t.TempDir()
	writeAttempt(t, repo, "run-test", 103, "job-103-1", 1, nil, nil)

	maxCost := 1.0
	decision := EvaluateBudget(BudgetOptions{
		RepoPath: repo,
		RunID:    "run-test",
		Issue:    103,
		Budget:   config.GuardrailBudget{MaxTotalCostUSD: &maxCost},
		Now:      fixedBudgetTime(),
	})

	if decision.Allowed || decision.Reason != "guardrails.budget.max_total_cost_usd.unavailable-evidence" {
		t.Fatalf("decision = %#v, want unavailable cost evidence block", decision)
	}
	if !strings.Contains(decision.Message, "exact cost evidence unavailable") {
		t.Fatalf("message missing unavailable cost evidence:\n%s", decision.Message)
	}
}

func TestEvaluateBudgetCostCapUsesExactEvidence(t *testing.T) {
	repo := t.TempDir()
	costUSD := 1.25
	writeAttempt(t, repo, "run-test", 103, "job-103-1", 1, nil, &costUSD)

	maxCost := 1.0
	decision := EvaluateBudget(BudgetOptions{
		RepoPath: repo,
		RunID:    "run-test",
		Issue:    103,
		Budget:   config.GuardrailBudget{MaxTotalCostUSD: &maxCost},
		Now:      fixedBudgetTime(),
	})

	if decision.Allowed || decision.Reason != "guardrails.budget.max_total_cost_usd" {
		t.Fatalf("decision = %#v, want exact cost cap block", decision)
	}
	if decision.Observed.TotalCostUSD == nil || *decision.Observed.TotalCostUSD != 1.25 {
		t.Fatalf("TotalCostUSD = %#v, want 1.25", decision.Observed.TotalCostUSD)
	}
	if !strings.Contains(decision.Message, "observed_cost_usd=1.25") {
		t.Fatalf("message missing exact cost:\n%s", decision.Message)
	}
}

func TestEvaluateCircuitBreakerTreatsHeartbeatAndLogGrowthAsNoProgress(t *testing.T) {
	repo := t.TempDir()
	_, err := state.WriteAttempt(repo, "run-test", state.AttemptRecord{
		Version:        1,
		JobID:          "job-103-1",
		Issue:          103,
		Attempt:        1,
		Provider:       "codex",
		PID:            1234,
		Phase:          "codex_started",
		Status:         "running",
		Branch:         "loop/issue-103",
		StartedAt:      state.FormatTimestamp(fixedBudgetTime()),
		HeartbeatAt:    state.FormatTimestamp(fixedBudgetTime().Add(1 * time.Minute)),
		LastProgressAt: state.FormatTimestamp(fixedBudgetTime().Add(1 * time.Minute)),
		LogBytes:       2048,
	})
	if err != nil {
		t.Fatalf("WriteAttempt: %v", err)
	}

	maxAttempts := 1
	decision := EvaluateCircuitBreaker(CircuitOptions{
		RepoPath:   repo,
		RunID:      "run-test",
		BaseBranch: "main",
		Issue:      103,
		CircuitBreaker: config.GuardrailCircuitBreaker{
			MaxNoProgressAttempts: &maxAttempts,
		},
		Now: fixedBudgetTime().Add(2 * time.Minute),
	})

	if decision.Allowed || decision.Reason != "guardrails.circuit_breaker.max_no_progress_attempts" {
		t.Fatalf("decision = %#v, want circuit breaker block", decision)
	}
	if decision.Observed.NoProgressAttempts != 1 {
		t.Fatalf("NoProgressAttempts = %d, want 1", decision.Observed.NoProgressAttempts)
	}
	if strings.Contains(decision.Message, "material-progress") {
		t.Fatalf("heartbeat/log growth should not be material progress:\n%s", decision.Message)
	}
}

func writeAllowedLedger(t *testing.T, repo, runID string, issue int, scopeID string, issues []int) {
	t.Helper()
	_, err := RecordDecision(repo, Decision{
		Enabled:         true,
		Allowed:         true,
		Status:          StatusAllowed,
		DeliveryScopeID: scopeID,
		BaseBranch:      "main",
		RunID:           runID,
		Issue:           issue,
		Issues:          issues,
		DecisionAt:      fixedBudgetTime(),
	})
	if err != nil {
		t.Fatalf("RecordDecision allowed ledger: %v", err)
	}
}

func writeAttempt(t *testing.T, repo, runID string, issue int, jobID string, attemptNumber int, usage *reporter.Usage, costUSD *float64) {
	t.Helper()
	_, err := state.WriteAttempt(repo, runID, state.AttemptRecord{
		Version:        1,
		JobID:          jobID,
		Issue:          issue,
		Attempt:        attemptNumber,
		Provider:       "codex",
		PID:            1234,
		Phase:          "cleanup",
		Status:         "succeeded",
		Branch:         "loop/issue-103",
		StartedAt:      state.FormatTimestamp(fixedBudgetTime()),
		HeartbeatAt:    state.FormatTimestamp(fixedBudgetTime()),
		LastProgressAt: state.FormatTimestamp(fixedBudgetTime()),
		LogBytes:       12,
		Usage:          usage,
		CostUSD:        costUSD,
	})
	if err != nil {
		t.Fatalf("WriteAttempt: %v", err)
	}
}

func usageTotalOnly(tokens int64) *reporter.Usage {
	return &reporter.Usage{TotalTokens: &tokens}
}

func fixedBudgetTime() time.Time {
	return time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
}
