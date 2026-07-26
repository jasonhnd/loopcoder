package providerinventory

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	QuotaTelemetrySourceSchema = "loopcoder.quota_telemetry_source.v1"
	QuotaSnapshotSchema        = "loopcoder.quota_snapshot.v1"
	QuotaUsageRefsSchema       = "loopcoder.quota_usage_refs.v1"
)

var (
	ErrQuotaSourceForbidden          = errors.New("ErrQuotaSourceForbidden")
	ErrQuotaSourceUnsupported        = errors.New("ErrQuotaSourceUnsupported")
	ErrTelemetryNetworkDenied        = errors.New("ErrTelemetryNetworkDenied")
	ErrQuotaConfidenceInsufficient   = errors.New("ErrQuotaConfidenceInsufficient")
	ErrQuotaSnapshotMalformed        = errors.New("ErrQuotaSnapshotMalformed")
	ErrQuotaSourceDisagreement       = errors.New("ErrQuotaSourceDisagreement")
	ErrQuotaCredentialMaterial       = errors.New("ErrQuotaCredentialMaterial")
	ErrQuotaCommandUndeclared        = errors.New("ErrQuotaCommandUndeclared")
	ErrQuotaNetworkDeclarationNeeded = errors.New("ErrQuotaNetworkDeclarationNeeded")
)

type QuotaSourceKind string

const (
	QuotaSourceOfficialAPI          QuotaSourceKind = "official-machine-readable-api"
	QuotaSourceOfficialCLICommand   QuotaSourceKind = "official-cli-machine-readable-command"
	QuotaSourceOfficialCLIError     QuotaSourceKind = "official-cli-status-or-error-class"
	QuotaSourceProviderExportFile   QuotaSourceKind = "provider-export-file"
	QuotaSourceLoopcoderLocalLedger QuotaSourceKind = "loopcoder-local-ledger"
	QuotaSourceOperatorOverlay      QuotaSourceKind = "operator-configured-policy-overlay"
	// QuotaSourceTrustedThirdPartyBridge is a third-party trusted bridge
	// (e.g. CodexBar) with explicit trust class/version/fingerprint.
	// Distinct from operator policy overlay — never exact confidence.
	QuotaSourceTrustedThirdPartyBridge QuotaSourceKind = "trusted-third-party-bridge"
	QuotaSourceFixture                 QuotaSourceKind = "fixture"
)

type QuantityKind string

const (
	QuantityInputTokens     QuantityKind = "input-tokens"
	QuantityOutputTokens    QuantityKind = "output-tokens"
	QuantityTotalTokens     QuantityKind = "total-tokens"
	QuantityRequests        QuantityKind = "requests"
	QuantityWallMS          QuantityKind = "wall-ms"
	QuantityConcurrency     QuantityKind = "concurrency"
	QuantityProviderDefined QuantityKind = "provider-defined"
	QuantityLocalPolicy     QuantityKind = "local-policy"
)

type WindowKind string

const (
	WindowFixedHour       WindowKind = "fixed-hour"
	WindowFixedDay        WindowKind = "fixed-day"
	WindowFixedWeek       WindowKind = "fixed-week"
	WindowRolling         WindowKind = "rolling"
	WindowProviderDefined WindowKind = "provider-defined"
	WindowUnbounded       WindowKind = "unbounded"
	WindowUnknown         WindowKind = "unknown"
)

type ResetSemantics string

const (
	ResetNone            ResetSemantics = "none"
	ResetProviderDefined ResetSemantics = "provider-defined"
	ResetWindowBoundary  ResetSemantics = "window-boundary"
	ResetRolling         ResetSemantics = "rolling"
	ResetUnknown         ResetSemantics = "unknown"
)

