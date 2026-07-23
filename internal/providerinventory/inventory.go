// Package providerinventory discovers provider CLI installations through
// bounded adapter-declared probes.
package providerinventory

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/models"
	"github.com/jasonhnd/loopcoder/internal/runtimecap"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
)

const (
	ProviderInventoryJSONSchema = "loopcoder.provider_inventory_json.v1"
	InventoryRefsSchema         = "loopcoder.inventory_refs.v1"
	ProviderInstallationSchema  = "loopcoder.provider_installation.v1"
	ProbeResultSchema           = "loopcoder.probe_result.v1"
	AccountProfileSchema        = "loopcoder.account_profile.v1"
	AuthReadinessSchema         = "loopcoder.auth_readiness.v1"

	AdapterDeclarationSchema = "loopcoder.adapter_declaration.v1"
	AdapterVersion           = "v1"
	PolicyVersion            = "provider-inventory-v1"

	// InstallProbeTimeout is a hang guard, not a startup race. Real Node-based
	// CLIs on Windows have measured 5-10s cold starts, including gemini
	// --version at 6-9s, so keep this generous enough for valid probes.
	InstallProbeTimeout = 20 * time.Second
	// AuthProbeTimeout uses the same hang-guard budget because auth readiness
	// probes can pay the same Node-shim cold-start cost before doing useful
	// local work.
	AuthProbeTimeout   = 20 * time.Second
	StdoutLimitBytes   = 64 * 1024
	StderrLimitBytes   = 64 * 1024
	CombinedLimitBytes = 128 * 1024
)

var ErrInvalidRecord = errors.New("ErrInvalidRecord")

type Confidence string

const (
	ConfidenceExact       Confidence = "exact"
	ConfidenceEstimated   Confidence = "estimated"
	ConfidenceUnknown     Confidence = "unknown"
	ConfidenceUnavailable Confidence = "unavailable"
	ConfidenceStale       Confidence = "stale"
)

type FreshnessState string

const (
	FreshnessFresh         FreshnessState = "fresh"
	FreshnessStale         FreshnessState = "stale"
	FreshnessExpired       FreshnessState = "expired"
	FreshnessNotApplicable FreshnessState = "not-applicable"
)

type InstallationState string

const (
	InstallationInstalled         InstallationState = "installed"
	InstallationNotInstalled      InstallationState = "not-installed"
	InstallationInstalledUnusable InstallationState = "installed-but-unusable"
	InstallationProbeFailed       InstallationState = "probe-failed"
	InstallationStale             InstallationState = "stale"
)

type ProbeOutcome string

const (
	OutcomeInstalled         ProbeOutcome = "installed"
	OutcomeNotInstalled      ProbeOutcome = "not-installed"
	OutcomeInstalledUnusable ProbeOutcome = "installed-but-unusable"
	OutcomeProbeFailed       ProbeOutcome = "probe-failed"
)

type ProbeMethod string

const (
	ProbeMethodLookPath     ProbeMethod = "look-path"
	ProbeMethodLstat        ProbeMethod = "lstat"
	ProbeMethodFixedCommand ProbeMethod = "fixed-command"
	ProbeMethodMachineJSON  ProbeMethod = "machine-readable-command"
	ProbeMethodConfigured   ProbeMethod = "configured-overlay"
)

type DiscoverySource string

const (
	DiscoveryPath           DiscoverySource = "path"
	DiscoveryExplicitConfig DiscoverySource = "explicit-config"
	DiscoveryFixture        DiscoverySource = "fixture"
)

type NetworkPermission string

const (
	NetworkNotNeeded NetworkPermission = "not-needed"
	NetworkDenied    NetworkPermission = "denied"
	NetworkGranted   NetworkPermission = "granted"
)

type NetworkPurpose string

const (
	NetworkPurposeAuthReadiness        NetworkPurpose = "auth-readiness"
	NetworkPurposeModelCatalog         NetworkPurpose = "model-catalog"
	NetworkPurposeAuthCatalogInventory NetworkPurpose = "auth-catalog-inventory"
	NetworkPurposeQuotaTelemetry       NetworkPurpose = "quota-telemetry"
)

type NetworkScope string

const (
	NetworkScopeMachineInventory NetworkScope = "machine-provider-inventory"
)

type NetworkGrant struct {
	ProviderID string
	Purpose    NetworkPurpose
	Scope      NetworkScope
}

type ProfileSource string

const (
	ProfileSourceStatusCommand ProfileSource = "provider-status-command"
	ProfileSourceConfigRef     ProfileSource = "provider-config-reference"
	ProfileSourceEnvRef        ProfileSource = "environment-reference"
	ProfileSourceUserConfig    ProfileSource = "user-config"
	ProfileSourceFixture       ProfileSource = "fixture"
	ProfileSourceUnknown       ProfileSource = "unknown"
)

type SelectionState string

const (
	SelectionDefault    SelectionState = "default"
	SelectionExplicit   SelectionState = "explicit"
	SelectionCandidate  SelectionState = "candidate"
	SelectionSuperseded SelectionState = "superseded"
	SelectionUnknown    SelectionState = "unknown"
)

type ReadinessState string

const (
	ReadinessReady            ReadinessState = "ready"
	ReadinessNotAuthenticated ReadinessState = "not-authenticated"
	ReadinessExpired          ReadinessState = "expired"
	ReadinessUnknown          ReadinessState = "unknown"
)

type EvidenceKind string

const (
	EvidenceExitCode           EvidenceKind = "exit-code"
	EvidenceStatusCommand      EvidenceKind = "sanctioned-status-command"
	EvidenceMachineStatus      EvidenceKind = "machine-readable-status"
	EvidenceFileExistence      EvidenceKind = "file-existence"
	EvidenceEnvironmentName    EvidenceKind = "environment-name-existence"
	EvidenceProviderErrorClass EvidenceKind = "provider-error-class"
	EvidenceUnsupported        EvidenceKind = "unsupported"
	EvidenceNotRun             EvidenceKind = "not-run"
)

type AuthorizationScopeState string

const (
	AuthorizationAllKnown      AuthorizationScopeState = "all-known"
	AuthorizationPartial       AuthorizationScopeState = "partial"
	AuthorizationUnknown       AuthorizationScopeState = "unknown"
	AuthorizationNotApplicable AuthorizationScopeState = "not-applicable"
)

func (c *Confidence) UnmarshalJSON(data []byte) error {
	value, err := unmarshalEnumString(data, "confidence")
	if err != nil {
		return err
	}
	switch Confidence(value) {
	case ConfidenceExact, ConfidenceEstimated, ConfidenceUnknown, ConfidenceUnavailable, ConfidenceStale:
		*c = Confidence(value)
		return nil
	default:
		return fmt.Errorf("%w: unknown confidence %q", ErrInvalidRecord, value)
	}
}

func (s *FreshnessState) UnmarshalJSON(data []byte) error {
	value, err := unmarshalEnumString(data, "freshness_state")
	if err != nil {
		return err
	}
	switch FreshnessState(value) {
	case FreshnessFresh, FreshnessStale, FreshnessExpired, FreshnessNotApplicable:
		*s = FreshnessState(value)
		return nil
	default:
		return fmt.Errorf("%w: unknown freshness_state %q", ErrInvalidRecord, value)
	}
}

func (s *InstallationState) UnmarshalJSON(data []byte) error {
	value, err := unmarshalEnumString(data, "installation_state")
	if err != nil {
		return err
	}
	switch InstallationState(value) {
	case InstallationInstalled, InstallationNotInstalled, InstallationInstalledUnusable, InstallationProbeFailed, InstallationStale:
		*s = InstallationState(value)
		return nil
	default:
		return fmt.Errorf("%w: unknown installation_state %q", ErrInvalidRecord, value)
	}
}

func (o *ProbeOutcome) UnmarshalJSON(data []byte) error {
	value, err := unmarshalEnumString(data, "outcome")
	if err != nil {
		return err
	}
	switch ProbeOutcome(value) {
	case OutcomeInstalled, OutcomeNotInstalled, OutcomeInstalledUnusable, OutcomeProbeFailed:
		*o = ProbeOutcome(value)
		return nil
	default:
		return fmt.Errorf("%w: unknown outcome %q", ErrInvalidRecord, value)
	}
}

func (m *ProbeMethod) UnmarshalJSON(data []byte) error {
	value, err := unmarshalEnumString(data, "probe_method")
	if err != nil {
		return err
	}
	switch ProbeMethod(value) {
	case ProbeMethodLookPath, ProbeMethodLstat, ProbeMethodFixedCommand, ProbeMethodMachineJSON, ProbeMethodConfigured:
		*m = ProbeMethod(value)
		return nil
	default:
		return fmt.Errorf("%w: unknown probe_method %q", ErrInvalidRecord, value)
	}
}

func (s *DiscoverySource) UnmarshalJSON(data []byte) error {
	value, err := unmarshalEnumString(data, "discovery_source")
	if err != nil {
		return err
	}
	switch DiscoverySource(value) {
	case DiscoveryPath, DiscoveryExplicitConfig, DiscoveryFixture:
		*s = DiscoverySource(value)
		return nil
	default:
		return fmt.Errorf("%w: unknown discovery_source %q", ErrInvalidRecord, value)
	}
}

func (p *NetworkPermission) UnmarshalJSON(data []byte) error {
	value, err := unmarshalEnumString(data, "network_permission")
	if err != nil {
		return err
	}
	switch NetworkPermission(value) {
	case NetworkNotNeeded, NetworkDenied, NetworkGranted:
		*p = NetworkPermission(value)
		return nil
	default:
		return fmt.Errorf("%w: unknown network_permission %q", ErrInvalidRecord, value)
	}
}

func (s *ReadinessState) UnmarshalJSON(data []byte) error {
	value, err := unmarshalEnumString(data, "readiness_state")
	if err != nil {
		return err
	}
	switch ReadinessState(value) {
	case ReadinessReady, ReadinessNotAuthenticated, ReadinessExpired, ReadinessUnknown:
		*s = ReadinessState(value)
		return nil
	default:
		return fmt.Errorf("%w: unknown readiness_state %q", ErrInvalidRecord, value)
	}
}

func (k *EvidenceKind) UnmarshalJSON(data []byte) error {
	value, err := unmarshalEnumString(data, "evidence_kind")
	if err != nil {
		return err
	}
	switch EvidenceKind(value) {
	case EvidenceExitCode, EvidenceStatusCommand, EvidenceMachineStatus, EvidenceFileExistence, EvidenceEnvironmentName, EvidenceProviderErrorClass, EvidenceUnsupported, EvidenceNotRun:
		*k = EvidenceKind(value)
		return nil
	default:
		return fmt.Errorf("%w: unknown evidence_kind %q", ErrInvalidRecord, value)
	}
}

func unmarshalEnumString(data []byte, field string) (string, error) {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return "", fmt.Errorf("%w: invalid %s enum: %v", ErrInvalidRecord, field, err)
	}
	if value == "" {
		return "", fmt.Errorf("%w: empty %s enum", ErrInvalidRecord, field)
	}
	return value, nil
}

type ActorProvenance struct {
	ActorKind         string `json:"actor_kind"`
	ActorID           string `json:"actor_id"`
	Display           string `json:"display"`
	DecisionAuthority string `json:"decision_authority"`
	Source            string `json:"source"`
}

type HostProvenance struct {
	HostKind         string `json:"host_kind"`
	HostID           string `json:"host_id"`
	SessionID        string `json:"session_id,omitempty"`
	ProcessID        int    `json:"process_id"`
	LoopcoderVersion string `json:"loopcoder_version"`
	Platform         string `json:"platform"`
}

type Report struct {
	SchemaVersion         string                 `json:"schema_version"`
	GeneratedAt           string                 `json:"generated_at"`
	InventoryFingerprint  string                 `json:"inventory_fingerprint"`
	Confidence            Confidence             `json:"confidence"`
	Installations         []ProviderInstallation `json:"installations"`
	ProbeResults          []ProbeResult          `json:"probe_results"`
	AccountProfiles       []AccountProfile       `json:"account_profiles"`
	AuthReadiness         []AuthReadiness        `json:"auth_readiness"`
	ModelCatalogSnapshots []ModelCatalogSnapshot `json:"model_catalog_snapshots"`
	ModelCapabilities     []ModelCapability      `json:"model_capabilities"`
	QuotaTelemetrySources []QuotaTelemetrySource `json:"quota_telemetry_sources"`
	QuotaSnapshots        []QuotaSnapshot        `json:"quota_snapshots"`
	GapReasons            []string               `json:"gap_reasons"`
}

type InventoryRefs struct {
	SchemaVersion           string     `json:"schema_version"`
	InventoryFingerprint    string     `json:"inventory_fingerprint"`
	ProviderInstallationIDs []string   `json:"provider_installation_ids"`
	AccountProfileIDs       []string   `json:"account_profile_ids"`
	AuthReadinessIDs        []string   `json:"auth_readiness_ids"`
	ModelCatalogSnapshotIDs []string   `json:"model_catalog_snapshot_ids"`
	ModelCapabilityIDs      []string   `json:"model_capability_ids"`
	QuotaSnapshotIDs        []string   `json:"quota_snapshot_ids"`
	Confidence              Confidence `json:"confidence"`
	GapReasons              []string   `json:"gap_reasons"`
}

type ProviderInstallation struct {
	SchemaVersion          string             `json:"schema_version"`
	RecordVersion          int                `json:"record_version"`
	Scope                  string             `json:"scope"`
	ProjectID              *string            `json:"project_id"`
	ProviderInstallationID string             `json:"provider_installation_id"`
	AdapterID              string             `json:"adapter_id"`
	AdapterDeclarationID   string             `json:"adapter_declaration_id"`
	ProviderDisplayName    string             `json:"provider_display_name"`
	ExecutableName         string             `json:"executable_name"`
	ExecutableIdentity     ExecutableIdentity `json:"executable_identity"`
	CanonicalPathRedacted  string             `json:"canonical_path_redacted"`
	DiscoverySource        DiscoverySource    `json:"discovery_source"`
	DiscoveryOrder         int                `json:"discovery_order"`
	Platform               string             `json:"platform"`
	Version                string             `json:"version,omitempty"`
	VersionConfidence      Confidence         `json:"version_confidence"`
	LatestProbeResultID    string             `json:"latest_probe_result_id,omitempty"`
	InstallationState      InstallationState  `json:"installation_state"`
	UsableForInvocation    string             `json:"usable_for_invocation"`
	KnownLimitations       []string           `json:"known_limitations"`
	CreatedAt              string             `json:"created_at"`
	UpdatedAt              string             `json:"updated_at"`
	CreatedBy              ActorProvenance    `json:"created_by"`
	UpdatedBy              ActorProvenance    `json:"updated_by"`
	Host                   HostProvenance     `json:"host"`
	PolicyVersion          string             `json:"policy_version"`
	CapturedAt             string             `json:"captured_at"`
	StaleAfter             string             `json:"stale_after,omitempty"`
	FreshnessState         FreshnessState     `json:"freshness_state"`
	Confidence             Confidence         `json:"confidence"`
	SideEffectClass        string             `json:"side_effect_class"`
	Classification         string             `json:"classification"`
	Source                 SourceDescriptor   `json:"source"`
	Evidence               EvidenceSummary    `json:"evidence"`
	GapReasons             []string           `json:"gap_reasons"`
	TerminalErrorCode      string             `json:"terminal_error_code,omitempty"`
}

type ExecutableIdentity struct {
	Basename          string `json:"basename"`
	Platform          string `json:"platform"`
	PathHash          string `json:"path_hash"`
	SymlinkResolution string `json:"symlink_resolution"`
	ResolvedPathHash  string `json:"resolved_path_hash,omitempty"`
	ExecutableMode    string `json:"executable_mode"`
}

type ProbeResult struct {
	SchemaVersion            string            `json:"schema_version"`
	RecordVersion            int               `json:"record_version"`
	Scope                    string            `json:"scope"`
	ProjectID                *string           `json:"project_id"`
	ProbeResultID            string            `json:"probe_result_id"`
	AdapterID                string            `json:"adapter_id"`
	AdapterDeclarationID     string            `json:"adapter_declaration_id"`
	ProviderInstallationID   *string           `json:"provider_installation_id"`
	ProbeKind                string            `json:"probe_kind"`
	ProbeCommandID           string            `json:"probe_command_id,omitempty"`
	ProbeMethod              ProbeMethod       `json:"probe_method"`
	Outcome                  ProbeOutcome      `json:"outcome"`
	Argv                     []string          `json:"argv,omitempty"`
	WorkingDirectory         string            `json:"working_directory"`
	EnvironmentKeys          []string          `json:"environment_keys"`
	TimeoutMS                int               `json:"timeout_ms"`
	StdoutLimitBytes         int               `json:"stdout_limit_bytes"`
	StderrLimitBytes         int               `json:"stderr_limit_bytes"`
	CombinedOutputLimitBytes int               `json:"combined_output_limit_bytes"`
	ExitCode                 *int              `json:"exit_code,omitempty"`
	TimedOut                 bool              `json:"timed_out"`
	Killed                   bool              `json:"killed"`
	NetworkDeclared          bool              `json:"network_declared"`
	NetworkPermission        NetworkPermission `json:"network_permission"`
	StdoutSummary            string            `json:"stdout_summary,omitempty"`
	StderrSummary            string            `json:"stderr_summary,omitempty"`
	ParsedFields             map[string]string `json:"parsed_fields_json,omitempty"`
	SecretFindingCount       int               `json:"secret_finding_count"`
	CreatedAt                string            `json:"created_at"`
	UpdatedAt                string            `json:"updated_at"`
	CreatedBy                ActorProvenance   `json:"created_by"`
	UpdatedBy                ActorProvenance   `json:"updated_by"`
	Host                     HostProvenance    `json:"host"`
	PolicyVersion            string            `json:"policy_version"`
	CapturedAt               string            `json:"captured_at"`
	StaleAfter               string            `json:"stale_after,omitempty"`
	FreshnessState           FreshnessState    `json:"freshness_state"`
	Confidence               Confidence        `json:"confidence"`
	SideEffectClass          string            `json:"side_effect_class"`
	Classification           string            `json:"classification"`
	Source                   SourceDescriptor  `json:"source"`
	Evidence                 EvidenceSummary   `json:"evidence"`
	GapReasons               []string          `json:"gap_reasons"`
	TerminalErrorCode        string            `json:"terminal_error_code,omitempty"`
}

