package taskroute

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capclass"
	"github.com/jasonhnd/loopcoder/internal/depthpolicy"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

// RouteRequirement is the immutable routing slice derived from a TaskRequirement.
type RouteRequirement struct {
	TaskClass    capclass.Class
	Difficulty   depthpolicy.Difficulty
	QualityFloor taskrequirements.QualityFloor
	RiskTier     taskrequirements.RiskTier
	Permission   taskrequirements.Permission
	Requirement  taskrequirements.TaskRequirement
}

// FromRequirement maps a classified TaskRequirement to routing fields.
func FromRequirement(req taskrequirements.TaskRequirement) RouteRequirement {
	out := RouteRequirement{
		TaskClass:    capclass.ClassTera,
		Difficulty:   depthpolicy.DifficultyStandard,
		QualityFloor: req.QualityFloor,
		RiskTier:     req.RiskTier,
		Permission:   req.PermissionRequired,
		Requirement:  req,
	}
	// Quality + risk drive model class floor.
	switch {
	case req.RiskTier == taskrequirements.RiskCritical ||
		req.QualityFloor == taskrequirements.QualityAdversarial:
		out.TaskClass = capclass.ClassSoul
		out.Difficulty = depthpolicy.DifficultyHard
	case req.RiskTier == taskrequirements.RiskHigh ||
		req.QualityFloor == taskrequirements.QualityStrong:
		out.TaskClass = capclass.ClassSoul
		out.Difficulty = depthpolicy.DifficultyHard
	case req.RiskTier == taskrequirements.RiskMedium:
		out.TaskClass = capclass.ClassTera
		out.Difficulty = depthpolicy.DifficultyStandard
	default:
		out.TaskClass = capclass.ClassTera
		out.Difficulty = depthpolicy.DifficultyStandard
	}
	// Docs-primary work: prefer low-cost/low-depth even when write scopes raise risk.
	if isDocsOnly(req) && !hasScopeFlags(req, "security", "credential", "secret") {
		out.TaskClass = capclass.ClassLuna
		out.Difficulty = depthpolicy.DifficultyTiny
	}
	// Ambiguous / security / migration scope → hard (overrides docs-only).
	if hasScopeFlags(req, "security", "migration", "ambiguous", "architecture", "credential", "secret") {
		out.TaskClass = maxClass(out.TaskClass, capclass.ClassSoul)
		out.Difficulty = depthpolicy.DifficultyHard
	}
	return out
}

// ClassifyRun builds a TaskRequirement from run command evidence and maps it.
func ClassifyRun(projectID, issue, title, permission string, now time.Time) (RouteRequirement, error) {
	if now.IsZero() {
		return RouteRequirement{}, fmt.Errorf("taskroute: now required")
	}
	issueNum := 0
	if n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(issue), "#")); err == nil {
		issueNum = n
	}
	scope := taskrequirements.Scope{
		Issues:               nonZero(issueNum),
		AllowsRepoWrite:      true,
		AllowsGitHubWrite:    true,
		AllowsProviderLaunch: true,
	}
	// Heuristic flags from title/permission text
	blob := strings.ToLower(title + " " + issue + " " + permission)
	if strings.Contains(blob, "doc") || strings.Contains(blob, "readme") || strings.Contains(blob, "typo") {
		scope.Documentation = true
	}
	if strings.Contains(blob, "test") {
		scope.Tests = true
	}
	if strings.Contains(blob, "security") || strings.Contains(blob, "auth") || strings.Contains(blob, "credential") {
		scope.SecuritySensitive = true
	}
	if strings.Contains(blob, "migrat") {
		// migration flag via large/ambiguous + security-ish
		scope.LargeChange = true
		scope.Ambiguous = true
	}
	if strings.Contains(blob, "architect") {
		scope.LargeChange = true
		scope.Ambiguous = true
	}
	if title == "" {
		title = "issue " + strings.TrimSpace(issue)
	}
	proj := strings.TrimSpace(projectID)
	if proj == "" {
		proj = "local-project"
	}
	taskID := strings.TrimSpace(issue)
	if taskID == "" {
		taskID = "unknown"
	}
	runID := "run-classify-" + taskID
	planFP := "run-plan|" + proj + "|" + taskID
	req, err := taskrequirements.Classify(taskrequirements.ClassificationInput{
		ProjectID:       proj,
		DeliveryRunID:   runID,
		TaskID:          taskID,
		TaskKey:         "run:" + taskID,
		Title:           title,
		IntentSummary:   title,
		RoleKey:         "worker",
		PlanFingerprint: planFP,
		Scope:           scope,
		Now:             now.UTC(),
	})
	if err != nil {
		return RouteRequirement{}, err
	}
	return FromRequirement(req), nil
}

func isDocsOnly(req taskrequirements.TaskRequirement) bool {
	blob := strings.ToLower(req.ScopeJSON + " " + strings.Join(req.ClassificationRules, " "))
	return strings.Contains(blob, "documentation") ||
		strings.Contains(blob, "scope.docs-only") ||
		strings.Contains(blob, `"documentation":true`)
}

func hasScopeFlags(req taskrequirements.TaskRequirement, needles ...string) bool {
	blob := strings.ToLower(req.ScopeJSON + " " + strings.Join(req.ClassificationRules, " "))
	for _, n := range needles {
		if strings.Contains(blob, n) {
			return true
		}
	}
	return false
}

func maxClass(a, b capclass.Class) capclass.Class {
	rank := map[capclass.Class]int{
		capclass.ClassLuna: 1, capclass.ClassTera: 2, capclass.ClassSoul: 3, capclass.ClassNeedsHuman: 4,
	}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func nonZero(n int) []int {
	if n <= 0 {
		return nil
	}
	return []int{n}
}
