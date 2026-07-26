// Package evidence — implementation authorization for ordinary development.
//
// Trust boundary (honest): PR CI executes code from the PR branch. A hostile PR
// can attempt to weaken the checker on that same branch. This policy is
// repository-enforced only when combined with CODEOWNERS review, a non-admin
// agent identity without Issues label-write (or fork without upstream triage),
// and branch protection the agent cannot bypass. It is not tamper-proof.
//
// Fail-closed rules for non-documentation PRs:
//  1. Exactly one issue pointer (closingIssuesReferences or closes:<N> label).
//  2. That issue must be OPEN and currently carry implementation-authorized.
//  3. The latest label-apply actor for implementation-authorized must be the
//     repository owner (default jasonhnd).
//  4. If closes:<N> is used, its latest apply actor must also be the owner.
//  5. status:ready, absence of status:planned, and body text never authorize.
package evidence

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// ImplementationAuthorizedLabel is the only owner-applied grant label.
const ImplementationAuthorizedLabel = "implementation-authorized"

// PlannedLabel is the catalog "published but not authorized" label.
const PlannedLabel = "status:planned"

// DefaultRepositoryOwner is the login that may apply authorization labels.
const DefaultRepositoryOwner = "jasonhnd"

// One-time bootstrap exception for PR #1218 only (all fields required).
const (
	BootstrapPRNumber   = 1218
	BootstrapHeadBranch = "ordinary/v090-stabilization-gate"
	BootstrapBaseBranch = "pre-prod"
	BootstrapBaseSHA    = "1a6fd6bd6a87232b23db2f6fa06de299604cf57e"
	BootstrapIssue      = 1092

	// SuspendedPromotion* freezes the owner-authorized, one-time promotion of
	// the stopped v0.9 snapshot. The immutable merge anchor and exact PR/base
	// identity prevent this exception from authorizing a different promotion.
	SuspendedPromotionPRNumber   = 1453
	SuspendedPromotionHeadBranch = "release/v090-suspended-main-promotion"
	SuspendedPromotionAnchorSHA  = "ae884dea062a812e5067d891391d578c1648dc29"
	SuspendedPromotionBaseBranch = "main"
	SuspendedPromotionBaseSHA    = "9646de33ed38189c74a13e8609d5811d83b58bad"
	SuspendedPromotionIssue      = 1452

	// CanaryRemediationBaseSHA is the #1218 merge tip whose integration-canary
	// failed because the canary job lacked GH_TOKEN for doctor's hard gh-auth
	// check. Exactly this base may skip canary green for one remediation PR so
	// the gate can be repaired without deadlock. Inert once pre-prod moves on.
	CanaryRemediationBaseSHA = "734bc26cf1c6b4c84139108545e318d9cdcf33d2"
)

// ClosingIssue is one issue pointer resolved from GitHub APIs (not body text).
type ClosingIssue struct {
	Number int
	Title  string
	// State is the live issue state from GitHub ("OPEN" / "CLOSED").
	State  string
	Labels []string
	Body   string
	// AuthLabelActor is the actor of the latest implementation-authorized apply.
	AuthLabelActor string
	// AuthLabelOK is true when AuthLabelActor is trusted owner and label is live.
	AuthLabelOK bool
}

// LabelEvent is one labeled/unlabeled timeline event from GitHub.
type LabelEvent struct {
	// Action is "labeled" or "unlabeled".
	Action    string
	Label     string
	Actor     string
	CreatedAt time.Time
}

// AuthorizationDecision is the pure policy result for one PR.
type AuthorizationDecision struct {
	Allowed       bool
	Reasons       []string
	BlockedIssues []int
}

// IsDocumentationOnly reports whether every changed path is pure documentation.
func IsDocumentationOnly(paths []string) bool {
	if len(paths) == 0 {
		return false
	}
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		p = filepath.ToSlash(p)
		if !isDocPath(p) {
			return false
		}
	}
	return true
}

// IsImplementationChange is the inverse of IsDocumentationOnly for non-empty paths.
func IsImplementationChange(paths []string) bool {
	if len(paths) == 0 {
		return false
	}
	return !IsDocumentationOnly(paths)
}

