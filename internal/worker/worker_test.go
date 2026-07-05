package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/agent"
	"github.com/jasonhnd/loopcoder/internal/attestation"
	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
)

func TestWorkerWatchdogDefaultsAreRaised(t *testing.T) {
	if WorkerHardCap != 45*time.Minute || WorkerStallTimeout != 5*time.Minute {
		t.Fatalf("worker watchdog defaults = hard cap %s stall %s, want 45m0s/5m0s", WorkerHardCap, WorkerStallTimeout)
	}
}

func TestBuildPromptWithAndWithoutRecoveryContext(t *testing.T) {
	base := BuildPrompt(PromptOptions{
		IssueNumber: 101,
		IssueTitle:  "Implement dispatch",
		IssueBody:   "Details here",
		Branch:      "loop/issue-101",
	})
	for _, want := range []string{
		"You are implementing GitHub issue #101.",
		"fresh git worktree on branch loop/issue-101",
		"# Title\nImplement dispatch",
		"# Details\nDetails here",
		"do NOT run git commit or git push",
		"final summary in English",
	} {
		if !strings.Contains(base, want) {
			t.Fatalf("prompt missing %q:\n%s", want, base)
		}
	}
	if strings.Contains(base, "Recovery context from a prior failed attempt") {
		t.Fatalf("prompt unexpectedly included recovery context:\n%s", base)
	}
	if strings.Contains(base, "Repo-local skills") {
		t.Fatalf("prompt unexpectedly included repo skills:\n%s", base)
	}

	withRepoSkills := BuildPrompt(PromptOptions{
		IssueNumber: 101,
		IssueTitle:  "Implement dispatch",
		Branch:      "loop/issue-101",
		RepoSkills:  "## Repo-local skills\nSummary: project conventions\n",
	})
	for _, want := range []string{
		"## Repo-local skills",
		"Summary: project conventions",
	} {
		if !strings.Contains(withRepoSkills, want) {
			t.Fatalf("repo-skill prompt missing %q:\n%s", want, withRepoSkills)
		}
	}

	withRecovery := BuildPrompt(PromptOptions{
		IssueNumber:     101,
		IssueTitle:      "Implement dispatch",
		Branch:          "loop/issue-101",
		RecoveryContext: "Previous failure details",
	})
	for _, want := range []string{
		"## Recovery context from a prior failed attempt",
		"Previous failure details",
	} {
		if !strings.Contains(withRecovery, want) {
			t.Fatalf("recovery prompt missing %q:\n%s", want, withRecovery)
		}
	}
}

func TestDispatchSuccessWritesStateAndReturnsParityJSONFields(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	var warnings strings.Builder
	fakeGit := &workerFakeGit{status: " M internal/worker/worker.go\n"}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result:    validWorkerAgentResult("Implemented dispatch.", 0),
		log:       "codex ok\n",
	}
	fakeGitHub := &workerFakeGitHub{prURL: "https://github.com/owner/repo/pull/101"}

	result, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 101,
		IssueTitle:  "Implement dispatch",
		IssueBody:   "Body",
		RunID:       "run-test",
		Provider:    "codex",
		Stderr:      &warnings,
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return fakeGitHub
		},
		AgentLookup: func(provider string) (agent.Runner, error) {
			if provider != "codex" {
				t.Fatalf("provider = %q, want codex", provider)
			}
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}

	if !result.OK || result.Issue != 101 || result.Branch != "loop/issue-101" || result.RunID != "run-test" {
		t.Fatalf("result has wrong identity fields: %#v", result)
	}
	if result.PR != "https://github.com/owner/repo/pull/101" {
		t.Fatalf("PR = %q", result.PR)
	}
	if result.Summary != "Implemented dispatch." || result.Status != "succeeded" || result.ExitCode != 0 || result.LogBytes == 0 {
		t.Fatalf("result has wrong status fields: %#v", result)
	}
	if result.AttemptPath != filepath.Join(repo, ".loopcoder", "runs", "run-test", "workers", "job-101-4321.attempt.json") {
		t.Fatalf("AttemptPath = %q", result.AttemptPath)
	}
	if result.Attestation == nil {
		t.Fatal("result missing attestation")
	}
	if err := result.Attestation.Validate(); err != nil {
		t.Fatalf("result attestation does not validate: %v", err)
	}
	canonicalAttestation, err := result.Attestation.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON returned error: %v", err)
	}
	if want := "Closes #101\n\nImplemented dispatch."; fakeGitHub.lastPRBody != want {
		t.Fatalf("PR body = %q, want %q", fakeGitHub.lastPRBody, want)
	}
	for _, forbidden := range []string{
		result.Attestation.Header(),
		string(canonicalAttestation),
		"[attestation]",
		"```json\n",
		`"role":"worker"`,
		`"provider":"codex"`,
		`"model_source":"parsed"`,
		`"permission":"write"`,
		`"action":"implement issue #101"`,
		`"usage":{"input_tokens":120,"output_tokens":34,"total_tokens":154}`,
	} {
		if strings.Contains(fakeGitHub.lastPRBody, forbidden) {
			t.Fatalf("PR body contains forbidden attestation text %q:\n%s", forbidden, fakeGitHub.lastPRBody)
		}
	}
	if fakeGit.lastCommitMessage != "Implement dispatch (closes #101)" {
		t.Fatalf("commit message = %q", fakeGit.lastCommitMessage)
	}

	data, err := MarshalResult(result)
	if err != nil {
		t.Fatalf("MarshalResult returned error: %v", err)
	}
	var jsonFields map[string]any
	if err := json.Unmarshal(data, &jsonFields); err != nil {
		t.Fatalf("result JSON invalid: %v", err)
	}
	for _, key := range []string{"ok", "issue", "branch", "run_id", "pr", "summary", "attempt_path", "status", "exit_code", "log_bytes"} {
		if _, ok := jsonFields[key]; !ok {
			t.Fatalf("success JSON missing field %q: %s", key, string(data))
		}
	}
	attestationField, ok := jsonFields["attestation"]
	if !ok {
		t.Fatalf("success JSON missing attestation field: %s", string(data))
	}
	attestationBytes, err := json.Marshal(attestationField)
	if err != nil {
		t.Fatalf("marshal attestation field: %v", err)
	}
	var renderedAttestation attestation.AttestationRecord
	if err := json.Unmarshal(attestationBytes, &renderedAttestation); err != nil {
		t.Fatalf("attestation field invalid: %v", err)
	}
	if err := renderedAttestation.Validate(); err != nil {
		t.Fatalf("attestation field does not validate: %v", err)
	}
	if renderedAttestation.Header() != result.Attestation.Header() {
		t.Fatalf("attestation JSON header = %q, want %q", renderedAttestation.Header(), result.Attestation.Header())
	}

	attempts, err := state.LoadAttempts(repo, "run-test")
	if err != nil {
		t.Fatalf("LoadAttempts returned error: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("LoadAttempts returned %d attempts, want 1", len(attempts))
	}
	if attempts[0].Phase != "cleanup" || attempts[0].Status != "succeeded" {
		t.Fatalf("final attempt = %#v", attempts[0])
	}
	if attempts[0].ExitCode == nil || *attempts[0].ExitCode != 0 {
		t.Fatalf("attempt exit code = %#v, want 0", attempts[0].ExitCode)
	}
	if attempts[0].Usage == nil || attempts[0].Usage.TotalTokens == nil || *attempts[0].Usage.TotalTokens != 154 {
		t.Fatalf("attempt usage = %#v, want total tokens 154", attempts[0].Usage)
	}
	if attempts[0].Attestation == nil {
		t.Fatal("attempt missing persisted attestation")
	}
	if err := attempts[0].Attestation.Validate(); err != nil {
		t.Fatalf("attempt attestation does not validate: %v", err)
	}
	if attempts[0].Attestation.Header() != result.Attestation.Header() {
		t.Fatalf("attempt attestation header = %q, want %q", attempts[0].Attestation.Header(), result.Attestation.Header())
	}
	eventCount, err := state.CountEvents(repo, "run-test")
	if err != nil {
		t.Fatalf("CountEvents returned error: %v", err)
	}
	if eventCount != 9 {
		t.Fatalf("event count = %d, want 9", eventCount)
	}
	if fakeAgent.invocation.WorktreePath == "" || fakeAgent.invocation.Prompt == "" || fakeAgent.invocation.LogPath == "" {
		t.Fatalf("agent invocation missing required fields: %#v", fakeAgent.invocation)
	}
	if fakeAgent.invocation.HardCap != WorkerHardCap || fakeAgent.invocation.StallTimeout != WorkerStallTimeout {
		t.Fatalf("agent supervision = hard cap %s stall %s, want %s/%s", fakeAgent.invocation.HardCap, fakeAgent.invocation.StallTimeout, WorkerHardCap, WorkerStallTimeout)
	}
	if strings.Contains(fakeAgent.invocation.Prompt, "Repo-local skills") {
		t.Fatalf("agent prompt unexpectedly included repo skills:\n%s", fakeAgent.invocation.Prompt)
	}
}

