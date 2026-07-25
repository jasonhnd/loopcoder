package workflowrun_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun/testspawn"
)

// TestDefaultIntegrateLedger_NoRepoLocal_HomeNamespaced: production Service
// with nil Integrator must write the integrate ledger under durable HomeDir
// (project/run namespaced) and never create <repo>/.loopcoder.
func TestDefaultIntegrateLedger_NoRepoLocal_HomeNamespaced(t *testing.T) {
	home := testHome(t)
	repo := initGitRepo(t)
	projectID := "proj-default-int-ledger"
	runID := "run_default_int_ledger_1"
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: testspawn.Executor{HomeDir: home, Now: t0},
		// Integrator intentionally nil → production default LedgerDir.
	}
	res, err := svc.Execute(context.Background(), withExpectedPlanDigest(t, workflowrun.Request{
		ProjectID: projectID, RunID: runID,
		Definition: workflowrun.OneNodeDefinition("g-def-int", "impl"),
		Actor:      "owner", RepoPath: repo, BaseRef: "main",
		ChildRoutes: map[string]workflowrun.ChildRoute{
			"only": {Provider: "fixture", Model: "fixture-model", TaskClass: "tera", Depth: "medium",
				Permission: "bounded_write", AccountRef: "a", InstallRef: "i", WindowKind: "five_hour",
				ReservationID: "r", RouteReason: "pin"},
		},
	}))
	if err != nil {
		t.Fatalf("%v %+v", err, res)
	}
	if res.Status != workflowrun.StatusHumanGate {
		t.Fatalf("status=%s msg=%s", res.Status, res.Message)
	}
	if len(res.IntegrateCommits) < 1 {
		t.Fatalf("want integrate commits: %+v", res.IntegrateCommits)
	}
	// Item: no repo-local .loopcoder.
	if _, err := os.Lstat(filepath.Join(repo, ".loopcoder")); !os.IsNotExist(err) {
		t.Fatalf("<repo>/.loopcoder must not exist after default integrate: err=%v", err)
	}
	// Durable namespaced ledger under HomeDir.
	wantDir, err := workflowrun.DefaultIntegrateLedgerDir(home, projectID, runID)
	if err != nil {
		t.Fatal(err)
	}
	ledgerFile := filepath.Join(wantDir, "integrate-ledger.json")
	if _, err := os.Stat(ledgerFile); err != nil {
		t.Fatalf("durable integrate ledger missing at %s: %v", ledgerFile, err)
	}
	// Path must be under home and contain sanitized project/run.
	if !strings.HasPrefix(filepath.Clean(ledgerFile), filepath.Clean(home)) {
		t.Fatalf("ledger %s not under home %s", ledgerFile, home)
	}
	if !strings.Contains(ledgerFile, "projects") || !strings.Contains(ledgerFile, projectID) {
		t.Fatalf("ledger not project-namespaced: %s", ledgerFile)
	}
	if !strings.Contains(ledgerFile, runID) {
		t.Fatalf("ledger not run-namespaced: %s", ledgerFile)
	}
}

