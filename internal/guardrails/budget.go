// Package guardrails enforces opt-in delivery guardrails from .delivery.yml.
package guardrails

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/attestation"
	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/state"
)

const (
	StatusAllowed    = "allowed"
	StatusNeedsHuman = "needs-human"
)

type BudgetOptions struct {
	RepoPath         string
	RunID            string
	BaseBranch       string
	Issue            int
	ScopeIssues      []int
	Budget           config.GuardrailBudget
	PlannedAttempts  int
	ProposedAttempts int
	Now              time.Time
}

type Decision struct {
	Enabled             bool
	Allowed             bool
	Status              string
	Reason              string
	Message             string
	DeliveryScopeID     string
	BaseBranch          string
	Issue               int
	Issues              []int
	RunID               string
	RunIDs              []string
	Observed            Observed
	Cap                 *CapEvidence
	LatestAttemptPath   string
	RecoveryContextPath string
	DecisionAt          time.Time
}

type Observed struct {
	Runs          int      `json:"runs"`
	TotalAttempts int      `json:"total_attempts"`
	TotalTokens   int64    `json:"total_tokens"`
	TotalCostUSD  *float64 `json:"total_cost_usd,omitempty"`
}

type CapEvidence struct {
	Name              string `json:"name"`
	Limit             string `json:"limit"`
	Observed          string `json:"observed"`
	PlannedIncrement  int    `json:"planned_increment,omitempty"`
	ProposedIncrement int    `json:"proposed_increment,omitempty"`
}

type Ledger struct {
	Version             int          `json:"version"`
	DeliveryScopeID     string       `json:"delivery_scope_id"`
	BaseBranch          string       `json:"base_branch,omitempty"`
	RunID               string       `json:"run_id"`
	Issue               int          `json:"issue"`
	Issues              []int        `json:"issues,omitempty"`
	Status              string       `json:"status"`
	Reason              string       `json:"reason,omitempty"`
	Cap                 *CapEvidence `json:"cap,omitempty"`
	Observed            Observed     `json:"observed"`
	RunIDs              []string     `json:"run_ids,omitempty"`
	LatestAttemptPath   string       `json:"latest_attempt_path,omitempty"`
	RecoveryContextPath string       `json:"recovery_context_path,omitempty"`
	DecisionAt          string       `json:"decision_at"`
}

type scopedAttempt struct {
	runID   string
	attempt state.Attempt
}

