package providerinventory

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/models"
	"github.com/jasonhnd/loopcoder/internal/providerinstall"
	"github.com/jasonhnd/loopcoder/internal/runtimecap"
)

const (
	ClaudeCapabilityProbeReceiptSchema = "loopcoder.claude_capability_probe_receipt.v1"
	claudeCapabilityProbeFreshness     = 30 * time.Minute
	// The CLI admits an explicitly bounded probe timeout up to five minutes;
	// allow that provider call plus bounded auth overhead while preserving the
	// required pre-auth-before-execution ordering.
	claudeCapabilityProbeAuthLeadMax = 6 * time.Minute
)

var (
	ErrClaudeAuthBindingMalformed = errors.New("ErrClaudeAuthBindingMalformed")
	ErrClaudeCatalogProbeInvalid  = errors.New("ErrClaudeCatalogProbeInvalid")
)

// ClaudeAuthBinding is the credential-blind identity derived from the exact
// Claude executable's immediately preceding `auth status --json` observation.
// It never retains provider principals, email addresses, organization names,
// tokens, or raw provider output.
type ClaudeAuthBinding struct {
	ProviderInstallationID string     `json:"provider_installation_id"`
	AccountProfileID       string     `json:"account_profile_id"`
	AuthReadinessID        string     `json:"auth_readiness_id"`
	ObservedAt             string     `json:"observed_at"`
	RawSHA256              string     `json:"raw_sha256"`
	Confidence             Confidence `json:"confidence"`
	// SafeRecords are carried in-process so the immediately preceding auth
	// observation is persisted with the receipt. They contain only the normal
	// redacted providerinventory records and are omitted from command output.
	SafeProfile   AccountProfile `json:"-"`
	SafeReadiness AuthReadiness  `json:"-"`
}

// ClaudeCapabilityProbeReceipt is the durable, allowlisted receipt for one
// paid bounded capability probe. Raw prompt/result/session/principal material
// is intentionally absent.
type ClaudeCapabilityProbeReceipt struct {
	SchemaVersion          string         `json:"schema_version"`
	Provider               string         `json:"provider"`
	RequestedModel         string         `json:"requested_model"`
	ActualModel            string         `json:"actual_model"`
	AcceptedEffort         string         `json:"accepted_effort"`
	ProviderInstallationID string         `json:"provider_installation_id"`
	AccountProfileID       string         `json:"account_profile_id"`
	AuthReadinessID        string         `json:"auth_readiness_id"`
	AuthObservedAt         string         `json:"auth_observed_at"`
	ExecutedAt             string         `json:"executed_at"`
	ExpiresAt              string         `json:"expires_at"`
	AuthRawSHA256          string         `json:"auth_raw_sha256"`
	OutputRawSHA256        string         `json:"output_raw_sha256"`
	ArgvDigest             string         `json:"argv_digest"`
	InputTokens            int64          `json:"input_tokens"`
	OutputTokens           int64          `json:"output_tokens"`
	CacheReadInputTokens   int64          `json:"cache_read_input_tokens"`
	CacheCreateInputTokens int64          `json:"cache_creation_input_tokens"`
	TotalTokens            int64          `json:"total_tokens"`
	CostUSDMicros          int64          `json:"cost_usd_micros"`
	BudgetReservationID    string         `json:"budget_reservation_id"`
	ReservedTokens         int64          `json:"reserved_tokens"`
	CommittedTokens        int64          `json:"committed_tokens"`
	ReleasedTokens         int64          `json:"released_tokens"`
	BudgetState            string         `json:"budget_state"`
	UsageRecordIDs         []string       `json:"usage_record_ids"`
	Source                 string         `json:"source"`
	Confidence             Confidence     `json:"confidence"`
	FreshnessState         FreshnessState `json:"freshness_state"`
	GapReasons             []string       `json:"gap_reasons"`
}

