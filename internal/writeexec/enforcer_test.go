package writeexec

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/gitutil"
)

func TestSessionAllowsOnlyDeclaredFileAndDirectoryChanges(t *testing.T) {
	t.Run("single file with dirty parent", func(t *testing.T) {
		repo := initWriteRepository(t)
		mustWrite(t, filepath.Join(repo, "parent-dirt.txt"), "user dirt\n")
		opts := writeOptions(t, repo, filepath.Join(repo, "tracked.txt"))
		session, _, err := Begin(context.Background(), opts)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		mustWrite(t, filepath.Join(opts.WorktreePath, "tracked.txt"), "bounded change\n")
		audit, err := session.Finish(context.Background(), "provider-completed")
		if err != nil || audit.Verification != VerificationPassed || len(audit.Changes) != 1 || audit.Changes[0].Path != "tracked.txt" {
			t.Fatalf("Finish audit=%#v error=%v", audit, err)
		}
		assertFile(t, filepath.Join(repo, "tracked.txt"), "baseline\n")
		assertFile(t, filepath.Join(repo, "parent-dirt.txt"), "user dirt\n")
		reconciled, err := ReconcileVerified(context.Background(), opts)
		if err != nil || reconciled.ManifestFingerprint != audit.ManifestFingerprint {
			t.Fatalf("ReconcileVerified audit=%#v error=%v", reconciled, err)
		}
	})

	t.Run("directory", func(t *testing.T) {
		repo := initWriteRepository(t)
		dir := filepath.Join(repo, "docs")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll docs: %v", err)
		}
		mustWrite(t, filepath.Join(dir, "existing.md"), "baseline\n")
		runGitTest(t, repo, "add", "docs/existing.md")
		runGitTest(t, repo, "commit", "-m", "add docs")
		opts := writeOptions(t, repo, dir)
		session, _, err := Begin(context.Background(), opts)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		mustWrite(t, filepath.Join(opts.WorktreePath, "docs", "new.md"), "new\n")
		audit, err := session.Finish(context.Background(), "provider-completed")
		if err != nil || audit.Verification != VerificationPassed || !hasChange(audit, "docs/new.md") {
			t.Fatalf("Finish audit=%#v error=%v", audit, err)
		}
	})
}

func TestSessionFailsClosedOnOutOfScopeAndGitAuthorityMutations(t *testing.T) {
	tests := []struct {
		name string
		code string
		run  func(t *testing.T, repo string, opts Options)
	}{
		{name: "out of scope", code: "out_of_scope_mutation", run: func(t *testing.T, _ string, opts Options) {
			mustWrite(t, filepath.Join(opts.WorktreePath, "outside.txt"), "outside\n")
		}},
		{name: "parent checkout", code: "checkout_state_modified", run: func(t *testing.T, repo string, _ Options) {
			mustWrite(t, filepath.Join(repo, "tracked.txt"), "parent changed\n")
		}},
		{name: "index", code: "git_index_modified", run: func(t *testing.T, _ string, opts Options) {
			mustWrite(t, filepath.Join(opts.WorktreePath, "tracked.txt"), "allowed but staged\n")
			runGitTest(t, opts.WorktreePath, "add", "tracked.txt")
		}},
		{name: "ref", code: "git_refs_modified", run: func(t *testing.T, _ string, opts Options) {
			runGitTest(t, opts.WorktreePath, "tag", "forbidden-tag")
		}},
		{name: "config", code: "git_config_modified", run: func(t *testing.T, _ string, opts Options) {
			runGitTest(t, opts.WorktreePath, "config", "bounded.fixture", "forbidden")
		}},
		{name: "remote", code: "git_config_modified", run: func(t *testing.T, _ string, opts Options) {
			runGitTest(t, opts.WorktreePath, "remote", "add", "forbidden", "https://example.invalid/repo.git")
		}},
		{name: "hook", code: "git_hooks_modified", run: func(t *testing.T, repo string, _ Options) {
			commonDir := strings.TrimSpace(runGitTest(t, repo, "rev-parse", "--path-format=absolute", "--git-common-dir"))
			mustWrite(t, filepath.Join(commonDir, "hooks", "pre-push"), "#!/bin/sh\nexit 1\n")
		}},
		{name: "linked worktree marker", code: "git_worktree_metadata_modified", run: func(t *testing.T, _ string, opts Options) {
			marker := filepath.Join(opts.WorktreePath, ".git")
			data, err := os.ReadFile(marker)
			if err != nil {
				t.Fatalf("read linked worktree marker: %v", err)
			}
			mustWrite(t, marker, string(data)+"\n")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := initWriteRepository(t)
			opts := writeOptions(t, repo, filepath.Join(repo, "tracked.txt"))
			session, _, err := Begin(context.Background(), opts)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			test.run(t, repo, opts)
			audit, err := session.Finish(context.Background(), "provider-completed")
			var violation *PolicyViolationError
			if !errors.As(err, &violation) || audit.Verification != VerificationViolation || !hasViolation(audit, test.code) {
				t.Fatalf("Finish audit=%#v error=%v, want %s", audit, err, test.code)
			}
		})
	}
}