func TestDispatchInjectsRepoSkillsFromWorktree(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	var warnings strings.Builder
	fakeGit := &workerFakeGit{
		status: " M internal/worker/worker.go\n",
		worktreeSetup: func(worktreePath string) error {
			skillDir := filepath.Join(worktreePath, ".claude", "skills", "go-style")
			if err := os.MkdirAll(skillDir, 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(`---
name: go-style
description: Repository Go conventions
---

# Go Style

This implementation detail should stay out of the prompt.
`), 0o644)
		},
	}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result:    validWorkerAgentResult("Implemented dispatch.", 0),
		log:       "codex ok\n",
	}

	_, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 101,
		IssueTitle:  "Implement dispatch",
		IssueBody:   "Body",
		RunID:       "run-test",
		Provider:    "codex",
		Stderr:      &warnings,
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return &workerFakeGitHub{prURL: "https://github.com/owner/repo/pull/101"}
		},
		AgentLookup: func(provider string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	for _, want := range []string{
		"## Repo-local skills",
		"Path: `.claude/skills/go-style/SKILL.md`",
		"Summary: Repository Go conventions",
		"- # Go Style",
	} {
		if !strings.Contains(fakeAgent.invocation.Prompt, want) {
			t.Fatalf("agent prompt missing %q:\n%s", want, fakeAgent.invocation.Prompt)
		}
	}
	if strings.Contains(fakeAgent.invocation.Prompt, "This implementation detail should stay out of the prompt") {
		t.Fatalf("agent prompt included skill body text:\n%s", fakeAgent.invocation.Prompt)
	}
}

func TestDispatchInjectsDomainConfiguredSkillsFromWorktree(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte(`
domain:
  skills:
    paths:
      - governance/**/skill.md
    machine_library:
      paths:
        - .loopcoder/skill-library/**/*.md
    select:
      - governance
      - disclosure
    prompt_budget_bytes: 2048
`), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}
	scratchRoot := t.TempDir()
	fakeGit := &workerFakeGit{
		status: " M internal/worker/worker.go\n",
		worktreeSetup: func(worktreePath string) error {
			if err := os.MkdirAll(filepath.Join(worktreePath, ".claude", "skills", "go-style"), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(worktreePath, ".claude", "skills", "go-style", "SKILL.md"), []byte(`---
name: go-style
description: Repository Go conventions
---

# Go Style
`), 0o644); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Join(worktreePath, "governance", "review"), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(worktreePath, "governance", "review", "skill.md"), []byte(`---
name: Governance Review
description: Governance review conventions
tags: [governance]
---

# Governance Review

Full governance body should stay out of the prompt.
`), 0o644); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Join(worktreePath, ".loopcoder", "skill-library"), 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(worktreePath, ".loopcoder", "skill-library", "disclosure.md"), []byte(`---
description: Disclosure library metadata
---

# Disclosure Library
`), 0o644)
		},
	}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result:    validWorkerAgentResult("Implemented domain skills.", 0),
		log:       "codex ok\n",
	}

	_, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 463,
		IssueTitle:  "Configurable skill discovery",
		IssueBody:   "Body",
		RunID:       "run-test",
		Provider:    "codex",
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return &workerFakeGitHub{prURL: "https://github.com/owner/repo/pull/463"}
		},
		AgentLookup: func(provider string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	for _, want := range []string{
		"Discovery paths:",
		"- skills: `governance/**/skill.md`",
		"- machine_library: `.loopcoder/skill-library/**/*.md`",
		"Selection filter: governance, disclosure.",
		"Budget: 2048 bytes.",
		"Path: `governance/review/skill.md`",
		"Summary: Governance review conventions",
		"Tags: `governance`",
		"Path: `.loopcoder/skill-library/disclosure.md`",
		"Summary: Disclosure library metadata",
	} {
		if !strings.Contains(fakeAgent.invocation.Prompt, want) {
			t.Fatalf("agent prompt missing %q:\n%s", want, fakeAgent.invocation.Prompt)
		}
	}
	for _, notWant := range []string{
		"Path: `.claude/skills/go-style/SKILL.md`",
		"Full governance body should stay out of the prompt",
	} {
		if strings.Contains(fakeAgent.invocation.Prompt, notWant) {
			t.Fatalf("agent prompt contained %q:\n%s", notWant, fakeAgent.invocation.Prompt)
		}
	}
}

func TestDispatchPassesConfiguredWorkerMCPServers(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte(`
mcp:
  servers:
    - name: worker-index
      transport: stdio
      command: ./tools/worker-index
      args: ["--root", "."]
      roles: [worker]
      read_only: false
    - name: shared-read
      transport: http
      url: https://mcp.example.com/shared
      auth:
        header: Authorization
        env: SHARED_MCP_TOKEN
      roles: [worker, verifier]
      read_only: true
    - name: verifier-only
      transport: stdio
      command: ./tools/verifier-only
      roles: [verifier]
      read_only: true
`), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}
	scratchRoot := t.TempDir()
	fakeGit := &workerFakeGit{status: " M file.go\n"}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result:    validWorkerAgentResult("Implemented MCP.", 0),
		log:       "codex ok\n",
	}

	_, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 465,
		IssueTitle:  "MCP invocation contract",
		IssueBody:   "Body",
		RunID:       "run-test",
		Provider:    "codex",
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return &workerFakeGitHub{prURL: "https://github.com/owner/repo/pull/465"}
		},
		AgentLookup: func(string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}

	servers := fakeAgent.invocation.MCPServers
	if len(servers) != 2 {
		t.Fatalf("MCPServers = %#v, want worker-index and shared-read", servers)
	}
	if servers[0].Name != "worker-index" || servers[0].Command != "./tools/worker-index" || servers[0].ReadOnly {
		t.Fatalf("first MCP server = %#v, want writable worker-index", servers[0])
	}
	if len(servers[0].Args) != 2 || servers[0].Args[0] != "--root" || servers[0].Args[1] != "." {
		t.Fatalf("worker-index args = %#v, want --root .", servers[0].Args)
	}
	if servers[1].Name != "shared-read" || servers[1].URL != "https://mcp.example.com/shared" || !servers[1].ReadOnly {
		t.Fatalf("second MCP server = %#v, want read-only shared-read", servers[1])
	}
	if servers[1].Auth.Header != "Authorization" || servers[1].Auth.Env != "SHARED_MCP_TOKEN" {
		t.Fatalf("shared-read auth = %#v, want env-backed Authorization", servers[1].Auth)
	}
}

