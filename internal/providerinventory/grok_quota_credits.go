package providerinventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/grokauth"
)

// Official Grok Build CLI-owned billing endpoint (not ACP agent stdio).
// Live CLI v0.2.111 embeds this path; ACP billing/read is not advertised.
const (
	grokCreditsBillingSourceSchema = "grok.cli.credits_billing.v1"
	grokCreditsBillingURL          = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"
	grokCreditsBillingTimeout      = 12 * time.Second
	grokCreditsBillingMaxBody      = 64 * 1024
)

var (
	ErrGrokCreditsAuthMissing   = errors.New("ErrGrokCreditsAuthMissing")
	ErrGrokCreditsHTTP          = errors.New("ErrGrokCreditsHTTP")
	ErrGrokCreditsMalformed     = errors.New("ErrGrokCreditsMalformed")
	ErrGrokCreditsCredentialMat = errors.New("ErrQuotaCredentialMaterial")
)

// GrokCreditsBillingRequest is a bounded, injectable HTTP billing fetch.
// Authorization is supplied only for the outbound request and never retained
// on the result or in redacted diagnostics.
type GrokCreditsBillingRequest struct {
	URL     string
	Token   string // bearer; caller must not log
	Timeout time.Duration
	// MaxBodyBytes caps response body retained for parse (raw never stored on snapshot).
	MaxBodyBytes int
}

// GrokCreditsBillingResult is the redacted-safe transport result.
// Body is raw JSON for parse only; must not contain credential material after scrub.
type GrokCreditsBillingResult struct {
	StatusCode int
	Body       []byte
	// Duration is optional diagnostic timing (never credentials).
	Duration time.Duration
}

// loadGrokCLIAuthToken uses the shared grokauth parser (same as agent + auth readiness).
// Returns bearer token for Authorization only and the canonical AccountProfileID
// (acct-+64hex) when exact-routable. Identity-less tokens return empty accountRef
// (never "root"/"cli") so they cannot invent a routable account.
func loadGrokCLIAuthToken(home string, getenv func(string) string) (token string, accountRef string, err error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	tok, bind, lerr := grokauth.LoadToken(home, getenv, time.Now().UTC())
	if lerr != nil {
		return "", "", fmt.Errorf("%w: %v", ErrGrokCreditsAuthMissing, lerr)
	}
	if tok == "" {
		return "", "", fmt.Errorf("%w: no bearer key in auth.json", ErrGrokCreditsAuthMissing)
	}
	// Exact-routable only; identity-less may still bill under network identity
	// but cannot join capacity account routing.
	if bind.ExactRoutable {
		accountRef = bind.AccountProfileID
	}
	return tok, accountRef, nil
}

// runGrokCreditsBilling performs the bounded official billing HTTP GET.
// Token is used only for the Authorization header and is not returned.
func runGrokCreditsBilling(ctx context.Context, req GrokCreditsBillingRequest) (GrokCreditsBillingResult, error) {
	url := strings.TrimSpace(req.URL)
	if url == "" {
		url = grokCreditsBillingURL
	}
	tok := strings.TrimSpace(req.Token)
	if tok == "" {
		return GrokCreditsBillingResult{}, ErrGrokCreditsAuthMissing
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = grokCreditsBillingTimeout
	}
	maxBody := req.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = grokCreditsBillingMaxBody
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(runCtx, http.MethodGet, url, nil)
	if err != nil {
		return GrokCreditsBillingResult{}, fmt.Errorf("%w: build request", ErrGrokCreditsHTTP)
	}
	httpReq.Header.Set("Authorization", "Bearer "+tok)
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", "loopcoder-quota-probe/0.9.0")
	// Clear tok from local after header set — GC will reclaim; avoid lingering copies in logs.
	tok = ""
	client := &http.Client{Timeout: timeout}
	start := time.Now()
	resp, err := client.Do(httpReq)
	if err != nil {
		return GrokCreditsBillingResult{}, fmt.Errorf("%w: %v", ErrGrokCreditsHTTP, err)
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, int64(maxBody)+1)
	body, rerr := io.ReadAll(limited)
	if rerr != nil {
		return GrokCreditsBillingResult{StatusCode: resp.StatusCode, Duration: time.Since(start)}, fmt.Errorf("%w: read body", ErrGrokCreditsHTTP)
	}
	if len(body) > maxBody {
		return GrokCreditsBillingResult{StatusCode: resp.StatusCode, Duration: time.Since(start)}, fmt.Errorf("%w: body too large", ErrGrokCreditsMalformed)
	}
	return GrokCreditsBillingResult{
		StatusCode: resp.StatusCode,
		Body:       body,
		Duration:   time.Since(start),
	}, nil
}

