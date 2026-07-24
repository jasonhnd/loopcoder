package providerinventory

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
)

func TestParseCodexModelListResult_ObjectShapedEfforts(t *testing.T) {
	raw := mustRawJSON(t, map[string]any{
		"data": []any{
			map[string]any{
				"id": "gpt-5.5", "model": "gpt-5.5", "displayName": "GPT-5.5",
				"hidden": false, "isDefault": false,
				"defaultReasoningEffort": "medium",
				"supportedReasoningEfforts": []any{
					map[string]any{"reasoningEffort": "low", "description": "fast"},
					map[string]any{"reasoningEffort": "medium", "description": "balanced"},
					map[string]any{"reasoningEffort": "high", "description": "deep"},
					map[string]any{"reasoningEffort": "xhigh", "description": "extra"},
					map[string]any{"reasoningEffort": "max", "description": "maps to xhigh"},
				},
			},
			map[string]any{
				"id": "hidden-model", "hidden": true,
				"supportedReasoningEfforts": []any{
					map[string]any{"reasoningEffort": "low"},
				},
			},
		},
		"nextCursor": nil,
	})
	entries, gaps, err := parseCodexModelListResult(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries=%d gaps=%v want 1 (hidden skipped)", len(entries), gaps)
	}
	e := entries[0]
	if e.ID != "gpt-5.5" || e.DefaultReasoningEffort != "medium" {
		t.Fatalf("entry=%#v", e)
	}
	// max normalizes to xhigh; unique sorted
	want := []string{"high", "low", "medium", "xhigh"}
	if strings.Join(e.SupportedReasoningEffort, ",") != strings.Join(want, ",") {
		t.Fatalf("efforts=%v want %v", e.SupportedReasoningEffort, want)
	}
}

