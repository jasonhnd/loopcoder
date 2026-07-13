// Package config reads loopcoder repository configuration.
package config

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/gitutil"
	"github.com/jasonhnd/loopcoder/internal/hostprofile"
	"github.com/jasonhnd/loopcoder/internal/migration"
)

const (
	roleDefinitionSchema = "loopcoder.role_definition.v1"
)

type Config struct {
	Version           int               `yaml:"version"`
	Adapters          Adapters          `yaml:"adapters"`
	Worker            Worker            `yaml:"worker"`
	Verifier          Verifier          `yaml:"verifier"`
	CI                CI                `yaml:"ci"`
	Models            Models            `yaml:"models"`
	Verification      Verification      `yaml:"verification"`
	Resilience        Resilience        `yaml:"resilience"`
	Guardrails        Guardrails        `yaml:"guardrails"`
	Environment       Environment       `yaml:"environment"`
	Evidence          Evidence          `yaml:"evidence"`
	Host              Host              `yaml:"host,omitempty"`
	Domain            Domain            `yaml:"domain,omitempty"`
	MCP               MCP               `yaml:"mcp,omitempty"`
	ProviderInventory ProviderInventory `yaml:"provider_inventory,omitempty"`
	RoleDefinitions   []RoleDefinition  `yaml:"role_definitions,omitempty" json:"role_definitions,omitempty"`
	Audit             Audit             `yaml:"audit,omitempty"`
	Report            Report            `yaml:"report"`
}

type ShowBaseConfigFunc func(ctx context.Context, repoPath, baseBranch string) ([]byte, error)

type LoadOptions struct {
	BaseBranch     string
	ConfigFromBase bool
	ShowBaseConfig ShowBaseConfigFunc
	Warnings       io.Writer
}

type ConfigMismatchError struct {
	BaseBranch string
}

func (e ConfigMismatchError) Error() string {
	return ConfigMismatchMessage(e.BaseBranch)
}

func ConfigMismatchMessage(baseBranch string) string {
	baseBranch = normalizeBaseBranch(baseBranch)
	return fmt.Sprintf(".delivery.yml is absent from working tree but present on %s; probably the wrong branch; checkout the base or pass --config-from-base", baseBranch)
}

type Adapters struct {
	WorkItems string `yaml:"work_items"`
	Workspace string `yaml:"workspace"`
	Conductor string `yaml:"conductor"`
	Worker    string `yaml:"worker"`
	VCS       string `yaml:"vcs"`
	Verifier  string `yaml:"verifier"`
	Gate      string `yaml:"gate"`
}

// ReviewerNotWorkerWarning returns an advisory warning when reviewer and worker
// roles are configured to the same provider.
func ReviewerNotWorkerWarning(adapters Adapters) string {
	if adapters.Worker == "" || adapters.Verifier == "" || adapters.Worker != adapters.Verifier {
		return ""
	}
	return fmt.Sprintf("adapters.verifier %q matches adapters.worker; reviewer and worker SHOULD differ, but this is advisory only", adapters.Verifier)
}

type Worker struct {
	BaseBranch      string `yaml:"base_branch"`
	Model           string `yaml:"model"`
	ReasoningEffort string `yaml:"reasoning_effort"`
	CommandHint     string `yaml:"command_hint"`
}

type Verifier struct {
	Model           string `yaml:"model"`
	ReasoningEffort string `yaml:"reasoning_effort"`
}

type Models struct {
	Strict bool `yaml:"strict"`
}

type CI struct {
	Checks    []string `yaml:"checks"`
	Tests     []string `yaml:"tests"`
	Typecheck []string `yaml:"typecheck"`
	Build     []string `yaml:"build"`
}

type Verification struct {
	SpecRequired bool    `yaml:"spec_required"`
	MaxFixPasses int     `yaml:"max_fix_passes"`
	Browser      Browser `yaml:"browser"`
}

type Browser struct {
	Enabled string   `yaml:"enabled"`
	Globs   []string `yaml:"globs"`
}

type Resilience struct {
	Worker   ResilienceWorker   `yaml:"worker"`
	Verifier ResilienceVerifier `yaml:"verifier"`
}

type ResilienceWorker struct {
	HeartbeatIntervalSeconds int   `yaml:"heartbeat_interval_seconds"`
	StaleAfterSeconds        int   `yaml:"stale_after_seconds"`
	HungAfterSeconds         int   `yaml:"hung_after_seconds"`
	MaxAttempts              int   `yaml:"max_attempts"`
	RetryBackoffSeconds      []int `yaml:"retry_backoff_seconds"`
	// Process-watchdog caps for the worker provider CLI (spec 0390, Decision 7).
	// Absent/zero falls back to the built-in default.
	HardCapSeconds      int `yaml:"hard_cap_seconds"`
	StallTimeoutSeconds int `yaml:"stall_timeout_seconds"`
}

// ResilienceVerifier holds process-watchdog caps for the verifier provider CLI.
type ResilienceVerifier struct {
	HardCapSeconds      int `yaml:"hard_cap_seconds"`
	StallTimeoutSeconds int `yaml:"stall_timeout_seconds"`
}

type Report struct {
	Channel string `yaml:"channel"`
}

type Environment struct {
	PreProdBranch string `yaml:"pre_prod_branch"`
}

type Evidence struct {
	Website EvidenceArtifact `yaml:"website"`
	CLI     EvidenceArtifact `yaml:"cli"`
	Library EvidenceArtifact `yaml:"library"`
	App     EvidenceArtifact `yaml:"app"`
}

type EvidenceArtifact struct {
	ProjectType   string `yaml:"-" json:"project_type"`
	PreviewURL    string `yaml:"preview_url" json:"preview_url,omitempty"`
	ExampleOutput string `yaml:"example_output" json:"example_output,omitempty"`
	TestResults   string `yaml:"test_results" json:"test_results,omitempty"`
	PreviewBuild  string `yaml:"preview_build" json:"preview_build,omitempty"`
}

