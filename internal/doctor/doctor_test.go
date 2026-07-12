package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/budget"
	"github.com/jasonhnd/loopcoder/internal/claudehooks"
	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/localcleanup"
	"github.com/jasonhnd/loopcoder/internal/migration"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/registry"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/usageledger"
)

func TestRunReportsHealthyPreflight(t *testing.T) {
	env := healthyDoctorEnv()

	report := Run(context.Background(), Options{
		RepoPath: "/repo",
		BuildInfo: BuildInfo{
			Version: "0.6.0",
			Commit:  "abc123",
			Date:    "2026-06-29T00:00:00Z",
		},
	}, env.deps())

	if got := report.ExitCode(); got != 0 {
		t.Fatalf("ExitCode = %d, want 0", got)
	}
	for _, name := range []string{
		"git",
		"gh",
		"local-state exclude",
		"tracked .loopcoder",
		".delivery.yml",
		"host profile",
		"model selection",
		"provider codex",
		"provider claude",
		"repository origin",
		"default branch",
		"loopcoder binary",
		"version compatibility",
		"version status",
		"audit config",
		"audit tools",
		"audit parsers",
		"audit rubric",
		"audit baseline",
		"audit ci check",
		"audit llm provider",
		"loopcoder skill",
		"conductor hooks",
		"report query",
		"storage permissions",
		"storage",
		"project registry",
		"migration status",
		"nested runs",
		"stale local state",
		"conductor runtime",
	} {
		check := requireCheck(t, report, name)
		if check.Status != StatusOK {
			t.Fatalf("%s status = %s, want ok (%s)", name, check.Status, check.Message)
		}
	}
	if check := requireCheck(t, report, "default branch"); !strings.Contains(check.Message, "trunk") {
		t.Fatalf("default branch message = %q, want trunk", check.Message)
	}
	if check := requireCheck(t, report, "version compatibility"); !strings.Contains(check.Message, "min_loopcoder_version=0.3.0 is satisfied") {
		t.Fatalf("compatibility message = %q", check.Message)
	}
	if check := requireCheck(t, report, "host profile"); !strings.Contains(check.Message, "profile=claude-code source=env") {
		t.Fatalf("host profile message = %q", check.Message)
	}
}

func TestRunChecksLocalStateExcludeProtection(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(*fakeDoctorEnv)
		want       Status
		wantFix    string
		wantSubstr string
	}{
		{
			name:       "protected",
			want:       StatusOK,
			wantSubstr: "is protected by",
		},
		{
			name: "missing exclude",
			setup: func(env *fakeDoctorEnv) {
				delete(env.files, filepath.Clean(filepath.Join("/repo", ".git", "info", "exclude")))
			},
			want:       StatusWarn,
			wantFix:    "loopcoder skill install --repo .",
			wantSubstr: "does not exist",
		},
		{
			name: "not excluded",
			setup: func(env *fakeDoctorEnv) {
				env.files[filepath.Clean(filepath.Join("/repo", ".git", "info", "exclude"))] = []byte("# other\n")
			},
			want:       StatusWarn,
			wantFix:    "loopcoder skill install --repo .",
			wantSubstr: "is not protected",
		},
		{
			name: "not a git repository",
			setup: func(env *fakeDoctorEnv) {
				env.commands[cmdKey("git", "rev-parse", "--is-inside-work-tree")] = CommandResult{
					Stderr:   "not a git repository",
					ExitCode: 128,
				}
			},
			want:       StatusWarn,
			wantSubstr: "could not resolve",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := healthyDoctorEnv()
			if tt.setup != nil {
				tt.setup(env)
			}

			report := Run(context.Background(), Options{RepoPath: "/repo"}, env.deps())

			check := requireCheck(t, report, "local-state exclude")
			if check.Status != tt.want {
				t.Fatalf("status = %s, want %s (%s)", check.Status, tt.want, check.Message)
			}
			if check.FixCommand != tt.wantFix {
				t.Fatalf("FixCommand = %q, want %q", check.FixCommand, tt.wantFix)
			}
			if !strings.Contains(check.Message, tt.wantSubstr) {
				t.Fatalf("message = %q, want containing %q", check.Message, tt.wantSubstr)
			}
		})
	}
}

func TestRunHardFailsForTrackedLoopcoderState(t *testing.T) {
	env := healthyDoctorEnv()
	env.commands[cmdKey("git", "ls-files", ".loopcoder")] = CommandResult{
		Stdout: ".loopcoder/runs/run-1/workers/worker.attempt.json\n",
	}

	report := Run(context.Background(), Options{RepoPath: "/repo"}, env.deps())

	if got := report.ExitCode(); got != 1 {
		t.Fatalf("ExitCode = %d, want 1", got)
	}
	check := requireCheck(t, report, "tracked .loopcoder")
	const wantFix = "git rm -r --cached .loopcoder && echo .loopcoder/ >> .git/info/exclude"
	if check.Status != StatusFail || !check.Hard {
		t.Fatalf("check = %#v, want hard fail", check)
	}
	if check.FixCommand != wantFix {
		t.Fatalf("FixCommand = %q, want %q", check.FixCommand, wantFix)
	}
	if !strings.Contains(check.Message, wantFix) {
		t.Fatalf("message = %q, want fix command", check.Message)
	}
}

func TestRenderJSONIncludesStableDoctorFields(t *testing.T) {
	report := WithMetadata(Report{Checks: []Check{
		{Name: "local-state exclude", Status: StatusWarn, Message: "missing", FixCommand: "loopcoder skill install --repo ."},
		{Name: "tracked .loopcoder", Status: StatusFail, Message: "tracked", Hard: true, FixCommand: "git rm -r --cached .loopcoder && echo .loopcoder/ >> .git/info/exclude"},
	}}, "/repo", BuildInfo{Version: "0.6.1", Commit: "abc123", Date: "2026-07-08T00:00:00Z"})

	var out bytes.Buffer
	if err := RenderJSON(&out, report); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}

	var payload struct {
		RepoPath string `json:"repo_path"`
		Version  string `json:"version"`
		Commit   string `json:"commit"`
		Date     string `json:"date"`
		ExitCode int    `json:"exit_code"`
		Host     struct {
			Name               string `json:"name"`
			Source             string `json:"source"`
			SupportsJSONOutput bool   `json:"supports_json_output"`
		} `json:"host_profile"`
		Runtime struct {
			Database struct {
				Status Status `json:"status"`
			} `json:"database"`
			ProjectRegistry struct {
				Status Status `json:"status"`
			} `json:"project_registry"`
			Migration struct {
				Status Status `json:"status"`
			} `json:"migration"`
			NestedRuns struct {
				Status Status `json:"status"`
			} `json:"nested_runs"`
		} `json:"runtime"`
		ProviderCompatibility []struct {
			Provider string `json:"provider"`
			Host     string `json:"host"`
			Role     string `json:"role"`
			Support  string `json:"support"`
			Status   Status `json:"status"`
			Code     string `json:"code"`
		} `json:"provider_compatibility"`
		Checks []struct {
			Name       string `json:"name"`
			Code       string `json:"code"`
			Status     Status `json:"status"`
			Hard       bool   `json:"hard"`
			Message    string `json:"message"`
			FixCommand string `json:"fix_command"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal: %v\n%s", err, out.String())
	}
	if payload.RepoPath != "/repo" || payload.Version != "0.6.1" || payload.Commit != "abc123" || payload.Date != "2026-07-08T00:00:00Z" {
		t.Fatalf("metadata = %#v", payload)
	}
	if payload.ExitCode != 1 {
		t.Fatalf("exit_code = %d, want 1", payload.ExitCode)
	}
	if payload.Host.Name != "generic-local" || payload.Host.Source != "fallback" || !payload.Host.SupportsJSONOutput {
		t.Fatalf("host_profile = %#v, want generic fallback", payload.Host)
	}
	if payload.Runtime.Database.Status != "" || payload.Runtime.ProjectRegistry.Status != "" || payload.Runtime.Migration.Status != "" || payload.Runtime.NestedRuns.Status != "" {
		t.Fatalf("runtime should be empty for manually constructed report: %#v", payload.Runtime)
	}
	if payload.ProviderCompatibility == nil {
		t.Fatalf("provider_compatibility missing from payload: %s", out.String())
	}
	if len(payload.Checks) != 2 {
		t.Fatalf("checks len = %d, want 2", len(payload.Checks))
	}
	if payload.Checks[1].Name != "tracked .loopcoder" || payload.Checks[1].FixCommand == "" || !payload.Checks[1].Hard {
		t.Fatalf("tracked check = %#v", payload.Checks[1])
	}
}

func TestRenderJSONIncludesQuotaUsageBudgetFallbackContract(t *testing.T) {
	now := time.Unix(0, 0).UTC().Add(803 * time.Hour)
	repo := t.TempDir()
	runID := state.RunIDForIssue(730, now)
	report := doctorUsageReport(730, now)
	if _, err := state.WriteAttempt(repo, runID, state.AttemptRecord{
		Version:        1,
		JobID:          "job-730-1",
		Issue:          730,
		Attempt:        1,
		Provider:       "codex",
		PID:            123,
		Phase:          "codex_exited",
		Status:         "succeeded",
		StartedAt:      report.StartedAt,
		HeartbeatAt:    report.EndedAt,
		LastProgressAt: report.EndedAt,
		Report:         &report,
	}); err != nil {
		t.Fatalf("WriteAttempt: %v", err)
	}

	budget, check := buildQuotaUsageBudget(context.Background(), repo, "proj_test", now)
	if check.Status != StatusOK {
		t.Fatalf("quota usage check = %#v, want OK", check)
	}
	var out bytes.Buffer
	if err := RenderJSON(&out, WithMetadata(Report{QuotaUsageBudget: budget, Checks: []Check{check}}, repo, BuildInfo{Version: "0.8.0", Commit: "abc123", Date: now.Format(time.RFC3339Nano)})); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}

	var payload struct {
		QuotaUsageBudget struct {
			SchemaVersion         string                       `json:"schema_version"`
			GeneratedAt           string                       `json:"generated_at"`
			QuotaUsageFingerprint string                       `json:"quota_usage_fingerprint"`
			Confidence            providerinventory.Confidence `json:"confidence"`
			UsageSummary          []struct {
				AdapterID      string                         `json:"adapter_id"`
				QuantityKind   providerinventory.QuantityKind `json:"quantity_kind"`
				TotalValue     int64                          `json:"total_value"`
				Confidence     providerinventory.Confidence   `json:"confidence"`
				UsageRecordIDs []string                       `json:"usage_record_ids"`
			} `json:"usage_summary"`
			GapReasons []string `json:"gap_reasons"`
		} `json:"quota_usage_budget"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal: %v\n%s", err, out.String())
	}
	if payload.QuotaUsageBudget.SchemaVersion != "loopcoder.quota_usage_budget_json.v1" || payload.QuotaUsageBudget.GeneratedAt == "" {
		t.Fatalf("quota usage metadata = %#v", payload.QuotaUsageBudget)
	}
	if !strings.HasPrefix(payload.QuotaUsageBudget.QuotaUsageFingerprint, "sha256:") {
		t.Fatalf("fingerprint = %q", payload.QuotaUsageBudget.QuotaUsageFingerprint)
	}
	if payload.QuotaUsageBudget.Confidence != providerinventory.ConfidenceEstimated {
		t.Fatalf("confidence = %q, want estimated fallback", payload.QuotaUsageBudget.Confidence)
	}
	for _, want := range []string{"persisted-ledger-empty", "derived-from-reports-fallback", "loopcoder-local-ledger-not-provider-global"} {
		if !containsString(payload.QuotaUsageBudget.GapReasons, want) {
			t.Fatalf("gap reasons = %#v, missing %q", payload.QuotaUsageBudget.GapReasons, want)
		}
	}
	if len(payload.QuotaUsageBudget.UsageSummary) == 0 {
		t.Fatalf("usage summary empty:\n%s", out.String())
	}
	foundTotal := false
	for _, summary := range payload.QuotaUsageBudget.UsageSummary {
		if summary.AdapterID == "codex" && summary.QuantityKind == providerinventory.QuantityTotalTokens && summary.TotalValue == 7300 {
			foundTotal = true
			if summary.Confidence != providerinventory.ConfidenceEstimated || len(summary.UsageRecordIDs) == 0 {
				t.Fatalf("total summary = %#v", summary)
			}
		}
	}
	if !foundTotal {
		t.Fatalf("total token summary missing: %#v", payload.QuotaUsageBudget.UsageSummary)
	}
}