func TestParseCodexModelListResult_MalformedAndEmpty(t *testing.T) {
	if _, _, err := parseCodexModelListResult(nil); err == nil {
		t.Fatal("empty result must fail")
	}
	if _, _, err := parseCodexModelListResult(json.RawMessage(`{"nope":true}`)); err == nil {
		t.Fatal("missing data must fail")
	}
	if _, _, err := parseCodexModelListResult(json.RawMessage(`{`)); err == nil {
		t.Fatal("malformed json must fail")
	}
	// Invalid id skipped via gap
	raw := mustRawJSON(t, map[string]any{
		"data": []any{
			map[string]any{"id": "", "supportedReasoningEfforts": []any{}},
			map[string]any{"id": "sk-secret-looking-token-xx", "supportedReasoningEfforts": []any{
				map[string]any{"reasoningEffort": "low"},
			}},
			map[string]any{"id": "ok-model", "hidden": false, "supportedReasoningEfforts": []any{
				map[string]any{"reasoningEffort": "low"},
				map[string]any{"reasoningEffort": ""}, // malformed effort object
			}},
		},
	})
	entries, gaps, err := parseCodexModelListResult(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != "ok-model" {
		t.Fatalf("entries=%#v gaps=%v", entries, gaps)
	}
	if len(gaps) == 0 {
		t.Fatal("want gaps for invalid id/effort")
	}
}

func TestCatalogSourcesFromCodexModelList_Constraints(t *testing.T) {
	entries := []codexModelListEntry{{
		ID: "gpt-5.5", DisplayName: "GPT-5.5",
		DefaultReasoningEffort:   "medium",
		SupportedReasoningEffort: []string{"low", "medium", "high", "xhigh"},
		IsDefault:                false,
	}}
	sources, gaps := catalogSourcesFromCodexModelList(AdapterDeclaration{AdapterID: "codex"}, "codex-cli 0.145.0", entries)
	if len(sources) != 1 || sources[0].Kind != CatalogSourceProviderMachineReadable {
		t.Fatalf("sources=%#v gaps=%v", sources, gaps)
	}
	if !strings.HasPrefix(sources[0].Reference, "codex-app-server:model-list#sha256:") {
		t.Fatalf("reference=%q want hash-only codex-app-server prefix", sources[0].Reference)
	}
	if sources[0].Confidence != ConfidenceExact || sources[0].FreshnessState != FreshnessFresh {
		t.Fatalf("confidence/freshness=%v/%v", sources[0].Confidence, sources[0].FreshnessState)
	}
	e := sources[0].Entries[0]
	joined := strings.Join(e.Constraints, ";")
	for _, need := range []string{
		"cli_model=gpt-5.5",
		"supported_depth=low",
		"supported_depth=medium",
		"supported_depth=high",
		"supported_depth=xhigh",
		"default_depth=medium",
	} {
		if !strings.Contains(joined, need) {
			t.Fatalf("constraints missing %q: %v", need, e.Constraints)
		}
	}
}

func TestDecodeCodexCatalogRPC_MultiPageAndNoRawRetention(t *testing.T) {
	page1 := map[string]any{
		"data": []any{
			map[string]any{
				"id": "gpt-5.5", "displayName": "GPT-5.5", "hidden": false,
				"defaultReasoningEffort": "medium",
				"supportedReasoningEfforts": []any{
					map[string]any{"reasoningEffort": "low"},
					map[string]any{"reasoningEffort": "medium"},
					map[string]any{"reasoningEffort": "high"},
					map[string]any{"reasoningEffort": "xhigh"},
				},
			},
		},
		"nextCursor": "cursor-page-2",
	}
	page2 := map[string]any{
		"data": []any{
			map[string]any{
				"id": "gpt-5.4", "displayName": "GPT-5.4", "hidden": false,
				"defaultReasoningEffort": "medium",
				"supportedReasoningEfforts": []any{
					map[string]any{"reasoningEffort": "low"},
					map[string]any{"reasoningEffort": "high"},
				},
			},
		},
		"nextCursor": "",
	}
	stdout := strings.Join([]string{
		codexQuotaJSONL(t, jsonRPCMessage{ID: 1, Result: mustRawJSON(t, map[string]any{
			"codexHome": "/tmp/codex", "platformFamily": "unix", "platformOs": "linux", "userAgent": "codex-test",
		})}),
		codexQuotaJSONL(t, jsonRPCMessage{ID: 2, Result: mustRawJSON(t, map[string]any{
			"requiresOpenaiAuth": false,
			"account":            map[string]any{"type": "chatgpt", "planType": "pro", "id": "acct_fixture"},
		})}),
		codexQuotaJSONL(t, jsonRPCMessage{ID: 10, Result: mustRawJSON(t, page1)}),
		codexQuotaJSONL(t, jsonRPCMessage{ID: 11, Result: mustRawJSON(t, page2)}),
	}, "\n") + "\n"
	account, entries, gaps, err := decodeCodexCatalogRPC(stdout)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries=%d gaps=%v", len(entries), gaps)
	}
	acct := codexCanonicalAccountProfileID(account)
	if acct == "" {
		t.Fatal("want opaque account profile id from account/read")
	}
	if !strings.HasPrefix(acct, "acct-") && acct != "acct_fixture" {
		// Canonical may hash to acct-<hex>
		if !strings.HasPrefix(acct, "acct") {
			t.Fatalf("account id=%q", acct)
		}
	}
	// No raw protocol payload in derived source reference
	sources, _ := catalogSourcesFromCodexModelList(AdapterDeclaration{AdapterID: "codex"}, "0.145.0", entries)
	blob, _ := json.Marshal(sources)
	if strings.Contains(string(blob), "cursor-page-2") || strings.Contains(string(blob), "usedPercent") {
		t.Fatalf("sources retained protocol cursor/raw: %s", blob)
	}
}

