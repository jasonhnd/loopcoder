package simulation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

const diagnosticLimitBytes = 256

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`(?i)(sk|api[_-]?key|token|secret)[A-Za-z0-9_\-:=]{8,}`),
}

var scenarioFields = map[string]bool{
	"schema_version":     true,
	"scenario_id":        true,
	"seed":               true,
	"clock":              true,
	"limits":             true,
	"durable_source_ids": true,
	"policy_provenance":  true,
	"starting_state":     true,
	"inventory":          true,
	"budget_authorities": true,
	"tasks":              true,
	"injected_events":    true,
	"concurrency_script": true,
	"invariants":         true,
}

func DecodeScenarioJSON(data []byte) (Scenario, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var raw map[string]json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return Scenario{}, typedError(ErrInvalidFixture, "decode scenario: %v", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Scenario{}, typedError(ErrInvalidFixture, "decode scenario: multiple JSON values")
	}
	extensions := map[string]json.RawMessage{}
	for key, value := range raw {
		if scenarioFields[key] {
			continue
		}
		if strings.HasPrefix(key, "x_") {
			extensions[key] = append(json.RawMessage(nil), value...)
			delete(raw, key)
			continue
		}
		return Scenario{}, typedError(ErrInvalidFixture, "unknown scenario field %q", key)
	}
	canonical, err := json.Marshal(raw)
	if err != nil {
		return Scenario{}, typedError(ErrInvalidFixture, "marshal checked scenario: %v", err)
	}
	var scenario Scenario
	decoder = json.NewDecoder(bytes.NewReader(canonical))
	decoder.UseNumber()
	if err := decoder.Decode(&scenario); err != nil {
		return Scenario{}, typedError(ErrInvalidFixture, "decode checked scenario: %v", err)
	}
	if len(extensions) > 0 {
		scenario.Extensions = extensions
	}
	return scenario, nil
}

func Execute(ctx context.Context, scenario Scenario, opts Options) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	scenario.Limits = defaultLimits(scenario.Limits)
	if err := validateScenario(scenario); err != nil {
		return Result{}, err
	}
	if err := validateReplayJournal(scenario, opts.ReplayJournal); err != nil {
		return Result{}, err
	}
	origin, err := time.Parse(time.RFC3339Nano, scenario.Clock.Origin)
	if err != nil {
		return Result{}, typedError(ErrInvalidFixture, "invalid clock origin: %v", err)
	}
	ordered, err := orderEvents(scenario)
	if err != nil {
		return Result{}, err
	}
	beforeHash, _, err := delivery.DigestCanonicalJSON(scenario.StartingState)
	if err != nil {
		return Result{}, typedError(ErrInvalidFixture, "digest starting state: %v", err)
	}
	state := normalizeDurableState(scenario.StartingState)
	budgetRemaining, err := budgetRemaining(scenario, state.BudgetCommitments)
	if err != nil {
		return Result{}, err
	}
	run := runner{
		scenario:          scenario,
		origin:            origin.UTC(),
		state:             state,
		applied:           stringSet(state.AppliedEventIDs),
		receipts:          receiptSet(state.ProviderReceipts),
		receiptRecords:    receiptMap(state.ProviderReceipts),
		commitments:       commitmentSet(state.BudgetCommitments),
		handoffs:          handoffSet(state.Handoffs),
		owners:            ownerSet(state.AgentOwners),
		completed:         stringSet(state.CompletedTaskIDs),
		decisionReplay:    mapByEvent(opts.ReplayJournal),
		eventReplay:       eventMapByID(opts.ReplayJournal),
		budgetRemaining:   budgetRemaining,
		providerCallCount: providerCallCount(state.ProviderReceipts),
		decisionsByTask:   map[string]DecisionRecord{},
	}
	result := Result{
		SchemaVersion:    ResultSchemaV1,
		ScenarioID:       scenario.ScenarioID,
		Seed:             scenario.Seed,
		Clock:            scenario.Clock,
		StartedAt:        delivery.CanonicalTimestamp(origin),
		PolicyProvenance: scenario.PolicyProvenance,
		DurableSourceIDs: scenario.DurableSourceIDs,
		ReplayJournal: ReplayJournal{
			SchemaVersion: ReplayJournalSchemaV1,
			ScenarioID:    scenario.ScenarioID,
			Seed:          scenario.Seed,
			Events:        []EventRecord{},
			Decisions:     []DecisionRecord{},
		},
	}
	if scenario.Extensions != nil {
		result.Extensions = copyRawMap(scenario.Extensions)
	}
	for i, event := range ordered {
		if i >= scenario.Limits.MaxEvents || i >= scenario.Limits.MaxSteps {
			result.Truncated = true
			result.TruncationReason = "event-or-step-limit"
			break
		}
		if opts.CrashAfter > 0 && len(result.EventLog) >= opts.CrashAfter {
			result.Truncated = true
			result.TruncationReason = ErrCrashInjected
			result.Diagnostics = append(result.Diagnostics, diagnostic(ErrCrashInjected, "crash injected after %d event(s)", opts.CrashAfter))
			break
		}
		record, decision, err := run.apply(ctx, event, i+1)
		if err != nil {
			var typed *TypedError
			if asTypedError(err, &typed) {
				result.Diagnostics = append(result.Diagnostics, typed.Diagnostics...)
			} else {
				result.Diagnostics = append(result.Diagnostics, diagnostic(ErrInvalidFixture, "%v", err))
			}
			record.Status = "failed"
		}
		result.EventLog = append(result.EventLog, record)
		result.ReplayJournal.Events = append(result.ReplayJournal.Events, record)
		if decision.DecisionID != "" {
			result.Decisions = append(result.Decisions, decision)
			result.ReplayJournal.Decisions = append(result.ReplayJournal.Decisions, decision)
		}
	}
	result.DurableState = run.sortedState()
	result.InvariantResults = evaluateInvariants(scenario, result.DurableState, run.decisionsByTask)
	for _, invariant := range result.InvariantResults {
		if !invariant.Passed {
			result.Diagnostics = append(result.Diagnostics, diagnostic(ErrInvariantFailed, "%s: %s", invariant.InvariantID, invariant.Diagnostic))
		}
	}
	afterHash, _, err := delivery.DigestCanonicalJSON(result.DurableState)
	if err != nil {
		return Result{}, typedError(ErrInvalidFixture, "digest ending state: %v", err)
	}
	result.Diff = StableDiff{
		BeforeHash:       beforeHash,
		AfterHash:        afterHash,
		Changed:          beforeHash != afterHash,
		AddedEventIDs:    diffAdded(scenario.StartingState.AppliedEventIDs, result.DurableState.AppliedEventIDs),
		AddedDecisionIDs: decisionIDs(result.Decisions),
	}
	result.EndedAt = delivery.CanonicalTimestamp(origin.Add(time.Duration(len(result.EventLog)) * time.Duration(scenario.Clock.StepMS) * time.Millisecond))
	return normalizeResult(result), nil
}

func CanonicalResultJSON(result Result) ([]byte, error) {
	return delivery.CanonicalJSON(normalizeResult(result))
}