func TestRenderJSONIncludesBudgetSummaryContract(t *testing.T) {
	now := time.Unix(0, 0).UTC().Add(804 * time.Hour)
	report := usageledger.QuotaUsageBudget{
		SchemaVersion:         usageledger.QuotaUsageBudgetSchema,
		GeneratedAt:           now.Format(time.RFC3339Nano),
		QuotaUsageFingerprint: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		Confidence:            providerinventory.ConfidenceEstimated,
		QuotaSources:          []providerinventory.QuotaTelemetrySource{},
		QuotaSnapshots:        []providerinventory.QuotaSnapshot{},
		UsageSummary:          []usageledger.UsageSummary{},
		BudgetSummary: []budget.Summary{{
			BudgetPolicyID:       "bpol_test",
			Scope:                budget.Scope{ScopeKind: budget.ScopeProject, ProjectID: "proj_test"},
			ScopeKey:             `{"scope_kind":"project","project_id":"proj_test"}`,
			QuantityKind:         providerinventory.QuantityTotalTokens,
			Unit:                 "token",
			WindowKind:           providerinventory.WindowUnbounded,
			PolicyMode:           budget.PolicyHard,
			CeilingValue:         100,
			ReservedValue:        40,
			CommittedValue:       25,
			AvailableValue:       35,
			EffectiveCeiling:     100,
			Confidence:           providerinventory.ConfidenceExact,
			PolicyVersion:        "test-v1",
			ActiveReservationIDs: []string{"bres_test"},
			Denial:               "ErrBudgetExhausted",
			ApprovalID:           "approval_test",
			GapReasons:           []string{"operator-configured-budget-policy"},
		}},
		AvailabilityScores: []any{},
		CircuitBreakers:    []any{},
		GapReasons:         []string{"operator-configured-budget-policy"},
	}
	var out bytes.Buffer
	if err := RenderJSON(&out, WithMetadata(Report{QuotaUsageBudget: report}, t.TempDir(), BuildInfo{Version: "0.8.0", Commit: "abc123", Date: now.Format(time.RFC3339Nano)})); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	var payload struct {
		QuotaUsageBudget struct {
			BudgetSummary []struct {
				BudgetPolicyID       string                         `json:"budget_policy_id"`
				QuantityKind         providerinventory.QuantityKind `json:"quantity_kind"`
				CeilingValue         int64                          `json:"ceiling_value"`
				ReservedValue        int64                          `json:"reserved_value"`
				CommittedValue       int64                          `json:"committed_value"`
				AvailableValue       int64                          `json:"available_value"`
				EffectiveCeiling     int64                          `json:"effective_ceiling"`
				Confidence           providerinventory.Confidence   `json:"confidence"`
				PolicyVersion        string                         `json:"policy_version"`
				ActiveReservationIDs []string                       `json:"active_reservation_ids"`
				Denial               string                         `json:"denial"`
				ApprovalID           string                         `json:"approval_id"`
				GapReasons           []string                       `json:"gap_reasons"`
			} `json:"budget_summary"`
		} `json:"quota_usage_budget"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal: %v\n%s", err, out.String())
	}
	if len(payload.QuotaUsageBudget.BudgetSummary) != 1 {
		t.Fatalf("budget summary = %#v", payload.QuotaUsageBudget.BudgetSummary)
	}
	got := payload.QuotaUsageBudget.BudgetSummary[0]
	if got.BudgetPolicyID != "bpol_test" || got.AvailableValue != 35 || got.ReservedValue != 40 || got.CommittedValue != 25 || got.Denial != "ErrBudgetExhausted" || got.ApprovalID != "approval_test" {
		t.Fatalf("budget summary = %#v, want accounting and provenance fields", got)
	}
	if got.Confidence != providerinventory.ConfidenceExact || len(got.ActiveReservationIDs) != 1 || !containsString(got.GapReasons, "operator-configured-budget-policy") {
		t.Fatalf("budget summary confidence/provenance = %#v", got)
	}
}

func TestRunJSONIncludesV07RuntimeHealth(t *testing.T) {
	env := healthyDoctorEnv()

	report := Run(context.Background(), Options{RepoPath: "/repo"}, env.deps())
	var out bytes.Buffer
	if err := RenderJSON(&out, report); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}

	var payload struct {
		Runtime struct {
			HomeDir  string `json:"home_dir"`
			Database struct {
				Path          string `json:"path"`
				Exists        bool   `json:"exists"`
				SchemaVersion int    `json:"schema_version"`
				Status        Status `json:"status"`
				Message       string `json:"message"`
			} `json:"database"`
			ProjectRegistry struct {
				Status         Status `json:"status"`
				Registered     bool   `json:"registered"`
				ProjectID      string `json:"project_id"`
				IdentitySource string `json:"identity_source"`
				ConflictCount  int    `json:"conflict_count"`
			} `json:"project_registry"`
			Migration struct {
				Status         Status `json:"status"`
				LegacySurfaces int    `json:"legacy_surfaces"`
			} `json:"migration"`
			NestedRuns struct {
				Status       Status `json:"status"`
				RunCount     int    `json:"run_count"`
				ProblemCount int    `json:"problem_count"`
			} `json:"nested_runs"`
		} `json:"runtime"`
		ProviderCompatibility []struct {
			Provider string `json:"provider"`
		} `json:"provider_compatibility"`
		Checks []struct {
			Name   string `json:"name"`
			Status Status `json:"status"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal: %v\n%s", err, out.String())
	}
	if payload.Runtime.HomeDir == "" || !strings.Contains(payload.Runtime.Database.Path, "loopcoder.db") {
		t.Fatalf("runtime paths missing: %#v", payload.Runtime)
	}
	if !payload.Runtime.Database.Exists || payload.Runtime.Database.SchemaVersion != storage.CurrentSchemaVersion || payload.Runtime.Database.Status != StatusOK {
		t.Fatalf("database runtime = %#v", payload.Runtime.Database)
	}
	if !payload.Runtime.ProjectRegistry.Registered || payload.Runtime.ProjectRegistry.ProjectID != "proj_test" || payload.Runtime.ProjectRegistry.IdentitySource != string(registry.IdentityGitHub) {
		t.Fatalf("project registry runtime = %#v", payload.Runtime.ProjectRegistry)
	}
	if payload.Runtime.Migration.Status != StatusOK || payload.Runtime.Migration.LegacySurfaces != 0 {
		t.Fatalf("migration runtime = %#v", payload.Runtime.Migration)
	}
	if payload.Runtime.NestedRuns.Status != StatusOK || payload.Runtime.NestedRuns.ProblemCount != 0 {
		t.Fatalf("nested runs runtime = %#v", payload.Runtime.NestedRuns)
	}
	if len(payload.ProviderCompatibility) == 0 {
		t.Fatalf("provider compatibility missing")
	}
	foundNestedCheck := false
	for _, check := range payload.Checks {
		if check.Name == "nested runs" {
			foundNestedCheck = true
			if check.Status != StatusOK {
				t.Fatalf("nested runs check = %#v", check)
			}
		}
	}
	if !foundNestedCheck {
		t.Fatalf("nested runs check missing")
	}
}

func TestRunJSONIncludesProviderCompatibilityMatrix(t *testing.T) {
	env := healthyDoctorEnv()

	report := Run(context.Background(), Options{RepoPath: "/repo"}, env.deps())
	var out bytes.Buffer
	if err := RenderJSON(&out, report); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}

	var payload struct {
		ProviderCompatibility []struct {
			Provider            string   `json:"provider"`
			Host                string   `json:"host"`
			Role                string   `json:"role"`
			Support             string   `json:"support"`
			Status              Status   `json:"status"`
			Code                string   `json:"code"`
			MissingCapabilities []string `json:"missing_capabilities"`
		} `json:"provider_compatibility"`
		Checks []struct {
			Name   string `json:"name"`
			Code   string `json:"code"`
			Status Status `json:"status"`
			Hard   bool   `json:"hard"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal: %v\n%s", err, out.String())
	}
	if len(payload.ProviderCompatibility) == 0 {
		t.Fatalf("provider_compatibility is empty:\n%s", out.String())
	}

	foundMatrixEntry := false
	for _, entry := range payload.ProviderCompatibility {
		if entry.Provider == "antigravity" && entry.Host == "claude-code" && entry.Role == "verifier" {
			foundMatrixEntry = true
			if entry.Support != "unsupported" || entry.Status != StatusFail || entry.Code != "unsupported_read_only_mode" {
				t.Fatalf("antigravity verifier matrix entry = %#v", entry)
			}
			if !containsString(entry.MissingCapabilities, "read-only") {
				t.Fatalf("antigravity verifier missing capabilities = %#v, want read-only", entry.MissingCapabilities)
			}
		}
	}
	if !foundMatrixEntry {
		t.Fatalf("provider_compatibility missing antigravity/claude-code/verifier entry")
	}

	foundSelectedWorker := false
	for _, check := range payload.Checks {
		if check.Name == "provider compatibility codex worker" {
			foundSelectedWorker = true
			if check.Status != StatusOK || check.Code != "supported" || check.Hard {
				t.Fatalf("selected worker compatibility check = %#v", check)
			}
		}
	}
	if !foundSelectedWorker {
		t.Fatalf("checks missing selected worker provider compatibility")
	}
}

func TestRunWarnsForAmbiguousProjectRegistryIdentity(t *testing.T) {
	env := healthyDoctorEnv()
	env.projectShow = func(_ context.Context, opts registry.Options) (registry.ShowResult, error) {
		return registry.ShowResult{
			Project: registry.Project{
				ProjectID:      "proj_current",
				LocalPath:      opts.RepoPath,
				IdentitySource: registry.IdentityGitHub,
			},
			Conflicts: []registry.Project{{
				ProjectID:      "proj_conflict",
				IdentitySource: registry.IdentityGitRemote,
			}},
		}, nil
	}

	report := Run(context.Background(), Options{RepoPath: "/repo"}, env.deps())

	check := requireCheck(t, report, "project registry")
	if check.Status != StatusWarn {
		t.Fatalf("status = %s, want warn (%s)", check.Status, check.Message)
	}
	for _, want := range []string{"ambiguous", "proj_conflict", "loopcoder projects show --repo ."} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("message = %q, want containing %q", check.Message, want)
		}
	}
}

func TestRunWarnsForDuplicatePhysicalProjectIdentities(t *testing.T) {
	env := healthyDoctorEnv()
	env.projectDupes = func(context.Context, registry.Options) ([]registry.DuplicatePhysicalIdentity, error) {
		return []registry.DuplicatePhysicalIdentity{{
			Canonical: "/repo",
			Projects: []registry.Project{
				{ProjectID: "proj_one"},
				{ProjectID: "proj_two"},
			},
		}}, nil
	}

	report := Run(context.Background(), Options{RepoPath: "/repo"}, env.deps())

	check := requireCheck(t, report, "project registry")
	if check.Status != StatusWarn {
		t.Fatalf("status = %s, want warn (%s)", check.Status, check.Message)
	}
	for _, want := range []string{"duplicate physical project identity", "loopcoder doctor --repo . --fix"} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("message = %q, want containing %q", check.Message, want)
		}
	}
	if report.Runtime.ProjectRegistry.Status != StatusWarn || report.Runtime.ProjectRegistry.ConflictCount != 1 {
		t.Fatalf("runtime project registry = %#v, want duplicate warning", report.Runtime.ProjectRegistry)
	}
}

func TestRunFixRepairsDuplicatePhysicalProjectIdentities(t *testing.T) {
	env := healthyDoctorEnv()
	called := false
	env.projectRepair = func(context.Context, registry.Options) ([]registry.DuplicatePhysicalIdentity, error) {
		called = true
		return []registry.DuplicatePhysicalIdentity{{Canonical: "/repo"}}, nil
	}

	report := Run(context.Background(), Options{RepoPath: "/repo", Fix: true}, env.deps())

	if !called {
		t.Fatal("ProjectRepair was not called")
	}
	check := requireCheck(t, report, "fix project registry duplicates")
	if check.Status != StatusOK || !strings.Contains(check.Message, "reconciled 1") {
		t.Fatalf("repair check = %#v, want reconciled OK", check)
	}
}

func TestRenderJSONIncludesProjectRegistryCheck(t *testing.T) {
	report := WithMetadata(Report{Checks: []Check{
		{Name: "project registry", Status: StatusWarn, Message: "project identity is ambiguous"},
	}}, "/repo", BuildInfo{})

	var out bytes.Buffer
	if err := RenderJSON(&out, report); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	var payload struct {
		Checks []struct {
			Name    string `json:"name"`
			Status  Status `json:"status"`
			Message string `json:"message"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal: %v\n%s", err, out.String())
	}
	if len(payload.Checks) != 1 || payload.Checks[0].Name != "project registry" || payload.Checks[0].Status != StatusWarn {
		t.Fatalf("payload checks = %#v", payload.Checks)
	}
}

