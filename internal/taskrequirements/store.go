package taskrequirements

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

type PersistOptions struct {
	Now time.Time
}

func PersistTaskRequirement(ctx context.Context, store storage.Store, req TaskRequirement, opts PersistOptions) (TaskRequirement, error) {
	if store == nil {
		return TaskRequirement{}, typed(ErrInvalidRecordCode, "store is required")
	}
	now := optionNow(opts)
	req = normalizeRequirement(req, now)
	if req.TaskRequirementFingerprint == "" {
		fp, err := FingerprintRequirement(req)
		if err != nil {
			return TaskRequirement{}, typed(ErrInvalidRecordCode, "fingerprint requirement: %v", err)
		}
		req.TaskRequirementFingerprint = fp
	}
	if err := Validate(req); err != nil {
		return TaskRequirement{}, err
	}
	payload, err := delivery.CanonicalJSON(req)
	if err != nil {
		return TaskRequirement{}, typed(ErrInvalidRecordCode, "canonical requirement payload: %v", err)
	}
	var out TaskRequirement
	err = store.WithWriteTx(ctx, func(tx storage.Tx) error {
		if err := ensureRunProject(ctx, tx, req.ProjectID, req.DeliveryRunID); err != nil {
			return err
		}
		if err := ensureTaskProject(ctx, tx, req.ProjectID, req.DeliveryRunID, req.TaskID); err != nil {
			return err
		}
		existing, ok, err := loadRequirementByID(ctx, tx, req.TaskRequirementID)
		if err != nil {
			return err
		}
		if ok {
			if existing.TaskRequirementFingerprint == req.TaskRequirementFingerprint {
				out = existing
				return nil
			}
			return typed(ErrDuplicateRecordCode, "task requirement %s already exists with different fingerprint", req.TaskRequirementID)
		}
		if err := insertRequirement(ctx, tx, req, string(payload)); err != nil {
			return err
		}
		out = req
		return nil
	})
	return out, err
}

func LoadTaskRequirement(ctx context.Context, store storage.Store, taskRequirementID string) (TaskRequirement, error) {
	if store == nil {
		return TaskRequirement{}, typed(ErrInvalidRecordCode, "store is required")
	}
	var out TaskRequirement
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		var err error
		var ok bool
		out, ok, err = loadRequirementByID(ctx, tx, taskRequirementID)
		if err != nil {
			return err
		}
		if !ok {
			return typed(ErrMissingReferenceCode, "task requirement %s is missing", taskRequirementID)
		}
		return nil
	})
	return out, err
}

func PersistRequirementOverride(ctx context.Context, store storage.Store, override RequirementOverride, opts PersistOptions) (RequirementOverride, error) {
	if store == nil {
		return RequirementOverride{}, typed(ErrInvalidRecordCode, "store is required")
	}
	now := optionNow(opts)
	override = normalizeOverride(override, now)
	if err := ValidateOverride(override); err != nil {
		return RequirementOverride{}, err
	}
	payload, err := delivery.CanonicalJSON(override)
	if err != nil {
		return RequirementOverride{}, typed(ErrInvalidRecordCode, "canonical override payload: %v", err)
	}
	var out RequirementOverride
	err = store.WithWriteTx(ctx, func(tx storage.Tx) error {
		if override.DeliveryRunID != "" {
			if err := ensureRunProject(ctx, tx, override.ProjectID, override.DeliveryRunID); err != nil {
				return err
			}
		}
		if override.TaskID != "" {
			if err := ensureTaskProject(ctx, tx, override.ProjectID, override.DeliveryRunID, override.TaskID); err != nil {
				return err
			}
		}
		existing, ok, err := loadOverrideByID(ctx, tx, override.RequirementOverrideID)
		if err != nil {
			return err
		}
		if ok {
			if existing.ValueJSON == override.ValueJSON && existing.Field == override.Field && existing.Status == override.Status {
				out = existing
				return nil
			}
			return typed(ErrDuplicateRecordCode, "requirement override %s already exists with different payload", override.RequirementOverrideID)
		}
		if err := insertOverride(ctx, tx, override, string(payload)); err != nil {
			return err
		}
		out = override
		return nil
	})
	return out, err
}