type AccountProfile struct {
	SchemaVersion                string           `json:"schema_version"`
	RecordVersion                int              `json:"record_version"`
	Scope                        string           `json:"scope"`
	ProjectID                    *string          `json:"project_id"`
	AccountProfileID             string           `json:"account_profile_id"`
	AdapterID                    string           `json:"adapter_id"`
	ProviderInstallationID       *string          `json:"provider_installation_id,omitempty"`
	ProfileSource                ProfileSource    `json:"profile_source"`
	ProfileReferenceHash         string           `json:"profile_reference_hash"`
	ProfileDisplay               string           `json:"profile_display,omitempty"`
	ProviderAccountKind          string           `json:"provider_account_kind,omitempty"`
	TenantOrProjectReferenceHash string           `json:"tenant_or_project_reference_hash,omitempty"`
	SelectionState               SelectionState   `json:"selection_state"`
	LatestAuthReadinessID        string           `json:"latest_auth_readiness_id,omitempty"`
	AllowedScopeSummary          string           `json:"allowed_scope_summary,omitempty"`
	CollisionSet                 []string         `json:"collision_set"`
	CreatedAt                    string           `json:"created_at"`
	UpdatedAt                    string           `json:"updated_at"`
	CreatedBy                    ActorProvenance  `json:"created_by"`
	UpdatedBy                    ActorProvenance  `json:"updated_by"`
	Host                         HostProvenance   `json:"host"`
	PolicyVersion                string           `json:"policy_version"`
	CapturedAt                   string           `json:"captured_at"`
	FreshnessState               FreshnessState   `json:"freshness_state"`
	Confidence                   Confidence       `json:"confidence"`
	SideEffectClass              string           `json:"side_effect_class"`
	Classification               string           `json:"classification"`
	Source                       SourceDescriptor `json:"source"`
	Evidence                     EvidenceSummary  `json:"evidence"`
	GapReasons                   []string         `json:"gap_reasons"`
	TerminalErrorCode            string           `json:"terminal_error_code,omitempty"`
}

type AuthReadiness struct {
	SchemaVersion             string                  `json:"schema_version"`
	RecordVersion             int                     `json:"record_version"`
	Scope                     string                  `json:"scope"`
	ProjectID                 *string                 `json:"project_id"`
	AuthReadinessID           string                  `json:"auth_readiness_id"`
	AdapterID                 string                  `json:"adapter_id"`
	ProviderInstallationID    *string                 `json:"provider_installation_id,omitempty"`
	AccountProfileID          *string                 `json:"account_profile_id,omitempty"`
	ReadinessState            ReadinessState          `json:"readiness_state"`
	ReadinessConfidence       Confidence              `json:"readiness_confidence"`
	EvidenceKind              EvidenceKind            `json:"evidence_kind"`
	AuthorizationScopeState   AuthorizationScopeState `json:"authorization_scope_state"`
	AuthorizationScopeSummary string                  `json:"authorization_scope_summary,omitempty"`
	RefreshRequired           bool                    `json:"refresh_required"`
	UnsupportedReason         string                  `json:"unsupported_reason,omitempty"`
	CreatedAt                 string                  `json:"created_at"`
	UpdatedAt                 string                  `json:"updated_at"`
	CreatedBy                 ActorProvenance         `json:"created_by"`
	UpdatedBy                 ActorProvenance         `json:"updated_by"`
	Host                      HostProvenance          `json:"host"`
	PolicyVersion             string                  `json:"policy_version"`
	CapturedAt                string                  `json:"captured_at"`
	FreshnessState            FreshnessState          `json:"freshness_state"`
	Confidence                Confidence              `json:"confidence"`
	SideEffectClass           string                  `json:"side_effect_class"`
	Classification            string                  `json:"classification"`
	Source                    SourceDescriptor        `json:"source"`
	Evidence                  EvidenceSummary         `json:"evidence"`
	GapReasons                []string                `json:"gap_reasons"`
	TerminalErrorCode         string                  `json:"terminal_error_code,omitempty"`
}

type SourceDescriptor struct {
	Kind            string `json:"kind"`
	AdapterID       string `json:"adapter_id"`
	ProbeCommandID  string `json:"probe_command_id,omitempty"`
	DiscoverySource string `json:"discovery_source,omitempty"`
	ExecutableName  string `json:"executable_name,omitempty"`
}

type EvidenceSummary struct {
	Kind                   string `json:"kind"`
	CommandBounded         bool   `json:"command_bounded"`
	NoShell                bool   `json:"no_shell"`
	RepositoryMutation     bool   `json:"repository_mutation"`
	SecretMaterialRetained bool   `json:"secret_material_retained"`
}

type AdapterDeclaration struct {
	AdapterID                  string
	AdapterVersion             string
	DeclarationSchemaVersion   string
	DisplayName                string
	Vendor                     string
	ExecutableNames            []string
	VersionArgv                []string
	ConformanceVersion         string
	AuthProbeCommand           []string
	AuthProbeParser            string
	AuthProbeMayNetwork        bool
	AuthArtifactPaths          []string
	AuthEnvironmentNames       []string
	AuthUnsupportedReason      string
	CatalogProbeCommand        []string
	CatalogProbeParser         string
	CatalogProbeMayNetwork     bool
	StaticCatalogEntries       []CatalogInputEntry
	SelectedAccountProfileID   string
	KnownLimitations           []string
	MayNetwork                 bool
	ExplicitPaths              []string
	DiscoverySourceForExplicit DiscoverySource
}

type Options struct {
	RepoPath        string
	Config          config.Config
	RuntimeContract runtimecap.Contract
	Now             func() time.Time
	NetworkGrants   []NetworkGrant
	ActiveProviders []string
}

type Deps struct {
	Getenv         func(string) string
	LookupEnv      func(string) (string, bool)
	UserHomeDir    func() (string, error)
	Lstat          func(string) (os.FileInfo, error)
	Abs            func(string) (string, error)
	EvalSymlinks   func(string) (string, error)
	RunProbe       func(context.Context, ProbeExecution) (ProbeExecutionResult, error)
	RunCodexRPC    func(context.Context, CodexAppServerRequest) (CodexAppServerResult, error)
	RunClaudePTY   func(context.Context, ClaudePTYRequest) (ClaudePTYResult, error)
	RunGrokACP     func(context.Context, GrokACPBillingRequest) (GrokACPBillingResult, error)
	RunGrokCredits func(context.Context, GrokCreditsBillingRequest) (GrokCreditsBillingResult, error)
	RunCodexBar    func(context.Context, CodexBarRequest) (CodexBarResult, error)
	MkdirTemp      func(string, string) (string, error)
	RemoveAll      func(string) error
	WriteFile      func(string, []byte, os.FileMode) error
	RandomID       func() string
}

type ProbeExecution struct {
	Argv               []string
	Env                []string
	Timeout            time.Duration
	StdoutLimitBytes   int
	StderrLimitBytes   int
	CombinedLimitBytes int
}

type ProbeExecutionResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	TimedOut bool
	Killed   bool
}

type discoveryContext struct {
	networkGrants map[NetworkGrant]bool
	probeResults  map[string]cachedProbeResult
}

type cachedProbeResult struct {
	result ProbeExecutionResult
	err    error
}

func DefaultDeps() Deps {
	return Deps{
		Getenv:         os.Getenv,
		LookupEnv:      os.LookupEnv,
		UserHomeDir:    os.UserHomeDir,
		Lstat:          os.Lstat,
		Abs:            filepath.Abs,
		EvalSymlinks:   filepath.EvalSymlinks,
		RunProbe:       runProbeCommand,
		RunCodexRPC:    runCodexAppServer,
		RunClaudePTY:   runClaudeUsagePTY,
		RunGrokACP:     runGrokACPBilling,
		RunGrokCredits: runGrokCreditsBilling,
		RunCodexBar:    runCodexBarQuota,
		MkdirTemp:      os.MkdirTemp,
		RemoveAll:      os.RemoveAll,
		WriteFile:      os.WriteFile,
		RandomID:       randomProbeID,
	}
}

func newDiscoveryContext(opts Options) *discoveryContext {
	grants := make(map[NetworkGrant]bool, len(opts.NetworkGrants))
	for _, grant := range opts.NetworkGrants {
		grant.ProviderID = strings.TrimSpace(grant.ProviderID)
		if grant.ProviderID == "" || grant.Purpose == "" || grant.Scope == "" {
			continue
		}
		grants[grant] = true
	}
	return &discoveryContext{
		networkGrants: grants,
		probeResults:  map[string]cachedProbeResult{},
	}
}

func networkPermissionFor(discovery *discoveryContext, adapter AdapterDeclaration, purpose NetworkPurpose, declared bool) NetworkPermission {
	if !declared {
		return NetworkNotNeeded
	}
	if discovery == nil {
		return NetworkDenied
	}
	exact := NetworkGrant{ProviderID: adapter.AdapterID, Purpose: purpose, Scope: NetworkScopeMachineInventory}
	combined := NetworkGrant{ProviderID: adapter.AdapterID, Purpose: NetworkPurposeAuthCatalogInventory, Scope: NetworkScopeMachineInventory}
	if discovery.networkGrants[exact] || discovery.networkGrants[combined] {
		return NetworkGranted
	}
	return NetworkDenied
}

func sharedProbeResult(ctx context.Context, discovery *discoveryContext, deps Deps, exec ProbeExecution) (ProbeExecutionResult, error) {
	if discovery == nil {
		return deps.RunProbe(ctx, exec)
	}
	key := sharedProbeKey(exec)
	if cached, ok := discovery.probeResults[key]; ok {
		return cached.result, cached.err
	}
	result, err := deps.RunProbe(ctx, exec)
	discovery.probeResults[key] = cachedProbeResult{result: result, err: err}
	return result, err
}

func sharedProbeKey(exec ProbeExecution) string {
	env := append([]string(nil), exec.Env...)
	sort.Strings(env)
	parts := []string{
		strings.Join(exec.Argv, "\x00"),
		strings.Join(env, "\x00"),
		exec.Timeout.String(),
		strconv.Itoa(exec.StdoutLimitBytes),
		strconv.Itoa(exec.StderrLimitBytes),
		strconv.Itoa(exec.CombinedLimitBytes),
	}
	return strings.Join(parts, "\x01")
}

func EmptyRefs() InventoryRefs {
	return InventoryRefs{
		SchemaVersion:           InventoryRefsSchema,
		InventoryFingerprint:    "sha256:" + hashHex("loopcoder.inventory_refs.v1", "empty"),
		ProviderInstallationIDs: []string{},
		AccountProfileIDs:       []string{},
		AuthReadinessIDs:        []string{},
		ModelCatalogSnapshotIDs: []string{},
		ModelCapabilityIDs:      []string{},
		QuotaSnapshotIDs:        []string{},
		Confidence:              ConfidenceUnknown,
		GapReasons:              []string{"inventory-not-bound-to-run"},
	}
}

func Discover(ctx context.Context, opts Options, deps Deps) (Report, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	deps = normalizeDeps(deps)
	now := normalizeNow(opts.Now)().UTC()
	contract := normalizeRuntimeContract(opts.RuntimeContract)
	if violations := contract.InvariantViolations(); len(violations) > 0 {
		return Report{}, fmt.Errorf("%w: runtime capability contract: %s", ErrInvalidRecord, strings.Join(violations, "; "))
	}
	discovery := newDiscoveryContext(opts)
	adapters, err := adapterDeclarations(opts.Config, contract)
	if err != nil {
		return Report{}, err
	}
	adapters = filterAdapterDeclarations(adapters, opts.ActiveProviders)
	var installations []ProviderInstallation
	var probes []ProbeResult
	var accountProfiles []AccountProfile
	var authReadiness []AuthReadiness
	var modelCatalogSnapshots []ModelCatalogSnapshot
	var modelCapabilities []ModelCapability
	var quotaTelemetrySources []QuotaTelemetrySource
	var quotaSnapshots []QuotaSnapshot
	var gaps []string

	for _, adapter := range adapters {
		quotaSource, quotaSnapshot := unsupportedQuotaTelemetryForAdapter(adapter, now)
		adapterQuotaCollected := false
		adapterQuotaAttempted := false
		snapshot, capabilities, err := staticCatalogForAdapter(adapter, now)
		if err != nil {
			return Report{}, err
		}
		modelCatalogSnapshots = append(modelCatalogSnapshots, snapshot)
		modelCapabilities = append(modelCapabilities, capabilities...)
		found := false
		candidates := discoverCandidates(adapter, deps)
		for _, candidate := range candidates {
			found = true
			installation, probe := inspectCandidate(ctx, adapter, candidate, now, deps)
			installations = append(installations, installation)
			probes = append(probes, probe)
			if installation.InstallationState == InstallationInstalled {
				profiles, readiness, authProbe := inspectAuthReadiness(ctx, discovery, adapter, candidate, installation.ProviderInstallationID, now, deps)
				accountProfiles = append(accountProfiles, profiles...)
				authReadiness = append(authReadiness, readiness...)
				if authProbe != nil {
					probes = append(probes, *authProbe)
				}
				if adapter.AdapterID == "codex" && !adapterQuotaAttempted {
					source, snapshots, quotaProbe := inspectCodexQuota(ctx, discovery, adapter, candidate, installation, now, deps)
					quotaTelemetrySources = append(quotaTelemetrySources, source)
					quotaSnapshots = append(quotaSnapshots, snapshots...)
					probes = append(probes, quotaProbe)
					adapterQuotaAttempted = true
					adapterQuotaCollected = quotaProbe.Outcome == OutcomeInstalled && len(snapshots) > 0 && snapshots[0].Confidence == ConfidenceExact
				}
				if adapter.AdapterID == "claude" && !adapterQuotaAttempted {
					source, snapshots, quotaProbe := inspectClaudeQuota(ctx, discovery, adapter, candidate, installation, now, deps)
					quotaTelemetrySources = append(quotaTelemetrySources, source)
					quotaSnapshots = append(quotaSnapshots, snapshots...)
					probes = append(probes, quotaProbe)
					adapterQuotaAttempted = true
					adapterQuotaCollected = quotaProbe.Outcome == OutcomeInstalled && len(snapshots) > 0 && snapshots[0].Confidence == ConfidenceExact
				}
				if adapter.AdapterID == "grok" && !adapterQuotaAttempted {
					source, snapshots, quotaProbe := inspectGrokACPBilling(ctx, discovery, adapter, candidate, installation, now, deps)
					quotaTelemetrySources = append(quotaTelemetrySources, source)
					quotaSnapshots = append(quotaSnapshots, snapshots...)
					probes = append(probes, quotaProbe)
					adapterQuotaAttempted = true
					adapterQuotaCollected = quotaProbe.Outcome == OutcomeInstalled && len(snapshots) > 0
				}
				if adapter.AdapterID == "antigravity" && !adapterQuotaAttempted {
					source, snapshots, quotaProbe := inspectAntigravityQuota(ctx, discovery, adapter, installation, now, deps)
					quotaTelemetrySources = append(quotaTelemetrySources, source)
					quotaSnapshots = append(quotaSnapshots, snapshots...)
					probes = append(probes, quotaProbe)
					adapterQuotaAttempted = true
					adapterQuotaCollected = quotaProbe.Outcome == OutcomeInstalled && len(snapshots) > 0 &&
						snapshots[0].Confidence != ConfidenceUnavailable
				}
				if adapter.AdapterID == "grok" {
					nativeProbe := inspectGrokNativeFederation(ctx, discovery, adapter, candidate, installation, now, deps)
					probes = append(probes, nativeProbe)
					if nativeProbe.Outcome != OutcomeInstalled {
						gaps = append(gaps, "provider-grok-native-federation-unsupported")
					}
				}
			} else {
				readiness := unsupportedAuthReadiness(adapter, &installation.ProviderInstallationID, now, firstNonEmpty(installation.TerminalErrorCode, "installation-not-usable"))
				readiness.EvidenceKind = EvidenceNotRun
				readiness.GapReasons = append(readiness.GapReasons, "installation-not-usable")
				authReadiness = append(authReadiness, readiness)
			}
			if len(adapter.CatalogProbeCommand) > 0 {
				snapshot, capabilities, catalogProbe := inspectCatalogCommand(ctx, discovery, adapter, candidate, installation, now, deps)
				modelCatalogSnapshots = append(modelCatalogSnapshots, snapshot)
				modelCapabilities = append(modelCapabilities, capabilities...)
				probes = append(probes, catalogProbe)
				if catalogProbe.NetworkDeclared && catalogProbe.NetworkPermission == NetworkDenied {
					gaps = append(gaps, "provider-"+adapter.AdapterID+"-catalog-network-permission-denied")
				}
			}
		}
		if !found {
			probe := absentProbe(adapter, now, deps)
			probes = append(probes, probe)
			gaps = append(gaps, "provider-"+adapter.AdapterID+"-not-installed")
		}
		if !adapterQuotaCollected && !adapterQuotaAttempted {
			if adapter.AdapterID == "codex" {
				quotaSource, quotaSnapshot = quotaTelemetryFallbackForAdapter(adapter, now, "quota-collection-not-granted", "ErrQuotaCollectionGrantRequired")
			}
			if adapter.AdapterID == "claude" {
				quotaSource, quotaSnapshot = quotaTelemetryFallbackForAdapter(adapter, now, "quota-collection-not-granted", "ErrQuotaCollectionGrantRequired")
			}
			if adapter.AdapterID == "antigravity" {
				quotaSource, quotaSnapshot = quotaTelemetryFallbackForAdapter(adapter, now, "quota-collection-not-granted", "ErrQuotaCollectionGrantRequired")
			}
			quotaTelemetrySources = append(quotaTelemetrySources, quotaSource)
			quotaSnapshots = append(quotaSnapshots, quotaSnapshot)
			gaps = append(gaps, "provider-"+adapter.AdapterID+"-quota-unsupported")
		}
	}
	if opts.Config.ProviderInventory.CodexBar.Enabled {
		sources, snapshots, codexbarProbes := inspectCodexBarQuota(ctx, opts.Config.ProviderInventory.CodexBar, now, deps)
		quotaTelemetrySources = append(quotaTelemetrySources, sources...)
		quotaSnapshots = append(quotaSnapshots, snapshots...)
		probes = append(probes, codexbarProbes...)
		for _, snapshot := range snapshots {
			if snapshot.Confidence == ConfidenceUnavailable {
				gaps = append(gaps, "provider-"+snapshot.AdapterID+"-codexbar-quota-unavailable")
			}
		}
	}
	quotaSnapshots = LinkQuotaConflicts(quotaSnapshots)
	// Installation discovery alone must leave usable_for_invocation=unknown.
	// Promote to "yes" only when a ready auth-readiness record binds the same
	// installation — auth evidence is separate from path/version probes.
	promoteUsableInstallations(installations, authReadiness)
	sort.SliceStable(installations, func(i, j int) bool {
		if installations[i].AdapterID != installations[j].AdapterID {
			return installations[i].AdapterID < installations[j].AdapterID
		}
		if installations[i].DiscoveryOrder != installations[j].DiscoveryOrder {
			return installations[i].DiscoveryOrder < installations[j].DiscoveryOrder
		}
		return installations[i].ProviderInstallationID < installations[j].ProviderInstallationID
	})
	report := Report{
		SchemaVersion:         ProviderInventoryJSONSchema,
		GeneratedAt:           formatTime(now),
		Confidence:            confidenceForInventory(installations, probes),
		Installations:         installations,
		ProbeResults:          probes,
		AccountProfiles:       accountProfiles,
		AuthReadiness:         authReadiness,
		ModelCatalogSnapshots: modelCatalogSnapshots,
		ModelCapabilities:     modelCapabilities,
		QuotaTelemetrySources: quotaTelemetrySources,
		QuotaSnapshots:        quotaSnapshots,
		GapReasons:            gaps,
	}
	fingerprint, err := fingerprint(report)
	if err != nil {
		return Report{}, err
	}
	report.InventoryFingerprint = fingerprint
	return report, nil
}

