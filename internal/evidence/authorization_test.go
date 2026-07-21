package evidence

import (
	"strings"
	"testing"
)

func TestDocsOnlyAllowed(t *testing.T) {
	paths := []string{"docs/reference/foo.md", "ROADMAP.md", "README.md"}
	if !IsDocumentationOnly(paths) {
		t.Fatal("expected documentation only")
	}
	d := EvaluateImplementationAuthorization(paths, nil)
	if !d.Allowed {
		t.Fatalf("docs-only must be allowed: %#v", d)
	}
}

func TestPolicyWorkflowScriptAreImplementation(t *testing.T) {
	cases := [][]string{
		{".github/workflows/ci.yml"},
		{"hooks/pre-push"},
		{"scripts/check-implementation-authorization.sh"},
		{"internal/evidence/authorization.go"},
		{"repository_policy_test.go"},
		{"scripts/cmd/checkimplauth/main.go"},
		{".delivery.yml"},
	}
	for _, paths := range cases {
		if IsDocumentationOnly(paths) {
			t.Fatalf("expected implementation for %v", paths)
		}
		if !IsImplementationChange(paths) {
			t.Fatalf("expected IsImplementationChange for %v", paths)
		}
	}
}

func TestUnlinkedImplementationRejected(t *testing.T) {
	paths := []string{"internal/foo/bar.go"}
	d := EvaluateImplementationAuthorization(paths, nil)
	if d.Allowed {
		t.Fatal("unlinked implementation must be rejected")
	}
	if !hasReason(d, "missing_closing_issue") {
		t.Fatalf("reasons=%v", d.Reasons)
	}
}

func TestMultipleClosingIssuesRejected(t *testing.T) {
	paths := []string{"internal/foo/bar.go"}
	closing := []ClosingIssue{
		{Number: 1, Labels: []string{ImplementationAuthorizedLabel}},
		{Number: 2, Labels: []string{ImplementationAuthorizedLabel}},
	}
	d := EvaluateImplementationAuthorization(paths, closing)
	if d.Allowed {
		t.Fatal("multiple closing issues must be rejected")
	}
	if !hasReason(d, "multiple_closing_issues") {
		t.Fatalf("reasons=%v", d.Reasons)
	}
}

func TestNonPlannedUnauthorizedRejected(t *testing.T) {
	// No status:planned, but also no implementation-authorized.
	paths := []string{"internal/foo/bar.go"}
	closing := []ClosingIssue{{
		Number: 42,
		Labels: []string{"status:ready", "v0.9.0", "kind:code"},
		Body:   "ready for work but not labeled implementation-authorized",
	}}
	d := EvaluateImplementationAuthorization(paths, closing)
	if d.Allowed {
		t.Fatal("non-planned without implementation-authorized must be rejected")
	}
	if !hasReason(d, "issue_42_missing_implementation_authorized_label") {
		t.Fatalf("reasons=%v", d.Reasons)
	}
}

func TestBodyOnlySelfAuthorizationRejected(t *testing.T) {
	paths := []string{"cmd/loopcoder/main.go"}
	closing := []ClosingIssue{{
		Number: 99,
		Labels: []string{PlannedLabel},
		Body:   "Implementation authorization: granted\n\nSelf-granted in body.",
	}}
	d := EvaluateImplementationAuthorization(paths, closing)
	if d.Allowed {
		t.Fatal("body-only grant must not authorize")
	}
	if !hasReason(d, "body_text_does_not_authorize") {
		t.Fatalf("expected body_text_does_not_authorize in %v", d.Reasons)
	}
}

func TestPlannedWithImplementationAuthorizedAllowed(t *testing.T) {
	paths := []string{"internal/evidence/authorization.go", ".github/workflows/ci.yml"}
	closing := []ClosingIssue{{
		Number: 1092,
		Labels: []string{PlannedLabel, ImplementationAuthorizedLabel, "v0.9.0"},
		Body:   "still planned catalog text; label grants work",
	}}
	d := EvaluateImplementationAuthorization(paths, closing)
	if !d.Allowed {
		t.Fatalf("planned + implementation-authorized must allow: %#v", d)
	}
}

func TestReadyLabelDoesNotAuthorize(t *testing.T) {
	paths := []string{"internal/x/y.go"}
	closing := []ClosingIssue{{
		Number: 7,
		Labels: []string{"status:ready"},
		Body:   "",
	}}
	d := EvaluateImplementationAuthorization(paths, closing)
	if d.Allowed {
		t.Fatal("status:ready alone must not authorize")
	}
}

func TestBaseSHAGate(t *testing.T) {
	g := EvaluateBaseSHAGate("abc", false, false, false)
	if g.Allowed {
		t.Fatal("expected fail")
	}
	g = EvaluateBaseSHAGate("abc", true, true, false)
	if !g.Allowed {
		t.Fatalf("%#v", g)
	}
	g = EvaluateBaseSHAGate("abc", false, false, true)
	if !g.Allowed || !hasReasonStr(g.Reasons, "bootstrap_exception_issue_1092") {
		t.Fatalf("%#v", g)
	}
}

func hasReason(d AuthorizationDecision, sub string) bool {
	return hasReasonStr(d.Reasons, sub)
}

func hasReasonStr(reasons []string, sub string) bool {
	for _, r := range reasons {
		if r == sub || strings.Contains(r, sub) {
			return true
		}
	}
	return false
}
