package availability

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jasonhnd/loopcoder/internal/storage"
)

func Persist(ctx context.Context, store storage.Store, result Result) error {
	if store == nil {
		return errors.New("availability persist: storage store is required")
	}
	return store.WithWriteTx(ctx, func(tx storage.Tx) error {
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
