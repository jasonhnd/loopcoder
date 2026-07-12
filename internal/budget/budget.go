// Package budget implements LoopCoder-local hierarchical budget accounting.
package budget

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
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

const (
	PolicySchema      = "loopcoder.budget_policy.v1"
	ReservationSchema = "loopcoder.budget_reservation.v1"
	EventSchema       = "loopcoder.quota_budget_event.v1"
)

var (
	ErrBudgetExhausted          = errors.New("ErrBudgetExhausted")
	ErrBudgetScopeConflict      = errors.New("ErrBudgetScopeConflict")
	ErrReservationExpired       = errors.New("ErrReservationExpired")
	ErrReservationStateConflict = errors.New("ErrReservationStateConflict")
	ErrBudgetRecordMalformed    = errors.New("ErrBudgetRecordMalformed")
)

type ScopeKind string

const (
	ScopeMachine     ScopeKind = "machine"
	ScopeProject     ScopeKind = "project"
	ScopeDeliveryRun ScopeKind = "delivery-run"
	ScopeTask        ScopeKind = "task"
	ScopeWorker      ScopeKind = "worker"
	ScopeSubAgent    ScopeKind = "sub-agent"
	ScopeProvider    ScopeKind = "provider-scope"
)

type PolicyMode string

const (
	PolicyHard PolicyMode = "hard"
	PolicySoft PolicyMode = "soft"
)

type ReservationState string

const (
	StateActive    ReservationState = "active"
	StateCommitted ReservationState = "committed"
	StateReleased  ReservationState = "released"
	StateCancelled ReservationState = "cancelled"
	StateExpired   ReservationState = "expired"
	StateRefused   ReservationState = "refused"
)

type Actor struct {
	ActorID string `json:"actor_id,omitempty"`
	Role    string `json:"role,omitempty"`
}

