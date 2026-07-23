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
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
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

// loadGrokCLIAuthToken reads the official CLI auth file.
// Current shape (truthful): account-keyed map with nested "key".
// Compatible prior shapes (only when present): root "access_token" or root "key".
// Never returns refresh_token as the bearer; never logs secrets.
func loadGrokCLIAuthToken(home string, getenv func(string) string) (token string, accountRef string, err error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	authPath := ""
	if gh := strings.TrimSpace(getenv("GROK_HOME")); gh != "" {
		authPath = filepath.Join(gh, "auth.json")
	} else {
		home = strings.TrimSpace(home)
		if home == "" {
			return "", "", fmt.Errorf("%w: home empty", ErrGrokCreditsAuthMissing)
		}
		authPath = filepath.Join(home, ".grok", "auth.json")
	}
	raw, rerr := os.ReadFile(authPath)
	if rerr != nil {
		return "", "", fmt.Errorf("%w: auth.json inaccessible", ErrGrokCreditsAuthMissing)
	}
	if credentialMaterialLike(string(raw)) && len(raw) > 0 {
		// File contains credentials by design; we extract then drop raw.
	}
	var root any
	if jerr := json.Unmarshal(raw, &root); jerr != nil {
		return "", "", fmt.Errorf("%w: auth.json malformed", ErrGrokCreditsAuthMissing)
	}
	// Prefer account-keyed map with nested .key (current official shape).
	if m, ok := root.(map[string]any); ok {
		// Root key / access_token (legacy flat shape).
		if tok := firstJSONText(m, "key", "access_token"); tok != "" {
			return tok, "root", nil
		}
		// Account-keyed entries.
		type cand struct {
			ref string
			tok string
			exp time.Time
		}
		var best *cand
		for k, v := range m {
			vm, ok := v.(map[string]any)
			if !ok {
				continue
			}
			tok := firstJSONText(vm, "key")
			if tok == "" {
				// Do not fall back to refresh_token as bearer.
				tok = firstJSONText(vm, "access_token")
			}
			if tok == "" {
				continue
			}
			c := cand{ref: sanitizeGrokAccountRef(k), tok: tok}
			if exp := firstJSONText(vm, "expires_at", "expiresAt"); exp != "" {
				if t, perr := time.Parse(time.RFC3339Nano, exp); perr == nil {
					c.exp = t
				} else if t, perr := time.Parse(time.RFC3339, exp); perr == nil {
					c.exp = t
				}
			}
			if best == nil {
				best = &c
				continue
			}
			// Prefer non-expired; then latest expires_at.
			now := time.Now().UTC()
			bestExpired := !best.exp.IsZero() && best.exp.Before(now)
			cExpired := !c.exp.IsZero() && c.exp.Before(now)
			if bestExpired && !cExpired {
				best = &c
				continue
			}
			if !bestExpired && cExpired {
				continue
			}
			if c.exp.After(best.exp) {
				best = &c
			}
		}
		if best != nil {
			return best.tok, best.ref, nil
		}
	}
	return "", "", fmt.Errorf("%w: no bearer key in auth.json", ErrGrokCreditsAuthMissing)
}

func sanitizeGrokAccountRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "unknown"
	}
	// Collapse OIDC issuer::uuid style to a short stable token.
	if i := strings.LastIndex(ref, "::"); i >= 0 && i+2 < len(ref) {
		ref = ref[i+2:]
	}
	ref = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, ref)
	if len(ref) > 48 {
		ref = ref[:48]
	}
	if !safeScopeToken(ref) {
		return "account"
	}
	return ref
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
	account := strings.TrimSpace(accountRef)
	if account == "" {
		account = "cli"
	}
	if !safeScopeToken(account) {
		account = "cli"
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
	// Primary: creditUsagePercent → used/remaining percent (limit=100).
	if cfg.CreditUsagePercent != nil {
		usedPct := clampPercent(*cfg.CreditUsagePercent)
		remainingPct := 100 - usedPct
		usedI := int64(math.Round(usedPct))
		remI := int64(math.Round(remainingPct))
		limI := int64(100)
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
		terminal := ""
		if remI <= 0 {
			terminal = "ErrQuotaExhausted"
			gaps = append(gaps, "quota-exhausted")
		}
		snaps = append(snaps, normalizeQuotaSnapshot(QuotaSnapshot{
			QuotaSnapshotID:        quotaSnapshotID("grok", source.QuotaSourceID, scope, "credit_usage_percent", formatTime(now)),
			QuotaSourceID:          source.QuotaSourceID,
			SourceKind:             source.SourceKind,
			AdapterID:              "grok",
			ProviderInstallationID: installationID,
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
			ValueScale:             0,
			Confidence:             ConfidenceExact,
			FieldConfidences:       fieldConf,
			FreshnessState:         FreshnessFresh,
			CapturedAt:             formatTime(now),
			ValidUntil:             resetNorm,
			StaleAfter:             firstNonEmpty(resetNorm, formatTime(now.Add(30*time.Minute))),
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
		usedPct := clampPercent(*pu.UsagePercent)
		remainingPct := 100 - usedPct
		usedI := int64(math.Round(usedPct))
		remI := int64(math.Round(remainingPct))
		limI := int64(100)
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
			ValueScale:             0,
			Confidence:             ConfidenceExact,
			FieldConfidences:       fieldConf,
			FreshnessState:         FreshnessFresh,
			CapturedAt:             formatTime(now),
			ValidUntil:             resetNorm,
			StaleAfter:             firstNonEmpty(resetNorm, formatTime(now.Add(30*time.Minute))),
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

func clampPercent(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
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
