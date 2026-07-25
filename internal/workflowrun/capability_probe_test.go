package workflowrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/agent"
)

type capabilityProbeRunner struct {
	result agent.Result
	err    error
	prompt string
	probe  bool
}

func (r *capabilityProbeRunner) Run(_ context.Context, inv agent.Invocation) (agent.Result, error) {
	r.prompt = inv.Prompt
	r.probe = inv.CapabilityProbeOnly
	return r.result, r.err
}

func TestProductionCapabilityProbePreservesAttemptedMUIdentity(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	runner := &capabilityProbeRunner{result: agent.Result{
		ExitCode: -1, FailureClass: "model_unavailable",
		ActualProvider:   "codex",
		ActualInstallRef: "pinst-test", ArgvDigest: "sha256:test",
	}}
	account := "acct-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	runner.result.ActualAccountRef = account
	executor := ProductionChildExecutor{
		HomeDir: t.TempDir(),
		Lookup:  func(string) (agent.Runner, error) { return runner, nil },
	}
	result, err := executor.Execute(context.Background(), ChildExecInput{
		ProjectID: "proj-probe", RunID: "run-probe", GraphID: "g-probe",
		WorkItemID: "wi_research", AttemptID: "att-probe-g0",
		Intent: "product prompt must not reach provider",
		Route: ChildRoute{
			Provider: "codex", Model: "gpt-5.3-codex", Depth: "low",
			Permission: "read-only", AccountRef: account, InstallRef: "pinst-test",
			WindowKind: "weekly", ReservationID: "sres-probe",
			CapabilityProbeOnly: true,
		},
		RepoPath: repo, BaseRef: "HEAD", ReadOnly: true,
	})
	if err != nil {
		t.Fatalf("typed nonzero result should return structured result: %v", err)
	}
	if runner.prompt != "Reply with exactly OK. Do not use tools." {
		t.Fatalf("provider received unsafe prompt %q", runner.prompt)
	}
	if !runner.probe {
		t.Fatal("provider invocation missing capability-probe-only boundary")
	}
	if result.FailureClass != "model_unavailable" ||
		result.InvokedRoute.Model != "gpt-5.3-codex" ||
		result.InvokedRoute.AccountRef != account {
		t.Fatalf("attempted identity missing: %+v", result)
	}
	if result.ActualSources.Model != agent.ActualSourceAttemptedInvocation {
		t.Fatalf("model source=%q", result.ActualSources.Model)
	}
	if filepath.Base(result.WorktreePath) == "" {
		t.Fatal("expected isolated worktree")
	}
	evidence, readErr := os.ReadFile(filepath.Join(
		result.WorktreePath, ".loopcoder", "child-evidence", "wi_research.json",
	))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(evidence), "product prompt must not reach provider") ||
		!strings.Contains(string(evidence), "fixed read-only capability probe") {
		t.Fatalf("probe evidence retained product intent or lost fixed marker: %s", evidence)
	}
}

func TestProductionCapabilityProbeRejectsProseDerivedModelUnavailable(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	runner := &capabilityProbeRunner{result: agent.Result{
		ExitCode: -1, Summary: "model_unavailable",
		ActualProvider: "codex", ActualAccountRef: "acct-a", ActualInstallRef: "pinst-a",
	}}
	executor := ProductionChildExecutor{
		HomeDir: t.TempDir(),
		Lookup:  func(string) (agent.Runner, error) { return runner, nil },
	}
	result, _ := executor.Execute(context.Background(), ChildExecInput{
		ProjectID: "proj-probe-prose", RunID: "run-probe-prose", GraphID: "g-probe",
		WorkItemID: "wi_research", AttemptID: "att-probe-prose-g0",
		Route: ChildRoute{
			Provider: "codex", Model: "gpt-5.3-codex", Depth: "low",
			Permission: "read-only", AccountRef: "acct-a", InstallRef: "pinst-a",
			CapabilityProbeOnly: true,
		},
		RepoPath: repo, BaseRef: "HEAD", ReadOnly: true,
	})
	if result.FailureClass != "capability_probe_unclassified_failure" {
		t.Fatalf("prose-derived class was accepted: %+v", result)
	}
}

func TestProductionCapabilityProbeUnexpectedSuccessNeverIntegrates(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	runner := &capabilityProbeRunner{result: agent.Result{
		ExitCode: 0, Summary: "OK",
		ActualProvider: "codex", ActualModel: "gpt-5.3-codex", ActualEffort: "low",
		ActualPermission: "read-only", ActualAccountRef: "acct-a", ActualInstallRef: "pinst-a",
	}}
	executor := ProductionChildExecutor{
		HomeDir: t.TempDir(),
		Lookup:  func(string) (agent.Runner, error) { return runner, nil },
	}
	result, err := executor.Execute(context.Background(), ChildExecInput{
		ProjectID: "proj-probe-success", RunID: "run-probe-success", GraphID: "g-probe",
		WorkItemID: "wi_research", AttemptID: "att-probe-success-g0",
		Route: ChildRoute{
			Provider: "codex", Model: "gpt-5.3-codex", Depth: "low",
			Permission: "read-only", AccountRef: "acct-a", InstallRef: "pinst-a",
			WindowKind: "weekly", ReservationID: "sres-probe",
			CapabilityProbeOnly: true,
		},
		RepoPath: repo, BaseRef: "HEAD", ReadOnly: true,
	})
	if err == nil {
		t.Fatal("unexpected probe success must fail closed")
	}
	if result.Terminal != "failed" || result.FailureClass != "capability_probe_unexpected_success" {
		t.Fatalf("unexpected result: %+v", result)
	}
}
