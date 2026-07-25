package claudecatalog

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/budget"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/usageledger"
)

const testClaudeAuth = `{"loggedIn":true,"email":"principal@example.invalid","authMethod":"claude.ai","apiProvider":"firstParty","subscriptionType":"max"}`

func TestVerifyRecordsAccountBoundSubsetAndReconciledUsage(t *testing.T) {
	now := time.Date(2026, 7, 26, 4, 0, 0, 0, time.UTC)
	store, err := storage.Open(context.Background(), storage.Options{
		Path: filepath.Join(t.TempDir(), "loopcoder.db"),
		Now:  func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	executable := "/opt/test/bin/claude"
	binding, err := providerinventory.ParseClaudeAuthBinding(executable, []byte(testClaudeAuth), 0, now)
	if err != nil {
		t.Fatal(err)
	}
	inventory := testInventory(binding, now)
	calls := 0
	deps := Deps{
		LookPath:  func(string) (string, error) { return executable, nil },
		MkdirTemp: func(string, string) (string, error) { return t.TempDir(), nil },
		RemoveAll: func(string) error { return nil },
		Run: func(_ context.Context, req CommandRequest) (CommandResult, error) {
			calls++
			switch calls {
			case 1, 3:
				if strings.Join(req.Args, " ") != "auth status --json" {
					t.Fatalf("auth argv = %#v", req.Args)
				}
				return CommandResult{Stdout: []byte(testClaudeAuth), ExitCode: 0}, nil
			case 2:
				assertSafeProbeRequest(t, req)
				return CommandResult{Stdout: []byte(testClaudeStream()), ExitCode: 0}, nil
			default:
				t.Fatalf("unexpected command %d", calls)
				return CommandResult{}, nil
			}
		},
	}
	result, err := Verify(context.Background(), store, Request{
		RepoPath:       t.TempDir(),
		ProjectID:      "proj_test",
		DeliveryRunID:  "run_test",
		Model:          "sonnet",
		Effort:         "low",
		AccountRef:     binding.AccountProfileID,
		InstallRef:     binding.ProviderInstallationID,
		ReservedTokens: 1000,
		Now:            func() time.Time { return now },
		Inventory:      inventory,
	}, deps)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
	receipt := result.Receipt
	if receipt.ActualModel != "claude-sonnet-5" || receipt.RequestedModel != "sonnet" || receipt.AcceptedEffort != "low" {
		t.Fatalf("receipt model/depth = %#v", receipt)
	}
	if receipt.TotalTokens != 31 || receipt.CommittedTokens != 31 || receipt.ReservedTokens != 1000 || receipt.ReleasedTokens != 969 || receipt.BudgetState != "released" {
		t.Fatalf("receipt reconciliation = %#v", receipt)
	}
	if receipt.AccountProfileID != binding.AccountProfileID || receipt.ProviderInstallationID != binding.ProviderInstallationID {
		t.Fatalf("receipt identity = %#v want %#v", receipt, binding)
	}
	if strings.Contains(mustJSON(t, result), "principal@") || strings.Contains(mustJSON(t, result), `"result"`) || strings.Contains(mustJSON(t, result), "session") {
		t.Fatalf("durable result retained forbidden provider material: %s", mustJSON(t, result))
	}
	var verified providerinventory.ModelCatalogSnapshot
	for _, snapshot := range result.Report.ModelCatalogSnapshots {
		if snapshot.CatalogSourceKind == providerinventory.CatalogSourceProviderMachineReadable {
			verified = snapshot
		}
	}
	if verified.CapabilityProbeReceipt == nil || verified.AccountProfileID == nil || *verified.AccountProfileID != binding.AccountProfileID {
		t.Fatalf("verified snapshot missing receipt/account binding: %#v", verified)
	}
	records, err := usageledger.QueryUsageRecords(context.Background(), store, usageledger.Query{ProjectID: "proj_test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Value != 31 || records[0].BudgetReservationID != receipt.BudgetReservationID {
		t.Fatalf("usage records = %#v", records)
	}
}

func TestVerifyFailsClosedOnAccountMismatchBeforePaidInvocation(t *testing.T) {
	now := time.Date(2026, 7, 26, 4, 0, 0, 0, time.UTC)
	store, err := storage.Open(context.Background(), storage.Options{
		Path: filepath.Join(t.TempDir(), "loopcoder.db"),
		Now:  func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	executable := "/opt/test/bin/claude"
	binding, err := providerinventory.ParseClaudeAuthBinding(executable, []byte(testClaudeAuth), 0, now)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	_, err = Verify(context.Background(), store, Request{
		ProjectID:  "proj_test",
		Model:      "sonnet",
		Effort:     "low",
		AccountRef: "acct_wrong",
		Now:        func() time.Time { return now },
		Inventory:  testInventory(binding, now),
	}, Deps{
		LookPath:  func(string) (string, error) { return executable, nil },
		MkdirTemp: func(string, string) (string, error) { return t.TempDir(), nil },
		RemoveAll: func(string) error { return nil },
		Run: func(context.Context, CommandRequest) (CommandResult, error) {
			calls++
			return CommandResult{Stdout: []byte(testClaudeAuth), ExitCode: 0}, nil
		},
	})
	if !errors.Is(err, ErrProbeInvalid) {
		t.Fatalf("err = %v, want ErrProbeInvalid", err)
	}
	if calls != 1 {
		t.Fatalf("paid invocation ran after mismatch; calls=%d", calls)
	}
}

func TestParseStreamJSONFailsClosedOnMalformedPartialAndCredentialOutput(t *testing.T) {
	tests := []struct {
		name   string
		output string
	}{
		{name: "malformed", output: "{not-json}\n"},
		{name: "partial", output: `{"type":"system","subtype":"init","model":"claude-sonnet-5"}` + "\n"},
		{name: "model-mismatch", output: strings.Replace(testClaudeStream(), `"claude-sonnet-5":{"inputTokens"`, `"claude-other":{"inputTokens"`, 1)},
		{name: "noncanonical-aux-model", output: strings.Join([]string{
			`{"type":"system","subtype":"init","model":"claude-sonnet-5"}`,
			`{"type":"result","subtype":"success","is_error":false,"modelUsage":{"claude-sonnet-5":{"inputTokens":2,"outputTokens":1}," claude-haiku":{"inputTokens":1,"outputTokens":1}}}`,
		}, "\n") + "\n"},
		{name: "credential", output: testClaudeStream() + "\n{\"type\":\"status\",\"access_token\":\"secret\"}\n"},
		{name: "email", output: testClaudeStream() + "\n{\"type\":\"status\",\"principal\":\"owner@example.com\"}\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseStreamJSON([]byte(tc.output)); !errors.Is(err, ErrProbeOutput) {
				t.Fatalf("err = %v, want ErrProbeOutput", err)
			}
		})
	}
}

func TestParseStreamJSONAcceptsSuccessfulResultWithInitAndExactModelUsage(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","model":"claude-sonnet-5","apiKeySource":"none"}`,
		`{"type":"result","subtype":"success","is_error":false,"modelUsage":{"claude-sonnet-5":{"inputTokens":2,"outputTokens":4,"cacheReadInputTokens":20,"cacheCreationInputTokens":5,"costUSD":0.000123}}}`,
	}, "\n") + "\n"
	parsed, err := parseStreamJSON([]byte(stream))
	if err != nil {
		t.Fatalf("parseStreamJSON: %v", err)
	}
	if parsed.ActualModel != "claude-sonnet-5" || parsed.TotalTokens != 31 {
		t.Fatalf("parsed = %#v", parsed)
	}
}

func TestParseStreamJSONAcceptsInitBoundPrimaryAndAccountsAuxiliaryUsage(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","model":"claude-opus-4-8[1m]","apiKeySource":"none"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"OK"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"modelUsage":{"claude-opus-4-8[1m]":{"inputTokens":3000,"outputTokens":40,"cacheReadInputTokens":500,"cacheCreationInputTokens":2,"costUSD":0.020000},"claude-haiku-4-5-20251001":{"inputTokens":10,"outputTokens":1,"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"costUSD":0.000010}}}`,
	}, "\n") + "\n"
	parsed, err := parseStreamJSON([]byte(stream))
	if err != nil {
		t.Fatalf("parseStreamJSON: %v", err)
	}
	if parsed.ActualModel != "claude-opus-4-8[1m]" {
		t.Fatalf("actual model = %q", parsed.ActualModel)
	}
	if parsed.InputTokens != 3010 || parsed.OutputTokens != 41 ||
		parsed.CacheReadInputTokens != 500 || parsed.CacheCreateInputTokens != 2 ||
		parsed.TotalTokens != 3553 || parsed.CostUSDMicros != 20010 {
		t.Fatalf("parsed aggregate usage = %#v", parsed)
	}
}

func TestParseStreamJSONRejectsAmbiguousMultipleModelUsageWithoutInit(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"OK"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"modelUsage":{"claude-opus-4-8[1m]":{"inputTokens":20,"outputTokens":1},"claude-haiku-4-5-20251001":{"inputTokens":10,"outputTokens":1}}}`,
	}, "\n") + "\n"
	if _, err := parseStreamJSON([]byte(stream)); !errors.Is(err, ErrProbeOutput) {
		t.Fatalf("err = %v, want ErrProbeOutput", err)
	}
}

