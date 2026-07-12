package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	AgentRegistrationSchemaV1       = "loopcoder.agent_registration.v1"
	AgentScopeGrantSchemaV1         = "loopcoder.agent_scope_grant.v1"
	AgentFederationPolicyVersionV1  = "0805.agent_federation.v1"
	AgentRegistrationStateActive    = "active"
	AgentRegistrationStateCompleted = "completed"

	maxProviderSessionRefBytes = 512
)

var (
	ErrAgentRegistrationRequired = errors.New("ErrAgentRegistrationRequired")
	ErrAgentRegistrationConflict = errors.New("ErrAgentRegistrationConflict")
	ErrUnsupportedNativeSubAgent = errors.New("ErrUnsupportedNativeSubAgent")
	ErrDuplicateReplay           = errors.New("ErrDuplicateReplay")
	ErrAgentFingerprintMismatch  = errors.New("ErrAgentFingerprintMismatch")
	ErrChildBudgetRequired       = errors.New("ErrChildBudgetReservationRequired")
	ErrOwnershipRequired         = errors.New("ErrOwnershipRequired")
)

type AgentBudgetBindingInput struct {
	BudgetPolicyID      string
	BudgetReservationID string
	ReservationScope    string
	ReservedQuantities  string
	AncestorBudgetRefs  string
	ReservationState    string
}

type AgentOwnershipLockInput struct {
	ResourceKind      string
	ResourceKey       string
	LockMode          string
	State             string
	LeaseExpiresAt    string
	HeartbeatAt       string
	ConflictsWithJSON string
}

type AgentRegistrationRequest struct {
	ProjectID                string
	DeliveryRunID            string
	RootRunID                string
	ParentRunID              string
	ChildRunID               string
	ParentAgentID            string
	TaskID                   string
	AttemptID                string
	PlanID                   string
	ChildKey                 string
	AdapterID                string
	ProviderInstallationID   string
	AccountProfileID         string
	ModelCapabilityID        string
	RoutingDecisionID        string
	ProviderSessionRef       string
	ScopeJSON                string
	Permission               string
	SideEffectClass          string
	BudgetBindings           []AgentBudgetBindingInput
	OwnershipLocks           []AgentOwnershipLockInput
	ExecutorID               string
	ClaimGeneration          int64
	ProviderIdempotencyKey   string
	CancellationChannel      string
	ExpectedOutputsJSON      string
	PolicyVersion            string
	PlanFingerprint          string
	PolicyFingerprint        string
	AuthorizationFingerprint string
	Classification           string
	GapReasons               []string
	Now                      time.Time
}

type AgentRegistrationRecord struct {
	ChildAgentID               string   `json:"child_agent_id"`
	ParentAgentID              string   `json:"parent_agent_id,omitempty"`
	ProjectID                  string   `json:"project_id"`
	DeliveryRunID              string   `json:"delivery_run_id"`
	RootRunID                  string   `json:"root_run_id"`
	ParentRunID                string   `json:"parent_run_id"`
	RunID                      string   `json:"run_id"`
	TaskID                     string   `json:"task_id"`
	AttemptID                  string   `json:"attempt_id"`
	PlanID                     string   `json:"plan_id"`
	ChildKey                   string   `json:"child_key"`
	AdapterID                  string   `json:"adapter_id"`
	ProviderInstallationID     string   `json:"provider_installation_id,omitempty"`
	AccountProfileID           string   `json:"account_profile_id,omitempty"`
	ModelCapabilityID          string   `json:"model_capability_id,omitempty"`
	RoutingDecisionID          string   `json:"routing_decision_id,omitempty"`
	ProviderSessionRef         string   `json:"provider_session_ref,omitempty"`
	ScopeGrantID               string   `json:"scope_grant_id"`
	Permission                 string   `json:"permission"`
	SideEffectClass            string   `json:"side_effect_class"`
	BudgetBindingIDs           []string `json:"budget_binding_ids"`
	BudgetReservationIDs       []string `json:"budget_reservation_ids"`
	OwnershipLockIDs           []string `json:"ownership_lock_ids"`
	ClaimGeneration            int64    `json:"claim_generation"`
	ExecutorID                 string   `json:"executor_id"`
	ProviderIdempotencyKey     string   `json:"provider_idempotency_key"`
	CancellationChannel        string   `json:"cancellation_channel"`
	RegistrationState          string   `json:"registration_state"`
	PolicyVersion              string   `json:"policy_version"`
	PlanFingerprint            string   `json:"plan_fingerprint"`
	PolicyFingerprint          string   `json:"policy_fingerprint"`
	AuthorizationFingerprint   string   `json:"authorization_fingerprint,omitempty"`
	AgentFederationFingerprint string   `json:"agent_federation_fingerprint"`
	Classification             string   `json:"classification"`
	GapReasons                 []string `json:"gap_reasons"`
	Depth                      int      `json:"depth"`
	CreatedAt                  string   `json:"created_at"`
	UpdatedAt                  string   `json:"updated_at"`
}

