package acceptharness_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/acceptharness"
)

func TestGoldenScenarioDocsOnly(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	clock := acceptharness.NewManualClock(time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC))
	res, err := acceptharness.RunGoldenScenario(ctx, acceptharness.ScenarioOptions{
		ID:       "golden-docs",
		RepoKind: acceptharness.RepoDocsOnly,
		Clock:    clock,
		WorkDir:  dir,
	})
	if err != nil {
		t.Fatalf("RunGoldenScenario: %v", err)
	}
	if res.CommitSHA == "" || res.PR.Number == 0 {
		t.Fatalf("missing commit/pr: %#v", res)
	}
	if len(res.LivePIDs) != 0 {
		t.Fatalf("surviving children: %v", res.LivePIDs)
	}
	assertOrdered(t, res.Events,
		"policy.frozen:",
		"repo.created:docs-only",
		"provider.completed:",
		"git.commit:",
		"github.pr_opened:",
		"github.checks_green",
		"ui.delivered:1",
		"ui.acked:rendered",
		"process.cleanup_complete",
	)
	if res.Manifest.TestedSHA != res.CommitSHA {
		t.Fatalf("manifest sha mismatch")
	}
	if res.Manifest.PolicyDigest == "" || res.Manifest.PolicyDigest != res.PolicyDigest {
		t.Fatalf("policy digest missing")
	}
	if err := res.Manifest.ValidateNoLeakage(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(res.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var m acceptharness.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.SchemaVersion != acceptharness.ManifestSchema {
		t.Fatalf("schema = %s", m.SchemaVersion)
	}
}

func TestGoldenScenarioSmallGo(t *testing.T) {
	res, err := acceptharness.RunGoldenScenario(context.Background(), acceptharness.ScenarioOptions{
		RepoKind: acceptharness.RepoSmallGo,
		WorkDir:  t.TempDir(),
		Clock:    acceptharness.NewManualClock(time.Time{}),
	})
	if err != nil {
		t.Fatalf("RunGoldenScenario: %v", err)
	}
	if res.Manifest.RepoKind != acceptharness.RepoSmallGo {
		t.Fatalf("kind = %s", res.Manifest.RepoKind)
	}
}

func TestFaultPushTimeoutResume(t *testing.T) {
	res, err := acceptharness.RunFaultScenario(context.Background(), acceptharness.ScenarioOptions{
		WorkDir: t.TempDir(),
		Clock:   acceptharness.NewManualClock(time.Time{}),
		Failure: acceptharness.FailurePlan{Point: acceptharness.FailPushTimeout, Resume: true},
	})
	if err != nil {
		t.Fatalf("fault: %v", err)
	}
	assertContains(t, res.Events, "github.push_timeout")
	assertContains(t, res.Events, "github.pr_resumed")
	if res.PR.Number == 0 {
		t.Fatal("expected PR after resume")
	}
}

func TestFaultUIDisconnectResume(t *testing.T) {
	res, err := acceptharness.RunFaultScenario(context.Background(), acceptharness.ScenarioOptions{
		WorkDir: t.TempDir(),
		Clock:   acceptharness.NewManualClock(time.Time{}),
		Failure: acceptharness.FailurePlan{Point: acceptharness.FailUIDisconnect, Resume: true},
	})
	if err != nil {
		t.Fatalf("fault: %v", err)
	}
	assertContains(t, res.Events, "ui.disconnect")
	assertContains(t, res.Events, "ui.reconnect")
	assertContains(t, res.Events, "ui.acked:rendered")
}

func TestFaultProviderNonZeroResume(t *testing.T) {
	res, err := acceptharness.RunFaultScenario(context.Background(), acceptharness.ScenarioOptions{
		WorkDir: t.TempDir(),
		Clock:   acceptharness.NewManualClock(time.Time{}),
		Failure: acceptharness.FailurePlan{Point: acceptharness.FailProviderExit, Resume: true},
	})
	if err != nil {
		t.Fatalf("fault: %v", err)
	}
	assertContains(t, res.Events, "provider.exit:7")
	assertContains(t, res.Events, "provider.resumed_complete")
}

func TestFaultDuplicatePRIdempotent(t *testing.T) {
	res, err := acceptharness.RunFaultScenario(context.Background(), acceptharness.ScenarioOptions{
		WorkDir: t.TempDir(),
		Clock:   acceptharness.NewManualClock(time.Time{}),
		Failure: acceptharness.FailurePlan{Point: acceptharness.FailDuplicatePR, Resume: true},
	})
	if err != nil {
		t.Fatalf("fault: %v", err)
	}
	assertContains(t, res.Events, "github.pr_duplicate_idempotent")
}

func TestProviderModesAndZeroChildren(t *testing.T) {
	dir := t.TempDir()
	obs := acceptharness.NewProcessObserver()
	// ensure helper builds once
	p := &acceptharness.FakeProvider{WorkDir: dir, Mode: acceptharness.ProviderSpawnChild, Observer: obs}
	res, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("spawn_child: %v", err)
	}
	if len(res.Children) != 1 {
		t.Fatalf("children = %v", res.Children)
	}
	if live := obs.LivePIDs(); len(live) != 0 {
		t.Fatalf("live pids after spawn_child: %v", live)
	}

	for _, mode := range []acceptharness.ProviderMode{
		acceptharness.ProviderSilent,
		acceptharness.ProviderEmitOutput,
		acceptharness.ProviderFlood,
		acceptharness.ProviderNonZero,
	} {
		fp := &acceptharness.FakeProvider{WorkDir: dir, Mode: mode, Observer: obs}
		got, err := fp.Run(context.Background())
		if mode == acceptharness.ProviderNonZero {
			if err != nil {
				t.Fatalf("nonzero returned err %v", err)
			}
			if got.ExitCode != 7 {
				t.Fatalf("exit = %d", got.ExitCode)
			}
			continue
		}
		if err != nil {
			t.Fatalf("mode %s: %v", mode, err)
		}
	}

	// hang cancelled via context — no wall-clock sleep in test logic
	hctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		hp := &acceptharness.FakeProvider{WorkDir: dir, Mode: acceptharness.ProviderHang, Observer: obs}
		_, err := hp.Run(hctx)
		done <- err
	}()
	// explicit barrier-style cancel without sleeping for correctness
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancel error from hang mode")
		}
	case <-time.After(10 * time.Second):
		// generous upper bound only for stuck helper infrastructure, not scenario logic
		t.Fatal("hang mode did not observe cancellation")
	}
	if live := obs.LivePIDs(); len(live) != 0 {
		t.Fatalf("live pids after hang cancel: %v", live)
	}
}