func TestVerifyTimeoutReleasesReservationAndNeverCatalogs(t *testing.T) {
	now := time.Date(2026, 7, 26, 4, 0, 0, 0, time.UTC)
	store, err := storage.Open(context.Background(), storage.Options{
		Path: filepath.Join(t.TempDir(), "loopcoder.db"),
		Now:  func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	executable := "/opt/test/bin/claude"
	binding, err := providerinventory.ParseClaudeAuthBinding(executable, []byte(testClaudeAuth), 0, now)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	result, err := Verify(context.Background(), store, Request{
		ProjectID: "proj_test",
		Model:     "sonnet",
		Effort:    "low",
		Now:       func() time.Time { return now },
		Inventory: testInventory(binding, now),
	}, Deps{
		LookPath:  func(string) (string, error) { return executable, nil },
		MkdirTemp: func(string, string) (string, error) { return t.TempDir(), nil },
		RemoveAll: func(string) error { return nil },
		Run: func(_ context.Context, _ CommandRequest) (CommandResult, error) {
			calls++
			if calls == 1 {
				return CommandResult{Stdout: []byte(testClaudeAuth), ExitCode: 0}, nil
			}
			return CommandResult{ExitCode: -1, TimedOut: true}, context.DeadlineExceeded
		},
	})
	if !errors.Is(err, ErrProbeExecution) {
		t.Fatalf("err = %v, want ErrProbeExecution", err)
	}
	records, queryErr := usageledger.QueryUsageRecords(context.Background(), store, usageledger.Query{ProjectID: "proj_test"})
	if queryErr != nil {
		t.Fatal(queryErr)
	}
	if len(records) != 1 ||
		records[0].EventKind != usageledger.EventFailure ||
		records[0].Value != defaultReservedTokens ||
		records[0].Confidence != providerinventory.ConfidenceEstimated ||
		records[0].Estimator != "reserved-token-upper-bound" {
		t.Fatalf("timeout did not preserve conservative usage: %#v", records)
	}
	var state string
	var reservedValue, committedValue, releasedValue int64
	if queryErr := store.WithTx(context.Background(), func(tx storage.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT state, reserved_value, committed_value, released_value
			FROM budget_reservations WHERE project_id = ?`, "proj_test").
			Scan(&state, &reservedValue, &committedValue, &releasedValue)
	}); queryErr != nil {
		t.Fatal(queryErr)
	}
	if state != string(budget.StateCommitted) ||
		reservedValue != 0 ||
		committedValue != defaultReservedTokens ||
		releasedValue != 0 {
		t.Fatalf("timeout budget not conservatively reconciled: state=%s reserved=%d committed=%d released=%d",
			state, reservedValue, committedValue, releasedValue)
	}
	var unavailable providerinventory.ModelCatalogSnapshot
	for _, snapshot := range result.Report.ModelCatalogSnapshots {
		if snapshot.AdapterID == "claude" && snapshot.TerminalErrorCode == "ErrClaudeCapabilityProbeTimeout" {
			unavailable = snapshot
		}
	}
	if unavailable.TerminalErrorCode == "" ||
		unavailable.EntryCount != 0 ||
		unavailable.Confidence != providerinventory.ConfidenceUnavailable ||
		unavailable.CapabilityProbeReceipt != nil {
		t.Fatalf("timeout did not return safe unavailable evidence: %#v", unavailable)
	}
	for _, capability := range result.Report.ModelCapabilities {
		if capability.AdapterID == "claude" && capability.ModelCatalogSnapshotID == unavailable.ModelCatalogSnapshotID {
			t.Fatalf("failed candidate became routable: %#v", capability)
		}
	}
}

func TestVerifyPostAuthDriftAccountsExactUsageAndNeverCatalogs(t *testing.T) {
	now := time.Date(2026, 7, 26, 4, 0, 0, 0, time.UTC)
	store, err := storage.Open(context.Background(), storage.Options{
		Path: filepath.Join(t.TempDir(), "loopcoder.db"),
		Now:  func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	executable := "/opt/test/bin/claude"
	binding, err := providerinventory.ParseClaudeAuthBinding(executable, []byte(testClaudeAuth), 0, now)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	result, err := Verify(context.Background(), store, Request{
		ProjectID:      "proj_test",
		DeliveryRunID:  "run_drift",
		Model:          "sonnet",
		Effort:         "low",
		ReservedTokens: 1000,
		Now:            func() time.Time { return now },
		Inventory:      testInventory(binding, now),
	}, Deps{
		LookPath:  func(string) (string, error) { return executable, nil },
		MkdirTemp: func(string, string) (string, error) { return t.TempDir(), nil },
		RemoveAll: func(string) error { return nil },
		Run: func(_ context.Context, _ CommandRequest) (CommandResult, error) {
			calls++
			switch calls {
			case 1:
				return CommandResult{Stdout: []byte(testClaudeAuth), ExitCode: 0}, nil
			case 2:
				return CommandResult{Stdout: []byte(testClaudeStream()), ExitCode: 0}, nil
			case 3:
				return CommandResult{Stdout: []byte(`{"loggedIn":true,"email":"different@example.invalid","authMethod":"claude.ai","apiProvider":"firstParty","subscriptionType":"max"}`), ExitCode: 0}, nil
			default:
				t.Fatalf("unexpected command %d", calls)
				return CommandResult{}, nil
			}
		},
	})
	if !errors.Is(err, ErrProbeAccountDrift) {
		t.Fatalf("err = %v, want ErrProbeAccountDrift", err)
	}
	records, queryErr := usageledger.QueryUsageRecords(context.Background(), store, usageledger.Query{ProjectID: "proj_test"})
	if queryErr != nil {
		t.Fatal(queryErr)
	}
	if len(records) != 1 ||
		records[0].Value != 31 ||
		records[0].Confidence != providerinventory.ConfidenceEstimated ||
		records[0].EventKind != usageledger.EventFailure ||
		records[0].Estimator != "provider-reported-usage-account-linkage-unconfirmed" {
		t.Fatalf("drift usage not exactly accounted: %#v", records)
	}
	for _, snapshot := range result.Report.ModelCatalogSnapshots {
		if snapshot.AdapterID == "claude" &&
			snapshot.CapabilityProbeReceipt != nil {
			t.Fatalf("drift produced verified catalog: %#v", snapshot)
		}
	}
	for _, capability := range result.Report.ModelCapabilities {
		if capability.AdapterID == "claude" &&
			capability.CanonicalModelID == "claude-sonnet-5" {
			t.Fatalf("drift produced route: %#v", capability)
		}
	}
}

func assertSafeProbeRequest(t *testing.T, req CommandRequest) {
	t.Helper()
	joined := strings.Join(req.Args, "\x00")
	for _, required := range []string{
		"--print", "--output-format\x00stream-json", "--safe-mode", "--tools\x00",
		"--disable-slash-commands", "--strict-mcp-config", "--no-session-persistence",
		"--no-chrome", "--max-turns\x001", "--model\x00sonnet", "--effort\x00low",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("probe argv missing %q: %#v", required, req.Args)
		}
	}
	if strings.Contains(joined, "--bare") || req.Stdin != probePrompt || req.Dir == "" {
		t.Fatalf("unsafe probe request: %#v", req)
	}
}

func testInventory(binding providerinventory.ClaudeAuthBinding, now time.Time) providerinventory.Report {
	installID := binding.ProviderInstallationID
	accountID := binding.AccountProfileID
	return providerinventory.Report{
		SchemaVersion: providerinventory.ProviderInventoryJSONSchema,
		GeneratedAt:   now.Format(time.RFC3339Nano),
		Confidence:    providerinventory.ConfidenceExact,
		Installations: []providerinventory.ProviderInstallation{{
			ProviderInstallationID: installID,
			AdapterID:              "claude",
			InstallationState:      providerinventory.InstallationInstalled,
		}},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AuthReadinessID:        "auth_inventory",
			AdapterID:              "claude",
			ProviderInstallationID: &installID,
			AccountProfileID:       &accountID,
			ReadinessState:         providerinventory.ReadinessReady,
			ReadinessConfidence:    providerinventory.ConfidenceExact,
			FreshnessState:         providerinventory.FreshnessFresh,
			Confidence:             providerinventory.ConfidenceExact,
		}},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{},
		ModelCapabilities:     []providerinventory.ModelCapability{},
		GapReasons:            []string{},
	}
}

func testClaudeStream() string {
	return strings.Join([]string{
		`{"type":"system","subtype":"init","model":"claude-sonnet-5","apiKeySource":"none","session_id":"provider-session-must-not-persist"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"OK"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"modelUsage":{"claude-sonnet-5":{"inputTokens":2,"outputTokens":4,"cacheReadInputTokens":20,"cacheCreationInputTokens":5,"costUSD":0.000123,"contextWindow":1000000,"maxOutputTokens":64000,"webSearchRequests":0}}}`,
	}, "\n") + "\n"
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
