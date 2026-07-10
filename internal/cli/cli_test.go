package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/audit"
	compiler "github.com/jasonhnd/loopcoder/internal/compile"
	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/doctor"
	"github.com/jasonhnd/loopcoder/internal/gitlocal"
	"github.com/jasonhnd/loopcoder/internal/loopreview"
	localmigrate "github.com/jasonhnd/loopcoder/internal/migrate"
	"github.com/jasonhnd/loopcoder/internal/migration"
	"github.com/jasonhnd/loopcoder/internal/orchestration"
	"github.com/jasonhnd/loopcoder/internal/perception"
	"github.com/jasonhnd/loopcoder/internal/recovery"
	"github.com/jasonhnd/loopcoder/internal/relaygate"
	"github.com/jasonhnd/loopcoder/internal/report"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/scaffold"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/statebranch"
	"github.com/jasonhnd/loopcoder/internal/upgrade"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
	"github.com/jasonhnd/loopcoder/internal/verify"
	"github.com/jasonhnd/loopcoder/internal/worker"
)

func TestRootHelpListsSubcommands(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := Run([]string{"--help"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("Run returned exit code %d, want 0", exitCode)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	help := stdout.String()
	for _, want := range []string{"loopcoder --version", "loopcoder -v"} {
		if !strings.Contains(help, want) {
			t.Fatalf("root help missing %q:\n%s", want, help)
		}
	}
	for _, command := range Commands() {
		if !strings.Contains(help, command.Name) {
			t.Fatalf("root help does not list %q:\n%s", command.Name, help)
		}
	}
}

func TestSubcommandHelpWorks(t *testing.T) {
	for _, command := range Commands() {
		t.Run(command.Name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			exitCode := Run([]string{command.Name, "--help"}, &stdout, &stderr)
			if exitCode != 0 {
				t.Fatalf("Run returned exit code %d, want 0", exitCode)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}

			help := stdout.String()
			if !strings.Contains(help, "loopcoder "+command.Name) {
				t.Fatalf("command help missing usage for %q:\n%s", command.Name, help)
			}
			if !strings.Contains(help, "--help") {
				t.Fatalf("command help missing --help flag:\n%s", help)
			}
		})
	}
}

func TestDoctorJSONStdoutIsMachineReadable(t *testing.T) {
	repo := t.TempDir()
	var stdout, stderr bytes.Buffer

	exitCode := RunWithDeps([]string{"doctor", "--repo", repo, "--format", "json"}, &stdout, &stderr, Deps{
		Doctor: func(_ context.Context, opts doctor.Options) doctor.Report {
			return doctor.WithMetadata(doctor.Report{
				HostProfile: doctor.HostProfile{
					Name:               "codex-cli",
					Source:             "env",
					Selector:           "LOOPCODER_HOST",
					InvocationStyle:    "interactive Codex CLI conductor session calls loopcoder as a local subprocess",
					SupportsJSONOutput: true,
				},
				Checks: []doctor.Check{{
					Name:    "host profile",
					Status:  doctor.StatusOK,
					Message: "profile=codex-cli source=env selector=LOOPCODER_HOST",
				}},
			}, opts.RepoPath, doctor.BuildInfo{Version: "0.7.0", Commit: "abc123", Date: "2026-07-10T00:00:00Z"})
		},
	})

	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	var payload struct {
		HostProfile struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"host_profile"`
		Checks []struct {
			Name string `json:"name"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not clean doctor JSON: %v\n%s", err, stdout.String())
	}
	if payload.HostProfile.Name != "codex-cli" || payload.HostProfile.Source != "env" || len(payload.Checks) != 1 {
		t.Fatalf("payload = %#v, want host profile and one check", payload)
	}
}

func TestReportCommandListsLocalReportsReadOnly(t *testing.T) {
	repo := t.TempDir()
	record := validDispatchReport()
	record.WorkID = "run-report-test"
	record.Issue = 101
	record.Branch = "loop/issue-101"
	record.Round = 1

	if _, err := state.WriteAttempt(repo, "run-report-test", state.AttemptRecord{
		Version:   1,
		JobID:     "job-101-1",
		Issue:     101,
		Attempt:   1,
		Provider:  "codex",
		Status:    "succeeded",
		Branch:    "loop/issue-101",
		StartedAt: "2026-06-28T00:00:00Z",
		Report:    &record,
	}); err != nil {
		t.Fatalf("write attempt: %v", err)
	}

	var stdout, stderr bytes.Buffer
	exitCode := RunWithDeps([]string{
		"report",
		"--repo", repo,
		"--work-id", "run-report-test",
		"--format", "json",
	}, &stdout, &stderr, Deps{})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	var payload struct {
		Reports []reporter.Report `json:"reports"`
		Records []struct {
			Report reporter.Report `json:"report"`
			Source string          `json:"source"`
			RunID  string          `json:"run_id"`
			Path   string          `json:"path"`
		} `json:"records"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("report output is not JSON: %v\n%s", err, stdout.String())
	}
	if len(payload.Reports) != 1 || payload.Reports[0].WorkID != "run-report-test" || payload.Reports[0].Issue != 101 {
		t.Fatalf("reports = %#v, want one filtered local report", payload.Reports)
	}
	if len(payload.Records) != 1 {
		t.Fatalf("records = %d, want one filtered local record: %s", len(payload.Records), stdout.String())
	}
	if payload.Records[0].Report.WorkID != "run-report-test" || payload.Records[0].Source != "attempt" || payload.Records[0].RunID != "run-report-test" || payload.Records[0].Path == "" {
		t.Fatalf("records = %#v, want one filtered local record with source context", payload.Records)
	}
	if strings.Contains(stdout.String(), `"`+migration.LegacyReportStateKey+`"`) {
		t.Fatalf("report JSON used legacy report key:\n%s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	exitCode = RunWithDeps([]string{
		"report",
		"--repo", repo,
		"--work-id", "run-report-test",
	}, &stdout, &stderr, Deps{})
	if exitCode != 0 {
		t.Fatalf("text report returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "loopcoder report: worker succeeded") || !strings.Contains(stdout.String(), "Next") {
		t.Fatalf("text report missing receipt:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "canonical JSON") || strings.Contains(stdout.String(), `"role":"worker"`) {
		t.Fatalf("default text report included raw JSON:\n%s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	exitCode = RunWithDeps([]string{
		"report",
		"--repo", repo,
		"--work-id", "run-report-test",
		"--verbose",
	}, &stdout, &stderr, Deps{})
	if exitCode != 0 {
		t.Fatalf("verbose report returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Raw record") || !strings.Contains(stdout.String(), "- canonical JSON: {\"work_id\":\"run-report-test\"") {
		t.Fatalf("verbose report missing raw canonical record:\n%s", stdout.String())
	}
	if pending := relaygate.Check(repo); len(pending) != 0 {
		t.Fatalf("report command mutated relay state: %#v", pending)
	}
}

func TestStatusCommandRendersRunTreeJSON(t *testing.T) {
	repo := t.TempDir()
	parent := "run-cli-parent"
	child := "run-cli-child"
	if err := state.AppendLifecycleTransition(repo, state.LifecycleTransition{
		Timestamp:  "2026-07-09T00:00:00Z",
		RunID:      parent,
		State:      state.StatePlanned,
		ChildRunID: child,
	}); err != nil {
		t.Fatalf("append parent lifecycle: %v", err)
	}
	if err := state.AppendLifecycleTransition(repo, state.LifecycleTransition{
		Timestamp:   "2026-07-09T00:00:01Z",
		RunID:       child,
		ParentRunID: parent,
		State:       state.StatePlanned,
	}); err != nil {
		t.Fatalf("append child lifecycle: %v", err)
	}
	record := validDispatchReport()
	record.Issue = 651
	if _, err := state.WriteAttempt(repo, child, state.AttemptRecord{
		Version:        1,
		JobID:          "job-651-1",
		Issue:          651,
		Attempt:        1,
		Provider:       "codex",
		Status:         "succeeded",
		Phase:          "codex_exited",
		StartedAt:      "2026-07-09T00:00:02Z",
		HeartbeatAt:    "2026-07-09T00:00:03Z",
		LastProgressAt: "2026-07-09T00:00:03Z",
		Report:         &record,
	}); err != nil {
		t.Fatalf("write child attempt: %v", err)
	}

	var stdout, stderr bytes.Buffer
	exitCode := RunWithDeps([]string{"status", "--repo", repo, "--run", child, "--format", "json"}, &stdout, &stderr, Deps{})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	var payload struct {
		RunID   string `json:"run_id"`
		Project struct {
			ProjectID string `json:"project_id"`
		} `json:"project"`
		RunTree struct {
			RootRunID string `json:"root_run_id"`
			Nodes     []struct {
				RunID       string `json:"run_id"`
				ParentRunID string `json:"parent_run_id"`
				Issue       int    `json:"issue"`
				Provider    string `json:"provider"`
			} `json:"nodes"`
		} `json:"run_tree"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("status output is not JSON: %v\n%s", err, stdout.String())
	}
	if payload.RunID != child || payload.Project.ProjectID == "" || payload.RunTree.RootRunID != parent || len(payload.RunTree.Nodes) != 2 {
		t.Fatalf("status JSON = %#v", payload)
	}
	var foundChild bool
	for _, node := range payload.RunTree.Nodes {
		if node.RunID == child && node.ParentRunID == parent && node.Issue == 651 && node.Provider == "codex" {
			foundChild = true
		}
	}
	if !foundChild {
		t.Fatalf("child node missing metadata: %#v", payload.RunTree.Nodes)
	}
}

func TestReportCommandJSONCanIncludeRunTree(t *testing.T) {
	repo := t.TempDir()
	parent := "run-report-parent"
	child := "run-report-child"
	if err := state.AppendLifecycleTransition(repo, state.LifecycleTransition{
		Timestamp:  "2026-07-09T00:00:00Z",
		RunID:      parent,
		State:      state.StatePlanned,
		ChildRunID: child,
	}); err != nil {
		t.Fatalf("append parent lifecycle: %v", err)
	}
	if err := state.AppendLifecycleTransition(repo, state.LifecycleTransition{
		Timestamp:   "2026-07-09T00:00:01Z",
		RunID:       child,
		ParentRunID: parent,
		State:       state.StatePlanned,
	}); err != nil {
		t.Fatalf("append child lifecycle: %v", err)
	}
	record := validDispatchReport()
	record.WorkID = child
	record.Issue = 651
	if _, err := state.WriteAttempt(repo, child, state.AttemptRecord{
		Version:        1,
		JobID:          "job-651-1",
		Issue:          651,
		Attempt:        1,
		Provider:       "codex",
		Status:         "succeeded",
		StartedAt:      "2026-07-09T00:00:02Z",
		HeartbeatAt:    "2026-07-09T00:00:03Z",
		LastProgressAt: "2026-07-09T00:00:03Z",
		Report:         &record,
	}); err != nil {
		t.Fatalf("write child attempt: %v", err)
	}

	var stdout, stderr bytes.Buffer
	exitCode := RunWithDeps([]string{"report", "--repo", repo, "--run", child, "--format", "json"}, &stdout, &stderr, Deps{})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	var payload struct {
		Reports []reporter.Report `json:"reports"`
		Records []struct {
			RunID string `json:"run_id"`
		} `json:"records"`
		RunTree struct {
			RootRunID string `json:"root_run_id"`
			Nodes     []struct {
				RunID string `json:"run_id"`
			} `json:"nodes"`
		} `json:"run_tree"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("report output is not JSON: %v\n%s", err, stdout.String())
	}
	if len(payload.Reports) != 1 || len(payload.Records) != 1 || payload.Records[0].RunID != child {
		t.Fatalf("report records = %#v %#v", payload.Reports, payload.Records)
	}
	if payload.RunTree.RootRunID != parent || len(payload.RunTree.Nodes) != 2 {
		t.Fatalf("run tree = %#v", payload.RunTree)
	}
}

func TestMigrateLocalStateCommandRunsInjectedMigration(t *testing.T) {
	repo := t.TempDir()
	var stdout, stderr bytes.Buffer

	exitCode := RunWithDeps([]string{"migrate", "local-state", "--repo", repo, "--dry-run", "--format", "json"}, &stdout, &stderr, Deps{
		Now: fixedCLINow,
		MigrateLocalState: func(_ context.Context, opts localmigrate.Options) (localmigrate.Result, error) {
			if opts.RepoPath != repo {
				t.Fatalf("RepoPath = %q, want %q", opts.RepoPath, repo)
			}
			if !opts.DryRun {
				t.Fatalf("DryRun = false, want true")
			}
			return localmigrate.Result{
				RepoPath:       opts.RepoPath,
				ProjectID:      "proj_test",
				DatabasePath:   filepath.Join(repo, "loopcoder.db"),
				DryRun:         opts.DryRun,
				Status:         "completed-with-warnings",
				ScannedCount:   3,
				ImportedCount:  2,
				SkippedCount:   1,
				MalformedCount: 1,
				Diagnostics: []localmigrate.Diagnostic{{
					SourcePath: ".loopcoder/runs/run-test/events.jsonl",
					Line:       2,
					Message:    "malformed event JSONL",
				}},
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%s", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	var payload struct {
		ProjectID      string                    `json:"project_id"`
		DryRun         bool                      `json:"dry_run"`
		Status         string                    `json:"status"`
		MalformedCount int                       `json:"malformed_count"`
		Diagnostics    []localmigrate.Diagnostic `json:"diagnostics"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if payload.ProjectID != "proj_test" || !payload.DryRun || payload.Status != "completed-with-warnings" || payload.MalformedCount != 1 || len(payload.Diagnostics) != 1 {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestVersionCommandAndRootFlagsPrintBuildInfo(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "subcommand", args: []string{"version"}},
		{name: "root long flag", args: []string{"--version"}},
		{name: "root short flag", args: []string{"-v"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			exitCode := RunWithDeps(tt.args, &stdout, &stderr, Deps{
				BuildInfo: BuildInfo{
					Version: "v0.3.1",
					Commit:  "abc123",
					Date:    "2026-06-29T00:00:00Z",
				},
			})
			if exitCode != 0 {
				t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}

			output := stdout.String()
			for _, want := range []string{
				"loopcoder",
				"version=v0.3.1",
				"commit=abc123",
				"date=2026-06-29T00:00:00Z",
				"go=" + runtime.Version(),
				"platform=" + runtime.GOOS + "/" + runtime.GOARCH,
			} {
				if !strings.Contains(output, want) {
					t.Fatalf("version output missing %q:\n%s", want, output)
				}
			}
		})
	}
}

func TestVersionDefaultsToDevBuildInfo(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := Run([]string{"version"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("Run returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	output := stdout.String()
	for _, want := range []string{"version=dev", "commit=unknown", "date=unknown"} {
		if !strings.Contains(output, want) {
			t.Fatalf("version output missing %q:\n%s", want, output)
		}
	}
}

func TestModelsCommandPrintsStaticRegistry(t *testing.T) {
	t.Setenv("PATH", "")
	var stdout, stderr bytes.Buffer

	exitCode := RunWithDeps([]string{"models"}, &stdout, &stderr, Deps{})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if got, want := stdout.String(), expectedModelsOutput(); got != want {
		t.Fatalf("models output mismatch:\ngot:\n%swant:\n%s", got, want)
	}
}

func TestModelsCommandFiltersProvider(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := RunWithDeps([]string{"models", "--provider", "antigravity"}, &stdout, &stderr, Deps{})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if got, want := stdout.String(), expectedAntigravityModelsOutput(); got != want {
		t.Fatalf("models --provider antigravity output mismatch:\ngot:\n%swant:\n%s", got, want)
	}
}

func TestModelsCommandRejectsAgyProviderWithHint(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := RunWithDeps([]string{"models", "--provider", "agy"}, &stdout, &stderr, Deps{})
	if exitCode != 2 {
		t.Fatalf("RunWithDeps returned exit code %d, want 2", exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	for _, want := range []string{
		`unknown provider "agy"`,
		"supported providers: codex, claude, antigravity",
		"use --provider antigravity",
		"agy is the CLI executable",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr.String())
		}
	}
}

func TestProjectsCommandRegisterListShowRemoveJSON(t *testing.T) {
	repo := initProjectRegistryCLITestRepo(t, "https://github.com/owner/repo.git")
	t.Setenv("LOOPCODER_HOME", t.TempDir())

	var registerOut, registerErr bytes.Buffer
	exitCode := RunWithDeps([]string{"projects", "register", "--repo", repo, "--format", "json"}, &registerOut, &registerErr, Deps{
		Now: fixedCLINow,
	})
	if exitCode != 0 {
		t.Fatalf("register exit = %d stderr=%q", exitCode, registerErr.String())
	}
	var registered struct {
		Project struct {
			ProjectID           string `json:"project_id"`
			DisplayName         string `json:"display_name"`
			RemoteURLNormalized string `json:"remote_url_normalized"`
			IdentitySource      string `json:"identity_source"`
		} `json:"project"`
		Created bool `json:"created"`
	}
	if err := json.Unmarshal(registerOut.Bytes(), &registered); err != nil {
		t.Fatalf("register JSON: %v\n%s", err, registerOut.String())
	}
	if !registered.Created || registered.Project.ProjectID == "" || registered.Project.DisplayName != "repo" || registered.Project.IdentitySource != "github" {
		t.Fatalf("registered = %#v", registered)
	}

	var secondOut, secondErr bytes.Buffer
	exitCode = RunWithDeps([]string{"projects", "register", "--repo", repo, "--format", "json"}, &secondOut, &secondErr, Deps{
		Now: fixedCLINow,
	})
	if exitCode != 0 {
		t.Fatalf("second register exit = %d stderr=%q", exitCode, secondErr.String())
	}
	var second struct {
		Updated bool `json:"updated"`
	}
	if err := json.Unmarshal(secondOut.Bytes(), &second); err != nil {
		t.Fatalf("second register JSON: %v\n%s", err, secondOut.String())
	}
	if !second.Updated {
		t.Fatalf("second = %#v, want updated", second)
	}

	var listOut, listErr bytes.Buffer
	exitCode = RunWithDeps([]string{"projects", "list", "--format", "json"}, &listOut, &listErr, Deps{})
	if exitCode != 0 {
		t.Fatalf("list exit = %d stderr=%q", exitCode, listErr.String())
	}
	var list struct {
		Projects []struct {
			ProjectID string `json:"project_id"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(listOut.Bytes(), &list); err != nil {
		t.Fatalf("list JSON: %v\n%s", err, listOut.String())
	}
	if len(list.Projects) != 1 || list.Projects[0].ProjectID != registered.Project.ProjectID {
		t.Fatalf("list = %#v, want one registered project", list)
	}

	var showOut, showErr bytes.Buffer
	exitCode = RunWithDeps([]string{"projects", "show", "--repo", repo, "--format", "json"}, &showOut, &showErr, Deps{})
	if exitCode != 0 {
		t.Fatalf("show exit = %d stderr=%q", exitCode, showErr.String())
	}
	var show struct {
		Registered bool `json:"registered"`
		Project    struct {
			ProjectID string `json:"project_id"`
		} `json:"project"`
	}
	if err := json.Unmarshal(showOut.Bytes(), &show); err != nil {
		t.Fatalf("show JSON: %v\n%s", err, showOut.String())
	}
	if !show.Registered || show.Project.ProjectID != registered.Project.ProjectID {
		t.Fatalf("show = %#v, want registered project", show)
	}

	var removeOut, removeErr bytes.Buffer
	exitCode = RunWithDeps([]string{"projects", "remove", "--repo", repo, "--format", "json"}, &removeOut, &removeErr, Deps{})
	if exitCode != 0 {
		t.Fatalf("remove exit = %d stderr=%q", exitCode, removeErr.String())
	}
	var removed struct {
		Removed           bool `json:"removed"`
		Detached          bool `json:"detached"`
		ProjectDeleted    bool `json:"project_deleted"`
		RunHistoryDeleted bool `json:"run_history_deleted"`
		Preserved         struct {
			Runs                int `json:"runs"`
			RunEvents           int `json:"run_events"`
			RunEdges            int `json:"run_edges"`
			Reports             int `json:"reports"`
			LegacyImportRecords int `json:"legacy_import_records"`
			LegacyImportStatus  int `json:"legacy_import_status"`
		} `json:"preserved"`
		Deleted struct {
			Runs                int `json:"runs"`
			RunEvents           int `json:"run_events"`
			RunEdges            int `json:"run_edges"`
			Reports             int `json:"reports"`
			LegacyImportRecords int `json:"legacy_import_records"`
			LegacyImportStatus  int `json:"legacy_import_status"`
		} `json:"deleted"`
	}
	if err := json.Unmarshal(removeOut.Bytes(), &removed); err != nil {
		t.Fatalf("remove JSON: %v\n%s", err, removeOut.String())
	}
	if !removed.Removed || !removed.Detached || removed.ProjectDeleted || removed.RunHistoryDeleted {
		t.Fatalf("removed = %#v, want detached without history deletion", removed)
	}
	if removed.Deleted != (struct {
		Runs                int `json:"runs"`
		RunEvents           int `json:"run_events"`
		RunEdges            int `json:"run_edges"`
		Reports             int `json:"reports"`
		LegacyImportRecords int `json:"legacy_import_records"`
		LegacyImportStatus  int `json:"legacy_import_status"`
	}{}) {
		t.Fatalf("removed deleted counts = %#v, want zero", removed.Deleted)
	}

	var listAfterRemoveOut, listAfterRemoveErr bytes.Buffer
	exitCode = RunWithDeps([]string{"projects", "list", "--format", "json"}, &listAfterRemoveOut, &listAfterRemoveErr, Deps{})
	if exitCode != 0 {
		t.Fatalf("list after remove exit = %d stderr=%q", exitCode, listAfterRemoveErr.String())
	}
	var listAfterRemove struct {
		Projects []struct {
			ProjectID string `json:"project_id"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(listAfterRemoveOut.Bytes(), &listAfterRemove); err != nil {
		t.Fatalf("list after remove JSON: %v\n%s", err, listAfterRemoveOut.String())
	}
	if len(listAfterRemove.Projects) != 0 {
		t.Fatalf("list after remove = %#v, want no active projects", listAfterRemove)
	}

	var showAfterRemoveOut, showAfterRemoveErr bytes.Buffer
	exitCode = RunWithDeps([]string{"projects", "show", "--repo", repo, "--format", "json"}, &showAfterRemoveOut, &showAfterRemoveErr, Deps{})
	if exitCode != 0 {
		t.Fatalf("show after remove exit = %d stderr=%q", exitCode, showAfterRemoveErr.String())
	}
	var showAfterRemove struct {
		Registered bool `json:"registered"`
		Detached   bool `json:"detached"`
		Project    struct {
			ProjectID  string `json:"project_id"`
			DetachedAt string `json:"detached_at"`
		} `json:"project"`
	}
	if err := json.Unmarshal(showAfterRemoveOut.Bytes(), &showAfterRemove); err != nil {
		t.Fatalf("show after remove JSON: %v\n%s", err, showAfterRemoveOut.String())
	}
	if showAfterRemove.Registered || !showAfterRemove.Detached || showAfterRemove.Project.ProjectID != registered.Project.ProjectID || showAfterRemove.Project.DetachedAt == "" {
		t.Fatalf("show after remove = %#v, want detached preserved project", showAfterRemove)
	}

	var reactivateOut, reactivateErr bytes.Buffer
	exitCode = RunWithDeps([]string{"projects", "register", "--repo", repo, "--format", "json"}, &reactivateOut, &reactivateErr, Deps{
		Now: fixedCLINow,
	})
	if exitCode != 0 {
		t.Fatalf("reactivate exit = %d stderr=%q", exitCode, reactivateErr.String())
	}
	var reactivated struct {
		Updated     bool `json:"updated"`
		Reactivated bool `json:"reactivated"`
		Project     struct {
			ProjectID  string `json:"project_id"`
			DetachedAt string `json:"detached_at"`
		} `json:"project"`
	}
	if err := json.Unmarshal(reactivateOut.Bytes(), &reactivated); err != nil {
		t.Fatalf("reactivate JSON: %v\n%s", err, reactivateOut.String())
	}
	if !reactivated.Updated || !reactivated.Reactivated || reactivated.Project.ProjectID != registered.Project.ProjectID || reactivated.Project.DetachedAt != "" {
		t.Fatalf("reactivated = %#v, want same active project", reactivated)
	}
}

func TestProjectsCommandNeverEmitsOrPersistsRemoteCredentialSentinel(t *testing.T) {
	secret := "loopcoder-sentinel-secret-687"
	remote := "https://alice:" + secret + "@github.com/owner/private.git?access_token=" + secret + "&X-Amz-Signature=" + secret + "#token=" + secret
	repo := initProjectRegistryCLITestRepo(t, remote)
	homeDir := t.TempDir()
	t.Setenv("LOOPCODER_HOME", homeDir)

	var combined bytes.Buffer
	runProjectsJSONForSecretTest := func(args ...string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		exitCode := RunWithDeps(args, &stdout, &stderr, Deps{Now: fixedCLINow})
		combined.Write(stdout.Bytes())
		combined.Write(stderr.Bytes())
		if exitCode != 0 {
			t.Fatalf("%v exit = %d stderr=%q", args, exitCode, stderr.String())
		}
		if strings.Contains(stdout.String(), secret) || strings.Contains(stderr.String(), secret) {
			t.Fatalf("%v leaked secret\nstdout:\n%s\nstderr:\n%s", args, stdout.String(), stderr.String())
		}
	}

	runProjectsJSONForSecretTest("projects", "register", "--repo", repo, "--format", "json")
	runProjectsJSONForSecretTest("projects", "list", "--format", "json")
	runProjectsJSONForSecretTest("projects", "show", "--repo", repo, "--format", "json")

	dbText := readAllSQLiteText(t, filepath.Join(homeDir, "data", "loopcoder.db"))
	if strings.Contains(dbText, secret) {
		t.Fatalf("SQLite text fields leaked secret:\n%s", dbText)
	}
	if !strings.Contains(dbText, "https://github.com/owner/private") {
		t.Fatalf("SQLite text fields missing sanitized remote:\n%s", dbText)
	}

	runProjectsJSONForSecretTest("projects", "remove", "--repo", repo, "--format", "json")
	if strings.Contains(combined.String(), secret) {
		t.Fatalf("combined command output leaked secret:\n%s", combined.String())
	}
}

func TestProjectsListJSONUsesEmptyArrayWhenNoProjects(t *testing.T) {
	t.Setenv("LOOPCODER_HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	exitCode := RunWithDeps([]string{"projects", "list", "--format", "json"}, &stdout, &stderr, Deps{})
	if exitCode != 0 {
		t.Fatalf("list exit = %d stderr=%q", exitCode, stderr.String())
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("list JSON: %v\n%s", err, stdout.String())
	}
	if got := strings.TrimSpace(string(payload["projects"])); got != "[]" {
		t.Fatalf("projects JSON = %s, want []\nfull output:\n%s", got, stdout.String())
	}
}

func TestProjectsShowJSONWorksWhenUnregistered(t *testing.T) {
	repo := initProjectRegistryCLITestRepo(t, "https://github.com/owner/unregistered.git")
	t.Setenv("LOOPCODER_HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	exitCode := RunWithDeps([]string{"projects", "show", "--repo", repo, "--format", "json"}, &stdout, &stderr, Deps{})
	if exitCode != 0 {
		t.Fatalf("show exit = %d stderr=%q", exitCode, stderr.String())
	}
	var show struct {
		Registered bool `json:"registered"`
		Project    struct {
			ProjectID      string `json:"project_id"`
			IdentitySource string `json:"identity_source"`
		} `json:"project"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &show); err != nil {
		t.Fatalf("show JSON: %v\n%s", err, stdout.String())
	}
	if show.Registered || show.Project.ProjectID == "" || show.Project.IdentitySource != "github" {
		t.Fatalf("show = %#v, want unregistered github candidate", show)
	}
}

func expectedModelsOutput() string {
	return "provider: codex\n" +
		"vendor: OpenAI Codex\n" +
		"cli: codex\n" +
		"default: gpt-5.5 / high\n" +
		"models:\n" +
		"  - gpt-5.5\n" +
		"    depths: low, medium, high*, xhigh\n" +
		"\n" +
		"provider: claude\n" +
		"vendor: Anthropic\n" +
		"cli: claude\n" +
		"default: claude-opus-4-8[1m] / max\n" +
		"models:\n" +
		"  - claude-opus-4-8[1m]\n" +
		"    depths: low, medium, high, max*\n" +
		"\n" +
		expectedAntigravityModelsOutput()
}

func expectedAntigravityModelsOutput() string {
	return "provider: antigravity\n" +
		"vendor: Google Antigravity\n" +
		"cli: agy\n" +
		"default: Gemini 3.1 Pro / High\n" +
		"models:\n" +
		"  - Gemini 3.1 Pro\n" +
		"    depths: Low, High*\n" +
		"  - Opus 4.6\n" +
		"    depths: Thinking*\n" +
		"  - GPT-OSS 120B\n" +
		"    depths: Medium*\n"
}

func TestAuditCommandRunsInjectedAuditAndRendersJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()

	exitCode := RunWithDeps([]string{
		"audit",
		"--repo", repo,
		"--format", "json",
		"--layer", "sast",
		"--threshold", "high",
	}, &stdout, &stderr, Deps{
		Audit: func(_ context.Context, opts audit.Options) (audit.Result, error) {
			if opts.RepoPath != repo {
				t.Fatalf("RepoPath = %q, want %q", opts.RepoPath, repo)
			}
			if !reflect.DeepEqual(opts.Layers, []string{"sast"}) {
				t.Fatalf("Layers = %#v, want sast", opts.Layers)
			}
			if opts.ThresholdOverride != "high" {
				t.Fatalf("ThresholdOverride = %q, want high", opts.ThresholdOverride)
			}
			result := audit.NewResult(repo, []string{audit.LayerSAST}, audit.SeverityHigh)
			result.Findings = []audit.Finding{{
				ID:          "gosec:G101:a.go:1",
				Layer:       audit.LayerSAST,
				Tool:        "gosec",
				Severity:    audit.SeverityHigh,
				File:        "a.go",
				Line:        1,
				Rule:        "G101",
				Category:    "security",
				Message:     "hardcoded credential",
				Evidence:    "[REDACTED]",
				Fingerprint: "sha256:test",
			}}
			return audit.Finalize(result), nil
		},
	})
	if exitCode != 1 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	for _, want := range []string{`"schema_version": 1`, `"verdict": "findings"`, `"threshold": "high"`} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("audit JSON missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestAuditCommandInvalidFormatUsesRuntimeFailureExit(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := RunWithDeps([]string{"audit", "--format", "xml"}, &stdout, &stderr, Deps{})
	if exitCode != auditCommandFailureExitCode {
		t.Fatalf("RunWithDeps returned exit code %d, want %d", exitCode, auditCommandFailureExitCode)
	}
	if !strings.Contains(stderr.String(), "invalid --format") {
		t.Fatalf("stderr missing invalid format message: %q", stderr.String())
	}
}

func TestAuditLLMStrictRejectsInvalidVerifierSelection(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	if err := os.WriteFile(repo+"/.delivery.yml", []byte(`version: 1
models:
  strict: true
verifier:
  model: custom-review-model
`), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}

	called := false
	exitCode := RunWithDeps([]string{
		"audit",
		"--repo", repo,
		"--layer", audit.LayerLLM,
	}, &stdout, &stderr, Deps{
		Audit: func(context.Context, audit.Options) (audit.Result, error) {
			called = true
			return audit.Result{}, nil
		},
	})
	if exitCode != auditCommandFailureExitCode {
		t.Fatalf("RunWithDeps returned exit code %d, want %d", exitCode, auditCommandFailureExitCode)
	}
	if called {
		t.Fatal("Audit dependency was called despite strict model rejection")
	}
	if !strings.Contains(stderr.String(), "reject") || !strings.Contains(stderr.String(), `model "custom-review-model"`) {
		t.Fatalf("stderr missing strict rejection:\n%s", stderr.String())
	}
}

func TestRelayGateBlocksMechanicalCommands(t *testing.T) {
	if relayGateExitCode == 0 || relayGateExitCode == 1 || relayGateExitCode == 2 || relayGateExitCode == 3 {
		t.Fatalf("relayGateExitCode = %d, want distinct from 0/1/2/3", relayGateExitCode)
	}

	tests := []struct {
		name string
		args func(repo string) []string
		deps func(t *testing.T) Deps
	}{
		{
			name: "dispatch",
			args: func(repo string) []string {
				return []string{"dispatch", "--repo", repo, "--issue-number", "101", "--issue-title", "Implement"}
			},
			deps: func(t *testing.T) Deps {
				return Deps{Dispatch: func(context.Context, worker.Options) (worker.Result, error) {
					t.Fatal("Dispatch dependency should not run while relay gate is blocked")
					return worker.Result{}, nil
				}}
			},
		},
		{
			name: "dispatch-wave",
			args: func(repo string) []string {
				return []string{"dispatch-wave", "--repo", repo, "--issue-numbers", "101"}
			},
			deps: func(t *testing.T) Deps {
				return Deps{Dispatch: func(context.Context, worker.Options) (worker.Result, error) {
					t.Fatal("Dispatch dependency should not run while relay gate is blocked")
					return worker.Result{}, nil
				}}
			},
		},
		{
			name: "loopreview",
			args: func(repo string) []string {
				return []string{"loopreview", "--repo", repo, "--pr-number", "101", "--provider", "claude"}
			},
			deps: func(t *testing.T) Deps {
				return Deps{Loopreview: func(context.Context, loopreview.Options) (loopreview.Result, error) {
					t.Fatal("Loopreview dependency should not run while relay gate is blocked")
					return loopreview.Result{}, nil
				}}
			},
		},
		{
			name: "ready-set",
			args: func(repo string) []string {
				return []string{"ready-set", "--repo", repo}
			},
			deps: func(t *testing.T) Deps {
				return Deps{ComputeReadySet: func(context.Context, orchestration.Options) (report.ReadySetReport, error) {
					t.Fatal("ComputeReadySet dependency should not run while relay gate is blocked")
					return report.ReadySetReport{}, nil
				}}
			},
		},
		{
			name: "verify-local",
			args: func(repo string) []string {
				return []string{"verify-local", "--repo", repo, "--pr-number", "101"}
			},
			deps: func(t *testing.T) Deps {
				return Deps{Verify: func(context.Context, verify.Options) verify.Result {
					t.Fatal("Verify dependency should not run while relay gate is blocked")
					return verify.Result{}
				}}
			},
		},
		{
			name: "recover",
			args: func(repo string) []string {
				return []string{"recover", "--repo", repo, "--issue-number", "101", "--issue-title", "Recover", "--run-id", "run-test"}
			},
			deps: func(t *testing.T) Deps {
				return Deps{Recover: func(context.Context, recovery.Options) (recovery.Result, error) {
					t.Fatal("Recover dependency should not run while relay gate is blocked")
					return recovery.Result{}, nil
				}}
			},
		},
		{
			name: "promote",
			args: func(repo string) []string {
				return []string{"promote", "--repo", repo}
			},
			deps: func(t *testing.T) Deps {
				return Deps{Promote: func(context.Context, orchestration.PromoteOptions) (orchestration.PromoteReport, error) {
					t.Fatal("Promote dependency should not run while relay gate is blocked")
					return orchestration.PromoteReport{}, nil
				}}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := t.TempDir()
			block := cliPendingPrettyBlock("worker")
			writePendingRelayForCLITest(t, repo, "worker", 101, block)

			var stdout, stderr bytes.Buffer
			exitCode := RunWithDeps(tt.args(repo), &stdout, &stderr, tt.deps(t))
			if exitCode != relayGateExitCode {
				t.Fatalf("RunWithDeps returned exit code %d, want %d; stderr=%q", exitCode, relayGateExitCode, stderr.String())
			}
			if !strings.Contains(stdout.String(), "Run `loopcoder relay flush") {
				t.Fatalf("stdout missing flush instruction:\n%s", stdout.String())
			}
			if !strings.Contains(stdout.String(), block) {
				t.Fatalf("stdout missing pending block verbatim:\n%s", stdout.String())
			}
		})
	}
}

func TestRelayListNonDestructiveAndFlushClears(t *testing.T) {
	repo := t.TempDir()
	block := cliPendingPrettyBlock("worker")
	rec := writePendingRelayForCLITest(t, repo, "worker", 101, block)

	var listStdout, listStderr bytes.Buffer
	exitCode := RunWithDeps([]string{"relay", "list", "--repo", repo}, &listStdout, &listStderr, Deps{})
	if exitCode != 0 {
		t.Fatalf("relay list exit = %d, stderr=%q", exitCode, listStderr.String())
	}
	for _, want := range []string{"role=worker", "pr=101", "nonce=" + rec.Nonce} {
		if !strings.Contains(listStdout.String(), want) {
			t.Fatalf("relay list missing %q:\n%s", want, listStdout.String())
		}
	}
	if strings.Contains(listStdout.String(), "TEST RELAY BLOCK") {
		t.Fatalf("relay list printed a pretty block, want metadata only:\n%s", listStdout.String())
	}
	if records := relaygate.Check(repo); len(records) != 1 {
		t.Fatalf("relay list cleared %d records, want 1 pending", 1-len(records))
	}

	var flushStdout, flushStderr bytes.Buffer
	exitCode = RunWithDeps([]string{"relay", "flush", "--repo", repo}, &flushStdout, &flushStderr, Deps{})
	if exitCode != 0 {
		t.Fatalf("relay flush exit = %d, stderr=%q", exitCode, flushStderr.String())
	}
	if flushStdout.String() != block {
		t.Fatalf("relay flush stdout = %q, want %q", flushStdout.String(), block)
	}
	if records := relaygate.Check(repo); len(records) != 0 {
		t.Fatalf("relay flush left %d pending records, want 0", len(records))
	}

	called := false
	var stdout, stderr bytes.Buffer
	exitCode = RunWithDeps([]string{"verify-local", "--repo", repo, "--pr-number", "101"}, &stdout, &stderr, Deps{
		Verify: func(context.Context, verify.Options) verify.Result {
			called = true
			return verify.Result{Summary: verify.Summary{Verdict: verify.StatusPass, LocalCommandGates: "not-configured"}, ExitCode: 0}
		},
	})
	if exitCode != 0 {
		t.Fatalf("verify-local after flush exit = %d, stderr=%q", exitCode, stderr.String())
	}
	if !called {
		t.Fatal("verify-local did not run after relay flush cleared pending records")
	}
}

func TestRelayFlushEmptyPrintsConfirmation(t *testing.T) {
	repo := t.TempDir()
	var stdout, stderr bytes.Buffer

	exitCode := RunWithDeps([]string{"relay", "flush", "--repo", repo}, &stdout, &stderr, Deps{})
	if exitCode != 0 {
		t.Fatalf("relay flush exit = %d, stderr=%q", exitCode, stderr.String())
	}
	if stdout.String() != "no pending relays\n" {
		t.Fatalf("relay flush stdout = %q, want no-pending confirmation", stdout.String())
	}
}

func TestRelayFlushAckFailureExitsNonZeroAndPrintsError(t *testing.T) {
	repo := t.TempDir()
	block := cliPendingPrettyBlock("worker")
	rec := writePendingRelayForCLITest(t, repo, "worker", 101, block)

	stdout := &relayFlushAckSabotageWriter{
		t:    t,
		path: filepath.Join(repo, ".loopcoder", "relay", "pending", rec.Nonce+".json"),
	}
	var stderr bytes.Buffer
	exitCode := runRelayFlush([]string{"--repo", repo}, stdout, &stderr)
	if exitCode != 1 {
		t.Fatalf("relay flush exit = %d, want 1; stderr=%q", exitCode, stderr.String())
	}
	if stdout.String() != block {
		t.Fatalf("relay flush stdout = %q, want surfaced block %q", stdout.String(), block)
	}
	for _, want := range []string{"relay flush:", "acknowledge pending relays"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr missing %q: %q", want, stderr.String())
		}
	}
}

func TestRelayGateExemptsEscapeAndInspectionCommands(t *testing.T) {
	repo := t.TempDir()
	block := cliPendingPrettyBlock("worker")
	writePendingRelayForCLITest(t, repo, "worker", 101, block)

	t.Run("relay list", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		exitCode := RunWithDeps([]string{"relay", "list", "--repo", repo}, &stdout, &stderr, Deps{})
		if exitCode != 0 {
			t.Fatalf("relay list exit = %d, stderr=%q", exitCode, stderr.String())
		}
	})
	t.Run("doctor", func(t *testing.T) {
		called := false
		var stdout, stderr bytes.Buffer
		exitCode := RunWithDeps([]string{"doctor", "--repo", repo}, &stdout, &stderr, Deps{
			Doctor: func(context.Context, doctor.Options) doctor.Report {
				called = true
				return doctor.Report{Checks: []doctor.Check{{Name: "git", Status: doctor.StatusOK, Message: "found"}}}
			},
		})
		if exitCode != 0 {
			t.Fatalf("doctor exit = %d, stderr=%q", exitCode, stderr.String())
		}
		if !called {
			t.Fatal("doctor did not run")
		}
	})
	t.Run("status", func(t *testing.T) {
		record := validDispatchReport()
		exitCode := 0
		if _, err := state.WriteAttempt(repo, "run-test", state.AttemptRecord{
			Version:        1,
			JobID:          "job-101-1",
			Issue:          101,
			Attempt:        1,
			Provider:       "codex",
			Phase:          "codex_exited",
			Status:         "succeeded",
			StartedAt:      record.StartedAt,
			HeartbeatAt:    record.EndedAt,
			LastProgressAt: record.EndedAt,
			ExitCode:       &exitCode,
			Report:         &record,
		}); err != nil {
			t.Fatalf("WriteAttempt: %v", err)
		}
		var stdout, stderr bytes.Buffer
		gotExit := RunWithDeps([]string{"status", "--repo", repo, "--run", "run-test"}, &stdout, &stderr, Deps{})
		if gotExit != 0 {
			t.Fatalf("status exit = %d, stderr=%q", gotExit, stderr.String())
		}
		if !strings.Contains(stdout.String(), "RUN STATUS") {
			t.Fatalf("status stdout missing report:\n%s", stdout.String())
		}
		if strings.Contains(stdout.String(), "loopcoder relay gate") {
			t.Fatalf("status was gated:\n%s", stdout.String())
		}
	})
	t.Run("attest", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		exitCode := RunWithDeps([]string{"attest", "--provider", "codex-cli", "--model", "gpt-5", "--action", "test", "--duration-ms", "1", "--total-tokens", "1"}, &stdout, &stderr, Deps{})
		if exitCode != 0 {
			t.Fatalf("attest exit = %d, stderr=%q", exitCode, stderr.String())
		}
	})
	t.Run("help", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		exitCode := RunWithDeps([]string{"dispatch", "--help"}, &stdout, &stderr, Deps{})
		if exitCode != 0 {
			t.Fatalf("help exit = %d, stderr=%q", exitCode, stderr.String())
		}
	})
	t.Run("version", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		exitCode := RunWithDeps([]string{"--version"}, &stdout, &stderr, Deps{})
		if exitCode != 0 {
			t.Fatalf("version exit = %d, stderr=%q", exitCode, stderr.String())
		}
	})
	t.Run("relay flush", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		exitCode := RunWithDeps([]string{"relay", "flush", "--repo", repo}, &stdout, &stderr, Deps{})
		if exitCode != 0 {
			t.Fatalf("relay flush exit = %d, stderr=%q", exitCode, stderr.String())
		}
		if stdout.String() != block {
			t.Fatalf("relay flush stdout = %q, want %q", stdout.String(), block)
		}
	})
}

