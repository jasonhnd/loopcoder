package readonlyexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionAcceptsCleanAndPreexistingDirtyCheckout(t *testing.T) {
	repo := initRepository(t)
	tracked := filepath.Join(repo, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("preexisting dirty\n"), 0o644); err != nil {
		t.Fatalf("write dirty tracked file: %v", err)
	}
	untracked := filepath.Join(repo, "notes.tmp")
	if err := os.WriteFile(untracked, []byte("preexisting untracked\n"), 0o644); err != nil {
		t.Fatalf("write preexisting untracked file: %v", err)
	}

	session, baseline, err := Begin(context.Background(), enforcementOptions(t, repo))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if baseline.Verification != "baseline-captured" || baseline.BaselineFingerprint == "" {
		t.Fatalf("baseline audit = %#v", baseline)
	}
	audit, err := session.Finish(context.Background(), "provider-completed")
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if audit.Verification != VerificationPassed || audit.BaselineFingerprint != audit.PostRunFingerprint || len(audit.Violations) != 0 {
		t.Fatalf("clean audit = %#v", audit)
	}
	assertFileText(t, tracked, "preexisting dirty\n")
	assertFileText(t, untracked, "preexisting untracked\n")
}

func TestSessionDetectsRepositoryAuthorityMutations(t *testing.T) {
	tests := []struct {
		name string
		code string
		run  func(*testing.T, string)
	}{
		{name: "tracked", code: "checkout_state_modified", run: func(t *testing.T, repo string) {
			mustWrite(t, filepath.Join(repo, "tracked.txt"), "changed\n")
		}},
		{name: "untracked", code: "untracked_file_created", run: func(t *testing.T, repo string) {
			mustWrite(t, filepath.Join(repo, "created.txt"), "created\n")
		}},
		{name: "index", code: "git_index_modified", run: func(t *testing.T, repo string) {
			mustWrite(t, filepath.Join(repo, "tracked.txt"), "staged\n")
			runGitTest(t, repo, "add", "tracked.txt")
		}},
		{name: "ref", code: "git_refs_modified", run: func(t *testing.T, repo string) {
			runGitTest(t, repo, "branch", "provider-created-ref")
		}},
		{name: "config", code: "git_config_modified", run: func(t *testing.T, repo string) {
			runGitTest(t, repo, "config", "loopcoder.providerMutation", "true")
		}},
		{name: "hook", code: "git_hooks_modified", run: func(t *testing.T, repo string) {
			mustWrite(t, filepath.Join(repo, ".git", "hooks", "provider-hook"), "#!/bin/sh\n")
		}},
		{name: "worktree metadata", code: "git_worktree_metadata_modified", run: func(t *testing.T, repo string) {
			other := filepath.Join(t.TempDir(), "other-worktree")
			runGitTest(t, repo, "worktree", "add", "--detach", other, "HEAD")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := initRepository(t)
			opts := enforcementOptions(t, repo)
			session, _, err := Begin(context.Background(), opts)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			tt.run(t, repo)
			audit, err := session.Finish(context.Background(), "provider-completed")
			var violation *PolicyViolationError
			if !errors.As(err, &violation) {
				t.Fatalf("Finish error = %v, want PolicyViolationError", err)
			}
			if audit.Verification != VerificationViolation || !hasViolationCode(audit.Violations, tt.code) {
				t.Fatalf("audit = %#v, want violation code %q", audit, tt.code)
			}
			data, marshalErr := json.Marshal(audit)
			if marshalErr != nil {
				t.Fatalf("Marshal audit: %v", marshalErr)
			}
			if strings.Contains(string(data), repo) || strings.Contains(string(data), "tracked.txt") || strings.Contains(string(data), "created.txt") {
				t.Fatalf("public audit leaked local path: %s", data)
			}
		})
	}
}

