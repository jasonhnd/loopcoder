package workflowrun

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/agent"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
)

type structuredVerifierRunner struct {
	decision    string
	raw         string
	mutate      func(map[string]any)
	actualModel string
}

func (r structuredVerifierRunner) Run(ctx context.Context, inv agent.Invocation) (agent.Result, error) {
	cmd := exec.Command("/bin/sleep", "0.01")
	cmd.Dir = inv.WorktreePath
	sup, err := supervisedexec.Run(ctx, cmd, supervisedexec.Options{
		HardCap: 5 * time.Second,
		RunID:   inv.RunID,
		Role:    inv.Role,
		OnStart: func(started supervisedexec.StartedProcess) error {
			if inv.OnProviderStart == nil {
				return nil
			}
			return inv.OnProviderStart(agent.ProviderProcess{
				PID: started.PID, PGID: started.PGID,
				ProcessBirthIdentity:  started.ProcessBirthIdentity,
				ExecutableIdentity:    started.ExecutableIdentity,
				ObservedAt:            started.ObservedAt,
				IdentityAmbiguous:     started.IdentityAmbiguous,
				IdentityAmbiguityNote: started.IdentityAmbiguityNote,
			})
		},
	})
	if err != nil {
		return agent.Result{ExitCode: sup.ExitCode}, err
	}
	summary := r.raw
	if summary == "" {
		var schema struct {
			Properties map[string]struct {
				Const string `json:"const"`
			} `json:"properties"`
		}
		if err := json.Unmarshal([]byte(inv.OutputSchema), &schema); err != nil {
			return agent.Result{ExitCode: 1}, err
		}
		value := func(key string) string { return schema.Properties[key].Const }
		findings := []map[string]string{}
		if r.decision == VerifierDecisionFail {
			findings = append(findings, map[string]string{
				"severity": "critical",
				"summary":  "The exact reviewed tree does not compile.",
			})
		}
		payload := map[string]any{
			"schema": value("schema"), "decision": r.decision,
			"project_id": value("project_id"), "run_id": value("run_id"),
			"graph_id":              value("graph_id"),
			"execution_plan_digest": value("execution_plan_digest"),
			"graph_digest":          value("graph_digest"),
			"work_item_id":          value("work_item_id"), "attempt_id": value("attempt_id"),
			"reviewed_head_sha": value("reviewed_head_sha"),
			"summary":           "Independent exact-head verification completed.",
			"findings":          findings,
		}
		if r.mutate != nil {
			r.mutate(payload)
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return agent.Result{ExitCode: 1}, err
		}
		summary = string(raw)
	}
	actualModel := r.actualModel
	if actualModel == "" {
		actualModel = "verifier-model"
	}
	return agent.Result{
		ExitCode: 0, Summary: summary,
		ActualProvider: "verifier-test", ActualModel: actualModel,
		ActualEffort: "high", ActualPermission: "read-only",
		ActualAccountRef: "acct-verifier", ActualInstallRef: "pinst-verifier",
		ActualSourceModel:      agent.ActualSourceProviderStream,
		ActualSourceEffort:     agent.ActualSourceAcceptedInvocation,
		ActualSourcePermission: agent.ActualSourceAcceptedInvocation,
		ActualSourceAccount:    agent.ActualSourceAuthBinding,
		ActualSourceInstall:    agent.ActualSourceInstallBinding,
		ArgvDigest:             "sha256:" + strings.Repeat("d", 64),
	}, nil
}

func productionVerifierInput(repo string) ChildExecInput {
	return ChildExecInput{
		ProjectID: "proj-verifier-production", RunID: "run-verifier-production",
		GraphID:             "g-verifier-production",
		ExecutionPlanDigest: "sha256:" + strings.Repeat("a", 64),
		GraphDigest:         "sha256:" + strings.Repeat("b", 64),
		WorkItemID:          "wi_verify", ClaimID: "claim-verifier",
		AttemptID: "att-wi_verify-production-g0",
		Intent:    "independent verification: adversarial review of exact integrated head",
		Route: ChildRoute{
			Provider: "verifier-test", Model: "verifier-model", Depth: "high",
			Permission: "read-only", AccountRef: "acct-verifier",
			InstallRef: "pinst-verifier", WindowKind: "rolling",
			ReservationID: "sres-verifier",
		},
		RepoPath: repo, BaseRef: "HEAD", ReadOnly: true,
	}
}

func newProductionVerifierRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	mustRun(t, repo, "git", "init")
	mustWrite(t, filepath.Join(repo, "go.mod"), "module example.invalid/verifier\n\ngo 1.22\n")
	mustWrite(t, filepath.Join(repo, "product.go"), "package verifier\n")
	mustRun(t, repo, "git", "add", "go.mod", "product.go")
	mustRun(t, repo, "git", "-c", "user.email=test@example.invalid", "-c", "user.name=test", "commit", "-m", "integrated product")
	return repo
}

func TestProductionVerifierVerdictDecisionBoundary(t *testing.T) {
	for _, tc := range []struct {
		name        string
		decision    string
		wantTerm    string
		wantFailure string
		wantErr     bool
	}{
		{"pass", VerifierDecisionPass, "succeeded", "", false},
		{"fail", VerifierDecisionFail, "failed", FailureClassVerifierFailed, false},
		{"needs_human", VerifierDecisionNeedsHuman, "failed", FailureClassVerifierHuman, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newProductionVerifierRepo(t)
			executor := ProductionChildExecutor{
				HomeDir: t.TempDir(), HardCap: 5 * time.Second,
				Lookup: func(string) (agent.Runner, error) {
					return structuredVerifierRunner{decision: tc.decision}, nil
				},
			}
			result, err := executor.Execute(context.Background(), productionVerifierInput(repo))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v result=%+v", err, result)
			}
			if string(result.Terminal) != tc.wantTerm || result.FailureClass != tc.wantFailure {
				t.Fatalf("terminal=%q class=%q result=%+v", result.Terminal, result.FailureClass, result)
			}
			if result.VerifierDecision != tc.decision ||
				!isExactSHA256Digest(result.VerifierVerdictDigest) ||
				!isExactGitOID(result.VerifierReviewedHeadSHA) {
				t.Fatalf("structured verdict binding missing: %+v", result)
			}
			for _, leaf := range []string{"verdict.json", "verdict.md"} {
				st, statErr := os.Lstat(filepath.Join(result.WorktreePath, leaf))
				if statErr != nil || !st.Mode().IsRegular() {
					t.Fatalf("%s not a regular materialized verdict: st=%v err=%v", leaf, st, statErr)
				}
			}
		})
	}
}

func TestProductionVerifierMalformedOrMismatchedFailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		runner structuredVerifierRunner
	}{
		{"malformed", structuredVerifierRunner{raw: `{"schema":`}},
		{"missing", structuredVerifierRunner{raw: `{}`}},
		{"unknown_field", structuredVerifierRunner{
			decision: VerifierDecisionPass,
			mutate:   func(payload map[string]any) { payload["provider_prose"] = "not allowlisted" },
		}},
		{"attempt_mismatch", structuredVerifierRunner{
			decision: VerifierDecisionPass,
			mutate:   func(payload map[string]any) { payload["attempt_id"] = "att-other-g0" },
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := newProductionVerifierRepo(t)
			executor := ProductionChildExecutor{
				HomeDir: t.TempDir(), HardCap: 5 * time.Second,
				Lookup: func(string) (agent.Runner, error) { return tc.runner, nil },
			}
			result, err := executor.Execute(context.Background(), productionVerifierInput(repo))
			if err == nil || result.Terminal != "failed" ||
				result.FailureClass != FailureClassVerifierInvalid ||
				result.VerifierDecision != "" {
				t.Fatalf("mutation accepted: err=%v result=%+v", err, result)
			}
		})
	}
}

func TestProductionNegativeVerifierRequiresExactObservedRoute(t *testing.T) {
	repo := newProductionVerifierRepo(t)
	executor := ProductionChildExecutor{
		HomeDir: t.TempDir(), HardCap: 5 * time.Second,
		Lookup: func(string) (agent.Runner, error) {
			return structuredVerifierRunner{
				decision:    VerifierDecisionFail,
				actualModel: "different-verifier-model",
			}, nil
		},
	}
	result, err := executor.Execute(context.Background(), productionVerifierInput(repo))
	if err == nil || result.Terminal != "failed" || result.FailureClass != "route_mismatch" {
		t.Fatalf("mismatched negative route accepted: err=%v result=%+v", err, result)
	}
	if result.VerifierDecision != "" || result.VerifierVerdictDigest != "" ||
		result.VerifierReviewedHeadSHA != "" {
		t.Fatalf("mismatched route minted authoritative verifier evidence: %+v", result)
	}
}
