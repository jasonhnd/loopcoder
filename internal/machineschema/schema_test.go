package machineschema_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/machineschema"
)

func fixedNow() time.Time {
	return time.Date(2026, 7, 21, 17, 0, 0, 0, time.UTC)
}

func TestMachineSchemaInstallAndReservation(t *testing.T) {
	ctx := context.Background()
	layout, err := home.EnsureMinimumLayout(filepath.Join(t.TempDir(), "home"), "")
	if err != nil {
		t.Fatal(err)
	}
	ms, err := layout.OpenMachine(ctx, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	defer ms.Close()

	if err := machineschema.Ensure(ctx, ms); err != nil {
		t.Fatal(err)
	}
	if err := machineschema.PutInstallation(ctx, ms, machineschema.Observation{
		ProviderKey: "codex",
		InstallPath: "/opt/synthetic/codex",
		Version:     "1.0.0",
		Source:      "fixture",
		ObservedAt:  fixedNow(),
		Freshness:   "fresh",
		Confidence:  "high",
		Digest:      "sha256:abc",
	}); err != nil {
		t.Fatal(err)
	}
	// Idempotent rewrite
	if err := machineschema.PutInstallation(ctx, ms, machineschema.Observation{
		ProviderKey: "codex",
		InstallPath: "/opt/synthetic/codex",
		Version:     "1.0.1",
		Source:      "fixture",
		ObservedAt:  fixedNow().Add(time.Minute),
		Freshness:   "fresh",
		Confidence:  "high",
		Digest:      "sha256:def",
	}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := machineschema.GetInstallation(ctx, ms, "codex")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Version != "1.0.1" {
		t.Fatalf("version = %s", got.Version)
	}

	if err := machineschema.PutReservation(ctx, ms, machineschema.Reservation{
		ID:         "res-1",
		Owner:      "attempt-1",
		BudgetKind: "process",
		Units:      1,
		State:      "active",
		CreatedAt:  fixedNow(),
		UpdatedAt:  fixedNow(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := machineschema.AssertNoProjectDomainColumns(ctx, ms); err != nil {
		t.Fatal(err)
	}
	// Forbidden credential-shaped content
	if err := machineschema.PutInstallation(ctx, ms, machineschema.Observation{
		ProviderKey: "x",
		Source:      "sk-ant-not-allowed",
		ObservedAt:  fixedNow(),
	}); err == nil {
		t.Fatal("expected forbidden content rejection")
	}

	// Reopen
	_ = ms.Close()
	ms2, err := layout.OpenMachine(ctx, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	defer ms2.Close()
	got2, ok, err := machineschema.GetInstallation(ctx, ms2, "codex")
	if err != nil || !ok || got2.Version != "1.0.1" {
		t.Fatalf("reopen get: %#v ok=%v err=%v", got2, ok, err)
	}
}