// Host selects the local agent host profile. It is separate from provider,
// model, and reasoning-depth selection.
type Host struct {
	Profile string `yaml:"profile,omitempty"`
}

// Domain is the optional 0.5.0 domain-profile schema. Runtime slices consume
// individual fields as they land; absent fields keep current behavior.
type Domain struct {
	Name         string             `yaml:"name,omitempty"`
	Description  string             `yaml:"description,omitempty"`
	Skills       DomainSkills       `yaml:"skills,omitempty"`
	Verification DomainVerification `yaml:"verification,omitempty"`
	Evidence     DomainEvidence     `yaml:"evidence,omitempty"`
	RedLines     []DomainRedLine    `yaml:"red_lines,omitempty"`
	PartialWork  DomainPartialWork  `yaml:"partial_work,omitempty"`
	Liveness     DomainLiveness     `yaml:"liveness,omitempty"`
}

type DomainSkills struct {
	Paths             []string             `yaml:"paths,omitempty"`
	MachineLibrary    DomainMachineLibrary `yaml:"machine_library,omitempty"`
	Select            []string             `yaml:"select,omitempty"`
	PromptBudgetBytes int                  `yaml:"prompt_budget_bytes,omitempty"`
}

type DomainMachineLibrary struct {
	Paths []string `yaml:"paths,omitempty"`
}

type DomainVerification struct {
	Rubric            DomainRubric `yaml:"rubric,omitempty"`
	ReviewPacketOrder []string     `yaml:"review_packet_order,omitempty"`
}

type DomainRubric struct {
	Paths     []string `yaml:"paths,omitempty"`
	Checklist []string `yaml:"checklist,omitempty"`
}

type DomainEvidence struct {
	Producer DomainEvidenceProducer `yaml:"producer,omitempty"`
}

type DomainEvidenceProducer struct {
	Command             string   `yaml:"command,omitempty"`
	Argv                []string `yaml:"argv,omitempty"`
	Outputs             []string `yaml:"outputs,omitempty"`
	TimeoutSeconds      int      `yaml:"timeout_seconds,omitempty"`
	IncludeInLoopreview *bool    `yaml:"include_in_loopreview,omitempty"`
}

type DomainRedLine struct {
	Category  string   `yaml:"category,omitempty"`
	Detail    string   `yaml:"detail,omitempty"`
	PathGlobs []string `yaml:"path_globs,omitempty"`
}

type DomainPartialWork struct {
	Mode string `yaml:"mode,omitempty"`
}

type DomainLiveness struct {
	Mode    string   `yaml:"mode,omitempty"`
	Command string   `yaml:"command,omitempty"`
	Argv    []string `yaml:"argv,omitempty"`
}

// MCP is the optional 0.5.0 MCP server schema. Provider wiring lands in later
// slices; this config package only preserves the parsed declaration.
type MCP struct {
	Servers []MCPServer `yaml:"servers,omitempty"`
}

type MCPServer struct {
	Name      string   `yaml:"name,omitempty"`
	Transport string   `yaml:"transport,omitempty"`
	Command   string   `yaml:"command,omitempty"`
	Args      []string `yaml:"args,omitempty"`
	URL       string   `yaml:"url,omitempty"`
	Auth      MCPAuth  `yaml:"auth,omitempty"`
	Roles     []string `yaml:"roles,omitempty"`
	ReadOnly  bool     `yaml:"read_only,omitempty"`
}

type MCPAuth struct {
	Header string `yaml:"header,omitempty"`
	Env    string `yaml:"env,omitempty"`
}

type ProviderInventory struct {
	Executables      map[string][]string `yaml:"executables,omitempty"`
	ProfileSelection map[string]string   `yaml:"profile_selection,omitempty"`
}

type RoleDefinition struct {
	SchemaVersion              string             `yaml:"schema_version,omitempty" json:"schema_version,omitempty"`
	RecordVersion              int                `yaml:"record_version,omitempty" json:"record_version,omitempty"`
	RoleDefinitionID           string             `yaml:"role_definition_id,omitempty" json:"role_definition_id,omitempty"`
	RoleKey                    string             `yaml:"role_key" json:"role_key"`
	RoleVersion                string             `yaml:"role_version,omitempty" json:"role_version,omitempty"`
	Description                string             `yaml:"description,omitempty" json:"description,omitempty"`
	AllowedRiskTiers           []string           `yaml:"allowed_risk_tiers,omitempty" json:"allowed_risk_tiers,omitempty"`
	MinimumCapabilities        []RoleCapability   `yaml:"minimum_capabilities,omitempty" json:"minimum_capabilities,omitempty"`
	PermissionFloor            string             `yaml:"permission_floor,omitempty" json:"permission_floor,omitempty"`
	PermissionCeiling          string             `yaml:"permission_ceiling,omitempty" json:"permission_ceiling,omitempty"`
	DefaultOutputContract      string             `yaml:"default_output_contract,omitempty" json:"default_output_contract,omitempty"`
	IndependenceRequirements   map[string]string  `yaml:"independence_requirements,omitempty" json:"independence_requirements,omitempty"`
	ForbiddenBindings          []string           `yaml:"forbidden_bindings,omitempty" json:"forbidden_bindings,omitempty"`
	QualityFloor               string             `yaml:"quality_floor,omitempty" json:"quality_floor,omitempty"`
	ReasoningDepth             string             `yaml:"reasoning_depth,omitempty" json:"reasoning_depth,omitempty"`
	RequiredTools              []string           `yaml:"required_tools,omitempty" json:"required_tools,omitempty"`
	MinimumContextWindowTokens int                `yaml:"minimum_context_window_tokens,omitempty" json:"minimum_context_window_tokens,omitempty"`
	MaxSideEffectClass         string             `yaml:"max_side_effect_class,omitempty" json:"max_side_effect_class,omitempty"`
	VerificationRequirements   []RoleVerification `yaml:"verification_requirements,omitempty" json:"verification_requirements,omitempty"`
	LatencyTolerance           string             `yaml:"latency_tolerance,omitempty" json:"latency_tolerance,omitempty"`
	CostTolerance              string             `yaml:"cost_tolerance,omitempty" json:"cost_tolerance,omitempty"`
	PolicyVersion              string             `yaml:"policy_version,omitempty" json:"policy_version,omitempty"`
}