func TestRunChecksConductorHookSettings(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(*fakeDoctorEnv)
		want     Status
		contains []string
	}{
		{
			name: "present",
			want: StatusOK,
			contains: []string{
				"include loopcoder conductor hooks",
			},
		},
		{
			name: "missing settings file",
			setup: func(env *fakeDoctorEnv) {
				env.settingsFile = nil
			},
			want: StatusWarn,
			contains: []string{
				"active Claude Code settings not found",
				"conductor-reporter",
				"conductor-relay-guard",
				"run: loopcoder skill install",
			},
		},
		{
			name: "missing relay hook",
			setup: func(env *fakeDoctorEnv) {
				settings, changed, err := claudehooks.MergeSettings(nil)
				if err != nil || !changed {
					t.Fatalf("MergeSettings returned changed=%v err=%v", changed, err)
				}
				env.settingsFile = bytes.ReplaceAll(settings, []byte(`loopcoder hook conductor-relay-guard`), []byte(`loopcoder hook other`))
			},
			want: StatusWarn,
			contains: []string{
				"missing loopcoder conductor hooks",
				"conductor-relay-guard",
				"PostToolUse",
				"Stop",
				"run: loopcoder skill install",
			},
		},
		{
			name: "old Bash-only PostToolUse matcher",
			setup: func(env *fakeDoctorEnv) {
				env.settingsFile = []byte(`{
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "loopcoder hook conductor-attest",
            "timeout": 10
          },
          {
            "type": "command",
            "command": "loopcoder hook conductor-relay-guard",
            "timeout": 10
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "loopcoder hook conductor-attest",
            "timeout": 10
          },
          {
            "type": "command",
            "command": "loopcoder hook conductor-relay-guard",
            "timeout": 10
          }
        ]
      }
    ]
  }
}`)
			},
			want: StatusWarn,
			contains: []string{
				"missing loopcoder conductor hooks",
				"matcher=Bash|PowerShell|pwsh",
				"run: loopcoder skill install",
			},
		},
		{
			name: "hooks present but loopcoder missing from PATH",
			setup: func(env *fakeDoctorEnv) {
				delete(env.paths, "loopcoder")
			},
			want: StatusWarn,
			contains: []string{
				"loopcoder binary is not on PATH",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := healthyDoctorEnv()
			if tt.setup != nil {
				tt.setup(env)
			}

			report := Run(context.Background(), Options{RepoPath: "/repo"}, env.deps())

			check := requireCheck(t, report, "conductor hooks")
			if check.Status != tt.want {
				t.Fatalf("status = %s, want %s (%s)", check.Status, tt.want, check.Message)
			}
			for _, want := range tt.contains {
				if !strings.Contains(check.Message, want) {
					t.Fatalf("message = %q, want containing %q", check.Message, want)
				}
			}
		})
	}
}

func TestRunChecksInstalledSkillState(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(*fakeDoctorEnv)
		want     Status
		contains []string
	}{
		{
			name: "current",
			want: StatusOK,
			contains: []string{
				"match selected binary embedded content",
			},
		},
		{
			name: "stale",
			setup: func(env *fakeDoctorEnv) {
				env.skillFiles[doctorSkillPath(env.userHome, "SKILL.md")] = []byte("old skill\n")
			},
			want: StatusWarn,
			contains: []string{
				"stale or partial",
				"stale SKILL.md",
				"run: loopcoder skill install",
			},
		},
		{
			name: "absent",
			setup: func(env *fakeDoctorEnv) {
				env.skillFiles = map[string][]byte{}
			},
			want: StatusInfo,
			contains: []string{
				"not installed",
				"run: loopcoder skill install",
			},
		},
		{
			name: "partial",
			setup: func(env *fakeDoctorEnv) {
				delete(env.skillFiles, doctorSkillPath(env.userHome, "AGENTS.md"))
			},
			want: StatusWarn,
			contains: []string{
				"stale or partial",
				"missing AGENTS.md",
				"run: loopcoder skill install",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := healthyDoctorEnv()
			if tt.setup != nil {
				tt.setup(env)
			}

			report := Run(context.Background(), Options{RepoPath: "/repo"}, env.deps())

			check := requireCheck(t, report, "loopcoder skill")
			if check.Status != tt.want {
				t.Fatalf("status = %s, want %s (%s)", check.Status, tt.want, check.Message)
			}
			for _, want := range tt.contains {
				if !strings.Contains(check.Message, want) {
					t.Fatalf("message = %q, want containing %q", check.Message, want)
				}
			}
		})
	}
}

func TestRunHardFailsWhenGitOrGHMissing(t *testing.T) {
	env := healthyDoctorEnv()
	delete(env.paths, "git")
	delete(env.paths, "gh")

	report := Run(context.Background(), Options{RepoPath: "/repo"}, env.deps())

	if got := report.ExitCode(); got != 1 {
		t.Fatalf("ExitCode = %d, want 1", got)
	}
	for _, name := range []string{"git", "gh"} {
		check := requireCheck(t, report, name)
		if check.Status != StatusFail || !check.Hard {
			t.Fatalf("%s = %#v, want hard fail", name, check)
		}
	}
}

func TestRunWarnsWhenProviderCLIMissing(t *testing.T) {
	env := healthyDoctorEnv()
	delete(env.paths, "codex")

	report := Run(context.Background(), Options{RepoPath: "/repo"}, env.deps())

	if got := report.ExitCode(); got != 0 {
		t.Fatalf("ExitCode = %d, want 0", got)
	}
	check := requireCheck(t, report, "provider codex")
	if check.Status != StatusWarn {
		t.Fatalf("provider codex status = %s, want warn", check.Status)
	}
	if !strings.Contains(check.Message, "not found through bounded provider inventory probes") {
		t.Fatalf("provider codex message = %q", check.Message)
	}
}

func TestRunWarnsForInvalidModelSelectionByDefault(t *testing.T) {
	env := healthyDoctorEnv()
	env.cfg.Worker.Model = "custom-worker-model"
	env.cfg.Worker.ReasoningEffort = "custom-depth"

	report := Run(context.Background(), Options{RepoPath: "/repo"}, env.deps())

	if got := report.ExitCode(); got != 0 {
		t.Fatalf("ExitCode = %d, want 0", got)
	}
	check := requireCheck(t, report, "model selection")
	if check.Status != StatusWarn || check.Hard {
		t.Fatalf("model selection check = %#v, want soft warning", check)
	}
	for _, want := range []string{`worker model selection`, `provider "codex"`, `model "custom-worker-model"`, `not listed`} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("model selection message = %q, want containing %q", check.Message, want)
		}
	}
}

func TestRunFailsForInvalidModelSelectionInStrictMode(t *testing.T) {
	env := healthyDoctorEnv()
	env.cfg.Models.Strict = true
	env.cfg.Worker.Model = "custom-worker-model"

	report := Run(context.Background(), Options{RepoPath: "/repo"}, env.deps())

	if got := report.ExitCode(); got != 1 {
		t.Fatalf("ExitCode = %d, want 1", got)
	}
	check := requireCheck(t, report, "model selection")
	if check.Status != StatusFail || !check.Hard {
		t.Fatalf("model selection check = %#v, want hard fail", check)
	}
	if !strings.Contains(check.Message, "reject") || !strings.Contains(check.Message, `model "custom-worker-model"`) {
		t.Fatalf("model selection message = %q, want strict rejection", check.Message)
	}
}

func TestRunChecksAntigravityProviderInstallOnly(t *testing.T) {
	env := healthyDoctorEnv()
	env.cfg.Adapters.Worker = "antigravity"
	env.paths["agy"] = "/bin/agy"

	report := Run(context.Background(), Options{RepoPath: "/repo"}, env.deps())

	if got := report.ExitCode(); got != 0 {
		t.Fatalf("ExitCode = %d, want 0", got)
	}
	check := requireCheck(t, report, "provider antigravity")
	if check.Status != StatusOK {
		t.Fatalf("provider antigravity status = %s, want ok (%s)", check.Status, check.Message)
	}
	for _, want := range []string{`CLI "agy" discovered`, "usable_for_invocation=unknown", "auth_readiness=unknown evidence=not-run", "model authorization, quota, and invocation approval remain separate"} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("provider antigravity message = %q, want containing %q", check.Message, want)
		}
	}
}

func TestQuotaTelemetryHumanLineNamesConflictSetIDs(t *testing.T) {
	check := checkQuotaTelemetry(providerinventory.Report{
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			QuotaSourceID:  "qsrc_fixture",
			AdapterID:      "codex",
			ScopeKey:       "provider:codex/account:acct_fixture",
			Unit:           "request",
			ResetSemantics: providerinventory.ResetUnknown,
			Confidence:     providerinventory.ConfidenceUnknown,
			FreshnessState: providerinventory.FreshnessFresh,
			ConflictSet:    []string{"qsnap_conflict_a", "qsnap_conflict_b"},
			GapReasons:     []string{"provider-disagreement"},
		}},
	})
	if check.Status != StatusOK || check.Code != "quota_telemetry_honest" {
		t.Fatalf("quota telemetry check = %#v, want ok honest", check)
	}
	for _, want := range []string{"conflict_set=qsnap_conflict_a,qsnap_conflict_b", "provider-disagreement"} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("quota telemetry message = %q, want containing %q", check.Message, want)
		}
	}
}

