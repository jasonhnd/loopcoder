package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/claudecatalog"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

func TestProvidersVerifyClaudeModelUsesIdentityOnlyAndExplicitPaidDependency(t *testing.T) {
	repo := t.TempDir()
	now := time.Date(2026, 7, 26, 5, 0, 0, 0, time.UTC)
	inventory := providerinventory.Report{
		SchemaVersion:         providerinventory.ProviderInventoryJSONSchema,
		GeneratedAt:           now.Format(time.RFC3339Nano),
		InventoryFingerprint:  "sha256:identity",
		Installations:         []providerinventory.ProviderInstallation{},
		AuthReadiness:         []providerinventory.AuthReadiness{},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{},
		ModelCapabilities:     []providerinventory.ModelCapability{},
	}
	verifyCalled := false
	var stdout, stderr bytes.Buffer
	code := RunWithDeps([]string{
		"providers", "verify-claude-model",
		"--repo", repo,
		"--project-id", "proj_cli",
		"--delivery-run-id", "run_cli",
		"--model", "sonnet",
		"--effort", "low",
		"--account-ref", "acct_cli",
		"--install-ref", "pinst_cli",
		"--timeout", "45s",
		"--reserve-tokens", "4096",
	}, &stdout, &stderr, Deps{
		Now: func() time.Time { return now },
		ProviderInventory: func(_ context.Context, opts providerinventory.Options) (providerinventory.Report, error) {
			if !opts.IdentityOnly || len(opts.ActiveProviders) != 1 || opts.ActiveProviders[0] != "claude" {
				t.Fatalf("inventory options = %#v", opts)
			}
			return inventory, nil
		},
		ClaudeCatalogVerify: func(_ context.Context, req claudecatalog.Request) (claudecatalog.Result, error) {
			verifyCalled = true
			if req.ProjectID != "proj_cli" || req.DeliveryRunID != "run_cli" ||
				req.Model != "sonnet" || req.Effort != "low" ||
				req.AccountRef != "acct_cli" || req.InstallRef != "pinst_cli" ||
				req.Timeout != 45*time.Second || req.ReservedTokens != 4096 ||
				req.Inventory.InventoryFingerprint != inventory.InventoryFingerprint {
				t.Fatalf("verify request = %#v", req)
			}
			return claudecatalog.Result{
				Receipt: providerinventory.ClaudeCapabilityProbeReceipt{
					RequestedModel:         "sonnet",
					ActualModel:            "claude-sonnet-5",
					AcceptedEffort:         "low",
					ProviderInstallationID: "pinst_cli",
					AccountProfileID:       "acct_cli",
					ReservedTokens:         4096,
					CommittedTokens:        31,
					ReleasedTokens:         4065,
					BudgetState:            "released",
					FreshnessState:         providerinventory.FreshnessFresh,
					ExpiresAt:              now.Add(30 * time.Minute).Format(time.RFC3339Nano),
				},
				Report: providerinventory.Report{InventoryFingerprint: "sha256:verified"},
			}, nil
		},
	})
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if !verifyCalled {
		t.Fatal("ClaudeCatalogVerify was not called")
	}
	for _, want := range []string{"requested=sonnet", "actual=claude-sonnet-5", "tokens_reserved=4096", "tokens_actual=31", "raw prompt/result/session/principal/credential material was not retained"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout.String())
		}
	}
}
