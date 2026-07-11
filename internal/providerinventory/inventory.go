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

	AdapterDeclarationSchema = "loopcoder.adapter_declaration.v1"
	AdapterVersion           = "v1"
	PolicyVersion            = "provider-inventory-v1"

	InstallProbeTimeout = 5 * time.Second
	StdoutLimitBytes    = 64 * 1024
	StderrLimitBytes    = 64 * 1024
	CombinedLimitBytes  = 128 * 1024
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
	case ProbeMethodLookPath, ProbeMethodLstat, ProbeMethodFixedCommand, ProbeMethodConfigured:
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
	AccountProfiles       []any                  `json:"account_profiles"`
	AuthReadiness         []any                  `json:"auth_readiness"`
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
	KnownLimitations           []string
	MayNetwork                 bool
	ExplicitPaths              []string
	DiscoverySourceForExplicit DiscoverySource
}

type Options struct {
	RepoPath string
	Config   config.Config
	Now      func() time.Time
}

type Deps struct {
	Getenv       func(string) string
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
	var gaps []string

	for _, adapter := range adapters {
		found := false
		candidates := discoverCandidates(adapter, deps)
		for _, candidate := range candidates {
			found = true
			installation, probe := inspectCandidate(ctx, adapter, candidate, now, deps)
			installations = append(installations, installation)
			probes = append(probes, probe)
		}
		if !found {
			probe := absentProbe(adapter, now, deps)
			probes = append(probes, probe)
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
	report := Report{
		SchemaVersion:         ProviderInventoryJSONSchema,
		GeneratedAt:           formatTime(now),
		Confidence:            confidenceForInventory(installations, probes),
		Installations:         installations,
		ProbeResults:          probes,
		AccountProfiles:       []any{},
		AuthReadiness:         []any{},
		ModelCatalogSnapshots: []any{},
		ModelCapabilities:     []any{},
		GapReasons:            gaps,
	}
	report.InventoryFingerprint = fingerprint(report)
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
		AccountProfiles:       []any{},
		AuthReadiness:         []any{},
		ModelCatalogSnapshots: []any{},
		ModelCapabilities:     []any{},
		GapReasons:            []string{},
	}
	report.InventoryFingerprint = fingerprint(report)
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
	version := parseVersion(result.Stdout, result.Stderr)
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
		KnownLimitations: append([]string(nil), provider.KnownLimitations...),
	}
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
	cmd.Env = append([]string{}, req.Env...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	result, err := supervisedexec.Run(ctx, cmd, supervisedexec.Options{HardCap: req.Timeout, LivenessMode: supervisedexec.LivenessModeLogOnly, Role: "provider-probe"})
	out := ProbeExecutionResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: result.ExitCode,
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
	regexp.MustCompile(`[:=]\s*["']?[A-Fa-f0-9]{32,}["']?`),
	regexp.MustCompile(`[:=]\s*["']?[A-Za-z0-9+/=_-]{32,}["']?`),
}

func probeEnvironment(getenv func(string) string) []string {
	if getenv == nil {
		getenv = os.Getenv
	}
	seen := map[string]bool{}
	var env []string
	for _, key := range allowedProbeEnvKeys() {
		if seen[key] {
			continue
		}
		seen[key] = true
		value := getenv(key)
		if value == "" {
			continue
		}
		env = append(env, key+"="+value)
	}
	return env
}

func allowedProbeEnvKeys() []string {
	return []string{
		"PATH",
		"PATHEXT",
		"HOME",
		"USERPROFILE",
		"SystemRoot",
		"WINDIR",
		"TEMP",
		"TMP",
		"TMPDIR",
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

func fingerprint(report Report) string {
	clone := report
	clone.GeneratedAt = ""
	clone.InventoryFingerprint = ""
	data, _ := json.Marshal(clone)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
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
