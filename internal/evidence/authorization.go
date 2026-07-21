// Package evidence helpers for implementation authorization (v0.9.0 stabilization).
//
// Catalog publication alone never authorizes implementation. Issues labeled
// status:planned are blocked until an explicit authorization signal is present.
package evidence

import (
	"fmt"
	"regexp"
	"strings"
)

// PlannedLabel is the catalog label that means "published, not authorized".
const PlannedLabel = "status:planned"

// Authorized labels that explicitly grant implementation work.
var AuthorizedLabels = []string{
	"status:authorized",
	"implementation-authorized",
	"status:ready", // catalog ready-for-implementation (still needs non-planned)
}

// authorizationGrantedRe matches explicit grant text in issue or PR bodies.
var authorizationGrantedRe = regexp.MustCompile(`(?i)implementation\s+authorization\s*:\s*\**\s*granted\b`)

// LinkedIssueRef is a GitHub issue referenced by a PR (Closes/Fixes/etc.).
type LinkedIssueRef struct {
	Number int
	Title  string
	Labels []string
	Body   string
	// Kind is "code" when the PR is treated as an implementation PR.
	// Callers set this based on changed paths.
}

// AuthorizationDecision is the pure policy result for one PR.
type AuthorizationDecision struct {
	// Allowed is true when the PR may proceed under ordinary-development policy.
	Allowed bool
	// Reasons explain allow or deny (stable tokens for tests and CI logs).
	Reasons []string
	// BlockedIssues lists planned issues that lack authorization.
	BlockedIssues []int
}

// IsImplementationChange reports whether any changed path is product
// implementation (not pure docs/meta). Stabilization-only doc PRs are not
// implementation changes.
func IsImplementationChange(paths []string) bool {
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// Normalize
		p = strings.ReplaceAll(p, "\\", "/")
		switch {
		case strings.HasPrefix(p, "docs/"),
			strings.HasPrefix(p, ".github/"),
			strings.HasPrefix(p, "hooks/"),
			p == "ROADMAP.md",
			p == "README.md",
			p == "CHANGELOG.md",
			p == "AGENTS.md",
			p == "GEMINI.md",
			p == "SKILL.md",
			p == "LICENSE",
			p == ".delivery.yml",
			// Gate/policy scripts and their pure unit tests are control-plane, not product features.
			strings.HasPrefix(p, "scripts/pre-push"),
			strings.HasPrefix(p, "scripts/check-implementation"),
			strings.HasPrefix(p, "scripts/assert-pre-prod"),
			strings.HasPrefix(p, "scripts/cmd/checkimplauth/"),
			strings.HasPrefix(p, "internal/evidence/"),
			strings.HasSuffix(p, "_test.go") && strings.Contains(p, "evidence"),
			p == "evidence_sentinel_test.go",
			p == "repository_policy_test.go",
			strings.HasSuffix(p, ".md"):
			continue
		case strings.HasPrefix(p, "internal/"),
			strings.HasPrefix(p, "cmd/"),
			strings.HasPrefix(p, "examples/"),
			strings.HasSuffix(p, ".go"),
			p == "go.mod",
			p == "go.sum":
			return true
		default:
			// Unknown path: treat as implementation to fail closed.
			if !strings.HasSuffix(p, ".md") {
				return true
			}
		}
	}
	return false
}

// IssueHasPlannedLabel reports whether labels include status:planned.
func IssueHasPlannedLabel(labels []string) bool {
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), PlannedLabel) {
			return true
		}
	}
	return false
}

// IssueExplicitlyAuthorized reports explicit implementation authorization.
//
// Authorization requires either:
//  1. an authorized label (status:authorized or implementation-authorized), or
//  2. body text matching "Implementation authorization: granted".
//
// Note: status:ready alone is NOT sufficient while status:planned is also
// present. Catalog "ready" without removing planned remains blocked.
func IssueExplicitlyAuthorized(labels []string, body string) bool {
	planned := IssueHasPlannedLabel(labels)
	hasAuthLabel := false
	for _, l := range labels {
		n := strings.ToLower(strings.TrimSpace(l))
		if n == "status:authorized" || n == "implementation-authorized" {
			hasAuthLabel = true
			break
		}
	}
	bodyGrant := authorizationGrantedRe.MatchString(body)
	if hasAuthLabel || bodyGrant {
		// Even with auth label, planned without grant body is still allowed if
		// explicit auth label present — owner deliberately dual-labeled.
		return true
	}
	// status:ready without planned is treated as authorized for implementation.
	if !planned {
		for _, l := range labels {
			if strings.EqualFold(strings.TrimSpace(l), "status:ready") {
				return true
			}
		}
	}
	_ = planned
	return false
}

// EvaluateImplementationAuthorization decides whether an ordinary-development
// PR may implement work against the linked issues.
//
// Rules:
//  1. Non-implementation PRs (docs/policy only) are always allowed.
//  2. Implementation PRs with no linked issues are allowed only when
//     allowUnlinkedImplementation is true (default false for product PRs).
//  3. Any linked issue with status:planned and without explicit authorization
//     blocks the PR.
func EvaluateImplementationAuthorization(isImplementation bool, issues []LinkedIssueRef, allowUnlinkedImplementation bool) AuthorizationDecision {
	d := AuthorizationDecision{Allowed: true}
	if !isImplementation {
		d.Reasons = append(d.Reasons, "non_implementation_change")
		return d
	}
	if len(issues) == 0 {
		if allowUnlinkedImplementation {
			d.Reasons = append(d.Reasons, "unlinked_implementation_allowed")
			return d
		}
		d.Allowed = false
		d.Reasons = append(d.Reasons, "implementation_requires_linked_authorized_issue")
		return d
	}
	for _, issue := range issues {
		if IssueHasPlannedLabel(issue.Labels) && !IssueExplicitlyAuthorized(issue.Labels, issue.Body) {
			d.Allowed = false
			d.BlockedIssues = append(d.BlockedIssues, issue.Number)
			d.Reasons = append(d.Reasons, fmt.Sprintf("issue_%d_status_planned_unauthorized", issue.Number))
			continue
		}
		if !IssueExplicitlyAuthorized(issue.Labels, issue.Body) && IssueHasPlannedLabel(issue.Labels) {
			// already handled
			continue
		}
		// Linked issue is either not planned, or planned+authorized.
		if IssueHasPlannedLabel(issue.Labels) && IssueExplicitlyAuthorized(issue.Labels, issue.Body) {
			d.Reasons = append(d.Reasons, fmt.Sprintf("issue_%d_planned_but_authorized", issue.Number))
		}
	}
	if d.Allowed {
		d.Reasons = append(d.Reasons, "implementation_authorized")
	}
	return d
}

// linkedIssueRe extracts issue numbers from closing keywords and explicit refs.
var linkedIssueRe = regexp.MustCompile(`(?i)(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?|refs?|related(?:\s+to)?|stabilization\s+for|implements?)\s*:?\s*#(\d+)`)

// ParseClosingIssueNumbers returns unique issue numbers referenced by PR body
// closing keywords and explicit ref forms used by the stabilization gate.
func ParseClosingIssueNumbers(body string) []int {
	matches := linkedIssueRe.FindAllStringSubmatch(body, -1)
	seen := map[int]struct{}{}
	var out []int
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		var n int
		for _, c := range m[1] {
			if c < '0' || c > '9' {
				n = 0
				break
			}
			n = n*10 + int(c-'0')
		}
		if n == 0 {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out
}