func TestDispatchPassesDomainLivenessLogOnlyToAgent(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte(`
domain:
  liveness:
    mode: log-only
`), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}
	scratchRoot := t.TempDir()
	fakeGit := &workerFakeGit{status: " M file.go\n"}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result:    validWorkerAgentResult("Implemented liveness.", 0),
		log:       "codex ok\n",
	}

	_, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 469,
		IssueTitle:  "Domain liveness",
		IssueBody:   "Body",
		RunID:       "run-test",
		Provider:    "codex",
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return &workerFakeGitHub{prURL: "https://github.com/owner/repo/pull/469"}
		},
		AgentLookup: func(string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if fakeAgent.invocation.LivenessMode != "log-only" {
		t.Fatalf("LivenessMode = %q, want log-only", fakeAgent.invocation.LivenessMode)
	}
	if fakeAgent.invocation.LivenessCommand != "" {
		t.Fatalf("LivenessCommand = %q, want empty", fakeAgent.invocation.LivenessCommand)
	}
}

func TestDispatchPassesDomainCustomLivenessCommandToAgent(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte(`
domain:
  liveness:
    mode: custom
    command: echo alive
`), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}
	scratchRoot := t.TempDir()
	fakeGit := &workerFakeGit{status: " M file.go\n"}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result:    validWorkerAgentResult("Implemented custom liveness.", 0),
		log:       "codex ok\n",
	}

	_, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 469,
		IssueTitle:  "Domain liveness",
		IssueBody:   "Body",
		RunID:       "run-test",
		Provider:    "codex",
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return &workerFakeGitHub{prURL: "https://github.com/owner/repo/pull/469"}
		},
		AgentLookup: func(string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if fakeAgent.invocation.LivenessMode != "custom" {
		t.Fatalf("LivenessMode = %q, want custom", fakeAgent.invocation.LivenessMode)
	}
	if fakeAgent.invocation.LivenessCommand != "echo alive" {
		t.Fatalf("LivenessCommand = %q, want echo alive", fakeAgent.invocation.LivenessCommand)
	}
}

func TestDispatchPassesDomainCustomLivenessArgvToAgent(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte(`
domain:
  liveness:
    mode: custom
    argv: ["./tools/liveness", "--literal", "alive && exit 9"]
`), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}
	scratchRoot := t.TempDir()
	fakeGit := &workerFakeGit{status: " M file.go\n"}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result:    validWorkerAgentResult("Implemented custom liveness argv.", 0),
		log:       "codex ok\n",
	}

	_, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 490,
		IssueTitle:  "Domain liveness argv",
		IssueBody:   "Body",
		RunID:       "run-test",
		Provider:    "codex",
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return &workerFakeGitHub{prURL: "https://github.com/owner/repo/pull/490"}
		},
		AgentLookup: func(string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if fakeAgent.invocation.LivenessMode != "custom" {
		t.Fatalf("LivenessMode = %q, want custom", fakeAgent.invocation.LivenessMode)
	}
	wantCommand := supervisedexec.EncodeLivenessArgv([]string{"./tools/liveness", "--literal", "alive && exit 9"})
	if fakeAgent.invocation.LivenessCommand != wantCommand {
		t.Fatalf("LivenessCommand = %q, want encoded argv command %q", fakeAgent.invocation.LivenessCommand, wantCommand)
	}
}

func TestGitHubBoundTextBuildersHaveZeroAttestationFootprint(t *testing.T) {
	record := buildWorkerAttestation(Options{
		IssueNumber: 101,
		IssueTitle:  "Implement dispatch",
		Provider:    "codex",
	}, validWorkerAgentResult("Implemented dispatch.", 0))
	if err := record.Validate(); err != nil {
		t.Fatalf("test attestation does not validate: %v", err)
	}

	surfaces := map[string]string{
		"pr body":        buildPRBody(101, "Implemented dispatch."),
		"commit message": buildCommitMessage("Implement dispatch", 101),
	}
	for name, text := range surfaces {
		assertNoAttestationFootprint(t, name, text, record)
	}
	if surfaces["pr body"] != "Closes #101\n\nImplemented dispatch." {
		t.Fatalf("PR body surface = %q", surfaces["pr body"])
	}
	if surfaces["commit message"] != "Implement dispatch (closes #101)" {
		t.Fatalf("commit message surface = %q", surfaces["commit message"])
	}
}

