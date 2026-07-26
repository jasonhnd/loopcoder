package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/capclass"
	"github.com/jasonhnd/loopcoder/internal/eligibility"
	"github.com/jasonhnd/loopcoder/internal/preflight"
	"github.com/jasonhnd/loopcoder/internal/quotapolicy"
)

// injectCodexProductRoute freezes codex/gpt-5.5 inventory + capacity snapshot for
// product-path CLI tests (fixture is never production-eligible).
func injectCodexProductRoute(t *testing.T, deps *Deps, now time.Time) {
	t.Helper()
	ok := func(id string) eligibility.Fact {
		return eligibility.Fact{State: eligibility.FactTrue, EvidenceID: id, Freshness: eligibility.FreshFresh}
	}
	ff := func(id string) eligibility.Fact {
		return eligibility.Fact{State: eligibility.FactFalse, EvidenceID: id, Freshness: eligibility.FreshFresh}
	}
	acct := "acct-codex-" + strings.Repeat("c", 48)
	var cands []eligibility.Candidate
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
		EvidenceDigest: "test-cli-codex-product",
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
			ResetAt: func() *time.Time { tt := now.Add(time.Hour); return &tt }(), CapturedAt: now, Source: "test",
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
	deps.AutoRouteInventory = &inv
	deps.LastCapacitySnapshot = &snap
	deps.PreflightEvaluate = func(ctx context.Context, in preflight.Input) (preflight.Snapshot, error) {
		return preflight.Snapshot{
			Schema: preflight.SchemaSnapshot, Decision: preflight.StatusPass, AllowLaunch: true,
			Digest: "test-preflight-ok", GeneratedAt: now,
		}, nil
	}
	deps.CapacityLedgerPath = filepath.Join(t.TempDir(), "cli-cap.json")
}
