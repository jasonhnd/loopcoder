package providerinventory

import (
	"testing"
	"time"
)

func TestParseAntigravityCodexBarUsageWindows(t *testing.T) {
	now := time.Date(2026, 7, 22, 18, 30, 0, 0, time.UTC)
	raw := `[
  {
    "provider": "antigravity",
    "source": "app",
    "usage": {
      "extraRateWindows": [
        {
          "id": "antigravity-quota-summary-gemini-5h",
          "title": "Gemini 5-hour",
          "window": {
            "resetsAt": "2026-07-22T23:13:39Z",
            "usedPercent": 0.1043,
            "windowMinutes": 300
          }
        },
        {
          "id": "antigravity-quota-summary-gemini-weekly",
          "title": "Gemini weekly",
          "window": {
            "resetsAt": "2026-07-29T04:51:24Z",
            "usedPercent": 0.0241,
            "windowMinutes": 10080
          }
        }
      ],
      "primary": {
        "resetsAt": "2026-07-22T23:13:39Z",
        "usedPercent": 0.1043,
        "windowMinutes": 300
      },
      "updatedAt": "2026-07-22T18:30:06Z"
    }
  }
]`
	source := QuotaTelemetrySource{QuotaSourceID: "qsrc_test"}
	snaps, err := parseAntigravityCodexBarUsage(raw, source, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) < 2 {
		t.Fatalf("want ≥2 windows, got %d", len(snaps))
	}
	var five *QuotaSnapshot
	for i := range snaps {
		if snaps[i].ProviderQuantityName == "antigravity-quota-summary-gemini-5h" {
			five = &snaps[i]
			break
		}
	}
	if five == nil {
		t.Fatalf("missing 5h window: %+v", snaps)
	}
	if five.UsedValue == nil || *five.UsedValue != 10 {
		t.Fatalf("used want ~10%%, got %+v", five.UsedValue)
	}
	if five.RemainingValue == nil || *five.RemainingValue != 90 {
		t.Fatalf("remaining want ~90%% derived, got %+v", five.RemainingValue)
	}
	if five.Confidence != ConfidenceExact {
		t.Fatalf("confidence %s", five.Confidence)
	}
	if five.FieldConfidences["remaining_value"] != ConfidenceEstimated {
		t.Fatalf("remaining must be estimated derivation: %+v", five.FieldConfidences)
	}
	if five.ResetAt == "" {
		t.Fatal("missing reset_at")
	}
	if five.FreshnessState != FreshnessFresh {
		t.Fatalf("freshness %s", five.FreshnessState)
	}
}

func TestParseAntigravityMissingUsedPercentNotCapacity(t *testing.T) {
	now := time.Date(2026, 7, 22, 18, 30, 0, 0, time.UTC)
	raw := `[{"provider":"antigravity","usage":{"primary":{"resetsAt":"2026-07-22T23:13:39Z","windowMinutes":300},"updatedAt":"2026-07-22T18:30:06Z"}}]`
	snaps, err := parseAntigravityCodexBarUsage(raw, QuotaTelemetrySource{QuotaSourceID: "qsrc"}, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 0 {
		t.Fatalf("window without usedPercent must not become capacity: %+v", snaps)
	}
}

func TestParseAntigravityInvalidResetAndObservedAtDowngradesTruthfully(t *testing.T) {
	now := time.Date(2026, 7, 22, 18, 30, 0, 0, time.UTC)
	raw := `[{"provider":"antigravity","usage":{"primary":{"resetsAt":"not-rfc3339","usedPercent":1,"windowMinutes":300},"updatedAt":"also-invalid"}}]`
	snaps, err := parseAntigravityCodexBarUsage(raw, QuotaTelemetrySource{QuotaSourceID: "qsrc"}, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 {
		t.Fatalf("snapshots=%d", len(snaps))
	}
	got := snaps[0]
	if got.ResetAt != "" || got.ResetSemantics != ResetUnknown ||
		got.CapturedAt != formatTime(now) {
		t.Fatalf("invalid timestamps passed as live evidence: %#v", got)
	}
	for _, reason := range []string{"invalid-reset-at", "invalid-observed-at", "missing-exact-account-identity"} {
		if !containsString(got.GapReasons, reason) {
			t.Fatalf("missing gap %q: %#v", reason, got.GapReasons)
		}
	}
}

func TestParseAntigravityExhaustionIsTyped(t *testing.T) {
	now := time.Date(2026, 7, 22, 18, 30, 0, 0, time.UTC)
	raw := `[{"provider":"antigravity","usage":{"primary":{"resetsAt":"2026-07-22T23:13:39Z","usedPercent":100,"windowMinutes":300},"updatedAt":"2026-07-22T18:30:00Z"}}]`
	snaps, err := parseAntigravityCodexBarUsage(raw, QuotaTelemetrySource{QuotaSourceID: "qsrc"}, nil, now)
	if err != nil || len(snaps) != 1 {
		t.Fatalf("snapshots=%d err=%v", len(snaps), err)
	}
	got := snaps[0]
	if got.RemainingValue == nil || *got.RemainingValue != 0 ||
		got.TerminalErrorCode != "ErrQuotaExhausted" ||
		!containsString(got.GapReasons, "quota-exhausted") {
		t.Fatalf("exhaustion not typed: %#v", got)
	}
}