func TestSessionDetectsSecondWorktreeAndProjectStateMutation(t *testing.T) {
	t.Run("second worktree", func(t *testing.T) {
		repo := initRepository(t)
		other := filepath.Join(t.TempDir(), "other-worktree")
		runGitTest(t, repo, "worktree", "add", "--detach", other, "HEAD")
		session, _, err := Begin(context.Background(), enforcementOptions(t, repo))
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		mustWrite(t, filepath.Join(other, "tracked.txt"), "changed elsewhere\n")
		audit, err := session.Finish(context.Background(), "provider-completed")
		if err == nil || !hasViolationCode(audit.Violations, "external_worktree_modified") {
			t.Fatalf("second-worktree audit=%#v error=%v", audit, err)
		}
	})

	t.Run("registered project state", func(t *testing.T) {
		repo := initRepository(t)
		projectState := filepath.Join(t.TempDir(), "project-state")
		if err := os.MkdirAll(projectState, 0o700); err != nil {
			t.Fatalf("MkdirAll project state: %v", err)
		}
		stateFile := filepath.Join(projectState, "state.json")
		mustWrite(t, stateFile, "{}\n")
		opts := enforcementOptions(t, repo)
		opts.ProjectStatePaths = []string{projectState}
		session, _, err := Begin(context.Background(), opts)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		mustWrite(t, stateFile, "{\"provider\":true}\n")
		audit, err := session.Finish(context.Background(), "provider-completed")
		if err == nil || !hasViolationCode(audit.Violations, "loopcoder_project_state_modified") {
			t.Fatalf("project-state audit=%#v error=%v", audit, err)
		}
	})

	t.Run("authorized run-state exclusion", func(t *testing.T) {
		repo := initRepository(t)
		projectState := filepath.Join(t.TempDir(), "project-state")
		runs := filepath.Join(projectState, "runs")
		if err := os.MkdirAll(runs, 0o700); err != nil {
			t.Fatalf("MkdirAll project state: %v", err)
		}
		stable := filepath.Join(projectState, "registry.json")
		mustWrite(t, stable, "{}\n")
		opts := enforcementOptions(t, repo)
		opts.ProjectStatePaths = []string{projectState}
		opts.ExcludedPaths = []string{runs}
		session, _, err := Begin(context.Background(), opts)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		mustWrite(t, filepath.Join(runs, "scheduler-owned.json"), "{}\n")
		audit, err := session.Finish(context.Background(), "provider-completed")
		if err != nil || audit.Verification != VerificationPassed {
			t.Fatalf("excluded run-state audit=%#v error=%v", audit, err)
		}
	})
}

func TestSessionDetectsIgnoredUntrackedMutation(t *testing.T) {
	repo := initRepository(t)
	mustWrite(t, filepath.Join(repo, ".gitignore"), "*.generated\n")
	runGitTest(t, repo, "add", ".gitignore")
	runGitTest(t, repo, "commit", "-m", "ignore generated fixtures")
	session, _, err := Begin(context.Background(), enforcementOptions(t, repo))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	mustWrite(t, filepath.Join(repo, "provider.generated"), "mutation\n")
	audit, err := session.Finish(context.Background(), "provider-completed")
	if err == nil || !hasViolationCode(audit.Violations, "ignored_untracked_file_created") {
		t.Fatalf("ignored mutation audit=%#v error=%v", audit, err)
	}
}

