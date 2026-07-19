package releasesmoke

import (
	"fmt"

	"github.com/jasonhnd/loopcoder/internal/storage"
)

// CurrentSchema is the binary's supported storage schema generation.
// Smoke asserts against this constant — never a hard-coded magic number.
func CurrentSchema() int {
	return storage.CurrentSchemaVersion
}

// LegacyV07Schema is the intentional v0.7 fixture anchor for upgrade smoke.
const LegacyV07Schema = 9

type storagePlanPayload struct {
	DryRun  bool `json:"dry_run"`
	Applied bool `json:"applied"`
	Status  string `json:"status"`
	Plan    struct {
		SourceSchemaVersion int    `json:"source_schema_version"`
		TargetSchemaVersion int    `json:"target_schema_version"`
		Status              string `json:"status"`
		BackupRequired      bool   `json:"backup_required"`
	} `json:"plan"`
	Health *struct {
		SchemaVersion int  `json:"schema_version"`
		OK            bool `json:"ok"`
	} `json:"health"`
	Backup *struct {
		Verified bool   `json:"verified"`
		Path     string `json:"path"`
		SHA256   string `json:"sha256"`
	} `json:"backup"`
	Rollback struct {
		Supported       bool   `json:"supported"`
		RequiresOffline bool   `json:"requires_offline"`
		BackupPath      string `json:"backup_path"`
		BackupSHA256    string `json:"backup_sha256"`
	} `json:"rollback"`
}

func assertFreshSchemaCurrent(plan storagePlanPayload) error {
	target := plan.Plan.TargetSchemaVersion
	source := plan.Plan.SourceSchemaVersion
	current := CurrentSchema()
	if !plan.DryRun || plan.Applied || plan.Status != "planned" {
		return fmt.Errorf("fresh-schema migration plan was not read-only: dry_run=%v applied=%v status=%q", plan.DryRun, plan.Applied, plan.Status)
	}
	if target != current || source != target || plan.Plan.Status != "current" {
		return fmt.Errorf("fresh-schema migration plan did not report current schema as current (source=%d target=%d status=%q want source=target=CurrentSchemaVersion=%d status=current)",
			source, target, plan.Plan.Status, current)
	}
	if plan.Plan.BackupRequired {
		return fmt.Errorf("fresh-schema migration plan unexpectedly required a backup")
	}
	return nil
}

func assertUpgradePlanFromV07(plan storagePlanPayload) error {
	target := plan.Plan.TargetSchemaVersion
	current := CurrentSchema()
	if !plan.DryRun || plan.Applied || plan.Status != "planned" {
		return fmt.Errorf("candidate storage migration plan was not read-only")
	}
	if plan.Plan.SourceSchemaVersion != LegacyV07Schema || target != current || plan.Plan.Status != "upgrade-required" {
		return fmt.Errorf("candidate storage migration plan did not describe schema %d -> current (got %d -> %d, status=%q, CurrentSchemaVersion=%d)",
			LegacyV07Schema, plan.Plan.SourceSchemaVersion, target, plan.Plan.Status, current)
	}
	if !plan.Plan.BackupRequired || !plan.Rollback.Supported || !plan.Rollback.RequiresOffline {
		return fmt.Errorf("candidate storage migration plan omitted backup or rollback requirements")
	}
	return nil
}

func assertMigratedToCurrent(apply storagePlanPayload, wantTarget int) error {
	if apply.Status != "migrated" || !apply.Applied {
		return fmt.Errorf("candidate storage migration did not report migrated/applied (status=%q applied=%v)", apply.Status, apply.Applied)
	}
	if apply.Health == nil || !apply.Health.OK || apply.Health.SchemaVersion != wantTarget {
		got := -1
		ok := false
		if apply.Health != nil {
			got = apply.Health.SchemaVersion
			ok = apply.Health.OK
		}
		return fmt.Errorf("candidate storage migration did not finish at healthy schema %d (got schema=%d ok=%v)", wantTarget, got, ok)
	}
	return nil
}