func TestDecodeCodexCatalogRPC_ErrorAndMissing(t *testing.T) {
	// Missing model/list
	stdout := strings.Join([]string{
		codexQuotaJSONL(t, jsonRPCMessage{ID: 1, Result: mustRawJSON(t, map[string]any{
			"codexHome": "/tmp/codex", "platformFamily": "unix", "platformOs": "linux", "userAgent": "codex-test",
		})}),
		codexQuotaJSONL(t, jsonRPCMessage{ID: 2, Result: mustRawJSON(t, map[string]any{
			"account": map[string]any{"id": "acct_fixture"},
		})}),
	}, "\n") + "\n"
	if _, _, _, err := decodeCodexCatalogRPC(stdout); err == nil {
		t.Fatal("missing model/list must fail")
	}
	// RPC error on list
	stdout = strings.Join([]string{
		codexQuotaJSONL(t, jsonRPCMessage{ID: 1, Result: mustRawJSON(t, map[string]any{
			"codexHome": "/tmp/codex", "platformFamily": "unix", "platformOs": "linux", "userAgent": "codex-test",
		})}),
		codexQuotaJSONL(t, jsonRPCMessage{ID: 2, Result: mustRawJSON(t, map[string]any{
			"account": map[string]any{"id": "acct_fixture"},
		})}),
		codexQuotaJSONL(t, jsonRPCMessage{ID: 10, Error: &jsonRPCError{Code: -32000, Message: "boom"}}),
	}, "\n") + "\n"
	if _, _, _, err := decodeCodexCatalogRPC(stdout); err == nil {
		t.Fatal("rpc error must fail")
	}
}

func TestDriveCodexCatalog_PageCeilingAndCursorCycle(t *testing.T) {
	// Direct ceiling check in decode after merge — inject one oversized page.
	frames := []string{
		codexQuotaJSONL(t, jsonRPCMessage{ID: 1, Result: mustRawJSON(t, map[string]any{
			"codexHome": "/tmp/codex", "platformFamily": "unix", "platformOs": "linux", "userAgent": "codex-test",
		})}),
		codexQuotaJSONL(t, jsonRPCMessage{ID: 2, Result: mustRawJSON(t, map[string]any{
			"account": map[string]any{"id": "acct_fixture"},
		})}),
	}
	// One page with too many items
	data := make([]any, 0, codexCatalogMaxItems+1)
	for i := 0; i < codexCatalogMaxItems+1; i++ {
		data = append(data, map[string]any{
			"id": fmt.Sprintf("model-%d", i), "hidden": false,
			"supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "medium"}},
		})
	}
	frames = append(frames, codexQuotaJSONL(t, jsonRPCMessage{ID: 10, Result: mustRawJSON(t, map[string]any{
		"data": data, "nextCursor": "",
	})}))
	if _, _, _, err := decodeCodexCatalogRPC(strings.Join(frames, "\n") + "\n"); err == nil {
		t.Fatal("item ceiling must fail closed")
	}
}

func TestInspectCodexCatalog_RequiresNetworkGrant(t *testing.T) {
	exe := writeFakeCodex(t)
	deps := codexQuotaDeps(t, exe, "codex-cli 0.145.0", CodexAppServerResult{}, nil)
	called := false
	deps.RunCodexRPC = func(context.Context, CodexAppServerRequest) (CodexAppServerResult, error) {
		called = true
		return CodexAppServerResult{}, nil
	}
	report, err := Discover(context.Background(), Options{
		Config: config.Config{Adapters: config.Adapters{Worker: "codex"}},
		Now:    fixedInventoryNow,
		// No network grants.
	}, deps)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if called {
		t.Fatal("RunCodexRPC must not run without model-catalog grant")
	}
	// Static adapter-declared still present; MR unavailable snapshot also recorded.
	var sawMRUnavailable, sawStatic bool
	for _, s := range report.ModelCatalogSnapshots {
		if s.AdapterID != "codex" {
			continue
		}
		if s.CatalogSourceKind == CatalogSourceAdapterDeclared {
			sawStatic = true
		}
		if s.CatalogSourceKind == CatalogSourceProviderMachineReadable &&
			(s.Confidence == ConfidenceUnavailable || s.TerminalErrorCode == "ErrNetworkPermissionDenied") {
			sawMRUnavailable = true
		}
	}
	if !sawStatic {
		t.Fatal("want static adapter-declared codex snapshot")
	}
	if !sawMRUnavailable {
		t.Fatalf("want unavailable MR catalog when grant missing: %#v", report.ModelCatalogSnapshots)
	}
}

