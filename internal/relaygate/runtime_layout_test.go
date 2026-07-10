package relaygate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/registry"
	"github.com/jasonhnd/loopcoder/internal/state"
)

func TestWriteRegisteredRelayUsesGlobalPayloadRoot(t *testing.T) {
	repo := registerRelayRuntimeProject(t)
	layout, err := state.ResolveRuntimeLayout(repo)
	if err != nil {
		t.Fatalf("ResolveRuntimeLayout: %v", err)
	}

	path, err := Write(WriteOptions{
		RepoPath:  repo,
		RunID:     "run-20260710T000000Z-issue-711",
		Role:      "worker",
		PRNumber:  711,
		Block:     "[reporter] role=worker provider=codex model=gpt-5\n",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if filepath.Dir(path) != filepath.Join(layout.RelayRoot, "pending") {
		t.Fatalf("relay path = %s, want under %s", path, layout.RelayRoot)
	}
	if _, err := os.Stat(filepath.Join(repo, ".loopcoder")); !os.IsNotExist(err) {
		t.Fatalf("registered relay write created repo-local .loopcoder: %v", err)
	}
}

func TestCheckRegisteredRelayFallsBackToLegacyPending(t *testing.T) {
	repo := registerRelayRuntimeProject(t)
	legacyDir := filepath.Join(repo, ".loopcoder", "relay", "pending")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatalf("mkdir legacy pending: %v", err)
	}
	nonce := Nonce("run-legacy", 12, "worker")
	data := []byte(`{"version":1,"nonce":"` + nonce + `","run_id":"run-legacy","role":"worker","pr_number":12,"block":"legacy block\n"}` + "\n")
	if err := os.WriteFile(filepath.Join(legacyDir, nonce+".json"), data, 0o600); err != nil {
		t.Fatalf("write legacy pending: %v", err)
	}

	records, err := CheckWithError(repo)
	if err != nil {
		t.Fatalf("CheckWithError: %v", err)
	}
	if len(records) != 1 || records[0].Nonce != nonce {
		t.Fatalf("records = %#v, want legacy pending fallback", records)
	}
}

func registerRelayRuntimeProject(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	t.Setenv(home.EnvHome, filepath.Join(t.TempDir(), "home"))
	if _, err := registry.Register(context.Background(), registry.Options{
		RepoPath: repo,
		Now: func() time.Time {
			return time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
		},
	}, registry.DefaultDeps()); err != nil {
		t.Fatalf("registry.Register: %v", err)
	}
	return repo
}
