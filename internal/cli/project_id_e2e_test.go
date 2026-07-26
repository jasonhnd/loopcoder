package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/agent"
	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/directrun"
	"github.com/jasonhnd/loopcoder/internal/registry"
	"github.com/jasonhnd/loopcoder/internal/runtimepath"
)

// TestRunRegisteredProjectID_EndToEndStoresSameID is a successful auto-route
// product run proving one registered ProjectID (≠ repo slug) binds route input,
// RunAccepted, capacity ledger, and directrun stage receipt. No optional or
// fallback assertions; no explicit pin (so auto-route Decision path runs).
func TestRunRegisteredProjectID_EndToEndStoresSameID(t *testing.T) {
	withTaskPayload(t, "Implement registered project identity end-to-end for capacity and receipts.")
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPCODER_HOME", home)

	repo := testGitRepo(t)
	ctx := context.Background()
	reg, err := registry.Register(ctx, registry.Options{RepoPath: repo}, registry.DefaultDeps())
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	wantID := strings.TrimSpace(reg.Project.ProjectID)
	if wantID == "" {
		t.Fatal("empty registered id")
	}
	slug := slugProjectFromRepo(repo)
	if wantID == slug {
		t.Fatalf("registered ProjectID must differ from repo slug for this proof: both %q", wantID)
	}
	if got := resolveCanonicalProjectID(repo, repo); got != wantID {
		t.Fatalf("resolver got %q want %q", got, wantID)
	}
	roots, err := runtimepath.Resolve(ctx, repo)
	if err != nil || !roots.Registered || roots.ProjectID != wantID {
		t.Fatalf("runtimepath: %+v %v", roots, err)
	}

	now := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)
	ledgerPath := filepath.Join(t.TempDir(), "reg-cap.json")
	deps := DefaultDeps()
	deps.Now = func() time.Time { return now }
	injectCodexProductRoute(t, &deps, now)
	deps.CapacityLedgerPath = ledgerPath

	// Capture exact autoroute.Input from the same run (no identity claims without capture).
	var capturedIn autoroute.Input
	var captureN int
	deps.RouteResolve = func(in autoroute.Input) (autoroute.Result, error) {
		captureN++
		capturedIn = in
		return autoroute.Resolve(in)
	}
	deps.AgentLookup = func(provider string) (agent.Runner, error) {
		return affirmingRunner{Provider: provider}, nil
	}
	shaOut, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimSpace(string(shaOut))
	deps.Delivery = testDeliveryDeps(t, deps.Now, sha)

	var stdout, stderr bytes.Buffer
	// Auto-route success path: no explicit provider/model pin.
	code := runRun([]string{
		"--repo", repo, "--issue", "1397",
		"--auto-route",
		"--permission", "bounded_write",
		"--format", "json",
		"--base", "HEAD",
	}, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("auto-route product run code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if captureN != 1 {
		t.Fatalf("RouteResolve called %d times want 1", captureN)
	}
	if !capturedIn.AutoRoute {
		t.Fatal("captured Input.AutoRoute must be true (no explicit pin)")
	}
	if capturedIn.ProjectID != wantID {
		t.Fatalf("captured autoroute.Input.ProjectID=%q want %q", capturedIn.ProjectID, wantID)
	}
	if strings.TrimSpace(capturedIn.DecisionKey) == "" {
		t.Fatal("captured autoroute.Input.DecisionKey must be nonempty")
	}
	if capturedIn.Provider != "" || capturedIn.Model != "" {
		t.Fatalf("auto-route input must not carry explicit pin: provider=%q model=%q",
			capturedIn.Provider, capturedIn.Model)
	}

	var acc RunAccepted
	if err := json.Unmarshal(stdout.Bytes(), &acc); err != nil {
		t.Fatalf("unmarshal RunAccepted: %v body=%s", err, stdout.String())
	}
	if acc.Status != "human_gate" {
		t.Fatalf("status=%q want human_gate msg=%q stderr=%s", acc.Status, acc.Message, stderr.String())
	}
	if acc.RunID == "" {
		t.Fatal("empty RunID")
	}
	if acc.ProjectID != wantID {
		t.Fatalf("RunAccepted.ProjectID=%q want %q", acc.ProjectID, wantID)
	}
	if acc.Capacity == nil {
		t.Fatal("RunAccepted.Capacity must be non-nil")
	}
	if acc.Capacity.ProjectID != wantID {
		t.Fatalf("Capacity.ProjectID=%q want %q", acc.Capacity.ProjectID, wantID)
	}
	if acc.PlanDigest == "" || acc.GraphDigest == "" || acc.TaskClass == "" || acc.ChildContractDigest == "" {
		t.Fatalf("accepted identity incomplete: plan=%q graph=%q class=%q ccd=%q",
			acc.PlanDigest, acc.GraphDigest, acc.TaskClass, acc.ChildContractDigest)
	}

	// Every ledger entry for this run uses registered ProjectID.
	raw, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("read ledger: %v stderr=%s", err, stderr.String())
	}
	var doc struct {
		Entries []capacityledger.Entry `json:"entries"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Entries) == 0 {
		t.Fatal("expected ledger entries after successful reserve")
	}
	for _, e := range doc.Entries {
		if e.ProjectID != wantID {
			t.Fatalf("ledger project_id=%q want registered %q (slug would be %q) entry=%+v",
				e.ProjectID, wantID, slug, e)
		}
		if e.RunID == acc.RunID && (e.PlanDigest == "" || e.GraphDigest == "" || e.ChildContractDigest == "") {
			t.Fatalf("ledger missing identity: %+v", e)
		}
	}

	// Stage receipt under registered ProjectID; absent under slug.
	rcpt, ok := directrun.LoadFullStageReceipt(home, wantID, acc.RunID)
	if !ok {
		t.Fatalf("LoadFullStageReceipt(home,%s,%s) missing", wantID, acc.RunID)
	}
	if rcpt.PlanDigest != acc.PlanDigest {
		t.Fatalf("receipt plan %q != accepted %q", rcpt.PlanDigest, acc.PlanDigest)
	}
	if rcpt.GraphDigest != acc.GraphDigest {
		t.Fatalf("receipt graph %q != accepted %q", rcpt.GraphDigest, acc.GraphDigest)
	}
	if rcpt.TaskClass != acc.TaskClass {
		t.Fatalf("receipt class %q != accepted %q", rcpt.TaskClass, acc.TaskClass)
	}
	if rcpt.ChildContractDigest != acc.ChildContractDigest {
		t.Fatalf("receipt ccd %q != accepted %q", rcpt.ChildContractDigest, acc.ChildContractDigest)
	}
	if _, slugOK := directrun.LoadFullStageReceipt(home, slug, acc.RunID); slugOK {
		t.Fatalf("LoadFullStageReceipt under slug %q must not exist", slug)
	}
}
