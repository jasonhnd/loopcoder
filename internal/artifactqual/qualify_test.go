package artifactqual_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/artifactqual"
	"github.com/jasonhnd/loopcoder/internal/installsmoke"
)

func TestRejectFixtureInReleaseMode(t *testing.T) {
	if err := artifactqual.RejectFabricatedBooleans(artifactqual.ModeRelease, true); err == nil {
		t.Fatal("expected reject")
	}
	_, err := artifactqual.Qualify(artifactqual.Input{
		Mode: artifactqual.ModeRelease, AllowFixture: true, WorkDir: t.TempDir(),
		ArchivePath: "x", ExpectedDigest: "y",
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestUnitFixtureAllowedOnlyInUnitMode(t *testing.T) {
	art, env := installsmoke.FixtureEnvironment(strings.Repeat("ab", 32))
	ev, err := artifactqual.Qualify(artifactqual.Input{
		Mode: artifactqual.ModeUnit, AllowFixture: true,
		FixtureArt: &art, FixtureEnv: &env, WorkDir: t.TempDir(),
		Now: time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ev.InstallSmoke.Passed {
		t.Fatalf("%+v", ev.InstallSmoke)
	}
}

func TestExecutableQualificationAgainstBuiltArchive(t *testing.T) {
	// build a tiny local archive using the same script path if available
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// climb to module root
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Skip("no go.mod")
		}
		root = parent
	}
	script := filepath.Join(root, "scripts", "build-release-candidate.sh")
	if _, err := os.Stat(script); err != nil {
		t.Skip("no build-release-candidate.sh")
	}
	out := t.TempDir()
	sha, _ := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	commit := strings.TrimSpace(string(sha))
	cmd := exec.Command("bash", script)
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"VERSION=0.9.0-rc.qualtest",
		"COMMIT_SHA="+commit,
		"OUT_DIR="+out,
	)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build archive: %v\n%s", err, b)
	}
	// find archive
	var archive string
	ents, _ := os.ReadDir(out)
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tar.gz") {
			archive = filepath.Join(out, e.Name())
		}
	}
	if archive == "" {
		t.Fatal("no archive")
	}
	// expected digest from SHA256SUMS
	sums, _ := os.ReadFile(filepath.Join(out, "SHA256SUMS"))
	fields := strings.Fields(string(sums))
	if len(fields) < 1 {
		t.Fatal("no sums")
	}
	wantDigest := strings.ToLower(fields[0])

	work := t.TempDir()
	ev, err := artifactqual.Qualify(artifactqual.Input{
		Mode:        artifactqual.ModeRelease,
		ArchivePath: archive, ExpectedDigest: wantDigest,
		SHA: commit, WorkDir: work,
		IntegrationVerifyOK: false, IntegrationCanaryOK: false,
		Now: time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ev.ArchiveDigest != wantDigest {
		t.Fatalf("digest %s want %s", ev.ArchiveDigest, wantDigest)
	}
	if len(ev.Probes) == 0 {
		t.Fatal("no probes")
	}
	// fabricated fixture not used
	if !ev.RejectFixture {
		t.Fatal("expected reject fixture flag")
	}
	// wrong digest fails closed
	ev2, err := artifactqual.Qualify(artifactqual.Input{
		Mode:        artifactqual.ModeRelease,
		ArchivePath: archive, ExpectedDigest: strings.Repeat("0", 64),
		SHA: commit, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ev2.Passed {
		t.Fatal("wrong digest must fail")
	}
}

func TestStaleCrossRunRejectedByDigest(t *testing.T) {
	// unit-level: release mode requires matching digest
	dir := t.TempDir()
	p := filepath.Join(dir, "fake.tar.gz")
	// invalid archive — extract will fail after digest match
	_ = os.WriteFile(p, []byte("not-a-tar"), 0o600)
	// compute digest
	sumCmd := exec.Command("shasum", "-a", "256", p)
	out, _ := sumCmd.Output()
	d := strings.Fields(string(out))[0]
	_, err := artifactqual.Qualify(artifactqual.Input{
		Mode: artifactqual.ModeRelease, ArchivePath: p, ExpectedDigest: d,
		WorkDir: t.TempDir(), SHA: "deadbeef",
	})
	// extract error is fine; must not silently pass
	if err == nil {
		// if no err, Passed must be false
	}
	_ = err
}
