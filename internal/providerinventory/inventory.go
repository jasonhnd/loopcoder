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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
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

	InstallProbeTimeout = 5 * time.Second
	StdoutLimitBytes    = 64 * 1024
	StderrLimitBytes    = 64 * 1024
	CombinedLimitBytes  = 128 * 1024
	AuthProbeTimeout    = 10 * time.Second
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
	ProbeMethodLookPath      ProbeMethod = "look-path"
	ProbeMethodLstat         ProbeMethod = "lstat"
	ProbeMethodFixedCommand  ProbeMethod = "fixed-command"
	ProbeMethodConfigured    ProbeMethod = "configured-overlay"
	ProbeMethodStatusCommand ProbeMethod = "sanctioned-status-command"
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
)

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
	EvidenceEnvNameExistence   EvidenceKind = "environment-name-existence"
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
	case ProbeMethodLookPath, ProbeMethodLstat, ProbeMethodFixedCommand, ProbeMethodConfigured, ProbeMethodStatusCommand:
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
	case NetworkNotNeeded, NetworkDenied:
		*p = NetworkPermission(value)
		return nil
	default:
		return fmt.Errorf("%w: unknown network_permission %q", ErrInvalidRecord, value)
	}
}

func (s *ProfileSource) UnmarshalJSON(data []byte) error {
	value, err := unmarshalEnumString(data, "profile_source")
	if err != nil {
		return err
	}
	switch ProfileSource(value) {
	case ProfileSourceStatusCommand, ProfileSourceConfigRef, ProfileSourceEnvRef, ProfileSourceUserConfig, ProfileSourceFixture, ProfileSourceUnknown:
		*s = ProfileSource(value)
		return nil
	default:
		return fmt.Errorf("%w: unknown profile_source %q", ErrInvalidRecord, value)
	}
}

func (s *SelectionState) UnmarshalJSON(data []byte) error {
	value, err := unmarshalEnumString(data, "selection_state")
	if err != nil {
		return err
	}
	switch SelectionState(value) {
	case SelectionDefault, SelectionExplicit, SelectionCandidate, SelectionSuperseded, SelectionUnknown:
		*s = SelectionState(value)
		return nil
	default:
		return fmt.Errorf("%w: unknown selection_state %q", ErrInvalidRecord, value)
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
	case EvidenceExitCode, EvidenceStatusCommand, EvidenceMachineStatus, EvidenceFileExistence, EvidenceEnvNameExistence, EvidenceProviderErrorClass, EvidenceUnsupported, EvidenceNotRun:
		*k = EvidenceKind(value)
		return nil
	default:
		return fmt.Errorf("%w: unknown evidence_kind %q", ErrInvalidRecord, value)
	}
}