func TestDirtyParentDirectoryCannotWidenAbsentFileScope(t *testing.T) {
	repo := initWriteRepository(t)
	allowed := filepath.Join(repo, "future")
	mustWrite(t, filepath.Join(allowed, "user-dirt.txt"), "parent only\n")
	opts := writeOptions(t, repo, allowed)
	session, _, err := Begin(context.Background(), opts)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	mustWrite(t, filepath.Join(opts.WorktreePath, "future", "nested.txt"), "must remain out of scope\n")
	audit, err := session.Finish(context.Background(), "provider-completed")
	var violation *PolicyViolationError
	if !errors.As(err, &violation) || !hasViolation(audit, "out_of_scope_mutation") {
		t.Fatalf("Finish audit=%#v error=%v, want parent dirt not to widen file scope", audit, err)
	}
	assertFile(t, filepath.Join(allowed, "user-dirt.txt"), "parent only\n")
}

func TestSessionDetectsGlobalGitCredentialSiblingAndExternalProcessMutations(t *testing.T) {
	t.Run("global config", func(t *testing.T) {
		homeDir := t.TempDir()
		t.Setenv("HOME", homeDir)
		if runtime.GOOS == "windows" {
			t.Setenv("USERPROFILE", homeDir)
		}
		repo := initWriteRepository(t)
		opts := writeOptions(t, repo, filepath.Join(repo, "tracked.txt"))
		session, _, err := Begin(context.Background(), opts)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		runGitTest(t, repo, "config", "--global", "bounded.fixture", "forbidden")
		audit, err := session.Finish(context.Background(), "provider-completed")
		if err == nil || !hasViolation(audit, "git_global_config_modified") {
			t.Fatalf("Finish audit=%#v error=%v, want global config violation", audit, err)
		}
	})

	t.Run("credentials", func(t *testing.T) {
		homeDir := t.TempDir()
		t.Setenv("HOME", homeDir)
		if runtime.GOOS == "windows" {
			t.Setenv("USERPROFILE", homeDir)
		}
		repo := initWriteRepository(t)
		opts := writeOptions(t, repo, filepath.Join(repo, "tracked.txt"))
		session, _, err := Begin(context.Background(), opts)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		mustWrite(t, filepath.Join(homeDir, ".git-credentials"), "https://credential.invalid\n")
		audit, err := session.Finish(context.Background(), "provider-completed")
		if err == nil || !hasViolation(audit, "git_credentials_modified") {
			t.Fatalf("Finish audit=%#v error=%v, want credential violation", audit, err)
		}
	})

	t.Run("sibling worktree", func(t *testing.T) {
		repo := initWriteRepository(t)
		sibling := filepath.Join(t.TempDir(), "sibling")
		runGitTest(t, repo, "worktree", "add", "--detach", sibling, "HEAD")
		opts := writeOptions(t, repo, filepath.Join(repo, "tracked.txt"))
		session, _, err := Begin(context.Background(), opts)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		mustWrite(t, filepath.Join(sibling, "tracked.txt"), "sibling changed\n")
		audit, err := session.Finish(context.Background(), "provider-completed")
		if err == nil || !hasViolation(audit, "external_worktree_modified") {
			t.Fatalf("Finish audit=%#v error=%v, want sibling worktree violation", audit, err)
		}
	})

	t.Run("external process", func(t *testing.T) {
		repo := initWriteRepository(t)
		opts := writeOptions(t, repo, filepath.Join(repo, "tracked.txt"))
		session, _, err := Begin(context.Background(), opts)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		target := filepath.Join(repo, "external-process.txt")
		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.Command("powershell", "-NoProfile", "-Command", "Set-Content -LiteralPath $args[0] -Value external", target)
		} else {
			cmd = exec.Command("sh", "-c", `printf 'external\n' > "$1"`, "sh", target)
		}
		if output, runErr := cmd.CombinedOutput(); runErr != nil {
			t.Fatalf("external process: %v\n%s", runErr, output)
		}
		audit, err := session.Finish(context.Background(), "provider-completed")
		if err == nil || (!hasViolation(audit, "untracked_file_created") && !hasViolation(audit, "external_worktree_modified")) {
			t.Fatalf("Finish audit=%#v error=%v, want external process violation", audit, err)
		}
	})
}

