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

	"github.com/jasonhnd/loopcoder/internal/claudehooks"
	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/localcleanup"
	"github.com/jasonhnd/loopcoder/internal/migration"
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
		"migration status",
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
		Checks   []struct {
			Name       string `json:"name"`
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
	if len(payload.Checks) != 2 {
		t.Fatalf("checks len = %d, want 2", len(payload.Checks))
	}
	if payload.Checks[1].Name != "tracked .loopcoder" || payload.Checks[1].FixCommand == "" || !payload.Checks[1].Hard {
		t.Fatalf("tracked check = %#v", payload.Checks[1])
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
	if !strings.Contains(check.Message, "not found on PATH") {
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

func TestRunChecksAntigravityProviderWithAgyModels(t *testing.T) {
	env := healthyDoctorEnv()
	env.cfg.Adapters.Worker = "antigravity"
	env.paths["agy"] = "/bin/agy"
	env.commands[cmdKey("agy", "models")] = CommandResult{
		Stdout: "Gemini 3.1 Pro\n",
	}

	report := Run(context.Background(), Options{RepoPath: "/repo"}, env.deps())

	if got := report.ExitCode(); got != 0 {
		t.Fatalf("ExitCode = %d, want 0", got)
	}
	check := requireCheck(t, report, "provider antigravity")
	if check.Status != StatusOK {
		t.Fatalf("provider antigravity status = %s, want ok (%s)", check.Status, check.Message)
	}
	for _, want := range []string{`CLI "agy" found at /bin/agy`, "agy models OAuth probe succeeded"} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("provider antigravity message = %q, want containing %q", check.Message, want)
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
	for _, want := range []string{`CLI "agy" was not found on PATH`, "run: agy login"} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("provider antigravity message = %q, want containing %q", check.Message, want)
		}
	}
}

func TestRunFailsAntigravityProviderWhenAgyModelsFails(t *testing.T) {
	env := healthyDoctorEnv()
	env.cfg.Adapters.Verifier = "antigravity"
	env.paths["agy"] = "/bin/agy"
	env.commands[cmdKey("agy", "models")] = CommandResult{
		Stderr:   "OAuth login required\n",
		ExitCode: 1,
	}

	report := Run(context.Background(), Options{RepoPath: "/repo"}, env.deps())

	if got := report.ExitCode(); got != 1 {
		t.Fatalf("ExitCode = %d, want 1", got)
	}
	check := requireCheck(t, report, "provider antigravity")
	if check.Status != StatusFail || !check.Hard {
		t.Fatalf("provider antigravity check = %#v, want hard fail", check)
	}
	for _, want := range []string{"agy models failed", "OAuth login required", "run: agy login"} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("provider antigravity message = %q, want containing %q", check.Message, want)
		}
	}
	auditProvider := requireCheck(t, report, "audit llm provider")
	if auditProvider.Status != StatusOK || !strings.Contains(auditProvider.Message, `CLI "agy" resolves`) {
		t.Fatalf("audit llm provider check = %#v, want agy CLI resolution", auditProvider)
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
	for _, want := range []string{"legacy surface(s)", "run: loopcoder doctor --repo . --fix"} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("message = %q, want containing %q", check.Message, want)
		}
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