func IsolatedSubprocessEnv(base []string, home string) []string {
	blocked := map[string]bool{
		"GIT_DIR":        true,
		"GIT_WORK_TREE":  true,
		"GIT_INDEX_FILE": true,
		"GIT_COMMON_DIR": true,
	}
	values := map[string]string{}
	for _, entry := range base {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || blocked[key] {
			continue
		}
		values[key] = value
	}
	if strings.TrimSpace(home) != "" {
		values["HOME"] = home
		values["LOOPCODER_HOME"] = home
		values["XDG_CONFIG_HOME"] = home + "/config"
		values["XDG_DATA_HOME"] = home + "/data"
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out
}

type runner struct {
	scenario          Scenario
	origin            time.Time
	state             DurableState
	applied           map[string]bool
	receipts          map[string]bool
	receiptRecords    map[string]ProviderReceipt
	commitments       map[string]bool
	handoffs          map[string]bool
	owners            map[string]bool
	completed         map[string]bool
	decisionReplay    map[string]DecisionRecord
	eventReplay       map[string]EventRecord
	budgetRemaining   map[string]int64
	providerCallCount map[string]int
	decisionsByTask   map[string]DecisionRecord
}

func (r *runner) apply(ctx context.Context, event InjectedEvent, sequence int) (EventRecord, DecisionRecord, error) {
	if err := ctx.Err(); err != nil {
		return EventRecord{}, DecisionRecord{}, err
	}
	at := delivery.CanonicalTimestamp(r.origin.Add(time.Duration(sequence-1) * time.Duration(r.scenario.Clock.StepMS) * time.Millisecond))
	record := EventRecord{
		Sequence:       sequence,
		EventID:        event.EventID,
		EventKind:      event.Kind,
		TaskID:         event.TaskID,
		At:             at,
		Status:         "applied",
		IdempotencyKey: stableID("idem", r.scenario.ScenarioID, event.EventID),
	}
	if replay, ok := r.eventReplay[event.EventID]; ok {
		record = replay
		record.Sequence = sequence
		record.At = at
	}
	if r.applied[event.EventID] {
		record.Status = "replayed"
		return record, DecisionRecord{}, nil
	}
	var decision DecisionRecord
	var err error
	switch event.Kind {
	case "route_task":
		decision, err = r.route(event)
		record.DecisionID = decision.DecisionID
	case "provider_call":
		decision, err = r.route(event)
		record.DecisionID = decision.DecisionID
		if err == nil {
			record.ReceiptID, err = r.providerCall(event, decision)
		}
	case "budget_commit":
		decision, err = r.route(event)
		record.DecisionID = decision.DecisionID
		if err == nil {
			err = r.commitBudget(event, decision)
		}
	case "handoff":
		decision, err = r.route(event)
		record.DecisionID = decision.DecisionID
		if err == nil {
			err = r.handoff(event, decision)
		}
	case "agent_own":
		decision, err = r.route(event)
		record.DecisionID = decision.DecisionID
		if err == nil {
			err = r.own(event, decision)
		}
	default:
		err = typedError(ErrInvalidFixture, "unknown injected event kind %q", event.Kind)
	}
	if err == nil {
		r.applied[event.EventID] = true
		r.state.AppliedEventIDs = appendUnique(r.state.AppliedEventIDs, event.EventID)
	}
	return record, decision, err
}

func (r *runner) route(event InjectedEvent) (DecisionRecord, error) {
	if replay, ok := r.decisionReplay[event.EventID]; ok {
		r.decisionsByTask[replay.TaskID] = replay
		return replay, nil
	}
	task, ok := taskByID(r.scenario.Tasks, event.TaskID)
	if !ok {
		return DecisionRecord{}, typedError(ErrMissingReference, "event %s references missing task %s", event.EventID, event.TaskID)
	}
	decision := DecisionRecord{
		DecisionID:               stableID("decision", r.scenario.ScenarioID, event.EventID),
		EventID:                  event.EventID,
		TaskID:                   task.TaskID,
		DecisionKind:             "routing",
		RejectedCandidates:       []Rejection{},
		QuotaSnapshotIDs:         []string{},
		BudgetAuthorityID:        task.BudgetAuthorityID,
		PolicyFingerprint:        r.scenario.PolicyProvenance.PolicyFingerprint,
		PlanFingerprint:          r.scenario.PolicyProvenance.PlanFingerprint,
		AuthorizationFingerprint: r.scenario.PolicyProvenance.AuthorizationFingerprint,
		Reason:                   "no eligible candidate",
	}
	candidates := r.candidateModels(task)
	sort.SliceStable(candidates, func(i, j int) bool {
		return scoreKey(r.scenario.Seed, task.TaskID, candidates[i]) < scoreKey(r.scenario.Seed, task.TaskID, candidates[j])
	})
	for _, model := range candidates {
		rejection, quotaID, eligible := r.eligible(task, model)
		if quotaID != "" {
			decision.QuotaSnapshotIDs = appendUnique(decision.QuotaSnapshotIDs, quotaID)
		}
		if !eligible {
			decision.RejectedCandidates = append(decision.RejectedCandidates, rejection)
			continue
		}
		decision.Accepted = true
		decision.ChosenCandidateID = model.ModelCapabilityID
		decision.Reason = "selected deterministic eligible candidate"
		break
	}
	sortRejections(decision.RejectedCandidates)
	sort.Strings(decision.QuotaSnapshotIDs)
	r.decisionsByTask[task.TaskID] = decision
	if !decision.Accepted {
		return decision, typedError(ErrQuotaUnknown, "no eligible model for task %s", task.TaskID)
	}
	return decision, nil
}

func (r *runner) providerCall(event InjectedEvent, decision DecisionRecord) (string, error) {
	task, _ := taskByID(r.scenario.Tasks, event.TaskID)
	model, _ := modelByID(r.scenario.Inventory.Models, decision.ChosenCandidateID)
	receiptID := stableID("provider_receipt", r.scenario.ScenarioID, event.EventID, decision.ChosenCandidateID)
	if receipt, ok := r.receiptRecords[receiptID]; ok {
		if receipt.Status == "failed" {
			return receiptID, typedError(ErrProviderFailure, "simulated provider failure %s for model %s", receipt.FailureCode, decision.ChosenCandidateID)
		}
		return receiptID, nil
	}
	callKey := providerCallKey(event.TaskID, decision.ChosenCandidateID)
	r.providerCallCount[callKey]++
	failure := r.failure(decision.ChosenCandidateID, r.providerCallCount[callKey])
	status := "succeeded"
	failureCode := ""
	if failure.FailureCode != "" {
		status = "failed"
		failureCode = failure.FailureCode
	}
	receipt := ProviderReceipt{
		ReceiptID:         receiptID,
		EventID:           event.EventID,
		TaskID:            task.TaskID,
		ModelCapabilityID: decision.ChosenCandidateID,
		Status:            status,
		FailureCode:       failureCode,
		LatencyMS:         r.latency(decision.ChosenCandidateID),
		CostMicrounits:    max64(model.CostMicrounits, failure.CostMicrounits),
	}
	if r.receipts[receipt.ReceiptID] {
		return receipt.ReceiptID, nil
	}
	r.receipts[receipt.ReceiptID] = true
	r.receiptRecords[receipt.ReceiptID] = receipt
	r.state.ProviderReceipts = append(r.state.ProviderReceipts, receipt)
	if status == "succeeded" {
		r.completed[task.TaskID] = true
		r.state.CompletedTaskIDs = appendUnique(r.state.CompletedTaskIDs, task.TaskID)
		return receipt.ReceiptID, nil
	}
	return receipt.ReceiptID, typedError(ErrProviderFailure, "simulated provider failure %s for model %s", failureCode, decision.ChosenCandidateID)
}

func (r *runner) commitBudget(event InjectedEvent, decision DecisionRecord) error {
	task, _ := taskByID(r.scenario.Tasks, event.TaskID)
	id := stableID("budget_commitment", r.scenario.ScenarioID, event.EventID, task.BudgetAuthorityID)
	if r.commitments[id] {
		return nil
	}
	remaining, ok := r.budgetRemaining[task.BudgetAuthorityID]
	if !ok {
		return typedError(ErrMissingReference, "task %s references missing budget authority %s", task.TaskID, task.BudgetAuthorityID)
	}
	if remaining < task.RequiredQuantity {
		return typedError(ErrBudgetDenied, "budget authority %s has %d remaining, needs %d", task.BudgetAuthorityID, remaining, task.RequiredQuantity)
	}
	r.budgetRemaining[task.BudgetAuthorityID] = remaining - task.RequiredQuantity
	r.commitments[id] = true
	r.state.BudgetCommitments = append(r.state.BudgetCommitments, BudgetCommitment{
		CommitmentID:      id,
		EventID:           event.EventID,
		TaskID:            task.TaskID,
		BudgetAuthorityID: task.BudgetAuthorityID,
		QuantityKind:      string(task.QuantityKind),
		CommittedValue:    task.RequiredQuantity,
	})
	_ = decision
	return nil
}

func (r *runner) handoff(event InjectedEvent, decision DecisionRecord) error {
	task, _ := taskByID(r.scenario.Tasks, event.TaskID)
	if task.HandoffTargetTaskID == "" {
		return typedError(ErrMissingReference, "task %s has no handoff target", task.TaskID)
	}
	if _, ok := taskByID(r.scenario.Tasks, task.HandoffTargetTaskID); !ok {
		return typedError(ErrMissingReference, "task %s handoff target %s is missing", task.TaskID, task.HandoffTargetTaskID)
	}
	id := stableID("handoff", r.scenario.ScenarioID, event.EventID, task.TaskID, task.HandoffTargetTaskID)
	if r.handoffs[id] {
		return nil
	}
	r.handoffs[id] = true
	r.state.Handoffs = append(r.state.Handoffs, HandoffRecord{
		HandoffID:        id,
		EventID:          event.EventID,
		SourceTaskID:     task.TaskID,
		TargetTaskID:     task.HandoffTargetTaskID,
		AuthorizationRef: decision.AuthorizationFingerprint,
	})
	return nil
}

func (r *runner) own(event InjectedEvent, decision DecisionRecord) error {
	task, _ := taskByID(r.scenario.Tasks, event.TaskID)
	if task.AgentDepth > r.scenario.Limits.MaxDepth {
		return typedError(ErrBoundExceeded, "task %s depth %d exceeds max_depth %d", task.TaskID, task.AgentDepth, r.scenario.Limits.MaxDepth)
	}
	if len(r.state.AgentOwners) >= r.scenario.Limits.MaxBreadth {
		return typedError(ErrBoundExceeded, "agent owner count exceeds max_breadth %d", r.scenario.Limits.MaxBreadth)
	}
	resource := task.OwnerResourceKey
	if resource == "" {
		resource = task.TaskID
	}
	id := stableID("agent_owner", r.scenario.ScenarioID, resource)
	if r.owners[id] {
		return nil
	}
	r.owners[id] = true
	r.state.AgentOwners = append(r.state.AgentOwners, AgentOwner{
		OwnershipID:     id,
		EventID:         event.EventID,
		TaskID:          task.TaskID,
		ResourceKey:     resource,
		OwnerState:      storage.OwnershipStateHeld,
		Permission:      permissionForTask(task),
		SideEffectClass: sideEffectForTask(task),
	})
	_ = decision
	return nil
}

func (r *runner) candidateModels(task Task) []Model {
	allowed := stringSet(task.CandidateModelIDs)
	out := []Model{}
	for _, model := range r.scenario.Inventory.Models {
		if len(allowed) > 0 && !allowed[model.ModelCapabilityID] {
			continue
		}
		if !roleSupported(model.Roles, task.Role) {
			continue
		}
		out = append(out, model)
	}
	return out
}

func (r *runner) eligible(task Task, model Model) (Rejection, string, bool) {
	if model.Availability != providerinventory.AvailabilityAvailable {
		return reject(model.ModelCapabilityID, "model-unavailable", string(model.Availability)), "", false
	}
	account, ok := accountByID(r.scenario.Inventory.Accounts, model.AccountProfileID)
	if !ok || account.Readiness != providerinventory.ReadinessReady {
		return reject(model.ModelCapabilityID, "account-not-ready", "account readiness is not ready"), "", false
	}
	provider, ok := providerByID(r.scenario.Inventory.Providers, account.ProviderInstallationID)
	if !ok || provider.State != providerinventory.InstallationInstalled {
		return reject(model.ModelCapabilityID, "provider-not-installed", "provider installation is not installed"), "", false
	}
	quota, ok := quotaFor(r.scenario.Inventory.Quotas, task, model)
	if !ok {
		return reject(model.ModelCapabilityID, ErrQuotaUnknown, "quota evidence is missing"), "", false
	}
	if quota.Confidence != providerinventory.ConfidenceExact && quota.Confidence != providerinventory.ConfidenceEstimated {
		return reject(model.ModelCapabilityID, ErrQuotaUnknown, "quota confidence is not authoritative"), quota.QuotaSnapshotID, false
	}
	if quota.RemainingValue == nil {
		return reject(model.ModelCapabilityID, ErrQuotaUnknown, "quota remaining is unknown"), quota.QuotaSnapshotID, false
	}
	if *quota.RemainingValue < task.RequiredQuantity {
		return reject(model.ModelCapabilityID, ErrQuotaInsufficient, "quota remaining is insufficient"), quota.QuotaSnapshotID, false
	}
	return Rejection{}, quota.QuotaSnapshotID, true
}

func (r *runner) latency(modelID string) int64 {
	for _, latency := range r.scenario.Inventory.Latencies {
		if latency.ModelCapabilityID == modelID {
			return latency.LatencyMS
		}
	}
	return 0
}

func (r *runner) failure(modelID string, ordinal int) ProviderFailure {
	for _, failure := range r.scenario.Inventory.Failures {
		if failure.ModelCapabilityID == modelID && failure.AtCallOrdinal == ordinal {
			return failure
		}
	}
	return ProviderFailure{}
}

func (r *runner) sortedState() DurableState {
	state := normalizeDurableState(r.state)
	sort.Strings(state.AppliedEventIDs)
	sort.Strings(state.CompletedTaskIDs)
	sort.Slice(state.ProviderReceipts, func(i, j int) bool { return state.ProviderReceipts[i].ReceiptID < state.ProviderReceipts[j].ReceiptID })
	sort.Slice(state.BudgetCommitments, func(i, j int) bool {
		return state.BudgetCommitments[i].CommitmentID < state.BudgetCommitments[j].CommitmentID
	})
	sort.Slice(state.Handoffs, func(i, j int) bool { return state.Handoffs[i].HandoffID < state.Handoffs[j].HandoffID })
	sort.Slice(state.AgentOwners, func(i, j int) bool { return state.AgentOwners[i].OwnershipID < state.AgentOwners[j].OwnershipID })
	return state
}

func validateScenario(s Scenario) error {
	if s.SchemaVersion != ScenarioSchemaV1 {
		return typedError(ErrUnknownSchemaVersion, "unsupported scenario schema_version %q", s.SchemaVersion)
	}
	if s.ScenarioID == "" {
		return typedError(ErrInvalidFixture, "scenario_id is required")
	}
	if s.Clock.Origin == "" || s.Clock.StepMS <= 0 {
		return typedError(ErrInvalidFixture, "clock origin and positive step_ms are required")
	}
	if _, err := time.Parse(time.RFC3339Nano, s.Clock.Origin); err != nil {
		return typedError(ErrInvalidFixture, "invalid clock origin: %v", err)
	}
	if s.DurableSourceIDs.ProjectID == "" || s.DurableSourceIDs.DeliveryRunID == "" || s.PolicyProvenance.PolicyFingerprint == "" || s.PolicyProvenance.PlanFingerprint == "" {
		return typedError(ErrInvalidFixture, "durable source IDs and policy fingerprints are required")
	}
	providers := map[string]bool{}
	for _, provider := range s.Inventory.Providers {
		if provider.ProviderInstallationID == "" || provider.AdapterID == "" {
			return typedError(ErrInvalidFixture, "provider installation and adapter IDs are required")
		}
		if providers[provider.ProviderInstallationID] {
			return typedError(ErrInvalidFixture, "duplicate provider installation %s", provider.ProviderInstallationID)
		}
		providers[provider.ProviderInstallationID] = true
	}
	accounts := map[string]bool{}
	for _, account := range s.Inventory.Accounts {
		if account.AccountProfileID == "" || account.AdapterID == "" || account.ProviderInstallationID == "" {
			return typedError(ErrInvalidFixture, "account profile, adapter, and provider IDs are required")
		}
		if accounts[account.AccountProfileID] {
			return typedError(ErrInvalidFixture, "duplicate account profile %s", account.AccountProfileID)
		}
		if !providers[account.ProviderInstallationID] {
			return typedError(ErrMissingReference, "account %s references missing provider %s", account.AccountProfileID, account.ProviderInstallationID)
		}
		accounts[account.AccountProfileID] = true
	}
	models := map[string]bool{}
	for _, model := range s.Inventory.Models {
		if model.ModelCapabilityID == "" || model.AdapterID == "" || model.AccountProfileID == "" {
			return typedError(ErrInvalidFixture, "model capability, adapter, and account IDs are required")
		}
		if models[model.ModelCapabilityID] {
			return typedError(ErrInvalidFixture, "duplicate model capability %s", model.ModelCapabilityID)
		}
		if !accounts[model.AccountProfileID] {
			return typedError(ErrMissingReference, "model %s references missing account %s", model.ModelCapabilityID, model.AccountProfileID)
		}
		models[model.ModelCapabilityID] = true
	}
	quotas := map[string]bool{}
	for _, quota := range s.Inventory.Quotas {
		if quota.QuotaSnapshotID == "" || quota.ModelCapabilityID == "" || quota.AccountProfileID == "" {
			return typedError(ErrInvalidFixture, "quota snapshot, model, and account IDs are required")
		}
		if quotas[quota.QuotaSnapshotID] {
			return typedError(ErrInvalidFixture, "duplicate quota snapshot %s", quota.QuotaSnapshotID)
		}
		if !models[quota.ModelCapabilityID] {
			return typedError(ErrMissingReference, "quota %s references missing model %s", quota.QuotaSnapshotID, quota.ModelCapabilityID)
		}
		if !accounts[quota.AccountProfileID] {
			return typedError(ErrMissingReference, "quota %s references missing account %s", quota.QuotaSnapshotID, quota.AccountProfileID)
		}
		quotas[quota.QuotaSnapshotID] = true
	}
	budgets := map[string]bool{}
	budgetByID := map[string]BudgetAuthority{}
	for _, budget := range s.BudgetAuthorities {
		if budget.BudgetAuthorityID == "" {
			return typedError(ErrInvalidFixture, "budget authority ID is required")
		}
		if budgets[budget.BudgetAuthorityID] {
			return typedError(ErrInvalidFixture, "duplicate budget authority %s", budget.BudgetAuthorityID)
		}
		if budget.LimitValue < 0 || budget.RemainingValue < 0 || budget.RemainingValue > budget.LimitValue {
			return typedError(ErrInvalidFixture, "budget authority %s has invalid limit/remaining values", budget.BudgetAuthorityID)
		}
		if budget.Confidence != providerinventory.ConfidenceExact && budget.Confidence != providerinventory.ConfidenceEstimated {
			return typedError(ErrInvalidFixture, "budget authority %s must have exact or estimated confidence", budget.BudgetAuthorityID)
		}
		budgets[budget.BudgetAuthorityID] = true
		budgetByID[budget.BudgetAuthorityID] = budget
	}
	tasks := map[string]Task{}
	for _, task := range s.Tasks {
		if task.TaskID == "" || task.TaskKey == "" || task.BudgetAuthorityID == "" {
			return typedError(ErrInvalidFixture, "task ID, key, and budget authority are required")
		}
		if _, exists := tasks[task.TaskID]; exists {
			return typedError(ErrInvalidFixture, "duplicate task %s", task.TaskID)
		}
		if task.RequiredQuantity < 0 {
			return typedError(ErrInvalidFixture, "task %s has negative required quantity", task.TaskID)
		}
		if !budgets[task.BudgetAuthorityID] {
			return typedError(ErrMissingReference, "task %s references missing budget authority %s", task.TaskID, task.BudgetAuthorityID)
		}
		if budgetByID[task.BudgetAuthorityID].QuantityKind != task.QuantityKind {
			return typedError(ErrInvalidFixture, "task %s quantity kind %s does not match budget authority %s quantity kind %s", task.TaskID, task.QuantityKind, task.BudgetAuthorityID, budgetByID[task.BudgetAuthorityID].QuantityKind)
		}
		for _, candidate := range task.CandidateModelIDs {
			if !models[candidate] {
				return typedError(ErrMissingReference, "task %s references missing candidate model %s", task.TaskID, candidate)
			}
		}
		tasks[task.TaskID] = task
	}
	events := map[string]bool{}
	eventByID := map[string]InjectedEvent{}
	for _, event := range s.InjectedEvents {
		if event.EventID == "" || event.Kind == "" {
			return typedError(ErrInvalidFixture, "event ID and kind are required")
		}
		if events[event.EventID] {
			return typedError(ErrInvalidFixture, "duplicate event %s", event.EventID)
		}
		if event.TaskID != "" {
			if _, ok := tasks[event.TaskID]; !ok {
				return typedError(ErrMissingReference, "event %s references missing task %s", event.EventID, event.TaskID)
			}
		}
		events[event.EventID] = true
		eventByID[event.EventID] = event
	}
	for _, order := range s.ConcurrencyScript {
		ordered := map[string]bool{}
		for _, eventID := range order.EventIDs {
			if !events[eventID] {
				return typedError(ErrMissingReference, "concurrency group %s references missing event %s", order.Group, eventID)
			}
			key := order.Group + "\x00" + eventID
			if ordered[key] {
				return typedError(ErrInvalidFixture, "concurrency group %s repeats event %s", order.Group, eventID)
			}
			ordered[key] = true
		}
	}
	if err := validateStartingState(s, tasks, models, budgetByID, eventByID); err != nil {
		return err
	}
	return nil
}

func validateReplayJournal(s Scenario, journal *ReplayJournal) error {
	if journal == nil {
		return nil
	}
	if journal.SchemaVersion != ReplayJournalSchemaV1 {
		return typedError(ErrReplayMismatch, "unsupported replay journal schema_version %q", journal.SchemaVersion)
	}
	if journal.ScenarioID != s.ScenarioID {
		return typedError(ErrReplayMismatch, "replay journal scenario_id %q does not match %q", journal.ScenarioID, s.ScenarioID)
	}
	if journal.Seed != s.Seed {
		return typedError(ErrReplayMismatch, "replay journal seed %d does not match %d", journal.Seed, s.Seed)
	}
	tasks := taskMap(s.Tasks)
	models := modelSet(s.Inventory.Models)
	budgets := budgetSet(s.BudgetAuthorities)
	events := eventMap(s.InjectedEvents)
	eventRecords := map[string]EventRecord{}
	for _, event := range journal.Events {
		if event.EventID == "" {
			return typedError(ErrReplayMismatch, "replay journal contains event with empty event_id")
		}
		if _, exists := eventRecords[event.EventID]; exists {
			return typedError(ErrReplayMismatch, "replay journal contains duplicate event %s", event.EventID)
		}
		fixtureEvent, ok := events[event.EventID]
		if !ok {
			return typedError(ErrReplayMismatch, "replay journal event %s is not in scenario", event.EventID)
		}
		if event.EventKind != fixtureEvent.Kind {
			return typedError(ErrReplayMismatch, "replay journal event %s kind %q does not match %q", event.EventID, event.EventKind, fixtureEvent.Kind)
		}
		if event.TaskID != fixtureEvent.TaskID {
			return typedError(ErrReplayMismatch, "replay journal event %s task %q does not match %q", event.EventID, event.TaskID, fixtureEvent.TaskID)
		}
		eventRecords[event.EventID] = event
	}
	decisionsByEvent := map[string]DecisionRecord{}
	decisionIDs := map[string]bool{}
	for _, decision := range journal.Decisions {
		if decision.DecisionID == "" || decision.EventID == "" {
			return typedError(ErrReplayMismatch, "replay journal decision ID and event ID are required")
		}
		if decisionIDs[decision.DecisionID] {
			return typedError(ErrReplayMismatch, "replay journal contains duplicate decision %s", decision.DecisionID)
		}
		decisionIDs[decision.DecisionID] = true
		if _, exists := decisionsByEvent[decision.EventID]; exists {
			return typedError(ErrReplayMismatch, "replay journal contains duplicate decision for event %s", decision.EventID)
		}
		event, ok := events[decision.EventID]
		if !ok {
			return typedError(ErrReplayMismatch, "replay journal decision %s references missing event %s", decision.DecisionID, decision.EventID)
		}
		task, ok := tasks[decision.TaskID]
		if !ok {
			return typedError(ErrReplayMismatch, "replay journal decision %s references missing task %s", decision.DecisionID, decision.TaskID)
		}
		if event.TaskID != "" && event.TaskID != decision.TaskID {
			return typedError(ErrReplayMismatch, "replay journal decision %s task %s does not match event task %s", decision.DecisionID, decision.TaskID, event.TaskID)
		}
		if decision.BudgetAuthorityID != "" && (!budgets[decision.BudgetAuthorityID] || decision.BudgetAuthorityID != task.BudgetAuthorityID) {
			return typedError(ErrReplayMismatch, "replay journal decision %s references invalid budget authority %s", decision.DecisionID, decision.BudgetAuthorityID)
		}
		if decision.ChosenCandidateID != "" {
			if !models[decision.ChosenCandidateID] || !taskAllowsModel(task, decision.ChosenCandidateID) {
				return typedError(ErrReplayMismatch, "replay journal decision %s references invalid chosen candidate %s", decision.DecisionID, decision.ChosenCandidateID)
			}
		}
		for _, rejection := range decision.RejectedCandidates {
			if rejection.CandidateID != "" && !models[rejection.CandidateID] {
				return typedError(ErrReplayMismatch, "replay journal decision %s references invalid rejected candidate %s", decision.DecisionID, rejection.CandidateID)
			}
		}
		decisionsByEvent[decision.EventID] = decision
	}
	for _, event := range journal.Events {
		if event.DecisionID == "" {
			continue
		}
		decision, ok := decisionsByEvent[event.EventID]
		if !ok || decision.DecisionID != event.DecisionID {
			return typedError(ErrReplayMismatch, "replay journal event %s decision %s has no matching decision", event.EventID, event.DecisionID)
		}
	}
	if err := validateReplayAgainstStartingState(s.StartingState, eventRecords); err != nil {
		return err
	}
	return nil
}

func orderEvents(s Scenario) ([]InjectedEvent, error) {
	events := append([]InjectedEvent(nil), s.InjectedEvents...)
	positions := map[string]int{}
	for _, order := range s.ConcurrencyScript {
		for index, eventID := range order.EventIDs {
			positions[order.Group+"\x00"+eventID] = index
		}
	}
	sort.SliceStable(events, func(i, j int) bool {
		left, right := events[i], events[j]
		if left.ConcurrencyGroup != right.ConcurrencyGroup {
			return left.ConcurrencyGroup < right.ConcurrencyGroup
		}
		if left.ConcurrencyGroup != "" {
			li, lok := positions[left.ConcurrencyGroup+"\x00"+left.EventID]
			ri, rok := positions[right.ConcurrencyGroup+"\x00"+right.EventID]
			if lok && rok && li != ri {
				return li < ri
			}
			if lok != rok {
				return lok
			}
		}
		leftKey := hashText(strconv.FormatInt(s.Seed, 10), left.ConcurrencyGroup, left.EventID)
		rightKey := hashText(strconv.FormatInt(s.Seed, 10), right.ConcurrencyGroup, right.EventID)
		if leftKey != rightKey {
			return leftKey < rightKey
		}
		return left.EventID < right.EventID
	})
	return events, nil
}

func evaluateInvariants(s Scenario, state DurableState, decisions map[string]DecisionRecord) []InvariantResult {
	out := make([]InvariantResult, 0, len(s.Invariants))
	completed := stringSet(state.CompletedTaskIDs)
	ownersByResource := map[string]int{}
	for _, owner := range state.AgentOwners {
		ownersByResource[owner.ResourceKey]++
	}
	for _, invariant := range s.Invariants {
		result := InvariantResult{InvariantID: invariant.InvariantID, Kind: invariant.Kind, Passed: true}
		switch invariant.Kind {
		case "task_completed":
			result.Passed = completed[invariant.TaskID]
			if !result.Passed {
				result.Diagnostic = "task was not completed"
			}
		case "route_accepted":
			decision := decisions[invariant.TaskID]
			result.Passed = decision.Accepted
			if !result.Passed {
				result.Diagnostic = "route was not accepted"
			}
		case "one_owner_per_resource":
			for resource, count := range ownersByResource {
				if count > 1 {
					result.Passed = false
					result.Diagnostic = "resource has duplicate owner: " + resource
					break
				}
			}
		default:
			result.Passed = false
			result.Diagnostic = "unknown invariant kind"
		}
		out = append(out, result)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].InvariantID < out[j].InvariantID })
	return out
}

