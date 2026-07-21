package evidence

import (
	"reflect"
	"testing"
)

func TestIsImplementationChange(t *testing.T) {
	if IsImplementationChange([]string{"docs/reference/foo.md", "ROADMAP.md"}) {
		t.Fatal("docs-only must not be implementation")
	}
	if !IsImplementationChange([]string{"docs/a.md", "internal/foo/bar.go"}) {
		t.Fatal("go under internal is implementation")
	}
	if !IsImplementationChange([]string{"cmd/loopcoder/main.go"}) {
		t.Fatal("cmd is implementation")
	}
	if IsImplementationChange([]string{"hooks/pre-push", "scripts/pre-push-sentinel.sh"}) {
		t.Fatal("hook/sentinel policy scripts are non-implementation for gate PRs")
	}
}

func TestRejectStatusPlannedWithoutAuthorization(t *testing.T) {
	issues := []LinkedIssueRef{{
		Number: 1108,
		Labels: []string{"v0.9.0", "status:planned", "phase:P2"},
		Body:   "Implementation authorization: **not granted by catalog publication**",
	}}
	d := EvaluateImplementationAuthorization(true, issues, false)
	if d.Allowed {
		t.Fatalf("planned unauthorized must be rejected: %#v", d)
	}
	if len(d.BlockedIssues) != 1 || d.BlockedIssues[0] != 1108 {
		t.Fatalf("blocked=%v", d.BlockedIssues)
	}
}

func TestAllowWhenExplicitlyAuthorizedDespitePlanned(t *testing.T) {
	issues := []LinkedIssueRef{{
		Number: 1092,
		Labels: []string{"status:planned", "status:authorized"},
		Body:   "Implementation authorization: granted by owner for stabilization gate.",
	}}
	d := EvaluateImplementationAuthorization(true, issues, false)
	if !d.Allowed {
		t.Fatalf("authorized planned should allow: %#v", d)
	}
}

func TestAllowBodyGrant(t *testing.T) {
	issues := []LinkedIssueRef{{
		Number: 42,
		Labels: []string{"status:planned"},
		Body:   "Implementation authorization: granted\n\nOwner signed off.",
	}}
	d := EvaluateImplementationAuthorization(true, issues, false)
	if !d.Allowed {
		t.Fatalf("%#v", d)
	}
}

func TestNonImplementationAlwaysAllowed(t *testing.T) {
	issues := []LinkedIssueRef{{
		Number: 1108,
		Labels: []string{"status:planned"},
	}}
	d := EvaluateImplementationAuthorization(false, issues, false)
	if !d.Allowed {
		t.Fatalf("%#v", d)
	}
}

func TestUnlinkedImplementationRejected(t *testing.T) {
	d := EvaluateImplementationAuthorization(true, nil, false)
	if d.Allowed {
		t.Fatal("expected reject")
	}
}

func TestParseClosingIssueNumbers(t *testing.T) {
	body := "Closes #1092\nFixes #1100\nAlso closes #1092 again"
	got := ParseClosingIssueNumbers(body)
	want := []int{1092, 1100}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestReadyWithoutPlannedIsAuthorized(t *testing.T) {
	issues := []LinkedIssueRef{{
		Number: 1,
		Labels: []string{"status:ready", "v0.9.0"},
		Body:   "ready for work",
	}}
	d := EvaluateImplementationAuthorization(true, issues, false)
	if !d.Allowed {
		t.Fatalf("%#v", d)
	}
}

func TestReadyPlusPlannedWithoutGrantBlocked(t *testing.T) {
	// Dual labels without explicit grant/auth label → still planned catalog item.
	issues := []LinkedIssueRef{{
		Number: 2,
		Labels: []string{"status:ready", "status:planned"},
		Body:   "not granted",
	}}
	d := EvaluateImplementationAuthorization(true, issues, false)
	if d.Allowed {
		t.Fatalf("planned must win without grant: %#v", d)
	}
}