func TestDispatchFailureWritesRecoveryBriefAndPreservesArtifacts(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	var warnings strings.Builder
	fakeGit := &workerFakeGit{status: " M file.go\n"}
	fakeAgent := &workerFakeAgent{
		exitCode: 7,
		log:      "Authorization: Bearer abc.def\npassword=hunter2\nlast line\n",
	}
	fakeGitHub := &workerFakeGitHub{
		prs: []gh.PullRequestReference{{Number: 11, URL: "https://github.com/owner/repo/pull/11"}},
	}

	_, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 101,
		IssueTitle:  "Implement dispatch",
		RunID:       "run-test",
		Provider:    "codex",
		Stderr:      &warnings,
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return fakeGitHub
		},
		AgentLookup: func(provider string) (agent.Runner, error) {
			if provider != "codex" {
				t.Fatalf("provider = %q, want codex", provider)
			}
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err == nil {
		t.Fatal("Dispatch returned nil error, want failure")
	}
	if !strings.Contains(err.Error(), "codex exec failed (exit 7)") {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(warnings.String(), "preserved failed attempt artifacts") {
		t.Fatalf("warnings missing artifact preservation note: %q", warnings.String())
	}

	briefPath := state.RecoveryBriefPath(repo, "run-test", "job-101-4321")
	brief, err := os.ReadFile(briefPath)
	if err != nil {
		t.Fatalf("ReadFile recovery brief: %v", err)
	}
	briefText := string(brief)
	for _, want := range []string{
		"# Recovery context for issue #101",
		"- Last phase: codex_exited",
		" M file.go",
		"#11 https://github.com/owner/repo/pull/11",
		"Bearer [REDACTED_TOKEN]",
		"password=[REDACTED_SECRET]",
	} {
		if !strings.Contains(briefText, want) {
			t.Fatalf("recovery brief missing %q:\n%s", want, briefText)
		}
	}
	for _, leaked := range []string{"abc.def", "hunter2"} {
		if strings.Contains(briefText, leaked) {
			t.Fatalf("recovery brief leaked %q:\n%s", leaked, briefText)
		}
	}

	attempts, err := state.LoadAttempts(repo, "run-test")
	if err != nil {
		t.Fatalf("LoadAttempts returned error: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("LoadAttempts returned %d attempts, want 1", len(attempts))
	}
	got := attempts[0]
	if got.Phase != "codex_exited" || got.Status != "failed" {
		t.Fatalf("failed attempt = %#v", got)
	}
	if got.ExitCode == nil || *got.ExitCode != 7 {
		t.Fatalf("failed attempt exit code = %#v, want 7", got.ExitCode)
	}
}

func TestDispatchHungWithEmptyWorktreeWritesHungStateAndNoAttestation(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	var warnings strings.Builder
	fakeGit := &workerFakeGit{}
	fakeGitHub := &workerFakeGitHub{}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result: agent.Result{
			ExitCode:   -1,
			Hung:       true,
			HungReason: agent.HungReasonStall,
		},
		log: "provider stopped producing output\n",
	}

	result, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 101,
		IssueTitle:  "Implement dispatch",
		RunID:       "run-test",
		Provider:    "codex",
		Stderr:      &warnings,
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return fakeGitHub
		},
		AgentLookup: func(provider string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err == nil {
		t.Fatal("Dispatch returned nil error, want hung failure")
	}
	if result.Status != "hung" || result.OK || result.Attestation != nil {
		t.Fatalf("hung result = %#v, want status hung, not ok, no attestation", result)
	}
	if fakeGit.addAllCalls != 0 || fakeGit.commitCalls != 0 || fakeGit.pushCalls != 0 || fakeGit.forcePushCalls != 0 {
		t.Fatalf("hung empty worktree made harvest git calls: add=%d commit=%d push=%d forcePush=%d", fakeGit.addAllCalls, fakeGit.commitCalls, fakeGit.pushCalls, fakeGit.forcePushCalls)
	}
	if fakeGitHub.createPRCalls != 0 {
		t.Fatalf("CreatePR calls = %d, want 0", fakeGitHub.createPRCalls)
	}
	for _, want := range []string{"exec hung", "reason=hung", "hung_reason=stall"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("hung error missing %q: %v", want, err)
		}
	}

	attempts, err := state.LoadAttempts(repo, "run-test")
	if err != nil {
		t.Fatalf("LoadAttempts returned error: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("LoadAttempts returned %d attempts, want 1", len(attempts))
	}
	got := attempts[0]
	if got.Status != "hung" || got.Attestation != nil {
		t.Fatalf("hung attempt = %#v, want status hung and no attestation", got)
	}
	if !strings.Contains(got.Error, "reason=hung") {
		t.Fatalf("hung attempt error = %q, want reason=hung", got.Error)
	}

	brief, err := os.ReadFile(state.RecoveryBriefPath(repo, "run-test", "job-101-4321"))
	if err != nil {
		t.Fatalf("ReadFile recovery brief: %v", err)
	}
	if !strings.Contains(string(brief), "- Status: hung") || !strings.Contains(string(brief), "reason=hung") {
		t.Fatalf("hung recovery brief missing status/reason:\n%s", string(brief))
	}

	events, err := os.ReadFile(state.EventsPath(repo, "run-test"))
	if err != nil {
		t.Fatalf("ReadFile events: %v", err)
	}
	for _, want := range []string{`"status":"hung"`, `"event":"worker_hung"`, `"outcome":"hung"`, `"reason":"hung"`} {
		if !strings.Contains(string(events), want) {
			t.Fatalf("events missing %q:\n%s", want, string(events))
		}
	}
}

func TestDispatchHungWithDirtyWorktreeHarvestsNeedsHumanPR(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	var warnings strings.Builder
	fakeGit := &workerFakeGit{status: " M file.go\n?? new.go\n"}
	totalTokens := int64(77)
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result: agent.Result{
			ExitCode:   -1,
			Hung:       true,
			HungReason: agent.HungReasonStall,
			Model:      "gpt-worker",
			Effort:     "high",
			Usage: attestation.Usage{
				TotalTokens: &totalTokens,
			},
			StartedAt:  "2026-06-28T00:00:00Z",
			EndedAt:    "2026-06-28T00:02:00Z",
			DurationMS: 120000,
		},
		log: "provider stopped producing output\nlast useful log line\n",
	}
	fakeGitHub := &workerFakeGitHub{prURL: "https://github.com/owner/repo/pull/101"}

	result, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 101,
		IssueTitle:  "Implement dispatch",
		RunID:       "run-test",
		Attempt:     2,
		Branch:      "loop/issue-101-retry-2",
		Provider:    "codex",
		Model:       "gpt-conductor",
		Effort:      "xhigh",
		Stderr:      &warnings,
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return fakeGitHub
		},
		AgentLookup: func(provider string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if !result.OK || result.Status != "needs-human" || result.Branch != "loop/issue-101-retry-2" || result.PR != "https://github.com/owner/repo/pull/101" {
		t.Fatalf("harvest result = %#v", result)
	}
	if result.Attestation == nil {
		t.Fatal("harvest result missing attestation")
	}
	if result.Attestation.Role != attestation.RoleConductor || result.Attestation.Permission != attestation.PermissionOrchestrate || result.Attestation.Verified {
		t.Fatalf("harvest attestation = %#v, want unverified conductor orchestrate", result.Attestation)
	}
	if err := result.Attestation.Validate(); err != nil {
		t.Fatalf("harvest attestation does not validate: %v", err)
	}
	if fakeGit.addAllCalls != 1 || fakeGit.commitCalls != 1 || fakeGit.pushCalls != 0 || fakeGit.forcePushCalls != 1 {
		t.Fatalf("harvest git calls add=%d commit=%d push=%d forcePush=%d, want add/commit/force only", fakeGit.addAllCalls, fakeGit.commitCalls, fakeGit.pushCalls, fakeGit.forcePushCalls)
	}
	if fakeGit.lastForcePushBranch != "loop/issue-101-retry-2" {
		t.Fatalf("force push branch = %q", fakeGit.lastForcePushBranch)
	}
	if strings.Contains(strings.ToLower(fakeGit.lastCommitMessage), "closes") || !strings.Contains(fakeGit.lastCommitMessage, "Harvest issue #101 from hung worker") {
		t.Fatalf("harvest commit message = %q", fakeGit.lastCommitMessage)
	}
	if fakeGitHub.createPRCalls != 1 || fakeGitHub.lastPRHead != "loop/issue-101-retry-2" || fakeGitHub.lastPRBase != "main" {
		t.Fatalf("CreatePR calls/head/base = %d/%q/%q", fakeGitHub.createPRCalls, fakeGitHub.lastPRHead, fakeGitHub.lastPRBase)
	}
	for _, want := range []string{
		"Refs #101",
		"needs-human",
		"harvested from a hung/killed worker",
		"possibly incomplete",
		"## Recovery brief",
		" M file.go",
		"?? new.go",
		"last useful log line",
		"## Hung worker partial attestation",
		"role=worker",
		"provider=codex",
		"model=gpt-worker",
		"total_tokens=77",
	} {
		if !strings.Contains(fakeGitHub.lastPRBody, want) {
			t.Fatalf("harvest PR body missing %q:\n%s", want, fakeGitHub.lastPRBody)
		}
	}
	for _, forbidden := range []string{"Part of #101", "Closes #101", "closes #101", `"role":"worker"`} {
		if strings.Contains(fakeGitHub.lastPRBody, forbidden) {
			t.Fatalf("harvest PR body contains forbidden %q:\n%s", forbidden, fakeGitHub.lastPRBody)
		}
	}

	attempts, err := state.LoadAttempts(repo, "run-test")
	if err != nil {
		t.Fatalf("LoadAttempts returned error: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("LoadAttempts returned %d attempts, want 1", len(attempts))
	}
	got := attempts[0]
	if got.Status != "needs-human" || got.Branch != "loop/issue-101-retry-2" {
		t.Fatalf("harvest attempt = %#v, want needs-human retry branch", got)
	}
	if got.Attestation == nil || got.Attestation.Role != attestation.RoleConductor {
		t.Fatalf("harvest attempt attestation = %#v, want conductor", got.Attestation)
	}
}

func TestDispatchHungReportOnlyPreservesPartialWorkWithoutHarvestPR(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte(`
domain:
  partial_work:
    mode: report-only
`), 0o644); err != nil {
		t.Fatalf("write delivery config: %v", err)
	}
	scratchRoot := t.TempDir()
	var warnings strings.Builder
	fakeGit := &workerFakeGit{status: " M file.go\n?? new.go\n"}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result: agent.Result{
			ExitCode:   -1,
			Hung:       true,
			HungReason: agent.HungReasonStall,
		},
		log: "provider stopped producing output\nlast useful log line\n",
	}
	fakeGitHub := &workerFakeGitHub{prURL: "https://github.com/owner/repo/pull/101"}

	result, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 101,
		IssueTitle:  "Implement dispatch",
		RunID:       "run-test",
		Attempt:     2,
		Provider:    "codex",
		Stderr:      &warnings,
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return fakeGitHub
		},
		AgentLookup: func(provider string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err == nil {
		t.Fatal("Dispatch returned nil error, want hung failure")
	}
	if result.OK || result.Status != "hung" || result.PR != "" || result.Attestation != nil {
		t.Fatalf("report-only hung result = %#v, want hung without PR or attestation", result)
	}
	if fakeGit.addAllCalls != 0 || fakeGit.commitCalls != 0 || fakeGit.pushCalls != 0 || fakeGit.forcePushCalls != 0 {
		t.Fatalf("report-only made harvest git calls: add=%d commit=%d push=%d force=%d", fakeGit.addAllCalls, fakeGit.commitCalls, fakeGit.pushCalls, fakeGit.forcePushCalls)
	}
	if fakeGitHub.createPRCalls != 0 {
		t.Fatalf("CreatePR calls = %d, want 0", fakeGitHub.createPRCalls)
	}
	for _, want := range []string{"report-only", "preserved partial work", "no harvest PR opened"} {
		if !strings.Contains(warnings.String(), want) {
			t.Fatalf("warnings missing %q:\n%s", want, warnings.String())
		}
	}

	attempts, err := state.LoadAttempts(repo, "run-test")
	if err != nil {
		t.Fatalf("LoadAttempts returned error: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Status != "hung" || attempts[0].Attestation != nil {
		t.Fatalf("report-only attempt = %#v, want hung with no attestation", attempts)
	}
	brief, err := os.ReadFile(state.RecoveryBriefPath(repo, "run-test", "job-101-4321"))
	if err != nil {
		t.Fatalf("ReadFile recovery brief: %v", err)
	}
	for _, want := range []string{" M file.go", "?? new.go", "last useful log line"} {
		if !strings.Contains(string(brief), want) {
			t.Fatalf("recovery brief missing %q:\n%s", want, string(brief))
		}
	}
	events, err := os.ReadFile(state.EventsPath(repo, "run-test"))
	if err != nil {
		t.Fatalf("ReadFile events: %v", err)
	}
	for _, want := range []string{`"event":"worker_partial_work_reported"`, `"mode":"report-only"`} {
		if !strings.Contains(string(events), want) {
			t.Fatalf("events missing %q:\n%s", want, string(events))
		}
	}
}

func TestDispatchHungHarvestWarnsAndContinuesWhenDedupCheckFails(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	var warnings strings.Builder
	fakeGit := &workerFakeGit{status: " M file.go\n"}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result: agent.Result{
			ExitCode:   -1,
			Hung:       true,
			HungReason: agent.HungReasonStall,
		},
		log: "hung\n",
	}
	fakeGitHub := &workerFakeGitHub{
		prURL:       "https://github.com/owner/repo/pull/101",
		listHeadErr: errors.New("head PR lookup unavailable"),
		listOpenErr: errors.New("github API temporarily unavailable"),
	}

	result, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 101,
		IssueTitle:  "Implement dispatch",
		RunID:       "run-test",
		Attempt:     2,
		Provider:    "codex",
		Stderr:      &warnings,
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return fakeGitHub
		},
		AgentLookup: func(provider string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if !result.OK || result.Status != "needs-human" || result.PR != "https://github.com/owner/repo/pull/101" {
		t.Fatalf("harvest result = %#v", result)
	}
	if fakeGit.addAllCalls != 1 || fakeGit.commitCalls != 1 || fakeGit.forcePushCalls != 1 || fakeGitHub.createPRCalls != 1 {
		t.Fatalf("harvest did not continue after dedup warning: add=%d commit=%d force=%d createPR=%d", fakeGit.addAllCalls, fakeGit.commitCalls, fakeGit.forcePushCalls, fakeGitHub.createPRCalls)
	}
	for _, want := range []string{"warning", "harvest idempotency check", "duplicate needs-human PR may result"} {
		if !strings.Contains(warnings.String(), want) {
			t.Fatalf("warnings missing %q:\n%s", want, warnings.String())
		}
	}
}

