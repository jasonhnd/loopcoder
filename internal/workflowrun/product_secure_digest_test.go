package workflowrun

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRegular(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// productOutputDigest must never follow root/parent/leaf symlinks into external content.
func TestProductOutputDigest_SymlinkPathsFailClosed(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "evil.go")
	writeRegular(t, secret, "package evil\n// EXTERNAL_SECRET\n")

	t.Run("leaf_symlink", func(t *testing.T) {
		wt := t.TempDir()
		initGitRepo(t, wt)
		if err := os.Symlink(secret, filepath.Join(wt, "evil.go")); err != nil {
			t.Fatal(err)
		}
		// Make git see the path as untracked product candidate.
		dig, files, err := productOutputDigest(wt)
		if err == nil {
			t.Fatalf("leaf symlink must fail digest, got dig=%q files=%v", dig, files)
		}
		if dig != "" || len(files) != 0 {
			t.Fatalf("on error digest/files must be empty: dig=%q files=%v", dig, files)
		}
		if !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("want symlink error, got %v", err)
		}
	})

	t.Run("parent_symlink", func(t *testing.T) {
		wt := t.TempDir()
		initGitRepo(t, wt)
		if err := os.Symlink(outside, filepath.Join(wt, "pkg")); err != nil {
			t.Fatal(err)
		}
		// Discovery may or may not list through symlink dirs; force via nested path
		// is covered when discovery finds it. Also assert direct secure hash fails.
		_, err := streamSecureRegularProduct(wt, "pkg/evil.go", io.Discard, maxProductHashBytes)
		if err == nil {
			t.Fatal("parent symlink component must refuse stream hash")
		}
	})

	t.Run("root_symlink", func(t *testing.T) {
		real := t.TempDir()
		initGitRepo(t, real)
		writeRegular(t, filepath.Join(real, "ok.go"), "package ok\n")
		parent := t.TempDir()
		linkRoot := filepath.Join(parent, "wt-link")
		if err := os.Symlink(real, linkRoot); err != nil {
			t.Fatal(err)
		}
		_, _, err := productOutputDigest(linkRoot)
		if err == nil {
			t.Fatal("symlink worktree root must fail product digest")
		}
		if !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("want root symlink error, got %v", err)
		}
	})

	t.Run("nested_legitimate", func(t *testing.T) {
		wt := t.TempDir()
		initGitRepo(t, wt)
		writeRegular(t, filepath.Join(wt, "pkg", "nested", "ok.go"), "package nested\n")
		dig, files, err := productOutputDigest(wt)
		if err != nil {
			t.Fatal(err)
		}
		if dig == "" {
			t.Fatal("empty digest for legitimate nested source")
		}
		found := false
		for _, f := range files {
			if f == "pkg/nested/ok.go" {
				found = true
			}
		}
		if !found {
			t.Fatalf("hashed=%v want pkg/nested/ok.go", files)
		}
	})
}

