package directcanary_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/acceptharness"
	"github.com/jasonhnd/loopcoder/internal/directcanary"
)

func TestGoldenDocsOnly(t *testing.T) {
	res, err := directcanary.Run(context.Background(), directcanary.Options{
		ID: "golden-docs", RepoKind: acceptharness.RepoDocsOnly, WorkDir: t.TempDir(),
		Now: func() time.Time { return time.Date(2026, 7, 22, 15, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertGolden(t, res, acceptharness.RepoDocsOnly)
}

func TestGoldenSmallGo(t *testing.T) {
	res, err := directcanary.Run(context.Background(), directcanary.Options{
		ID: "golden-go", RepoKind: acceptharness.RepoSmallGo, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertGolden(t, res, acceptharness.RepoSmallGo)
}

func assertGolden(t *testing.T, res directcanary.Result, kind acceptharness.RepoKind) {
	t.Helper()
	if res.PRNumber == 0 || res.CommitSHA == "" {
		t.Fatalf("missing pr/commit: %+v", res)
	}
	if res.WorkerLaunches != 1 {
		t.Fatalf("worker launches=%d", res.WorkerLaunches)
	}
	if !res.Manifest.RouteMatch {
		t.Fatalf("route mismatch: req=%s act=%s", res.Manifest.RequestedRoute, res.Manifest.ActualRoute)
	}
	if res.Manifest.ProviderCallsCI != 0 {
		t.Fatalf("ci provider calls=%d", res.Manifest.ProviderCallsCI)
	}
	if !res.Manifest.VerifierAfterReady {
		t.Fatal("verifier must run after CI ready")
	}
	if res.Manifest.HumanGate != "approve_merge" {
		t.Fatalf("human gate=%s", res.Manifest.HumanGate)
	}
	if len(res.Manifest.Residue) != 0 {
		t.Fatalf("residue=%v", res.Manifest.Residue)
	}
	if res.Manifest.RepoKind != string(kind) {
		t.Fatalf("kind=%s", res.Manifest.RepoKind)
	}
	assertOrdered(t, res.Events,
		"repo.created:",
		"preflight.allow_launch",
		"intake.frozen:",
		"routepin.ready:",
		"wtclaim.ok",
		"report.start_rendered:term+uibridge",
		"worker.launch_count:1",
		"worker.cleanup_terminal",
		"localverify.ok:",
		"commit.ok",
		"hookpolicy.ok",
		"push.ok",
		"pr.opened:",
		"ci.ready",
		"verifier.blocked_before_ci",
		"verifier.pass",
		"human_gate.approve_merge",
		"residue.clean",
	)
	if err := res.Manifest.ValidateNoLeakage(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(res.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var m directcanary.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.SchemaVersion != directcanary.ManifestSchema {
		t.Fatalf("schema=%s", m.SchemaVersion)
	}
	if m.TestedSHA != res.CommitSHA {
		t.Fatal("manifest sha mismatch")
	}
}

func TestPushTimeoutResumeNoWorkerReplay(t *testing.T) {
	res, err := directcanary.Run(context.Background(), directcanary.Options{
		ID: "push-timeout", RepoKind: acceptharness.RepoDocsOnly,
		Fault: directcanary.FaultPushTimeout, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.WorkerLaunches != 1 {
		t.Fatalf("launches=%d", res.WorkerLaunches)
	}
	assertHas(t, res.Events, "push.timeout_reconciled", "push.adopted_after_timeout", "deliveryresume.plan:", "push.idempotent_adopt")
	// success path after resume still reaches human gate
	if res.Manifest.HumanGate != "approve_merge" {
		t.Fatalf("gate=%s", res.Manifest.HumanGate)
	}
}

func TestDeliveryResumeTerminal(t *testing.T) {
	res, err := directcanary.Run(context.Background(), directcanary.Options{
		Fault: directcanary.FaultDeliveryResume, WorkDir: t.TempDir(),
		RepoKind: acceptharness.RepoSmallGo,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.WorkerLaunches != 1 {
		t.Fatalf("launches=%d", res.WorkerLaunches)
	}
	assertHas(t, res.Events, "deliveryresume.terminal_done")
}

func TestWorkerFailSingleLaunch(t *testing.T) {
	res, err := directcanary.Run(context.Background(), directcanary.Options{
		Fault: directcanary.FaultWorkerFail, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.WorkerLaunches != 1 {
		t.Fatalf("launches=%d", res.WorkerLaunches)
	}
	if res.Manifest.HumanGate != "blocked_worker_fail" {
		t.Fatalf("gate=%s", res.Manifest.HumanGate)
	}
	assertHas(t, res.Events, "worker.failed")
}

func TestCancelCleanup(t *testing.T) {
	res, err := directcanary.Run(context.Background(), directcanary.Options{
		Fault: directcanary.FaultCancel, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertHas(t, res.Events, "cancel.cleanup", "reservation.released")
	if len(res.LivePIDs) != 0 {
		t.Fatalf("live pids=%v", res.LivePIDs)
	}
	if len(res.Manifest.Residue) != 0 {
		t.Fatalf("residue=%v", res.Manifest.Residue)
	}
}

func TestUIReconnect(t *testing.T) {
	res, err := directcanary.Run(context.Background(), directcanary.Options{
		Fault: directcanary.FaultUIReconnect, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertHas(t, res.Events, "ui.disconnect", "ui.reconnect", "human_gate.approve_merge")
	if res.WorkerLaunches != 1 {
		t.Fatalf("launches=%d", res.WorkerLaunches)
	}
}

func TestChangedHead(t *testing.T) {
	res, err := directcanary.Run(context.Background(), directcanary.Options{
		Fault: directcanary.FaultChangedHead, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// either explicit changed_head class or rebind path
	found := false
	for _, e := range res.Events {
		if strings.Contains(e, "changed_head") || strings.Contains(e, "stale") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected changed-head evidence in %v", res.Events)
	}
	if res.Manifest.HumanGate != "approve_merge" {
		t.Fatalf("gate=%s", res.Manifest.HumanGate)
	}
}

func TestScanResidueFindsMarker(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/README.md", []byte("ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir+"/.loopcoder", 0o700); err != nil {
		t.Fatal(err)
	}
	hits, err := directcanary.ScanResidue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("expected residue hit")
	}
}

func TestManifestRejectsLeakage(t *testing.T) {
	m := directcanary.Manifest{
		SchemaVersion: directcanary.ManifestSchema,
		Events:        []string{"path=/Users/secret/work"},
	}
	if err := m.ValidateNoLeakage(); err == nil {
		t.Fatal("expected leakage error")
	}
}

func assertOrdered(t *testing.T, events []string, prefixes ...string) {
	t.Helper()
	i := 0
	for _, p := range prefixes {
		found := false
		for ; i < len(events); i++ {
			if strings.HasPrefix(events[i], p) || strings.Contains(events[i], p) {
				found = true
				i++
				break
			}
		}
		if !found {
			t.Fatalf("missing event prefix %q in %v", p, events)
		}
	}
}

func assertHas(t *testing.T, events []string, want ...string) {
	t.Helper()
	for _, w := range want {
		ok := false
		for _, e := range events {
			if strings.Contains(e, w) || strings.HasPrefix(e, w) {
				ok = true
				break
			}
		}
		if !ok {
			t.Fatalf("missing %q in %v", w, events)
		}
	}
}