func LoadActiveOverrides(ctx context.Context, store storage.Store, projectID, deliveryRunID, taskKey, planFingerprint string) ([]RequirementOverride, error) {
	if store == nil {
		return nil, typed(ErrInvalidRecordCode, "store is required")
	}
	var out []RequirementOverride
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		rows, err := tx.Query(ctx, `SELECT payload_json FROM task_requirement_overrides
			WHERE project_id = ?
				AND status = 'active'
				AND (delivery_run_id IS NULL OR delivery_run_id = '' OR delivery_run_id = ?)
				AND (task_key = '' OR task_key = ?)
				AND (plan_fingerprint = '' OR plan_fingerprint = ?)
			ORDER BY created_at, requirement_override_id`, projectID, deliveryRunID, taskKey, planFingerprint)
		if err != nil {
			return fmt.Errorf("list requirement overrides: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var payload string
			if err := rows.Scan(&payload); err != nil {
				return fmt.Errorf("scan requirement override: %w", err)
			}
			var override RequirementOverride
			if err := json.Unmarshal([]byte(payload), &override); err != nil {
				return typed(ErrInvalidRecordCode, "stored requirement override is invalid: %v", err)
			}
			out = append(out, override)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("list requirement override rows: %w", err)
		}
		return nil
	})
	return out, err
}

func normalizeRequirement(req TaskRequirement, now time.Time) TaskRequirement {
	if req.SchemaVersion == "" {
		req.SchemaVersion = SchemaTaskRequirement
	}
	if req.RecordVersion == 0 {
		req.RecordVersion = 1
	}
	if req.PolicyVersion == "" {
		req.PolicyVersion = PolicyVersion
	}
	if req.Confidence == "" {
		req.Confidence = providerinventory.ConfidenceExact
	}
	if req.Classification == "" {
		req.Classification = "planner-metadata"
	}
	if req.Source.SourceKind == "" {
		req.Source = SourceDescriptor{SourceKind: "deterministic-classifier", SourceReference: "0804-task-requirement-rules"}
	}
	if req.TaskRequirementID == "" && req.ProjectID != "" && req.DeliveryRunID != "" && req.TaskID != "" && req.PlanFingerprint != "" {
		req.TaskRequirementID = TaskRequirementID(req.ProjectID, req.DeliveryRunID, req.TaskID, req.PlanFingerprint)
	}
	if req.GapReasons == nil {
		req.GapReasons = []string{}
	}
	if req.CreatedAt == "" {
		req.CreatedAt = canonicalTimestamp(now)
	}
	if req.UpdatedAt == "" {
		req.UpdatedAt = req.CreatedAt
	}
	return req
}

func normalizeOverride(override RequirementOverride, now time.Time) RequirementOverride {
	if override.SchemaVersion == "" {
		override.SchemaVersion = SchemaTaskRequirementOverride
	}
	if override.RecordVersion == 0 {
		override.RecordVersion = 1
	}
	if override.ScopeKey == "" {
		override.ScopeKey = firstNonEmpty(override.TaskID, override.TaskKey, override.PlanFingerprint, "project")
	}
	if override.RequirementOverrideID == "" {
		override.RequirementOverrideID = RequirementOverrideID(override.ProjectID, override.DeliveryRunID, override.ScopeKey, override.Field, override.ValueJSON)
	}
	if override.Status == "" {
		override.Status = "active"
	}
	if override.ValidationStatus == "" {
		override.ValidationStatus = "pending"
	}
	if override.PolicyVersion == "" {
		override.PolicyVersion = PolicyVersion
	}
	if override.Classification == "" {
		override.Classification = "planner-metadata"
	}
	if override.SideEffectClass == "" {
		override.SideEffectClass = SideEffectLocalRead
	}
	if override.Confidence == "" {
		override.Confidence = providerinventory.ConfidenceExact
	}
	if override.Source.SourceKind == "" {
		override.Source = SourceDescriptor{SourceKind: "user-correction", SourceReference: "operator-override"}
	}
	if override.GapReasons == nil {
		override.GapReasons = []string{}
	}
	if override.CreatedAt == "" {
		override.CreatedAt = canonicalTimestamp(now)
	}
	if override.UpdatedAt == "" {
		override.UpdatedAt = override.CreatedAt
	}
	return override
}

