package goalrun

import "testing"

func TestWindowKindExactNoProviderDefinedAlias(t *testing.T) {
	// Exact account+observed window only — no provider-defined↔five_hour alias.
	if windowKindCompatible("five_hour", "provider-defined") {
		t.Fatal("five_hour must NOT match provider-defined")
	}
	if !windowKindCompatible("five_hour", "five_hour") {
		t.Fatal("exact match")
	}
	if !windowKindCompatible("five_hour", "five-hour") {
		t.Fatal("spelling alias five-hour ↔ five_hour")
	}
	if windowKindCompatible("weekly", "five_hour") {
		t.Fatal("weekly must not match five_hour")
	}
	if windowKindCompatible("", "five_hour") {
		t.Fatal("empty want must not match")
	}
	if windowKindCompatible("five_hour", "") {
		t.Fatal("empty have must not match")
	}
	if windowKindCompatible("unknown", "unknown") {
		t.Fatal("unknown is not a known reservable fixed window")
	}
	if windowKindCompatible("provider-defined", "provider-defined") {
		t.Fatal("provider-defined is not a known reservable fixed window")
	}
}