func TestInspectCodexCatalog_SuccessAccountBoundExact(t *testing.T) {
	exe := writeFakeCodex(t)
	stdout := codexCatalogFrames(t,
		map[string]any{"requiresOpenaiAuth": false, "account": map[string]any{"type": "chatgpt", "planType": "pro", "id": "acct_fixture"}},
		map[string]any{
			"data": []any{
				map[string]any{
					"id": "gpt-5.5", "model": "gpt-5.5", "displayName": "GPT-5.5",
					"hidden": false, "isDefault": false,
					"defaultReasoningEffort": "medium",
					"supportedReasoningEfforts": []any{
						map[string]any{"reasoningEffort": "low", "description": "fast"},
						map[string]any{"reasoningEffort": "medium", "description": "mid"},
						map[string]any{"reasoningEffort": "high", "description": "deep"},
						map[string]any{"reasoningEffort": "xhigh", "description": "extra"},
					},
				},
			},
			"nextCursor": "",
		},
	)
	var calls int
	var drives []bool
	deps := codexQuotaDeps(t, exe, "codex-cli 0.145.0", CodexAppServerResult{}, nil)
	deps.RunCodexRPC = func(_ context.Context, req CodexAppServerRequest) (CodexAppServerResult, error) {
		calls++
		drives = append(drives, req.Drive != nil)
		for _, arg := range req.Argv {
			if strings.Contains(arg, "login") || strings.Contains(arg, "exec") {
				t.Fatalf("unsafe argv: %#v", req.Argv)
			}
		}
		// Quota session has Drive=nil; catalog sets Drive.
		if req.Drive != nil {
			return CodexAppServerResult{Stdout: stdout, ExitCode: 0}, nil
		}
		// Quota path: reuse minimal valid quota frames so discover succeeds.
		resetFiveHour := fixedInventoryNow().Add(5 * time.Hour).Unix()
		resetWeek := fixedInventoryNow().Add(7 * 24 * time.Hour).Unix()
		q := codexQuotaFrames(t,
			map[string]any{"requiresOpenaiAuth": false, "account": map[string]any{"type": "chatgpt", "planType": "pro", "id": "acct_fixture"}},
			map[string]any{
				"rateLimits": map[string]any{
					"limitId":   "codex",
					"primary":   map[string]any{"usedPercent": 25, "windowDurationMins": 300, "resetsAt": resetFiveHour},
					"secondary": map[string]any{"usedPercent": 10, "windowDurationMins": 10080, "resetsAt": resetWeek},
				},
			},
		)
		return CodexAppServerResult{Stdout: q, ExitCode: 0}, nil
	}

	report, err := Discover(context.Background(), Options{
		Config: config.Config{Adapters: config.Adapters{Worker: "codex"}},
		Now:    fixedInventoryNow,
		NetworkGrants: []NetworkGrant{
			{ProviderID: "codex", Purpose: NetworkPurposeQuotaTelemetry, Scope: NetworkScopeMachineInventory},
			{ProviderID: "codex", Purpose: NetworkPurposeModelCatalog, Scope: NetworkScopeMachineInventory},
		},
	}, deps)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if calls < 2 {
		t.Fatalf("RunCodexRPC calls=%d want >=2 (quota+catalog)", calls)
	}
	// At least one call must be catalog drive.
	sawCatalogDrive := false
	for _, d := range drives {
		if d {
			sawCatalogDrive = true
		}
	}
	if !sawCatalogDrive {
		t.Fatal("catalog session must set Drive")
	}

	reportJSON, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(reportJSON), "supportedReasoningEfforts") ||
		strings.Contains(string(reportJSON), "requiresOpenaiAuth") ||
		strings.Contains(string(reportJSON), stdout) {
		t.Fatalf("report retained raw app-server protocol payload")
	}

	var mr *ModelCatalogSnapshot
	for i := range report.ModelCatalogSnapshots {
		s := &report.ModelCatalogSnapshots[i]
		if s.AdapterID == "codex" && s.CatalogSourceKind == CatalogSourceProviderMachineReadable && s.Confidence == ConfidenceExact {
			mr = s
			break
		}
	}
	if mr == nil {
		t.Fatalf("missing exact MR codex catalog snapshot: %#v", report.ModelCatalogSnapshots)
	}
	if mr.FreshnessState != FreshnessFresh {
		t.Fatalf("freshness=%v", mr.FreshnessState)
	}
	if mr.ProviderInstallationID == nil || *mr.ProviderInstallationID == "" {
		t.Fatal("catalog must bind install")
	}
	if mr.AccountProfileID == nil || *mr.AccountProfileID == "" {
		t.Fatal("catalog must bind opaque account when account/read succeeds")
	}
	if !strings.HasPrefix(mr.CatalogSourceReference, "codex-app-server:model-list#sha256:") {
		t.Fatalf("reference=%q", mr.CatalogSourceReference)
	}

	var gpt55 *ModelCapability
	for i := range report.ModelCapabilities {
		c := &report.ModelCapabilities[i]
		if c.AdapterID == "codex" && c.CanonicalModelID == "gpt-5.5" &&
			len(c.EntrySources) > 0 && c.EntrySources[0].SourceKind == CatalogSourceProviderMachineReadable {
			gpt55 = c
			break
		}
	}
	if gpt55 == nil {
		t.Fatalf("missing MR gpt-5.5 capability: %#v", report.ModelCapabilities)
	}
	joined := strings.Join(gpt55.Constraints, ";")
	for _, d := range []string{"low", "medium", "high", "xhigh"} {
		if !strings.Contains(joined, "supported_depth="+d) {
			t.Fatalf("missing depth %s in %v", d, gpt55.Constraints)
		}
	}
}