func TestSessionDetectsSymlinkEscapeAndInterruptedMutation(t *testing.T) {
	t.Run("created symlink escape", func(t *testing.T) {
		repo := initWriteRepository(t)
		allowed := filepath.Join(repo, "new-link")
		opts := writeOptions(t, repo, allowed)
		session, _, err := Begin(context.Background(), opts)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		outside := filepath.Join(t.TempDir(), "outside.txt")
		mustWrite(t, outside, "outside\n")
		if err := os.Symlink(outside, filepath.Join(opts.WorktreePath, "new-link")); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		audit, err := session.Finish(context.Background(), "provider-completed")
		if err == nil || !hasViolation(audit, "symlink_or_junction_escape") {
			t.Fatalf("Finish audit=%#v error=%v", audit, err)
		}
	})

	t.Run("interrupted mutation", func(t *testing.T) {
		repo := initWriteRepository(t)
		opts := writeOptions(t, repo, filepath.Join(repo, "tracked.txt"))
		if _, _, err := Begin(context.Background(), opts); err != nil {
			t.Fatalf("initial Begin: %v", err)
		}
		mustWrite(t, filepath.Join(opts.WorktreePath, "tracked.txt"), "partial\n")
		relaunched, audit, err := Begin(context.Background(), opts)
		if relaunched != nil || err == nil || !audit.Recovered || !hasViolation(audit, "interrupted_execution_changed_state") {
			t.Fatalf("recovery session=%#v audit=%#v error=%v", relaunched, audit, err)
		}
		assertFile(t, filepath.Join(opts.WorktreePath, "tracked.txt"), "partial\n")
	})
}

func TestResolveAuthorityPinsBaseAndRejectsContractChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "authority.json")
	baseA := strings.Repeat("a", 40)
	baseB := strings.Repeat("b", 40)
	got, err := ResolveAuthority(path, "contract-a", baseA)
	if err != nil || got != baseA {
		t.Fatalf("ResolveAuthority initial=%q error=%v", got, err)
	}
	got, err = ResolveAuthority(path, "contract-a", baseB)
	if err != nil || got != baseA {
		t.Fatalf("ResolveAuthority replay=%q error=%v", got, err)
	}
	got, err = ResolveAuthority(path, "contract-a", "")
	if err != nil || got != baseA {
		t.Fatalf("ResolveAuthority replay without remote ref=%q error=%v", got, err)
	}
	if _, err := ResolveAuthority(path, "contract-b", baseA); err == nil {
		t.Fatal("ResolveAuthority accepted changed contract")
	}
	if _, err := ResolveAuthority(filepath.Join(t.TempDir(), "authority.json"), "contract-a", ""); err == nil {
		t.Fatal("ResolveAuthority created initial authority without an exact base")
	}
}

