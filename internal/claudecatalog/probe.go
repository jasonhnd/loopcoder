// Package claudecatalog implements the explicit paid Claude Code capability
// probe. Normal provider inventory refresh never calls this package.
package claudecatalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/budget"
	"github.com/jasonhnd/loopcoder/internal/providerinstall"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/usageledger"
)

const (
	DefaultTimeout        = 90 * time.Second
	authTimeout           = 15 * time.Second
	defaultOutputLimit    = 1024 * 1024
	authOutputLimit       = 64 * 1024
	defaultReservedTokens = int64(32768)
	probePrompt           = "Reply with exactly OK."
)

var (
	ErrProbeInvalid       = errors.New("ErrClaudeCapabilityProbeInvalid")
	ErrProbeExecution     = errors.New("ErrClaudeCapabilityProbeExecution")
	ErrProbeOutput        = errors.New("ErrClaudeCapabilityProbeOutput")
	ErrProbeAccountDrift  = errors.New("ErrClaudeCapabilityProbeAccountDrift")
	ErrProbeUsageOverflow = errors.New("ErrClaudeCapabilityProbeUsageOverflow")

	credentialPattern = regexp.MustCompile(`(?i)(authorization\s*:\s*bearer|bearer\s+[A-Za-z0-9._~-]{12,}|sk-[A-Za-z0-9_-]{12,})`)
	emailPattern      = regexp.MustCompile(`(?i)\b[A-Z0-9._%+\-]{1,64}@[A-Z0-9.\-]{1,190}\.[A-Z]{2,24}\b`)
	modelIDPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,119}$`)
)

type Request struct {
	RepoPath       string
	ProjectID      string
	DeliveryRunID  string
	Model          string
	Effort         string
	AccountRef     string
	InstallRef     string
	Timeout        time.Duration
	OutputLimit    int
	ReservedTokens int64
	Now            func() time.Time
	Inventory      providerinventory.Report
}

type Result struct {
	Receipt providerinventory.ClaudeCapabilityProbeReceipt `json:"receipt"`
	Report  providerinventory.Report                       `json:"report"`
}

type CommandRequest struct {
	Executable string
	Args       []string
	Stdin      string
	Dir        string
	Timeout    time.Duration
	Limit      int
}

type CommandResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	TimedOut bool
	Overflow bool
}

type Deps struct {
	LookPath  func(string) (string, error)
	Run       func(context.Context, CommandRequest) (CommandResult, error)
	MkdirTemp func(string, string) (string, error)
	RemoveAll func(string) error
}

func DefaultDeps() Deps {
	return Deps{
		LookPath:  exec.LookPath,
		Run:       runCommand,
		MkdirTemp: os.MkdirTemp,
		RemoveAll: os.RemoveAll,
	}
}

func Verify(ctx context.Context, store storage.Store, req Request, deps Deps) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if store == nil {
		return Result{}, fmt.Errorf("%w: storage store is required", ErrProbeInvalid)
	}
	deps = normalizeDeps(deps)
	now := time.Now
	if req.Now != nil {
		now = req.Now
	}
	started := now().UTC()
	req.Model = strings.TrimSpace(req.Model)
	req.Effort = strings.TrimSpace(req.Effort)
	if _, ok := providerinventory.ClaudeCatalogCandidate(req.Model); !ok {
		return Result{}, fmt.Errorf("%w: model must be an adapter-declared Claude alias or full ID", ErrProbeInvalid)
	}
	if !validEffort(req.Effort) {
		return Result{}, fmt.Errorf("%w: unsupported effort %q", ErrProbeInvalid, req.Effort)
	}
	if strings.TrimSpace(req.ProjectID) == "" {
		return Result{}, fmt.Errorf("%w: project id is required for usage accounting", ErrProbeInvalid)
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	limit := req.OutputLimit
	if limit <= 0 {
		limit = defaultOutputLimit
	}
	reservedTokens := req.ReservedTokens
	if reservedTokens <= 0 {
		reservedTokens = defaultReservedTokens
	}
	executable, err := deps.LookPath("claude")
	if err != nil {
		return Result{}, fmt.Errorf("%w: claude executable unavailable", ErrProbeExecution)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return Result{}, fmt.Errorf("%w: resolve claude executable", ErrProbeExecution)
	}
	installID, err := providerinstall.ComputeInstallationID("claude", executable)
	if err != nil {
		return Result{}, fmt.Errorf("%w: installation identity unavailable", ErrProbeExecution)
	}
	if want := strings.TrimSpace(req.InstallRef); want != "" && want != installID {
		return Result{}, fmt.Errorf("%w: selected installation does not match resolved executable", ErrProbeInvalid)
	}

	isolationDir, err := deps.MkdirTemp("", "loopcoder-claude-capability-probe.")
	if err != nil {
		return Result{}, fmt.Errorf("%w: create isolation directory", ErrProbeExecution)
	}
	defer deps.RemoveAll(isolationDir)

	preAuth, err := observeAuth(ctx, deps, executable, isolationDir, now().UTC())
	if err != nil {
		return Result{}, err
	}
	if want := strings.TrimSpace(req.AccountRef); want != "" && want != preAuth.AccountProfileID {
		return Result{}, fmt.Errorf("%w: selected account does not match active Claude auth", ErrProbeInvalid)
	}
	if preAuth.ProviderInstallationID != installID {
		return Result{}, fmt.Errorf("%w: auth observation installation mismatch", ErrProbeInvalid)
	}
	failureResult := func(code string, cause error) (Result, error) {
		inventory := req.Inventory
		inventory.AccountProfiles = append(inventory.AccountProfiles, preAuth.SafeProfile)
		inventory.AuthReadiness = append(inventory.AuthReadiness, preAuth.SafeReadiness)
		report, applyErr := providerinventory.ApplyClaudeUnavailableSubset(inventory, preAuth, req.Model, req.Effort, code, now().UTC())
		if applyErr != nil {
			return Result{}, errors.Join(cause, applyErr)
		}
		return Result{Report: report}, cause
	}

	reserveKey := "claude-capability-probe:" + digestStrings(req.ProjectID, req.DeliveryRunID, installID, preAuth.AccountProfileID, req.Model, req.Effort, started.Format(time.RFC3339Nano))
	reserved, err := budget.Reserve(ctx, store, budget.ReserveRequest{
		ScopeChain: []budget.Scope{
			{ScopeKind: budget.ScopeProject, ProjectID: req.ProjectID},
			{ScopeKind: budget.ScopeProvider, ProjectID: req.ProjectID, DeliveryRunID: req.DeliveryRunID, AdapterID: "claude", AccountProfileID: preAuth.AccountProfileID},
		},
		QuantityKind:             providerinventory.QuantityTotalTokens,
		Unit:                     "tokens",
		ValueScale:               0,
		WindowKind:               providerinventory.WindowUnbounded,
		RequestedValue:           reservedTokens,
		LeaseExpiresAt:           now().UTC().Add(timeout + 2*time.Minute),
		IdempotencyKey:           reserveKey,
		RequesterID:              "claude-capability-probe",
		AuthorizationFingerprint: digestStrings(installID, preAuth.AccountProfileID, preAuth.RawSHA256),
		RequirementConfidence:    providerinventory.ConfidenceExact,
		Actor:                    budget.Actor{ActorID: "loopcoder", Role: "provider-capability-probe"},
		Host:                     budget.Host{Provider: "claude", Model: req.Model},
	})
	if err != nil {
		return Result{}, err
	}
	cleanupReservation := reserved.Reservation
	reconciled := false
	defer func() {
		if !reconciled && cleanupReservation.ReservedValue > 0 {
			_, _ = budget.Release(context.Background(), store, budget.MutationRequest{
				ReservationID:  cleanupReservation.BudgetReservationID,
				IdempotencyKey: reserveKey + ":failure-release",
				Generation:     cleanupReservation.Generation,
				Value:          cleanupReservation.ReservedValue,
				Actor:          budget.Actor{ActorID: "loopcoder", Role: "provider-capability-probe"},
				Host:           budget.Host{Provider: "claude", Model: req.Model},
			})
		}
	}()
	recordFailedUsage := func(value int64, confidence providerinventory.Confidence, sourceHash, gap, model string) error {
		if value <= 0 || strings.TrimSpace(sourceHash) == "" {
			reconciled = true
			return fmt.Errorf("%w: failed probe usage evidence unavailable", ErrProbeUsageOverflow)
		}
		usageID := "usage_" + digestBase32("claude-capability-probe-failure", sourceHash, reserveKey, gap)[:26]
		record := usageledger.UsageRecord{
			SchemaVersion:       usageledger.UsageRecordSchema,
			UsageRecordID:       usageID,
			EventKind:           usageledger.EventFailure,
			EventTime:           now().UTC().Format(time.RFC3339Nano),
			ProjectID:           req.ProjectID,
			DeliveryRunID:       req.DeliveryRunID,
			AdapterID:           "claude",
			AccountProfileID:    preAuth.AccountProfileID,
			BudgetReservationID: cleanupReservation.BudgetReservationID,
			QuantityKind:        providerinventory.QuantityTotalTokens,
			Value:               value,
			Unit:                "tokens",
			ValueScale:          0,
			Confidence:          confidence,
			SourceRecordIDs:     []string{sourceHash},
			IdempotencyKey:      "usage-failure:" + reserveKey,
			DedupeKey:           sourceHash,
			GapReasons:          []string{"paid-bounded-capability-probe-failed", gap},
		}
		if confidence == providerinventory.ConfidenceEstimated {
			record.Estimator = "provider-reported-usage-account-linkage-unconfirmed"
			if strings.Contains(gap, "upper-bound") {
				record.Estimator = "reserved-token-upper-bound"
			}
			record.EstimatorVersion = "v1"
		}
		if _, err := usageledger.RecordUsage(ctx, store, record); err != nil {
			// The provider attempt occurred. Retain the full reservation
			// conservatively when even the usage ledger cannot be written.
			reconciled = true
			return err
		}
		if value > cleanupReservation.ReservedValue {
			// Exact observed usage exceeded the reservation. Keep the
			// reservation active and preserve the usage fact for audit.
			reconciled = true
			return fmt.Errorf("%w: observed usage exceeds reservation", ErrProbeUsageOverflow)
		}
		committed, err := budget.Commit(ctx, store, budget.MutationRequest{
			ReservationID:  cleanupReservation.BudgetReservationID,
			IdempotencyKey: reserveKey + ":failure-commit",
			Generation:     cleanupReservation.Generation,
			Value:          value,
			UsageRecordIDs: []string{usageID},
			Actor:          budget.Actor{ActorID: "loopcoder", Role: "provider-capability-probe"},
			Host:           budget.Host{Provider: "claude", Model: model},
		})
		if err != nil {
			reconciled = true
			return err
		}
		cleanupReservation = committed.Reservation
		if cleanupReservation.ReservedValue > 0 {
			released, err := budget.Release(ctx, store, budget.MutationRequest{
				ReservationID:  cleanupReservation.BudgetReservationID,
				IdempotencyKey: reserveKey + ":failure-release-unused",
				Generation:     cleanupReservation.Generation,
				Value:          cleanupReservation.ReservedValue,
				UsageRecordIDs: []string{usageID},
				Actor:          budget.Actor{ActorID: "loopcoder", Role: "provider-capability-probe"},
				Host:           budget.Host{Provider: "claude", Model: model},
			})
			if err != nil {
				return err
			}
			cleanupReservation = released.Reservation
		}
		reconciled = true
		return nil
	}

	args := probeArgs(req.Model, req.Effort)
	execResult, runErr := deps.Run(ctx, CommandRequest{
		Executable: executable,
		Args:       args,
		Stdin:      probePrompt,
		Dir:        isolationDir,
		Timeout:    timeout,
		Limit:      limit,
	})
	if runErr != nil || execResult.TimedOut || execResult.Overflow || execResult.ExitCode != 0 {
		sourceHash := rawSHA256(execResult.Stdout)
		zeroBytes(execResult.Stdout)
		zeroBytes(execResult.Stderr)
		cause := sanitizedExecutionError(execResult, runErr)
		if accountingErr := recordFailedUsage(reserved.Reservation.ReservedValue, providerinventory.ConfidenceEstimated, sourceHash, "provider-usage-unavailable-reserved-upper-bound", req.Model); accountingErr != nil {
			cause = errors.Join(cause, accountingErr)
		}
		return failureResult(executionFailureCode(execResult), cause)
	}
	rawHash := rawSHA256(execResult.Stdout)
	parsed, err := parseStreamJSON(execResult.Stdout)
	if err != nil {
		zeroBytes(execResult.Stdout)
		zeroBytes(execResult.Stderr)
		if accountingErr := recordFailedUsage(reserved.Reservation.ReservedValue, providerinventory.ConfidenceEstimated, rawHash, "provider-usage-unparseable-reserved-upper-bound", req.Model); accountingErr != nil {
			err = errors.Join(err, accountingErr)
		}
		return failureResult("ErrClaudeCapabilityProbeOutput", err)
	}
	zeroBytes(execResult.Stdout)
	zeroBytes(execResult.Stderr)
	if parsed.TotalTokens > reserved.Reservation.ReservedValue {
		cause := fmt.Errorf("%w: actual tokens exceed bounded reservation", ErrProbeUsageOverflow)
		if accountingErr := recordFailedUsage(parsed.TotalTokens, providerinventory.ConfidenceExact, rawHash, "provider-usage-exceeded-reservation", parsed.ActualModel); accountingErr != nil {
			cause = errors.Join(cause, accountingErr)
		}
		return failureResult("ErrClaudeCapabilityProbeUsageOverflow", cause)
	}

	postAuth, err := observeAuth(ctx, deps, executable, isolationDir, now().UTC())
	if err != nil {
		if accountingErr := recordFailedUsage(parsed.TotalTokens, providerinventory.ConfidenceEstimated, rawHash, "post-auth-observation-unavailable", parsed.ActualModel); accountingErr != nil {
			err = errors.Join(err, accountingErr)
		}
		return failureResult("ErrClaudeCapabilityProbePostAuth", err)
	}
	if postAuth.ProviderInstallationID != preAuth.ProviderInstallationID || postAuth.AccountProfileID != preAuth.AccountProfileID {
		cause := fmt.Errorf("%w: active Claude identity changed during probe", ErrProbeAccountDrift)
		if accountingErr := recordFailedUsage(parsed.TotalTokens, providerinventory.ConfidenceEstimated, rawHash, "post-auth-account-drift", parsed.ActualModel); accountingErr != nil {
			cause = errors.Join(cause, accountingErr)
		}
		return failureResult("ErrClaudeCapabilityProbeAccountDrift", cause)
	}

	executedAt := now().UTC()
	usageID := "usage_" + digestBase32("claude-capability-probe", rawHash, preAuth.AccountProfileID, parsed.ActualModel)[:26]
	usageRecord := usageledger.UsageRecord{
		SchemaVersion:       usageledger.UsageRecordSchema,
		UsageRecordID:       usageID,
		EventKind:           usageledger.EventCompletion,
		EventTime:           executedAt.Format(time.RFC3339Nano),
		ProjectID:           req.ProjectID,
		DeliveryRunID:       req.DeliveryRunID,
		AdapterID:           "claude",
		AccountProfileID:    preAuth.AccountProfileID,
		BudgetReservationID: reserved.Reservation.BudgetReservationID,
		QuantityKind:        providerinventory.QuantityTotalTokens,
		Value:               parsed.TotalTokens,
		Unit:                "tokens",
		ValueScale:          0,
		Confidence:          providerinventory.ConfidenceExact,
		SourceRecordIDs:     []string{rawHash},
		IdempotencyKey:      "usage:" + reserveKey,
		DedupeKey:           rawHash,
		GapReasons:          []string{"paid-bounded-capability-probe", "loopcoder-local-ledger-not-provider-global"},
	}
	if _, err := usageledger.RecordUsage(ctx, store, usageRecord); err != nil {
		return failureResult("ErrClaudeCapabilityProbeUsageAccounting", err)
	}
	committed, err := budget.Commit(ctx, store, budget.MutationRequest{
		ReservationID:  reserved.Reservation.BudgetReservationID,
		IdempotencyKey: reserveKey + ":commit",
		Generation:     reserved.Reservation.Generation,
		Value:          parsed.TotalTokens,
		UsageRecordIDs: []string{usageID},
		Actor:          budget.Actor{ActorID: "loopcoder", Role: "provider-capability-probe"},
		Host:           budget.Host{Provider: "claude", Model: parsed.ActualModel},
	})
	if err != nil {
		// The paid invocation is already represented in the usage ledger.
		// Keep the reservation conservative instead of releasing it and
		// making the actual provider spend disappear from capacity.
		reconciled = true
		return failureResult("ErrClaudeCapabilityProbeBudgetCommit", err)
	}
	cleanupReservation = committed.Reservation
	releasedTokens := committed.Reservation.ReservedValue
	finalBudget := committed
	if releasedTokens > 0 {
		finalBudget, err = budget.Release(ctx, store, budget.MutationRequest{
			ReservationID:  committed.Reservation.BudgetReservationID,
			IdempotencyKey: reserveKey + ":release-unused",
			Generation:     committed.Reservation.Generation,
			Value:          releasedTokens,
			UsageRecordIDs: []string{usageID},
			Actor:          budget.Actor{ActorID: "loopcoder", Role: "provider-capability-probe"},
			Host:           budget.Host{Provider: "claude", Model: parsed.ActualModel},
		})
		if err != nil {
			return failureResult("ErrClaudeCapabilityProbeBudgetRelease", err)
		}
		cleanupReservation = finalBudget.Reservation
	}
	reconciled = true

	receipt := providerinventory.ClaudeCapabilityProbeReceipt{
		SchemaVersion:          providerinventory.ClaudeCapabilityProbeReceiptSchema,
		Provider:               "claude",
		RequestedModel:         req.Model,
		ActualModel:            parsed.ActualModel,
		AcceptedEffort:         req.Effort,
		ProviderInstallationID: preAuth.ProviderInstallationID,
		AccountProfileID:       preAuth.AccountProfileID,
		AuthReadinessID:        preAuth.AuthReadinessID,
		AuthObservedAt:         preAuth.ObservedAt,
		ExecutedAt:             executedAt.Format(time.RFC3339Nano),
		ExpiresAt:              executedAt.Add(30 * time.Minute).Format(time.RFC3339Nano),
		AuthRawSHA256:          preAuth.RawSHA256,
		OutputRawSHA256:        rawHash,
		ArgvDigest:             argvDigest(executable, args),
		InputTokens:            parsed.InputTokens,
		OutputTokens:           parsed.OutputTokens,
		CacheReadInputTokens:   parsed.CacheReadInputTokens,
		CacheCreateInputTokens: parsed.CacheCreateInputTokens,
		TotalTokens:            parsed.TotalTokens,
		CostUSDMicros:          parsed.CostUSDMicros,
		BudgetReservationID:    finalBudget.Reservation.BudgetReservationID,
		ReservedTokens:         reserved.Reservation.RequestedValue,
		CommittedTokens:        parsed.TotalTokens,
		ReleasedTokens:         releasedTokens,
		BudgetState:            string(finalBudget.Reservation.State),
		UsageRecordIDs:         []string{usageID},
		Source:                 "claude-code-stream-json",
		Confidence:             providerinventory.ConfidenceExact,
		FreshnessState:         providerinventory.FreshnessFresh,
		GapReasons:             []string{},
	}
	inventory := req.Inventory
	inventory.AccountProfiles = append(inventory.AccountProfiles, preAuth.SafeProfile)
	inventory.AuthReadiness = append(inventory.AuthReadiness, preAuth.SafeReadiness)
	report, err := providerinventory.ApplyClaudeVerifiedSubset(inventory, receipt, executedAt)
	if err != nil {
		return failureResult("ErrClaudeCapabilityProbeCatalogPersist", err)
	}
	return Result{Receipt: receipt, Report: report}, nil
}

func probeArgs(model, effort string) []string {
	return []string{
		"--print",
		"--output-format", "stream-json",
		"--verbose",
		"--safe-mode",
		"--tools", "",
		"--disable-slash-commands",
		"--strict-mcp-config",
		"--mcp-config", `{"mcpServers":{}}`,
		"--no-session-persistence",
		"--no-chrome",
		"--max-turns", "1",
		"--model", model,
		"--effort", effort,
	}
}

func observeAuth(ctx context.Context, deps Deps, executable, dir string, now time.Time) (providerinventory.ClaudeAuthBinding, error) {
	result, err := deps.Run(ctx, CommandRequest{
		Executable: executable,
		Args:       []string{"auth", "status", "--json"},
		Dir:        dir,
		Timeout:    authTimeout,
		Limit:      authOutputLimit,
	})
	defer zeroBytes(result.Stdout)
	defer zeroBytes(result.Stderr)
	if err != nil || result.TimedOut || result.Overflow || result.ExitCode != 0 {
		return providerinventory.ClaudeAuthBinding{}, fmt.Errorf("%w: bounded auth observation failed", ErrProbeExecution)
	}
	binding, err := providerinventory.ParseClaudeAuthBinding(executable, result.Stdout, result.ExitCode, now)
	if err != nil {
		return providerinventory.ClaudeAuthBinding{}, err
	}
	return binding, nil
}

type parsedUsage struct {
	ActualModel            string
	InputTokens            int64
	OutputTokens           int64
	CacheReadInputTokens   int64
	CacheCreateInputTokens int64
	TotalTokens            int64
	CostUSDMicros          int64
}

func parseStreamJSON(output []byte) (parsedUsage, error) {
	if len(output) == 0 || credentialPattern.Match(output) || emailPattern.Match(output) {
		return parsedUsage{}, fmt.Errorf("%w: empty or credential-shaped output refused", ErrProbeOutput)
	}
	var initModel string
	var assistantSeen bool
	var resultSeen bool
	var resultSucceeded bool
	var isError bool
	var modelUsage map[string]struct {
		InputTokens          int64   `json:"inputTokens"`
		OutputTokens         int64   `json:"outputTokens"`
		CacheReadInputTokens int64   `json:"cacheReadInputTokens"`
		CacheCreationTokens  int64   `json:"cacheCreationInputTokens"`
		CostUSD              float64 `json:"costUSD"`
		ContextWindow        int64   `json:"contextWindow"`
		MaxOutputTokens      int64   `json:"maxOutputTokens"`
		WebSearchRequests    int64   `json:"webSearchRequests"`
	}
	for _, line := range bytes.Split(output, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil {
			return parsedUsage{}, fmt.Errorf("%w: malformed stream event", ErrProbeOutput)
		}
		if containsCredentialField(line) {
			return parsedUsage{}, fmt.Errorf("%w: credential field refused", ErrProbeOutput)
		}
		switch envelope.Type {
		case "system":
			var event struct {
				Subtype string `json:"subtype"`
				Model   string `json:"model"`
			}
			if err := json.Unmarshal(line, &event); err != nil {
				return parsedUsage{}, fmt.Errorf("%w: malformed init event", ErrProbeOutput)
			}
			if event.Subtype == "init" {
				if event.Model != "" && !modelIDPattern.MatchString(event.Model) {
					return parsedUsage{}, fmt.Errorf("%w: unsafe init model identity", ErrProbeOutput)
				}
				if initModel != "" && initModel != event.Model {
					return parsedUsage{}, fmt.Errorf("%w: conflicting init model identity", ErrProbeOutput)
				}
				initModel = event.Model
			}
		case "assistant":
			assistantSeen = true
		case "result":
			if resultSeen {
				return parsedUsage{}, fmt.Errorf("%w: duplicate result event", ErrProbeOutput)
			}
			var event struct {
				Subtype    string `json:"subtype"`
				IsError    bool   `json:"is_error"`
				ModelUsage map[string]struct {
					InputTokens          int64   `json:"inputTokens"`
					OutputTokens         int64   `json:"outputTokens"`
					CacheReadInputTokens int64   `json:"cacheReadInputTokens"`
					CacheCreationTokens  int64   `json:"cacheCreationInputTokens"`
					CostUSD              float64 `json:"costUSD"`
					ContextWindow        int64   `json:"contextWindow"`
					MaxOutputTokens      int64   `json:"maxOutputTokens"`
					WebSearchRequests    int64   `json:"webSearchRequests"`
				} `json:"modelUsage"`
			}
			if err := json.Unmarshal(line, &event); err != nil {
				return parsedUsage{}, fmt.Errorf("%w: malformed result event", ErrProbeOutput)
			}
			resultSeen = true
			isError = event.IsError
			resultSucceeded = event.Subtype == "success" && !event.IsError
			modelUsage = event.ModelUsage
		default:
			// Unknown event kinds are not persisted and do not establish any fact.
		}
	}
	if !resultSeen || !resultSucceeded || isError || len(modelUsage) != 1 {
		return parsedUsage{}, fmt.Errorf("%w: successful result and one exact model are required", ErrProbeOutput)
	}
	actualModel := ""
	for modelID := range modelUsage {
		actualModel = modelID
	}
	if !modelIDPattern.MatchString(actualModel) {
		return parsedUsage{}, fmt.Errorf("%w: unsafe or absent model identity", ErrProbeOutput)
	}
	if initModel != "" && initModel != actualModel {
		return parsedUsage{}, fmt.Errorf("%w: init and modelUsage identities differ", ErrProbeOutput)
	}
	if !assistantSeen && strings.TrimSpace(initModel) == "" {
		// A successful result plus modelUsage is sufficient, but completely
		// context-free result events remain too weak.
		return parsedUsage{}, fmt.Errorf("%w: result lacks assistant or init context", ErrProbeOutput)
	}
	usage := modelUsage[actualModel]
	values := []int64{usage.InputTokens, usage.OutputTokens, usage.CacheReadInputTokens, usage.CacheCreationTokens}
	total := int64(0)
	for _, value := range values {
		if value < 0 || value > math.MaxInt64-total {
			return parsedUsage{}, fmt.Errorf("%w: invalid token usage", ErrProbeOutput)
		}
		total += value
	}
	costMicros := usage.CostUSD * 1_000_000
	if total <= 0 ||
		math.IsNaN(usage.CostUSD) ||
		math.IsInf(usage.CostUSD, 0) ||
		usage.CostUSD < 0 ||
		costMicros > float64(math.MaxInt64) {
		return parsedUsage{}, fmt.Errorf("%w: exact positive usage is required", ErrProbeOutput)
	}
	return parsedUsage{
		ActualModel:            actualModel,
		InputTokens:            usage.InputTokens,
		OutputTokens:           usage.OutputTokens,
		CacheReadInputTokens:   usage.CacheReadInputTokens,
		CacheCreateInputTokens: usage.CacheCreationTokens,
		TotalTokens:            total,
		CostUSDMicros:          int64(math.Round(costMicros)),
	}, nil
}

func containsCredentialField(line []byte) bool {
	var value any
	if err := json.Unmarshal(line, &value); err != nil {
		return true
	}
	var visit func(any) bool
	visit = func(current any) bool {
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
				switch normalized {
				case "authorization", "accesstoken", "refreshtoken", "apikey", "cookie":
					if text, ok := child.(string); !ok || strings.TrimSpace(text) != "" {
						return true
					}
				}
				if visit(child) {
					return true
				}
			}
		case []any:
			for _, child := range typed {
				if visit(child) {
					return true
				}
			}
		}
		return false
	}
	return visit(value)
}

func runCommand(ctx context.Context, req CommandRequest) (CommandResult, error) {
	runCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, req.Executable, req.Args...)
	cmd.Dir = req.Dir
	cmd.Stdin = strings.NewReader(req.Stdin)
	var stdout, stderr cappedBuffer
	stdout.limit = req.Limit
	stderr.limit = minInt(req.Limit, 64*1024)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := CommandResult{
		Stdout:   append([]byte(nil), stdout.Bytes()...),
		Stderr:   append([]byte(nil), stderr.Bytes()...),
		ExitCode: exitCode(err),
		TimedOut: errors.Is(runCtx.Err(), context.DeadlineExceeded),
		Overflow: stdout.overflow || stderr.overflow,
	}
	stdout.Zero()
	stderr.Zero()
	return result, err
}

type cappedBuffer struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (w *cappedBuffer) Write(p []byte) (int, error) {
	if w.limit <= 0 {
		w.limit = defaultOutputLimit
	}
	remaining := w.limit - w.buf.Len()
	if remaining <= 0 {
		w.overflow = true
		return len(p), nil
	}
	if len(p) > remaining {
		_, _ = w.buf.Write(p[:remaining])
		w.overflow = true
		return len(p), nil
	}
	return w.buf.Write(p)
}

func (w *cappedBuffer) Bytes() []byte { return w.buf.Bytes() }

func (w *cappedBuffer) Zero() {
	zeroBytes(w.buf.Bytes())
	w.buf.Reset()
}

func normalizeDeps(deps Deps) Deps {
	defaults := DefaultDeps()
	if deps.LookPath == nil {
		deps.LookPath = defaults.LookPath
	}
	if deps.Run == nil {
		deps.Run = defaults.Run
	}
	if deps.MkdirTemp == nil {
		deps.MkdirTemp = defaults.MkdirTemp
	}
	if deps.RemoveAll == nil {
		deps.RemoveAll = defaults.RemoveAll
	}
	return deps
}

func validEffort(value string) bool {
	switch value {
	case "low", "medium", "high", "xhigh", "max":
		return true
	default:
		return false
	}
}

func sanitizedExecutionError(result CommandResult, err error) error {
	switch {
	case result.TimedOut:
		return fmt.Errorf("%w: timeout", ErrProbeExecution)
	case result.Overflow:
		return fmt.Errorf("%w: output limit exceeded", ErrProbeExecution)
	case result.ExitCode != 0:
		return fmt.Errorf("%w: nonzero exit %d", ErrProbeExecution, result.ExitCode)
	case err != nil:
		return fmt.Errorf("%w: command failed", ErrProbeExecution)
	default:
		return fmt.Errorf("%w: unknown command failure", ErrProbeExecution)
	}
}

func executionFailureCode(result CommandResult) string {
	switch {
	case result.TimedOut:
		return "ErrClaudeCapabilityProbeTimeout"
	case result.Overflow:
		return "ErrClaudeCapabilityProbeOutputLimit"
	case result.ExitCode != 0:
		return "ErrClaudeCapabilityProbeNonZero"
	default:
		return "ErrClaudeCapabilityProbeExecution"
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func rawSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func zeroBytes(data []byte) {
	for i := range data {
		data[i] = 0
	}
}

func digestStrings(parts ...string) string {
	sum := sha256.New()
	for _, part := range parts {
		_, _ = io.WriteString(sum, part)
		_, _ = sum.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(sum.Sum(nil))
}

func digestBase32(parts ...string) string {
	sum := sha256.New()
	for _, part := range parts {
		_, _ = io.WriteString(sum, part)
		_, _ = sum.Write([]byte{0})
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum.Sum(nil)))
}

func argvDigest(executable string, args []string) string {
	values := append([]string{providerinstall.RedactedExecutableEvidence(executable)}, args...)
	return digestStrings(values...)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
