// Package config reads loopcoder repository configuration.
package config

import (
	"context"
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
	"gopkg.in/yaml.v3"
)

type Config struct {
	Version      int          `yaml:"version"`
	Adapters     Adapters     `yaml:"adapters"`
	Worker       Worker       `yaml:"worker"`
	Verifier     Verifier     `yaml:"verifier"`
	CI           CI           `yaml:"ci"`
	Verification Verification `yaml:"verification"`
	Resilience   Resilience   `yaml:"resilience"`
	Guardrails   Guardrails   `yaml:"guardrails"`
	Environment  Environment  `yaml:"environment"`
	Evidence     Evidence     `yaml:"evidence"`
	Domain       Domain       `yaml:"domain,omitempty"`
	MCP          MCP          `yaml:"mcp,omitempty"`
	Report       Report       `yaml:"report"`
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
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read delivery config: %w", err)
	}
	return Parse(data)
}

// Parse reads .delivery.yml data, tolerating absent optional sections.
func Parse(data []byte) (Config, error) {
	cfg := Default()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse delivery config: %w", err)
	}
	if err := validateGuardrailBudget(cfg.Guardrails.Budget); err != nil {
		return Config{}, err
	}
	if err := validateGuardrailCircuitBreaker(cfg.Guardrails.CircuitBreaker); err != nil {
		return Config{}, err
	}
	if err := validateMCP(cfg.MCP); err != nil {
		return Config{}, err
	}
	if err := validateDomainCommands(cfg.Domain); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func LoadForRepo(ctx context.Context, repoPath string, opts LoadOptions) (Config, error) {
	cfg := Default()
	loaded, err := Load(filepath.Join(repoPath, ".delivery.yml"))
	if err == nil {
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
		loaded, err := Parse(baseConfig)
		if err != nil {
			return cfg, err
		}
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
