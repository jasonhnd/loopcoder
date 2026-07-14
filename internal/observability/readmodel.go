// Package observability builds provider-neutral orchestration read models from
// durable storage records. It is a projection only: all authority remains in
// the source tables.
package observability

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

const (
	SummarySchemaVersion = "loopcoder.orchestration_summary.v1"
	DetailSchemaVersion  = "loopcoder.orchestration_detail.v1"

	defaultLimit = 100
	maxLimit     = 1000
	sourceIDCap  = 256
)

type ErrorCode string

const (
	ErrInvalidQueryCode     ErrorCode = "invalid_query"
	ErrNotFoundCode         ErrorCode = "not_found"
	ErrPartialMigrationCode ErrorCode = "partial_migration"
	ErrStorageReadCode      ErrorCode = "storage_read"
	ErrCorruptCursorCode    ErrorCode = "corrupt_cursor"
)

type QueryError struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

func (e *QueryError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Message
}

type Options struct {
	ProjectID     string
	DeliveryRunID string
	Limit         int
	Cursor        string
	Sections      []string
	Now           func() time.Time
}

type Snapshot struct {
	SchemaVersion    int    `json:"schema_version"`
	Consistency      string `json:"consistency"`
	Canonicalization string `json:"canonicalization"`
	ReadOnly         bool   `json:"read_only"`
	ObservedAt       string `json:"observed_at"`
}

type Summary struct {
	SchemaVersion string           `json:"schema_version"`
	ProjectID     string           `json:"project_id"`
	DeliveryRunID string           `json:"delivery_run_id"`
	Snapshot      Snapshot         `json:"snapshot"`
	Counts        []SummaryCount   `json:"counts"`
	Evidence      []Evidence       `json:"evidence"`
	Redaction     RedactionSummary `json:"redaction"`
}

type SummaryCount struct {
	Section            string      `json:"section"`
	Kind               string      `json:"kind"`
	Count              int         `json:"count"`
	SourceRecordIDs    []string    `json:"source_record_ids"`
	SourceIDsTruncated bool        `json:"source_ids_truncated"`
	Confidence         string      `json:"confidence"`
	Freshness          string      `json:"freshness"`
	SourceRefs         []SourceRef `json:"source_refs"`
	Evidence           []Evidence  `json:"evidence"`
}

type Detail struct {
	SchemaVersion string           `json:"schema_version"`
	ProjectID     string           `json:"project_id"`
	DeliveryRunID string           `json:"delivery_run_id"`
	Snapshot      Snapshot         `json:"snapshot"`
	Page          Page             `json:"page"`
	Entries       []Entry          `json:"entries"`
	Summary       []SummaryCount   `json:"summary"`
	Evidence      []Evidence       `json:"evidence"`
	Redaction     RedactionSummary `json:"redaction"`
}

type Page struct {
	Limit      int    `json:"limit"`
	Cursor     string `json:"cursor,omitempty"`
	NextCursor string `json:"next_cursor,omitempty"`
	Truncated  bool   `json:"truncated"`
	Returned   int    `json:"returned"`
	TotalKnown int    `json:"total_known"`
}

type Entry struct {
	Section        string            `json:"section"`
	Kind           string            `json:"kind"`
	RecordID       string            `json:"record_id"`
	SchemaVersion  string            `json:"schema_version"`
	RecordVersion  int               `json:"record_version"`
	ProjectID      string            `json:"project_id,omitempty"`
	DeliveryRunID  string            `json:"delivery_run_id,omitempty"`
	CreatedAt      string            `json:"created_at,omitempty"`
	UpdatedAt      string            `json:"updated_at,omitempty"`
	Status         string            `json:"status,omitempty"`
	Correlation    map[string]string `json:"correlation"`
	Confidence     string            `json:"confidence"`
	Freshness      string            `json:"freshness"`
	Classification string            `json:"classification,omitempty"`
	PayloadDigest  string            `json:"payload_digest,omitempty"`
	Redaction      RedactionSummary  `json:"redaction"`
	SourceRefs     []SourceRef       `json:"source_refs"`
	Evidence       []Evidence        `json:"evidence"`
}

type SourceRef struct {
	Table         string `json:"table"`
	RecordID      string `json:"record_id"`
	ProjectID     string `json:"project_id,omitempty"`
	DeliveryRunID string `json:"delivery_run_id,omitempty"`
	Field         string `json:"field,omitempty"`
	Provenance    string `json:"provenance"`
}

type Evidence struct {
	Type       string      `json:"type"`
	Code       string      `json:"code"`
	Severity   string      `json:"severity"`
	Section    string      `json:"section,omitempty"`
	Kind       string      `json:"kind,omitempty"`
	SourceRefs []SourceRef `json:"source_refs,omitempty"`
	Message    string      `json:"message,omitempty"`
}

type RedactionSummary struct {
	Applied        bool     `json:"applied"`
	Mode           string   `json:"mode"`
	Fields         []string `json:"fields"`
	PayloadExposed bool     `json:"payload_exposed"`
}

type rowSpec struct {
	section string
	kind    string
	table   string
	idAlias string
	from    string
	args    func(scope) []any
	selects string
}

type scope struct {
	projectID string
	runID     string
	now       time.Time
}

type cursor struct {
	SchemaVersion string `json:"schema_version"`
	Offset        int    `json:"offset"`
}

type rawRow struct {
	recordID       string
	projectID      string
	deliveryRunID  string
	schemaVersion  string
	recordVersion  int
	createdAt      string
	updatedAt      string
	correlation    map[string]string
	confidence     string
	freshness      string
	classification string
	status         string
	payloadJSON    string
	jsonA          string
	jsonB          string
	semanticKey    string
}

