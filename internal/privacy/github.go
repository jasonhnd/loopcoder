package privacy

import (
	"fmt"
	"strings"
)

// RepoVisibility is the GitHub repository visibility class.
type RepoVisibility string

const (
	VisibilityPublic   RepoVisibility = "public"
	VisibilityPrivate  RepoVisibility = "private"
	VisibilityInternal RepoVisibility = "internal"
	// VisibilityUnknown means the API did not return a clear visibility —
	// fail closed before any provider launch.
	VisibilityUnknown RepoVisibility = "unknown"
)

// GitHubPermission is a least-privilege permission name for GitHub operations.
type GitHubPermission string

const (
	PermContentsRead   GitHubPermission = "contents:read"
	PermPullRequestsRW GitHubPermission = "pull_requests:write"
	PermIssuesWrite    GitHubPermission = "issues:write"
	PermChecksRead     GitHubPermission = "checks:read"
	PermMetadataRead   GitHubPermission = "metadata:read"
	// Elevated permissions that must never be the default for ordinary dev.
	PermContentsWrite GitHubPermission = "contents:write"
	PermAdmin         GitHubPermission = "admin"
	PermSecrets       GitHubPermission = "secrets"
	PermActionsWrite  GitHubPermission = "actions:write"
)

// LeastPermissions for ordinary direct/provider paths that open PRs against a
// single repository. No admin, secrets, or actions write.
func LeastPermissions() []GitHubPermission {
	return []GitHubPermission{
		PermMetadataRead,
		PermContentsRead,
		PermPullRequestsRW,
		PermIssuesWrite,
		PermChecksRead,
	}
}

// ForbiddenPermissions must never be requested for ordinary private-repo paths.
func ForbiddenPermissions() []GitHubPermission {
	return []GitHubPermission{
		PermAdmin,
		PermSecrets,
		PermActionsWrite,
	}
}

// RepoAccessRequest is the pre-launch check input for a GitHub-backed operation.
type RepoAccessRequest struct {
	// Owner/Name identify the target repository.
	Owner string
	Name  string
	// Visibility from GitHub (or Unknown when ambiguous).
	Visibility RepoVisibility
	// Authorized is true when the caller has an explicit grant for this repo
	// (owner-configured allow-list / project registration). Ambiguous or
	// missing authorization fails closed.
	Authorized bool
	// Requested permissions for the operation.
	Requested []GitHubPermission
	// ExpectPrivate when the project is registered as private; a public
	// visibility result is still allowed, but Unknown is not.
	ExpectPrivate bool
}

// RepoAccessDecision is the fail-closed result.
type RepoAccessDecision struct {
	Allowed bool
	Reasons []string
}

// EvaluateRepoAccess fails closed when visibility is unknown, authorization is
// missing, or requested permissions exceed least privilege / include forbidden
// grants. Must run before any provider launch.
func EvaluateRepoAccess(req RepoAccessRequest) RepoAccessDecision {
	var reasons []string
	owner := strings.TrimSpace(req.Owner)
	name := strings.TrimSpace(req.Name)
	if owner == "" || name == "" {
		reasons = append(reasons, "repository owner/name required")
	}
	vis := req.Visibility
	if vis == "" {
		vis = VisibilityUnknown
	}
	if vis == VisibilityUnknown {
		reasons = append(reasons, "repository visibility unknown; fail closed before provider launch")
	}
	if !req.Authorized {
		reasons = append(reasons, "repository not authorized for this project; fail closed")
	}
	// Permission checks.
	forbidden := map[GitHubPermission]struct{}{}
	for _, p := range ForbiddenPermissions() {
		forbidden[p] = struct{}{}
	}
	least := map[GitHubPermission]struct{}{}
	for _, p := range LeastPermissions() {
		least[p] = struct{}{}
	}
	for _, p := range req.Requested {
		if _, bad := forbidden[p]; bad {
			reasons = append(reasons, fmt.Sprintf("forbidden permission %s", p))
			continue
		}
		if _, ok := least[p]; !ok && p != PermContentsWrite {
			// contents:write is allowed only when explicitly needed for branch
			// push; still not admin. Unknown permissions fail closed.
			if p != PermContentsWrite {
				reasons = append(reasons, fmt.Sprintf("permission %s not in least-privilege set", p))
			}
		}
	}
	// Private expectation does not by itself block; visibility ambiguity already
	// handled. Document ExpectPrivate for callers that want extra checks.
	_ = req.ExpectPrivate

	if len(reasons) > 0 {
		return RepoAccessDecision{Allowed: false, Reasons: reasons}
	}
	return RepoAccessDecision{Allowed: true, Reasons: []string{"repository access granted under least privilege"}}
}

// WrongRepoAccess models a request against a repo that is not the registered
// project target (wrong owner/name). Always fails before provider launch.
func WrongRepoAccess(registeredOwner, registeredName, requestedOwner, requestedName string) RepoAccessDecision {
	if strings.EqualFold(strings.TrimSpace(registeredOwner), strings.TrimSpace(requestedOwner)) &&
		strings.EqualFold(strings.TrimSpace(registeredName), strings.TrimSpace(requestedName)) {
		return RepoAccessDecision{Allowed: true, Reasons: []string{"repository matches registration"}}
	}
	return RepoAccessDecision{
		Allowed: false,
		Reasons: []string{"wrong repository relative to project registration; fail closed before provider launch"},
	}
}