func Refresh(ctx context.Context, store storage.Store, report Report, now time.Time) error {
	if store == nil {
		return errors.New("provider inventory refresh: storage store is required")
	}
	sourceByID := map[string]QuotaTelemetrySource{}
	for _, source := range report.QuotaTelemetrySources {
		source = normalizeQuotaTelemetrySource(source)
		if err := ValidateQuotaTelemetrySource(source); err != nil {
			return err
		}
		sourceByID[source.QuotaSourceID] = source
	}
	for _, snapshot := range report.QuotaSnapshots {
		source, ok := sourceByID[snapshot.QuotaSourceID]
		if !ok {
			return fmt.Errorf("%w: quota snapshot %q references undeclared source %q", ErrQuotaSnapshotMalformed, snapshot.QuotaSnapshotID, snapshot.QuotaSourceID)
		}
		if err := ValidateQuotaSnapshot(source, snapshot); err != nil {
			return err
		}
	}
	nowText := formatTime(now.UTC())
	currentByAdapter := map[string]map[string]bool{}
	for _, installation := range report.Installations {
		if currentByAdapter[installation.AdapterID] == nil {
			currentByAdapter[installation.AdapterID] = map[string]bool{}
		}
		currentByAdapter[installation.AdapterID][installation.ProviderInstallationID] = true
	}
	for _, probe := range report.ProbeResults {
		if currentByAdapter[probe.AdapterID] == nil {
			currentByAdapter[probe.AdapterID] = map[string]bool{}
		}
	}
	return store.WithWriteTx(ctx, func(tx storage.Tx) error {
		for _, installation := range report.Installations {
			if err := upsertInstallation(ctx, tx, installation); err != nil {
				return err
			}
		}
		for _, probe := range report.ProbeResults {
			if err := insertProbe(ctx, tx, probe); err != nil {
				return err
			}
		}
		for _, profile := range report.AccountProfiles {
			if err := upsertAccountProfile(ctx, tx, profile); err != nil {
				return err
			}
		}
		for _, readiness := range report.AuthReadiness {
			if err := insertAuthReadiness(ctx, tx, readiness); err != nil {
				return err
			}
		}
		for _, snapshot := range report.ModelCatalogSnapshots {
			if err := insertModelCatalogSnapshot(ctx, tx, snapshot); err != nil {
				return err
			}
		}
		for _, capability := range report.ModelCapabilities {
			if err := insertModelCapability(ctx, tx, capability); err != nil {
				return err
			}
		}
		for _, source := range report.QuotaTelemetrySources {
			if err := upsertQuotaTelemetrySource(ctx, tx, source); err != nil {
				return err
			}
		}
		for _, snapshot := range report.QuotaSnapshots {
			if err := insertQuotaSnapshot(ctx, tx, snapshot); err != nil {
				return err
			}
		}
		for adapterID, seen := range currentByAdapter {
			rows, err := tx.Query(ctx, `SELECT provider_installation_id FROM provider_installations WHERE adapter_id = ?`, adapterID)
			if err != nil {
				return err
			}
			var stale []string
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return err
				}
				if !seen[id] {
					stale = append(stale, id)
				}
			}
			if err := rows.Close(); err != nil {
				return err
			}
			for _, id := range stale {
				if err := markInstallationStale(ctx, tx, id, nowText); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func markInstallationStale(ctx context.Context, tx storage.Tx, id, nowText string) error {
	var payload string
	if err := tx.QueryRow(ctx, `SELECT payload_json FROM provider_installations WHERE provider_installation_id = ?`, id).Scan(&payload); err != nil {
		return err
	}
	var installation ProviderInstallation
	if err := json.Unmarshal([]byte(payload), &installation); err != nil {
		return err
	}
	installation.RecordVersion++
	installation.UpdatedAt = nowText
	installation.UpdatedBy = defaultActorProvenance()
	installation.PolicyVersion = PolicyVersion
	installation.InstallationState = InstallationStale
	installation.FreshnessState = FreshnessStale
	installation.Confidence = ConfidenceStale
	nextPayload, err := json.Marshal(installation)
	if err != nil {
		return err
	}
	updatedBy, err := marshalJSONText(installation.UpdatedBy)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE provider_installations
		SET record_version = record_version + 1,
			updated_at = ?,
			updated_by_json = ?,
			policy_version = ?,
			installation_state = 'stale',
			freshness_state = 'stale',
			confidence = 'stale',
			payload_json = ?
		WHERE provider_installation_id = ?`, nowText, updatedBy, PolicyVersion, string(nextPayload), id)
	return err
}

func Load(ctx context.Context, store storage.Store) (Report, error) {
	if store == nil {
		return Report{}, errors.New("provider inventory load: storage store is required")
	}
	var installations []ProviderInstallation
	var probes []ProbeResult
	var profiles []AccountProfile
	var readiness []AuthReadiness
	var snapshots []ModelCatalogSnapshot
	var capabilities []ModelCapability
	var quotaSources []QuotaTelemetrySource
	var quotaSnapshots []QuotaSnapshot
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		rows, err := tx.Query(ctx, `SELECT payload_json FROM provider_installations ORDER BY adapter_id, discovery_order, provider_installation_id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var payload string
			if err := rows.Scan(&payload); err != nil {
				rows.Close()
				return err
			}
			var installation ProviderInstallation
			if err := json.Unmarshal([]byte(payload), &installation); err != nil {
				rows.Close()
				return err
			}
			installations = append(installations, installation)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT payload_json FROM provider_probe_results ORDER BY captured_at, probe_result_id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var payload string
			if err := rows.Scan(&payload); err != nil {
				rows.Close()
				return err
			}
			var probe ProbeResult
			if err := json.Unmarshal([]byte(payload), &probe); err != nil {
				rows.Close()
				return err
			}
			probes = append(probes, probe)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT payload_json FROM account_profiles ORDER BY adapter_id, account_profile_id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var payload string
			if err := rows.Scan(&payload); err != nil {
				rows.Close()
				return err
			}
			var profile AccountProfile
			if err := json.Unmarshal([]byte(payload), &profile); err != nil {
				rows.Close()
				return err
			}
			profiles = append(profiles, profile)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT payload_json FROM auth_readiness ORDER BY auth_readiness_id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var payload string
			if err := rows.Scan(&payload); err != nil {
				rows.Close()
				return err
			}
			var item AuthReadiness
			if err := json.Unmarshal([]byte(payload), &item); err != nil {
				rows.Close()
				return err
			}
			readiness = append(readiness, item)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT payload_json FROM model_catalog_snapshots ORDER BY adapter_id, model_catalog_snapshot_id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var payload string
			if err := rows.Scan(&payload); err != nil {
				rows.Close()
				return err
			}
			var item ModelCatalogSnapshot
			if err := json.Unmarshal([]byte(payload), &item); err != nil {
				rows.Close()
				return err
			}
			snapshots = append(snapshots, item)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT payload_json FROM model_capabilities ORDER BY adapter_id, model_catalog_snapshot_id, model_capability_id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var payload string
			if err := rows.Scan(&payload); err != nil {
				rows.Close()
				return err
			}
			var item ModelCapability
			if err := json.Unmarshal([]byte(payload), &item); err != nil {
				rows.Close()
				return err
			}
			capabilities = append(capabilities, item)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT payload_json FROM quota_telemetry_sources ORDER BY adapter_id, quota_source_id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var payload string
			if err := rows.Scan(&payload); err != nil {
				rows.Close()
				return err
			}
			var item QuotaTelemetrySource
			if err := json.Unmarshal([]byte(payload), &item); err != nil {
				rows.Close()
				return err
			}
			quotaSources = append(quotaSources, item)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT payload_json FROM quota_snapshots ORDER BY adapter_id, captured_at, quota_snapshot_id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var payload string
			if err := rows.Scan(&payload); err != nil {
				rows.Close()
				return err
			}
			var item QuotaSnapshot
			if err := json.Unmarshal([]byte(payload), &item); err != nil {
				rows.Close()
				return err
			}
			quotaSnapshots = append(quotaSnapshots, item)
		}
		return rows.Close()
	})
	if err != nil {
		return Report{}, err
	}
	now := store.Now()
	for i := range snapshots {
		snapshots[i], capabilities = markCatalogFreshness(snapshots[i], capabilities, now)
	}
	quotaSnapshots = markQuotaFreshness(LinkQuotaConflicts(quotaSnapshots), now)
	report := Report{
		SchemaVersion:         ProviderInventoryJSONSchema,
		GeneratedAt:           formatTime(now),
		Confidence:            confidenceForInventory(installations, probes),
		Installations:         installations,
		ProbeResults:          probes,
		AccountProfiles:       profiles,
		AuthReadiness:         readiness,
		ModelCatalogSnapshots: snapshots,
		ModelCapabilities:     capabilities,
		QuotaTelemetrySources: quotaSources,
		QuotaSnapshots:        quotaSnapshots,
		GapReasons:            []string{},
	}
	fingerprint, err := fingerprint(report)
	if err != nil {
		return Report{}, err
	}
	report.InventoryFingerprint = fingerprint
	return report, nil
}

func OpenDefaultStore(ctx context.Context, deps Deps, now func() time.Time) (storage.Store, error) {
	deps = normalizeDeps(deps)
	layout, err := home.Resolve(home.Deps{Getenv: deps.Getenv, UserHomeDir: deps.UserHomeDir})
	if err != nil {
		return nil, err
	}
	return storage.Open(ctx, storage.Options{Path: layout.DatabasePath(), Now: now})
}

type candidate struct {
	path   string
	source DiscoverySource
	order  int
}

func discoverCandidates(adapter AdapterDeclaration, deps Deps) []candidate {
	var out []candidate
	seen := map[string]bool{}
	add := func(path string, source DiscoverySource, order int) {
		canonical := canonicalPath(path, deps)
		key := strings.ToLower(canonical)
		if runtime.GOOS != "windows" {
			key = canonical
		}
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, candidate{path: canonical, source: source, order: order})
	}
	for index, path := range adapter.ExplicitPaths {
		if executableFile(path, deps) {
			add(path, DiscoveryExplicitConfig, index)
		}
	}
	order := len(out)
	for _, exe := range adapter.ExecutableNames {
		for _, path := range pathCandidates(exe, deps) {
			if executableFile(path, deps) {
				add(path, DiscoveryPath, order)
				order++
			}
		}
	}
	return out
}

// promoteUsableInstallations sets usable_for_invocation=yes when installation
// is installed and at least one bound auth-readiness record is ready.
// Spec: usable MUST NOT be true from install evidence alone.
func promoteUsableInstallations(installations []ProviderInstallation, readiness []AuthReadiness) {
	readyInstallations := map[string]bool{}
	for _, auth := range readiness {
		if auth.ReadinessState != ReadinessReady {
			continue
		}
		if auth.ProviderInstallationID == nil {
			continue
		}
		id := strings.TrimSpace(*auth.ProviderInstallationID)
		if id != "" {
			readyInstallations[id] = true
		}
	}
	for i := range installations {
		if installations[i].InstallationState != InstallationInstalled {
			continue
		}
		if !readyInstallations[installations[i].ProviderInstallationID] {
			continue
		}
		installations[i].UsableForInvocation = "yes"
	}
}

func inspectCandidate(ctx context.Context, adapter AdapterDeclaration, candidate candidate, now time.Time, deps Deps) (ProviderInstallation, ProbeResult) {
	platform := runtime.GOOS + "-" + runtime.GOARCH
	identity := executableIdentity(candidate.path, platform, deps)
	installationID := installationID(adapter.AdapterID, identity, candidate.path, platform)
	staleAfter := formatTime(now.Add(24 * time.Hour))
	argv := append([]string{candidate.path}, adapter.VersionArgv...)
	env := probeEnvironment(deps.Getenv)
	probe := baseProbe(adapter, now, deps)
	probe.ProviderInstallationID = &installationID
	probe.ProbeCommandID = "version"
	probe.ProbeMethod = ProbeMethodFixedCommand
	probe.Argv = redactArgv(argv)
	probe.EnvironmentKeys = environmentKeys(env)
	probe.TimeoutMS = int(InstallProbeTimeout / time.Millisecond)
	probe.StdoutLimitBytes = StdoutLimitBytes
	probe.StderrLimitBytes = StderrLimitBytes
	probe.CombinedOutputLimitBytes = CombinedLimitBytes
	probe.StaleAfter = staleAfter
	probe.Source = SourceDescriptor{Kind: "command", AdapterID: adapter.AdapterID, ProbeCommandID: "version", DiscoverySource: string(candidate.source), ExecutableName: filepath.Base(candidate.path)}
	probe.Evidence = EvidenceSummary{Kind: "bounded-version-command", CommandBounded: true, NoShell: true, RepositoryMutation: false, SecretMaterialRetained: false}

	result, err := deps.RunProbe(ctx, ProbeExecution{
		Argv:               argv,
		Env:                env,
		Timeout:            InstallProbeTimeout,
		StdoutLimitBytes:   StdoutLimitBytes,
		StderrLimitBytes:   StderrLimitBytes,
		CombinedLimitBytes: CombinedLimitBytes,
	})
	stdout, stdoutFindings := redactProviderOutput(result.Stdout)
	stderr, stderrFindings := redactProviderOutput(result.Stderr)
	probe.StdoutSummary = stdout
	probe.StderrSummary = stderr
	probe.SecretFindingCount = stdoutFindings + stderrFindings
	probe.TimedOut = result.TimedOut
	probe.Killed = result.Killed
	probe.ExitCode = &result.ExitCode
	version := ""
	state := InstallationInstalled
	versionConfidence := ConfidenceExact
	confidence := ConfidenceExact
	outcome := OutcomeInstalled
	var gaps []string
	var terminal string
	if err != nil || result.TimedOut || result.Killed {
		state = InstallationProbeFailed
		versionConfidence = ConfidenceUnknown
		confidence = ConfidenceUnknown
		outcome = OutcomeProbeFailed
		terminal = "ErrProbeExecutionFailed"
		gaps = append(gaps, "version-probe-failed")
	}
	if result.ExitCode != 0 && terminal == "" {
		state = InstallationInstalledUnusable
		versionConfidence = ConfidenceUnknown
		confidence = ConfidenceUnknown
		outcome = OutcomeInstalledUnusable
		terminal = "ErrProbeNonZeroExit"
		gaps = append(gaps, "version-probe-nonzero-exit")
	}
	if terminal == "" {
		version = parseVersion(result.Stdout, result.Stderr)
	}
	if terminal == "" && malformedAdapterVersion(adapter, version) {
		version = ""
		versionConfidence = ConfidenceUnknown
		state = InstallationInstalledUnusable
		confidence = ConfidenceUnknown
		outcome = OutcomeInstalledUnusable
		terminal = "ErrProbeUnparseableVersion"
	}
	if version == "" {
		versionConfidence = ConfidenceUnknown
		gaps = append(gaps, "version-unknown")
		if terminal == "" {
			state = InstallationInstalledUnusable
			confidence = ConfidenceUnknown
			outcome = OutcomeInstalledUnusable
			terminal = "ErrProbeUnparseableVersion"
		}
	}
	if terminal == "" && unsupportedAdapterVersion(adapter, version) {
		state = InstallationInstalledUnusable
		confidence = ConfidenceUnknown
		outcome = OutcomeInstalledUnusable
		terminal = "ErrUnsupportedVersion"
		gaps = append(gaps, "unsupported-version")
	}
	probe.Outcome = outcome
	probe.Confidence = confidence
	probe.GapReasons = gaps
	probe.TerminalErrorCode = terminal
	if len(probe.GapReasons) == 0 {
		probe.GapReasons = []string{}
	}
	installation := ProviderInstallation{
		SchemaVersion:          ProviderInstallationSchema,
		RecordVersion:          1,
		Scope:                  "machine",
		ProjectID:              nil,
		ProviderInstallationID: installationID,
		AdapterID:              adapter.AdapterID,
		AdapterDeclarationID:   adapterDeclarationID(adapter),
		ProviderDisplayName:    adapter.DisplayName,
		ExecutableName:         filepath.Base(candidate.path),
		ExecutableIdentity:     identity,
		CanonicalPathRedacted:  redactPath(candidate.path),
		DiscoverySource:        candidate.source,
		DiscoveryOrder:         candidate.order,
		Platform:               platform,
		Version:                version,
		VersionConfidence:      versionConfidence,
		LatestProbeResultID:    probe.ProbeResultID,
		InstallationState:      state,
		UsableForInvocation:    "unknown",
		KnownLimitations:       append([]string(nil), adapter.KnownLimitations...),
		CreatedAt:              formatTime(now),
		UpdatedAt:              formatTime(now),
		CreatedBy:              defaultActorProvenance(),
		UpdatedBy:              defaultActorProvenance(),
		Host:                   hostProvenance(),
		PolicyVersion:          PolicyVersion,
		CapturedAt:             formatTime(now),
		StaleAfter:             staleAfter,
		FreshnessState:         FreshnessFresh,
		Confidence:             confidence,
		SideEffectClass:        "local-read",
		Classification:         "sensitive-path",
		Source:                 SourceDescriptor{Kind: "executable-discovery", AdapterID: adapter.AdapterID, DiscoverySource: string(candidate.source), ExecutableName: filepath.Base(candidate.path)},
		Evidence:               EvidenceSummary{Kind: "bounded-local-installation-probe", CommandBounded: true, NoShell: true, RepositoryMutation: false, SecretMaterialRetained: false},
		GapReasons:             gaps,
		TerminalErrorCode:      terminal,
	}
	if len(installation.GapReasons) == 0 {
		installation.GapReasons = []string{}
	}
	return installation, probe
}

