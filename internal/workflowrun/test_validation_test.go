package workflowrun

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/agent"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
	"github.com/jasonhnd/loopcoder/internal/workflowdef"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

type goValidationProductRunner struct {
	testBody string
}

func (r goValidationProductRunner) Run(ctx context.Context, inv agent.Invocation) (agent.Result, error) {
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
	testBody := r.testBody
	if testBody == "" {
		testBody = `package slug

import "testing"
import "os"

func TestNormalize(t *testing.T) {
	if got := os.Getenv("LOOPCODER_TEST_SECRET"); got != "" {
		t.Fatalf("production test inherited secret environment: %q", got)
	}
	if got := Normalize("e\u0301"); got != "é" {
		t.Fatalf("Normalize() = %q", got)
	}
}
`
	}
	if err := os.MkdirAll(filepath.Join(inv.WorktreePath, "slug"), 0o700); err != nil {
		return agent.Result{ExitCode: 1}, err
	}
	if err := os.WriteFile(filepath.Join(inv.WorktreePath, "slug", "slug_test.go"), []byte(testBody), 0o600); err != nil {
		return agent.Result{ExitCode: 1}, err
	}
	return agent.Result{
		ExitCode: 0, Summary: "provider added focused tests",
		ActualProvider: "validation-test", ActualModel: "validation-model",
		ActualEffort: "medium", ActualPermission: "bounded_write",
		ActualAccountRef: "acct-validation", ActualInstallRef: "pinst-validation",
		ActualSourceModel:      agent.ActualSourceAcceptedInvocation,
		ActualSourceEffort:     agent.ActualSourceAcceptedInvocation,
		ActualSourcePermission: agent.ActualSourceAcceptedInvocation,
		ActualSourceAccount:    agent.ActualSourceAuthBinding,
		ActualSourceInstall:    agent.ActualSourceInstallBinding,
		ArgvDigest:             "sha256:" + strings.Repeat("a", 64),
	}, nil
}

func newGoValidationRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	mustRun(t, repo, "git", "init")
	mustWrite(t, filepath.Join(repo, ".gitignore"), ".loopcoder/\n")
	mustWrite(t, filepath.Join(repo, "go.mod"), `module example.invalid/validation

go 1.22

require golang.org/x/text v0.40.0
`)
	mustWrite(t, filepath.Join(repo, "slug", "slug.go"), `package slug

import "golang.org/x/text/unicode/norm"

func Normalize(s string) string { return norm.NFC.String(s) }
`)
	mustRun(t, repo, "git", "add", ".gitignore", "go.mod", "slug/slug.go")
	mustRun(t, repo, "git", "-c", "user.email=test@example.invalid", "-c", "user.name=test", "commit", "-m", "base without go.sum")
	if _, err := os.Lstat(filepath.Join(repo, "go.sum")); !os.IsNotExist(err) {
		t.Fatalf("fixture unexpectedly has go.sum: %v", err)
	}
	return repo
}

func productionGoTestsInput(repo string) ChildExecInput {
	return ChildExecInput{
		ProjectID: "proj-go-validation", RunID: "run-go-validation",
		GraphID:             "g-go-validation",
		ExecutionPlanDigest: "sha256:" + strings.Repeat("b", 64),
		GraphDigest:         "sha256:" + strings.Repeat("c", 64),
		WorkItemID:          "wi_tests", ClaimID: "claim-go-validation",
		AttemptID: "att-wi_tests-go-validation-g0",
		Intent:    "tests: add focused tests and run repository test commands",
		Route: ChildRoute{
			Provider: "validation-test", Model: "validation-model", Depth: "medium",
			Permission: "bounded_write", AccountRef: "acct-validation",
			InstallRef: "pinst-validation", WindowKind: "weekly",
			ReservationID: "sres-validation",
		},
		RepoPath: repo, BaseRef: "HEAD",
	}
}

