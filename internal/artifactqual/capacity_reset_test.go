package artifactqual_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/artifactqual"
)

func TestCanary_CapacityReset_FixedWindowMissingFails(t *testing.T) {
	now := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	ev, dig, sha := baseValidCanary(now)
	// Strip reset from fixed-window children → cannot green CapacityAfterOK.
	for i := range ev.Children {
		ev.Children[i].ResetAt = nil
	}
	for i := range ev.ProviderObservations {
		ev.ProviderObservations[i].ResetAt = nil
	}
	ev.ContentDigest = artifactqual.DigestCanaryBody(ev)
	v := artifactqual.ValidateCanaryEvidence(ev, dig, sha, now)
	if v.CapacityAfterOK {
		t.Fatal("fixed-window missing reset must not CapacityAfterOK")
	}
	joined := strings.Join(v.Reasons, ";")
	if !strings.Contains(joined, "capacity_reset_at_missing") {
		t.Fatalf("want capacity_reset_at_missing, got %v", v.Reasons)
	}
	// Reasons must not embed credentials.
	if strings.Contains(joined, "secret") || strings.Contains(joined, "token") {
		t.Fatalf("credential-like text in reasons: %v", v.Reasons)
	}
}

func TestCanary_CapacityReset_StaleVsObservationFails(t *testing.T) {
	now := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	ev, dig, sha := baseValidCanary(now)
	// Reset before observation times → stale/expired.
	stale := now.Add(-3 * time.Hour)
	for i := range ev.Children {
		ev.Children[i].ResetAt = &stale
	}
	for i := range ev.ProviderObservations {
		ev.ProviderObservations[i].ResetAt = &stale
	}
	ev.ContentDigest = artifactqual.DigestCanaryBody(ev)
	v := artifactqual.ValidateCanaryEvidence(ev, dig, sha, now)
	if v.CapacityAfterOK {
		t.Fatal("stale reset must not CapacityAfterOK")
	}
	joined := strings.Join(v.Reasons, ";")
	if !strings.Contains(joined, "capacity_reset_at_stale_vs_observation") {
		t.Fatalf("want stale reason, got %v", v.Reasons)
	}
}

func TestCanary_CapacityReset_ValidFuturePasses(t *testing.T) {
	now := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	ev, dig, sha := baseValidCanary(now)
	// baseValidCanary children already have future ResetAt from child().
	// Ensure provider obs also have future reset (baseValidCanary fixture).
	for i := range ev.ProviderObservations {
		if ev.ProviderObservations[i].ResetAt == nil {
			r := now.Add(2 * time.Hour)
			ev.ProviderObservations[i].ResetAt = &r
		}
	}
	// Full PR binding for Valid path optional; CapacityAfterOK can be true with valid capacity alone.
	ev.ContentDigest = artifactqual.DigestCanaryBody(ev)
	v := artifactqual.ValidateCanaryEvidence(ev, dig, sha, now)
	// May be invalid for PR/other reasons in base fixture — CapacityAfterOK is the gate.
	if !v.CapacityAfterOK {
		t.Fatalf("want CapacityAfterOK with valid future reset: %v", v.Reasons)
	}
}

func TestCanary_CapacityReset_UnboundedDoesNotRequireReset(t *testing.T) {
	now := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	ev, dig, sha := baseValidCanary(now)
	for i := range ev.Children {
		ev.Children[i].WindowKind = "unbounded"
		ev.Children[i].ResetAt = nil
	}
	for i := range ev.ProviderObservations {
		ev.ProviderObservations[i].WindowKind = "unbounded"
		ev.ProviderObservations[i].ResetAt = nil
	}
	ev.ContentDigest = artifactqual.DigestCanaryBody(ev)
	v := artifactqual.ValidateCanaryEvidence(ev, dig, sha, now)
	joined := strings.Join(v.Reasons, ";")
	if strings.Contains(joined, "capacity_reset_at_missing") {
		t.Fatalf("unbounded must not require reset: %v", v.Reasons)
	}
}
