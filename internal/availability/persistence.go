package availability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jasonhnd/loopcoder/internal/storage"
)

func Persist(ctx context.Context, store storage.Store, result Result) error {
	if store == nil {
		return errors.New("availability persist: storage store is required")
	}
	return withAvailabilityWriteTx(ctx, store, func(tx storage.Tx) error {
		for _, observation := range result.Observations {
			if err := insertObservation(ctx, tx, observation); err != nil {
				return err
			}
		}
		for _, score := range result.Scores {
			if err := insertScore(ctx, tx, score); err != nil {
				return err
			}
		}
		for _, breaker := range result.CircuitBreakers {
			if err := upsertBreaker(ctx, tx, breaker); err != nil {
				return err
			}
		}
		return nil
	})
}

func Load(ctx context.Context, store storage.Store) (Result, error) {
	if store == nil {
		return Result{}, errors.New("availability load: storage store is required")
	}
	var result Result
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		rows, err := tx.Query(ctx, `SELECT payload_json FROM availability_observations ORDER BY observed_at, availability_observation_id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var payload string
			if err := rows.Scan(&payload); err != nil {
				rows.Close()
				return err
			}
			var observation Observation
			if err := json.Unmarshal([]byte(payload), &observation); err != nil {
				rows.Close()
				return err
			}
			result.Observations = append(result.Observations, observation)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT payload_json FROM availability_scores ORDER BY captured_at, availability_score_id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var payload string
			if err := rows.Scan(&payload); err != nil {
				rows.Close()
				return err
			}
			var score Score
			if err := json.Unmarshal([]byte(payload), &score); err != nil {
				rows.Close()
				return err
			}
			result.Scores = append(result.Scores, score)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT payload_json FROM circuit_breakers ORDER BY circuit_breaker_id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var payload string
			if err := rows.Scan(&payload); err != nil {
				rows.Close()
				return err
			}
			var breaker CircuitBreaker
			if err := json.Unmarshal([]byte(payload), &breaker); err != nil {
				rows.Close()
				return err
			}
			result.CircuitBreakers = append(result.CircuitBreakers, breaker)
		}
		return rows.Close()
	})
	return result, err
}

func insertObservation(ctx context.Context, tx storage.Tx, observation Observation) error {
	observation = normalizeObservation(observation)
	if err := ValidateObservation(observation); err != nil {
		return err
	}
	payload, err := json.Marshal(observation)
	if err != nil {
		return err
	}
	sourceIDs, err := json.Marshal(observation.SourceRecordIDs)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT OR IGNORE INTO availability_observations(
		availability_observation_id, observation_kind, scope_key, adapter_id, provider_installation_id,
		account_profile_id, model_capability_id, failure_class, observed_at, confidence, source_record_ids_json, payload_json
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		observation.AvailabilityObservationID, string(observation.ObservationKind), observation.ScopeKey,
		observation.Scope.AdapterID, observation.Scope.ProviderInstallationID, observation.Scope.AccountProfileID,
		observation.Scope.ModelCapabilityID, string(observation.FailureClass), observation.ObservedAt, string(observation.Confidence),
		string(sourceIDs), string(payload))
	return err
}

func insertScore(ctx context.Context, tx storage.Tx, score Score) error {
	score = normalizeScore(score)
	if err := ValidateScore(score); err != nil {
		return err
	}
	payload, err := json.Marshal(score)
	if err != nil {
		return err
	}
	hard, err := json.Marshal(score.HardIneligibleReasons)
	if err != nil {
		return err
	}
	evidence, err := json.Marshal(score.EvidenceRecordIDs)
	if err != nil {
		return err
	}
	eligible := 0
	if score.Eligible {
		eligible = 1
	}
	_, err = tx.Exec(ctx, `INSERT OR IGNORE INTO availability_scores(
		availability_score_id, scope_key, adapter_id, provider_installation_id, account_profile_id,
		model_capability_id, score, eligible, score_confidence, hard_ineligible_reasons_json,
		evidence_record_ids_json, captured_at, policy_version, payload_json
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		score.AvailabilityScoreID, score.ScopeKey, score.Scope.AdapterID, score.Scope.ProviderInstallationID,
		score.Scope.AccountProfileID, score.Scope.ModelCapabilityID, score.Score, eligible, string(score.ScoreConfidence),
		string(hard), string(evidence), score.CapturedAt, score.PolicyVersion, string(payload))
	return err
}

func upsertBreaker(ctx context.Context, tx storage.Tx, breaker CircuitBreaker) error {
	breaker = normalizeBreaker(breaker)
	if err := ValidateBreaker(breaker); err != nil {
		return err
	}
	payload, err := json.Marshal(breaker)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO circuit_breakers(
		circuit_breaker_id, breaker_kind, state, scope_key, adapter_id, model_capability_id,
		open_until, last_observation_id, state_reason, policy_version, record_version, payload_json,
		opened_at, half_open_probe_budget, half_open_probe_count, failure_count, success_count,
		last_observation_at, probe_lease_owner, probe_lease_expires_at, probe_lease_generation
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(circuit_breaker_id) DO UPDATE SET
		breaker_kind = excluded.breaker_kind,
		state = excluded.state,
		scope_key = excluded.scope_key,
		adapter_id = excluded.adapter_id,
		model_capability_id = excluded.model_capability_id,
		open_until = excluded.open_until,
		last_observation_id = excluded.last_observation_id,
		state_reason = excluded.state_reason,
		policy_version = excluded.policy_version,
		record_version = excluded.record_version,
		payload_json = excluded.payload_json,
		opened_at = excluded.opened_at,
		half_open_probe_budget = excluded.half_open_probe_budget,
		half_open_probe_count = excluded.half_open_probe_count,
		failure_count = excluded.failure_count,
		success_count = excluded.success_count,
		last_observation_at = excluded.last_observation_at,
		probe_lease_owner = excluded.probe_lease_owner,
		probe_lease_expires_at = excluded.probe_lease_expires_at,
		probe_lease_generation = excluded.probe_lease_generation`,
		breaker.CircuitBreakerID, string(breaker.BreakerKind), string(breaker.State), breaker.ScopeKey,
		breaker.Scope.AdapterID, breaker.Scope.ModelCapabilityID, breaker.OpenUntil, breaker.LastObservationID,
		string(breaker.StateReason), breaker.PolicyVersion, breaker.RecordVersion, string(payload),
		breaker.OpenedAt, breaker.HalfOpenProbeBudget, breaker.HalfOpenProbeCount, breaker.FailureCount,
		breaker.SuccessCount, breaker.LastObservationAt, breaker.ProbeLeaseOwner, breaker.ProbeLeaseExpiresAt,
		breaker.ProbeLeaseGeneration)
	return err
}

type ProbeLease struct {
	Acquired bool           `json:"acquired"`
	Breaker  CircuitBreaker `json:"circuit_breaker"`
	Reason   ReasonCode     `json:"reason,omitempty"`
}

func AcquireHalfOpenProbeLease(ctx context.Context, store storage.Store, breakerID string, owner string, policy Policy, now time.Time) (ProbeLease, error) {
	if store == nil {
		return ProbeLease{}, errors.New("availability probe lease: storage store is required")
	}
	if breakerID == "" {
		return ProbeLease{}, errors.New("availability probe lease: breaker id is required")
	}
	owner = firstNonEmpty(owner, "anonymous-probe")
	policy = normalizePolicy(policy)
	now = now.UTC()
	if now.IsZero() {
		now = store.Now().UTC()
	}
	var lease ProbeLease
	err := withAvailabilityWriteTx(ctx, store, func(tx storage.Tx) error {
		breaker, err := loadBreakerForUpdate(ctx, tx, breakerID)
		if err != nil {
			return err
		}
		breaker = advanceBreakerCooldown(breaker, policy, now)
		if breaker.State != BreakerHalfOpen {
			lease = ProbeLease{Acquired: false, Breaker: breaker, Reason: ReasonOpenBreaker}
			return upsertBreaker(ctx, tx, breaker)
		}
		if breaker.HalfOpenProbeBudget <= 0 {
			breaker.HalfOpenProbeBudget = policy.HalfOpenProbeBudget
		}
		if breaker.HalfOpenProbeCount >= breaker.HalfOpenProbeBudget {
			lease = ProbeLease{Acquired: false, Breaker: breaker, Reason: ReasonProbeLeaseActive}
			return upsertBreaker(ctx, tx, breaker)
		}
		if activeLease(breaker, owner, now) {
			lease = ProbeLease{Acquired: false, Breaker: breaker, Reason: ReasonProbeLeaseActive}
			return nil
		}
		before := breaker
		breaker.ProbeLeaseOwner = owner
		breaker.ProbeLeaseExpiresAt = formatTime(now.Add(policy.ProbeLeaseDuration))
		breaker.ProbeLeaseGeneration++
		breaker.HalfOpenProbeCount++
		breaker.StateReason = ReasonCooldownElapsed
		if breakerChanged(before, breaker) {
			breaker.RecordVersion = before.RecordVersion + 1
		}
		if err := upsertBreaker(ctx, tx, breaker); err != nil {
			return err
		}
		lease = ProbeLease{Acquired: true, Breaker: breaker}
		return nil
	})
	return lease, err
}

func loadBreakerForUpdate(ctx context.Context, tx storage.Tx, breakerID string) (CircuitBreaker, error) {
	var payload string
	if err := tx.QueryRow(ctx, `SELECT payload_json FROM circuit_breakers WHERE circuit_breaker_id = ?`, breakerID).Scan(&payload); err != nil {
		return CircuitBreaker{}, fmt.Errorf("%w: load breaker %s: %v", ErrBreakerOpen, breakerID, err)
	}
	var breaker CircuitBreaker
	if err := json.Unmarshal([]byte(payload), &breaker); err != nil {
		return CircuitBreaker{}, err
	}
	return normalizeBreaker(breaker), nil
}

func activeLease(breaker CircuitBreaker, owner string, now time.Time) bool {
	if breaker.ProbeLeaseOwner == "" || breaker.ProbeLeaseOwner == owner {
		return false
	}
	expires := parseOptionalTime(breaker.ProbeLeaseExpiresAt)
	return expires != nil && now.Before(*expires)
}

func withAvailabilityWriteTx(ctx context.Context, store storage.Store, fn func(storage.Tx) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	var err error
	for attempt := 0; attempt < 8; attempt++ {
		err = store.WithWriteTx(ctx, fn)
		if err == nil || !storage.IsBusy(err) {
			return err
		}
		delay := time.Duration(attempt+1) * 10 * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return err
}