func grokCreditsBillingSource(now time.Time) QuotaTelemetrySource {
	now = now.UTC()
	return normalizeQuotaTelemetrySource(QuotaTelemetrySource{
		AdapterID:              "grok",
		SourceKind:             QuotaSourceOfficialCLICommand,
		SourceKey:              "grok-cli-credits-billing-v1",
		SourceSchemaVersion:    grokCreditsBillingSourceSchema,
		SupportedQuantities:    []QuantityKind{QuantityProviderDefined},
		SupportedWindows:       []WindowKind{WindowFixedWeek, WindowProviderDefined, WindowUnknown},
		ScopeDimensions:        []string{"provider", "installation", "account", "product", "authority"},
		ConfidenceContract:     map[string]Confidence{"limit_value": ConfidenceExact, "used_value": ConfidenceExact, "remaining_value": ConfidenceExact, "reset_at": ConfidenceExact},
		NetworkDeclared:        true,
		NetworkPermissionScope: "provider:grok/action:quota-read/side-effect:read/freshness:interactive",
		Argv:                   []string{"official-cli-owned-http", "GET", "/v1/billing?format=credits"},
		EnvironmentKeys:        []string{"HOME", "GROK_HOME"},
		TimeoutMS:              int(grokCreditsBillingTimeout / time.Millisecond),
		OutputLimits:           OutputLimits{StdoutBytes: grokCreditsBillingMaxBody, StderrBytes: 4096, CombinedBytes: grokCreditsBillingMaxBody + 4096, DecodedBytes: grokCreditsBillingMaxBody},
		ClassificationRules:    []string{"official-cli-billing-endpoint", "auth-from-cli-auth-json", "redact-before-retain", "no-credential-material", "no-refresh-token-as-bearer"},
		CreatedAt:              formatTime(now),
		UpdatedAt:              formatTime(now),
		PolicyVersion:          PolicyVersion,
		GapReasons:             []string{},
	})
}

