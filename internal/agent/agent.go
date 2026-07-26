package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/providerinstall"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
)

// ProviderCallRefusedError marks a deliberate pre-launch refusal such as a
// per-run cost cap. It is safe to inspect through wrapping and must not be
// treated as a provider execution failure or retried automatically.
type ProviderCallRefusedError struct {
	Err error
}

func (e ProviderCallRefusedError) Error() string {
	if e.Err == nil {
		return "provider call refused"
	}
	return e.Err.Error()
}

func (e ProviderCallRefusedError) Unwrap() error { return e.Err }

func IsProviderCallRefused(err error) bool {
	var target ProviderCallRefusedError
	return errors.As(err, &target)
}

type Invocation struct {
	WorktreePath string
	Prompt       string
	Model        string
	Effort       string
	// Permission is the requested permission mode (default|readonly|bounded_write…).
	Permission string
	// AccountRef is the selected non-secret account binding the runner must verify.
	AccountRef string
	// InstallRef is the selected provider installation identity the runner must bind.
	InstallRef string
	// WindowKind / ReservationID are capacity bindings (scheduler may attach after
	// verified account/install match).
	WindowKind    string
	ReservationID string
	ReadOnly      bool
	// BoundedWrite selects a provider mode that may modify only the supplied
	// workspace and must not inherit mutation-capable user configuration.
	BoundedWrite bool
	// DisableDelegation is mandatory for LoopCoder-managed nested roles. It is
	// set by the scheduler boundary and converted to provider-specific hard
	// controls; prompt or environment text cannot unset it.
	DisableDelegation bool
	// CapabilityProbeOnly marks a fixed, read-only canary capability probe.
	// Adapters may classify terminal model refusal for this path only; arbitrary
	// product output must never become model_unavailable evidence.
	CapabilityProbeOnly bool
	OutputSchema        string
	LogPath             string
	Stderr              io.Writer
	HardCap             time.Duration
	StallTimeout        time.Duration
	LivenessMode        string
	LivenessCommand     string
	Guardian            supervisedexec.GuardianOptions
	// RunID and Role tag the spawned provider process as loopcoder-managed and
	// place it in a per-run kill-group (spec 0390, Decision 11).
	RunID string
	Role  string
	// ProviderKey is loopcoder's durable idempotency key for the logical child
	// operation. Runners may pass it to providers with native support; providers
	// without native support receive it only as loopcoder metadata.
	ProviderKey      string
	OnProviderLaunch func(pid int)
	OnProviderStart  func(ProviderProcess) error
	// MCPServers carries provider-neutral MCP declarations. Provider-specific
	// flags and config files are still owned by each runner.
	MCPServers []MCPServer
	// Environment contains trusted per-invocation overrides applied after the
	// runner's normal environment isolation.
	Environment map[string]string
}

func environmentWithOverrides(environ []string, overrides map[string]string) []string {
	if len(overrides) == 0 {
		return append([]string(nil), environ...)
	}
	cleaned := make([]string, 0, len(environ)+len(overrides))
	for _, entry := range environ {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		if _, replaced := overrides[key]; !replaced {
			cleaned = append(cleaned, entry)
		}
	}
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		cleaned = append(cleaned, key+"="+overrides[key])
	}
	return cleaned
}

func mcpInvocationRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "nested-read-only":
		return "verifier"
	case "nested-bounded-write":
		return "worker"
	default:
		return role
	}
}

type MCPServer = config.MCPServer

type MCPAuth = config.MCPAuth

type Result struct {
	ExitCode           int
	Summary            string
	Model              string
	Effort             string
	Usage              reporter.Usage
	StartedAt          string
	EndedAt            string
	DurationMS         int64
	Hung               bool
	HungReason         string
	AdapterVersion     string
	ExternalSessionRef string
	// FailureClass is an optional structured outcome class set by adapters
	// (provideroutcome.Class values). Orchestration classifies using this field
	// and other structured flags, never by user-facing error-string matching.
	FailureClass string
	// Independently verified actual route binding (never a silent request copy).
	// Empty fields mean the runner could not affirm that identity.
	ActualProvider     string
	ActualModel        string
	ActualEffort       string
	ActualPermission   string
	ActualAccountRef   string
	ActualInstallRef   string
	ExecutableIdentity string // exact executable/install used by the runner
	// ActualSource* is the honest evidence class for each Actual* dimension.
	// Never pretends request-copy is observed; accepted_invocation is never
	// collapsed into provider_stream.
	ActualSourceModel      string
	ActualSourceEffort     string
	ActualSourcePermission string
	ActualSourceAccount    string
	ActualSourceInstall    string
	// ArgvDigest is a redacted sha256 of the exact launched argv (no secrets).
	ArgvDigest string
}

