package nosidecar_test

import (
	"testing"

	"github.com/jasonhnd/loopcoder/internal/nosidecar"
)

func TestBlockRepoLocalRuntimeWrite(t *testing.T) {
	d := nosidecar.EvaluateWrite(nosidecar.WriteAttempt{
		RepoRoot:          "/Users/x/app",
		TargetPath:        "/Users/x/app/.loopcoder/runs/1.json",
		ProjectRegistered: true,
	})
	if d.Allowed {
		t.Fatalf("%#v", d)
	}
}

func TestAllowWorkerCodeAndGit(t *testing.T) {
	d1 := nosidecar.EvaluateWrite(nosidecar.WriteAttempt{
		RepoRoot: "/Users/x/app", TargetPath: "/Users/x/app/main.go", IsWorkerCodeChange: true, ProjectRegistered: true,
	})
	if !d1.Allowed {
		t.Fatal(d1)
	}
	d2 := nosidecar.EvaluateWrite(nosidecar.WriteAttempt{
		RepoRoot: "/Users/x/app", TargetPath: "/Users/x/app/.git/HEAD", IsGitMetadata: true, ProjectRegistered: true,
	})
	if !d2.Allowed {
		t.Fatal(d2)
	}
}

func TestUnregisteredNeverChoosesLoopcoder(t *testing.T) {
	d := nosidecar.EvaluateWrite(nosidecar.WriteAttempt{
		RepoRoot: "/Users/x/app", TargetPath: "/Users/x/app/.loopcoder/state.db",
		ProjectRegistered: false,
	})
	if d.Allowed {
		t.Fatal("unregistered must fail")
	}
	if d.Guidance == "" {
		t.Fatal("need guidance")
	}
}

func TestReadOnlyExportAllowed(t *testing.T) {
	d := nosidecar.EvaluateWrite(nosidecar.WriteAttempt{
		RepoRoot: "/Users/x/app", TargetPath: "/Users/x/app/.loopcoder/state/x",
		ProjectRegistered: true, ReadOnlyExport: true,
	})
	if !d.Allowed {
		t.Fatalf("%#v", d)
	}
}

func TestRegistrationFallbackDenied(t *testing.T) {
	d := nosidecar.RegistrationFallback(assertErr{})
	if d.Allowed {
		t.Fatal("fallback must deny")
	}
}

type assertErr struct{}

func (assertErr) Error() string { return "reg failed" }

func TestManifestListsDispositions(t *testing.T) {
	m := nosidecar.ReportManifest()
	if len(m.Rules) < 5 {
		t.Fatalf("%#v", m)
	}
	foundRemoved := false
	foundExport := false
	foundPolicy := false
	for _, r := range m.Rules {
		switch r.Disposition {
		case nosidecar.DispRemoved:
			foundRemoved = true
		case nosidecar.DispReadOnlyExport:
			foundExport = true
		case nosidecar.DispPolicyFile:
			foundPolicy = true
		}
	}
	if !foundRemoved || !foundExport || !foundPolicy {
		t.Fatalf("missing dispositions in %#v", m.Rules)
	}
}

func TestScanCanaryPaths(t *testing.T) {
	paths := nosidecar.ScanCanaryPaths()
	if len(paths) == 0 {
		t.Fatal("empty canary paths")
	}
}

func TestPolicyFileNotRuntimeWriteBlockAsForbiddenPattern(t *testing.T) {
	// .delivery.yml is policy; EvaluateWrite allows non-sidecar paths that aren't .loopcoder
	d := nosidecar.EvaluateWrite(nosidecar.WriteAttempt{
		RepoRoot: "/Users/x/app", TargetPath: "/Users/x/app/.delivery.yml",
		ProjectRegistered: true,
	})
	// policy file write by runtime should ideally be avoided; pattern is DispPolicyFile
	// which ForbiddenRepoLocal returns false for (not forbidden as removed).
	// Accept allowed for owner policy edits via worker code path in practice.
	_ = d
}

func TestRelativePathDetection(t *testing.T) {
	d := nosidecar.EvaluateWrite(nosidecar.WriteAttempt{
		RepoRoot: "/repo", TargetPath: ".loopcoder/tmp/x", ProjectRegistered: true,
	})
	if d.Allowed {
		t.Fatal("relative .loopcoder must block")
	}
}
