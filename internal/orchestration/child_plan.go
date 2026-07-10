package orchestration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/state"
)

const (
	ChildPlanSchemaVersionV1 = "loopcoder.child_plan.v1"
	NestedHardMaxDepth       = 4

	ChildAggregationCollect = "collect"
	ChildAggregationGate    = "gate"
	ChildAggregationIgnore  = "ignore"
)

// ChildPlan is the versioned parent-authored envelope accepted before child
// scheduling begins.
type ChildPlan struct {
	SchemaVersion  string         `json:"schema_version"`
	PlanID         string         `json:"plan_id"`
	ParentRunID    string         `json:"parent_run_id"`
	RootRunID      string         `json:"root_run_id"`
	ParentDepth    int            `json:"parent_depth"`
	MaxDepth       int            `json:"max_depth"`
	MaxConcurrency int            `json:"max_concurrency"`
	CreatedAt      string         `json:"created_at"`
	Items          []ChildRunPlan `json:"items"`
}

type ChildScope struct {
	Repo         string   `json:"repo,omitempty"`
	Paths        []string `json:"paths,omitempty"`
	Issues       []int    `json:"issues,omitempty"`
	PullRequests []int    `json:"pull_requests,omitempty"`
	Commands     []string `json:"commands,omitempty"`
	Data         []string `json:"data,omitempty"`
}

type ChildAggregation struct {
	Mode          string `json:"mode"`
	Required      bool   `json:"required"`
	IncludeReport bool   `json:"include_report"`
}

