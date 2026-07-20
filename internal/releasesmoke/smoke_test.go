package releasesmoke

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestSchemaAssertionsUseCurrentSchemaVersion(t *testing.T) {
	if CurrentSchema() != storage.CurrentSchemaVersion {
		t.Fatalf("CurrentSchema() = %d, want storage.CurrentSchemaVersion=%d", CurrentSchema(), storage.CurrentSchemaVersion)
	}

	freshOK := storagePlanPayload{
		DryRun: true, Applied: false, Status: "planned",
	}
	freshOK.Plan.SourceSchemaVersion = storage.CurrentSchemaVersion
	freshOK.Plan.TargetSchemaVersion = storage.CurrentSchemaVersion
	freshOK.Plan.Status = "current"
	if err := assertFreshSchemaCurrent(freshOK); err != nil {
		t.Fatalf("fresh current plan rejected: %v", err)
	}

	// Hard-coded 30 must fail when current is not 30.
	stale := freshOK
	stale.Plan.SourceSchemaVersion = 30
	stale.Plan.TargetSchemaVersion = 30
	if storage.CurrentSchemaVersion != 30 {
		if err := assertFreshSchemaCurrent(stale); err == nil {
			t.Fatal("expected stale schema 30 assertion to fail against CurrentSchemaVersion")
		}
	}

	upgradeOK := storagePlanPayload{
		DryRun: true, Applied: false, Status: "planned",
	}
	upgradeOK.Plan.SourceSchemaVersion = LegacyV07Schema
	upgradeOK.Plan.TargetSchemaVersion = storage.CurrentSchemaVersion
	upgradeOK.Plan.Status = "upgrade-required"
	upgradeOK.Plan.BackupRequired = true
	upgradeOK.Rollback.Supported = true
	upgradeOK.Rollback.RequiresOffline = true
	if err := assertUpgradePlanFromV07(upgradeOK); err != nil {
		t.Fatalf("upgrade plan rejected: %v", err)
	}

	// Hard-coded target 30 must fail when current is not 30.
	staleUpgrade := upgradeOK
	staleUpgrade.Plan.TargetSchemaVersion = 30
	if storage.CurrentSchemaVersion != 30 {
		if err := assertUpgradePlanFromV07(staleUpgrade); err == nil {
			t.Fatal("expected stale upgrade target 30 to fail")
		}
	}

	applyOK := storagePlanPayload{Status: "migrated", Applied: true}
	applyOK.Health = &struct {
		SchemaVersion int  `json:"schema_version"`
		OK            bool `json:"ok"`
	}{SchemaVersion: storage.CurrentSchemaVersion, OK: true}
	if err := assertMigratedToCurrent(applyOK, storage.CurrentSchemaVersion); err != nil {
		t.Fatalf("migrated health rejected: %v", err)
	}
}

func TestSelfBootstrapSmoke(t *testing.T) {
	if os.Getenv("LOOPCODER_SMOKE_MODE") != "self-bootstrap" {
		t.Skip("set LOOPCODER_SMOKE_MODE=self-bootstrap to run self-bootstrap smoke")
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("self-bootstrap smoke requires darwin/arm64")
	}
	repo := os.Getenv("LOOPCODER_SMOKE_REPO")
	if repo == "" {
		_, file, _, ok := runtime.Caller(0)
		if !ok {
			t.Fatal("resolve caller")
		}
		repo = filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	}
	version := os.Getenv("LOOPCODER_SMOKE_VERSION")
	if version == "" {
		version = "0.8.1"
	}
	opts := SelfBootstrapOptions{
		Repo:          repo,
		Binary:        os.Getenv("LOOPCODER_SMOKE_BINARY"),
		Version:       version,
		KeepArtifacts: os.Getenv("LOOPCODER_SMOKE_KEEP_ARTIFACTS") == "1",
	}
	if err := RunSelfBootstrap(opts); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseSmoke(t *testing.T) {
	if os.Getenv("LOOPCODER_SMOKE_MODE") != "release" {
		t.Skip("set LOOPCODER_SMOKE_MODE=release to run release smoke")
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("release smoke requires darwin/arm64")
	}
	version := os.Getenv("LOOPCODER_SMOKE_VERSION")
	if version == "" {
		t.Fatal("LOOPCODER_SMOKE_VERSION is required for release smoke")
	}
	previous := os.Getenv("LOOPCODER_SMOKE_PREVIOUS_VERSION")
	if previous == "" {
		previous = "0.7.0"
	}
	repo := os.Getenv("LOOPCODER_SMOKE_GITHUB_REPO")
	if repo == "" {
		repo = "jasonhnd/loopcoder"
	}
	source := os.Getenv("LOOPCODER_SMOKE_REPO")
	if source == "" {
		_, file, _, ok := runtime.Caller(0)
		if !ok {
			t.Fatal("resolve caller")
		}
		source = filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	}
	opts := ReleaseOptions{
		Version:         version,
		PreviousVersion: previous,
		Repo:            repo,
		SourceRepo:      source,
		KeepArtifacts:   os.Getenv("LOOPCODER_SMOKE_KEEP_ARTIFACTS") == "1",
	}
	if err := RunRelease(opts); err != nil {
		t.Fatal(err)
	}
}

func TestNoPowerShellScriptsInTree(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	scripts := filepath.Join(root, "scripts")
	entries, err := os.ReadDir(scripts)
	if err != nil {
		t.Fatalf("read scripts: %v", err)
	}
	var ps1 []string
	for _, e := range entries {
		if strings.HasSuffix(strings.ToLower(e.Name()), ".ps1") {
			ps1 = append(ps1, e.Name())
		}
	}
	if len(ps1) > 0 {
		t.Fatalf("scripts/ must not contain PowerShell files (spec 1058); found %v", ps1)
	}
}

func TestReleaseWorkflowHasNoPwsh(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	data, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read release.yml: %v", err)
	}
	text := string(data)
	for _, bad := range []string{"shell: pwsh", "pwsh ", ".ps1"} {
		if strings.Contains(text, bad) {
			t.Fatalf("release.yml must not contain %q after depowershell (spec 1058)", bad)
		}
	}
	if !strings.Contains(text, "bash scripts/release-smoke.sh") {
		t.Fatal("release.yml must invoke bash scripts/release-smoke.sh")
	}
}
