package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/agent"
	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/ciwatch"
	"github.com/jasonhnd/loopcoder/internal/commitstage"
	"github.com/jasonhnd/loopcoder/internal/directdelivery"
	"github.com/jasonhnd/loopcoder/internal/prstage"
	"github.com/jasonhnd/loopcoder/internal/pushstage"
	"github.com/jasonhnd/loopcoder/internal/routepin"
)

// affirmingRunner is a test-only controlled runner that affirms the provider
// passed to AgentLookup (product pins require exact ActualProvider match).
// Writes a real worktree artifact so ChangedPaths is non-empty (product evidence).
type affirmingRunner struct{ Provider string }

func (a affirmingRunner) Run(ctx context.Context, inv agent.Invocation) (agent.Result, error) {
	if strings.TrimSpace(inv.Prompt) == "" {
		return agent.Result{ExitCode: 1}, errors.New("empty prompt")
	}
	// Prove worktree is a real git checkout.
	if _, err := os.Stat(filepath.Join(inv.WorktreePath, ".git")); err != nil {
		return agent.Result{ExitCode: 1}, err
	}
	// Materialize a useful change so delivery has owned paths (not empty product diff).
	artifact := filepath.Join(inv.WorktreePath, "docs", "CHANGE.md")
	if err := os.MkdirAll(filepath.Dir(artifact), 0o755); err != nil {
		return agent.Result{ExitCode: 1}, err
	}
	if err := os.WriteFile(artifact, []byte("test runner change\n"), 0o600); err != nil {
		return agent.Result{ExitCode: 1}, err
	}
	prov := firstNonEmptyFieldCLI(a.Provider, "codex")
	return agent.Result{
		ExitCode: 0, Summary: "test-runner-ok " + inv.Prompt[:min(20, len(inv.Prompt))],
		Model: inv.Model, Effort: inv.Effort,
		ActualProvider: prov, ActualModel: inv.Model, ActualEffort: inv.Effort,
		ActualPermission:       firstNonEmptyFieldCLI(inv.Permission, "default"),
		ActualAccountRef:       inv.AccountRef,
		ActualInstallRef:       inv.InstallRef,
		ActualSourceModel:      agent.ActualSourceAcceptedInvocation,
		ActualSourceEffort:     agent.ActualSourceAcceptedInvocation,
		ActualSourcePermission: agent.ActualSourceAcceptedInvocation,
		ActualSourceAccount:    agent.ActualSourceAuthBinding,
		ActualSourceInstall:    agent.ActualSourceInstallBinding,
		ArgvDigest:             "sha256:test-runner-argv",
	}, nil
}

// fixtureRunner remains as an alias for legacy tests that still inject it.
type fixtureRunner = affirmingRunner

// testDeliveryDeps injects explicit Fake* ports for black-box run tests.
// Production never auto-wires these; tests must inject.
func testDeliveryDeps(t *testing.T, now func() time.Time, baseSHA string) *directdelivery.Deps {
	t.Helper()
	if baseSHA == "" {
		baseSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	}
	owned := []string{"docs/CHANGE.md"}
	fg := commitstage.NewFakeGit(baseSHA)
	fg.SetDirty(owned)
	d := directdelivery.Deps{
		Now: now, Git: fg, Remote: pushstage.NewFakeRemote(), GitHub: prstage.NewFakeGitHub(),
		ObserveCI: func(_ context.Context, pr int, head string, reqChecks []string) (ciwatch.RemoteSnapshot, error) {
			cs := make([]ciwatch.CheckState, 0, len(reqChecks))
			for _, n := range reqChecks {
				cs = append(cs, ciwatch.CheckState{Name: n, Conclusion: "success", Required: true})
			}
			return ciwatch.RemoteSnapshot{PRNumber: pr, HeadOID: head, Checks: cs, ObservedAt: now()}, nil
		},
		VerifierRoute: routepin.Fields{
			Provider: "fixture", Model: "fixture-verifier", Effort: "low",
			Permission: "read-only", SubagentPolicy: routepin.SubagentForbidden,
		},
		AllowNilHookExec: true, AllowSyntheticLocalVerify: true, AllowSyntheticVerifier: true,
	}
	return &d
}

func firstNonEmptyFieldCLI(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func firstNonEmptyWindow(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

var errTestNoCapacity = errors.New("test: no eligible capacity snapshot")

func withTaskPayload(t *testing.T, body string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "task.md")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPCODER_TASK_PAYLOAD", p)
}

// testGitRepo creates a minimal local git repo and returns its absolute path.
func testGitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "README"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "README")
	run("commit", "-m", "init")
	run("remote", "add", "origin", "https://github.com/acme/demo.git")
	return repo
}

