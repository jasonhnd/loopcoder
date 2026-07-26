package codexquota_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/codexquota"
)

func t0() time.Time { return time.Date(2026, 7, 23, 3, 0, 0, 0, time.UTC) }

func TestMultiWindowNormalize(t *testing.T) {
	used := 10.0
	rem := 90.0
	lim := 100.0
	raw, _ := json.Marshal([]codexquota.RawWindow{
		{Kind: "five_hour", Scope: "account", Used: &used, Remaining: &rem, Limit: &lim, Unit: "percent", ResetRFC3339: "2026-07-23T08:00:00Z", Source: "app_server"},
		{Kind: "weekly", Scope: "account", RemainingClass: "unlimited", Unit: "tokens", Source: "app_server"},
		{Kind: "credit", Scope: "account", LimitClass: "missing", Used: &used, Unit: "credits", Source: "local"},
	})
	snap, err := codexquota.ParseJSONFixture(raw, codexquota.ParseOptions{Now: t0(), Source: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Status != "ok" && snap.Status != "partial" {
		t.Fatalf("status=%s diags=%v", snap.Status, snap.Diagnostics)
	}
	if len(snap.Windows) != 3 {
		t.Fatalf("%+v", snap.Windows)
	}
	// five_hour finite
	var five codexquota.Window
	for _, w := range snap.Windows {
		if w.Kind == codexquota.WindowFiveHour {
			five = w
		}
	}
	if five.Limit.Class != codexquota.QtyFinite || five.ResetAt == nil {
		t.Fatalf("%+v", five)
	}
	if five.ResetAt.Location() != time.UTC {
		t.Fatal("reset not UTC")
	}
	// weekly unlimited remaining not zero
	for _, w := range snap.Windows {
		if w.Kind == codexquota.WindowWeekly {
			if w.Remaining.Class != codexquota.QtyUnlimited {
				t.Fatalf("%+v", w.Remaining)
			}
			if codexquota.IsNumericZero(w.Remaining) {
				t.Fatal("unlimited as zero")
			}
		}
		if w.Kind == codexquota.WindowCredit {
			if w.Limit.Class != codexquota.QtyMissing {
				t.Fatal(w.Limit)
			}
			// must not fabricate limit
			if w.Diagnostic != "limit_missing_not_fabricated" && w.Limit.Class == codexquota.QtyFinite {
				t.Fatal("fabricated limit")
			}
		}
	}
}

func TestMissingDistinctFromZero(t *testing.T) {
	raw, _ := json.Marshal([]codexquota.RawWindow{
		{Kind: "five_hour", Scope: "account", RemainingClass: "missing", Unit: "tokens"},
		{Kind: "weekly", Scope: "account", Remaining: floatPtr(0), Unit: "tokens"},
	})
	snap, err := codexquota.ParseJSONFixture(raw, codexquota.ParseOptions{Now: t0()})
	if err != nil {
		t.Fatal(err)
	}
	var miss, zero codexquota.Quantity
	for _, w := range snap.Windows {
		if w.Kind == codexquota.WindowFiveHour {
			miss = w.Remaining
		}
		if w.Kind == codexquota.WindowWeekly {
			zero = w.Remaining
		}
	}
	if miss.Class != codexquota.QtyMissing || codexquota.IsNumericZero(miss) {
		t.Fatalf("missing=%+v", miss)
	}
	if zero.Class != codexquota.QtyZero || !codexquota.IsNumericZero(zero) {
		t.Fatalf("zero=%+v", zero)
	}
}

func TestMalformedAndUnavailable(t *testing.T) {
	_, err := codexquota.ParseJSONFixture([]byte("not-json"), codexquota.ParseOptions{Now: t0()})
	if err == nil {
		t.Fatal("expected malformed")
	}
	snap, err := codexquota.ParseJSONFixture([]byte("[]"), codexquota.ParseOptions{Now: t0(), ForceStatus: "unavailable"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Status != "unavailable" {
		t.Fatal(snap.Status)
	}
}

func TestResetTimezoneAndBoundary(t *testing.T) {
	raw, _ := json.Marshal([]codexquota.RawWindow{
		{Kind: "five_hour", Scope: "account", ResetRFC3339: "2026-07-23T08:00:00-05:00", RemainingClass: "unknown"},
	})
	snap, err := codexquota.ParseJSONFixture(raw, codexquota.ParseOptions{Now: t0()})
	if err != nil {
		t.Fatal(err)
	}
	w := snap.Windows[0]
	if w.ResetAt == nil {
		t.Fatal("reset")
	}
	// -05:00 08:00 -> 13:00 UTC
	if w.ResetAt.Hour() != 13 {
		t.Fatalf("hour=%d utc=%s", w.ResetAt.Hour(), w.ResetAt)
	}
	if w.Remaining.Class != codexquota.QtyUnknown {
		t.Fatal(w.Remaining)
	}
	// bare small number rejected
	raw2, _ := json.Marshal([]codexquota.RawWindow{
		{Kind: "weekly", Scope: "account", ResetRFC3339: "12345"},
	})
	snap2, _ := codexquota.ParseJSONFixture(raw2, codexquota.ParseOptions{Now: t0()})
	// window may be skipped due to parse error
	if len(snap2.Windows) > 0 && snap2.Windows[0].ResetAt != nil {
		t.Fatal("accepted unsafe reset")
	}
}

func TestSecretRedaction(t *testing.T) {
	raw, _ := json.Marshal([]codexquota.RawWindow{
		{Kind: "credit", Scope: "account", AccountRef: "sk-abcsecret999", RemainingClass: "unknown"},
	})
	snap, err := codexquota.ParseJSONFixture(raw, codexquota.ParseOptions{Now: t0()})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Windows[0].AccountRef != "redacted" {
		t.Fatal(snap.Windows[0].AccountRef)
	}
}

func TestPartialSourceChange(t *testing.T) {
	raw, _ := json.Marshal([]codexquota.RawWindow{
		{Kind: "five_hour", Scope: "account", Remaining: floatPtr(1), Limit: floatPtr(10), Source: "a"},
		{Kind: "nope", Scope: "account"}, // bad kind skipped
	})
	snap, err := codexquota.ParseJSONFixture(raw, codexquota.ParseOptions{Now: t0()})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Status != "partial" || len(snap.Windows) != 1 {
		t.Fatalf("%+v", snap)
	}
}

func floatPtr(f float64) *float64 { return &f }
