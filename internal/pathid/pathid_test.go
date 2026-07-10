package pathid

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCanonicalizeResolvesSymlinkAliasToSameIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not reliable in unprivileged Windows test runs")
	}
	root := t.TempDir()
	physical := filepath.Join(root, "physical", "repo")
	aliasRoot := filepath.Join(root, "alias")
	if err := os.MkdirAll(physical, 0o755); err != nil {
		t.Fatalf("mkdir physical: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "physical"), aliasRoot); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	viaPhysical, err := Canonicalize(physical)
	if err != nil {
		t.Fatalf("Canonicalize physical: %v", err)
	}
	viaAlias, err := Canonicalize(filepath.Join(aliasRoot, "repo"))
	if err != nil {
		t.Fatalf("Canonicalize alias: %v", err)
	}
	if viaPhysical.Identity != viaAlias.Identity {
		t.Fatalf("identity physical=%q alias=%q, want equal", viaPhysical.Identity, viaAlias.Identity)
	}
	if viaPhysical.Display == viaAlias.Display {
		t.Fatalf("display paths unexpectedly equal; display should preserve user-facing lexical input")
	}
}

func TestCanonicalizeResolvesNestedSymlinkAndMissingLeaf(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not reliable in unprivileged Windows test runs")
	}
	root := t.TempDir()
	physical := filepath.Join(root, "physical")
	nested := filepath.Join(root, "links", "nested")
	if err := os.MkdirAll(physical, 0o755); err != nil {
		t.Fatalf("mkdir physical: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(nested), 0o755); err != nil {
		t.Fatalf("mkdir links: %v", err)
	}
	if err := os.Symlink(physical, nested); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	got, err := Canonicalize(filepath.Join(nested, "future", "file.txt"))
	if err != nil {
		t.Fatalf("Canonicalize missing leaf: %v", err)
	}
	want := filepath.Join(canonicalTestIdentity(t, physical), "future", "file.txt")
	if got.Identity != want {
		t.Fatalf("identity = %q, want %q", got.Identity, want)
	}
}

func TestCanonicalizeExistingPath(t *testing.T) {
	dir := t.TempDir()
	got, err := Canonicalize(filepath.Join(dir, "."))
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	want, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	want = filepath.Clean(want)
	if got.Display != want || got.Identity == "" {
		t.Fatalf("canonical = %#v, want display %q and non-empty identity", got, want)
	}
}

func canonicalTestIdentity(t *testing.T, path string) string {
	t.Helper()
	identity, err := Identity(path)
	if err != nil {
		t.Fatalf("Identity %q: %v", path, err)
	}
	return identity
}