func TestBarrierAndManualClock(t *testing.T) {
	clock := acceptharness.NewManualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	start := clock.Now()
	clock.Advance(5 * time.Minute)
	if !clock.Now().Equal(start.Add(5 * time.Minute)) {
		t.Fatalf("clock advance failed")
	}
	b := acceptharness.NewBarrier()
	released := make(chan struct{})
	go func() {
		_ = b.Wait(context.Background())
		close(released)
	}()
	b.Release()
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("barrier did not release")
	}
}

func TestCleanProcessEnvStripsSecretsAndGit(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"GIT_DIR=/tmp/evil",
		"GITHUB_TOKEN=ghp_synthetictokenvalue99",
		"OPENAI_API_KEY=sk-synthetic",
		"LANG=C",
	}
	out := strings.Join(acceptharness.CleanProcessEnv(env), "\n")
	for _, bad := range []string{"GIT_DIR=", "GITHUB_TOKEN=", "OPENAI_API_KEY="} {
		if strings.Contains(out, bad) {
			t.Fatalf("env still contains %s: %s", bad, out)
		}
	}
	if !strings.Contains(out, "PATH=/usr/bin") {
		t.Fatalf("lost PATH: %s", out)
	}
}

func TestManifestRejectsLeakage(t *testing.T) {
	m := acceptharness.Manifest{
		SchemaVersion: acceptharness.ManifestSchema,
		ScenarioID:    "x",
		TestedSHA:     "abc",
		Inputs:        map[string]string{"path": "/Users/someone/secret"},
	}
	if err := m.ValidateNoLeakage(); err == nil {
		t.Fatal("expected leakage error")
	}
}

func TestRepeatedGoldenIsStableShape(t *testing.T) {
	// Run twice to catch flaky process cleanup without depending on wall sleeps.
	for i := 0; i < 2; i++ {
		res, err := acceptharness.RunGoldenScenario(context.Background(), acceptharness.ScenarioOptions{
			WorkDir: filepath.Join(t.TempDir(), "run"),
			Clock:   acceptharness.NewManualClock(time.Time{}),
		})
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if len(res.LivePIDs) != 0 {
			t.Fatalf("run %d live pids %v", i, res.LivePIDs)
		}
	}
}

func assertOrdered(t *testing.T, events []string, prefixes ...string) {
	t.Helper()
	idx := 0
	for _, prefix := range prefixes {
		found := false
		for idx < len(events) {
			if strings.HasPrefix(events[idx], prefix) || strings.Contains(events[idx], prefix) {
				found = true
				idx++
				break
			}
			idx++
		}
		if !found {
			t.Fatalf("missing event prefix %q in %v", prefix, events)
		}
	}
}

func assertContains(t *testing.T, events []string, needle string) {
	t.Helper()
	for _, e := range events {
		if strings.Contains(e, needle) {
			return
		}
	}
	t.Fatalf("missing %q in %v", needle, events)
}
