package providerinventory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/depthpolicy"
)

const (
	codexCatalogSourceSchema = "codex.app_server.protocol.v2.model_list.v1"
	codexCatalogTimeout      = 20 * time.Second
	// Hard ceilings for fail-closed pagination (never unbounded fetch).
	codexCatalogMaxPages = 20
	codexCatalogMaxItems = 500
	// First model/list JSON-RPC id; subsequent pages use +1.
	codexCatalogListIDBase = 10
)

// inspectCodexCatalog collects account-visible models via the official Codex
// app-server model/list surface (same transport class as quota: read-only
// untrusted app-server JSONL RPC). Results are provider-machine-readable
// catalog rows bound to installation and, when proven in-session, opaque account.
func inspectCodexCatalog(ctx context.Context, discovery *discoveryContext, adapter AdapterDeclaration, candidate candidate, installation ProviderInstallation, now time.Time, deps Deps) (ModelCatalogSnapshot, []ModelCapability, ProbeResult) {
	installationID := installation.ProviderInstallationID
	probe := baseProbe(adapter, now, deps)
	probe.ProviderInstallationID = &installationID
	probe.ProbeKind = "catalog"
	probe.ProbeCommandID = "codex-app-server-model-list"
	probe.ProbeMethod = ProbeMethodMachineJSON
	probe.TimeoutMS = int(codexCatalogTimeout / time.Millisecond)
	probe.StdoutLimitBytes = codexQuotaOutputBytes
	probe.StderrLimitBytes = StdoutLimitBytes
	probe.CombinedOutputLimitBytes = codexQuotaOutputBytes + StdoutLimitBytes
	probe.StaleAfter = formatTime(now.Add(30 * time.Minute))
	probe.NetworkDeclared = true
	probe.NetworkPermission = networkPermissionFor(discovery, adapter, NetworkPurposeModelCatalog, true)
	argv := []string{candidate.path, "-s", "read-only", "-a", "untrusted", "app-server"}
	env := probeEnvironment(deps.Getenv)
	probe.Argv = redactArgv(argv)
	probe.EnvironmentKeys = environmentKeys(env)
	probe.Source = SourceDescriptor{
		Kind: "command", AdapterID: adapter.AdapterID, ProbeCommandID: probe.ProbeCommandID,
		DiscoverySource: string(candidate.source), ExecutableName: filepath.Base(candidate.path),
	}
	probe.Evidence = EvidenceSummary{
		Kind: "bounded-codex-app-server-jsonl-rpc", CommandBounded: true, NoShell: true,
		RepositoryMutation: false, SecretMaterialRetained: false,
	}

	unavailable := func(gaps []string, terminal string) (ModelCatalogSnapshot, []ModelCapability, ProbeResult) {
		source := CatalogSourceInput{
			Kind:                CatalogSourceProviderMachineReadable,
			Reference:           "codex-app-server:model-list:unavailable",
			SourceSchemaVersion: codexCatalogSourceSchema,
			ProviderCLIVersion:  installation.Version,
			Precedence:          200,
			Confidence:          ConfidenceUnavailable,
			FreshnessState:      FreshnessNotApplicable,
			Gaps:                append([]string(nil), gaps...),
		}
		snapshot, capabilities, _ := buildCatalogSnapshot(adapter, &installationID, []CatalogSourceInput{source}, now)
		snapshot.TerminalErrorCode = terminal
		snapshot.Evidence = EvidenceSummary{
			Kind: "bounded-codex-app-server-jsonl-rpc", CommandBounded: true, NoShell: true,
			RepositoryMutation: false, SecretMaterialRetained: false,
		}
		probe.Outcome = OutcomeProbeFailed
		probe.Confidence = ConfidenceUnavailable
		probe.FreshnessState = FreshnessNotApplicable
		probe.GapReasons = append([]string(nil), gaps...)
		probe.TerminalErrorCode = terminal
		return snapshot, capabilities, probe
	}

	if installation.InstallationState != InstallationInstalled {
		probe.SideEffectClass = "not-run"
		return unavailable([]string{"installation-not-usable"}, firstNonEmpty(installation.TerminalErrorCode, "ErrInstallationNotUsable"))
	}
	if probe.NetworkPermission != NetworkGranted {
		probe.SideEffectClass = "not-run"
		return unavailable([]string{"network-permission-denied"}, "ErrNetworkPermissionDenied")
	}

	result, err := deps.RunCodexRPC(ctx, CodexAppServerRequest{
		Argv:               argv,
		Env:                env,
		Timeout:            codexCatalogTimeout,
		StdoutLimitBytes:   codexQuotaOutputBytes,
		StderrLimitBytes:   StdoutLimitBytes,
		CombinedLimitBytes: codexQuotaOutputBytes + StdoutLimitBytes,
		Drive:              driveCodexCatalogProtocolEvents,
	})
	_, stdoutFindings := redactProviderOutputNoTruncate(result.Stdout)
	stderr, stderrFindings := redactProviderOutputBeforeTruncate(result.Stderr, 4096)
	probe.StdoutSummary = codexProtocolSummary(result.Stdout)
	probe.StderrSummary = stderr
	probe.SecretFindingCount = stdoutFindings + stderrFindings
	probe.TimedOut = result.TimedOut
	probe.Killed = result.Killed
	probe.ExitCode = &result.ExitCode

	if err != nil && codexCatalogProtocolError(err) {
		return unavailable([]string{codexCatalogReason(err)}, codexCatalogTerminal(err))
	}
	if err != nil || result.TimedOut || result.Killed {
		if result.TimedOut {
			return unavailable([]string{"catalog-probe-timeout"}, "ErrCodexCatalogTimeout")
		}
		return unavailable([]string{"catalog-probe-failed"}, "ErrCatalogProbeExecutionFailed")
	}
	if result.ExitCode != 0 {
		return unavailable([]string{"catalog-probe-nonzero-exit"}, "ErrCatalogProbeNonZeroExit")
	}

	account, entries, gaps, parseErr := decodeCodexCatalogRPC(result.Stdout)
	if parseErr != nil {
		return unavailable([]string{codexCatalogReason(parseErr)}, codexCatalogTerminal(parseErr))
	}
	if len(entries) == 0 {
		return unavailable(append([]string{"catalog-empty-or-unrecognized"}, gaps...), "ErrCatalogMalformedOutput")
	}

	accountID := ""
	if account != nil {
		accountID = codexCanonicalAccountProfileID(account)
	}
	sources, sourceGaps := catalogSourcesFromCodexModelList(adapter, installation.Version, entries)
	if len(sources) == 0 {
		return unavailable(append([]string{"catalog-empty-or-unrecognized"}, append(gaps, sourceGaps...)...), "ErrCatalogMalformedOutput")
	}
	if len(sourceGaps) > 0 {
		gaps = append(gaps, sourceGaps...)
	}

	snapshot, capabilities, err := buildCatalogSnapshot(adapter, &installationID, sources, now)
	if err != nil {
		return unavailable([]string{"catalog-build-failed"}, "ErrCatalogBuildFailed")
	}
	if accountID != "" {
		acct := accountID
		snapshot.AccountProfileID = &acct
	}
	snapshot.Evidence = EvidenceSummary{
		Kind: "bounded-codex-app-server-jsonl-rpc", CommandBounded: true, NoShell: true,
		RepositoryMutation: false, SecretMaterialRetained: false,
	}
	snapshot.StalePolicy = "provider-machine-readable-catalog-30m"
	if len(gaps) > 0 {
		snapshot.GapReasons = append(snapshot.GapReasons, gaps...)
	}

	probe.Outcome = OutcomeInstalled
	probe.Confidence = ConfidenceExact
	probe.FreshnessState = FreshnessFresh
	probe.setParsedFields(map[string]string{
		"model_count":   fmt.Sprintf("%d", len(entries)),
		"source_count":  fmt.Sprintf("%d", len(sources)),
		"parser":        "codex-app-server-model-list",
		"account_bound": strconv.FormatBool(accountID != ""),
	})
	if len(gaps) > 0 {
		probe.GapReasons = append(probe.GapReasons, gaps...)
	}
	return snapshot, capabilities, probe
}