func normalizeResult(result Result) Result {
	sort.Slice(result.EventLog, func(i, j int) bool {
		if result.EventLog[i].Sequence != result.EventLog[j].Sequence {
			return result.EventLog[i].Sequence < result.EventLog[j].Sequence
		}
		return result.EventLog[i].EventID < result.EventLog[j].EventID
	})
	sort.Slice(result.Decisions, func(i, j int) bool { return result.Decisions[i].DecisionID < result.Decisions[j].DecisionID })
	sort.Slice(result.InvariantResults, func(i, j int) bool {
		return result.InvariantResults[i].InvariantID < result.InvariantResults[j].InvariantID
	})
	result.DurableState = normalizeDurableState(result.DurableState)
	result.ReplayJournal.Events = append([]EventRecord(nil), result.EventLog...)
	result.ReplayJournal.Decisions = append([]DecisionRecord(nil), result.Decisions...)
	sort.Slice(result.Diagnostics, func(i, j int) bool {
		if result.Diagnostics[i].Code != result.Diagnostics[j].Code {
			return result.Diagnostics[i].Code < result.Diagnostics[j].Code
		}
		return result.Diagnostics[i].Message < result.Diagnostics[j].Message
	})
	return result
}

func normalizeDurableState(state DurableState) DurableState {
	state.AppliedEventIDs = sortedUnique(state.AppliedEventIDs)
	state.CompletedTaskIDs = sortedUnique(state.CompletedTaskIDs)
	sort.Slice(state.ProviderReceipts, func(i, j int) bool { return state.ProviderReceipts[i].ReceiptID < state.ProviderReceipts[j].ReceiptID })
	sort.Slice(state.BudgetCommitments, func(i, j int) bool {
		return state.BudgetCommitments[i].CommitmentID < state.BudgetCommitments[j].CommitmentID
	})
	sort.Slice(state.Handoffs, func(i, j int) bool { return state.Handoffs[i].HandoffID < state.Handoffs[j].HandoffID })
	sort.Slice(state.AgentOwners, func(i, j int) bool { return state.AgentOwners[i].OwnershipID < state.AgentOwners[j].OwnershipID })
	return state
}