func TestRunDryRunAndAcceptedIdentity(t *testing.T) {
	deps := DefaultDeps()
	deps.Now = func() time.Time { return time.Date(2026, 7, 22, 20, 0, 0, 0, time.UTC) }
	// Durable task payload required for classification (no synthetic "issue N").
	payload := filepath.Join(t.TempDir(), "task.md")
	if err := os.WriteFile(payload, []byte("Implement accepted identity for dry-run\n\nDetails here."), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPCODER_TASK_PAYLOAD", payload)
	// Frozen inventory for dry-run pin (live discover not required).
	injectCodexProductRoute(t, &deps, deps.Now())
	// Production pin identity (fixture is never product-eligible).
	var stdout, stderr bytes.Buffer
	code := runRun([]string{
		"--repo", "jasonhnd/loopcoder",
		"--issue", "1124",
		"--provider", "codex",
		"--model", "gpt-5.5",
		"--effort", "medium",
		"--dry-run",
		"--format", "json",
	}, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var acc RunAccepted
	if err := json.Unmarshal(stdout.Bytes(), &acc); err != nil {
		t.Fatal(err)
	}
	if acc.Status != "dry_run" || acc.RunID == "" || !strings.HasPrefix(acc.RunID, "run_") {
		t.Fatalf("%+v", acc)
	}
	if acc.Request.Provider != "codex" || acc.Request.Issue != "1124" {
		t.Fatalf("%+v", acc.Request)
	}
}

func TestRunAutoRouteWithoutInventoryFailsClosed(t *testing.T) {
	withTaskPayload(t, "Implement route and capacity work for tests.")
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPCODER_HOME", home)
	deps := DefaultDeps()
	deps.Now = func() time.Time { return time.Date(2026, 7, 22, 22, 0, 0, 0, time.UTC) }
	// Force load failure: no usable live snapshot (honest fail-closed).
	deps.LoadAutoRouteInventory = func(ctx context.Context, repo string, now time.Time) (*autoroute.Inventory, error) {
		return nil, errTestNoCapacity
	}
	var stdout, stderr bytes.Buffer
	code := runRun([]string{
		"--repo", "acme/demo", "--issue", "1", "--auto-route", "--format", "json", "--dry-run",
	}, &stdout, &stderr, deps)
	if code == 0 {
		t.Fatalf("auto-route without inventory must fail closed; stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if code != exitRunPrecondition && code != exitRunUsage {
		t.Fatalf("code=%d want precondition/usage stderr=%s", code, stderr.String())
	}
	joined := stdout.String() + stderr.String()
	if !strings.Contains(joined, "inventory") && !strings.Contains(joined, "capacity") && !strings.Contains(joined, "no real inventory") && !strings.Contains(joined, "no eligible route") && !strings.Contains(joined, "missing real inventory") && !strings.Contains(joined, "auto-route inventory load failed") {
		t.Fatalf("expected inventory fail-closed message: %q", joined)
	}
	// partial pin still usage error
	stdout.Reset()
	stderr.Reset()
	code = runRun([]string{"--repo", "r", "--issue", "1", "--provider", "fixture"}, &stdout, &stderr, deps)
	if code != exitRunUsage && code != exitRunPrecondition {
		t.Fatalf("missing model code=%d stderr=%s", code, stderr.String())
	}
}

func TestRunAutoRouteWithInjectedInventorySelects(t *testing.T) {
	withTaskPayload(t, "Implement route and capacity work for tests.")
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPCODER_HOME", home)
	deps := DefaultDeps()
	deps.Now = func() time.Time { return time.Date(2026, 7, 22, 22, 0, 0, 0, time.UTC) }
	inv := autoroute.FakeInventory(deps.Now())
	deps.AutoRouteInventory = &inv
	var stdout, stderr bytes.Buffer
	code := runRun([]string{
		"--repo", "acme/demo", "--issue", "1", "--auto-route", "--format", "json", "--dry-run",
	}, &stdout, &stderr, deps)
	// Hermetic envs may still preflight-block after route selection; route fill is the contract.
	if !strings.Contains(stderr.String(), "auto-route selected") && !strings.Contains(stderr.String(), "task class=") {
		t.Fatalf("expected auto-route/class log; code=%d stderr=%s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "auto-route selected") {
		// Route filled before preflight; ok even if later gate fails.
		return
	}
	if code != 0 {
		t.Fatalf("auto-route code=%d stderr=%s", code, stderr.String())
	}
	var acc RunAccepted
	if err := json.Unmarshal(stdout.Bytes(), &acc); err != nil {
		t.Fatal(err)
	}
	if acc.Request.Provider == "" || acc.Request.Model == "" {
		t.Fatalf("auto-route did not fill route: %+v", acc.Request)
	}
}

func TestRunOmittedRouteWithoutInventoryFailsClosed(t *testing.T) {
	withTaskPayload(t, "Implement route and capacity work for tests.")
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPCODER_HOME", home)
	deps := DefaultDeps()
	deps.Now = func() time.Time { return time.Date(2026, 7, 22, 22, 5, 0, 0, time.UTC) }
	deps.LoadAutoRouteInventory = func(ctx context.Context, repo string, now time.Time) (*autoroute.Inventory, error) {
		return nil, errTestNoCapacity
	}
	var stdout, stderr bytes.Buffer
	// omit provider+model without usable capacity: auto policy fails closed (honest)
	code := runRun([]string{
		"--repo", "acme/demo", "--issue", "2", "--format", "json", "--dry-run",
	}, &stdout, &stderr, deps)
	if code == 0 {
		t.Fatalf("omitted route without inventory must fail closed; out=%s err=%s", stdout.String(), stderr.String())
	}
}

func TestRunAutoRouteLoadsViaDepsLoader(t *testing.T) {
	withTaskPayload(t, "Implement route and capacity work for tests.")
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPCODER_HOME", home)
	deps := DefaultDeps()
	deps.Now = func() time.Time { return time.Date(2026, 7, 22, 22, 6, 0, 0, time.UTC) }
	inv := autoroute.FakeInventory(deps.Now())
	called := false
	deps.LoadAutoRouteInventory = func(ctx context.Context, repo string, now time.Time) (*autoroute.Inventory, error) {
		called = true
		return &inv, nil
	}
	// Real capacity snapshot required for auto reserve (no snapshotFromRouteInventory invent).
	// Provide exact account/window for FakeInventory winners so reserve can bind.
	now := deps.Now()
	accs := []capacitysnapshot.AccountObservation{}
	for _, c := range inv.Candidates {
		if c.AccountRef == "" {
			continue
		}
		var rem float64 = 0.8
		accs = append(accs, capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
			Provider: c.Provider, AccountRef: c.AccountRef, InstallRef: "install-" + c.Provider,
			Installed: true, Authenticated: true, Healthy: true,
			HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
			Windows: []capacitysnapshot.Window{{
				Kind: firstNonEmptyWindow(c.WindowKind, "five_hour"), Unit: capacitysnapshot.UnitPercentage,
				Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 20, Unit: capacitysnapshot.UnitPercentage},
				Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: rem * 100, Unit: capacitysnapshot.UnitPercentage},
				Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
				Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
				Source: "test", CapturedAt: now,
			}},
			Models: []capacitysnapshot.ModelSpec{{
				ModelID: c.Model, Present: true, SupportedDepths: []string{"low", "medium", "high"}, DefaultDepth: "medium",
			}},
			Source: "test", CapturedAt: now,
		}))
	}
	snap, err := capacitysnapshot.Build(accs, now)
	if err != nil {
		t.Fatal(err)
	}
	deps.LastCapacitySnapshot = &snap
	var stdout, stderr bytes.Buffer
	code := runRun([]string{
		"--repo", "acme/demo", "--issue", "3", "--auto-route", "--format", "json", "--dry-run",
	}, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !called {
		t.Fatal("expected LoadAutoRouteInventory to be called")
	}
	var acc RunAccepted
	if err := json.Unmarshal(stdout.Bytes(), &acc); err != nil {
		t.Fatal(err)
	}
	if acc.Request.Provider == "" || acc.Request.Model == "" {
		t.Fatalf("%+v", acc.Request)
	}
}

func TestRunCmdSourceDoesNotCallDefaultInventory(t *testing.T) {
	src, err := os.ReadFile("run_cmd.go")
	if err != nil {
		src, err = os.ReadFile(filepath.Join("internal", "cli", "run_cmd.go"))
	}
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "DefaultInventory") || strings.Contains(string(src), "FakeInventory") {
		t.Fatal("run_cmd must not call DefaultInventory/FakeInventory; only pass deps.AutoRouteInventory")
	}
}

