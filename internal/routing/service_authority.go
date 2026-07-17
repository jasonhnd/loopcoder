package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

const (
	routeAttemptAuthoritySchema    = "loopcoder.route_attempt_authority.v1"
	routeAttemptAuthorityOperation = "route_first_authority_v1"
)

type routeAttemptIdentity struct {
	SchemaVersion   string `json:"schema_version"`
	ProjectID       string `json:"project_id"`
	DeliveryRunID   string `json:"delivery_run_id"`
	DecisionKey     string `json:"decision_key"`
	RuntimeHostName string `json:"runtime_host_name"`
}

type routeAttemptAuthority struct {
	SchemaVersion     string `json:"schema_version"`
	ProjectID         string `json:"project_id"`
	DeliveryRunID     string `json:"delivery_run_id"`
	DecisionKey       string `json:"decision_key"`
	RuntimeHostName   string `json:"runtime_host_name"`
	RoutingDecisionID string `json:"routing_decision_id"`
}

type routeAttemptAuthorityRow struct {
	projectID     string
	deliveryRunID string
	operation     string
	requestHash   string
	requestJSON   string
	resultJSON    string
}

func routeAttemptIdentityFor(projectID, deliveryRunID, decisionKey, runtimeHostName string) routeAttemptIdentity {
	return routeAttemptIdentity{
		SchemaVersion:   routeAttemptAuthoritySchema,
		ProjectID:       strings.TrimSpace(projectID),
		DeliveryRunID:   strings.TrimSpace(deliveryRunID),
		DecisionKey:     strings.TrimSpace(decisionKey),
		RuntimeHostName: strings.TrimSpace(runtimeHostName),
	}
}

func routeAttemptAuthorityKey(identity routeAttemptIdentity) string {
	// RuntimeHostName is deliberately excluded from the lookup key. A changed
	// host must find the same attempt authority and fail its request binding,
	// not create a second first-decision namespace.
	return "routeauth_" + digestBase32(
		routeAttemptAuthoritySchema,
		identity.ProjectID,
		identity.DeliveryRunID,
		identity.DecisionKey,
	)[:32]
}

func loadRouteAttemptPrior(ctx context.Context, store storage.Store, projectID, deliveryRunID, decisionKey, runtimeHostName string) (RoutingDecision, bool, bool, error) {
	decision, found, err := loadRouteAttemptAuthority(ctx, store, projectID, deliveryRunID, decisionKey, runtimeHostName)
	if err != nil || found {
		return decision, found, found, err
	}
	decision, found, err = loadLegacySingleRoutingDecisionByKey(ctx, store, projectID, deliveryRunID, decisionKey)
	return decision, found, false, err
}