type AgentTree struct {
	SchemaVersion              string                    `json:"schema_version"`
	RootRunID                  string                    `json:"root_run_id"`
	AgentFederationFingerprint string                    `json:"agent_federation_fingerprint"`
	Registrations              []AgentRegistrationRecord `json:"registrations"`
	Blocked                    []string                  `json:"blocked"`
	NeedsHuman                 []string                  `json:"needs_human"`
}

type claimAndRegisterCanonical struct {
	RequestHash  string   `json:"request_hash"`
	Registration any      `json:"registration"`
	BudgetIDs    []string `json:"budget_ids"`
	LockIDs      []string `json:"lock_ids"`
	ScopeGrantID string   `json:"scope_grant_id"`
}

func ClaimAndRegisterNativeChild(ctx context.Context, store Store, parentRunID, childRunID, executorID string, now, leaseUntil time.Time, req AgentRegistrationRequest) (ClaimResult, AgentRegistrationRecord, error) {
	if store == nil {
		return ClaimResult{Outcome: ClaimOutcomeClaimed, RunID: strings.TrimSpace(childRunID), ExecutorID: strings.TrimSpace(executorID), ClaimGeneration: 1}, AgentRegistrationRecord{}, nil
	}
	parentRunID = strings.TrimSpace(parentRunID)
	childRunID = strings.TrimSpace(childRunID)
	executorID = strings.TrimSpace(executorID)
	if parentRunID == "" || childRunID == "" || executorID == "" {
		return ClaimResult{}, AgentRegistrationRecord{}, fmt.Errorf("claim and register native child: parent_run_id, child_run_id, and executor_id are required")
	}
	now = now.UTC()
	leaseUntil = leaseUntil.UTC()
	if now.IsZero() || leaseUntil.IsZero() || !leaseUntil.After(now) {
		return ClaimResult{}, AgentRegistrationRecord{}, fmt.Errorf("claim and register native child: valid now and future lease_until are required")
	}
	req.ParentRunID = parentRunID
	req.ChildRunID = childRunID
	req.ExecutorID = executorID
	req.Now = now

	var claim ClaimResult
	var registration AgentRegistrationRecord
	err := withRetry(ctx, func() error {
		return store.WithWriteTx(ctx, func(tx Tx) error {
			txClaim, err := claimChildRunExecutionTx(ctx, tx, parentRunID, childRunID, executorID, formatTimestamp(now), formatTimestamp(leaseUntil))
			if err != nil {
				return err
			}
			claim = txClaim
			switch txClaim.Outcome {
			case ClaimOutcomeClaimed, ClaimOutcomeStaleClaim:
				req.ClaimGeneration = txClaim.ClaimGeneration
				req.ExecutorID = txClaim.ExecutorID
				req.ProviderIdempotencyKey = txClaim.ProviderKey
				record, err := registerAgentRegistrationTx(ctx, tx, req)
				if err != nil {
					return err
				}
				registration = record
			default:
				return nil
			}
			return nil
		})
	})
	if err != nil {
		return ClaimResult{}, AgentRegistrationRecord{}, err
	}
	return claim, registration, nil
}

func RegisterAgentRegistration(ctx context.Context, store Store, req AgentRegistrationRequest) (AgentRegistrationRecord, error) {
	if store == nil {
		return AgentRegistrationRecord{}, nil
	}
	if req.Now.IsZero() {
		req.Now = store.Now()
	}
	var registration AgentRegistrationRecord
	err := withRetry(ctx, func() error {
		return store.WithWriteTx(ctx, func(tx Tx) error {
			record, err := registerAgentRegistrationTx(ctx, tx, req)
			if err != nil {
				return err
			}
			registration = record
			return nil
		})
	})
	if err != nil {
		return AgentRegistrationRecord{}, err
	}
	return registration, nil
}

func RequireActiveAgentRegistrationForLaunch(ctx context.Context, store Store, childRunID, executorID string, claimGeneration int64) (AgentRegistrationRecord, error) {
	if store == nil {
		return AgentRegistrationRecord{}, nil
	}
	childRunID = strings.TrimSpace(childRunID)
	executorID = strings.TrimSpace(executorID)
	if childRunID == "" || executorID == "" || claimGeneration <= 0 {
		return AgentRegistrationRecord{}, fmt.Errorf("%w: child_run_id, executor_id, and claim_generation are required", ErrAgentRegistrationRequired)
	}
	var record AgentRegistrationRecord
	err := store.WithTx(ctx, func(tx Tx) error {
		loaded, ok, err := loadAgentRegistrationByRunTx(ctx, tx, childRunID)
		if err != nil {
			return err
		}
		if !ok || loaded.RegistrationState != AgentRegistrationStateActive {
			return fmt.Errorf("%w: child run %q has no active agent registration", ErrAgentRegistrationRequired, childRunID)
		}
		if loaded.ExecutorID != executorID || loaded.ClaimGeneration != claimGeneration {
			return fmt.Errorf("%w: registration claim owner/generation mismatch for child run %q", ErrAgentRegistrationConflict, childRunID)
		}
		if err := verifyAgentFingerprint(ctx, tx, loaded); err != nil {
			return err
		}
		record = loaded
		return nil
	})
	if err != nil {
		return AgentRegistrationRecord{}, err
	}
	return record, nil
}