// collectGrokCreditsBilling is the production path for official credits billing.
// Returns ok=true only when at least one usable snapshot was produced.
func collectGrokCreditsBilling(
	ctx context.Context,
	discovery *discoveryContext,
	adapter AdapterDeclaration,
	installation ProviderInstallation,
	now time.Time,
	deps Deps,
) (QuotaTelemetrySource, []QuotaSnapshot, ProbeResult, error) {
	source := grokCreditsBillingSource(now)
	installationID := installation.ProviderInstallationID
	probe := baseProbe(adapter, now, deps)
	probe.ProviderInstallationID = &installationID
	probe.ProbeKind = "quota"
	probe.ProbeCommandID = "grok-cli-credits-billing"
	probe.ProbeMethod = ProbeMethodMachineJSON
	probe.TimeoutMS = int(grokCreditsBillingTimeout / time.Millisecond)
	probe.StdoutLimitBytes = grokCreditsBillingMaxBody
	probe.StderrLimitBytes = 4096
	probe.CombinedOutputLimitBytes = grokCreditsBillingMaxBody + 4096
	probe.StaleAfter = formatTime(now.Add(30 * time.Minute))
	probe.NetworkDeclared = true
	probe.NetworkPermission = grokQuotaNetworkPermissionFor(discovery, adapter)
	probe.Argv = redactArgv([]string{"GET", "/v1/billing?format=credits"})
	probe.EnvironmentKeys = []string{"HOME", "GROK_HOME"}
	probe.Source = SourceDescriptor{Kind: "command", AdapterID: adapter.AdapterID, ProbeCommandID: probe.ProbeCommandID, DiscoverySource: "official-cli-billing-endpoint", ExecutableName: "cli-chat-proxy"}
	probe.Evidence = EvidenceSummary{Kind: "bounded-grok-cli-credits-billing-http", CommandBounded: true, NoShell: true, RepositoryMutation: false, SecretMaterialRetained: false}
	probe.SideEffectClass = "network-read"

	fail := func(reason, terminal string) (QuotaTelemetrySource, []QuotaSnapshot, ProbeResult, error) {
		snap := grokCreditsUnavailableSnapshot(source, &installationID, now, reason, terminal)
		probe.Outcome = OutcomeProbeFailed
		probe.Confidence = ConfidenceUnavailable
		probe.FreshnessState = FreshnessNotApplicable
		probe.GapReasons = []string{reason}
		probe.TerminalErrorCode = terminal
		return source, []QuotaSnapshot{snap}, probe, fmt.Errorf("%s", reason)
	}

	if probe.NetworkPermission != NetworkGranted {
		probe.SideEffectClass = "not-run"
		return fail("quota-collection-not-granted", "ErrQuotaCollectionGrantRequired")
	}

	home, _ := deps.UserHomeDir()
	if h := strings.TrimSpace(deps.Getenv("HOME")); h != "" {
		home = h
	}
	token, accountRef, aerr := loadGrokCLIAuthToken(home, deps.Getenv)
	if aerr != nil {
		return fail("credits-auth-missing", "ErrGrokCreditsAuthMissing")
	}
	// Ensure token never enters probe summaries.
	run := deps.RunGrokCredits
	if run == nil {
		run = runGrokCreditsBilling
	}
	result, err := run(ctx, GrokCreditsBillingRequest{
		URL:          grokCreditsBillingURL,
		Token:        token,
		Timeout:      grokCreditsBillingTimeout,
		MaxBodyBytes: grokCreditsBillingMaxBody,
	})
	// Drop token immediately after call.
	token = ""
	if err != nil {
		reason := grokCreditsReason(err)
		return fail(reason, grokCreditsTerminal(err))
	}
	if result.StatusCode == http.StatusUnauthorized || result.StatusCode == http.StatusForbidden {
		return fail("auth-expired", "ErrAuthExpired")
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		return fail("credits-http-status", "ErrGrokCreditsHTTP")
	}
	if credentialMaterialLike(string(result.Body)) {
		// Body should not contain bearer tokens; if it does, refuse.
		return fail("credential-material-redacted", "ErrQuotaCredentialMaterial")
	}
	snaps, perr := snapshotsFromGrokCreditsBilling(source, &installationID, installation.Version, accountRef, result.Body, now)
	if perr != nil {
		return fail(grokCreditsReason(perr), grokCreditsTerminal(perr))
	}
	if len(snaps) == 0 {
		return fail("credits-empty-snapshot", "ErrGrokCreditsMalformed")
	}
	probe.Outcome = OutcomeInstalled
	probe.Confidence = ConfidenceExact
	probe.FreshnessState = FreshnessFresh
	code := result.StatusCode
	probe.ExitCode = &code
	probe.setParsedFields(map[string]string{
		"parser":         grokCreditsBillingSourceSchema,
		"snapshot_count": strconv.Itoa(len(snaps)),
		"http_status":    strconv.Itoa(result.StatusCode),
	})
	// Redacted body summary: hash only.
	probe.StdoutSummary = "billing_json#sha256:" + rawSourceHash(result.Body)[:16]
	return source, snaps, probe, nil
}