func EvaluateBudget(opts BudgetOptions) Decision {
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	baseBranch := strings.TrimSpace(opts.BaseBranch)
	if baseBranch == "" {
		baseBranch = "main"
	}
	proposedAttempts := opts.ProposedAttempts
	if proposedAttempts <= 0 {
		proposedAttempts = 1
	}

	decision := Decision{
		Enabled:    opts.Budget.Enabled(),
		Allowed:    true,
		Status:     StatusAllowed,
		BaseBranch: baseBranch,
		Issue:      opts.Issue,
		RunID:      strings.TrimSpace(opts.RunID),
		DecisionAt: now,
	}
	if !opts.Budget.Enabled() {
		return decision
	}

	ledgers, err := LoadLedgers(opts.RepoPath)
	if err != nil {
		return blockDecision(decision, "guardrails.budget.unavailable-evidence", fmt.Sprintf("read guardrail ledger: %v", err), nil)
	}
	scopeID, issues := resolveScope(baseBranch, opts.Issue, opts.ScopeIssues, ledgers)
	decision.DeliveryScopeID = scopeID
	decision.Issues = issues

	totals, err := collectTotals(opts.RepoPath, decision.RunID, scopeID, issues, ledgers)
	if err != nil {
		return blockDecision(decision, "guardrails.budget.unavailable-evidence", err.Error(), nil)
	}
	decision.Observed = totals.observed
	decision.RunIDs = totals.runIDs
	decision.LatestAttemptPath = totals.latestAttemptPath
	decision.RecoveryContextPath = totals.recoveryContextPath

	if opts.Budget.MaxRuns != nil {
		proposedRunIncrement := 0
		if proposedAttempts > 0 && !containsString(totals.workRunIDs, decision.RunID) {
			proposedRunIncrement = 1
		}
		if totals.observed.Runs+proposedRunIncrement > *opts.Budget.MaxRuns {
			cap := &CapEvidence{
				Name:              "guardrails.budget.max_runs",
				Limit:             strconv.Itoa(*opts.Budget.MaxRuns),
				Observed:          strconv.Itoa(totals.observed.Runs),
				ProposedIncrement: proposedRunIncrement,
			}
			return blockDecision(decision, cap.Name, "run budget would be exceeded", cap)
		}
	}
	if opts.Budget.MaxTotalAttempts != nil {
		currentAttempts := totals.observed.TotalAttempts + opts.PlannedAttempts
		if currentAttempts+proposedAttempts > *opts.Budget.MaxTotalAttempts {
			cap := &CapEvidence{
				Name:              "guardrails.budget.max_total_attempts",
				Limit:             strconv.Itoa(*opts.Budget.MaxTotalAttempts),
				Observed:          strconv.Itoa(totals.observed.TotalAttempts),
				PlannedIncrement:  opts.PlannedAttempts,
				ProposedIncrement: proposedAttempts,
			}
			return blockDecision(decision, cap.Name, "attempt budget would be exceeded", cap)
		}
	}
	if opts.Budget.MaxTotalTokens != nil {
		if totals.missingTokenEvidence != "" {
			cap := &CapEvidence{
				Name:     "guardrails.budget.max_total_tokens",
				Limit:    strconv.FormatInt(*opts.Budget.MaxTotalTokens, 10),
				Observed: strconv.FormatInt(totals.observed.TotalTokens, 10),
			}
			return blockDecision(decision, cap.Name+".unavailable-evidence", totals.missingTokenEvidence, cap)
		}
		if totals.observed.TotalTokens >= *opts.Budget.MaxTotalTokens {
			cap := &CapEvidence{
				Name:     "guardrails.budget.max_total_tokens",
				Limit:    strconv.FormatInt(*opts.Budget.MaxTotalTokens, 10),
				Observed: strconv.FormatInt(totals.observed.TotalTokens, 10),
			}
			return blockDecision(decision, cap.Name, "token budget has been reached", cap)
		}
	}
	if opts.Budget.MaxTotalCostUSD != nil {
		if totals.missingCostEvidence != "" {
			cap := &CapEvidence{
				Name:     "guardrails.budget.max_total_cost_usd",
				Limit:    formatUSD(*opts.Budget.MaxTotalCostUSD),
				Observed: formatUSDPtr(totals.observed.TotalCostUSD),
			}
			return blockDecision(decision, cap.Name+".unavailable-evidence", totals.missingCostEvidence, cap)
		}
		observedCost := 0.0
		if totals.observed.TotalCostUSD != nil {
			observedCost = *totals.observed.TotalCostUSD
		}
		if observedCost >= *opts.Budget.MaxTotalCostUSD {
			cap := &CapEvidence{
				Name:     "guardrails.budget.max_total_cost_usd",
				Limit:    formatUSD(*opts.Budget.MaxTotalCostUSD),
				Observed: formatUSD(observedCost),
			}
			return blockDecision(decision, cap.Name, "cost budget has been reached", cap)
		}
	}

	decision.Message = formatDecisionMessage(decision, "")
	return decision
}

func RecordDecision(repoPath string, decision Decision) (string, error) {
	if !decision.Enabled {
		return "", nil
	}
	path := LedgerPath(repoPath, decision.RunID, decision.Issue)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return path, fmt.Errorf("create guardrail ledger directory: %w", err)
	}
	data, err := json.MarshalIndent(Ledger{
		Version:             1,
		DeliveryScopeID:     decision.DeliveryScopeID,
		BaseBranch:          decision.BaseBranch,
		RunID:               decision.RunID,
		Issue:               decision.Issue,
		Issues:              append([]int(nil), decision.Issues...),
		Status:              decision.Status,
		Reason:              decision.Reason,
		Cap:                 decision.Cap,
		Observed:            decision.Observed,
		RunIDs:              append([]string(nil), decision.RunIDs...),
		LatestAttemptPath:   decision.LatestAttemptPath,
		RecoveryContextPath: decision.RecoveryContextPath,
		DecisionAt:          state.FormatTimestamp(decision.DecisionAt),
	}, "", "  ")
	if err != nil {
		return path, fmt.Errorf("marshal guardrail ledger: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return path, fmt.Errorf("write guardrail ledger: %w", err)
	}
	return path, nil
}