func LoadAgentTree(ctx context.Context, store Store, rootRunID string) (AgentTree, error) {
	tree := AgentTree{
		SchemaVersion: "loopcoder.agent_tree.v1",
		RootRunID:     strings.TrimSpace(rootRunID),
		Registrations: []AgentRegistrationRecord{},
		Blocked:       []string{},
		NeedsHuman:    []string{},
	}
	if store == nil || tree.RootRunID == "" {
		return tree, nil
	}
	err := store.WithTx(ctx, func(tx Tx) error {
		rows, err := tx.Query(ctx, `SELECT child_run_id FROM agent_registrations WHERE root_run_id = ? ORDER BY depth, child_key, id`, tree.RootRunID)
		if err != nil {
			return fmt.Errorf("load agent tree: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var runID string
			if err := rows.Scan(&runID); err != nil {
				return fmt.Errorf("load agent tree: %w", err)
			}
			record, ok, err := loadAgentRegistrationByRunTx(ctx, tx, runID)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			tree.Registrations = append(tree.Registrations, record)
			if record.AgentFederationFingerprint > tree.AgentFederationFingerprint {
				tree.AgentFederationFingerprint = record.AgentFederationFingerprint
			}
			if record.RegistrationState == "needs-human" {
				tree.NeedsHuman = append(tree.NeedsHuman, record.ChildAgentID)
			}
		}
		return rows.Err()
	})
	if err != nil {
		return AgentTree{}, err
	}
	return tree, nil
}

func registerAgentRegistrationTx(ctx context.Context, tx Tx, req AgentRegistrationRequest) (AgentRegistrationRecord, error) {
	req = normalizeAgentRegistrationRequest(req)
	if err := validateAgentRegistrationRequest(req); err != nil {
		return AgentRegistrationRecord{}, err
	}
	depth, err := validateAgentRegistrationGraph(ctx, tx, req)
	if err != nil {
		return AgentRegistrationRecord{}, err
	}
	if err := validateAgentRegistrationClaim(ctx, tx, req); err != nil {
		return AgentRegistrationRecord{}, err
	}

	childAgentID := StableChildAgentID(req)
	scopeGrantID := stablePrefixedID("ascope_", childAgentID, req.ScopeJSON, req.PolicyFingerprint)
	budgetIDs := stableBudgetBindingIDs(childAgentID, req.BudgetBindings)
	lockIDs := stableOwnershipLockIDs(req.ProjectID, childAgentID, req.OwnershipLocks)
	fingerprint := AgentFederationFingerprint(req, childAgentID, scopeGrantID, budgetIDs, lockIDs)
	registrationHash := agentRegistrationHash(req, childAgentID, scopeGrantID, budgetIDs, lockIDs, fingerprint)
	at := formatTimestamp(req.Now)
	gaps, _ := json.Marshal(req.GapReasons)

	if existing, ok, err := loadAgentRegistrationByRunTx(ctx, tx, req.ChildRunID); err != nil {
		return AgentRegistrationRecord{}, err
	} else if ok {
		if existing.ChildAgentID != childAgentID || existing.AgentFederationFingerprint != fingerprint {
			return AgentRegistrationRecord{}, fmt.Errorf("%w: child run %q registration fingerprint changed", ErrAgentRegistrationConflict, req.ChildRunID)
		}
		var storedHash string
		if err := tx.QueryRow(ctx, `SELECT registration_hash FROM agent_registrations WHERE child_run_id = ?`, req.ChildRunID).Scan(&storedHash); err != nil {
			return AgentRegistrationRecord{}, fmt.Errorf("inspect existing agent registration: %w", err)
		}
		if storedHash != registrationHash {
			return AgentRegistrationRecord{}, fmt.Errorf("%w: stable child id %q replayed with different canonical registration bytes", ErrDuplicateReplay, childAgentID)
		}
		return existing, nil
	}

	if _, err := tx.Exec(ctx, `INSERT INTO agent_scope_grants(
			id, project_id, delivery_run_id, child_agent_id, schema_version, record_version, scope_json, permission, side_effect_class,
			policy_version, policy_fingerprint, plan_fingerprint, authorization_fingerprint, agent_federation_fingerprint,
			created_at, updated_at, terminal_error_code
		) VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '')`,
		scopeGrantID, req.ProjectID, req.DeliveryRunID, childAgentID, AgentScopeGrantSchemaV1, req.ScopeJSON, req.Permission, req.SideEffectClass,
		req.PolicyVersion, req.PolicyFingerprint, req.PlanFingerprint, req.AuthorizationFingerprint, fingerprint, at, at); err != nil {
		return AgentRegistrationRecord{}, fmt.Errorf("insert agent scope grant: %w", err)
	}

	for i, binding := range req.BudgetBindings {
		id := budgetIDs[i]
		if _, err := tx.Exec(ctx, `INSERT INTO agent_budget_bindings(
				id, project_id, delivery_run_id, child_agent_id, budget_policy_id, budget_reservation_id, reservation_scope,
				reserved_quantities_json, ancestor_budget_refs_json, reservation_state, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, req.ProjectID, req.DeliveryRunID, childAgentID, binding.BudgetPolicyID, binding.BudgetReservationID, binding.ReservationScope,
			binding.ReservedQuantities, binding.AncestorBudgetRefs, binding.ReservationState, at, at); err != nil {
			return AgentRegistrationRecord{}, fmt.Errorf("insert agent budget binding: %w", err)
		}
	}

	for i, lock := range req.OwnershipLocks {
		id := lockIDs[i]
		if _, err := tx.Exec(ctx, `INSERT INTO agent_ownership_locks(
				id, project_id, delivery_run_id, child_agent_id, run_id, claim_generation, lock_generation, resource_kind, resource_key,
				lock_mode, state, lease_expires_at, heartbeat_at, conflicts_with_json, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, req.ProjectID, req.DeliveryRunID, childAgentID, req.ChildRunID, req.ClaimGeneration, lock.ResourceKind, lock.ResourceKey,
			lock.LockMode, lock.State, lock.LeaseExpiresAt, lock.HeartbeatAt, lock.ConflictsWithJSON, at, at); err != nil {
			return AgentRegistrationRecord{}, fmt.Errorf("insert agent ownership lock: %w", err)
		}
	}

	if _, err := tx.Exec(ctx, `INSERT INTO agent_registrations(
			id, project_id, delivery_run_id, root_run_id, parent_run_id, child_run_id, parent_agent_id, task_id, attempt_id,
			plan_id, child_key, adapter_id, provider_installation_id, account_profile_id, model_capability_id, routing_decision_id,
			provider_session_ref, scope_grant_id, permission, side_effect_class, claim_generation, executor_id,
			provider_idempotency_key, provider_receipt, cancellation_channel, expected_outputs_json, registration_state,
			policy_version, plan_fingerprint, policy_fingerprint, authorization_fingerprint, agent_federation_fingerprint,
			registration_hash, classification, gap_reasons_json, depth, created_at, updated_at, terminal_error_code
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '')`,
		childAgentID, req.ProjectID, req.DeliveryRunID, req.RootRunID, req.ParentRunID, req.ChildRunID, req.ParentAgentID, req.TaskID, req.AttemptID,
		req.PlanID, req.ChildKey, req.AdapterID, req.ProviderInstallationID, req.AccountProfileID, req.ModelCapabilityID, req.RoutingDecisionID,
		req.ProviderSessionRef, scopeGrantID, req.Permission, req.SideEffectClass, req.ClaimGeneration, req.ExecutorID, req.ProviderIdempotencyKey,
		req.CancellationChannel, req.ExpectedOutputsJSON, AgentRegistrationStateActive, req.PolicyVersion, req.PlanFingerprint, req.PolicyFingerprint,
		req.AuthorizationFingerprint, fingerprint, registrationHash, req.Classification, string(gaps), depth, at, at); err != nil {
		return AgentRegistrationRecord{}, fmt.Errorf("insert agent registration: %w", err)
	}

	return loadAgentRegistrationByIDTx(ctx, tx, childAgentID)
}

func normalizeAgentRegistrationRequest(req AgentRegistrationRequest) AgentRegistrationRequest {
	req.ProjectID = strings.TrimSpace(req.ProjectID)
	req.DeliveryRunID = strings.TrimSpace(req.DeliveryRunID)
	req.RootRunID = strings.TrimSpace(req.RootRunID)
	req.ParentRunID = strings.TrimSpace(req.ParentRunID)
	req.ChildRunID = strings.TrimSpace(req.ChildRunID)
	req.ParentAgentID = strings.TrimSpace(req.ParentAgentID)
	req.TaskID = strings.TrimSpace(req.TaskID)
	req.AttemptID = strings.TrimSpace(req.AttemptID)
	req.PlanID = strings.TrimSpace(req.PlanID)
	req.ChildKey = strings.TrimSpace(req.ChildKey)
	req.AdapterID = strings.TrimSpace(req.AdapterID)
	req.ProviderInstallationID = strings.TrimSpace(req.ProviderInstallationID)
	req.AccountProfileID = strings.TrimSpace(req.AccountProfileID)
	req.ModelCapabilityID = strings.TrimSpace(req.ModelCapabilityID)
	req.RoutingDecisionID = strings.TrimSpace(req.RoutingDecisionID)
	req.ProviderSessionRef = strings.TrimSpace(req.ProviderSessionRef)
	req.ScopeJSON = normalizeJSONText(req.ScopeJSON, "{}")
	req.Permission = strings.ToLower(strings.TrimSpace(req.Permission))
	req.SideEffectClass = strings.TrimSpace(req.SideEffectClass)
	req.ExecutorID = strings.TrimSpace(req.ExecutorID)
	req.ProviderIdempotencyKey = strings.TrimSpace(req.ProviderIdempotencyKey)
	req.CancellationChannel = strings.TrimSpace(req.CancellationChannel)
	req.ExpectedOutputsJSON = normalizeJSONText(req.ExpectedOutputsJSON, "{}")
	req.PolicyVersion = strings.TrimSpace(req.PolicyVersion)
	if req.PolicyVersion == "" {
		req.PolicyVersion = AgentFederationPolicyVersionV1
	}
	req.PlanFingerprint = strings.TrimSpace(req.PlanFingerprint)
	req.PolicyFingerprint = strings.TrimSpace(req.PolicyFingerprint)
	req.AuthorizationFingerprint = strings.TrimSpace(req.AuthorizationFingerprint)
	req.Classification = strings.TrimSpace(req.Classification)
	if req.Classification == "" {
		req.Classification = "internal"
	}
	req.GapReasons = normalizeTextList(req.GapReasons)
	req.Now = req.Now.UTC()
	for i := range req.BudgetBindings {
		req.BudgetBindings[i].BudgetPolicyID = strings.TrimSpace(req.BudgetBindings[i].BudgetPolicyID)
		req.BudgetBindings[i].BudgetReservationID = strings.TrimSpace(req.BudgetBindings[i].BudgetReservationID)
		req.BudgetBindings[i].ReservationScope = strings.TrimSpace(req.BudgetBindings[i].ReservationScope)
		req.BudgetBindings[i].ReservedQuantities = normalizeJSONText(req.BudgetBindings[i].ReservedQuantities, "{}")
		req.BudgetBindings[i].AncestorBudgetRefs = normalizeJSONText(req.BudgetBindings[i].AncestorBudgetRefs, "[]")
		req.BudgetBindings[i].ReservationState = strings.TrimSpace(req.BudgetBindings[i].ReservationState)
	}
	for i := range req.OwnershipLocks {
		req.OwnershipLocks[i].ResourceKind = strings.TrimSpace(req.OwnershipLocks[i].ResourceKind)
		req.OwnershipLocks[i].ResourceKey = strings.TrimSpace(req.OwnershipLocks[i].ResourceKey)
		req.OwnershipLocks[i].LockMode = strings.TrimSpace(req.OwnershipLocks[i].LockMode)
		req.OwnershipLocks[i].State = strings.TrimSpace(req.OwnershipLocks[i].State)
		if req.OwnershipLocks[i].State == "" {
			req.OwnershipLocks[i].State = "held"
		}
		req.OwnershipLocks[i].LeaseExpiresAt = strings.TrimSpace(req.OwnershipLocks[i].LeaseExpiresAt)
		req.OwnershipLocks[i].HeartbeatAt = strings.TrimSpace(req.OwnershipLocks[i].HeartbeatAt)
		req.OwnershipLocks[i].ConflictsWithJSON = normalizeJSONText(req.OwnershipLocks[i].ConflictsWithJSON, "[]")
	}
	return req
}

func validateAgentRegistrationRequest(req AgentRegistrationRequest) error {
	required := map[string]string{
		"project_id":               req.ProjectID,
		"delivery_run_id":          req.DeliveryRunID,
		"root_run_id":              req.RootRunID,
		"parent_run_id":            req.ParentRunID,
		"child_run_id":             req.ChildRunID,
		"task_id":                  req.TaskID,
		"attempt_id":               req.AttemptID,
		"plan_id":                  req.PlanID,
		"child_key":                req.ChildKey,
		"adapter_id":               req.AdapterID,
		"scope_json":               req.ScopeJSON,
		"permission":               req.Permission,
		"side_effect_class":        req.SideEffectClass,
		"executor_id":              req.ExecutorID,
		"provider_idempotency_key": req.ProviderIdempotencyKey,
		"cancellation_channel":     req.CancellationChannel,
		"expected_outputs_json":    req.ExpectedOutputsJSON,
		"policy_version":           req.PolicyVersion,
		"plan_fingerprint":         req.PlanFingerprint,
		"policy_fingerprint":       req.PolicyFingerprint,
	}
	for name, value := range required {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%w: %s is required", ErrAgentRegistrationRequired, name)
		}
	}
	if !json.Valid([]byte(req.ScopeJSON)) {
		return fmt.Errorf("%w: scope_json is invalid", ErrAgentRegistrationRequired)
	}
	if !json.Valid([]byte(req.ExpectedOutputsJSON)) {
		return fmt.Errorf("%w: expected_outputs_json is invalid", ErrAgentRegistrationRequired)
	}
	if req.ProviderSessionRef != "" && len(req.ProviderSessionRef) > maxProviderSessionRefBytes {
		return fmt.Errorf("%w: provider_session_ref exceeds %d bytes", ErrAgentRegistrationConflict, maxProviderSessionRefBytes)
	}
	if len(req.BudgetBindings) == 0 {
		return fmt.Errorf("%w: native child launch requires at least one budget binding", ErrChildBudgetRequired)
	}
	for _, binding := range req.BudgetBindings {
		if binding.BudgetPolicyID == "" || binding.BudgetReservationID == "" || binding.ReservationScope == "" || binding.ReservationState == "" {
			return fmt.Errorf("%w: budget policy, reservation, scope, and state are required", ErrChildBudgetRequired)
		}
		if !json.Valid([]byte(binding.ReservedQuantities)) || !json.Valid([]byte(binding.AncestorBudgetRefs)) {
			return fmt.Errorf("%w: budget binding JSON is invalid", ErrChildBudgetRequired)
		}
	}
	if req.Permission != "read-only" && len(req.OwnershipLocks) == 0 {
		return fmt.Errorf("%w: write-capable native child requires ownership locks", ErrOwnershipRequired)
	}
	for _, lock := range req.OwnershipLocks {
		if lock.ResourceKind == "" || lock.ResourceKey == "" || lock.LockMode == "" || lock.State == "" || lock.LeaseExpiresAt == "" || lock.HeartbeatAt == "" {
			return fmt.Errorf("%w: ownership lock kind, key, mode, state, lease, and heartbeat are required", ErrOwnershipRequired)
		}
		if !json.Valid([]byte(lock.ConflictsWithJSON)) {
			return fmt.Errorf("%w: ownership lock conflicts JSON is invalid", ErrOwnershipRequired)
		}
	}
	return nil
}

func validateAgentRegistrationGraph(ctx context.Context, tx Tx, req AgentRegistrationRequest) (int, error) {
	var parentRunID, rootRunID string
	var depth int
	err := tx.QueryRow(ctx, `SELECT COALESCE(parent_run_id, ''), root_run_id, depth FROM runs WHERE id = ?`, req.ChildRunID).Scan(&parentRunID, &rootRunID, &depth)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0, fmt.Errorf("%w: child run %q is missing", ErrAgentRegistrationRequired, req.ChildRunID)
		}
		return 0, fmt.Errorf("inspect child run for registration: %w", err)
	}
	if parentRunID != req.ParentRunID || rootRunID != req.RootRunID {
		return 0, fmt.Errorf("%w: child run graph mismatch for %q", ErrAgentRegistrationConflict, req.ChildRunID)
	}
	var edgeRoot, edgePlan, edgeKey string
	var edgeDepth int
	err = tx.QueryRow(ctx, `SELECT root_run_id, plan_id, child_key, depth FROM run_edges WHERE parent_run_id = ? AND child_run_id = ?`, req.ParentRunID, req.ChildRunID).Scan(&edgeRoot, &edgePlan, &edgeKey, &edgeDepth)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0, fmt.Errorf("%w: child run %q has no parent edge", ErrAgentRegistrationRequired, req.ChildRunID)
		}
		return 0, fmt.Errorf("inspect child edge for registration: %w", err)
	}
	if edgeRoot != req.RootRunID || edgePlan != req.PlanID || edgeKey != req.ChildKey || edgeDepth != depth {
		return 0, fmt.Errorf("%w: child edge mismatch for %q", ErrAgentRegistrationConflict, req.ChildRunID)
	}
	if ancestors, err := runAncestors(ctx, tx, req.ParentRunID); err != nil {
		return 0, err
	} else if stringSetContains(ancestors, req.ChildRunID) {
		return 0, fmt.Errorf("%w: child run %q would create a cycle", ErrAgentRegistrationConflict, req.ChildRunID)
	}
	return depth, nil
}