func grokCreditsUnavailableSnapshot(source QuotaTelemetrySource, installationID *string, now time.Time, reason, terminal string) QuotaSnapshot {
	return normalizeQuotaSnapshot(QuotaSnapshot{
		QuotaSnapshotID:        quotaSnapshotID("grok", source.QuotaSourceID, reason, formatTime(now)),
		QuotaSourceID:          source.QuotaSourceID,
		SourceKind:             source.SourceKind,
		AdapterID:              "grok",
		ProviderInstallationID: installationID,
		ScopeKey:               "provider:grok",
		QuantityKind:           QuantityProviderDefined,
		ProviderQuantityName:   "credits_billing",
		Unit:                   "percent",
		WindowKind:             WindowUnknown,
		ResetSemantics:         ResetUnknown,
		ValueScale:             0,
		Confidence:             ConfidenceUnavailable,
		FieldConfidences:       map[string]Confidence{"limit_value": ConfidenceUnavailable, "used_value": ConfidenceUnavailable, "remaining_value": ConfidenceUnavailable, "reset_at": ConfidenceUnavailable},
		FreshnessState:         FreshnessNotApplicable,
		CapturedAt:             formatTime(now),
		RedactedDiagnostics:    "grok official CLI credits billing unavailable due to " + reason,
		ConflictSet:            []string{},
		GapReasons:             []string{reason, "not-collected"},
		TerminalErrorCode:      terminal,
		CreatedAt:              formatTime(now),
		UpdatedAt:              formatTime(now),
		PolicyVersion:          PolicyVersion,
	})
}

func grokCreditsReason(err error) string {
	switch {
	case err == nil:
		return "unknown"
	case errors.Is(err, ErrGrokCreditsAuthMissing):
		return "credits-auth-missing"
	case errors.Is(err, ErrGrokCreditsHTTP):
		return "credits-http-failed"
	case errors.Is(err, ErrGrokCreditsMalformed):
		return "credits-malformed"
	case errors.Is(err, ErrQuotaCredentialMaterial), errors.Is(err, ErrGrokCreditsCredentialMat):
		return "credential-material-redacted"
	default:
		msg := strings.ToLower(err.Error())
		switch {
		case strings.Contains(msg, "auth"):
			return "credits-auth-missing"
		case strings.Contains(msg, "malformed"), strings.Contains(msg, "parse"):
			return "credits-malformed"
		default:
			return "credits-http-failed"
		}
	}
}

func grokCreditsTerminal(err error) string {
	switch {
	case errors.Is(err, ErrGrokCreditsAuthMissing):
		return "ErrGrokCreditsAuthMissing"
	case errors.Is(err, ErrGrokCreditsHTTP):
		return "ErrGrokCreditsHTTP"
	case errors.Is(err, ErrGrokCreditsMalformed):
		return "ErrGrokCreditsMalformed"
	case errors.Is(err, ErrQuotaCredentialMaterial), errors.Is(err, ErrGrokCreditsCredentialMat):
		return "ErrQuotaCredentialMaterial"
	default:
		return "ErrGrokCreditsHTTP"
	}
}

// grokCreditsBillingConfig is the truthful subset of the official billing response.
type grokCreditsBillingConfig struct {
	CurrentPeriod struct {
		Type  string `json:"type"`
		Start string `json:"start"`
		End   string `json:"end"`
	} `json:"currentPeriod"`
	CreditUsagePercent *float64 `json:"creditUsagePercent"`
	ProductUsage       []struct {
		Product      string   `json:"product"`
		UsagePercent *float64 `json:"usagePercent"`
	} `json:"productUsage"`
	IsUnifiedBillingUser *bool  `json:"isUnifiedBillingUser"`
	BillingPeriodStart   string `json:"billingPeriodStart"`
	BillingPeriodEnd     string `json:"billingPeriodEnd"`
}

type grokCreditsBillingEnvelope struct {
	Config grokCreditsBillingConfig `json:"config"`
}

