package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/attestation"
	compiler "github.com/jasonhnd/loopcoder/internal/compile"
	"github.com/jasonhnd/loopcoder/internal/doctor"
	"github.com/jasonhnd/loopcoder/internal/loopreview"
	"github.com/jasonhnd/loopcoder/internal/orchestration"
	"github.com/jasonhnd/loopcoder/internal/recovery"
	"github.com/jasonhnd/loopcoder/internal/report"
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
	for _, want := range []string{"loopcoder loopreview", "--repo", "--pr-number", "--provider", "--base-branch", "--model", "--effort", "--timeout", "--pretty", "--no-pretty", "LOOPCODER_PRETTY", "LOOPCODER_NO_PRETTY"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q:\n%s", want, help)
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
	record := validDispatchAttestation()
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
		Attestation:    &record,
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
	for _, want := range []string{"--model string", "worker model override", "--effort string", "worker reasoning effort override", "--pretty", "--no-pretty", "LOOPCODER_PRETTY", "LOOPCODER_NO_PRETTY"} {
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
	for _, want := range []string{"loopcoder dispatch-wave", "--pretty", "--no-pretty", "LOOPCODER_PRETTY", "LOOPCODER_NO_PRETTY", "plain on non-TTY"} {
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
	for _, want := range []string{"loopcoder doctor", "--repo"} {
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
	for _, want := range []string{"loopcoder init", "--force", "--worker-model", "--worker-effort", "--verifier-model", "--verifier-effort"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q:\n%s", want, help)
		}
	}
}

func TestInitRunsWithInjectedDepsAndAliases(t *testing.T) {
	var stdout, stderr bytes.Buffer
	called := false

	exitCode := RunWithDeps([]string{
		"init",
		"-Force",
		"-WorkerModel", "gpt-5",
		"-WorkerEffort", "high",
		"-VerifierModel", "claude-sonnet",
		"-VerifierEffort", "max",
	}, &stdout, &stderr, Deps{
		Init: func(_ context.Context, opts scaffold.Options) (scaffold.Result, error) {
			called = true
			if strings.TrimSpace(opts.RepoPath) == "" {
				t.Fatal("RepoPath is empty")
			}
			if !opts.Force || opts.WorkerModel != "gpt-5" || opts.WorkerEffort != "high" || opts.VerifierModel != "claude-sonnet" || opts.VerifierEffort != "max" {
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
				Warnings: []string{"gh label setup skipped: gh not found"},
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if !called {
		t.Fatal("Init dependency was not called")
	}
	for _, want := range []string{"loopcoder init complete", "overwritten .delivery.yml", "exists ROADMAP.md", "created label delivery:unit"} {
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
		"Skill refresh: /home/.loopcoder/bin/loopcoder skill install",
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
	if !strings.Contains(stdout.String(), "Skill refresh: /home/.loopcoder/bin/loopcoder skill install") {
		t.Fatalf("stdout missing skill refresh line:\n%s", stdout.String())
	}
	for _, want := range []string{"[loopcoder] warning: skill refresh failed after upgrade", "permission denied", "run: loopcoder skill install"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr.String())
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
			var record attestation.AttestationRecord
			if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
				t.Fatalf("stdout first line is not attestation JSON: %v\n%s", err, stdout.String())
			}
			if err := record.Validate(); err != nil {
				t.Fatalf("attestation JSON does not validate: %v", err)
			}
			if record.ModelSource != attestation.ModelSourceSelfReported {
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
		"⚠️ attestation self-reported",
		"   role        conductor",
		"   model       gpt-5 (self-reported)",
		"   tokens      total=18,266",
		"   verified    false",
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
		"attestation: self-reported",
		"  role        conductor",
		"  model       gpt-5 (self-reported)",
		"  tokens      total=12,345",
		"  verified    false",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stdout missing %q:\n%s", want, got)
		}
	}
	for _, disallowed := range []string{"✅", "❌", "⚠", "\x1b["} {
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
				"invalid attestation record",
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
				"invalid attestation record",
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
		"-Model", "claude-opus",
		"-Effort", "max",
		"-Timeout", "15s",
	}, &stdout, &stderr, Deps{
		Loopreview: func(_ context.Context, opts loopreview.Options) (loopreview.Result, error) {
			called = true
			if opts.RepoPath != repo || opts.PRNumber != 152 || opts.Provider != "claude" || opts.BaseBranch != "trunk" || opts.Timeout != 15*time.Second {
				t.Fatalf("loopreview opts = %#v", opts)
			}
			if opts.Model != "claude-opus" || opts.Effort != "max" {
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
	record := validLoopreviewAttestation()
	result := loopreview.Result{
		Verdict: loopreview.Verdict{
			Verdict:         loopreview.VerdictPass,
			Findings:        []loopreview.Finding{},
			Evidence:        "review passed",
			SpecConformance: loopreview.SpecConformancePass,
			Attestation:     &record,
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
		"attestation: verified",
		"  role        verifier",
		"  permission  read-only",
		"  action      \"review PR #152\"",
	} {
		if !strings.Contains(gotStderr, want) {
			t.Fatalf("stderr missing %q:\n%s", want, gotStderr)
		}
	}
	for _, disallowed := range []string{"✅", "❌", "⚠"} {
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
	record := validLoopreviewAttestation()
	result := loopreview.Result{
		Verdict: loopreview.Verdict{
			Verdict:         loopreview.VerdictPass,
			Findings:        []loopreview.Finding{},
			Evidence:        "review passed",
			SpecConformance: loopreview.SpecConformancePass,
			Attestation:     &record,
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
		record.Pretty(attestation.PrettyOptions{Mode: attestation.PrettyModePlain}),
	} {
		if !strings.Contains(ledger, want) {
			t.Fatalf("relay ledger missing %q:\n%s", want, ledger)
		}
	}
}

func TestLoopreviewPrettyFlagWritesEmojiToStderrWithoutChangingStdout(t *testing.T) {
	clearPrettyEnv(t)
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	record := validLoopreviewAttestation()
	result := loopreview.Result{
		Verdict: loopreview.Verdict{
			Verdict:         loopreview.VerdictPass,
			Findings:        []loopreview.Finding{},
			Evidence:        "review passed",
			SpecConformance: loopreview.SpecConformancePass,
			Attestation:     &record,
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
		"✅ attestation verified",
		"   role        verifier",
		"   permission  read-only",
		"   action      \"review PR #152\"",
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
	record := validLoopreviewAttestation()
	result := loopreview.Result{
		Verdict: loopreview.Verdict{
			Verdict:         loopreview.VerdictPass,
			Findings:        []loopreview.Finding{},
			Evidence:        "review passed",
			SpecConformance: loopreview.SpecConformancePass,
			Attestation:     &record,
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
	record := validLoopreviewAttestation()

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
					Attestation:     &record,
				},
				ExitCode: 0,
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "attestation: verified") {
		t.Fatalf("stderr missing plain pretty attestation:\n%s", stderr.String())
	}
	for _, disallowed := range []string{"✅", "❌", "⚠"} {
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

func TestDispatchRunsWithInjectedWorker(t *testing.T) {
	clearPrettyEnv(t)
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	record := validDispatchAttestation()

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
				Attestation: &record,
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	gotStderr := stderr.String()
	for _, want := range []string{
		"attestation: verified",
		"  role        worker",
		"  permission  write",
		"  action      \"implement issue #101\"",
	} {
		if !strings.Contains(gotStderr, want) {
			t.Fatalf("stderr missing %q:\n%s", want, gotStderr)
		}
	}
	for _, disallowed := range []string{"✅", "❌", "⚠"} {
		if strings.Contains(gotStderr, disallowed) {
			t.Fatalf("plain stderr contains %q:\n%s", disallowed, gotStderr)
		}
	}

	lines := nonEmptyLines(stdout.String())
	if len(lines) != 3 {
		t.Fatalf("stdout lines = %d, want 3:\n%s", len(lines), stdout.String())
	}
	var attestationLine attestation.AttestationRecord
	if err := json.Unmarshal([]byte(lines[1]), &attestationLine); err != nil {
		t.Fatalf("stdout second line is not attestation JSON: %v\n%s", err, stdout.String())
	}
	if err := attestationLine.Validate(); err != nil {
		t.Fatalf("attestation JSON does not validate: %v", err)
	}
	if lines[0] != attestationLine.Header() {
		t.Fatalf("stdout first line = %q, want %q", lines[0], attestationLine.Header())
	}
	canonical, err := attestationLine.CanonicalJSON()
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
	if got.Attestation == nil {
		t.Fatalf("dispatch JSON missing attestation: %s", lines[2])
	}
	nestedCanonical, err := got.Attestation.CanonicalJSON()
	if err != nil {
		t.Fatalf("nested attestation CanonicalJSON returned error: %v", err)
	}
	if string(nestedCanonical) != lines[1] {
		t.Fatalf("nested attestation = %s, want %s", string(nestedCanonical), lines[1])
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(lines[2]), &fields); err != nil {
		t.Fatalf("dispatch JSON invalid: %v", err)
	}
	for _, key := range []string{"ok", "issue", "branch", "run_id", "pr", "summary", "attempt_path", "status", "exit_code", "log_bytes", "attestation"} {
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

func TestDispatchPrettyDefaultNonInteractiveWritesPlainToStderrWithoutChangingStdout(t *testing.T) {
	clearPrettyEnv(t)
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	record := validDispatchAttestation()
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
		"attestation: verified",
		"  role        worker",
		"  tokens      input=120  output=34  total=154",
	} {
		if !strings.Contains(gotStderr, want) {
			t.Fatalf("stderr missing %q:\n%s", want, gotStderr)
		}
	}
	for _, disallowed := range []string{"✅", "❌", "⚠"} {
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
	record := validDispatchAttestation()
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
		record.Pretty(attestation.PrettyOptions{Mode: attestation.PrettyModePlain}),
	} {
		if !strings.Contains(ledger, want) {
			t.Fatalf("relay ledger missing %q:\n%s", want, ledger)
		}
	}
}

func TestDispatchPrettyWritesEmojiToStderrWhenInteractive(t *testing.T) {
	clearPrettyEnv(t)
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	record := validDispatchAttestation()
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
		Attestation: &record,
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
		"✅ attestation verified",
		"   role        worker",
		"   permission  write",
		"   action      \"implement issue #101\"",
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
	record := validDispatchAttestation()
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
		Attestation: &record,
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
		"✅ attestation verified",
		"   role        worker",
		"   tokens      input=120  output=34  total=154",
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
	record := validDispatchAttestation()

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
				Attestation: &record,
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "attestation: verified") {
		t.Fatalf("stderr missing plain pretty attestation:\n%s", stderr.String())
	}
	for _, disallowed := range []string{"✅", "❌", "⚠", "\x1b["} {
		if strings.Contains(stderr.String(), disallowed) {
			t.Fatalf("stderr contains %q despite NO_COLOR:\n%s", disallowed, stderr.String())
		}
	}
}

func TestDispatchNoPrettySuppressesStderrWithoutChangingStdout(t *testing.T) {
	clearPrettyEnv(t)
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	record := validDispatchAttestation()
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
	record := validDispatchAttestation()
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
	record := validDispatchAttestation()
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
			record := validDispatchAttestation()
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
				Attestation: &record,
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
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
			record := validDispatchAttestation()
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
				Attestation: &record,
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
}

func TestDispatchDoesNotRenderSuccessJSONWithoutAttestation(t *testing.T) {
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
	if !strings.Contains(stderr.String(), "worker attestation is missing") {
		t.Fatalf("stderr missing attestation error: %q", stderr.String())
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
		"--model", "gpt-5",
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
	if dispatchOpts.RunID != "run-test-wave" || dispatchOpts.Model != "gpt-5" || dispatchOpts.Effort != "high" {
		t.Fatalf("dispatch opts run/model/effort = %#v", dispatchOpts)
	}
	text := stdout.String()
	for _, want := range []string{"DISPATCH WAVE", "RunId: run-test-wave", "- #201 succeeded", "Verify successful PRs"} {
		if !strings.Contains(text, want) {
			t.Fatalf("stdout missing %q:\n%s", want, text)
		}
	}
}

func TestDispatchWavePrettyDefaultNonInteractiveWritesPlainBlocksWithoutChangingStdout(t *testing.T) {
	clearPrettyEnv(t)
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()
	now := time.Date(2026, 6, 30, 1, 2, 3, 0, time.UTC)
	record201 := validDispatchAttestation()
	record201.Action = "implement issue #201"
	record202 := validDispatchAttestation()
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
				Attestation: &record201,
			},
			{
				Issue:       202,
				Status:      orchestration.DispatchWaveStatusSucceeded,
				Branch:      "loop/issue-202",
				PR:          "https://github.com/owner/repo/pull/202",
				AttemptPath: "/repo/.loopcoder/runs/run-test-wave/workers/job-202.attempt.json",
				Attestation: &record202,
			},
		},
	}
	wantStdout := orchestration.RenderDispatchWaveText(expectedReport)

	exitCode := RunWithDeps([]string{
		"dispatch-wave",
		"--repo", repo,
		"--issue-numbers", "201,202",
		"--run-id", "run-test-wave",
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
					Attestation: &record201,
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
					Attestation: &record202,
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
	gotStderr := stderr.String()
	if count := strings.Count(gotStderr, "attestation: verified"); count != 2 {
		t.Fatalf("stderr pretty block count = %d, want 2:\n%s", count, gotStderr)
	}
	for _, want := range []string{
		"  action      \"implement issue #201\"",
		"  action      \"implement issue #202\"",
	} {
		if !strings.Contains(gotStderr, want) {
			t.Fatalf("stderr missing %q:\n%s", want, gotStderr)
		}
	}
	for _, disallowed := range []string{"✅", "❌", "⚠"} {
		if strings.Contains(gotStderr, disallowed) {
			t.Fatalf("plain stderr contains %q:\n%s", disallowed, gotStderr)
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
		"-Model", "gpt-5",
		"-Effort", "high",
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
			if opts.Provider != "codex" || opts.Model != "gpt-5" || opts.Effort != "high" {
				t.Fatalf("recover opts provider/model/effort = %#v", opts)
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
	if result.Attestation == nil {
		t.Fatal("expected dispatch result attestation")
	}
	canonical, err := result.Attestation.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON returned error: %v", err)
	}
	data, err := worker.MarshalResult(result)
	if err != nil {
		t.Fatalf("MarshalResult returned error: %v", err)
	}
	return result.Attestation.Header() + "\n" + string(canonical) + "\n" + string(data) + "\n"
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

func validDispatchAttestation() attestation.AttestationRecord {
	return attestation.AttestationRecord{
		Role:        attestation.RoleWorker,
		Provider:    "codex",
		Model:       "gpt-5.5",
		ModelSource: attestation.ModelSourceParsed,
		Effort:      "high",
		Permission:  attestation.PermissionWrite,
		Action:      "implement issue #101",
		ExitCode:    0,
		StartedAt:   "2026-06-28T00:00:00Z",
		EndedAt:     "2026-06-28T00:00:42Z",
		DurationMS:  42000,
		Usage: attestation.Usage{
			InputTokens:  int64TestPtr(120),
			OutputTokens: int64TestPtr(34),
			TotalTokens:  int64TestPtr(154),
		},
		Verified: true,
	}
}

func validDispatchResult(record attestation.AttestationRecord) worker.Result {
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
		Attestation: &record,
	}
}

func validLoopreviewAttestation() attestation.AttestationRecord {
	record := validDispatchAttestation()
	record.Role = attestation.RoleVerifier
	record.Provider = "claude"
	record.Permission = attestation.PermissionReadOnly
	record.Action = "review PR #152"
	return record
}

func int64TestPtr(value int64) *int64 {
	return &value
}

type cliFakeReader struct {
	issues []gh.Issue
	views  map[int]gh.Issue
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
	return nil, nil
}

func (f cliFakeReader) PRChecks(context.Context, int) ([]gh.Check, error) {
	return nil, nil
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