func TestProductionGoTestsGateMaterializesAndIntegratesGoSum(t *testing.T) {
	t.Setenv("LOOPCODER_TEST_SECRET", "credential-must-not-appear")
	repo := newGoValidationRepo(t)
	home := t.TempDir()
	executor := ProductionChildExecutor{
		HomeDir: home, HardCap: 5 * time.Second,
		TestValidationHardCap: 2 * time.Minute,
		Lookup: func(string) (agent.Runner, error) {
			return goValidationProductRunner{}, nil
		},
	}
	result, err := executor.Execute(context.Background(), productionGoTestsInput(repo))
	if err != nil {
		t.Fatalf("production tests executor: %v result=%+v", err, result)
	}
	if result.Terminal != "succeeded" || result.TestValidationStatus != testValidationStatusPassed {
		t.Fatalf("tests child did not pass exact production gate: %+v", result)
	}
	for _, value := range []string{
		result.TestValidationEvidence,
		result.TestValidationCommandDigest,
		result.OutputEvidence,
	} {
		if !isExactSHA256Digest(value) {
			t.Fatalf("non-exact digest %q in %+v", value, result)
		}
	}
	if !isExactGitOID(result.TestValidationHeadSHA) {
		t.Fatalf("validation HEAD is not exact: %q", result.TestValidationHeadSHA)
	}
	wantFiles := map[string]bool{"go.sum": false, "slug/slug_test.go": false}
	for _, rel := range result.FilesTouched {
		if _, ok := wantFiles[rel]; ok {
			wantFiles[rel] = true
		}
	}
	for rel, found := range wantFiles {
		if !found {
			t.Fatalf("production gate did not include %s in product files: %v", rel, result.FilesTouched)
		}
	}
	raw, err := os.ReadFile(result.TestValidationReceiptPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "credential-must-not-appear") {
		t.Fatal("validation receipt retained environment credential material")
	}
	if err := validatePassedGoTestReceipt(
		context.Background(),
		home,
		"proj-go-validation",
		"run-go-validation",
		"wi_tests",
		"att-wi_tests-go-validation-g0",
		result.WorktreePath,
		result,
	); err != nil {
		t.Fatalf("exact receipt did not revalidate: %v", err)
	}

	branch := "loopcoder/test-go-validation"
	integrator := GitBranchIntegrator{LedgerDir: filepath.Join(home, "integrate-ledger")}
	if _, err := integrator.EnsureGoalBranch(context.Background(), repo, "HEAD", branch); err != nil {
		t.Fatal(err)
	}
	integrated, err := integrator.IntegrateChild(context.Background(), IntegrateRequest{
		RepoPath: repo, GoalBranch: branch,
		WorkItemID: "wi_tests", AttemptID: "att-wi_tests-go-validation-g0",
		ChildWorktree: result.WorktreePath, ProductFiles: result.FilesTouched,
		Intent: "tests: production-owned Go test gate",
	})
	if err != nil {
		t.Fatalf("integrate validated product: %v", err)
	}
	if !isExactGitOID(integrated.CommitSHA) {
		t.Fatalf("integrate commit is not exact: %+v", integrated)
	}
	mustRun(t, repo, "git", "show", branch+":go.sum")
}

func TestProductionGoTestsGateFailureAndTimeoutFailClosed(t *testing.T) {
	tests := []struct {
		name        string
		testBody    string
		hardCap     time.Duration
		wantFailure string
	}{
		{
			name: "test_failure",
			testBody: `package slug

import (
	"fmt"
	"os"
	"testing"
)

func TestBroken(t *testing.T) {
	if got := os.Getenv("LOOPCODER_TEST_SECRET"); got != "" {
		t.Fatalf("production test inherited secret environment: %q", got)
	}
	fmt.Fprintln(os.Stderr, "credential-output-must-not-appear")
	t.Fatal("intentional failure")
}
`,
			hardCap:     2 * time.Minute,
			wantFailure: "test_validation_failed",
		},
		{
			name:        "timeout",
			hardCap:     time.Nanosecond,
			wantFailure: "test_validation_timeout",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LOOPCODER_TEST_SECRET", "credential-output-must-not-appear")
			repo := newGoValidationRepo(t)
			home := t.TempDir()
			executor := ProductionChildExecutor{
				HomeDir: home, HardCap: 5 * time.Second,
				TestValidationHardCap: tc.hardCap,
				Lookup: func(string) (agent.Runner, error) {
					return goValidationProductRunner{testBody: tc.testBody}, nil
				},
			}
			result, err := executor.Execute(context.Background(), productionGoTestsInput(repo))
			if err == nil || result.Terminal != "failed" || result.FailureClass != tc.wantFailure {
				t.Fatalf("gate failure escaped: err=%v result=%+v", err, result)
			}
			if result.TestValidationStatus != testValidationStatusFailed ||
				!isExactSHA256Digest(result.TestValidationEvidence) ||
				result.TestValidationReceiptPath == "" {
				t.Fatalf("failed gate lacks exact redacted receipt: %+v", result)
			}
			raw, readErr := os.ReadFile(result.TestValidationReceiptPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if strings.Contains(string(raw), "credential-output-must-not-appear") {
				t.Fatal("failed validation receipt persisted raw command output")
			}
			if err := validatePassedGoTestReceipt(
				context.Background(),
				home,
				"proj-go-validation",
				"run-go-validation",
				"wi_tests",
				"att-wi_tests-go-validation-g0",
				result.WorktreePath,
				result,
			); err == nil {
				t.Fatal("failed validation receipt was accepted as passed")
			}
		})
	}
}