func snapshotsFromGrokCreditsBilling(
	source QuotaTelemetrySource,
	installationID *string,
	cliVersion, accountRef string,
	body []byte,
	now time.Time,
) ([]QuotaSnapshot, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("%w: empty body", ErrGrokCreditsMalformed)
	}
	if credentialMaterialLike(string(body)) {
		return nil, ErrQuotaCredentialMaterial
	}
	var env grokCreditsBillingEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("%w: json", ErrGrokCreditsMalformed)
	}
	cfg := env.Config
	rawHash := rawSourceHash(body)
	// accountRef is already the shared grokauth AccountProfileID (acct-+64hex)
	// or empty when identity-less. Never invent "cli"/"root".
	account := strings.TrimSpace(accountRef)
	var accountProfileIDPtr *string
	if account != "" && strings.HasPrefix(account, "acct-") && len(account) == 5+64 {
		accountProfileIDPtr = &account
	} else {
		// Identity-less token: may still emit quota observations but without
		// exact AccountProfileID so capacity routing cannot bind them.
		account = ""
		accountProfileIDPtr = nil
	}

	periodType := strings.TrimSpace(cfg.CurrentPeriod.Type)
	resetAt := firstNonEmpty(cfg.CurrentPeriod.End, cfg.BillingPeriodEnd)
	periodStart := firstNonEmpty(cfg.CurrentPeriod.Start, cfg.BillingPeriodStart)

	// Normalize period bounds to RFC3339 when parseable. ValidateQuotaSnapshot
	// requires window_start+window_end for fixed-week; without them Refresh
	// rejects the whole inventory report and Grok installs never persist.
	windowStart := normalizeGrokRFC3339(periodStart)
	resetNorm := normalizeGrokRFC3339(resetAt)

	windowKind := WindowProviderDefined
	if strings.Contains(strings.ToUpper(periodType), "WEEKLY") {
		// Only claim fixed-week when both bounds are present and truthful.
		if windowStart != "" && resetNorm != "" {
			windowKind = WindowFixedWeek
		} else {
			windowKind = WindowProviderDefined
		}
	} else if periodType == "" {
		windowKind = WindowUnknown
	}

	var snaps []QuotaSnapshot
	// Primary: creditUsagePercent with preserved scale (ValueScale=2).
	if cfg.CreditUsagePercent != nil {
		usedF, remF, okPct := validatePercentPair(*cfg.CreditUsagePercent)
		if !okPct {
			return nil, fmt.Errorf("%w: creditUsagePercent out of range", ErrGrokCreditsMalformed)
		}
		// ValueScale=2 → store hundredths of a percent (100.00% → 10000).
		usedI := int64(math.Round(usedF * 100))
		remI := int64(math.Round(remF * 100))
		limI := int64(10000)
		scope := grokScope(account, "", "credits_usage")
		fieldConf := map[string]Confidence{
			"limit_value":     ConfidenceExact,
			"used_value":      ConfidenceExact,
			"remaining_value": ConfidenceExact,
			"reset_at":        ConfidenceUnknown,
		}
		if resetNorm != "" {
			fieldConf["reset_at"] = ConfidenceExact
		}
		gaps := []string{}
		if resetNorm == "" {
			gaps = append(gaps, "missing-reset-at")
		}
		if windowKind == WindowFixedWeek && (windowStart == "" || resetNorm == "") {
			gaps = append(gaps, "missing-fixed-window-bounds")
		}
		if accountProfileIDPtr == nil {
			gaps = append(gaps, "missing-exact-account-identity")
		}
		terminal := ""
		if remI <= 0 {
			terminal = "ErrQuotaExhausted"
			gaps = append(gaps, "quota-exhausted")
		}
		// Interactive freshness TTL (30m); ResetAt preserved separately (not as StaleAfter alone).
		staleAfter := formatTime(now.Add(30 * time.Minute))
		snaps = append(snaps, normalizeQuotaSnapshot(QuotaSnapshot{
			QuotaSnapshotID:        quotaSnapshotID("grok", source.QuotaSourceID, scope, "credit_usage_percent", formatTime(now)),
			QuotaSourceID:          source.QuotaSourceID,
			SourceKind:             source.SourceKind,
			AdapterID:              "grok",
			ProviderInstallationID: installationID,
			AccountProfileID:       accountProfileIDPtr,
			ScopeKey:               scope,
			QuantityKind:           QuantityProviderDefined,
			ProviderQuantityName:   "credit_usage_percent",
			Unit:                   "percent",
			WindowKind:             windowKind,
			WindowStart:            windowStart,
			WindowEnd:              resetNorm,
			ResetAt:                resetNorm,
			ResetSemantics:         grokResetSemantics(resetNorm, windowKind),
			LimitValue:             &limI,
			UsedValue:              &usedI,
			RemainingValue:         &remI,
			ValueScale:             2,
			Confidence:             ConfidenceExact,
			FieldConfidences:       fieldConf,
			FreshnessState:         FreshnessFresh,
			CapturedAt:             formatTime(now),
			ValidUntil:             resetNorm,
			StaleAfter:             staleAfter,
			RawSourceHash:          rawHash,
			// Avoid key=value forms that trip secretLike generic patterns
			// (e.g. period=USAGE_PERIOD_TYPE_WEEKLY) and block Refresh.
			RedactedDiagnostics: fmt.Sprintf("grok official CLI credits billing parser %s cli version %s period_type %s unified %s", grokCreditsBillingSourceSchema, safeSummary(cliVersion), safeSummary(periodType), boolPtrString(cfg.IsUnifiedBillingUser)),
			ConflictSet:         []string{},
			GapReasons:          gaps,
			TerminalErrorCode:   terminal,
			CreatedAt:           formatTime(now),
			UpdatedAt:           formatTime(now),
			PolicyVersion:       PolicyVersion,
		}))
	}

	// Product-scoped usage (e.g. GrokBuild).
	for _, pu := range cfg.ProductUsage {
		if pu.UsagePercent == nil {
			continue
		}
		product := strings.TrimSpace(pu.Product)
		if product == "" || !safeScopeToken(product) {
			product = "product"
		}
		usedF, remF, okPct := validatePercentPair(*pu.UsagePercent)
		if !okPct {
			continue // reject malformed product percent; do not invent exact values
		}
		usedI := int64(math.Round(usedF * 100))
		remI := int64(math.Round(remF * 100))
		limI := int64(10000)
		scope := grokScope(account, "", "product_"+strings.ToLower(product))
		fieldConf := map[string]Confidence{
			"limit_value":     ConfidenceExact,
			"used_value":      ConfidenceExact,
			"remaining_value": ConfidenceExact,
			"reset_at":        ConfidenceUnknown,
		}
		if resetNorm != "" {
			fieldConf["reset_at"] = ConfidenceExact
		}
		snaps = append(snaps, normalizeQuotaSnapshot(QuotaSnapshot{
			QuotaSnapshotID:        quotaSnapshotID("grok", source.QuotaSourceID, scope, "product_usage_percent", formatTime(now)),
			QuotaSourceID:          source.QuotaSourceID,
			SourceKind:             source.SourceKind,
			AdapterID:              "grok",
			ProviderInstallationID: installationID,
			AccountProfileID:       accountProfileIDPtr,
			ScopeKey:               scope,
			QuantityKind:           QuantityProviderDefined,
			ProviderQuantityName:   "product_usage_percent",
			Unit:                   "percent",
			WindowKind:             windowKind,
			WindowStart:            windowStart,
			WindowEnd:              resetNorm,
			ResetAt:                resetNorm,
			ResetSemantics:         grokResetSemantics(resetNorm, windowKind),
			LimitValue:             &limI,
			UsedValue:              &usedI,
			RemainingValue:         &remI,
			ValueScale:             2,
			Confidence:             ConfidenceExact,
			FieldConfidences:       fieldConf,
			FreshnessState:         FreshnessFresh,
			CapturedAt:             formatTime(now),
			ValidUntil:             resetNorm,
			StaleAfter:             formatTime(now.Add(30 * time.Minute)),
			RawSourceHash:          rawHash,
			RedactedDiagnostics:    fmt.Sprintf("grok official CLI credits billing product %s parser %s", safeSummary(product), grokCreditsBillingSourceSchema),
			ConflictSet:            []string{},
			GapReasons:             []string{},
			CreatedAt:              formatTime(now),
			UpdatedAt:              formatTime(now),
			PolicyVersion:          PolicyVersion,
		}))
	}

	// Period metadata window when percent missing but period present (estimated/unknown usage).
	if len(snaps) == 0 && (windowStart != "" || resetNorm != "") {
		scope := grokScope(account, "", "billing_period")
		snaps = append(snaps, normalizeQuotaSnapshot(QuotaSnapshot{
			QuotaSnapshotID:        quotaSnapshotID("grok", source.QuotaSourceID, scope, "billing_period", formatTime(now)),
			QuotaSourceID:          source.QuotaSourceID,
			SourceKind:             source.SourceKind,
			AdapterID:              "grok",
			ProviderInstallationID: installationID,
			AccountProfileID:       accountProfileIDPtr,
			ScopeKey:               scope,
			QuantityKind:           QuantityProviderDefined,
			ProviderQuantityName:   "billing_period",
			Unit:                   "provider-defined",
			WindowKind:             windowKind,
			WindowStart:            windowStart,
			WindowEnd:              resetNorm,
			ResetAt:                resetNorm,
			ResetSemantics:         grokResetSemantics(resetNorm, windowKind),
			ValueScale:             0,
			Confidence:             ConfidenceUnknown,
			FieldConfidences: map[string]Confidence{
				"limit_value": ConfidenceUnknown, "used_value": ConfidenceUnknown,
				"remaining_value": ConfidenceUnknown,
				"reset_at": func() Confidence {
					if resetNorm != "" {
						return ConfidenceExact
					}
					return ConfidenceUnknown
				}(),
			},
			FreshnessState:      FreshnessFresh,
			CapturedAt:          formatTime(now),
			ValidUntil:          resetNorm,
			StaleAfter:          firstNonEmpty(resetNorm, formatTime(now.Add(30*time.Minute))),
			RawSourceHash:       rawHash,
			RedactedDiagnostics: "grok official CLI credits billing period-only (usage percent absent)",
			ConflictSet:         []string{},
			GapReasons:          []string{"missing-credit-usage-percent"},
			CreatedAt:           formatTime(now),
			UpdatedAt:           formatTime(now),
			PolicyVersion:       PolicyVersion,
		}))
	}

	if len(snaps) == 0 {
		return nil, fmt.Errorf("%w: no usable billing fields", ErrGrokCreditsMalformed)
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].QuotaSnapshotID < snaps[j].QuotaSnapshotID })
	return snaps, nil
}

// normalizeGrokRFC3339 parses common RFC3339/RFC3339Nano timestamps and
// rewrites them in UTC RFC3339Nano. Unparseable non-empty values are returned
// as-is for diagnostics; empty input stays empty.
func normalizeGrokRFC3339(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t.UTC().Format(time.RFC3339Nano)
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t.UTC().Format(time.RFC3339Nano)
	}
	return value
}

// validatePercentPair rejects NaN/Inf and out-of-range [0,100] values.
// Does not invent clamped exact values.
func validatePercentPair(used float64) (usedOut, remaining float64, ok bool) {
	if math.IsNaN(used) || math.IsInf(used, 0) {
		return 0, 0, false
	}
	if used < 0 || used > 100 {
		return 0, 0, false
	}
	return used, 100 - used, true
}

func boolPtrString(b *bool) string {
	if b == nil {
		return "unknown"
	}
	if *b {
		return "true"
	}
	return "false"
}
