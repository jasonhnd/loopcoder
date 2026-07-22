// Package workgraphstore persists Work Graph versions through typed repositories
// (V090-057). Lifecycle changes append events; completed versions are never
// overwritten by replan.
package workgraphstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

const SchemaVersionLabel = "loopcoder.workgraph.store.v1"

var (
	ErrInvalid      = errors.New("workgraphstore: invalid")
	ErrNotFound     = errors.New("workgraphstore: not found")
	ErrConflict     = errors.New("workgraphstore: conflict")
	ErrImmutable    = errors.New("workgraphstore: version immutable")
	ErrCrossProject = errors.New("workgraphstore: cross-project dependency")
)

// Repository reads/writes work graphs on a storage.Store.
type Repository struct {
	store storage.Store
}

// New returns a repository.
func New(store storage.Store) *Repository {
	return &Repository{store: store}
}

// PutVersion inserts an immutable graph version. Replan creates a new version
// row; prior versions remain queryable and cannot be UPDATE'd.
func (r *Repository) PutVersion(ctx context.Context, projectID string, g workgraph.Graph) error {
	if r == nil || r.store == nil {
		return fmt.Errorf("%w: nil", ErrInvalid)
	}
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return fmt.Errorf("%w: project_id", ErrInvalid)
	}
	if err := workgraph.ValidateGraph(g); err != nil {
		return err
	}
	if g.PlanDigest == "" {
		g.PlanDigest = workgraph.DigestGraph(g)
	}
	// Reject cross-project endpoints in deps (all items same graph/project by construction).
	for _, d := range g.Dependencies {
		if strings.Contains(d.From, "/") || strings.Contains(d.To, "/") {
			// keys must be stable keys within graph, not project-qualified foreign ids
			return fmt.Errorf("%w: dependency endpoint", ErrCrossProject)
		}
	}
	limitsJSON, _ := json.Marshal(g.Limits)
	payloadJSON, _ := json.Marshal(g)
	now := r.store.Now().UTC().Format(time.RFC3339Nano)
	rowID := fmt.Sprintf("wgv_%s_%s_v%d", projectID, g.GraphID, g.Version)

	return r.store.WithWriteTx(ctx, func(tx storage.Tx) error {
		// Ensure prior versions with same graph_id are not updated — insert only.
		var existing int
		rows, err := tx.Query(ctx, `SELECT COUNT(1) FROM workgraph_versions WHERE project_id=? AND graph_id=? AND version=?`,
			projectID, g.GraphID, g.Version)
		if err != nil {
			return err
		}
		if rows.Next() {
			_ = rows.Scan(&existing)
		}
		_ = rows.Close()
		if existing > 0 {
			return fmt.Errorf("%w: version %d already exists", ErrImmutable, g.Version)
		}

		_, err = tx.Exec(ctx, `INSERT INTO workgraph_versions(
			graph_version_row_id, schema_version, project_id, graph_id, version, plan_digest,
			source, explicit_opt_in, approved_by, direct_run_equivalent, execution_started,
			limits_json, payload_json, created_at, obsolete)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,0)`,
			rowID, SchemaVersionLabel, projectID, g.GraphID, g.Version, g.PlanDigest,
			string(g.Source), boolInt(g.ExplicitOptIn), g.ApprovedBy, boolInt(g.DirectRunEquivalent),
			boolInt(g.ExecutionStarted), string(limitsJSON), string(payloadJSON), now,
		)
		if err != nil {
			return err
		}

		for _, it := range g.Items {
			itemPayload, _ := json.Marshal(it)
			itemID := fmt.Sprintf("wgi_%s_%s_v%d_%s", projectID, g.GraphID, g.Version, it.ID)
			_, err = tx.Exec(ctx, `INSERT INTO workgraph_items(
				workitem_row_id, project_id, graph_id, graph_version, stable_key, intent, status,
				owner, ownership, route_requirement, output_contract, integration_order, terminal,
				attempt_id, provider_child_ref, github_ref, payload_json)
				VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				itemID, projectID, g.GraphID, g.Version, it.ID, it.Intent, string(it.Status),
				it.Owner, string(it.Ownership), it.RouteRequirement, it.OutputContract, it.IntegrationOrder,
				string(it.Terminal), it.AttemptID, it.ProviderChildRef, it.GitHubRef, string(itemPayload),
			)
			if err != nil {
				return err
			}
		}
		for i, d := range g.Dependencies {
			depID := fmt.Sprintf("wgd_%s_%s_v%d_%d", projectID, g.GraphID, g.Version, i)
			_, err = tx.Exec(ctx, `INSERT INTO workgraph_dependencies(
				dep_row_id, project_id, graph_id, graph_version, from_key, to_key, kind)
				VALUES(?,?,?,?,?,?,?)`,
				depID, projectID, g.GraphID, g.Version, d.From, d.To, string(d.Kind),
			)
			if err != nil {
				return err
			}
		}
		// Append create event
		evID := fmt.Sprintf("wge_%s_%s_v%d_create", projectID, g.GraphID, g.Version)
		_, err = tx.Exec(ctx, `INSERT INTO workgraph_events(
			event_id, project_id, graph_id, graph_version, event_type, payload_json, created_at)
			VALUES(?,?,?,?,?,?,?)`,
			evID, projectID, g.GraphID, g.Version, "version_created", `{}`, now,
		)
		return err
	})
}

// GetVersion loads one immutable graph version.
func (r *Repository) GetVersion(ctx context.Context, projectID, graphID string, version int) (workgraph.Graph, error) {
	var g workgraph.Graph
	err := r.store.WithTx(ctx, func(tx storage.Tx) error {
		rows, err := tx.Query(ctx, `SELECT payload_json FROM workgraph_versions WHERE project_id=? AND graph_id=? AND version=?`,
			projectID, graphID, version)
		if err != nil {
			return err
		}
		defer rows.Close()
		if !rows.Next() {
			return ErrNotFound
		}
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return err
		}
		return json.Unmarshal([]byte(payload), &g)
	})
	return g, err
}

// ListVersions returns version numbers for a graph (including obsolete).
func (r *Repository) ListVersions(ctx context.Context, projectID, graphID string) ([]int, error) {
	var out []int
	err := r.store.WithTx(ctx, func(tx storage.Tx) error {
		rows, err := tx.Query(ctx, `SELECT version FROM workgraph_versions WHERE project_id=? AND graph_id=? ORDER BY version`,
			projectID, graphID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v int
			if err := rows.Scan(&v); err != nil {
				return err
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	return out, err
}

// MarkObsolete flags a version without deleting it (replan successor).
func (r *Repository) MarkObsolete(ctx context.Context, projectID, graphID string, version int) error {
	return r.store.WithWriteTx(ctx, func(tx storage.Tx) error {
		res, err := tx.Exec(ctx, `UPDATE workgraph_versions SET obsolete=1 WHERE project_id=? AND graph_id=? AND version=?`,
			projectID, graphID, version)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return ErrNotFound
		}
		now := r.store.Now().UTC().Format(time.RFC3339Nano)
		evID := fmt.Sprintf("wge_%s_%s_v%d_obsolete_%d", projectID, graphID, version, time.Now().UnixNano())
		_, err = tx.Exec(ctx, `INSERT INTO workgraph_events(event_id, project_id, graph_id, graph_version, event_type, payload_json, created_at)
			VALUES(?,?,?,?,?,?,?)`, evID, projectID, graphID, version, "version_obsolete", `{}`, now)
		return err
	})
}

// Inventory reports schema inventory claims for acceptance #5.
func Inventory() map[string]string {
	return map[string]string{
		"workgraph_versions":          "immutable graph versions / digests / limits / approval",
		"workgraph_items":             "WorkItems keyed by stable_key within version",
		"workgraph_dependencies":      "typed deps finish_to_start|output|soft_order",
		"workgraph_events":            "append-only lifecycle events",
		"no_provider_credentials":     "true",
		"no_process_truth":            "true",
		"no_report_table":             "true",
		"no_v08_federation_ownership": "true",
	}
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// Ensure project exists helper for tests — uses minimal projects insert if needed.
func EnsureProject(ctx context.Context, store storage.Store, projectID string) error {
	return store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `INSERT OR IGNORE INTO projects(id, created_at) VALUES(?, ?)`,
			projectID, store.Now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			// try alternate schema
			_, err2 := tx.Exec(ctx, `INSERT OR IGNORE INTO projects(id) VALUES(?)`, projectID)
			if err2 != nil {
				return err
			}
		}
		return nil
	})
}

// OpenTestStore is a test helper.
func OpenTestStore(ctx context.Context, path string) (storage.Store, error) {
	return storage.Open(ctx, storage.Options{Path: path})
}

// Silence unused sql import if any
var _ = sql.ErrNoRows