type RoleCapability struct {
	Dimension         string `yaml:"dimension" json:"dimension"`
	RequiredValue     any    `yaml:"required_value" json:"required_value"`
	MinimumConfidence string `yaml:"minimum_confidence,omitempty" json:"minimum_confidence,omitempty"`
	FreshnessRequired string `yaml:"freshness_required,omitempty" json:"freshness_required,omitempty"`
	Source            string `yaml:"source,omitempty" json:"source,omitempty"`
}

type RoleVerification struct {
	VerificationKind     string   `yaml:"verification_kind" json:"verification_kind"`
	RequiredForRiskTiers []string `yaml:"required_for_risk_tiers,omitempty" json:"required_for_risk_tiers,omitempty"`
	IndependenceLevel    string   `yaml:"independence_level,omitempty" json:"independence_level,omitempty"`
	PermissionRequired   string   `yaml:"permission_required,omitempty" json:"permission_required,omitempty"`
	OutputContract       string   `yaml:"output_contract,omitempty" json:"output_contract,omitempty"`
	Source               string   `yaml:"source,omitempty" json:"source,omitempty"`
}

// Audit is the optional 0.5.3 audit command configuration surface. It is
// additive: absent fields preserve built-in audit defaults.
type Audit struct {
	SeverityThreshold string        `yaml:"severity_threshold,omitempty"`
	SAST              AuditSAST     `yaml:"sast,omitempty"`
	Review            AuditReview   `yaml:"review,omitempty"`
	Baseline          AuditBaseline `yaml:"baseline,omitempty"`
}

type AuditSAST struct {
	Commands []AuditSASTCommand `yaml:"commands,omitempty"`
	Native   AuditSASTNative    `yaml:"native,omitempty"`
}

type AuditSASTCommand struct {
	ID             string   `yaml:"id,omitempty"`
	Argv           []string `yaml:"argv,omitempty"`
	Parser         string   `yaml:"parser,omitempty"`
	TimeoutSeconds int      `yaml:"timeout_seconds,omitempty"`
}

type AuditSASTNative struct {
	Secrets         *bool    `yaml:"secrets,omitempty"`
	FilePermissions *bool    `yaml:"file_permissions,omitempty"`
	Include         []string `yaml:"include,omitempty"`
	Exclude         []string `yaml:"exclude,omitempty"`
}

type AuditReview struct {
	RubricPath string `yaml:"rubric_path,omitempty"`
}

type AuditBaseline struct {
	Path string `yaml:"path,omitempty"`
}

type MCPReadOnlyPolicy int

const (
	MCPReadOnlyReject MCPReadOnlyPolicy = iota
	MCPReadOnlyFilter
)

type MCPInvocationOptions struct {
	Role                     string
	ReadOnly                 bool
	ReadOnlyPolicy           MCPReadOnlyPolicy
	RequireRole              bool
	ErrorPrefix              string
	InvocationRoleError      string
	InvocationRoleErrorValue string
}

func (e Evidence) Artifacts() []EvidenceArtifact {
	artifacts := make([]EvidenceArtifact, 0, 4)
	for _, candidate := range []EvidenceArtifact{
		e.artifact("website", e.Website),
		e.artifact("cli", e.CLI),
		e.artifact("library", e.Library),
		e.artifact("app", e.App),
	} {
		if !candidate.empty() {
			artifacts = append(artifacts, candidate)
		}
	}
	return artifacts
}

func (e Evidence) artifact(projectType string, artifact EvidenceArtifact) EvidenceArtifact {
	artifact.ProjectType = projectType
	artifact.PreviewURL = strings.TrimSpace(artifact.PreviewURL)
	artifact.ExampleOutput = strings.TrimSpace(artifact.ExampleOutput)
	artifact.TestResults = strings.TrimSpace(artifact.TestResults)
	artifact.PreviewBuild = strings.TrimSpace(artifact.PreviewBuild)
	return artifact
}

func (a EvidenceArtifact) empty() bool {
	return strings.TrimSpace(a.PreviewURL) == "" &&
		strings.TrimSpace(a.ExampleOutput) == "" &&
		strings.TrimSpace(a.TestResults) == "" &&
		strings.TrimSpace(a.PreviewBuild) == ""
}

type Guardrails struct {
	Budget         GuardrailBudget         `yaml:"budget"`
	CircuitBreaker GuardrailCircuitBreaker `yaml:"circuit_breaker"`
}

type GuardrailBudget struct {
	MaxRuns          *int     `yaml:"max_runs"`
	MaxTotalAttempts *int     `yaml:"max_total_attempts"`
	MaxTotalTokens   *int64   `yaml:"max_total_tokens"`
	MaxTotalCostUSD  *float64 `yaml:"max_total_cost_usd"`
}

func (b GuardrailBudget) Enabled() bool {
	return b.MaxRuns != nil ||
		b.MaxTotalAttempts != nil ||
		b.MaxTotalTokens != nil ||
		b.MaxTotalCostUSD != nil
}

type GuardrailCircuitBreaker struct {
	MaxNoProgressWaves    *int `yaml:"max_no_progress_waves"`
	MaxNoProgressAttempts *int `yaml:"max_no_progress_attempts"`
}

func (c GuardrailCircuitBreaker) Enabled() bool {
	return c.MaxNoProgressWaves != nil ||
		c.MaxNoProgressAttempts != nil
}

