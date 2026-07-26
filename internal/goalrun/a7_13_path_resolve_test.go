package goalrun

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveLongestExistingAncestor_InjectedNonIsNotExist(t *testing.T) {
	// Deterministic pure-helper injection — no log-and-pass.
	cases := []struct {
		name string
		err  error
	}{
		{"EACCES", errors.New("permission denied")},
		{"EIO", errors.New("input/output error")},
		{"ELOOP", errors.New("too many levels of symbolic links")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			_, err := resolveLongestExistingAncestorWith("/tmp/a/b/c", func(p string) (string, error) {
				calls++
				return "", tc.err
			})
			if err == nil {
				t.Fatal("want immediate non-IsNotExist fail")
			}
			if calls != 1 {
				t.Fatalf("must return immediately without walking up; calls=%d", calls)
			}
			if !strings.Contains(err.Error(), "EvalSymlinks") {
				t.Fatalf("want EvalSymlinks error, got %v", err)
			}
		})
	}
}

func TestResolveLongestExistingAncestor_RootUnresolvedFails(t *testing.T) {
	_, err := resolveLongestExistingAncestorWith("/no/such/deep/path/x", func(p string) (string, error) {
		return "", os.ErrNotExist
	})
	if err == nil {
		t.Fatal("want root unresolved fail")
	}
	if !strings.Contains(err.Error(), "failed at root") {
		t.Fatalf("want failed at root, got %v", err)
	}
}

func TestResolveLongestExistingAncestor_IsNotExistWalksThenSuccess(t *testing.T) {
	var seen []string
	got, err := resolveLongestExistingAncestorWith("/prefix/a/b/c", func(p string) (string, error) {
		seen = append(seen, p)
		if p == "/prefix" || p == filepath.Clean("/prefix") {
			return "/resolved-prefix", nil
		}
		return "", os.ErrNotExist
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "resolved-prefix") {
		t.Fatalf("want rejoined under resolved prefix, got %q seen=%v", got, seen)
	}
	if len(seen) < 2 {
		t.Fatalf("want walk-up, seen=%v", seen)
	}
}

func TestResolveLongestExistingAncestor_ProductionWrapper(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, "projects", "p", "runs", "r", "workflow-events.jsonl")
	got, err := resolveLongestExistingAncestor(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "projects") {
		t.Fatalf("got %q", got)
	}
}

func TestResolveLongestExistingAncestor_SymlinkLeafRejectedBySecureWalk(t *testing.T) {
	home := t.TempDir()
	projectID, runID := "proj", "run1"
	can, err := eventLogPathRead(home, projectID, runID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(can), 0o700); err != nil {
		t.Fatal(err)
	}
	target := can + ".real"
	if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, can); err != nil {
		t.Fatal(err)
	}
	_, err = resolveCanonicalEventLogPath(home, projectID, runID, can, can)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("want symlink fail, got %v", err)
	}
}

func TestResolveCanonical_NonexistentRemainderOK(t *testing.T) {
	home := t.TempDir()
	projectID, runID := "proj", "runx"
	can, err := eventLogPathRead(home, projectID, runID)
	if err != nil {
		t.Fatal(err)
	}
	p, err := resolveCanonicalEventLogPath(home, projectID, runID, can, can)
	if err != nil {
		t.Fatalf("nonexistent remainder: %v", err)
	}
	if p != can {
		t.Fatalf("want %q got %q", can, p)
	}
}

func TestResolveLongestExistingAncestor_SymlinkParentComponent(t *testing.T) {
	home := t.TempDir()
	projectID, runID := "proj", "runsym"
	can, err := eventLogPathRead(home, projectID, runID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(can), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(can, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Dir(can)
	evil := runDir + ".evil"
	if err := os.MkdirAll(evil, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(can, filepath.Join(evil, "workflow-events.jsonl")); err != nil {
		t.Fatal(err)
	}
	_ = os.RemoveAll(runDir)
	if err := os.Symlink(evil, runDir); err != nil {
		t.Fatal(err)
	}
	_, err = resolveCanonicalEventLogPath(home, projectID, runID, can, can)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("want symlink parent fail, got %v", err)
	}
}