func validateStartingState(s Scenario, tasks map[string]Task, models map[string]bool, budgets map[string]BudgetAuthority, events map[string]InjectedEvent) error {
	applied := map[string]bool{}
	for _, eventID := range s.StartingState.AppliedEventIDs {
		if eventID == "" {
			return typedError(ErrInvalidFixture, "starting state contains empty applied event ID")
		}
		if applied[eventID] {
			return typedError(ErrInvalidFixture, "starting state contains duplicate applied event %s", eventID)
		}
		if _, ok := events[eventID]; !ok {
			return typedError(ErrMissingReference, "starting state applied event %s is not in scenario", eventID)
		}
		applied[eventID] = true
	}
	completed := map[string]bool{}
	for _, taskID := range s.StartingState.CompletedTaskIDs {
		if taskID == "" {
			return typedError(ErrInvalidFixture, "starting state contains empty completed task ID")
		}
		if completed[taskID] {
			return typedError(ErrInvalidFixture, "starting state contains duplicate completed task %s", taskID)
		}
		if _, ok := tasks[taskID]; !ok {
			return typedError(ErrMissingReference, "starting state completed task %s is not in scenario", taskID)
		}
		completed[taskID] = true
	}
	receipts := map[string]bool{}
	receiptSemantics := map[string]bool{}
	succeededTasks := map[string]bool{}
	for _, receipt := range s.StartingState.ProviderReceipts {
		if receipt.ReceiptID == "" || receipt.EventID == "" || receipt.TaskID == "" || receipt.ModelCapabilityID == "" {
			return typedError(ErrInvalidFixture, "starting state provider receipt identity fields are required")
		}
		if receipts[receipt.ReceiptID] {
			return typedError(ErrInvalidFixture, "starting state contains duplicate provider receipt %s", receipt.ReceiptID)
		}
		receipts[receipt.ReceiptID] = true
		semantic := strings.Join([]string{receipt.EventID, receipt.TaskID, receipt.ModelCapabilityID}, "\x00")
		if receiptSemantics[semantic] {
			return typedError(ErrInvalidFixture, "starting state contains duplicate semantic provider receipt for event %s task %s model %s", receipt.EventID, receipt.TaskID, receipt.ModelCapabilityID)
		}
		receiptSemantics[semantic] = true
		event, ok := events[receipt.EventID]
		if !ok {
			return typedError(ErrMissingReference, "provider receipt %s references missing event %s", receipt.ReceiptID, receipt.EventID)
		}
		if event.Kind != "provider_call" {
			return typedError(ErrInvalidFixture, "provider receipt %s references non-provider event %s", receipt.ReceiptID, receipt.EventID)
		}
		if event.TaskID != receipt.TaskID {
			return typedError(ErrInvalidFixture, "provider receipt %s task %s does not match event task %s", receipt.ReceiptID, receipt.TaskID, event.TaskID)
		}
		task, ok := tasks[receipt.TaskID]
		if !ok {
			return typedError(ErrMissingReference, "provider receipt %s references missing task %s", receipt.ReceiptID, receipt.TaskID)
		}
		if !models[receipt.ModelCapabilityID] || !taskAllowsModel(task, receipt.ModelCapabilityID) {
			return typedError(ErrMissingReference, "provider receipt %s references invalid model %s for task %s", receipt.ReceiptID, receipt.ModelCapabilityID, receipt.TaskID)
		}
		switch receipt.Status {
		case "succeeded":
			if receipt.FailureCode != "" {
				return typedError(ErrInvalidFixture, "succeeded provider receipt %s has failure code", receipt.ReceiptID)
			}
			if !applied[receipt.EventID] {
				return typedError(ErrInvalidFixture, "succeeded provider receipt %s event %s is not applied", receipt.ReceiptID, receipt.EventID)
			}
			succeededTasks[receipt.TaskID] = true
		case "failed":
			if receipt.FailureCode == "" {
				return typedError(ErrInvalidFixture, "failed provider receipt %s is missing failure code", receipt.ReceiptID)
			}
			if applied[receipt.EventID] {
				return typedError(ErrInvalidFixture, "failed provider receipt %s event %s is marked applied", receipt.ReceiptID, receipt.EventID)
			}
		default:
			return typedError(ErrInvalidFixture, "provider receipt %s has invalid status %q", receipt.ReceiptID, receipt.Status)
		}
	}
	for taskID := range completed {
		if !succeededTasks[taskID] {
			return typedError(ErrInvalidFixture, "completed task %s has no succeeded provider receipt", taskID)
		}
	}
	for taskID := range succeededTasks {
		if !completed[taskID] {
			return typedError(ErrInvalidFixture, "task %s has succeeded provider receipt but is not completed", taskID)
		}
	}
	if _, err := budgetRemaining(s, s.StartingState.BudgetCommitments); err != nil {
		return err
	}
	commitments := map[string]bool{}
	commitmentSemantics := map[string]bool{}
	for _, commitment := range s.StartingState.BudgetCommitments {
		if commitment.CommitmentID == "" || commitment.EventID == "" || commitment.TaskID == "" || commitment.BudgetAuthorityID == "" {
			return typedError(ErrInvalidFixture, "starting state budget commitment identity fields are required")
		}
		if commitments[commitment.CommitmentID] {
			return typedError(ErrInvalidFixture, "starting state contains duplicate budget commitment %s", commitment.CommitmentID)
		}
		commitments[commitment.CommitmentID] = true
		semantic := strings.Join([]string{commitment.EventID, commitment.TaskID, commitment.BudgetAuthorityID}, "\x00")
		if commitmentSemantics[semantic] {
			return typedError(ErrInvalidFixture, "starting state contains duplicate semantic budget commitment for event %s task %s authority %s", commitment.EventID, commitment.TaskID, commitment.BudgetAuthorityID)
		}
		commitmentSemantics[semantic] = true
		event, ok := events[commitment.EventID]
		if !ok {
			return typedError(ErrMissingReference, "budget commitment %s references missing event %s", commitment.CommitmentID, commitment.EventID)
		}
		if event.Kind != "budget_commit" {
			return typedError(ErrInvalidFixture, "budget commitment %s references non-budget event %s", commitment.CommitmentID, commitment.EventID)
		}
		if event.TaskID != commitment.TaskID {
			return typedError(ErrInvalidFixture, "budget commitment %s task %s does not match event task %s", commitment.CommitmentID, commitment.TaskID, event.TaskID)
		}
		if !applied[commitment.EventID] {
			return typedError(ErrInvalidFixture, "budget commitment %s event %s is not applied", commitment.CommitmentID, commitment.EventID)
		}
		task, ok := tasks[commitment.TaskID]
		if !ok {
			return typedError(ErrMissingReference, "budget commitment %s references missing task %s", commitment.CommitmentID, commitment.TaskID)
		}
		budget, ok := budgets[commitment.BudgetAuthorityID]
		if !ok {
			return typedError(ErrMissingReference, "budget commitment %s references missing budget authority %s", commitment.CommitmentID, commitment.BudgetAuthorityID)
		}
		if task.BudgetAuthorityID != commitment.BudgetAuthorityID {
			return typedError(ErrInvalidFixture, "budget commitment %s authority %s does not match task authority %s", commitment.CommitmentID, commitment.BudgetAuthorityID, task.BudgetAuthorityID)
		}
		if commitment.QuantityKind != string(task.QuantityKind) || budget.QuantityKind != task.QuantityKind {
			return typedError(ErrInvalidFixture, "budget commitment %s quantity kind %s does not match task/budget quantity kind %s", commitment.CommitmentID, commitment.QuantityKind, task.QuantityKind)
		}
		if commitment.CommittedValue != task.RequiredQuantity {
			return typedError(ErrInvalidFixture, "budget commitment %s committed value %d does not match task required quantity %d", commitment.CommitmentID, commitment.CommittedValue, task.RequiredQuantity)
		}
	}
	handoffs := map[string]bool{}
	handoffSemantics := map[string]bool{}
	for _, handoff := range s.StartingState.Handoffs {
		if handoff.HandoffID == "" || handoff.EventID == "" || handoff.SourceTaskID == "" || handoff.TargetTaskID == "" {
			return typedError(ErrInvalidFixture, "starting state handoff identity fields are required")
		}
		if handoffs[handoff.HandoffID] {
			return typedError(ErrInvalidFixture, "starting state contains duplicate handoff %s", handoff.HandoffID)
		}
		handoffs[handoff.HandoffID] = true
		semantic := strings.Join([]string{handoff.EventID, handoff.SourceTaskID, handoff.TargetTaskID}, "\x00")
		if handoffSemantics[semantic] {
			return typedError(ErrInvalidFixture, "starting state contains duplicate semantic handoff for event %s source %s target %s", handoff.EventID, handoff.SourceTaskID, handoff.TargetTaskID)
		}
		handoffSemantics[semantic] = true
		event, ok := events[handoff.EventID]
		if !ok {
			return typedError(ErrMissingReference, "handoff %s references missing event %s", handoff.HandoffID, handoff.EventID)
		}
		if event.Kind != "handoff" || event.TaskID != handoff.SourceTaskID {
			return typedError(ErrInvalidFixture, "handoff %s does not match event %s", handoff.HandoffID, handoff.EventID)
		}
		if !applied[handoff.EventID] {
			return typedError(ErrInvalidFixture, "handoff %s event %s is not applied", handoff.HandoffID, handoff.EventID)
		}
		source, ok := tasks[handoff.SourceTaskID]
		if !ok {
			return typedError(ErrMissingReference, "handoff %s references missing source task %s", handoff.HandoffID, handoff.SourceTaskID)
		}
		if _, ok := tasks[handoff.TargetTaskID]; !ok {
			return typedError(ErrMissingReference, "handoff %s references missing target task %s", handoff.HandoffID, handoff.TargetTaskID)
		}
		if source.HandoffTargetTaskID != handoff.TargetTaskID {
			return typedError(ErrInvalidFixture, "handoff %s target %s does not match source task target %s", handoff.HandoffID, handoff.TargetTaskID, source.HandoffTargetTaskID)
		}
	}
	owners := map[string]bool{}
	ownerSemantics := map[string]bool{}
	for _, owner := range s.StartingState.AgentOwners {
		if owner.OwnershipID == "" || owner.EventID == "" || owner.TaskID == "" || owner.ResourceKey == "" {
			return typedError(ErrInvalidFixture, "starting state agent owner identity fields are required")
		}
		if owners[owner.OwnershipID] {
			return typedError(ErrInvalidFixture, "starting state contains duplicate agent owner %s", owner.OwnershipID)
		}
		owners[owner.OwnershipID] = true
		semantic := owner.ResourceKey
		if ownerSemantics[semantic] {
			return typedError(ErrInvalidFixture, "starting state contains duplicate semantic agent owner for resource %s", owner.ResourceKey)
		}
		ownerSemantics[semantic] = true
		event, ok := events[owner.EventID]
		if !ok {
			return typedError(ErrMissingReference, "agent owner %s references missing event %s", owner.OwnershipID, owner.EventID)
		}
		if event.Kind != "agent_own" || event.TaskID != owner.TaskID {
			return typedError(ErrInvalidFixture, "agent owner %s does not match event %s", owner.OwnershipID, owner.EventID)
		}
		if !applied[owner.EventID] {
			return typedError(ErrInvalidFixture, "agent owner %s event %s is not applied", owner.OwnershipID, owner.EventID)
		}
		task, ok := tasks[owner.TaskID]
		if !ok {
			return typedError(ErrMissingReference, "agent owner %s references missing task %s", owner.OwnershipID, owner.TaskID)
		}
		resource := task.OwnerResourceKey
		if resource == "" {
			resource = task.TaskID
		}
		if owner.ResourceKey != resource {
			return typedError(ErrInvalidFixture, "agent owner %s resource %s does not match task resource %s", owner.OwnershipID, owner.ResourceKey, resource)
		}
	}
	return nil
}