// Default returns the documented defaults for optional .delivery.yml sections.
func Default() Config {
	return Config{
		Verification: Verification{
			SpecRequired: true,
			MaxFixPasses: lcdefaults.VerificationMaxFixPasses,
			Browser: Browser{
				Enabled: lcdefaults.VerificationBrowserMode,
			},
		},
		Resilience: Resilience{
			Worker: ResilienceWorker{
				HeartbeatIntervalSeconds: lcdefaults.WorkerHeartbeatIntervalSeconds,
				StaleAfterSeconds:        lcdefaults.WorkerStaleAfterSeconds,
				HungAfterSeconds:         lcdefaults.WorkerHungAfterSeconds,
				MaxAttempts:              lcdefaults.WorkerMaxAttempts,
				RetryBackoffSeconds:      lcdefaults.WorkerRetryBackoffSeconds(),
				HardCapSeconds:           lcdefaults.WorkerHardCapSeconds,
				StallTimeoutSeconds:      lcdefaults.WorkerStallTimeoutSeconds,
			},
			Verifier: ResilienceVerifier{
				HardCapSeconds:      lcdefaults.VerifierHardCapSeconds,
				StallTimeoutSeconds: lcdefaults.VerifierStallTimeoutSeconds,
			},
		},
		Environment: Environment{
			PreProdBranch: lcdefaults.PreProdBranch,
		},
	}
}

// Load reads and parses a .delivery.yml file from disk.
func Load(path string) (Config, error) {
	cfg, _, err := LoadWithDiagnostics(path)
	return cfg, err
}

func LoadWithDiagnostics(path string) (Config, []migration.Diagnostic, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, nil, fmt.Errorf("read delivery config: %w", err)
	}
	return ParseWithDiagnostics(data)
}

// Parse reads .delivery.yml data, tolerating absent optional sections.
func Parse(data []byte) (Config, error) {
	cfg, _, err := ParseWithDiagnostics(data)
	return cfg, err
}

func validateParsedConfig(cfg Config) error {
	if err := validateAdapterProviderNames(cfg.Adapters); err != nil {
		return err
	}
	if err := validateGuardrailBudget(cfg.Guardrails.Budget); err != nil {
		return err
	}
	if err := validateGuardrailCircuitBreaker(cfg.Guardrails.CircuitBreaker); err != nil {
		return err
	}
	if err := validateMCP(cfg.MCP); err != nil {
		return err
	}
	if err := validateProviderInventory(cfg.ProviderInventory); err != nil {
		return err
	}
	if err := validateRoleDefinitions(cfg.RoleDefinitions); err != nil {
		return err
	}
	if err := validateDomainCommands(cfg.Domain); err != nil {
		return err
	}
	if err := validateAudit(cfg.Audit); err != nil {
		return err
	}
	if err := validateHost(cfg.Host); err != nil {
		return err
	}
	return nil
}

func validateAdapterProviderNames(adapters Adapters) error {
	for _, candidate := range []struct {
		path  string
		value string
	}{
		{path: "adapters.worker", value: adapters.Worker},
		{path: "adapters.verifier", value: adapters.Verifier},
	} {
		value := strings.TrimSpace(candidate.value)
		if value == "" {
			continue
		}
		if !validMCPServerName(value) {
			return fmt.Errorf("invalid delivery config: %s %q is not a safe provider adapter name", candidate.path, candidate.value)
		}
	}
	return nil
}

func LoadForRepo(ctx context.Context, repoPath string, opts LoadOptions) (Config, error) {
	cfg := Default()
	loaded, diagnostics, err := LoadWithDiagnostics(filepath.Join(repoPath, ".delivery.yml"))
	if err == nil {
		warnMigrationDiagnostics(opts.Warnings, diagnostics)
		return loaded, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return cfg, err
	}
	baseBranch := normalizeBaseBranch(opts.BaseBranch)
	baseConfig, ok, checkErr := showBaseConfig(ctx, repoPath, baseBranch, opts.ShowBaseConfig)
	if checkErr != nil {
		warnBaseConfigCheckFailed(opts.Warnings, baseBranch, checkErr)
		return cfg, nil
	}
	if !ok {
		return cfg, nil
	}
	if opts.ConfigFromBase {
		loaded, diagnostics, err := ParseWithDiagnostics(baseConfig)
		if err != nil {
			return cfg, err
		}
		warnMigrationDiagnostics(opts.Warnings, diagnostics)
		return loaded, nil
	}
	return cfg, ConfigMismatchError{BaseBranch: baseBranch}
}

// ResilienceForRepo loads the resilience config from repoPath/.delivery.yml,
// falling back to built-in defaults when the file is genuinely absent.
func ResilienceForRepo(ctx context.Context, repoPath string, opts LoadOptions) (Resilience, error) {
	cfg, err := LoadForRepo(ctx, repoPath, opts)
	if err != nil {
		return Resilience{}, err
	}
	return cfg.Resilience, nil
}

// DurationSeconds converts a positive seconds value to a Duration, falling back
// to def when seconds is zero or negative.
func DurationSeconds(seconds int, def time.Duration) time.Duration {
	if seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return def
}

func validateGuardrailBudget(b GuardrailBudget) error {
	if b.MaxRuns != nil && *b.MaxRuns <= 0 {
		return fmt.Errorf("invalid delivery config: guardrails.budget.max_runs must be greater than zero")
	}
	if b.MaxTotalAttempts != nil && *b.MaxTotalAttempts <= 0 {
		return fmt.Errorf("invalid delivery config: guardrails.budget.max_total_attempts must be greater than zero")
	}
	if b.MaxTotalTokens != nil && *b.MaxTotalTokens <= 0 {
		return fmt.Errorf("invalid delivery config: guardrails.budget.max_total_tokens must be greater than zero")
	}
	if b.MaxTotalCostUSD != nil && *b.MaxTotalCostUSD <= 0 {
		return fmt.Errorf("invalid delivery config: guardrails.budget.max_total_cost_usd must be greater than zero")
	}
	return nil
}

func validateGuardrailCircuitBreaker(c GuardrailCircuitBreaker) error {
	if c.MaxNoProgressWaves != nil && *c.MaxNoProgressWaves <= 0 {
		return fmt.Errorf("invalid delivery config: guardrails.circuit_breaker.max_no_progress_waves must be greater than zero")
	}
	if c.MaxNoProgressAttempts != nil && *c.MaxNoProgressAttempts <= 0 {
		return fmt.Errorf("invalid delivery config: guardrails.circuit_breaker.max_no_progress_attempts must be greater than zero")
	}
	return nil
}

