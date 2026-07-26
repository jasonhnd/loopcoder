package availability

import (
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

func deriveInventoryObservations(inventory providerinventory.Report, now time.Time) []Observation {
	var out []Observation
	usableInstallations := map[string]bool{}
	for _, installation := range inventory.Installations {
		if installation.InstallationState == providerinventory.InstallationInstalled && installation.UsableForInvocation == "yes" {
			usableInstallations[installation.ProviderInstallationID] = true
			usableInstallations[installation.AdapterID] = true
		}
	}
	for _, probe := range inventory.ProbeResults {
		scope := Scope{
			ProjectID:              ptrValue(probe.ProjectID),
			AdapterID:              probe.AdapterID,
			ProviderInstallationID: ptrValue(probe.ProviderInstallationID),
		}
		networkDeclared := probe.NetworkDeclared
		kind := ObservationProbeSuccess
		confidence := probe.Confidence
		var failure ReasonCode
		var reasons []ReasonCode
		gaps := append([]string(nil), probe.GapReasons...)
		if probe.Outcome != providerinventory.OutcomeInstalled {
			// Networked telemetry probes (quota/catalog) can flake without
			// meaning the installation is unusable for launch. Skip those
			// secondary failures when a usable installation already exists.
			if probe.NetworkDeclared && (usableInstallations[ptrValue(probe.ProviderInstallationID)] || usableInstallations[probe.AdapterID]) {
				continue
			}
			kind = ObservationProbeFailure
			failure = ReasonInstallationUnavailable
			reasons = append(reasons, ReasonInstallationUnavailable)
			if probe.TimedOut || probe.Killed || probe.Outcome == providerinventory.OutcomeProbeFailed {
				kind = ObservationTransportFailure
				failure = ReasonTransport
				reasons = append(reasons, ReasonTransport)
			}
			if probe.FreshnessState == providerinventory.FreshnessStale || probe.FreshnessState == providerinventory.FreshnessExpired || probe.Confidence == providerinventory.ConfidenceStale {
				reasons = append(reasons, ReasonStaleEvidence)
			}
		}
		out = append(out, normalizeObservation(Observation{
			ObservationKind:   kind,
			Scope:             scope,
			SourceRecordIDs:   []string{probe.ProbeResultID},
			ObservedAt:        firstNonEmpty(probe.CapturedAt, formatTime(now)),
			FailureClass:      failure,
			Confidence:        confidence,
			NetworkDeclared:   &networkDeclared,
			NetworkPermission: probe.NetworkPermission,
			ReasonCodes:       reasons,
			GapReasons:        gaps,
		}))
	}
	for _, readiness := range inventory.AuthReadiness {
		if readiness.ReadinessState == providerinventory.ReadinessReady && readiness.FreshnessState == providerinventory.FreshnessFresh && readiness.Confidence != providerinventory.ConfidenceStale {
			continue
		}
		scope := Scope{
			ProjectID:              ptrValue(readiness.ProjectID),
			AdapterID:              readiness.AdapterID,
			ProviderInstallationID: ptrValue(readiness.ProviderInstallationID),
			AccountProfileID:       ptrValue(readiness.AccountProfileID),
		}
		reasons := []ReasonCode{ReasonAuth}
		if readiness.FreshnessState == providerinventory.FreshnessStale || readiness.FreshnessState == providerinventory.FreshnessExpired || readiness.Confidence == providerinventory.ConfidenceStale {
			reasons = append(reasons, ReasonStaleEvidence)
		}
		out = append(out, normalizeObservation(Observation{
			ObservationKind: ObservationAuthFailure,
			Scope:           scope,
			SourceRecordIDs: []string{readiness.AuthReadinessID},
			ObservedAt:      firstNonEmpty(readiness.CapturedAt, formatTime(now)),
			FailureClass:    ReasonAuth,
			Confidence:      readiness.Confidence,
			ReasonCodes:     reasons,
			GapReasons:      readiness.GapReasons,
		}))
	}
	for _, capability := range inventory.ModelCapabilities {
		if capability.FreshnessState == providerinventory.FreshnessFresh &&
			capability.Confidence != providerinventory.ConfidenceStale &&
			capability.LifecycleState != providerinventory.LifecycleRemoved &&
			capability.AvailabilityState == providerinventory.AvailabilityAvailable {
			continue
		}
		scope := Scope{AdapterID: capability.AdapterID, ModelCapabilityID: capability.ModelCapabilityID, CanonicalModelID: capability.CanonicalModelID}
		reasons := []ReasonCode{ReasonModelUnavailable}
		if capability.FreshnessState == providerinventory.FreshnessStale || capability.FreshnessState == providerinventory.FreshnessExpired || capability.Confidence == providerinventory.ConfidenceStale {
			reasons = append(reasons, ReasonStaleEvidence)
		}
		out = append(out, normalizeObservation(Observation{
			ObservationKind: ObservationModelUnavailable,
			Scope:           scope,
			SourceRecordIDs: []string{capability.ModelCapabilityID},
			ObservedAt:      firstNonEmpty(capability.CapturedAt, formatTime(now)),
			FailureClass:    ReasonModelUnavailable,
			Confidence:      capability.Confidence,
			ReasonCodes:     reasons,
			GapReasons:      capability.GapReasons,
		}))
	}
	for _, snapshot := range inventory.QuotaSnapshots {
		scope := Scope{
			AdapterID:              snapshot.AdapterID,
			ProviderInstallationID: ptrValue(snapshot.ProviderInstallationID),
			AccountProfileID:       ptrValue(snapshot.AccountProfileID),
			ModelCapabilityID:      ptrValue(snapshot.ModelCapabilityID),
		}
		var kind ObservationKind
		var failure ReasonCode
		var reasons []ReasonCode
		switch {
		case strings.EqualFold(snapshot.TerminalErrorCode, "ErrRateLimited") || containsReason(snapshot.GapReasons, ReasonRateLimited429):
			kind = ObservationRateLimited
			failure = ReasonRateLimited429
			reasons = append(reasons, ReasonRateLimited429)
		case strings.EqualFold(snapshot.TerminalErrorCode, "ErrQuotaSnapshotMalformed") || containsReason(snapshot.GapReasons, ReasonMalformedResponse):
			if adapterHasRemainingCapacity(inventory.QuotaSnapshots, snapshot.AdapterID) {
				continue
			}
			kind = ObservationMalformedResponse
			failure = ReasonMalformedResponse
			reasons = append(reasons, ReasonMalformedResponse)
		case snapshot.Confidence == providerinventory.ConfidenceExact && snapshot.FreshnessState == providerinventory.FreshnessFresh && snapshot.RemainingValue != nil && *snapshot.RemainingValue <= 0:
			// Skip secondary empty windows when another window for the same
			// adapter still reports remaining capacity (e.g. credits=0 vs weekly %).
			if adapterHasRemainingCapacity(inventory.QuotaSnapshots, snapshot.AdapterID) {
				continue
			}
			kind = ObservationQuotaExhausted
			failure = ReasonQuotaExhausted
			reasons = append(reasons, ReasonQuotaExhausted)
		case snapshot.Confidence == providerinventory.ConfidenceStale || snapshot.FreshnessState == providerinventory.FreshnessStale || snapshot.FreshnessState == providerinventory.FreshnessExpired:
			kind = ObservationProbeFailure
			failure = ReasonStaleEvidence
			reasons = append(reasons, ReasonStaleEvidence)
		}
		if kind == "" {
			continue
		}
		out = append(out, normalizeObservation(Observation{
			ObservationKind: kind,
			Scope:           scope,
			SourceRecordIDs: []string{snapshot.QuotaSnapshotID},
			ObservedAt:      firstNonEmpty(snapshot.CapturedAt, formatTime(now)),
			FailureClass:    failure,
			Confidence:      snapshot.Confidence,
			ReasonCodes:     reasons,
			GapReasons:      snapshot.GapReasons,
		}))
	}
	return out
}

func adapterHasRemainingCapacity(snapshots []providerinventory.QuotaSnapshot, adapterID string) bool {
	for _, snapshot := range snapshots {
		if snapshot.AdapterID != adapterID {
			continue
		}
		if snapshot.Confidence != providerinventory.ConfidenceExact || snapshot.FreshnessState != providerinventory.FreshnessFresh {
			continue
		}
		if snapshot.RemainingValue != nil && *snapshot.RemainingValue > 0 {
			return true
		}
	}
	return false
}