func TestReconcileVerifiedRejectsTamperedEvidence(t *testing.T) {
	repo := initWriteRepository(t)
	opts := writeOptions(t, repo, filepath.Join(repo, "tracked.txt"))
	session, _, err := Begin(context.Background(), opts)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := session.Finish(context.Background(), "provider-completed"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	data, err := os.ReadFile(opts.EvidencePath)
	if err != nil {
		t.Fatalf("ReadFile evidence: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal evidence: %v", err)
	}
	raw["base_revision"] = strings.Repeat("f", 40)
	data, err = json.Marshal(raw)
	if err != nil {
		t.Fatalf("Marshal evidence: %v", err)
	}
	mustWrite(t, opts.EvidencePath, string(data))
	if _, err := ReconcileVerified(context.Background(), opts); err == nil {
		t.Fatal("ReconcileVerified accepted tampered evidence")
	}
}

func TestReconcileVerifiedRejectsPostManifestWorktreeAndIndexChanges(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(t *testing.T, opts Options)
	}{
		{name: "worktree", run: func(t *testing.T, opts Options) {
			mustWrite(t, filepath.Join(opts.WorktreePath, "tracked.txt"), "changed after manifest\n")
		}},
		{name: "index", run: func(t *testing.T, opts Options) {
			runGitTest(t, opts.WorktreePath, "update-index", "--chmod=+x", "tracked.txt")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := initWriteRepository(t)
			opts := writeOptions(t, repo, filepath.Join(repo, "tracked.txt"))
			session, _, err := Begin(context.Background(), opts)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			if _, err := session.Finish(context.Background(), "provider-completed"); err != nil {
				t.Fatalf("Finish: %v", err)
			}
			test.run(t, opts)
			if _, err := ReconcileVerified(context.Background(), opts); err == nil {
				t.Fatal("ReconcileVerified accepted post-manifest state change")
			}
		})
	}
}

func writeOptions(t *testing.T, repo string, allowed ...string) Options {
	t.Helper()
	base := strings.TrimSpace(runGitTest(t, repo, "rev-parse", "HEAD"))
	root := t.TempDir()
	return Options{
		RepoPath: repo, WorktreePath: filepath.Join(root, "claim", "wt"), EvidencePath: filepath.Join(root, "evidence.json"),
		ContractFingerprint: "sha256:" + strings.Repeat("1", 64), ClaimGeneration: 1, BaseRevision: base, AllowedPaths: allowed,
	}
}

func initWriteRepository(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runGitTest(t, repo, "init", "-q")
	runGitTest(t, repo, "config", "user.name", "LoopCoder Test")
	runGitTest(t, repo, "config", "user.email", "loopcoder@example.invalid")
	mustWrite(t, filepath.Join(repo, "tracked.txt"), "baseline\n")
	runGitTest(t, repo, "add", "tracked.txt")
	runGitTest(t, repo, "commit", "-qm", "baseline")
	return repo
}

func runGitTest(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Env = gitutil.CleanEnv(os.Environ())
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}

func mustWrite(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != want {
		t.Fatalf("file %s = %q error=%v, want %q", path, data, err, want)
	}
}

func hasChange(audit Audit, path string) bool {
	for _, change := range audit.Changes {
		if change.Path == path {
			return true
		}
	}
	return false
}

func hasViolation(audit Audit, code string) bool {
	for _, violation := range audit.Violations {
		if violation.Code == code {
			return true
		}
	}
	return false
}
