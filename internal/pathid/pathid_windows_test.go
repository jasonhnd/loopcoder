//go:build windows

package pathid

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFoldIdentityNormalizesWindowsVolumeAndCase(t *testing.T) {
	got := foldIdentity(`c:\Users\Owner\Repo`)
	want := `C:\users\owner\repo`
	if got != want {
		t.Fatalf("foldIdentity = %q, want %q", got, want)
	}
}

func TestCanonicalizeResolvesWindowsJunctionAliasToSameIdentity(t *testing.T) {
	root := t.TempDir()
	physicalRoot := filepath.Join(root, "physical")
	physicalScope := filepath.Join(physicalRoot, "repo", "src")
	if err := os.MkdirAll(physicalScope, 0o755); err != nil {
		t.Fatalf("mkdir physical scope: %v", err)
	}
	if err := os.WriteFile(filepath.Join(physicalScope, "owned.go"), []byte("package src\n"), 0o644); err != nil {
		t.Fatalf("write physical file: %v", err)
	}
	junctionRoot := filepath.Join(root, "junction")
	createWindowsJunction(t, junctionRoot, physicalRoot)

	viaPhysical, err := Canonicalize(filepath.Join(physicalScope, "owned.go"))
	if err != nil {
		t.Fatalf("Canonicalize physical: %v", err)
	}
	viaJunction, err := Canonicalize(filepath.Join(junctionRoot, "repo", "src", "owned.go"))
	if err != nil {
		t.Fatalf("Canonicalize junction: %v", err)
	}
	if viaPhysical.Identity != viaJunction.Identity {
		t.Fatalf("identity physical=%q junction=%q, want equal", viaPhysical.Identity, viaJunction.Identity)
	}
	if viaPhysical.Display == viaJunction.Display {
		t.Fatalf("display paths unexpectedly equal; display should preserve user-facing lexical input")
	}
}

func createWindowsJunction(t *testing.T, link, target string) {
	t.Helper()
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", `$ErrorActionPreference = 'Stop'; New-Item -ItemType Junction -Path $env:LOOPCODER_JUNCTION_LINK -Target $env:LOOPCODER_JUNCTION_TARGET | Out-Null`)
	cmd.Env = append(os.Environ(),
		"LOOPCODER_JUNCTION_LINK="+link,
		"LOOPCODER_JUNCTION_TARGET="+target,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("create Windows junction %q -> %q: %v: %s", link, target, err, strings.TrimSpace(string(output)))
	}
}