func LoadSummary(ctx context.Context, store storage.Store, opts Options) (Summary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	sc, err := validateOptions(store, opts)
	if err != nil {
		return Summary{}, err
	}
	allowed := sectionSet(opts.Sections)
	specs := filteredSpecs(allowed)
	var out Summary
	err = store.WithTx(ctx, func(tx storage.Tx) error {
		snapshot, err := readSnapshot(ctx, tx, sc)
		if err != nil {
			return err
		}
		if err := assertScopedRun(ctx, tx, sc); err != nil {
			return err
		}
		counts, evidence, err := loadCounts(ctx, tx, sc, specs)
		if err != nil {
			return err
		}
		scopedEvidence, err := loadScopedIntegrityEvidence(ctx, tx, sc)
		if err != nil {
			return err
		}
		evidence = append(evidence, scopedEvidence...)
		out = Summary{
			SchemaVersion: SummarySchemaVersion,
			ProjectID:     sc.projectID,
			DeliveryRunID: sc.runID,
			Snapshot:      snapshot,
			Counts:        counts,
			Evidence:      evidence,
			Redaction:     projectionRedaction(),
		}
		return nil
	})
	if err != nil {
		return Summary{}, classifyReadError(err)
	}
	return out, nil
}

func LoadDetail(ctx context.Context, store storage.Store, opts Options) (Detail, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	sc, err := validateOptions(store, opts)
	if err != nil {
		return Detail{}, err
	}
	limit := boundedLimit(opts.Limit)
	cur, err := decodeCursor(opts.Cursor)
	if err != nil {
		return Detail{}, err
	}
	allowed := sectionSet(opts.Sections)
	specs := filteredSpecs(allowed)
	var out Detail
	err = store.WithTx(ctx, func(tx storage.Tx) error {
		snapshot, err := readSnapshot(ctx, tx, sc)
		if err != nil {
			return err
		}
		if err := assertScopedRun(ctx, tx, sc); err != nil {
			return err
		}
		counts, evidence, err := loadCounts(ctx, tx, sc, specs)
		if err != nil {
			return err
		}
		scopedEvidence, err := loadScopedIntegrityEvidence(ctx, tx, sc)
		if err != nil {
			return err
		}
		evidence = append(evidence, scopedEvidence...)
		entries, total, nextOffset, truncated, err := loadEntries(ctx, tx, sc, specs, cur.Offset, limit)
		if err != nil {
			return err
		}
		evidence = append(evidence, validatePage(entries)...)
		next := ""
		if truncated {
			next, err = encodeCursor(cursor{SchemaVersion: DetailSchemaVersion, Offset: nextOffset})
			if err != nil {
				return err
			}
			evidence = append(evidence, Evidence{
				Type:     "partial",
				Code:     "continuation_required",
				Severity: "info",
				Message:  "result set exceeded page limit; use next_cursor for continuation",
			})
		}
		out = Detail{
			SchemaVersion: DetailSchemaVersion,
			ProjectID:     sc.projectID,
			DeliveryRunID: sc.runID,
			Snapshot:      snapshot,
			Page: Page{
				Limit:      limit,
				Cursor:     opts.Cursor,
				NextCursor: next,
				Truncated:  truncated,
				Returned:   len(entries),
				TotalKnown: total,
			},
			Entries:   entries,
			Summary:   counts,
			Evidence:  evidence,
			Redaction: projectionRedaction(),
		}
		return nil
	})
	if err != nil {
		return Detail{}, classifyReadError(err)
	}
	return out, nil
}

func CanonicalJSON(v any) ([]byte, error) {
	return delivery.CanonicalJSON(v)
}

func SummaryJSON(summary Summary) ([]byte, error) {
	return CanonicalJSON(summary)
}

func DetailJSON(detail Detail) ([]byte, error) {
	return CanonicalJSON(detail)
}

func validateOptions(store storage.Store, opts Options) (scope, error) {
	if store == nil {
		return scope{}, &QueryError{Code: ErrInvalidQueryCode, Message: "storage store is required"}
	}
	projectID := strings.TrimSpace(opts.ProjectID)
	runID := strings.TrimSpace(opts.DeliveryRunID)
	if projectID == "" {
		return scope{}, &QueryError{Code: ErrInvalidQueryCode, Message: "project_id is required"}
	}
	if runID == "" {
		return scope{}, &QueryError{Code: ErrInvalidQueryCode, Message: "delivery_run_id is required"}
	}
	now := time.Now().UTC()
	if opts.Now != nil {
		now = opts.Now().UTC()
	}
	return scope{projectID: projectID, runID: runID, now: now}, nil
}

func boundedLimit(value int) int {
	if value <= 0 {
		return defaultLimit
	}
	if value > maxLimit {
		return maxLimit
	}
	return value
}

