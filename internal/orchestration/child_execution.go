package orchestration

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/pathid"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

const ChildExecutionRequestSchemaVersionV1 = "loopcoder.child_execution_request.v1"

// ChildExecutionRequest is the provider-neutral, immutable authority envelope
// passed to every nested executor. ClaimGeneration and LifecycleStatus are
// fenced runtime bindings and are intentionally excluded from the contract
// fingerprint.
type ChildExecutionRequest struct {
	SchemaVersion            string                     `json:"schema_version"`
	ParentRunID              string                     `json:"parent_run_id"`
	PlanID                   string                     `json:"plan_id"`
	ID                       string                     `json:"id"`
	ChildKey                 string                     `json:"child_key"`
	RunID                    string                     `json:"child_run_id"`
	Title                    string                     `json:"title"`
	Role                     string                     `json:"role"`
	Issue                    int                        `json:"issue,omitempty"`
	Permission               string                     `json:"permission"`
	Scope                    ChildScope                 `json:"scope"`
	RepositoryIdentity       string                     `json:"repository_identity"`
	CheckoutIdentity         string                     `json:"checkout_identity"`
	ScopedRepositoryIdentity string                     `json:"scoped_repository_identity"`
	CanonicalPaths           []string                   `json:"canonical_paths"`
	MutationScope            ChildMutationScope         `json:"allowed_mutation_scope"`
	Capabilities             ChildExecutionCapabilities `json:"capabilities"`
	DependsOn                []string                   `json:"depends_on"`
	Aggregation              ChildAggregation           `json:"aggregation"`
	Required                 bool                       `json:"required"`
	Optional                 bool                       `json:"optional"`
	Ordinal                  int                        `json:"ordinal"`
	Depth                    int                        `json:"depth"`
	IdempotencyKey           string                     `json:"idempotency_key"`
	ClaimGeneration          int64                      `json:"claim_generation"`
	ProviderDecision         ChildProviderDecisionRef   `json:"provider_decision"`
	BudgetReferences         []string                   `json:"budget_references"`
	DeadlineReference        string                     `json:"deadline_reference"`
	Work                     ChildExecutionWork         `json:"work"`
	ContractFingerprint      string                     `json:"contract_fingerprint"`
	LifecycleStatus          string                     `json:"lifecycle_status"`
}

type ChildMutationScope struct {
	Paths        []string `json:"paths"`
	Issues       []int    `json:"issues"`
	PullRequests []int    `json:"pull_requests"`
	Data         []string `json:"data"`
}

type ChildExecutionCapabilities struct {
	Network    []string `json:"network"`
	Commands   []string `json:"commands"`
	Delegation []string `json:"delegation"`
}

type ChildProviderDecisionRef struct {
	RoutingDecisionID  string `json:"routing_decision_id"`
	AdapterID          string `json:"adapter_id"`
	ModelCapabilityID  string `json:"model_capability_id"`
	ReasoningProfileID string `json:"reasoning_profile_id"`
}