type Host struct {
	HostID   string `json:"host_id,omitempty"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}

type Scope struct {
	ScopeKind         ScopeKind `json:"scope_kind"`
	ProjectID         string    `json:"project_id,omitempty"`
	DeliveryRunID     string    `json:"delivery_run_id,omitempty"`
	TaskID            string    `json:"task_id,omitempty"`
	WorkerID          string    `json:"worker_id,omitempty"`
	SubAgentID        string    `json:"sub_agent_id,omitempty"`
	AdapterID         string    `json:"adapter_id,omitempty"`
	AccountProfileID  string    `json:"account_profile_id,omitempty"`
	ModelCapabilityID string    `json:"model_capability_id,omitempty"`
}

type Policy struct {
	SchemaVersion     string                         `json:"schema_version"`
	RecordVersion     int                            `json:"record_version"`
	BudgetPolicyID    string                         `json:"budget_policy_id"`
	Scope             Scope                          `json:"scope"`
	ScopeKey          string                         `json:"scope_key"`
	QuantityKind      providerinventory.QuantityKind `json:"quantity_kind"`
	Unit              string                         `json:"unit"`
	ValueScale        int                            `json:"value_scale"`
	WindowKind        providerinventory.WindowKind   `json:"window_kind"`
	PolicyMode        PolicyMode                     `json:"policy_mode"`
	CeilingValue      int64                          `json:"ceiling_value"`
	Active            bool                           `json:"active"`
	PolicyVersion     string                         `json:"policy_version"`
	CreatedAt         string                         `json:"created_at"`
	UpdatedAt         string                         `json:"updated_at"`
	CreatedBy         Actor                          `json:"created_by"`
	UpdatedBy         Actor                          `json:"updated_by"`
	Host              Host                           `json:"host"`
	Source            string                         `json:"source"`
	Evidence          string                         `json:"evidence"`
	OverridePolicyID  string                         `json:"override_policy_id,omitempty"`
	ApprovalID        string                         `json:"approval_id,omitempty"`
	GapReasons        []string                       `json:"gap_reasons"`
	TerminalErrorCode string                         `json:"terminal_error_code,omitempty"`
}

type Reservation struct {
	SchemaVersion         string                         `json:"schema_version"`
	RecordVersion         int                            `json:"record_version"`
	BudgetReservationID   string                         `json:"budget_reservation_id"`
	IdempotencyKey        string                         `json:"idempotency_key"`
	RequestFingerprint    string                         `json:"request_fingerprint"`
	Scope                 Scope                          `json:"scope"`
	ScopeKey              string                         `json:"scope_key"`
	QuantityKind          providerinventory.QuantityKind `json:"quantity_kind"`
	Unit                  string                         `json:"unit"`
	ValueScale            int                            `json:"value_scale"`
	RequestedValue        int64                          `json:"requested_value"`
	ReservedValue         int64                          `json:"reserved_value"`
	CommittedValue        int64                          `json:"committed_value"`
	ReleasedValue         int64                          `json:"released_value"`
	State                 ReservationState               `json:"state"`
	Generation            int64                          `json:"generation"`
	LeaseExpiresAt        string                         `json:"lease_expires_at"`
	PolicyIDs             []string                       `json:"budget_policy_ids"`
	RequirementConfidence providerinventory.Confidence   `json:"requirement_confidence"`
	ApprovalID            string                         `json:"approval_id,omitempty"`
	CreatedAt             string                         `json:"created_at"`
	UpdatedAt             string                         `json:"updated_at"`
	CreatedBy             Actor                          `json:"created_by"`
	UpdatedBy             Actor                          `json:"updated_by"`
	Host                  Host                           `json:"host"`
	GapReasons            []string                       `json:"gap_reasons"`
	TerminalErrorCode     string                         `json:"terminal_error_code,omitempty"`
}

type PolicyInput struct {
	Scope         Scope
	QuantityKind  providerinventory.QuantityKind
	Unit          string
	ValueScale    int
	WindowKind    providerinventory.WindowKind
	PolicyMode    PolicyMode
	CeilingValue  int64
	PolicyVersion string
	Ordinal       string
	Actor         Actor
	Host          Host
	Source        string
	Evidence      string
	ApprovalID    string
}

type ReserveRequest struct {
	ScopeChain            []Scope
	QuantityKind          providerinventory.QuantityKind
	Unit                  string
	ValueScale            int
	WindowKind            providerinventory.WindowKind
	RequestedValue        int64
	LeaseExpiresAt        time.Time
	IdempotencyKey        string
	RequirementConfidence providerinventory.Confidence
	ApprovalID            string
	Actor                 Actor
	Host                  Host
}

type MutationRequest struct {
	ReservationID  string
	IdempotencyKey string
	Generation     int64
	Value          int64
	Actor          Actor
	Host           Host
}

type Result struct {
	Reservation Reservation `json:"reservation"`
	Replay      bool        `json:"replay"`
}

type Summary struct {
	BudgetPolicyID       string                         `json:"budget_policy_id"`
	Scope                Scope                          `json:"scope"`
	ScopeKey             string                         `json:"scope_key"`
	QuantityKind         providerinventory.QuantityKind `json:"quantity_kind"`
	Unit                 string                         `json:"unit"`
	ValueScale           int                            `json:"value_scale"`
	WindowKind           providerinventory.WindowKind   `json:"window_kind"`
	PolicyMode           PolicyMode                     `json:"policy_mode"`
	CeilingValue         int64                          `json:"ceiling_value"`
	ReservedValue        int64                          `json:"reserved_value"`
	CommittedValue       int64                          `json:"committed_value"`
	AvailableValue       int64                          `json:"available_value"`
	EffectiveCeiling     int64                          `json:"effective_ceiling"`
	Confidence           providerinventory.Confidence   `json:"confidence"`
	PolicyVersion        string                         `json:"policy_version"`
	ActiveReservationIDs []string                       `json:"active_reservation_ids"`
	Denial               string                         `json:"denial,omitempty"`
	OverrideProvenance   string                         `json:"override_provenance,omitempty"`
	ApprovalID           string                         `json:"approval_id,omitempty"`
	GapReasons           []string                       `json:"gap_reasons"`
}

func UpsertPolicy(ctx context.Context, store storage.Store, input PolicyInput) (Policy, error) {
	if store == nil {
		return Policy{}, errors.New("budget policy: storage store is required")
	}
	now := formatTime(store.Now())
	policy := normalizePolicy(Policy{
		Scope:         input.Scope,
		QuantityKind:  input.QuantityKind,
		Unit:          input.Unit,
		ValueScale:    input.ValueScale,
		WindowKind:    input.WindowKind,
		PolicyMode:    input.PolicyMode,
		CeilingValue:  input.CeilingValue,
		Active:        true,
		PolicyVersion: input.PolicyVersion,
		CreatedAt:     now,
		UpdatedAt:     now,
		CreatedBy:     input.Actor,
		UpdatedBy:     input.Actor,
		Host:          input.Host,
		Source:        firstNonEmpty(input.Source, "operator-configured-policy-overlay"),
		Evidence:      firstNonEmpty(input.Evidence, "local policy ceiling"),
		ApprovalID:    input.ApprovalID,
	})
	if strings.TrimSpace(input.Ordinal) != "" {
		policy.BudgetPolicyID = policyID(policy.ScopeKey, policy.QuantityKind, policy.WindowKind, policy.PolicyVersion, input.Ordinal)
	}
	if err := ValidatePolicy(policy); err != nil {
		return Policy{}, err
	}
	payload, _ := json.Marshal(policy)
	err := withBudgetRetry(ctx, func() error {
		return store.WithWriteTx(ctx, func(tx storage.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO budget_policies(
				budget_policy_id, project_id, delivery_run_id, task_id, worker_id, sub_agent_id,
				adapter_id, account_profile_id, model_capability_id, scope_kind, scope_key,
				quantity_kind, unit, value_scale, window_kind, policy_mode, ceiling_value,
				active, policy_version, payload_json
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(budget_policy_id) DO UPDATE SET
				ceiling_value = excluded.ceiling_value,
				active = excluded.active,
				policy_mode = excluded.policy_mode,
				payload_json = excluded.payload_json`,
				policy.BudgetPolicyID, policy.Scope.ProjectID, policy.Scope.DeliveryRunID, policy.Scope.TaskID,
				policy.Scope.WorkerID, policy.Scope.SubAgentID, policy.Scope.AdapterID,
				policy.Scope.AccountProfileID, policy.Scope.ModelCapabilityID, string(policy.Scope.ScopeKind),
				policy.ScopeKey, string(policy.QuantityKind), policy.Unit, policy.ValueScale,
				string(policy.WindowKind), string(policy.PolicyMode), policy.CeilingValue,
				boolInt(policy.Active), policy.PolicyVersion, string(payload))
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `INSERT OR IGNORE INTO budget_aggregates(budget_policy_id, reserved_value, committed_value, record_version, updated_at)
				VALUES (?, 0, 0, 1, ?)`, policy.BudgetPolicyID, now)
			return err
		})
	})
	return policy, err
}