// ParseClaudeAuthBinding applies the exact same account identity algorithm as
// inventory discovery to one raw machine-readable auth observation.
func ParseClaudeAuthBinding(executablePath string, output []byte, exitCode int, now time.Time) (ClaudeAuthBinding, error) {
	installID, err := providerinstall.ComputeInstallationID("claude", executablePath)
	if err != nil {
		return ClaudeAuthBinding{}, fmt.Errorf("%w: %v", ErrClaudeAuthBindingMalformed, err)
	}
	parsed := parseClaudeAuthStatus(string(output), exitCode)
	if exitCode != 0 || len(parsed) != 1 {
		return ClaudeAuthBinding{}, fmt.Errorf("%w: expected exactly one successful account profile, got exit=%d profiles=%d", ErrClaudeAuthBindingMalformed, exitCode, len(parsed))
	}
	adapter := AdapterDeclaration{AdapterID: "claude", DisplayName: "Claude", Vendor: "Anthropic"}
	profiles, readiness := authRecordsFromParsed(adapter, installID, parsed, now.UTC())
	if len(profiles) != 1 || len(readiness) != 1 || !ExactFreshReadyAuth(readiness[0]) {
		return ClaudeAuthBinding{}, fmt.Errorf("%w: auth observation is not exact, fresh, and ready", ErrClaudeAuthBindingMalformed)
	}
	if readiness[0].AccountProfileID == nil || strings.TrimSpace(*readiness[0].AccountProfileID) == "" {
		return ClaudeAuthBinding{}, fmt.Errorf("%w: account profile identity unavailable", ErrClaudeAuthBindingMalformed)
	}
	sum := sha256.Sum256(output)
	return ClaudeAuthBinding{
		ProviderInstallationID: installID,
		AccountProfileID:       *readiness[0].AccountProfileID,
		AuthReadinessID:        readiness[0].AuthReadinessID,
		ObservedAt:             formatTime(now.UTC()),
		RawSHA256:              "sha256:" + hex.EncodeToString(sum[:]),
		Confidence:             ConfidenceExact,
		SafeProfile:            profiles[0],
		SafeReadiness:          readiness[0],
	}, nil
}

// ClaudeCatalogCandidate reports whether a requested model is an
// adapter-declared full ID or alias. It does not claim availability.
func ClaudeCatalogCandidate(requested string) (models.Model, bool) {
	requested = strings.TrimSpace(requested)
	provider, ok := models.LookupProvider("claude")
	if !ok || requested == "" {
		return models.Model{}, false
	}
	for _, model := range provider.Models {
		if requested == model.Name {
			return model, true
		}
		for _, alias := range model.Aliases {
			if requested == alias {
				return model, true
			}
		}
	}
	return models.Model{}, false
}

