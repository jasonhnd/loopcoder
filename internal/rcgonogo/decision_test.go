package rcgonogo_test

import (
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/installsmoke"
	"github.com/jasonhnd/loopcoder/internal/rcgonogo"
	"github.com/jasonhnd/loopcoder/internal/releaseslo"
)

func fixed() time.Time {
	return time.Date(2026, 7, 22, 18, 0, 0, 0, time.UTC)
}

func greenScorecard() releaseslo.Scorecard {
	return releaseslo.Scorecard{GO: true, Overall: releaseslo.VerdictPass, EvidenceLinks: []string{"sc:1"}}
}

func greenSmoke(digest string) installsmoke.Report {
	return installsmoke.Report{Passed: true, ArchiveDigest: digest, Schema: installsmoke.SchemaReport}
}

func baseInput() rcgonogo.Input {
	digest := "aaaabbbbccccddddeeeeffff0000111122223333444455556666777788889999"
	return rcgonogo.Input{
		SHA: "sha1sha1sha1sha1sha1sha1sha1sha1sha1sha1", ArchiveDigest: digest,
		IntegrationVerifyOK: true, IntegrationCanaryOK: true,
		Scorecard: greenScorecard(), InstallSmoke: greenSmoke(digest),
		Canaries:   rcgonogo.AllCanariesPass(),
		SecurityOK: true, SBOMPresent: true, DocsCapabilityOK: true, MigrationOK: true,
		Operator: "jasonhnd", OperatorApproved: true, ProtectedEnvApproved: true,
		RollbackLimitations: []string{"restore backup stores; no binary rollback"},
		Now:                 fixed(),
	}
}

func TestGO(t *testing.T) {
	r := rcgonogo.Evaluate(baseInput())
	if r.Decision != rcgonogo.DecisionGO || !r.PublishAllowed {
		t.Fatalf("%#v", r)
	}
	if r.CanariesPass != r.CanariesTotal || r.CanariesTotal != 9 {
		t.Fatalf("canaries %d/%d", r.CanariesPass, r.CanariesTotal)
	}
}

func TestNOGOOpenP0(t *testing.T) {
	in := baseInput()
	in.OpenDefects = []rcgonogo.Defect{{ID: "bug-1", Severity: "P0", Title: "crash"}}
	r := rcgonogo.Evaluate(in)
	if r.Decision != rcgonogo.DecisionNOGO || r.PublishAllowed {
		t.Fatalf("%#v", r)
	}
}

func TestNOGOMissingCanary(t *testing.T) {
	in := baseInput()
	in.Canaries = in.Canaries[:5]
	r := rcgonogo.Evaluate(in)
	if r.Decision != rcgonogo.DecisionNOGO {
		t.Fatal(r.Decision)
	}
}

func TestGOWithoutProtectedEnvNotPublishable(t *testing.T) {
	in := baseInput()
	in.ProtectedEnvApproved = false
	r := rcgonogo.Evaluate(in)
	if r.Decision != rcgonogo.DecisionGO {
		t.Fatalf("still GO: %#v", r)
	}
	if r.PublishAllowed {
		t.Fatal("publish must wait protected env")
	}
}

func TestDigestMismatch(t *testing.T) {
	in := baseInput()
	in.InstallSmoke.ArchiveDigest = "other"
	r := rcgonogo.Evaluate(in)
	if r.Decision != rcgonogo.DecisionNOGO {
		t.Fatal(r.Decision)
	}
}

func TestRequiredCanaries(t *testing.T) {
	if len(rcgonogo.RequiredCanaries()) != 9 {
		t.Fatal(len(rcgonogo.RequiredCanaries()))
	}
}
