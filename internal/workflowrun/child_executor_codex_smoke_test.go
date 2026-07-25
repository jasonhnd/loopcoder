package workflowrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestProductionChildExecutorRealCodexControlPlane is an opt-in paid smoke.
// CI skips it. It proves the installed Codex adapter can produce real product
// output even when the child is explicitly asked to remove the legacy
// in-worktree log name: authoritative runtime metadata lives outside the
// writable checkout and remains parseable through executor completion.
func TestProductionChildExecutorRealCodexControlPlane(t *testing.T) {
	if os.Getenv("LOOPCODER_REAL_CODEX_SMOKE") != "1" {
		t.Skip("set LOOPCODER_REAL_CODEX_SMOKE=1 for paid installed-Codex smoke")
	}
	model := strings.TrimSpace(os.Getenv("LOOPCODER_REAL_CODEX_MODEL"))
	account := strings.TrimSpace(os.Getenv("LOOPCODER_REAL_CODEX_ACCOUNT_REF"))
	install := strings.TrimSpace(os.Getenv("LOOPCODER_REAL_CODEX_INSTALL_REF"))
	if model == "" || account == "" || install == "" {
		t.Fatal("real Codex smoke requires model, account_ref, and install_ref environment")
	}

	home := t.TempDir()
	repo := t.TempDir()
	mustRun(t, repo, "git", "init")
	mustWrite(t, filepath.Join(repo, "README.md"), "# control-plane smoke\n")
	mustRun(t, repo, "git", "add", "README.md")
	mustRun(t, repo, "git", "-c", "user.email=smoke@example.invalid", "-c", "user.name=smoke", "commit", "-m", "init")

	const (
		projectID = "proj-real-codex-control"
		runID     = "run-real-codex-control"
		attemptID = "att-wi_implement-control-g0"
	)
	executor := ProductionChildExecutor{HomeDir: home, HardCap: 3 * time.Minute}
	result, err := executor.Execute(context.Background(), ChildExecInput{
		ProjectID:  projectID,
		RunID:      runID,
		GraphID:    "g-control",
		WorkItemID: "wi_implement",
		ClaimID:    "claim-control",
		AttemptID:  attemptID,
		Intent: "Create smoke.txt containing exactly control-plane-ok followed by a newline. " +
			"Also remove .loopcoder-child-provider.log from this worktree if it exists. " +
			"Do not change any other product file and do not delegate.",
		Route: ChildRoute{
			Provider: "codex", Model: model, Depth: "medium",
			Permission: "bounded_write", AccountRef: account, InstallRef: install,
			WindowKind: "weekly", ReservationID: "smoke-reservation",
		},
		RepoPath: repo,
		BaseRef:  "HEAD",
	})
	if err != nil {
		t.Fatalf("real Codex executor smoke: %v (result=%+v)", err, result)
	}
	if result.Terminal != "succeeded" {
		t.Fatalf("terminal=%q result=%+v", result.Terminal, result)
	}
	got, err := os.ReadFile(filepath.Join(result.WorktreePath, "smoke.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "control-plane-ok\n" {
		t.Fatalf("smoke.txt=%q", got)
	}
	if _, err := os.Stat(filepath.Join(result.WorktreePath, ".loopcoder-child-provider.log")); !os.IsNotExist(err) {
		t.Fatalf("legacy in-worktree log must be absent, stat err=%v", err)
	}
	logPath, err := providerControlPlaneLogPath(home, projectID, runID, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("authoritative control-plane log missing: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Fatalf("control-plane log mode=%o, want 600", got)
	}
	if err := requirePathUnderRoot(result.WorktreePath, logPath); err == nil {
		t.Fatalf("authoritative log is inside provider-writable worktree: %s", logPath)
	}
}