type QuotaTelemetrySource struct {
	SchemaVersion          string                `json:"schema_version"`
	RecordVersion          int                   `json:"record_version"`
	QuotaSourceID          string                `json:"quota_source_id"`
	AdapterID              string                `json:"adapter_id"`
	SourceKind             QuotaSourceKind       `json:"source_kind"`
	SourceKey              string                `json:"source_key"`
	SourceSchemaVersion    string                `json:"source_schema_version"`
	SupportedQuantities    []QuantityKind        `json:"supported_quantities"`
	SupportedWindows       []WindowKind          `json:"supported_windows"`
	ScopeDimensions        []string              `json:"scope_dimensions"`
	ConfidenceContract     map[string]Confidence `json:"confidence_contract"`
	NetworkDeclared        bool                  `json:"network_declared"`
	NetworkPermissionScope string                `json:"network_permission_scope,omitempty"`
	Argv                   []string              `json:"argv,omitempty"`
	EnvironmentKeys        []string              `json:"environment_keys"`
	TimeoutMS              int                   `json:"timeout_ms"`
	OutputLimits           OutputLimits          `json:"output_limits"`
	ClassificationRules    []string              `json:"classification_rules"`
	UnsupportedReason      string                `json:"unsupported_reason,omitempty"`
	CreatedAt              string                `json:"created_at"`
	UpdatedAt              string                `json:"updated_at"`
	PolicyVersion          string                `json:"policy_version"`
	GapReasons             []string              `json:"gap_reasons"`
}

type OutputLimits struct {
	StdoutBytes   int `json:"stdout_bytes"`
	StderrBytes   int `json:"stderr_bytes"`
	CombinedBytes int `json:"combined_bytes"`
	DecodedBytes  int `json:"decoded_bytes"`
}

type QuotaSnapshot struct {
	SchemaVersion          string                `json:"schema_version"`
	RecordVersion          int                   `json:"record_version"`
	QuotaSnapshotID        string                `json:"quota_snapshot_id"`
	QuotaSourceID          string                `json:"quota_source_id"`
	SourceKind             QuotaSourceKind       `json:"source_kind"`
	AdapterID              string                `json:"adapter_id,omitempty"`
	ProviderInstallationID *string               `json:"provider_installation_id,omitempty"`
	AccountProfileID       *string               `json:"account_profile_id,omitempty"`
	ModelCapabilityID      *string               `json:"model_capability_id,omitempty"`
	ScopeKey               string                `json:"scope_key"`
	QuantityKind           QuantityKind          `json:"quantity_kind"`
	ProviderQuantityName   string                `json:"provider_quantity_name,omitempty"`
	Unit                   string                `json:"unit"`
	WindowKind             WindowKind            `json:"window_kind"`
	WindowStart            string                `json:"window_start,omitempty"`
	WindowEnd              string                `json:"window_end,omitempty"`
	RollingDurationMS      int64                 `json:"rolling_duration_ms,omitempty"`
	ResetAt                string                `json:"reset_at,omitempty"`
	ResetSemantics         ResetSemantics        `json:"reset_semantics"`
	LimitValue             *int64                `json:"limit_value,omitempty"`
	UsedValue              *int64                `json:"used_value,omitempty"`
	RemainingValue         *int64                `json:"remaining_value,omitempty"`
	ReservedValue          *int64                `json:"reserved_value,omitempty"`
	ValueScale             int                   `json:"value_scale"`
	Confidence             Confidence            `json:"confidence"`
	FieldConfidences       map[string]Confidence `json:"field_confidences"`
	FreshnessState         FreshnessState        `json:"freshness_state"`
	CapturedAt             string                `json:"captured_at"`
	ValidUntil             string                `json:"valid_until,omitempty"`
	StaleAfter             string                `json:"stale_after,omitempty"`
	RawSourceHash          string                `json:"raw_source_hash,omitempty"`
	RedactedDiagnostics    string                `json:"redacted_diagnostics,omitempty"`
	ConflictSet            []string              `json:"conflict_set"`
	GapReasons             []string              `json:"gap_reasons"`
	TerminalErrorCode      string                `json:"terminal_error_code,omitempty"`
	CreatedAt              string                `json:"created_at"`
	UpdatedAt              string                `json:"updated_at"`
	PolicyVersion          string                `json:"policy_version"`
}

type QuotaUsageRefs struct {
	SchemaVersion         string     `json:"schema_version"`
	QuotaUsageFingerprint string     `json:"quota_usage_fingerprint"`
	QuotaSnapshotIDs      []string   `json:"quota_snapshot_ids"`
	UsageRecordIDs        []string   `json:"usage_record_ids"`
	BudgetPolicyIDs       []string   `json:"budget_policy_ids"`
	BudgetReservationIDs  []string   `json:"budget_reservation_ids"`
	AvailabilityScoreIDs  []string   `json:"availability_score_ids"`
	CircuitBreakerIDs     []string   `json:"circuit_breaker_ids"`
	Confidence            Confidence `json:"confidence"`
	HardIneligibleReasons []string   `json:"hard_ineligible_reasons"`
	GapReasons            []string   `json:"gap_reasons"`
}