func readSnapshot(ctx context.Context, tx storage.Tx, sc scope) (Snapshot, error) {
	var version int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM migrations`).Scan(&version); err != nil {
		return Snapshot{}, fmt.Errorf("read schema snapshot: %w", err)
	}
	return Snapshot{
		SchemaVersion:    version,
		Consistency:      "single_sqlite_transaction",
		Canonicalization: delivery.CanonicalJSONVersion,
		ReadOnly:         true,
		ObservedAt:       delivery.CanonicalTimestamp(sc.now),
	}, nil
}

func assertScopedRun(ctx context.Context, tx storage.Tx, sc scope) error {
	var count int
	if err := tx.QueryRow(ctx, `SELECT COUNT(1) FROM delivery_runs WHERE project_id = ? AND delivery_run_id = ?`, sc.projectID, sc.runID).Scan(&count); err != nil {
		return fmt.Errorf("read scoped delivery run: %w", err)
	}
	if count == 0 {
		return &QueryError{Code: ErrNotFoundCode, Message: "delivery run not found in requested project scope"}
	}
	return nil
}

func loadCounts(ctx context.Context, tx storage.Tx, sc scope, specs []rowSpec) ([]SummaryCount, []Evidence, error) {
	counts := make([]SummaryCount, 0, len(specs))
	var evidence []Evidence
	for _, spec := range specs {
		count, err := countSpec(ctx, tx, sc, spec)
		if err != nil {
			return nil, nil, err
		}
		ids, truncated, err := sourceIDs(ctx, tx, sc, spec)
		if err != nil {
			return nil, nil, err
		}
		refs := make([]SourceRef, 0, len(ids))
		for _, id := range ids {
			refs = append(refs, SourceRef{Table: spec.table, RecordID: id, ProjectID: sc.projectID, DeliveryRunID: sc.runID, Provenance: "durable_sql"})
		}
		var countEvidence []Evidence
		if truncated {
			ev := Evidence{
				Type:     "partial",
				Code:     "source_ids_truncated",
				Severity: "info",
				Section:  spec.section,
				Kind:     spec.kind,
				Message:  "source id list exceeded bounded summary cap",
			}
			countEvidence = append(countEvidence, ev)
			evidence = append(evidence, ev)
		}
		counts = append(counts, SummaryCount{
			Section:            spec.section,
			Kind:               spec.kind,
			Count:              count,
			SourceRecordIDs:    ids,
			SourceIDsTruncated: truncated,
			Confidence:         confidenceForCount(count),
			Freshness:          "storage_snapshot",
			SourceRefs:         refs,
			Evidence:           countEvidence,
		})
	}
	sort.SliceStable(counts, func(i, j int) bool {
		if counts[i].Section != counts[j].Section {
			return counts[i].Section < counts[j].Section
		}
		return counts[i].Kind < counts[j].Kind
	})
	return counts, evidence, nil
}

func countSpec(ctx context.Context, tx storage.Tx, sc scope, spec rowSpec) (int, error) {
	var count int
	if err := tx.QueryRow(ctx, `SELECT COUNT(1) `+spec.from, spec.args(sc)...).Scan(&count); err != nil {
		return 0, fmt.Errorf("count %s/%s: %w", spec.section, spec.kind, err)
	}
	return count, nil
}

func sourceIDs(ctx context.Context, tx storage.Tx, sc scope, spec rowSpec) ([]string, bool, error) {
	query := `SELECT ` + spec.idAlias + ` ` + spec.from + ` ORDER BY ` + spec.idAlias + ` LIMIT ?`
	args := append(spec.args(sc), sourceIDCap+1)
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("source ids %s/%s: %w", spec.section, spec.kind, err)
	}
	defer rows.Close()
	var ids []string
	truncated := false
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, false, err
		}
		if len(ids) < sourceIDCap {
			ids = append(ids, id)
		} else {
			truncated = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	return ids, truncated, nil
}

func loadEntries(ctx context.Context, tx storage.Tx, sc scope, specs []rowSpec, offset, limit int) ([]Entry, int, int, bool, error) {
	if offset < 0 {
		return nil, 0, 0, false, &QueryError{Code: ErrCorruptCursorCode, Message: "cursor offset is negative"}
	}
	total := 0
	remainingOffset := offset
	remainingLimit := limit
	var entries []Entry
	for _, spec := range specs {
		count, err := countSpec(ctx, tx, sc, spec)
		if err != nil {
			return nil, 0, 0, false, err
		}
		total += count
		if remainingOffset >= count {
			remainingOffset -= count
			continue
		}
		if remainingLimit <= 0 {
			continue
		}
		rows, err := loadSpecRows(ctx, tx, sc, spec, remainingLimit, remainingOffset)
		if err != nil {
			return nil, 0, 0, false, err
		}
		for _, row := range rows {
			entries = append(entries, entryFromRaw(spec, sc, row))
			remainingLimit--
			if remainingLimit == 0 {
				break
			}
		}
		remainingOffset = 0
	}
	nextOffset := offset + len(entries)
	truncated := nextOffset < total
	return entries, total, nextOffset, truncated, nil
}

func loadScopedIntegrityEvidence(ctx context.Context, tx storage.Tx, sc scope) ([]Evidence, error) {
	checks := []struct {
		section string
		kind    string
		table   string
		field   string
		query   string
	}{
		{
			section: "plans_tasks",
			kind:    "delivery_task",
			table:   "delivery_tasks",
			field:   "active_attempt_id",
			query: `SELECT task_id FROM delivery_tasks dt
				WHERE dt.project_id = ? AND dt.delivery_run_id = ? AND COALESCE(dt.active_attempt_id, '') <> ''
					AND NOT EXISTS (
						SELECT 1 FROM delivery_attempts da
						WHERE da.project_id = dt.project_id
							AND da.delivery_run_id = dt.delivery_run_id
							AND da.attempt_id = dt.active_attempt_id
					)
				ORDER BY task_id LIMIT ?`,
		},
		{
			section: "routing",
			kind:    "fallback_decision",
			table:   "fallback_decisions",
			field:   "routing_decision_id",
			query: `SELECT fallback_decision_id FROM fallback_decisions fd
				WHERE fd.project_id = ? AND fd.delivery_run_id = ?
					AND NOT EXISTS (
						SELECT 1 FROM routing_decisions rd
						WHERE rd.project_id = fd.project_id
							AND rd.delivery_run_id = fd.delivery_run_id
							AND rd.routing_decision_id = fd.routing_decision_id
					)
				ORDER BY fallback_decision_id LIMIT ?`,
		},
		{
			section: "routing",
			kind:    "verification_decision",
			table:   "verification_decisions",
			field:   "worker_routing_decision_id",
			query: `SELECT verification_decision_id FROM verification_decisions vd
				WHERE vd.project_id = ? AND vd.delivery_run_id = ?
					AND NOT EXISTS (
						SELECT 1 FROM routing_decisions rd
						WHERE rd.project_id = vd.project_id
							AND rd.delivery_run_id = vd.delivery_run_id
							AND rd.routing_decision_id = vd.worker_routing_decision_id
					)
				ORDER BY verification_decision_id LIMIT ?`,
		},
		{
			section: "agents",
			kind:    "agent_registration",
			table:   "agent_registrations",
			field:   "scope_grant_id",
			query: `SELECT id FROM agent_registrations ar
				WHERE ar.project_id = ? AND ar.delivery_run_id = ?
					AND NOT EXISTS (
						SELECT 1 FROM agent_scope_grants sg
						WHERE sg.project_id = ar.project_id
							AND sg.delivery_run_id = ar.delivery_run_id
							AND sg.id = ar.scope_grant_id
					)
				ORDER BY id LIMIT ?`,
		},
		{
			section: "agents",
			kind:    "agent_registration",
			table:   "agent_registrations",
			field:   "child_run_id",
			query: `SELECT id FROM agent_registrations ar
				WHERE ar.project_id = ? AND ar.delivery_run_id = ?
					AND NOT EXISTS (
						SELECT 1 FROM runs r
						WHERE r.project_id = ar.project_id
							AND r.id = ar.child_run_id
					)
				ORDER BY id LIMIT ?`,
		},
		{
			section: "handoffs",
			kind:    "handoff_transaction",
			table:   "handoff_transactions",
			field:   "child_run_id",
			query: `SELECT handoff_id FROM handoff_transactions ht
				WHERE ht.project_id = ? AND ht.delivery_run_id = ?
					AND NOT EXISTS (
						SELECT 1 FROM runs r
						WHERE r.project_id = ht.project_id
							AND r.id = ht.child_run_id
					)
				ORDER BY handoff_id LIMIT ?`,
		},
	}
	var evidence []Evidence
	for _, check := range checks {
		rows, err := tx.Query(ctx, check.query, sc.projectID, sc.runID, sourceIDCap+1)
		if err != nil {
			return nil, fmt.Errorf("integrity check %s/%s: %w", check.section, check.kind, err)
		}
		count := 0
		truncated := false
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				if closeErr := rows.Close(); closeErr != nil {
					return nil, fmt.Errorf("close integrity rows after scan error: %w", closeErr)
				}
				return nil, err
			}
			count++
			if count > sourceIDCap {
				truncated = true
				continue
			}
			evidence = append(evidence, Evidence{
				Type:     "unknown",
				Code:     "dangling_or_cross_scope_ref",
				Severity: "error",
				Section:  check.section,
				Kind:     check.kind,
				SourceRefs: []SourceRef{{
					Table:         check.table,
					RecordID:      id,
					ProjectID:     sc.projectID,
					DeliveryRunID: sc.runID,
					Field:         check.field,
					Provenance:    "durable_sql",
				}},
				Message: "source record references a missing or out-of-scope target",
			})
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if truncated {
			evidence = append(evidence, Evidence{
				Type:     "partial",
				Code:     "integrity_evidence_truncated",
				Severity: "warning",
				Section:  check.section,
				Kind:     check.kind,
				Message:  "integrity evidence exceeded bounded source-id cap",
			})
		}
	}
	return evidence, nil
}

func loadSpecRows(ctx context.Context, tx storage.Tx, sc scope, spec rowSpec, limit, offset int) ([]rawRow, error) {
	query := `SELECT ` + spec.selects + ` ` + spec.from + ` ORDER BY 1 LIMIT ? OFFSET ?`
	args := append(spec.args(sc), limit, offset)
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("load %s/%s: %w", spec.section, spec.kind, err)
	}
	defer rows.Close()
	var out []rawRow
	for rows.Next() {
		var r rawRow
		var correlation string
		if err := rows.Scan(
			&r.recordID,
			&r.projectID,
			&r.deliveryRunID,
			&r.schemaVersion,
			&r.recordVersion,
			&r.createdAt,
			&r.updatedAt,
			&correlation,
			&r.confidence,
			&r.freshness,
			&r.classification,
			&r.status,
			&r.payloadJSON,
			&r.jsonA,
			&r.jsonB,
			&r.semanticKey,
		); err != nil {
			return nil, err
		}
		r.correlation = parseCorrelation(correlation)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func entryFromRaw(spec rowSpec, sc scope, row rawRow) Entry {
	if row.projectID == "" {
		row.projectID = sc.projectID
	}
	if row.deliveryRunID == "" {
		row.deliveryRunID = sc.runID
	}
	if row.correlation == nil {
		row.correlation = map[string]string{}
	}
	row.correlation["project_id"] = sc.projectID
	row.correlation["delivery_run_id"] = sc.runID
	source := SourceRef{Table: spec.table, RecordID: row.recordID, ProjectID: sc.projectID, DeliveryRunID: sc.runID, Provenance: "durable_sql"}
	entry := Entry{
		Section:        spec.section,
		Kind:           spec.kind,
		RecordID:       row.recordID,
		SchemaVersion:  firstNonEmpty(row.schemaVersion, "unknown"),
		RecordVersion:  row.recordVersion,
		ProjectID:      row.projectID,
		DeliveryRunID:  row.deliveryRunID,
		CreatedAt:      row.createdAt,
		UpdatedAt:      row.updatedAt,
		Status:         row.status,
		Correlation:    row.correlation,
		Confidence:     firstNonEmpty(row.confidence, "unknown"),
		Freshness:      firstNonEmpty(row.freshness, "unknown"),
		Classification: row.classification,
		Redaction:      projectionRedaction(),
		SourceRefs:     []SourceRef{source},
	}
	for field, value := range map[string]string{"payload_json": row.payloadJSON, "json_a": row.jsonA, "json_b": row.jsonB} {
		if strings.TrimSpace(value) == "" {
			continue
		}
		digest, ev := digestJSONField(value, field, source)
		if field == "payload_json" && digest != "" {
			entry.PayloadDigest = digest
		}
		if ev != nil {
			entry.Evidence = append(entry.Evidence, *ev)
		}
	}
	if row.recordVersion > 1 {
		entry.Evidence = append(entry.Evidence, Evidence{
			Type:       "unknown",
			Code:       "unsupported_record_version",
			Severity:   "warning",
			Section:    spec.section,
			Kind:       spec.kind,
			SourceRefs: []SourceRef{source},
			Message:    "record version is newer than this read model understands",
		})
	}
	if strings.TrimSpace(row.confidence) == "" {
		entry.Evidence = append(entry.Evidence, Evidence{
			Type:       "unknown",
			Code:       "missing_confidence",
			Severity:   "info",
			Section:    spec.section,
			Kind:       spec.kind,
			SourceRefs: []SourceRef{source},
		})
	}
	if strings.TrimSpace(row.freshness) == "" {
		entry.Evidence = append(entry.Evidence, Evidence{
			Type:       "unknown",
			Code:       "missing_freshness",
			Severity:   "info",
			Section:    spec.section,
			Kind:       spec.kind,
			SourceRefs: []SourceRef{source},
		})
	}
	return entry
}

func digestJSONField(value, field string, source SourceRef) (string, *Evidence) {
	canonical, err := delivery.CanonicalJSONBytes([]byte(value))
	if err != nil {
		src := source
		src.Field = field
		return "", &Evidence{
			Type:       "corrupt",
			Code:       "corrupt_json",
			Severity:   "error",
			SourceRefs: []SourceRef{src},
			Message:    "durable JSON field is not canonical-readable",
		}
	}
	return delivery.SHA256Digest(canonical), nil
}

func validatePage(entries []Entry) []Evidence {
	var evidence []Evidence
	seen := map[string]SourceRef{}
	for _, entry := range entries {
		key := entry.Section + "\x00" + entry.Kind + "\x00" + semanticIdentity(entry)
		if key == entry.Section+"\x00"+entry.Kind+"\x00" {
			continue
		}
		if prior, ok := seen[key]; ok {
			evidence = append(evidence, Evidence{
				Type:       "partial",
				Code:       "duplicate_semantic_identity",
				Severity:   "warning",
				Section:    entry.Section,
				Kind:       entry.Kind,
				SourceRefs: []SourceRef{prior, entry.SourceRefs[0]},
				Message:    "two records share the same semantic identity within the returned page",
			})
			continue
		}
		if len(entry.SourceRefs) > 0 {
			seen[key] = entry.SourceRefs[0]
		}
	}
	evidence = append(evidence, agentCycleEvidence(entries)...)
	return evidence
}

func semanticIdentity(entry Entry) string {
	for _, key := range []string{"child_run_id", "task_id", "attempt_id", "routing_decision_id", "correlation_id"} {
		if value := strings.TrimSpace(entry.Correlation[key]); value != "" {
			return key + ":" + value
		}
	}
	return ""
}

func agentCycleEvidence(entries []Entry) []Evidence {
	parents := map[string]string{}
	refs := map[string]SourceRef{}
	for _, entry := range entries {
		if entry.Section != "agents" || entry.Kind != "agent_registration" {
			continue
		}
		child := entry.Correlation["child_agent_id"]
		parent := entry.Correlation["parent_agent_id"]
		if child == "" || parent == "" {
			continue
		}
		parents[child] = parent
		if len(entry.SourceRefs) > 0 {
			refs[child] = entry.SourceRefs[0]
		}
	}
	var out []Evidence
	for child := range parents {
		seen := map[string]bool{}
		for cur := child; cur != ""; cur = parents[cur] {
			if seen[cur] {
				out = append(out, Evidence{
					Type:       "corrupt",
					Code:       "agent_tree_cycle",
					Severity:   "error",
					Section:    "agents",
					Kind:       "agent_registration",
					SourceRefs: []SourceRef{refs[child]},
					Message:    "agent parent chain cycles within the returned page",
				})
				break
			}
			seen[cur] = true
		}
	}
	return out
}

func projectionRedaction() RedactionSummary {
	return RedactionSummary{
		Applied:        true,
		Mode:           "payload_hash_only",
		Fields:         []string{"payload_json", "provider_output", "diagnostics", "paths", "secret_reference"},
		PayloadExposed: false,
	}
}

func confidenceForCount(count int) string {
	if count == 0 {
		return "unknown"
	}
	return "observed"
}

func parseCorrelation(value string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(value, "\x1f") {
		if part == "" {
			continue
		}
		key, val, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if unsafeCorrelationKey(key) {
			continue
		}
		if key != "" && val != "" {
			out[key] = val
		}
	}
	return out
}

func unsafeCorrelationKey(key string) bool {
	switch key {
	case "active_attempt_id",
		"budget_policy_id",
		"budget_reservation_id",
		"child_run_id",
		"from_task_id",
		"parent_run_id",
		"routing_decision_id",
		"scope_grant_id",
		"to_task_id",
		"verifier_routing_decision_id",
		"worker_routing_decision_id":
		return true
	default:
		return false
	}
}

func encodeCursor(cur cursor) (string, error) {
	data, err := json.Marshal(cur)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func decodeCursor(value string) (cursor, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return cursor{SchemaVersion: DetailSchemaVersion}, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return cursor{}, &QueryError{Code: ErrCorruptCursorCode, Message: "cursor is not valid base64url JSON"}
	}
	var cur cursor
	if err := json.Unmarshal(data, &cur); err != nil {
		return cursor{}, &QueryError{Code: ErrCorruptCursorCode, Message: "cursor JSON is invalid"}
	}
	if cur.SchemaVersion != DetailSchemaVersion {
		return cursor{}, &QueryError{Code: ErrCorruptCursorCode, Message: "cursor schema version is unsupported"}
	}
	return cur, nil
}

func sectionSet(sections []string) map[string]bool {
	if len(sections) == 0 {
		return nil
	}
	out := map[string]bool{}
	for _, section := range sections {
		section = strings.TrimSpace(section)
		if section != "" {
			out[section] = true
		}
	}
	return out
}

func filteredSpecs(allowed map[string]bool) []rowSpec {
	specs := allSpecs()
	if len(allowed) == 0 {
		return specs
	}
	out := specs[:0]
	for _, spec := range specs {
		if allowed[spec.section] {
			out = append(out, spec)
		}
	}
	return out
}

func classifyReadError(err error) error {
	if err == nil {
		return nil
	}
	var qerr *QueryError
	if errors.As(err, &qerr) {
		return qerr
	}
	if strings.Contains(err.Error(), "no such table") || strings.Contains(err.Error(), "no such column") {
		return &QueryError{Code: ErrPartialMigrationCode, Message: "storage schema is missing required read-model tables or columns"}
	}
	return &QueryError{Code: ErrStorageReadCode, Message: err.Error()}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func allSpecs() []rowSpec {
	arg := func(sc scope) []any { return []any{sc.projectID, sc.runID} }
	projectRun := `WHERE project_id = ? AND delivery_run_id = ?`
	return []rowSpec{
		spec("agents", "agent_budget_binding", "agent_budget_bindings", "id", `FROM agent_budget_bindings `+projectRun, arg,
			`id, project_id, delivery_run_id, 'loopcoder.agent_budget_binding.v1', 1, created_at, updated_at, 'child_agent_id=' || child_agent_id || char(31) || 'budget_policy_id=' || budget_policy_id || char(31) || 'budget_reservation_id=' || budget_reservation_id, 'unknown', 'unknown', '', reservation_state, '{}', reserved_quantities_json, ancestor_budget_refs_json, child_agent_id`),
		spec("agents", "agent_event", "agent_events", "id", `FROM agent_events `+projectRun, arg,
			`id, project_id, delivery_run_id, 'loopcoder.agent_event.v1', 1, created_at, created_at, 'child_agent_id=' || child_agent_id || char(31) || 'event_kind=' || event_kind, 'observed', 'storage_snapshot', '', event_kind, payload_json, '{}', '{}', child_agent_id || ':' || event_kind`),
		spec("agents", "agent_ownership_lock", "agent_ownership_locks", "id", `FROM agent_ownership_locks `+projectRun, arg,
			`id, project_id, delivery_run_id, 'loopcoder.agent_ownership_lock.v1', 1, created_at, updated_at, 'child_agent_id=' || child_agent_id || char(31) || 'run_id=' || run_id || char(31) || 'claim_generation=' || claim_generation, 'unknown', 'unknown', '', state, '{}', conflicts_with_json, '{}', child_agent_id || ':' || resource_kind || ':' || resource_key`),
		spec("agents", "agent_registration", "agent_registrations", "id", `FROM agent_registrations `+projectRun, arg,
			`id, project_id, delivery_run_id, 'loopcoder.agent_registration.v1', record_version, created_at, updated_at, 'child_agent_id=' || id || char(31) || 'parent_agent_id=' || parent_agent_id || char(31) || 'child_run_id=' || child_run_id || char(31) || 'parent_run_id=' || parent_run_id || char(31) || 'task_id=' || task_id || char(31) || 'attempt_id=' || attempt_id, 'unknown', 'unknown', classification, registration_state, '{}', expected_outputs_json, gap_reasons_json, child_run_id`),
		spec("agents", "agent_scope_grant", "agent_scope_grants", "id", `FROM agent_scope_grants `+projectRun, arg,
			`id, project_id, delivery_run_id, schema_version, record_version, created_at, updated_at, 'child_agent_id=' || child_agent_id || char(31) || 'permission=' || permission, 'unknown', 'unknown', '', terminal_error_code, '{}', scope_json, '{}', child_agent_id`),
		spec("agents", "nested_scheduler_reservation", "nested_scheduler_resource_reservations", "reservation_id",
			`FROM nested_scheduler_resource_reservations nsr WHERE EXISTS (SELECT 1 FROM runs r JOIN delivery_runs d ON d.project_id = ? AND d.delivery_run_id = ? AND r.root_run_id = d.root_run_id WHERE r.id = nsr.run_id AND r.project_id = d.project_id)`, arg,
			`reservation_id, '', '', 'loopcoder.nested_scheduler_resource_reservation.v1', 1, created_at, updated_at, 'run_id=' || run_id || char(31) || 'parent_run_id=' || parent_run_id || char(31) || 'claim_generation=' || claim_generation, 'unknown', 'unknown', '', state, '{}', '{}', '{}', run_id || ':' || resource_kind || ':' || resource_key`),
		spec("agents", "run", "runs", "id",
			`FROM runs r WHERE r.project_id = ? AND EXISTS (SELECT 1 FROM delivery_runs d WHERE d.project_id = r.project_id AND d.delivery_run_id = ? AND d.root_run_id = r.root_run_id)`, arg,
			`id, project_id, '', 'loopcoder.run.v1', 1, created_at, updated_at, 'run_id=' || id || char(31) || 'parent_run_id=' || COALESCE(parent_run_id, '') || char(31) || 'root_run_id=' || root_run_id, 'observed', 'storage_snapshot', '', status, '{}', '{}', '{}', id`),
		spec("agents", "run_claim", "run_claims", "run_id",
			`FROM run_claims rc WHERE EXISTS (SELECT 1 FROM runs r JOIN delivery_runs d ON d.project_id = ? AND d.delivery_run_id = ? AND r.root_run_id = d.root_run_id WHERE r.id = rc.run_id AND r.project_id = d.project_id)`, arg,
			`run_id, '', '', 'loopcoder.run_claim.v1', 1, claimed_at, heartbeat_at, 'run_id=' || run_id || char(31) || 'claim_generation=' || claim_generation || char(31) || 'executor_id=' || executor_id, 'unknown', 'unknown', '', phase, '{}', '{}', '{}', run_id`),
		spec("agents", "run_edge", "run_edges", "parent_run_id || ':' || child_run_id",
			`FROM run_edges e WHERE EXISTS (SELECT 1 FROM runs pr JOIN runs cr ON cr.id = e.child_run_id JOIN delivery_runs d ON d.project_id = ? AND d.delivery_run_id = ? AND pr.root_run_id = d.root_run_id WHERE pr.id = e.parent_run_id AND pr.project_id = d.project_id AND cr.project_id = d.project_id)`, arg,
			`parent_run_id || ':' || child_run_id, '', '', 'loopcoder.run_edge.v1', 1, created_at, updated_at, 'parent_run_id=' || parent_run_id || char(31) || 'child_run_id=' || child_run_id || char(31) || 'plan_id=' || plan_id, 'observed', 'storage_snapshot', '', status, '{}', scope_json, aggregation_json, parent_run_id || ':' || child_run_id`),
		spec("budgets", "budget_policy", "budget_policies", "budget_policy_id", `FROM budget_policies `+projectRun, arg,
			`budget_policy_id, project_id, delivery_run_id, 'loopcoder.budget_policy.v1', 1, '', '', 'task_id=' || task_id || char(31) || 'worker_id=' || worker_id || char(31) || 'sub_agent_id=' || sub_agent_id, 'unknown', 'unknown', '', policy_mode, payload_json, parent_budget_policy_ids_json, '{}', scope_key || ':' || quantity_kind || ':' || window_kind`),
		spec("budgets", "budget_reservation", "budget_reservations", "budget_reservation_id", `FROM budget_reservations `+projectRun, arg,
			`budget_reservation_id, project_id, delivery_run_id, 'loopcoder.budget_reservation.v1', 1, created_at, updated_at, 'task_id=' || task_id || char(31) || 'worker_id=' || worker_id || char(31) || 'sub_agent_id=' || sub_agent_id || char(31) || 'generation=' || generation, 'unknown', 'unknown', '', state, payload_json, policy_ids_json, source_estimate_usage_record_ids_json, budget_reservation_id`),
		spec("budgets", "quota_budget_event", "quota_budget_events", "event_id",
			`FROM quota_budget_events qbe WHERE EXISTS (SELECT 1 FROM budget_reservations br WHERE br.project_id = ? AND br.delivery_run_id = ? AND br.budget_reservation_id = qbe.budget_reservation_id) OR EXISTS (SELECT 1 FROM budget_policies bp WHERE bp.project_id = ? AND bp.delivery_run_id = ? AND bp.budget_policy_id = qbe.budget_policy_id)`,
			func(sc scope) []any { return []any{sc.projectID, sc.runID, sc.projectID, sc.runID} },
			`event_id, '', '', 'loopcoder.quota_budget_event.v1', 1, event_time, event_time, 'budget_reservation_id=' || budget_reservation_id || char(31) || 'budget_policy_id=' || budget_policy_id || char(31) || 'generation=' || generation, 'observed', 'storage_snapshot', '', event_kind, payload_json, actor_json, host_json, event_id`),
		spec("handoffs", "handoff_transaction", "handoff_transactions", "handoff_id", `FROM handoff_transactions `+projectRun, arg,
			`handoff_id, project_id, delivery_run_id, schema_version, record_version, created_at, updated_at, 'task_id=' || task_id || char(31) || 'child_run_id=' || child_run_id || char(31) || 'source_attempt_id=' || source_attempt_id || char(31) || 'handoff_generation=' || handoff_generation, 'unknown', 'unknown', '', handoff_status, '{}', evidence_record_ids_json, reason_codes_json, task_id || ':' || handoff_generation`),
		spec("inventory", "inventory_event", "inventory_events", "inventory_event_id", `FROM inventory_events WHERE project_id = ?`, func(sc scope) []any { return []any{sc.projectID} },
			`inventory_event_id, project_id, '', 'loopcoder.inventory_event.v1', 1, created_at, created_at, 'event_kind=' || event_kind, 'observed', 'storage_snapshot', '', event_kind, payload_json, '{}', '{}', event_hash`),
		spec("inventory", "provider_installation", "provider_installations", "provider_installation_id", `FROM provider_installations WHERE project_id = ?`, func(sc scope) []any { return []any{sc.projectID} },
			`provider_installation_id, project_id, '', schema_version, record_version, created_at, updated_at, 'adapter_id=' || adapter_id || char(31) || 'probe_result_id=' || COALESCE(latest_probe_result_id, ''), confidence, freshness_state, classification, installation_state, payload_json, source_json, evidence_json, adapter_id || ':' || executable_name`),
		spec("inventory", "provider_probe_result", "provider_probe_results", "probe_result_id", `FROM provider_probe_results WHERE project_id = ?`, func(sc scope) []any { return []any{sc.projectID} },
			`probe_result_id, project_id, '', schema_version, record_version, created_at, updated_at, 'adapter_id=' || adapter_id || char(31) || 'provider_installation_id=' || COALESCE(provider_installation_id, ''), confidence, freshness_state, '', outcome, payload_json, '{}', '{}', probe_result_id`),
		spec("plans_tasks", "accepted_task_graph_version", "accepted_task_graph_versions", "graph_version_id", `FROM accepted_task_graph_versions `+projectRun, arg,
			`graph_version_id, project_id, delivery_run_id, schema_version, record_version, accepted_at, accepted_at, 'plan_fingerprint=' || plan_fingerprint || char(31) || 'task_graph_validation_id=' || task_graph_validation_id, 'observed', 'storage_snapshot', '', approval_id, proposal_json, accepted_by_json, host_json, graph_fingerprint`),
		spec("plans_tasks", "delivery_attempt", "delivery_attempts", "attempt_id", `FROM delivery_attempts `+projectRun, arg,
			`attempt_id, project_id, delivery_run_id, schema_version, record_version, created_at, updated_at, 'task_id=' || task_id || char(31) || 'attempt_id=' || attempt_id || char(31) || 'claim_generation=' || COALESCE(claim_generation, 0), 'unknown', 'unknown', '', state, '{}', created_by_json, updated_by_json, task_id || ':' || attempt_ordinal`),
		spec("plans_tasks", "delivery_dependency_edge", "delivery_dependency_edges", "edge_id", `FROM delivery_dependency_edges `+projectRun, arg,
			`edge_id, project_id, delivery_run_id, schema_version, record_version, created_at, updated_at, 'from_task_id=' || from_task_id || char(31) || 'to_task_id=' || to_task_id, 'observed', 'storage_snapshot', '', edge_kind, '{}', created_by_json, host_json, from_task_id || ':' || to_task_id || ':' || edge_kind`),
		spec("plans_tasks", "delivery_run", "delivery_runs", "delivery_run_id", `FROM delivery_runs `+projectRun, arg,
			`delivery_run_id, project_id, delivery_run_id, schema_version, record_version, created_at, updated_at, 'root_run_id=' || root_run_id || char(31) || 'parent_run_id=' || COALESCE(parent_run_id, ''), 'observed', 'storage_snapshot', '', state, '{}', report_ids_json, host_json, delivery_run_id`),
		spec("plans_tasks", "delivery_task", "delivery_tasks", "task_id", `FROM delivery_tasks `+projectRun, arg,
			`task_id, project_id, delivery_run_id, schema_version, record_version, created_at, updated_at, 'task_id=' || task_id || char(31) || 'active_attempt_id=' || COALESCE(active_attempt_id, ''), 'unknown', 'unknown', '', state, '{}', requirements_json, scope_json, task_key`),
		spec("plans_tasks", "task_graph_validation", "task_graph_validations", "task_graph_validation_id", `FROM task_graph_validations `+projectRun, arg,
			`task_graph_validation_id, project_id, delivery_run_id, schema_version, record_version, created_at, created_at, 'plan_fingerprint=' || plan_fingerprint, 'observed', 'storage_snapshot', '', validation_status, validation_json, created_by_json, host_json, plan_fingerprint`),
		spec("plans_tasks", "task_requirement", "task_requirements", "task_requirement_id", `FROM task_requirements `+projectRun, arg,
			`task_requirement_id, project_id, delivery_run_id, schema_version, record_version, created_at, updated_at, 'task_id=' || task_id || char(31) || 'task_requirement_id=' || task_requirement_id, confidence, 'storage_snapshot', classification, terminal_error_code, payload_json, source_json, gap_reasons_json, task_id || ':' || plan_fingerprint`),
		spec("plans_tasks", "task_requirement_override", "task_requirement_overrides", "requirement_override_id", `FROM task_requirement_overrides `+projectRun, arg,
			`requirement_override_id, project_id, delivery_run_id, schema_version, record_version, created_at, updated_at, 'task_id=' || COALESCE(task_id, '') || char(31) || 'field=' || field, confidence, 'storage_snapshot', classification, status, payload_json, source_json, gap_reasons_json, task_key || ':' || field`),
		spec("progress", "progress_receipt", "progress_receipts", "progress_receipt_id", `FROM progress_receipts `+projectRun, arg,
			`progress_receipt_id, project_id, delivery_run_id, schema_version, record_version, occurred_at, persisted_at, 'run_id=' || run_id || char(31) || 'task_id=' || task_id || char(31) || 'attempt_id=' || attempt_id || char(31) || 'correlation_id=' || correlation_id || char(31) || 'correlation_sequence=' || correlation_sequence, 'observed', 'storage_snapshot', '', status, payload_json, evidence_json, redaction_json, correlation_id || ':' || semantic_fingerprint`),
		spec("quota_usage", "usage_record", "usage_records", "usage_record_id", `FROM usage_records `+projectRun, arg,
			`usage_record_id, project_id, delivery_run_id, 'loopcoder.usage_record.v1', 1, event_time, event_time, 'task_id=' || task_id || char(31) || 'attempt_id=' || attempt_id || char(31) || 'worker_id=' || worker_id || char(31) || 'sub_agent_id=' || sub_agent_id || char(31) || 'adapter_id=' || adapter_id, confidence, 'storage_snapshot', '', event_kind, payload_json, '{}', '{}', idempotency_key`),
		spec("routing", "delivery_decision", "delivery_decisions", "decision_id", `FROM delivery_decisions `+projectRun, arg,
			`decision_id, project_id, delivery_run_id, schema_version, record_version, created_at, created_at, 'task_id=' || COALESCE(task_id, '') || char(31) || 'decision_key=' || decision_key, 'observed', 'storage_snapshot', '', decision_kind, output_json, alternatives_json, decided_by_json, decision_key`),
		spec("routing", "fallback_decision", "fallback_decisions", "fallback_decision_id", `FROM fallback_decisions `+projectRun, arg,
			`fallback_decision_id, project_id, delivery_run_id, schema_version, record_version, created_at, updated_at, 'task_id=' || task_id || char(31) || 'routing_decision_id=' || routing_decision_id || char(31) || 'fallback_ordinal=' || fallback_ordinal, 'observed', 'storage_snapshot', '', decision_status, payload_json, legality_results_json, attempt_lineage_json, routing_decision_id || ':' || fallback_ordinal`),
		spec("routing", "replan_decision", "replan_decisions", "replan_decision_id", `FROM replan_decisions `+projectRun, arg,
			`replan_decision_id, project_id, delivery_run_id, schema_version, record_version, created_at, updated_at, 'routing_decision_id=' || routing_decision_id || char(31) || 'replan_ordinal=' || replan_ordinal, 'observed', 'storage_snapshot', '', decision_status, payload_json, changed_authority_inputs_json, attempt_lineage_json, replan_ordinal`),
		spec("routing", "routing_decision", "routing_decisions", "routing_decision_id", `FROM routing_decisions `+projectRun, arg,
			`routing_decision_id, project_id, delivery_run_id, schema_version, record_version, created_at, updated_at, 'task_id=' || task_id || char(31) || 'task_requirement_id=' || task_requirement_id || char(31) || 'decision_key=' || decision_key, 'observed', 'storage_snapshot', '', decision_status, payload_json, input_record_refs_json, rejected_summary_json, decision_key || ':' || routing_fingerprint`),
		spec("routing", "routing_event", "routing_events", "routing_event_id", `FROM routing_events `+projectRun, arg,
			`routing_event_id, project_id, delivery_run_id, 'loopcoder.routing_event.v1', 1, created_at, created_at, 'routing_policy_profile_id=' || routing_policy_profile_id || char(31) || 'routing_fingerprint=' || routing_fingerprint, 'observed', 'storage_snapshot', '', event_kind, payload_json, '{}', '{}', routing_event_id`),
		spec("routing", "routing_policy_input", "routing_policy_inputs", "routing_policy_input_id", `FROM routing_policy_inputs `+projectRun, arg,
			`routing_policy_input_id, project_id, delivery_run_id, schema_version, record_version, created_at, updated_at, 'routing_policy_profile_id=' || routing_policy_profile_id || char(31) || 'input_kind=' || input_kind, 'observed', 'storage_snapshot', '', status, payload_json, constraint_json, diagnostics_json, routing_policy_profile_id || ':' || input_kind || ':' || scope`),
		spec("routing", "verification_decision", "verification_decisions", "verification_decision_id", `FROM verification_decisions `+projectRun, arg,
			`verification_decision_id, project_id, delivery_run_id, schema_version, record_version, created_at, updated_at, 'task_id=' || task_id || char(31) || 'worker_routing_decision_id=' || worker_routing_decision_id || char(31) || 'verifier_routing_decision_id=' || verifier_routing_decision_id, 'observed', 'storage_snapshot', '', decision_status, payload_json, evidence_refs_json, disagreements_json, task_id || ':' || decision_key || ':' || idempotency_key`),
		spec("routing", "verification_member", "verification_decision_members", "verification_decision_id || ':' || member_ordinal",
			`FROM verification_decision_members vdm WHERE EXISTS (SELECT 1 FROM verification_decisions vd WHERE vd.project_id = ? AND vd.delivery_run_id = ? AND vd.verification_decision_id = vdm.verification_decision_id)`, arg,
			`verification_decision_id || ':' || member_ordinal, '', '', 'loopcoder.verification_decision_member.v1', 1, '', '', 'verification_decision_id=' || verification_decision_id || char(31) || 'routing_decision_id=' || routing_decision_id || char(31) || 'member_id=' || member_id, 'observed', 'storage_snapshot', '', actual_independence, payload_json, '{}', '{}', verification_decision_id || ':' || member_ordinal`),
	}
}

func spec(section, kind, table, idAlias, from string, args func(scope) []any, selects string) rowSpec {
	return rowSpec{
		section: section,
		kind:    kind,
		table:   table,
		idAlias: idAlias,
		from:    from,
		args:    args,
		selects: selects,
	}
}