// ApplyClaudeVerifiedSubset adds one successful, account-bound invocation as
// a fresh provider-machine-readable catalog snapshot. Adapter-declared rows
// remain hints and never satisfy this receipt gate.
func ApplyClaudeVerifiedSubset(report Report, receipt ClaudeCapabilityProbeReceipt, now time.Time) (Report, error) {
	now = now.UTC()
	if err := validateClaudeCapabilityProbeReceipt(receipt, now); err != nil {
		return Report{}, err
	}
	if !reportHasClaudeBinding(report, receipt.ProviderInstallationID, receipt.AccountProfileID) {
		return Report{}, fmt.Errorf("%w: receipt does not match an exact fresh inventory installation/account binding", ErrClaudeCatalogProbeInvalid)
	}
	provider, ok := runtimecap.LookupProvider("claude")
	if !ok {
		return Report{}, fmt.Errorf("%w: claude runtime contract unavailable", ErrClaudeCatalogProbeInvalid)
	}
	adapter := declarationFromRuntime(provider)
	sourceRef := "claude-capability-probe#" + receipt.OutputRawSHA256
	source := CatalogSourceInput{
		Kind:                CatalogSourceProviderMachineReadable,
		Reference:           sourceRef,
		SourceSchemaVersion: ClaudeCapabilityProbeReceiptSchema,
		Precedence:          1000,
		Confidence:          ConfidenceExact,
		FreshnessState:      FreshnessFresh,
		Entries: []CatalogInputEntry{{
			CanonicalModelID:    receipt.ActualModel,
			DisplayName:         receipt.ActualModel,
			Aliases:             []string{receipt.RequestedModel},
			LifecycleState:      LifecycleAvailable,
			AvailabilityState:   AvailabilityAvailable,
			RolesSupported:      []CatalogRole{CatalogRoleWorker, CatalogRoleVerifier, CatalogRoleAuditReview},
			ReadOnly:            CapabilityTrue,
			JSONOutput:          CapabilityTrue,
			NestedSubagents:     CapabilityUnknown,
			MCPConfig:           CapabilityTrue,
			Cancellation:        CapabilityTrue,
			TokenUsageReporting: CapabilityTrue,
			ImageInput:          CapabilityUnknown,
			ImageOutput:         CapabilityUnknown,
			Constraints: []string{
				"account-bound-verified-subset",
				"paid-bounded-capability-probe",
				"verified-effort:" + receipt.AcceptedEffort,
				"supported_depth=" + receipt.AcceptedEffort,
				"default_depth=" + receipt.AcceptedEffort,
			},
		}},
	}
	installID := receipt.ProviderInstallationID
	snapshot, capabilities, err := buildCatalogSnapshot(adapter, &installID, []CatalogSourceInput{source}, now)
	if err != nil {
		return Report{}, err
	}
	accountID := receipt.AccountProfileID
	authID := receipt.AuthReadinessID
	snapshot.AccountProfileID = &accountID
	snapshot.AuthReadinessID = &authID
	snapshot.StalePolicy = "paid-capability-probe-30m"
	snapshot.StaleAfter = receipt.ExpiresAt
	snapshot.SideEffectClass = "provider-paid-bounded-read"
	snapshot.Classification = "account-bound-provider-capability-receipt"
	snapshot.Source = SourceDescriptor{Kind: string(CatalogSourceProviderMachineReadable), AdapterID: "claude", ProbeCommandID: "claude-code-verified-subset-v1", ExecutableName: "claude"}
	snapshot.Evidence = EvidenceSummary{Kind: "account-bound-paid-capability-probe", CommandBounded: true, NoShell: true, RepositoryMutation: false, SecretMaterialRetained: false}
	copyReceipt := receipt
	copyReceipt.UsageRecordIDs = append([]string(nil), receipt.UsageRecordIDs...)
	copyReceipt.GapReasons = append([]string(nil), receipt.GapReasons...)
	snapshot.CapabilityProbeReceipt = &copyReceipt
	for i := range capabilities {
		capabilities[i].StaleAfter = receipt.ExpiresAt
		capabilities[i].SideEffectClass = snapshot.SideEffectClass
		capabilities[i].Classification = snapshot.Classification
		capabilities[i].Source = snapshot.Source
		capabilities[i].Evidence = snapshot.Evidence
	}
	snapshot.InventoryFingerprint, err = catalogFingerprint(snapshot, capabilities)
	if err != nil {
		return Report{}, err
	}
	report.ModelCatalogSnapshots = append(report.ModelCatalogSnapshots, snapshot)
	report.ModelCapabilities = append(report.ModelCapabilities, capabilities...)
	report.InventoryFingerprint, err = fingerprint(report)
	if err != nil {
		return Report{}, err
	}
	return report, nil
}