func (k *QuotaSourceKind) UnmarshalJSON(data []byte) error {
	value, err := unmarshalEnumString(data, "source_kind")
	if err != nil {
		return err
	}
	switch QuotaSourceKind(value) {
	case QuotaSourceOfficialAPI, QuotaSourceOfficialCLICommand, QuotaSourceOfficialCLIError, QuotaSourceProviderExportFile, QuotaSourceLoopcoderLocalLedger, QuotaSourceOperatorOverlay, QuotaSourceTrustedThirdPartyBridge, QuotaSourceFixture:
		*k = QuotaSourceKind(value)
		return nil
	default:
		return fmt.Errorf("%w: unknown quota source kind %q", ErrInvalidRecord, value)
	}
}

func (k *QuantityKind) UnmarshalJSON(data []byte) error {
	value, err := unmarshalEnumString(data, "quantity_kind")
	if err != nil {
		return err
	}
	if !knownQuantityKind(QuantityKind(value)) {
		return fmt.Errorf("%w: unknown quantity_kind %q", ErrInvalidRecord, value)
	}
	*k = QuantityKind(value)
	return nil
}

func (k *WindowKind) UnmarshalJSON(data []byte) error {
	value, err := unmarshalEnumString(data, "window_kind")
	if err != nil {
		return err
	}
	if !knownWindowKind(WindowKind(value)) {
		return fmt.Errorf("%w: unknown window_kind %q", ErrInvalidRecord, value)
	}
	*k = WindowKind(value)
	return nil
}

func (s *ResetSemantics) UnmarshalJSON(data []byte) error {
	value, err := unmarshalEnumString(data, "reset_semantics")
	if err != nil {
		return err
	}
	switch ResetSemantics(value) {
	case ResetNone, ResetProviderDefined, ResetWindowBoundary, ResetRolling, ResetUnknown:
		*s = ResetSemantics(value)
		return nil
	default:
		return fmt.Errorf("%w: unknown reset_semantics %q", ErrInvalidRecord, value)
	}
}

func EmptyQuotaUsageRefs() QuotaUsageRefs {
	return QuotaUsageRefs{
		SchemaVersion:         QuotaUsageRefsSchema,
		QuotaUsageFingerprint: "sha256:" + hashHex("loopcoder.quota_usage_refs.v1", "empty"),
		QuotaSnapshotIDs:      []string{},
		UsageRecordIDs:        []string{},
		BudgetPolicyIDs:       []string{},
		BudgetReservationIDs:  []string{},
		AvailabilityScoreIDs:  []string{},
		CircuitBreakerIDs:     []string{},
		Confidence:            ConfidenceUnknown,
		HardIneligibleReasons: []string{},
		GapReasons:            []string{"quota-usage-not-bound-to-run"},
	}
}

func unsupportedQuotaTelemetryForAdapter(adapter AdapterDeclaration, now time.Time) (QuotaTelemetrySource, QuotaSnapshot) {
	return quotaTelemetryFallbackForAdapter(adapter, now, "unsupported-source", "ErrQuotaSourceUnsupported")
}