func LedgerPath(repoPath, runID string, issue int) string {
	return filepath.Join(state.RunPath(repoPath, runID), "guardrails", fmt.Sprintf("%d.json", issue))
}

func LoadLedgers(repoPath string) ([]Ledger, error) {
	runsRoot := state.RunsRoot(repoPath)
	entries, err := os.ReadDir(runsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read runs directory: %w", err)
	}

	var ledgers []Ledger
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(runsRoot, entry.Name(), "guardrails")
		guardrailEntries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read guardrails directory %s: %w", dir, err)
		}
		for _, guardrailEntry := range guardrailEntries {
			if guardrailEntry.IsDir() || !strings.HasSuffix(guardrailEntry.Name(), ".json") {
				continue
			}
			path := filepath.Join(dir, guardrailEntry.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("read guardrail ledger %s: %w", path, err)
			}
			var ledger Ledger
			if err := json.Unmarshal(data, &ledger); err != nil {
				return nil, fmt.Errorf("parse guardrail ledger %s: %w", path, err)
			}
			if ledger.RunID == "" {
				ledger.RunID = entry.Name()
			}
			ledgers = append(ledgers, ledger)
		}
	}
	sort.Slice(ledgers, func(i, j int) bool {
		if ledgers[i].DecisionAt != ledgers[j].DecisionAt {
			return ledgers[i].DecisionAt < ledgers[j].DecisionAt
		}
		if ledgers[i].RunID != ledgers[j].RunID {
			return ledgers[i].RunID < ledgers[j].RunID
		}
		return ledgers[i].Issue < ledgers[j].Issue
	})
	return ledgers, nil
}

type totals struct {
	observed             Observed
	runIDs               []string
	workRunIDs           []string
	latestAttemptPath    string
	recoveryContextPath  string
	missingTokenEvidence string
	missingCostEvidence  string
}

func collectTotals(repoPath, currentRunID, scopeID string, issues []int, ledgers []Ledger) (totals, error) {
	issueSet := map[int]bool{}
	for _, issue := range issues {
		issueSet[issue] = true
	}
	runSet := map[string]bool{}
	workRunSet := map[string]bool{}
	for _, ledger := range ledgers {
		if ledger.DeliveryScopeID != scopeID {
			continue
		}
		if strings.TrimSpace(ledger.RunID) != "" {
			runSet[ledger.RunID] = true
			if ledger.Status == StatusAllowed {
				workRunSet[ledger.RunID] = true
			}
		}
	}
	if strings.TrimSpace(currentRunID) != "" {
		runSet[currentRunID] = true
	}

	var scoped []scopedAttempt
	for runID := range runSet {
		attempts, err := loadAttemptsStrict(repoPath, runID)
		if err != nil {
			return totals{}, err
		}
		for _, attempt := range attempts {
			if !issueSet[attempt.Issue] {
				continue
			}
			scoped = append(scoped, scopedAttempt{runID: runID, attempt: attempt})
			workRunSet[runID] = true
		}
	}
	sort.Slice(scoped, func(i, j int) bool {
		if !scoped[i].attempt.LastWriteUTC.Equal(scoped[j].attempt.LastWriteUTC) {
			return scoped[i].attempt.LastWriteUTC.Before(scoped[j].attempt.LastWriteUTC)
		}
		if scoped[i].runID != scoped[j].runID {
			return scoped[i].runID < scoped[j].runID
		}
		return scoped[i].attempt.JobID < scoped[j].attempt.JobID
	})

	var totalTokens int64
	totalCost := 0.0
	anyCostEvidence := false
	var missingTokenEvidence []string
	var missingCostEvidence []string
	var latest *scopedAttempt
	for i := range scoped {
		item := scoped[i]
		latest = &item
		if tokens, ok := usageTotal(item.attempt.Usage); ok {
			totalTokens += tokens
		} else {
			missingTokenEvidence = append(missingTokenEvidence, fmt.Sprintf("%s/%s", item.runID, item.attempt.JobID))
		}
		if item.attempt.CostUSD == nil || *item.attempt.CostUSD < 0 {
			missingCostEvidence = append(missingCostEvidence, fmt.Sprintf("%s/%s", item.runID, item.attempt.JobID))
		} else {
			totalCost += *item.attempt.CostUSD
			anyCostEvidence = true
		}
	}

	runIDs := sortedKeys(runSet)
	workRunIDs := sortedKeys(workRunSet)
	observed := Observed{
		Runs:          len(workRunIDs),
		TotalAttempts: len(scoped),
		TotalTokens:   totalTokens,
	}
	if anyCostEvidence {
		observed.TotalCostUSD = &totalCost
	}

	out := totals{
		observed:   observed,
		runIDs:     runIDs,
		workRunIDs: workRunIDs,
	}
	if len(missingTokenEvidence) > 0 {
		out.missingTokenEvidence = "token evidence unavailable for attempts: " + strings.Join(missingTokenEvidence, ", ")
	}
	if len(missingCostEvidence) > 0 {
		out.missingCostEvidence = "exact cost evidence unavailable for attempts: " + strings.Join(missingCostEvidence, ", ")
	}
	if latest != nil {
		out.latestAttemptPath = latest.attempt.Path
		out.recoveryContextPath = recoveryContextPath(repoPath, latest.runID, latest.attempt)
	}
	return out, nil
}