func validateMCP(m MCP) error {
	return validateMCPDeclarations(m, "invalid delivery config: ")
}

func validateProviderInventory(inventory ProviderInventory) error {
	for provider, paths := range inventory.Executables {
		if !validMCPServerName(provider) {
			return fmt.Errorf("invalid delivery config: provider_inventory.executables contains unsafe provider key %q", provider)
		}
		for index, path := range paths {
			if strings.TrimSpace(path) == "" {
				return fmt.Errorf("invalid delivery config: provider_inventory.executables.%s[%d] must not be empty", provider, index)
			}
		}
	}
	for provider, accountProfileID := range inventory.ProfileSelection {
		if !validMCPServerName(provider) {
			return fmt.Errorf("invalid delivery config: provider_inventory.profile_selection contains unsafe provider key %q", provider)
		}
		accountProfileID = strings.TrimSpace(accountProfileID)
		if !strings.HasPrefix(accountProfileID, "acct_") || len(accountProfileID) < len("acct_")+8 {
			return fmt.Errorf("invalid delivery config: provider_inventory.profile_selection.%s must be an opaque acct_ account profile id", provider)
		}
	}
	return nil
}

func validateRoleDefinitions(roles []RoleDefinition) error {
	seen := map[string]bool{}
	for index, role := range roles {
		key := strings.ToLower(strings.TrimSpace(role.RoleKey))
		if key == "" {
			return fmt.Errorf("invalid delivery config: role_definitions[%d].role_key must not be empty", index)
		}
		if role.SchemaVersion != roleDefinitionSchema {
			return fmt.Errorf("invalid delivery config: role_definitions[%d].schema_version %q is not supported", index, role.SchemaVersion)
		}
		if role.RecordVersion != 1 {
			return fmt.Errorf("invalid delivery config: role_definitions[%d].record_version %d is not supported", index, role.RecordVersion)
		}
		if !validMCPServerName(key) {
			return fmt.Errorf("invalid delivery config: role_definitions[%d].role_key %q is not a safe role key", index, role.RoleKey)
		}
		if seen[key] {
			return fmt.Errorf("invalid delivery config: role_definitions[%d].role_key %q is duplicated", index, role.RoleKey)
		}
		seen[key] = true
		if strings.TrimSpace(role.RoleVersion) == "" {
			return fmt.Errorf("invalid delivery config: role_definitions[%d].role_version must not be empty", index)
		}
		if strings.TrimSpace(role.Description) == "" {
			return fmt.Errorf("invalid delivery config: role_definitions[%d].description must not be empty", index)
		}
		policyVersion := strings.TrimSpace(role.PolicyVersion)
		if policyVersion == "" {
			return fmt.Errorf("invalid delivery config: role_definitions[%d].policy_version must not be empty", index)
		}
		if role.RoleDefinitionID != "" && role.RoleDefinitionID != configRoleDefinitionID(key, role.RoleVersion, policyVersion) {
			return fmt.Errorf("invalid delivery config: role_definitions[%d].role_definition_id does not match role_key, role_version, and policy_version", index)
		}
		if len(role.AllowedRiskTiers) == 0 {
			return fmt.Errorf("invalid delivery config: role_definitions[%d].allowed_risk_tiers must not be empty", index)
		}
		for riskIndex, risk := range role.AllowedRiskTiers {
			if !validConfigRiskTier(risk) {
				return fmt.Errorf("invalid delivery config: role_definitions[%d].allowed_risk_tiers[%d] %q is unknown", index, riskIndex, risk)
			}
		}
		if len(role.MinimumCapabilities) == 0 {
			return fmt.Errorf("invalid delivery config: role_definitions[%d].minimum_capabilities must not be empty", index)
		}
		for capIndex, capability := range role.MinimumCapabilities {
			if err := validateConfigRoleCapability(capability); err != nil {
				return fmt.Errorf("invalid delivery config: role_definitions[%d].minimum_capabilities[%d]: %w", index, capIndex, err)
			}
		}
		if !validConfigPermission(role.PermissionFloor) || !validConfigPermission(role.PermissionCeiling) || configPermissionRank(role.PermissionFloor) > configPermissionRank(role.PermissionCeiling) {
			return fmt.Errorf("invalid delivery config: role_definitions[%d].permission_floor and permission_ceiling are invalid", index)
		}
		if !validConfigOutput(role.DefaultOutputContract) {
			return fmt.Errorf("invalid delivery config: role_definitions[%d].default_output_contract %q is unknown", index, role.DefaultOutputContract)
		}
		for risk, independence := range role.IndependenceRequirements {
			if !validConfigRiskTier(risk) || !validConfigIndependence(independence) {
				return fmt.Errorf("invalid delivery config: role_definitions[%d].independence_requirements contains unknown field", index)
			}
		}
		if !validConfigQuality(role.QualityFloor) || !validConfigReasoningDepth(role.ReasoningDepth) || !validConfigSideEffect(role.MaxSideEffectClass) || !validConfigLatency(role.LatencyTolerance) || !validConfigCost(role.CostTolerance) {
			return fmt.Errorf("invalid delivery config: role_definitions[%d] contains an unknown envelope enum", index)
		}
		if role.MinimumContextWindowTokens < 0 {
			return fmt.Errorf("invalid delivery config: role_definitions[%d].minimum_context_window_tokens must be non-negative", index)
		}
		for verificationIndex, verification := range role.VerificationRequirements {
			if err := validateConfigRoleVerification(verification); err != nil {
				return fmt.Errorf("invalid delivery config: role_definitions[%d].verification_requirements[%d]: %w", index, verificationIndex, err)
			}
		}
	}
	return nil
}