// ApplyClaudeUnavailableSubset persists a safe failed-candidate observation.
// It intentionally creates no ModelCapability row, so failures can never
// become routes.
func ApplyClaudeUnavailableSubset(report Report, binding ClaudeAuthBinding, requestedModel, effort, terminal string, now time.Time) (Report, error) {
	now = now.UTC()
	if _, ok := ClaudeCatalogCandidate(requestedModel); !ok ||
		!validClaudeEffort(effort) ||
		!strings.HasPrefix(binding.ProviderInstallationID, "pinst_") ||
		!strings.HasPrefix(binding.AccountProfileID, "acct-") ||
		strings.TrimSpace(binding.AuthReadinessID) == "" ||
		binding.Confidence != ConfidenceExact ||
		!exactSHA256(binding.RawSHA256) ||
		!safeAdapterKey(strings.TrimPrefix(strings.TrimSpace(terminal), "Err")) {
		return Report{}, fmt.Errorf("%w: invalid unavailable probe evidence", ErrClaudeCatalogProbeInvalid)
	}
	observedAt, err := time.Parse(time.RFC3339Nano, binding.ObservedAt)
	if err != nil || observedAt.After(now) || now.Sub(observedAt) > claudeCapabilityProbeAuthLeadMax {
		return Report{}, fmt.Errorf("%w: unavailable probe auth binding is not immediate", ErrClaudeCatalogProbeInvalid)
	}
	provider, ok := runtimecap.LookupProvider("claude")
	if !ok {
		return Report{}, fmt.Errorf("%w: claude runtime contract unavailable", ErrClaudeCatalogProbeInvalid)
	}
	adapter := declarationFromRuntime(provider)
	reference := "claude-capability-probe-unavailable#sha256:" + hashHex(
		"claude-capability-probe-unavailable-v1",
		binding.ProviderInstallationID,
		binding.AccountProfileID,
		requestedModel,
		effort,
		terminal,
		binding.RawSHA256,
		binding.ObservedAt,
		formatTime(now),
	)
	source := CatalogSourceInput{
		Kind:                CatalogSourceProviderMachineReadable,
		Reference:           reference,
		SourceSchemaVersion: ClaudeCapabilityProbeReceiptSchema,
		Precedence:          1000,
		Confidence:          ConfidenceUnavailable,
		FreshnessState:      FreshnessFresh,
		Entries:             []CatalogInputEntry{},
		Gaps:                []string{"claude-capability-probe-unavailable", strings.TrimPrefix(terminal, "Err")},
	}
	installID := binding.ProviderInstallationID
	snapshot, _, err := buildCatalogSnapshot(adapter, &installID, []CatalogSourceInput{source}, now)
	if err != nil {
		return Report{}, err
	}
	accountID := binding.AccountProfileID
	authID := binding.AuthReadinessID
	snapshot.AccountProfileID = &accountID
	snapshot.AuthReadinessID = &authID
	snapshot.StalePolicy = "failed-capability-probe-not-routable"
	snapshot.StaleAfter = formatTime(now)
	snapshot.SideEffectClass = "provider-paid-bounded-read"
	snapshot.Classification = "account-bound-provider-capability-failure"
	snapshot.Source = SourceDescriptor{Kind: string(CatalogSourceProviderMachineReadable), AdapterID: "claude", ProbeCommandID: "claude-code-verified-subset-v1", ExecutableName: "claude"}
	snapshot.Evidence = EvidenceSummary{Kind: "account-bound-paid-capability-probe-failed", CommandBounded: true, NoShell: true, RepositoryMutation: false, SecretMaterialRetained: false}
	snapshot.TerminalErrorCode = terminal
	snapshot.InventoryFingerprint, err = catalogFingerprint(snapshot, nil)
	if err != nil {
		return Report{}, err
	}
	report.ModelCatalogSnapshots = append(report.ModelCatalogSnapshots, snapshot)
	report.GapReasons = dedupeStrings(append(report.GapReasons, "provider-claude-capability-probe-unavailable"))
	report.InventoryFingerprint, err = fingerprint(report)
	if err != nil {
		return Report{}, err
	}
	return report, nil
}

// ValidClaudeVerifiedSnapshot is the production route/qualification boundary
// for Claude catalog truth. Generic machine-readable labels are insufficient:
// the embedded receipt must be exact, unexpired, account/install bound, and
// arithmetically reconciled.
func ValidClaudeVerifiedSnapshot(snapshot ModelCatalogSnapshot, now time.Time) bool {
	if snapshot.AdapterID != "claude" ||
		snapshot.CatalogSourceKind != CatalogSourceProviderMachineReadable ||
		snapshot.SourceSchemaVersion != ClaudeCapabilityProbeReceiptSchema ||
		snapshot.Confidence != ConfidenceExact ||
		snapshot.FreshnessState != FreshnessFresh ||
		snapshot.EntryCount != 1 ||
		snapshot.TerminalErrorCode != "" ||
		snapshot.ProviderInstallationID == nil ||
		snapshot.AccountProfileID == nil ||
		snapshot.AuthReadinessID == nil ||
		snapshot.CapabilityProbeReceipt == nil {
		return false
	}
	receipt := *snapshot.CapabilityProbeReceipt
	if err := validateClaudeCapabilityProbeReceipt(receipt, now.UTC()); err != nil {
		return false
	}
	if *snapshot.ProviderInstallationID != receipt.ProviderInstallationID ||
		*snapshot.AccountProfileID != receipt.AccountProfileID ||
		*snapshot.AuthReadinessID != receipt.AuthReadinessID ||
		snapshot.CatalogSourceReference != "claude-capability-probe#"+receipt.OutputRawSHA256 {
		return false
	}
	return true
}