func Reserve(ctx context.Context, store storage.Store, req ReserveRequest) (Result, error) {
	if store == nil {
		return Result{}, errors.New("budget reserve: storage store is required")
	}
	req = normalizeReserveRequest(req)
	if err := validateReserveRequest(req); err != nil {
		return Result{}, err
	}
	fingerprint := reserveFingerprint(req)
	reservationID := "bres_" + hashBase32(req.IdempotencyKey, fingerprint)[:26]
	var result Result
	var refusalErr error
	err := withBudgetRetry(ctx, func() error {
		return store.WithWriteTx(ctx, func(tx storage.Tx) error {
			existing, found, err := loadReservationByIdempotency(ctx, tx, req.IdempotencyKey)
			if err != nil {
				return err
			}
			if found {
				if existing.RequestFingerprint != fingerprint {
					return fmt.Errorf("%w: idempotency key replayed with different reserve request", ErrReservationStateConflict)
				}
				result = Result{Reservation: existing, Replay: true}
				return nil
			}
			policies, err := loadApplicablePolicies(ctx, tx, req.ScopeChain, req.QuantityKind, req.WindowKind)
			if err != nil {
				return err
			}
			if err := checkRequirementConfidence(req); err != nil {
				refusalErr = err
				return insertRefusal(ctx, tx, store.Now(), reservationID, req, fingerprint, nil, err)
			}
			if err := checkPolicyCapacity(policies, req.RequestedValue); err != nil {
				refusalErr = err
				return insertRefusal(ctx, tx, store.Now(), reservationID, req, fingerprint, policyIDs(policies), err)
			}
			now := formatTime(store.Now())
			reservation := normalizeReservation(Reservation{
				BudgetReservationID:   reservationID,
				IdempotencyKey:        req.IdempotencyKey,
				RequestFingerprint:    fingerprint,
				Scope:                 req.ScopeChain[len(req.ScopeChain)-1],
				QuantityKind:          req.QuantityKind,
				Unit:                  req.Unit,
				ValueScale:            req.ValueScale,
				RequestedValue:        req.RequestedValue,
				ReservedValue:         req.RequestedValue,
				State:                 StateActive,
				Generation:            1,
				LeaseExpiresAt:        formatTime(req.LeaseExpiresAt),
				PolicyIDs:             policyIDs(policies),
				RequirementConfidence: req.RequirementConfidence,
				ApprovalID:            req.ApprovalID,
				CreatedAt:             now,
				UpdatedAt:             now,
				CreatedBy:             req.Actor,
				UpdatedBy:             req.Actor,
				Host:                  req.Host,
				GapReasons:            reserveGapReasons(req),
			})
			if err := insertReservation(ctx, tx, reservation); err != nil {
				return err
			}
			for _, policy := range policies {
				if err := addAggregate(ctx, tx, policy.Policy.BudgetPolicyID, req.RequestedValue, 0, now); err != nil {
					return err
				}
			}
			if err := insertEvent(ctx, tx, eventRecord{
				OperationKey:  "reserve:" + req.IdempotencyKey,
				ReservationID: reservationID,
				EventKind:     "reserve",
				Generation:    1,
				DeltaReserved: req.RequestedValue,
				EventTime:     now,
				Actor:         req.Actor,
				Host:          req.Host,
			}); err != nil {
				return err
			}
			result = Result{Reservation: reservation}
			return nil
		})
	})
	if errors.Is(err, ErrBudgetExhausted) || errors.Is(err, providerinventory.ErrQuotaConfidenceInsufficient) {
		return result, err
	}
	if refusalErr != nil {
		return result, refusalErr
	}
	return result, err
}

func Commit(ctx context.Context, store storage.Store, req MutationRequest) (Result, error) {
	return mutateReservation(ctx, store, req, "commit")
}

func Release(ctx context.Context, store storage.Store, req MutationRequest) (Result, error) {
	return mutateReservation(ctx, store, req, "release")
}

func Cancel(ctx context.Context, store storage.Store, req MutationRequest) (Result, error) {
	return mutateReservation(ctx, store, req, "cancel")
}