func TestDispatchHungHarvestUsesForceWithLeaseForPreExistingRemoteBranch(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	fakeGit := &workerFakeGit{status: " M file.go\n", remoteBranchExists: true}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result: agent.Result{
			ExitCode:   -1,
			Hung:       true,
			HungReason: agent.HungReasonStall,
		},
		log: "hung\n",
	}

	_, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 101,
		IssueTitle:  "Implement dispatch",
		RunID:       "run-test",
		Attempt:     2,
		Provider:    "codex",
		Stderr:      io.Discard,
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return &workerFakeGitHub{prURL: "https://github.com/owner/repo/pull/101"}
		},
		AgentLookup: func(provider string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if fakeGit.forcePushCalls != 1 || !fakeGit.remoteBranchExists {
		t.Fatalf("force-with-lease path not exercised: calls=%d remoteExists=%v", fakeGit.forcePushCalls, fakeGit.remoteBranchExists)
	}
}

func TestDispatchHungHarvestNoOpsWhenHarvestPRExistsForDifferentAttempt(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	fakeGit := &workerFakeGit{status: " M file.go\n"}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result: agent.Result{
			ExitCode:   -1,
			Hung:       true,
			HungReason: agent.HungReasonStall,
		},
		log: "hung\n",
	}
	fakeGitHub := &workerFakeGitHub{
		openPRs: []gh.PullRequest{{
			Number:      77,
			Title:       "[needs-human] Harvested issue #101 from hung/killed worker",
			URL:         "https://github.com/owner/repo/pull/77",
			HeadRefName: "loop/issue-101-retry-1",
		}},
	}

	result, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 101,
		IssueTitle:  "Implement dispatch",
		RunID:       "run-test",
		Attempt:     3,
		Provider:    "codex",
		Stderr:      io.Discard,
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return fakeGitHub
		},
		AgentLookup: func(provider string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if result.Status != "needs-human" || result.PR != "https://github.com/owner/repo/pull/77" || result.Branch != "loop/issue-101-retry-1" {
		t.Fatalf("idempotent harvest result = %#v", result)
	}
	if fakeGit.addAllCalls != 0 || fakeGit.commitCalls != 0 || fakeGit.forcePushCalls != 0 || fakeGitHub.createPRCalls != 0 {
		t.Fatalf("idempotent harvest made calls add=%d commit=%d force=%d createPR=%d", fakeGit.addAllCalls, fakeGit.commitCalls, fakeGit.forcePushCalls, fakeGitHub.createPRCalls)
	}

	attempts, err := state.LoadAttempts(repo, "run-test")
	if err != nil {
		t.Fatalf("LoadAttempts returned error: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Status != "needs-human" || attempts[0].Branch != "loop/issue-101-retry-1" {
		t.Fatalf("idempotent harvest attempt = %#v", attempts)
	}
}

func TestDispatchHardFailsInvalidAttestationBeforeDelivery(t *testing.T) {
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	var warnings strings.Builder
	fakeGit := &workerFakeGit{status: " M internal/worker/worker.go\n"}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result: agent.Result{
			ExitCode:   0,
			Summary:    "Implemented dispatch.",
			Effort:     "xhigh",
			StartedAt:  "2026-06-28T00:00:00Z",
			EndedAt:    "2026-06-28T00:00:42Z",
			DurationMS: 42000,
		},
		log: "codex ok without metadata\n",
	}
	fakeGitHub := &workerFakeGitHub{prURL: "https://github.com/owner/repo/pull/101"}

	_, err := Dispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 101,
		IssueTitle:  "Implement dispatch",
		IssueBody:   "Body",
		RunID:       "run-test",
		Provider:    "codex",
		Stderr:      &warnings,
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return fakeGitHub
		},
		AgentLookup: func(provider string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 4321
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err == nil {
		t.Fatal("Dispatch returned nil error, want attestation failure")
	}
	for _, want := range []string{"validate worker attestation", "model is required", "usage requires total_tokens or both input_tokens and output_tokens"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q: %v", want, err)
		}
	}
	if fakeGit.addAllCalls != 0 || fakeGit.commitCalls != 0 || fakeGit.pushCalls != 0 {
		t.Fatalf("delivery git calls after attestation failure: add=%d commit=%d push=%d", fakeGit.addAllCalls, fakeGit.commitCalls, fakeGit.pushCalls)
	}
	if fakeGitHub.createPRCalls != 0 {
		t.Fatalf("CreatePR calls = %d, want 0", fakeGitHub.createPRCalls)
	}
	if !strings.Contains(warnings.String(), "preserved failed attempt artifacts") {
		t.Fatalf("warnings missing artifact preservation note: %q", warnings.String())
	}

	attempts, err := state.LoadAttempts(repo, "run-test")
	if err != nil {
		t.Fatalf("LoadAttempts returned error: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("LoadAttempts returned %d attempts, want 1", len(attempts))
	}
	got := attempts[0]
	if got.Phase != "codex_exited" || got.Status != "failed" {
		t.Fatalf("failed attempt = %#v", got)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("failed attempt exit code = %#v, want 0", got.ExitCode)
	}
}