func inspectAuthReadiness(ctx context.Context, discovery *discoveryContext, adapter AdapterDeclaration, candidate candidate, installationID string, now time.Time, deps Deps) ([]AccountProfile, []AuthReadiness, *ProbeResult) {
	if len(adapter.AuthProbeCommand) > 0 {
		return inspectAuthCommand(ctx, discovery, adapter, candidate, installationID, now, deps)
	}
	if len(adapter.AuthArtifactPaths) > 0 {
		profiles, readiness, probe := inspectAuthArtifacts(adapter, installationID, now, deps)
		if len(adapter.AuthEnvironmentNames) == 0 || !authArtifactOnlyNotFound(readiness) {
			return profiles, readiness, probe
		}
	}
	if len(adapter.AuthEnvironmentNames) > 0 {
		return inspectAuthEnvironment(adapter, installationID, now, deps)
	}
	readiness := unsupportedAuthReadiness(adapter, &installationID, now, firstNonEmpty(adapter.AuthUnsupportedReason, "auth-readiness-unsupported"))
	return nil, []AuthReadiness{readiness}, nil
}

func authArtifactOnlyNotFound(readiness []AuthReadiness) bool {
	return len(readiness) == 1 && readiness[0].EvidenceKind == EvidenceFileExistence && stringSliceContains(readiness[0].GapReasons, "auth-artifact-not-found")
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func inspectAuthCommand(ctx context.Context, discovery *discoveryContext, adapter AdapterDeclaration, candidate candidate, installationID string, now time.Time, deps Deps) ([]AccountProfile, []AuthReadiness, *ProbeResult) {
	probe := baseProbe(adapter, now, deps)
	probe.ProviderInstallationID = &installationID
	probe.ProbeKind = "auth-readiness"
	probe.ProbeCommandID = authProbeCommandID(adapter)
	probe.ProbeMethod = ProbeMethodFixedCommand
	if adapter.AuthProbeParser == "claude-auth-status-json" {
		probe.ProbeMethod = ProbeMethodMachineJSON
	}
	probe.NetworkDeclared = adapter.AuthProbeMayNetwork || adapter.MayNetwork
	probe.NetworkPermission = networkPermissionFor(discovery, adapter, NetworkPurposeAuthReadiness, probe.NetworkDeclared)
	probe.TimeoutMS = int(AuthProbeTimeout / time.Millisecond)
	probe.StaleAfter = formatTime(now.Add(30 * time.Minute))
	argv := authProbeArgv(adapter, candidate.path)
	env := probeEnvironment(deps.Getenv)
	probe.Argv = redactArgv(argv)
	probe.EnvironmentKeys = environmentKeys(env)
	probe.Source = SourceDescriptor{Kind: "command", AdapterID: adapter.AdapterID, ProbeCommandID: probe.ProbeCommandID, DiscoverySource: string(candidate.source), ExecutableName: filepath.Base(candidate.path)}
	probe.Evidence = EvidenceSummary{Kind: "bounded-auth-readiness-command", CommandBounded: true, NoShell: true, RepositoryMutation: false, SecretMaterialRetained: false}

	if probe.NetworkDeclared && probe.NetworkPermission != NetworkGranted {
		probe.Outcome = OutcomeProbeFailed
		probe.Confidence = ConfidenceUnknown
		probe.FreshnessState = FreshnessNotApplicable
		probe.GapReasons = []string{"network-permission-denied"}
		probe.TerminalErrorCode = "ErrNetworkPermissionDenied"
		readiness := baseAuthReadiness(adapter, &installationID, nil, now)
		readiness.EvidenceKind = EvidenceNotRun
		readiness.GapReasons = []string{"network-permission-denied"}
		readiness.TerminalErrorCode = "ErrNetworkPermissionDenied"
		readiness.UnsupportedReason = "network permission denied for declared network auth probe"
		return nil, []AuthReadiness{readiness}, &probe
	}

	result, err := sharedProbeResult(ctx, discovery, deps, ProbeExecution{
		Argv:               argv,
		Env:                env,
		Timeout:            AuthProbeTimeout,
		StdoutLimitBytes:   StdoutLimitBytes,
		StderrLimitBytes:   StderrLimitBytes,
		CombinedLimitBytes: CombinedLimitBytes,
	})
	stdout, stdoutFindings := redactProviderOutput(result.Stdout)
	stderr, stderrFindings := redactProviderOutput(result.Stderr)
	probe.StdoutSummary = stdout
	probe.StderrSummary = stderr
	probe.SecretFindingCount = stdoutFindings + stderrFindings
	probe.TimedOut = result.TimedOut
	probe.Killed = result.Killed
	probe.ExitCode = &result.ExitCode

	if err != nil || result.TimedOut || result.Killed {
		probe.Outcome = OutcomeProbeFailed
		probe.Confidence = ConfidenceUnknown
		probe.GapReasons = []string{"auth-probe-failed"}
		probe.TerminalErrorCode = "ErrAuthProbeExecutionFailed"
		if result.TimedOut {
			probe.GapReasons = []string{"auth-probe-timeout"}
			probe.TerminalErrorCode = "ErrAuthProbeTimeout"
		}
		readiness := baseAuthReadiness(adapter, &installationID, nil, now)
		readiness.EvidenceKind = authEvidenceKind(adapter)
		readiness.GapReasons = append([]string(nil), probe.GapReasons...)
		readiness.TerminalErrorCode = probe.TerminalErrorCode
		return nil, []AuthReadiness{readiness}, &probe
	}

	parsed := parseAuthStatus(adapter, result.Stdout+"\n"+result.Stderr, result.ExitCode)
	probe.Outcome = OutcomeInstalled
	probe.Confidence = ConfidenceExact
	probe.setParsedFields(map[string]string{
		"profile_count": fmt.Sprintf("%d", len(parsed)),
		"parser":        firstNonEmpty(adapter.AuthProbeParser, "allowlisted-text-auth-status"),
	})
	if result.ExitCode != 0 {
		probe.Confidence = ConfidenceUnknown
		probe.GapReasons = []string{"auth-probe-nonzero-exit"}
		probe.TerminalErrorCode = "ErrAuthProbeNonZeroExit"
	}
	if len(parsed) == 0 {
		readiness := baseAuthReadiness(adapter, &installationID, nil, now)
		readiness.EvidenceKind = authEvidenceKind(adapter)
		readiness.GapReasons = []string{"auth-status-unrecognized"}
		readiness.TerminalErrorCode = "ErrAuthStatusUnrecognized"
		probe.GapReasons = append(probe.GapReasons, "auth-status-unrecognized")
		if result.ExitCode != 0 {
			readiness.GapReasons = []string{"auth-probe-nonzero-exit", "auth-status-unrecognized"}
		}
		return nil, []AuthReadiness{readiness}, &probe
	}

	profiles, readiness := authRecordsFromParsed(adapter, installationID, parsed, now)
	if result.ExitCode != 0 {
		for i := range readiness {
			readiness[i].GapReasons = append(readiness[i].GapReasons, "auth-probe-nonzero-exit")
			if readiness[i].ReadinessState == ReadinessReady {
				readiness[i].ReadinessState = ReadinessUnknown
				readiness[i].ReadinessConfidence = ConfidenceUnknown
				readiness[i].Confidence = ConfidenceUnknown
				readiness[i].TerminalErrorCode = "ErrAuthProbeNonZeroExit"
			}
		}
	}
	return profiles, readiness, &probe
}

func inspectAuthArtifacts(adapter AdapterDeclaration, installationID string, now time.Time, deps Deps) ([]AccountProfile, []AuthReadiness, *ProbeResult) {
	for _, path := range adapter.AuthArtifactPaths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		resolved, resolveErr := expandAuthArtifactPath(path, deps)
		var info os.FileInfo
		var err error
		if resolveErr == nil {
			info, err = deps.Lstat(resolved)
		} else {
			err = resolveErr
		}
		readiness := baseAuthReadiness(adapter, &installationID, nil, now)
		readiness.EvidenceKind = EvidenceFileExistence
		readiness.Source = SourceDescriptor{Kind: "file-existence", AdapterID: adapter.AdapterID}
		readiness.Evidence = EvidenceSummary{Kind: "lstat-only-auth-artifact", CommandBounded: true, NoShell: true, RepositoryMutation: false, SecretMaterialRetained: false}
		readiness.UnsupportedReason = safeSummary(adapter.AuthUnsupportedReason)
		switch {
		case err == nil && !info.IsDir():
			readiness.ReadinessState = ReadinessUnknown
			readiness.ReadinessConfidence = ConfidenceUnknown
			readiness.Confidence = ConfidenceUnknown
			readiness.GapReasons = []string{"auth-artifact-exists-readiness-unknown"}
		case errors.Is(err, os.ErrNotExist):
			readiness.ReadinessState = ReadinessNotAuthenticated
			readiness.ReadinessConfidence = ConfidenceEstimated
			readiness.Confidence = ConfidenceEstimated
			readiness.RefreshRequired = true
			readiness.GapReasons = []string{"auth-artifact-not-found"}
		default:
			readiness.GapReasons = []string{"auth-artifact-inaccessible"}
			readiness.TerminalErrorCode = "ErrAuthArtifactInaccessible"
		}
		return nil, []AuthReadiness{readiness}, nil
	}
	readiness := unsupportedAuthReadiness(adapter, &installationID, now, firstNonEmpty(adapter.AuthUnsupportedReason, "auth-readiness-unsupported"))
	return nil, []AuthReadiness{readiness}, nil
}

func inspectAuthEnvironment(adapter AdapterDeclaration, installationID string, now time.Time, deps Deps) ([]AccountProfile, []AuthReadiness, *ProbeResult) {
	lookup := deps.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	for _, name := range adapter.AuthEnvironmentNames {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		_, ok := lookup(name)
		readiness := baseAuthReadiness(adapter, &installationID, nil, now)
		readiness.EvidenceKind = EvidenceEnvironmentName
		readiness.Source = SourceDescriptor{Kind: "environment-name", AdapterID: adapter.AdapterID}
		readiness.Evidence = EvidenceSummary{Kind: "environment-name-only-auth-reference", CommandBounded: true, NoShell: true, RepositoryMutation: false, SecretMaterialRetained: false}
		if ok {
			readiness.GapReasons = []string{"environment-reference-present-readiness-unknown"}
			readiness.UnsupportedReason = safeSummary(adapter.AuthUnsupportedReason)
		} else {
			readiness.ReadinessState = ReadinessNotAuthenticated
			readiness.ReadinessConfidence = ConfidenceEstimated
			readiness.Confidence = ConfidenceEstimated
			readiness.RefreshRequired = true
			readiness.GapReasons = []string{"environment-reference-not-found"}
		}
		return nil, []AuthReadiness{readiness}, nil
	}
	readiness := unsupportedAuthReadiness(adapter, &installationID, now, firstNonEmpty(adapter.AuthUnsupportedReason, "auth-readiness-unsupported"))
	return nil, []AuthReadiness{readiness}, nil
}

func authProbeArgv(adapter AdapterDeclaration, executablePath string) []string {
	command := append([]string(nil), adapter.AuthProbeCommand...)
	if len(command) == 0 {
		return nil
	}
	command[0] = executablePath
	return command
}

func authProbeCommandID(adapter AdapterDeclaration) string {
	parser := strings.TrimSpace(adapter.AuthProbeParser)
	if parser != "" {
		return parser
	}
	return "auth-readiness"
}

func authEvidenceKind(adapter AdapterDeclaration) EvidenceKind {
	if adapter.AuthProbeParser == "claude-auth-status-json" {
		return EvidenceMachineStatus
	}
	return EvidenceStatusCommand
}

func expandAuthArtifactPath(path string, deps Deps) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("auth artifact path is empty")
	}
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		homeDir, err := deps.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			path = homeDir
		} else {
			path = filepath.Join(homeDir, path[2:])
		}
	}
	return canonicalPath(path, deps), nil
}

type parsedAuthStatus struct {
	ReferenceHash string
	Display       string
	AccountKind   string
	TenantHash    string
	EvidenceKind  EvidenceKind
	State         ReadinessState
	ScopeState    AuthorizationScopeState
	ScopeSummary  string
	Refresh       bool
	Gaps          []string
}

func parseAuthStatus(adapter AdapterDeclaration, output string, exitCode int) []parsedAuthStatus {
	switch adapter.AuthProbeParser {
	case "claude-auth-status-json":
		return parseClaudeAuthStatus(output, exitCode)
	case "grok-models":
		return parseGrokModelsAuthStatus(output, exitCode)
	case "agy-models":
		return parseAgyModelsAuthStatus(output, exitCode)
	default:
		return parseTextAuthStatus(adapter.AdapterID, output)
	}
}

// parseAgyModelsAuthStatus treats a successful `agy models` listing as auth-ready
// reachability evidence (authorization scope still unknown). Empty/error output
// stays not-authenticated or unknown without inventing capacity.
func parseAgyModelsAuthStatus(output string, exitCode int) []parsedAuthStatus {
	lower := strings.ToLower(output)
	if networkFailureText(lower) {
		return []parsedAuthStatus{{
			ReferenceHash: "sha256:" + hashHex("antigravity", "models-status", "network"),
			Display:       "antigravity-profile",
			EvidenceKind:  EvidenceStatusCommand,
			State:         ReadinessUnknown,
			ScopeState:    AuthorizationUnknown,
			ScopeSummary:  "model inventory network unavailable",
			Gaps:          []string{"catalog-network-unavailable"},
		}}
	}
	// Count non-empty model id lines (agy models prints one id per line).
	lines := 0
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Skip help-ish noise.
		if strings.HasPrefix(line, "Usage:") || strings.HasPrefix(line, "Available") {
			continue
		}
		if strings.Contains(strings.ToLower(line), "not logged") || strings.Contains(strings.ToLower(line), "not authenticated") {
			return []parsedAuthStatus{{
				ReferenceHash: "sha256:" + hashHex("antigravity", "models-status", "not-auth"),
				Display:       "antigravity-profile",
				EvidenceKind:  EvidenceStatusCommand,
				State:         ReadinessNotAuthenticated,
				ScopeState:    AuthorizationNotApplicable,
				ScopeSummary:  "provider reports not authenticated",
				Gaps:          []string{"provider-reports-not-authenticated"},
			}}
		}
		lines++
	}
	if exitCode == 0 && lines > 0 {
		return []parsedAuthStatus{{
			ReferenceHash: "sha256:" + hashHex("antigravity", "models-status", "ready", fmt.Sprintf("%d", lines)),
			Display:       "antigravity-profile",
			EvidenceKind:  EvidenceStatusCommand,
			State:         ReadinessReady,
			ScopeState:    AuthorizationUnknown,
			ScopeSummary:  "model authorization unknown",
			Gaps:          []string{"authorization-scope-unknown"},
		}}
	}
	if exitCode != 0 {
		return []parsedAuthStatus{{
			ReferenceHash: "sha256:" + hashHex("antigravity", "models-status", "exit", fmt.Sprintf("%d", exitCode)),
			Display:       "antigravity-profile",
			EvidenceKind:  EvidenceStatusCommand,
			State:         ReadinessUnknown,
			ScopeState:    AuthorizationUnknown,
			ScopeSummary:  "agy models nonzero exit",
			Gaps:          []string{"auth-probe-nonzero-exit"},
		}}
	}
	return parseTextAuthStatus("antigravity", output)
}

func parseGrokModelsAuthStatus(output string, exitCode int) []parsedAuthStatus {
	lower := strings.ToLower(output)
	state, scope, summary, refresh, gaps, recognized := classifyAuthLine(output)
	if networkFailureText(lower) {
		state = ReadinessUnknown
		scope = AuthorizationUnknown
		summary = "model inventory network unavailable"
		refresh = false
		gaps = []string{"catalog-network-unavailable"}
		recognized = true
	}
	if !recognized && exitCode == 0 && len(parseGrokModelEntries(output)) > 0 {
		state = ReadinessReady
		scope = AuthorizationUnknown
		summary = "model authorization unknown"
		gaps = []string{"authorization-scope-unknown"}
		recognized = true
	}
	if !recognized {
		return parseTextAuthStatus("grok", output)
	}
	return []parsedAuthStatus{{
		ReferenceHash: "sha256:" + hashHex("grok", "models-status", string(state), strings.Join(gaps, ",")),
		Display:       "grok-profile",
		EvidenceKind:  EvidenceStatusCommand,
		State:         state,
		ScopeState:    scope,
		ScopeSummary:  summary,
		Refresh:       refresh,
		Gaps:          gaps,
	}}
}

func parseTextAuthStatus(adapterID, output string) []parsedAuthStatus {
	lines := strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n")
	var parsed []parsedAuthStatus
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		state, scope, summary, refresh, gaps, recognizedState := classifyAuthLine(line)
		display, recognizedDisplay := safeProfileDisplay(line)
		referenceHash := "sha256:" + hashHex(adapterID, "provider-status-command", line)
		if !recognizedDisplay {
			display = "profile-" + hashBase32(referenceHash)[:8]
		}
		if !recognizedState && !recognizedDisplay {
			continue
		}
		if !recognizedState {
			state = ReadinessUnknown
			scope = AuthorizationUnknown
			gaps = append(gaps, "auth-status-state-unrecognized")
		}
		parsed = append(parsed, parsedAuthStatus{
			ReferenceHash: referenceHash,
			Display:       display,
			EvidenceKind:  EvidenceStatusCommand,
			State:         state,
			ScopeState:    scope,
			ScopeSummary:  summary,
			Refresh:       refresh,
			Gaps:          gaps,
		})
	}
	return parsed
}