func isDocPath(p string) bool {
	switch {
	case strings.HasPrefix(p, ".github/"),
		strings.HasPrefix(p, "hooks/"),
		strings.HasPrefix(p, "scripts/"),
		strings.HasPrefix(p, "internal/"),
		strings.HasPrefix(p, "cmd/"),
		strings.HasPrefix(p, "examples/"),
		p == "go.mod",
		p == "go.sum",
		p == ".delivery.yml",
		p == "repository_policy_test.go",
		p == "evidence_sentinel_test.go",
		p == "CODEOWNERS",
		strings.HasSuffix(p, ".go"),
		strings.HasSuffix(p, ".yml"),
		strings.HasSuffix(p, ".yaml"),
		strings.HasSuffix(p, ".sh"),
		strings.HasSuffix(p, ".json") && !strings.HasPrefix(p, "docs/"):
		return false
	}
	base := filepath.Base(p)
	switch base {
	case "README.md", "ROADMAP.md", "CHANGELOG.md", "LICENSE",
		"AGENTS.md", "GEMINI.md", "SKILL.md", "DESIGN.md", "PROCESS.md":
		return true
	}
	if strings.HasPrefix(p, "docs/") && (strings.HasSuffix(p, ".md") || strings.HasSuffix(p, ".json")) {
		return true
	}
	return false
}

// HasLabel reports a case-insensitive label match.
func HasLabel(labels []string, want string) bool {
	want = strings.ToLower(strings.TrimSpace(want))
	for _, l := range labels {
		if strings.ToLower(strings.TrimSpace(l)) == want {
			return true
		}
	}
	return false
}

// IssueStateOpen reports whether the issue state is open.
func IssueStateOpen(state string) bool {
	return strings.EqualFold(strings.TrimSpace(state), "open")
}

// LatestLabelApplyActor returns the actor of the most recent "labeled" event for
// label after discarding later "unlabeled" (walk newest first).
// ok is false when the label is not currently applied per event history, or no events.
func LatestLabelApplyActor(events []LabelEvent, label string) (actor string, ok bool) {
	label = strings.ToLower(strings.TrimSpace(label))
	// Sort by CreatedAt descending (caller may already order; we re-order).
	// Simple insertion by scanning for max each time is fine for small N.
	remaining := append([]LabelEvent(nil), events...)
	// Filter to this label.
	var filtered []LabelEvent
	for _, e := range remaining {
		if strings.EqualFold(strings.TrimSpace(e.Label), label) {
			filtered = append(filtered, e)
		}
	}
	if len(filtered) == 0 {
		return "", false
	}
	// Newest first.
	for i := 0; i < len(filtered); i++ {
		for j := i + 1; j < len(filtered); j++ {
			if filtered[j].CreatedAt.After(filtered[i].CreatedAt) {
				filtered[i], filtered[j] = filtered[j], filtered[i]
			}
		}
	}
	// Walk newest → oldest: first event decides current apply state.
	top := filtered[0]
	action := strings.ToLower(strings.TrimSpace(top.Action))
	if action == "unlabeled" || action == "unlabel" {
		return "", false
	}
	if action != "labeled" && action != "label" {
		return "", false
	}
	actor = strings.TrimSpace(top.Actor)
	if actor == "" {
		return "", false
	}
	return actor, true
}

// LabelAppliedByOwner reports whether the latest apply of label was by owner.
func LabelAppliedByOwner(events []LabelEvent, label, owner string) bool {
	actor, ok := LatestLabelApplyActor(events, label)
	if !ok {
		return false
	}
	return strings.EqualFold(actor, strings.TrimSpace(owner))
}

// EvaluateImplementationAuthorization decides whether a PR may land.
//
// closing pointers are untrusted until live state + owner label-actor evidence
// is attached by the caller.
func EvaluateImplementationAuthorization(paths []string, closing []ClosingIssue) AuthorizationDecision {
	d := AuthorizationDecision{Allowed: true}

	if IsDocumentationOnly(paths) {
		d.Reasons = append(d.Reasons, "documentation_only_exempt")
		return d
	}

	d.Reasons = append(d.Reasons, "implementation_or_policy_change")

	if len(closing) == 0 {
		d.Allowed = false
		d.Reasons = append(d.Reasons, "missing_closing_issue")
		return d
	}
	if len(closing) > 1 {
		d.Allowed = false
		d.Reasons = append(d.Reasons, "multiple_closing_issues")
		for _, c := range closing {
			d.BlockedIssues = append(d.BlockedIssues, c.Number)
		}
		return d
	}

	issue := closing[0]
	if !IssueStateOpen(issue.State) {
		d.Allowed = false
		d.BlockedIssues = append(d.BlockedIssues, issue.Number)
		d.Reasons = append(d.Reasons, fmt.Sprintf("issue_%d_not_open", issue.Number))
		return d
	}
	if !HasLabel(issue.Labels, ImplementationAuthorizedLabel) {
		d.Allowed = false
		d.BlockedIssues = append(d.BlockedIssues, issue.Number)
		d.Reasons = append(d.Reasons, fmt.Sprintf("issue_%d_missing_implementation_authorized_label", issue.Number))
		if strings.Contains(strings.ToLower(issue.Body), "implementation authorization") {
			d.Reasons = append(d.Reasons, "body_text_does_not_authorize")
		}
		return d
	}
	if !issue.AuthLabelOK {
		d.Allowed = false
		d.BlockedIssues = append(d.BlockedIssues, issue.Number)
		d.Reasons = append(d.Reasons, fmt.Sprintf("issue_%d_auth_label_not_owner_applied", issue.Number))
		if issue.AuthLabelActor != "" {
			d.Reasons = append(d.Reasons, fmt.Sprintf("auth_label_actor=%s", issue.AuthLabelActor))
		}
		return d
	}

	if HasLabel(issue.Labels, PlannedLabel) {
		d.Reasons = append(d.Reasons, fmt.Sprintf("issue_%d_planned_with_implementation_authorized", issue.Number))
	} else {
		d.Reasons = append(d.Reasons, fmt.Sprintf("issue_%d_implementation_authorized", issue.Number))
	}
	d.Reasons = append(d.Reasons, "implementation_authorized")
	return d
}