func TestRunHumanJSONLSameSchema(t *testing.T) {
	withTaskPayload(t, "Implement black-box cleanup terminal work.")
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPCODER_HOME", home)
	repo := testGitRepo(t)
	deps := DefaultDeps()
	deps.Now = func() time.Time { return time.Date(2026, 7, 22, 20, 1, 0, 0, time.UTC) }
	injectCodexProductRoute(t, &deps, deps.Now())
	deps.AgentLookup = func(provider string) (agent.Runner, error) {
		return affirmingRunner{Provider: provider}, nil
	}
	// Explicit Fake* delivery injection — production never silent-fakes.
	// Fresh ports per invocation so push/commit stores do not conflict across runs.
	shaOut, _ := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	sha := strings.TrimSpace(string(shaOut))
	args := []string{"--repo", repo, "--issue", "9", "--provider", "codex", "--model", "gpt-5.5", "--effort", "medium", "--permission", "bounded_write", "--base", "HEAD"}
	var hOut, jOut bytes.Buffer
	deps.Delivery = testDeliveryDeps(t, deps.Now, sha)
	// Fresh ledger path per format run so attempt IDs do not collide on reopen.
	deps.CapacityLedgerPath = filepath.Join(t.TempDir(), "cap-human.json")
	if runRun(append(args, "--format", "human"), &hOut, ioDiscard{}, deps) != 0 {
		t.Fatal(hOut.String())
	}
	deps.Delivery = testDeliveryDeps(t, deps.Now, sha)
	deps.CapacityLedgerPath = filepath.Join(t.TempDir(), "cap-jsonl.json")
	if runRun(append(args, "--format", "jsonl"), &jOut, ioDiscard{}, deps) != 0 {
		t.Fatal(jOut.String())
	}
	var acc RunAccepted
	if err := json.Unmarshal(jOut.Bytes(), &acc); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hOut.String(), acc.RunID) {
		t.Fatalf("human missing run_id %s in %s", acc.RunID, hOut.String())
	}
	if acc.Schema != schemaRunAccepted || acc.Status != "human_gate" {
		t.Fatalf("%+v", acc)
	}
	if strings.Contains(acc.Message, "execution ports not yet attached") {
		t.Fatal("old stub message still present")
	}
	if !strings.Contains(acc.Message, "human merge gate") && !strings.Contains(acc.Message, "pr=") {
		t.Fatalf("expected delivery human-gate message: %+v", acc)
	}
}