func ExpireStale(ctx context.Context, store storage.Store, now time.Time, actor Actor, host Host) ([]Reservation, error) {
	if store == nil {
		return nil, errors.New("budget expire: storage store is required")
	}
	var expired []Reservation
	err := withBudgetRetry(ctx, func() error {
		return store.WithWriteTx(ctx, func(tx storage.Tx) error {
			rows, err := tx.Query(ctx, `SELECT payload_json FROM budget_reservations WHERE state = ? AND lease_expires_at <= ? ORDER BY lease_expires_at, budget_reservation_id`,
				string(StateActive), formatTime(now))
			if err != nil {
				return err
			}
			defer rows.Close()
			var candidates []Reservation
			for rows.Next() {
				var payload string
				if err := rows.Scan(&payload); err != nil {
					return err
				}
				var reservation Reservation
				if err := json.Unmarshal([]byte(payload), &reservation); err != nil {
					return err
				}
				candidates = append(candidates, reservation)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			for _, reservation := range candidates {
				if reservation.ReservedValue <= 0 {
					continue
				}
				next := reservation
				next.ReservedValue = 0
				next.ReleasedValue += reservation.ReservedValue
				next.State = StateExpired
				next.Generation++
				next.UpdatedAt = formatTime(now)
				next.UpdatedBy = actor
				next.Host = host
				if err := updateReservation(ctx, tx, next); err != nil {
					return err
				}
				for _, policyID := range reservation.PolicyIDs {
					if err := addAggregate(ctx, tx, policyID, -reservation.ReservedValue, 0, formatTime(now)); err != nil {
						return err
					}
				}
				if err := insertEvent(ctx, tx, eventRecord{
					OperationKey:  "expire:" + reservation.BudgetReservationID + ":" + strconv.FormatInt(next.Generation, 10),
					ReservationID: reservation.BudgetReservationID,
					EventKind:     "expire",
					Generation:    next.Generation,
					DeltaReserved: -reservation.ReservedValue,
					EventTime:     formatTime(now),
					Actor:         actor,
					Host:          host,
				}); err != nil {
					return err
				}
				expired = append(expired, next)
			}
			return nil
		})
	})
	return expired, err
}

func Summaries(ctx context.Context, store storage.Store, projectID string) ([]Summary, error) {
	if store == nil {
		return nil, errors.New("budget summaries: storage store is required")
	}
	var summaries []Summary
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		clauses := []string{"p.active = 1"}
		args := []any{}
		if strings.TrimSpace(projectID) != "" {
			clauses = append(clauses, "(p.project_id = '' OR p.project_id = ?)")
			args = append(args, strings.TrimSpace(projectID))
		}
		rows, err := tx.Query(ctx, `SELECT p.payload_json, a.reserved_value, a.committed_value
			FROM budget_policies p
			JOIN budget_aggregates a ON a.budget_policy_id = p.budget_policy_id
			WHERE `+strings.Join(clauses, " AND ")+`
			ORDER BY p.scope_key, p.quantity_kind, p.budget_policy_id`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var payload string
			var reserved, committed int64
			if err := rows.Scan(&payload, &reserved, &committed); err != nil {
				return err
			}
			var policy Policy
			if err := json.Unmarshal([]byte(payload), &policy); err != nil {
				return err
			}
			activeIDs, err := activeReservationIDs(ctx, tx, policy.BudgetPolicyID)
			if err != nil {
				return err
			}
			available := policy.CeilingValue - reserved - committed
			if available < 0 {
				available = 0
			}
			summaries = append(summaries, Summary{
				BudgetPolicyID:       policy.BudgetPolicyID,
				Scope:                policy.Scope,
				ScopeKey:             policy.ScopeKey,
				QuantityKind:         policy.QuantityKind,
				Unit:                 policy.Unit,
				ValueScale:           policy.ValueScale,
				WindowKind:           policy.WindowKind,
				PolicyMode:           policy.PolicyMode,
				CeilingValue:         policy.CeilingValue,
				ReservedValue:        reserved,
				CommittedValue:       committed,
				AvailableValue:       available,
				EffectiveCeiling:     policy.CeilingValue,
				Confidence:           providerinventory.ConfidenceExact,
				PolicyVersion:        policy.PolicyVersion,
				ActiveReservationIDs: activeIDs,
				OverrideProvenance:   policy.OverridePolicyID,
				ApprovalID:           policy.ApprovalID,
				GapReasons:           dedupeStrings(policy.GapReasons),
			})
		}
		return rows.Err()
	})
	return summaries, err
}

type policyWithAggregate struct {
	Policy    Policy
	Reserved  int64
	Committed int64
}

type eventRecord struct {
	OperationKey   string
	ReservationID  string
	BudgetPolicyID string
	EventKind      string
	Generation     int64
	DeltaReserved  int64
	DeltaCommitted int64
	EventTime      string
	Actor          Actor
	Host           Host
}

func mutateReservation(ctx context.Context, store storage.Store, req MutationRequest, kind string) (Result, error) {
	if store == nil {
		return Result{}, errors.New("budget mutation: storage store is required")
	}
	req.ReservationID = strings.TrimSpace(req.ReservationID)
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
	if req.ReservationID == "" || req.IdempotencyKey == "" {
		return Result{}, fmt.Errorf("%w: reservation id and idempotency key are required", ErrReservationStateConflict)
	}
	if req.Value < 0 {
		return Result{}, fmt.Errorf("%w: mutation value must be non-negative", ErrReservationStateConflict)
	}
	var result Result
	err := withBudgetRetry(ctx, func() error {
		return store.WithWriteTx(ctx, func(tx storage.Tx) error {
			opKey := kind + ":" + req.ReservationID + ":" + req.IdempotencyKey
			if eventExists(ctx, tx, opKey) {
				existing, found, err := loadReservation(ctx, tx, req.ReservationID)
				if err != nil {
					return err
				}
				if !found {
					return fmt.Errorf("%w: reservation missing after replay event", ErrReservationStateConflict)
				}
				result = Result{Reservation: existing, Replay: true}
				return nil
			}
			reservation, found, err := loadReservation(ctx, tx, req.ReservationID)
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("%w: reservation not found", ErrReservationStateConflict)
			}
			if req.Generation != reservation.Generation {
				return fmt.Errorf("%w: stale generation %d for reservation generation %d", ErrReservationExpired, req.Generation, reservation.Generation)
			}
			if reservation.State != StateActive {
				return fmt.Errorf("%w: reservation state is %s", ErrReservationStateConflict, reservation.State)
			}
			if expired(store.Now(), reservation.LeaseExpiresAt) {
				return fmt.Errorf("%w: reservation lease expired", ErrReservationExpired)
			}
			value := req.Value
			if value == 0 {
				value = reservation.ReservedValue
			}
			if value > reservation.ReservedValue {
				return fmt.Errorf("%w: mutation value exceeds reserved balance", ErrReservationStateConflict)
			}
			now := formatTime(store.Now())
			next := reservation
			next.Generation++
			next.UpdatedAt = now
			next.UpdatedBy = req.Actor
			next.Host = req.Host
			deltaReserved := -value
			deltaCommitted := int64(0)
			switch kind {
			case "commit":
				next.ReservedValue -= value
				next.CommittedValue += value
				deltaCommitted = value
				if next.ReservedValue == 0 {
					next.State = StateCommitted
				}
			case "release":
				next.ReservedValue -= value
				next.ReleasedValue += value
				if next.ReservedValue == 0 {
					next.State = StateReleased
				}
			case "cancel":
				next.ReleasedValue += next.ReservedValue
				deltaReserved = -next.ReservedValue
				next.ReservedValue = 0
				next.State = StateCancelled
			default:
				return fmt.Errorf("%w: unsupported mutation kind %s", ErrReservationStateConflict, kind)
			}
			if err := updateReservation(ctx, tx, next); err != nil {
				return err
			}
			for _, policyID := range reservation.PolicyIDs {
				if err := addAggregate(ctx, tx, policyID, deltaReserved, deltaCommitted, now); err != nil {
					return err
				}
			}
			if err := insertEvent(ctx, tx, eventRecord{
				OperationKey:   opKey,
				ReservationID:  reservation.BudgetReservationID,
				EventKind:      kind,
				Generation:     next.Generation,
				DeltaReserved:  deltaReserved,
				DeltaCommitted: deltaCommitted,
				EventTime:      now,
				Actor:          req.Actor,
				Host:           req.Host,
			}); err != nil {
				return err
			}
			result = Result{Reservation: next}
			return nil
		})
	})
	return result, err
}