func quotaTelemetryFallbackForAdapter(adapter AdapterDeclaration, now time.Time, reason, terminal string) (QuotaTelemetrySource, QuotaSnapshot) {
	now = now.UTC()
	sourceKind := QuotaSourceOfficialCLIError
	sourceKey := "unsupported-quota-v1"
	sourceSchema := "loopcoder.unsupported_quota.v1"
	unsupportedReason := "adapter declares no supported official machine-readable quota telemetry source"
	sourceGaps := []string{"unsupported-source"}
	supportedQuantities := []QuantityKind{QuantityProviderDefined}
	supportedWindows := []WindowKind{WindowUnknown}
	scopeDimensions := []string{"provider"}
	timeoutMS := int((1 * time.Millisecond).Milliseconds())
	outputLimits := defaultQuotaOutputLimits()
	environmentKeys := []string{}
	classificationRules := []string{"no-raw-provider-output", "no-credential-material", "redacted-diagnostics-only"}
	if adapter.AdapterID == "codex" {
		sourceKind = QuotaSourceOfficialCLICommand
		sourceKey = "codex-app-server-rate-limits-v1"
		sourceSchema = codexQuotaSourceSchema
		unsupportedReason = ""
		sourceGaps = []string{"not-collected"}
		supportedQuantities = []QuantityKind{QuantityRequests, QuantityProviderDefined}
		supportedWindows = []WindowKind{WindowRolling, WindowFixedWeek, WindowProviderDefined, WindowUnbounded, WindowUnknown}
		scopeDimensions = []string{"provider", "account", "scope"}
		timeoutMS = int(codexQuotaTimeout / time.Millisecond)
		outputLimits = OutputLimits{StdoutBytes: codexQuotaOutputBytes, StderrBytes: StdoutLimitBytes, CombinedBytes: codexQuotaOutputBytes + StdoutLimitBytes, DecodedBytes: codexQuotaOutputBytes}
		environmentKeys = allowedProbeEnvKeys()
		classificationRules = []string{"json-rpc-field-allowlist", "redact-before-truncate", "no-credential-material", "no-login-update-or-provider-work"}
	}
	if adapter.AdapterID == "claude" {
		sourceKind = QuotaSourceOfficialCLIError
		sourceKey = "claude-usage-rendered-status-v1"
		sourceSchema = claudeQuotaSourceSchema
		unsupportedReason = ""
		sourceGaps = []string{"not-collected"}
		supportedQuantities = []QuantityKind{QuantityProviderDefined}
		supportedWindows = []WindowKind{WindowFixedWeek, WindowProviderDefined, WindowUnknown}
		scopeDimensions = []string{"provider", "account", "scope"}
		timeoutMS = int(claudeQuotaTimeout / time.Millisecond)
		outputLimits = OutputLimits{StdoutBytes: claudeQuotaOutputBytes, StderrBytes: StderrLimitBytes, CombinedBytes: claudeQuotaOutputBytes + StderrLimitBytes, DecodedBytes: claudeQuotaOutputBytes}
		environmentKeys = claudeQuotaEnvKeys()
		classificationRules = []string{"rendered-status-allowlist", "ansi-normalized", "redact-before-truncate", "no-credential-material", "no-login-update-or-provider-work"}
	}
	if strings.TrimSpace(reason) == "" {
		reason = "not-collected"
	}
	if strings.TrimSpace(terminal) == "" {
		terminal = "ErrQuotaSourceUnsupported"
	}
	source := normalizeQuotaTelemetrySource(QuotaTelemetrySource{
		AdapterID:           adapter.AdapterID,
		SourceKind:          sourceKind,
		SourceKey:           sourceKey,
		SourceSchemaVersion: sourceSchema,
		SupportedQuantities: supportedQuantities,
		SupportedWindows:    supportedWindows,
		ScopeDimensions:     scopeDimensions,
		ConfidenceContract:  map[string]Confidence{"limit_value": ConfidenceUnavailable, "used_value": ConfidenceUnavailable, "remaining_value": ConfidenceUnavailable, "reset_at": ConfidenceUnavailable},
		NetworkDeclared:     adapter.AdapterID == "codex" || adapter.AdapterID == "claude",
		NetworkPermissionScope: func() string {
			if adapter.AdapterID == "codex" {
				return "provider:codex/action:quota-read/side-effect:read/freshness:interactive"
			}
			if adapter.AdapterID == "claude" {
				return "provider:claude/action:quota-read/side-effect:read/freshness:interactive"
			}
			return ""
		}(),
		Argv: func() []string {
			if adapter.AdapterID == "codex" {
				return []string{"codex", "-s", "read-only", "-a", "untrusted", "app-server"}
			}
			if adapter.AdapterID == "claude" {
				return claudeQuotaSourceArgv()
			}
			return nil
		}(),
		EnvironmentKeys:     environmentKeys,
		TimeoutMS:           timeoutMS,
		OutputLimits:        outputLimits,
		ClassificationRules: classificationRules,
		UnsupportedReason:   unsupportedReason,
		CreatedAt:           formatTime(now),
		UpdatedAt:           formatTime(now),
		PolicyVersion:       PolicyVersion,
		GapReasons:          sourceGaps,
	})
	snapshot := normalizeQuotaSnapshot(QuotaSnapshot{
		QuotaSnapshotID:      quotaSnapshotID(adapter.AdapterID, source.QuotaSourceID, reason, formatTime(now)),
		QuotaSourceID:        source.QuotaSourceID,
		SourceKind:           source.SourceKind,
		AdapterID:            adapter.AdapterID,
		ScopeKey:             "provider:" + adapter.AdapterID,
		QuantityKind:         QuantityProviderDefined,
		ProviderQuantityName: "quota",
		Unit:                 "provider-defined",
		WindowKind:           WindowUnknown,
		ResetSemantics:       ResetUnknown,
		ValueScale:           0,
		Confidence:           ConfidenceUnavailable,
		FieldConfidences: map[string]Confidence{
			"limit_value":     ConfidenceUnavailable,
			"used_value":      ConfidenceUnavailable,
			"remaining_value": ConfidenceUnavailable,
			"reset_at":        ConfidenceUnavailable,
		},
		FreshnessState:      FreshnessNotApplicable,
		CapturedAt:          formatTime(now),
		RedactedDiagnostics: "quota telemetry not collected for " + adapter.AdapterID + " due to " + reason,
		ConflictSet:         []string{},
		GapReasons:          []string{reason, "not-collected"},
		TerminalErrorCode:   terminal,
		CreatedAt:           formatTime(now),
		UpdatedAt:           formatTime(now),
		PolicyVersion:       PolicyVersion,
	})
	return source, snapshot
}