func validateConfigRoleCapability(capability RoleCapability) error {
	dimension := strings.TrimSpace(capability.Dimension)
	if !validConfigCapabilityDimension(dimension) {
		return fmt.Errorf("dimension %q is unknown", capability.Dimension)
	}
	if capability.RequiredValue == nil {
		return fmt.Errorf("required_value must be set")
	}
	if !validConfigHardConfidence(capability.MinimumConfidence) {
		return fmt.Errorf("minimum_confidence %q is not a supported hard evidence floor", capability.MinimumConfidence)
	}
	if strings.TrimSpace(capability.FreshnessRequired) != "fresh" {
		return fmt.Errorf("freshness_required must be fresh")
	}
	switch dimension {
	case "roles_supported", "tool_support":
		if values, ok := configStringList(capability.RequiredValue); !ok || len(values) == 0 {
			return fmt.Errorf("%s required_value must be a non-empty string or string list", dimension)
		}
	case "context_window_tokens":
		if value, ok := configIntValue(capability.RequiredValue); !ok || value < 0 {
			return fmt.Errorf("context_window_tokens required_value must be a non-negative integer")
		}
	default:
		value, ok := capability.RequiredValue.(bool)
		if !ok || !value {
			return fmt.Errorf("%s required_value must be true", dimension)
		}
	}
	return nil
}

func validateConfigRoleVerification(verification RoleVerification) error {
	if !validConfigVerificationKind(verification.VerificationKind) {
		return fmt.Errorf("verification_kind %q is unknown", verification.VerificationKind)
	}
	for _, risk := range verification.RequiredForRiskTiers {
		if !validConfigRiskTier(risk) {
			return fmt.Errorf("required_for_risk_tiers contains unknown risk %q", risk)
		}
	}
	if !validConfigIndependence(verification.IndependenceLevel) || !validConfigPermission(verification.PermissionRequired) || !validConfigOutput(verification.OutputContract) {
		return fmt.Errorf("verification requirement contains an unknown enum")
	}
	return nil
}

func validConfigRiskTier(value string) bool {
	switch strings.TrimSpace(value) {
	case "low", "medium", "high", "critical":
		return true
	default:
		return false
	}
}

func validConfigPermission(value string) bool {
	return configPermissionRank(value) > 0
}

func configPermissionRank(value string) int {
	switch strings.TrimSpace(value) {
	case "read-only":
		return 1
	case "write":
		return 2
	case "orchestrate":
		return 3
	default:
		return 0
	}
}

func validConfigOutput(value string) bool {
	switch strings.TrimSpace(value) {
	case "freeform", "markdown", "json", "json-schema", "patch", "branch", "pr", "report", "verification-verdict":
		return true
	default:
		return false
	}
}

func validConfigIndependence(value string) bool {
	switch strings.TrimSpace(value) {
	case "none", "different-model", "different-account", "different-provider", "human":
		return true
	default:
		return false
	}
}

func validConfigQuality(value string) bool {
	switch strings.TrimSpace(value) {
	case "standard", "strong", "adversarial":
		return true
	default:
		return false
	}
}

func validConfigReasoningDepth(value string) bool {
	switch strings.TrimSpace(value) {
	case "standard", "deep", "adversarial":
		return true
	default:
		return false
	}
}

func validConfigSideEffect(value string) bool {
	switch strings.TrimSpace(value) {
	case "none", "local-read", "local-write", "repo-write", "provider-launch", "git-remote-write", "github-write", "external-write":
		return true
	default:
		return false
	}
}

func validConfigLatency(value string) bool {
	switch strings.TrimSpace(value) {
	case "low", "standard", "relaxed":
		return true
	default:
		return false
	}
}

func validConfigCost(value string) bool {
	switch strings.TrimSpace(value) {
	case "low", "standard", "high":
		return true
	default:
		return false
	}
}

func validConfigVerificationKind(value string) bool {
	switch strings.TrimSpace(value) {
	case "none", "self-check", "local-command", "hosted-check", "loopreview", "security-review", "human-approval", "override":
		return true
	default:
		return false
	}
}

func validConfigCapabilityDimension(value string) bool {
	switch strings.TrimSpace(value) {
	case "roles_supported", "read_only", "json_output", "nested_subagents", "mcp_config", "cancellation", "token_usage_reporting", "context_window_tokens", "tool_support", "image_input", "image_output":
		return true
	default:
		return false
	}
}

func validConfigHardConfidence(value string) bool {
	switch strings.TrimSpace(value) {
	case "exact", "estimated":
		return true
	default:
		return false
	}
}

func configStringList(value any) ([]string, bool) {
	switch v := value.(type) {
	case string:
		text := strings.TrimSpace(v)
		if text == "" {
			return nil, false
		}
		return []string{text}, true
	case []string:
		out := make([]string, 0, len(v))
		for _, item := range v {
			item = strings.TrimSpace(item)
			if item == "" {
				return nil, false
			}
			out = append(out, item)
		}
		return out, len(out) > 0
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			text, ok := item.(string)
			if !ok || strings.TrimSpace(text) == "" {
				return nil, false
			}
			out = append(out, strings.TrimSpace(text))
		}
		return out, len(out) > 0
	default:
		return nil, false
	}
}

func configIntValue(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		if v > int64(^uint(0)>>1) {
			return int(^uint(0) >> 1), true
		}
		return int(v), true
	case float64:
		if v != float64(int(v)) {
			return 0, false
		}
		return int(v), true
	default:
		return 0, false
	}
}

func configRoleDefinitionID(roleKey, roleVersion, policyVersion string) string {
	return "roledef_" + configRoleDigestBase32(strings.ToLower(strings.TrimSpace(roleKey)), strings.TrimSpace(roleVersion), strings.TrimSpace(policyVersion))
}

func configRoleDigestBase32(parts ...string) string {
	sum := sha256.New()
	for _, part := range parts {
		sum.Write([]byte(part))
		sum.Write([]byte{0})
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum.Sum(nil)))
}

func validateMCPDeclarations(m MCP, errorPrefix string) error {
	for index, server := range m.Servers {
		if err := validateMCPServerRoles(index, server.Roles, errorPrefix); err != nil {
			return err
		}
		if !mcpServerCanReachProvider(server) {
			continue
		}
		if err := validateMCPServer(index, server, errorPrefix); err != nil {
			return err
		}
	}
	return nil
}

