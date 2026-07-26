package artifactqual

import (
	"encoding/json"
	"os"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
)

// writeSnapshotFile builds a dual-provider measured snapshot for exact-binary CRO probes.
// Explicit file path only — never silent DefaultInventory.
func writeSnapshotFile(path string, now, reset time.Time) error {
	mk := func(provider, model string, rem float64, r time.Time) capacitysnapshot.AccountObservation {
		return capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
			Provider: provider, AccountRef: "acct-" + provider, InstallRef: "install-" + provider,
			Installed: true, Authenticated: true, Healthy: true,
			HealthConfidence: capacitysnapshot.ConfidenceExact,
			HealthFreshness:  capacitysnapshot.FreshnessFresh,
			Source:           "cro_measure_snapshot", CapturedAt: now,
			Windows: []capacitysnapshot.Window{{
				Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
				Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100 - rem, Unit: capacitysnapshot.UnitPercentage},
				Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: rem, Unit: capacitysnapshot.UnitPercentage},
				Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
				ResetAt:    &r,
				Confidence: capacitysnapshot.ConfidenceEstimated,
				Freshness:  capacitysnapshot.FreshnessFresh,
				Source:     "cro_measure", CapturedAt: now,
			}},
			Models: []capacitysnapshot.ModelSpec{{
				ModelID: model, Present: true,
				SupportedDepths: []string{"low", "medium", "high"}, DefaultDepth: "medium",
				ClassHint: "tera",
			}},
		})
	}
	s, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{
		mk("codex", "gpt-5.5", 70, reset),
		mk("claude", "claude-sonnet-4-5", 40, reset.Add(2*time.Hour)),
	}, now)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// writeCROExhaustedSnapshot: codex remaining=0 + antigravity healthy — write must not pick codex.
func writeCROExhaustedSnapshot(path string) error {
	now := time.Now().UTC()
	reset := now.Add(time.Hour)
	codex := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "codex", AccountRef: "acct-codex", InstallRef: "install-codex",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Source: "cro_exhausted_snapshot", CapturedAt: now,
		Windows: []capacitysnapshot.Window{{
			Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
			Used:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Remaining: capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 0, Unit: capacitysnapshot.UnitPercentage},
			Limit:     capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			ResetAt:   &reset, Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
			Source: "cro_exhausted", CapturedAt: now,
		}},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "gpt-5.5", Present: true,
			SupportedDepths: []string{"low", "medium", "high"}, DefaultDepth: "medium", ClassHint: "tera",
		}},
	})
	ag := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "antigravity", AccountRef: "acct-ag", InstallRef: "install-ag",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Source: "cro_exhausted_snapshot", CapturedAt: now,
		Windows: []capacitysnapshot.Window{{
			Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
			Used:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 10, Unit: capacitysnapshot.UnitPercentage},
			Remaining: capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 90, Unit: capacitysnapshot.UnitPercentage},
			Limit:     capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			ResetAt:   &reset, Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
			Source: "cro_healthy", CapturedAt: now,
		}},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "GPT-OSS 120B", Present: true,
			SupportedDepths: []string{"low", "medium", "high"}, DefaultDepth: "medium", ClassHint: "tera",
		}},
	})
	s, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{codex, ag}, now)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