func loadAttemptsStrict(repoPath, runID string) ([]state.Attempt, error) {
	attempts, err := state.LoadAttempts(repoPath, runID)
	if err != nil {
		return nil, err
	}
	workersDir := state.WorkersPath(repoPath, runID)
	entries, err := os.ReadDir(workersDir)
	if err != nil {
		if os.IsNotExist(err) {
			return attempts, nil
		}
		return nil, fmt.Errorf("read workers directory %s: %w", workersDir, err)
	}
	expected := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".attempt.json") {
			expected++
		}
	}
	if len(attempts) != expected {
		return nil, fmt.Errorf("attempt evidence corrupt or unreadable in %s", workersDir)
	}
	return attempts, nil
}

func resolveScope(baseBranch string, issue int, scopeIssues []int, ledgers []Ledger) (string, []int) {
	issues := normalizeIssues(scopeIssues)
	if len(issues) <= 1 {
		for i := len(ledgers) - 1; i >= 0; i-- {
			ledger := ledgers[i]
			if ledger.Issue != issue || !ledgerMatchesBase(ledger, baseBranch) || strings.TrimSpace(ledger.DeliveryScopeID) == "" {
				continue
			}
			ledgerIssues := normalizeIssues(ledger.Issues)
			if len(ledgerIssues) == 0 {
				ledgerIssues = issuesFromScopeID(ledger.DeliveryScopeID)
			}
			if len(ledgerIssues) > 0 {
				return ledger.DeliveryScopeID, ledgerIssues
			}
		}
	}
	if len(issues) == 0 && issue > 0 {
		issues = []int{issue}
	}
	return scopeID(baseBranch, issues), issues
}

func ledgerMatchesBase(ledger Ledger, baseBranch string) bool {
	if strings.TrimSpace(ledger.BaseBranch) != "" {
		return ledger.BaseBranch == baseBranch
	}
	return strings.HasPrefix(ledger.DeliveryScopeID, baseBranch+":")
}

func scopeID(baseBranch string, issues []int) string {
	parts := make([]string, 0, len(issues))
	for _, issue := range normalizeIssues(issues) {
		parts = append(parts, strconv.Itoa(issue))
	}
	return baseBranch + ":" + strings.Join(parts, ",")
}

func issuesFromScopeID(id string) []int {
	_, rest, ok := strings.Cut(id, ":")
	if !ok {
		return nil
	}
	parts := strings.Split(rest, ",")
	issues := make([]int, 0, len(parts))
	for _, part := range parts {
		value, err := strconv.Atoi(strings.TrimSpace(part))
		if err == nil && value > 0 {
			issues = append(issues, value)
		}
	}
	return normalizeIssues(issues)
}