func TestPrepareDispatchDefaultsAndResolvesDependencies(t *testing.T) {
	repo := t.TempDir()
	fakeAgent := &workerFakeAgent{}
	fakeGitHub := &workerFakeGitHub{}
	var gotProvider string
	var gotRepoPath string

	dispatch, err := prepareDispatch(context.Background(), Options{
		RepoPath:    repo,
		IssueNumber: 509,
		IssueTitle:  "Split worker dispatch",
	}, Deps{
		AgentLookup: func(provider string) (agent.Runner, error) {
			gotProvider = provider
			return fakeAgent, nil
		},
		GitHub: func(repoPath string) GitHubClient {
			gotRepoPath = repoPath
			return fakeGitHub
		},
		Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("prepareDispatch returned error: %v", err)
	}
	if gotProvider != "codex" {
		t.Fatalf("AgentLookup provider = %q, want codex", gotProvider)
	}
	if dispatch.opts.Provider != "codex" || dispatch.opts.BaseBranch != "main" || dispatch.opts.Branch != "loop/issue-509" || dispatch.opts.Attempt != 1 {
		t.Fatalf("normalized options = %#v", dispatch.opts)
	}
	if dispatch.opts.RunID != state.RunIDForIssue(509, fixedNow()) {
		t.Fatalf("RunID = %q, want generated run ID", dispatch.opts.RunID)
	}
	if !filepath.IsAbs(dispatch.repoPath) || gotRepoPath != dispatch.repoPath {
		t.Fatalf("repo path = %q, GitHub saw %q", dispatch.repoPath, gotRepoPath)
	}
	if dispatch.agentRun != fakeAgent || dispatch.github != fakeGitHub {
		t.Fatalf("resolved deps agent=%#v github=%#v", dispatch.agentRun, dispatch.github)
	}
	if dispatch.warnings == nil {
		t.Fatal("warnings writer was nil")
	}
}

func TestDispatchHelperSeamsSuccessPath(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	var removedScratch string
	fakeGit := &workerFakeGit{status: " M file.go\n"}
	fakeGitHub := &workerFakeGitHub{prURL: "https://github.com/owner/repo/pull/509"}
	fakeAgent := &workerFakeAgent{
		resultSet: true,
		result:    validWorkerAgentResult("Split dispatch into helpers.", 0),
		log:       "codex ok\n",
	}

	dispatch, err := prepareDispatch(ctx, Options{
		RepoPath:    repo,
		IssueNumber: 509,
		IssueTitle:  "Split worker dispatch",
		IssueBody:   "Body",
		RunID:       "run-seam",
		Provider:    "codex",
		Model:       "gpt-worker",
		Effort:      "high",
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return fakeGitHub
		},
		AgentLookup: func(string) (agent.Runner, error) {
			return fakeAgent, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 2468
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: func(path string) error {
			removedScratch = path
			return os.RemoveAll(path)
		},
		RepoSkills: func(repoPath string, domainSkills config.DomainSkills) (string, error) {
			if repoPath == "" {
				t.Fatal("RepoSkills repoPath was empty")
			}
			return "## Repo-local skills\nSummary: seam coverage\n", nil
		},
	})
	if err != nil {
		t.Fatalf("prepareDispatch returned error: %v", err)
	}

	if err := prepareWorktree(ctx, dispatch); err != nil {
		t.Fatalf("prepareWorktree returned error: %v", err)
	}
	if fakeGit.fetchCalls != 1 || fakeGit.fetchBase != "main" {
		t.Fatalf("fetch calls/base = %d/%q, want 1/main", fakeGit.fetchCalls, fakeGit.fetchBase)
	}
	if fakeGit.worktreeAddCalls != 1 || fakeGit.worktreeBranch != "loop/issue-509" || fakeGit.worktreeBase != "main" {
		t.Fatalf("worktree add = calls %d branch %q base %q", fakeGit.worktreeAddCalls, fakeGit.worktreeBranch, fakeGit.worktreeBase)
	}
	if dispatch.jobID != "job-509-2468" || dispatch.attemptPath != filepath.Join(repo, ".loopcoder", "runs", "run-seam", "workers", "job-509-2468.attempt.json") {
		t.Fatalf("attempt identity job=%q path=%q", dispatch.jobID, dispatch.attemptPath)
	}
	attempts, err := state.LoadAttempts(repo, "run-seam")
	if err != nil {
		t.Fatalf("LoadAttempts after prepareWorktree: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Phase != "worktree_created" || attempts[0].Status != "running" {
		t.Fatalf("prepareWorktree attempt = %#v", attempts)
	}

	invocation, err := buildInvocation(ctx, dispatch)
	if err != nil {
		t.Fatalf("buildInvocation returned error: %v", err)
	}
	if invocation.WorktreePath != dispatch.worktreePath || invocation.LogPath != dispatch.logPath || invocation.RunID != "run-seam" || invocation.Role != "worker" {
		t.Fatalf("invocation identity = %#v", invocation)
	}
	if invocation.Model != "gpt-worker" || invocation.Effort != "high" || invocation.HardCap != WorkerHardCap || invocation.StallTimeout != WorkerStallTimeout {
		t.Fatalf("invocation supervision/model = %#v", invocation)
	}
	if !strings.Contains(invocation.Prompt, "## Repo-local skills") || !strings.Contains(invocation.Prompt, "Summary: seam coverage") {
		t.Fatalf("invocation prompt missing repo skills:\n%s", invocation.Prompt)
	}
	promptInfo, err := os.Stat(dispatch.promptPath)
	if err != nil {
		t.Fatalf("stat prompt: %v", err)
	}
	if runtime.GOOS != "windows" && promptInfo.Mode().Perm() != 0o600 {
		t.Fatalf("prompt mode = %#o, want 0600", promptInfo.Mode().Perm())
	}

	agentResult, agentErr := runAgent(ctx, dispatch, invocation)
	if agentErr != nil {
		t.Fatalf("runAgent returned error: %v", agentErr)
	}
	if agentResult.ExitCode != 0 || dispatch.activePhase != "codex_exited" || dispatch.tracker.exitCode == nil || *dispatch.tracker.exitCode != 0 {
		t.Fatalf("runAgent state result=%#v phase=%q exit=%#v", agentResult, dispatch.activePhase, dispatch.tracker.exitCode)
	}

	result, err := commitAndOpenPR(ctx, dispatch, agentResult)
	if err != nil {
		t.Fatalf("commitAndOpenPR returned error: %v", err)
	}
	if !result.OK || result.Status != "succeeded" || result.PR != "https://github.com/owner/repo/pull/509" || result.Summary != "Split dispatch into helpers." {
		t.Fatalf("commit result = %#v", result)
	}
	if fakeGit.addAllCalls != 1 || fakeGit.commitCalls != 1 || fakeGit.pushCalls != 1 || fakeGitHub.createPRCalls != 1 {
		t.Fatalf("delivery calls add=%d commit=%d push=%d pr=%d", fakeGit.addAllCalls, fakeGit.commitCalls, fakeGit.pushCalls, fakeGitHub.createPRCalls)
	}
	if fakeGit.lastCommitMessage != "Split worker dispatch (closes #509)" || fakeGitHub.lastPRBody != "Closes #509\n\nSplit dispatch into helpers." {
		t.Fatalf("commit/PR text = %q / %q", fakeGit.lastCommitMessage, fakeGitHub.lastPRBody)
	}

	cleanup(ctx, dispatch, nil)
	if fakeGit.removeCalls != 1 || fakeGit.branchDeleteCalls != 1 || removedScratch != dispatch.scratch {
		t.Fatalf("cleanup calls remove=%d branchDelete=%d scratch=%q want %q", fakeGit.removeCalls, fakeGit.branchDeleteCalls, removedScratch, dispatch.scratch)
	}
	attempts, err = state.LoadAttempts(repo, "run-seam")
	if err != nil {
		t.Fatalf("LoadAttempts after cleanup: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Phase != "cleanup" || attempts[0].Status != "succeeded" {
		t.Fatalf("cleanup attempt = %#v", attempts)
	}
}

func TestHandleHungReportOnlyAndWriteRecoveryHelpers(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	scratchRoot := t.TempDir()
	var warnings strings.Builder
	fakeGit := &workerFakeGit{status: " M partial.go\n"}
	fakeGitHub := &workerFakeGitHub{}

	dispatch, err := prepareDispatch(ctx, Options{
		RepoPath:    repo,
		IssueNumber: 509,
		IssueTitle:  "Split worker dispatch",
		RunID:       "run-hung",
		Provider:    "codex",
		Stderr:      &warnings,
	}, Deps{
		Git: fakeGit,
		GitHub: func(string) GitHubClient {
			return fakeGitHub
		},
		AgentLookup: func(string) (agent.Runner, error) {
			return &workerFakeAgent{}, nil
		},
		AcquireLock: func(string, time.Duration) (Lock, error) {
			return &workerFakeLock{}, nil
		},
		Now: fixedNow,
		PID: func() int {
			return 1357
		},
		MkdirTemp: func(dir, pattern string) (string, error) {
			return os.MkdirTemp(scratchRoot, pattern)
		},
		RemoveAll: os.RemoveAll,
	})
	if err != nil {
		t.Fatalf("prepareDispatch returned error: %v", err)
	}
	if err := prepareWorktree(ctx, dispatch); err != nil {
		t.Fatalf("prepareWorktree returned error: %v", err)
	}
	dispatch.domainPolicy = domainWorkerPolicy{PartialWorkMode: partialWorkModeReportOnly}
	if err := os.WriteFile(dispatch.logPath, []byte("provider stalled\nlast useful line\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	agentResult := agent.Result{
		ExitCode:   -1,
		Hung:       true,
		HungReason: agent.HungReasonStall,
	}

	result, err := handleHungOrPartialWork(ctx, dispatch, agentResult)
	if err == nil {
		t.Fatal("handleHungOrPartialWork returned nil error, want hung error")
	}
	if result.OK || result.Status != "hung" || result.Branch != "loop/issue-509" || result.Attestation != nil {
		t.Fatalf("hung helper result = %#v", result)
	}
	if fakeGit.addAllCalls != 0 || fakeGit.commitCalls != 0 || fakeGit.forcePushCalls != 0 || fakeGitHub.createPRCalls != 0 {
		t.Fatalf("report-only helper made delivery calls add=%d commit=%d force=%d pr=%d", fakeGit.addAllCalls, fakeGit.commitCalls, fakeGit.forcePushCalls, fakeGitHub.createPRCalls)
	}
	if !strings.Contains(warnings.String(), "report-only preserved partial work") {
		t.Fatalf("warnings missing report-only note:\n%s", warnings.String())
	}

	writeRecovery(ctx, dispatch, err)
	brief, err := os.ReadFile(state.RecoveryBriefPath(repo, "run-hung", "job-509-1357"))
	if err != nil {
		t.Fatalf("read recovery brief: %v", err)
	}
	briefInfo, err := os.Stat(state.RecoveryBriefPath(repo, "run-hung", "job-509-1357"))
	if err != nil {
		t.Fatalf("stat recovery brief: %v", err)
	}
	if runtime.GOOS != "windows" && briefInfo.Mode().Perm() != 0o600 {
		t.Fatalf("recovery brief mode = %#o, want 0600", briefInfo.Mode().Perm())
	}
	for _, want := range []string{"- Status: hung", "- Last phase: worktree_created", " M partial.go", "last useful line"} {
		if !strings.Contains(string(brief), want) {
			t.Fatalf("recovery brief missing %q:\n%s", want, string(brief))
		}
	}
	attempts, err := state.LoadAttempts(repo, "run-hung")
	if err != nil {
		t.Fatalf("LoadAttempts returned error: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Status != "hung" || attempts[0].Attestation != nil {
		t.Fatalf("hung recovery attempt = %#v", attempts)
	}
	events, err := os.ReadFile(state.EventsPath(repo, "run-hung"))
	if err != nil {
		t.Fatalf("ReadFile events: %v", err)
	}
	for _, want := range []string{`"event":"worker_hung"`, `"event":"worker_partial_work_reported"`, `"status":"hung"`} {
		if !strings.Contains(string(events), want) {
			t.Fatalf("events missing %q:\n%s", want, string(events))
		}
	}
}

func fixedNow() time.Time {
	return time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
}

func validWorkerAgentResult(summary string, exitCode int) agent.Result {
	inputTokens := int64(120)
	outputTokens := int64(34)
	totalTokens := int64(154)
	return agent.Result{
		ExitCode: exitCode,
		Summary:  summary,
		Model:    "gpt-5.5",
		Effort:   "xhigh",
		Usage: attestation.Usage{
			InputTokens:  &inputTokens,
			OutputTokens: &outputTokens,
			TotalTokens:  &totalTokens,
		},
		StartedAt:  "2026-06-28T00:00:00Z",
		EndedAt:    "2026-06-28T00:00:42Z",
		DurationMS: 42000,
	}
}

func assertNoAttestationFootprint(t *testing.T, surface, text string, record attestation.AttestationRecord) {
	t.Helper()
	canonical, err := record.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON returned error: %v", err)
	}
	for _, forbidden := range []string{record.Header(), string(canonical), "[attestation]"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("%s contains forbidden attestation text %q:\n%s", surface, forbidden, text)
		}
	}
}

type workerFakeGit struct {
	status              string
	err                 error
	worktreeSetup       func(worktreePath string) error
	remoteBranchExists  bool
	fetchCalls          int
	fetchBase           string
	worktreeAddCalls    int
	worktreeBranch      string
	worktreeBase        string
	worktreePath        string
	removeCalls         int
	removePath          string
	branchDeleteCalls   int
	deletedBranch       string
	addAllCalls         int
	commitCalls         int
	pushCalls           int
	forcePushCalls      int
	lastCommitMessage   string
	lastForcePushBranch string
}

func (f *workerFakeGit) FetchOriginBase(_ context.Context, _, baseBranch string) error {
	f.fetchCalls++
	f.fetchBase = baseBranch
	return f.err
}

func (f *workerFakeGit) WorktreeAdd(_ context.Context, _, branch, worktreePath, baseBranch string) error {
	f.worktreeAddCalls++
	f.worktreeBranch = branch
	f.worktreeBase = baseBranch
	f.worktreePath = worktreePath
	if f.err != nil {
		return f.err
	}
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		return err
	}
	if f.worktreeSetup != nil {
		return f.worktreeSetup(worktreePath)
	}
	return nil
}

func (f *workerFakeGit) WorktreeRemove(_ context.Context, _, worktreePath string) error {
	f.removeCalls++
	f.removePath = worktreePath
	return nil
}

func (f *workerFakeGit) StatusPorcelain(context.Context, string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.status, nil
}

func (f *workerFakeGit) AddAll(context.Context, string) error {
	f.addAllCalls++
	return f.err
}

func (f *workerFakeGit) Commit(_ context.Context, _, message string) error {
	f.commitCalls++
	f.lastCommitMessage = message
	return f.err
}

func (f *workerFakeGit) PushUpstream(context.Context, string, string) error {
	f.pushCalls++
	if f.remoteBranchExists {
		return os.ErrExist
	}
	return f.err
}

func (f *workerFakeGit) PushUpstreamForceWithLease(_ context.Context, _, branch string) error {
	f.forcePushCalls++
	f.lastForcePushBranch = branch
	return f.err
}

func (f *workerFakeGit) BranchDelete(_ context.Context, _, branch string) error {
	f.branchDeleteCalls++
	f.deletedBranch = branch
	return nil
}

type workerFakeGitHub struct {
	prURL         string
	prs           []gh.PullRequestReference
	openPRs       []gh.PullRequest
	err           error
	repoErr       error
	createErr     error
	listHeadErr   error
	listOpenErr   error
	createPRCalls int
	lastPRHead    string
	lastPRBase    string
	lastPRTitle   string
	lastPRBody    string
}

func (f *workerFakeGitHub) RepoName(context.Context) (string, error) {
	if f.repoErr != nil {
		return "", f.repoErr
	}
	if f.err != nil {
		return "", f.err
	}
	return "owner/repo", nil
}

func (f *workerFakeGitHub) CreatePR(_ context.Context, head, base, title, body string) (string, error) {
	f.createPRCalls++
	if f.createErr != nil {
		return "", f.createErr
	}
	if f.err != nil {
		return "", f.err
	}
	f.lastPRHead = head
	f.lastPRBase = base
	f.lastPRTitle = title
	f.lastPRBody = body
	if f.prURL == "" {
		return "https://github.com/owner/repo/pull/1", nil
	}
	return f.prURL, nil
}

func (f *workerFakeGitHub) ListHeadPRs(context.Context, string) ([]gh.PullRequestReference, error) {
	if f.listHeadErr != nil {
		return nil, f.listHeadErr
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.prs, nil
}

func (f *workerFakeGitHub) ListOpenPRs(context.Context) ([]gh.PullRequest, error) {
	if f.listOpenErr != nil {
		return nil, f.listOpenErr
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.openPRs, nil
}

type workerFakeAgent struct {
	invocation agent.Invocation
	result     agent.Result
	resultSet  bool
	summary    string
	log        string
	exitCode   int
	err        error
}

func (f *workerFakeAgent) Run(_ context.Context, invocation agent.Invocation) (agent.Result, error) {
	f.invocation = invocation
	if err := os.WriteFile(invocation.LogPath, []byte(f.log), 0o644); err != nil {
		return agent.Result{ExitCode: -1}, err
	}
	if f.err != nil {
		return agent.Result{ExitCode: -1}, f.err
	}
	if f.resultSet {
		return f.result, nil
	}
	return agent.Result{ExitCode: f.exitCode, Summary: f.summary}, nil
}

type workerFakeLock struct {
	err error
}

func (l *workerFakeLock) Release() error {
	if l.err != nil {
		return l.err
	}
	return nil
}