func TestSessionRecoversInterruptedBaselineBeforeRelaunch(t *testing.T) {
	t.Run("unchanged recovery", func(t *testing.T) {
		repo := initRepository(t)
		opts := enforcementOptions(t, repo)
		if _, _, err := Begin(context.Background(), opts); err != nil {
			t.Fatalf("initial Begin: %v", err)
		}
		recovered, audit, err := Begin(context.Background(), opts)
		if err != nil {
			t.Fatalf("recovery Begin: %v", err)
		}
		if !audit.Recovered || recovered == nil {
			t.Fatalf("recovery audit/session = %#v / %#v", audit, recovered)
		}
		finished, err := recovered.Finish(context.Background(), "provider-canceled")
		if err != nil || finished.Verification != VerificationPassed || !finished.Recovered {
			t.Fatalf("recovered finish audit=%#v error=%v", finished, err)
		}
	})

	t.Run("changed recovery", func(t *testing.T) {
		repo := initRepository(t)
		opts := enforcementOptions(t, repo)
		if _, _, err := Begin(context.Background(), opts); err != nil {
			t.Fatalf("initial Begin: %v", err)
		}
		mustWrite(t, filepath.Join(repo, "tracked.txt"), "changed after crash\n")
		session, audit, err := Begin(context.Background(), opts)
		var violation *PolicyViolationError
		if session != nil || !errors.As(err, &violation) || audit.Verification != VerificationViolation || !audit.Recovered {
			t.Fatalf("changed recovery session=%#v audit=%#v error=%v", session, audit, err)
		}
	})
}

func TestSessionDoesNotRelaunchVerifiedExecutionWhenAttemptDeliveryWasLost(t *testing.T) {
	repo := initRepository(t)
	opts := enforcementOptions(t, repo)
	session, _, err := Begin(context.Background(), opts)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if audit, err := session.Finish(context.Background(), "provider-completed"); err != nil || audit.Verification != VerificationPassed {
		t.Fatalf("Finish audit=%#v error=%v", audit, err)
	}
	relaunched, audit, err := Begin(context.Background(), opts)
	var violation *PolicyViolationError
	if relaunched != nil || !errors.As(err, &violation) || audit.Verification != VerificationInconclusive || !hasViolationCode(audit.Violations, "verified_execution_requires_reconciliation") {
		t.Fatalf("relaunch session=%#v audit=%#v error=%v", relaunched, audit, err)
	}
}

func TestSessionFailsClosedWhenEvidenceIsModified(t *testing.T) {
	repo := initRepository(t)
	opts := enforcementOptions(t, repo)
	session, _, err := Begin(context.Background(), opts)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	mustWrite(t, opts.EvidencePath, "{}\n")
	audit, err := session.Finish(context.Background(), "provider-completed")
	if err == nil || !hasViolationCode(audit.Violations, "enforcement_evidence_modified") {
		t.Fatalf("tampered evidence audit=%#v error=%v", audit, err)
	}
}