func validateAgentRegistrationClaim(ctx context.Context, tx Tx, req AgentRegistrationRequest) error {
	claim, ok, err := currentRunClaim(ctx, tx, req.ChildRunID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: child run %q has no execution claim", ErrAgentRegistrationRequired, req.ChildRunID)
	}
	if claim.ExecutorID != req.ExecutorID || claim.ClaimGeneration != req.ClaimGeneration || claim.ProviderKey != req.ProviderIdempotencyKey {
		return fmt.Errorf("%w: child run %q claim does not match registration", ErrAgentRegistrationConflict, req.ChildRunID)
	}
	if claim.ClaimPhase != ClaimPhaseClaimed {
		return fmt.Errorf("%w: child run %q claim phase is %q, want claimed before provider launch", ErrAgentRegistrationConflict, req.ChildRunID, claim.ClaimPhase)
	}
	return nil
}

func loadAgentRegistrationByRunTx(ctx context.Context, tx Tx, childRunID string) (AgentRegistrationRecord, bool, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM agent_registrations WHERE child_run_id = ?`, strings.TrimSpace(childRunID)).Scan(&id)
	if err != nil {
		if err == sql.ErrNoRows {
			return AgentRegistrationRecord{}, false, nil
		}
		return AgentRegistrationRecord{}, false, fmt.Errorf("load agent registration: %w", err)
	}
	record, err := loadAgentRegistrationByIDTx(ctx, tx, id)
	return record, err == nil, err
}

func loadAgentRegistrationByIDTx(ctx context.Context, tx Tx, id string) (AgentRegistrationRecord, error) {
	var record AgentRegistrationRecord
	var gapReasonsJSON string
	err := tx.QueryRow(ctx, `SELECT
			id, parent_agent_id, project_id, delivery_run_id, root_run_id, parent_run_id, child_run_id, task_id, attempt_id,
			plan_id, child_key, adapter_id, provider_installation_id, account_profile_id, model_capability_id, routing_decision_id,
			provider_session_ref, scope_grant_id, permission, side_effect_class, claim_generation, executor_id,
			provider_idempotency_key, cancellation_channel, registration_state, policy_version, plan_fingerprint, policy_fingerprint,
			authorization_fingerprint, agent_federation_fingerprint, classification, gap_reasons_json, depth, created_at, updated_at
		FROM agent_registrations WHERE id = ?`, strings.TrimSpace(id)).Scan(
		&record.ChildAgentID, &record.ParentAgentID, &record.ProjectID, &record.DeliveryRunID, &record.RootRunID, &record.ParentRunID, &record.RunID, &record.TaskID, &record.AttemptID,
		&record.PlanID, &record.ChildKey, &record.AdapterID, &record.ProviderInstallationID, &record.AccountProfileID, &record.ModelCapabilityID, &record.RoutingDecisionID,
		&record.ProviderSessionRef, &record.ScopeGrantID, &record.Permission, &record.SideEffectClass, &record.ClaimGeneration, &record.ExecutorID,
		&record.ProviderIdempotencyKey, &record.CancellationChannel, &record.RegistrationState, &record.PolicyVersion, &record.PlanFingerprint, &record.PolicyFingerprint,
		&record.AuthorizationFingerprint, &record.AgentFederationFingerprint, &record.Classification, &gapReasonsJSON, &record.Depth, &record.CreatedAt, &record.UpdatedAt)
	if err != nil {
		return AgentRegistrationRecord{}, fmt.Errorf("load agent registration %q: %w", id, err)
	}
	_ = json.Unmarshal([]byte(gapReasonsJSON), &record.GapReasons)
	record.BudgetBindingIDs, record.BudgetReservationIDs, err = loadAgentBudgetRefsTx(ctx, tx, record.ChildAgentID)
	if err != nil {
		return AgentRegistrationRecord{}, err
	}
	record.OwnershipLockIDs, err = loadAgentOwnershipLockIDsTx(ctx, tx, record.ChildAgentID)
	if err != nil {
		return AgentRegistrationRecord{}, err
	}
	return record, nil
}

func loadAgentBudgetRefsTx(ctx context.Context, tx Tx, childAgentID string) ([]string, []string, error) {
	rows, err := tx.Query(ctx, `SELECT id, budget_reservation_id FROM agent_budget_bindings WHERE child_agent_id = ? ORDER BY id`, childAgentID)
	if err != nil {
		return nil, nil, fmt.Errorf("load agent budget refs: %w", err)
	}
	defer rows.Close()
	var ids []string
	var reservations []string
	for rows.Next() {
		var id, reservation string
		if err := rows.Scan(&id, &reservation); err != nil {
			return nil, nil, fmt.Errorf("load agent budget refs: %w", err)
		}
		ids = append(ids, id)
		reservations = append(reservations, reservation)
	}
	return ids, reservations, rows.Err()
}

func loadAgentOwnershipLockIDsTx(ctx context.Context, tx Tx, childAgentID string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT id FROM agent_ownership_locks WHERE child_agent_id = ? ORDER BY id`, childAgentID)
	if err != nil {
		return nil, fmt.Errorf("load agent ownership locks: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("load agent ownership locks: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func verifyAgentFingerprint(ctx context.Context, tx Tx, record AgentRegistrationRecord) error {
	var stored string
	if err := tx.QueryRow(ctx, `SELECT agent_federation_fingerprint FROM agent_registrations WHERE id = ?`, record.ChildAgentID).Scan(&stored); err != nil {
		return fmt.Errorf("verify agent fingerprint: %w", err)
	}
	if stored == "" || stored != record.AgentFederationFingerprint || !strings.HasPrefix(stored, "sha256:") {
		return fmt.Errorf("%w: registration %q fingerprint is invalid", ErrAgentFingerprintMismatch, record.ChildAgentID)
	}
	return nil
}

func StableChildAgentID(req AgentRegistrationRequest) string {
	req = normalizeAgentRegistrationRequest(req)
	return stablePrefixedID("agent_", req.ProjectID, req.DeliveryRunID, req.ParentRunID, req.TaskID, req.AttemptID, req.ChildKey, req.PlanFingerprint)
}

func AgentFederationFingerprint(req AgentRegistrationRequest, childAgentID, scopeGrantID string, budgetIDs, lockIDs []string) string {
	req = normalizeAgentRegistrationRequest(req)
	payload := claimAndRegisterCanonical{
		Registration: map[string]any{
			"child_agent_id":            childAgentID,
			"project_id":                req.ProjectID,
			"delivery_run_id":           req.DeliveryRunID,
			"root_run_id":               req.RootRunID,
			"parent_run_id":             req.ParentRunID,
			"child_run_id":              req.ChildRunID,
			"task_id":                   req.TaskID,
			"attempt_id":                req.AttemptID,
			"plan_id":                   req.PlanID,
			"child_key":                 req.ChildKey,
			"adapter_id":                req.AdapterID,
			"provider_installation_id":  req.ProviderInstallationID,
			"account_profile_id":        req.AccountProfileID,
			"model_capability_id":       req.ModelCapabilityID,
			"routing_decision_id":       req.RoutingDecisionID,
			"provider_session_ref":      req.ProviderSessionRef,
			"scope_json":                canonicalJSONString(req.ScopeJSON),
			"permission":                req.Permission,
			"side_effect_class":         req.SideEffectClass,
			"claim_generation":          req.ClaimGeneration,
			"executor_id":               req.ExecutorID,
			"provider_idempotency_key":  req.ProviderIdempotencyKey,
			"cancellation_channel":      req.CancellationChannel,
			"expected_outputs_json":     canonicalJSONString(req.ExpectedOutputsJSON),
			"policy_version":            req.PolicyVersion,
			"plan_fingerprint":          req.PlanFingerprint,
			"policy_fingerprint":        req.PolicyFingerprint,
			"authorization_fingerprint": req.AuthorizationFingerprint,
			"classification":            req.Classification,
			"gap_reasons":               req.GapReasons,
		},
		BudgetIDs:    sortedCopy(budgetIDs),
		LockIDs:      sortedCopy(lockIDs),
		ScopeGrantID: scopeGrantID,
	}
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func agentRegistrationHash(req AgentRegistrationRequest, childAgentID, scopeGrantID string, budgetIDs, lockIDs []string, fingerprint string) string {
	req = normalizeAgentRegistrationRequest(req)
	payload := claimAndRegisterCanonical{
		RequestHash: fingerprint,
		Registration: map[string]any{
			"child_agent_id":            childAgentID,
			"project_id":                req.ProjectID,
			"delivery_run_id":           req.DeliveryRunID,
			"root_run_id":               req.RootRunID,
			"parent_run_id":             req.ParentRunID,
			"child_run_id":              req.ChildRunID,
			"parent_agent_id":           req.ParentAgentID,
			"task_id":                   req.TaskID,
			"attempt_id":                req.AttemptID,
			"plan_id":                   req.PlanID,
			"child_key":                 req.ChildKey,
			"adapter_id":                req.AdapterID,
			"provider_installation_id":  req.ProviderInstallationID,
			"account_profile_id":        req.AccountProfileID,
			"model_capability_id":       req.ModelCapabilityID,
			"routing_decision_id":       req.RoutingDecisionID,
			"provider_session_ref":      req.ProviderSessionRef,
			"scope_json":                canonicalJSONString(req.ScopeJSON),
			"permission":                req.Permission,
			"side_effect_class":         req.SideEffectClass,
			"budget_bindings":           req.BudgetBindings,
			"ownership_locks":           req.OwnershipLocks,
			"executor_id":               req.ExecutorID,
			"claim_generation":          req.ClaimGeneration,
			"provider_idempotency_key":  req.ProviderIdempotencyKey,
			"cancellation_channel":      req.CancellationChannel,
			"expected_outputs_json":     canonicalJSONString(req.ExpectedOutputsJSON),
			"policy_version":            req.PolicyVersion,
			"plan_fingerprint":          req.PlanFingerprint,
			"policy_fingerprint":        req.PolicyFingerprint,
			"authorization_fingerprint": req.AuthorizationFingerprint,
			"classification":            req.Classification,
			"gap_reasons":               req.GapReasons,
		},
		BudgetIDs:    sortedCopy(budgetIDs),
		LockIDs:      sortedCopy(lockIDs),
		ScopeGrantID: scopeGrantID,
	}
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func stableBudgetBindingIDs(childAgentID string, bindings []AgentBudgetBindingInput) []string {
	ids := make([]string, 0, len(bindings))
	for _, binding := range bindings {
		ids = append(ids, stablePrefixedID("abudget_", childAgentID, binding.BudgetReservationID, binding.BudgetPolicyID))
	}
	return ids
}

func stableOwnershipLockIDs(projectID, childAgentID string, locks []AgentOwnershipLockInput) []string {
	ids := make([]string, 0, len(locks))
	for _, lock := range locks {
		ids = append(ids, stablePrefixedID("alock_", projectID, lock.ResourceKind, lock.ResourceKey, childAgentID))
	}
	return ids
}

func stablePrefixedID(prefix string, parts ...string) string {
	data, _ := json.Marshal(parts)
	sum := sha256.Sum256(data)
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])
	return prefix + strings.ToLower(encoded)
}

func canonicalJSONString(raw string) string {
	raw = normalizeJSONText(raw, "{}")
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return raw
	}
	data, err := json.Marshal(value)
	if err != nil {
		return raw
	}
	return string(data)
}

func normalizeJSONText(raw, fallback string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	return raw
}

func normalizeTextList(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func sortedCopy(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}