// ActualSource values for independently verified Actual* fields.
const (
	// ActualSourceProviderStream: value echoed/parsed from provider output stream.
	ActualSourceProviderStream = "provider_stream"
	// ActualSourceAcceptedInvocation: exact CLI argv option after full success;
	// NOT provider-reported actual — reports must expose this distinction.
	ActualSourceAcceptedInvocation = "accepted_invocation"
	// ActualSourceAttemptedInvocation binds exact argv option positions on a
	// provider-refused invocation. It is never accepted/success evidence.
	ActualSourceAttemptedInvocation = "attempted_invocation"
	// ActualSourceAuthBinding: local exact auth binding (auth.json / grokauth), not stream.
	ActualSourceAuthBinding = "auth_binding"
	// ActualSourceInstallBinding: pinst_* from exact launched executable path.
	ActualSourceInstallBinding = "install_binding"
	// ActualSourceUnknown: dimension required or empty with no allowed source.
	ActualSourceUnknown = "unknown"
)

type ProviderProcess struct {
	PID                   int
	PGID                  int
	ProcessBirthIdentity  string
	ExecutableIdentity    string
	ObservedAt            time.Time
	IdentityAmbiguous     bool
	IdentityAmbiguityNote string
}

type Runner interface {
	Run(ctx context.Context, inv Invocation) (Result, error)
}

func validateNestedDelegationBoundary(inv Invocation) error {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(inv.Role)), "nested-") && !inv.DisableDelegation {
		return errors.New("nested provider invocation must disable provider-native delegation")
	}
	return nil
}

// AffirmBasicActual fills independently verified actual route fields that all
// runners can assert without provider-specific account parsers: provider name
// and InstallRef as pinst_* from the exact executable (install_binding).
//
// CRITICAL: never copies requested inv.Model / inv.Effort / inv.Permission into
// Actual*. Actual model/effort must come from provider-parsed output already on
// res.Model / res.Effort (or stay empty = unknown). ActualPermission stays empty
// unless the runner observed the permission mode from provider evidence.
// AccountRef remains empty unless the provider-specific runner verified it
// (e.g. Grok via shared grokauth). Never copies requested AccountRef.
func AffirmBasicActual(res *Result, provider, executable string, inv Invocation) {
	if res == nil {
		return
	}
	if strings.TrimSpace(res.ActualProvider) == "" {
		res.ActualProvider = strings.TrimSpace(provider)
	}
	// Model/effort: only from independently parsed Result fields — NEVER inv.
	if strings.TrimSpace(res.ActualModel) == "" && strings.TrimSpace(res.Model) != "" {
		res.ActualModel = strings.TrimSpace(res.Model)
		if res.ActualSourceModel == "" {
			res.ActualSourceModel = ActualSourceProviderStream
		}
	}
	if strings.TrimSpace(res.ActualEffort) == "" && strings.TrimSpace(res.Effort) != "" {
		res.ActualEffort = strings.TrimSpace(res.Effort)
		if res.ActualSourceEffort == "" {
			res.ActualSourceEffort = ActualSourceProviderStream
		}
	}
	if strings.TrimSpace(res.ExecutableIdentity) == "" && strings.TrimSpace(executable) != "" {
		res.ExecutableIdentity = strings.TrimSpace(executable)
	}
	// ActualInstallRef = pinst_* from exact executable — source is install_binding.
	if strings.TrimSpace(res.ActualInstallRef) == "" && strings.TrimSpace(executable) != "" {
		if id, err := computeInstallID(provider, executable); err == nil {
			res.ActualInstallRef = id
			res.ActualSourceInstall = ActualSourceInstallBinding
		}
	}
	_ = inv
}