func TestRunFailsAntigravityProviderWhenAgyMissing(t *testing.T) {
	env := healthyDoctorEnv()
	env.cfg.Adapters.Worker = "antigravity"

	report := Run(context.Background(), Options{RepoPath: "/repo"}, env.deps())

	if got := report.ExitCode(); got != 1 {
		t.Fatalf("ExitCode = %d, want 1", got)
	}
	check := requireCheck(t, report, "provider antigravity")
	if check.Status != StatusFail || !check.Hard {
		t.Fatalf("provider antigravity check = %#v, want hard fail", check)
	}
	if check.Code != "missing_executable" {
		t.Fatalf("provider antigravity code = %q, want missing_executable", check.Code)
	}
	for _, want := range []string{`CLI "agy" was not found through bounded provider inventory probes`, "install Google Antigravity CLI"} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("provider antigravity message = %q, want containing %q", check.Message, want)
		}
	}
}

func TestRunDoesNotTreatAntigravityInstallAsAuthReadiness(t *testing.T) {
	env := healthyDoctorEnv()
	env.cfg.Adapters.Verifier = "antigravity"
	env.paths["agy"] = "/bin/agy"

	report := Run(context.Background(), Options{RepoPath: "/repo"}, env.deps())

	if got := report.ExitCode(); got != 1 {
		t.Fatalf("ExitCode = %d, want 1 from compatibility hard fail", got)
	}
	check := requireCheck(t, report, "provider antigravity")
	if check.Status != StatusOK || check.Hard {
		t.Fatalf("provider antigravity check = %#v, want install-only ok", check)
	}
	if check.Code != "provider_installed" {
		t.Fatalf("provider antigravity code = %q, want provider_installed", check.Code)
	}
	for _, want := range []string{"usable_for_invocation=unknown", "auth_readiness=unknown evidence=not-run", "model authorization, quota, and invocation approval remain separate"} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("provider antigravity message = %q, want containing %q", check.Message, want)
		}
	}
	compatibility := requireCheck(t, report, "provider compatibility antigravity verifier")
	if compatibility.Status != StatusFail || !compatibility.Hard || compatibility.Code != "unsupported_read_only_mode" {
		t.Fatalf("provider compatibility antigravity verifier = %#v, want hard read-only failure", compatibility)
	}
	auditProvider := requireCheck(t, report, "audit llm provider")
	if auditProvider.Status != StatusOK || !strings.Contains(auditProvider.Message, `CLI "agy" resolves`) {
		t.Fatalf("audit llm provider check = %#v, want agy CLI resolution", auditProvider)
	}
}

func TestRunFailsClosedForUnsupportedVerifierReadOnlyProvider(t *testing.T) {
	env := healthyDoctorEnv()
	env.cfg.Adapters.Verifier = "antigravity"
	env.paths["agy"] = "/bin/agy"
	env.commands[cmdKey("agy", "models")] = CommandResult{
		Stdout: "Gemini 3.1 Pro\n",
	}

	report := Run(context.Background(), Options{RepoPath: "/repo"}, env.deps())

	if got := report.ExitCode(); got != 1 {
		t.Fatalf("ExitCode = %d, want 1", got)
	}
	check := requireCheck(t, report, "provider compatibility antigravity verifier")
	if check.Status != StatusFail || !check.Hard || check.Code != "unsupported_read_only_mode" {
		t.Fatalf("compatibility check = %#v, want hard unsupported read-only fail", check)
	}
	for _, want := range []string{"role=verifier", "support=unsupported", "missing=read-only"} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("message = %q, want containing %q", check.Message, want)
		}
	}
}

func TestRunAuditBaselineAcceptsNormalizedEvidenceWithoutFingerprint(t *testing.T) {
	env := healthyDoctorEnv()
	env.files[filepath.Clean(filepath.Join("/repo", "docs", "security", "audit-baseline.yml"))] = []byte(`
version: 1
waivers:
  - id: normalized-evidence-waiver
    rule: G204
    path: internal/audit/run.go
    normalized_evidence: exec.Command(invocation.Argv[0], invocation.Argv[1:]...)
    original_severity: medium
    justification: Operator-trusted command argv self-audit waiver fixture.
    date_added: 2026-07-05
    review_by: 2099-01-01
`)

	report := Run(context.Background(), Options{RepoPath: "/repo"}, env.deps())

	check := requireCheck(t, report, "audit baseline")
	if check.Status != StatusOK {
		t.Fatalf("audit baseline status = %s, want ok (%s)", check.Status, check.Message)
	}
	if !strings.Contains(check.Message, "1 waiver") {
		t.Fatalf("audit baseline message = %q, want waiver count", check.Message)
	}
}

func TestRunReportsOriginAndDefaultBranchProblemsWithoutHardExit(t *testing.T) {
	env := healthyDoctorEnv()
	env.commands[cmdKey("git", "remote", "get-url", "origin")] = CommandResult{
		Stderr:   "No such remote 'origin'",
		ExitCode: 2,
	}

	report := Run(context.Background(), Options{RepoPath: "/repo"}, env.deps())

	if got := report.ExitCode(); got != 0 {
		t.Fatalf("ExitCode = %d, want 0", got)
	}
	origin := requireCheck(t, report, "repository origin")
	if origin.Status != StatusFail {
		t.Fatalf("origin status = %s, want fail", origin.Status)
	}
	branch := requireCheck(t, report, "default branch")
	if branch.Status != StatusFail || !strings.Contains(branch.Message, "origin remote is missing") {
		t.Fatalf("default branch check = %#v", branch)
	}
}

func TestRunWarnsWhenDeliveryConfigAbsentAndUsesDefaultProviders(t *testing.T) {
	env := healthyDoctorEnv()
	env.configErr = os.ErrNotExist

	report := Run(context.Background(), Options{RepoPath: "/repo"}, env.deps())

	if got := report.ExitCode(); got != 0 {
		t.Fatalf("ExitCode = %d, want 0", got)
	}
	delivery := requireCheck(t, report, ".delivery.yml")
	if delivery.Status != StatusWarn || !strings.Contains(delivery.Message, "defaults apply") {
		t.Fatalf("delivery check = %#v", delivery)
	}
	compatibility := requireCheck(t, report, "version compatibility")
	if compatibility.Status != StatusWarn || !strings.Contains(compatibility.Message, "no min_loopcoder_version") {
		t.Fatalf("compatibility check = %#v", compatibility)
	}
	if requireCheck(t, report, "provider codex").Status != StatusOK {
		t.Fatal("default worker provider codex was not checked")
	}
	if requireCheck(t, report, "provider claude").Status != StatusOK {
		t.Fatal("default verifier provider claude was not checked")
	}
	if requireCheck(t, report, "model selection").Status != StatusOK {
		t.Fatal("default model selection was not checked")
	}
}

func TestRunResolvesHostProfileFromConfig(t *testing.T) {
	env := healthyDoctorEnv()
	env.env = map[string]string{}
	env.cfg.Host.Profile = "codex"

	report := Run(context.Background(), Options{RepoPath: "/repo"}, env.deps())

	check := requireCheck(t, report, "host profile")
	if check.Status != StatusOK {
		t.Fatalf("status = %s, want ok (%s)", check.Status, check.Message)
	}
	if report.HostProfile.Name != "codex-cli" || report.HostProfile.Source != "config" || report.HostProfile.Selector != "host.profile" {
		t.Fatalf("HostProfile = %#v, want codex-cli from config", report.HostProfile)
	}
}

func TestRunWarnsForGenericHostFallback(t *testing.T) {
	env := healthyDoctorEnv()
	env.env = map[string]string{}

	report := Run(context.Background(), Options{RepoPath: "/repo"}, env.deps())

	check := requireCheck(t, report, "host profile")
	if check.Status != StatusWarn {
		t.Fatalf("status = %s, want warn (%s)", check.Status, check.Message)
	}
	if report.HostProfile.Name != "generic-local" || report.HostProfile.Source != "fallback" {
		t.Fatalf("HostProfile = %#v, want generic fallback", report.HostProfile)
	}
}

func TestRunHardFailsUnknownHostEnv(t *testing.T) {
	env := healthyDoctorEnv()
	env.env = map[string]string{"LOOPCODER_HOST": "unknown"}

	report := Run(context.Background(), Options{RepoPath: "/repo"}, env.deps())

	if got := report.ExitCode(); got != 1 {
		t.Fatalf("ExitCode = %d, want 1", got)
	}
	check := requireCheck(t, report, "host profile")
	if check.Status != StatusFail || !check.Hard {
		t.Fatalf("check = %#v, want hard fail", check)
	}
	if report.HostProfile.Source != "error" || report.HostProfile.Selector != "LOOPCODER_HOST" {
		t.Fatalf("HostProfile = %#v, want error from LOOPCODER_HOST", report.HostProfile)
	}
	if !strings.Contains(check.Message, "LOOPCODER_HOST") || !strings.Contains(check.Message, "unknown") {
		t.Fatalf("message = %q, want env and profile", check.Message)
	}
}

func TestRunReportsDeliveryConfigWorkingTreeBaseMismatch(t *testing.T) {
	env := healthyDoctorEnv()
	env.configErr = os.ErrNotExist
	env.commands[cmdKey("git", "show", "main:.delivery.yml")] = CommandResult{
		Stdout: "version: 1\n",
	}

	report := Run(context.Background(), Options{RepoPath: "/repo", BaseBranch: "main"}, env.deps())

	if got := report.ExitCode(); got != 0 {
		t.Fatalf("ExitCode = %d, want 0", got)
	}
	delivery := requireCheck(t, report, ".delivery.yml")
	if delivery.Status != StatusWarn {
		t.Fatalf("delivery status = %s, want warn (%s)", delivery.Status, delivery.Message)
	}
	for _, want := range []string{
		"absent from working tree",
		"present on main",
		"--config-from-base",
	} {
		if !strings.Contains(delivery.Message, want) {
			t.Fatalf("delivery message = %q, want containing %q", delivery.Message, want)
		}
	}
	if strings.Contains(delivery.Message, "documented defaults apply") {
		t.Fatalf("delivery message should supersede defaults message: %q", delivery.Message)
	}
}

func TestRunReportsInvalidDeliveryConfigWithoutHardExit(t *testing.T) {
	env := healthyDoctorEnv()
	env.configErr = errors.New("parse delivery config: broken yaml")

	report := Run(context.Background(), Options{RepoPath: "/repo"}, env.deps())

	if got := report.ExitCode(); got != 0 {
		t.Fatalf("ExitCode = %d, want 0", got)
	}
	delivery := requireCheck(t, report, ".delivery.yml")
	if delivery.Status != StatusFail || !strings.Contains(delivery.Message, "broken yaml") {
		t.Fatalf("delivery check = %#v", delivery)
	}
	compatibility := requireCheck(t, report, "version compatibility")
	if compatibility.Status != StatusFail {
		t.Fatalf("compatibility status = %s, want fail", compatibility.Status)
	}
}