func parseClaudeAuthStatus(output string, exitCode int) []parsedAuthStatus {
	type profileJSON struct {
		ID               string `json:"id"`
		Email            string `json:"email"`
		Display          string `json:"display"`
		OrgID            string `json:"orgId"`
		OrgName          string `json:"orgName"`
		AuthMethod       string `json:"authMethod"`
		APIProvider      string `json:"apiProvider"`
		SubscriptionType string `json:"subscriptionType"`
		Status           string `json:"status"`
	}
	type statusJSON struct {
		LoggedIn         bool          `json:"loggedIn"`
		Email            string        `json:"email"`
		Display          string        `json:"display"`
		OrgID            string        `json:"orgId"`
		OrgName          string        `json:"orgName"`
		AuthMethod       string        `json:"authMethod"`
		APIProvider      string        `json:"apiProvider"`
		SubscriptionType string        `json:"subscriptionType"`
		Status           string        `json:"status"`
		Profiles         []profileJSON `json:"profiles"`
	}
	var status statusJSON
	if err := json.Unmarshal([]byte(output), &status); err != nil {
		return parseTextAuthStatus("claude", output)
	}
	profiles := status.Profiles
	if len(profiles) == 0 && firstNonEmpty(status.Email, status.Display, status.OrgID, status.OrgName, status.AuthMethod, status.APIProvider, status.SubscriptionType, status.Status) != "" {
		profiles = []profileJSON{{
			Email:            status.Email,
			Display:          status.Display,
			OrgID:            status.OrgID,
			OrgName:          status.OrgName,
			AuthMethod:       status.AuthMethod,
			APIProvider:      status.APIProvider,
			SubscriptionType: status.SubscriptionType,
			Status:           status.Status,
		}}
	}
	if len(profiles) == 0 {
		state := ReadinessNotAuthenticated
		confGap := "provider-reports-not-authenticated"
		if status.LoggedIn && exitCode == 0 {
			state = ReadinessUnknown
			confGap = "auth-status-profile-unavailable"
		}
		return []parsedAuthStatus{{
			ReferenceHash: "sha256:" + hashHex("claude", "machine-readable-status", "default"),
			Display:       "profile-" + hashBase32("claude", "machine-readable-status", "default")[:8],
			EvidenceKind:  EvidenceMachineStatus,
			State:         state,
			ScopeState:    AuthorizationNotApplicable,
			ScopeSummary:  "provider reports not authenticated",
			Refresh:       state == ReadinessNotAuthenticated,
			Gaps:          []string{confGap},
		}}
	}
	parsed := make([]parsedAuthStatus, 0, len(profiles))
	for index, profile := range profiles {
		state, scope, summary, refresh, gaps, _ := classifyAuthLine(firstNonEmpty(profile.Status, status.Status, boolAuthStatus(status.LoggedIn, exitCode)))
		reference := firstNonEmpty(profile.ID, profile.Email, profile.OrgID, profile.OrgName, fmt.Sprintf("claude-profile-%d", index))
		display := safeSummary(firstNonEmpty(profile.Display, profile.Email, profile.OrgName, fmt.Sprintf("profile-%d", index+1)))
		display = emailPattern.ReplaceAllStringFunc(display, redactEmail)
		if strings.TrimSpace(display) == "" || secretLike(display) {
			display = "profile-" + hashBase32("claude", reference)[:8]
		}
		summaryParts := []string{}
		if profile.APIProvider != "" {
			summaryParts = append(summaryParts, "api_provider="+boundedToken(safeSummary(profile.APIProvider), 48))
		}
		if profile.SubscriptionType != "" {
			summaryParts = append(summaryParts, "subscription="+boundedToken(safeSummary(profile.SubscriptionType), 48))
		}
		if len(summaryParts) > 0 {
			summary = strings.Join(summaryParts, "; ")
		}
		tenantHash := ""
		if tenant := firstNonEmpty(profile.OrgID, profile.OrgName); tenant != "" {
			tenantHash = "sha256:" + hashHex("claude", "tenant", tenant)
		}
		parsed = append(parsed, parsedAuthStatus{
			ReferenceHash: "sha256:" + hashHex("claude", "machine-readable-status", reference),
			Display:       display,
			AccountKind:   "user",
			TenantHash:    tenantHash,
			EvidenceKind:  EvidenceMachineStatus,
			State:         state,
			ScopeState:    scope,
			ScopeSummary:  summary,
			Refresh:       refresh,
			Gaps:          gaps,
		})
	}
	return parsed
}

func boolAuthStatus(loggedIn bool, exitCode int) string {
	if loggedIn && exitCode == 0 {
		return "ready"
	}
	return "not authenticated"
}

func classifyAuthLine(line string) (ReadinessState, AuthorizationScopeState, string, bool, []string, bool) {
	lower := strings.ToLower(line)
	switch {
	case strings.Contains(lower, "expired") || strings.Contains(lower, "revoked") || strings.Contains(lower, "refresh required"):
		return ReadinessExpired, AuthorizationUnknown, "", true, []string{"provider-reports-expired"}, true
	case strings.Contains(lower, "not authenticated") || strings.Contains(lower, "unauthenticated") || strings.Contains(lower, "logged out") || strings.Contains(lower, "not logged in") || strings.Contains(lower, "login required"):
		return ReadinessNotAuthenticated, AuthorizationNotApplicable, "", true, []string{"provider-reports-not-authenticated"}, true
	case strings.Contains(lower, "not authorized") || strings.Contains(lower, "forbidden") || strings.Contains(lower, "permission denied") || strings.Contains(lower, "access denied") || strings.Contains(lower, "partial") || strings.Contains(lower, "limited scope") || strings.Contains(lower, "limited authorization"):
		return ReadinessUnknown, AuthorizationPartial, "partial authorization reported by provider status", false, []string{"provider-authorization-partial"}, true
	case strings.Contains(lower, "authenticated") || strings.Contains(lower, "authorized") || strings.Contains(lower, "logged in") || strings.Contains(lower, "ready"):
		return ReadinessReady, AuthorizationUnknown, "model authorization unknown", false, []string{"authorization-scope-unknown"}, true
	default:
		return ReadinessUnknown, AuthorizationUnknown, "", false, nil, false
	}
}

var (
	emailPattern       = regexp.MustCompile(`(?i)\b[A-Z0-9._%+\-]{1,64}@[A-Z0-9.\-]{1,190}\.[A-Z]{2,24}\b`)
	shortHandlePattern = regexp.MustCompile(`\b[A-Za-z0-9._-]{1,48}\b`)
)

func safeProfileDisplay(line string) (string, bool) {
	if email := emailPattern.FindString(line); email != "" {
		return redactEmail(email), true
	}
	tokens := shortHandlePattern.FindAllString(line, -1)
	for i, token := range tokens {
		lower := strings.ToLower(token)
		if lower == "profile" || lower == "account" || lower == "user" || lower == "tenant" || lower == "project" || lower == "workspace" || lower == "as" {
			for _, next := range tokens[i+1:] {
				nextLower := strings.ToLower(next)
				if !authStatusWord(nextLower) {
					return next, true
				}
			}
		}
	}
	return "", false
}

func authStatusWord(token string) bool {
	switch token {
	case "authenticated", "authorized", "ready", "logged", "in", "out", "not", "login", "required", "expired", "revoked", "refresh", "partial", "limited", "scope", "authorization", "profile", "account", "user", "tenant", "project", "workspace", "models", "model", "as":
		return true
	default:
		return false
	}
}

func redactEmail(email string) string {
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" || domain == "" {
		return "email-profile"
	}
	prefix := local[:1]
	return prefix + "***@" + strings.ToLower(domain)
}

func authRecordsFromParsed(adapter AdapterDeclaration, installationID string, parsed []parsedAuthStatus, now time.Time) ([]AccountProfile, []AuthReadiness) {
	profiles := make([]AccountProfile, 0, len(parsed))
	readiness := make([]AuthReadiness, 0, len(parsed))
	displayCounts := map[string]int{}
	for _, item := range parsed {
		displayCounts[item.Display]++
	}
	for _, item := range parsed {
		accountID := accountProfileID(adapter.AdapterID, string(ProfileSourceStatusCommand), item.ReferenceHash)
		auth := baseAuthReadiness(adapter, &installationID, &accountID, now)
		auth.ReadinessState = item.State
		auth.ReadinessConfidence = readinessConfidence(item.State)
		auth.EvidenceKind = item.EvidenceKind
		if auth.EvidenceKind == "" {
			auth.EvidenceKind = EvidenceStatusCommand
		}
		auth.AuthorizationScopeState = item.ScopeState
		auth.AuthorizationScopeSummary = safeSummary(item.ScopeSummary)
		auth.RefreshRequired = item.Refresh
		auth.GapReasons = append([]string(nil), item.Gaps...)
		auth.Confidence = auth.ReadinessConfidence
		display := safeSummary(item.Display)
		if strings.TrimSpace(display) == "" {
			display = "profile-" + hashBase32(accountID)[:8]
		}
		collisionSet := []string{}
		selection := SelectionDefault
		if len(parsed) > 1 {
			selection = SelectionCandidate
		}
		if strings.TrimSpace(adapter.SelectedAccountProfileID) != "" {
			if accountID == strings.TrimSpace(adapter.SelectedAccountProfileID) {
				selection = SelectionExplicit
			} else {
				selection = SelectionSuperseded
			}
		}
		if displayCounts[item.Display] > 1 {
			display = item.Display + "-" + hashBase32(accountID)[:6]
			for _, other := range parsed {
				otherID := accountProfileID(adapter.AdapterID, string(ProfileSourceStatusCommand), other.ReferenceHash)
				if otherID != accountID && other.Display == item.Display {
					collisionSet = append(collisionSet, otherID)
				}
			}
		}
		profile := baseAccountProfile(adapter, &installationID, now)
		profile.AccountProfileID = accountID
		profile.ProfileSource = ProfileSourceStatusCommand
		profile.ProfileReferenceHash = item.ReferenceHash
		profile.ProfileDisplay = display
		profile.ProviderAccountKind = safeSummary(item.AccountKind)
		profile.TenantOrProjectReferenceHash = item.TenantHash
		profile.SelectionState = selection
		profile.LatestAuthReadinessID = auth.AuthReadinessID
		profile.AllowedScopeSummary = auth.AuthorizationScopeSummary
		profile.CollisionSet = collisionSet
		profile.Confidence = auth.ReadinessConfidence
		profile.GapReasons = append([]string(nil), item.Gaps...)
		profiles = append(profiles, profile)
		readiness = append(readiness, auth)
	}
	return profiles, readiness
}

func baseAccountProfile(adapter AdapterDeclaration, installationID *string, now time.Time) AccountProfile {
	return AccountProfile{
		SchemaVersion:          AccountProfileSchema,
		RecordVersion:          1,
		Scope:                  "machine",
		ProjectID:              nil,
		AdapterID:              adapter.AdapterID,
		ProviderInstallationID: installationID,
		ProfileSource:          ProfileSourceUnknown,
		SelectionState:         SelectionUnknown,
		CollisionSet:           []string{},
		CreatedAt:              formatTime(now),
		UpdatedAt:              formatTime(now),
		CreatedBy:              defaultActorProvenance(),
		UpdatedBy:              defaultActorProvenance(),
		Host:                   hostProvenance(),
		PolicyVersion:          PolicyVersion,
		CapturedAt:             formatTime(now),
		FreshnessState:         FreshnessFresh,
		Confidence:             ConfidenceUnknown,
		SideEffectClass:        "local-read",
		Classification:         "local-provider-reference",
		Source:                 SourceDescriptor{Kind: "auth-readiness", AdapterID: adapter.AdapterID},
		Evidence:               EvidenceSummary{Kind: "credential-blind-profile-reference", CommandBounded: true, NoShell: true, RepositoryMutation: false, SecretMaterialRetained: false},
		GapReasons:             []string{},
	}
}

func baseAuthReadiness(adapter AdapterDeclaration, installationID, accountID *string, now time.Time) AuthReadiness {
	return AuthReadiness{
		SchemaVersion:           AuthReadinessSchema,
		RecordVersion:           1,
		Scope:                   "machine",
		ProjectID:               nil,
		AuthReadinessID:         randomAuthReadinessID(adapter.AdapterID, installationID, accountID, now),
		AdapterID:               adapter.AdapterID,
		ProviderInstallationID:  installationID,
		AccountProfileID:        accountID,
		ReadinessState:          ReadinessUnknown,
		ReadinessConfidence:     ConfidenceUnknown,
		EvidenceKind:            EvidenceUnsupported,
		AuthorizationScopeState: AuthorizationUnknown,
		RefreshRequired:         false,
		CreatedAt:               formatTime(now),
		UpdatedAt:               formatTime(now),
		CreatedBy:               defaultActorProvenance(),
		UpdatedBy:               defaultActorProvenance(),
		Host:                    hostProvenance(),
		PolicyVersion:           PolicyVersion,
		CapturedAt:              formatTime(now),
		FreshnessState:          FreshnessFresh,
		Confidence:              ConfidenceUnknown,
		SideEffectClass:         "local-read",
		Classification:          "local-provider-reference",
		Source:                  SourceDescriptor{Kind: "auth-readiness", AdapterID: adapter.AdapterID},
		Evidence:                EvidenceSummary{Kind: "credential-blind-auth-readiness", CommandBounded: true, NoShell: true, RepositoryMutation: false, SecretMaterialRetained: false},
		GapReasons:              []string{},
	}
}

func unsupportedAuthReadiness(adapter AdapterDeclaration, installationID *string, now time.Time, reason string) AuthReadiness {
	readiness := baseAuthReadiness(adapter, installationID, nil, now)
	readiness.EvidenceKind = EvidenceUnsupported
	readiness.UnsupportedReason = reason
	readiness.GapReasons = []string{reason}
	return readiness
}

func readinessConfidence(state ReadinessState) Confidence {
	switch state {
	case ReadinessReady, ReadinessNotAuthenticated, ReadinessExpired:
		return ConfidenceExact
	default:
		return ConfidenceUnknown
	}
}

func accountProfileID(adapterID, source, referenceHash string) string {
	return "acct_" + hashBase32(adapterID, source, referenceHash)[:32]
}

func randomAuthReadinessID(adapterID string, installationID, accountID *string, now time.Time) string {
	install := ""
	if installationID != nil {
		install = *installationID
	}
	account := ""
	if accountID != nil {
		account = *accountID
	}
	return "auth_" + hashBase32(adapterID, install, account, now.Format(time.RFC3339Nano), randomProbeID())[:26]
}

func absentProbe(adapter AdapterDeclaration, now time.Time, deps Deps) ProbeResult {
	probe := baseProbe(adapter, now, deps)
	probe.ProbeMethod = ProbeMethodLookPath
	probe.Outcome = OutcomeNotInstalled
	probe.Confidence = ConfidenceUnavailable
	probe.FreshnessState = FreshnessNotApplicable
	probe.Source = SourceDescriptor{Kind: "path-lookup", AdapterID: adapter.AdapterID, ExecutableName: strings.Join(adapter.ExecutableNames, ",")}
	probe.Evidence = EvidenceSummary{Kind: "declared-executable-not-found", CommandBounded: true, NoShell: true, RepositoryMutation: false, SecretMaterialRetained: false}
	probe.GapReasons = []string{"executable-not-found"}
	return probe
}

func inspectCatalogCommand(ctx context.Context, discovery *discoveryContext, adapter AdapterDeclaration, candidate candidate, installation ProviderInstallation, now time.Time, deps Deps) (ModelCatalogSnapshot, []ModelCapability, ProbeResult) {
	installationID := installation.ProviderInstallationID
	probe := baseProbe(adapter, now, deps)
	probe.ProviderInstallationID = &installationID
	probe.ProbeKind = "catalog"
	probe.ProbeCommandID = catalogProbeCommandID(adapter)
	probe.ProbeMethod = ProbeMethodMachineJSON
	probe.TimeoutMS = int(AuthProbeTimeout / time.Millisecond)
	probe.StaleAfter = formatTime(now.Add(30 * time.Minute))
	probe.NetworkDeclared = adapter.CatalogProbeMayNetwork
	probe.NetworkPermission = networkPermissionFor(discovery, adapter, NetworkPurposeModelCatalog, probe.NetworkDeclared)
	argv := catalogProbeArgv(adapter, candidate.path)
	env := probeEnvironment(deps.Getenv)
	probe.Argv = redactArgv(argv)
	probe.EnvironmentKeys = environmentKeys(env)
	probe.Source = SourceDescriptor{Kind: "command", AdapterID: adapter.AdapterID, ProbeCommandID: probe.ProbeCommandID, DiscoverySource: string(candidate.source), ExecutableName: filepath.Base(candidate.path)}
	probe.Evidence = EvidenceSummary{Kind: "bounded-model-catalog-command", CommandBounded: true, NoShell: true, RepositoryMutation: false, SecretMaterialRetained: false}

	unavailable := func(gaps []string, terminal string) (ModelCatalogSnapshot, []ModelCapability, ProbeResult) {
		source := CatalogSourceInput{
			Kind:                CatalogSourceProviderMachineReadable,
			Reference:           string(CatalogSourceProviderMachineReadable) + ":" + adapter.AdapterID + ":unavailable",
			SourceSchemaVersion: firstNonEmpty(adapter.CatalogProbeParser, "provider-text"),
			ProviderCLIVersion:  installation.Version,
			Precedence:          200,
			Confidence:          ConfidenceUnavailable,
			FreshnessState:      FreshnessNotApplicable,
			Gaps:                append([]string(nil), gaps...),
		}
		snapshot, capabilities, _ := buildCatalogSnapshot(adapter, &installationID, []CatalogSourceInput{source}, now)
		snapshot.TerminalErrorCode = terminal
		return snapshot, capabilities, probe
	}

	if installation.InstallationState != InstallationInstalled {
		probe.Outcome = OutcomeProbeFailed
		probe.Confidence = ConfidenceUnavailable
		probe.FreshnessState = FreshnessNotApplicable
		probe.SideEffectClass = "not-run"
		probe.GapReasons = []string{"installation-not-usable"}
		probe.TerminalErrorCode = firstNonEmpty(installation.TerminalErrorCode, "ErrInstallationNotUsable")
		return unavailable(probe.GapReasons, probe.TerminalErrorCode)
	}

	if probe.NetworkDeclared && probe.NetworkPermission != NetworkGranted {
		probe.Outcome = OutcomeProbeFailed
		probe.Confidence = ConfidenceUnavailable
		probe.FreshnessState = FreshnessNotApplicable
		probe.SideEffectClass = "not-run"
		probe.GapReasons = []string{"network-permission-denied"}
		probe.TerminalErrorCode = "ErrNetworkPermissionDenied"
		return unavailable(probe.GapReasons, probe.TerminalErrorCode)
	}

	result, err := sharedProbeResult(ctx, discovery, deps, ProbeExecution{
		Argv:               argv,
		Env:                env,
		Timeout:            AuthProbeTimeout,
		StdoutLimitBytes:   StdoutLimitBytes,
		StderrLimitBytes:   StderrLimitBytes,
		CombinedLimitBytes: CombinedLimitBytes,
	})
	stdout, stdoutFindings := redactProviderOutput(result.Stdout)
	stderr, stderrFindings := redactProviderOutput(result.Stderr)
	probe.StdoutSummary = stdout
	probe.StderrSummary = stderr
	probe.SecretFindingCount = stdoutFindings + stderrFindings
	probe.TimedOut = result.TimedOut
	probe.Killed = result.Killed
	probe.ExitCode = &result.ExitCode
	if err != nil || result.TimedOut || result.Killed {
		probe.Outcome = OutcomeProbeFailed
		probe.Confidence = ConfidenceUnknown
		probe.GapReasons = []string{"catalog-probe-failed"}
		probe.TerminalErrorCode = "ErrCatalogProbeExecutionFailed"
		if result.TimedOut {
			probe.GapReasons = []string{"catalog-probe-timeout"}
			probe.TerminalErrorCode = "ErrCatalogProbeTimeout"
		}
		return unavailable(probe.GapReasons, probe.TerminalErrorCode)
	}

	output := result.Stdout + "\n" + result.Stderr
	if result.ExitCode != 0 {
		probe.Outcome = OutcomeInstalledUnusable
		probe.Confidence = ConfidenceUnknown
		probe.GapReasons = []string{"catalog-probe-nonzero-exit"}
		probe.TerminalErrorCode = "ErrCatalogProbeNonZeroExit"
		if networkFailureText(strings.ToLower(output)) {
			probe.GapReasons = []string{"catalog-network-unavailable"}
			probe.TerminalErrorCode = "ErrCatalogNetworkUnavailable"
		}
		return unavailable(probe.GapReasons, probe.TerminalErrorCode)
	}

	sources, modelGaps := catalogSourcesFromGrokModels(adapter, installation.Version, output)
	if len(sources) == 0 {
		probe.Outcome = OutcomeInstalledUnusable
		probe.Confidence = ConfidenceUnknown
		probe.GapReasons = append([]string{"catalog-output-malformed"}, modelGaps...)
		probe.TerminalErrorCode = "ErrCatalogMalformedOutput"
		return unavailable(probe.GapReasons, probe.TerminalErrorCode)
	}
	probe.Outcome = OutcomeInstalled
	probe.Confidence = ConfidenceExact
	probe.setParsedFields(map[string]string{
		"model_count":  fmt.Sprintf("%d", countCatalogEntries(sources)),
		"source_count": fmt.Sprintf("%d", len(sources)),
		"parser":       firstNonEmpty(adapter.CatalogProbeParser, "provider-text"),
	})
	if len(modelGaps) > 0 {
		probe.GapReasons = append(probe.GapReasons, modelGaps...)
	}
	snapshot, capabilities, err := buildCatalogSnapshot(adapter, &installationID, sources, now)
	if err != nil {
		probe.Outcome = OutcomeProbeFailed
		probe.Confidence = ConfidenceUnknown
		probe.GapReasons = append(probe.GapReasons, "catalog-build-failed")
		probe.TerminalErrorCode = "ErrCatalogBuildFailed"
		return unavailable(probe.GapReasons, probe.TerminalErrorCode)
	}
	return snapshot, capabilities, probe
}

