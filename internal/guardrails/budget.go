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

	GuardrailBudget         = "budget"
	GuardrailCircuitBreaker = "circuit_breaker"

	CircuitOutcomeWave    = "wave"
	CircuitOutcomeAttempt = "attempt"
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
	Guardrail              string
	Enabled                bool
	Allowed                bool
	Status                 string
	Reason                 string
	Message                string
	DeliveryScopeID        string
	BaseBranch             string
	Issue                  int
	Issues                 []int
	RunID                  string
	RunIDs                 []string
	Observed               Observed
	Cap                    *CapEvidence
	LatestAttemptPath      string
	RecoveryContextPath    string
	LastMaterialProgressAt string
	AttemptHistory         []AttemptEvidence
	Outcome                string
	MaterialProgress       bool
	DecisionAt             time.Time
}

type Observed struct {
	Runs               int      `json:"runs"`
	TotalAttempts      int      `json:"total_attempts"`
	TotalTokens        int64    `json:"total_tokens"`
	TotalCostUSD       *float64 `json:"total_cost_usd,omitempty"`
	NoProgressWaves    int      `json:"no_progress_waves,omitempty"`
	NoProgressAttempts int      `json:"no_progress_attempts,omitempty"`
}

type CapEvidence struct {
	Name              string `json:"name"`
	Limit             string `json:"limit"`
	Observed          string `json:"observed"`
	PlannedIncrement  int    `json:"planned_increment,omitempty"`
	ProposedIncrement int    `json:"proposed_increment,omitempty"`
}

type Ledger struct {
	Version                int               `json:"version"`
	Guardrail              string            `json:"guardrail,omitempty"`
	DeliveryScopeID        string            `json:"delivery_scope_id"`
	BaseBranch             string            `json:"base_branch,omitempty"`
	RunID                  string            `json:"run_id"`
	Issue                  int               `json:"issue"`
	Issues                 []int             `json:"issues,omitempty"`
	Status                 string            `json:"status"`
	Reason                 string            `json:"reason,omitempty"`
	Cap                    *CapEvidence      `json:"cap,omitempty"`
	Observed               Observed          `json:"observed"`
	RunIDs                 []string          `json:"run_ids,omitempty"`
	LatestAttemptPath      string            `json:"latest_attempt_path,omitempty"`
	RecoveryContextPath    string            `json:"recovery_context_path,omitempty"`
	LastMaterialProgressAt string            `json:"last_material_progress_at,omitempty"`
	AttemptHistory         []AttemptEvidence `json:"attempt_history,omitempty"`
	Outcome                string            `json:"outcome,omitempty"`
	MaterialProgress       bool              `json:"material_progress,omitempty"`
	DecisionAt             string            `json:"decision_at"`
}

type AttemptEvidence struct {
	RunID               string `json:"run_id,omitempty"`
	JobID               string `json:"job_id"`
	Attempt             int    `json:"attempt"`
	Status              string `json:"status,omitempty"`
	Phase               string `json:"phase,omitempty"`
	Branch              string `json:"branch,omitempty"`
	Path                string `json:"path,omitempty"`
	RecoveryContextPath string `json:"recovery_context_path,omitempty"`
	Progress            string `json:"progress,omitempty"`
}

type CircuitOptions struct {
	RepoPath       string
	RunID          string
	BaseBranch     string
	Issue          int
	ScopeIssues    []int
	CircuitBreaker config.GuardrailCircuitBreaker
	Outcome        *CircuitOutcome
	Now            time.Time
}

type CircuitOutcome struct {
	Kind             string
	MaterialProgress bool
	Detail           string
	CountedInState   bool
}

