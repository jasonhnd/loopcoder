package providerexec_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/agent"
	"github.com/jasonhnd/loopcoder/internal/providerexec"
)

// conformanceRunner affirms Actual* via accepted_invocation after success only.
type conformanceRunner struct {
	provider  string
	failClass string
	failErr   error
	exitCode  int
	account   bool
	depth     bool
}

func (c conformanceRunner) Run(ctx context.Context, inv agent.Invocation) (agent.Result, error) {
	if c.failClass != "" || c.failErr != nil || c.exitCode != 0 {
		res := agent.Result{
			ExitCode: firstNonZero(c.exitCode, 1), FailureClass: c.failClass,
			Summary: "failed", ActualProvider: c.provider,
		}
		// Must NOT set accepted Actual* on failure.
		return res, c.failErr
	}
	res := agent.Result{
		ExitCode: 0, Summary: "conformance ok",
		ActualProvider: c.provider, AdapterVersion: "conformance-1",
	}
	argv := []string{c.provider}
	if inv.ReadOnly {
		argv = append(argv, "--sandbox", "read-only")
	} else {
		argv = append(argv, "-s", "workspace-write")
	}
	if inv.Model != "" {
		argv = append(argv, "-m", inv.Model)
	}
	if inv.Effort != "" {
		argv = append(argv, "--effort", inv.Effort)
	}
	// Only after success (we already know exit 0).
	agent.AffirmAcceptedInvocation(&res, inv, argv, true, agent.AcceptedInvocationOpts{
		PermissionNoFallback: true,
		ModelNoFallback:      true,
		EffortNoFallback:     c.depth,
	})
	if c.account && inv.AccountRef != "" {
		res.ActualAccountRef = inv.AccountRef
		res.ActualSourceAccount = agent.ActualSourceAuthBinding
	}
	if inv.InstallRef != "" {
		res.ActualInstallRef = inv.InstallRef
		res.ActualSourceInstall = agent.ActualSourceInstallBinding
	}
	if inv.Effort != "" && !c.depth {
		res.ActualEffort = ""
		res.ActualSourceEffort = ""
	}
	return res, nil
}

func firstNonZero(v, def int) int {
	if v != 0 {
		return v
	}
	return def
}

func TestAgentAdapterConformance_SupportedProvidersAffirmPermission(t *testing.T) {
	for _, tc := range []struct {
		prov    string
		account bool
		depth   bool
	}{
		{"codex", true, true},
		{"grok", true, true},
	} {
		t.Run(tc.prov, func(t *testing.T) {
			a := &providerexec.AgentAdapter{
				Lookup: func(p string) (agent.Runner, error) {
					return conformanceRunner{provider: p, account: tc.account, depth: tc.depth}, nil
				},
			}
			out, err := a.Execute(context.Background(), providerexec.Request{
				RequestID: "r1", ProjectID: "p", AttemptID: "a1",
				WorkDir: t.TempDir(), PromptRef: "do useful work",
				Route: providerexec.Route{
					Provider: tc.prov, Model: "m1", Effort: "medium",
					Permission: "bounded_write",
					AccountRef: "acct-" + strings.Repeat("a", 64),
					InstallRef: "install-" + tc.prov,
				},
				Timeout: 5 * time.Second,
			})
			if err != nil {
				t.Fatalf("execute: %v out=%+v", err, out)
			}
			if out.Failure != "" {
				t.Fatalf("failure=%s msg=%s sources=%+v actual=%+v", out.Failure, out.Message, out.ActualSources, out.ActualRoute)
			}
			if out.ActualRoute.Permission == "" {
				t.Fatal("ActualPermission empty")
			}
			if out.ActualSources.Permission != agent.ActualSourceAcceptedInvocation {
				t.Fatalf("permission source=%q want accepted_invocation (not provider_stream)", out.ActualSources.Permission)
			}
			if out.ActualSources.Account != agent.ActualSourceAuthBinding {
				t.Fatalf("account source=%q want auth_binding", out.ActualSources.Account)
			}
			if out.ActualSources.Install != agent.ActualSourceInstallBinding {
				t.Fatalf("install source=%q want install_binding", out.ActualSources.Install)
			}
			if out.ArgvDigest == "" {
				t.Fatal("ArgvDigest required on Outcome")
			}
		})
	}
}