// ValidClaudeVerifiedCapability binds one routable capability row to the
// receipt's exact model and accepted effort. Sharing a valid snapshot ID is
// insufficient for any extra or mutated row.
func ValidClaudeVerifiedCapability(snapshot ModelCatalogSnapshot, capability ModelCapability, now time.Time) bool {
	if !ValidClaudeVerifiedSnapshot(snapshot, now) || snapshot.CapabilityProbeReceipt == nil {
		return false
	}
	receipt := snapshot.CapabilityProbeReceipt
	if capability.ModelCatalogSnapshotID != snapshot.ModelCatalogSnapshotID ||
		capability.AdapterID != "claude" ||
		capability.CanonicalModelID != receipt.ActualModel ||
		capability.AvailabilityState != AvailabilityAvailable ||
		capability.LifecycleState != LifecycleAvailable ||
		capability.Confidence != ConfidenceExact ||
		capability.FreshnessState != FreshnessFresh ||
		capability.Source.Kind != string(CatalogSourceProviderMachineReadable) {
		return false
	}
	supported, defaulted := 0, 0
	for _, constraint := range capability.Constraints {
		switch {
		case strings.HasPrefix(constraint, "supported_depth="):
			if constraint != "supported_depth="+receipt.AcceptedEffort {
				return false
			}
			supported++
		case strings.HasPrefix(constraint, "default_depth="):
			if constraint != "default_depth="+receipt.AcceptedEffort {
				return false
			}
			defaulted++
		}
	}
	if supported != 1 || defaulted != 1 {
		return false
	}
	for _, source := range capability.EntrySources {
		if source.SourceKind == CatalogSourceProviderMachineReadable &&
			source.SourceReference == snapshot.CatalogSourceReference &&
			source.Confidence == ConfidenceExact &&
			source.FreshnessState == FreshnessFresh {
			return true
		}
	}
	return false
}

// ValidateClaudeCapabilityProbeReceipt exposes the strict receipt validator to
// route and exact-artifact qualification boundaries.
func ValidateClaudeCapabilityProbeReceipt(receipt ClaudeCapabilityProbeReceipt, now time.Time) error {
	return validateClaudeCapabilityProbeReceipt(receipt, now.UTC())
}