func validateReplayAgainstStartingState(state DurableState, replayEvents map[string]EventRecord) error {
	for _, eventID := range state.AppliedEventIDs {
		if _, ok := replayEvents[eventID]; !ok {
			return typedError(ErrReplayMismatch, "starting state applied event %s is missing from replay journal", eventID)
		}
	}
	for _, receipt := range state.ProviderReceipts {
		event, ok := replayEvents[receipt.EventID]
		if !ok {
			return typedError(ErrReplayMismatch, "starting state provider receipt %s event %s is missing from replay journal", receipt.ReceiptID, receipt.EventID)
		}
		if event.ReceiptID != receipt.ReceiptID {
			return typedError(ErrReplayMismatch, "replay journal event %s receipt %s does not match durable receipt %s", receipt.EventID, event.ReceiptID, receipt.ReceiptID)
		}
	}
	for _, commitment := range state.BudgetCommitments {
		if _, ok := replayEvents[commitment.EventID]; !ok {
			return typedError(ErrReplayMismatch, "starting state budget commitment %s event %s is missing from replay journal", commitment.CommitmentID, commitment.EventID)
		}
	}
	for _, handoff := range state.Handoffs {
		if _, ok := replayEvents[handoff.EventID]; !ok {
			return typedError(ErrReplayMismatch, "starting state handoff %s event %s is missing from replay journal", handoff.HandoffID, handoff.EventID)
		}
	}
	for _, owner := range state.AgentOwners {
		if _, ok := replayEvents[owner.EventID]; !ok {
			return typedError(ErrReplayMismatch, "starting state agent owner %s event %s is missing from replay journal", owner.OwnershipID, owner.EventID)
		}
	}
	return nil
}