func ValidateQuotaTelemetrySource(source QuotaTelemetrySource) error {
	source = normalizeQuotaTelemetrySource(source)
	if strings.TrimSpace(source.UnsupportedReason) != "" {
		if source.NetworkDeclared && strings.TrimSpace(source.NetworkPermissionScope) == "" {
			return fmt.Errorf("%w: network_permission_scope is required when network_declared is true", ErrQuotaNetworkDeclarationNeeded)
		}
		return nil
	}
	if !safeAdapterKey(source.AdapterID) {
		return fmt.Errorf("%w: adapter_id must be a non-empty safe provider key", ErrQuotaSourceUnsupported)
	}
	if !knownQuotaSourceKind(source.SourceKind) {
		return fmt.Errorf("%w: source_kind %q is not allowlisted", ErrQuotaSourceForbidden, source.SourceKind)
	}
	if strings.TrimSpace(source.SourceKey) == "" || strings.ContainsAny(source.SourceKey, ";&|`$<>") {
		return fmt.Errorf("%w: source_key must be stable and non-shell", ErrQuotaSourceForbidden)
	}
	if source.NetworkDeclared && strings.TrimSpace(source.NetworkPermissionScope) == "" {
		return fmt.Errorf("%w: network_permission_scope is required when network_declared is true", ErrQuotaNetworkDeclarationNeeded)
	}
	if (source.SourceKind == QuotaSourceOfficialCLICommand || source.SourceKind == QuotaSourceOfficialCLIError) && len(source.Argv) == 0 {
		return fmt.Errorf("%w: official CLI quota sources must declare fixed argv", ErrQuotaCommandUndeclared)
	}
	for _, violation := range validateFixedArgv("quota_argv", source.Argv, true) {
		return fmt.Errorf("%w: %s", ErrQuotaSourceForbidden, violation)
	}
	for index, arg := range source.Argv {
		if strings.ContainsAny(arg, ";&|`$<>") {
			return fmt.Errorf("%w: quota_argv[%d] must not contain shell metacharacters", ErrQuotaSourceForbidden, index)
		}
	}
	for _, key := range source.EnvironmentKeys {
		if strings.TrimSpace(key) == "" || strings.Contains(key, "=") || probeEnvNameDenied(key) {
			return fmt.Errorf("%w: environment key %q is empty, value-shaped, or credential-shaped", ErrQuotaCredentialMaterial, key)
		}
	}
	if source.TimeoutMS <= 0 || source.OutputLimits.StdoutBytes < 0 || source.OutputLimits.StderrBytes < 0 || source.OutputLimits.CombinedBytes < 0 {
		return fmt.Errorf("%w: quota source must bound timeout and output", ErrInvalidRecord)
	}
	if len(source.SupportedQuantities) == 0 || len(source.SupportedWindows) == 0 || len(source.ScopeDimensions) == 0 || len(source.ClassificationRules) == 0 {
		return fmt.Errorf("%w: quota source must declare quantities, windows, scopes, and classification rules", ErrInvalidRecord)
	}
	for _, kind := range source.SupportedQuantities {
		if !knownQuantityKind(kind) {
			return fmt.Errorf("%w: unknown quantity %q", ErrInvalidRecord, kind)
		}
	}
	for _, kind := range source.SupportedWindows {
		if !knownWindowKind(kind) {
			return fmt.Errorf("%w: unknown window %q", ErrInvalidRecord, kind)
		}
	}
	for field, confidence := range source.ConfidenceContract {
		if strings.TrimSpace(field) == "" {
			return fmt.Errorf("%w: confidence contract contains empty field", ErrInvalidRecord)
		}
		if !confidenceAllowedForSource(source.SourceKind, confidence) {
			return fmt.Errorf("%w: confidence %q exceeds source kind %q contract", ErrQuotaConfidenceInsufficient, confidence, source.SourceKind)
		}
	}
	return nil
}