// BootstrapContext is the full PR identity required for the one-time exception.
type BootstrapContext struct {
	PRNumber    int
	HeadBranch  string
	HeadSHA     string
	BaseBranch  string
	BaseSHA     string
	IssueNumber int
	// PromotionAnchorOK is set only after live GitHub comparison proves that
	// PromotionAnchorSHA is equal to or an ancestor of HeadSHA.
	PromotionAnchorSHA string
	PromotionAnchorOK  bool
}

// EvaluateBootstrapException is true only when every field matches the frozen
// stabilization PR identity.
func EvaluateBootstrapException(ctx BootstrapContext) bool {
	return ctx.PRNumber == BootstrapPRNumber &&
		ctx.HeadBranch == BootstrapHeadBranch &&
		ctx.BaseBranch == BootstrapBaseBranch &&
		strings.EqualFold(strings.TrimSpace(ctx.BaseSHA), BootstrapBaseSHA) &&
		ctx.IssueNumber == BootstrapIssue
}

// EvaluateSuspendedPromotionException matches only the frozen v0.9 archival
// promotion. It does not authorize a tag, release, later head, or another PR.
func EvaluateSuspendedPromotionException(ctx BootstrapContext) bool {
	return ctx.PRNumber == SuspendedPromotionPRNumber &&
		ctx.HeadBranch == SuspendedPromotionHeadBranch &&
		strings.TrimSpace(ctx.HeadSHA) != "" &&
		ctx.BaseBranch == SuspendedPromotionBaseBranch &&
		strings.EqualFold(strings.TrimSpace(ctx.BaseSHA), SuspendedPromotionBaseSHA) &&
		ctx.IssueNumber == SuspendedPromotionIssue &&
		strings.EqualFold(strings.TrimSpace(ctx.PromotionAnchorSHA), SuspendedPromotionAnchorSHA) &&
		ctx.PromotionAnchorOK
}

// BaseSHAIntegrationGate describes whether a PR base pre-prod SHA is allowed.
type BaseSHAIntegrationGate struct {
	Allowed bool
	Reasons []string
}

// EvaluateBaseSHAGate fails closed unless the exact base SHA is green or the
// fully-matched bootstrap exception is active.
func EvaluateBaseSHAGate(baseSHA string, integrationVerifyOK, integrationCanaryOK bool, bootstrap BootstrapContext) BaseSHAIntegrationGate {
	g := BaseSHAIntegrationGate{Allowed: true}
	baseSHA = strings.TrimSpace(baseSHA)
	if baseSHA == "" {
		g.Allowed = false
		g.Reasons = append(g.Reasons, "missing_base_sha")
		return g
	}
	if EvaluateBootstrapException(bootstrap) {
		// Still require base SHA field on bootstrap context match includes SHA.
		g.Reasons = append(g.Reasons, "bootstrap_exception_pr_1218")
		return g
	}
	if EvaluateSuspendedPromotionException(bootstrap) {
		g.Reasons = append(g.Reasons, "suspended_promotion_exception_pr_1453")
		return g
	}
	// One-time canary remediation against the known #1218 merge tip.
	if strings.EqualFold(baseSHA, CanaryRemediationBaseSHA) && integrationVerifyOK {
		g.Reasons = append(g.Reasons, "canary_remediation_base_734bc26")
		if !integrationCanaryOK {
			g.Reasons = append(g.Reasons, "canary_remediation_skips_canary_green")
		}
		return g
	}
	if !integrationVerifyOK {
		g.Allowed = false
		g.Reasons = append(g.Reasons, "base_sha_missing_integration_verify")
	}
	if !integrationCanaryOK {
		g.Allowed = false
		g.Reasons = append(g.Reasons, "base_sha_missing_integration_canary")
	}
	if g.Allowed {
		g.Reasons = append(g.Reasons, "base_sha_integration_green")
	}
	return g
}