func loadRouteAttemptAuthority(ctx context.Context, store storage.Store, projectID, deliveryRunID, decisionKey, runtimeHostName string) (RoutingDecision, bool, error) {
	identity := routeAttemptIdentityFor(projectID, deliveryRunID, decisionKey, runtimeHostName)
	key := routeAttemptAuthorityKey(identity)
	var row routeAttemptAuthorityRow
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT project_id, delivery_run_id, operation, request_hash, request_json, result_json
			FROM delivery_idempotency WHERE idempotency_key = ?`, key).Scan(
			&row.projectID,
			&row.deliveryRunID,
			&row.operation,
			&row.requestHash,
			&row.requestJSON,
			&row.resultJSON,
		)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return RoutingDecision{}, false, nil
	}
	if err != nil {
		return RoutingDecision{}, false, err
	}
	authority, err := validateRouteAttemptAuthorityRow(identity, row)
	if err != nil {
		return RoutingDecision{}, false, err
	}
	decision, err := LoadRoutingDecision(ctx, store, authority.RoutingDecisionID)
	if err != nil {
		return RoutingDecision{}, false, routeAuthorityMismatch("stored route authority references a missing or unreadable routing decision")
	}
	if err := validateRoutingDecision(decision); err != nil {
		return RoutingDecision{}, false, routeAuthorityMismatch("stored route authority references an invalid routing decision")
	}
	if decision.RoutingDecisionID != authority.RoutingDecisionID ||
		decision.ProjectID != identity.ProjectID ||
		decision.DeliveryRunID != identity.DeliveryRunID ||
		decision.DecisionKey != identity.DecisionKey ||
		decision.RuntimeHostName != identity.RuntimeHostName {
		return RoutingDecision{}, false, routeAuthorityMismatch("stored route authority does not match its routing decision")
	}
	return decision, true, nil
}

func claimRouteAttemptAuthority(ctx context.Context, store storage.Store, decision RoutingDecision) error {
	identity := routeAttemptIdentityFor(decision.ProjectID, decision.DeliveryRunID, decision.DecisionKey, decision.RuntimeHostName)
	if identity.RuntimeHostName == "" {
		return routeAuthorityMismatch("first routing decision is missing runtime host authority")
	}
	requestJSON, err := delivery.CanonicalJSON(identity)
	if err != nil {
		return err
	}
	authority := routeAttemptAuthority{
		SchemaVersion:     routeAttemptAuthoritySchema,
		ProjectID:         identity.ProjectID,
		DeliveryRunID:     identity.DeliveryRunID,
		DecisionKey:       identity.DecisionKey,
		RuntimeHostName:   identity.RuntimeHostName,
		RoutingDecisionID: decision.RoutingDecisionID,
	}
	resultJSON, err := delivery.CanonicalJSON(authority)
	if err != nil {
		return err
	}
	key := routeAttemptAuthorityKey(identity)
	requestHash := delivery.SHA256Digest(requestJSON)
	return store.WithWriteTx(ctx, func(tx storage.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO delivery_idempotency(
			idempotency_key, project_id, delivery_run_id, operation, request_hash, request_json, result_json, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(idempotency_key) DO NOTHING`,
			key,
			identity.ProjectID,
			identity.DeliveryRunID,
			routeAttemptAuthorityOperation,
			requestHash,
			string(requestJSON),
			string(resultJSON),
			delivery.CanonicalTimestamp(store.Now()),
		); err != nil {
			return err
		}
		var row routeAttemptAuthorityRow
		if err := tx.QueryRow(ctx, `SELECT project_id, delivery_run_id, operation, request_hash, request_json, result_json
			FROM delivery_idempotency WHERE idempotency_key = ?`, key).Scan(
			&row.projectID,
			&row.deliveryRunID,
			&row.operation,
			&row.requestHash,
			&row.requestJSON,
			&row.resultJSON,
		); err != nil {
			return err
		}
		stored, err := validateRouteAttemptAuthorityRow(identity, row)
		if err != nil {
			return err
		}
		if stored.RoutingDecisionID != decision.RoutingDecisionID {
			return routeAuthorityMismatch("another first routing decision already owns this execution attempt")
		}
		return nil
	})
}

func validateRouteAttemptAuthorityRow(identity routeAttemptIdentity, row routeAttemptAuthorityRow) (routeAttemptAuthority, error) {
	requestJSON, err := delivery.CanonicalJSON(identity)
	if err != nil {
		return routeAttemptAuthority{}, err
	}
	if row.projectID != identity.ProjectID ||
		row.deliveryRunID != identity.DeliveryRunID ||
		row.operation != routeAttemptAuthorityOperation ||
		row.requestHash != delivery.SHA256Digest(requestJSON) ||
		row.requestJSON != string(requestJSON) {
		return routeAttemptAuthority{}, routeAuthorityMismatch("stored route authority identity is inconsistent")
	}
	var authority routeAttemptAuthority
	if err := json.Unmarshal([]byte(row.resultJSON), &authority); err != nil {
		return routeAttemptAuthority{}, routeAuthorityMismatch("stored route authority result is invalid")
	}
	if authority.SchemaVersion != routeAttemptAuthoritySchema ||
		authority.ProjectID != identity.ProjectID ||
		authority.DeliveryRunID != identity.DeliveryRunID ||
		authority.DecisionKey != identity.DecisionKey ||
		authority.RuntimeHostName != identity.RuntimeHostName ||
		strings.TrimSpace(authority.RoutingDecisionID) == "" {
		return routeAttemptAuthority{}, routeAuthorityMismatch("stored route authority result is inconsistent")
	}
	return authority, nil
}

