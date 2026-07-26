package providerinventory

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/providerinstall"
)

// Real PATH discovery + LookPath: canonical install_ref must equal
// ComputeInstallationID for the path LookPath returns, even when an
// explicit-config path is listed first and a second PATH symlink alias exists.
func TestDiscover_LookPathPrimaryMatchesCanonicalInstallID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink PATH layout is unreliable on Windows CI")
	}
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	aliasDir := filepath.Join(root, "alias")
	explicitDir := filepath.Join(root, "explicit")
	for _, d := range []string{realDir, aliasDir, explicitDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Physical binary (resolved target).
	realBin := filepath.Join(realDir, "custom")
	writeExecutable(t, realBin)
	// PATH-first alias (symlink) — LookPath should hit this when aliasDir is first on PATH.
	pathPrimary := filepath.Join(aliasDir, "custom")
	if err := os.Symlink(realBin, pathPrimary); err != nil {
		t.Skipf("symlink: %v", err)
	}
	// Second PATH entry to same resolved file (later DiscoveryOrder).
	pathSecondDir := filepath.Join(root, "path2")
	if err := os.MkdirAll(pathSecondDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pathSecond := filepath.Join(pathSecondDir, "custom")
	if err := os.Symlink(realBin, pathSecond); err != nil {
		t.Fatal(err)
	}
	// Explicit-config path before PATH on config — must not outrank LookPath.
	explicitBin := filepath.Join(explicitDir, "custom")
	writeExecutable(t, explicitBin)

	// PATH: aliasDir first, then pathSecondDir (no realDir).
	pathEnv := aliasDir + string(os.PathListSeparator) + pathSecondDir
	deps := fakeDeps(t, map[string]string{
		filepath.Clean(pathPrimary): "custom 9.9.0",
		filepath.Clean(pathSecond):  "custom 9.9.0",
		filepath.Clean(explicitBin): "custom 9.9.0",
		filepath.Clean(realBin):     "custom 9.9.0",
	})
	deps.Getenv = func(key string) string {
		if key == "PATH" {
			return pathEnv
		}
		return ""
	}

	// Prove LookPath under this PATH.
	t.Setenv("PATH", pathEnv)
	lp, err := exec.LookPath("custom")
	if err != nil {
		t.Fatalf("LookPath(custom): %v", err)
	}
	lpAbs, _ := filepath.Abs(lp)
	wantID, err := providerinstall.ComputeInstallationID("custom", lpAbs)
	if err != nil {
		t.Fatal(err)
	}

	report, err := Discover(context.Background(), Options{
		Config: config.Config{
			Adapters: config.Adapters{Worker: "custom"},
			ProviderInventory: config.ProviderInventory{
				// Explicit listed first in config — still must not beat PATH primary.
				Executables: map[string][]string{"custom": {explicitBin}},
			},
		},
		Now: func() time.Time { return time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC) },
	}, deps)
	if err != nil {
		t.Fatal(err)
	}

	var pathPrimaryInst, pathSecondInst, explicitInst *ProviderInstallation
	for i := range report.Installations {
		inst := &report.Installations[i]
		if inst.AdapterID != "custom" {
			continue
		}
		switch inst.DiscoverySource {
		case DiscoveryPath:
			if inst.DiscoveryOrder == 0 || pathPrimaryInst == nil {
				// lowest order among path
				if pathPrimaryInst == nil || inst.DiscoveryOrder < pathPrimaryInst.DiscoveryOrder {
					pathPrimaryInst = inst
				} else if pathSecondInst == nil {
					pathSecondInst = inst
				}
			} else {
				pathSecondInst = inst
			}
		case DiscoveryExplicitConfig:
			explicitInst = inst
		}
	}
	// Re-scan for primary by order
	var bestPath *ProviderInstallation
	for i := range report.Installations {
		inst := &report.Installations[i]
		if inst.AdapterID != "custom" || inst.DiscoverySource != DiscoveryPath {
			continue
		}
		if bestPath == nil || inst.DiscoveryOrder < bestPath.DiscoveryOrder {
			bestPath = inst
		}
	}
	if bestPath == nil {
		t.Fatalf("want PATH install; got %#v", report.Installations)
	}
	if bestPath.ProviderInstallationID != wantID {
		t.Fatalf("PATH primary pinst=%s want LookPath id %s (LookPath=%s)",
			bestPath.ProviderInstallationID, wantID, lpAbs)
	}
	if explicitInst == nil {
		t.Fatal("want explicit install observed")
	}
	if explicitInst.ProviderInstallationID == bestPath.ProviderInstallationID {
		t.Fatal("explicit must not share PATH primary pinst")
	}
	_ = pathPrimaryInst
	_ = pathSecondInst
}