type FrozenIssue struct {
	Issue                  int
	Reason                 string
	Message                string
	Observed               Observed
	LatestAttemptPath      string
	RecoveryContextPath    string
	LastMaterialProgressAt string
	AttemptHistory         []AttemptEvidence
	DecisionAt             string
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
		Guardrail:  GuardrailBudget,
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

func EvaluateCircuitBreaker(opts CircuitOptions) Decision {
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

	decision := Decision{
		Guardrail:  GuardrailCircuitBreaker,
		Enabled:    opts.CircuitBreaker.Enabled(),
		Allowed:    true,
		Status:     StatusAllowed,
		BaseBranch: baseBranch,
		Issue:      opts.Issue,
		RunID:      strings.TrimSpace(opts.RunID),
		DecisionAt: now,
	}
	if !opts.CircuitBreaker.Enabled() {
		return decision
	}

	ledgers, err := LoadLedgers(opts.RepoPath)
	if err != nil {
		return blockDecision(decision, "guardrails.circuit_breaker.unavailable-evidence", fmt.Sprintf("read guardrail ledger: %v", err), nil)
	}
	scopeID, issues := resolveScope(baseBranch, opts.Issue, opts.ScopeIssues, ledgers)
	decision.DeliveryScopeID = scopeID
	decision.Issues = issues

	circuit, err := collectCircuitState(opts.RepoPath, decision.RunID, scopeID, opts.Issue, ledgers)
	if err != nil {
		return blockDecision(decision, "guardrails.circuit_breaker.unavailable-evidence", err.Error(), nil)
	}
	decision.Observed = circuit.observed
	decision.RunIDs = circuit.runIDs
	decision.LatestAttemptPath = circuit.latestAttemptPath
	decision.RecoveryContextPath = circuit.recoveryContextPath
	decision.LastMaterialProgressAt = circuit.lastMaterialProgressAt
	decision.AttemptHistory = circuit.attemptHistory

	if opts.Outcome != nil {
		decision.Outcome = strings.TrimSpace(opts.Outcome.Kind)
		decision.MaterialProgress = opts.Outcome.MaterialProgress
		if opts.Outcome.MaterialProgress {
			decision.Observed.NoProgressWaves = 0
			decision.Observed.NoProgressAttempts = 0
			decision.LastMaterialProgressAt = state.FormatTimestamp(now)
		} else {
			switch opts.Outcome.Kind {
			case CircuitOutcomeWave:
				decision.Observed.NoProgressWaves++
			case CircuitOutcomeAttempt:
				if !opts.Outcome.CountedInState {
					decision.Observed.NoProgressAttempts++
				}
			}
		}
	}

	if opts.CircuitBreaker.MaxNoProgressWaves != nil && decision.Observed.NoProgressWaves >= *opts.CircuitBreaker.MaxNoProgressWaves {
		cap := &CapEvidence{
			Name:     "guardrails.circuit_breaker.max_no_progress_waves",
			Limit:    strconv.Itoa(*opts.CircuitBreaker.MaxNoProgressWaves),
			Observed: strconv.Itoa(decision.Observed.NoProgressWaves),
		}
		return blockDecision(decision, cap.Name, "no-progress wave streak reached the circuit-breaker threshold", cap)
	}
	if opts.CircuitBreaker.MaxNoProgressAttempts != nil && decision.Observed.NoProgressAttempts >= *opts.CircuitBreaker.MaxNoProgressAttempts {
		cap := &CapEvidence{
			Name:     "guardrails.circuit_breaker.max_no_progress_attempts",
			Limit:    strconv.Itoa(*opts.CircuitBreaker.MaxNoProgressAttempts),
			Observed: strconv.Itoa(decision.Observed.NoProgressAttempts),
		}
		return blockDecision(decision, cap.Name, "no-progress attempt streak reached the circuit-breaker threshold", cap)
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
		Version:                1,
		Guardrail:              decision.Guardrail,
		DeliveryScopeID:        decision.DeliveryScopeID,
		BaseBranch:             decision.BaseBranch,
		RunID:                  decision.RunID,
		Issue:                  decision.Issue,
		Issues:                 append([]int(nil), decision.Issues...),
		Status:                 decision.Status,
		Reason:                 decision.Reason,
		Cap:                    decision.Cap,
		Observed:               decision.Observed,
		RunIDs:                 append([]string(nil), decision.RunIDs...),
		LatestAttemptPath:      decision.LatestAttemptPath,
		RecoveryContextPath:    decision.RecoveryContextPath,
		LastMaterialProgressAt: decision.LastMaterialProgressAt,
		AttemptHistory:         append([]AttemptEvidence(nil), decision.AttemptHistory...),
		Outcome:                decision.Outcome,
		MaterialProgress:       decision.MaterialProgress,
		DecisionAt:             state.FormatTimestamp(decision.DecisionAt),
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

type circuitState struct {
	observed               Observed
	runIDs                 []string
	latestAttemptPath      string
	recoveryContextPath    string
	lastMaterialProgressAt string
	attemptHistory         []AttemptEvidence
}

func collectCircuitState(repoPath, currentRunID, scopeID string, issue int, ledgers []Ledger) (circuitState, error) {
	runSet := map[string]bool{}
	var latestLedger *Ledger
	for i := range ledgers {
		ledger := ledgers[i]
		if ledger.Issue != issue || ledger.DeliveryScopeID != scopeID {
			continue
		}
		if strings.TrimSpace(ledger.RunID) != "" {
			runSet[ledger.RunID] = true
		}
		if !isCircuitLedger(ledger) {
			continue
		}
		item := ledger
		latestLedger = &item
	}
	if strings.TrimSpace(currentRunID) != "" {
		runSet[currentRunID] = true
	}

	var attempts []scopedAttempt
	for runID := range runSet {
		loaded, err := loadAttemptsStrict(repoPath, runID)
		if err != nil {
			return circuitState{}, err
		}
		for _, attempt := range loaded {
			if attempt.Issue == issue {
				attempts = append(attempts, scopedAttempt{runID: runID, attempt: attempt})
			}
		}
	}
	sort.Slice(attempts, func(i, j int) bool {
		if attempts[i].attempt.Attempt != attempts[j].attempt.Attempt {
			return attempts[i].attempt.Attempt < attempts[j].attempt.Attempt
		}
		if !attempts[i].attempt.LastWriteUTC.Equal(attempts[j].attempt.LastWriteUTC) {
			return attempts[i].attempt.LastWriteUTC.Before(attempts[j].attempt.LastWriteUTC)
		}
		if attempts[i].runID != attempts[j].runID {
			return attempts[i].runID < attempts[j].runID
		}
		return attempts[i].attempt.JobID < attempts[j].attempt.JobID
	})

	observed := Observed{TotalAttempts: len(attempts)}
	lastMaterial := ""
	if latestLedger != nil {
		observed.NoProgressWaves = latestLedger.Observed.NoProgressWaves
		observed.NoProgressAttempts = latestLedger.Observed.NoProgressAttempts
		lastMaterial = latestLedger.LastMaterialProgressAt
	}
	lastMaterialTime := parseOptionalTimestamp(lastMaterial)

	noProgressAttempts := 0
	attemptHistory := make([]AttemptEvidence, 0, len(attempts))
	var latestAttempt *scopedAttempt
	for i := range attempts {
		item := attempts[i]
		latestAttempt = &item
		material := AttemptHasMaterialProgress(repoPath, item.runID, item.attempt)
		progress := "no-progress"
		if material {
			progress = "material-progress"
			noProgressAttempts = 0
			if ts := materialProgressTimestamp(item.attempt); ts != "" {
				lastMaterial = ts
				lastMaterialTime = parseOptionalTimestamp(ts)
			}
		} else {
			attemptTime := attemptTimestamp(item.attempt)
			if lastMaterialTime.IsZero() || attemptTime.IsZero() || attemptTime.After(lastMaterialTime) {
				noProgressAttempts++
			}
		}
		attemptHistory = append(attemptHistory, attemptEvidence(repoPath, item.runID, item.attempt, progress))
	}
	if noProgressAttempts > observed.NoProgressAttempts {
		observed.NoProgressAttempts = noProgressAttempts
	}

	out := circuitState{
		observed:               observed,
		runIDs:                 sortedKeys(runSet),
		lastMaterialProgressAt: lastMaterial,
		attemptHistory:         attemptHistory,
	}
	if latestAttempt != nil {
		out.latestAttemptPath = latestAttempt.attempt.Path
		out.recoveryContextPath = recoveryContextPath(repoPath, latestAttempt.runID, latestAttempt.attempt)
	}
	if latestLedger != nil {
		out.latestAttemptPath = firstNonEmpty(out.latestAttemptPath, latestLedger.LatestAttemptPath)
		out.recoveryContextPath = firstNonEmpty(out.recoveryContextPath, latestLedger.RecoveryContextPath)
		if len(out.attemptHistory) == 0 {
			out.attemptHistory = append([]AttemptEvidence(nil), latestLedger.AttemptHistory...)
		}
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

func AttemptHasMaterialProgress(repoPath, runID string, attempt state.Attempt) bool {
	status := strings.ToLower(strings.TrimSpace(attempt.Status))
	phase := strings.ToLower(strings.TrimSpace(attempt.Phase))
	switch status {
	case "succeeded", "success", "completed", "complete", "done":
		return true
	}
	switch phase {
	case "committed", "pushed", "pr_opened", "cleanup":
		return true
	}
	return recoveryBriefHasChangedFiles(recoveryContextPath(repoPath, runID, attempt))
}

func recoveryBriefHasChangedFiles(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	const marker = "## Changed files\n\n```text\n"
	start := strings.Index(text, marker)
	if start < 0 {
		return false
	}
	rest := text[start+len(marker):]
	end := strings.Index(rest, "\n```")
	if end < 0 {
		return false
	}
	changed := strings.TrimSpace(rest[:end])
	if changed == "" {
		return false
	}
	lower := strings.ToLower(changed)
	return changed != "(none)" &&
		!strings.Contains(lower, "worktree path does not exist") &&
		!strings.Contains(lower, "git status failed")
}

func materialProgressTimestamp(attempt state.Attempt) string {
	if strings.TrimSpace(attempt.LastProgressAt) != "" {
		return attempt.LastProgressAt
	}
	if !attempt.LastWriteUTC.IsZero() {
		return state.FormatTimestamp(attempt.LastWriteUTC)
	}
	return ""
}

func attemptTimestamp(attempt state.Attempt) time.Time {
	if strings.TrimSpace(attempt.LastProgressAt) != "" {
		if parsed, err := state.ParseTimestamp(attempt.LastProgressAt); err == nil {
			return parsed
		}
	}
	if !attempt.LastWriteUTC.IsZero() {
		return attempt.LastWriteUTC.UTC()
	}
	return time.Time{}
}

func parseOptionalTimestamp(value string) time.Time {
	if strings.TrimSpace(value) == "" {
		return time.Time{}
	}
	parsed, err := state.ParseTimestamp(value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func attemptEvidence(repoPath, runID string, attempt state.Attempt, progress string) AttemptEvidence {
	return AttemptEvidence{
		RunID:               runID,
		JobID:               attempt.JobID,
		Attempt:             attempt.Attempt,
		Status:              attempt.Status,
		Phase:               attempt.Phase,
		Branch:              attempt.Branch,
		Path:                filepath.ToSlash(repoRelativePath(repoPath, attempt.Path)),
		RecoveryContextPath: filepath.ToSlash(repoRelativePath(repoPath, recoveryContextPath(repoPath, runID, attempt))),
		Progress:            progress,
	}
}

func repoRelativePath(repoPath, path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	absRepo, err := filepath.Abs(repoPath)
	if err != nil {
		return absPath
	}
	rel, err := filepath.Rel(absRepo, absPath)
	if err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return absPath
}

func isCircuitLedger(ledger Ledger) bool {
	if ledger.Guardrail == GuardrailCircuitBreaker {
		return true
	}
	return strings.HasPrefix(ledger.Reason, "guardrails.circuit_breaker.") ||
		ledger.Observed.NoProgressWaves > 0 ||
		ledger.Observed.NoProgressAttempts > 0 ||
		strings.TrimSpace(ledger.LastMaterialProgressAt) != "" ||
		strings.TrimSpace(ledger.Outcome) != ""
}

func FrozenIssues(repoPath, baseBranch string) (map[int]FrozenIssue, error) {
	ledgers, err := LoadLedgers(repoPath)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(baseBranch) == "" {
		baseBranch = "main"
	}
	out := map[int]FrozenIssue{}
	for _, ledger := range ledgers {
		if !ledgerMatchesBase(ledger, baseBranch) || !isCircuitLedger(ledger) {
			continue
		}
		if ledger.Status != StatusNeedsHuman || !strings.HasPrefix(ledger.Reason, "guardrails.circuit_breaker.") {
			if existing, ok := out[ledger.Issue]; ok && ledger.DecisionAt >= existing.DecisionAt && ledger.Status == StatusAllowed {
				delete(out, ledger.Issue)
			}
			continue
		}
		out[ledger.Issue] = FrozenIssue{
			Issue:                  ledger.Issue,
			Reason:                 ledger.Reason,
			Message:                frozenLedgerMessage(ledger),
			Observed:               ledger.Observed,
			LatestAttemptPath:      ledger.LatestAttemptPath,
			RecoveryContextPath:    ledger.RecoveryContextPath,
			LastMaterialProgressAt: ledger.LastMaterialProgressAt,
			AttemptHistory:         append([]AttemptEvidence(nil), ledger.AttemptHistory...),
			DecisionAt:             ledger.DecisionAt,
		}
	}
	return out, nil
}

func frozenLedgerMessage(ledger Ledger) string {
	decision := Decision{
		Guardrail:              GuardrailCircuitBreaker,
		Enabled:                true,
		Allowed:                false,
		Status:                 StatusNeedsHuman,
		Reason:                 ledger.Reason,
		DeliveryScopeID:        ledger.DeliveryScopeID,
		BaseBranch:             ledger.BaseBranch,
		Issue:                  ledger.Issue,
		Issues:                 append([]int(nil), ledger.Issues...),
		RunID:                  ledger.RunID,
		RunIDs:                 append([]string(nil), ledger.RunIDs...),
		Observed:               ledger.Observed,
		Cap:                    ledger.Cap,
		LatestAttemptPath:      ledger.LatestAttemptPath,
		RecoveryContextPath:    ledger.RecoveryContextPath,
		LastMaterialProgressAt: ledger.LastMaterialProgressAt,
		AttemptHistory:         append([]AttemptEvidence(nil), ledger.AttemptHistory...),
	}
	return formatDecisionMessage(decision, "issue is guardrail-frozen")
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
		if decision.Guardrail == GuardrailCircuitBreaker {
			return "guardrails.circuit_breaker allowed"
		}
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
	if decision.Observed.NoProgressWaves > 0 {
		parts = append(parts, fmt.Sprintf("no_progress_waves=%d", decision.Observed.NoProgressWaves))
	}
	if decision.Observed.NoProgressAttempts > 0 {
		parts = append(parts, fmt.Sprintf("no_progress_attempts=%d", decision.Observed.NoProgressAttempts))
	}
	if strings.TrimSpace(decision.LastMaterialProgressAt) != "" {
		parts = append(parts, fmt.Sprintf("last_material_progress=%s", decision.LastMaterialProgressAt))
	} else if decision.Guardrail == GuardrailCircuitBreaker {
		parts = append(parts, "last_material_progress=unknown")
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
	if len(decision.AttemptHistory) > 0 {
		parts = append(parts, "attempt_history="+formatAttemptEvidence(decision.AttemptHistory))
	}
	if strings.TrimSpace(detail) != "" {
		parts = append(parts, "detail="+detail)
	}
	if decision.Guardrail == GuardrailCircuitBreaker {
		parts = append(parts, "human_decision=clarify the issue, raise the no-progress threshold, close/supersede the issue, or explicitly start a new scoped run")
	} else {
		parts = append(parts, "human_decision=raise cap, inspect recovery context, clarify the issue, close/supersede it, or explicitly start a new scoped run")
	}
	return strings.Join(parts, "; ")
}

func formatAttemptEvidence(history []AttemptEvidence) string {
	parts := make([]string, 0, len(history))
	for _, attempt := range history {
		piece := fmt.Sprintf("attempt %d job %s status=%s phase=%s progress=%s", attempt.Attempt, attempt.JobID, attempt.Status, attempt.Phase, attempt.Progress)
		if strings.TrimSpace(attempt.Path) != "" {
			piece += " sidecar=" + filepath.ToSlash(attempt.Path)
		}
		if strings.TrimSpace(attempt.RecoveryContextPath) != "" {
			piece += " recovery=" + filepath.ToSlash(attempt.RecoveryContextPath)
		}
		parts = append(parts, piece)
	}
	return strings.Join(parts, " | ")
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