func TestRunChecksVersionCompatibility(t *testing.T) {
	tests := []struct {
		name     string
		build    string
		file     string
		want     Status
		contains string
	}{
		{
			name:     "minimum satisfied",
			build:    "0.3.1",
			file:     "version: 1\nmin_loopcoder_version: 0.3.0\n",
			want:     StatusOK,
			contains: "satisfied",
		},
		{
			name:     "selected binary too old",
			build:    "0.2.9",
			file:     "version: 1\nmin_loopcoder_version: 0.3.0\n",
			want:     StatusFail,
			contains: "older than min_loopcoder_version=0.3.0",
		},
		{
			name:     "dev build cannot be compared",
			build:    "dev",
			file:     "version: 1\nmin_loopcoder_version: 0.3.0\n",
			want:     StatusWarn,
			contains: "cannot be compared",
		},
		{
			name:     "invalid minimum",
			build:    "0.3.1",
			file:     "version: 1\nmin_loopcoder_version: nope\n",
			want:     StatusFail,
			contains: "not a valid semantic version",
		},
		{
			name:     "unsupported schema",
			build:    "0.3.1",
			file:     "version: 2\nmin_loopcoder_version: 0.3.0\n",
			want:     StatusFail,
			contains: "schema version=2 is unsupported",
		},
		{
			name:     "missing minimum remains ok",
			build:    "0.3.1",
			file:     "version: 1\n",
			want:     StatusOK,
			contains: "no min_loopcoder_version declared",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := healthyDoctorEnv()
			env.file = []byte(tt.file)
			report := Run(context.Background(), Options{
				RepoPath:  "/repo",
				BuildInfo: BuildInfo{Version: tt.build},
			}, env.deps())

			if got := report.ExitCode(); got != 0 {
				t.Fatalf("ExitCode = %d, want 0", got)
			}
			check := requireCheck(t, report, "version compatibility")
			if check.Status != tt.want {
				t.Fatalf("status = %s, want %s (%s)", check.Status, tt.want, check.Message)
			}
			if !strings.Contains(check.Message, tt.contains) {
				t.Fatalf("message = %q, want containing %q", check.Message, tt.contains)
			}
		})
	}
}

func TestCheckMigrationStatusWarnsForLegacySurfaces(t *testing.T) {
	repo := t.TempDir()
	writeDoctorTextFile(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeDoctorTextFile(t, filepath.Join(repo, ".delivery.yml"), "version: 1\n"+migration.LegacyReportConfigRoot+":\n  channel: chat\n")

	check := checkMigrationStatus(repo, Deps{
		Getenv: func(key string) string {
			if key == migration.LegacyReporterScopeEnv {
				return "auto"
			}
			return ""
		},
		ReadFile: os.ReadFile,
	})

	if check.Status != StatusWarn {
		t.Fatalf("status = %s, want warn (%s)", check.Status, check.Message)
	}
	for _, want := range []string{"legacy surface(s)", "per-surface remediation"} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("message = %q, want containing %q", check.Message, want)
		}
	}
	if len(check.LegacySurfaces) != 3 {
		t.Fatalf("LegacySurfaces = %#v, want env plus two config surfaces", check.LegacySurfaces)
	}
	assertLegacySurface(t, check.LegacySurfaces, migration.LegacyReporterScopeEnv, "manual", "set LOOPCODER_CONDUCTOR_REPORTER_SCOPE")
	assertLegacySurface(t, check.LegacySurfaces, `.delivery.yml key "`+migration.LegacyReportConfigRoot+`"`, "fix-with-flag", "loopcoder doctor --repo . --fix")
}

func TestCheckMigrationStatusEnumeratesHookStateAndStateKeySurfaces(t *testing.T) {
	repo := t.TempDir()
	writeDoctorTextFile(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeDoctorTextFile(t, filepath.Join(repo, ".delivery.yml"), "version: 1\n")
	writeDoctorTextFile(t, filepath.Join(repo, ".loopcoder", "hooks", migration.LegacyReporterHookName, "session-a.json"), `{"ok":true}`)
	statePath := filepath.Join(repo, ".loopcoder", "runs", "run-b", "workers", "job.attempt.json")
	writeDoctorTextFile(t, statePath, fmt.Sprintf(`{"status":"succeeded","%s":{"role":"worker"}}`, migration.LegacyReportStateKey))

	check := checkMigrationStatus(repo, Deps{
		Getenv:   func(string) string { return "" },
		ReadFile: os.ReadFile,
	})

	if check.Status != StatusWarn {
		t.Fatalf("status = %s, want warn (%s)", check.Status, check.Message)
	}
	if len(check.LegacySurfaces) != 2 {
		t.Fatalf("LegacySurfaces = %#v, want hook-state and state-key", check.LegacySurfaces)
	}
	assertLegacySurface(t, check.LegacySurfaces, filepath.Join(repo, ".loopcoder", "hooks", migration.LegacyReporterHookName), "fix-with-flag", "loopcoder doctor --repo . --fix")
	assertLegacySurface(t, check.LegacySurfaces, statePath, "fix-with-flag", "loopcoder doctor --repo . --fix")
}

func TestCheckMigrationStatusAfterFixDoesNotPointAtDoctorFix(t *testing.T) {
	repo := t.TempDir()
	writeDoctorTextFile(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeDoctorTextFile(t, filepath.Join(repo, ".delivery.yml"), "version: 1\n")

	check := checkMigrationStatusWithMode(repo, Deps{
		Getenv: func(key string) string {
			if key == migration.LegacyReporterScopeEnv {
				return "auto"
			}
			return ""
		},
		ReadFile: os.ReadFile,
	}, true)

	if check.Status != StatusWarn {
		t.Fatalf("status = %s, want warn (%s)", check.Status, check.Message)
	}
	if len(check.LegacySurfaces) != 1 {
		t.Fatalf("LegacySurfaces = %#v, want env surface", check.LegacySurfaces)
	}
	if strings.Contains(check.LegacySurfaces[0].Remediation, "loopcoder doctor --repo . --fix") {
		t.Fatalf("after-fix remediation points at doctor --fix: %#v", check.LegacySurfaces[0])
	}
	assertLegacySurface(t, check.LegacySurfaces, migration.LegacyReporterScopeEnv, "manual", "unset LOOPCODER_CONDUCTOR_ATTEST_SCOPE")
}

func TestCheckStorageHealthReportsHealthyDatabase(t *testing.T) {
	homeDir := t.TempDir()
	dbPath := filepath.Join(homeDir, "data", "loopcoder.db")

	check := checkStorageHealth(context.Background(), Deps{
		Getenv:      func(string) string { return homeDir },
		UserHomeDir: func() (string, error) { return "unused", nil },
		StorageHealth: func(_ context.Context, path string) (storage.Health, error) {
			if path != dbPath {
				t.Fatalf("storage path = %q, want %q", path, dbPath)
			}
			return storage.Health{
				Path:          path,
				Exists:        true,
				SchemaVersion: storage.CurrentSchemaVersion,
				OK:            true,
				Message:       "healthy",
			}, nil
		},
	})

	if check.Status != StatusOK {
		t.Fatalf("status = %s, want ok (%s)", check.Status, check.Message)
	}
	for _, want := range []string{"loopcoder.db", fmt.Sprintf("schema_version=%d", storage.CurrentSchemaVersion), "health=ok"} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("message = %q, want containing %q", check.Message, want)
		}
	}
}

func TestCheckStoragePermissionsReportsInsecurePath(t *testing.T) {
	homeDir := t.TempDir()
	dbPath := filepath.Join(homeDir, "data", "loopcoder.db")

	check := checkStoragePermissions(Deps{
		Getenv:      func(string) string { return homeDir },
		UserHomeDir: func() (string, error) { return "unused", nil },
		StoragePermissions: func(path string, fix bool) (storage.PermissionReport, error) {
			if path != dbPath {
				t.Fatalf("storage path = %q, want %q", path, dbPath)
			}
			if fix {
				t.Fatal("read-only check requested repair")
			}
			return storage.PermissionReport{
				Path:      path,
				Platform:  "linux",
				Supported: true,
				Secure:    false,
				Items: []storage.PermissionItem{{
					Path:       path,
					Kind:       "database file",
					Exists:     true,
					BeforeMode: 0o644,
					AfterMode:  0o644,
					Message:    "mode 0644 is broader than 0600",
				}},
			}, nil
		},
	})

	if check.Status != StatusWarn {
		t.Fatalf("status = %s, want warn (%s)", check.Status, check.Message)
	}
	if check.FixCommand != "loopcoder doctor --repo . --fix" {
		t.Fatalf("FixCommand = %q", check.FixCommand)
	}
	for _, want := range []string{"permissions=insecure", "loopcoder.db", "0644"} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("message = %q, want containing %q", check.Message, want)
		}
	}
}

func TestFixStoragePermissionsReportsBeforeAfter(t *testing.T) {
	homeDir := t.TempDir()
	dbPath := filepath.Join(homeDir, "data", "loopcoder.db")
	calls := 0

	check := fixStoragePermissions(Deps{
		Getenv:      func(string) string { return homeDir },
		UserHomeDir: func() (string, error) { return "unused", nil },
		StoragePermissions: func(path string, fix bool) (storage.PermissionReport, error) {
			if path != dbPath {
				t.Fatalf("storage path = %q, want %q", path, dbPath)
			}
			calls++
			if !fix {
				return storage.PermissionReport{
					Path:      path,
					Platform:  "linux",
					Supported: true,
					Secure:    false,
					Items: []storage.PermissionItem{{
						Path:       path,
						Kind:       "database file",
						Exists:     true,
						BeforeMode: 0o644,
						AfterMode:  0o644,
						Message:    "mode 0644 is broader than 0600",
					}},
				}, nil
			}
			return storage.PermissionReport{
				Path:      path,
				Platform:  "linux",
				Supported: true,
				Secure:    true,
				Repaired:  true,
				Items: []storage.PermissionItem{{
					Path:       path,
					Kind:       "database file",
					Exists:     true,
					BeforeMode: 0o644,
					AfterMode:  0o600,
					Secure:     true,
					Repaired:   true,
					Message:    "tightened from 0644 to 0600",
				}},
			}, nil
		},
	})

	if calls != 2 {
		t.Fatalf("permission calls = %d, want 2", calls)
	}
	if check.Status != StatusOK {
		t.Fatalf("status = %s, want ok (%s)", check.Status, check.Message)
	}
	for _, want := range []string{"changed", "0644->0600", "loopcoder.db"} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("message = %q, want containing %q", check.Message, want)
		}
	}
}

func TestCheckStorageHealthReportsMissingDatabaseAsInfo(t *testing.T) {
	homeDir := t.TempDir()

	check := checkStorageHealth(context.Background(), Deps{
		Getenv:      func(string) string { return homeDir },
		UserHomeDir: func() (string, error) { return "unused", nil },
		StorageHealth: func(_ context.Context, path string) (storage.Health, error) {
			return storage.Health{Path: path, Message: "database has not been created"}, nil
		},
	})

	if check.Status != StatusInfo {
		t.Fatalf("status = %s, want info (%s)", check.Status, check.Message)
	}
	for _, want := range []string{"schema_version=0", "health=not-created"} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("message = %q, want containing %q", check.Message, want)
		}
	}
}

func TestCheckStorageHealthFailsUnsupportedDatabase(t *testing.T) {
	homeDir := t.TempDir()

	check := checkStorageHealth(context.Background(), Deps{
		Getenv:      func(string) string { return homeDir },
		UserHomeDir: func() (string, error) { return "unused", nil },
		StorageHealth: func(_ context.Context, path string) (storage.Health, error) {
			return storage.Health{Path: path, Exists: true, SchemaVersion: 999}, errors.New("unsupported storage schema version 999")
		},
	})

	if check.Status != StatusFail {
		t.Fatalf("status = %s, want fail (%s)", check.Status, check.Message)
	}
	for _, want := range []string{"health=fail", "unsupported storage schema version 999"} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("message = %q, want containing %q", check.Message, want)
		}
	}
}