func catalogProbeArgv(adapter AdapterDeclaration, executablePath string) []string {
	command := append([]string(nil), adapter.CatalogProbeCommand...)
	if len(command) == 0 {
		return nil
	}
	command[0] = executablePath
	return command
}

func catalogProbeCommandID(adapter AdapterDeclaration) string {
	parser := strings.TrimSpace(adapter.CatalogProbeParser)
	if parser != "" {
		return parser
	}
	return "model-catalog"
}

func catalogSourcesFromGrokModels(adapter AdapterDeclaration, cliVersion, output string) ([]CatalogSourceInput, []string) {
	entries := parseGrokModelEntries(output)
	if len(entries) == 0 {
		return nil, []string{"catalog-empty-or-unrecognized"}
	}
	byReference := map[string][]CatalogInputEntry{}
	var refs []string
	for _, entry := range entries {
		if entry.ModelID == "" {
			continue
		}
		ref, constraint := grokModelSourceReference(entry)
		if len(byReference[ref]) == 0 {
			refs = append(refs, ref)
		}
		input := CatalogInputEntry{
			CanonicalModelID:    entry.ModelID,
			DisplayName:         firstNonEmpty(entry.DisplayName, entry.ModelID),
			Aliases:             append([]string(nil), entry.Aliases...),
			LifecycleState:      LifecycleAvailable,
			AvailabilityState:   AvailabilityAvailable,
			ReadOnly:            CapabilityUnknown,
			JSONOutput:          CapabilityUnknown,
			NestedSubagents:     CapabilityFalse,
			MCPConfig:           CapabilityUnknown,
			Cancellation:        CapabilityTrue,
			TokenUsageReporting: CapabilityUnknown,
			ImageInput:          CapabilityUnknown,
			ImageOutput:         CapabilityUnknown,
			Constraints:         []string{"reported-by-grok-models"},
		}
		if constraint != "" {
			input.Constraints = append(input.Constraints, constraint)
		}
		byReference[ref] = append(byReference[ref], input)
	}
	sort.Strings(refs)
	sources := make([]CatalogSourceInput, 0, len(refs))
	for _, ref := range refs {
		sources = append(sources, CatalogSourceInput{
			Kind:                CatalogSourceProviderMachineReadable,
			Reference:           ref,
			SourceSchemaVersion: firstNonEmpty(adapter.CatalogProbeParser, "provider-text"),
			ProviderCLIVersion:  cliVersion,
			Precedence:          200,
			Confidence:          ConfidenceExact,
			FreshnessState:      FreshnessFresh,
			Entries:             byReference[ref],
		})
	}
	return sources, nil
}

type grokModelEntry struct {
	ModelID     string
	DisplayName string
	Aliases     []string
	Provider    string
	BaseURL     string
	Custom      bool
}

func parseGrokModelEntries(output string) []grokModelEntry {
	var decoded any
	decoder := json.NewDecoder(strings.NewReader(output))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err == nil {
		return dedupeGrokModelEntries(collectGrokJSONModels(decoded, ""))
	}
	return dedupeGrokModelEntries(parseGrokTextModels(output))
}

func collectGrokJSONModels(value any, keyHint string) []grokModelEntry {
	switch typed := value.(type) {
	case []any:
		var out []grokModelEntry
		for _, item := range typed {
			out = append(out, collectGrokJSONModels(item, keyHint)...)
		}
		return out
	case map[string]any:
		if rawModels, ok := typed["models"]; ok {
			return collectGrokJSONModels(rawModels, "models")
		}
		if rawData, ok := typed["data"]; ok {
			return collectGrokJSONModels(rawData, "data")
		}
		entry := grokModelEntry{
			ModelID:     firstJSONText(typed, "model", "id", "model_id", "canonical_model_id"),
			DisplayName: firstJSONText(typed, "name", "display_name", "displayName"),
			Provider:    firstJSONText(typed, "provider", "vendor", "api_provider"),
			BaseURL:     firstJSONText(typed, "base_url", "baseUrl", "url"),
			Custom:      boolJSON(typed, "custom"),
		}
		if alias := firstJSONText(typed, "alias", "key"); alias != "" {
			entry.Aliases = append(entry.Aliases, alias)
		}
		if aliases, ok := typed["aliases"]; ok {
			entry.Aliases = append(entry.Aliases, stringListJSON(aliases)...)
		}
		if entry.ModelID != "" {
			return []grokModelEntry{entry}
		}
		var out []grokModelEntry
		for key, child := range typed {
			if key == "models" || key == "data" {
				continue
			}
			out = append(out, collectGrokJSONModels(child, key)...)
		}
		return out
	case string:
		if keyHint == "models" || looksLikeModelID(typed) {
			return []grokModelEntry{{ModelID: typed}}
		}
	}
	return nil
}

func parseGrokTextModels(output string) []grokModelEntry {
	var entries []grokModelEntry
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(strings.TrimLeft(line, "-*• "))
		if line == "" || strings.HasPrefix(line, "#") || strings.Contains(line, "---") {
			continue
		}
		if strings.ContainsAny(line, "{}") {
			continue
		}
		lower := strings.ToLower(line)
		if strings.Contains(lower, "available models") || strings.HasPrefix(lower, "model ") || strings.HasPrefix(lower, "alias ") || strings.Contains(lower, "not authenticated") || networkFailureText(lower) {
			continue
		}
		entry := grokModelEntry{}
		if left, right, ok := strings.Cut(line, "->"); ok {
			entry.Aliases = append(entry.Aliases, strings.Fields(left)...)
			line = strings.TrimSpace(right)
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		entry.ModelID = strings.Trim(fields[0], "`'\",")
		for _, field := range fields[1:] {
			clean := strings.Trim(field, "`'\",()[]")
			lowerField := strings.ToLower(clean)
			switch {
			case strings.HasPrefix(lowerField, "alias="), strings.HasPrefix(lowerField, "aliases="):
				_, aliases, _ := strings.Cut(clean, "=")
				entry.Aliases = append(entry.Aliases, splitAliases(aliases)...)
			case strings.HasPrefix(lowerField, "provider="), strings.HasPrefix(lowerField, "vendor="):
				_, entry.Provider, _ = strings.Cut(clean, "=")
			case strings.HasPrefix(lowerField, "base_url="), strings.HasPrefix(lowerField, "url="):
				_, entry.BaseURL, _ = strings.Cut(clean, "=")
			case lowerField == "custom" || lowerField == "custom=true":
				entry.Custom = true
			}
		}
		if looksLikeModelID(entry.ModelID) {
			entries = append(entries, entry)
		}
	}
	return entries
}

func dedupeGrokModelEntries(entries []grokModelEntry) []grokModelEntry {
	seen := map[string]grokModelEntry{}
	var ids []string
	for _, entry := range entries {
		entry.ModelID = strings.TrimSpace(entry.ModelID)
		if entry.ModelID == "" || secretLike(entry.ModelID) {
			continue
		}
		entry.DisplayName = safeSummary(entry.DisplayName)
		entry.Provider = safeSummary(entry.Provider)
		entry.BaseURL = safeSummary(entry.BaseURL)
		entry.Aliases = dedupeStrings(entry.Aliases)
		if _, ok := seen[entry.ModelID]; !ok {
			ids = append(ids, entry.ModelID)
			seen[entry.ModelID] = entry
			continue
		}
		existing := seen[entry.ModelID]
		existing.Aliases = dedupeStrings(append(existing.Aliases, entry.Aliases...))
		existing.DisplayName = firstNonEmpty(existing.DisplayName, entry.DisplayName)
		existing.Provider = firstNonEmpty(existing.Provider, entry.Provider)
		existing.BaseURL = firstNonEmpty(existing.BaseURL, entry.BaseURL)
		existing.Custom = existing.Custom || entry.Custom
		seen[entry.ModelID] = existing
	}
	sort.Strings(ids)
	out := make([]grokModelEntry, 0, len(ids))
	for _, id := range ids {
		out = append(out, seen[id])
	}
	return out
}

func grokModelSourceReference(entry grokModelEntry) (string, string) {
	provider := strings.ToLower(strings.TrimSpace(entry.Provider))
	host := hostFromURL(entry.BaseURL)
	isXAI := provider == "" || provider == "xai" || provider == "grok" || strings.Contains(host, "x.ai") || strings.Contains(host, "grok.com") || strings.HasPrefix(entry.ModelID, "grok-")
	if !entry.Custom && isXAI {
		return "grok-models:xai", "provider_attribution=xai"
	}
	attribution := firstNonEmpty(provider, host, providerPrefix(entry.ModelID), "custom")
	if attribution == "" || secretLike(attribution) {
		attribution = "custom-" + hashBase32(entry.ModelID)[:8]
	}
	attribution = boundedToken(safeSummary(attribution), 64)
	return "grok-models:custom:" + attribution, "provider_attribution=custom-non-xai:" + attribution
}

func hostFromURL(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}

func providerPrefix(modelID string) string {
	prefix, _, ok := strings.Cut(modelID, "/")
	if !ok {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(prefix))
}

func countCatalogEntries(sources []CatalogSourceInput) int {
	count := 0
	for _, source := range sources {
		count += len(source.Entries)
	}
	return count
}

func firstJSONText(values map[string]any, keys ...string) string {
	for _, key := range keys {
		value, ok := values[key]
		if !ok {
			continue
		}
		if text, ok := value.(string); ok {
			text = strings.TrimSpace(text)
			if text != "" {
				return text
			}
		}
	}
	return ""
}

func boolJSON(values map[string]any, key string) bool {
	value, ok := values[key]
	if !ok {
		return false
	}
	if typed, ok := value.(bool); ok {
		return typed
	}
	if text, ok := value.(string); ok {
		return strings.EqualFold(strings.TrimSpace(text), "true") || strings.EqualFold(strings.TrimSpace(text), "yes")
	}
	return false
}

func stringListJSON(value any) []string {
	switch typed := value.(type) {
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok {
				out = append(out, text)
			}
		}
		return out
	case []string:
		return append([]string(nil), typed...)
	case string:
		return splitAliases(typed)
	default:
		return nil
	}
}

func splitAliases(value string) []string {
	var aliases []string
	for _, alias := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ';' || r == '|' }) {
		alias = strings.TrimSpace(alias)
		if alias != "" {
			aliases = append(aliases, alias)
		}
	}
	return aliases
}

func looksLikeModelID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || secretLike(value) {
		return false
	}
	lower := strings.ToLower(value)
	if strings.ContainsAny(value, " \t\n\r") || strings.HasPrefix(lower, "http") || strings.Contains(lower, "=") {
		return false
	}
	switch lower {
	case "default", "name", "description", "provider", "custom", "models":
		return false
	default:
		return true
	}
}

func networkFailureText(lower string) bool {
	return strings.Contains(lower, "network") ||
		strings.Contains(lower, "connection refused") ||
		strings.Contains(lower, "connection reset") ||
		strings.Contains(lower, "dns") ||
		strings.Contains(lower, "tls") ||
		strings.Contains(lower, "timeout") ||
		strings.Contains(lower, "timed out") ||
		strings.Contains(lower, "proxy")
}

func baseProbe(adapter AdapterDeclaration, now time.Time, deps Deps) ProbeResult {
	probe := ProbeResult{
		SchemaVersion:            ProbeResultSchema,
		RecordVersion:            1,
		Scope:                    "machine",
		ProjectID:                nil,
		ProbeResultID:            deps.RandomID(),
		AdapterID:                adapter.AdapterID,
		AdapterDeclarationID:     adapterDeclarationID(adapter),
		ProbeKind:                "install",
		WorkingDirectory:         "none",
		EnvironmentKeys:          []string{},
		TimeoutMS:                0,
		StdoutLimitBytes:         StdoutLimitBytes,
		StderrLimitBytes:         StderrLimitBytes,
		CombinedOutputLimitBytes: CombinedLimitBytes,
		TimedOut:                 false,
		Killed:                   false,
		NetworkDeclared:          false,
		NetworkPermission:        NetworkNotNeeded,
		CreatedAt:                formatTime(now),
		UpdatedAt:                formatTime(now),
		CreatedBy:                defaultActorProvenance(),
		UpdatedBy:                defaultActorProvenance(),
		Host:                     hostProvenance(),
		PolicyVersion:            PolicyVersion,
		CapturedAt:               formatTime(now),
		FreshnessState:           FreshnessFresh,
		Confidence:               ConfidenceUnknown,
		SideEffectClass:          "local-read",
		Classification:           "provider-output-untrusted",
		GapReasons:               []string{},
	}
	return probe
}

func adapterDeclarations(cfg config.Config, contract runtimecap.Contract) ([]AdapterDeclaration, error) {
	byID := map[string]AdapterDeclaration{}
	for _, provider := range contract.Providers {
		byID[provider.Name] = declarationFromRuntime(provider)
	}
	for _, provider := range models.DefaultRegistry().Providers {
		decl := byID[provider.Name]
		if decl.AdapterID == "" {
			decl.AdapterID = provider.Name
			decl.AdapterVersion = AdapterVersion
			decl.DeclarationSchemaVersion = AdapterDeclarationSchema
			decl.ConformanceVersion = "loopcoder.adapter_conformance.v1"
			decl.DisplayName = provider.DisplayName
			decl.Vendor = provider.Vendor
			decl.ExecutableNames = []string{firstNonEmpty(provider.CLI, provider.Name)}
			decl.VersionArgv = []string{"--version"}
			decl.AuthUnsupportedReason = "adapter declares no credential-blind auth readiness mechanism"
			decl.KnownLimitations = []string{"adapter has installation discovery and static catalog only; auth readiness is unknown"}
		}
		if decl.DisplayName == "" {
			decl.DisplayName = provider.DisplayName
		}
		byID[provider.Name] = decl
	}
	for _, provider := range configuredProviderNames(cfg) {
		if _, ok := byID[provider]; ok {
			continue
		}
		byID[provider] = AdapterDeclaration{
			AdapterID:                provider,
			AdapterVersion:           AdapterVersion,
			DeclarationSchemaVersion: AdapterDeclarationSchema,
			ConformanceVersion:       "loopcoder.adapter_conformance.v1",
			DisplayName:              provider,
			Vendor:                   "custom",
			ExecutableNames:          []string{provider},
			VersionArgv:              []string{"--version"},
			AuthUnsupportedReason:    "custom adapter declares no credential-blind auth readiness mechanism",
			KnownLimitations:         []string{"custom adapter has installation discovery only; auth, catalog, quota, and invocation readiness are unknown"},
		}
	}
	for adapterID, paths := range cfg.ProviderInventory.Executables {
		adapterID = strings.TrimSpace(adapterID)
		if adapterID == "" {
			continue
		}
		decl := byID[adapterID]
		if decl.AdapterID == "" {
			decl = AdapterDeclaration{
				AdapterID:                adapterID,
				AdapterVersion:           AdapterVersion,
				DeclarationSchemaVersion: AdapterDeclarationSchema,
				ConformanceVersion:       "loopcoder.adapter_conformance.v1",
				DisplayName:              adapterID,
				Vendor:                   "custom",
				ExecutableNames:          []string{adapterID},
				VersionArgv:              []string{"--version"},
				AuthUnsupportedReason:    "custom adapter declares no credential-blind auth readiness mechanism",
				KnownLimitations:         []string{"custom adapter has installation discovery only; auth, catalog, quota, and invocation readiness are unknown"},
			}
		}
		decl.ExplicitPaths = append(decl.ExplicitPaths, paths...)
		byID[adapterID] = decl
	}
	for adapterID, accountProfileID := range cfg.ProviderInventory.ProfileSelection {
		adapterID = strings.TrimSpace(adapterID)
		if adapterID == "" {
			continue
		}
		decl := byID[adapterID]
		if decl.AdapterID == "" {
			decl = AdapterDeclaration{
				AdapterID:                adapterID,
				AdapterVersion:           AdapterVersion,
				DeclarationSchemaVersion: AdapterDeclarationSchema,
				ConformanceVersion:       "loopcoder.adapter_conformance.v1",
				DisplayName:              adapterID,
				Vendor:                   "custom",
				ExecutableNames:          []string{adapterID},
				VersionArgv:              []string{"--version"},
				AuthUnsupportedReason:    "custom adapter declares no credential-blind auth readiness mechanism",
				KnownLimitations:         []string{"custom adapter has installation discovery only; auth, catalog, quota, and invocation readiness are unknown"},
			}
		}
		decl.SelectedAccountProfileID = strings.TrimSpace(accountProfileID)
		byID[adapterID] = decl
	}
	out := make([]AdapterDeclaration, 0, len(byID))
	for _, decl := range byID {
		decl = normalizeAdapterDeclaration(decl)
		if violations := ValidateAdapterDeclaration(decl); len(violations) > 0 {
			return nil, fmt.Errorf("%w: adapter declaration %q: %s", ErrInvalidRecord, decl.AdapterID, strings.Join(violations, "; "))
		}
		out = append(out, decl)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AdapterID < out[j].AdapterID })
	return out, nil
}

