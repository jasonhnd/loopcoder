package kickback

import (
	"strings"
	"testing"
)

func TestParseItemCanonicalizesPRForms(t *testing.T) {
	for _, input := range []string{"#101", "101", "pr:101", "PR#101", "pr-101", "pr:#101", "pr: #101"} {
		got := ParseItem(input)
		if got.PRNumber != 101 || got.Canonical != "#101" || got.SHA != "" {
			t.Fatalf("ParseItem(%q) = %#v, want PR #101", input, got)
		}
	}
}

func TestParseItemTreatsLongHexLikeBareNumericAsSHA(t *testing.T) {
	for _, input := range []string{"abcdef0", "ABCDEF0", "1234567"} {
		got := ParseItem(input)
		if got.PRNumber != 0 || got.Canonical != strings.ToLower(input) || got.SHA != strings.ToLower(input) {
			t.Fatalf("ParseItem(%q) = %#v, want SHA canonicalization", input, got)
		}
	}
}