func typedError(code, format string, args ...any) error {
	return &TypedError{Code: code, Diagnostics: []Diagnostic{diagnostic(code, format, args...)}}
}

func diagnostic(code, format string, args ...any) Diagnostic {
	return Diagnostic{Code: code, Message: boundDiagnostic(redact(fmt.Sprintf(format, args...)))}
}

func redact(value string) string {
	for _, pattern := range secretPatterns {
		value = pattern.ReplaceAllString(value, "[REDACTED]")
	}
	return value
}

func boundDiagnostic(value string) string {
	if len(value) <= diagnosticLimitBytes {
		return value
	}
	cut := diagnosticLimitBytes
	for cut > 0 && (value[cut]&0xc0) == 0x80 {
		cut--
	}
	return value[:cut] + "...[truncated]"
}

func asTypedError(err error, target **TypedError) bool {
	typed, ok := err.(*TypedError)
	if ok {
		*target = typed
	}
	return ok
}

func stableID(prefix string, parts ...string) string {
	return prefix + "_" + hashText(parts...)[:32]
}

func hashText(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func scoreKey(seed int64, taskID string, model Model) string {
	return hashText(strconv.FormatInt(seed, 10), taskID, model.AdapterID, model.AccountProfileID, model.ModelCapabilityID, strconv.FormatInt(model.CostMicrounits, 10))
}

func reject(candidateID, code, reason string) Rejection {
	return Rejection{CandidateID: candidateID, Code: code, Reason: reason}
}

func sortRejections(rejections []Rejection) {
	sort.Slice(rejections, func(i, j int) bool {
		if rejections[i].CandidateID != rejections[j].CandidateID {
			return rejections[i].CandidateID < rejections[j].CandidateID
		}
		return rejections[i].Code < rejections[j].Code
	})
}

func sortedUnique(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func stringSet(values []string) map[string]bool {
	out := map[string]bool{}
	for _, value := range values {
		out[value] = true
	}
	return out
}

func receiptSet(values []ProviderReceipt) map[string]bool {
	out := map[string]bool{}
	for _, value := range values {
		out[value.ReceiptID] = true
	}
	return out
}

func receiptMap(values []ProviderReceipt) map[string]ProviderReceipt {
	out := map[string]ProviderReceipt{}
	for _, value := range values {
		out[value.ReceiptID] = value
	}
	return out
}

func commitmentSet(values []BudgetCommitment) map[string]bool {
	out := map[string]bool{}
	for _, value := range values {
		out[value.CommitmentID] = true
	}
	return out
}

func handoffSet(values []HandoffRecord) map[string]bool {
	out := map[string]bool{}
	for _, value := range values {
		out[value.HandoffID] = true
	}
	return out
}

func ownerSet(values []AgentOwner) map[string]bool {
	out := map[string]bool{}
	for _, value := range values {
		out[value.OwnershipID] = true
	}
	return out
}

func budgetRemaining(s Scenario, commitments []BudgetCommitment) (map[string]int64, error) {
	out := map[string]int64{}
	for _, value := range s.BudgetAuthorities {
		if value.LimitValue < 0 || value.RemainingValue < 0 || value.RemainingValue > value.LimitValue {
			return nil, typedError(ErrInvalidFixture, "budget authority %s has invalid limit/remaining values", value.BudgetAuthorityID)
		}
		out[value.BudgetAuthorityID] = value.RemainingValue
	}
	for _, commitment := range commitments {
		remaining, ok := out[commitment.BudgetAuthorityID]
		if !ok {
			return nil, typedError(ErrMissingReference, "budget commitment %s references missing budget authority %s", commitment.CommitmentID, commitment.BudgetAuthorityID)
		}
		if commitment.CommittedValue < 0 {
			return nil, typedError(ErrInvalidFixture, "budget commitment %s has negative committed value %d", commitment.CommitmentID, commitment.CommittedValue)
		}
		if remaining < commitment.CommittedValue {
			return nil, typedError(ErrBudgetDenied, "budget authority %s has %d remaining before durable commitment %s needs %d", commitment.BudgetAuthorityID, remaining, commitment.CommitmentID, commitment.CommittedValue)
		}
		out[commitment.BudgetAuthorityID] = remaining - commitment.CommittedValue
	}
	return out, nil
}

func providerCallCount(receipts []ProviderReceipt) map[string]int {
	out := map[string]int{}
	for _, receipt := range receipts {
		out[providerCallKey(receipt.TaskID, receipt.ModelCapabilityID)]++
	}
	return out
}

func providerCallKey(taskID, modelID string) string {
	return taskID + "\x00" + modelID
}

func mapByEvent(journal *ReplayJournal) map[string]DecisionRecord {
	out := map[string]DecisionRecord{}
	if journal == nil {
		return out
	}
	for _, decision := range journal.Decisions {
		out[decision.EventID] = decision
	}
	return out
}

func eventMapByID(journal *ReplayJournal) map[string]EventRecord {
	out := map[string]EventRecord{}
	if journal == nil {
		return out
	}
	for _, event := range journal.Events {
		out[event.EventID] = event
	}
	return out
}

func taskMap(tasks []Task) map[string]Task {
	out := map[string]Task{}
	for _, task := range tasks {
		out[task.TaskID] = task
	}
	return out
}

func modelSet(models []Model) map[string]bool {
	out := map[string]bool{}
	for _, model := range models {
		out[model.ModelCapabilityID] = true
	}
	return out
}

func budgetSet(budgets []BudgetAuthority) map[string]bool {
	out := map[string]bool{}
	for _, budget := range budgets {
		out[budget.BudgetAuthorityID] = true
	}
	return out
}

func eventMap(events []InjectedEvent) map[string]InjectedEvent {
	out := map[string]InjectedEvent{}
	for _, event := range events {
		out[event.EventID] = event
	}
	return out
}

func taskAllowsModel(task Task, modelID string) bool {
	if len(task.CandidateModelIDs) == 0 {
		return true
	}
	for _, candidate := range task.CandidateModelIDs {
		if candidate == modelID {
			return true
		}
	}
	return false
}

func taskByID(tasks []Task, id string) (Task, bool) {
	for _, task := range tasks {
		if task.TaskID == id {
			return task, true
		}
	}
	return Task{}, false
}

func modelByID(models []Model, id string) (Model, bool) {
	for _, model := range models {
		if model.ModelCapabilityID == id {
			return model, true
		}
	}
	return Model{}, false
}

func accountByID(accounts []Account, id string) (Account, bool) {
	for _, account := range accounts {
		if account.AccountProfileID == id {
			return account, true
		}
	}
	return Account{}, false
}

func providerByID(providers []Provider, id string) (Provider, bool) {
	for _, provider := range providers {
		if provider.ProviderInstallationID == id {
			return provider, true
		}
	}
	return Provider{}, false
}

func quotaFor(quotas []QuotaWindow, task Task, model Model) (QuotaWindow, bool) {
	var found QuotaWindow
	ok := false
	for _, quota := range quotas {
		if quota.ModelCapabilityID != model.ModelCapabilityID || quota.AccountProfileID != model.AccountProfileID || quota.QuantityKind != task.QuantityKind {
			continue
		}
		if !ok || quota.QuotaSnapshotID < found.QuotaSnapshotID {
			found = quota
			ok = true
		}
	}
	return found, ok
}

func roleSupported(roles []providerinventory.CatalogRole, role providerinventory.CatalogRole) bool {
	for _, existing := range roles {
		if existing == role {
			return true
		}
	}
	return false
}

func diffAdded(before, after []string) []string {
	seen := stringSet(before)
	out := []string{}
	for _, value := range after {
		if !seen[value] {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func decisionIDs(decisions []DecisionRecord) []string {
	out := make([]string, 0, len(decisions))
	for _, decision := range decisions {
		out = append(out, decision.DecisionID)
	}
	sort.Strings(out)
	return out
}

func copyRawMap(values map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(values))
	for key, value := range values {
		out[key] = append(json.RawMessage(nil), value...)
	}
	return out
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
