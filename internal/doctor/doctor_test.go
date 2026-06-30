package doctor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/config"
)

func TestRunReportsHealthyPreflight(t *testing.T) {
	env := healthyDoctorEnv()

	report := Run(context.Background(), Options{
		RepoPath: "/repo",
		BuildInfo: BuildInfo{
			Version: "0.3.1",
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
		".delivery.yml",
		"provider codex",
		"provider claude",
		"repository origin",
		"default branch",
		"loopcoder binary",
		"version compatibility",
		"loopcoder skill",
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

func healthyDoctorEnv() *fakeDoctorEnv {
	return &fakeDoctorEnv{
		paths: map[string]string{
			"git":    "/bin/git",
			"gh":     "/bin/gh",
			"codex":  "/bin/codex",
			"claude": "/bin/claude",
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
		},
		cfg: config.Config{
			Version: 1,
			Adapters: config.Adapters{
				Worker:   "codex",
				Verifier: "claude",
			},
		},
		file:           []byte("version: 1\nmin_loopcoder_version: 0.3.0\n"),
		executablePath: "/bin/loopcoder",
		userHome:       filepath.Join("home", "user"),
		skillFiles: map[string][]byte{
			doctorSkillPath(filepath.Join("home", "user"), "SKILL.md"):  []byte("skill content\n"),
			doctorSkillPath(filepath.Join("home", "user"), "AGENTS.md"): []byte("agents content\n"),
		},
	}
}

type fakeDoctorEnv struct {
	paths          map[string]string
	commands       map[string]CommandResult
	cfg            config.Config
	configErr      error
	file           []byte
	fileErr        error
	executablePath string
	userHome       string
	skillFiles     map[string][]byte
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
		if f.fileErr != nil {
			return nil, f.fileErr
		}
		return f.file, nil
	}
	return deps
}

func doctorSkillPath(home string, name string) string {
	return filepath.Clean(filepath.Join(home, ".claude", "skills", "loopcoder", name))
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