func MCPServersForInvocation(m MCP, opts MCPInvocationOptions) ([]MCPServer, error) {
	role := normalizeMCPRole(opts.Role)
	if opts.RequireRole || role != "" {
		if !validMCPRole(role) {
			label := opts.InvocationRoleError
			if label == "" {
				label = "invalid MCP invocation role"
			}
			value := role
			if opts.InvocationRoleErrorValue != "" {
				value = opts.InvocationRoleErrorValue
			}
			return nil, fmt.Errorf("%s %q", label, value)
		}
	}
	if len(m.Servers) == 0 {
		return nil, nil
	}

	requiresRole := false
	for index, server := range m.Servers {
		if err := validateMCPServerRoles(index, server.Roles, opts.ErrorPrefix); err != nil {
			return nil, err
		}
		if len(server.Roles) > 0 {
			requiresRole = true
		}
	}
	if requiresRole && role == "" {
		return nil, errors.New("invocation role is required when MCP servers declare role filters")
	}

	servers := make([]MCPServer, 0, len(m.Servers))
	for index, server := range m.Servers {
		if !mcpRoleAllowed(server.Roles, role) {
			continue
		}
		if opts.ReadOnly && !server.ReadOnly && opts.ReadOnlyPolicy == MCPReadOnlyFilter {
			continue
		}
		if err := validateMCPServer(index, server, opts.ErrorPrefix); err != nil {
			return nil, err
		}
		if opts.ReadOnly && !server.ReadOnly {
			return nil, fmt.Errorf("%smcp.servers[%d] %q is not locally classified read-only for a read-only invocation", opts.ErrorPrefix, index, server.Name)
		}
		servers = append(servers, copyMCPServer(server))
	}
	if len(servers) == 0 {
		return nil, nil
	}
	return servers, nil
}

func validateMCPServerRoles(index int, roles []string, errorPrefix string) error {
	for _, role := range roles {
		normalized := normalizeMCPRole(role)
		if !validMCPRole(normalized) {
			return fmt.Errorf("%smcp.servers[%d].roles contains unknown role %q", errorPrefix, index, role)
		}
	}
	return nil
}

func validateMCPServer(index int, server MCPServer, errorPrefix string) error {
	if !validMCPServerName(server.Name) {
		return fmt.Errorf("%smcp.servers[%d].name %q is not a safe provider MCP server name", errorPrefix, index, server.Name)
	}

	transport, err := MCPServerTransport(server)
	if err != nil {
		return fmt.Errorf("%smcp.servers[%d] %q: %w", errorPrefix, index, server.Name, err)
	}
	switch transport {
	case "stdio":
		if strings.TrimSpace(server.Command) == "" {
			return fmt.Errorf("%smcp.servers[%d] %q stdio transport requires command", errorPrefix, index, server.Name)
		}
		if strings.TrimSpace(server.URL) != "" {
			return fmt.Errorf("%smcp.servers[%d] %q stdio transport cannot include url", errorPrefix, index, server.Name)
		}
		if MCPAuthConfigured(server.Auth) {
			return fmt.Errorf("%smcp.servers[%d] %q stdio transport cannot use HTTP auth", errorPrefix, index, server.Name)
		}
	case "http":
		if strings.TrimSpace(server.URL) == "" {
			return fmt.Errorf("%smcp.servers[%d] %q http transport requires url", errorPrefix, index, server.Name)
		}
		if strings.TrimSpace(server.Command) != "" || len(server.Args) > 0 {
			return fmt.Errorf("%smcp.servers[%d] %q http transport cannot include command or args", errorPrefix, index, server.Name)
		}
		if !validMCPHTTPURL(server.URL) {
			return fmt.Errorf("%smcp.servers[%d] %q http transport requires an http or https url", errorPrefix, index, server.Name)
		}
		if err := validateMCPAuth(index, server, errorPrefix); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%smcp.servers[%d] %q has unsupported transport %q", errorPrefix, index, server.Name, server.Transport)
	}
	return nil
}

func validateMCPAuth(index int, server MCPServer, errorPrefix string) error {
	header := strings.TrimSpace(server.Auth.Header)
	envName := strings.TrimSpace(server.Auth.Env)
	if header == "" && envName == "" {
		return nil
	}
	if header == "" || envName == "" {
		return fmt.Errorf("%smcp.servers[%d] %q http auth requires both header and env", errorPrefix, index, server.Name)
	}
	if !validHTTPHeaderName(header) {
		return fmt.Errorf("%smcp.servers[%d] %q http auth header %q is invalid", errorPrefix, index, server.Name, header)
	}
	if !validEnvName(envName) {
		return fmt.Errorf("%smcp.servers[%d] %q http auth env %q is invalid", errorPrefix, index, server.Name, envName)
	}
	return nil
}

func MCPServerTransport(server MCPServer) (string, error) {
	transport := strings.ToLower(strings.TrimSpace(server.Transport))
	if transport == "" {
		switch {
		case strings.TrimSpace(server.URL) != "":
			return "http", nil
		case strings.TrimSpace(server.Command) != "":
			return "stdio", nil
		default:
			return "", errors.New("transport is required")
		}
	}
	switch transport {
	case "stdio", "http":
		return transport, nil
	default:
		return "", fmt.Errorf("unsupported transport %q", server.Transport)
	}
}

func normalizeMCPRole(role string) string {
	return strings.ToLower(strings.TrimSpace(role))
}

func validMCPRole(role string) bool {
	switch role {
	case "worker", "verifier":
		return true
	default:
		return false
	}
}

func mcpRoleAllowed(roles []string, role string) bool {
	if len(roles) == 0 {
		return true
	}
	for _, candidate := range roles {
		if normalizeMCPRole(candidate) == role {
			return true
		}
	}
	return false
}

func mcpServerCanReachProvider(server MCPServer) bool {
	if len(server.Roles) == 0 {
		return true
	}
	for _, role := range server.Roles {
		switch normalizeMCPRole(role) {
		case "worker":
			return true
		case "verifier":
			if server.ReadOnly {
				return true
			}
		}
	}
	return false
}