func (s *AuthorizationScopeState) UnmarshalJSON(data []byte) error {
	value, err := unmarshalEnumString(data, "authorization_scope_state")
	if err != nil {
		return err
	}
	switch AuthorizationScopeState(value) {
	case AuthorizationAllKnown, AuthorizationPartial, AuthorizationUnknown, AuthorizationNotApplicable:
		*s = AuthorizationScopeState(value)
		return nil
	default:
		return fmt.Errorf("%w: unknown authorization_scope_state %q", ErrInvalidRecord, value)
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
	ModelCatalogSnapshots []any                  `json:"model_catalog_snapshots"`
	ModelCapabilities     []any                  `json:"model_capabilities"`
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
	StaleAfter                   string           `json:"stale_after,omitempty"`
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
	ExpiresAt                 string                  `json:"expires_at,omitempty"`
	RefreshRequired           bool                    `json:"refresh_required"`
	UnsupportedReason         string                  `json:"unsupported_reason,omitempty"`
	CreatedAt                 string                  `json:"created_at"`
	UpdatedAt                 string                  `json:"updated_at"`
	CreatedBy                 ActorProvenance         `json:"created_by"`
	UpdatedBy                 ActorProvenance         `json:"updated_by"`
	Host                      HostProvenance          `json:"host"`
	PolicyVersion             string                  `json:"policy_version"`
	CapturedAt                string                  `json:"captured_at"`
	StaleAfter                string                  `json:"stale_after,omitempty"`
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
	DisplayName                string
	Vendor                     string
	ExecutableNames            []string
	VersionArgv                []string
	AuthReadiness              AuthReadinessDeclaration
	KnownLimitations           []string
	MayNetwork                 bool
	ExplicitPaths              []string
	DiscoverySourceForExplicit DiscoverySource
}

type AuthReadinessDeclaration struct {
	CommandID         string
	Argv              []string
	Parser            string
	EnvSecretRefs     []string
	AuthArtifactPaths []string
	UnsupportedReason string
}

type Options struct {
	RepoPath string
	Config   config.Config
	Now      func() time.Time
}

type Deps struct {
	Getenv       func(string) string
	LookupEnv    func(string) (string, bool)
	UserHomeDir  func() (string, error)
	Lstat        func(string) (os.FileInfo, error)
	Abs          func(string) (string, error)
	EvalSymlinks func(string) (string, error)
	RunProbe     func(context.Context, ProbeExecution) (ProbeExecutionResult, error)
	RandomID     func() string
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

func DefaultDeps() Deps {
	return Deps{
		Getenv:       os.Getenv,
		LookupEnv:    os.LookupEnv,
		UserHomeDir:  os.UserHomeDir,
		Lstat:        os.Lstat,
		Abs:          filepath.Abs,
		EvalSymlinks: filepath.EvalSymlinks,
		RunProbe:     runProbeCommand,
		RandomID:     randomProbeID,
	}
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
	adapters := adapterDeclarations(opts.Config)
	var installations []ProviderInstallation
	var probes []ProbeResult
	var profiles []AccountProfile
	var readiness []AuthReadiness
	var gaps []string

	for _, adapter := range adapters {
		found := false
		candidates := discoverCandidates(adapter, deps)
		for _, candidate := range candidates {
			found = true
			installation, probe := inspectCandidate(ctx, adapter, candidate, now, deps)
			installations = append(installations, installation)
			probes = append(probes, probe)
			authProbes, authProfiles, authReadiness := inspectAuthReadiness(ctx, adapter, installation, candidate.path, now, deps)
			probes = append(probes, authProbes...)
			profiles = append(profiles, authProfiles...)
			readiness = append(readiness, authReadiness...)
		}
		if !found {
			probe := absentProbe(adapter, now, deps)
			probes = append(probes, probe)
			readiness = append(readiness, absentAuthReadiness(adapter, now))
			gaps = append(gaps, "provider-"+adapter.AdapterID+"-not-installed")
		}
	}
	sort.SliceStable(installations, func(i, j int) bool {
		if installations[i].AdapterID != installations[j].AdapterID {
			return installations[i].AdapterID < installations[j].AdapterID
		}
		if installations[i].DiscoveryOrder != installations[j].DiscoveryOrder {
			return installations[i].DiscoveryOrder < installations[j].DiscoveryOrder
		}
		return installations[i].ProviderInstallationID < installations[j].ProviderInstallationID
	})
	finalizeProfiles(profiles, opts.Config.ProviderInventory.ProfileOverrides)
	sort.SliceStable(profiles, func(i, j int) bool {
		if profiles[i].AdapterID != profiles[j].AdapterID {
			return profiles[i].AdapterID < profiles[j].AdapterID
		}
		return profiles[i].AccountProfileID < profiles[j].AccountProfileID
	})
	sort.SliceStable(readiness, func(i, j int) bool {
		if readiness[i].AdapterID != readiness[j].AdapterID {
			return readiness[i].AdapterID < readiness[j].AdapterID
		}
		return readiness[i].AuthReadinessID < readiness[j].AuthReadinessID
	})
	report := Report{
		SchemaVersion:         ProviderInventoryJSONSchema,
		GeneratedAt:           formatTime(now),
		Confidence:            confidenceForInventory(installations, probes),
		Installations:         installations,
		ProbeResults:          probes,
		AccountProfiles:       profiles,
		AuthReadiness:         readiness,
		ModelCatalogSnapshots: []any{},
		ModelCapabilities:     []any{},
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
		rows, err = tx.Query(ctx, `SELECT payload_json FROM auth_readiness ORDER BY adapter_id, auth_readiness_id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var payload string
			if err := rows.Scan(&payload); err != nil {
				rows.Close()
				return err
			}
			var entry AuthReadiness
			if err := json.Unmarshal([]byte(payload), &entry); err != nil {
				rows.Close()
				return err
			}
			readiness = append(readiness, entry)
		}
		return rows.Close()
	})
	if err != nil {
		return Report{}, err
	}
	now := time.Now().UTC()
	report := Report{
		SchemaVersion:         ProviderInventoryJSONSchema,
		GeneratedAt:           formatTime(now),
		Confidence:            confidenceForInventory(installations, probes),
		Installations:         installations,
		ProbeResults:          probes,
		AccountProfiles:       profiles,
		AuthReadiness:         readiness,
		ModelCatalogSnapshots: []any{},
		ModelCapabilities:     []any{},
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

type authCapture struct {
	ProfileReference          string
	ProfileDisplay            string
	ProviderAccountKind       string
	TenantOrProjectReference  string
	Selection                 SelectionState
	AllowedScopeSummary       string
	State                     ReadinessState
	Confidence                Confidence
	EvidenceKind              EvidenceKind
	AuthorizationScopeState   AuthorizationScopeState
	AuthorizationScopeSummary string
	ExpiresAt                 string
	RefreshRequired           bool
	UnsupportedReason         string
	GapReasons                []string
	TerminalErrorCode         string
}

func inspectAuthReadiness(ctx context.Context, adapter AdapterDeclaration, installation ProviderInstallation, executablePath string, now time.Time, deps Deps) ([]ProbeResult, []AccountProfile, []AuthReadiness) {
	if len(adapter.AuthReadiness.Argv) == 0 {
		capture := authReadinessFromReferences(adapter, installation, now, deps)
		return nil, nil, []AuthReadiness{authReadinessRecord(adapter, installation, nil, capture, now)}
	}

	argv := append([]string{executablePath}, adapter.AuthReadiness.Argv...)
	env := probeEnvironment(deps.Getenv)
	probe := baseProbe(adapter, now, deps)
	probe.ProbeKind = "auth-readiness"
	probe.ProviderInstallationID = &installation.ProviderInstallationID
	probe.ProbeCommandID = adapter.AuthReadiness.CommandID
	probe.ProbeMethod = ProbeMethodStatusCommand
	probe.Argv = redactArgv(argv)
	probe.EnvironmentKeys = environmentKeys(env)
	probe.TimeoutMS = int(AuthProbeTimeout / time.Millisecond)
	probe.StdoutLimitBytes = StdoutLimitBytes
	probe.StderrLimitBytes = StderrLimitBytes
	probe.CombinedOutputLimitBytes = CombinedLimitBytes
	probe.Source = SourceDescriptor{Kind: "command", AdapterID: adapter.AdapterID, ProbeCommandID: adapter.AuthReadiness.CommandID, ExecutableName: filepath.Base(executablePath)}
	probe.Evidence = EvidenceSummary{Kind: "bounded-auth-readiness-command", CommandBounded: true, NoShell: true, RepositoryMutation: false, SecretMaterialRetained: false}

	result, err := deps.RunProbe(ctx, ProbeExecution{
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
	probe.Confidence = ConfidenceExact
	probe.Outcome = OutcomeInstalled
	if err != nil || result.TimedOut || result.Killed {
		probe.Outcome = OutcomeProbeFailed
		probe.Confidence = ConfidenceUnknown
		probe.TerminalErrorCode = "ErrAuthProbeExecutionFailed"
		probe.GapReasons = append(probe.GapReasons, "auth-readiness-probe-failed")
	}

	captures := parseAuthCaptures(adapter, result, err)
	if len(captures) == 0 {
		captures = []authCapture{unknownAuthCapture("auth-readiness-unparseable", "safe auth readiness command did not return a supported status shape")}
	}
	probe.ParsedFields = map[string]string{
		"readiness_state": string(captures[0].State),
		"profile_count":   fmt.Sprintf("%d", len(captures)),
	}
	if captures[0].AuthorizationScopeState != "" {
		probe.ParsedFields["authorization_scope_state"] = string(captures[0].AuthorizationScopeState)
	}
	if captures[0].State == ReadinessNotAuthenticated || captures[0].State == ReadinessExpired {
		probe.Outcome = OutcomeInstalledUnusable
		probe.Confidence = captures[0].Confidence
	}
	if captures[0].State == ReadinessUnknown && probe.Outcome == OutcomeInstalled {
		probe.Outcome = OutcomeInstalledUnusable
		probe.Confidence = ConfidenceUnknown
	}

	var profiles []AccountProfile
	var readiness []AuthReadiness
	for _, capture := range captures {
		var profileID *string
		if strings.TrimSpace(capture.ProfileReference) != "" || strings.TrimSpace(capture.ProfileDisplay) != "" {
			profile := accountProfileRecord(adapter, installation, capture, now)
			profileID = &profile.AccountProfileID
			captureReadiness := authReadinessRecord(adapter, installation, profileID, capture, now)
			profile.LatestAuthReadinessID = captureReadiness.AuthReadinessID
			profiles = append(profiles, profile)
			readiness = append(readiness, captureReadiness)
			continue
		}
		readiness = append(readiness, authReadinessRecord(adapter, installation, nil, capture, now))
	}
	return []ProbeResult{probe}, profiles, readiness
}

func absentAuthReadiness(adapter AdapterDeclaration, now time.Time) AuthReadiness {
	capture := unknownAuthCapture("provider-executable-not-installed", "provider executable not installed; auth readiness was not run")
	capture.EvidenceKind = EvidenceNotRun
	return authReadinessRecord(adapter, ProviderInstallation{}, nil, capture, now)
}

func authReadinessFromReferences(adapter AdapterDeclaration, installation ProviderInstallation, now time.Time, deps Deps) authCapture {
	for _, key := range adapter.AuthReadiness.EnvSecretRefs {
		if _, ok := deps.LookupEnv(key); ok {
			return authCapture{
				State:                     ReadinessUnknown,
				Confidence:                ConfidenceUnknown,
				EvidenceKind:              EvidenceEnvNameExistence,
				AuthorizationScopeState:   AuthorizationUnknown,
				AuthorizationScopeSummary: "secret reference exists; credential validity unknown",
				UnsupportedReason:         firstNonEmpty(adapter.AuthReadiness.UnsupportedReason, "adapter has no credential-blind status command"),
				GapReasons:                []string{"auth-reference-present-validity-unknown"},
			}
		}
	}
	for _, rawPath := range adapter.AuthReadiness.AuthArtifactPaths {
		path, err := expandAuthArtifactPath(rawPath, deps)
		if err != nil {
			return unknownAuthCapture("auth-reference-inaccessible", "declared auth reference path could not be resolved")
		}
		if _, err := deps.Lstat(path); err == nil {
			return authCapture{
				State:                     ReadinessUnknown,
				Confidence:                ConfidenceUnknown,
				EvidenceKind:              EvidenceFileExistence,
				AuthorizationScopeState:   AuthorizationUnknown,
				AuthorizationScopeSummary: "auth artifact exists; credential validity unknown",
				UnsupportedReason:         firstNonEmpty(adapter.AuthReadiness.UnsupportedReason, "adapter has no credential-blind status command"),
				GapReasons:                []string{"auth-artifact-present-validity-unknown"},
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			capture := unknownAuthCapture("auth-reference-inaccessible", "declared auth reference path was inaccessible")
			capture.EvidenceKind = EvidenceFileExistence
			capture.TerminalErrorCode = "ErrAuthReferenceInaccessible"
			return capture
		}
	}
	reason := firstNonEmpty(adapter.AuthReadiness.UnsupportedReason, "adapter declares no safe auth readiness mechanism")
	if len(adapter.AuthReadiness.EnvSecretRefs) > 0 || len(adapter.AuthReadiness.AuthArtifactPaths) > 0 {
		return authCapture{
			State:                     ReadinessNotAuthenticated,
			Confidence:                ConfidenceEstimated,
			EvidenceKind:              EvidenceFileExistence,
			AuthorizationScopeState:   AuthorizationNotApplicable,
			AuthorizationScopeSummary: "declared auth references are absent",
			RefreshRequired:           true,
			UnsupportedReason:         reason,
			GapReasons:                []string{"declared-auth-references-absent"},
		}
	}
	return unknownAuthCapture("auth-readiness-unsupported", reason)
}

func parseAuthCaptures(adapter AdapterDeclaration, result ProbeExecutionResult, runErr error) []authCapture {
	output := strings.TrimSpace(result.Stdout + "\n" + result.Stderr)
	if runErr != nil || result.TimedOut || result.Killed {
		capture := unknownAuthCapture("auth-readiness-probe-failed", "safe auth readiness command failed")
		capture.TerminalErrorCode = "ErrAuthProbeExecutionFailed"
		return []authCapture{capture}
	}
	switch adapter.AuthReadiness.Parser {
	case "claude-auth-status-json":
		return parseClaudeAuthStatus(output, result.ExitCode)
	case "codex-login-status":
		return []authCapture{parseTextAuthStatus("codex", output, result.ExitCode)}
	case "agy-models":
		return []authCapture{parseTextAuthStatus("antigravity", output, result.ExitCode)}
	default:
		return []authCapture{parseTextAuthStatus(adapter.AdapterID, output, result.ExitCode)}
	}
}

func parseClaudeAuthStatus(output string, exitCode int) []authCapture {
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
		AuthMethod       string        `json:"authMethod"`
		APIProvider      string        `json:"apiProvider"`
		Email            string        `json:"email"`
		OrgID            string        `json:"orgId"`
		OrgName          string        `json:"orgName"`
		SubscriptionType string        `json:"subscriptionType"`
		Profiles         []profileJSON `json:"profiles"`
	}
	var parsed statusJSON
	if err := json.Unmarshal([]byte(output), &parsed); err != nil {
		return []authCapture{parseTextAuthStatus("claude", output, exitCode)}
	}
	if len(parsed.Profiles) == 0 {
		if !parsed.LoggedIn && firstNonEmpty(parsed.Email, parsed.OrgID, parsed.OrgName, parsed.AuthMethod, parsed.APIProvider, parsed.SubscriptionType) == "" {
			return []authCapture{{
				State:                     ReadinessNotAuthenticated,
				Confidence:                ConfidenceExact,
				EvidenceKind:              EvidenceMachineStatus,
				AuthorizationScopeState:   AuthorizationNotApplicable,
				AuthorizationScopeSummary: "provider reports not authenticated",
				RefreshRequired:           true,
				GapReasons:                []string{"provider-reports-not-authenticated"},
			}}
		}
		parsed.Profiles = []profileJSON{{
			Email:            parsed.Email,
			OrgID:            parsed.OrgID,
			OrgName:          parsed.OrgName,
			AuthMethod:       parsed.AuthMethod,
			APIProvider:      parsed.APIProvider,
			SubscriptionType: parsed.SubscriptionType,
		}}
	}
	captures := make([]authCapture, 0, len(parsed.Profiles))
	for index, profile := range parsed.Profiles {
		state := ReadinessReady
		confidence := ConfidenceExact
		refresh := false
		gaps := []string{}
		status := strings.ToLower(strings.TrimSpace(profile.Status))
		if !parsed.LoggedIn || strings.Contains(status, "not") || strings.Contains(status, "logged-out") {
			state = ReadinessNotAuthenticated
			refresh = true
			gaps = append(gaps, "provider-reports-not-authenticated")
		}
		if strings.Contains(status, "expired") || strings.Contains(status, "revoked") || strings.Contains(status, "refresh") {
			state = ReadinessExpired
			refresh = true
			gaps = append(gaps, "provider-reports-expired")
		}
		reference := firstNonEmpty(profile.ID, profile.Email, profile.OrgID, profile.OrgName, fmt.Sprintf("claude-profile-%d", index))
		display := firstNonEmpty(profile.Display, profile.Email, profile.OrgName, "Claude profile")
		summaryParts := []string{}
		if profile.APIProvider != "" {
			summaryParts = append(summaryParts, "api_provider="+boundedToken(profile.APIProvider, 48))
		}
		if profile.SubscriptionType != "" {
			summaryParts = append(summaryParts, "subscription="+boundedToken(profile.SubscriptionType, 48))
		}
		captures = append(captures, authCapture{
			ProfileReference:          reference,
			ProfileDisplay:            display,
			ProviderAccountKind:       "user",
			TenantOrProjectReference:  firstNonEmpty(profile.OrgID, profile.OrgName),
			Selection:                 selectionForIndex(index),
			AllowedScopeSummary:       strings.Join(summaryParts, "; "),
			State:                     state,
			Confidence:                confidence,
			EvidenceKind:              EvidenceMachineStatus,
			AuthorizationScopeState:   AuthorizationUnknown,
			AuthorizationScopeSummary: "model authorization unknown",
			RefreshRequired:           refresh,
			GapReasons:                gaps,
		})
	}
	return captures
}

func parseTextAuthStatus(adapterID, output string, exitCode int) authCapture {
	lower := strings.ToLower(output)
	capture := authCapture{
		State:                     ReadinessUnknown,
		Confidence:                ConfidenceUnknown,
		EvidenceKind:              EvidenceStatusCommand,
		AuthorizationScopeState:   AuthorizationUnknown,
		AuthorizationScopeSummary: "authorization scope unknown",
		UnsupportedReason:         "",
		GapReasons:                []string{},
	}
	if exitCode == 0 {
		capture.State = ReadinessReady
		capture.Confidence = ConfidenceExact
		capture.ProfileReference = firstNonEmpty(firstNonEmptyLine(output), adapterID+"-default")
		capture.ProfileDisplay = capture.ProfileReference
		capture.Selection = SelectionDefault
		capture.AuthorizationScopeSummary = "readiness command succeeded; model authorization may vary"
	}
	if strings.Contains(lower, "not logged") || strings.Contains(lower, "not authenticated") || strings.Contains(lower, "login required") || strings.Contains(lower, "no credentials") {
		capture.State = ReadinessNotAuthenticated
		capture.Confidence = ConfidenceExact
		capture.RefreshRequired = true
		capture.GapReasons = append(capture.GapReasons, "provider-reports-not-authenticated")
	}
	if strings.Contains(lower, "expired") || strings.Contains(lower, "revoked") || strings.Contains(lower, "refresh") {
		capture.State = ReadinessExpired
		capture.Confidence = ConfidenceExact
		capture.RefreshRequired = true
		capture.GapReasons = append(capture.GapReasons, "provider-reports-expired")
	}
	if strings.Contains(lower, "permission") || strings.Contains(lower, "not authorized") || strings.Contains(lower, "forbidden") || strings.Contains(lower, "ineligible") || strings.Contains(lower, "unsupported_client") {
		capture.State = ReadinessUnknown
		capture.Confidence = ConfidenceExact
		capture.EvidenceKind = EvidenceProviderErrorClass
		capture.AuthorizationScopeState = AuthorizationPartial
		capture.AuthorizationScopeSummary = "provider reports partial or unavailable authorization"
		capture.GapReasons = append(capture.GapReasons, "provider-authorization-partial")
	}
	if exitCode != 0 && len(capture.GapReasons) == 0 {
		capture.State = ReadinessUnknown
		capture.Confidence = ConfidenceUnknown
		capture.EvidenceKind = EvidenceProviderErrorClass
		capture.GapReasons = append(capture.GapReasons, "auth-readiness-command-nonzero-exit")
		capture.TerminalErrorCode = "ErrAuthProbeNonZeroExit"
	}
	return capture
}

func unknownAuthCapture(gap, reason string) authCapture {
	return authCapture{
		State:                     ReadinessUnknown,
		Confidence:                ConfidenceUnknown,
		EvidenceKind:              EvidenceUnsupported,
		AuthorizationScopeState:   AuthorizationUnknown,
		AuthorizationScopeSummary: "authorization scope unknown",
		UnsupportedReason:         reason,
		GapReasons:                []string{gap},
	}
}

func accountProfileRecord(adapter AdapterDeclaration, installation ProviderInstallation, capture authCapture, now time.Time) AccountProfile {
	source := ProfileSourceStatusCommand
	reference := firstNonEmpty(capture.ProfileReference, capture.ProfileDisplay, adapter.AdapterID+"-default")
	referenceHash := "sha256:" + hashHex(adapter.AdapterID, string(source), strings.ToLower(strings.TrimSpace(reference)))
	accountProfileID := "acct_" + hashBase32(adapter.AdapterID, string(source), referenceHash)[:32]
	installationID := installation.ProviderInstallationID
	tenantHash := ""
	if strings.TrimSpace(capture.TenantOrProjectReference) != "" {
		tenantHash = "sha256:" + hashHex(adapter.AdapterID, strings.TrimSpace(capture.TenantOrProjectReference))
	}
	selection := capture.Selection
	if selection == "" {
		selection = SelectionCandidate
	}
	return AccountProfile{
		SchemaVersion:                AccountProfileSchema,
		RecordVersion:                1,
		Scope:                        "machine",
		ProjectID:                    nil,
		AccountProfileID:             accountProfileID,
		AdapterID:                    adapter.AdapterID,
		ProviderInstallationID:       &installationID,
		ProfileSource:                source,
		ProfileReferenceHash:         referenceHash,
		ProfileDisplay:               safeDisplay(capture.ProfileDisplay),
		ProviderAccountKind:          boundedToken(capture.ProviderAccountKind, 48),
		TenantOrProjectReferenceHash: tenantHash,
		SelectionState:               selection,
		AllowedScopeSummary:          safeSummary(capture.AllowedScopeSummary),
		CollisionSet:                 []string{},
		CreatedAt:                    formatTime(now),
		UpdatedAt:                    formatTime(now),
		CreatedBy:                    defaultActorProvenance(),
		UpdatedBy:                    defaultActorProvenance(),
		Host:                         hostProvenance(),
		PolicyVersion:                PolicyVersion,
		CapturedAt:                   formatTime(now),
		StaleAfter:                   formatTime(now.Add(24 * time.Hour)),
		FreshnessState:               FreshnessFresh,
		Confidence:                   capture.Confidence,
		SideEffectClass:              "local-read",
		Classification:               "secret-reference",
		Source:                       SourceDescriptor{Kind: "auth-readiness-profile", AdapterID: adapter.AdapterID, ProbeCommandID: adapter.AuthReadiness.CommandID},
		Evidence:                     EvidenceSummary{Kind: "credential-blind-profile-reference", CommandBounded: true, NoShell: true, RepositoryMutation: false, SecretMaterialRetained: false},
		GapReasons:                   copyStringSlice(capture.GapReasons),
		TerminalErrorCode:            capture.TerminalErrorCode,
	}
}

func authReadinessRecord(adapter AdapterDeclaration, installation ProviderInstallation, profileID *string, capture authCapture, now time.Time) AuthReadiness {
	var installationID *string
	if installation.ProviderInstallationID != "" {
		id := installation.ProviderInstallationID
		installationID = &id
	}
	confidence := capture.Confidence
	if confidence == "" {
		confidence = ConfidenceUnknown
	}
	evidenceKind := capture.EvidenceKind
	if evidenceKind == "" {
		evidenceKind = EvidenceUnsupported
	}
	scopeState := capture.AuthorizationScopeState
	if scopeState == "" {
		scopeState = AuthorizationUnknown
	}
	return AuthReadiness{
		SchemaVersion:             AuthReadinessSchema,
		RecordVersion:             1,
		Scope:                     "machine",
		ProjectID:                 nil,
		AuthReadinessID:           randomAuthReadinessID(),
		AdapterID:                 adapter.AdapterID,
		ProviderInstallationID:    installationID,
		AccountProfileID:          profileID,
		ReadinessState:            capture.State,
		ReadinessConfidence:       confidence,
		EvidenceKind:              evidenceKind,
		AuthorizationScopeState:   scopeState,
		AuthorizationScopeSummary: safeSummary(capture.AuthorizationScopeSummary),
		ExpiresAt:                 capture.ExpiresAt,
		RefreshRequired:           capture.RefreshRequired,
		UnsupportedReason:         safeSummary(capture.UnsupportedReason),
		CreatedAt:                 formatTime(now),
		UpdatedAt:                 formatTime(now),
		CreatedBy:                 defaultActorProvenance(),
		UpdatedBy:                 defaultActorProvenance(),
		Host:                      hostProvenance(),
		PolicyVersion:             PolicyVersion,
		CapturedAt:                formatTime(now),
		StaleAfter:                formatTime(now.Add(24 * time.Hour)),
		FreshnessState:            FreshnessFresh,
		Confidence:                confidence,
		SideEffectClass:           "local-read",
		Classification:            "local-diagnostic",
		Source:                    SourceDescriptor{Kind: "auth-readiness", AdapterID: adapter.AdapterID, ProbeCommandID: adapter.AuthReadiness.CommandID},
		Evidence:                  EvidenceSummary{Kind: string(evidenceKind), CommandBounded: true, NoShell: true, RepositoryMutation: false, SecretMaterialRetained: false},
		GapReasons:                copyStringSlice(capture.GapReasons),
		TerminalErrorCode:         capture.TerminalErrorCode,
	}
}

func finalizeProfiles(profiles []AccountProfile, overrides map[string]string) {
	byDisplay := map[string][]int{}
	for i := range profiles {
		key := strings.ToLower(strings.TrimSpace(profiles[i].AdapterID + "\x00" + profiles[i].ProfileDisplay))
		if strings.TrimSpace(profiles[i].ProfileDisplay) != "" {
			byDisplay[key] = append(byDisplay[key], i)
		}
		if override := strings.TrimSpace(overrides[profiles[i].AdapterID]); override != "" {
			if override == profiles[i].AccountProfileID {
				profiles[i].SelectionState = SelectionExplicit
			} else if profiles[i].SelectionState == SelectionDefault {
				profiles[i].SelectionState = SelectionCandidate
			}
		}
	}
	for _, indexes := range byDisplay {
		if len(indexes) < 2 {
			continue
		}
		ids := make([]string, 0, len(indexes))
		for _, index := range indexes {
			ids = append(ids, profiles[index].AccountProfileID)
		}
		sort.Strings(ids)
		for _, index := range indexes {
			profiles[index].CollisionSet = append([]string(nil), ids...)
			if profiles[index].SelectionState == SelectionDefault {
				profiles[index].SelectionState = SelectionCandidate
			}
		}
	}
}

func expandAuthArtifactPath(path string, deps Deps) (string, error) {
	path = strings.TrimSpace(path)
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) || path == "~" {
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

func randomAuthReadinessID() string {
	var buf [16]byte
	if _, err := io.ReadFull(rand.Reader, buf[:]); err != nil {
		return "auth_" + hashBase32(time.Now().UTC().Format(time.RFC3339Nano))[:26]
	}
	return "auth_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf[:]))
}

func selectionForIndex(index int) SelectionState {
	if index == 0 {
		return SelectionDefault
	}
	return SelectionCandidate
}

func safeDisplay(value string) string {
	value = safeSummary(value)
	value = emailPattern.ReplaceAllStringFunc(value, redactEmail)
	if secretLike(value) {
		return "[REDACTED]"
	}
	return value
}

func safeSummary(value string) string {
	value, _ = redactProviderOutput(value)
	value = strings.TrimSpace(strings.ReplaceAll(value, "\r\n", "\n"))
	if len(value) > 240 {
		value = value[:240] + "..."
	}
	return value
}

func boundedToken(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) > limit {
		return value[:limit]
	}
	return value
}

func copyStringSlice(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	return append([]string(nil), values...)
}

func secretLike(value string) bool {
	for _, pattern := range secretPatterns {
		if pattern.MatchString(value) {
			return true
		}
	}
	return genericKeyValueSecretPattern.MatchString(value)
}

var emailPattern = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

func redactEmail(email string) string {
	parts := strings.Split(email, "@")
	if len(parts) != 2 || parts[0] == "" {
		return "[REDACTED_EMAIL]"
	}
	local := parts[0]
	if len(local) > 1 {
		local = local[:1] + "***"
	} else {
		local = "***"
	}
	return local + "@" + parts[1]
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
		NetworkDeclared:          adapter.MayNetwork,
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
	if adapter.MayNetwork {
		probe.NetworkPermission = NetworkDenied
		probe.GapReasons = append(probe.GapReasons, "network-permission-denied")
	}
	return probe
}

func adapterDeclarations(cfg config.Config) []AdapterDeclaration {
	byID := map[string]AdapterDeclaration{}
	for _, provider := range runtimecap.DefaultContract().Providers {
		byID[provider.Name] = declarationFromRuntime(provider)
	}
	for _, provider := range models.DefaultRegistry().Providers {
		decl := byID[provider.Name]
		if decl.AdapterID == "" {
			decl.AdapterID = provider.Name
			decl.DisplayName = provider.DisplayName
			decl.Vendor = provider.Vendor
			decl.ExecutableNames = []string{firstNonEmpty(provider.CLI, provider.Name)}
			decl.VersionArgv = []string{"--version"}
		}
		if decl.DisplayName == "" {
			decl.DisplayName = provider.DisplayName
		}
		if authReadinessDeclarationEmpty(decl.AuthReadiness) {
			decl.AuthReadiness = defaultAuthReadinessDeclaration(provider.Name)
		}
		byID[provider.Name] = decl
	}
	for _, provider := range configuredProviderNames(cfg) {
		if _, ok := byID[provider]; ok {
			continue
		}
		byID[provider] = AdapterDeclaration{
			AdapterID:        provider,
			DisplayName:      provider,
			Vendor:           "custom",
			ExecutableNames:  []string{provider},
			VersionArgv:      []string{"--version"},
			AuthReadiness:    defaultAuthReadinessDeclaration(provider),
			KnownLimitations: []string{"custom adapter has installation discovery only; auth, catalog, quota, and invocation readiness are unknown"},
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
				AdapterID:        adapterID,
				DisplayName:      adapterID,
				Vendor:           "custom",
				ExecutableNames:  []string{adapterID},
				VersionArgv:      []string{"--version"},
				AuthReadiness:    defaultAuthReadinessDeclaration(adapterID),
				KnownLimitations: []string{"custom adapter has installation discovery only; auth, catalog, quota, and invocation readiness are unknown"},
			}
		}
		decl.ExplicitPaths = append(decl.ExplicitPaths, paths...)
		byID[adapterID] = decl
	}
	out := make([]AdapterDeclaration, 0, len(byID))
	for _, decl := range byID {
		if len(decl.VersionArgv) == 0 {
			decl.VersionArgv = []string{"--version"}
		}
		out = append(out, decl)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AdapterID < out[j].AdapterID })
	return out
}

func declarationFromRuntime(provider runtimecap.ProviderRuntime) AdapterDeclaration {
	display := provider.Name
	if modelProvider, ok := models.LookupProvider(provider.Name); ok {
		display = firstNonEmpty(modelProvider.DisplayName, provider.Name)
	}
	return AdapterDeclaration{
		AdapterID:        provider.Name,
		DisplayName:      display,
		Vendor:           provider.Name,
		ExecutableNames:  []string{provider.Executable},
		VersionArgv:      []string{"--version"},
		AuthReadiness:    defaultAuthReadinessDeclaration(provider.Name),
		KnownLimitations: append([]string(nil), provider.KnownLimitations...),
	}
}

func defaultAuthReadinessDeclaration(adapterID string) AuthReadinessDeclaration {
	switch adapterID {
	case "codex":
		return AuthReadinessDeclaration{
			CommandID: "codex-login-status",
			Argv:      []string{"login", "status"},
			Parser:    "codex-login-status",
		}
	case "claude":
		return AuthReadinessDeclaration{
			CommandID: "claude-auth-status-json",
			Argv:      []string{"auth", "status", "--json"},
			Parser:    "claude-auth-status-json",
		}
	case "antigravity":
		return AuthReadinessDeclaration{
			CommandID: "agy-models",
			Argv:      []string{"models"},
			Parser:    "agy-models",
		}
	case "gemini":
		return AuthReadinessDeclaration{
			EnvSecretRefs:     []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"},
			AuthArtifactPaths: []string{"~/.gemini/oauth_creds.json"},
			UnsupportedReason: "gemini adapter has no dedicated credential-blind auth status command; only secret-reference existence can be reported",
		}
	default:
		return AuthReadinessDeclaration{
			UnsupportedReason: "adapter declares no safe credential-blind auth readiness mechanism",
		}
	}
}

func authReadinessDeclarationEmpty(decl AuthReadinessDeclaration) bool {
	return strings.TrimSpace(decl.CommandID) == "" &&
		len(decl.Argv) == 0 &&
		strings.TrimSpace(decl.Parser) == "" &&
		len(decl.EnvSecretRefs) == 0 &&
		len(decl.AuthArtifactPaths) == 0 &&
		strings.TrimSpace(decl.UnsupportedReason) == ""
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
	if len(out) == 0 {
		out = []string{"codex", "claude"}
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
	return "adapter_" + hashBase32(adapter.AdapterID, AdapterDeclarationSchema, AdapterVersion)[:32]
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
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
		installation.CanonicalPathRedacted, string(installation.DiscoverySource), installation.DiscoveryOrder,
		installation.Platform, installation.Version, string(installation.VersionConfidence),
		installation.LatestProbeResultID, string(installation.InstallationState), installation.UsableForInvocation,
		knownLimitations, installation.CreatedAt, installation.UpdatedAt, installation.CapturedAt,
		installation.StaleAfter, createdBy, updatedBy, host, installation.PolicyVersion,
		string(installation.FreshnessState), string(installation.Confidence),
		installation.SideEffectClass, installation.Classification, source,
		evidence, gaps, installation.TerminalErrorCode, string(payload))
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
		adapter_id = excluded.adapter_id,
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
	_, err = tx.Exec(ctx, `INSERT INTO auth_readiness(
		auth_readiness_id, adapter_id, provider_installation_id, account_profile_id, payload_json
	) VALUES (?, ?, ?, ?, ?)
	ON CONFLICT(auth_readiness_id) DO NOTHING`,
		readiness.AuthReadinessID, readiness.AdapterID, providerInstallationID, accountProfileID, string(payload))
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

func firstNonEmptyLine(text string) string {
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
