package goalrun

import "testing"

func TestWindowKindCompatibleFiveHourAlias(t *testing.T) {
	if !windowKindCompatible("five_hour", "provider-defined") {
		t.Fatal("five_hour should match provider-defined")
	}
	if !windowKindCompatible("five_hour", "five_hour") {
		t.Fatal("exact match")
	}
	if windowKindCompatible("weekly", "five_hour") {
		t.Fatal("weekly must not match five_hour")
	}
}