func insertRequirement(ctx context.Context, tx storage.Tx, req TaskRequirement, payload string) error {
	createdBy, err := marshalJSON(req.CreatedBy)
	if err != nil {
		return err
	}
	updatedBy, err := marshalJSON(req.UpdatedBy)
	if err != nil {
		return err
	}
	host, err := marshalJSON(req.Host)
	if err != nil {
		return err
	}
	rules, err := marshalJSON(req.ClassificationRules)
	if err != nil {
		return err
	}
	caps, err := marshalJSON(req.RequiredCapabilities)
	if err != nil {
		return err
	}
	verification, err := marshalJSON(req.VerificationRequirements)
	if err != nil {
		return err
	}
	heuristics, err := marshalJSON(req.Heuristics)
	if err != nil {
		return err
	}
	source, err := marshalJSON(req.Source)
	if err != nil {
		return err
	}
	gaps, err := marshalJSON(req.GapReasons)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO task_requirements(
		task_requirement_id, schema_version, record_version, task_requirement_fingerprint, project_id, delivery_run_id, task_id, task_key,
		role_key, risk_tier, permission_required, side_effect_class, required_output, scope_json, data_classification, network_required,
		nested_allowed, cancellation_required, quality_floor, classification_rules_json, required_capabilities_json, verification_requirements_json,
		heuristics_json, provenance_summary, policy_version, plan_fingerprint, created_at, updated_at, created_by_json, updated_by_json,
		host_json, classification, confidence, source_json, heuristic, heuristic_reason, terminal_error_code, gap_reasons_json, payload_json
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.TaskRequirementID, req.SchemaVersion, req.RecordVersion, req.TaskRequirementFingerprint, req.ProjectID, req.DeliveryRunID, req.TaskID, req.TaskKey,
		req.RoleKey, string(req.RiskTier), string(req.PermissionRequired), string(req.SideEffectClass), string(req.RequiredOutput), req.ScopeJSON, req.DataClassification, string(req.NetworkRequired),
		boolInt(req.NestedAllowed), boolInt(req.CancellationRequired), string(req.QualityFloor), rules, caps, verification,
		heuristics, req.ProvenanceSummary, req.PolicyVersion, req.PlanFingerprint, req.CreatedAt, req.UpdatedAt, createdBy, updatedBy,
		host, req.Classification, string(req.Confidence), source, boolInt(req.Heuristic), req.HeuristicReason, req.TerminalErrorCode, gaps, payload)
	if err != nil {
		return fmt.Errorf("persist task requirement: %w", err)
	}
	return nil
}

func insertOverride(ctx context.Context, tx storage.Tx, override RequirementOverride, payload string) error {
	createdBy, err := marshalJSON(override.CreatedBy)
	if err != nil {
		return err
	}
	updatedBy, err := marshalJSON(override.UpdatedBy)
	if err != nil {
		return err
	}
	host, err := marshalJSON(override.Host)
	if err != nil {
		return err
	}
	source, err := marshalJSON(override.Source)
	if err != nil {
		return err
	}
	gaps, err := marshalJSON(override.GapReasons)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO task_requirement_overrides(
		requirement_override_id, schema_version, record_version, project_id, delivery_run_id, task_id, task_key, plan_fingerprint,
		scope_key, field, value_json, justification, status, validation_status, validation_error_code, input_requirement_fingerprint,
		output_requirement_fingerprint, created_at, updated_at, created_by_json, updated_by_json, host_json, policy_version,
		classification, side_effect_class, confidence, source_json, terminal_error_code, gap_reasons_json, payload_json
	) VALUES (?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		override.RequirementOverrideID, override.SchemaVersion, override.RecordVersion, override.ProjectID, override.DeliveryRunID, override.TaskID, override.TaskKey, override.PlanFingerprint,
		override.ScopeKey, override.Field, override.ValueJSON, override.Justification, override.Status, override.ValidationStatus, override.ValidationErrorCode, override.InputRequirementFingerprint,
		override.OutputRequirementFingerprint, override.CreatedAt, override.UpdatedAt, createdBy, updatedBy, host, override.PolicyVersion,
		override.Classification, string(override.SideEffectClass), string(override.Confidence), source, override.TerminalErrorCode, gaps, payload)
	if err != nil {
		return fmt.Errorf("persist requirement override: %w", err)
	}
	return nil
}