func loadLegacySingleRoutingDecisionByKey(ctx context.Context, store storage.Store, projectID, deliveryRunID, decisionKey string) (RoutingDecision, bool, error) {
	type decisionRow struct {
		id      string
		payload string
	}
	var rowsFound []decisionRow
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		rows, err := tx.Query(ctx, `SELECT routing_decision_id, payload_json FROM routing_decisions
			WHERE project_id = ? AND delivery_run_id = ? AND decision_key = ?
			ORDER BY created_at, routing_decision_id LIMIT 2`,
			strings.TrimSpace(projectID), strings.TrimSpace(deliveryRunID), strings.TrimSpace(decisionKey))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var row decisionRow
			if err := rows.Scan(&row.id, &row.payload); err != nil {
				return err
			}
			rowsFound = append(rowsFound, row)
		}
		return rows.Err()
	})
	if err != nil {
		return RoutingDecision{}, false, err
	}
	if len(rowsFound) == 0 {
		return RoutingDecision{}, false, nil
	}
	if len(rowsFound) > 1 {
		return RoutingDecision{}, false, routeAuthorityMismatch("multiple first routing decisions exist for one execution attempt")
	}
	var decision RoutingDecision
	if err := json.Unmarshal([]byte(rowsFound[0].payload), &decision); err != nil {
		return RoutingDecision{}, false, routeAuthorityMismatch("legacy route authority is unreadable")
	}
	if err := validateRoutingDecision(decision); err != nil {
		return RoutingDecision{}, false, routeAuthorityMismatch("legacy route authority is invalid")
	}
	if decision.RoutingDecisionID != rowsFound[0].id ||
		decision.ProjectID != strings.TrimSpace(projectID) ||
		decision.DeliveryRunID != strings.TrimSpace(deliveryRunID) ||
		decision.DecisionKey != strings.TrimSpace(decisionKey) {
		return RoutingDecision{}, false, routeAuthorityMismatch("legacy route authority identity is inconsistent")
	}
	return decision, true, nil
}

func validateStoredRouteReplay(ctx context.Context, store storage.Store, request StoredRouteRequest, profile RoutingPolicyProfile, prior RoutingDecision) error {
	if prior.TaskRequirementID != request.TaskRequirementID || prior.RoutingPolicyProfileID != profile.RoutingPolicyProfileID || prior.PolicyFingerprint != profile.PolicyFingerprint {
		return routeAuthorityMismatch("stored routing decision authority does not match replay request")
	}
	budgetClass, deadlineClass, err := resolveStoredRouteClasses(ctx, store, request)
	if err != nil {
		return err
	}
	priorBudgetClass := prior.BudgetClass
	priorDeadlineClass := prior.DeadlineClass
	if priorBudgetClass == "" || priorDeadlineClass == "" {
		legacyRequest := request
		legacyRequest.BudgetClass = ""
		legacyRequest.DeadlineClass = ""
		legacyBudgetClass, legacyDeadlineClass, legacyErr := resolveStoredRouteClasses(ctx, store, legacyRequest)
		if legacyErr != nil {
			return legacyErr
		}
		if priorBudgetClass == "" {
			priorBudgetClass = legacyBudgetClass
		}
		if priorDeadlineClass == "" {
			priorDeadlineClass = legacyDeadlineClass
		}
	}
	if priorBudgetClass != budgetClass || priorDeadlineClass != deadlineClass {
		return routeAuthorityMismatch("stored routing decision budget or deadline class does not match replay request")
	}
	if prior.RuntimeHostName == "" || prior.RuntimeHostName != request.HostName {
		return routeAuthorityMismatch("stored routing decision runtime host does not match replay request")
	}
	authorizationFingerprint, err := loadRunAuthorizationFingerprint(ctx, store, request.ProjectID, request.DeliveryRunID)
	if err != nil {
		return err
	}
	if prior.AuthorizationFingerprint == "" || prior.AuthorizationFingerprint != authorizationFingerprint {
		return routeAuthorityMismatch("stored routing decision authorization does not match the active delivery run")
	}
	if request.Pin != nil && !decisionContainsPin(prior, *request.Pin) {
		return routeAuthorityMismatch("stored routing decision cannot be replaced by a different explicit pin")
	}
	return nil
}

func routeAuthorityMismatch(message string) error {
	return &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: message}
}