func TestBeginRejectsEvidencePathThatPhysicallyResolvesInsideCheckout(t *testing.T) {
	repo := initRepository(t)
	alias := filepath.Join(t.TempDir(), "checkout-alias")
	if err := os.Symlink(repo, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	opts := enforcementOptions(t, repo)
	opts.EvidencePath = filepath.Join(alias, "evidence.json")
	session, audit, err := Begin(context.Background(), opts)
	var violation *PolicyViolationError
	if session != nil || !errors.As(err, &violation) || audit.Verification != VerificationInconclusive {
		t.Fatalf("session=%#v audit=%#v error=%v", session, audit, err)
	}
	if _, statErr := os.Stat(filepath.Join(repo, "evidence.json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unsafe evidence path was written: %v", statErr)
	}
}

func TestSessionDoesNotOverwritePriorViolationOnRelaunch(t *testing.T) {
	repo := initRepository(t)
	opts := enforcementOptions(t, repo)
	session, _, err := Begin(context.Background(), opts)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	mustWrite(t, filepath.Join(repo, "created.txt"), "mutation\n")
	if _, err := session.Finish(context.Background(), "provider-completed"); err == nil {
		t.Fatal("Finish returned nil error, want policy violation")
	}
	relaunched, audit, err := Begin(context.Background(), opts)
	var violation *PolicyViolationError
	if relaunched != nil || !errors.As(err, &violation) || audit.Verification != VerificationViolation {
		t.Fatalf("relaunch session=%#v audit=%#v error=%v", relaunched, audit, err)
	}
}

func TestBeginFailsClosedOnUnknownPriorEvidenceState(t *testing.T) {
	repo := initRepository(t)
	opts := enforcementOptions(t, repo)
	mustWrite(t, opts.EvidencePath, `{"schema_version":"loopcoder.read_only_enforcement.v1","status":"unknown"}`)
	session, audit, err := Begin(context.Background(), opts)
	var violation *PolicyViolationError
	if session != nil || !errors.As(err, &violation) || audit.Verification != VerificationInconclusive || !hasViolationCode(audit.Violations, "execution_contract_evidence_mismatch") {
		t.Fatalf("session=%#v audit=%#v error=%v", session, audit, err)
	}
	data, readErr := os.ReadFile(opts.EvidencePath)
	if readErr != nil || !strings.Contains(string(data), `"status":"unknown"`) {
		t.Fatalf("unknown prior evidence was overwritten: %q error=%v", data, readErr)
	}
}

func TestBeginDoesNotRelayTamperedEvidenceFields(t *testing.T) {
	repo := initRepository(t)
	opts := enforcementOptions(t, repo)
	session, _, err := Begin(context.Background(), opts)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	mustWrite(t, filepath.Join(repo, "created.txt"), "mutation\n")
	if _, err := session.Finish(context.Background(), "provider-completed"); err == nil {
		t.Fatal("Finish returned nil error, want policy violation")
	}
	record, ok, err := loadRecord(opts.EvidencePath)
	if err != nil || !ok || len(record.Violations) == 0 {
		t.Fatalf("loadRecord = %#v/%t error=%v", record, ok, err)
	}
	record.Violations[0].Code = repo
	if err := writeRecord(opts.EvidencePath, record); err != nil {
		t.Fatalf("write tampered record: %v", err)
	}
	_, audit, err := Begin(context.Background(), opts)
	var violation *PolicyViolationError
	if !errors.As(err, &violation) || audit.Verification != VerificationInconclusive || !hasViolationCode(audit.Violations, "execution_contract_evidence_mismatch") {
		t.Fatalf("audit=%#v error=%v", audit, err)
	}
	data, marshalErr := json.Marshal(audit)
	if marshalErr != nil {
		t.Fatalf("Marshal audit: %v", marshalErr)
	}
	if strings.Contains(string(data), repo) {
		t.Fatalf("tampered evidence leaked into public audit: %s", data)
	}
}

func TestCompareBoundsPublicViolationList(t *testing.T) {
	before := Snapshot{Entries: map[string]string{}}
	after := Snapshot{Entries: map[string]string{}}
	for i := 0; i < maxPublicViolations+25; i++ {
		after.Entries[fmt.Sprintf("worktree:primary:untracked:file-%03d", i)] = "sha256:changed"
	}
	violations := Compare(before, after)
	if len(violations) != maxPublicViolations+1 || violations[len(violations)-1].Code != "additional_guarded_changes_omitted" {
		t.Fatalf("violations = %d last=%#v", len(violations), violations[len(violations)-1])
	}
}

func enforcementOptions(t *testing.T, repo string) Options {
	t.Helper()
	return Options{
		RepoPath:            repo,
		EvidencePath:        filepath.Join(t.TempDir(), "read-only-evidence.json"),
		ContractFingerprint: "sha256:contract",
		ClaimGeneration:     1,
	}
}

func initRepository(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runGitTest(t, repo, "init", "-b", "main")
	runGitTest(t, repo, "config", "user.name", "LoopCoder Test")
	runGitTest(t, repo, "config", "user.email", "loopcoder@example.invalid")
	mustWrite(t, filepath.Join(repo, "tracked.txt"), "baseline\n")
	runGitTest(t, repo, "add", "tracked.txt")
	runGitTest(t, repo, "commit", "-m", "baseline")
	return repo
}

func runGitTest(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Env = append(cleanGitEnv(os.Environ()), "GIT_OPTIONAL_LOCKS=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
}

func mustWrite(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

func assertFileText(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if string(data) != want {
		t.Fatalf("file text = %q, want %q", data, want)
	}
}

func hasViolationCode(violations []Violation, want string) bool {
	for _, violation := range violations {
		if violation.Code == want {
			return true
		}
	}
	return false
}