func normalizeIssues(issues []int) []int {
	seen := map[int]bool{}
	out := make([]int, 0, len(issues))
	for _, issue := range issues {
		if issue <= 0 || seen[issue] {
			continue
		}
		seen[issue] = true
		out = append(out, issue)
	}
	sort.Ints(out)
	return out
}

func usageTotal(usage *attestation.Usage) (int64, bool) {
	if usage == nil {
		return 0, false
	}
	if usage.TotalTokens != nil && *usage.TotalTokens >= 0 {
		return *usage.TotalTokens, true
	}
	if usage.InputTokens != nil && usage.OutputTokens != nil && *usage.InputTokens >= 0 && *usage.OutputTokens >= 0 {
		return *usage.InputTokens + *usage.OutputTokens, true
	}
	return 0, false
}

func recoveryContextPath(repoPath, runID string, attempt state.Attempt) string {
	if strings.TrimSpace(attempt.RecoveryContextPath) != "" {
		if filepath.IsAbs(attempt.RecoveryContextPath) {
			return filepath.Clean(attempt.RecoveryContextPath)
		}
		return filepath.Join(repoPath, attempt.RecoveryContextPath)
	}
	if strings.TrimSpace(attempt.JobID) == "" {
		return ""
	}
	return state.RecoveryBriefPath(repoPath, runID, attempt.JobID)
}

func blockDecision(decision Decision, reason, detail string, cap *CapEvidence) Decision {
	decision.Allowed = false
	decision.Status = StatusNeedsHuman
	decision.Reason = reason
	decision.Cap = cap
	decision.Message = formatDecisionMessage(decision, detail)
	return decision
}

func formatDecisionMessage(decision Decision, detail string) string {
	if decision.Allowed {
		return "guardrails.budget allowed"
	}
	parts := []string{
		fmt.Sprintf("needs-human: %s", decision.Reason),
		fmt.Sprintf("issue=#%d", decision.Issue),
		fmt.Sprintf("run_id=%s", decision.RunID),
		fmt.Sprintf("scope=%s", decision.DeliveryScopeID),
		fmt.Sprintf("observed_runs=%d", decision.Observed.Runs),
		fmt.Sprintf("observed_attempts=%d", decision.Observed.TotalAttempts),
		fmt.Sprintf("observed_tokens=%d", decision.Observed.TotalTokens),
	}
	if decision.Observed.TotalCostUSD != nil {
		parts = append(parts, fmt.Sprintf("observed_cost_usd=%s", formatUSD(*decision.Observed.TotalCostUSD)))
	}
	if decision.Cap != nil {
		parts = append(parts,
			fmt.Sprintf("cap=%s", decision.Cap.Name),
			fmt.Sprintf("limit=%s", decision.Cap.Limit),
			fmt.Sprintf("cap_observed=%s", decision.Cap.Observed),
		)
		if decision.Cap.PlannedIncrement != 0 {
			parts = append(parts, fmt.Sprintf("planned_increment=%d", decision.Cap.PlannedIncrement))
		}
		if decision.Cap.ProposedIncrement != 0 {
			parts = append(parts, fmt.Sprintf("proposed_increment=%d", decision.Cap.ProposedIncrement))
		}
	}
	if len(decision.RunIDs) > 0 {
		parts = append(parts, fmt.Sprintf("run_ids=%s", strings.Join(decision.RunIDs, ",")))
	}
	if strings.TrimSpace(decision.LatestAttemptPath) != "" {
		parts = append(parts, fmt.Sprintf("latest_attempt=%s", filepath.ToSlash(decision.LatestAttemptPath)))
	}
	if strings.TrimSpace(decision.RecoveryContextPath) != "" {
		parts = append(parts, fmt.Sprintf("recovery=%s", filepath.ToSlash(decision.RecoveryContextPath)))
	}
	if strings.TrimSpace(detail) != "" {
		parts = append(parts, "detail="+detail)
	}
	parts = append(parts, "human_decision=raise cap, inspect recovery context, clarify the issue, close/supersede it, or explicitly start a new scoped run")
	return strings.Join(parts, "; ")
}

func formatUSD(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func formatUSDPtr(value *float64) string {
	if value == nil {
		return "unavailable"
	}
	return formatUSD(*value)
}

func sortedKeys(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