func TestRelayGateNeverGatesAutomationCommands(t *testing.T) {
	repo := t.TempDir()
	writePendingRelayForCLITest(t, repo, "worker", 101, cliPendingPrettyBlock("worker"))

	tests := []struct {
		name string
		args []string
	}{
		{name: "hook conductor-reporter", args: []string{"hook", "conductor-reporter"}},
		{name: "hook conductor-attest", args: []string{"hook", "conductor-attest"}},
		{name: "hook conductor-relay-guard", args: []string{"hook", "conductor-relay-guard"}},
		{name: "attest", args: []string{"attest", "--provider", "codex-cli", "--model", "gpt-5", "--action", "test", "--duration-ms", "1", "--total-tokens", "1"}},
		{name: "version", args: []string{"version"}},
		{name: "root help", args: []string{"--help"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			exitCode := RunWithDeps(tt.args, &stdout, &stderr, Deps{})
			if exitCode != 0 {
				t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
			}
			if strings.Contains(stdout.String(), "loopcoder relay gate") || strings.Contains(stderr.String(), "loopcoder relay gate") {
				t.Fatalf("automation command was gated; stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestRelayGateFailOpenOnCorruptState(t *testing.T) {
	repo := t.TempDir()
	pendingDir := filepath.Join(repo, ".loopcoder", "relay", "pending")
	if err := os.MkdirAll(pendingDir, 0o755); err != nil {
		t.Fatalf("mkdir pending: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pendingDir, "bad.json"), []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("write corrupt pending: %v", err)
	}

	called := false
	var stdout, stderr bytes.Buffer
	exitCode := RunWithDeps([]string{"verify-local", "--repo", repo, "--pr-number", "101"}, &stdout, &stderr, Deps{
		Verify: func(context.Context, verify.Options) verify.Result {
			called = true
			return verify.Result{Summary: verify.Summary{Verdict: verify.StatusPass, LocalCommandGates: "not-configured"}, ExitCode: 0}
		},
	})
	if exitCode != 0 {
		t.Fatalf("verify-local with corrupt relay state exit = %d, stderr=%q", exitCode, stderr.String())
	}
	if !called {
		t.Fatal("verify-local did not run through corrupt relay state")
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = RunWithDeps([]string{"relay", "flush", "--repo", repo}, &stdout, &stderr, Deps{})
	if exitCode != 0 {
		t.Fatalf("relay flush with corrupt state exit = %d, stderr=%q", exitCode, stderr.String())
	}
}

func TestRelayGateWarnsAndFailsOpenOnRealPendingReadError(t *testing.T) {
	repo := t.TempDir()
	relayDir := filepath.Join(repo, ".loopcoder", "relay")
	if err := os.MkdirAll(relayDir, 0o755); err != nil {
		t.Fatalf("mkdir relay dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(relayDir, "pending"), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write pending file: %v", err)
	}

	called := false
	var stdout, stderr bytes.Buffer
	exitCode := RunWithDeps([]string{"verify-local", "--repo", repo, "--pr-number", "101"}, &stdout, &stderr, Deps{
		Verify: func(context.Context, verify.Options) verify.Result {
			called = true
			return verify.Result{Summary: verify.Summary{Verdict: verify.StatusPass, LocalCommandGates: "not-configured"}, ExitCode: 0}
		},
	})
	if exitCode != 0 {
		t.Fatalf("verify-local with relay read error exit = %d, stderr=%q", exitCode, stderr.String())
	}
	if !called {
		t.Fatal("verify-local did not run through relay read error")
	}
	if !strings.Contains(stderr.String(), "relay gate: could not read pending records:") || !strings.Contains(stderr.String(), "proceeding") {
		t.Fatalf("stderr missing relay warning: %q", stderr.String())
	}
}

func TestRelayGateMissingPendingDirectoryStaysSilent(t *testing.T) {
	repo := t.TempDir()

	called := false
	var stdout, stderr bytes.Buffer
	exitCode := RunWithDeps([]string{"verify-local", "--repo", repo, "--pr-number", "101"}, &stdout, &stderr, Deps{
		Verify: func(context.Context, verify.Options) verify.Result {
			called = true
			return verify.Result{Summary: verify.Summary{Verdict: verify.StatusPass, LocalCommandGates: "not-configured"}, ExitCode: 0}
		},
	})
	if exitCode != 0 {
		t.Fatalf("verify-local with missing relay state exit = %d, stderr=%q", exitCode, stderr.String())
	}
	if !called {
		t.Fatal("verify-local did not run with missing relay state")
	}
	if strings.Contains(stderr.String(), "relay gate:") {
		t.Fatalf("stderr contains relay warning for missing directory: %q", stderr.String())
	}
}

func TestLoadDeliveryConfigLoudResolution(t *testing.T) {
	baseConfig := []byte("version: 1\nworker:\n  model: base-worker-model\nci:\n  checks: [verify]\n")
	tests := []struct {
		name           string
		configFromBase bool
		show           config.ShowBaseConfigFunc
		wantErr        bool
		wantModel      string
		wantChecks     []string
	}{
		{
			name: "cwd lacks and base has errors loud",
			show: func(context.Context, string, string) ([]byte, error) {
				return baseConfig, nil
			},
			wantErr: true,
		},
		{
			name:           "config-from-base reads base config",
			configFromBase: true,
			show: func(context.Context, string, string) ([]byte, error) {
				return baseConfig, nil
			},
			wantModel:  "base-worker-model",
			wantChecks: []string{"verify"},
		},
		{
			name: "no config anywhere uses defaults",
			show: func(context.Context, string, string) ([]byte, error) {
				return nil, os.ErrNotExist
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadDeliveryConfigWithOptions(t.TempDir(), config.LoadOptions{
				BaseBranch:     "main",
				ConfigFromBase: tt.configFromBase,
				ShowBaseConfig: tt.show,
			})
			if tt.wantErr {
				var mismatch config.ConfigMismatchError
				if !errors.As(err, &mismatch) {
					t.Fatalf("err = %v, want ConfigMismatchError", err)
				}
				if !strings.Contains(err.Error(), "probably the wrong branch") {
					t.Fatalf("mismatch error = %q", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("loadDeliveryConfigWithOptions returned error: %v", err)
			}
			if tt.wantModel != "" && cfg.Worker.Model != tt.wantModel {
				t.Fatalf("worker model = %q, want %q", cfg.Worker.Model, tt.wantModel)
			}
			if tt.wantChecks != nil && !reflect.DeepEqual(cfg.CI.Checks, tt.wantChecks) {
				t.Fatalf("CI checks = %#v, want %#v", cfg.CI.Checks, tt.wantChecks)
			}
			if tt.wantModel == "" && cfg.Resilience.Worker.HardCapSeconds != 2700 {
				t.Fatalf("default worker hard cap = %d, want 2700", cfg.Resilience.Worker.HardCapSeconds)
			}
		})
	}
}

func TestLoopreviewHelpDocumentsFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := Run([]string{"loopreview", "--help"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("Run returned exit code %d, want 0", exitCode)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	help := stdout.String()
	for _, want := range []string{"loopcoder loopreview", "--repo", "--pr-number", "--provider", "--base-branch", "--model", "--effort", "--strict", "--timeout", "--pretty", "--no-pretty", "LOOPCODER_PRETTY", "LOOPCODER_NO_PRETTY", "Exit codes:", "0   clean verifier verdict: pass", "1   clean verifier verdict: fail", "2   clean verifier verdict: needs-human", "3   command failure", "4   pending local relay block"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q:\n%s", want, help)
		}
	}
}

func TestReadmeDocumentsLoopreviewExitCodeMap(t *testing.T) {
	readme := readRepoFile(t, "README.md")
	for _, want := range []string{
		"loopreview exit codes",
		"`0` means clean verifier verdict `pass`",
		"`1` means clean verifier verdict `fail`",
		"`2` means clean verifier verdict `needs-human`",
		"`3` means the `loopreview` command itself failed",
		"`4` is reserved for the cross-command relay hard gate",
	} {
		if !strings.Contains(readme, want) {
			t.Fatalf("README missing %q", want)
		}
	}
}

func TestStatusHelpDocumentsFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := Run([]string{"status", "--help"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("Run returned exit code %d, want 0", exitCode)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	help := stdout.String()
	for _, want := range []string{"loopcoder status", "--repo", "--run", "latest modified local run"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q:\n%s", want, help)
		}
	}
}

func TestStatusRendersLocalRunState(t *testing.T) {
	repo := t.TempDir()
	record := validDispatchReport()
	exitCode := 0
	if _, err := state.WriteAttempt(repo, "run-test", state.AttemptRecord{
		Version:        1,
		JobID:          "job-101-1",
		Issue:          101,
		Attempt:        1,
		Provider:       "codex",
		PID:            1234,
		Phase:          "codex_exited",
		Status:         "succeeded",
		StartedAt:      record.StartedAt,
		HeartbeatAt:    record.EndedAt,
		LastProgressAt: record.EndedAt,
		LogBytes:       55,
		ExitCode:       &exitCode,
		Report:         &record,
	}); err != nil {
		t.Fatalf("WriteAttempt: %v", err)
	}

	var stdout, stderr bytes.Buffer
	gotExit := Run([]string{"status", "--repo", repo, "--run", "run-test"}, &stdout, &stderr)
	if gotExit != 0 {
		t.Fatalf("Run returned exit code %d, stderr=%q", gotExit, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{
		"RUN STATUS",
		"RunId: run-test (requested run)",
		"| #101 | job-101-1 | not reported | codex | gpt-5.5 | parsed | high | write | 42s | 120 | 34 | 154 | true | codex_exited | succeeded |",
		"status is read-only and local-only",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("status output missing %q:\n%s", want, output)
		}
	}
}

func TestStatusMissingRunReturnsClearError(t *testing.T) {
	repo := t.TempDir()
	var stdout, stderr bytes.Buffer

	exitCode := Run([]string{"status", "--repo", repo, "--run", "run-missing"}, &stdout, &stderr)
	if exitCode != 1 {
		t.Fatalf("Run returned exit code %d, want 1", exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), `status: run "run-missing" not found`) {
		t.Fatalf("stderr missing clear status error:\n%s", stderr.String())
	}
}

func TestDispatchHelpDocumentsProviderAgnosticModelEffortFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := Run([]string{"dispatch", "--help"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("Run returned exit code %d, want 0", exitCode)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	help := stdout.String()
	for _, want := range []string{"--model string", "worker model override", "--effort string", "worker reasoning effort override", "--strict", "--pretty", "--no-pretty", "LOOPCODER_PRETTY", "LOOPCODER_NO_PRETTY"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q:\n%s", want, help)
		}
	}
	if strings.Contains(help, "Codex model") || strings.Contains(help, "Codex reasoning") {
		t.Fatalf("dispatch help still describes model/effort as Codex-specific:\n%s", help)
	}
}

func TestDispatchWaveHelpDocumentsPrettyFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := Run([]string{"dispatch-wave", "--help"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("Run returned exit code %d, want 0", exitCode)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	help := stdout.String()
	for _, want := range []string{"loopcoder dispatch-wave", "--strict", "--pretty", "--no-pretty", "LOOPCODER_PRETTY", "LOOPCODER_NO_PRETTY", "plain on non-TTY"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q:\n%s", want, help)
		}
	}
}

func TestAttestHelpDocumentsFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := Run([]string{"attest", "--help"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("Run returned exit code %d, want 0", exitCode)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	help := stdout.String()
	for _, want := range []string{
		"loopcoder attest",
		"--role",
		"--provider",
		"--model",
		"--effort",
		"--permission",
		"--action",
		"--exit-code",
		"--started-at",
		"--ended-at",
		"--duration-ms",
		"--input-tokens",
		"--output-tokens",
		"--total-tokens",
		"--model-source",
		"--verified",
		"--pretty",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q:\n%s", want, help)
		}
	}
}

func TestDoctorHelpDocumentsFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := Run([]string{"doctor", "--help"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("Run returned exit code %d, want 0", exitCode)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	help := stdout.String()
	for _, want := range []string{"loopcoder doctor", "--repo", "--format"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q:\n%s", want, help)
		}
	}
}

func TestDoctorRunsWithInjectedDepsAndAliases(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	called := false

	exitCode := RunWithDeps([]string{
		"doctor",
		"-Repo", repo,
	}, &stdout, &stderr, Deps{
		BuildInfo: BuildInfo{
			Version: "0.3.1",
			Commit:  "abc123",
			Date:    "2026-06-29T00:00:00Z",
		},
		Doctor: func(_ context.Context, opts doctor.Options) doctor.Report {
			called = true
			if opts.RepoPath != repo {
				t.Fatalf("RepoPath = %q, want %q", opts.RepoPath, repo)
			}
			if opts.BuildInfo.Version != "0.3.1" || opts.BuildInfo.Commit != "abc123" || opts.BuildInfo.Date != "2026-06-29T00:00:00Z" {
				t.Fatalf("BuildInfo = %#v", opts.BuildInfo)
			}
			return doctor.Report{Checks: []doctor.Check{{
				Name:    "git",
				Status:  doctor.StatusOK,
				Message: "found",
			}}}
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if !called {
		t.Fatal("Doctor dependency was not called")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if stdout.String() != "[ok] git: found\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestDoctorRendersJSONFormat(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	called := false

	exitCode := RunWithDeps([]string{
		"doctor",
		"--repo", repo,
		"--format", "json",
	}, &stdout, &stderr, Deps{
		BuildInfo: BuildInfo{
			Version: "0.6.1",
			Commit:  "abc123",
			Date:    "2026-07-08T00:00:00Z",
		},
		Doctor: func(_ context.Context, opts doctor.Options) doctor.Report {
			called = true
			return doctor.Report{Checks: []doctor.Check{{
				Name:       "tracked .loopcoder",
				Status:     doctor.StatusFail,
				Message:    "tracked",
				Hard:       true,
				FixCommand: "git rm -r --cached .loopcoder && echo .loopcoder/ >> .git/info/exclude",
			}}}
		},
	})
	if exitCode != 1 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if !called {
		t.Fatal("Doctor dependency was not called")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	var payload struct {
		RepoPath string `json:"repo_path"`
		Version  string `json:"version"`
		Commit   string `json:"commit"`
		Date     string `json:"date"`
		ExitCode int    `json:"exit_code"`
		Checks   []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Hard       bool   `json:"hard"`
			Message    string `json:"message"`
			FixCommand string `json:"fix_command"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal: %v\n%s", err, stdout.String())
	}
	if payload.RepoPath != repo || payload.Version != "0.6.1" || payload.Commit != "abc123" || payload.Date != "2026-07-08T00:00:00Z" {
		t.Fatalf("metadata = %#v", payload)
	}
	if payload.ExitCode != 1 || len(payload.Checks) != 1 {
		t.Fatalf("payload = %#v", payload)
	}
	if payload.Checks[0].Name != "tracked .loopcoder" || payload.Checks[0].Status != "fail" || !payload.Checks[0].Hard || payload.Checks[0].FixCommand == "" {
		t.Fatalf("check = %#v", payload.Checks[0])
	}
}

func TestDoctorRejectsUnsupportedFormat(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	called := false

	exitCode := RunWithDeps([]string{
		"doctor",
		"--repo", repo,
		"--format", "yaml",
	}, &stdout, &stderr, Deps{
		Doctor: func(context.Context, doctor.Options) doctor.Report {
			called = true
			return doctor.Report{}
		},
	})
	if exitCode != 2 {
		t.Fatalf("RunWithDeps returned exit code %d, want 2", exitCode)
	}
	if called {
		t.Fatal("Doctor dependency should not be called for invalid format")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "unsupported --format") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestDoctorPassesFixFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	called := false

	exitCode := RunWithDeps([]string{
		"doctor",
		"--repo", repo,
		"--fix",
	}, &stdout, &stderr, Deps{
		Doctor: func(_ context.Context, opts doctor.Options) doctor.Report {
			called = true
			if opts.RepoPath != repo {
				t.Fatalf("RepoPath = %q, want %q", opts.RepoPath, repo)
			}
			if !opts.Fix {
				t.Fatal("Fix = false, want true")
			}
			return doctor.Report{Checks: []doctor.Check{{
				Name:    "fix .delivery.yml",
				Status:  doctor.StatusOK,
				Message: "unchanged",
			}}}
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if !called {
		t.Fatal("Doctor dependency was not called")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if stdout.String() != "[ok] fix .delivery.yml: unchanged\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestInitHelpDocumentsFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := Run([]string{"init", "--help"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("Run returned exit code %d, want 0", exitCode)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	help := stdout.String()
	for _, want := range []string{"loopcoder init", "--force", "--repo", "--gate", "--worker-model", "--worker-effort", "--verifier-model", "--verifier-effort"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q:\n%s", want, help)
		}
	}
}

func TestInitRunsWithInjectedDepsAndAliases(t *testing.T) {
	var stdout, stderr bytes.Buffer
	called := false
	repo := t.TempDir()

	exitCode := RunWithDeps([]string{
		"init",
		"-Repo", repo,
		"-Gate", "auto",
		"-Force",
		"-WorkerModel", "gpt-5",
		"-WorkerEffort", "high",
		"-VerifierModel", "claude-sonnet",
		"-VerifierEffort", "max",
	}, &stdout, &stderr, Deps{
		Init: func(_ context.Context, opts scaffold.Options) (scaffold.Result, error) {
			called = true
			if opts.RepoPath != repo {
				t.Fatalf("RepoPath = %q, want %q", opts.RepoPath, repo)
			}
			if opts.Gate != "auto" || !opts.Force || opts.WorkerModel != "gpt-5" || opts.WorkerEffort != "high" || opts.VerifierModel != "claude-sonnet" || opts.VerifierEffort != "max" {
				t.Fatalf("init opts = %#v", opts)
			}
			return scaffold.Result{
				Files: []scaffold.FileResult{
					{Path: ".delivery.yml", Status: scaffold.FileOverwritten},
					{Path: "ROADMAP.md", Status: scaffold.FileExists},
				},
				Labels: []scaffold.LabelResult{
					{Name: "delivery:unit", Status: scaffold.LabelCreated},
				},
				LocalStateExclude: &scaffold.LocalStateResult{Path: filepath.Join(repo, ".git", "info", "exclude"), Status: gitlocal.ProtectUpdated},
				Warnings:          []string{"gh label setup skipped: gh not found"},
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if !called {
		t.Fatal("Init dependency was not called")
	}
	for _, want := range []string{"loopcoder init complete", "overwritten .delivery.yml", "exists ROADMAP.md", "created label delivery:unit", "local-state updated"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout.String())
		}
	}
	if !strings.Contains(stderr.String(), "[loopcoder] warning: gh label setup skipped") {
		t.Fatalf("stderr missing warning:\n%s", stderr.String())
	}
}

func TestUpgradeHelpDocumentsFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := Run([]string{"upgrade", "--help"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("Run returned exit code %d, want 0", exitCode)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	help := stdout.String()
	for _, want := range []string{"loopcoder upgrade", "--version", "latest stable"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q:\n%s", want, help)
		}
	}
}

func TestUpgradeRunsWithInjectedDepsAndAliases(t *testing.T) {
	var stdout, stderr bytes.Buffer
	called := false

	exitCode := RunWithDeps([]string{
		"upgrade",
		"-Version", "v0.3.3",
	}, &stdout, &stderr, Deps{
		BuildInfo: BuildInfo{
			Version: "v0.3.2",
			Commit:  "abc123",
			Date:    "2026-06-29T00:00:00Z",
		},
		Upgrade: func(_ context.Context, opts upgrade.Options) (upgrade.Result, error) {
			called = true
			if opts.RequestedVersion != "v0.3.3" || opts.CurrentVersion != "v0.3.2" {
				t.Fatalf("upgrade opts = %#v", opts)
			}
			return upgrade.Result{
				CurrentPath:       "/old/loopcoder",
				CurrentVersion:    opts.CurrentVersion,
				TargetVersion:     "v0.3.3",
				Platform:          "linux/amd64",
				AssetName:         "loopcoder_0.3.3_linux_amd64.tar.gz",
				VersionBinaryPath: "/home/.loopcoder/versions/v0.3.3/loopcoder",
				StableBinaryPath:  "/home/.loopcoder/bin/loopcoder",
				SkillRefresh: upgrade.SkillRefreshResult{
					BinaryPath: "/home/.loopcoder/bin/loopcoder",
					Dir:        "/home/.claude/skills/loopcoder",
					Files: []upgrade.SkillRefreshFileResult{
						{Path: "/home/.claude/skills/loopcoder/SKILL.md", Status: upgrade.SkillRefreshFileUpdated},
						{Path: "/home/.claude/skills/loopcoder/AGENTS.md", Status: upgrade.SkillRefreshFileUnchanged},
					},
				},
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if !called {
		t.Fatal("Upgrade dependency was not called")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	for _, want := range []string{
		"Current selected binary: path=/old/loopcoder version=v0.3.2",
		"Resolved target version: v0.3.3",
		"Before: path=/old/loopcoder version=v0.3.2",
		"After: path=/home/.loopcoder/bin/loopcoder version=v0.3.3",
		"Skill refresh: /home/.loopcoder/bin/loopcoder skill install --global-only",
		"updated /home/.claude/skills/loopcoder/SKILL.md",
		"unchanged /home/.claude/skills/loopcoder/AGENTS.md",
		"Run: loopcoder doctor",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestUpgradeRendersSkillRefreshWarning(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := RunWithDeps([]string{"upgrade"}, &stdout, &stderr, Deps{
		BuildInfo: BuildInfo{
			Version: "v0.3.2",
		},
		Upgrade: func(_ context.Context, opts upgrade.Options) (upgrade.Result, error) {
			return upgrade.Result{
				CurrentPath:       "/old/loopcoder",
				CurrentVersion:    opts.CurrentVersion,
				TargetVersion:     "v0.3.3",
				Platform:          "linux/amd64",
				AssetName:         "loopcoder_0.3.3_linux_amd64.tar.gz",
				VersionBinaryPath: "/home/.loopcoder/versions/v0.3.3/loopcoder",
				StableBinaryPath:  "/home/.loopcoder/bin/loopcoder",
				SkillRefresh: upgrade.SkillRefreshResult{
					BinaryPath: "/home/.loopcoder/bin/loopcoder",
					Warning:    "permission denied",
				},
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Skill refresh: /home/.loopcoder/bin/loopcoder skill install --global-only") {
		t.Fatalf("stdout missing skill refresh line:\n%s", stdout.String())
	}
	for _, want := range []string{"[loopcoder] warning: skill refresh failed after upgrade", "permission denied", "run: loopcoder skill install --global-only"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr.String())
		}
	}
}

func TestUpgradeRenders060MigrationStatus(t *testing.T) {
	var stdout, stderr bytes.Buffer
	configEntry, ok := migration.ReporterRenameEntry(migration.LegacyReportConfigRoot)
	if !ok {
		t.Fatal("missing config migration entry")
	}
	envEntry, ok := migration.ReporterRenameEntry(migration.LegacyReporterScopeEnv)
	if !ok {
		t.Fatal("missing env migration entry")
	}
	hookEntry, ok := migration.ReporterRenameEntry(migration.LegacyReporterHookCommand)
	if !ok {
		t.Fatal("missing hook migration entry")
	}

	exitCode := RunWithDeps([]string{"upgrade", "--version", "v0.6.0"}, &stdout, &stderr, Deps{
		BuildInfo: BuildInfo{
			Version: "v0.5.4",
			Commit:  "abc123",
			Date:    "2026-07-08T00:00:00Z",
		},
		Upgrade: func(_ context.Context, opts upgrade.Options) (upgrade.Result, error) {
			return upgrade.Result{
				CurrentPath:       "/old/loopcoder",
				CurrentVersion:    opts.CurrentVersion,
				CurrentCommit:     opts.CurrentCommit,
				CurrentDate:       opts.CurrentDate,
				RequestedVersion:  opts.RequestedVersion,
				TargetVersion:     "v0.6.0",
				Platform:          "linux/amd64",
				AssetName:         "loopcoder_0.6.0_linux_amd64.tar.gz",
				VersionBinaryPath: "/home/.loopcoder/versions/v0.6.0/loopcoder",
				StableBinaryPath:  "/home/.loopcoder/bin/loopcoder",
				VersionStatus: upgrade.VersionStatus{
					CurrentClassification:      upgrade.VersionPreBreaking,
					TargetClassification:       upgrade.VersionBreakingTransition,
					BreakingBoundary:           true,
					CompatibilityAliasesActive: true,
				},
				SkillRefresh: upgrade.SkillRefreshResult{
					BinaryPath: "/home/.loopcoder/bin/loopcoder",
					Dir:        "/home/.claude/skills/loopcoder",
					Files: []upgrade.SkillRefreshFileResult{
						{Path: "/home/.claude/skills/loopcoder/SKILL.md", Status: upgrade.SkillRefreshFileUpdated},
						{Path: "/home/.claude/skills/loopcoder/AGENTS.md", Status: upgrade.SkillRefreshFileUpdated},
					},
				},
				MigrationStatus: upgrade.MigrationStatus{
					RepoPath:            "/repo",
					RepoAvailable:       true,
					ConfigPresent:       true,
					DeliveryVersion:     "1",
					MinLoopcoderVersion: "0.5.0",
					ConfigDiagnostics: []migration.Diagnostic{
						migration.NewDiagnostic(configEntry, false, ""),
					},
					EnvDiagnostics: []migration.Diagnostic{
						migration.NewDiagnostic(envEntry, false, ""),
					},
					HookDiagnostics: []migration.Diagnostic{
						migration.NewDiagnostic(hookEntry, false, "found in .claude/settings.json"),
					},
					OldSurfaceDiagnostics: []upgrade.OldSurfaceDiagnostic{
						{
							Surface:    "state-key",
							Legacy:     migration.LegacyReportStateKey,
							Current:    migration.ReportStateKey,
							Location:   "/repo/.loopcoder/runs/run/workers/job.attempt.json",
							FixCommand: "loopcoder doctor --repo . --fix",
							Detail:     "legacy report result key is still present",
						},
					},
				},
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	for _, want := range []string{
		"Current selected binary: path=/old/loopcoder version=v0.5.4 commit=abc123 date=2026-07-08T00:00:00Z",
		"Requested target: v0.6.0",
		"Upgrade version status: current=v0.5.4 (pre-breaking) target=v0.6.0 (breaking transition)",
		"0.5.x -> 0.6.0 boundary detected",
		"verified managed files: SKILL.md, AGENTS.md",
		"Migration status:",
		"delivery version: schema=1 min_loopcoder_version=0.5.0",
		"config: legacy config-key \"" + migration.LegacyReportConfigRoot + "\" accepted as \"" + migration.ReportConfigRoot + "\"",
		"env: legacy env \"" + migration.LegacyReporterScopeEnv + "\" accepted as \"" + migration.ReporterScopeEnv + "\"",
		"hook: legacy hook-command \"" + migration.LegacyReporterHookCommand + "\" accepted as \"" + migration.ReporterHookCommand + "\"",
		"old local state: legacy state-key \"" + migration.LegacyReportStateKey + "\" accepted as \"" + migration.ReportStateKey + "\"",
		"fix: loopcoder doctor --repo . --fix",
		"Run: loopcoder doctor --repo .",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestUpgradeRendersAlreadyLatest(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := RunWithDeps([]string{"upgrade"}, &stdout, &stderr, Deps{
		BuildInfo: BuildInfo{
			Version: "0.3.3",
			Commit:  "abc123",
			Date:    "2026-06-29T00:00:00Z",
		},
		Upgrade: func(_ context.Context, opts upgrade.Options) (upgrade.Result, error) {
			if opts.CurrentVersion != "0.3.3" {
				t.Fatalf("CurrentVersion = %q, want 0.3.3", opts.CurrentVersion)
			}
			return upgrade.Result{
				CurrentPath:    "/home/.loopcoder/bin/loopcoder",
				CurrentVersion: opts.CurrentVersion,
				TargetVersion:  "v0.3.3",
				Platform:       "linux/amd64",
				AlreadyLatest:  true,
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	for _, want := range []string{
		"Current selected binary: path=/home/.loopcoder/bin/loopcoder version=0.3.3",
		"Resolved target version: v0.3.3",
		"Already latest; no download needed.",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout.String())
		}
	}
	for _, unwanted := range []string{"Installed versioned binary", "Stable selected binary", "After:"} {
		if strings.Contains(stdout.String(), unwanted) {
			t.Fatalf("stdout included install line %q:\n%s", unwanted, stdout.String())
		}
	}
}

func TestAttestSuccessPaths(t *testing.T) {
	now := time.Date(2026, 6, 28, 1, 2, 3, 0, time.UTC)
	tests := []struct {
		name           string
		args           []string
		wantStartedAt  string
		wantEndedAt    string
		wantDurationMS int64
		wantInput      *int64
		wantOutput     *int64
		wantTotal      *int64
	}{
		{
			name: "duration total tokens and forced trust markers",
			args: []string{
				"attest",
				"--provider", "codex-cli",
				"--model", "gpt-5",
				"--effort", "high",
				"--action", "implement issue #175",
				"--duration-ms", "2000",
				"--total-tokens", "123",
				"--model-source", "parsed",
				"--verified=true",
			},
			wantStartedAt:  "2026-06-28T01:02:01Z",
			wantEndedAt:    "2026-06-28T01:02:03Z",
			wantDurationMS: 2000,
			wantTotal:      int64TestPtr(123),
		},
		{
			name: "timestamp pair split tokens and aliases",
			args: []string{
				"attest",
				"-Role", "conductor",
				"-Provider", "claude-code",
				"-Model", "opus",
				"-Permission", "orchestrate",
				"-Action", "review run",
				"-ExitCode", "0",
				"-StartedAt", "2026-06-28T00:00:00Z",
				"-EndedAt", "2026-06-28T00:00:01Z",
				"-InputTokens", "10",
				"-OutputTokens", "20",
			},
			wantStartedAt:  "2026-06-28T00:00:00Z",
			wantEndedAt:    "2026-06-28T00:00:01Z",
			wantDurationMS: 1000,
			wantInput:      int64TestPtr(10),
			wantOutput:     int64TestPtr(20),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			exitCode := RunWithDeps(tt.args, &stdout, &stderr, Deps{
				Now: func() time.Time {
					return now
				},
			})
			if exitCode != 0 {
				t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}

			lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
			if len(lines) != 2 {
				t.Fatalf("stdout lines = %d, want 2:\n%s", len(lines), stdout.String())
			}
			var record reporter.Report
			if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
				t.Fatalf("stdout first line is not report JSON: %v\n%s", err, stdout.String())
			}
			if err := record.Validate(); err != nil {
				t.Fatalf("report JSON does not validate: %v", err)
			}
			if record.ModelSource != reporter.ModelSourceSelfReported {
				t.Fatalf("ModelSource = %q, want self-reported", record.ModelSource)
			}
			if record.Verified {
				t.Fatal("Verified = true, want false")
			}
			if record.StartedAt != tt.wantStartedAt || record.EndedAt != tt.wantEndedAt || record.DurationMS != tt.wantDurationMS {
				t.Fatalf("timing = (%q, %q, %d), want (%q, %q, %d)", record.StartedAt, record.EndedAt, record.DurationMS, tt.wantStartedAt, tt.wantEndedAt, tt.wantDurationMS)
			}
			assertOptionalInt64(t, "input", record.Usage.InputTokens, tt.wantInput)
			assertOptionalInt64(t, "output", record.Usage.OutputTokens, tt.wantOutput)
			assertOptionalInt64(t, "total", record.Usage.TotalTokens, tt.wantTotal)
			if lines[1] != record.Header() {
				t.Fatalf("header line = %q, want %q", lines[1], record.Header())
			}
		})
	}
}

func TestAttestPrettyRendersEmojiWhenInteractive(t *testing.T) {
	clearPrettyEnv(t)
	var stdout, stderr bytes.Buffer

	exitCode := RunWithDeps([]string{
		"attest",
		"--pretty",
		"--provider", "codex-cli",
		"--model", "gpt-5",
		"--effort", "xhigh",
		"--action", "merge PR #214",
		"--duration-ms", "72000",
		"--total-tokens", "18266",
	}, &stdout, &stderr, Deps{
		Now: func() time.Time {
			return time.Date(2026, 6, 28, 0, 1, 12, 0, time.UTC)
		},
		IsTerminal: func(io.Writer) bool {
			return true
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	got := stdout.String()
	for _, want := range []string{
		"\u26a0\ufe0f loopcoder report: conductor self reported",
		"- conductor: codex-cli / gpt-5 (xhigh) (self-reported) / xhigh",
		"- tokens: total=18,266",
		"- verified: false",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stdout missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "[attestation]") || strings.Contains(got, `"role":"conductor"`) {
		t.Fatalf("pretty stdout includes durable output:\n%s", got)
	}
}

func TestAttestPrettyRendersPlainWhenNonInteractive(t *testing.T) {
	clearPrettyEnv(t)
	var stdout, stderr bytes.Buffer

	exitCode := RunWithDeps([]string{
		"attest",
		"--pretty",
		"--provider", "codex-cli",
		"--model", "gpt-5",
		"--action", "dispatch issue #41",
		"--duration-ms", "120000",
		"--total-tokens", "12345",
	}, &stdout, &stderr, Deps{
		Now: func() time.Time {
			return time.Date(2026, 6, 28, 0, 2, 0, 0, time.UTC)
		},
		IsTerminal: func(io.Writer) bool {
			return false
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	got := stdout.String()
	for _, want := range []string{
		"loopcoder report: conductor self reported",
		"- conductor: codex-cli / gpt-5 (self-reported) / unset",
		"- tokens: total=12,345",
		"- verified: false",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stdout missing %q:\n%s", want, got)
		}
	}
	for _, disallowed := range []string{"\u2705", "\u274c", "\u26a0", "\x1b["} {
		if strings.Contains(got, disallowed) {
			t.Fatalf("plain pretty stdout contains %q:\n%s", disallowed, got)
		}
	}
}

func TestAttestValidationHardFails(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		wants []string
	}{
		{
			name: "missing identity and usage",
			args: []string{"attest", "--duration-ms", "1"},
			wants: []string{
				"invalid report",
				"provider is required",
				"model is required",
				"action is required",
				"usage is required",
			},
		},
		{
			name: "missing timing",
			args: []string{
				"attest",
				"--provider", "codex-cli",
				"--model", "gpt-5",
				"--action", "implement issue #175",
				"--total-tokens", "123",
			},
			wants: []string{
				"invalid report",
				"started_at is required",
				"ended_at is required",
				"duration_ms is required",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			exitCode := RunWithDeps(tt.args, &stdout, &stderr, Deps{
				Now: func() time.Time {
					return time.Date(2026, 6, 28, 1, 2, 3, 0, time.UTC)
				},
			})
			if exitCode == 0 {
				t.Fatalf("RunWithDeps returned exit code 0, want non-zero")
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			for _, want := range tt.wants {
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("stderr missing %q:\n%s", want, stderr.String())
				}
			}
		})
	}
}

func TestLoopreviewRunsWithInjectedDepsAndAliases(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	called := false

	exitCode := RunWithDeps([]string{
		"loopreview",
		"-Repo", repo,
		"-PrNumber", "152",
		"-Provider", "claude",
		"-BaseBranch", "trunk",
		"-Model", "claude-opus-4-8[1m]",
		"-Effort", "max",
		"-Timeout", "15s",
	}, &stdout, &stderr, Deps{
		Loopreview: func(_ context.Context, opts loopreview.Options) (loopreview.Result, error) {
			called = true
			if opts.RepoPath != repo || opts.PRNumber != 152 || opts.Provider != "claude" || opts.BaseBranch != "trunk" || opts.Timeout != 15*time.Second {
				t.Fatalf("loopreview opts = %#v", opts)
			}
			if opts.Model != "claude-opus-4-8[1m]" || opts.Effort != "max" {
				t.Fatalf("loopreview opts model/effort = %#v", opts)
			}
			if opts.Stderr == nil {
				t.Fatal("loopreview opts Stderr is nil")
			}
			return loopreview.Result{
				Verdict: loopreview.Verdict{
					Verdict:         loopreview.VerdictPass,
					Findings:        []loopreview.Finding{},
					Evidence:        "review passed",
					SpecConformance: loopreview.SpecConformancePass,
				},
				ExitCode: 0,
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if !called {
		t.Fatal("Loopreview dependency was not called")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if got["verdict"] != "pass" || got["spec_conformance"] != "pass" {
		t.Fatalf("stdout JSON has wrong verdict fields: %#v", got)
	}
}

func TestLoopreviewPrettyDefaultNonInteractiveWritesPlainToStderrWithoutChangingStdout(t *testing.T) {
	clearPrettyEnv(t)
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	record := validLoopreviewReport()
	result := loopreview.Result{
		Verdict: loopreview.Verdict{
			Verdict:         loopreview.VerdictPass,
			Findings:        []loopreview.Finding{},
			Evidence:        "review passed",
			SpecConformance: loopreview.SpecConformancePass,
			Report:          &record,
		},
		ExitCode: 0,
	}
	wantStdout := expectedLoopreviewStdout(t, result)

	exitCode := RunWithDeps([]string{
		"loopreview",
		"--repo", repo,
		"--pr-number", "152",
		"--provider", "claude",
	}, &stdout, &stderr, Deps{
		IsTerminal: func(io.Writer) bool {
			return false
		},
		Loopreview: func(context.Context, loopreview.Options) (loopreview.Result, error) {
			return result, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stdout.String() != wantStdout {
		t.Fatalf("stdout = %q, want %q", stdout.String(), wantStdout)
	}
	gotStderr := stderr.String()
	for _, want := range []string{
		"loopcoder report: verifier pass",
		"- verifier: Anthropic / claude / gpt-5.5 (high) (parsed) / high",
		"- permission: read-only",
		"- action: \"review PR #152\"",
	} {
		if !strings.Contains(gotStderr, want) {
			t.Fatalf("stderr missing %q:\n%s", want, gotStderr)
		}
	}
	for _, disallowed := range []string{"\u2705", "\u274c", "\u26a0"} {
		if strings.Contains(gotStderr, disallowed) {
			t.Fatalf("plain stderr contains %q:\n%s", disallowed, gotStderr)
		}
	}
}

func TestLoopreviewWritesRelayLedger(t *testing.T) {
	clearPrettyEnv(t)
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	record := validLoopreviewReport()
	result := loopreview.Result{
		Verdict: loopreview.Verdict{
			Verdict:         loopreview.VerdictPass,
			Findings:        []loopreview.Finding{},
			Evidence:        "review passed",
			SpecConformance: loopreview.SpecConformancePass,
			Report:          &record,
		},
		ExitCode: 0,
	}

	exitCode := RunWithDeps([]string{
		"loopreview",
		"--repo", repo,
		"--pr-number", "152",
		"--provider", "claude",
	}, &stdout, &stderr, Deps{
		Now: func() time.Time {
			return now
		},
		IsTerminal: func(io.Writer) bool {
			return false
		},
		Loopreview: func(context.Context, loopreview.Options) (loopreview.Result, error) {
			return result, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}

	pattern := filepath.Join(repo, ".loopcoder", "relay", "loopreview-pr-152", "loopreview-pr-152-*.attest")
	ledger := readSingleFile(t, pattern)
	for _, want := range []string{
		"# command=loopreview",
		"# role=verifier",
		"# pr_number=152",
		record.Header(),
		loopreviewPrettyBlock(result.Verdict, reporter.PrettyModePlain),
	} {
		if !strings.Contains(ledger, want) {
			t.Fatalf("relay ledger missing %q:\n%s", want, ledger)
		}
	}
	pending := relaygate.Check(repo)
	if len(pending) != 1 {
		t.Fatalf("pending relay records = %d, want 1", len(pending))
	}
	if pending[0].Role != "verifier" || pending[0].PRNumber != 152 || pending[0].Nonce != relaygate.Nonce("loopreview-pr-152", 152, "verifier") {
		t.Fatalf("pending relay record = %#v, want verifier PR 152 deterministic nonce", pending[0])
	}
	if pending[0].Block != loopreviewPrettyBlock(result.Verdict, reporter.PrettyModePlain)+"\n" {
		t.Fatalf("pending relay block = %q, want plain pretty block", pending[0].Block)
	}
}

func TestLoopreviewPrettyFlagWritesEmojiToStderrWithoutChangingStdout(t *testing.T) {
	clearPrettyEnv(t)
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	record := validLoopreviewReport()
	result := loopreview.Result{
		Verdict: loopreview.Verdict{
			Verdict:         loopreview.VerdictPass,
			Findings:        []loopreview.Finding{},
			Evidence:        "review passed",
			SpecConformance: loopreview.SpecConformancePass,
			Report:          &record,
		},
		ExitCode: 0,
	}
	wantStdout := expectedLoopreviewStdout(t, result)

	exitCode := RunWithDeps([]string{
		"loopreview",
		"--pretty",
		"--repo", repo,
		"--pr-number", "152",
		"--provider", "claude",
	}, &stdout, &stderr, Deps{
		IsTerminal: func(io.Writer) bool {
			return false
		},
		Loopreview: func(context.Context, loopreview.Options) (loopreview.Result, error) {
			return result, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stdout.String() != wantStdout {
		t.Fatalf("stdout = %q, want %q", stdout.String(), wantStdout)
	}
	gotStderr := stderr.String()
	for _, want := range []string{
		"\u2705 loopcoder report: verifier pass",
		"- verifier: Anthropic / claude / gpt-5.5 (high) (parsed) / high",
		"- permission: read-only",
		"- action: \"review PR #152\"",
	} {
		if !strings.Contains(gotStderr, want) {
			t.Fatalf("stderr missing %q:\n%s", want, gotStderr)
		}
	}
}

func TestLoopreviewNoPrettySuppressesStderrWithoutChangingStdout(t *testing.T) {
	clearPrettyEnv(t)
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	record := validLoopreviewReport()
	result := loopreview.Result{
		Verdict: loopreview.Verdict{
			Verdict:         loopreview.VerdictPass,
			Findings:        []loopreview.Finding{},
			Evidence:        "review passed",
			SpecConformance: loopreview.SpecConformancePass,
			Report:          &record,
		},
		ExitCode: 0,
	}
	wantStdout := expectedLoopreviewStdout(t, result)

	exitCode := RunWithDeps([]string{
		"loopreview",
		"--no-pretty",
		"--repo", repo,
		"--pr-number", "152",
		"--provider", "claude",
	}, &stdout, &stderr, Deps{
		IsTerminal: func(io.Writer) bool {
			return false
		},
		Loopreview: func(context.Context, loopreview.Options) (loopreview.Result, error) {
			return result, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stdout.String() != wantStdout {
		t.Fatalf("stdout = %q, want %q", stdout.String(), wantStdout)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestLoopreviewPrettyInteractiveHonorsNoEmojiEnv(t *testing.T) {
	clearPrettyEnv(t)
	t.Setenv("LOOPCODER_NO_EMOJI", "1")
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	record := validLoopreviewReport()

	exitCode := RunWithDeps([]string{
		"loopreview",
		"--repo", repo,
		"--pr-number", "152",
		"--provider", "claude",
	}, &stdout, &stderr, Deps{
		IsTerminal: func(io.Writer) bool {
			return true
		},
		Loopreview: func(context.Context, loopreview.Options) (loopreview.Result, error) {
			return loopreview.Result{
				Verdict: loopreview.Verdict{
					Verdict:         loopreview.VerdictPass,
					Findings:        []loopreview.Finding{},
					Evidence:        "review passed",
					SpecConformance: loopreview.SpecConformancePass,
					Report:          &record,
				},
				ExitCode: 0,
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "loopcoder report: verifier pass") {
		t.Fatalf("stderr missing plain pretty report:\n%s", stderr.String())
	}
	for _, disallowed := range []string{"\u2705", "\u274c", "\u26a0"} {
		if strings.Contains(stderr.String(), disallowed) {
			t.Fatalf("stderr contains %q despite LOOPCODER_NO_EMOJI:\n%s", disallowed, stderr.String())
		}
	}
}

func TestLoopreviewSurfacesNeedsHumanExitCode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()

	exitCode := RunWithDeps([]string{
		"loopreview",
		"--repo", repo,
		"--pr-number", "152",
		"--provider", "codex",
	}, &stdout, &stderr, Deps{
		Loopreview: func(context.Context, loopreview.Options) (loopreview.Result, error) {
			return loopreview.Result{
				Verdict: loopreview.Verdict{
					Verdict: loopreview.VerdictNeedsHuman,
					Findings: []loopreview.Finding{{
						Severity: "warning",
						File:     "",
						Note:     "needs manual review",
					}},
					Evidence:        "manual review required",
					SpecConformance: loopreview.SpecConformanceNotApplicable,
				},
				ExitCode: 2,
			}, nil
		},
	})
	if exitCode != 2 {
		t.Fatalf("RunWithDeps returned exit code %d, want 2", exitCode)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if !strings.Contains(stdout.String(), `"verdict":"needs-human"`) {
		t.Fatalf("stdout missing needs-human verdict: %s", stdout.String())
	}
}

func TestLoopreviewReturnsCleanVerdictExitCodes(t *testing.T) {
	tests := []struct {
		name     string
		verdict  string
		wantCode int
	}{
		{name: "pass", verdict: loopreview.VerdictPass, wantCode: 0},
		{name: "fail", verdict: loopreview.VerdictFail, wantCode: 1},
		{name: "needs human", verdict: loopreview.VerdictNeedsHuman, wantCode: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			repo := t.TempDir()

			exitCode := RunWithDeps([]string{
				"loopreview",
				"--repo", repo,
				"--pr-number", "152",
				"--provider", "codex",
			}, &stdout, &stderr, Deps{
				Loopreview: func(context.Context, loopreview.Options) (loopreview.Result, error) {
					return loopreview.Result{
						Verdict: loopreview.Verdict{
							Verdict:         tt.verdict,
							Findings:        []loopreview.Finding{},
							Evidence:        "review completed",
							SpecConformance: loopreview.SpecConformancePass,
						},
						ExitCode: 99,
					}, nil
				},
			})
			if exitCode != tt.wantCode {
				t.Fatalf("RunWithDeps returned exit code %d, want %d", exitCode, tt.wantCode)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
			if !strings.Contains(stdout.String(), `"verdict":"`+tt.verdict+`"`) {
				t.Fatalf("stdout missing verdict %q: %s", tt.verdict, stdout.String())
			}
		})
	}
}

func TestLoopreviewCommandFailuresUseDistinctExitCode(t *testing.T) {
	t.Run("bad repo", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		repo := filepath.Join(t.TempDir(), "missing")

		exitCode := RunWithDeps([]string{
			"loopreview",
			"--repo", repo,
			"--pr-number", "152",
			"--provider", "codex",
		}, &stdout, &stderr, Deps{})
		if exitCode != loopreviewCommandFailureExitCode {
			t.Fatalf("RunWithDeps returned exit code %d, want %d", exitCode, loopreviewCommandFailureExitCode)
		}
		if stdout.Len() != 0 {
			t.Fatalf("stdout = %q, want empty", stdout.String())
		}
		if !strings.Contains(stderr.String(), "resolve repo path") {
			t.Fatalf("stderr missing repo error: %q", stderr.String())
		}
	})

	t.Run("runtime error", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		repo := t.TempDir()

		exitCode := RunWithDeps([]string{
			"loopreview",
			"--repo", repo,
			"--pr-number", "152",
			"--provider", "codex",
		}, &stdout, &stderr, Deps{
			Loopreview: func(context.Context, loopreview.Options) (loopreview.Result, error) {
				return loopreview.Result{}, errors.New("provider crashed")
			},
		})
		if exitCode != loopreviewCommandFailureExitCode {
			t.Fatalf("RunWithDeps returned exit code %d, want %d", exitCode, loopreviewCommandFailureExitCode)
		}
		if stdout.Len() != 0 {
			t.Fatalf("stdout = %q, want empty", stdout.String())
		}
		if !strings.Contains(stderr.String(), "provider crashed") {
			t.Fatalf("stderr missing runtime error: %q", stderr.String())
		}
	})
}

func TestLoopreviewWarnsWhenVerifierMatchesConfiguredWorker(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	if err := os.WriteFile(repo+"/.delivery.yml", []byte("version: 1\nadapters:\n  worker: codex\n"), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}
	called := false

	exitCode := RunWithDeps([]string{
		"loopreview",
		"--repo", repo,
		"--pr-number", "152",
		"--provider", "codex",
	}, &stdout, &stderr, Deps{
		Loopreview: func(context.Context, loopreview.Options) (loopreview.Result, error) {
			called = true
			return loopreview.Result{
				Verdict: loopreview.Verdict{
					Verdict:         loopreview.VerdictPass,
					Findings:        []loopreview.Finding{},
					Evidence:        "review passed",
					SpecConformance: loopreview.SpecConformancePass,
				},
				ExitCode: 0,
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if !called {
		t.Fatal("Loopreview dependency was not called")
	}
	if !strings.Contains(stderr.String(), `adapters.verifier "codex" matches adapters.worker`) {
		t.Fatalf("stderr missing advisory warning: %q", stderr.String())
	}
}

func TestLoopreviewUsesVerifierConfigModelEffortWhenFlagsAbsent(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	if err := os.WriteFile(repo+"/.delivery.yml", []byte(`version: 1
adapters:
  worker: codex
verifier:
  model: config-verifier-model
  reasoning_effort: config-verifier-effort
`), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}

	exitCode := RunWithDeps([]string{
		"loopreview",
		"--repo", repo,
		"--pr-number", "152",
		"--provider", "claude",
	}, &stdout, &stderr, Deps{
		Loopreview: func(_ context.Context, opts loopreview.Options) (loopreview.Result, error) {
			if opts.Model != "config-verifier-model" || opts.Effort != "config-verifier-effort" {
				t.Fatalf("loopreview opts model/effort = %#v", opts)
			}
			return loopreview.Result{
				Verdict: loopreview.Verdict{
					Verdict:         loopreview.VerdictPass,
					Findings:        []loopreview.Finding{},
					Evidence:        "review passed",
					SpecConformance: loopreview.SpecConformancePass,
				},
				ExitCode: 0,
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
}

func TestLoopreviewUsesConfiguredVerifierProviderAndRegistryDefaults(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	if err := os.WriteFile(repo+"/.delivery.yml", []byte(`version: 1
adapters:
  verifier: claude
`), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}

	called := false
	exitCode := RunWithDeps([]string{
		"loopreview",
		"--repo", repo,
		"--pr-number", "152",
	}, &stdout, &stderr, Deps{
		Loopreview: func(_ context.Context, opts loopreview.Options) (loopreview.Result, error) {
			called = true
			if opts.Provider != "claude" || opts.Model != "claude-opus-4-8[1m]" || opts.Effort != "max" {
				t.Fatalf("loopreview opts provider/model/effort = %#v", opts)
			}
			return loopreview.Result{
				Verdict: loopreview.Verdict{
					Verdict:         loopreview.VerdictPass,
					Findings:        []loopreview.Finding{},
					Evidence:        "review passed",
					SpecConformance: loopreview.SpecConformancePass,
				},
				ExitCode: 0,
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if !called {
		t.Fatal("Loopreview dependency was not called")
	}
}

func TestLoopreviewStrictFlagRejectsInvalidVerifierSelection(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	called := false

	exitCode := RunWithDeps([]string{
		"loopreview",
		"--repo", repo,
		"--pr-number", "152",
		"--provider", "claude",
		"--model", "claude-opus-4-8[1m]",
		"--effort", "xhigh",
		"--strict",
	}, &stdout, &stderr, Deps{
		Loopreview: func(context.Context, loopreview.Options) (loopreview.Result, error) {
			called = true
			return loopreview.Result{}, nil
		},
	})
	if exitCode != loopreviewCommandFailureExitCode {
		t.Fatalf("RunWithDeps returned exit code %d, want %d", exitCode, loopreviewCommandFailureExitCode)
	}
	if called {
		t.Fatal("Loopreview dependency was called despite strict model rejection")
	}
	if !strings.Contains(stderr.String(), "reject") || !strings.Contains(stderr.String(), "valid depths: low, medium, high, max") {
		t.Fatalf("stderr missing strict rejection:\n%s", stderr.String())
	}
}

func TestLoopreviewFlagsOverrideVerifierConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	if err := os.WriteFile(repo+"/.delivery.yml", []byte(`version: 1
adapters:
  worker: codex
verifier:
  model: config-verifier-model
  reasoning_effort: config-verifier-effort
`), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}
	called := false

	exitCode := RunWithDeps([]string{
		"loopreview",
		"--repo", repo,
		"--pr-number", "152",
		"--provider", "claude",
		"--model", "flag-verifier-model",
		"--effort", "flag-verifier-effort",
	}, &stdout, &stderr, Deps{
		Loopreview: func(_ context.Context, opts loopreview.Options) (loopreview.Result, error) {
			called = true
			if opts.Model != "flag-verifier-model" || opts.Effort != "flag-verifier-effort" {
				t.Fatalf("loopreview opts model/effort = %#v", opts)
			}
			return loopreview.Result{
				Verdict: loopreview.Verdict{
					Verdict:         loopreview.VerdictPass,
					Findings:        []loopreview.Finding{},
					Evidence:        "review passed",
					SpecConformance: loopreview.SpecConformancePass,
				},
				ExitCode: 0,
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if !called {
		t.Fatal("Loopreview dependency was not called")
	}
}

func TestVerifyLocalRunsWithInjectedVerifierAndAliases(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	called := false
	prNumber := 105

	exitCode := RunWithDeps([]string{
		"verify-local",
		"-Repo", repo,
		"-PrNumber", "105",
		"-BaseBranch", "trunk",
	}, &stdout, &stderr, Deps{
		Verify: func(_ context.Context, opts verify.Options) verify.Result {
			called = true
			if opts.RepoPath != repo || opts.PRNumber != 105 || opts.Branch != "" || opts.BaseBranch != "trunk" {
				t.Fatalf("verify opts = %#v", opts)
			}
			return verify.Result{
				ExitCode: 1,
				Summary: verify.Summary{
					Repo:              repo,
					PR:                &prNumber,
					BaseBranch:        opts.BaseBranch,
					GeneratedAt:       "2026-06-26T12:00:00Z",
					LocalCommandGates: "configured",
					Verdict:           verify.StatusFail,
					Groups: []verify.GroupResult{{
						Group:  "tests",
						Status: verify.StatusFail,
						Commands: []verify.CommandResult{{
							Command:  "go test ./...",
							ExitCode: 1,
							Status:   verify.StatusFail,
							Reason:   "command-exit-nonzero",
						}},
					}},
				},
			}
		},
	})
	if exitCode != 1 {
		t.Fatalf("RunWithDeps returned exit code %d, want 1", exitCode)
	}
	if !called {
		t.Fatal("Verify dependency was not called")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	text := stdout.String()
	if !strings.Contains(text, "LOCAL VERIFICATION SUMMARY") || !strings.Contains(text, "JSON SUMMARY") {
		t.Fatalf("stdout missing verification report:\n%s", text)
	}
	if !strings.Contains(text, "verdict: fail") {
		t.Fatalf("stdout missing fail verdict:\n%s", text)
	}
}

func TestVerifyLocalRequiresExactlyOneTarget(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := RunWithDeps([]string{
		"verify-local",
		"--repo", t.TempDir(),
		"--pr-number", "105",
		"--branch", "loop/issue-105",
	}, &stdout, &stderr, Deps{})
	if exitCode != 2 {
		t.Fatalf("RunWithDeps returned exit code %d, want 2", exitCode)
	}
	if !strings.Contains(stderr.String(), "exactly one of --pr-number or --branch is required") {
		t.Fatalf("stderr missing target-choice message: %q", stderr.String())
	}
}

func TestStatePushRunsWithInjectedDepsAndAliases(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()

	exitCode := RunWithDeps([]string{
		"state",
		"push",
		"-Repo", repo,
		"-RunId", "run-test",
		"-Branch", "loopcoder/state-test",
		"-Remote", "upstream",
	}, &stdout, &stderr, Deps{
		StatePush: func(_ context.Context, opts statebranch.PushOptions) (statebranch.PushResult, error) {
			if opts.RepoPath != repo || opts.RunID != "run-test" || opts.Branch != "loopcoder/state-test" || opts.Remote != "upstream" {
				t.Fatalf("state push opts = %#v", opts)
			}
			return statebranch.PushResult{
				RepoPath:  repo,
				RunID:     opts.RunID,
				Branch:    opts.Branch,
				Remote:    opts.Remote,
				Committed: true,
				PushError: "offline",
				Files:     []string{"runs/run-test/state.json"},
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	for _, want := range []string{"STATE PUSH", "RunId: run-test", "Branch: loopcoder/state-test", "local state branch commit retained"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestLeaseAcquireRunsWithInjectedDepsAndAliases(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()

	exitCode := RunWithDeps([]string{
		"lease",
		"acquire",
		"-Repo", repo,
		"-RunId", "run-test",
		"-Branch", "loopcoder/state-test",
		"-Remote", "upstream",
		"-Ttl", "42",
	}, &stdout, &stderr, Deps{
		LeaseAcquire: func(_ context.Context, opts statebranch.LeaseOptions) (statebranch.LeaseResult, error) {
			if opts.RepoPath != repo || opts.RunID != "run-test" || opts.Branch != "loopcoder/state-test" || opts.Remote != "upstream" {
				t.Fatalf("lease acquire opts = %#v", opts)
			}
			if opts.TTL != 42*time.Second {
				t.Fatalf("TTL = %s, want 42s", opts.TTL)
			}
			return statebranch.LeaseResult{
				RepoPath:    repo,
				RunID:       opts.RunID,
				Branch:      opts.Branch,
				Remote:      opts.Remote,
				Status:      "observe-only",
				ObserveOnly: true,
				Lease: &statebranch.Lease{
					LeaseID:        "host-123-abc",
					Host:           "host",
					PID:            123,
					LeaseExpiresAt: "2026-06-27T01:10:00Z",
				},
				Message: "observe only: another conductor holds a valid lease",
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	for _, want := range []string{"LEASE ACQUIRE", "Status: observe-only", "Observe only: true", "LeaseId: host-123-abc"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestUnknownCommandReturnsUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := Run([]string{"unknown"}, &stdout, &stderr)
	if exitCode != 2 {
		t.Fatalf("Run returned exit code %d, want 2", exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("stderr missing unknown-command message: %q", stderr.String())
	}
}

func TestReadySetRunsWithInjectedReader(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()

	exitCode := RunWithDeps([]string{"ready-set", "--repo", repo, "--format", "json"}, &stdout, &stderr, Deps{
		NewGitHubReader: func(string) orchestration.GitHubReader {
			return cliFakeReader{
				issues: []gh.Issue{{Number: 93, Title: "Implement ready-set", State: "OPEN"}},
			}
		},
		ProcessAlive: func(int) bool { return false },
		Now: func() time.Time {
			return time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if got["repo"] != "owner/repo" {
		t.Fatalf("repo = %#v, want owner/repo", got["repo"])
	}
	summary := got["summary"].(map[string]any)
	if summary["ready_count"] != float64(1) {
		t.Fatalf("ready_count = %#v, want 1", summary["ready_count"])
	}
}

func TestReadySetRequiresRepo(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := RunWithDeps([]string{"ready-set"}, &stdout, &stderr, Deps{})
	if exitCode != 2 {
		t.Fatalf("RunWithDeps returned exit code %d, want 2", exitCode)
	}
	if !strings.Contains(stderr.String(), "--repo is required") {
		t.Fatalf("stderr missing required repo message: %q", stderr.String())
	}
}

func TestCompileRunsWithDualReadOutput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "ROADMAP.md"), []byte(`# ROADMAP

## Auth Flow
- doc: Design auth
`), 0o644); err != nil {
		t.Fatalf("write ROADMAP.md: %v", err)
	}
	writer := newCLIFakeIssueWriter()

	exitCode := RunWithDeps([]string{"compile", "--repo", repo}, &stdout, &stderr, Deps{
		NewIssueWriter: func(string) compiler.IssueWriter {
			return writer
		},
		Now: func() time.Time {
			return time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	var got compiler.Report
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not compile JSON: %v\n%s", err, stdout.String())
	}
	if !got.PlanApprovalRequired || len(got.Created) != 1 || got.Created[0].Issue != 1 {
		t.Fatalf("compile report = %#v, want one created issue and approval required", got)
	}
	for _, want := range []string{"COMPILE", "Plan approval required: yes", "Created: 1"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr.String())
		}
	}
}

func TestCompileRequiresRepo(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := RunWithDeps([]string{"compile"}, &stdout, &stderr, Deps{})
	if exitCode != 2 {
		t.Fatalf("RunWithDeps returned exit code %d, want 2", exitCode)
	}
	if !strings.Contains(stderr.String(), "--repo is required") {
		t.Fatalf("stderr missing required repo message: %q", stderr.String())
	}
}

func TestDiscoverRunsWithDualReadOutput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	writer := newCLIFakeIssueWriter()

	exitCode := RunWithDeps([]string{"discover", "--repo", repo}, &stdout, &stderr, Deps{
		NewGitHubReader: func(path string) orchestration.GitHubReader {
			if path != repo {
				t.Fatalf("reader repo = %q, want %q", path, repo)
			}
			return cliFakeReader{
				prs: []gh.PullRequest{{
					Number:      44,
					Title:       "Fix failure",
					URL:         "https://github.com/owner/repo/pull/44",
					HeadRefName: "loop/issue-44",
				}},
				checks: map[int][]gh.Check{
					44: {{Name: "verify", Bucket: "fail"}},
				},
			}
		},
		NewIssueWriter: func(path string) compiler.IssueWriter {
			if path != repo {
				t.Fatalf("writer repo = %q, want %q", path, repo)
			}
			return writer
		},
		Now: func() time.Time {
			return time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	var got perception.Report
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not discover JSON: %v\n%s", err, stdout.String())
	}
	if len(got.Created) != 1 || got.Created[0].Issue != 1 || got.Summary.CreatedCount != 1 {
		t.Fatalf("discover report = %#v, want one created issue", got)
	}
	for _, want := range []string{"DISCOVER", "Created: 1", "Skipped held: 0", "Skipped duplicate: 0"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr.String())
		}
	}
}

func TestDiscoverRequiresRepo(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := RunWithDeps([]string{"discover"}, &stdout, &stderr, Deps{})
	if exitCode != 2 {
		t.Fatalf("RunWithDeps returned exit code %d, want 2", exitCode)
	}
	if !strings.Contains(stderr.String(), "--repo is required") {
		t.Fatalf("stderr missing required repo message: %q", stderr.String())
	}
}

func TestTickHelpDocumentsFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := Run([]string{"tick", "--help"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("Run returned exit code %d, want 0", exitCode)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	help := stdout.String()
	for _, want := range []string{
		"loopcoder tick",
		"--repo",
		"--base-branch",
		"--pre-prod-branch",
		"--run-id",
		"--worker-provider",
		"--verifier-provider",
		"--worker-model",
		"--worker-effort",
		"--verifier-model",
		"--verifier-effort",
		"--verifier-timeout",
		"--strict",
		"--throttle-limit",
		"--pretty",
		"--no-pretty",
		"LOOPCODER_PRETTY",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q:\n%s", want, help)
		}
	}
}

func TestPromoteHelpDocumentsFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := Run([]string{"promote", "--help"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("Run returned exit code %d, want 0", exitCode)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	help := stdout.String()
	for _, want := range []string{"loopcoder promote", "--repo", "--pre-prod-branch", "--run-id", "--kick-back"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q:\n%s", want, help)
		}
	}
}

func TestPromoteRunsWithConfigDefaultsAndKickBacks(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte(`version: 1
adapters:
  gate: human-merge
environment:
  pre_prod_branch: staging
ci:
  checks:
    - verify
    - go
`), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}

	called := false
	exitCode := RunWithDeps([]string{"promote", "--repo", repo, "--run-id", "run-test-promote", "--kick-back", "#101", "--kick-back", "merge-sha"}, &stdout, &stderr, Deps{
		NewPromoteWriter: func(path string) orchestration.PromotionWriter {
			if path != repo {
				t.Fatalf("writer repo = %q, want %q", path, repo)
			}
			return cliFakePromotionWriter{}
		},
		Promote: func(_ context.Context, opts orchestration.PromoteOptions) (orchestration.PromoteReport, error) {
			called = true
			if opts.RepoPath != repo || opts.RunID != "run-test-promote" || opts.PreProdBranch != "staging" || opts.Gate != "human-merge" {
				t.Fatalf("promote opts = %#v", opts)
			}
			if !reflect.DeepEqual(opts.KickBackItems, []string{"#101", "merge-sha"}) {
				t.Fatalf("kick-back items = %#v", opts.KickBackItems)
			}
			if !reflect.DeepEqual(opts.RequiredChecks, []string{"verify", "go"}) {
				t.Fatalf("promote required checks = %#v", opts.RequiredChecks)
			}
			if opts.Writer == nil {
				t.Fatal("promotion writer was not set")
			}
			if opts.Clock == nil || opts.StatePush == nil {
				t.Fatalf("promote opts missing clock or state push: %#v", opts)
			}
			if opts.ResolveAutoGate == nil {
				t.Fatal("promote auto-gate resolver was not set")
			}
			return orchestration.PromoteReport{
				Version:       orchestration.PromoteReportVersion,
				RepoPath:      opts.RepoPath,
				RunID:         opts.RunID,
				PreProdBranch: opts.PreProdBranch,
				MainBranch:    "main",
				Gate:          opts.Gate,
				Status:        orchestration.PromoteStatusSucceeded,
				KickedBack: []orchestration.PromoteKickBackResult{{
					Item:        "#101",
					PRNumber:    101,
					Branch:      opts.PreProdBranch,
					RevertedSHA: "merge-sha",
					SHA:         "revert-sha",
					Status:      orchestration.PromoteStatusSucceeded,
				}},
				Promoted: orchestration.PromoteMainResult{
					PreProdBranch: opts.PreProdBranch,
					MainBranch:    "main",
					Head:          opts.PreProdBranch,
					SHA:           "main-sha",
					Status:        orchestration.PromoteStatusSucceeded,
				},
				Sync: orchestration.PromoteSyncResult{
					PreProdBranch: opts.PreProdBranch,
					MainBranch:    "main",
					SHA:           "main-sha",
					Status:        orchestration.PromoteStatusSucceeded,
				},
				Summary: orchestration.PromoteSummary{
					KickedBackCount: 1,
					PromotedCount:   1,
				},
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if !called {
		t.Fatal("Promote dependency was not called")
	}
	var got orchestration.PromoteReport
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not promote JSON: %v\n%s", err, stdout.String())
	}
	if got.Status != orchestration.PromoteStatusSucceeded || got.Summary.PromotedCount != 1 {
		t.Fatalf("promote report = %#v", got)
	}
	for _, want := range []string{"PROMOTE", "Status: succeeded", "Kicked back", "Promoted", "Pre-prod sync"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr.String())
		}
	}
}

func TestTickRunsWithDualReadOutputAndConfigDefaults(t *testing.T) {
	clearPrettyEnv(t)
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte(`version: 1
adapters:
  worker: codex
  verifier: claude
worker:
  base_branch: develop
  model: config-worker-model
  reasoning_effort: config-worker-effort
environment:
  pre_prod_branch: staging
verifier:
  model: config-verifier-model
  reasoning_effort: config-verifier-effort
ci:
  checks: [verify, go]
domain:
  red_lines:
    - category: disclosure-compliance
      detail: unresolved disclosure approval
      path_globs:
        - disclosure/**
evidence:
  website:
    preview_url: https://preview.example.com
  cli:
    example_output: |
      $ loopcoder --version
      version=dev
`), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}

	called := false
	exitCode := RunWithDeps([]string{"tick", "--repo", repo, "--run-id", "run-test-wave", "--no-pretty"}, &stdout, &stderr, Deps{
		NewGitHubReader: func(path string) orchestration.GitHubReader {
			if path != repo {
				t.Fatalf("reader repo = %q, want %q", path, repo)
			}
			return cliFakeReader{}
		},
		NewIssueWriter: func(path string) compiler.IssueWriter {
			if path != repo {
				t.Fatalf("writer repo = %q, want %q", path, repo)
			}
			return newCLIFakeIssueWriter()
		},
		Now: func() time.Time {
			return time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
		},
		Tick: func(_ context.Context, opts orchestration.TickOptions) (orchestration.TickReport, error) {
			called = true
			if opts.RepoPath != repo || opts.BaseBranch != "develop" || opts.PreProdBranch != "staging" || opts.RunID != "run-test-wave" {
				t.Fatalf("tick opts repo/base/run = %#v", opts)
			}
			if !reflect.DeepEqual(opts.RequiredChecks, []string{"verify", "go"}) {
				t.Fatalf("tick required checks = %#v", opts.RequiredChecks)
			}
			wantEvidence := []config.EvidenceArtifact{
				{ProjectType: "website", PreviewURL: "https://preview.example.com"},
				{ProjectType: "cli", ExampleOutput: "$ loopcoder --version\nversion=dev"},
			}
			if !reflect.DeepEqual(opts.ConfiguredEvidence, wantEvidence) {
				t.Fatalf("tick configured evidence = %#v, want %#v", opts.ConfiguredEvidence, wantEvidence)
			}
			wantRedLines := []orchestration.RiskRedLine{{
				Category:  "disclosure-compliance",
				Detail:    "unresolved disclosure approval",
				PathGlobs: []string{"disclosure/**"},
			}}
			if !reflect.DeepEqual(opts.AdditionalRiskRedLines, wantRedLines) {
				t.Fatalf("tick additional risk red lines = %#v, want %#v", opts.AdditionalRiskRedLines, wantRedLines)
			}
			if opts.WorkerProvider != "codex" || opts.VerifierProvider != "claude" {
				t.Fatalf("tick opts providers = %#v", opts)
			}
			if opts.WorkerModel != "config-worker-model" || opts.WorkerEffort != "config-worker-effort" {
				t.Fatalf("tick worker model/effort = %#v", opts)
			}
			if opts.VerifierModel != "config-verifier-model" || opts.VerifierEffort != "config-verifier-effort" {
				t.Fatalf("tick verifier model/effort = %#v", opts)
			}
			if opts.Clock == nil {
				t.Fatal("tick opts clock is nil")
			}
			if got := opts.Clock(); !got.Equal(time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)) {
				t.Fatalf("tick opts clock = %s", got)
			}
			return orchestration.TickReport{
				Version:       orchestration.TickReportVersion,
				Repo:          "owner/repo",
				RepoPath:      repo,
				BaseBranch:    opts.BaseBranch,
				PreProdBranch: opts.PreProdBranch,
				RunID:         opts.RunID,
				Status:        orchestration.TickStatusSucceeded,
				StopReason:    orchestration.TickStopCompleted,
				StartedAt:     "2026-07-02T12:00:00Z",
				FinishedAt:    "2026-07-02T12:00:00Z",
				Compile: compiler.Report{
					Repo: "owner/repo",
					Summary: compiler.Summary{
						UnchangedCount: 1,
						TotalCount:     1,
					},
				},
				ReadySet: report.ReadySetReport{
					Repo:       "owner/repo",
					BaseBranch: opts.BaseBranch,
					Ready: []report.ReadyIssue{{
						Issue:  10,
						Title:  "Ready",
						Reason: "ready",
					}},
				},
				DispatchWave: &orchestration.DispatchWaveReport{
					Repo:       "owner/repo",
					BaseBranch: opts.BaseBranch,
					RunID:      opts.RunID,
					Results: []orchestration.DispatchWaveIssueResult{{
						Issue:  10,
						Status: orchestration.DispatchWaveStatusSucceeded,
						PR:     "https://github.com/owner/repo/pull/10",
					}},
				},
				Reviews: []orchestration.TickReviewResult{{
					Issue:           10,
					PR:              "https://github.com/owner/repo/pull/10",
					PRNumber:        10,
					Verdict:         loopreview.VerdictPass,
					SpecConformance: loopreview.SpecConformancePass,
					Evidence:        "review passed",
					Findings:        []loopreview.Finding{},
				}},
				NeedsHuman: []orchestration.TickIssue{},
				Failures:   []orchestration.TickIssue{},
				StatePush: &orchestration.TickStatePush{
					Branch: statebranch.DefaultBranch,
					Remote: statebranch.DefaultRemote,
					Pushed: true,
					Files:  []string{"runs/run-test-wave/state.json"},
				},
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if !called {
		t.Fatal("Tick dependency was not called")
	}
	var got orchestration.TickReport
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not tick JSON: %v\n%s", err, stdout.String())
	}
	if got.Status != orchestration.TickStatusSucceeded || got.Summary.DispatchedPRCount != 1 {
		t.Fatalf("tick report = %#v", got)
	}
	if strings.Contains(stdout.String(), "TICK") {
		t.Fatalf("stdout should be JSON only, got:\n%s", stdout.String())
	}
	for _, want := range []string{"TICK", "Status: succeeded", "Dispatch", "Reviews", "State"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr.String())
		}
	}
}

func TestTickOptionsFromConfigWiresDomainRedLines(t *testing.T) {
	cfg := config.Default()
	cfg.Domain.RedLines = []config.DomainRedLine{{
		Category:  "disclosure-compliance",
		Detail:    "unresolved disclosure approval",
		PathGlobs: []string{"disclosure/**"},
	}}
	opts, ok := tickOptionsFromConfig(t.TempDir(), io.Discard, Deps{
		NewGitHubReader: func(string) orchestration.GitHubReader {
			return cliFakeReader{}
		},
		NewIssueWriter: func(string) compiler.IssueWriter {
			return newCLIFakeIssueWriter()
		},
		NewPreProdWriter: func(string) orchestration.PreProdWriter {
			return nil
		},
	}, cfg, false, "", false)
	if !ok {
		t.Fatal("tickOptionsFromConfig returned ok=false")
	}

	want := []orchestration.RiskRedLine{{
		Category:  "disclosure-compliance",
		Detail:    "unresolved disclosure approval",
		PathGlobs: []string{"disclosure/**"},
	}}
	if !reflect.DeepEqual(opts.AdditionalRiskRedLines, want) {
		t.Fatalf("AdditionalRiskRedLines = %#v, want %#v", opts.AdditionalRiskRedLines, want)
	}
}

func TestTickSelfAcksOwnRelayRecordsWithoutGatingStartup(t *testing.T) {
	clearPrettyEnv(t)
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	stale := writePendingRelayForCLITest(t, repo, "worker", 202, cliPendingPrettyBlock("worker"))
	record := validDispatchReport()
	called := false

	exitCode := RunWithDeps([]string{"tick", "--repo", repo, "--run-id", "run-test-wave"}, &stdout, &stderr, Deps{
		IsTerminal: func(io.Writer) bool {
			return false
		},
		Tick: func(_ context.Context, opts orchestration.TickOptions) (orchestration.TickReport, error) {
			called = true
			return orchestration.TickReport{
				Version:       orchestration.TickReportVersion,
				Repo:          "owner/repo",
				RepoPath:      repo,
				BaseBranch:    opts.BaseBranch,
				PreProdBranch: opts.PreProdBranch,
				RunID:         opts.RunID,
				Status:        orchestration.TickStatusSucceeded,
				StopReason:    orchestration.TickStopCompleted,
				StartedAt:     "2026-07-02T12:00:00Z",
				FinishedAt:    "2026-07-02T12:00:00Z",
				DispatchWave: &orchestration.DispatchWaveReport{
					Repo:       "owner/repo",
					BaseBranch: opts.BaseBranch,
					RunID:      opts.RunID,
					Results: []orchestration.DispatchWaveIssueResult{{
						Issue:  101,
						Status: orchestration.DispatchWaveStatusSucceeded,
						PR:     "https://github.com/owner/repo/pull/101",
						Report: &record,
					}},
				},
				Reviews:    []orchestration.TickReviewResult{},
				NeedsHuman: []orchestration.TickIssue{},
				Failures:   []orchestration.TickIssue{},
			}, nil
		},
	})
	if exitCode == relayGateExitCode {
		t.Fatalf("tick exit = relay gate code %d; stderr=%q", exitCode, stderr.String())
	}
	if exitCode != 0 {
		t.Fatalf("tick exit = %d, stderr=%q", exitCode, stderr.String())
	}
	if !called {
		t.Fatal("Tick dependency was not called")
	}
	if !strings.Contains(stderr.String(), "loopcoder report: worker succeeded") || !strings.Contains(stderr.String(), "- worker: OpenAI Codex / codex") {
		t.Fatalf("tick stderr missing self-surfaced worker block:\n%s", stderr.String())
	}
	pending := relaygate.Check(repo)
	if len(pending) != 1 || pending[0].Nonce != stale.Nonce {
		t.Fatalf("pending relay records after tick = %#v, want only stale startup record", pending)
	}
}

func TestTickNeedsHumanExitCode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()

	exitCode := RunWithDeps([]string{"tick", "--repo", repo, "--no-pretty"}, &stdout, &stderr, Deps{
		Tick: func(context.Context, orchestration.TickOptions) (orchestration.TickReport, error) {
			return orchestration.TickReport{
				Version:    orchestration.TickReportVersion,
				Repo:       "owner/repo",
				RepoPath:   repo,
				BaseBranch: "main",
				RunID:      "run-test-wave",
				Status:     orchestration.TickStatusNeedsHuman,
				StopReason: orchestration.TickStopReviewNeedsHuman,
				NeedsHuman: []orchestration.TickIssue{{
					Step:   "loopreview",
					PR:     "https://github.com/owner/repo/pull/10",
					Detail: "manual review required",
				}},
				Failures: []orchestration.TickIssue{},
			}, nil
		},
	})
	if exitCode != 2 {
		t.Fatalf("RunWithDeps returned exit code %d, want 2; stderr=%q", exitCode, stderr.String())
	}
	if stdout.Len() == 0 || !strings.Contains(stderr.String(), "Status: needs-human") {
		t.Fatalf("tick did not emit dual output; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestTriggerCronRunsTickWithExplicitRepo(t *testing.T) {
	clearPrettyEnv(t)
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	called := false

	exitCode := RunWithDeps([]string{
		"trigger",
		"cron",
		"--repo", repo,
		"--schedule", "@hourly",
		"--no-pretty",
	}, &stdout, &stderr, Deps{
		Now: func() time.Time {
			return time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
		},
		Tick: func(_ context.Context, opts orchestration.TickOptions) (orchestration.TickReport, error) {
			called = true
			if opts.RepoPath != repo {
				t.Fatalf("tick RepoPath = %q, want %q", opts.RepoPath, repo)
			}
			if opts.RunID == "" {
				t.Fatal("tick RunID is empty")
			}
			return cliTriggerTickReport(opts, orchestration.TickStatusSucceeded, orchestration.TickStopCompleted), nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if !called {
		t.Fatal("Tick dependency was not called")
	}
	var got orchestration.TriggerReport
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not trigger JSON: %v\n%s", err, stdout.String())
	}
	if got.Kind != orchestration.TriggerKindCron || got.Schedule != "@hourly" || got.Status != orchestration.TriggerStatusSucceeded || got.Iterations != 1 {
		t.Fatalf("trigger report = %#v", got)
	}
	for _, want := range []string{"TRIGGER", "Kind: cron", "Schedule: @hourly", "Ticks"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr.String())
		}
	}
}

func TestTriggerSelfAcksOwnRelayRecordsWithoutGatingStartup(t *testing.T) {
	clearPrettyEnv(t)
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	stale := writePendingRelayForCLITest(t, repo, "worker", 202, cliPendingPrettyBlock("worker"))
	record := validDispatchReport()
	called := false

	exitCode := RunWithDeps([]string{
		"trigger",
		"cron",
		"--repo", repo,
		"--schedule", "@hourly",
	}, &stdout, &stderr, Deps{
		IsTerminal: func(io.Writer) bool {
			return false
		},
		Now: func() time.Time {
			return time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
		},
		Tick: func(_ context.Context, opts orchestration.TickOptions) (orchestration.TickReport, error) {
			called = true
			report := cliTriggerTickReport(opts, orchestration.TickStatusSucceeded, orchestration.TickStopCompleted)
			report.DispatchWave = &orchestration.DispatchWaveReport{
				Repo:       "owner/repo",
				BaseBranch: opts.BaseBranch,
				RunID:      opts.RunID,
				Results: []orchestration.DispatchWaveIssueResult{{
					Issue:  101,
					Status: orchestration.DispatchWaveStatusSucceeded,
					PR:     "https://github.com/owner/repo/pull/101",
					Report: &record,
				}},
			}
			return report, nil
		},
	})
	if exitCode == relayGateExitCode {
		t.Fatalf("trigger exit = relay gate code %d; stderr=%q", exitCode, stderr.String())
	}
	if exitCode != 0 {
		t.Fatalf("trigger exit = %d, stderr=%q", exitCode, stderr.String())
	}
	if !called {
		t.Fatal("Tick dependency was not called")
	}
	if !strings.Contains(stderr.String(), "loopcoder report: worker succeeded") || !strings.Contains(stderr.String(), "- worker: OpenAI Codex / codex") {
		t.Fatalf("trigger stderr missing self-surfaced worker block:\n%s", stderr.String())
	}
	pending := relaygate.Check(repo)
	if len(pending) != 1 || pending[0].Nonce != stale.Nonce {
		t.Fatalf("pending relay records after trigger = %#v, want only stale startup record", pending)
	}
}

func TestTriggerBaseBranchThreadsConfigMismatchCheck(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := initRepoWithDeliveryOnlyOnBranch(t, "trunk")

	exitCode := RunWithDeps([]string{
		"trigger",
		"cron",
		"--repo", repo,
		"--base-branch", "trunk",
		"--schedule", "@hourly",
		"--no-pretty",
	}, &stdout, &stderr, Deps{
		Tick: func(context.Context, orchestration.TickOptions) (orchestration.TickReport, error) {
			t.Fatal("Tick should not run when base config mismatch is detected")
			return orchestration.TickReport{}, nil
		},
	})
	if exitCode != 1 {
		t.Fatalf("RunWithDeps returned exit code %d, want 1; stderr=%q", exitCode, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	for _, want := range []string{"trigger cron", "present on trunk", "--config-from-base"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr.String())
		}
	}
}

func TestTriggerGoalLoopMaxIterationsAliasRoutesNeedsHuman(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	called := 0

	exitCode := RunWithDeps([]string{
		"trigger",
		"goal-loop",
		"--repo", repo,
		"--max_iterations", "2",
		"--no-pretty",
	}, &stdout, &stderr, Deps{
		Tick: func(_ context.Context, opts orchestration.TickOptions) (orchestration.TickReport, error) {
			called++
			return cliTriggerTickReport(opts, orchestration.TickStatusSucceeded, orchestration.TickStopCompleted), nil
		},
	})
	if exitCode != 2 {
		t.Fatalf("RunWithDeps returned exit code %d, want 2; stderr=%q", exitCode, stderr.String())
	}
	if called != 2 {
		t.Fatalf("tick calls = %d, want 2", called)
	}
	var got orchestration.TriggerReport
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not trigger JSON: %v\n%s", err, stdout.String())
	}
	if got.Status != orchestration.TriggerStatusNeedsHuman || got.StopReason != orchestration.TriggerStopMaxIterations || got.Iterations != 2 {
		t.Fatalf("trigger report = %#v", got)
	}
}

func TestTriggerHookRequiresEvent(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := RunWithDeps([]string{
		"trigger",
		"hook",
		"--repo", t.TempDir(),
	}, &stdout, &stderr, Deps{})
	if exitCode != 2 {
		t.Fatalf("RunWithDeps returned exit code %d, want 2", exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "--event is required") {
		t.Fatalf("stderr missing required event message: %q", stderr.String())
	}
}

func TestDispatchRunsWithInjectedWorker(t *testing.T) {
	clearPrettyEnv(t)
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	record := validDispatchReport()

	exitCode := RunWithDeps([]string{
		"dispatch",
		"--repo", repo,
		"--issue-number", "101",
		"--issue-title", "Implement dispatch",
		"--issue-body", "Body",
		"--model", "gpt-5",
		"--effort", "high",
	}, &stdout, &stderr, Deps{
		Dispatch: func(_ context.Context, opts worker.Options) (worker.Result, error) {
			if opts.RepoPath != repo {
				t.Fatalf("RepoPath = %q, want %q", opts.RepoPath, repo)
			}
			if opts.IssueNumber != 101 || opts.IssueTitle != "Implement dispatch" || opts.IssueBody != "Body" {
				t.Fatalf("dispatch opts issue fields = %#v", opts)
			}
			if opts.BaseBranch != "main" || opts.Provider != "codex" || opts.Model != "gpt-5" || opts.Effort != "high" {
				t.Fatalf("dispatch opts defaults/pass-through = %#v", opts)
			}
			if opts.Stderr == nil {
				t.Fatal("dispatch opts Stderr is nil")
			}
			return worker.Result{
				OK:          true,
				Issue:       opts.IssueNumber,
				Branch:      "loop/issue-101",
				RunID:       "run-test",
				PR:          "https://github.com/owner/repo/pull/101",
				Summary:     "Implemented dispatch.",
				AttemptPath: "/repo/.loopcoder/runs/run-test/workers/job-101-1.attempt.json",
				Status:      "succeeded",
				ExitCode:    0,
				LogBytes:    12,
				Report:      &record,
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	gotStderr := stderr.String()
	for _, want := range []string{
		"loopcoder report: worker succeeded",
		"- worker: OpenAI Codex / codex / gpt-5.5 (high) (parsed) / high",
		"- permission: write",
		"- action: \"implement issue #101\"",
	} {
		if !strings.Contains(gotStderr, want) {
			t.Fatalf("stderr missing %q:\n%s", want, gotStderr)
		}
	}
	for _, disallowed := range []string{"\u2705", "\u274c", "\u26a0"} {
		if strings.Contains(gotStderr, disallowed) {
			t.Fatalf("plain stderr contains %q:\n%s", disallowed, gotStderr)
		}
	}

	lines := nonEmptyLines(stdout.String())
	if len(lines) != 3 {
		t.Fatalf("stdout lines = %d, want 3:\n%s", len(lines), stdout.String())
	}
	var reportLine reporter.Report
	if err := json.Unmarshal([]byte(lines[1]), &reportLine); err != nil {
		t.Fatalf("stdout second line is not report JSON: %v\n%s", err, stdout.String())
	}
	if err := reportLine.Validate(); err != nil {
		t.Fatalf("report JSON does not validate: %v", err)
	}
	if lines[0] != reportLine.Header() {
		t.Fatalf("stdout first line = %q, want %q", lines[0], reportLine.Header())
	}
	canonical, err := reportLine.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON returned error: %v", err)
	}
	if lines[1] != string(canonical) {
		t.Fatalf("stdout second line = %q, want canonical %q", lines[1], string(canonical))
	}

	var got worker.Result
	if err := json.Unmarshal([]byte(lines[2]), &got); err != nil {
		t.Fatalf("stdout final line is not dispatch JSON: %v\n%s", err, stdout.String())
	}
	if !got.OK || got.Status != "succeeded" {
		t.Fatalf("dispatch JSON has wrong success fields: %#v", got)
	}
	if got.Report == nil {
		t.Fatalf("dispatch JSON missing report: %s", lines[2])
	}
	nestedCanonical, err := got.Report.CanonicalJSON()
	if err != nil {
		t.Fatalf("nested report CanonicalJSON returned error: %v", err)
	}
	if string(nestedCanonical) != lines[1] {
		t.Fatalf("nested report = %s, want %s", string(nestedCanonical), lines[1])
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(lines[2]), &fields); err != nil {
		t.Fatalf("dispatch JSON invalid: %v", err)
	}
	for _, key := range []string{"ok", "issue", "branch", "run_id", "pr", "summary", "attempt_path", "status", "exit_code", "log_bytes", "report"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("dispatch JSON missing %q: %s", key, lines[2])
		}
	}
	for _, key := range []string{"worker_model", "worker_tokens"} {
		if _, ok := fields[key]; ok {
			t.Fatalf("dispatch JSON unexpectedly contains %q: %s", key, lines[2])
		}
	}
}

func TestNestedRunDispatchesThreeChildrenConcurrentlyAndHonorsDependencies(t *testing.T) {
	clearPrettyEnv(t)
	t.Setenv("LOOPCODER_HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte("version: 1\nadapters:\n  worker: codex\n"), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}
	planPath := writeNestedPlanFixture(t, repo, []orchestration.ChildRunPlan{
		nestedPlanItem("alpha", 701, nil, true, "write", nil),
		nestedPlanItem("beta", 702, nil, true, "write", nil),
		nestedPlanItem("gamma", 703, []string{"alpha"}, true, "write", nil),
	}, 2)

	startedAlpha := make(chan struct{})
	startedBeta := make(chan struct{})
	var onceAlpha sync.Once
	var onceBeta sync.Once
	var mu sync.Mutex
	active := 0
	maxActive := 0
	completed := map[string]bool{}
	var calls []worker.Options

	exitCode := RunWithDeps([]string{
		"nested", "run",
		"--repo", repo,
		"--plan", planPath,
		"--provider", "claude",
		"--format", "json",
	}, &stdout, &stderr, Deps{
		Now: fixedCLINow,
		Dispatch: func(ctx context.Context, opts worker.Options) (worker.Result, error) {
			key := strings.ToLower(strings.TrimSpace(opts.IssueTitle))
			mu.Lock()
			calls = append(calls, opts)
			active++
			if active > maxActive {
				maxActive = active
			}
			if key == "gamma" && !completed["alpha"] {
				mu.Unlock()
				return worker.Result{}, errors.New("dependent child gamma started before alpha completed")
			}
			mu.Unlock()
			defer func() {
				mu.Lock()
				active--
				mu.Unlock()
			}()

			switch key {
			case "alpha":
				onceAlpha.Do(func() { close(startedAlpha) })
				select {
				case <-startedBeta:
				case <-ctx.Done():
					return worker.Result{}, ctx.Err()
				case <-time.After(2 * time.Second):
					return worker.Result{}, errors.New("beta did not start concurrently with alpha")
				}
			case "beta":
				onceBeta.Do(func() { close(startedBeta) })
				select {
				case <-startedAlpha:
				case <-ctx.Done():
					return worker.Result{}, ctx.Err()
				case <-time.After(2 * time.Second):
					return worker.Result{}, errors.New("alpha did not start concurrently with beta")
				}
			}

			mu.Lock()
			completed[key] = true
			mu.Unlock()
			record := validDispatchReport()
			record.Provider = opts.Provider
			record.WorkID = opts.RunID
			record.Issue = opts.IssueNumber
			record.Action = "nested child " + key
			return worker.Result{
				OK:          true,
				Issue:       opts.IssueNumber,
				Branch:      opts.Branch,
				RunID:       opts.RunID,
				PR:          "https://github.com/owner/repo/pull/" + strconv.Itoa(opts.IssueNumber),
				Summary:     "nested " + key,
				AttemptPath: filepath.Join(repo, ".loopcoder", "runs", opts.RunID, "workers", "job.attempt.json"),
				Status:      "succeeded",
				ExitCode:    0,
				LogBytes:    1,
				Report:      &record,
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	var report orchestration.NestedScheduleReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("stdout is not nested JSON: %v\n%s", err, stdout.String())
	}
	if report.Status != orchestration.NestedStatusSucceeded || len(report.Children) != 3 {
		t.Fatalf("nested report = %#v", report)
	}
	if maxActive < 2 {
		t.Fatalf("max concurrent dispatches = %d, want at least 2", maxActive)
	}
	if len(calls) != 3 {
		t.Fatalf("dispatch calls = %d, want 3", len(calls))
	}
	for _, call := range calls {
		if call.Provider != "claude" {
			t.Fatalf("nested dispatch provider = %q, want claude", call.Provider)
		}
		if call.RunID == "" || !strings.Contains(call.Branch, strings.ToLower(call.IssueTitle)) {
			t.Fatalf("nested dispatch did not propagate run/branch context: %#v", call)
		}
	}
}

func TestNestedRunUsesConfiguredCodexProvider(t *testing.T) {
	t.Setenv("LOOPCODER_HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte("version: 1\nadapters:\n  worker: codex\n"), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}
	planPath := writeNestedPlanFixture(t, repo, []orchestration.ChildRunPlan{
		nestedPlanItem("alpha", 701, nil, true, "write", nil),
	}, 1)

	var got worker.Options
	exitCode := RunWithDeps([]string{"nested", "run", "--repo", repo, "--plan", planPath, "--format", "json"}, &stdout, &stderr, Deps{
		Now: fixedCLINow,
		Dispatch: func(_ context.Context, opts worker.Options) (worker.Result, error) {
			got = opts
			record := validDispatchReport()
			record.Provider = opts.Provider
			record.WorkID = opts.RunID
			record.Issue = opts.IssueNumber
			return worker.Result{
				OK:          true,
				Issue:       opts.IssueNumber,
				Branch:      opts.Branch,
				RunID:       opts.RunID,
				Summary:     "nested alpha",
				AttemptPath: filepath.Join(repo, ".loopcoder", "runs", opts.RunID, "workers", "job.attempt.json"),
				Status:      "succeeded",
				ExitCode:    0,
				LogBytes:    1,
				Report:      &record,
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if got.Provider != "codex" {
		t.Fatalf("provider = %q, want codex", got.Provider)
	}
}

func TestNestedRunRejectsPermissionEscalationBeforeDispatch(t *testing.T) {
	t.Setenv("LOOPCODER_HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	planPath := writeNestedPlanFixture(t, repo, []orchestration.ChildRunPlan{
		nestedPlanItem("admin", 701, nil, true, "orchestrate", nil),
	}, 1)
	called := false

	exitCode := RunWithDeps([]string{
		"nested", "run",
		"--repo", repo,
		"--plan", planPath,
		"--parent-permission", "write",
	}, &stdout, &stderr, Deps{
		Dispatch: func(context.Context, worker.Options) (worker.Result, error) {
			called = true
			return worker.Result{}, nil
		},
	})
	if exitCode != 2 {
		t.Fatalf("RunWithDeps returned exit code %d, want 2; stderr=%q", exitCode, stderr.String())
	}
	if called {
		t.Fatal("dispatch was called despite permission escalation")
	}
	if !strings.Contains(stderr.String(), "exceeds parent permission") {
		t.Fatalf("stderr missing permission diagnostic: %q", stderr.String())
	}
}

func TestNestedRunTestSubprocessExecutesRealChildProcesses(t *testing.T) {
	t.Setenv("LOOPCODER_HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	planPath := writeNestedPlanFixture(t, repo, []orchestration.ChildRunPlan{
		nestedPlanItem("alpha", 701, nil, true, "read-only", []string{"go env GOOS"}),
		nestedPlanItem("beta", 702, nil, true, "read-only", []string{"go env GOARCH"}),
		nestedPlanItem("gamma", 703, []string{"alpha"}, false, "read-only", []string{"go env GOVERSION"}),
	}, 2)

	exitCode := RunWithDeps([]string{
		"nested", "run",
		"--repo", repo,
		"--plan", planPath,
		"--provider", nestedTestSubprocessProvider,
		"--format", "json",
	}, &stdout, &stderr, Deps{Now: fixedCLINow})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q stdout=%q", exitCode, stderr.String(), stdout.String())
	}
	var report orchestration.NestedScheduleReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("stdout is not nested JSON: %v\n%s", err, stdout.String())
	}
	if report.Status != orchestration.NestedStatusSucceeded || len(report.Children) != 3 {
		t.Fatalf("nested subprocess report = %#v", report)
	}
	attempts, err := state.LoadAttempts(repo, report.Children[0].RunID)
	if err != nil {
		t.Fatalf("LoadAttempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Provider != nestedTestSubprocessProvider || attempts[0].Report == nil {
		t.Fatalf("attempts = %#v, want test-subprocess report", attempts)
	}
}

func TestDispatchPrettyDefaultNonInteractiveWritesPlainToStderrWithoutChangingStdout(t *testing.T) {
	clearPrettyEnv(t)
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	record := validDispatchReport()
	result := validDispatchResult(record)
	wantStdout := expectedDispatchStdout(t, result)

	exitCode := RunWithDeps([]string{
		"dispatch",
		"--repo", repo,
		"--issue-number", "101",
		"--issue-title", "Implement dispatch",
	}, &stdout, &stderr, Deps{
		IsTerminal: func(io.Writer) bool {
			return false
		},
		Dispatch: func(context.Context, worker.Options) (worker.Result, error) {
			return result, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stdout.String() != wantStdout {
		t.Fatalf("stdout = %q, want %q", stdout.String(), wantStdout)
	}
	gotStderr := stderr.String()
	for _, want := range []string{
		"loopcoder report: worker succeeded",
		"- worker: OpenAI Codex / codex / gpt-5.5 (high) (parsed) / high",
		"- tokens: input=120  output=34  total=154",
	} {
		if !strings.Contains(gotStderr, want) {
			t.Fatalf("stderr missing %q:\n%s", want, gotStderr)
		}
	}
	for _, disallowed := range []string{"\u2705", "\u274c", "\u26a0"} {
		if strings.Contains(gotStderr, disallowed) {
			t.Fatalf("plain stderr contains %q:\n%s", disallowed, gotStderr)
		}
	}
}

func TestDispatchWritesRelayLedger(t *testing.T) {
	clearPrettyEnv(t)
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	record := validDispatchReport()
	result := validDispatchResult(record)
	result.AttemptPath = filepath.Join(repo, ".loopcoder", "runs", result.RunID, "workers", "job-101-1.attempt.json")

	exitCode := RunWithDeps([]string{
		"dispatch",
		"--repo", repo,
		"--issue-number", "101",
		"--issue-title", "Implement dispatch",
	}, &stdout, &stderr, Deps{
		Now: func() time.Time {
			return now
		},
		IsTerminal: func(io.Writer) bool {
			return false
		},
		Dispatch: func(context.Context, worker.Options) (worker.Result, error) {
			return result, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}

	ledger := readSingleFile(t, filepath.Join(repo, ".loopcoder", "relay", result.RunID, "job-101-1.attest"))
	for _, want := range []string{
		"# command=dispatch",
		"# role=worker",
		"# run_id=run-test",
		"# issue=101",
		record.Header(),
		dispatchPrettyBlock(record, result.Status, result.PR, "", reporter.PrettyModePlain),
	} {
		if !strings.Contains(ledger, want) {
			t.Fatalf("relay ledger missing %q:\n%s", want, ledger)
		}
	}
	pending := relaygate.Check(repo)
	if len(pending) != 1 {
		t.Fatalf("pending relay records = %d, want 1", len(pending))
	}
	if pending[0].Role != "worker" || pending[0].PRNumber != 101 || pending[0].Nonce != relaygate.Nonce("run-test", 101, "worker") {
		t.Fatalf("pending relay record = %#v, want worker PR 101 deterministic nonce", pending[0])
	}
	if pending[0].Block != dispatchPrettyBlock(record, result.Status, result.PR, "", reporter.PrettyModePlain)+"\n" {
		t.Fatalf("pending relay block = %q, want plain pretty block", pending[0].Block)
	}
}

func TestDispatchPrettyWritesEmojiToStderrWhenInteractive(t *testing.T) {
	clearPrettyEnv(t)
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	record := validDispatchReport()
	result := worker.Result{
		OK:          true,
		Issue:       101,
		Branch:      "loop/issue-101",
		RunID:       "run-test",
		PR:          "https://github.com/owner/repo/pull/101",
		Summary:     "Implemented dispatch.",
		AttemptPath: "/repo/.loopcoder/runs/run-test/workers/job-101-1.attempt.json",
		Status:      "succeeded",
		ExitCode:    0,
		LogBytes:    12,
		Report:      &record,
	}
	wantStdout := expectedDispatchStdout(t, result)

	exitCode := RunWithDeps([]string{
		"dispatch",
		"--repo", repo,
		"--issue-number", "101",
		"--issue-title", "Implement dispatch",
	}, &stdout, &stderr, Deps{
		IsTerminal: func(io.Writer) bool {
			return true
		},
		Dispatch: func(context.Context, worker.Options) (worker.Result, error) {
			return result, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stdout.String() != wantStdout {
		t.Fatalf("stdout = %q, want %q", stdout.String(), wantStdout)
	}
	for _, want := range []string{
		"\u2705 loopcoder report: worker succeeded",
		"- worker: OpenAI Codex / codex / gpt-5.5 (high) (parsed) / high",
		"- permission: write",
		"- action: \"implement issue #101\"",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr.String())
		}
	}
}

func TestDispatchPrettyEnvOptInWritesEmojiToStderrWithoutChangingStdout(t *testing.T) {
	clearPrettyEnv(t)
	t.Setenv("LOOPCODER_PRETTY", "1")
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	record := validDispatchReport()
	result := worker.Result{
		OK:          true,
		Issue:       101,
		Branch:      "loop/issue-101",
		RunID:       "run-test",
		PR:          "https://github.com/owner/repo/pull/101",
		Summary:     "Implemented dispatch.",
		AttemptPath: "/repo/.loopcoder/runs/run-test/workers/job-101-1.attempt.json",
		Status:      "succeeded",
		ExitCode:    0,
		LogBytes:    12,
		Report:      &record,
	}
	wantStdout := expectedDispatchStdout(t, result)

	exitCode := RunWithDeps([]string{
		"dispatch",
		"--repo", repo,
		"--issue-number", "101",
		"--issue-title", "Implement dispatch",
	}, &stdout, &stderr, Deps{
		IsTerminal: func(io.Writer) bool {
			return false
		},
		Dispatch: func(context.Context, worker.Options) (worker.Result, error) {
			return result, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stdout.String() != wantStdout {
		t.Fatalf("stdout = %q, want %q", stdout.String(), wantStdout)
	}
	for _, want := range []string{
		"\u2705 loopcoder report: worker succeeded",
		"- worker: OpenAI Codex / codex / gpt-5.5 (high) (parsed) / high",
		"- tokens: input=120  output=34  total=154",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr.String())
		}
	}
}

func TestDispatchPrettyFlagHonorsNoColorPlainFallback(t *testing.T) {
	clearPrettyEnv(t)
	t.Setenv("NO_COLOR", "1")
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	record := validDispatchReport()

	exitCode := RunWithDeps([]string{
		"dispatch",
		"--pretty",
		"--repo", repo,
		"--issue-number", "101",
		"--issue-title", "Implement dispatch",
	}, &stdout, &stderr, Deps{
		IsTerminal: func(io.Writer) bool {
			return true
		},
		Dispatch: func(context.Context, worker.Options) (worker.Result, error) {
			return worker.Result{
				OK:          true,
				Issue:       101,
				Branch:      "loop/issue-101",
				RunID:       "run-test",
				PR:          "https://github.com/owner/repo/pull/101",
				Summary:     "Implemented dispatch.",
				AttemptPath: "/repo/.loopcoder/runs/run-test/workers/job-101-1.attempt.json",
				Status:      "succeeded",
				ExitCode:    0,
				LogBytes:    12,
				Report:      &record,
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "loopcoder report: worker succeeded") {
		t.Fatalf("stderr missing plain pretty report:\n%s", stderr.String())
	}
	for _, disallowed := range []string{"\u2705", "\u274c", "\u26a0", "\x1b["} {
		if strings.Contains(stderr.String(), disallowed) {
			t.Fatalf("stderr contains %q despite NO_COLOR:\n%s", disallowed, stderr.String())
		}
	}
}

func TestDispatchNoPrettySuppressesStderrWithoutChangingStdout(t *testing.T) {
	clearPrettyEnv(t)
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	record := validDispatchReport()
	result := validDispatchResult(record)
	wantStdout := expectedDispatchStdout(t, result)

	exitCode := RunWithDeps([]string{
		"dispatch",
		"--no-pretty",
		"--repo", repo,
		"--issue-number", "101",
		"--issue-title", "Implement dispatch",
	}, &stdout, &stderr, Deps{
		IsTerminal: func(io.Writer) bool {
			return false
		},
		Dispatch: func(context.Context, worker.Options) (worker.Result, error) {
			return result, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stdout.String() != wantStdout {
		t.Fatalf("stdout = %q, want %q", stdout.String(), wantStdout)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestDispatchNoPrettyEnvSuppressesStderrWithoutChangingStdout(t *testing.T) {
	clearPrettyEnv(t)
	t.Setenv("LOOPCODER_PRETTY", "1")
	t.Setenv("LOOPCODER_NO_PRETTY", "1")
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	record := validDispatchReport()
	result := validDispatchResult(record)
	wantStdout := expectedDispatchStdout(t, result)

	exitCode := RunWithDeps([]string{
		"dispatch",
		"--repo", repo,
		"--issue-number", "101",
		"--issue-title", "Implement dispatch",
	}, &stdout, &stderr, Deps{
		IsTerminal: func(io.Writer) bool {
			return false
		},
		Dispatch: func(context.Context, worker.Options) (worker.Result, error) {
			return result, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stdout.String() != wantStdout {
		t.Fatalf("stdout = %q, want %q", stdout.String(), wantStdout)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestDispatchNoPrettyFlagBeatsPrettyFlag(t *testing.T) {
	clearPrettyEnv(t)
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	record := validDispatchReport()
	result := validDispatchResult(record)
	wantStdout := expectedDispatchStdout(t, result)

	exitCode := RunWithDeps([]string{
		"dispatch",
		"--pretty",
		"--no-pretty",
		"--repo", repo,
		"--issue-number", "101",
		"--issue-title", "Implement dispatch",
	}, &stdout, &stderr, Deps{
		IsTerminal: func(io.Writer) bool {
			return false
		},
		Dispatch: func(context.Context, worker.Options) (worker.Result, error) {
			return result, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stdout.String() != wantStdout {
		t.Fatalf("stdout = %q, want %q", stdout.String(), wantStdout)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestDispatchUsesWorkerConfigModelEffortWhenFlagsAbsent(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	if err := os.WriteFile(repo+"/.delivery.yml", []byte(`version: 1
worker:
  model: config-worker-model
  reasoning_effort: config-worker-effort
`), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}

	exitCode := RunWithDeps([]string{
		"dispatch",
		"--repo", repo,
		"--issue-number", "101",
		"--issue-title", "Implement dispatch",
		"--provider", "claude",
	}, &stdout, &stderr, Deps{
		Dispatch: func(_ context.Context, opts worker.Options) (worker.Result, error) {
			if opts.Provider != "claude" {
				t.Fatalf("provider = %q, want claude", opts.Provider)
			}
			if opts.Model != "config-worker-model" || opts.Effort != "config-worker-effort" {
				t.Fatalf("dispatch opts model/effort = %#v", opts)
			}
			record := validDispatchReport()
			record.Provider = opts.Provider
			return worker.Result{
				OK:          true,
				Issue:       opts.IssueNumber,
				Branch:      "loop/issue-101",
				RunID:       "run-test",
				PR:          "https://github.com/owner/repo/pull/101",
				Summary:     "Implemented dispatch.",
				AttemptPath: "/repo/.loopcoder/runs/run-test/workers/job-101-1.attempt.json",
				Status:      "succeeded",
				ExitCode:    0,
				LogBytes:    12,
				Report:      &record,
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	for _, want := range []string{`[loopcoder] warning: worker model selection`, `provider "claude"`, `model "config-worker-model"`, "not listed"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr.String())
		}
	}
}

func TestDispatchFlagsOverrideWorkerConfigForSelectedProvider(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	if err := os.WriteFile(repo+"/.delivery.yml", []byte(`version: 1
worker:
  model: config-worker-model
  reasoning_effort: config-worker-effort
`), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}

	exitCode := RunWithDeps([]string{
		"dispatch",
		"--repo", repo,
		"--issue-number", "101",
		"--issue-title", "Implement dispatch",
		"--provider", "claude",
		"--model", "flag-worker-model",
		"--effort", "flag-worker-effort",
	}, &stdout, &stderr, Deps{
		Dispatch: func(_ context.Context, opts worker.Options) (worker.Result, error) {
			if opts.Provider != "claude" {
				t.Fatalf("provider = %q, want claude", opts.Provider)
			}
			if opts.Model != "flag-worker-model" || opts.Effort != "flag-worker-effort" {
				t.Fatalf("dispatch opts model/effort = %#v", opts)
			}
			record := validDispatchReport()
			record.Provider = opts.Provider
			return worker.Result{
				OK:          true,
				Issue:       opts.IssueNumber,
				Branch:      "loop/issue-101",
				RunID:       "run-test",
				PR:          "https://github.com/owner/repo/pull/101",
				Summary:     "Implemented dispatch.",
				AttemptPath: "/repo/.loopcoder/runs/run-test/workers/job-101-1.attempt.json",
				Status:      "succeeded",
				ExitCode:    0,
				LogBytes:    12,
				Report:      &record,
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
}

func TestDispatchUsesConfiguredWorkerProviderAndRegistryDefaults(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	if err := os.WriteFile(repo+"/.delivery.yml", []byte(`version: 1
adapters:
  worker: claude
`), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}

	exitCode := RunWithDeps([]string{
		"dispatch",
		"--repo", repo,
		"--issue-number", "101",
		"--issue-title", "Implement dispatch",
	}, &stdout, &stderr, Deps{
		Dispatch: func(_ context.Context, opts worker.Options) (worker.Result, error) {
			if opts.Provider != "claude" || opts.Model != "claude-opus-4-8[1m]" || opts.Effort != "max" {
				t.Fatalf("dispatch opts provider/model/effort = %#v", opts)
			}
			record := validDispatchReport()
			record.Provider = opts.Provider
			record.Model = opts.Model
			record.Effort = opts.Effort
			return validDispatchResult(record), nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
}

func TestDispatchStrictRejectsInvalidWorkerSelection(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	if err := os.WriteFile(repo+"/.delivery.yml", []byte(`version: 1
models:
  strict: true
worker:
  model: config-worker-model
`), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}

	called := false
	exitCode := RunWithDeps([]string{
		"dispatch",
		"--repo", repo,
		"--issue-number", "101",
		"--issue-title", "Implement dispatch",
	}, &stdout, &stderr, Deps{
		Dispatch: func(context.Context, worker.Options) (worker.Result, error) {
			called = true
			return worker.Result{}, nil
		},
	})
	if exitCode != 1 {
		t.Fatalf("RunWithDeps returned exit code %d, want 1", exitCode)
	}
	if called {
		t.Fatal("Dispatch dependency was called despite strict model rejection")
	}
	if !strings.Contains(stderr.String(), "reject") || !strings.Contains(stderr.String(), `model "config-worker-model"`) {
		t.Fatalf("stderr missing strict rejection:\n%s", stderr.String())
	}
}

func TestDispatchDoesNotRenderSuccessJSONWithoutReport(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()

	exitCode := RunWithDeps([]string{
		"dispatch",
		"--repo", repo,
		"--issue-number", "101",
		"--issue-title", "Implement dispatch",
	}, &stdout, &stderr, Deps{
		Dispatch: func(_ context.Context, opts worker.Options) (worker.Result, error) {
			return worker.Result{
				OK:     true,
				Issue:  opts.IssueNumber,
				Status: "succeeded",
			}, nil
		},
	})
	if exitCode != 1 {
		t.Fatalf("RunWithDeps returned exit code %d, want 1", exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "dispatch report is missing") {
		t.Fatalf("stderr missing report error: %q", stderr.String())
	}
}

func TestDispatchRequiresIssueFields(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := RunWithDeps([]string{"dispatch"}, &stdout, &stderr, Deps{})
	if exitCode != 2 {
		t.Fatalf("RunWithDeps returned exit code %d, want 2", exitCode)
	}
	if !strings.Contains(stderr.String(), "--repo is required") {
		t.Fatalf("stderr missing required repo message: %q", stderr.String())
	}
}

func TestDispatchWaveRunsFromReadySetWithInjectedDeps(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	var dispatchOpts worker.Options

	exitCode := RunWithDeps([]string{
		"dispatch-wave",
		"--repo", repo,
		"--from-ready-set",
		"--run-id", "run-test-wave",
		"--model", "gpt-5.5",
		"--effort", "high",
	}, &stdout, &stderr, Deps{
		Stdin: strings.NewReader(`{"ready":[{"issue":201,"title":"Wave","reason":"ready"}]}`),
		NewGitHubReader: func(string) orchestration.GitHubReader {
			return cliFakeReader{
				views: map[int]gh.Issue{
					201: {Number: 201, Title: "Wave", Body: "Body"},
				},
			}
		},
		ComputeReadySet: func(context.Context, orchestration.Options) (report.ReadySetReport, error) {
			return report.ReadySetReport{
				Repo:       "owner/repo",
				BaseBranch: "main",
				Ready: []report.ReadyIssue{{
					Issue:  201,
					Title:  "Wave",
					Reason: "ready",
				}},
			}, nil
		},
		Dispatch: func(_ context.Context, opts worker.Options) (worker.Result, error) {
			dispatchOpts = opts
			return worker.Result{
				OK:          true,
				Issue:       opts.IssueNumber,
				Branch:      "loop/issue-201",
				RunID:       opts.RunID,
				PR:          "https://github.com/owner/repo/pull/201",
				AttemptPath: "/repo/.loopcoder/runs/run-test-wave/workers/job-201.attempt.json",
				Status:      "succeeded",
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if dispatchOpts.IssueNumber != 201 || dispatchOpts.IssueTitle != "Wave" || dispatchOpts.IssueBody != "Body" {
		t.Fatalf("dispatch opts issue fields = %#v", dispatchOpts)
	}
	if dispatchOpts.RunID != "run-test-wave" || dispatchOpts.Model != "gpt-5.5" || dispatchOpts.Effort != "high" {
		t.Fatalf("dispatch opts run/model/effort = %#v", dispatchOpts)
	}
	text := stdout.String()
	for _, want := range []string{"DISPATCH WAVE", "RunId: run-test-wave", "- #201 succeeded", "Verify successful PRs"} {
		if !strings.Contains(text, want) {
			t.Fatalf("stdout missing %q:\n%s", want, text)
		}
	}
}

func TestDispatchWavePrettyDefaultNonInteractiveStreamsPlainBlocksToStdout(t *testing.T) {
	clearPrettyEnv(t)
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	now := time.Date(2026, 6, 30, 1, 2, 3, 0, time.UTC)
	record201 := validDispatchReport()
	record201.Action = "implement issue #201"
	record202 := validDispatchReport()
	record202.Action = "implement issue #202"

	expectedReport := orchestration.DispatchWaveReport{
		Repo:            "owner/repo",
		BaseBranch:      "main",
		RunID:           "run-test-wave",
		IssuesRequested: []int{201, 202},
		StartedAt:       now.UTC().Format(time.RFC3339),
		FinishedAt:      now.UTC().Format(time.RFC3339),
		Results: []orchestration.DispatchWaveIssueResult{
			{
				Issue:       201,
				Status:      orchestration.DispatchWaveStatusSucceeded,
				Branch:      "loop/issue-201",
				PR:          "https://github.com/owner/repo/pull/201",
				AttemptPath: "/repo/.loopcoder/runs/run-test-wave/workers/job-201.attempt.json",
				Report:      &record201,
			},
			{
				Issue:       202,
				Status:      orchestration.DispatchWaveStatusSucceeded,
				Branch:      "loop/issue-202",
				PR:          "https://github.com/owner/repo/pull/202",
				AttemptPath: "/repo/.loopcoder/runs/run-test-wave/workers/job-202.attempt.json",
				Report:      &record202,
			},
		},
	}
	wantStdout := orchestration.RenderDispatchWaveIssueCompletion(expectedReport.Results[0], dispatchPrettyBlock(record201, expectedReport.Results[0].Status, expectedReport.Results[0].PR, expectedReport.Results[0].Error, reporter.PrettyModePlain)) +
		orchestration.RenderDispatchWaveIssueCompletion(expectedReport.Results[1], dispatchPrettyBlock(record202, expectedReport.Results[1].Status, expectedReport.Results[1].PR, expectedReport.Results[1].Error, reporter.PrettyModePlain)) +
		orchestration.RenderDispatchWaveText(expectedReport)

	exitCode := RunWithDeps([]string{
		"dispatch-wave",
		"--repo", repo,
		"--issue-numbers", "201,202",
		"--run-id", "run-test-wave",
		"--throttle-limit", "1",
	}, &stdout, &stderr, Deps{
		IsTerminal: func(io.Writer) bool {
			return false
		},
		Now: func() time.Time {
			return now
		},
		NewGitHubReader: func(string) orchestration.GitHubReader {
			return cliFakeReader{
				views: map[int]gh.Issue{
					201: {Number: 201, Title: "Wave 201", Body: "Body 201"},
					202: {Number: 202, Title: "Wave 202", Body: "Body 202"},
				},
			}
		},
		ComputeReadySet: func(context.Context, orchestration.Options) (report.ReadySetReport, error) {
			return report.ReadySetReport{
				Repo:       "owner/repo",
				BaseBranch: "main",
				Ready: []report.ReadyIssue{
					{Issue: 201, Title: "Wave 201", Reason: "ready"},
					{Issue: 202, Title: "Wave 202", Reason: "ready"},
				},
			}, nil
		},
		Dispatch: func(_ context.Context, opts worker.Options) (worker.Result, error) {
			switch opts.IssueNumber {
			case 201:
				return worker.Result{
					OK:          true,
					Issue:       opts.IssueNumber,
					Branch:      "loop/issue-201",
					RunID:       opts.RunID,
					PR:          "https://github.com/owner/repo/pull/201",
					AttemptPath: "/repo/.loopcoder/runs/run-test-wave/workers/job-201.attempt.json",
					Status:      "succeeded",
					Report:      &record201,
				}, nil
			case 202:
				return worker.Result{
					OK:          true,
					Issue:       opts.IssueNumber,
					Branch:      "loop/issue-202",
					RunID:       opts.RunID,
					PR:          "https://github.com/owner/repo/pull/202",
					AttemptPath: "/repo/.loopcoder/runs/run-test-wave/workers/job-202.attempt.json",
					Status:      "succeeded",
					Report:      &record202,
				}, nil
			default:
				t.Fatalf("unexpected issue number %d", opts.IssueNumber)
				return worker.Result{}, nil
			}
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stdout.String() != wantStdout {
		t.Fatalf("stdout = %q, want %q", stdout.String(), wantStdout)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	gotStdout := stdout.String()
	if count := strings.Count(gotStdout, "loopcoder report: worker succeeded"); count != 2 {
		t.Fatalf("stdout pretty block count = %d, want 2:\n%s", count, gotStdout)
	}
	for _, want := range []string{
		"- action: \"implement issue #201\"",
		"- action: \"implement issue #202\"",
	} {
		if !strings.Contains(gotStdout, want) {
			t.Fatalf("stdout missing %q:\n%s", want, gotStdout)
		}
	}
	for _, disallowed := range []string{"\u2705", "\u274c", "\u26a0"} {
		if strings.Contains(gotStdout, disallowed) {
			t.Fatalf("plain stdout contains %q:\n%s", disallowed, gotStdout)
		}
	}
	if pending := relaygate.Check(repo); len(pending) != 2 {
		t.Fatalf("pending relay records = %d, want 2", len(pending))
	}
}

func TestDispatchWaveCompletionErrorStillWritesAggregateReport(t *testing.T) {
	clearPrettyEnv(t)
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	relayDir := filepath.Join(repo, ".loopcoder", "relay")
	if err := os.MkdirAll(relayDir, 0o755); err != nil {
		t.Fatalf("mkdir relay dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(relayDir, "pending"), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write pending file: %v", err)
	}

	now := time.Date(2026, 6, 30, 1, 2, 3, 0, time.UTC)
	record201 := validDispatchReport()
	record201.Action = "implement issue #201"
	record202 := validDispatchReport()
	record202.Action = "implement issue #202"

	exitCode := RunWithDeps([]string{
		"dispatch-wave",
		"--repo", repo,
		"--issue-numbers", "201,202",
		"--run-id", "run-test-wave",
		"--throttle-limit", "1",
	}, &stdout, &stderr, Deps{
		IsTerminal: func(io.Writer) bool {
			return false
		},
		Now: func() time.Time {
			return now
		},
		NewGitHubReader: func(string) orchestration.GitHubReader {
			return cliFakeReader{
				views: map[int]gh.Issue{
					201: {Number: 201, Title: "Wave 201", Body: "Body 201"},
					202: {Number: 202, Title: "Wave 202", Body: "Body 202"},
				},
			}
		},
		ComputeReadySet: func(context.Context, orchestration.Options) (report.ReadySetReport, error) {
			return report.ReadySetReport{
				Repo:       "owner/repo",
				BaseBranch: "main",
				Ready: []report.ReadyIssue{
					{Issue: 201, Title: "Wave 201", Reason: "ready"},
					{Issue: 202, Title: "Wave 202", Reason: "ready"},
				},
			}, nil
		},
		Dispatch: func(_ context.Context, opts worker.Options) (worker.Result, error) {
			record := &record201
			if opts.IssueNumber == 202 {
				record = &record202
			}
			return worker.Result{
				OK:          true,
				Issue:       opts.IssueNumber,
				Branch:      "loop/issue-" + strconv.Itoa(opts.IssueNumber),
				RunID:       opts.RunID,
				PR:          "https://github.com/owner/repo/pull/" + strconv.Itoa(opts.IssueNumber),
				AttemptPath: "/repo/.loopcoder/runs/run-test-wave/workers/job-" + strconv.Itoa(opts.IssueNumber) + ".attempt.json",
				Status:      "succeeded",
				Report:      record,
			}, nil
		},
	})
	if exitCode != 1 {
		t.Fatalf("RunWithDeps returned exit code %d, want 1; stderr=%q", exitCode, stderr.String())
	}
	text := stdout.String()
	if !strings.HasPrefix(text, "DISPATCH WAVE\n") {
		t.Fatalf("stdout should start with aggregate report, got:\n%s", text)
	}
	for _, want := range []string{"RunId: run-test-wave", "- #201 succeeded", "- #202 succeeded", "Verify successful PRs"} {
		if !strings.Contains(text, want) {
			t.Fatalf("stdout missing %q:\n%s", want, text)
		}
	}
	for _, want := range []string{
		"relay gate: could not read pending records:",
		"dispatch-wave: write relay record for worker #201:",
		"dispatch-wave: pending relay records may remain;",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr.String())
		}
	}
}

func TestRecoverRunsWithInjectedRecoverAndAliases(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()

	exitCode := RunWithDeps([]string{
		"recover",
		"-Repo", repo,
		"-IssueNumber", "103",
		"-IssueTitle", "Implement recover",
		"-IssueBody", "Body",
		"-RunId", "run-test",
		"-BaseBranch", "trunk",
		"-MaxAttempts", "4",
		"-BackoffSeconds", "1,2,3",
		"-Provider", "codex",
		"-Model", "gpt-5.5",
		"-Effort", "high",
		"-UpgradedModel", "gpt-6",
		"-UpgradedEffort", "xhigh",
		"-VerifierProvider", "claude",
		"-VerifierModel", "claude-opus-4-8[1m]",
		"-VerifierEffort", "max",
		"-VerifierTimeout", "15s",
	}, &stdout, &stderr, Deps{
		Recover: func(_ context.Context, opts recovery.Options) (recovery.Result, error) {
			if opts.RepoPath != repo {
				t.Fatalf("RepoPath = %q, want %q", opts.RepoPath, repo)
			}
			if opts.IssueNumber != 103 || opts.IssueTitle != "Implement recover" || opts.IssueBody != "Body" {
				t.Fatalf("recover opts issue fields = %#v", opts)
			}
			if opts.RunID != "run-test" || opts.BaseBranch != "trunk" || opts.MaxAttempts != 4 {
				t.Fatalf("recover opts run/base/max = %#v", opts)
			}
			if len(opts.BackoffSeconds) != 3 || opts.BackoffSeconds[0] != 1 || opts.BackoffSeconds[2] != 3 {
				t.Fatalf("BackoffSeconds = %#v, want [1 2 3]", opts.BackoffSeconds)
			}
			if opts.Provider != "codex" || opts.Model != "gpt-5.5" || opts.Effort != "high" {
				t.Fatalf("recover opts provider/model/effort = %#v", opts)
			}
			if opts.UpgradedModel != "gpt-6" || opts.UpgradedEffort != "xhigh" {
				t.Fatalf("recover opts upgraded model/effort = %#v", opts)
			}
			if opts.VerifierProvider != "claude" || opts.VerifierModel != "claude-opus-4-8[1m]" || opts.VerifierEffort != "max" || opts.VerifierTimeout != 15*time.Second {
				t.Fatalf("recover opts verifier fields = %#v", opts)
			}
			if opts.Stderr == nil {
				t.Fatal("recover opts Stderr is nil")
			}
			return recovery.Result{
				Action: recovery.ActionRetry,
				Report: "RETRY: dispatching issue #103 attempt 2\n",
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if !strings.Contains(stdout.String(), "RETRY: dispatching issue #103 attempt 2") {
		t.Fatalf("stdout missing retry report: %q", stdout.String())
	}
}

func TestRecoverRequiresRunID(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := RunWithDeps([]string{
		"recover",
		"--repo", t.TempDir(),
		"--issue-number", "103",
		"--issue-title", "Implement recover",
	}, &stdout, &stderr, Deps{})
	if exitCode != 2 {
		t.Fatalf("RunWithDeps returned exit code %d, want 2", exitCode)
	}
	if !strings.Contains(stderr.String(), "--run-id is required") {
		t.Fatalf("stderr missing required run-id message: %q", stderr.String())
	}
}

func TestResumeRunsWithInjectedReaderAndDefaultConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()

	exitCode := RunWithDeps([]string{"resume", "--repo", repo}, &stdout, &stderr, Deps{
		NewGitHubReader: func(string) orchestration.GitHubReader {
			return cliFakeReader{
				issues: []gh.Issue{{Number: 97, Title: "Implement resume", State: "OPEN"}},
			}
		},
		ProcessAlive: func(int) bool { return false },
		Now: func() time.Time {
			return time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	text := stdout.String()
	for _, want := range []string{
		"RESUME REPORT",
		"Repo: owner/repo",
		"RunId: (none) (.loopcoder/runs not found)",
		"GitHub snapshot: open issues=1, open PRs=0",
		"Local state: attempts=0, events=0",
		"classification: ready",
		"resume is read-only",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("stdout missing %q:\n%s", want, text)
		}
	}
}

func TestResumeRendersJSONWithRunTreeAndRecoveryDecision(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	if err := state.AppendLifecycleTransition(repo, state.LifecycleTransition{
		Timestamp:  "2026-07-09T00:00:00Z",
		RunID:      "run-parent",
		State:      state.StateRunning,
		ChildRunID: "run-child",
	}); err != nil {
		t.Fatalf("append parent lifecycle: %v", err)
	}
	if err := state.AppendLifecycleTransition(repo, state.LifecycleTransition{
		Timestamp:   "2026-07-09T00:00:01Z",
		RunID:       "run-child",
		ParentRunID: "run-parent",
		State:       state.StateFailed,
	}); err != nil {
		t.Fatalf("append child lifecycle: %v", err)
	}

	exitCode := RunWithDeps([]string{"resume", "--repo", repo, "--run-id", "run-parent", "--format", "json"}, &stdout, &stderr, Deps{
		NewGitHubReader: func(string) orchestration.GitHubReader {
			return cliFakeReader{
				issues: []gh.Issue{{Number: 650, Title: "Interrupted child", State: "OPEN"}},
			}
		},
		ProcessAlive: func(int) bool { return false },
		Now: func() time.Time {
			return time.Date(2026, 7, 9, 0, 1, 0, 0, time.UTC)
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	var got struct {
		RunTree struct {
			Nodes []struct {
				RunID            string `json:"run_id"`
				RecoveryDecision struct {
					Outcome      string `json:"outcome"`
					RetryAllowed bool   `json:"retry_allowed"`
				} `json:"recovery_decision"`
			} `json:"nodes"`
		} `json:"run_tree"`
		Issues []struct {
			RecoveryDecision struct {
				Outcome      string `json:"outcome"`
				SafeToResume bool   `json:"safe_to_resume"`
			} `json:"recovery_decision"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("resume JSON did not unmarshal: %v\n%s", err, stdout.String())
	}
	if len(got.RunTree.Nodes) != 2 {
		t.Fatalf("run tree nodes = %#v, want parent and child", got.RunTree.Nodes)
	}
	childRetry := false
	for _, node := range got.RunTree.Nodes {
		if node.RunID == "run-child" && node.RecoveryDecision.Outcome == "retry" && node.RecoveryDecision.RetryAllowed {
			childRetry = true
		}
	}
	if !childRetry {
		t.Fatalf("run tree missing retryable child decision: %#v", got.RunTree.Nodes)
	}
	if len(got.Issues) != 1 || got.Issues[0].RecoveryDecision.Outcome != "dispatch" || !got.Issues[0].RecoveryDecision.SafeToResume {
		t.Fatalf("issue recovery decision = %#v", got.Issues)
	}
}

func TestResumeRequiresRepo(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := RunWithDeps([]string{"resume"}, &stdout, &stderr, Deps{})
	if exitCode != 2 {
		t.Fatalf("RunWithDeps returned exit code %d, want 2", exitCode)
	}
	if !strings.Contains(stderr.String(), "--repo is required") {
		t.Fatalf("stderr missing required repo message: %q", stderr.String())
	}
}

func clearPrettyEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"LOOPCODER_PRETTY", "LOOPCODER_NO_PRETTY", "LOOPCODER_NO_EMOJI", "LOOPCODER_PLAIN", "NO_COLOR"} {
		name := name
		old, ok := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
		t.Cleanup(func() {
			if ok {
				if err := os.Setenv(name, old); err != nil {
					t.Fatalf("restore %s: %v", name, err)
				}
			} else if err := os.Unsetenv(name); err != nil {
				t.Fatalf("restore unset %s: %v", name, err)
			}
		})
	}
}

func expectedDispatchStdout(t *testing.T, result worker.Result) string {
	t.Helper()
	if result.Report == nil {
		t.Fatal("expected dispatch result report")
	}
	canonical, err := result.Report.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON returned error: %v", err)
	}
	data, err := worker.MarshalResult(result)
	if err != nil {
		t.Fatalf("MarshalResult returned error: %v", err)
	}
	return result.Report.Header() + "\n" + string(canonical) + "\n" + string(data) + "\n"
}

func expectedLoopreviewStdout(t *testing.T, result loopreview.Result) string {
	t.Helper()
	data, err := json.Marshal(result.Verdict)
	if err != nil {
		t.Fatalf("Marshal verdict returned error: %v", err)
	}
	return string(data) + "\n"
}

func assertOptionalInt64(t *testing.T, name string, got, want *int64) {
	t.Helper()
	if got == nil || want == nil {
		if got != want {
			t.Fatalf("%s token pointer = %#v, want %#v", name, got, want)
		}
		return
	}
	if *got != *want {
		t.Fatalf("%s tokens = %d, want %d", name, *got, *want)
	}
}

func nonEmptyLines(output string) []string {
	rawLines := strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n")
	lines := make([]string, 0, len(rawLines))
	for _, line := range rawLines {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func readSingleFile(t *testing.T, pattern string) string {
	t.Helper()
	matches, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatalf("glob %q: %v", pattern, err)
	}
	if len(matches) != 1 {
		t.Fatalf("glob %q matched %d files, want 1: %#v", pattern, len(matches), matches)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read %s: %v", matches[0], err)
	}
	return string(data)
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	data, err := os.ReadFile(filepath.Join(repoRoot, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

func writeNestedPlanFixture(t *testing.T, repo string, items []orchestration.ChildRunPlan, maxConcurrency int) string {
	t.Helper()
	for i := range items {
		items[i].RunID = state.RunIDForChild(items[i].ChildKey, i, fixedCLINow())
		items[i].Ordinal = i
	}
	plan := orchestration.ChildPlan{
		SchemaVersion:  orchestration.ChildPlanSchemaVersionV1,
		PlanID:         "plan-run-20260102T030405Z-wave",
		ParentRunID:    state.RunIDForWave(fixedCLINow()),
		RootRunID:      state.RunIDForWave(fixedCLINow()),
		ParentDepth:    0,
		MaxDepth:       2,
		MaxConcurrency: maxConcurrency,
		CreatedAt:      state.FormatTimestamp(fixedCLINow()),
		Items:          items,
	}
	data, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		t.Fatalf("marshal child plan: %v", err)
	}
	path := filepath.Join(repo, "child-plan.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write child plan: %v", err)
	}
	return path
}

func nestedPlanItem(key string, issue int, dependsOn []string, required bool, permission string, commands []string) orchestration.ChildRunPlan {
	return orchestration.ChildRunPlan{
		ChildKey:   key,
		Title:      key,
		Role:       string(reporter.RoleWorker),
		Issue:      issue,
		Scope:      orchestration.ChildScope{Repo: ".", Paths: []string{"internal/cli/nested.go"}, Issues: []int{issue}, Commands: commands},
		Permission: permission,
		DependsOn:  append([]string(nil), dependsOn...),
		Aggregation: orchestration.ChildAggregation{
			Mode:          orchestration.ChildAggregationCollect,
			Required:      required,
			IncludeReport: true,
		},
	}
}

func initRepoWithDeliveryOnlyOnBranch(t *testing.T, baseBranch string) string {
	t.Helper()
	repo := t.TempDir()
	runCLITestGit(t, repo, "init", "-b", "main")
	runCLITestGit(t, repo, "config", "user.email", "loopcoder-test@example.com")
	runCLITestGit(t, repo, "config", "user.name", "Loopcoder Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# Test\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runCLITestGit(t, repo, "add", "README.md")
	runCLITestGit(t, repo, "commit", "-m", "initial")
	runCLITestGit(t, repo, "checkout", "-b", baseBranch)
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte("version: 1\nworker:\n  base_branch: "+baseBranch+"\n"), 0o644); err != nil {
		t.Fatalf("write .delivery.yml: %v", err)
	}
	runCLITestGit(t, repo, "add", ".delivery.yml")
	runCLITestGit(t, repo, "commit", "-m", "add delivery config")
	runCLITestGit(t, repo, "checkout", "main")
	return repo
}

func initProjectRegistryCLITestRepo(t *testing.T, remote string) string {
	t.Helper()
	repo := t.TempDir()
	runCLITestGit(t, repo, "init", "-b", "main")
	runCLITestGit(t, repo, "config", "user.email", "loopcoder-test@example.com")
	runCLITestGit(t, repo, "config", "user.name", "Loopcoder Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# Test\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runCLITestGit(t, repo, "add", "README.md")
	runCLITestGit(t, repo, "commit", "-m", "initial")
	runCLITestGit(t, repo, "remote", "add", "origin", remote)
	return repo
}

func fixedCLINow() time.Time {
	return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
}

func runCLITestGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmdArgs := append([]string{"-C", repo}, args...)
	if len(args) > 0 && args[0] == "init" {
		cmdArgs = args
		cmd := exec.Command("git", cmdArgs...)
		cmd.Dir = repo
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, string(output))
		}
		return
	}
	cmd := exec.Command("git", cmdArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(cmdArgs, " "), err, string(output))
	}
}

func readAllSQLiteText(t *testing.T, dbPath string) string {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	defer db.Close()
	tables, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatalf("query sqlite tables: %v", err)
	}
	defer tables.Close()
	var out strings.Builder
	for tables.Next() {
		var table string
		if err := tables.Scan(&table); err != nil {
			t.Fatalf("scan table: %v", err)
		}
		columns := sqliteTextColumns(t, db, table)
		for _, column := range columns {
			rows, err := db.Query(`SELECT ` + quoteSQLiteIdentifier(column) + ` FROM ` + quoteSQLiteIdentifier(table))
			if err != nil {
				t.Fatalf("query %s.%s: %v", table, column, err)
			}
			for rows.Next() {
				var value sql.NullString
				if err := rows.Scan(&value); err != nil {
					rows.Close()
					t.Fatalf("scan %s.%s: %v", table, column, err)
				}
				if value.Valid {
					out.WriteString(table)
					out.WriteByte('.')
					out.WriteString(column)
					out.WriteByte('=')
					out.WriteString(value.String)
					out.WriteByte('\n')
				}
			}
			if err := rows.Close(); err != nil {
				t.Fatalf("close %s.%s rows: %v", table, column, err)
			}
		}
	}
	if err := tables.Err(); err != nil {
		t.Fatalf("iterate sqlite tables: %v", err)
	}
	return out.String()
}

func sqliteTextColumns(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + quoteSQLiteIdentifier(table) + `)`)
	if err != nil {
		t.Fatalf("pragma table_info(%s): %v", table, err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var cid int
		var name string
		var typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			t.Fatalf("scan table_info(%s): %v", table, err)
		}
		if strings.Contains(strings.ToUpper(typ), "TEXT") {
			columns = append(columns, name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table_info(%s): %v", table, err)
	}
	return columns
}

func quoteSQLiteIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

func cliPendingPrettyBlock(role string) string {
	return strings.Join([]string{
		"TEST RELAY BLOCK",
		"role=" + role,
		"pr=101",
	}, "\n") + "\n"
}

type relayFlushAckSabotageWriter struct {
	t         *testing.T
	path      string
	buf       bytes.Buffer
	sabotaged bool
}

func (w *relayFlushAckSabotageWriter) Write(p []byte) (int, error) {
	if _, err := w.buf.Write(p); err != nil {
		return 0, err
	}
	if !w.sabotaged {
		w.sabotaged = true
		if err := os.Remove(w.path); err != nil {
			w.t.Fatalf("remove pending file before sabotage: %v", err)
		}
		if err := os.Mkdir(w.path, 0o755); err != nil {
			w.t.Fatalf("mkdir pending record path: %v", err)
		}
		if err := os.WriteFile(filepath.Join(w.path, "child"), []byte("keep directory non-empty"), 0o600); err != nil {
			w.t.Fatalf("write pending record child: %v", err)
		}
	}
	return len(p), nil
}

func (w *relayFlushAckSabotageWriter) String() string {
	return w.buf.String()
}

func writePendingRelayForCLITest(t *testing.T, repo, role string, pr int, block string) relaygate.Record {
	t.Helper()
	if _, err := relaygate.Write(relaygate.WriteOptions{
		RepoPath: repo,
		RunID:    "run-test",
		Role:     role,
		PRNumber: pr,
		Block:    block,
	}); err != nil {
		t.Fatalf("relaygate.Write: %v", err)
	}
	records := relaygate.Check(repo)
	if len(records) != 1 {
		t.Fatalf("relaygate.Check returned %d records, want 1", len(records))
	}
	return records[0]
}

func cliTriggerTickReport(opts orchestration.TickOptions, status, stopReason string) orchestration.TickReport {
	baseBranch := opts.BaseBranch
	if strings.TrimSpace(baseBranch) == "" {
		baseBranch = "main"
	}
	preProdBranch := opts.PreProdBranch
	if strings.TrimSpace(preProdBranch) == "" {
		preProdBranch = "pre-prod"
	}
	return orchestration.TickReport{
		Version:       orchestration.TickReportVersion,
		Repo:          "owner/repo",
		RepoPath:      opts.RepoPath,
		BaseBranch:    baseBranch,
		PreProdBranch: preProdBranch,
		RunID:         opts.RunID,
		Status:        status,
		StopReason:    stopReason,
		StartedAt:     "2026-07-02T12:00:00Z",
		FinishedAt:    "2026-07-02T12:00:00Z",
		NeedsHuman:    []orchestration.TickIssue{},
		Failures:      []orchestration.TickIssue{},
	}
}

func validDispatchReport() reporter.Report {
	return reporter.Report{
		Role:        reporter.RoleWorker,
		Provider:    "codex",
		Model:       "gpt-5.5",
		ModelSource: reporter.ModelSourceParsed,
		Effort:      "high",
		Permission:  reporter.PermissionWrite,
		Action:      "implement issue #101",
		ExitCode:    0,
		StartedAt:   "2026-06-28T00:00:00Z",
		EndedAt:     "2026-06-28T00:00:42Z",
		DurationMS:  42000,
		Usage: reporter.Usage{
			InputTokens:  int64TestPtr(120),
			OutputTokens: int64TestPtr(34),
			TotalTokens:  int64TestPtr(154),
		},
		Verified: true,
	}
}

func validDispatchResult(record reporter.Report) worker.Result {
	return worker.Result{
		OK:          true,
		Issue:       101,
		Branch:      "loop/issue-101",
		RunID:       "run-test",
		PR:          "https://github.com/owner/repo/pull/101",
		Summary:     "Implemented dispatch.",
		AttemptPath: "/repo/.loopcoder/runs/run-test/workers/job-101-1.attempt.json",
		Status:      "succeeded",
		ExitCode:    0,
		LogBytes:    12,
		Report:      &record,
	}
}

func validLoopreviewReport() reporter.Report {
	record := validDispatchReport()
	record.Role = reporter.RoleVerifier
	record.Provider = "claude"
	record.Permission = reporter.PermissionReadOnly
	record.Action = "review PR #152"
	return record
}

func int64TestPtr(value int64) *int64 {
	return &value
}

type cliFakePromotionWriter struct{}

func (cliFakePromotionWriter) BranchHeadSHA(context.Context, string) (string, error) {
	return "main-prior-sha", nil
}

func (cliFakePromotionWriter) BranchChecks(context.Context, string) (gh.BranchChecksResult, error) {
	return gh.BranchChecksResult{
		Branch:  "pre-prod",
		HeadSHA: "preprod-sha",
		Checks:  []gh.Check{{Name: "verify", State: "success", Bucket: "pass"}},
	}, nil
}

func (cliFakePromotionWriter) CompareBranches(context.Context, string, string) ([]string, string, error) {
	return []string{"README.md"}, "diff --git a/README.md b/README.md\n", nil
}

func (cliFakePromotionWriter) KickBackFromPreProd(context.Context, string, string) (gh.PreProdKickBackResult, error) {
	return gh.PreProdKickBackResult{}, nil
}

func (cliFakePromotionWriter) RouteKickBackToNeedsHuman(context.Context, int) (gh.NeedsHumanRouteResult, error) {
	return gh.NeedsHumanRouteResult{}, nil
}

func (cliFakePromotionWriter) PromotePreProdToMain(context.Context, string) (gh.MainPromotionResult, error) {
	return gh.MainPromotionResult{}, nil
}

func (cliFakePromotionWriter) RevertProductionMerge(context.Context, string, string, string) (gh.ProductionRevertResult, error) {
	return gh.ProductionRevertResult{}, nil
}

func (cliFakePromotionWriter) SyncPreProdFromMain(context.Context, string) (gh.PreProdSyncResult, error) {
	return gh.PreProdSyncResult{}, nil
}

type cliFakeReader struct {
	issues []gh.Issue
	views  map[int]gh.Issue
	prs    []gh.PullRequest
	checks map[int][]gh.Check
}

func (f cliFakeReader) RepoName(context.Context) (string, error) {
	return "owner/repo", nil
}

func (f cliFakeReader) ListIssues(context.Context, string) ([]gh.Issue, error) {
	return f.issues, nil
}

func (f cliFakeReader) ViewIssue(_ context.Context, number int) (gh.Issue, error) {
	if f.views != nil {
		return f.views[number], nil
	}
	return gh.Issue{}, nil
}

func (f cliFakeReader) ListOpenPRs(context.Context) ([]gh.PullRequest, error) {
	return append([]gh.PullRequest(nil), f.prs...), nil
}

func (f cliFakeReader) PRChecks(_ context.Context, number int) ([]gh.Check, error) {
	return append([]gh.Check(nil), f.checks[number]...), nil
}

type cliFakeIssueWriter struct {
	issues     map[int]gh.Issue
	nextNumber int
}

func newCLIFakeIssueWriter() *cliFakeIssueWriter {
	return &cliFakeIssueWriter{
		issues:     map[int]gh.Issue{},
		nextNumber: 1,
	}
}

func (f *cliFakeIssueWriter) RepoName(context.Context) (string, error) {
	return "owner/repo", nil
}

func (f *cliFakeIssueWriter) ListIssues(context.Context, string) ([]gh.Issue, error) {
	out := make([]gh.Issue, 0, len(f.issues))
	for _, issue := range f.issues {
		out = append(out, issue)
	}
	return out, nil
}

func (f *cliFakeIssueWriter) CreateIssue(_ context.Context, title, body string, labels []string) (gh.Issue, error) {
	number := f.nextNumber
	f.nextNumber++
	issue := gh.Issue{
		Number: number,
		Title:  title,
		Body:   body,
		State:  "OPEN",
		Labels: cliLabels(labels),
	}
	f.issues[number] = issue
	return issue, nil
}

func (f *cliFakeIssueWriter) UpdateIssue(_ context.Context, number int, title, body string, addLabels, removeLabels []string) (gh.Issue, error) {
	issue := f.issues[number]
	issue.Title = title
	issue.Body = body
	issue.Labels = cliApplyLabelChanges(issue.Labels, addLabels, removeLabels)
	f.issues[number] = issue
	return issue, nil
}

func (f *cliFakeIssueWriter) CloseIssue(_ context.Context, number int) error {
	issue := f.issues[number]
	issue.State = "CLOSED"
	f.issues[number] = issue
	return nil
}

func cliLabels(names []string) []gh.Label {
	labels := make([]gh.Label, 0, len(names))
	for _, name := range names {
		labels = append(labels, gh.Label{Name: name})
	}
	return labels
}

func cliApplyLabelChanges(labels []gh.Label, addLabels, removeLabels []string) []gh.Label {
	remove := map[string]bool{}
	for _, label := range removeLabels {
		remove[label] = true
	}
	seen := map[string]bool{}
	out := make([]gh.Label, 0, len(labels)+len(addLabels))
	for _, label := range labels {
		if remove[label.Name] {
			continue
		}
		seen[label.Name] = true
		out = append(out, label)
	}
	for _, label := range addLabels {
		if !seen[label] {
			out = append(out, gh.Label{Name: label})
		}
	}
	return out
}