func ParseChildPlanJSON(data []byte) (ChildPlan, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var plan ChildPlan
	if err := decoder.Decode(&plan); err != nil {
		return ChildPlan{}, fmt.Errorf("parse child plan: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return ChildPlan{}, fmt.Errorf("parse child plan: trailing JSON value")
	}
	if err := ValidateChildPlan(&plan); err != nil {
		return ChildPlan{}, err
	}
	return plan, nil
}

func ValidateChildPlan(plan *ChildPlan) error {
	if plan == nil {
		return fmt.Errorf("child plan is required")
	}
	plan.SchemaVersion = strings.TrimSpace(plan.SchemaVersion)
	if plan.SchemaVersion != ChildPlanSchemaVersionV1 {
		return fmt.Errorf("child plan schema_version must be %q", ChildPlanSchemaVersionV1)
	}
	plan.PlanID = strings.TrimSpace(plan.PlanID)
	if plan.PlanID == "" {
		return fmt.Errorf("child plan plan_id is required")
	}
	plan.ParentRunID = strings.TrimSpace(plan.ParentRunID)
	if plan.ParentRunID == "" {
		return fmt.Errorf("child plan parent_run_id is required")
	}
	plan.RootRunID = strings.TrimSpace(plan.RootRunID)
	if plan.RootRunID == "" {
		return fmt.Errorf("child plan root_run_id is required")
	}
	if plan.ParentDepth < 0 {
		return fmt.Errorf("child plan parent_depth must be non-negative")
	}
	if plan.MaxDepth <= 0 {
		plan.MaxDepth = lcdefaults.NestedSchedulerMaxDepth
	}
	if plan.MaxDepth > NestedHardMaxDepth {
		return fmt.Errorf("child plan max_depth %d exceeds hard maximum %d", plan.MaxDepth, NestedHardMaxDepth)
	}
	if plan.MaxDepth < plan.ParentDepth {
		return fmt.Errorf("child plan max_depth %d is below parent_depth %d", plan.MaxDepth, plan.ParentDepth)
	}
	if plan.ParentDepth >= plan.MaxDepth && len(plan.Items) > 0 {
		return fmt.Errorf("child plan parent_depth %d cannot create children at max_depth %d", plan.ParentDepth, plan.MaxDepth)
	}
	if plan.MaxConcurrency <= 0 {
		plan.MaxConcurrency = lcdefaults.NestedSchedulerMaxConcurrency
	}
	if strings.TrimSpace(plan.CreatedAt) == "" {
		return fmt.Errorf("child plan created_at is required")
	}
	if _, err := state.ParseTimestamp(plan.CreatedAt); err != nil {
		return fmt.Errorf("child plan %w", err)
	}
	if len(plan.Items) == 0 {
		return fmt.Errorf("child plan items must contain at least one child")
	}
	if err := normalizeAndValidateChildItems(plan); err != nil {
		return err
	}
	return nil
}

func normalizeAndValidateChildItems(plan *ChildPlan) error {
	seenKeys := map[string]int{}
	for index := range plan.Items {
		item := &plan.Items[index]
		if strings.TrimSpace(item.ChildKey) == "" && strings.TrimSpace(item.ID) != "" {
			item.ChildKey = strings.TrimSpace(item.ID)
		}
		item.ChildKey = strings.TrimSpace(item.ChildKey)
		item.ID = item.ChildKey
		if item.ChildKey == "" {
			return fmt.Errorf("child[%d].child_key is required", index)
		}
		if previous, ok := seenKeys[item.ChildKey]; ok {
			return fmt.Errorf("duplicate child_key %q at child[%d] and child[%d]", item.ChildKey, previous, index)
		}
		seenKeys[item.ChildKey] = index
		item.Ordinal = index
		item.Title = strings.TrimSpace(item.Title)
		if item.Title == "" {
			item.Title = item.ChildKey
		}
		item.Role = strings.TrimSpace(item.Role)
		if item.Role == "" {
			return fmt.Errorf("child %q role is required", item.ChildKey)
		}
		item.Permission = normalizeChildPermission(item.Permission)
		if !validChildPermission(item.Permission) {
			return fmt.Errorf("child %q permission must be one of read-only, write, orchestrate", item.ChildKey)
		}
		item.Scope = normalizeStructuredScope(item.Scope, item.Issue, item.ScopeIssues)
		item.Issue = firstPositive(item.Issue, firstScopeIssue(item.Scope))
		item.ScopeIssues = append([]int(nil), item.Scope.Issues...)
		if err := validateChildScope(item.ChildKey, item.Permission, item.Scope); err != nil {
			return err
		}
		if item.Aggregation.Mode == "" {
			if item.Optional {
				item.Aggregation = ChildAggregation{Mode: ChildAggregationCollect, Required: false, IncludeReport: true}
			} else if item.Required {
				item.Aggregation = ChildAggregation{Mode: ChildAggregationCollect, Required: true, IncludeReport: true}
			} else {
				return fmt.Errorf("child %q aggregation is required", item.ChildKey)
			}
		}
		item.Aggregation.Mode = strings.TrimSpace(item.Aggregation.Mode)
		if !validAggregationMode(item.Aggregation.Mode) {
			return fmt.Errorf("child %q aggregation.mode must be one of collect, gate, ignore", item.ChildKey)
		}
		item.Required = item.Aggregation.Required
		item.Optional = !item.Aggregation.Required
		item.DependsOn = normalizeStringList(item.DependsOn)
		if item.Depth > 0 && item.Depth > plan.MaxDepth {
			return fmt.Errorf("child %q depth %d exceeds max depth %d", item.ChildKey, item.Depth, plan.MaxDepth)
		}
		item.Depth = plan.ParentDepth + 1
		if item.Depth > plan.MaxDepth {
			return fmt.Errorf("child %q depth %d exceeds max depth %d", item.ChildKey, item.Depth, plan.MaxDepth)
		}
		if strings.TrimSpace(item.RunID) != "" && !state.IsRunID(item.RunID) {
			return fmt.Errorf("child %q run id %q is invalid", item.ChildKey, item.RunID)
		}
	}
	for _, item := range plan.Items {
		for _, dep := range item.DependsOn {
			if dep == item.ChildKey {
				return fmt.Errorf("child %q depends on itself", item.ChildKey)
			}
			if _, ok := seenKeys[dep]; !ok {
				return fmt.Errorf("child %q depends on unknown child_key %q", item.ChildKey, dep)
			}
		}
	}
	if err := detectChildDependencyCycle(plan.Items); err != nil {
		return err
	}
	return nil
}

func BuildChildPlanFromLegacy(opts NestedScheduleOptions, started time.Time) (ChildPlan, error) {
	parentRunID := strings.TrimSpace(opts.ParentRunID)
	if parentRunID == "" {
		parentRunID = state.RunIDForWave(started)
	}
	rootRunID := strings.TrimSpace(opts.RootRunID)
	if rootRunID == "" {
		rootRunID = parentRunID
	}
	maxDepth := opts.MaxDepth
	if maxDepth <= 0 {
		maxDepth = lcdefaults.NestedSchedulerMaxDepth
	}
	maxConcurrency := opts.ConcurrencyLimit
	if maxConcurrency <= 0 {
		maxConcurrency = lcdefaults.NestedSchedulerMaxConcurrency
	}
	plan := ChildPlan{
		SchemaVersion:  ChildPlanSchemaVersionV1,
		PlanID:         strings.TrimSpace(opts.PlanID),
		ParentRunID:    parentRunID,
		RootRunID:      rootRunID,
		ParentDepth:    opts.ParentDepth,
		MaxDepth:       maxDepth,
		MaxConcurrency: maxConcurrency,
		CreatedAt:      state.FormatTimestamp(started),
		Items:          cloneChildPlans(opts.Children),
	}
	if plan.PlanID == "" {
		plan.PlanID = "plan-" + parentRunID
	}
	for i := range plan.Items {
		item := &plan.Items[i]
		if strings.TrimSpace(item.ChildKey) == "" {
			item.ChildKey = strings.TrimSpace(item.ID)
		}
		if strings.TrimSpace(item.Title) == "" {
			item.Title = firstNonEmptyChild(item.ChildKey, item.ID)
		}
		if strings.TrimSpace(item.Role) == "" {
			item.Role = string(reporter.RoleWorker)
		}
		if item.Aggregation.Mode == "" && (item.Required || item.Optional) {
			item.Aggregation = ChildAggregation{
				Mode:          ChildAggregationCollect,
				Required:      item.Required,
				IncludeReport: true,
			}
		}
		if len(item.Scope.Issues) == 0 && (item.Issue > 0 || len(item.ScopeIssues) > 0) {
			item.Scope.Issues = normalizeChildScopeIssues(append([]int{item.Issue}, item.ScopeIssues...))
		}
		if strings.TrimSpace(item.Scope.Repo) == "" {
			item.Scope.Repo = "."
		}
	}
	if err := ValidateChildPlan(&plan); err != nil {
		return ChildPlan{}, err
	}
	return plan, nil
}

func cloneChildPlans(in []ChildRunPlan) []ChildRunPlan {
	out := make([]ChildRunPlan, len(in))
	copy(out, in)
	for i := range out {
		out[i].DependsOn = append([]string(nil), out[i].DependsOn...)
		out[i].ScopeIssues = append([]int(nil), out[i].ScopeIssues...)
		out[i].Scope = cloneChildScope(out[i].Scope)
		out[i].Metadata = append(json.RawMessage(nil), out[i].Metadata...)
	}
	return out
}

func cloneChildScope(scope ChildScope) ChildScope {
	return ChildScope{
		Repo:         scope.Repo,
		Paths:        append([]string(nil), scope.Paths...),
		Issues:       append([]int(nil), scope.Issues...),
		PullRequests: append([]int(nil), scope.PullRequests...),
		Commands:     append([]string(nil), scope.Commands...),
		Data:         append([]string(nil), scope.Data...),
	}
}

func normalizeChildPermission(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "read":
		return string(reporter.PermissionReadOnly)
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func validChildPermission(value string) bool {
	switch reporter.Permission(value) {
	case reporter.PermissionReadOnly, reporter.PermissionWrite, reporter.PermissionOrchestrate:
		return true
	default:
		return false
	}
}

func normalizeStructuredScope(scope ChildScope, issue int, legacyIssues []int) ChildScope {
	scope.Repo = strings.TrimSpace(scope.Repo)
	scope.Paths = normalizeStringList(scope.Paths)
	scope.Commands = normalizeStringList(scope.Commands)
	scope.Data = normalizeStringList(scope.Data)
	scope.Issues = normalizeChildScopeIssues(append(scope.Issues, append([]int{issue}, legacyIssues...)...))
	scope.PullRequests = normalizeChildScopeIssues(scope.PullRequests)
	return scope
}

func validateChildScope(childKey, permission string, scope ChildScope) error {
	if scope.Repo == "" && len(scope.Paths) == 0 && len(scope.Issues) == 0 && len(scope.PullRequests) == 0 && len(scope.Commands) == 0 && len(scope.Data) == 0 {
		return fmt.Errorf("child %q scope is required", childKey)
	}
	if permission == string(reporter.PermissionWrite) || permission == string(reporter.PermissionOrchestrate) {
		if len(scope.Paths) == 0 && len(scope.Issues) == 0 && len(scope.PullRequests) == 0 && len(scope.Data) == 0 {
			return fmt.Errorf("child %q write-capable permission requires bounded mutable scope", childKey)
		}
		for _, path := range scope.Paths {
			switch strings.TrimSpace(path) {
			case "", ".", "./", "**", "**/*", "/*", "/":
				return fmt.Errorf("child %q write-capable permission rejects unbounded path scope %q", childKey, path)
			}
		}
	}
	return nil
}

func normalizeStringList(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func normalizeChildScopeIssues(scope []int) []int {
	if len(scope) == 0 {
		return nil
	}
	seen := map[int]bool{}
	out := make([]int, 0, len(scope))
	for _, issue := range scope {
		if issue <= 0 || seen[issue] {
			continue
		}
		seen[issue] = true
		out = append(out, issue)
	}
	sort.Ints(out)
	return out
}

func detectChildDependencyCycle(items []ChildRunPlan) error {
	byKey := map[string]ChildRunPlan{}
	for _, item := range items {
		byKey[item.ChildKey] = item
	}
	const (
		unvisited = 0
		visiting  = 1
		visited   = 2
	)
	stateByKey := map[string]int{}
	var visit func(string, []string) error
	visit = func(key string, stack []string) error {
		switch stateByKey[key] {
		case visiting:
			return fmt.Errorf("child dependency cycle detected: %s -> %s", strings.Join(stack, " -> "), key)
		case visited:
			return nil
		}
		stateByKey[key] = visiting
		for _, dep := range byKey[key].DependsOn {
			if err := visit(dep, append(stack, key)); err != nil {
				return err
			}
		}
		stateByKey[key] = visited
		return nil
	}
	for _, item := range items {
		if err := visit(item.ChildKey, nil); err != nil {
			return err
		}
	}
	return nil
}

func validAggregationMode(value string) bool {
	switch value {
	case ChildAggregationCollect, ChildAggregationGate, ChildAggregationIgnore:
		return true
	default:
		return false
	}
}

func firstScopeIssue(scope ChildScope) int {
	if len(scope.Issues) == 0 {
		return 0
	}
	return scope.Issues[0]
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func firstNonEmptyChild(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