func validMCPServerName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

func validMCPHTTPURL(rawURL string) bool {
	rawURL = strings.TrimSpace(rawURL)
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return false
	}
	return parsed.Scheme == "https" || parsed.Scheme == "http"
}

func MCPAuthConfigured(auth MCPAuth) bool {
	return strings.TrimSpace(auth.Header) != "" || strings.TrimSpace(auth.Env) != ""
}

func validHTTPHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", r):
		default:
			return false
		}
	}
	return true
}

func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for index, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r == '_':
		case index > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

func copyMCPServer(server MCPServer) MCPServer {
	server.Args = append([]string(nil), server.Args...)
	server.Roles = append([]string(nil), server.Roles...)
	return server
}

func validateDomainCommands(domain Domain) error {
	producerConfigured, err := validateCommandSpec("domain.evidence.producer", domain.Evidence.Producer.Command, domain.Evidence.Producer.Argv)
	if err != nil {
		return err
	}
	if producerConfigured && len(normalizeNonEmptyStrings(domain.Evidence.Producer.Outputs)) == 0 {
		return errors.New("invalid delivery config: domain.evidence.producer.outputs is required when domain.evidence.producer.command or domain.evidence.producer.argv is configured")
	}

	livenessConfigured, err := validateCommandSpec("domain.liveness", domain.Liveness.Command, domain.Liveness.Argv)
	if err != nil {
		return err
	}
	switch mode := strings.ToLower(strings.TrimSpace(domain.Liveness.Mode)); mode {
	case "":
		if livenessConfigured {
			return errors.New("invalid delivery config: domain.liveness.mode must be \"custom\" when domain.liveness.command or domain.liveness.argv is configured")
		}
	case "worktree-mtime", "log-only":
		if livenessConfigured {
			return fmt.Errorf("invalid delivery config: domain.liveness.command and domain.liveness.argv are only valid when domain.liveness.mode is %q", "custom")
		}
	case "custom":
		if !livenessConfigured {
			return errors.New("invalid delivery config: domain.liveness.mode custom requires exactly one of domain.liveness.command or domain.liveness.argv")
		}
	default:
		return nil
	}
	return nil
}

func validateAudit(a Audit) error {
	threshold := strings.ToLower(strings.TrimSpace(a.SeverityThreshold))
	if threshold != "" && !validAuditSeverity(threshold) {
		return fmt.Errorf("invalid delivery config: audit.severity_threshold %q is not one of critical, high, medium, low, info", a.SeverityThreshold)
	}
	for index, command := range a.SAST.Commands {
		path := fmt.Sprintf("audit.sast.commands[%d]", index)
		if strings.TrimSpace(command.ID) == "" {
			return fmt.Errorf("invalid delivery config: %s.id is required", path)
		}
		if len(command.Argv) == 0 {
			return fmt.Errorf("invalid delivery config: %s.argv must be a non-empty array of non-empty strings", path)
		}
		for argIndex, arg := range command.Argv {
			if strings.TrimSpace(arg) == "" {
				return fmt.Errorf("invalid delivery config: %s.argv[%d] must not be empty", path, argIndex)
			}
		}
		if strings.TrimSpace(command.Parser) == "" {
			return fmt.Errorf("invalid delivery config: %s.parser is required", path)
		}
		if command.TimeoutSeconds < 0 {
			return fmt.Errorf("invalid delivery config: %s.timeout_seconds must not be negative", path)
		}
	}
	return nil
}

func validateHost(host Host) error {
	profile := strings.TrimSpace(host.Profile)
	if profile == "" {
		return nil
	}
	if _, ok := hostprofile.NormalizeName(profile); ok {
		return nil
	}
	return fmt.Errorf("invalid delivery config: host.profile %q is not one of %s", host.Profile, strings.Join(hostprofile.KnownNames(), ", "))
}

func validAuditSeverity(severity string) bool {
	switch severity {
	case "critical", "high", "medium", "low", "info":
		return true
	default:
		return false
	}
}

func validateCommandSpec(path, command string, argv []string) (bool, error) {
	hasCommand := strings.TrimSpace(command) != ""
	hasArgv := argv != nil
	if hasCommand && hasArgv {
		return false, fmt.Errorf("invalid delivery config: %s.command and %s.argv are mutually exclusive", path, path)
	}
	if !hasArgv {
		return hasCommand, nil
	}
	if len(argv) == 0 {
		return false, fmt.Errorf("invalid delivery config: %s.argv must be a non-empty array of non-empty strings", path)
	}
	for index, arg := range argv {
		if strings.TrimSpace(arg) == "" {
			return false, fmt.Errorf("invalid delivery config: %s.argv[%d] must not be empty", path, index)
		}
	}
	return true, nil
}

func normalizeNonEmptyStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}
	return out
}

func showBaseConfig(ctx context.Context, repoPath, baseBranch string, show ShowBaseConfigFunc) ([]byte, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if show == nil {
		show = defaultShowBaseConfig
	}
	data, err := show(ctx, repoPath, baseBranch)
	if err != nil {
		if gitutil.IsPathAbsentOnRef(err, ".delivery.yml") {
			return nil, false, nil
		}
		return nil, false, err
	}
	return data, true, nil
}

func defaultShowBaseConfig(ctx context.Context, repoPath, baseBranch string) ([]byte, error) {
	content, err := gitutil.New().Show(ctx, repoPath, normalizeBaseBranch(baseBranch)+":.delivery.yml")
	if err != nil {
		return nil, err
	}
	return []byte(content), nil
}

func normalizeBaseBranch(baseBranch string) string {
	baseBranch = strings.TrimSpace(baseBranch)
	if baseBranch == "" {
		return lcdefaults.BaseBranch
	}
	return baseBranch
}

func warnBaseConfigCheckFailed(w io.Writer, baseBranch string, err error) {
	if err == nil {
		return
	}
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintf(w, "[loopcoder] warning: base .delivery.yml consistency check could not run for %s: %v; using defaults (configuration may be stale)\n", normalizeBaseBranch(baseBranch), err)
}