// AffirmAcceptedInvocation applies Actual* from exact CLI option positions after
// FULL runner success validation only. Callers MUST pass success=false for
// nonzero exit, transport loss, missing terminal, model_unavailable, malformed
// metadata, or any pre-terminal failure — never affirm partial failures.
//
// Proof never scans free-form prompt/model/path values by substring. Only exact
// option tokens and their paired values are considered.
func AffirmAcceptedInvocation(res *Result, inv Invocation, argv []string, success bool, opts AcceptedInvocationOpts) {
	if res == nil || !success {
		return
	}
	if dig := RedactedArgvDigest(argv); dig != "" && res.ArgvDigest == "" {
		res.ArgvDigest = dig
	}
	if strings.TrimSpace(res.ActualPermission) == "" && opts.PermissionNoFallback {
		want := strings.TrimSpace(inv.Permission)
		if want == "" {
			if inv.ReadOnly {
				want = "read-only"
			} else if inv.BoundedWrite {
				want = "bounded_write"
			}
		}
		if want != "" && argvHasExactPermissionOption(argv, inv.ReadOnly) {
			res.ActualPermission = want
			res.ActualSourcePermission = ActualSourceAcceptedInvocation
		}
	}
	if strings.TrimSpace(res.ActualModel) == "" && opts.ModelNoFallback {
		want := strings.TrimSpace(inv.Model)
		if want != "" && argvOptionValueEquals(argv, want, "-m", "--model") {
			res.ActualModel = want
			res.Model = want
			res.ActualSourceModel = ActualSourceAcceptedInvocation
		}
	}
	if strings.TrimSpace(res.ActualEffort) == "" && opts.EffortNoFallback {
		want := strings.TrimSpace(inv.Effort)
		if want != "" && argvHasExactEffortOption(argv, want) {
			res.ActualEffort = want
			res.Effort = want
			res.ActualSourceEffort = ActualSourceAcceptedInvocation
		}
	}
}

// AcceptedInvocationOpts controls which dimensions may be affirmed from argv.
type AcceptedInvocationOpts struct {
	PermissionNoFallback bool
	ModelNoFallback      bool
	EffortNoFallback     bool
}

// RedactedArgvDigest hashes argv tokens with path-like and secret-like values redacted.
func RedactedArgvDigest(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	h := sha256.New()
	for _, a := range argv {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if strings.HasPrefix(a, "/") || (!strings.HasPrefix(a, "-") && len(a) > 80) {
			if strings.Contains(a, "/") || len(a) > 80 {
				a = "<redacted>"
			}
		}
		if strings.Contains(strings.ToLower(a), "key=") || strings.Contains(strings.ToLower(a), "token=") {
			a = "<redacted>"
		}
		h.Write([]byte(a))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))[:24]
}

// argvHasExactPermissionOption reports exact option-position permission proof.
// Never scans free-form prompt/model/path values by substring.
func argvHasExactPermissionOption(argv []string, readOnly bool) bool {
	// Boolean flags that must appear as exact tokens (not inside free-form values).
	boolFlagsRO := map[string]struct{}{
		"--safe-mode": {}, "--skip-trust": {},
	}
	boolFlagsWrite := map[string]struct{}{
		"--dangerously-skip-permissions":             {},
		"--dangerously-bypass-approvals-and-sandbox": {},
		"--yolo": {},
	}
	// Flag → allowed values (exact next arg or =suffix).
	valuedRO := map[string][]string{
		"-s":                {"read-only", "read_only"},
		"--sandbox":         {"read-only", "read_only"},
		"--permission-mode": {"plan"},
	}
	valuedWrite := map[string][]string{
		"-s":        {"workspace-write"},
		"--sandbox": {"strict", "workspace-write"},
	}
	if readOnly {
		return argvHasExactBoolFlag(argv, boolFlagsRO) || argvHasExactValuedFlag(argv, valuedRO)
	}
	return argvHasExactBoolFlag(argv, boolFlagsWrite) || argvHasExactValuedFlag(argv, valuedWrite)
}

// argvValueTakingFlags are option tokens whose next argument is a value, never
// a flag. Used so free-form model/path/prompt values equal to flag names cannot
// be misread as boolean permission proof.
var argvValueTakingFlags = map[string]struct{}{
	"-m": {}, "--model": {}, "-p": {}, "--prompt": {}, "--cwd": {},
	"-s": {}, "--sandbox": {}, "--effort": {}, "--reasoning-effort": {},
	"-c": {}, "--permission-mode": {}, "--allow": {}, "--deny": {},
	"--add-dir": {}, "--output-format": {}, "--json-schema": {},
	"--output-schema": {}, "-o": {}, "--cd": {},
}

func argvTokenIsOptionValue(argv []string, i int) bool {
	if i <= 0 {
		return false
	}
	prev := argv[i-1]
	if _, ok := argvValueTakingFlags[prev]; ok {
		return true
	}
	return false
}