func declarationFromRuntime(provider runtimecap.ProviderRuntime) AdapterDeclaration {
	display := firstNonEmpty(provider.DisplayName, provider.Name)
	if modelProvider, ok := models.LookupProvider(provider.Name); ok {
		display = firstNonEmpty(modelProvider.DisplayName, provider.Name)
	}
	decl := AdapterDeclaration{
		AdapterID:                provider.Name,
		AdapterVersion:           firstNonEmpty(provider.AdapterVersion, AdapterVersion),
		DeclarationSchemaVersion: firstNonEmpty(provider.DeclarationSchemaVersion, AdapterDeclarationSchema),
		ConformanceVersion:       firstNonEmpty(provider.ConformanceVersion, "loopcoder.adapter_conformance.v1"),
		DisplayName:              display,
		Vendor:                   firstNonEmpty(provider.Vendor, provider.Name),
		ExecutableNames:          []string{provider.Executable},
		VersionArgv:              append([]string(nil), provider.VersionArgv...),
		AuthProbeCommand:         append([]string(nil), provider.AuthProbeCommand...),
		AuthProbeParser:          provider.AuthProbeParser,
		AuthProbeMayNetwork:      provider.MayNetwork,
		AuthArtifactPaths:        append([]string(nil), provider.AuthArtifactPaths...),
		AuthEnvironmentNames:     append([]string(nil), provider.AuthEnvironmentNames...),
		AuthUnsupportedReason:    provider.AuthUnsupportedReason,
		CatalogProbeCommand:      append([]string(nil), provider.CatalogProbeCommand...),
		CatalogProbeParser:       provider.CatalogProbeParser,
		CatalogProbeMayNetwork:   provider.CatalogProbeMayNetwork,
		StaticCatalogEntries:     catalogEntriesFromRuntime(provider.StaticModelCatalog),
		MayNetwork:               provider.MayNetwork,
		KnownLimitations:         append([]string(nil), provider.KnownLimitations...),
	}
	return decl
}

func normalizeRuntimeContract(contract runtimecap.Contract) runtimecap.Contract {
	defaultContract := runtimecap.DefaultContract()
	if len(contract.Providers) == 0 && len(contract.Hosts) == 0 {
		return defaultContract
	}
	if len(contract.Hosts) == 0 {
		contract.Hosts = defaultContract.Hosts
	}
	return contract
}

func normalizeAdapterDeclaration(decl AdapterDeclaration) AdapterDeclaration {
	decl.AdapterID = strings.TrimSpace(decl.AdapterID)
	if decl.AdapterVersion == "" {
		decl.AdapterVersion = AdapterVersion
	}
	if decl.DeclarationSchemaVersion == "" {
		decl.DeclarationSchemaVersion = AdapterDeclarationSchema
	}
	if decl.ConformanceVersion == "" {
		decl.ConformanceVersion = "loopcoder.adapter_conformance.v1"
	}
	if decl.DisplayName == "" {
		decl.DisplayName = decl.AdapterID
	}
	if decl.Vendor == "" {
		decl.Vendor = decl.AdapterID
	}
	if len(decl.ExecutableNames) == 0 && decl.AdapterID != "" {
		decl.ExecutableNames = []string{decl.AdapterID}
	}
	if len(decl.VersionArgv) == 0 {
		decl.VersionArgv = []string{"--version"}
	}
	if len(decl.StaticCatalogEntries) == 0 {
		decl.StaticCatalogEntries = []CatalogInputEntry{}
	}
	return decl
}

func ValidateAdapterDeclaration(decl AdapterDeclaration) []string {
	decl = normalizeAdapterDeclaration(decl)
	var violations []string
	if !safeAdapterKey(decl.AdapterID) {
		violations = append(violations, "adapter_id must be a non-empty safe provider key")
	}
	if decl.DeclarationSchemaVersion != AdapterDeclarationSchema {
		violations = append(violations, fmt.Sprintf("declaration_schema_version %q is unsupported", decl.DeclarationSchemaVersion))
	}
	if strings.TrimSpace(decl.AdapterVersion) == "" {
		violations = append(violations, "adapter_version is required")
	}
	if strings.TrimSpace(decl.ConformanceVersion) == "" {
		violations = append(violations, "conformance_version is required")
	}
	if strings.TrimSpace(decl.DisplayName) == "" {
		violations = append(violations, "display_name is required")
	}
	if strings.TrimSpace(decl.Vendor) == "" {
		violations = append(violations, "vendor is required")
	}
	if len(decl.ExecutableNames) == 0 {
		violations = append(violations, "executable_names must declare at least one command name")
	}
	for index, name := range decl.ExecutableNames {
		if !safeCommandName(name) {
			violations = append(violations, fmt.Sprintf("executable_names[%d] must be a command name without path separators", index))
		}
	}
	violations = append(violations, validateFixedArgv("version_argv", decl.VersionArgv, false)...)
	violations = append(violations, validateFixedArgv("auth_probe_command", decl.AuthProbeCommand, true)...)
	violations = append(violations, validateFixedArgv("catalog_probe_command", decl.CatalogProbeCommand, true)...)
	if decl.AuthProbeMayNetwork && len(decl.AuthProbeCommand) == 0 {
		violations = append(violations, "auth_probe_may_network requires auth_probe_command")
	}
	if decl.CatalogProbeMayNetwork && len(decl.CatalogProbeCommand) == 0 {
		violations = append(violations, "catalog_probe_may_network requires catalog_probe_command")
	}
	if decl.MayNetwork && len(decl.AuthProbeCommand) == 0 && len(decl.CatalogProbeCommand) == 0 {
		violations = append(violations, "may_network requires at least one declared bounded command")
	}
	if len(decl.AuthProbeCommand) == 0 && len(decl.AuthArtifactPaths) == 0 && len(decl.AuthEnvironmentNames) == 0 && strings.TrimSpace(decl.AuthUnsupportedReason) == "" {
		violations = append(violations, "auth_readiness_contract must declare credential-blind evidence or unsupported reason")
	}
	for index, name := range decl.AuthEnvironmentNames {
		name = strings.TrimSpace(name)
		if name == "" || strings.Contains(name, "=") {
			violations = append(violations, fmt.Sprintf("auth_environment_names[%d] must be an environment variable name, not a value", index))
		}
	}
	for index, entry := range decl.StaticCatalogEntries {
		if strings.TrimSpace(entry.CanonicalModelID) == "" {
			violations = append(violations, fmt.Sprintf("static_catalog_entries[%d].canonical_model_id is required", index))
		}
	}
	return violations
}

func validateFixedArgv(field string, argv []string, includesExecutable bool) []string {
	if len(argv) == 0 {
		return nil
	}
	var violations []string
	for index, arg := range argv {
		if strings.TrimSpace(arg) == "" {
			violations = append(violations, fmt.Sprintf("%s[%d] must not be empty", field, index))
		}
	}
	if includesExecutable && !safeCommandName(argv[0]) {
		violations = append(violations, fmt.Sprintf("%s[0] must be a command name without path separators", field))
	}
	return violations
}

var safeAdapterKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func safeAdapterKey(value string) bool {
	value = strings.TrimSpace(value)
	return safeAdapterKeyPattern.MatchString(value) && !strings.ContainsAny(value, `/\`)
}

func safeCommandName(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && !strings.ContainsAny(value, `/\`)
}

func catalogEntriesFromRuntime(models []runtimecap.ProviderModelCapability) []CatalogInputEntry {
	out := make([]CatalogInputEntry, 0, len(models))
	for _, model := range models {
		entry := CatalogInputEntry{
			CanonicalModelID:    model.ModelID,
			DisplayName:         model.DisplayName,
			Aliases:             append([]string(nil), model.Aliases...),
			LifecycleState:      LifecycleState(model.LifecycleState),
			ReplacementModelID:  model.ReplacementModelID,
			AvailabilityState:   AvailabilityState(model.AvailabilityState),
			ReadOnly:            boolCapability(model.ReadOnly),
			JSONOutput:          boolCapability(model.JSONOutput),
			NestedSubagents:     boolCapability(model.NestedSubagents),
			MCPConfig:           boolCapability(model.MCPConfig),
			Cancellation:        boolCapability(model.Cancellation),
			TokenUsageReporting: boolCapability(model.TokenUsageReporting),
			ImageInput:          boolCapability(model.ImageInput),
			ImageOutput:         boolCapability(model.ImageOutput),
			Constraints:         append([]string(nil), model.Constraints...),
		}
		for _, role := range model.RolesSupported {
			entry.RolesSupported = append(entry.RolesSupported, CatalogRole(role))
		}
		out = append(out, entry)
	}
	return out
}

func configuredProviderNames(cfg config.Config) []string {
	seen := map[string]bool{}
	var out []string
	for _, provider := range []string{cfg.Adapters.Worker, cfg.Adapters.Verifier} {
		provider = strings.TrimSpace(provider)
		if provider == "" {
			continue
		}
		if !seen[provider] {
			seen[provider] = true
			out = append(out, provider)
		}
	}
	if cfg.ProviderInventory.CodexBar.Enabled {
		for _, provider := range cfg.ProviderInventory.CodexBar.Providers {
			name := strings.TrimSpace(provider.Provider)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		out = []string{"codex", "claude"}
	}
	return out
}

// refreshableProvider reports whether an adapter may be refreshed when
// explicitly requested (e.g. --grant-quota-telemetry all). Includes delivery
// worker/verifier plus static registry providers so multi-company capacity
// can be persisted without elevating every Status listing.
func refreshableProvider(cfg config.Config, provider string) bool {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return false
	}
	for _, p := range configuredProviderNames(cfg) {
		if p == provider {
			return true
		}
	}
	if _, ok := models.LookupProvider(provider); ok {
		return true
	}
	return false
}

func filterAdapterDeclarations(adapters []AdapterDeclaration, active []string) []AdapterDeclaration {
	if len(active) == 0 {
		return adapters
	}
	allowed := map[string]bool{}
	for _, provider := range active {
		provider = strings.TrimSpace(provider)
		if provider == "" {
			continue
		}
		allowed[provider] = true
	}
	if len(allowed) == 0 {
		return adapters
	}
	out := make([]AdapterDeclaration, 0, len(adapters))
	for _, adapter := range adapters {
		if allowed[adapter.AdapterID] {
			out = append(out, adapter)
		}
	}
	return out
}

func pathCandidates(executable string, deps Deps) []string {
	var out []string
	pathValue := deps.Getenv("PATH")
	if strings.TrimSpace(pathValue) == "" {
		return out
	}
	for _, dir := range filepath.SplitList(pathValue) {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			dir = "."
		}
		for _, name := range executableNamesForPlatform(executable, deps) {
			out = append(out, filepath.Join(dir, name))
		}
	}
	return out
}

func executableNamesForPlatform(executable string, deps Deps) []string {
	executable = strings.TrimSpace(executable)
	if runtime.GOOS != "windows" || filepath.Ext(executable) != "" {
		return []string{executable}
	}
	pathext := deps.Getenv("PATHEXT")
	if strings.TrimSpace(pathext) == "" {
		pathext = ".COM;.EXE;.BAT;.CMD"
	}
	names := []string{executable}
	for _, ext := range strings.Split(pathext, ";") {
		ext = strings.TrimSpace(ext)
		if ext == "" {
			continue
		}
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		names = append(names, executable+strings.ToLower(ext), executable+strings.ToUpper(ext))
	}
	return names
}

func executableFile(path string, deps Deps) bool {
	info, err := deps.Lstat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode().Perm()&0o111 != 0
}

func executableIdentity(path, platform string, deps Deps) ExecutableIdentity {
	canonical := canonicalPath(path, deps)
	resolved := canonical
	resolution := "not-symlink"
	if target, err := deps.EvalSymlinks(canonical); err == nil {
		target = canonicalPath(target, deps)
		if target != canonical {
			resolved = target
			resolution = "resolved"
		}
	} else {
		resolution = "unresolved"
	}
	mode := "unknown"
	if info, err := deps.Lstat(canonical); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			if resolution == "not-symlink" {
				resolution = "symlink"
			}
		}
		if runtime.GOOS == "windows" || info.Mode().Perm()&0o111 != 0 {
			mode = "executable"
		} else {
			mode = "not-executable"
		}
	}
	return ExecutableIdentity{
		Basename:          filepath.Base(canonical),
		Platform:          platform,
		PathHash:          "sha256:" + hashHex(canonical),
		SymlinkResolution: resolution,
		ResolvedPathHash:  "sha256:" + hashHex(resolved),
		ExecutableMode:    mode,
	}
}

func canonicalPath(path string, deps Deps) string {
	path = filepath.Clean(path)
	if abs, err := deps.Abs(path); err == nil {
		path = abs
	}
	return filepath.Clean(path)
}

func installationID(adapterID string, identity ExecutableIdentity, path, platform string) string {
	return "pinst_" + hashBase32(adapterID, identity.Basename, identity.PathHash, hashHex(path), platform)[:32]
}

func adapterDeclarationID(adapter AdapterDeclaration) string {
	adapter = normalizeAdapterDeclaration(adapter)
	return "adapter_" + hashBase32(adapter.AdapterID, adapter.DeclarationSchemaVersion, adapter.AdapterVersion)[:32]
}

func runProbeCommand(ctx context.Context, req ProbeExecution) (ProbeExecutionResult, error) {
	if len(req.Argv) == 0 || strings.TrimSpace(req.Argv[0]) == "" {
		return ProbeExecutionResult{ExitCode: -1}, errors.New("probe argv is empty")
	}
	budget := newOutputBudget(req.CombinedLimitBytes)
	stdout := newBoundedBuffer(req.StdoutLimitBytes, budget)
	stderr := newBoundedBuffer(req.StderrLimitBytes, budget)
	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Dir = os.TempDir()
	cmd.Env = append([]string{}, req.Env...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	result, err := supervisedexec.Run(ctx, cmd, supervisedexec.Options{HardCap: req.Timeout, LivenessMode: supervisedexec.LivenessModeLogOnly, Role: "provider-probe"})
	exitCode := result.ExitCode
	if (err != nil || result.Outcome == supervisedexec.OutcomeDeadline || result.Killed) && exitCode == 0 {
		exitCode = -1
	}
	out := ProbeExecutionResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
		TimedOut: result.Outcome == supervisedexec.OutcomeDeadline,
		Killed:   result.Killed,
	}
	if stdout.Truncated() || stderr.Truncated() {
		if out.Stderr != "" {
			out.Stderr += "\n"
		}
		out.Stderr += "[loopcoder] provider probe output truncated"
	}
	return out, err
}

type boundedBuffer struct {
	mu        sync.Mutex
	limit     int64
	combined  *outputBudget
	written   int64
	data      strings.Builder
	truncated bool
}

type outputBudget struct {
	mu        sync.Mutex
	remaining int64
}

func newOutputBudget(limit int) *outputBudget {
	return &outputBudget{remaining: int64(limit)}
}

func newBoundedBuffer(limit int, combined *outputBudget) *boundedBuffer {
	return &boundedBuffer{limit: int64(limit), combined: combined}
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limit <= 0 {
		return len(p), nil
	}
	remain := b.limit - b.written
	if remain <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if int64(len(p)) < remain {
		remain = int64(len(p))
	}
	if b.combined != nil {
		remain = b.combined.reserve(remain)
		if remain <= 0 {
			b.truncated = true
			return len(p), nil
		}
	}
	chunk := p
	if int64(len(chunk)) > remain {
		chunk = chunk[:remain]
		b.truncated = true
	}
	b.data.Write(chunk)
	b.written += int64(len(chunk))
	return len(p), nil
}

func (b *outputBudget) reserve(max int64) int64 {
	if b == nil {
		return max
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.remaining <= 0 {
		return 0
	}
	if max > b.remaining {
		max = b.remaining
	}
	b.remaining -= max
	return max
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.String()
}

func (b *boundedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

func redactProviderOutput(output string) (string, int) {
	output = strings.ReplaceAll(output, "\r\n", "\n")
	output = strings.TrimSpace(output)
	if len(output) > 4096 {
		output = output[:4096] + "\n[loopcoder] summary truncated"
	}
	findings := 0
	for _, pattern := range secretPatterns {
		matches := pattern.FindAllStringIndex(output, -1)
		if len(matches) == 0 {
			continue
		}
		findings += len(matches)
		output = pattern.ReplaceAllString(output, "[REDACTED]")
	}
	output = genericKeyValueSecretPattern.ReplaceAllStringFunc(output, func(match string) string {
		submatches := genericKeyValueSecretPattern.FindStringSubmatch(match)
		if len(submatches) < 3 || !looksLikeOpaqueSecretValue(submatches[2]) {
			return match
		}
		findings++
		return "[REDACTED]"
	})
	output = emailPattern.ReplaceAllStringFunc(output, redactEmail)
	return output, findings
}

func safeSummary(value string) string {
	summary, _ := redactProviderOutput(value)
	summary = strings.TrimSpace(summary)
	if len(summary) > 240 {
		summary = summary[:240] + "..."
	}
	return summary
}

func boundedToken(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit > 0 && len(value) > limit {
		return value[:limit]
	}
	return value
}

func secretLike(value string) bool {
	for _, pattern := range secretPatterns {
		if pattern.MatchString(value) {
			return true
		}
	}
	return genericKeyValueSecretPattern.MatchString(value)
}

func (p *ProbeResult) setParsedFields(fields map[string]string) {
	if len(fields) == 0 {
		p.ParsedFields = nil
		return
	}
	out := make(map[string]string, len(fields))
	for key, value := range fields {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		out[key] = safeSummary(value)
	}
	if len(out) == 0 {
		p.ParsedFields = nil
		return
	}
	p.ParsedFields = out
}

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b[A-Z0-9_-]*(api[_-]?key|secret|token|password|credential)[A-Z0-9_-]*\s*[:=]\s*["']?[^"'\s,}]{6,}["']?`),
	regexp.MustCompile(`AKIA[A-Z0-9]{16}`),
	regexp.MustCompile(`(?i)sk-ant-[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`(?i)sk-[A-Za-z0-9_-]{12,}`),
	regexp.MustCompile(`(?i)gh[pousr]_[A-Za-z0-9_]{12,}`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]{12,}`),
	regexp.MustCompile(`(?i)(cookie|set-cookie):\s*[^;\n]+`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
}

var genericKeyValueSecretPattern = regexp.MustCompile(`(?i)(["']?[A-Za-z0-9_.-]+["']?\s*[:=]\s*["']?([A-Za-z0-9._~+/=_-]{12,})["']?)`)

func looksLikeOpaqueSecretValue(value string) bool {
	value = strings.Trim(value, `"'`)
	if len(value) < 12 || strings.Contains(value, ",") {
		return false
	}
	hasDigit := false
	hasTokenPunctuation := false
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case strings.ContainsRune("._~+/=_-", r):
			hasTokenPunctuation = true
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		default:
			return false
		}
	}
	return hasDigit || hasTokenPunctuation
}