func TestAgentAdapter_ModelUnavailableNotRewrittenToRouteMismatch(t *testing.T) {
	a := &providerexec.AgentAdapter{
		Lookup: func(p string) (agent.Runner, error) {
			return conformanceRunner{
				provider: p, failClass: "model_unavailable",
				failErr: errors.New("antigravity invalid model selection (model_unavailable)"),
			}, nil
		},
	}
	out, _ := a.Execute(context.Background(), providerexec.Request{
		RequestID: "r-mu", ProjectID: "p", AttemptID: "a-mu",
		WorkDir: t.TempDir(), PromptRef: "do useful work",
		Route: providerexec.Route{
			Provider: "antigravity", Model: "bad-model", Effort: "medium",
			Permission: "bounded_write",
			AccountRef: "acct-" + strings.Repeat("c", 64),
			InstallRef: "install-ag",
		},
	})
	if out.Failure != providerexec.FailModelUnavailable {
		t.Fatalf("want model_unavailable, got %s msg=%s", out.Failure, out.Message)
	}
	// Must not have accepted Actual* from failed run
	if out.ActualRoute.Permission != "" && out.ActualSources.Permission == agent.ActualSourceAcceptedInvocation {
		t.Fatal("failed model_unavailable must not carry accepted_invocation permission")
	}
}

func TestAgentAdapter_NonzeroNeverAccepted(t *testing.T) {
	a := &providerexec.AgentAdapter{
		Lookup: func(p string) (agent.Runner, error) {
			return conformanceRunner{provider: p, exitCode: 2}, nil
		},
	}
	out, _ := a.Execute(context.Background(), providerexec.Request{
		RequestID: "r-nz", ProjectID: "p", AttemptID: "a-nz",
		WorkDir: t.TempDir(), PromptRef: "do useful work",
		Route: providerexec.Route{Provider: "codex", Model: "m", Effort: "low", Permission: "bounded_write"},
	})
	if out.Failure != providerexec.FailProcess {
		t.Fatalf("want process_failure, got %s", out.Failure)
	}
	if out.ExitCode != 2 {
		t.Fatalf("exit=%d", out.ExitCode)
	}
}

func TestAgentAdapter_UnsupportedAccountRejects(t *testing.T) {
	a := &providerexec.AgentAdapter{
		Lookup: func(p string) (agent.Runner, error) {
			return conformanceRunner{provider: p, account: false, depth: true}, nil
		},
	}
	out, _ := a.Execute(context.Background(), providerexec.Request{
		RequestID: "r2", ProjectID: "p", AttemptID: "a2",
		WorkDir: t.TempDir(), PromptRef: "do useful work",
		Route: providerexec.Route{
			Provider: "claude", Model: "m1", Effort: "medium",
			Permission: "bounded_write",
			AccountRef: "acct-" + strings.Repeat("b", 64),
			InstallRef: "install-claude",
		},
	})
	if out.Failure != providerexec.FailRouteMismatch {
		t.Fatalf("want route_mismatch for unaffirmed account, got %s msg=%s", out.Failure, out.Message)
	}
}

func TestAgentAdapter_GeminiDepthUnsupported(t *testing.T) {
	a := &providerexec.AgentAdapter{
		Lookup: func(p string) (agent.Runner, error) {
			return conformanceRunner{provider: "gemini", account: false, depth: false}, nil
		},
	}
	out, _ := a.Execute(context.Background(), providerexec.Request{
		RequestID: "r3", ProjectID: "p", AttemptID: "a3",
		WorkDir: t.TempDir(), PromptRef: "do useful work",
		Route: providerexec.Route{
			Provider: "gemini", Model: "gemini-2.5-flash", Effort: "medium",
			Permission: "bounded_write",
		},
	})
	if out.Failure != providerexec.FailRouteMismatch {
		t.Fatalf("want route_mismatch for unaffirmed depth, got %s msg=%s", out.Failure, out.Message)
	}
}

func TestAffirmAcceptedInvocationPermissionFromArgv(t *testing.T) {
	res := agent.Result{ExitCode: 0}
	inv := agent.Invocation{Permission: "bounded_write", BoundedWrite: true, Model: "m", Effort: "high"}
	argv := []string{"codex", "exec", "-s", "workspace-write", "-m", "m", "-c", "model_reasoning_effort=high"}
	agent.AffirmAcceptedInvocation(&res, inv, argv, true, agent.AcceptedInvocationOpts{
		PermissionNoFallback: true, ModelNoFallback: true, EffortNoFallback: true,
	})
	if res.ActualPermission != "bounded_write" {
		t.Fatalf("perm=%q", res.ActualPermission)
	}
	if res.ActualSourcePermission != agent.ActualSourceAcceptedInvocation {
		t.Fatalf("source=%q", res.ActualSourcePermission)
	}
}
