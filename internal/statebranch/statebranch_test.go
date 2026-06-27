package statebranch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPushScrubsStateBeforeCommitAndKeepsRawLogsOut(t *testing.T) {
	repo := t.TempDir()
	runID := "run-test"
	runPath := filepath.Join(repo, ".loopcoder", "runs", runID)
	if err := os.MkdirAll(filepath.Join(runPath, "workers"), 0o755); err != nil {
		t.Fatalf("MkdirAll workers: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(runPath, "recovery"), 0o755); err != nil {
		t.Fatalf("MkdirAll recovery: %v", err)
	}

	externalLog := filepath.Join(t.TempDir(), "codex.log")
	if err := os.WriteFile(externalLog, []byte("line 1\nAuthorization: Bearer abc.def\napi_key=sk-abcdefghijklmnopqrstuvwxyz123456\n"), 0o644); err != nil {
		t.Fatalf("WriteFile external log: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runPath, "events.jsonl"), []byte("{\"error\":\"password=hunter2\"}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile events: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runPath, "workers", "job-1.attempt.json"), []byte(`{"version":1,"job_id":"job-1","issue":1,"attempt":1,"error":"token=ghp_abcdefghijklmnopqrstuvwxyz123456"}`), 0o644); err != nil {
		t.Fatalf("WriteFile attempt: %v", err)
	}
	brief := "- Log path: " + externalLog + "\n\npassword=hunter2\n"
	if err := os.WriteFile(filepath.Join(runPath, "recovery", "job-1-context.md"), []byte(brief), 0o644); err != nil {
		t.Fatalf("WriteFile recovery brief: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runPath, "codex.log"), []byte("Bearer raw.secret\npassword=hunter2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile raw local log: %v", err)
	}

	fake := newStateBranchFakeGit(t)
	scratchRoot := t.TempDir()
	var scratch string
	result, err := Push(context.Background(), PushOptions{
		RepoPath: repo,
		RunID:    runID,
	}, Deps{
		Git: fake,
		Now: func() time.Time {
			return time.Date(2026, 6, 27, 1, 2, 3, 0, time.UTC)
		},
		MkdirTemp: func(string, string) (string, error) {
			scratch = filepath.Join(scratchRoot, "scratch")
			return scratch, os.MkdirAll(scratch, 0o755)
		},
		RemoveAll: func(string) error {
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Push returned error: %v", err)
	}
	if !result.Committed || !result.Pushed {
		t.Fatalf("result committed/pushed = %v/%v, want true/true", result.Committed, result.Pushed)
	}

	worktree := filepath.Join(scratch, "wt")
	all := readAllTestFiles(t, filepath.Join(worktree, "runs", runID))
	for _, leaked := range []string{"hunter2", "abc.def", "sk-abcdefghijklmnopqrstuvwxyz123456", "ghp_abcdefghijklmnopqrstuvwxyz123456", "raw.secret"} {
		if strings.Contains(all, leaked) {
			t.Fatalf("state branch leaked %q:\n%s", leaked, all)
		}
	}
	for _, want := range []string{"[REDACTED_SECRET]", "Bearer [REDACTED_TOKEN]"} {
		if !strings.Contains(all, want) {
			t.Fatalf("state branch missing scrubbed marker %q:\n%s", want, all)
		}
	}
	if _, err := os.Stat(filepath.Join(worktree, "runs", runID, "codex.log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("raw codex.log was committed or stat failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(worktree, "runs", runID, "logs", "log_manifest.json")); err != nil {
		t.Fatalf("log manifest missing: %v", err)
	}
	if !strings.Contains(all, "sha256:") || !strings.Contains(all, ".tail.txt") {
		t.Fatalf("log manifest missing hash or tail path:\n%s", all)
	}
}

func TestLeaseAcquireRenewObserveTakeoverAndRelease(t *testing.T) {
	repo := t.TempDir()
	fake := newStateBranchFakeGit(t)
	now := time.Date(2026, 6, 27, 1, 0, 0, 0, time.UTC)
	host := "host-a"
	pid := 111
	random := "aaa"
	deps := leaseTestDeps(fake, &now, &host, &pid, &random)

	acquired, err := Acquire(context.Background(), LeaseOptions{
		RepoPath: repo,
		RunID:    "run-test",
		TTL:      10 * time.Minute,
	}, deps)
	if err != nil {
		t.Fatalf("Acquire returned error: %v", err)
	}
	if acquired.Status != "acquired" || !acquired.Pushed || acquired.Lease == nil {
		t.Fatalf("acquired result = %#v", acquired)
	}
	firstLeaseID := acquired.Lease.LeaseID
	if firstLeaseID != "host-a-111-aaa" {
		t.Fatalf("lease id = %q, want host-a-111-aaa", firstLeaseID)
	}

	now = now.Add(time.Minute)
	random = "bbb"
	renewed, err := Acquire(context.Background(), LeaseOptions{
		RepoPath: repo,
		RunID:    "run-test",
		TTL:      10 * time.Minute,
	}, deps)
	if err != nil {
		t.Fatalf("renew Acquire returned error: %v", err)
	}
	if renewed.Status != "renewed" || renewed.Lease == nil || renewed.Lease.LeaseID != firstLeaseID {
		t.Fatalf("renewed result = %#v, want same lease id %q", renewed, firstLeaseID)
	}

	host = "host-b"
	pid = 222
	random = "ccc"
	now = now.Add(time.Minute)
	observed, err := Acquire(context.Background(), LeaseOptions{
		RepoPath: repo,
		RunID:    "run-test",
		TTL:      10 * time.Minute,
	}, deps)
	if err != nil {
		t.Fatalf("foreign Acquire returned error: %v", err)
	}
	if observed.Status != "observe-only" || !observed.ObserveOnly || observed.Pushed {
		t.Fatalf("foreign lease result = %#v, want observe-only without push", observed)
	}

	now = time.Date(2026, 6, 27, 1, 12, 0, 0, time.UTC)
	taken, err := Acquire(context.Background(), LeaseOptions{
		RepoPath: repo,
		RunID:    "run-test",
		TTL:      10 * time.Minute,
	}, deps)
	if err != nil {
		t.Fatalf("takeover Acquire returned error: %v", err)
	}
	if taken.Status != "taken-over" || taken.Lease == nil || taken.Lease.LeaseID != "host-b-222-ccc" {
		t.Fatalf("takeover result = %#v", taken)
	}

	released, err := Release(context.Background(), LeaseOptions{
		RepoPath: repo,
		RunID:    "run-test",
	}, deps)
	if err != nil {
		t.Fatalf("Release returned error: %v", err)
	}
	if released.Status != "released" || !released.Pushed {
		t.Fatalf("release result = %#v", released)
	}
	if _, err := os.Stat(filepath.Join(fake.branchRoot, "lease.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lease.json still exists or stat failed: %v", err)
	}
	events := readAllTestFiles(t, filepath.Join(fake.branchRoot, "runs", "run-test"))
	for _, want := range []string{"lease.acquire", "taken-over", "lease.release"} {
		if !strings.Contains(events, want) {
			t.Fatalf("events missing %q:\n%s", want, events)
		}
	}
}

func TestAcquireReportsObserveOnlyWhenPushCASLoses(t *testing.T) {
	repo := t.TempDir()
	fake := newStateBranchFakeGit(t)
	now := time.Date(2026, 6, 27, 1, 0, 0, 0, time.UTC)
	foreign := Lease{
		Version:        1,
		RunID:          "run-test",
		LeaseID:        "other-999-won",
		Host:           "other",
		PID:            999,
		StartedAt:      stateFormat(now),
		LeaseExpiresAt: stateFormat(now.Add(10 * time.Minute)),
	}
	fake.pushErr = errors.New("non-fast-forward")
	fake.onPushError = func() {
		fake.branchExists = true
		writeBranchLeaseTest(t, fake.branchRoot, foreign)
	}
	host := "host-a"
	pid := 111
	random := "aaa"
	deps := leaseTestDeps(fake, &now, &host, &pid, &random)

	result, err := Acquire(context.Background(), LeaseOptions{
		RepoPath: repo,
		RunID:    "run-test",
		TTL:      10 * time.Minute,
	}, deps)
	if err != nil {
		t.Fatalf("Acquire returned error: %v", err)
	}
	if result.Status != "observe-only" || !result.ObserveOnly || result.PushError == "" {
		t.Fatalf("CAS conflict result = %#v, want observe-only with push error", result)
	}
	if result.Lease == nil || result.Lease.LeaseID != foreign.LeaseID {
		t.Fatalf("CAS conflict lease = %#v, want %#v", result.Lease, foreign)
	}
}

func TestPullMirrorsStateBranchAndHydratesMissingRuns(t *testing.T) {
	repo := t.TempDir()
	fake := newStateBranchFakeGit(t)
	fake.branchExists = true
	if err := os.MkdirAll(filepath.Join(fake.branchRoot, "runs", "run-test"), 0o755); err != nil {
		t.Fatalf("MkdirAll branch run: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fake.branchRoot, "runs", "run-test", "state.json"), []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatalf("WriteFile branch state: %v", err)
	}

	result, err := Pull(context.Background(), PullOptions{RepoPath: repo}, Deps{
		Git: fake,
		MkdirTemp: func(string, string) (string, error) {
			dir := filepath.Join(t.TempDir(), "scratch")
			return dir, os.MkdirAll(dir, 0o755)
		},
		RemoveAll: func(string) error { return nil },
	})
	if err != nil {
		t.Fatalf("Pull returned error: %v", err)
	}
	if !reflect.DeepEqual(result.Runs, []string{"run-test"}) {
		t.Fatalf("runs = %#v, want run-test", result.Runs)
	}
	if _, err := os.Stat(filepath.Join(result.MirrorPath, "runs", "run-test", "state.json")); err != nil {
		t.Fatalf("mirror state missing: %v", err)
	}
	for _, path := range []string{
		result.MirrorPath,
		filepath.Join(result.MirrorPath, "runs"),
		filepath.Join(result.MirrorPath, "runs", "run-test"),
		filepath.Join(result.MirrorPath, "runs", "run-test", "state.json"),
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat mirror path %s: %v", path, err)
		}
		if info.Mode().Perm()&0o200 == 0 {
			t.Fatalf("mirror path %s mode = %v, want owner-writable", path, info.Mode().Perm())
		}
	}
	if _, err := os.Stat(filepath.Join(repo, ".loopcoder", "runs", "run-test", "state.json")); err != nil {
		t.Fatalf("hydrated run state missing: %v", err)
	}
}

func leaseTestDeps(fake *stateBranchFakeGit, now *time.Time, host *string, pid *int, random *string) Deps {
	return Deps{
		Git: fake,
		Now: func() time.Time {
			return *now
		},
		Hostname: func() (string, error) {
			return *host, nil
		},
		PID: func() int {
			return *pid
		},
		RandomSuffix: func() (string, error) {
			return *random, nil
		},
		MkdirTemp: func(string, string) (string, error) {
			dir := filepath.Join(fake.scratchRoot, "scratch-"+time.Now().Format("150405.000000000"))
			return dir, os.MkdirAll(dir, 0o755)
		},
		RemoveAll: func(string) error {
			return nil
		},
	}
}

func stateFormat(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

type stateBranchFakeGit struct {
	t            *testing.T
	branchExists bool
	branchRoot   string
	scratchRoot  string
	calls        [][]string
	pushErr      error
	onPushError  func()
}

func newStateBranchFakeGit(t *testing.T) *stateBranchFakeGit {
	t.Helper()
	return &stateBranchFakeGit{
		t:           t,
		branchRoot:  filepath.Join(t.TempDir(), "branch"),
		scratchRoot: t.TempDir(),
	}
}

func (f *stateBranchFakeGit) RunGit(_ context.Context, repoPath string, args ...string) ([]byte, error) {
	call := append([]string{repoPath}, args...)
	f.calls = append(f.calls, call)
	if len(args) == 0 {
		return nil, nil
	}

	switch args[0] {
	case "fetch":
		if f.branchExists {
			return nil, nil
		}
		return nil, errors.New("remote branch not found")
	case "rev-parse":
		if f.branchExists {
			return []byte("abc123\n"), nil
		}
		return nil, errors.New("local branch not found")
	case "worktree":
		return f.runWorktree(repoPath, args[1:]...)
	case "checkout", "rm", "merge", "add", "commit":
		return nil, nil
	case "status":
		return []byte(" M state\n"), nil
	case "push":
		if f.pushErr != nil {
			if f.onPushError != nil {
				f.onPushError()
			}
			return nil, f.pushErr
		}
		if err := replaceTreeTest(f.branchRoot, repoPath); err != nil {
			return nil, err
		}
		f.branchExists = true
		return nil, nil
	default:
		return nil, nil
	}
}

func (f *stateBranchFakeGit) runWorktree(_ string, args ...string) ([]byte, error) {
	if len(args) == 0 {
		return nil, nil
	}
	switch args[0] {
	case "add":
		target := ""
		if len(args) >= 4 && args[1] == "--detach" {
			target = args[2]
		} else if len(args) >= 5 && args[1] == "-b" {
			target = args[3]
		} else if len(args) >= 2 {
			target = args[1]
		}
		if target == "" {
			return nil, errors.New("fake worktree add target missing")
		}
		if err := os.MkdirAll(target, 0o755); err != nil {
			return nil, err
		}
		if f.branchExists {
			if err := copyTreeTest(f.branchRoot, target); err != nil {
				return nil, err
			}
		}
		return nil, nil
	case "remove":
		return nil, nil
	default:
		return nil, nil
	}
}

func writeBranchLeaseTest(t *testing.T, root string, lease Lease) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll branch root: %v", err)
	}
	data, err := json.Marshal(lease)
	if err != nil {
		t.Fatalf("Marshal lease: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "lease.json"), data, 0o644); err != nil {
		t.Fatalf("WriteFile lease: %v", err)
	}
}

func readAllTestFiles(t *testing.T, root string) string {
	t.Helper()
	var out strings.Builder
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out.WriteString(filepath.ToSlash(path))
		out.WriteByte('\n')
		out.Write(data)
		out.WriteByte('\n')
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir %s: %v", root, err)
	}
	return out.String()
}

func replaceTreeTest(dest, source string) error {
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	return copyTreeTest(source, dest)
}

func copyTreeTest(source, dest string) error {
	return filepath.WalkDir(source, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}