func probeEnvironment(getenv func(string) string) []string {
	return probeEnvironmentFromKeys(getenv, allowedProbeEnvKeys())
}

func probeEnvironmentFromKeys(getenv func(string) string, allowedKeys []string) []string {
	if getenv == nil {
		getenv = os.Getenv
	}
	seen := map[string]bool{}
	var env []string
	for _, key := range allowedKeys {
		if probeEnvNameDenied(key) {
			continue
		}
		seenKey := strings.ToLower(key)
		if seen[seenKey] {
			continue
		}
		seen[seenKey] = true
		value := getenv(key)
		if value == "" {
			continue
		}
		env = append(env, key+"="+value)
	}
	return env
}

var probeEnvNameDenylist = regexp.MustCompile(`(?i)(key|secret|token|password|credential|auth)`)

func probeEnvNameDenied(key string) bool {
	return probeEnvNameDenylist.MatchString(key)
}

func allowedProbeEnvKeys() []string {
	return []string{
		"LOCALAPPDATA",
		"APPDATA",
		"ProgramData",
		"ProgramFiles",
		"ProgramFiles(x86)",
		"CommonProgramFiles",
		"CommonProgramFiles(x86)",
		"ALLUSERSPROFILE",
		"PUBLIC",
		"SystemDrive",
		"SystemRoot",
		"windir",
		"ComSpec",
		"PSModulePath",
		"PATHEXT",
		"PATH",
		"TEMP",
		"TMP",
		"HOME",
		"USER",
		"LOGNAME",
		"USERPROFILE",
		"OS",
		"PROCESSOR_ARCHITECTURE",
		"NUMBER_OF_PROCESSORS",
		"TMPDIR",
		"LANG",
		"LC_ALL",
	}
}

func environmentKeys(env []string) []string {
	keys := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if ok && key != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func defaultActorProvenance() ActorProvenance {
	return ActorProvenance{
		ActorKind:         "policy-engine",
		ActorID:           "provider-inventory",
		Display:           "LoopCoder provider inventory",
		DecisionAuthority: "deterministic-policy-engine",
		Source:            "adapter-probe",
	}
}

func hostProvenance() HostProvenance {
	platform := runtime.GOOS + "-" + runtime.GOARCH
	return HostProvenance{
		HostKind:         "generic-local",
		HostID:           "host-local",
		ProcessID:        os.Getpid(),
		LoopcoderVersion: "dev",
		Platform:         platform,
	}
}

func parseVersion(stdout, stderr string) string {
	for _, line := range strings.Split(strings.ReplaceAll(stdout+"\n"+stderr, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			if len(line) > 200 {
				line = line[:200]
			}
			redacted, _ := redactProviderOutput(line)
			return redacted
		}
	}
	return ""
}

func unsupportedAdapterVersion(adapter AdapterDeclaration, version string) bool {
	if adapter.AdapterID != "grok" {
		return false
	}
	return semanticVersionLess(version, []int{0, 1, 0})
}

func malformedAdapterVersion(adapter AdapterDeclaration, version string) bool {
	return adapter.AdapterID == "grok" && strings.TrimSpace(version) != "" && len(firstVersionParts(version)) == 0
}

func semanticVersionLess(value string, minimum []int) bool {
	parts := firstVersionParts(value)
	if len(parts) == 0 {
		return false
	}
	for len(parts) < len(minimum) {
		parts = append(parts, 0)
	}
	for index, min := range minimum {
		if parts[index] < min {
			return true
		}
		if parts[index] > min {
			return false
		}
	}
	return false
}

func firstVersionParts(value string) []int {
	match := versionPattern.FindString(value)
	if match == "" {
		return nil
	}
	var parts []int
	for _, piece := range strings.Split(match, ".") {
		n, err := strconv.Atoi(piece)
		if err != nil {
			return nil
		}
		parts = append(parts, n)
	}
	return parts
}

var versionPattern = regexp.MustCompile(`\d+(?:\.\d+){0,3}`)

func redactPath(path string) string {
	base := filepath.Base(path)
	parent := filepath.Base(filepath.Dir(path))
	if parent == "." || parent == string(filepath.Separator) || parent == "" {
		return base
	}
	return filepath.ToSlash(filepath.Join("...", parent, base))
}

func redactArgv(argv []string) []string {
	if len(argv) == 0 {
		return nil
	}
	out := append([]string(nil), argv...)
	out[0] = redactPath(out[0])
	return out
}

func confidenceForInventory(installations []ProviderInstallation, probes []ProbeResult) Confidence {
	if len(installations) == 0 {
		return ConfidenceUnavailable
	}
	for _, probe := range probes {
		if probe.ProbeKind == "native-federation" {
			continue
		}
		if probe.Outcome == OutcomeProbeFailed {
			return ConfidenceUnknown
		}
	}
	return ConfidenceExact
}

func insertProbe(ctx context.Context, tx storage.Tx, probe ProbeResult) error {
	payload, err := json.Marshal(probe)
	if err != nil {
		return err
	}
	createdBy, err := marshalJSONText(probe.CreatedBy)
	if err != nil {
		return err
	}
	updatedBy, err := marshalJSONText(probe.UpdatedBy)
	if err != nil {
		return err
	}
	host, err := marshalJSONText(probe.Host)
	if err != nil {
		return err
	}
	providerInstallationID := any(nil)
	if probe.ProviderInstallationID != nil {
		providerInstallationID = *probe.ProviderInstallationID
	}
	exitCode := any(nil)
	if probe.ExitCode != nil {
		exitCode = *probe.ExitCode
	}
	_, err = tx.Exec(ctx, `INSERT INTO provider_probe_results(
		probe_result_id, schema_version, record_version, scope, project_id, adapter_id, adapter_declaration_id,
		provider_installation_id, probe_kind, probe_command_id, probe_method, outcome,
		timeout_ms, stdout_limit_bytes, stderr_limit_bytes, combined_output_limit_bytes,
		exit_code, timed_out, killed, network_declared, network_permission,
		secret_finding_count, created_at, updated_at, created_by_json, updated_by_json,
		host_json, policy_version, captured_at, freshness_state, confidence,
		terminal_error_code, payload_json
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		probe.ProbeResultID, probe.SchemaVersion, probe.RecordVersion, probe.Scope, probe.ProjectID,
		probe.AdapterID, probe.AdapterDeclarationID, providerInstallationID, probe.ProbeKind, probe.ProbeCommandID,
		string(probe.ProbeMethod), string(probe.Outcome), probe.TimeoutMS, probe.StdoutLimitBytes,
		probe.StderrLimitBytes, probe.CombinedOutputLimitBytes, exitCode, boolInt(probe.TimedOut),
		boolInt(probe.Killed), boolInt(probe.NetworkDeclared), string(probe.NetworkPermission),
		probe.SecretFindingCount, probe.CreatedAt, probe.UpdatedAt, createdBy, updatedBy, host,
		probe.PolicyVersion, probe.CapturedAt, string(probe.FreshnessState),
		string(probe.Confidence), probe.TerminalErrorCode, string(payload))
	return err
}

func upsertAccountProfile(ctx context.Context, tx storage.Tx, profile AccountProfile) error {
	payload, err := json.Marshal(profile)
	if err != nil {
		return err
	}
	providerInstallationID := any(nil)
	if profile.ProviderInstallationID != nil {
		providerInstallationID = *profile.ProviderInstallationID
	}
	_, err = tx.Exec(ctx, `INSERT INTO account_profiles(
		account_profile_id, adapter_id, provider_installation_id, payload_json
	) VALUES (?, ?, ?, ?)
	ON CONFLICT(account_profile_id) DO UPDATE SET
		provider_installation_id = excluded.provider_installation_id,
		payload_json = excluded.payload_json`,
		profile.AccountProfileID, profile.AdapterID, providerInstallationID, string(payload))
	return err
}

func insertAuthReadiness(ctx context.Context, tx storage.Tx, readiness AuthReadiness) error {
	payload, err := json.Marshal(readiness)
	if err != nil {
		return err
	}
	providerInstallationID := any(nil)
	if readiness.ProviderInstallationID != nil {
		providerInstallationID = *readiness.ProviderInstallationID
	}
	accountProfileID := any(nil)
	if readiness.AccountProfileID != nil {
		accountProfileID = *readiness.AccountProfileID
	}
	_, err = tx.Exec(ctx, `INSERT OR IGNORE INTO auth_readiness(
		auth_readiness_id, adapter_id, provider_installation_id, account_profile_id, payload_json
	) VALUES (?, ?, ?, ?, ?)`,
		readiness.AuthReadinessID, readiness.AdapterID, providerInstallationID, accountProfileID, string(payload))
	return err
}

func insertModelCatalogSnapshot(ctx context.Context, tx storage.Tx, snapshot ModelCatalogSnapshot) error {
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	providerInstallationID := any(nil)
	if snapshot.ProviderInstallationID != nil {
		providerInstallationID = *snapshot.ProviderInstallationID
	}
	_, err = tx.Exec(ctx, `INSERT OR IGNORE INTO model_catalog_snapshots(
		model_catalog_snapshot_id, adapter_id, provider_installation_id, payload_json
	) VALUES (?, ?, ?, ?)`,
		snapshot.ModelCatalogSnapshotID, snapshot.AdapterID, providerInstallationID, string(payload))
	return err
}

func insertModelCapability(ctx context.Context, tx storage.Tx, capability ModelCapability) error {
	payload, err := json.Marshal(capability)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT OR IGNORE INTO model_capabilities(
		model_capability_id, model_catalog_snapshot_id, adapter_id, payload_json
	) VALUES (?, ?, ?, ?)`,
		capability.ModelCapabilityID, capability.ModelCatalogSnapshotID, capability.AdapterID, string(payload))
	return err
}

func upsertQuotaTelemetrySource(ctx context.Context, tx storage.Tx, source QuotaTelemetrySource) error {
	source = normalizeQuotaTelemetrySource(source)
	if err := ValidateQuotaTelemetrySource(source); err != nil {
		return err
	}
	payload, err := json.Marshal(source)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO quota_telemetry_sources(
		quota_source_id, schema_version, record_version, adapter_id, source_kind,
		source_key, source_schema_version, network_declared, unsupported_reason,
		payload_json
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(quota_source_id) DO UPDATE SET
		record_version = excluded.record_version,
		source_kind = excluded.source_kind,
		source_key = excluded.source_key,
		source_schema_version = excluded.source_schema_version,
		network_declared = excluded.network_declared,
		unsupported_reason = excluded.unsupported_reason,
		payload_json = excluded.payload_json`,
		source.QuotaSourceID, source.SchemaVersion, source.RecordVersion,
		source.AdapterID, string(source.SourceKind), source.SourceKey,
		source.SourceSchemaVersion, boolInt(source.NetworkDeclared),
		source.UnsupportedReason, string(payload))
	return err
}

func insertQuotaSnapshot(ctx context.Context, tx storage.Tx, snapshot QuotaSnapshot) error {
	snapshot = normalizeQuotaSnapshot(snapshot)
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT OR IGNORE INTO quota_snapshots(
		quota_snapshot_id, quota_source_id, source_kind, adapter_id, scope_key,
		quantity_kind, unit, window_kind, confidence, freshness_state,
		captured_at, stale_after, terminal_error_code, payload_json
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		snapshot.QuotaSnapshotID, snapshot.QuotaSourceID, string(snapshot.SourceKind),
		snapshot.AdapterID, snapshot.ScopeKey, string(snapshot.QuantityKind),
		snapshot.Unit, string(snapshot.WindowKind), string(snapshot.Confidence),
		string(snapshot.FreshnessState), snapshot.CapturedAt, snapshot.StaleAfter,
		snapshot.TerminalErrorCode, string(payload))
	return err
}

func upsertInstallation(ctx context.Context, tx storage.Tx, installation ProviderInstallation) error {
	payload, err := json.Marshal(installation)
	if err != nil {
		return err
	}
	knownLimitations, err := marshalJSONText(installation.KnownLimitations)
	if err != nil {
		return err
	}
	gaps, err := marshalJSONText(installation.GapReasons)
	if err != nil {
		return err
	}
	identity, err := marshalJSONText(installation.ExecutableIdentity)
	if err != nil {
		return err
	}
	source, err := marshalJSONText(installation.Source)
	if err != nil {
		return err
	}
	evidence, err := marshalJSONText(installation.Evidence)
	if err != nil {
		return err
	}
	createdBy, err := marshalJSONText(installation.CreatedBy)
	if err != nil {
		return err
	}
	updatedBy, err := marshalJSONText(installation.UpdatedBy)
	if err != nil {
		return err
	}
	host, err := marshalJSONText(installation.Host)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO provider_installations(
		provider_installation_id, schema_version, record_version, adapter_id, adapter_declaration_id,
		scope, project_id, provider_display_name, executable_name, executable_identity_json, canonical_path,
		canonical_path_redacted, discovery_source, discovery_order, platform, version,
		version_confidence, latest_probe_result_id, installation_state, usable_for_invocation,
		known_limitations_json, created_at, updated_at, captured_at, stale_after,
		created_by_json, updated_by_json, host_json, policy_version,
		freshness_state, confidence, side_effect_class, classification, source_json,
		evidence_json, gap_reasons_json, terminal_error_code, payload_json
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(provider_installation_id) DO UPDATE SET
		record_version = provider_installations.record_version + 1,
		adapter_declaration_id = excluded.adapter_declaration_id,
		provider_display_name = excluded.provider_display_name,
		executable_name = excluded.executable_name,
		executable_identity_json = excluded.executable_identity_json,
		canonical_path_redacted = excluded.canonical_path_redacted,
		discovery_source = excluded.discovery_source,
		discovery_order = excluded.discovery_order,
		platform = excluded.platform,
		version = excluded.version,
		version_confidence = excluded.version_confidence,
		latest_probe_result_id = excluded.latest_probe_result_id,
		installation_state = excluded.installation_state,
		usable_for_invocation = excluded.usable_for_invocation,
		known_limitations_json = excluded.known_limitations_json,
		updated_at = excluded.updated_at,
		captured_at = excluded.captured_at,
		stale_after = excluded.stale_after,
		updated_by_json = excluded.updated_by_json,
		host_json = excluded.host_json,
		policy_version = excluded.policy_version,
		freshness_state = excluded.freshness_state,
		confidence = excluded.confidence,
		side_effect_class = excluded.side_effect_class,
		classification = excluded.classification,
		source_json = excluded.source_json,
		evidence_json = excluded.evidence_json,
		gap_reasons_json = excluded.gap_reasons_json,
		terminal_error_code = excluded.terminal_error_code,
		payload_json = excluded.payload_json`,
		installation.ProviderInstallationID, installation.SchemaVersion, installation.RecordVersion,
		installation.AdapterID, installation.AdapterDeclarationID, installation.Scope, installation.ProjectID, installation.ProviderDisplayName,
		installation.ExecutableName, identity,
		installation.CanonicalPathRedacted, installation.CanonicalPathRedacted, string(installation.DiscoverySource), installation.DiscoveryOrder,
		installation.Platform, installation.Version, string(installation.VersionConfidence),
		installation.LatestProbeResultID, string(installation.InstallationState), installation.UsableForInvocation,
		knownLimitations, installation.CreatedAt, installation.UpdatedAt, installation.CapturedAt,
		installation.StaleAfter, createdBy, updatedBy, host, installation.PolicyVersion,
		string(installation.FreshnessState), string(installation.Confidence),
		installation.SideEffectClass, installation.Classification, source,
		evidence, gaps, installation.TerminalErrorCode, string(payload))
	return err
}

func marshalJSONText(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func fingerprint(report Report) (string, error) {
	clone := report
	clone.GeneratedAt = ""
	clone.InventoryFingerprint = ""
	data, err := json.Marshal(clone)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func randomProbeID() string {
	var buf [16]byte
	if _, err := io.ReadFull(rand.Reader, buf[:]); err != nil {
		return "probe_" + hashBase32(time.Now().UTC().Format(time.RFC3339Nano))[:26]
	}
	return "probe_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf[:]))
}

func hashHex(parts ...string) string {
	sum := sha256.New()
	for _, part := range parts {
		sum.Write([]byte(part))
		sum.Write([]byte{0})
	}
	return hex.EncodeToString(sum.Sum(nil))
}

func hashBase32(parts ...string) string {
	sum := sha256.New()
	for _, part := range parts {
		sum.Write([]byte(part))
		sum.Write([]byte{0})
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum.Sum(nil)))
}

func normalizeDeps(deps Deps) Deps {
	defaults := DefaultDeps()
	if deps.Getenv == nil {
		deps.Getenv = defaults.Getenv
	}
	if deps.LookupEnv == nil {
		deps.LookupEnv = defaults.LookupEnv
	}
	if deps.UserHomeDir == nil {
		deps.UserHomeDir = defaults.UserHomeDir
	}
	if deps.Lstat == nil {
		deps.Lstat = defaults.Lstat
	}
	if deps.Abs == nil {
		deps.Abs = defaults.Abs
	}
	if deps.EvalSymlinks == nil {
		deps.EvalSymlinks = defaults.EvalSymlinks
	}
	if deps.RunProbe == nil {
		deps.RunProbe = defaults.RunProbe
	}
	if deps.RunCodexRPC == nil {
		deps.RunCodexRPC = defaults.RunCodexRPC
	}
	if deps.RunClaudePTY == nil {
		deps.RunClaudePTY = defaults.RunClaudePTY
	}
	if deps.RunGrokACP == nil {
		deps.RunGrokACP = defaults.RunGrokACP
	}
	if deps.RunGrokCredits == nil {
		deps.RunGrokCredits = defaults.RunGrokCredits
	}
	if deps.RunCodexBar == nil {
		deps.RunCodexBar = defaults.RunCodexBar
	}
	if deps.MkdirTemp == nil {
		deps.MkdirTemp = defaults.MkdirTemp
	}
	if deps.RemoveAll == nil {
		deps.RemoveAll = defaults.RemoveAll
	}
	if deps.WriteFile == nil {
		deps.WriteFile = defaults.WriteFile
	}
	if deps.RandomID == nil {
		deps.RandomID = defaults.RandomID
	}
	return deps
}

func normalizeNow(now func() time.Time) func() time.Time {
	if now == nil {
		return time.Now
	}
	return now
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