func TestCheckNestedRunHealthWarnsForMissingParent(t *testing.T) {
	repo := t.TempDir()
	child := state.RunIDForChild("docs-pass", 0, time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC))
	if err := state.AppendLifecycleTransition(repo, state.LifecycleTransition{
		Timestamp:   "2026-07-09T00:00:00Z",
		RunID:       child,
		ParentRunID: "run-20260709T000000Z-wave",
		State:       state.StateQueued,
	}); err != nil {
		t.Fatalf("AppendLifecycleTransition: %v", err)
	}

	check := checkNestedRunHealth(repo)
	if check.Status != StatusWarn {
		t.Fatalf("status = %s, want warn (%s)", check.Status, check.Message)
	}
	for _, want := range []string{"missing parent", "loopcoder status --repo . --format json"} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("message = %q, want containing %q", check.Message, want)
		}
	}
}

func TestCheckNestedRunHealthCountsEventEdges(t *testing.T) {
	repo := t.TempDir()
	parent := state.RunIDForWave(time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC))
	child := state.RunIDForChild("docs-pass", 0, time.Date(2026, 7, 9, 0, 0, 1, 0, time.UTC))
	if err := state.AppendEvent(repo, parent, state.Event{
		Timestamp: "2026-07-09T00:00:00Z",
		RunID:     parent,
		JobID:     "nested-scheduler",
		Issue:     690,
		Phase:     "nested-scheduler",
		Status:    state.StatusSucceeded,
		Event:     "nested.child.finished",
		Outcome:   state.StatusSucceeded,
		Details:   json.RawMessage(fmt.Sprintf(`{"parent_run_id":%q,"child":{"run_id":%q},"result":{"run_id":%q}}`, parent, child, child)),
	}); err != nil {
		t.Fatalf("AppendEvent parent: %v", err)
	}
	if err := state.AppendEvent(repo, child, state.Event{
		Timestamp: "2026-07-09T00:00:01Z",
		RunID:     child,
		JobID:     "nested-scheduler",
		Issue:     690,
		Phase:     "nested-scheduler",
		Status:    state.StatusSucceeded,
		Event:     "nested.child.finished",
		Outcome:   state.StatusSucceeded,
		Details:   json.RawMessage(fmt.Sprintf(`{"parent_run_id":%q,"child":{"run_id":%q},"result":{"run_id":%q}}`, parent, child, child)),
	}); err != nil {
		t.Fatalf("AppendEvent child: %v", err)
	}

	health := runtimeNestedRuns(repo)
	if health.Status != StatusOK || health.ParentEdges != 1 || health.ChildEdges != 1 || health.ProblemCount != 0 {
		t.Fatalf("nested health = %#v", health)
	}
}

func TestCheckMigrationStatusUsesInjectedEnv(t *testing.T) {
	t.Setenv(migration.LegacyReporterScopeEnv, "auto")
	repo := t.TempDir()
	writeDoctorTextFile(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeDoctorTextFile(t, filepath.Join(repo, ".delivery.yml"), "version: 1\n")

	check := checkMigrationStatus(repo, Deps{
		Getenv:   func(string) string { return "" },
		ReadFile: os.ReadFile,
	})

	if check.Status != StatusOK {
		t.Fatalf("status = %s, want ok when injected env is empty (%s)", check.Status, check.Message)
	}
}

func TestCheckLocalStateImportIgnoresAuditOnlyState(t *testing.T) {
	repo := t.TempDir()
	writeDoctorTextFile(t, filepath.Join(repo, ".loopcoder", "audit", "audit.jsonl"), `{"ok":true}`+"\n")

	check := checkLocalStateImport(context.Background(), repo, Deps{})

	if check.Status != StatusOK {
		t.Fatalf("status = %s, want ok (%s)", check.Status, check.Message)
	}
	if !strings.Contains(check.Message, "no repo-local .loopcoder history found") {
		t.Fatalf("message = %q, want no history message", check.Message)
	}
}

func TestCheckVersionStatusWarnsBeforeBreakingBoundary(t *testing.T) {
	repo := t.TempDir()
	writeDoctorTextFile(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeDoctorTextFile(t, filepath.Join(repo, ".delivery.yml"), "version: 1\nmin_loopcoder_version: 0.5.0\n")

	check := checkVersionStatus(BuildInfo{Version: "0.5.4"}, repo, Deps{
		Getenv:   func(string) string { return "" },
		ReadFile: os.ReadFile,
	})

	if check.Status != StatusWarn {
		t.Fatalf("status = %s, want warn (%s)", check.Status, check.Message)
	}
	for _, want := range []string{"pre-breaking", "breaking transition", "run: loopcoder upgrade --version 0.6.0"} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("message = %q, want containing %q", check.Message, want)
		}
	}
}

func TestCheckVersionStatusPointsLegacySurfacesAtMigrationStatus(t *testing.T) {
	repo := t.TempDir()
	writeDoctorTextFile(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeDoctorTextFile(t, filepath.Join(repo, ".delivery.yml"), "version: 1\n"+migration.LegacyReportConfigRoot+":\n  channel: chat\n")

	check := checkVersionStatus(BuildInfo{Version: "0.7.0"}, repo, Deps{
		Getenv:   func(string) string { return "" },
		ReadFile: os.ReadFile,
	})

	if check.Status != StatusWarn {
		t.Fatalf("status = %s, want warn (%s)", check.Status, check.Message)
	}
	if strings.Contains(check.Message, "loopcoder doctor --repo . --fix") {
		t.Fatalf("version status points at doctor --fix: %q", check.Message)
	}
	if !strings.Contains(check.Message, "see migration status check") {
		t.Fatalf("message = %q, want migration status pointer", check.Message)
	}
}

func TestCheckStaleStateWarnsForCleanupEligibleItems(t *testing.T) {
	check := checkStaleState("/repo", Deps{
		CleanupPlan: func(opts localcleanup.Options) (localcleanup.Result, error) {
			if opts.RepoPath != "/repo" {
				t.Fatalf("RepoPath = %q, want /repo", opts.RepoPath)
			}
			return localcleanup.Result{
				Planned: []localcleanup.Action{{
					Kind:   localcleanup.KindRun,
					Path:   filepath.Join("/repo", ".loopcoder", "runs", "old"),
					Reason: "outside retention",
				}},
			}, nil
		},
	})

	if check.Status != StatusWarn {
		t.Fatalf("status = %s, want warn (%s)", check.Status, check.Message)
	}
	for _, want := range []string{"cleanup-eligible item(s)", "run: loopcoder doctor --repo . --fix"} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("message = %q, want containing %q", check.Message, want)
		}
	}
}

func TestCheckStaleStateWarnsWhenCleanupPlanErrors(t *testing.T) {
	check := checkStaleState("/repo", Deps{
		CleanupPlan: func(localcleanup.Options) (localcleanup.Result, error) {
			return localcleanup.Result{}, errors.New("permission denied")
		},
	})

	if check.Status != StatusWarn {
		t.Fatalf("status = %s, want warn (%s)", check.Status, check.Message)
	}
	for _, want := range []string{"could not scan local state", "permission denied", "rerun: loopcoder doctor --repo ."} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("message = %q, want containing %q", check.Message, want)
		}
	}
}

func TestFixDeliveryConfigMigratesLegacyReportKeys(t *testing.T) {
	repo := t.TempDir()
	path := filepath.Join(repo, ".delivery.yml")
	writeDoctorTextFile(t, path, strings.Join([]string{
		"version: 1",
		migration.LegacyReportConfigRoot + ":",
		"  channel: chat",
		"",
	}, "\n"))

	check := fixDeliveryConfig(repo, Deps{ReadFile: os.ReadFile})
	if check.Status != StatusOK || !strings.Contains(check.Message, "changed") {
		t.Fatalf("check = %#v, want changed ok", check)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migrated config: %v", err)
	}
	text := string(data)
	if strings.Contains(text, migration.LegacyReportConfigRoot+":") {
		t.Fatalf("migrated config still contains legacy root:\n%s", text)
	}
	if !strings.Contains(text, migration.ReportConfigRoot+":") {
		t.Fatalf("migrated config missing report root:\n%s", text)
	}

	second := fixDeliveryConfig(repo, Deps{ReadFile: os.ReadFile})
	if second.Status != StatusOK || !strings.Contains(second.Message, "unchanged") {
		t.Fatalf("second check = %#v, want unchanged ok", second)
	}
}