func TestPassedGoTestsReceiptTamperAndPathSwapFailClosed(t *testing.T) {
	repo := newGoValidationRepo(t)
	home := t.TempDir()
	executor := ProductionChildExecutor{
		HomeDir: home, HardCap: 5 * time.Second,
		TestValidationHardCap: 2 * time.Minute,
		Lookup: func(string) (agent.Runner, error) {
			return goValidationProductRunner{}, nil
		},
	}
	result, err := executor.Execute(context.Background(), productionGoTestsInput(repo))
	if err != nil {
		t.Fatal(err)
	}
	swapped := result
	swapped.TestValidationReceiptPath = filepath.Join(t.TempDir(), "test-validation.json")
	if err := validatePassedGoTestReceipt(
		context.Background(),
		home,
		"proj-go-validation",
		"run-go-validation",
		"wi_tests",
		"att-wi_tests-go-validation-g0",
		result.WorktreePath,
		swapped,
	); err == nil {
		t.Fatal("receipt path swap was accepted")
	}
	raw, err := os.ReadFile(result.TestValidationReceiptPath)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n', '{', '}')
	if err := os.WriteFile(result.TestValidationReceiptPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validatePassedGoTestReceipt(
		context.Background(),
		home,
		"proj-go-validation",
		"run-go-validation",
		"wi_tests",
		"att-wi_tests-go-validation-g0",
		result.WorktreePath,
		result,
	); err == nil {
		t.Fatal("receipt trailing JSON tamper was accepted")
	}
}

func TestServiceRefusesGoTestsGateFailureBeforeIntegrate(t *testing.T) {
	repo := newGoValidationRepo(t)
	home := t.TempDir()
	at := time.Date(2026, 7, 26, 4, 30, 0, 0, time.UTC)
	def := workflowdef.Definition{
		SchemaVersion: 1, GraphID: "g-go-gate-failure", Source: "explicit_definition",
		Items: []workflowdef.DefItem{{
			ID: "wi_tests", Intent: "tests: add focused tests and run repository test commands",
			Status: "required", Owner: "worker", IntegrationOrder: 1,
			RouteRequirement: "class=tera,depth=medium,permission=bounded_write",
			OutputContract:   "test_pass",
		}},
	}
	req := Request{
		ProjectID: "proj-go-service-failure", RunID: "run-go-service-failure",
		Definition: def, Actor: "owner", RepoPath: repo, BaseRef: "HEAD",
		GoalBranch: "loopcoder/go-service-failure",
		ChildRoutes: map[string]ChildRoute{
			"wi_tests": {
				Provider: "validation-test", Model: "validation-model",
				TaskClass: "tera", Depth: "medium", Permission: "bounded_write",
				AccountRef: "acct-validation", InstallRef: "pinst-validation",
				WindowKind: "weekly", ReservationID: "sres-validation",
				RouteReason: "exact production regression",
			},
		},
	}
	req = withExactWorkflowDigests(t, req, at)
	executor := ProductionChildExecutor{
		HomeDir: home, HardCap: 5 * time.Second,
		TestValidationHardCap: 2 * time.Minute,
		Lookup: func(string) (agent.Runner, error) {
			return goValidationProductRunner{testBody: `package slug

import "testing"

func TestBroken(t *testing.T) { missingSymbol() }
`}, nil
		},
	}
	result, err := (Service{Now: func() time.Time { return at }, HomeDir: home, Executor: executor}).
		Execute(context.Background(), req)
	if err == nil || result.Status != StatusBlocked {
		t.Fatalf("failed test gate did not block service: err=%v result=%+v", err, result)
	}
	for _, child := range result.Children {
		if child.WorkItemID != "wi_tests" {
			continue
		}
		if child.Terminal == "succeeded" || child.FailureClass != "test_validation_failed" ||
			child.IntegrateCommitSHA != "" {
			t.Fatalf("failed test gate escaped terminal/integrate boundary: %+v", child)
		}
	}
	show := exec.Command("git", "-C", repo, "show", "loopcoder/go-service-failure:slug/slug_test.go")
	if err := show.Run(); err == nil {
		t.Fatal("failed tests product was integrated onto the goal branch")
	}
	raw, readErr := os.ReadFile(result.EventLogPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	text := string(raw)
	if strings.Contains(text, `"kind":"integrate"`) ||
		strings.Contains(text, `"terminal":"succeeded"`) ||
		!strings.Contains(text, `"test_validation_evidence":"sha256:`) {
		t.Fatalf("event log does not prove fail-closed test gate:\n%s", text)
	}
}

func withExactWorkflowDigests(t *testing.T, req Request, at time.Time) Request {
	t.Helper()
	plan, err := workflowdef.Normalize(req.Definition)
	if err != nil {
		t.Fatal(err)
	}
	approval, err := workflowdef.Approve(plan.Digest, req.Actor, "test exact digest", at)
	if err != nil {
		t.Fatal(err)
	}
	registry := workflowdef.NewRegistry()
	materialized, err := registry.Materialize(req.ProjectID, req.Definition, approval, at)
	if err != nil {
		t.Fatal(err)
	}
	req.ExpectedPlanDigest = plan.Digest
	req.ExpectedGraphDigest = workgraph.DigestGraph(materialized.Graph)
	return req
}