func ValidateQuotaSnapshot(source QuotaTelemetrySource, snapshot QuotaSnapshot) error {
	source = normalizeQuotaTelemetrySource(source)
	snapshot = normalizeQuotaSnapshot(snapshot)
	if strings.TrimSpace(snapshot.QuotaSnapshotID) == "" || !strings.HasPrefix(snapshot.QuotaSnapshotID, "qsnap_") {
		return fmt.Errorf("%w: quota_snapshot_id must use qsnap_ prefix", ErrQuotaSnapshotMalformed)
	}
	if snapshot.QuotaSourceID != source.QuotaSourceID {
		return fmt.Errorf("%w: quota_source_id does not match source", ErrQuotaSnapshotMalformed)
	}
	if snapshot.SourceKind != source.SourceKind {
		return fmt.Errorf("%w: source_kind does not match source", ErrQuotaSnapshotMalformed)
	}
	if !knownQuantityKind(snapshot.QuantityKind) || !containsQuantity(source.SupportedQuantities, snapshot.QuantityKind) {
		return fmt.Errorf("%w: quantity_kind %q is not supported by source", ErrQuotaSnapshotMalformed, snapshot.QuantityKind)
	}
	if !knownWindowKind(snapshot.WindowKind) || !containsWindow(source.SupportedWindows, snapshot.WindowKind) {
		return fmt.Errorf("%w: window_kind %q is not supported by source", ErrQuotaSnapshotMalformed, snapshot.WindowKind)
	}
	if snapshot.WindowKind == WindowRolling && snapshot.RollingDurationMS <= 0 {
		return fmt.Errorf("%w: rolling windows require rolling_duration_ms", ErrQuotaSnapshotMalformed)
	}
	if (snapshot.WindowKind == WindowFixedHour || snapshot.WindowKind == WindowFixedDay || snapshot.WindowKind == WindowFixedWeek) && (snapshot.WindowStart == "" || snapshot.WindowEnd == "") {
		return fmt.Errorf("%w: fixed windows require window_start and window_end", ErrQuotaSnapshotMalformed)
	}
	if strings.TrimSpace(snapshot.ResetAt) != "" && snapshot.ResetSemantics == ResetUnknown {
		return fmt.Errorf("%w: reset_at requires non-unknown reset_semantics", ErrQuotaSnapshotMalformed)
	}
	if strings.TrimSpace(snapshot.ResetAt) == "" && snapshot.ResetSemantics == ResetProviderDefined {
		return fmt.Errorf("%w: provider-defined reset requires reset_at", ErrQuotaSnapshotMalformed)
	}
	if snapshot.ValueScale < 0 || strings.TrimSpace(snapshot.Unit) == "" || strings.TrimSpace(snapshot.ScopeKey) == "" {
		return fmt.Errorf("%w: snapshot requires unit, non-negative value_scale, and scope_key", ErrQuotaSnapshotMalformed)
	}
	if !confidenceAllowedForSource(source.SourceKind, snapshot.Confidence) {
		return fmt.Errorf("%w: snapshot confidence %q exceeds source kind %q contract", ErrQuotaConfidenceInsufficient, snapshot.Confidence, source.SourceKind)
	}
	if source.NetworkDeclared && containsString(snapshot.GapReasons, "network-denied") {
		return fmt.Errorf("%w: network-declared source was not granted", ErrTelemetryNetworkDenied)
	}
	if secretLike(snapshot.RedactedDiagnostics) {
		return fmt.Errorf("%w: redacted_diagnostics still looks credential-shaped", ErrQuotaCredentialMaterial)
	}
	return nil
}