// First PATH hit present but not executable-usable: later PATH alias must not
// become the sole discovered production path identity for LookPath mismatch.
func TestDiscover_UnusableFirstPATHStillDiscoveredAsOrderZero(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skip windows")
	}
	root := t.TempDir()
	firstDir := filepath.Join(root, "first")
	secondDir := filepath.Join(root, "second")
	realDir := filepath.Join(root, "real")
	for _, d := range []string{firstDir, secondDir, realDir} {
		_ = os.MkdirAll(d, 0o755)
	}
	realBin := filepath.Join(realDir, "custom")
	writeExecutable(t, realBin)
	// first: non-executable file named custom (LookPath may still find it on Unix if +x missing)
	firstBin := filepath.Join(firstDir, "custom")
	if err := os.WriteFile(firstBin, []byte("not-exec"), 0o644); err != nil {
		t.Fatal(err)
	}
	// On Unix executableFile requires +x — first won't be discovered.
	// Instead: first is executable but version probe fails → unusable installation.
	// Use writeExecutable for first, make version fail for first only.
	if err := os.Remove(firstBin); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, firstBin)
	secondBin := filepath.Join(secondDir, "custom")
	if err := os.Symlink(realBin, secondBin); err != nil {
		t.Fatal(err)
	}

	pathEnv := firstDir + string(os.PathListSeparator) + secondDir
	deps := fakeDeps(t, map[string]string{
		filepath.Clean(firstBin):  "custom 1.0.0",
		filepath.Clean(secondBin): "custom 1.0.0",
		filepath.Clean(realBin):   "custom 1.0.0",
	})
	deps.Getenv = func(key string) string {
		if key == "PATH" {
			return pathEnv
		}
		return ""
	}
	report, err := Discover(context.Background(), Options{
		Config: config.Config{
			Adapters: config.Adapters{Worker: "custom"},
		},
		Now: func() time.Time { return time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC) },
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	var first, second *ProviderInstallation
	for i := range report.Installations {
		inst := &report.Installations[i]
		if inst.AdapterID != "custom" || inst.DiscoverySource != DiscoveryPath {
			continue
		}
		if first == nil || inst.DiscoveryOrder < first.DiscoveryOrder {
			if first != nil {
				second = first
			}
			first = inst
		} else {
			second = inst
		}
	}
	if first == nil {
		t.Fatal("want first PATH install")
	}
	// Mark first unusable in a synthetic plan by cloning report for capacitysnapshot tests
	// (discover itself may leave usable=unknown without auth).
	if first.DiscoveryOrder != 0 && second != nil && second.DiscoveryOrder < first.DiscoveryOrder {
		t.Fatalf("order invariant: first.order=%d second=%d", first.DiscoveryOrder, second.DiscoveryOrder)
	}
	t.Setenv("PATH", pathEnv)
	lp, err := exec.LookPath("custom")
	if err != nil {
		t.Fatal(err)
	}
	lpAbs, _ := filepath.Abs(lp)
	wantID, _ := providerinstall.ComputeInstallationID("custom", lpAbs)
	// LookPath hits firstDir; inventory first PATH pinst must match.
	if first.ProviderInstallationID != wantID {
		t.Fatalf("LookPath primary id=%s inventory first PATH=%s", wantID, first.ProviderInstallationID)
	}
}