func loadApplicablePolicies(ctx context.Context, tx storage.Tx, scopes []Scope, quantity providerinventory.QuantityKind, window providerinventory.WindowKind) ([]policyWithAggregate, error) {
	var policies []policyWithAggregate
	seen := map[string]bool{}
	for _, scope := range scopes {
		key, err := ScopeKey(scope)
		if err != nil {
			return nil, err
		}
		rows, err := tx.Query(ctx, `SELECT p.payload_json, a.reserved_value, a.committed_value
			FROM budget_policies p
			JOIN budget_aggregates a ON a.budget_policy_id = p.budget_policy_id
			WHERE p.scope_key = ? AND p.quantity_kind = ? AND p.window_kind = ? AND p.active = 1
			ORDER BY p.budget_policy_id`, key, string(quantity), string(window))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var payload string
			var reserved, committed int64
			if err := rows.Scan(&payload, &reserved, &committed); err != nil {
				rows.Close()
				return nil, err
			}
			var policy Policy
			if err := json.Unmarshal([]byte(payload), &policy); err != nil {
				rows.Close()
				return nil, err
			}
			if seen[policy.BudgetPolicyID] {
				continue
			}
			seen[policy.BudgetPolicyID] = true
			policies = append(policies, policyWithAggregate{Policy: policy, Reserved: reserved, Committed: committed})
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	sort.Slice(policies, func(i, j int) bool {
		return policies[i].Policy.ScopeKey < policies[j].Policy.ScopeKey ||
			(policies[i].Policy.ScopeKey == policies[j].Policy.ScopeKey && policies[i].Policy.BudgetPolicyID < policies[j].Policy.BudgetPolicyID)
	})
	return policies, nil
}

func checkRequirementConfidence(req ReserveRequest) error {
	switch req.RequirementConfidence {
	case providerinventory.ConfidenceExact:
		return nil
	case providerinventory.ConfidenceEstimated:
		if strings.TrimSpace(req.ApprovalID) != "" {
			return nil
		}
		return fmt.Errorf("%w: estimated requirement requires explicit approval", providerinventory.ErrQuotaConfidenceInsufficient)
	default:
		return fmt.Errorf("%w: requirement confidence %q cannot reserve without approval", providerinventory.ErrQuotaConfidenceInsufficient, req.RequirementConfidence)
	}
}

func checkPolicyCapacity(policies []policyWithAggregate, value int64) error {
	for _, policy := range policies {
		if policy.Policy.PolicyMode != PolicyHard {
			continue
		}
		available := policy.Policy.CeilingValue - policy.Reserved - policy.Committed
		if value > available {
			return fmt.Errorf("%w: policy %s available=%d requested=%d", ErrBudgetExhausted, policy.Policy.BudgetPolicyID, available, value)
		}
	}
	return nil
}

func insertRefusal(ctx context.Context, tx storage.Tx, now time.Time, reservationID string, req ReserveRequest, fingerprint string, policyIDs []string, refusal error) error {
	at := formatTime(now)
	code := errorCode(refusal)
	reservation := normalizeReservation(Reservation{
		BudgetReservationID:   reservationID,
		IdempotencyKey:        req.IdempotencyKey,
		RequestFingerprint:    fingerprint,
		Scope:                 req.ScopeChain[len(req.ScopeChain)-1],
		QuantityKind:          req.QuantityKind,
		Unit:                  req.Unit,
		ValueScale:            req.ValueScale,
		RequestedValue:        req.RequestedValue,
		ReservedValue:         0,
		State:                 StateRefused,
		Generation:            1,
		LeaseExpiresAt:        formatTime(req.LeaseExpiresAt),
		PolicyIDs:             policyIDs,
		RequirementConfidence: req.RequirementConfidence,
		ApprovalID:            req.ApprovalID,
		CreatedAt:             at,
		UpdatedAt:             at,
		CreatedBy:             req.Actor,
		UpdatedBy:             req.Actor,
		Host:                  req.Host,
		GapReasons:            append(reserveGapReasons(req), code),
		TerminalErrorCode:     code,
	})
	if err := insertReservation(ctx, tx, reservation); err != nil {
		return err
	}
	_ = insertEvent(ctx, tx, eventRecord{
		OperationKey:  "refuse:" + req.IdempotencyKey,
		ReservationID: reservationID,
		EventKind:     "refuse",
		Generation:    1,
		EventTime:     at,
		Actor:         req.Actor,
		Host:          req.Host,
	})
	return nil
}

func insertReservation(ctx context.Context, tx storage.Tx, reservation Reservation) error {
	if err := ValidateReservation(reservation); err != nil {
		return err
	}
	payload, _ := json.Marshal(reservation)
	policyIDs, _ := json.Marshal(reservation.PolicyIDs)
	_, err := tx.Exec(ctx, `INSERT INTO budget_reservations(
		budget_reservation_id, idempotency_key, request_fingerprint, project_id, delivery_run_id,
		task_id, worker_id, sub_agent_id, adapter_id, account_profile_id, model_capability_id,
		quantity_kind, unit, value_scale, requested_value, reserved_value, committed_value,
		released_value, state, generation, lease_expires_at, scope_key, policy_ids_json,
		payload_json, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		reservation.BudgetReservationID, reservation.IdempotencyKey, reservation.RequestFingerprint,
		reservation.Scope.ProjectID, reservation.Scope.DeliveryRunID, reservation.Scope.TaskID,
		reservation.Scope.WorkerID, reservation.Scope.SubAgentID, reservation.Scope.AdapterID,
		reservation.Scope.AccountProfileID, reservation.Scope.ModelCapabilityID,
		string(reservation.QuantityKind), reservation.Unit, reservation.ValueScale,
		reservation.RequestedValue, reservation.ReservedValue, reservation.CommittedValue,
		reservation.ReleasedValue, string(reservation.State), reservation.Generation,
		reservation.LeaseExpiresAt, reservation.ScopeKey, string(policyIDs), string(payload),
		reservation.CreatedAt, reservation.UpdatedAt)
	return err
}

func updateReservation(ctx context.Context, tx storage.Tx, reservation Reservation) error {
	if err := ValidateReservation(reservation); err != nil {
		return err
	}
	payload, _ := json.Marshal(reservation)
	res, err := tx.Exec(ctx, `UPDATE budget_reservations SET
		reserved_value = ?, committed_value = ?, released_value = ?, state = ?, generation = ?,
		payload_json = ?, updated_at = ?
		WHERE budget_reservation_id = ?`,
		reservation.ReservedValue, reservation.CommittedValue, reservation.ReleasedValue,
		string(reservation.State), reservation.Generation, string(payload), reservation.UpdatedAt,
		reservation.BudgetReservationID)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("%w: reservation update affected %d rows", ErrReservationStateConflict, rows)
	}
	return nil
}

func addAggregate(ctx context.Context, tx storage.Tx, policyID string, deltaReserved, deltaCommitted int64, updatedAt string) error {
	res, err := tx.Exec(ctx, `UPDATE budget_aggregates SET
		reserved_value = reserved_value + ?,
		committed_value = committed_value + ?,
		record_version = record_version + 1,
		updated_at = ?
		WHERE budget_policy_id = ?
			AND reserved_value + ? >= 0
			AND committed_value + ? >= 0`,
		deltaReserved, deltaCommitted, updatedAt, policyID, deltaReserved, deltaCommitted)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("%w: aggregate for policy %s would go negative", ErrReservationStateConflict, policyID)
	}
	return nil
}

func insertEvent(ctx context.Context, tx storage.Tx, event eventRecord) error {
	actor, _ := json.Marshal(event.Actor)
	host, _ := json.Marshal(event.Host)
	payload, _ := json.Marshal(map[string]any{
		"schema_version": EventSchema,
		"operation_key":  event.OperationKey,
	})
	_, err := tx.Exec(ctx, `INSERT OR IGNORE INTO quota_budget_events(
		event_id, idempotency_key, budget_reservation_id, budget_policy_id, event_kind,
		generation, delta_reserved, delta_committed, event_time, actor_json, host_json, payload_json
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"qbe_"+hashBase32(event.OperationKey)[:26], event.OperationKey, event.ReservationID,
		event.BudgetPolicyID, event.EventKind, event.Generation, event.DeltaReserved,
		event.DeltaCommitted, event.EventTime, string(actor), string(host), string(payload))
	return err
}

func eventExists(ctx context.Context, tx storage.Tx, operationKey string) bool {
	var count int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM quota_budget_events WHERE idempotency_key = ?`, operationKey).Scan(&count); err != nil {
		return false
	}
	return count > 0
}

func loadReservationByIdempotency(ctx context.Context, tx storage.Tx, key string) (Reservation, bool, error) {
	var payload string
	err := tx.QueryRow(ctx, `SELECT payload_json FROM budget_reservations WHERE idempotency_key = ?`, key).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return Reservation{}, false, nil
	}
	if err != nil {
		return Reservation{}, false, err
	}
	var reservation Reservation
	if err := json.Unmarshal([]byte(payload), &reservation); err != nil {
		return Reservation{}, false, err
	}
	return reservation, true, nil
}

func loadReservation(ctx context.Context, tx storage.Tx, id string) (Reservation, bool, error) {
	var payload string
	err := tx.QueryRow(ctx, `SELECT payload_json FROM budget_reservations WHERE budget_reservation_id = ?`, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return Reservation{}, false, nil
	}
	if err != nil {
		return Reservation{}, false, err
	}
	var reservation Reservation
	if err := json.Unmarshal([]byte(payload), &reservation); err != nil {
		return Reservation{}, false, err
	}
	return reservation, true, nil
}

func activeReservationIDs(ctx context.Context, tx storage.Tx, policyID string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT budget_reservation_id FROM budget_reservations
		WHERE state = ? AND EXISTS (
			SELECT 1 FROM json_each(policy_ids_json) WHERE value = ?
		)
		ORDER BY budget_reservation_id`, string(StateActive), policyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, rows.Err()
}

func ValidatePolicy(policy Policy) error {
	policy = normalizePolicy(policy)
	if policy.SchemaVersion != PolicySchema || policy.RecordVersion != 1 {
		return fmt.Errorf("%w: invalid policy schema or record version", ErrBudgetRecordMalformed)
	}
	if !strings.HasPrefix(policy.BudgetPolicyID, "bpol_") || strings.TrimSpace(policy.ScopeKey) == "" {
		return fmt.Errorf("%w: policy id and scope key are required", ErrBudgetRecordMalformed)
	}
	if !knownScopeKind(policy.Scope.ScopeKind) || !knownPolicyMode(policy.PolicyMode) {
		return fmt.Errorf("%w: unknown policy enum", providerinventory.ErrInvalidRecord)
	}
	if policy.CeilingValue < 0 || policy.ValueScale < 0 {
		return fmt.Errorf("%w: negative policy value", ErrBudgetRecordMalformed)
	}
	if policy.QuantityKind == "" || policy.WindowKind == "" || policy.Unit == "" || policy.PolicyVersion == "" {
		return fmt.Errorf("%w: quantity, window, unit, and policy version are required", ErrBudgetRecordMalformed)
	}
	return nil
}

func ValidateReservation(reservation Reservation) error {
	reservation = normalizeReservation(reservation)
	if reservation.SchemaVersion != ReservationSchema || reservation.RecordVersion != 1 {
		return fmt.Errorf("%w: invalid reservation schema or record version", ErrBudgetRecordMalformed)
	}
	if !strings.HasPrefix(reservation.BudgetReservationID, "bres_") || strings.TrimSpace(reservation.IdempotencyKey) == "" || strings.TrimSpace(reservation.ScopeKey) == "" {
		return fmt.Errorf("%w: reservation id, idempotency key, and scope key are required", ErrBudgetRecordMalformed)
	}
	if !knownReservationState(reservation.State) || !knownScopeKind(reservation.Scope.ScopeKind) {
		return fmt.Errorf("%w: unknown reservation enum", providerinventory.ErrInvalidRecord)
	}
	if reservation.RequestedValue < 0 || reservation.ReservedValue < 0 || reservation.CommittedValue < 0 || reservation.ReleasedValue < 0 || reservation.ValueScale < 0 {
		return fmt.Errorf("%w: negative reservation value", ErrBudgetRecordMalformed)
	}
	if reservation.Generation <= 0 {
		return fmt.Errorf("%w: generation must be positive", ErrBudgetRecordMalformed)
	}
	if _, err := time.Parse(time.RFC3339Nano, reservation.LeaseExpiresAt); err != nil {
		return fmt.Errorf("%w: lease_expires_at must be RFC3339", ErrBudgetRecordMalformed)
	}
	return nil
}

func ScopeKey(scope Scope) (string, error) {
	scope = normalizeScope(scope)
	if !knownScopeKind(scope.ScopeKind) {
		return "", fmt.Errorf("%w: unknown scope %q", providerinventory.ErrInvalidRecord, scope.ScopeKind)
	}
	if scope.ScopeKind != ScopeMachine && strings.TrimSpace(scope.ProjectID) == "" {
		return "", fmt.Errorf("%w: project_id is required for %s scope", ErrBudgetScopeConflict, scope.ScopeKind)
	}
	payload, err := json.Marshal(scope)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func normalizePolicy(policy Policy) Policy {
	if policy.SchemaVersion == "" {
		policy.SchemaVersion = PolicySchema
	}
	if policy.RecordVersion == 0 {
		policy.RecordVersion = 1
	}
	policy.Scope = normalizeScope(policy.Scope)
	if policy.ScopeKey == "" {
		policy.ScopeKey, _ = ScopeKey(policy.Scope)
	}
	if policy.QuantityKind == "" {
		policy.QuantityKind = providerinventory.QuantityLocalPolicy
	}
	if policy.Unit == "" {
		policy.Unit = unitForQuantity(policy.QuantityKind)
	}
	if policy.WindowKind == "" {
		policy.WindowKind = providerinventory.WindowUnbounded
	}
	if policy.PolicyMode == "" {
		policy.PolicyMode = PolicyHard
	}
	if policy.PolicyVersion == "" {
		policy.PolicyVersion = "local-v1"
	}
	if policy.BudgetPolicyID == "" {
		policy.BudgetPolicyID = policyID(policy.ScopeKey, policy.QuantityKind, policy.WindowKind, policy.PolicyVersion, "0")
	}
	if policy.GapReasons == nil {
		policy.GapReasons = []string{}
	}
	policy.GapReasons = dedupeStrings(policy.GapReasons)
	return policy
}

func normalizeReservation(reservation Reservation) Reservation {
	if reservation.SchemaVersion == "" {
		reservation.SchemaVersion = ReservationSchema
	}
	if reservation.RecordVersion == 0 {
		reservation.RecordVersion = 1
	}
	reservation.Scope = normalizeScope(reservation.Scope)
	if reservation.ScopeKey == "" {
		reservation.ScopeKey, _ = ScopeKey(reservation.Scope)
	}
	if reservation.QuantityKind == "" {
		reservation.QuantityKind = providerinventory.QuantityLocalPolicy
	}
	if reservation.Unit == "" {
		reservation.Unit = unitForQuantity(reservation.QuantityKind)
	}
	if reservation.State == "" {
		reservation.State = StateActive
	}
	if reservation.Generation == 0 {
		reservation.Generation = 1
	}
	if reservation.PolicyIDs == nil {
		reservation.PolicyIDs = []string{}
	}
	if reservation.GapReasons == nil {
		reservation.GapReasons = []string{}
	}
	reservation.PolicyIDs = dedupeStrings(reservation.PolicyIDs)
	reservation.GapReasons = dedupeStrings(reservation.GapReasons)
	return reservation
}

func normalizeReserveRequest(req ReserveRequest) ReserveRequest {
	for i := range req.ScopeChain {
		req.ScopeChain[i] = normalizeScope(req.ScopeChain[i])
	}
	if req.QuantityKind == "" {
		req.QuantityKind = providerinventory.QuantityLocalPolicy
	}
	if req.Unit == "" {
		req.Unit = unitForQuantity(req.QuantityKind)
	}
	if req.WindowKind == "" {
		req.WindowKind = providerinventory.WindowUnbounded
	}
	if req.RequirementConfidence == "" {
		req.RequirementConfidence = providerinventory.ConfidenceUnknown
	}
	return req
}

func validateReserveRequest(req ReserveRequest) error {
	if len(req.ScopeChain) == 0 {
		return fmt.Errorf("%w: scope chain is required", ErrBudgetScopeConflict)
	}
	for _, scope := range req.ScopeChain {
		if _, err := ScopeKey(scope); err != nil {
			return err
		}
	}
	if strings.TrimSpace(req.IdempotencyKey) == "" || req.RequestedValue < 0 {
		return fmt.Errorf("%w: idempotency key and non-negative value are required", ErrReservationStateConflict)
	}
	if req.LeaseExpiresAt.IsZero() {
		return fmt.Errorf("%w: lease expiry is required", ErrReservationExpired)
	}
	return nil
}

func normalizeScope(scope Scope) Scope {
	scope.ProjectID = strings.TrimSpace(scope.ProjectID)
	scope.DeliveryRunID = strings.TrimSpace(scope.DeliveryRunID)
	scope.TaskID = strings.TrimSpace(scope.TaskID)
	scope.WorkerID = strings.TrimSpace(scope.WorkerID)
	scope.SubAgentID = strings.TrimSpace(scope.SubAgentID)
	scope.AdapterID = strings.TrimSpace(scope.AdapterID)
	scope.AccountProfileID = strings.TrimSpace(scope.AccountProfileID)
	scope.ModelCapabilityID = strings.TrimSpace(scope.ModelCapabilityID)
	return scope
}

func reserveFingerprint(req ReserveRequest) string {
	type canonical struct {
		ScopeChain            []Scope                        `json:"scope_chain"`
		QuantityKind          providerinventory.QuantityKind `json:"quantity_kind"`
		Unit                  string                         `json:"unit"`
		ValueScale            int                            `json:"value_scale"`
		WindowKind            providerinventory.WindowKind   `json:"window_kind"`
		RequestedValue        int64                          `json:"requested_value"`
		LeaseExpiresAt        string                         `json:"lease_expires_at"`
		RequirementConfidence providerinventory.Confidence   `json:"requirement_confidence"`
		ApprovalID            string                         `json:"approval_id,omitempty"`
	}
	data, _ := json.Marshal(canonical{
		ScopeChain:            req.ScopeChain,
		QuantityKind:          req.QuantityKind,
		Unit:                  req.Unit,
		ValueScale:            req.ValueScale,
		WindowKind:            req.WindowKind,
		RequestedValue:        req.RequestedValue,
		LeaseExpiresAt:        formatTime(req.LeaseExpiresAt),
		RequirementConfidence: req.RequirementConfidence,
		ApprovalID:            req.ApprovalID,
	})
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func reserveGapReasons(req ReserveRequest) []string {
	switch req.RequirementConfidence {
	case providerinventory.ConfidenceExact:
		return []string{}
	case providerinventory.ConfidenceEstimated:
		if strings.TrimSpace(req.ApprovalID) != "" {
			return []string{"estimated-requirement-approved"}
		}
		return []string{"estimated-requirement-needs-approval"}
	default:
		return []string{"unknown-or-unavailable-requirement"}
	}
}

func policyIDs(policies []policyWithAggregate) []string {
	out := make([]string, 0, len(policies))
	for _, policy := range policies {
		out = append(out, policy.Policy.BudgetPolicyID)
	}
	sort.Strings(out)
	return out
}

func policyID(scopeKey string, quantity providerinventory.QuantityKind, window providerinventory.WindowKind, version, ordinal string) string {
	return "bpol_" + hashBase32(scopeKey, string(quantity), string(window), version, ordinal)[:26]
}

func unitForQuantity(kind providerinventory.QuantityKind) string {
	switch kind {
	case providerinventory.QuantityInputTokens, providerinventory.QuantityOutputTokens, providerinventory.QuantityTotalTokens:
		return "token"
	case providerinventory.QuantityRequests:
		return "request"
	case providerinventory.QuantityWallMS:
		return "millisecond"
	case providerinventory.QuantityConcurrency:
		return "slot"
	default:
		return "local-policy-unit"
	}
}

func expired(now time.Time, lease string) bool {
	parsed, err := time.Parse(time.RFC3339Nano, lease)
	if err != nil {
		return true
	}
	return !now.UTC().Before(parsed.UTC())
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func hashBase32(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:]))
}

func knownScopeKind(kind ScopeKind) bool {
	switch kind {
	case ScopeMachine, ScopeProject, ScopeDeliveryRun, ScopeTask, ScopeWorker, ScopeSubAgent, ScopeProvider:
		return true
	default:
		return false
	}
}

func knownPolicyMode(mode PolicyMode) bool {
	return mode == PolicyHard || mode == PolicySoft
}

func knownReservationState(state ReservationState) bool {
	switch state {
	case StateActive, StateCommitted, StateReleased, StateCancelled, StateExpired, StateRefused:
		return true
	default:
		return false
	}
}

func withBudgetRetry(ctx context.Context, op func() error) error {
	var err error
	for attempt := 0; attempt < 8; attempt++ {
		err = op()
		if err == nil || !busyError(err) {
			return err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return err
}

func busyError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "sqlite_busy") || strings.Contains(text, "database is locked") || strings.Contains(text, "database table is locked")
}

func errorCode(err error) string {
	switch {
	case errors.Is(err, ErrBudgetExhausted):
		return "ErrBudgetExhausted"
	case errors.Is(err, providerinventory.ErrQuotaConfidenceInsufficient):
		return "ErrQuotaConfidenceInsufficient"
	case errors.Is(err, ErrReservationExpired):
		return "ErrReservationExpired"
	case errors.Is(err, ErrReservationStateConflict):
		return "ErrReservationStateConflict"
	default:
		return "ErrBudgetRefused"
	}
}

func dedupeStrings(values []string) []string {
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