func LinkQuotaConflicts(snapshots []QuotaSnapshot) []QuotaSnapshot {
	out := append([]QuotaSnapshot(nil), snapshots...)
	groups := map[string][]int{}
	for i, snapshot := range out {
		key := strings.Join([]string{
			snapshot.AdapterID,
			ptrValue(snapshot.AccountProfileID),
			ptrValue(snapshot.ModelCapabilityID),
			snapshot.ScopeKey,
			string(snapshot.QuantityKind),
			string(snapshot.WindowKind),
			snapshot.WindowStart,
			snapshot.WindowEnd,
			strconv.FormatInt(snapshot.RollingDurationMS, 10),
		}, "\x00")
		groups[key] = append(groups[key], i)
	}
	for _, indexes := range groups {
		if len(indexes) < 2 {
			continue
		}
		if !quotaSnapshotsDisagree(out, indexes) {
			continue
		}
		ids := make([]string, 0, len(indexes))
		for _, index := range indexes {
			ids = append(ids, out[index].QuotaSnapshotID)
		}
		sort.Strings(ids)
		for _, index := range indexes {
			out[index].ConflictSet = idsWithout(ids, out[index].QuotaSnapshotID)
			out[index].GapReasons = dedupeStrings(append(out[index].GapReasons, "provider-disagreement"))
		}
	}
	return out
}

func markQuotaFreshness(snapshots []QuotaSnapshot, now time.Time) []QuotaSnapshot {
	out := append([]QuotaSnapshot(nil), snapshots...)
	for i := range out {
		if out[i].FreshnessState == FreshnessNotApplicable {
			continue
		}
		if quotaExpired(out[i].ValidUntil, now) || quotaExpired(out[i].StaleAfter, now) {
			out[i].FreshnessState = FreshnessStale
			out[i].Confidence = ConfidenceStale
			out[i].GapReasons = dedupeStrings(append(out[i].GapReasons, "stale-cache"))
		}
	}
	return out
}

func normalizeQuotaTelemetrySource(source QuotaTelemetrySource) QuotaTelemetrySource {
	source.AdapterID = strings.TrimSpace(source.AdapterID)
	if source.SchemaVersion == "" {
		source.SchemaVersion = QuotaTelemetrySourceSchema
	}
	if source.RecordVersion == 0 {
		source.RecordVersion = 1
	}
	if source.SourceSchemaVersion == "" {
		source.SourceSchemaVersion = "loopcoder.quota_source.v1"
	}
	if source.SourceKind == "" {
		source.SourceKind = QuotaSourceFixture
	}
	if source.SourceKey == "" {
		source.SourceKey = "quota-source"
	}
	if source.QuotaSourceID == "" {
		source.QuotaSourceID = quotaSourceID(source.AdapterID, string(source.SourceKind), source.SourceKey, source.SourceSchemaVersion)
	}
	if source.TimeoutMS == 0 {
		source.TimeoutMS = int((5 * time.Second).Milliseconds())
	}
	if source.OutputLimits == (OutputLimits{}) {
		source.OutputLimits = defaultQuotaOutputLimits()
	}
	if source.ConfidenceContract == nil {
		source.ConfidenceContract = map[string]Confidence{}
	}
	if source.EnvironmentKeys == nil {
		source.EnvironmentKeys = []string{}
	}
	source.SupportedQuantities = dedupeQuantities(source.SupportedQuantities)
	source.SupportedWindows = dedupeWindows(source.SupportedWindows)
	source.ScopeDimensions = dedupeStrings(source.ScopeDimensions)
	source.ClassificationRules = dedupeStrings(source.ClassificationRules)
	source.GapReasons = dedupeStrings(source.GapReasons)
	return source
}

func normalizeQuotaSnapshot(snapshot QuotaSnapshot) QuotaSnapshot {
	if snapshot.SchemaVersion == "" {
		snapshot.SchemaVersion = QuotaSnapshotSchema
	}
	if snapshot.RecordVersion == 0 {
		snapshot.RecordVersion = 1
	}
	if snapshot.WindowKind == "" {
		snapshot.WindowKind = WindowUnknown
	}
	if snapshot.QuantityKind == "" {
		snapshot.QuantityKind = QuantityProviderDefined
	}
	if snapshot.Unit == "" {
		snapshot.Unit = unitForQuantity(snapshot.QuantityKind, snapshot.ProviderQuantityName)
	}
	if snapshot.ResetSemantics == "" {
		snapshot.ResetSemantics = ResetUnknown
	}
	if snapshot.ValueScale < 0 {
		snapshot.ValueScale = 0
	}
	if snapshot.Confidence == "" {
		snapshot.Confidence = ConfidenceUnknown
	}
	if snapshot.FreshnessState == "" {
		snapshot.FreshnessState = FreshnessFresh
	}
	if snapshot.FieldConfidences == nil {
		snapshot.FieldConfidences = map[string]Confidence{}
	}
	if snapshot.ConflictSet == nil {
		snapshot.ConflictSet = []string{}
	}
	snapshot.ConflictSet = dedupeStrings(snapshot.ConflictSet)
	snapshot.GapReasons = dedupeStrings(snapshot.GapReasons)
	if snapshot.PolicyVersion == "" {
		snapshot.PolicyVersion = PolicyVersion
	}
	return snapshot
}

