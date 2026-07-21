package silentcanary_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/silentcanary"
)

func TestComplete12mMultiUI(t *testing.T) {
	res, err := silentcanary.Run(context.Background(), silentcanary.Options{
		ID: "complete", Variant: silentcanary.VariantComplete, WorkDir: t.TempDir(),
		TestedSHA: "sha-complete",
		Start:     time.Date(2026, 7, 22, 17, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ProviderCalls != 1 {
		t.Fatalf("provider calls=%d", res.ProviderCalls)
	}
	if res.Manifest.WorkerRestarts != 0 || res.Manifest.SurvivingChildren != 0 {
		t.Fatalf("cleanup: %+v", res.Manifest)
	}
	if res.Manifest.ReservationHeld {
		t.Fatal("reservation held")
	}
	if !res.Manifest.DigestParity {
		t.Fatalf("digest parity: %v", res.Manifest.ClientDigests)
	}
	if len(res.Manifest.ClientDigests) < 3 {
		t.Fatalf("clients=%v", res.Manifest.ClientDigests)
	}
	assertHas(t, res.Events,
		"scheduler.provider_free",
		"worker.silent_launched",
		"report.start",
		"report.five_minute",
		"report.ten_minute",
		"report.terminal:completed",
		"reservation.released",
	)
	// kinds include start, periodic, terminal
	joined := strings.Join(res.ReportKinds, ",")
	for _, k := range []string{"start", "periodic", "terminal"} {
		if !strings.Contains(joined, k) {
			t.Fatalf("missing kind %s in %v", k, res.ReportKinds)
		}
	}
	if err := res.Manifest.ValidateNoLeakage(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(res.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var m silentcanary.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.SchemaVersion != silentcanary.ManifestSchema {
		t.Fatalf("schema=%s", m.SchemaVersion)
	}
	if m.TestedSHA != "sha-complete" {
		t.Fatalf("sha=%s", m.TestedSHA)
	}
	if m.ProviderCalls != 1 {
		t.Fatal("provider calls in manifest")
	}
}

func TestCancelCleanup(t *testing.T) {
	res, err := silentcanary.Run(context.Background(), silentcanary.Options{
		Variant: silentcanary.VariantCancel, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertHas(t, res.Events, "report.terminal:cancelled", "reservation.released")
	if res.Manifest.SurvivingChildren != 0 {
		t.Fatalf("live=%d", res.Manifest.SurvivingChildren)
	}
}

func TestUIReconnectNoWorkerRestart(t *testing.T) {
	res, err := silentcanary.Run(context.Background(), silentcanary.Options{
		Variant: silentcanary.VariantUIReconnect, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertHas(t, res.Events, "ui.disconnect:term", "ui.reconnect:term", "report.terminal:completed")
	if res.Manifest.WorkerRestarts != 0 {
		t.Fatal("worker restart")
	}
	if res.ProviderCalls != 1 {
		t.Fatalf("calls=%d", res.ProviderCalls)
	}
}

func TestCoreRestart(t *testing.T) {
	res, err := silentcanary.Run(context.Background(), silentcanary.Options{
		Variant: silentcanary.VariantCoreRestart, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertHas(t, res.Events, "core.restart:scheduler_restored", "report.terminal:completed")
	if res.ProviderCalls != 1 || res.Manifest.WorkerRestarts != 0 {
		t.Fatalf("calls=%d restarts=%d", res.ProviderCalls, res.Manifest.WorkerRestarts)
	}
}

func TestRequiredClientOutage(t *testing.T) {
	res, err := silentcanary.Run(context.Background(), silentcanary.Options{
		Variant: silentcanary.VariantRequiredOutage, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertHas(t, res.Events, "required_client.restored", "report.terminal:completed")
}

func TestResourceBreach(t *testing.T) {
	res, err := silentcanary.Run(context.Background(), silentcanary.Options{
		Variant: silentcanary.VariantResourceBreach, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertHas(t, res.Events, "report.blocker:resource_breach", "reservation.released")
	if res.Manifest.ResourceState != "breach" {
		t.Fatalf("resource=%s", res.Manifest.ResourceState)
	}
}

func TestAmbiguousChildAttention(t *testing.T) {
	res, err := silentcanary.Run(context.Background(), silentcanary.Options{
		Variant: silentcanary.VariantAmbiguousChild, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Either attention path or clean cancel after join — both ok if zero survivors
	if res.Manifest.SurvivingChildren != 0 {
		t.Fatalf("survivors=%d", res.Manifest.SurvivingChildren)
	}
	assertHas(t, res.Events, "reservation.released")
}

func TestManifestRejectsMachineID(t *testing.T) {
	m := silentcanary.Manifest{
		SchemaVersion: silentcanary.ManifestSchema,
		Events:        []string{"hostname=secret-mac.local"},
	}
	if err := m.ValidateNoLeakage(); err == nil {
		t.Fatal("expected machine id rejection")
	}
}

func assertHas(t *testing.T, events []string, want ...string) {
	t.Helper()
	for _, w := range want {
		ok := false
		for _, e := range events {
			if strings.Contains(e, w) {
				ok = true
				break
			}
		}
		if !ok {
			t.Fatalf("missing %q in %v", w, events)
		}
	}
}