func loadRequirementByID(ctx context.Context, tx storage.Tx, id string) (TaskRequirement, bool, error) {
	var payload string
	err := tx.QueryRow(ctx, `SELECT payload_json FROM task_requirements WHERE task_requirement_id = ?`, strings.TrimSpace(id)).Scan(&payload)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TaskRequirement{}, false, nil
		}
		return TaskRequirement{}, false, fmt.Errorf("load task requirement: %w", err)
	}
	var req TaskRequirement
	if err := json.Unmarshal([]byte(payload), &req); err != nil {
		return TaskRequirement{}, false, typed(ErrInvalidRecordCode, "stored task requirement is invalid: %v", err)
	}
	if err := Validate(req); err != nil {
		return TaskRequirement{}, false, err
	}
	return req, true, nil
}

func loadOverrideByID(ctx context.Context, tx storage.Tx, id string) (RequirementOverride, bool, error) {
	var payload string
	err := tx.QueryRow(ctx, `SELECT payload_json FROM task_requirement_overrides WHERE requirement_override_id = ?`, strings.TrimSpace(id)).Scan(&payload)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RequirementOverride{}, false, nil
		}
		return RequirementOverride{}, false, fmt.Errorf("load requirement override: %w", err)
	}
	var override RequirementOverride
	if err := json.Unmarshal([]byte(payload), &override); err != nil {
		return RequirementOverride{}, false, typed(ErrInvalidRecordCode, "stored requirement override is invalid: %v", err)
	}
	return override, true, nil
}

func ensureRunProject(ctx context.Context, tx storage.Tx, projectID, deliveryRunID string) error {
	var found string
	err := tx.QueryRow(ctx, `SELECT project_id FROM delivery_runs WHERE delivery_run_id = ?`, deliveryRunID).Scan(&found)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return typed(ErrMissingReferenceCode, "delivery run %s is missing", deliveryRunID)
		}
		return fmt.Errorf("inspect delivery run: %w", err)
	}
	if found != projectID {
		return typed(ErrCrossProjectReferenceCode, "delivery run %s belongs to %s", deliveryRunID, found)
	}
	return nil
}

func ensureTaskProject(ctx context.Context, tx storage.Tx, projectID, deliveryRunID, taskID string) error {
	var foundProject, foundRun string
	err := tx.QueryRow(ctx, `SELECT project_id, delivery_run_id FROM delivery_tasks WHERE task_id = ?`, taskID).Scan(&foundProject, &foundRun)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return typed(ErrMissingReferenceCode, "task %s is missing", taskID)
		}
		return fmt.Errorf("inspect task: %w", err)
	}
	if foundProject != projectID || (deliveryRunID != "" && foundRun != deliveryRunID) {
		return typed(ErrCrossProjectReferenceCode, "task %s belongs to %s/%s", taskID, foundProject, foundRun)
	}
	return nil
}

func optionNow(opts PersistOptions) time.Time {
	if opts.Now.IsZero() {
		return time.Now().UTC()
	}
	return opts.Now.UTC()
}

func marshalJSON(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", typed(ErrInvalidRecordCode, "marshal JSON: %v", err)
	}
	return string(data), nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}