func TestAccept_ImplementTestsGeneric_SymlinkAndUnreadable(t *testing.T) {
	digest := "sha256:" + strings.Repeat("ee", 32)
	outside := t.TempDir()
	extSrc := filepath.Join(outside, "evil.go")
	writeRegular(t, extSrc, "package evil\n")
	extTest := filepath.Join(outside, "evil_test.go")
	writeRegular(t, extTest, "package evil\n")

	// --- implement ---
	t.Run("implement_leaf_symlink", func(t *testing.T) {
		wt := t.TempDir()
		if err := os.Symlink(extSrc, filepath.Join(wt, "evil.go")); err != nil {
			t.Fatal(err)
		}
		if err := AcceptSucceededChild("wi_implement", "implementation: deliver the change", "worker",
			[]string{"evil.go"}, wt, digest); err == nil {
			t.Fatal("implement leaf symlink must fail Accept")
		}
	})
	t.Run("implement_parent_symlink", func(t *testing.T) {
		wt := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(wt, "pkg")); err != nil {
			t.Fatal(err)
		}
		if err := AcceptSucceededChild("wi_implement", "implementation: deliver the change", "worker",
			[]string{"pkg/evil.go"}, wt, digest); err == nil {
			t.Fatal("implement parent symlink must fail Accept")
		}
	})
	t.Run("implement_root_symlink", func(t *testing.T) {
		real := t.TempDir()
		writeRegular(t, filepath.Join(real, "ok.go"), "package ok\n")
		parent := t.TempDir()
		link := filepath.Join(parent, "wt")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		if err := AcceptSucceededChild("wi_implement", "implementation: deliver the change", "worker",
			[]string{"ok.go"}, link, digest); err == nil {
			t.Fatal("implement root symlink must fail Accept")
		}
	})
	t.Run("implement_missing", func(t *testing.T) {
		wt := t.TempDir()
		if err := AcceptSucceededChild("wi_implement", "implementation: deliver the change", "worker",
			[]string{"missing.go"}, wt, digest); err == nil {
			t.Fatal("implement missing source must fail Accept")
		}
	})
	t.Run("implement_nonregular_dir", func(t *testing.T) {
		wt := t.TempDir()
		if err := os.Mkdir(filepath.Join(wt, "pkg.go"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := AcceptSucceededChild("wi_implement", "implementation: deliver the change", "worker",
			[]string{"pkg.go"}, wt, digest); err == nil {
			t.Fatal("implement directory-as-source must fail Accept")
		}
	})
	t.Run("implement_nested_legitimate", func(t *testing.T) {
		wt := t.TempDir()
		writeRegular(t, filepath.Join(wt, "pkg", "nested", "ok.go"), "package nested\n")
		if err := AcceptSucceededChild("wi_implement", "implementation: deliver the change", "worker",
			[]string{"pkg/nested/ok.go"}, wt, digest); err != nil {
			t.Fatalf("implement nested legitimate must Accept: %v", err)
		}
	})

	// --- tests ---
	t.Run("tests_leaf_symlink", func(t *testing.T) {
		wt := t.TempDir()
		if err := os.Symlink(extTest, filepath.Join(wt, "evil_test.go")); err != nil {
			t.Fatal(err)
		}
		if err := AcceptSucceededChild("wi_tests", "tests: add/adjust focused tests", "worker",
			[]string{"evil_test.go"}, wt, digest); err == nil {
			t.Fatal("tests leaf symlink must fail Accept")
		}
	})
	t.Run("tests_parent_symlink", func(t *testing.T) {
		wt := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(wt, "pkg")); err != nil {
			t.Fatal(err)
		}
		if err := AcceptSucceededChild("wi_tests", "tests: add/adjust focused tests", "worker",
			[]string{"pkg/evil_test.go"}, wt, digest); err == nil {
			t.Fatal("tests parent symlink must fail Accept")
		}
	})
	t.Run("tests_root_symlink", func(t *testing.T) {
		real := t.TempDir()
		writeRegular(t, filepath.Join(real, "ok_test.go"), "package ok\n")
		parent := t.TempDir()
		link := filepath.Join(parent, "wt")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		if err := AcceptSucceededChild("wi_tests", "tests: add/adjust focused tests", "worker",
			[]string{"ok_test.go"}, link, digest); err == nil {
			t.Fatal("tests root symlink must fail Accept")
		}
	})
	t.Run("tests_missing", func(t *testing.T) {
		wt := t.TempDir()
		if err := AcceptSucceededChild("wi_tests", "tests: add/adjust focused tests", "worker",
			[]string{"missing_test.go"}, wt, digest); err == nil {
			t.Fatal("tests missing must fail Accept")
		}
	})
	t.Run("tests_nested_legitimate", func(t *testing.T) {
		wt := t.TempDir()
		writeRegular(t, filepath.Join(wt, "pkg", "nested", "ok_test.go"), "package nested\n")
		if err := AcceptSucceededChild("wi_tests", "tests: add/adjust focused tests", "worker",
			[]string{"pkg/nested/ok_test.go"}, wt, digest); err != nil {
			t.Fatalf("tests nested legitimate must Accept: %v", err)
		}
	})

	// --- generic ---
	t.Run("generic_leaf_symlink", func(t *testing.T) {
		wt := t.TempDir()
		if err := os.Symlink(extSrc, filepath.Join(wt, "note.txt")); err != nil {
			t.Fatal(err)
		}
		if err := AcceptSucceededChild("wi_other", "generic task", "worker",
			[]string{"note.txt"}, wt, digest); err == nil {
			t.Fatal("generic leaf symlink must fail Accept")
		}
	})
	t.Run("generic_parent_symlink", func(t *testing.T) {
		wt := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(wt, "out")); err != nil {
			t.Fatal(err)
		}
		if err := AcceptSucceededChild("wi_other", "generic task", "worker",
			[]string{"out/evil.go"}, wt, digest); err == nil {
			t.Fatal("generic parent symlink must fail Accept")
		}
	})
	t.Run("generic_root_symlink", func(t *testing.T) {
		real := t.TempDir()
		writeRegular(t, filepath.Join(real, "note.txt"), "hello product\n")
		parent := t.TempDir()
		link := filepath.Join(parent, "wt")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		if err := AcceptSucceededChild("wi_other", "generic task", "worker",
			[]string{"note.txt"}, link, digest); err == nil {
			t.Fatal("generic root symlink must fail Accept")
		}
	})
	t.Run("generic_missing", func(t *testing.T) {
		wt := t.TempDir()
		if err := AcceptSucceededChild("wi_other", "generic task", "worker",
			[]string{"missing.txt"}, wt, digest); err == nil {
			t.Fatal("generic missing must fail Accept")
		}
	})
	t.Run("generic_nested_legitimate", func(t *testing.T) {
		wt := t.TempDir()
		writeRegular(t, filepath.Join(wt, "data", "nested", "note.txt"), "hello product\n")
		if err := AcceptSucceededChild("wi_other", "generic task", "worker",
			[]string{"data/nested/note.txt"}, wt, digest); err != nil {
			t.Fatalf("generic nested legitimate must Accept: %v", err)
		}
	})
}

func TestFailureClassProductDigestStable(t *testing.T) {
	if FailureClassProductDigest != "product_digest_failed" {
		t.Fatalf("stable class drifted: %q", FailureClassProductDigest)
	}
}