func defaultQuotaOutputLimits() OutputLimits {
	return OutputLimits{
		StdoutBytes:   StdoutLimitBytes,
		StderrBytes:   StderrLimitBytes,
		CombinedBytes: CombinedLimitBytes,
		DecodedBytes:  CombinedLimitBytes,
	}
}

func quotaSourceID(adapterID, sourceKind, sourceKey, sourceSchemaVersion string) string {
	return "qsrc_" + hashBase32(adapterID, sourceKind, sourceKey, sourceSchemaVersion)[:32]
}

func quotaSnapshotID(parts ...string) string {
	return "qsnap_" + hashBase32(parts...)[:26]
}

func rawSourceHash(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func quantityValues(snapshot QuotaSnapshot) []any {
	return []any{snapshot.LimitValue, snapshot.UsedValue, snapshot.RemainingValue, snapshot.ReservedValue, snapshot.ResetAt, snapshot.ResetSemantics}
}

func quotaSnapshotsDisagree(snapshots []QuotaSnapshot, indexes []int) bool {
	first := quantityValues(snapshots[indexes[0]])
	for _, index := range indexes[1:] {
		if !reflect.DeepEqual(first, quantityValues(snapshots[index])) {
			return true
		}
	}
	return false
}

func quotaExpired(value string, now time.Time) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}
	cutoff, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return true
	}
	return now.UTC().After(cutoff)
}

func knownQuotaSourceKind(kind QuotaSourceKind) bool {
	switch kind {
	case QuotaSourceOfficialAPI, QuotaSourceOfficialCLICommand, QuotaSourceOfficialCLIError, QuotaSourceProviderExportFile, QuotaSourceLoopcoderLocalLedger, QuotaSourceOperatorOverlay, QuotaSourceTrustedThirdPartyBridge, QuotaSourceFixture:
		return true
	default:
		return false
	}
}

func knownQuantityKind(kind QuantityKind) bool {
	switch kind {
	case QuantityInputTokens, QuantityOutputTokens, QuantityTotalTokens, QuantityRequests, QuantityWallMS, QuantityConcurrency, QuantityProviderDefined, QuantityLocalPolicy:
		return true
	default:
		return false
	}
}

func knownWindowKind(kind WindowKind) bool {
	switch kind {
	case WindowFixedHour, WindowFixedDay, WindowFixedWeek, WindowRolling, WindowProviderDefined, WindowUnbounded, WindowUnknown:
		return true
	default:
		return false
	}
}

func confidenceAllowedForSource(kind QuotaSourceKind, confidence Confidence) bool {
	switch confidence {
	case ConfidenceExact:
		// Exact is reserved for official machine-readable sources — never
		// third-party bridges or operator policy overlays.
		return kind != QuotaSourceOperatorOverlay && kind != QuotaSourceTrustedThirdPartyBridge
	case ConfidenceEstimated, ConfidenceUnknown, ConfidenceUnavailable, ConfidenceStale:
		return true
	default:
		return false
	}
}

func unitForQuantity(kind QuantityKind, providerName string) string {
	switch kind {
	case QuantityInputTokens, QuantityOutputTokens, QuantityTotalTokens:
		return "token"
	case QuantityRequests:
		return "request"
	case QuantityWallMS:
		return "millisecond"
	case QuantityConcurrency:
		return "slot"
	case QuantityLocalPolicy:
		return "policy"
	case QuantityProviderDefined:
		if strings.TrimSpace(providerName) != "" {
			return strings.TrimSpace(providerName)
		}
		return "provider-defined"
	default:
		return "unknown"
	}
}

func containsQuantity(values []QuantityKind, want QuantityKind) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsWindow(values []WindowKind, want WindowKind) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func dedupeQuantities(values []QuantityKind) []QuantityKind {
	seen := map[QuantityKind]bool{}
	out := make([]QuantityKind, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func dedupeWindows(values []WindowKind) []WindowKind {
	seen := map[WindowKind]bool{}
	out := make([]WindowKind, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func idsWithout(values []string, skip string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == skip {
			continue
		}
		out = append(out, value)
	}
	return out
}

func ptrValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