// driveCodexCatalogProtocolEvents drives initialize → account/read + paginated model/list.
func driveCodexCatalogProtocolEvents(ctx context.Context, stdin io.Writer, events <-chan codexProtocolStdoutEvent) error {
	if err := writeJSONL(stdin, jsonRPCMessage{ID: 1, Method: "initialize", Params: map[string]any{
		"clientInfo": map[string]any{"name": "loopcoder", "version": PolicyVersion},
		"capabilities": map[string]any{
			"experimentalApi":           false,
			"optOutNotificationMethods": []string{"thread/started", "thread/status_changed", "account/rate_limits/updated"},
		},
	}}); err != nil {
		return err
	}
	initialized := false
	accountSeen := false
	listDone := false
	pages := 0
	items := 0
	seenCursors := map[string]bool{}
	var pendingCursor string

	for {
		select {
		case event := <-events:
			if event.err != nil {
				if errors.Is(event.err, io.EOF) {
					return fmt.Errorf("%w: eof before required catalog responses", ErrCodexQuotaMalformed)
				}
				return event.err
			}
			msg, err := decodeCodexJSONLMessage(event.line)
			if err != nil {
				return err
			}
			if msg.Error != nil {
				return fmt.Errorf("%w: %s", ErrCodexQuotaRPC, msg.Error.Message)
			}
			id := jsonRPCID(msg.ID)
			switch {
			case id == "1":
				if initialized {
					continue
				}
				if err := validateCodexInitializeResponse(msg.Result); err != nil {
					return err
				}
				if err := writeJSONL(stdin, jsonRPCMessage{Method: "initialized"}); err != nil {
					return err
				}
				initialized = true
				if err := writeJSONL(stdin, jsonRPCMessage{ID: 2, Method: "account/read", Params: map[string]any{"refreshToken": false}}); err != nil {
					return err
				}
				if err := writeJSONL(stdin, jsonRPCMessage{ID: codexCatalogListIDBase, Method: "model/list", Params: map[string]any{}}); err != nil {
					return err
				}
			case id == "2":
				if !initialized {
					return fmt.Errorf("%w: account/read before initialize", ErrCodexQuotaUnsupported)
				}
				accountSeen = true
			case strings.HasPrefix(id, "") && isCodexCatalogListID(id):
				if !initialized {
					return fmt.Errorf("%w: model/list before initialize", ErrCodexQuotaUnsupported)
				}
				pages++
				if pages > codexCatalogMaxPages {
					return fmt.Errorf("%w: model/list page ceiling", ErrCodexQuotaMalformed)
				}
				pageItems, nextCursor, err := parseCodexModelListPageMeta(msg.Result)
				if err != nil {
					return err
				}
				items += pageItems
				if items > codexCatalogMaxItems {
					return fmt.Errorf("%w: model/list item ceiling", ErrCodexQuotaMalformed)
				}
				if nextCursor == "" {
					listDone = true
				} else {
					// Cursor cycle rejection.
					if seenCursors[nextCursor] {
						return fmt.Errorf("%w: model/list cursor cycle", ErrCodexQuotaMalformed)
					}
					seenCursors[nextCursor] = true
					// Avoid re-requesting the same pending cursor twice.
					if pendingCursor == nextCursor {
						return fmt.Errorf("%w: model/list cursor cycle", ErrCodexQuotaMalformed)
					}
					pendingCursor = nextCursor
					nextID := codexCatalogListIDBase + pages // pages already incremented for current page
					if err := writeJSONL(stdin, jsonRPCMessage{
						ID: nextID, Method: "model/list",
						Params: map[string]any{"cursor": nextCursor},
					}); err != nil {
						return err
					}
				}
			}
			if initialized && accountSeen && listDone {
				return nil
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func isCodexCatalogListID(id string) bool {
	n, err := strconv.Atoi(id)
	if err != nil {
		return false
	}
	return n >= codexCatalogListIDBase
}

func parseCodexModelListPageMeta(result json.RawMessage) (itemCount int, nextCursor string, err error) {
	if len(result) == 0 {
		return 0, "", fmt.Errorf("%w: empty model/list result", ErrCodexQuotaMalformed)
	}
	var page codexModelListPage
	if err := json.Unmarshal(result, &page); err != nil {
		return 0, "", fmt.Errorf("%w: model/list page: %v", ErrCodexQuotaMalformed, err)
	}
	if page.Data == nil {
		return 0, "", fmt.Errorf("%w: model/list missing data", ErrCodexQuotaMalformed)
	}
	return len(page.Data), strings.TrimSpace(page.NextCursor), nil
}

type codexModelListPage struct {
	Data       []json.RawMessage `json:"data"`
	NextCursor string            `json:"nextCursor"`
}

type codexModelListEntry struct {
	ID                       string
	DisplayName              string
	Hidden                   bool
	IsDefault                bool
	DefaultReasoningEffort   string
	SupportedReasoningEffort []string
}

func decodeCodexCatalogRPC(output string) (account map[string]any, entries []codexModelListEntry, gaps []string, err error) {
	if len(output) > codexQuotaOutputBytes {
		return nil, nil, nil, fmt.Errorf("%w: decoded output exceeded limit", ErrCodexQuotaMalformed)
	}
	messages, _, err := decodeJSONLMessages([]byte(output))
	if err != nil {
		return nil, nil, nil, err
	}
	var accountSeen, listSeen bool
	seenIDs := map[string]bool{}
	for _, msg := range messages {
		// Skip pure notifications (method set, no id).
		if msg.Method != "" && jsonRPCID(msg.ID) == "" {
			continue
		}
		id := jsonRPCID(msg.ID)
		if id != "1" && id != "2" && !isCodexCatalogListID(id) {
			continue
		}
		if msg.Error != nil {
			return nil, nil, nil, fmt.Errorf("%w: %s", ErrCodexQuotaRPC, msg.Error.Message)
		}
		if err := validateCodexAppServerEnvelope(msg); err != nil {
			return nil, nil, nil, err
		}
		switch {
		case id == "1":
			if err := validateCodexInitializeResponse(msg.Result); err != nil {
				return nil, nil, nil, err
			}
		case id == "2":
			var raw map[string]any
			if err := json.Unmarshal(msg.Result, &raw); err != nil {
				return nil, nil, nil, fmt.Errorf("%w: account/read result", ErrCodexQuotaMalformed)
			}
			account = raw
			accountSeen = true
		case isCodexCatalogListID(id):
			listSeen = true
			pageEntries, pageGaps, pErr := parseCodexModelListResult(msg.Result)
			if pErr != nil {
				return nil, nil, nil, pErr
			}
			gaps = append(gaps, pageGaps...)
			for _, e := range pageEntries {
				key := strings.ToLower(e.ID)
				if seenIDs[key] {
					continue
				}
				seenIDs[key] = true
				entries = append(entries, e)
			}
		}
	}
	if !accountSeen {
		return nil, nil, nil, fmt.Errorf("%w: missing account/read response", ErrCodexQuotaMalformed)
	}
	if !listSeen {
		return nil, nil, nil, fmt.Errorf("%w: missing model/list response", ErrCodexQuotaMalformed)
	}
	if len(entries) > codexCatalogMaxItems {
		return nil, nil, nil, fmt.Errorf("%w: model/list item ceiling", ErrCodexQuotaMalformed)
	}
	return account, entries, gaps, nil
}

func parseCodexModelListResult(result json.RawMessage) ([]codexModelListEntry, []string, error) {
	if len(result) == 0 {
		return nil, nil, fmt.Errorf("%w: empty model/list result", ErrCodexQuotaMalformed)
	}
	// Per-row raw JSON so we can require exact field presence and reject
	// non-object effort items without partially accepting a model.
	var page struct {
		Data       []json.RawMessage `json:"data"`
		NextCursor string            `json:"nextCursor"`
	}
	if err := json.Unmarshal(result, &page); err != nil {
		return nil, nil, fmt.Errorf("%w: model/list decode: %v", ErrCodexQuotaMalformed, err)
	}
	if page.Data == nil {
		return nil, nil, fmt.Errorf("%w: model/list missing data array", ErrCodexQuotaMalformed)
	}
	var gaps []string
	out := make([]codexModelListEntry, 0, len(page.Data))
	for _, rowRaw := range page.Data {
		entry, gap, ok := parseCodexModelListRow(rowRaw)
		if gap != "" {
			gaps = append(gaps, gap)
		}
		if !ok {
			// Entire model skipped (malformed or intentionally hidden).
			continue
		}
		out = append(out, entry)
	}
	return out, gaps, nil
}

// parseCodexModelListRow applies the fail-closed per-model schema gate for an
// exact/fresh MR catalog row. On any violation the entire model is skipped
// (never emit a partial exact model that would later invent medium-only).
// hidden=true is intentionally skipped without a malformation gap.
// ok=false means do not emit; gap may be empty for intentional hidden skips.
func parseCodexModelListRow(rowRaw json.RawMessage) (entry codexModelListEntry, gap string, ok bool) {
	if len(rowRaw) == 0 || !json.Valid(rowRaw) {
		return codexModelListEntry{}, "catalog-entry-invalid-row", false
	}
	// Reject non-object rows.
	if trimmed := strings.TrimSpace(string(rowRaw)); !strings.HasPrefix(trimmed, "{") {
		return codexModelListEntry{}, "catalog-entry-invalid-row", false
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rowRaw, &raw); err != nil {
		return codexModelListEntry{}, "catalog-entry-invalid-row", false
	}

	// --- id / model (required safe nonempty) ---
	id := strings.TrimSpace(codexRawString(raw["id"]))
	if id == "" {
		id = strings.TrimSpace(codexRawString(raw["model"]))
	}
	if id == "" || secretLike(id) || !safeCatalogModelID(id) {
		return codexModelListEntry{}, "catalog-entry-invalid-id", false
	}

	// --- hidden (required present) ---
	hiddenRaw, hasHidden := raw["hidden"]
	if !hasHidden || len(hiddenRaw) == 0 || string(hiddenRaw) == "null" {
		return codexModelListEntry{}, "catalog-entry-missing-hidden", false
	}
	var hidden bool
	if err := json.Unmarshal(hiddenRaw, &hidden); err != nil {
		return codexModelListEntry{}, "catalog-entry-invalid-hidden", false
	}
	if hidden {
		// Intentionally non-routable; not a malformation.
		return codexModelListEntry{}, "", false
	}

	// --- isDefault (required present) ---
	defFlagRaw, hasDefFlag := raw["isDefault"]
	if !hasDefFlag || len(defFlagRaw) == 0 || string(defFlagRaw) == "null" {
		return codexModelListEntry{}, "catalog-entry-missing-isDefault", false
	}
	var isDefault bool
	if err := json.Unmarshal(defFlagRaw, &isDefault); err != nil {
		return codexModelListEntry{}, "catalog-entry-invalid-isDefault", false
	}

	// --- defaultReasoningEffort (required nonempty) ---
	defEffortRaw, hasDefEffort := raw["defaultReasoningEffort"]
	if !hasDefEffort || len(defEffortRaw) == 0 || string(defEffortRaw) == "null" {
		return codexModelListEntry{}, "catalog-entry-missing-default-effort", false
	}
	defEffort := strings.TrimSpace(codexRawString(defEffortRaw))
	if defEffort == "" || secretLike(defEffort) {
		return codexModelListEntry{}, "catalog-entry-missing-default-effort", false
	}
	defNorm := depthpolicy.NormalizeDepth(defEffort)
	if defNorm == "" {
		return codexModelListEntry{}, "catalog-entry-invalid-default-effort", false
	}

	// --- supportedReasoningEfforts (required nonempty array of objects) ---
	effRaw, hasEfforts := raw["supportedReasoningEfforts"]
	if !hasEfforts || len(effRaw) == 0 || string(effRaw) == "null" {
		return codexModelListEntry{}, "catalog-entry-missing-efforts", false
	}
	var effortItems []json.RawMessage
	if err := json.Unmarshal(effRaw, &effortItems); err != nil {
		return codexModelListEntry{}, "catalog-entry-invalid-efforts", false
	}
	if len(effortItems) == 0 {
		return codexModelListEntry{}, "catalog-entry-empty-efforts", false
	}
	efforts := make([]string, 0, len(effortItems))
	for _, item := range effortItems {
		tok, tokOK := parseCodexEffortItem(item)
		if !tokOK {
			// One bad effort object invalidates the entire model.
			return codexModelListEntry{}, "catalog-entry-malformed-effort", false
		}
		efforts = append(efforts, tok)
	}
	efforts = uniqueSortedStrings(efforts)
	if len(efforts) == 0 {
		return codexModelListEntry{}, "catalog-entry-empty-efforts", false
	}
	// Default must be a member of the normalized supported set.
	defInSupported := false
	for _, e := range efforts {
		if e == defNorm {
			defInSupported = true
			break
		}
	}
	if !defInSupported {
		return codexModelListEntry{}, "catalog-entry-default-not-in-supported", false
	}

	display := strings.TrimSpace(codexRawString(raw["displayName"]))
	return codexModelListEntry{
		ID:                       id,
		DisplayName:              firstNonEmpty(display, id),
		Hidden:                   false,
		IsDefault:                isDefault,
		DefaultReasoningEffort:   defNorm,
		SupportedReasoningEffort: efforts,
	}, "", true
}

// parseCodexEffortItem requires a JSON object with a safe nonempty reasoningEffort.
// Returns normalized depth token. Fail closed on non-object or bad token.
func parseCodexEffortItem(item json.RawMessage) (string, bool) {
	if len(item) == 0 || string(item) == "null" {
		return "", false
	}
	trimmed := strings.TrimSpace(string(item))
	if !strings.HasPrefix(trimmed, "{") {
		return "", false
	}
	var obj struct {
		ReasoningEffort string `json:"reasoningEffort"`
	}
	if err := json.Unmarshal(item, &obj); err != nil {
		return "", false
	}
	tok := strings.TrimSpace(obj.ReasoningEffort)
	if tok == "" || secretLike(tok) {
		return "", false
	}
	// max→xhigh via depthpolicy; ultra retained as exact observed token when
	// NormalizeDepth passes it through unchanged.
	n := depthpolicy.NormalizeDepth(tok)
	if n == "" {
		return "", false
	}
	return n, true
}

func codexRawString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Non-string JSON values are not accepted as model ids / effort strings.
	return ""
}

func catalogSourcesFromCodexModelList(adapter AdapterDeclaration, cliVersion string, entries []codexModelListEntry) ([]CatalogSourceInput, []string) {
	if len(entries) == 0 {
		return nil, []string{"catalog-empty-or-unrecognized"}
	}
	outEntries := make([]CatalogInputEntry, 0, len(entries))
	var gaps []string
	for _, e := range entries {
		if e.ID == "" {
			continue
		}
		constraints := []string{
			"provider=codex",
			"catalog_source=codex-app-server-model-list",
			"cli_model=" + e.ID,
		}
		for _, d := range e.SupportedReasoningEffort {
			constraints = append(constraints, "supported_depth="+d)
		}
		if e.DefaultReasoningEffort != "" {
			constraints = append(constraints, "default_depth="+e.DefaultReasoningEffort)
		}
		if e.IsDefault {
			constraints = append(constraints, "is_default=true")
		}
		outEntries = append(outEntries, CatalogInputEntry{
			CanonicalModelID:    e.ID,
			DisplayName:         firstNonEmpty(e.DisplayName, e.ID),
			LifecycleState:      LifecycleAvailable,
			AvailabilityState:   AvailabilityAvailable,
			ReadOnly:            CapabilityTrue, // codex supports RO and write; catalog does not restrict
			JSONOutput:          CapabilityTrue,
			NestedSubagents:     CapabilityUnknown,
			MCPConfig:           CapabilityTrue,
			Cancellation:        CapabilityTrue,
			TokenUsageReporting: CapabilityTrue,
			ImageInput:          CapabilityUnknown,
			ImageOutput:         CapabilityUnknown,
			Constraints:         constraints,
		})
	}
	if len(outEntries) == 0 {
		return nil, []string{"catalog-empty-or-unrecognized"}
	}
	// Redacted/hash-only source reference (model ids + efforts only; no secrets).
	ref := codexCatalogSourceReference(outEntries)
	return []CatalogSourceInput{{
		Kind:                CatalogSourceProviderMachineReadable,
		Reference:           ref,
		SourceSchemaVersion: codexCatalogSourceSchema,
		ProviderCLIVersion:  cliVersion,
		Precedence:          200,
		Confidence:          ConfidenceExact,
		FreshnessState:      FreshnessFresh,
		Entries:             outEntries,
		Gaps:                gaps,
	}}, gaps
}

func codexCatalogSourceReference(entries []CatalogInputEntry) string {
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		// Stable digest material: id + sorted depth constraints only.
		var depths []string
		for _, c := range e.Constraints {
			if strings.HasPrefix(c, "supported_depth=") {
				depths = append(depths, c)
			}
		}
		sort.Strings(depths)
		parts = append(parts, e.CanonicalModelID+"|"+strings.Join(depths, ","))
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return "codex-app-server:model-list#sha256:" + hex.EncodeToString(sum[:])[:24]
}

func safeCatalogModelID(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 120 {
		return false
	}
	// Model ids are slug-like; reject path/shell/credential shapes.
	if strings.ContainsAny(id, " \t\n\r/\\@") {
		return false
	}
	return true
}

func uniqueSortedStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func codexCatalogProtocolError(err error) bool {
	return errors.Is(err, ErrCodexQuotaUnsupported) ||
		errors.Is(err, ErrCodexQuotaMalformed) ||
		errors.Is(err, ErrCodexQuotaRPC) ||
		errors.Is(err, ErrCodexQuotaTimeout)
}

func codexCatalogReason(err error) string {
	switch {
	case errors.Is(err, ErrCodexQuotaTimeout):
		return "catalog-probe-timeout"
	case errors.Is(err, ErrCodexQuotaRPC):
		return "catalog-rpc-error"
	case errors.Is(err, ErrCodexQuotaUnsupported):
		return "catalog-unsupported"
	case errors.Is(err, ErrCodexQuotaMalformed):
		if err != nil && strings.Contains(err.Error(), "page ceiling") {
			return "catalog-page-ceiling"
		}
		if err != nil && strings.Contains(err.Error(), "item ceiling") {
			return "catalog-item-ceiling"
		}
		if err != nil && strings.Contains(err.Error(), "cursor cycle") {
			return "catalog-cursor-cycle"
		}
		return "catalog-output-malformed"
	default:
		return "catalog-probe-failed"
	}
}

func codexCatalogTerminal(err error) string {
	switch {
	case errors.Is(err, ErrCodexQuotaTimeout):
		return "ErrCodexCatalogTimeout"
	case errors.Is(err, ErrCodexQuotaRPC):
		return "ErrCodexCatalogRPCError"
	case errors.Is(err, ErrCodexQuotaUnsupported):
		return "ErrUnsupportedVersion"
	case errors.Is(err, ErrCodexQuotaMalformed):
		return "ErrCatalogMalformedOutput"
	default:
		return "ErrCatalogProbeExecutionFailed"
	}
}