func validateClaudeCapabilityProbeReceipt(receipt ClaudeCapabilityProbeReceipt, now time.Time) error {
	_, candidateOK := ClaudeCatalogCandidate(receipt.RequestedModel)
	tokenTotalOK := exactTokenTotal(
		receipt.TotalTokens,
		receipt.InputTokens,
		receipt.OutputTokens,
		receipt.CacheReadInputTokens,
		receipt.CacheCreateInputTokens,
	)
	budgetStateOK := (receipt.ReleasedTokens == 0 && receipt.BudgetState == "committed") ||
		(receipt.ReleasedTokens > 0 && receipt.BudgetState == "released")
	if receipt.SchemaVersion != ClaudeCapabilityProbeReceiptSchema ||
		receipt.Provider != "claude" ||
		receipt.RequestedModel != strings.TrimSpace(receipt.RequestedModel) ||
		receipt.ActualModel != strings.TrimSpace(receipt.ActualModel) ||
		!candidateOK ||
		!safeCatalogModelID(receipt.ActualModel) ||
		!validClaudeEffort(receipt.AcceptedEffort) ||
		!strings.HasPrefix(receipt.ProviderInstallationID, "pinst_") || !safeAdapterKey(receipt.ProviderInstallationID) ||
		!strings.HasPrefix(receipt.AccountProfileID, "acct-") || !exactLowerHex64(strings.TrimPrefix(receipt.AccountProfileID, "acct-")) ||
		!strings.HasPrefix(receipt.AuthReadinessID, "auth_") || !safeAdapterKey(receipt.AuthReadinessID) ||
		!exactSHA256(receipt.AuthRawSHA256) ||
		!exactSHA256(receipt.OutputRawSHA256) ||
		!exactSHA256(receipt.ArgvDigest) ||
		!tokenTotalOK ||
		receipt.TotalTokens <= 0 ||
		receipt.CostUSDMicros < 0 ||
		!strings.HasPrefix(receipt.BudgetReservationID, "bres_") || !safeAdapterKey(receipt.BudgetReservationID) ||
		!budgetStateOK ||
		receipt.ReservedTokens <= 0 ||
		receipt.CommittedTokens != receipt.TotalTokens ||
		receipt.ReleasedTokens < 0 ||
		!exactTokenTotal(receipt.ReservedTokens, receipt.CommittedTokens, receipt.ReleasedTokens) ||
		len(receipt.UsageRecordIDs) == 0 ||
		receipt.Source != "claude-code-stream-json" ||
		receipt.Confidence != ConfidenceExact ||
		receipt.FreshnessState != FreshnessFresh ||
		len(receipt.GapReasons) != 0 {
		return fmt.Errorf("%w: incomplete or non-exact receipt", ErrClaudeCatalogProbeInvalid)
	}
	seenUsage := map[string]struct{}{}
	for _, usageID := range receipt.UsageRecordIDs {
		if !strings.HasPrefix(usageID, "usage_") || !safeAdapterKey(usageID) {
			return fmt.Errorf("%w: invalid usage record identity", ErrClaudeCatalogProbeInvalid)
		}
		if _, exists := seenUsage[usageID]; exists {
			return fmt.Errorf("%w: duplicate usage record identity", ErrClaudeCatalogProbeInvalid)
		}
		seenUsage[usageID] = struct{}{}
	}
	executed, err := time.Parse(time.RFC3339Nano, receipt.ExecutedAt)
	if err != nil {
		return fmt.Errorf("%w: invalid executed_at", ErrClaudeCatalogProbeInvalid)
	}
	authObserved, err := time.Parse(time.RFC3339Nano, receipt.AuthObservedAt)
	if err != nil || authObserved.After(executed) || executed.Sub(authObserved) > claudeCapabilityProbeAuthLeadMax {
		return fmt.Errorf("%w: auth observation is not immediately preceding", ErrClaudeCatalogProbeInvalid)
	}
	expires, err := time.Parse(time.RFC3339Nano, receipt.ExpiresAt)
	if err != nil || !expires.After(executed) || expires.After(executed.Add(claudeCapabilityProbeFreshness+time.Second)) || !expires.After(now) {
		return fmt.Errorf("%w: invalid or expired receipt", ErrClaudeCatalogProbeInvalid)
	}
	return nil
}

func validClaudeEffort(value string) bool {
	switch value {
	case "low", "medium", "high", "xhigh", "max":
		return true
	default:
		return false
	}
}

func exactSHA256(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func exactLowerHex64(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func exactTokenTotal(want int64, values ...int64) bool {
	total := int64(0)
	for _, value := range values {
		if value < 0 || value > int64(^uint64(0)>>1)-total {
			return false
		}
		total += value
	}
	return want == total
}

func reportHasClaudeBinding(report Report, installationID, accountID string) bool {
	installed := false
	for _, installation := range report.Installations {
		if installation.AdapterID == "claude" && installation.ProviderInstallationID == installationID &&
			installation.InstallationState == InstallationInstalled {
			installed = true
			break
		}
	}
	if !installed {
		return false
	}
	for _, auth := range report.AuthReadiness {
		if auth.AdapterID != "claude" || auth.ProviderInstallationID == nil || auth.AccountProfileID == nil {
			continue
		}
		if *auth.ProviderInstallationID == installationID && *auth.AccountProfileID == accountID && ExactFreshReadyAuth(auth) {
			return true
		}
	}
	return false
}
