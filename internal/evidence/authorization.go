// Package evidence — implementation authorization for ordinary development.
//
// Trust boundary (honest): PR CI executes code from the PR branch. A hostile PR
// can attempt to weaken the checker on that same branch. This policy is
// repository-enforced only when combined with CODEOWNERS review, a non-admin
// agent identity, and branch protection that the PR cannot bypass. It is not
// tamper-proof against an admin or against self-approving authors.
//
// Fail-closed rules for non-documentation PRs:
//  1. Exactly one GitHub closingIssuesReference is required.
//  2. That issue must carry the owner-applied label implementation-authorized.
//  3. status:ready, absence of status:planned, and body text never authorize.
//  4. status:planned may proceed only together with implementation-authorized.
package evidence

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ImplementationAuthorizedLabel is the only owner-applied grant label.
const ImplementationAuthorizedLabel = "implementation-authorized"

// PlannedLabel is the catalog "published but not authorized" label.
const PlannedLabel = "status:planned"

// ClosingIssue is one GitHub closingIssuesReference (from the GraphQL/API field).
// Callers must populate this from GitHub's closingIssuesReferences — not from
// a local regex over PR bodies.
type ClosingIssue struct {
	Number int
	Title  string
	Labels []string
	Body   string
}

// AuthorizationDecision is the pure policy result for one PR.
type AuthorizationDecision struct {
	Allowed       bool
	Reasons       []string
	BlockedIssues []int
}

// IsDocumentationOnly reports whether every changed path is pure documentation.
//
// Policy, workflow, hooks, scripts, evidence/policy tests, and any Go code are
// NOT documentation and return false.
func IsDocumentationOnly(paths []string) bool {
	if len(paths) == 0 {
		// Empty diff is not an implementation land; treat as non-doc for fail-closed
		// callers that still require a closing issue when not docs-only.
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
	// Explicit non-doc roots (policy / control plane).
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
		strings.HasSuffix(p, ".go"),
		strings.HasSuffix(p, ".yml"),
		strings.HasSuffix(p, ".yaml"),
		strings.HasSuffix(p, ".sh"),
		strings.HasSuffix(p, ".json") && !strings.HasPrefix(p, "docs/"):
		return false
	}

	base := filepath.Base(p)
	// Root markdown / license / skill manuals are documentation.
	switch base {
	case "README.md", "ROADMAP.md", "CHANGELOG.md", "LICENSE",
		"AGENTS.md", "GEMINI.md", "SKILL.md", "DESIGN.md", "PROCESS.md":
		return true
	}
	if strings.HasPrefix(p, "docs/") && (strings.HasSuffix(p, ".md") || strings.HasSuffix(p, ".json")) {
		return true
	}
	// Any other path: fail closed as non-doc.
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

// IssueAuthorized reports whether the issue carries implementation-authorized.
// Body text and status:ready are intentionally ignored.
func IssueAuthorized(labels []string, _ string /* body ignored */) bool {
	return HasLabel(labels, ImplementationAuthorizedLabel)
}

// EvaluateImplementationAuthorization decides whether a PR may land.
//
// closing must be the list from GitHub closingIssuesReferences (already
// resolved). Zero or multiple entries fail closed for implementation PRs.
func EvaluateImplementationAuthorization(paths []string, closing []ClosingIssue) AuthorizationDecision {
	d := AuthorizationDecision{Allowed: true}

	if IsDocumentationOnly(paths) {
		d.Reasons = append(d.Reasons, "documentation_only_exempt")
		return d
	}

	// Implementation / policy / workflow / scripts / hooks / config.
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
	if !IssueAuthorized(issue.Labels, issue.Body) {
		d.Allowed = false
		d.BlockedIssues = append(d.BlockedIssues, issue.Number)
		d.Reasons = append(d.Reasons, fmt.Sprintf("issue_%d_missing_implementation_authorized_label", issue.Number))
		if HasLabel(issue.Labels, PlannedLabel) {
			d.Reasons = append(d.Reasons, fmt.Sprintf("issue_%d_status_planned", issue.Number))
		}
		// Explicitly note that body grants are ignored.
		if strings.Contains(strings.ToLower(issue.Body), "implementation authorization") {
			d.Reasons = append(d.Reasons, "body_text_does_not_authorize")
		}
		return d
	}

	// Authorized (including planned + implementation-authorized).
	if HasLabel(issue.Labels, PlannedLabel) {
		d.Reasons = append(d.Reasons, fmt.Sprintf("issue_%d_planned_with_implementation_authorized", issue.Number))
	} else {
		d.Reasons = append(d.Reasons, fmt.Sprintf("issue_%d_implementation_authorized", issue.Number))
	}
	d.Reasons = append(d.Reasons, "implementation_authorized")
	return d
}

// BaseSHAIntegrationGate describes whether a PR base pre-prod SHA is allowed.
type BaseSHAIntegrationGate struct {
	// Allowed when base SHA passed integration-verify + integration-canary,
	// or a documented one-time bootstrap exception applies.
	Allowed bool
	Reasons []string
}

// EvaluateBaseSHAGate fails closed unless the exact pre-prod base SHA is green
// or the narrow bootstrap exception is active.
//
// bootstrapException may be true only for the one-time stabilization PR that
// closes #1092 (caller enforces that constraint).
func EvaluateBaseSHAGate(baseSHA string, integrationVerifyOK, integrationCanaryOK, bootstrapException bool) BaseSHAIntegrationGate {
	g := BaseSHAIntegrationGate{Allowed: true}
	baseSHA = strings.TrimSpace(baseSHA)
	if baseSHA == "" {
		g.Allowed = false
		g.Reasons = append(g.Reasons, "missing_base_sha")
		return g
	}
	if bootstrapException {
		g.Reasons = append(g.Reasons, "bootstrap_exception_issue_1092")
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