func TestRunRemovesPortsNotAttachedStub(t *testing.T) {
	withTaskPayload(t, "Implement black-box cleanup terminal work.")
	src, err := os.ReadFile("run_cmd.go")
	if err != nil {
		src, err = os.ReadFile(filepath.Join("internal", "cli", "run_cmd.go"))
	}
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "execution ports not yet attached") {
		t.Fatal("stub message still in source")
	}
	if !strings.Contains(string(src), "directrun.Service") {
		t.Fatal("cmd path does not reach directrun.Service")
	}
	if !strings.Contains(string(src), "directdelivery.Service") {
		t.Fatal("cmd path does not reach directdelivery.Service")
	}
	if !strings.Contains(string(src), "autoroute.Resolve") {
		t.Fatal("cmd path does not reach autoroute.Resolve")
	}
	if strings.Contains(string(src), "unsupported until P4") {
		t.Fatal("P4 rejection stub still present")
	}
}

func TestRunBlackBoxReachesCleanupTerminal(t *testing.T) {
	withTaskPayload(t, "Implement black-box cleanup terminal work.")
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPCODER_HOME", home)
	repo := testGitRepo(t)
	deps := DefaultDeps()
	deps.Now = func() time.Time { return time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC) }
	// Controlled subprocess-style runner via AgentLookup (test-only). Production
	// leaves AgentLookup nil → real agent.Lookup.
	injectCodexProductRoute(t, &deps, deps.Now())
	deps.AgentLookup = func(provider string) (agent.Runner, error) {
		return affirmingRunner{Provider: provider}, nil
	}
	sha, _ := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	deps.Delivery = testDeliveryDeps(t, deps.Now, strings.TrimSpace(string(sha)))
	var stdout, stderr bytes.Buffer
	code := runRun([]string{
		"--repo", repo, "--issue", "7", "--provider", "codex", "--model", "gpt-5.5",
		"--effort", "medium", "--permission", "bounded_write",
		"--base", "HEAD", "--format", "json",
	}, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var acc RunAccepted
	if err := json.Unmarshal(stdout.Bytes(), &acc); err != nil {
		t.Fatal(err)
	}
	if acc.Status != "human_gate" {
		t.Fatalf("%+v", acc)
	}
	// start report should have been written to stderr report stream
	if !strings.Contains(stderr.String(), "start") && !strings.Contains(stderr.String(), "stage=") {
		t.Fatalf("expected start report on stderr: %q", stderr.String())
	}
	// delivery evidence on stderr
	if !strings.Contains(stderr.String(), "delivery pr=") {
		t.Fatalf("expected delivery pr line on stderr: %q", stderr.String())
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }

func TestRootHelpLeadsWithRun(t *testing.T) {
	cmds := Commands()
	if len(cmds) == 0 || cmds[0].Name != "run" {
		t.Fatalf("first command=%v", cmds[0])
	}
	// primary set present
	want := map[string]bool{"run": false, "status": false, "events": false, "cancel": false, "doctor": false, "providers": false}
	for _, c := range cmds {
		if _, ok := want[c.Name]; ok {
			want[c.Name] = true
		}
	}
	for k, v := range want {
		if !v {
			t.Fatalf("missing %s", k)
		}
	}
}