func TestFixConductorHookSettingsMigratesLegacyCommand(t *testing.T) {
	repo := t.TempDir()
	settingsPath := claudehooks.SettingsPath(repo)
	writeDoctorTextFile(t, settingsPath, fmt.Sprintf(`{
  "hooks": {
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": %q,
            "timeout": 10
          }
        ]
      }
    ]
  }
}
`, migration.LegacyReporterHookCommand))

	check := fixConductorHookSettings(repo, Deps{ReadFile: os.ReadFile})
	if check.Status != StatusOK || !strings.Contains(check.Message, "changed") {
		t.Fatalf("check = %#v, want changed ok", check)
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	text := string(data)
	if strings.Contains(text, migration.LegacyReporterHookCommand) {
		t.Fatalf("settings still contain legacy hook command:\n%s", text)
	}
	for _, want := range []string{migration.ReporterHookCommand, "loopcoder hook conductor-relay-guard"} {
		if !strings.Contains(text, want) {
			t.Fatalf("settings missing %q:\n%s", want, text)
		}
	}
}

func TestFixConductorHookStateMovesLegacyDirectory(t *testing.T) {
	repo := t.TempDir()
	oldDir := filepath.Join(repo, ".loopcoder", "hooks", migration.LegacyReporterHookName)
	newDir := filepath.Join(repo, ".loopcoder", "hooks", migration.ReporterHookName)
	writeDoctorTextFile(t, filepath.Join(oldDir, "session-a.json"), `{"delivery_seen":true}`)

	check := fixConductorHookState(repo)
	if check.Status != StatusOK || !strings.Contains(check.Message, "changed") {
		t.Fatalf("check = %#v, want changed ok", check)
	}
	if _, err := os.Stat(filepath.Join(newDir, "session-a.json")); err != nil {
		t.Fatalf("new state file missing: %v", err)
	}
	if _, err := os.Stat(oldDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy state dir still exists or unexpected error: %v", err)
	}
}

func TestFixLegacyStateKeysRewritesLocalJSON(t *testing.T) {
	repo := t.TempDir()
	statePath := filepath.Join(repo, ".loopcoder", "runs", "run-1", "workers", "job.attempt.json")
	writeDoctorTextFile(t, statePath, fmt.Sprintf(`{"status":"succeeded","%s":{"role":"worker"}}`, migration.LegacyReportStateKey))

	check := fixLegacyStateKeys(repo)
	if check.Status != StatusOK || !strings.Contains(check.Message, "changed") {
		t.Fatalf("check = %#v, want changed ok", check)
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	text := string(data)
	if strings.Contains(text, `"`+migration.LegacyReportStateKey+`"`) {
		t.Fatalf("state still contains legacy key:\n%s", text)
	}
	if !strings.Contains(text, `"`+migration.ReportStateKey+`"`) {
		t.Fatalf("state missing current key:\n%s", text)
	}
}

func TestFixLegacyStateKeysKeepsJSONLOneRecordPerLine(t *testing.T) {
	repo := t.TempDir()
	statePath := filepath.Join(repo, ".loopcoder", "runs", "run-1", "events.jsonl")
	writeDoctorTextFile(t, statePath, strings.Join([]string{
		fmt.Sprintf(`{"status":"succeeded","%s":{"role":"worker"}}`, migration.LegacyReportStateKey),
		`{"status":"ordinary","detail":"unchanged"}`,
		"",
	}, "\n"))

	check := fixLegacyStateKeys(repo)
	if check.Status != StatusOK || !strings.Contains(check.Message, "changed") {
		t.Fatalf("check = %#v, want changed ok", check)
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	text := string(data)
	if strings.Contains(text, `"`+migration.LegacyReportStateKey+`"`) {
		t.Fatalf("state still contains legacy key:\n%s", text)
	}

	nonEmptyLines := 0
	foundCurrentKey := false
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		nonEmptyLines++
		var object map[string]any
		if err := json.Unmarshal([]byte(line), &object); err != nil {
			t.Fatalf("line is not a complete JSON object: %q: %v", line, err)
		}
		if _, ok := object[migration.ReportStateKey]; ok {
			foundCurrentKey = true
		}
	}
	if nonEmptyLines != 2 {
		t.Fatalf("non-empty JSONL lines = %d, want 2; text:\n%s", nonEmptyLines, text)
	}
	if !foundCurrentKey {
		t.Fatalf("state missing current key:\n%s", text)
	}
}

func TestRenderPrintsOneMarkedLinePerCheck(t *testing.T) {
	report := Report{Checks: []Check{
		{Name: "git", Status: StatusOK, Message: "found"},
		{Name: "gh", Status: StatusFail, Message: "missing"},
	}}
	var output bytes.Buffer

	if err := Render(&output, report); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	want := "[ok] git: found\n[fail] gh: missing\n"
	if output.String() != want {
		t.Fatalf("output = %q, want %q", output.String(), want)
	}
}

func TestRenderPrintsLegacySurfaceDetails(t *testing.T) {
	report := Report{Checks: []Check{{
		Name:    "migration status",
		Status:  StatusWarn,
		Message: "found 1 legacy surface(s) requiring migration; see per-surface remediation below",
		LegacySurfaces: []LegacySurface{{
			Surface:        "hook-state",
			Identifier:     ".loopcoder/hooks/conductor-attest",
			Classification: "fix-with-flag",
			Remediation:    "run: loopcoder doctor --repo . --fix",
		}},
	}}}
	var output bytes.Buffer

	if err := Render(&output, report); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	for _, want := range []string{
		"[warn] migration status:",
		"identifier=.loopcoder/hooks/conductor-attest",
		"classification=fix-with-flag",
		"remediation=run: loopcoder doctor --repo . --fix",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("output = %q, want containing %q", output.String(), want)
		}
	}
}

func TestRenderJSONIncludesLegacySurfaceDetails(t *testing.T) {
	report := WithMetadata(Report{
		Runtime: RuntimeHealth{Migration: RuntimeMigration{
			Status:         StatusWarn,
			LegacySurfaces: 1,
			Surfaces: []LegacySurface{{
				Surface:        "state-key",
				Identifier:     filepath.Join(".loopcoder", "runs", "run-1", "workers", "job.attempt.json"),
				Classification: "fix-with-flag",
				Remediation:    "run: loopcoder doctor --repo . --fix",
				Legacy:         migration.LegacyReportStateKey,
				Current:        migration.ReportStateKey,
				Location:       filepath.Join(".loopcoder", "runs", "run-1", "workers", "job.attempt.json"),
			}},
		}},
		Checks: []Check{{
			Name:    "migration status",
			Status:  StatusWarn,
			Message: "found 1 legacy surface(s) requiring migration; see per-surface remediation below",
			LegacySurfaces: []LegacySurface{{
				Surface:        "state-key",
				Identifier:     filepath.Join(".loopcoder", "runs", "run-1", "workers", "job.attempt.json"),
				Classification: "fix-with-flag",
				Remediation:    "run: loopcoder doctor --repo . --fix",
				Legacy:         migration.LegacyReportStateKey,
				Current:        migration.ReportStateKey,
			}},
		}},
	}, "/repo", BuildInfo{Version: "0.7.0"})
	var out bytes.Buffer

	if err := RenderJSON(&out, report); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	var payload struct {
		Runtime struct {
			Migration struct {
				Surfaces []LegacySurface `json:"surfaces"`
			} `json:"migration"`
		} `json:"runtime"`
		Checks []struct {
			LegacySurfaces []LegacySurface `json:"legacy_surfaces"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal: %v\n%s", err, out.String())
	}
	if len(payload.Runtime.Migration.Surfaces) != 1 || len(payload.Checks[0].LegacySurfaces) != 1 {
		t.Fatalf("legacy surfaces missing from JSON: %s", out.String())
	}
	if got := payload.Runtime.Migration.Surfaces[0].Identifier; got != ".loopcoder/runs/run-1/workers/job.attempt.json" {
		t.Fatalf("runtime surface identifier = %q", got)
	}
	if payload.Checks[0].LegacySurfaces[0].Classification != "fix-with-flag" {
		t.Fatalf("check surface = %#v", payload.Checks[0].LegacySurfaces[0])
	}
}

func TestExecRunCommandCapturesOutputAndNonZeroExit(t *testing.T) {
	withTestCommandCap(t, 2*time.Second)

	result, err := execRunCommand(context.Background(), "", os.Args[0], "-test.run=TestDoctorExecHelper", "--", "stdout", "ok")
	if err != nil {
		t.Fatalf("execRunCommand returned error: %v", err)
	}
	if result.ExitCode != 0 || result.Stdout != "ok\n" || result.Stderr != "" {
		t.Fatalf("result = %#v, want stdout ok exit 0", result)
	}

	result, err = execRunCommand(context.Background(), "", os.Args[0], "-test.run=TestDoctorExecHelper", "--", "combined-exit", "stdout", "stderr", "7")
	if err != nil {
		t.Fatalf("execRunCommand returned error for non-zero exit: %v", err)
	}
	if result.ExitCode != 7 || result.Stdout != "stdout\n" || result.Stderr != "stderr\n" {
		t.Fatalf("result = %#v, want captured output and exit 7", result)
	}
}

func TestExecRunCommandTimesOut(t *testing.T) {
	withTestCommandCap(t, 50*time.Millisecond)

	start := time.Now()
	_, err := execRunCommand(context.Background(), "", os.Args[0], "-test.run=TestDoctorExecHelper", "--", "sleep", "500ms")
	if err == nil {
		t.Fatal("execRunCommand error = nil, want timeout")
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("execRunCommand elapsed = %s, want bounded timeout", elapsed)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %q, want timeout", err.Error())
	}
}

func withTestCommandCap(t *testing.T, hardCap time.Duration) {
	t.Helper()
	oldHardCap := commandHardCap
	commandHardCap = hardCap
	t.Setenv("GO_WANT_DOCTOR_HELPER", "1")
	t.Cleanup(func() {
		commandHardCap = oldHardCap
	})
}

func TestDoctorExecHelper(t *testing.T) {
	if os.Getenv("GO_WANT_DOCTOR_HELPER") != "1" {
		return
	}
	runExecHelper()
}

func runExecHelper() {
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		fmt.Fprintln(os.Stderr, "missing helper mode")
		os.Exit(2)
	}
	mode := os.Args[separator+1]
	args := os.Args[separator+2:]
	switch mode {
	case "stdout":
		fmt.Fprintln(os.Stdout, args[0])
	case "combined-exit":
		fmt.Fprintln(os.Stdout, args[0])
		fmt.Fprintln(os.Stderr, args[1])
		os.Exit(parseHelperInt(args[2]))
	case "sleep":
		time.Sleep(parseHelperDuration(args[0]))
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
		os.Exit(2)
	}
	os.Exit(0)
}

func parseHelperDuration(value string) time.Duration {
	duration, err := time.ParseDuration(value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse duration %q: %v\n", value, err)
		os.Exit(2)
	}
	return duration
}

func parseHelperInt(value string) int {
	var n int
	if _, err := fmt.Sscanf(value, "%d", &n); err != nil {
		fmt.Fprintf(os.Stderr, "parse int %q: %v\n", value, err)
		os.Exit(2)
	}
	return n
}

func healthyDoctorEnv() *fakeDoctorEnv {
	return &fakeDoctorEnv{
		paths: map[string]string{
			"git":         "/bin/git",
			"gh":          "/bin/gh",
			"codex":       "/bin/codex",
			"claude":      "/bin/claude",
			"loopcoder":   "/bin/loopcoder",
			"govulncheck": "/bin/govulncheck",
			"staticcheck": "/bin/staticcheck",
			"gosec":       "/bin/gosec",
		},
		env: map[string]string{
			"LOOPCODER_HOST": "claude-code",
		},
		commands: map[string]CommandResult{
			cmdKey("gh", "auth", "status"): {
				Stdout: "Logged in\n",
			},
			cmdKey("git", "remote", "get-url", "origin"): {
				Stdout: "https://github.com/owner/repo.git\n",
			},
			cmdKey("git", "remote", "show", "origin"): {
				Stdout: "* remote origin\n  HEAD branch: trunk\n",
			},
			cmdKey("git", "rev-parse", "--is-inside-work-tree"): {
				Stdout: "true\n",
			},
			cmdKey("git", "rev-parse", "--path-format=absolute", "--git-path", "info/exclude"): {
				Stdout: filepath.Join(".git", "info", "exclude") + "\n",
			},
			cmdKey("git", "ls-files", ".loopcoder"): {},
		},
		cfg: config.Config{
			Version: 1,
			Adapters: config.Adapters{
				Worker:   "codex",
				Verifier: "claude",
			},
			CI: config.CI{
				Checks: []string{"verify", "go", "staticcheck", "govulncheck", "audit"},
			},
			Audit: config.Audit{
				SeverityThreshold: "medium",
				SAST: config.AuditSAST{
					Commands: []config.AuditSASTCommand{
						{ID: "govulncheck", Argv: []string{"govulncheck", "-json", "./..."}, Parser: "govulncheck-json"},
						{ID: "staticcheck", Argv: []string{"staticcheck", "-f", "json", "./..."}, Parser: "staticcheck-json"},
						{ID: "gosec", Argv: []string{"gosec", "-fmt", "json", "-quiet", "./..."}, Parser: "gosec-json"},
					},
				},
				Review: config.AuditReview{
					RubricPath: "docs/security/audit-rubric.md",
				},
				Baseline: config.AuditBaseline{
					Path: "docs/security/audit-baseline.yml",
				},
			},
		},
		file:           []byte("version: 1\nmin_loopcoder_version: 0.3.0\n"),
		settingsFile:   healthyClaudeSettings(),
		executablePath: "/bin/loopcoder",
		userHome:       filepath.Join("home", "user"),
		skillFiles: map[string][]byte{
			doctorSkillPath(filepath.Join("home", "user"), "SKILL.md"):  []byte("skill content\n"),
			doctorSkillPath(filepath.Join("home", "user"), "AGENTS.md"): []byte("agents content\n"),
		},
		files: map[string][]byte{
			filepath.Clean(filepath.Join("/repo", ".github", "workflows", "ci.yml")):         []byte("jobs:\n  audit:\n    runs-on: ubuntu-latest\n"),
			filepath.Clean(filepath.Join("/repo", "docs", "security", "audit-rubric.md")):    []byte("# Audit Rubric\n"),
			filepath.Clean(filepath.Join("/repo", "docs", "security", "audit-baseline.yml")): []byte("version: 1\nwaivers: []\n"),
			filepath.Clean(filepath.Join("/repo", ".git", "info", "exclude")):                []byte("# loopcoder local state\n.loopcoder/\n"),
		},
	}
}

type fakeDoctorEnv struct {
	paths          map[string]string
	commands       map[string]CommandResult
	env            map[string]string
	cfg            config.Config
	configErr      error
	file           []byte
	fileErr        error
	settingsFile   []byte
	settingsErr    error
	executablePath string
	userHome       string
	skillFiles     map[string][]byte
	files          map[string][]byte
	projectShow    func(context.Context, registry.Options) (registry.ShowResult, error)
	projectDupes   func(context.Context, registry.Options) ([]registry.DuplicatePhysicalIdentity, error)
	projectRepair  func(context.Context, registry.Options) ([]registry.DuplicatePhysicalIdentity, error)
}

func (f *fakeDoctorEnv) deps() Deps {
	deps := Deps{
		LookPath: func(file string) (string, error) {
			if path, ok := f.paths[file]; ok {
				return path, nil
			}
			return "", execNotFound(file)
		},
		RunCommand: func(_ context.Context, _ string, name string, args ...string) (CommandResult, error) {
			if result, ok := f.commands[cmdKey(name, args...)]; ok {
				return result, nil
			}
			return CommandResult{
				Stderr:   "unexpected command",
				ExitCode: 127,
			}, nil
		},
		LoadConfig: func(string) (config.Config, error) {
			if f.configErr != nil {
				return config.Config{}, f.configErr
			}
			return f.cfg, nil
		},
		Getenv: func(key string) string {
			if value, ok := f.env[key]; ok {
				return value
			}
			return ""
		},
		ReadFile: func(string) ([]byte, error) {
			panic("unreachable")
		},
		ExecutablePath: func() (string, error) {
			return f.executablePath, nil
		},
		UserHomeDir: func() (string, error) {
			return f.userHome, nil
		},
		SkillMarkdown: func() ([]byte, error) {
			return []byte("skill content\n"), nil
		},
		AgentsMarkdown: func() ([]byte, error) {
			return []byte("agents content\n"), nil
		},
		StorageHealth: func(_ context.Context, path string) (storage.Health, error) {
			return storage.Health{
				Path:          path,
				Exists:        true,
				SchemaVersion: storage.CurrentSchemaVersion,
				OK:            true,
				Message:       "healthy",
			}, nil
		},
		StoragePermissions: func(path string, fix bool) (storage.PermissionReport, error) {
			return storage.PermissionReport{
				Path:      path,
				Platform:  "test",
				Supported: true,
				Secure:    true,
				Message:   "storage permissions are owner-only",
			}, nil
		},
		ProjectShow: func(_ context.Context, opts registry.Options) (registry.ShowResult, error) {
			if f.projectShow != nil {
				return f.projectShow(context.Background(), opts)
			}
			return registry.ShowResult{
				Registered: true,
				Project: registry.Project{
					ProjectID:      "proj_test",
					DisplayName:    "repo",
					LocalPath:      opts.RepoPath,
					IdentitySource: registry.IdentityGitHub,
				},
			}, nil
		},
		ProjectDuplicates: func(_ context.Context, opts registry.Options) ([]registry.DuplicatePhysicalIdentity, error) {
			if f.projectDupes != nil {
				return f.projectDupes(context.Background(), opts)
			}
			return nil, nil
		},
		ProjectRepair: func(_ context.Context, opts registry.Options) ([]registry.DuplicatePhysicalIdentity, error) {
			if f.projectRepair != nil {
				return f.projectRepair(context.Background(), opts)
			}
			return nil, nil
		},
		ProviderInventory: func(_ context.Context, opts providerinventory.Options) (providerinventory.Report, error) {
			return f.providerInventory(opts.Config), nil
		},
		Now: func() time.Time {
			return time.Unix(0, 0).UTC().Add(807 * time.Hour)
		},
	}
	deps.ReadFile = func(path string) ([]byte, error) {
		clean := filepath.Clean(path)
		if data, ok := f.skillFiles[clean]; ok {
			return append([]byte(nil), data...), nil
		}
		for _, name := range []string{"SKILL.md", "AGENTS.md"} {
			if clean == doctorSkillPath(f.userHome, name) {
				return nil, os.ErrNotExist
			}
		}
		if clean == filepath.Clean(claudehooks.SettingsPath("/repo")) {
			if f.settingsErr != nil {
				return nil, f.settingsErr
			}
			if f.settingsFile == nil {
				return nil, os.ErrNotExist
			}
			return append([]byte(nil), f.settingsFile...), nil
		}
		if data, ok := f.files[clean]; ok {
			return append([]byte(nil), data...), nil
		}
		if clean == filepath.Clean(filepath.Join("/repo", ".git", "info", "exclude")) {
			return nil, os.ErrNotExist
		}
		if f.fileErr != nil {
			return nil, f.fileErr
		}
		return f.file, nil
	}
	return deps
}

func (f *fakeDoctorEnv) providerInventory(cfg config.Config) providerinventory.Report {
	now := "2026-07-12T00:00:00Z"
	providers := []struct {
		id      string
		cli     string
		display string
	}{
		{id: "codex", cli: "codex", display: "Codex"},
		{id: "claude", cli: "claude", display: "Claude"},
		{id: "gemini", cli: "gemini", display: "Gemini"},
		{id: "antigravity", cli: "agy", display: "Antigravity"},
	}
	configured := []string{strings.TrimSpace(cfg.Adapters.Worker), strings.TrimSpace(cfg.Adapters.Verifier)}
	for _, name := range configured {
		if name == "" || fakeProviderKnown(providers, name) {
			continue
		}
		providers = append(providers, struct {
			id      string
			cli     string
			display string
		}{id: name, cli: name, display: name})
	}
	var installations []providerinventory.ProviderInstallation
	var probes []providerinventory.ProbeResult
	for _, provider := range providers {
		path, ok := f.paths[provider.cli]
		probeID := "probe_" + provider.id
		if !ok {
			probes = append(probes, providerinventory.ProbeResult{
				SchemaVersion:     providerinventory.ProbeResultSchema,
				ProbeResultID:     probeID,
				AdapterID:         provider.id,
				Outcome:           providerinventory.OutcomeNotInstalled,
				ProbeMethod:       providerinventory.ProbeMethodLookPath,
				Confidence:        providerinventory.ConfidenceUnavailable,
				FreshnessState:    providerinventory.FreshnessNotApplicable,
				CapturedAt:        now,
				CreatedAt:         now,
				UpdatedAt:         now,
				NetworkPermission: providerinventory.NetworkNotNeeded,
				EnvironmentKeys:   []string{},
				GapReasons:        []string{"executable-not-found"},
			})
			continue
		}
		installationID := "pinst_" + provider.id
		probes = append(probes, providerinventory.ProbeResult{
			SchemaVersion:            providerinventory.ProbeResultSchema,
			ProbeResultID:            probeID,
			AdapterID:                provider.id,
			ProviderInstallationID:   &installationID,
			Outcome:                  providerinventory.OutcomeInstalled,
			ProbeMethod:              providerinventory.ProbeMethodFixedCommand,
			Confidence:               providerinventory.ConfidenceExact,
			FreshnessState:           providerinventory.FreshnessFresh,
			CapturedAt:               now,
			CreatedAt:                now,
			UpdatedAt:                now,
			NetworkPermission:        providerinventory.NetworkNotNeeded,
			EnvironmentKeys:          []string{},
			GapReasons:               []string{},
			TimeoutMS:                5000,
			StdoutLimitBytes:         providerinventory.StdoutLimitBytes,
			StderrLimitBytes:         providerinventory.StderrLimitBytes,
			CombinedOutputLimitBytes: providerinventory.CombinedLimitBytes,
		})
		installations = append(installations, providerinventory.ProviderInstallation{
			SchemaVersion:          providerinventory.ProviderInstallationSchema,
			RecordVersion:          1,
			Scope:                  "machine",
			ProviderInstallationID: installationID,
			AdapterID:              provider.id,
			ProviderDisplayName:    provider.display,
			ExecutableName:         provider.cli,
			CanonicalPathRedacted:  filepath.ToSlash(filepath.Join("...", filepath.Base(filepath.Dir(path)), filepath.Base(path))),
			DiscoverySource:        providerinventory.DiscoveryPath,
			DiscoveryOrder:         len(installations),
			Platform:               "test",
			VersionConfidence:      providerinventory.ConfidenceExact,
			LatestProbeResultID:    probeID,
			InstallationState:      providerinventory.InstallationInstalled,
			UsableForInvocation:    "unknown",
			KnownLimitations:       []string{},
			CreatedAt:              now,
			UpdatedAt:              now,
			CapturedAt:             now,
			FreshnessState:         providerinventory.FreshnessFresh,
			Confidence:             providerinventory.ConfidenceExact,
			GapReasons:             []string{},
		})
	}
	return providerinventory.Report{
		SchemaVersion:         providerinventory.ProviderInventoryJSONSchema,
		GeneratedAt:           now,
		InventoryFingerprint:  "sha256:test",
		Confidence:            providerinventory.ConfidenceExact,
		Installations:         installations,
		ProbeResults:          probes,
		AccountProfiles:       []providerinventory.AccountProfile{},
		AuthReadiness:         []providerinventory.AuthReadiness{},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{},
		ModelCapabilities:     []providerinventory.ModelCapability{},
		GapReasons:            []string{},
	}
}

func fakeProviderKnown(providers []struct {
	id      string
	cli     string
	display string
}, name string) bool {
	for _, provider := range providers {
		if provider.id == name {
			return true
		}
	}
	return false
}

func writeDoctorTextFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func doctorSkillPath(home string, name string) string {
	return filepath.Clean(filepath.Join(home, ".claude", "skills", "loopcoder", name))
}

func healthyClaudeSettings() []byte {
	settings, _, err := claudehooks.MergeSettings(nil)
	if err != nil {
		panic(err)
	}
	return settings
}

func execNotFound(file string) error {
	return &os.PathError{Op: "exec", Path: file, Err: os.ErrNotExist}
}

func cmdKey(name string, args ...string) string {
	return name + "\x00" + strings.Join(args, "\x00")
}

func requireCheck(t *testing.T, report Report, name string) Check {
	t.Helper()
	check, ok := report.Find(name)
	if !ok {
		t.Fatalf("missing check %q in %#v", name, report.Checks)
	}
	return check
}

func assertLegacySurface(t *testing.T, surfaces []LegacySurface, identifier string, classification string, remediationContains string) {
	t.Helper()
	for _, surface := range surfaces {
		if surface.Identifier != identifier {
			continue
		}
		if surface.Classification != classification {
			t.Fatalf("surface %q classification = %q, want %q (%#v)", identifier, surface.Classification, classification, surface)
		}
		if !strings.Contains(surface.Remediation, remediationContains) {
			t.Fatalf("surface %q remediation = %q, want containing %q", identifier, surface.Remediation, remediationContains)
		}
		return
	}
	t.Fatalf("surface %q not found in %#v", identifier, surfaces)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func doctorUsageReport(issue int, start time.Time) reporter.Report {
	total := int64(issue * 10)
	return reporter.Report{
		WorkID:      state.RunIDForIssue(issue, start),
		Issue:       issue,
		Role:        reporter.RoleWorker,
		Provider:    "codex",
		Model:       "gpt-fixture",
		ModelSource: reporter.ModelSourceParsed,
		Effort:      "high",
		Permission:  reporter.PermissionWrite,
		Action:      "implement issue #" + fmt.Sprint(issue),
		ExitCode:    0,
		StartedAt:   start.UTC().Format(time.RFC3339Nano),
		EndedAt:     start.Add(42 * time.Second).UTC().Format(time.RFC3339Nano),
		DurationMS:  int64((42 * time.Second).Milliseconds()),
		Usage: reporter.Usage{
			TotalTokens: &total,
		},
		Verified: true,
	}
}
