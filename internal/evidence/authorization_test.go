package evidence

import (
	"strings"
	"testing"
	"time"
)

func TestDocsOnlyAllowed(t *testing.T) {
	paths := []string{"docs/reference/foo.md", "ROADMAP.md", "README.md"}
	d := EvaluateImplementationAuthorization(paths, nil)
	if !d.Allowed {
		t.Fatalf("%#v", d)
	}
}

func TestPolicyWorkflowScriptAreImplementation(t *testing.T) {
	for _, paths := range [][]string{
		{".github/workflows/ci.yml"},
		{"hooks/pre-push"},
		{"scripts/check-implementation-authorization.sh"},
		{"internal/evidence/authorization.go"},
		{"repository_policy_test.go"},
		{"CODEOWNERS"},
	} {
		if IsDocumentationOnly(paths) {
			t.Fatalf("expected implementation for %v", paths)
		}
	}
}

func authorizedIssue(n int) ClosingIssue {
	return ClosingIssue{
		Number:         n,
		State:          "OPEN",
		Labels:         []string{ImplementationAuthorizedLabel},
		AuthLabelActor: DefaultRepositoryOwner,
		AuthLabelOK:    true,
	}
}

func TestUnlinkedImplementationRejected(t *testing.T) {
	d := EvaluateImplementationAuthorization([]string{"internal/foo/bar.go"}, nil)
	if d.Allowed || !hasReason(d, "missing_closing_issue") {
		t.Fatalf("%#v", d)
	}
}

func TestMultipleClosingIssuesRejected(t *testing.T) {
	d := EvaluateImplementationAuthorization([]string{"internal/foo/bar.go"}, []ClosingIssue{
		authorizedIssue(1), authorizedIssue(2),
	})
	if d.Allowed || !hasReason(d, "multiple_closing_issues") {
		t.Fatalf("%#v", d)
	}
}

func TestNonPlannedUnauthorizedRejected(t *testing.T) {
	d := EvaluateImplementationAuthorization([]string{"internal/foo/bar.go"}, []ClosingIssue{{
		Number: 42, State: "OPEN", Labels: []string{"status:ready"},
	}})
	if d.Allowed {
		t.Fatal("expected reject")
	}
}

func TestBodyOnlySelfAuthorizationRejected(t *testing.T) {
	d := EvaluateImplementationAuthorization([]string{"cmd/loopcoder/main.go"}, []ClosingIssue{{
		Number: 99, State: "OPEN", Labels: []string{PlannedLabel},
		Body: "Implementation authorization: granted",
	}})
	if d.Allowed || !hasReason(d, "body_text_does_not_authorize") {
		t.Fatalf("%#v", d)
	}
}

func TestPlannedWithImplementationAuthorizedAllowed(t *testing.T) {
	iss := authorizedIssue(1092)
	iss.Labels = append(iss.Labels, PlannedLabel)
	d := EvaluateImplementationAuthorization([]string{"internal/evidence/authorization.go"}, []ClosingIssue{iss})
	if !d.Allowed {
		t.Fatalf("%#v", d)
	}
}

func TestClosedIssueRejected(t *testing.T) {
	iss := authorizedIssue(5)
	iss.State = "CLOSED"
	d := EvaluateImplementationAuthorization([]string{"internal/x.go"}, []ClosingIssue{iss})
	if d.Allowed || !hasReason(d, "issue_5_not_open") {
		t.Fatalf("%#v", d)
	}
}

func TestAuthLabelNotOwnerRejected(t *testing.T) {
	iss := authorizedIssue(5)
	iss.AuthLabelOK = false
	iss.AuthLabelActor = "some-bot"
	d := EvaluateImplementationAuthorization([]string{"internal/x.go"}, []ClosingIssue{iss})
	if d.Allowed || !hasReason(d, "auth_label_not_owner_applied") {
		t.Fatalf("%#v", d)
	}
}