// TestDefaultIntegrateLedger_ResumeSameRunExactlyOnce_CrossRunIsolated:
// fresh Service for same project/run reads the durable ledger (exactly-once
// skip on re-integrate of same attempt); a different project/run cannot reuse it.
func TestDefaultIntegrateLedger_ResumeSameRunExactlyOnce_CrossRunIsolated(t *testing.T) {
	home := testHome(t)
	repo := initGitRepo(t)
	projectID := "proj-int-resume"
	runID := "run_int_resume_1"
	routes := map[string]workflowrun.ChildRoute{
		"only": {Provider: "fixture", Model: "fixture-model", TaskClass: "tera", Depth: "medium",
			Permission: "bounded_write", AccountRef: "a", InstallRef: "i", WindowKind: "five_hour",
			ReservationID: "r", RouteReason: "pin"},
	}
	svc1 := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: testspawn.Executor{HomeDir: home, Now: t0},
	}
	r1, err := svc1.Execute(context.Background(), withExpectedPlanDigest(t, workflowrun.Request{
		ProjectID: projectID, RunID: runID,
		Definition: workflowrun.OneNodeDefinition("g-int-res", "impl"),
		Actor:      "owner", RepoPath: repo, BaseRef: "main",
		ChildRoutes: routes,
	}))
	if err != nil {
		t.Fatalf("pass1: %v %+v", err, r1)
	}
	if len(r1.IntegrateCommits) < 1 || r1.IntegrateCommits[0].CommitSHA == "" {
		t.Fatalf("pass1 integrate missing: %+v", r1.IntegrateCommits)
	}
	sha1 := r1.IntegrateCommits[0].CommitSHA
	att1 := r1.IntegrateCommits[0].AttemptID
	if att1 == "" {
		att1 = r1.Children[0].AttemptID
	}
	ledgerDir, err := workflowrun.DefaultIntegrateLedgerDir(home, projectID, runID)
	if err != nil {
		t.Fatal(err)
	}
	ledgerFile := filepath.Join(ledgerDir, "integrate-ledger.json")
	raw1, err := os.ReadFile(ledgerFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw1), att1) && !strings.Contains(string(raw1), sha1) {
		t.Fatalf("ledger missing attempt/sha: att=%s sha=%s raw=%s", att1, sha1, raw1)
	}

	// Fresh Service invocation same project/run + PriorSucceeded: reuse, no re-exec,
	// integrate ledger still resolves (exactly-once).
	prior := map[string]workflowrun.ChildOutcome{}
	for _, c := range r1.Children {
		if c.Terminal == "succeeded" {
			prior[c.WorkItemID] = c
		}
	}
	calls2 := map[string]int{}
	svc2 := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: testspawn.Executor{HomeDir: home, Now: t0, Calls: calls2},
	}
	r2, err := svc2.Execute(context.Background(), withExpectedPlanDigest(t, workflowrun.Request{
		ProjectID: projectID, RunID: runID,
		Definition: workflowrun.OneNodeDefinition("g-int-res", "impl"),
		Actor:      "owner", RepoPath: repo, BaseRef: "main",
		PriorSucceeded: prior,
		ChildRoutes:    routes,
	}))
	if err != nil {
		t.Fatalf("resume: %v %+v", err, r2)
	}
	if calls2["only"] != 0 {
		t.Fatalf("provider re-exec on resume: %+v", calls2)
	}
	if r2.ReuseCount < 1 {
		t.Fatalf("reuse_count=%d", r2.ReuseCount)
	}
	// Ledger file unchanged path; still under home; no repo-local pollution.
	if _, err := os.Stat(ledgerFile); err != nil {
		t.Fatalf("resume lost durable ledger: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(repo, ".loopcoder")); !os.IsNotExist(err) {
		t.Fatalf("repo .loopcoder after resume: %v", err)
	}

	// Different project/run must not share or see the first ledger entries as its own.
	otherDir, err := workflowrun.DefaultIntegrateLedgerDir(home, "proj-int-other", "run_int_other_1")
	if err != nil {
		t.Fatal(err)
	}
	if otherDir == ledgerDir {
		t.Fatal("different project/run must not share integrate ledger dir")
	}
	otherFile := filepath.Join(otherDir, "integrate-ledger.json")
	if _, err := os.Stat(otherFile); err == nil {
		t.Fatalf("other run must not inherit ledger file at %s", otherFile)
	}
	// Escape containment: path traversal project_id must not leave home.
	evil, err := workflowrun.DefaultIntegrateLedgerDir(home, "../../../etc", "passwd")
	if err != nil {
		// fail-closed is OK
		return
	}
	if !strings.HasPrefix(filepath.Clean(evil), filepath.Clean(home)) {
		t.Fatalf("evil project_id escaped home: %s", evil)
	}
	if strings.Contains(evil, "/etc/") || strings.Contains(evil, "passwd") && !strings.Contains(evil, "projects") {
		// sanitized: "etc" segment may appear sanitized under projects — still under home
	}
	if _, err := os.Lstat(filepath.Join(repo, ".loopcoder")); !os.IsNotExist(err) {
		t.Fatalf("repo pollution after escape probe: %v", err)
	}
}

// TestDefaultIntegrateLedgerDir_ExactCollisionMatrixAndContainment proves
// collision-resistant exact identity (not lossy sanitize alone).
func TestDefaultIntegrateLedgerDir_ExactCollisionMatrixAndContainment(t *testing.T) {
	home := testHome(t)
	dirOf := func(proj, run string) string {
		t.Helper()
		d, err := workflowrun.DefaultIntegrateLedgerDir(home, proj, run)
		if err != nil {
			t.Fatalf("%q/%q: %v", proj, run, err)
		}
		if !strings.HasPrefix(filepath.Clean(d), filepath.Clean(home)) {
			t.Fatalf("escaped home: %s", d)
		}
		for _, seg := range strings.Split(filepath.Clean(d), string(filepath.Separator)) {
			if seg == ".." {
				t.Fatalf("parent segment: %s", d)
			}
		}
		return d
	}

	// a/b vs a-b must never share a dir.
	abSlash := dirOf("a/b", "run1")
	abDash := dirOf("a-b", "run1")
	if abSlash == abDash {
		t.Fatalf("a/b and a-b collided: %s", abSlash)
	}

	// Two >80-char IDs with identical 80-char sanitize prefix must differ.
	prefix := strings.Repeat("p", 90)
	longA := prefix + "AAAA"
	longB := prefix + "BBBB"
	dA := dirOf(longA, "r")
	dB := dirOf(longB, "r")
	if dA == dB {
		t.Fatalf("long IDs with same prefix collided:\n%s\n%s", dA, dB)
	}

	// Dot-like IDs: ".." vs another path-like id must be distinct and contained.
	dot := dirOf("..", "run")
	dotish := dirOf("....", "run")
	if dot == dotish {
		t.Fatalf("dot-like IDs collided: %s", dot)
	}

	// Project/run swaps must not share.
	sw1 := dirOf("alpha", "beta")
	sw2 := dirOf("beta", "alpha")
	if sw1 == sw2 {
		t.Fatalf("project/run swap collided: %s", sw1)
	}

	// Same exact pair → byte-identical path across fresh invocations.
	p1 := dirOf("proj-stable", "run-stable-1")
	p2 := dirOf("proj-stable", "run-stable-1")
	if p1 != p2 {
		t.Fatalf("same pair not identical:\n%s\n%s", p1, p2)
	}

	// Path traversal still contained under home.
	evil := dirOf("../../../etc", "passwd")
	if !strings.HasPrefix(filepath.Clean(evil), filepath.Clean(home)) {
		t.Fatalf("traversal escaped: %s", evil)
	}

	_, err := workflowrun.DefaultIntegrateLedgerDir(home, "", "run")
	if err == nil {
		t.Fatal("empty project_id must fail")
	}
	_, err = workflowrun.DefaultIntegrateLedgerDir(home, "proj", "")
	if err == nil {
		t.Fatal("empty run_id must fail")
	}
}