func argvHasExactBoolFlag(argv []string, flags map[string]struct{}) bool {
	for i, a := range argv {
		if argvTokenIsOptionValue(argv, i) {
			continue // free-form value slot — never a flag
		}
		if strings.Contains(a, "=") {
			continue
		}
		if _, ok := flags[a]; ok {
			return true
		}
	}
	return false
}

func argvHasExactValuedFlag(argv []string, flags map[string][]string) bool {
	for i := 0; i < len(argv); i++ {
		if argvTokenIsOptionValue(argv, i) {
			continue
		}
		a := argv[i]
		// --flag=value form
		if name, val, ok := strings.Cut(a, "="); ok {
			if allowed, hit := flags[name]; hit {
				for _, want := range allowed {
					if strings.EqualFold(strings.TrimSpace(val), want) {
						return true
					}
				}
			}
			continue
		}
		allowed, hit := flags[a]
		if !hit {
			continue
		}
		if i+1 >= len(argv) {
			continue
		}
		// Next token is the option value — never treat free-form tokens elsewhere.
		got := strings.TrimSpace(argv[i+1])
		for _, want := range allowed {
			if strings.EqualFold(got, want) {
				return true
			}
		}
		i++ // skip consumed value
	}
	return false
}

// argvOptionValueEquals requires exact -m/--model (or listed flags) option-value
// pairing. Free-form prompt equal to model is never proof.
func argvOptionValueEquals(argv []string, want string, flags ...string) bool {
	want = strings.TrimSpace(want)
	if want == "" || len(flags) == 0 {
		return false
	}
	flagSet := map[string]struct{}{}
	for _, f := range flags {
		flagSet[f] = struct{}{}
	}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if name, val, ok := strings.Cut(a, "="); ok {
			if _, hit := flagSet[name]; hit && strings.TrimSpace(val) == want {
				return true
			}
			continue
		}
		if _, hit := flagSet[a]; !hit {
			continue
		}
		if i+1 < len(argv) && strings.TrimSpace(argv[i+1]) == want {
			return true
		}
	}
	return false
}

// argvHasExactEffortOption requires exact --effort / --reasoning-effort pairing
// or Codex -c model_reasoning_effort=<effort>. Never matches free-form prompt.
func argvHasExactEffortOption(argv []string, effort string) bool {
	effort = strings.TrimSpace(effort)
	if effort == "" {
		return false
	}
	if argvOptionValueEquals(argv, effort, "--effort", "--reasoning-effort") {
		return true
	}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		// Bare model_reasoning_effort= is never a free-form prompt token shape
		// we accept unless it is the value of -c.
		if a == "-c" && i+1 < len(argv) {
			v := strings.TrimSpace(argv[i+1])
			if strings.HasPrefix(v, "model_reasoning_effort=") &&
				strings.EqualFold(strings.TrimPrefix(v, "model_reasoning_effort="), effort) {
				return true
			}
		}
	}
	return false
}

// ClearAcceptedActual strips accepted_invocation Actual* so a failed run never
// retains success-only affirmations. Stream/auth/install bindings may remain as
// partial evidence with their own sources.
func ClearAcceptedActual(res *Result) {
	if res == nil {
		return
	}
	if res.ActualSourcePermission == ActualSourceAcceptedInvocation {
		res.ActualPermission = ""
		res.ActualSourcePermission = ActualSourceUnknown
	}
	if res.ActualSourceModel == ActualSourceAcceptedInvocation {
		// Keep Model field for diagnostics; clear Actual* accepted claim.
		res.ActualModel = ""
		res.ActualSourceModel = ActualSourceUnknown
	}
	if res.ActualSourceEffort == ActualSourceAcceptedInvocation {
		res.ActualEffort = ""
		res.ActualSourceEffort = ActualSourceUnknown
	}
}

var registry = map[string]Runner{
	"codex": ExecCodexRunner{},
}

func Lookup(provider string) (Runner, error) {
	if runner, ok := registry[provider]; ok {
		return runner, nil
	}
	return nil, fmt.Errorf("unknown provider %q (supported providers: %s)", provider, strings.Join(SupportedProviders(), ", "))
}

func SupportedProviders() []string {
	providers := make([]string, 0, len(registry))
	for provider := range registry {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	return providers
}

func computeInstallID(provider, executable string) (string, error) {
	return providerinstall.ComputeInstallationID(provider, executable)
}