func TestLatestLabelApplyActor(t *testing.T) {
	base := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	events := []LabelEvent{
		{Action: "labeled", Label: ImplementationAuthorizedLabel, Actor: "attacker", CreatedAt: base},
		{Action: "unlabeled", Label: ImplementationAuthorizedLabel, Actor: "attacker", CreatedAt: base.Add(time.Minute)},
		{Action: "labeled", Label: ImplementationAuthorizedLabel, Actor: DefaultRepositoryOwner, CreatedAt: base.Add(2 * time.Minute)},
	}
	actor, ok := LatestLabelApplyActor(events, ImplementationAuthorizedLabel)
	if !ok || actor != DefaultRepositoryOwner {
		t.Fatalf("actor=%q ok=%v", actor, ok)
	}
	if !LabelAppliedByOwner(events, ImplementationAuthorizedLabel, DefaultRepositoryOwner) {
		t.Fatal("expected owner apply")
	}
	// Unlabel after owner
	events = append(events, LabelEvent{
		Action: "unlabeled", Label: ImplementationAuthorizedLabel, Actor: "attacker", CreatedAt: base.Add(3 * time.Minute),
	})
	if _, ok := LatestLabelApplyActor(events, ImplementationAuthorizedLabel); ok {
		t.Fatal("unlabeled should clear")
	}
}