type ChildExecutionWork struct {
	Instructions   string `json:"instructions"`
	Branch         string `json:"branch"`
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	Effort         string `json:"effort"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

type childExecutionMetadata struct {
	IssueBody              string   `json:"issue_body"`
	Prompt                 string   `json:"prompt"`
	Branch                 string   `json:"branch"`
	Provider               string   `json:"provider"`
	Model                  string   `json:"model"`
	Effort                 string   `json:"effort"`
	TimeoutSeconds         int      `json:"timeout_seconds"`
	DeadlineAt             string   `json:"deadline_at"`
	NetworkCapabilities    []string `json:"network_capabilities"`
	DelegationCapabilities []string `json:"delegation_capabilities"`
	BudgetReferenceIDs     []string `json:"budget_reference_ids"`
	BudgetBindings         []struct {
		BindingID     string `json:"agent_budget_binding_id"`
		PolicyID      string `json:"budget_policy_id"`
		ReservationID string `json:"budget_reservation_id"`
	} `json:"budget_bindings"`
}

// BuildChildExecutionRequest canonicalizes the declared checkout and path
// scope before the request can be persisted or claimed.
func BuildChildExecutionRequest(repoPath string, plan ChildPlan, child ChildRunPlan) (ChildExecutionRequest, error) {
	child.ChildKey = strings.TrimSpace(firstNonEmptyChild(child.ChildKey, child.ID))
	child.ID = child.ChildKey
	child.RunID = strings.TrimSpace(child.RunID)
	child.Role = strings.TrimSpace(child.Role)
	child.Permission = normalizeChildPermission(child.Permission)
	child.Scope = normalizeStructuredScope(child.Scope, child.Issue, child.ScopeIssues)
	if child.ChildKey == "" || child.RunID == "" || child.Role == "" || !validChildPermission(child.Permission) {
		return ChildExecutionRequest{}, fmt.Errorf("build child execution request: child_key, run_id, role, and canonical permission are required")
	}
	if err := validateChildScope(child.ChildKey, child.Permission, child.Scope); err != nil {
		return ChildExecutionRequest{}, err
	}
	checkout, scopedRepo, canonicalPaths, err := canonicalChildExecutionScope(repoPath, child)
	if err != nil {
		return ChildExecutionRequest{}, err
	}
	metadata, err := decodeChildExecutionMetadata(child.Metadata)
	if err != nil {
		return ChildExecutionRequest{}, fmt.Errorf("child %q execution metadata: %w", child.ChildKey, err)
	}
	authority := schedulerAuthorityFromChild(child)
	deadline, err := childExecutionDeadline(plan.CreatedAt, metadata)
	if err != nil {
		return ChildExecutionRequest{}, fmt.Errorf("child %q deadline: %w", child.ChildKey, err)
	}
	delegation := sortedUniqueExecutionStrings(metadata.DelegationCapabilities)
	if len(delegation) > 0 && child.Permission != string(reporter.PermissionOrchestrate) {
		return ChildExecutionRequest{}, fmt.Errorf("child %q delegation capabilities require orchestrate permission", child.ChildKey)
	}
	budgetRefs := append([]string(nil), metadata.BudgetReferenceIDs...)
	for _, binding := range metadata.BudgetBindings {
		budgetRefs = append(budgetRefs, binding.BindingID, binding.PolicyID, binding.ReservationID)
	}
	if authority.DeliveryRunID != "" {
		budgetRefs = append(budgetRefs, "delivery-run:"+authority.DeliveryRunID)
	}
	if authority.TaskID != "" {
		budgetRefs = append(budgetRefs, "task:"+authority.TaskID)
	}
	if authority.SubAgentID != "" {
		budgetRefs = append(budgetRefs, "sub-agent:"+authority.SubAgentID)
	}
	repositoryIdentity := strings.TrimSpace(authority.ProjectID)
	if repositoryIdentity == "" {
		sum := sha256.Sum256([]byte(checkout.Identity))
		repositoryIdentity = fmt.Sprintf("local:%x", sum[:])
	}
	request := ChildExecutionRequest{
		SchemaVersion:            ChildExecutionRequestSchemaVersionV1,
		ParentRunID:              strings.TrimSpace(plan.ParentRunID),
		PlanID:                   strings.TrimSpace(plan.PlanID),
		ID:                       child.ChildKey,
		ChildKey:                 child.ChildKey,
		RunID:                    child.RunID,
		Title:                    strings.TrimSpace(child.Title),
		Role:                     child.Role,
		Issue:                    firstPositive(child.Issue, firstScopeIssue(child.Scope)),
		Permission:               child.Permission,
		Scope:                    cloneChildScope(child.Scope),
		RepositoryIdentity:       repositoryIdentity,
		CheckoutIdentity:         filepath.ToSlash(checkout.Identity),
		ScopedRepositoryIdentity: filepath.ToSlash(scopedRepo.Identity),
		CanonicalPaths:           canonicalPaths,
		MutationScope: ChildMutationScope{
			Paths:        []string{},
			Issues:       []int{},
			PullRequests: []int{},
			Data:         []string{},
		},
		Capabilities: ChildExecutionCapabilities{
			Network:    sortedUniqueExecutionStrings(metadata.NetworkCapabilities),
			Commands:   sortedUniqueExecutionStrings(child.Scope.Commands),
			Delegation: delegation,
		},
		DependsOn:       append([]string{}, child.DependsOn...),
		Aggregation:     child.Aggregation,
		Required:        child.Aggregation.Required,
		Optional:        !child.Aggregation.Required,
		Ordinal:         child.Ordinal,
		Depth:           child.Depth,
		IdempotencyKey:  "child-run:" + child.RunID,
		ClaimGeneration: 0,
		ProviderDecision: ChildProviderDecisionRef{
			RoutingDecisionID:  strings.TrimSpace(authority.RoutingDecisionID),
			AdapterID:          firstNonEmptyChild(authority.AdapterID, metadata.Provider),
			ModelCapabilityID:  firstNonEmptyChild(authority.ModelCapabilityID, metadata.Model),
			ReasoningProfileID: strings.TrimSpace(metadata.Effort),
		},
		BudgetReferences:  sortedUniqueExecutionStrings(budgetRefs),
		DeadlineReference: deadline,
		Work: ChildExecutionWork{
			Instructions:   firstNonEmptyChild(metadata.IssueBody, metadata.Prompt),
			Branch:         strings.TrimSpace(metadata.Branch),
			Provider:       strings.TrimSpace(metadata.Provider),
			Model:          strings.TrimSpace(metadata.Model),
			Effort:         strings.TrimSpace(metadata.Effort),
			TimeoutSeconds: metadata.TimeoutSeconds,
		},
		LifecycleStatus: NestedStatusQueued,
	}
	if child.Permission == string(reporter.PermissionWrite) || child.Permission == string(reporter.PermissionOrchestrate) {
		request.MutationScope = ChildMutationScope{
			Paths:        append([]string{}, canonicalPaths...),
			Issues:       append([]int{}, child.Scope.Issues...),
			PullRequests: append([]int{}, child.Scope.PullRequests...),
			Data:         append([]string{}, child.Scope.Data...),
		}
	}
	request.ContractFingerprint = childExecutionRequestFingerprint(request)
	if err := ValidateChildExecutionRequest(request, false); err != nil {
		return ChildExecutionRequest{}, err
	}
	return request, nil
}

// ValidateChildExecutionRequest validates the immutable contract. requireClaim
// is used at the executor boundary, after storage binds a fenced generation.
func ValidateChildExecutionRequest(request ChildExecutionRequest, requireClaim bool) error {
	if strings.TrimSpace(request.SchemaVersion) != ChildExecutionRequestSchemaVersionV1 {
		return fmt.Errorf("child execution request schema_version must be %q", ChildExecutionRequestSchemaVersionV1)
	}
	if request.ParentRunID == "" || request.PlanID == "" || request.ChildKey == "" || request.RunID == "" || request.Role == "" {
		return fmt.Errorf("child execution request parent_run_id, plan_id, child_key, child_run_id, and role are required")
	}
	if request.ID != request.ChildKey {
		return fmt.Errorf("child execution request id must match child_key")
	}
	if !validChildPermission(request.Permission) {
		return fmt.Errorf("child execution request permission %q is invalid", request.Permission)
	}
	normalizedScope := normalizeStructuredScope(cloneChildScope(request.Scope), request.Issue, nil)
	if !equalChildExecutionScope(normalizedScope, request.Scope) {
		return fmt.Errorf("child execution request scope is not canonical")
	}
	if err := validateChildScope(request.ChildKey, request.Permission, request.Scope); err != nil {
		return fmt.Errorf("child execution request scope: %w", err)
	}
	if request.RepositoryIdentity == "" || request.CheckoutIdentity == "" || request.ScopedRepositoryIdentity == "" {
		return fmt.Errorf("child execution request repository and checkout identities are required")
	}
	if !filepath.IsAbs(filepath.FromSlash(request.CheckoutIdentity)) || !filepath.IsAbs(filepath.FromSlash(request.ScopedRepositoryIdentity)) {
		return fmt.Errorf("child execution request checkout identities must be absolute")
	}
	checkoutIdentity := filepath.FromSlash(request.CheckoutIdentity)
	scopedIdentity := filepath.FromSlash(request.ScopedRepositoryIdentity)
	if !childExecutionPathWithin(checkoutIdentity, scopedIdentity) {
		return fmt.Errorf("child execution request scoped repository escapes checkout identity")
	}
	for _, path := range request.CanonicalPaths {
		path = filepath.FromSlash(strings.TrimSpace(path))
		if !filepath.IsAbs(path) || !childExecutionPathWithin(checkoutIdentity, path) || !childExecutionPathWithin(scopedIdentity, path) {
			return fmt.Errorf("child execution request canonical path %q escapes checkout or scoped repository", filepath.ToSlash(path))
		}
	}
	for _, path := range request.MutationScope.Paths {
		path = filepath.FromSlash(strings.TrimSpace(path))
		if !filepath.IsAbs(path) || !childExecutionPathWithin(checkoutIdentity, path) || !childExecutionPathWithin(scopedIdentity, path) {
			return fmt.Errorf("child execution request mutation path %q escapes checkout or scoped repository", filepath.ToSlash(path))
		}
	}
	if request.IdempotencyKey != "child-run:"+request.RunID {
		return fmt.Errorf("child execution request idempotency_key does not match child_run_id")
	}
	if requireClaim && request.ClaimGeneration <= 0 {
		return fmt.Errorf("child execution request claim_generation must be positive at executor launch")
	}
	if request.ClaimGeneration < 0 {
		return fmt.Errorf("child execution request claim_generation must not be negative")
	}
	if request.Permission == string(reporter.PermissionReadOnly) && childMutationScopeHasValues(request.MutationScope) {
		return fmt.Errorf("read-only child execution request cannot carry mutation scope")
	}
	if request.Permission != string(reporter.PermissionReadOnly) && !equalExecutionStrings(request.MutationScope.Paths, request.CanonicalPaths) {
		return fmt.Errorf("child execution request mutation paths do not match canonical paths")
	}
	if request.Permission != string(reporter.PermissionReadOnly) && (!equalExecutionInts(request.MutationScope.Issues, request.Scope.Issues) || !equalExecutionInts(request.MutationScope.PullRequests, request.Scope.PullRequests) || !equalExecutionStrings(request.MutationScope.Data, request.Scope.Data)) {
		return fmt.Errorf("child execution request mutation scope does not match declared scope")
	}
	if !equalExecutionStrings(request.Capabilities.Commands, sortedUniqueExecutionStrings(request.Scope.Commands)) {
		return fmt.Errorf("child execution request command capabilities do not match declared scope")
	}
	if request.Required != request.Aggregation.Required || request.Optional == request.Required {
		return fmt.Errorf("child execution request aggregation requirement is inconsistent")
	}
	if len(request.Capabilities.Delegation) > 0 && request.Permission != string(reporter.PermissionOrchestrate) {
		return fmt.Errorf("child execution request delegation capabilities require orchestrate permission")
	}
	if request.DeadlineReference != "" {
		if _, err := state.ParseTimestamp(request.DeadlineReference); err != nil {
			return fmt.Errorf("child execution request deadline_reference: %w", err)
		}
	}
	want := childExecutionRequestFingerprint(request)
	if request.ContractFingerprint == "" || request.ContractFingerprint != want {
		return fmt.Errorf("child execution request contract fingerprint mismatch: have %q want %q", request.ContractFingerprint, want)
	}
	return nil
}

func bindChildExecutionRequest(request ChildExecutionRequest, generation int64, lifecycle string) (ChildExecutionRequest, error) {
	request = cloneChildExecutionRequest(request)
	request.ClaimGeneration = generation
	request.LifecycleStatus = strings.TrimSpace(lifecycle)
	if err := ValidateChildExecutionRequest(request, true); err != nil {
		return ChildExecutionRequest{}, err
	}
	return request, nil
}

func childExecutionRequestFromRecord(record storage.ChildExecutionRequestRecord) (ChildExecutionRequest, error) {
	if record.LegacyAmbiguous {
		return ChildExecutionRequest{}, fmt.Errorf("child execution request %q is legacy-ambiguous; human review is required", record.ChildRunID)
	}
	var request ChildExecutionRequest
	if err := json.Unmarshal([]byte(record.RequestJSON), &request); err != nil {
		return ChildExecutionRequest{}, fmt.Errorf("decode child execution request %q: %w", record.ChildRunID, err)
	}
	if request.RunID != record.ChildRunID || request.ParentRunID != record.ParentRunID || request.PlanID != record.PlanID || request.ChildKey != record.ChildKey {
		return ChildExecutionRequest{}, fmt.Errorf("child execution request %q durable identity mismatch", record.ChildRunID)
	}
	if request.SchemaVersion != record.SchemaVersion || request.ContractFingerprint != record.ContractFingerprint || request.RepositoryIdentity != record.RepositoryIdentity || request.CheckoutIdentity != record.CheckoutIdentity || request.Permission != record.Permission {
		return ChildExecutionRequest{}, fmt.Errorf("child execution request %q durable contract projection mismatch", record.ChildRunID)
	}
	var persistedScope ChildScope
	if err := json.Unmarshal([]byte(record.ScopeJSON), &persistedScope); err != nil {
		return ChildExecutionRequest{}, fmt.Errorf("decode child execution request %q scope projection: %w", record.ChildRunID, err)
	}
	if !reflect.DeepEqual(persistedScope, request.Scope) {
		return ChildExecutionRequest{}, fmt.Errorf("child execution request %q durable scope projection mismatch", record.ChildRunID)
	}
	return bindChildExecutionRequest(request, record.ClaimGeneration, record.LifecycleStatus)
}

func validateChildExecutionResult(request ChildExecutionRequest, result ChildRunResult) error {
	checks := []struct {
		field string
		got   string
		want  string
	}{
		{field: "id", got: result.ID, want: request.ID},
		{field: "child_key", got: result.ChildKey, want: request.ChildKey},
		{field: "title", got: result.Title, want: request.Title},
		{field: "run_id", got: result.RunID, want: request.RunID},
		{field: "role", got: result.Role, want: request.Role},
		{field: "permission", got: result.Permission, want: request.Permission},
		{field: "provider_idempotency_key", got: result.ProviderKey, want: request.IdempotencyKey},
		{field: "execution_contract_schema", got: result.ContractSchema, want: request.SchemaVersion},
		{field: "execution_contract_fingerprint", got: result.ContractFingerprint, want: request.ContractFingerprint},
	}
	for _, check := range checks {
		if strings.TrimSpace(check.got) != "" && strings.TrimSpace(check.got) != strings.TrimSpace(check.want) {
			return fmt.Errorf("child executor changed immutable %s from %q to %q", check.field, check.want, check.got)
		}
	}
	if !childScopeEmpty(result.Scope) {
		got := normalizeStructuredScope(result.Scope, result.Issue, result.Scope.Issues)
		if !equalChildExecutionScope(got, request.Scope) {
			return fmt.Errorf("child executor changed immutable scope for %q", request.ChildKey)
		}
	}
	if result.Issue > 0 && result.Issue != request.Issue {
		return fmt.Errorf("child executor changed immutable issue from %d to %d", request.Issue, result.Issue)
	}
	if len(result.DependsOn) > 0 && !reflect.DeepEqual(result.DependsOn, request.DependsOn) {
		return fmt.Errorf("child executor changed immutable dependencies for %q", request.ChildKey)
	}
	if strings.TrimSpace(result.Aggregation.Mode) != "" && !reflect.DeepEqual(result.Aggregation, request.Aggregation) {
		return fmt.Errorf("child executor changed immutable aggregation for %q", request.ChildKey)
	}
	if (result.Required || result.Optional) && (result.Required != request.Required || result.Optional != request.Optional) {
		return fmt.Errorf("child executor changed immutable aggregation requirement for %q", request.ChildKey)
	}
	if result.Ordinal > 0 && result.Ordinal != request.Ordinal {
		return fmt.Errorf("child executor changed immutable ordinal from %d to %d", request.Ordinal, result.Ordinal)
	}
	if result.Depth > 0 && result.Depth != request.Depth {
		return fmt.Errorf("child executor changed immutable depth from %d to %d", request.Depth, result.Depth)
	}
	if result.ClaimGeneration > 0 && result.ClaimGeneration != request.ClaimGeneration {
		return fmt.Errorf("child executor changed immutable claim generation from %d to %d", request.ClaimGeneration, result.ClaimGeneration)
	}
	return nil
}

// ChildRunPlan returns compatibility work metadata reconstructed only from the
// immutable request. Permission and scope never come from executor defaults.
func (request ChildExecutionRequest) ChildRunPlan() ChildRunPlan {
	return ChildRunPlan{
		ID:          request.ChildKey,
		ChildKey:    request.ChildKey,
		Title:       request.Title,
		Role:        request.Role,
		RunID:       request.RunID,
		ProviderKey: request.IdempotencyKey,
		Issue:       request.Issue,
		ScopeIssues: append([]int(nil), request.Scope.Issues...),
		Scope:       cloneChildScope(request.Scope),
		Permission:  request.Permission,
		DependsOn:   append([]string(nil), request.DependsOn...),
		Aggregation: request.Aggregation,
		Required:    request.Required,
		Optional:    request.Optional,
		Ordinal:     request.Ordinal,
		Depth:       request.Depth,
	}
}

func cloneChildExecutionRequest(request ChildExecutionRequest) ChildExecutionRequest {
	request.Scope = cloneChildScope(request.Scope)
	request.CanonicalPaths = append([]string{}, request.CanonicalPaths...)
	request.MutationScope.Paths = append([]string{}, request.MutationScope.Paths...)
	request.MutationScope.Issues = append([]int{}, request.MutationScope.Issues...)
	request.MutationScope.PullRequests = append([]int{}, request.MutationScope.PullRequests...)
	request.MutationScope.Data = append([]string{}, request.MutationScope.Data...)
	request.Capabilities.Network = append([]string{}, request.Capabilities.Network...)
	request.Capabilities.Commands = append([]string{}, request.Capabilities.Commands...)
	request.Capabilities.Delegation = append([]string{}, request.Capabilities.Delegation...)
	request.DependsOn = append([]string{}, request.DependsOn...)
	request.BudgetReferences = append([]string{}, request.BudgetReferences...)
	return request
}

func childExecutionRequestFingerprint(request ChildExecutionRequest) string {
	canonical := cloneChildExecutionRequest(request)
	canonical.ContractFingerprint = ""
	canonical.ClaimGeneration = 0
	canonical.LifecycleStatus = ""
	data, err := json.Marshal(canonical)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", sum[:])
}

func canonicalChildExecutionScope(repoPath string, child ChildRunPlan) (pathid.Path, pathid.Path, []string, error) {
	checkout, err := pathid.Canonicalize(repoPath)
	if err != nil {
		return pathid.Path{}, pathid.Path{}, nil, fmt.Errorf("canonicalize registered checkout: %w", err)
	}
	scopeRepoInput := strings.TrimSpace(child.Scope.Repo)
	if scopeRepoInput == "" {
		scopeRepoInput = "."
	}
	if !filepath.IsAbs(scopeRepoInput) {
		scopeRepoInput = filepath.Join(checkout.Display, scopeRepoInput)
	}
	if !childExecutionPathWithin(checkout.Display, filepath.Clean(scopeRepoInput)) {
		return pathid.Path{}, pathid.Path{}, nil, fmt.Errorf("child %q scope repo %q escapes registered checkout", child.ChildKey, child.Scope.Repo)
	}
	scopedRepo, err := pathid.Canonicalize(scopeRepoInput)
	if err != nil {
		return pathid.Path{}, pathid.Path{}, nil, fmt.Errorf("child %q canonicalize scope repo: %w", child.ChildKey, err)
	}
	if !childExecutionPathWithin(checkout.Identity, scopedRepo.Identity) {
		return pathid.Path{}, pathid.Path{}, nil, fmt.Errorf("child %q scope repo %q resolves outside registered checkout", child.ChildKey, child.Scope.Repo)
	}
	paths := make([]string, 0, len(child.Scope.Paths))
	for _, declared := range child.Scope.Paths {
		if strings.ContainsAny(declared, "*?[") {
			return pathid.Path{}, pathid.Path{}, nil, fmt.Errorf("child %q scope path %q is not a canonical concrete path", child.ChildKey, declared)
		}
		candidate := declared
		if !filepath.IsAbs(candidate) {
			candidate = filepath.Join(scopedRepo.Display, candidate)
		}
		candidate = filepath.Clean(candidate)
		if !childExecutionPathWithin(checkout.Display, candidate) || !childExecutionPathWithin(scopedRepo.Display, candidate) {
			return pathid.Path{}, pathid.Path{}, nil, fmt.Errorf("child %q scope path %q escapes registered checkout", child.ChildKey, declared)
		}
		canonical, err := pathid.Canonicalize(candidate)
		if err != nil {
			return pathid.Path{}, pathid.Path{}, nil, fmt.Errorf("child %q canonicalize scope path %q: %w", child.ChildKey, declared, err)
		}
		if !childExecutionPathWithin(checkout.Identity, canonical.Identity) || !childExecutionPathWithin(scopedRepo.Identity, canonical.Identity) {
			return pathid.Path{}, pathid.Path{}, nil, fmt.Errorf("child %q scope path %q resolves outside registered checkout", child.ChildKey, declared)
		}
		paths = append(paths, filepath.ToSlash(canonical.Identity))
	}
	return checkout, scopedRepo, sortedUniqueExecutionStrings(paths), nil
}

func childExecutionPathWithin(parent, child string) bool {
	rel, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func decodeChildExecutionMetadata(raw json.RawMessage) (childExecutionMetadata, error) {
	var metadata childExecutionMetadata
	if len(strings.TrimSpace(string(raw))) == 0 {
		return metadata, nil
	}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return childExecutionMetadata{}, err
	}
	if metadata.TimeoutSeconds < 0 {
		return childExecutionMetadata{}, fmt.Errorf("timeout_seconds must be non-negative")
	}
	return metadata, nil
}

func childExecutionDeadline(createdAt string, metadata childExecutionMetadata) (string, error) {
	if strings.TrimSpace(metadata.DeadlineAt) != "" {
		deadline, err := state.ParseTimestamp(metadata.DeadlineAt)
		if err != nil {
			return "", err
		}
		return state.FormatTimestamp(deadline), nil
	}
	if metadata.TimeoutSeconds <= 0 {
		return "", nil
	}
	created, err := state.ParseTimestamp(createdAt)
	if err != nil {
		return "", err
	}
	return state.FormatTimestamp(created.Add(time.Duration(metadata.TimeoutSeconds) * time.Second)), nil
}

func sortedUniqueExecutionStrings(values []string) []string {
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
	sort.Strings(out)
	if out == nil {
		return []string{}
	}
	return out
}

func childMutationScopeHasValues(scope ChildMutationScope) bool {
	return len(scope.Paths) > 0 || len(scope.Issues) > 0 || len(scope.PullRequests) > 0 || len(scope.Data) > 0
}

func equalExecutionStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func equalExecutionInts(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func equalChildExecutionScope(left, right ChildScope) bool {
	return left.Repo == right.Repo &&
		equalExecutionStrings(left.Paths, right.Paths) &&
		equalExecutionInts(left.Issues, right.Issues) &&
		equalExecutionInts(left.PullRequests, right.PullRequests) &&
		equalExecutionStrings(left.Commands, right.Commands) &&
		equalExecutionStrings(left.Data, right.Data)
}
