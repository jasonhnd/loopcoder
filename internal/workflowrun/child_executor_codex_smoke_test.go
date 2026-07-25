package workflowrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/agent"
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

// TestProductionChildExecutorRealCodexUnavailableProbe is an opt-in installed
// Codex smoke for the release-canary boundary. It spends no successful model
// tokens: the same-account CLI must reject the declared-only model before any
// assistant turn, and the executor must retain only redacted attempted-route
// evidence for the scheduler's reserved-capacity reconciliation.
func TestProductionChildExecutorRealCodexUnavailableProbe(t *testing.T) {
	if os.Getenv("LOOPCODER_REAL_UNAVAILABLE_PROBE") != "1" {
		t.Skip("set LOOPCODER_REAL_UNAVAILABLE_PROBE=1 for installed-Codex unavailable smoke")
	}
	model := strings.TrimSpace(os.Getenv("LOOPCODER_REAL_UNAVAILABLE_MODEL"))
	account := strings.TrimSpace(os.Getenv("LOOPCODER_REAL_CODEX_ACCOUNT_REF"))
	install := strings.TrimSpace(os.Getenv("LOOPCODER_REAL_CODEX_INSTALL_REF"))
	if model == "" || account == "" || install == "" {
		t.Fatal("real unavailable probe requires model, account_ref, and install_ref environment")
	}

	home := t.TempDir()
	repo := t.TempDir()
	mustRun(t, repo, "git", "init")
	mustWrite(t, filepath.Join(repo, "README.md"), "# unavailable probe smoke\n")
	mustRun(t, repo, "git", "add", "README.md")
	mustRun(t, repo, "git", "-c", "user.email=smoke@example.invalid", "-c", "user.name=smoke", "commit", "-m", "init")

	const (
		projectID = "proj-real-codex-unavailable"
		runID     = "run-real-codex-unavailable"
		attemptID = "att-wi_research-unavailable-g0"
	)
	executor := ProductionChildExecutor{HomeDir: home, HardCap: time.Minute}
	result, err := executor.Execute(context.Background(), ChildExecInput{
		ProjectID:  projectID,
		RunID:      runID,
		GraphID:    "g-unavailable",
		WorkItemID: "wi_research",
		ClaimID:    "claim-unavailable",
		AttemptID:  attemptID,
		Intent:     "this product prompt must not reach the provider",
		Route: ChildRoute{
			Provider: "codex", Model: model, Depth: "low",
			Permission: "read-only", AccountRef: account, InstallRef: install,
			WindowKind: "weekly", ReservationID: "smoke-unavailable-reservation",
			CapabilityProbeOnly: true,
		},
		RepoPath: repo,
		BaseRef:  "HEAD",
		ReadOnly: true,
	})
	if result.Terminal != "failed" || result.FailureClass != "model_unavailable" {
		t.Fatalf("real unavailable probe not typed exactly: err=%v result=%+v", err, result)
	}
	if result.InvokedRoute.Provider != "codex" ||
		result.InvokedRoute.Model != model ||
		result.InvokedRoute.Depth != "low" ||
		result.InvokedRoute.Permission != "read-only" ||
		result.InvokedRoute.AccountRef != account ||
		result.InvokedRoute.InstallRef != install ||
		!result.InvokedRoute.CapabilityProbeOnly {
		t.Fatalf("attempted route binding mismatch: %+v", result.InvokedRoute)
	}
	if result.ActualSources.Model != agent.ActualSourceAttemptedInvocation ||
		result.ActualSources.Effort != agent.ActualSourceAttemptedInvocation ||
		result.ActualSources.Permission != agent.ActualSourceAttemptedInvocation ||
		result.ActualSources.Account != agent.ActualSourceAuthBinding ||
		result.ActualSources.Install != agent.ActualSourceInstallBinding {
		t.Fatalf("attempted route sources mismatch: %+v", result.ActualSources)
	}
	if !strings.HasPrefix(result.ArgvDigest, "sha256:") {
		t.Fatalf("missing redacted argv digest: %q", result.ArgvDigest)
	}
	if result.InputTokens != 0 || result.OutputTokens != 0 {
		t.Fatalf("rejected pre-turn probe reported token usage: input=%d output=%d", result.InputTokens, result.OutputTokens)
	}

	logPath, pathErr := providerControlPlaneLogPath(home, projectID, runID, attemptID)
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	logBytes, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	lower := strings.ToLower(string(logBytes))
	for _, forbidden := range []string{
		"access_token", "refresh_token", "api_key", "authorization:", "bearer ",
		"this product prompt must not reach the provider",
	} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("control-plane log retained forbidden material %q", forbidden)
		}
	}
}