func TestBootstrapExceptionRequiresAllFields(t *testing.T) {
	good := BootstrapContext{
		PRNumber: BootstrapPRNumber, HeadBranch: BootstrapHeadBranch,
		BaseBranch: BootstrapBaseBranch, BaseSHA: BootstrapBaseSHA, IssueNumber: BootstrapIssue,
	}
	if !EvaluateBootstrapException(good) {
		t.Fatal("expected match")
	}
	negatives := []BootstrapContext{
		{PRNumber: 9999, HeadBranch: BootstrapHeadBranch, BaseBranch: BootstrapBaseBranch, BaseSHA: BootstrapBaseSHA, IssueNumber: BootstrapIssue},
		{PRNumber: BootstrapPRNumber, HeadBranch: "other-branch", BaseBranch: BootstrapBaseBranch, BaseSHA: BootstrapBaseSHA, IssueNumber: BootstrapIssue},
		{PRNumber: BootstrapPRNumber, HeadBranch: BootstrapHeadBranch, BaseBranch: "main", BaseSHA: BootstrapBaseSHA, IssueNumber: BootstrapIssue},
		{PRNumber: BootstrapPRNumber, HeadBranch: BootstrapHeadBranch, BaseBranch: BootstrapBaseBranch, BaseSHA: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", IssueNumber: BootstrapIssue},
		{PRNumber: BootstrapPRNumber, HeadBranch: BootstrapHeadBranch, BaseBranch: BootstrapBaseBranch, BaseSHA: BootstrapBaseSHA, IssueNumber: 1108},
	}
	for i, n := range negatives {
		if EvaluateBootstrapException(n) {
			t.Fatalf("negative %d should fail: %#v", i, n)
		}
		g := EvaluateBaseSHAGate(n.BaseSHA, false, false, n)
		if g.Allowed {
			t.Fatalf("base gate should reject mismatched bootstrap %d: %#v", i, g)
		}
	}
	// Good bootstrap allows even without integration green.
	g := EvaluateBaseSHAGate(BootstrapBaseSHA, false, false, good)
	if !g.Allowed || !hasReasonStr(g.Reasons, "bootstrap_exception_pr_1218") {
		t.Fatalf("%#v", g)
	}
}

func TestSuspendedPromotionExceptionRequiresExactIdentity(t *testing.T) {
	good := BootstrapContext{
		PRNumber: SuspendedPromotionPRNumber, HeadBranch: SuspendedPromotionHeadBranch,
		HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BaseBranch: SuspendedPromotionBaseBranch,
		BaseSHA: SuspendedPromotionBaseSHA, IssueNumber: SuspendedPromotionIssue,
		PromotionAnchorSHA: SuspendedPromotionAnchorSHA, PromotionAnchorOK: true,
	}
	if !EvaluateSuspendedPromotionException(good) {
		t.Fatal("expected exact promotion identity to match")
	}
	negatives := []BootstrapContext{
		{PRNumber: 9999, HeadBranch: good.HeadBranch, HeadSHA: good.HeadSHA, BaseBranch: good.BaseBranch, BaseSHA: good.BaseSHA, IssueNumber: good.IssueNumber, PromotionAnchorSHA: good.PromotionAnchorSHA, PromotionAnchorOK: true},
		{PRNumber: good.PRNumber, HeadBranch: "release/other", HeadSHA: good.HeadSHA, BaseBranch: good.BaseBranch, BaseSHA: good.BaseSHA, IssueNumber: good.IssueNumber, PromotionAnchorSHA: good.PromotionAnchorSHA, PromotionAnchorOK: true},
		{PRNumber: good.PRNumber, HeadBranch: good.HeadBranch, HeadSHA: "", BaseBranch: good.BaseBranch, BaseSHA: good.BaseSHA, IssueNumber: good.IssueNumber, PromotionAnchorSHA: good.PromotionAnchorSHA, PromotionAnchorOK: true},
		{PRNumber: good.PRNumber, HeadBranch: good.HeadBranch, HeadSHA: good.HeadSHA, BaseBranch: "pre-prod", BaseSHA: good.BaseSHA, IssueNumber: good.IssueNumber, PromotionAnchorSHA: good.PromotionAnchorSHA, PromotionAnchorOK: true},
		{PRNumber: good.PRNumber, HeadBranch: good.HeadBranch, HeadSHA: good.HeadSHA, BaseBranch: good.BaseBranch, BaseSHA: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", IssueNumber: good.IssueNumber, PromotionAnchorSHA: good.PromotionAnchorSHA, PromotionAnchorOK: true},
		{PRNumber: good.PRNumber, HeadBranch: good.HeadBranch, HeadSHA: good.HeadSHA, BaseBranch: good.BaseBranch, BaseSHA: good.BaseSHA, IssueNumber: 1092, PromotionAnchorSHA: good.PromotionAnchorSHA, PromotionAnchorOK: true},
		{PRNumber: good.PRNumber, HeadBranch: good.HeadBranch, HeadSHA: good.HeadSHA, BaseBranch: good.BaseBranch, BaseSHA: good.BaseSHA, IssueNumber: good.IssueNumber, PromotionAnchorSHA: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", PromotionAnchorOK: true},
		{PRNumber: good.PRNumber, HeadBranch: good.HeadBranch, HeadSHA: good.HeadSHA, BaseBranch: good.BaseBranch, BaseSHA: good.BaseSHA, IssueNumber: good.IssueNumber, PromotionAnchorSHA: good.PromotionAnchorSHA, PromotionAnchorOK: false},
	}
	for i, n := range negatives {
		if EvaluateSuspendedPromotionException(n) {
			t.Fatalf("negative %d should fail: %#v", i, n)
		}
		if EvaluateBaseSHAGate(n.BaseSHA, false, false, n).Allowed {
			t.Fatalf("base gate should reject mismatched promotion %d", i)
		}
	}
	g := EvaluateBaseSHAGate(SuspendedPromotionBaseSHA, false, false, good)
	if !g.Allowed || !hasReasonStr(g.Reasons, "suspended_promotion_exception_pr_1453") {
		t.Fatalf("%#v", g)
	}
}

func TestBaseSHARequiresBothIntegrationChecks(t *testing.T) {
	empty := BootstrapContext{}
	if EvaluateBaseSHAGate("abc", true, false, empty).Allowed {
		t.Fatal("canary missing")
	}
	if EvaluateBaseSHAGate("abc", false, true, empty).Allowed {
		t.Fatal("verify missing")
	}
	if !EvaluateBaseSHAGate("abc", true, true, empty).Allowed {
		t.Fatal("both green")
	}

	// One-time canary remediation base: verify OK required; canary may be red.
	rem := EvaluateBaseSHAGate(CanaryRemediationBaseSHA, true, false, empty)
	if !rem.Allowed {
		t.Fatalf("canary remediation base must allow with verify green: %#v", rem)
	}
	if !hasReasonStr(rem.Reasons, "canary_remediation_base_734bc26") {
		t.Fatalf("expected remediation reason, got %v", rem.Reasons)
	}
	if EvaluateBaseSHAGate(CanaryRemediationBaseSHA, false, false, empty).Allowed {
		t.Fatal("canary remediation must still require integration-verify")
	}
	if EvaluateBaseSHAGate("ffffffffffffffffffffffffffffffffffffffff", true, false, empty).Allowed {
		t.Fatal("unrelated base must not inherit canary remediation")
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
