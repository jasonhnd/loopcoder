package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/agent"
	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/capclass"
	"github.com/jasonhnd/loopcoder/internal/eligibility"
	"github.com/jasonhnd/loopcoder/internal/preflight"
	"github.com/jasonhnd/loopcoder/internal/quotapolicy"
)

// TestPreflightErrorLeavesNoLiveReservation: preflight runs after route/pin bind
// and BEFORE capacity reserve. A preflight error must leave zero ledger entries
// and never launch a provider.
func TestPreflightErrorLeavesNoLiveReservation(t *testing.T) {
	withTaskPayload(t, "Implement preflight reserve safety for capacity holds.")
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPCODER_HOME", home)
	now := time.Date(2026, 7, 23, 16, 0, 0, 0, time.UTC)
	ledgerPath := filepath.Join(t.TempDir(), "run-cap.json")

	ok := func(id string) eligibility.Fact {
		return eligibility.Fact{State: eligibility.FactTrue, EvidenceID: id, Freshness: eligibility.FreshFresh}
	}
	ff := func(id string) eligibility.Fact {
		return eligibility.Fact{State: eligibility.FactFalse, EvidenceID: id, Freshness: eligibility.FreshFresh}
	}
	acct := "acct-codex-" + strings.Repeat("c", 48)
	cands := []eligibility.Candidate{}
	for _, effort := range []string{"low", "medium", "high"} {
		for _, perm := range []string{"read-only", "bounded_write", "default"} {
			cands = append(cands, eligibility.Candidate{
				Provider: "codex", Model: "gpt-5.5", Effort: effort, Permission: perm,
				ModelClass: capclass.ClassSoul, AccountRef: acct, InstallRef: "install-codex", WindowKind: "five_hour",
				Installed: ok("i"), Authenticated: ok("a"), ModelPresent: ok("m"),
				PermissionOK: ok("p"), EffortOK: ok("e"), Healthy: ok("h"),
				CooldownActive: ff("cd"), ResourceFit: ok("r"), QuotaRemaining: 9999,
			})
		}
	}
	rf, ttr, rel := 0.9, 2*time.Hour, 0.95
	inv := autoroute.Inventory{
		EvidenceDigest: "test-run-preflight-cap",
		Candidates:     cands,
		Soft: []quotapolicy.Candidate{{
			Provider: "codex", Model: "gpt-5.5", AccountRef: acct, InstallRef: "install-codex", WindowKind: "five_hour",
			Windows: []quotapolicy.Window{{
				Kind: quotapolicy.WindowFiveHour, RemainingFraction: &rf,
				Evidence: quotapolicy.EvidenceExact, TimeToReset: &ttr,
			}},
			Reliability: &rel, ReliabilityEvidence: quotapolicy.EvidenceExact,
		}},
		Machine: eligibility.MachineAdmission{CapacityOK: ok("mach"), ConcurrentSlots: 4},
	}
	acc := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "codex", AccountRef: acct, InstallRef: "install-codex",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Windows: []capacitysnapshot.Window{{
			Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
			Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 10, Unit: capacitysnapshot.UnitPercentage},
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 90, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
			ResetAt: func() *time.Time { t := now.Add(time.Hour); return &t }(), CapturedAt: now, Source: "test",
		}},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "gpt-5.5", SupportedDepths: []string{"low", "medium", "high"}, DefaultDepth: "medium", Present: true,
		}},
		Source: "test", CapturedAt: now,
	})
	snap, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{acc}, now)
	if err != nil {
		t.Fatal(err)
	}

	pfN, launchN := 0, 0
	deps := DefaultDeps()
	deps.Now = func() time.Time { return now }
	deps.AutoRouteInventory = &inv
	deps.LastCapacitySnapshot = &snap
	deps.CapacityLedgerPath = ledgerPath
	deps.PreflightEvaluate = func(ctx context.Context, in preflight.Input) (preflight.Snapshot, error) {
		pfN++
		if !strings.EqualFold(in.Provider, "codex") || !strings.EqualFold(in.Model, "gpt-5.5") {
			t.Fatalf("preflight after pin/route must see bound provider/model, got %q/%q", in.Provider, in.Model)
		}
		return preflight.Snapshot{}, errors.New("injected preflight hard failure")
	}
	deps.AgentLookup = func(provider string) (agent.Runner, error) {
		launchN++
		return nil, errors.New("AgentLookup must not run after preflight error")
	}

	repo := testGitRepo(t)
	var stdout, stderr bytes.Buffer
	// CLI flag is --base (not --base-branch).
	code := runRun([]string{
		"--repo", repo, "--issue", "1397",
		"--provider", "codex", "--model", "gpt-5.5", "--effort", "medium",
		"--permission", "bounded_write", "--format", "json",
		"--base", "HEAD",
	}, &stdout, &stderr, deps)
	if code == 0 {
		t.Fatalf("preflight error must reject; stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if pfN != 1 {
		t.Fatalf("PreflightEvaluate called %d times, want 1; stderr=%s", pfN, stderr.String())
	}
	if launchN != 0 {
		t.Fatalf("AgentLookup/provider launch count=%d want 0", launchN)
	}
	// Pin bind happens before preflight (non-dry-run explicit pin).
	if !strings.Contains(stderr.String(), "explicit pin bound") {
		t.Fatalf("expected inventory pin bind before preflight; stderr=%s", stderr.String())
	}

	// Ledger file absent or exactly zero entries (no reserve after preflight fail).
	b, rerr := os.ReadFile(ledgerPath)
	if rerr != nil && !os.IsNotExist(rerr) {
		t.Fatal(rerr)
	}
	if os.IsNotExist(rerr) || len(b) == 0 {
		// absent/empty — ok
	} else {
		var doc struct {
			Entries []capacityledger.Entry `json:"entries"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		if len(doc.Entries) != 0 {
			t.Fatalf("ledger must have exactly zero entries after preflight-before-reserve, got %d: %+v", len(doc.Entries), doc.Entries)
		}
	}

	joined := stdout.String() + stderr.String()
	if !strings.Contains(joined, "injected preflight hard failure") {
		t.Fatalf("must surface injected preflight hard failure, not flag/generic error: %q", joined)
	}
	if strings.Contains(joined, "flag provided but not defined") {
		t.Fatalf("flag parse error — wrong CLI flags: %q", joined)
	}
}