func TestInspectCodexCatalog_FailureKeepsStaticHintOnlyPath(t *testing.T) {
	exe := writeFakeCodex(t)
	deps := codexQuotaDeps(t, exe, "codex-cli 0.145.0", CodexAppServerResult{}, nil)
	deps.RunCodexRPC = func(_ context.Context, req CodexAppServerRequest) (CodexAppServerResult, error) {
		if req.Drive != nil {
			// Catalog fails.
			return CodexAppServerResult{Stdout: "not-json\n", ExitCode: 0}, nil
		}
		resetFiveHour := fixedInventoryNow().Add(5 * time.Hour).Unix()
		q := codexQuotaFrames(t,
			map[string]any{"account": map[string]any{"id": "acct_fixture"}},
			map[string]any{"rateLimits": map[string]any{
				"primary": map[string]any{"usedPercent": 1, "windowDurationMins": 300, "resetsAt": resetFiveHour},
			}},
		)
		return CodexAppServerResult{Stdout: q, ExitCode: 0}, nil
	}
	report, err := Discover(context.Background(), Options{
		Config: config.Config{Adapters: config.Adapters{Worker: "codex"}},
		Now:    fixedInventoryNow,
		NetworkGrants: []NetworkGrant{
			{ProviderID: "codex", Purpose: NetworkPurposeQuotaTelemetry, Scope: NetworkScopeMachineInventory},
			{ProviderID: "codex", Purpose: NetworkPurposeModelCatalog, Scope: NetworkScopeMachineInventory},
		},
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	// Static still present; no exact MR success.
	for _, s := range report.ModelCatalogSnapshots {
		if s.AdapterID == "codex" && s.CatalogSourceKind == CatalogSourceProviderMachineReadable && s.Confidence == ConfidenceExact {
			t.Fatalf("failed model/list must not emit exact MR: %#v", s)
		}
	}
	var staticOK bool
	for _, s := range report.ModelCatalogSnapshots {
		if s.AdapterID == "codex" && s.CatalogSourceKind == CatalogSourceAdapterDeclared {
			staticOK = true
		}
	}
	if !staticOK {
		t.Fatal("static adapter-declared must remain")
	}
}

func codexCatalogFrames(t *testing.T, account, listPage map[string]any) string {
	t.Helper()
	return strings.Join([]string{
		codexQuotaJSONL(t, jsonRPCMessage{ID: 1, Result: mustRawJSON(t, map[string]any{
			"codexHome": "/tmp/codex", "platformFamily": "unix", "platformOs": "linux", "userAgent": "codex-test",
		})}),
		codexQuotaJSONL(t, jsonRPCMessage{ID: 2, Result: mustRawJSON(t, account)}),
		codexQuotaJSONL(t, jsonRPCMessage{ID: 10, Result: mustRawJSON(t, listPage)}),
	}, "\n") + "\n"
}

// Ensure package imports used.
var _ = filepath.Join
var _ = time.Second
var _ = context.Background
