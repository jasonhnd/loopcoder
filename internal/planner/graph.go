// Package planner constructs side-effect-free v0.8 task graph proposals.
package planner

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

const (
	SchemaTaskGraphProposal   = "loopcoder.task_graph_proposal.v1"
	SchemaTaskGraphValidation = "loopcoder.task_graph_validation.v1"

	DefaultRoutingPolicyProfileID = "rprof_balanced_v1"
	DefaultRoutingPolicyProfile   = "balanced-v1"
	DefaultRoutingProfileVersion  = "1"
)

type GraphBounds struct {
	MaxTasks               int `json:"max_tasks"`
	MaxDepth               int `json:"max_depth"`
	MaxFanOut              int `json:"max_fan_out"`
	MaxDependenciesPerTask int `json:"max_dependencies_per_task"`
	MaxParallelReady       int `json:"max_parallel_ready"`
	MaxReplanPasses        int `json:"max_replan_passes"`
	MaxGraphValidationMS   int `json:"max_graph_validation_ms"`
	MaxRequirementBytes    int `json:"max_requirement_bytes"`
	MaxScopePathsPerTask   int `json:"max_scope_paths_per_task"`
}

type PlannerDecision struct {
	DecisionKey     string `json:"decision_key"`
	DecisionKind    string `json:"decision_kind"`
	Summary         string `json:"summary"`
	Heuristic       bool   `json:"heuristic"`
	HeuristicReason string `json:"heuristic_reason,omitempty"`
}

type TaskInput struct {
	TaskKey        string                                 `json:"task_key"`
	Title          string                                 `json:"title"`
	Scope          taskrequirements.Scope                 `json:"scope"`
	RoleKey        string                                 `json:"role_key,omitempty"`
	RequiredOutput taskrequirements.OutputContract        `json:"required_output,omitempty"`
	Overrides      []taskrequirements.RequirementOverride `json:"overrides,omitempty"`
}

type EdgeInput struct {
	FromTaskKey string `json:"from_task_key"`
	ToTaskKey   string `json:"to_task_key"`
	EdgeKind    string `json:"edge_kind"`
}

type ProposalInput struct {
	ProjectID                   string            `json:"project_id"`
	DeliveryRunID               string            `json:"delivery_run_id"`
	IntentSummary               string            `json:"intent_summary"`
	PolicyVersion               string            `json:"policy_version"`
	MaxSideEffectClass          string            `json:"max_side_effect_class,omitempty"`
	RoutingPolicyProfileID      string            `json:"routing_policy_profile_id,omitempty"`
	RoutingPolicyProfilePayload any               `json:"routing_policy_profile_payload,omitempty"`
	GraphBounds                 GraphBounds       `json:"graph_bounds,omitempty"`
	RootTaskKeys                []string          `json:"root_task_keys,omitempty"`
	TerminalTaskKeys            []string          `json:"terminal_task_keys,omitempty"`
	ReplanPasses                int               `json:"replan_passes,omitempty"`
	PlannerDecisions            []PlannerDecision `json:"planner_decisions,omitempty"`
	Tasks                       []TaskInput       `json:"tasks"`
	Edges                       []EdgeInput       `json:"edges,omitempty"`
	CreatedBy                   delivery.Actor    `json:"created_by"`
	Host                        delivery.Host     `json:"host"`
	Now                         time.Time         `json:"-"`
}

type TaskProposal struct {
	Task                       delivery.Task                    `json:"task"`
	Requirement                taskrequirements.TaskRequirement `json:"requirement"`
	TaskRequirementID          string                           `json:"task_requirement_id"`
	TaskRequirementFingerprint string                           `json:"task_requirement_fingerprint"`
	TaskRequirementPayloadHash string                           `json:"task_requirement_payload_hash"`
}

type TaskGraphValidation struct {
	SchemaVersion           string      `json:"schema_version"`
	TaskGraphValidationID   string      `json:"task_graph_validation_id"`
	DeliveryRunID           string      `json:"delivery_run_id"`
	PlanFingerprint         string      `json:"plan_fingerprint"`
	TaskCount               int         `json:"task_count"`
	EdgeCount               int         `json:"edge_count"`
	MaxObservedDepth        int         `json:"max_observed_depth"`
	MaxObservedFanOut       int         `json:"max_observed_fan_out"`
	MaxObservedDependencies int         `json:"max_observed_dependencies"`
	ParallelReadyWidths     []int       `json:"parallel_ready_widths"`
	ValidationStatus        string      `json:"validation_status"`
	RejectedReasons         []string    `json:"rejected_reasons"`
	CyclePath               []string    `json:"cycle_path,omitempty"`
	DisconnectedTaskIDs     []string    `json:"disconnected_task_ids"`
	PolicyBounds            GraphBounds `json:"policy_bounds"`
}

type GraphProposal struct {
	SchemaVersion                   string                    `json:"schema_version"`
	ProjectID                       string                    `json:"project_id"`
	DeliveryRunID                   string                    `json:"delivery_run_id"`
	IntentSummary                   string                    `json:"intent_summary"`
	InputFingerprint                string                    `json:"input_fingerprint"`
	PolicyFingerprint               string                    `json:"policy_fingerprint"`
	PlanSeedFingerprint             string                    `json:"plan_seed_fingerprint"`
	PlanFingerprint                 string                    `json:"plan_fingerprint"`
	AuthorizationFingerprint        string                    `json:"authorization_fingerprint"`
	ApprovalRequirement             string                    `json:"approval_requirement"`
	PolicyVersion                   string                    `json:"policy_version"`
	MaxSideEffectClass              string                    `json:"max_side_effect_class"`
	RoutingPolicyProfileID          string                    `json:"routing_policy_profile_id"`
	RoutingPolicyProfileFingerprint string                    `json:"routing_policy_profile_fingerprint"`
	GraphBounds                     GraphBounds               `json:"graph_bounds"`
	RootTaskIDs                     []string                  `json:"root_task_ids"`
	TerminalTaskIDs                 []string                  `json:"terminal_task_ids"`
	Validation                      TaskGraphValidation       `json:"validation"`
	Tasks                           []TaskProposal            `json:"tasks"`
	Edges                           []delivery.DependencyEdge `json:"edges"`
	PlannerDecisions                []PlannerDecision         `json:"planner_decisions"`
}

func DefaultGraphBounds() GraphBounds {
	return GraphBounds{
		MaxTasks:               32,
		MaxDepth:               4,
		MaxFanOut:              8,
		MaxDependenciesPerTask: 8,
		MaxParallelReady:       6,
		MaxReplanPasses:        2,
		MaxGraphValidationMS:   5000,
		MaxRequirementBytes:    65536,
		MaxScopePathsPerTask:   128,
	}
}

func HardGraphBounds() GraphBounds {
	return GraphBounds{
		MaxTasks:               64,
		MaxDepth:               6,
		MaxFanOut:              16,
		MaxDependenciesPerTask: 16,
		MaxParallelReady:       12,
		MaxReplanPasses:        4,
		MaxGraphValidationMS:   30000,
		MaxRequirementBytes:    262144,
		MaxScopePathsPerTask:   512,
	}
}

func BuildGraphProposal(input ProposalInput) (GraphProposal, error) {
	if input.Now.IsZero() {
		return GraphProposal{}, taskErr(taskrequirements.ErrInvalidRecordCode, "proposal now is required")
	}
	now := input.Now.UTC()
	projectID := strings.TrimSpace(input.ProjectID)
	runID := strings.TrimSpace(input.DeliveryRunID)
	if projectID == "" || runID == "" || strings.TrimSpace(input.IntentSummary) == "" {
		return GraphProposal{}, taskErr(taskrequirements.ErrInvalidRecordCode, "project_id, delivery_run_id, and intent_summary are required")
	}
	policyVersion := strings.TrimSpace(input.PolicyVersion)
	if policyVersion == "" {
		policyVersion = taskrequirements.PolicyVersion
	}
	maxSideEffect := strings.TrimSpace(input.MaxSideEffectClass)
	if maxSideEffect == "" {
		maxSideEffect = string(taskrequirements.SideEffectExternalWrite)
	}
	if !knownSideEffect(maxSideEffect) {
		return GraphProposal{}, taskErr(taskrequirements.ErrInvalidRecordCode, "unknown max_side_effect_class %q", maxSideEffect)
	}
	bounds, err := normalizeBounds(input.GraphBounds)
	if err != nil {
		return GraphProposal{}, err
	}
	if input.ReplanPasses > bounds.MaxReplanPasses {
		return GraphProposal{}, taskErr(taskrequirements.ErrReplanBoundExceededCode, "replan passes %d exceeds max_replan_passes %d", input.ReplanPasses, bounds.MaxReplanPasses)
	}
	tasksIn, err := normalizeTaskInputs(input.Tasks, bounds)
	if err != nil {
		return GraphProposal{}, err
	}
	edgesIn, err := normalizeEdgeInputs(input.Edges)
	if err != nil {
		return GraphProposal{}, err
	}
	taskIDs := make(map[string]string, len(tasksIn))
	for _, task := range tasksIn {
		taskIDs[task.TaskKey] = stableID("task", projectID, runID, task.TaskKey)
	}
	rootKeys, err := normalizeKeySet(input.RootTaskKeys, taskIDs, "root_task_keys")
	if err != nil {
		return GraphProposal{}, err
	}
	terminalKeys, err := normalizeKeySet(input.TerminalTaskKeys, taskIDs, "terminal_task_keys")
	if err != nil {
		return GraphProposal{}, err
	}
	graph, err := buildGraph(projectID, runID, taskIDs, edgesIn)
	if err != nil {
		return GraphProposal{}, err
	}
	validation, err := validateGraph(runID, taskIDs, graph, rootKeys, terminalKeys, bounds)
	if err != nil {
		return GraphProposal{}, err
	}
	profileID := strings.TrimSpace(input.RoutingPolicyProfileID)
	if profileID == "" {
		profileID = DefaultRoutingPolicyProfileID
	}
	profileFingerprint, err := routingProfileFingerprint(profileID, policyVersion, bounds, input.RoutingPolicyProfilePayload)
	if err != nil {
		return GraphProposal{}, err
	}
	decisions := normalizePlannerDecisions(input.PlannerDecisions)
	seed, _, err := delivery.DigestCanonicalJSON(map[string]any{
		"schema_version":                     "loopcoder.task_graph_seed.v1",
		"project_id":                         projectID,
		"delivery_run_id":                    runID,
		"intent_summary":                     strings.TrimSpace(input.IntentSummary),
		"tasks":                              seedTasks(tasksIn),
		"edges":                              seedEdges(graph.Edges),
		"routing_policy_profile_id":          profileID,
		"routing_policy_profile_fingerprint": profileFingerprint,
		"graph_bounds":                       bounds,
		"root_task_keys":                     rootKeys,
		"terminal_task_keys":                 terminalKeys,
		"planner_decisions":                  decisions,
	})
	if err != nil {
		return GraphProposal{}, err
	}

	proposedTasks := make([]TaskProposal, 0, len(tasksIn))
	for _, task := range tasksIn {
		taskID := taskIDs[task.TaskKey]
		req, err := taskrequirements.Classify(taskrequirements.ClassificationInput{
			ProjectID:       projectID,
			DeliveryRunID:   runID,
			TaskID:          taskID,
			TaskKey:         task.TaskKey,
			Title:           task.Title,
			IntentSummary:   strings.TrimSpace(input.IntentSummary),
			RoleKey:         task.RoleKey,
			PlanFingerprint: seed,
			PolicyVersion:   policyVersion,
			RequiredOutput:  task.RequiredOutput,
			Scope:           task.Scope,
			CreatedBy:       input.CreatedBy,
			Host:            input.Host,
			Now:             now,
			Overrides:       task.Overrides,
		})
		if err != nil {
			return GraphProposal{}, err
		}
		if sideEffectRank(string(req.SideEffectClass)) > sideEffectRank(maxSideEffect) {
			return GraphProposal{}, taskErr(taskrequirements.ErrGraphBoundExceededCode, "task %s side effect %s exceeds proposal maximum %s", task.TaskKey, req.SideEffectClass, maxSideEffect)
		}
		reqPayload, err := delivery.CanonicalJSON(req)
		if err != nil {
			return GraphProposal{}, taskErr(taskrequirements.ErrInvalidRecordCode, "canonical requirement payload: %v", err)
		}
		if len(reqPayload) > bounds.MaxRequirementBytes {
			return GraphProposal{}, taskErr(taskrequirements.ErrGraphBoundExceededCode, "task %s requirement payload is %d bytes, exceeds max_requirement_bytes %d", task.TaskKey, len(reqPayload), bounds.MaxRequirementBytes)
		}
		proposedTasks = append(proposedTasks, TaskProposal{
			Task: delivery.Task{
				SchemaVersion:     delivery.SchemaTask,
				RecordVersion:     1,
				TaskID:            taskID,
				TaskKey:           task.TaskKey,
				DeliveryRunID:     runID,
				ProjectID:         projectID,
				State:             delivery.TaskPending,
				Title:             task.Title,
				RequirementsJSON:  string(reqPayload),
				ScopeJSON:         req.ScopeJSON,
				Permission:        string(req.PermissionRequired),
				SideEffectClass:   string(req.SideEffectClass),
				PolicyVersion:     policyVersion,
				PlanFingerprint:   seed,
				DependsOn:         graph.DependsOn[taskID],
				CreatedAt:         delivery.CanonicalTimestamp(now),
				UpdatedAt:         delivery.CanonicalTimestamp(now),
				CreatedBy:         input.CreatedBy,
				UpdatedBy:         input.CreatedBy,
				Host:              input.Host,
				TerminalErrorCode: req.TerminalErrorCode,
			},
			Requirement:                req,
			TaskRequirementID:          req.TaskRequirementID,
			TaskRequirementFingerprint: req.TaskRequirementFingerprint,
			TaskRequirementPayloadHash: delivery.SHA256Digest(reqPayload),
		})
	}

	inputFingerprint, _, err := delivery.DigestCanonicalJSON(map[string]any{
		"schema_version":  "loopcoder.delivery_input.v1",
		"project_id":      projectID,
		"delivery_run_id": runID,
		"intent_summary":  strings.TrimSpace(input.IntentSummary),
	})
	if err != nil {
		return GraphProposal{}, err
	}
	policyFingerprint, _, err := delivery.DigestCanonicalJSON(map[string]any{
		"schema_version":                     "loopcoder.delivery_policy.v1",
		"policy_version":                     policyVersion,
		"max_side_effect_class":              maxSideEffect,
		"routing_policy_profile_id":          profileID,
		"routing_policy_profile_fingerprint": profileFingerprint,
		"graph_bounds":                       bounds,
		"approval_required_when":             "side_effect_class_above_none",
	})
	if err != nil {
		return GraphProposal{}, err
	}
	validation.ValidationStatus = "passed"
	validation.RejectedReasons = []string{}
	planFingerprint, _, err := delivery.DigestCanonicalJSON(map[string]any{
		"schema_version":                     "loopcoder.delivery_plan.v1",
		"tasks":                              fingerprintTasks(proposedTasks),
		"edges":                              fingerprintEdges(graph.Edges),
		"routing_policy_profile_id":          profileID,
		"routing_policy_profile_fingerprint": profileFingerprint,
		"graph_bounds":                       bounds,
		"validation_status":                  validation.ValidationStatus,
		"parallel_ready_widths":              validation.ParallelReadyWidths,
		"terminal_task_ids":                  taskIDsForKeys(terminalKeysOrDefault(terminalKeys, graph), taskIDs),
		"verification_requirements":          verificationRequirements(proposedTasks),
		"planner_decisions":                  decisions,
	})
	if err != nil {
		return GraphProposal{}, err
	}
	validation.PlanFingerprint = planFingerprint
	validation.TaskGraphValidationID = stableID("tgraphval", projectID, runID, planFingerprint, validation.ValidationStatus)
	for i := range proposedTasks {
		proposedTasks[i].Task.PlanFingerprint = planFingerprint
	}
	for i := range graph.Edges {
		graph.Edges[i].PlanFingerprint = planFingerprint
	}
	scope := approvedScope(proposedTasks)
	auth, _, err := delivery.AuthorizationFingerprint(inputFingerprint, policyFingerprint, planFingerprint, maxSideEffect, scope)
	if err != nil {
		return GraphProposal{}, err
	}
	return GraphProposal{
		SchemaVersion:                   SchemaTaskGraphProposal,
		ProjectID:                       projectID,
		DeliveryRunID:                   runID,
		IntentSummary:                   strings.TrimSpace(input.IntentSummary),
		InputFingerprint:                inputFingerprint,
		PolicyFingerprint:               policyFingerprint,
		PlanSeedFingerprint:             seed,
		PlanFingerprint:                 planFingerprint,
		AuthorizationFingerprint:        auth,
		ApprovalRequirement:             approvalRequirement(proposedTasks),
		PolicyVersion:                   policyVersion,
		MaxSideEffectClass:              maxSideEffect,
		RoutingPolicyProfileID:          profileID,
		RoutingPolicyProfileFingerprint: profileFingerprint,
		GraphBounds:                     bounds,
		RootTaskIDs:                     taskIDsForKeys(validationRootKeys(rootKeys, graph), taskIDs),
		TerminalTaskIDs:                 taskIDsForKeys(terminalKeysOrDefault(terminalKeys, graph), taskIDs),
		Validation:                      validation,
		Tasks:                           proposedTasks,
		Edges:                           graph.Edges,
		PlannerDecisions:                decisions,
	}, nil
}

func normalizeBounds(input GraphBounds) (GraphBounds, error) {
	def := DefaultGraphBounds()
	hard := HardGraphBounds()
	out := GraphBounds{
		MaxTasks:               firstPositive(input.MaxTasks, def.MaxTasks),
		MaxDepth:               firstPositive(input.MaxDepth, def.MaxDepth),
		MaxFanOut:              firstPositive(input.MaxFanOut, def.MaxFanOut),
		MaxDependenciesPerTask: firstPositive(input.MaxDependenciesPerTask, def.MaxDependenciesPerTask),
		MaxParallelReady:       firstPositive(input.MaxParallelReady, def.MaxParallelReady),
		MaxReplanPasses:        firstPositive(input.MaxReplanPasses, def.MaxReplanPasses),
		MaxGraphValidationMS:   firstPositive(input.MaxGraphValidationMS, def.MaxGraphValidationMS),
		MaxRequirementBytes:    firstPositive(input.MaxRequirementBytes, def.MaxRequirementBytes),
		MaxScopePathsPerTask:   firstPositive(input.MaxScopePathsPerTask, def.MaxScopePathsPerTask),
	}
	if out.MaxTasks > hard.MaxTasks || out.MaxDepth > hard.MaxDepth || out.MaxFanOut > hard.MaxFanOut ||
		out.MaxDependenciesPerTask > hard.MaxDependenciesPerTask || out.MaxParallelReady > hard.MaxParallelReady ||
		out.MaxReplanPasses > hard.MaxReplanPasses || out.MaxGraphValidationMS > hard.MaxGraphValidationMS ||
		out.MaxRequirementBytes > hard.MaxRequirementBytes || out.MaxScopePathsPerTask > hard.MaxScopePathsPerTask {
		return GraphBounds{}, taskErr(taskrequirements.ErrGraphBoundExceededCode, "graph bounds exceed hard maximum")
	}
	return out, nil
}

func normalizeTaskInputs(tasks []TaskInput, bounds GraphBounds) ([]TaskInput, error) {
	if len(tasks) == 0 {
		return nil, taskErr(taskrequirements.ErrGraphDisconnectedCode, "task graph must include at least one task")
	}
	if len(tasks) > bounds.MaxTasks {
		return nil, taskErr(taskrequirements.ErrGraphBoundExceededCode, "task count %d exceeds max_tasks %d", len(tasks), bounds.MaxTasks)
	}
	out := make([]TaskInput, 0, len(tasks))
	seen := map[string]bool{}
	for _, task := range tasks {
		task.TaskKey = strings.TrimSpace(task.TaskKey)
		task.Title = strings.TrimSpace(task.Title)
		if task.TaskKey == "" || task.Title == "" {
			return nil, taskErr(taskrequirements.ErrInvalidRecordCode, "task_key and title are required")
		}
		if seen[task.TaskKey] {
			return nil, taskErr(taskrequirements.ErrDuplicateRecordCode, "duplicate task_key %q", task.TaskKey)
		}
		if len(task.Scope.Paths) > bounds.MaxScopePathsPerTask {
			return nil, taskErr(taskrequirements.ErrGraphBoundExceededCode, "task %s has %d scope paths, exceeds max_scope_paths_per_task %d", task.TaskKey, len(task.Scope.Paths), bounds.MaxScopePathsPerTask)
		}
		seen[task.TaskKey] = true
		out = append(out, task)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TaskKey < out[j].TaskKey })
	return out, nil
}

func normalizeEdgeInputs(edges []EdgeInput) ([]EdgeInput, error) {
	out := make([]EdgeInput, 0, len(edges))
	seen := map[string]bool{}
	for _, edge := range edges {
		edge.FromTaskKey = strings.TrimSpace(edge.FromTaskKey)
		edge.ToTaskKey = strings.TrimSpace(edge.ToTaskKey)
		edge.EdgeKind = strings.TrimSpace(edge.EdgeKind)
		if edge.EdgeKind == "" {
			edge.EdgeKind = "requires"
		}
		if edge.FromTaskKey == "" || edge.ToTaskKey == "" {
			return nil, taskErr(taskrequirements.ErrInvalidRecordCode, "edge task keys are required")
		}
		if !knownEdgeKind(edge.EdgeKind) {
			return nil, taskErr(taskrequirements.ErrInvalidRecordCode, "unknown edge_kind %q", edge.EdgeKind)
		}
		key := edge.FromTaskKey + "\x00" + edge.ToTaskKey + "\x00" + edge.EdgeKind
		if seen[key] {
			return nil, taskErr(taskrequirements.ErrDuplicateRecordCode, "duplicate edge %s -> %s (%s)", edge.FromTaskKey, edge.ToTaskKey, edge.EdgeKind)
		}
		seen[key] = true
		out = append(out, edge)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].FromTaskKey != out[j].FromTaskKey {
			return out[i].FromTaskKey < out[j].FromTaskKey
		}
		if out[i].ToTaskKey != out[j].ToTaskKey {
			return out[i].ToTaskKey < out[j].ToTaskKey
		}
		return out[i].EdgeKind < out[j].EdgeKind
	})
	return out, nil
}

type graphState struct {
	Edges     []delivery.DependencyEdge
	Outgoing  map[string][]string
	Incoming  map[string][]string
	DependsOn map[string][]string
	KeyForID  map[string]string
}

func buildGraph(projectID, runID string, taskIDs map[string]string, edges []EdgeInput) (graphState, error) {
	graph := graphState{
		Outgoing:  map[string][]string{},
		Incoming:  map[string][]string{},
		DependsOn: map[string][]string{},
		KeyForID:  map[string]string{},
	}
	for key, id := range taskIDs {
		graph.Outgoing[id] = []string{}
		graph.Incoming[id] = []string{}
		graph.DependsOn[id] = []string{}
		graph.KeyForID[id] = key
	}
	for ordinal, edge := range edges {
		from, ok := taskIDs[edge.FromTaskKey]
		if !ok {
			return graphState{}, taskErr(taskrequirements.ErrMissingReferenceCode, "edge references unknown from_task_key %q", edge.FromTaskKey)
		}
		to, ok := taskIDs[edge.ToTaskKey]
		if !ok {
			return graphState{}, taskErr(taskrequirements.ErrMissingReferenceCode, "edge references unknown to_task_key %q", edge.ToTaskKey)
		}
		if from == to {
			return graphState{}, taskErr(taskrequirements.ErrGraphCycleDetectedCode, "self dependency %s", from)
		}
		graph.Outgoing[from] = append(graph.Outgoing[from], to)
		graph.Incoming[to] = append(graph.Incoming[to], from)
		graph.DependsOn[to] = append(graph.DependsOn[to], from)
		graph.Edges = append(graph.Edges, delivery.DependencyEdge{
			SchemaVersion: delivery.SchemaDependencyEdge,
			RecordVersion: 1,
			EdgeID:        stableID("edge", projectID, runID, from, to, edge.EdgeKind),
			ProjectID:     projectID,
			DeliveryRunID: runID,
			FromTaskID:    from,
			ToTaskID:      to,
			EdgeKind:      edge.EdgeKind,
			Ordinal:       ordinal,
		})
	}
	for id := range graph.DependsOn {
		sort.Strings(graph.DependsOn[id])
	}
	return graph, nil
}

func validateGraph(runID string, taskIDs map[string]string, graph graphState, rootKeys, terminalKeys []string, bounds GraphBounds) (TaskGraphValidation, error) {
	maxFanOut := 0
	maxDeps := 0
	for id := range graph.Outgoing {
		if len(graph.Outgoing[id]) > maxFanOut {
			maxFanOut = len(graph.Outgoing[id])
		}
		if len(graph.Incoming[id]) > maxDeps {
			maxDeps = len(graph.Incoming[id])
		}
		if len(graph.Outgoing[id]) > bounds.MaxFanOut {
			return TaskGraphValidation{}, taskErr(taskrequirements.ErrGraphBoundExceededCode, "task %s fan-out %d exceeds max_fan_out %d", id, len(graph.Outgoing[id]), bounds.MaxFanOut)
		}
		if len(graph.Incoming[id]) > bounds.MaxDependenciesPerTask {
			return TaskGraphValidation{}, taskErr(taskrequirements.ErrGraphBoundExceededCode, "task %s dependency count %d exceeds max_dependencies_per_task %d", id, len(graph.Incoming[id]), bounds.MaxDependenciesPerTask)
		}
	}
	layers, err := topologicalLayers(graph)
	if err != nil {
		return TaskGraphValidation{}, err
	}
	depths := map[string]int{}
	for depth, layer := range layers {
		for _, id := range layer {
			depths[id] = depth + 1
		}
	}
	maxDepth := len(layers)
	if maxDepth > bounds.MaxDepth {
		return TaskGraphValidation{}, taskErr(taskrequirements.ErrGraphBoundExceededCode, "graph depth %d exceeds max_depth %d", maxDepth, bounds.MaxDepth)
	}
	widths := make([]int, 0, len(layers))
	for _, layer := range layers {
		widths = append(widths, len(layer))
		if len(layer) > bounds.MaxParallelReady {
			return TaskGraphValidation{}, taskErr(taskrequirements.ErrGraphBoundExceededCode, "parallel ready width %d exceeds max_parallel_ready %d", len(layer), bounds.MaxParallelReady)
		}
	}
	roots := validationRootKeys(rootKeys, graph)
	reachable := reachableFrom(taskIDsForKeys(roots, taskIDs), graph.Outgoing)
	disconnected := []string{}
	for _, id := range sortedTaskIDs(taskIDs) {
		if !reachable[id] {
			disconnected = append(disconnected, id)
		}
	}
	if len(disconnected) > 0 {
		return TaskGraphValidation{}, taskErr(taskrequirements.ErrGraphDisconnectedCode, "tasks are not reachable from root intent: %s", strings.Join(disconnected, ","))
	}
	terminals := taskIDsForKeys(terminalKeysOrDefault(terminalKeys, graph), taskIDs)
	if len(terminals) == 0 {
		return TaskGraphValidation{}, taskErr(taskrequirements.ErrGraphDisconnectedCode, "task graph must include at least one terminal task")
	}
	for _, terminal := range terminals {
		if !reachable[terminal] {
			return TaskGraphValidation{}, taskErr(taskrequirements.ErrGraphDisconnectedCode, "terminal task %s is not reachable from root intent", terminal)
		}
		if depths[terminal] == 0 {
			return TaskGraphValidation{}, taskErr(taskrequirements.ErrGraphDisconnectedCode, "terminal task %s is not in the graph", terminal)
		}
	}
	return TaskGraphValidation{
		SchemaVersion:           SchemaTaskGraphValidation,
		DeliveryRunID:           runID,
		TaskCount:               len(taskIDs),
		EdgeCount:               len(graph.Edges),
		MaxObservedDepth:        maxDepth,
		MaxObservedFanOut:       maxFanOut,
		MaxObservedDependencies: maxDeps,
		ParallelReadyWidths:     widths,
		ValidationStatus:        "passed",
		RejectedReasons:         []string{},
		DisconnectedTaskIDs:     []string{},
		PolicyBounds:            bounds,
	}, nil
}

func topologicalLayers(graph graphState) ([][]string, error) {
	indegree := map[string]int{}
	for id := range graph.Outgoing {
		indegree[id] = len(graph.Incoming[id])
	}
	ready := []string{}
	for id, degree := range indegree {
		if degree == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	layers := [][]string{}
	visited := 0
	for len(ready) > 0 {
		layer := append([]string(nil), ready...)
		layers = append(layers, layer)
		visited += len(layer)
		next := []string{}
		for _, id := range layer {
			for _, child := range graph.Outgoing[id] {
				indegree[child]--
				if indegree[child] == 0 {
					next = append(next, child)
				}
			}
		}
		sort.Strings(next)
		ready = next
	}
	if visited != len(indegree) {
		return nil, taskErr(taskrequirements.ErrGraphCycleDetectedCode, "dependency graph contains a cycle")
	}
	return layers, nil
}

func normalizeKeySet(keys []string, taskIDs map[string]string, label string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, ok := taskIDs[key]; !ok {
			return nil, taskErr(taskrequirements.ErrMissingReferenceCode, "%s references unknown task key %q", label, key)
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out, nil
}

func validationRootKeys(rootKeys []string, graph graphState) []string {
	if len(rootKeys) > 0 {
		return rootKeys
	}
	roots := []string{}
	for id, parents := range graph.Incoming {
		if len(parents) == 0 {
			roots = append(roots, graph.KeyForID[id])
		}
	}
	sort.Strings(roots)
	return roots
}

func terminalKeysOrDefault(terminalKeys []string, graph graphState) []string {
	if len(terminalKeys) > 0 {
		return terminalKeys
	}
	terminals := []string{}
	for id, children := range graph.Outgoing {
		if len(children) == 0 {
			terminals = append(terminals, graph.KeyForID[id])
		}
	}
	sort.Strings(terminals)
	return terminals
}

func taskIDsForKeys(keys []string, taskIDs map[string]string) []string {
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		if id := taskIDs[key]; id != "" {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func sortedTaskIDs(taskIDs map[string]string) []string {
	out := make([]string, 0, len(taskIDs))
	for _, id := range taskIDs {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func reachableFrom(roots []string, outgoing map[string][]string) map[string]bool {
	seen := map[string]bool{}
	stack := append([]string(nil), roots...)
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[id] {
			continue
		}
		seen[id] = true
		stack = append(stack, outgoing[id]...)
	}
	return seen
}

func routingProfileFingerprint(profileID, policyVersion string, bounds GraphBounds, payload any) (string, error) {
	if payload == nil {
		payload = map[string]any{
			"schema_version":  "loopcoder.routing_policy_profile.v1",
			"profile_key":     DefaultRoutingPolicyProfile,
			"profile_version": DefaultRoutingProfileVersion,
			"policy_version":  policyVersion,
			"graph_bounds":    bounds,
		}
	}
	fp, _, err := delivery.DigestCanonicalJSON(map[string]any{
		"routing_policy_profile_id": profileID,
		"payload":                   payload,
	})
	return fp, err
}

func seedTasks(tasks []TaskInput) []any {
	out := make([]any, 0, len(tasks))
	for _, task := range tasks {
		out = append(out, map[string]any{
			"task_key":        task.TaskKey,
			"title":           task.Title,
			"scope":           task.Scope,
			"role_key":        strings.TrimSpace(task.RoleKey),
			"required_output": task.RequiredOutput,
		})
	}
	return out
}

func seedEdges(edges []delivery.DependencyEdge) []any {
	out := make([]any, 0, len(edges))
	for _, edge := range edges {
		out = append(out, map[string]any{
			"from_task_id": edge.FromTaskID,
			"to_task_id":   edge.ToTaskID,
			"edge_kind":    edge.EdgeKind,
			"ordinal":      edge.Ordinal,
		})
	}
	return out
}

func fingerprintTasks(tasks []TaskProposal) []any {
	out := make([]any, 0, len(tasks))
	for _, task := range tasks {
		out = append(out, map[string]any{
			"task_key":                             task.Task.TaskKey,
			"title":                                task.Task.Title,
			"scope":                                json.RawMessage(task.Task.ScopeJSON),
			"permission":                           task.Task.Permission,
			"side_effect_class":                    task.Task.SideEffectClass,
			"task_requirement_id":                  task.TaskRequirementID,
			"task_requirement_fingerprint":         task.TaskRequirementFingerprint,
			"task_requirement_payload_hash":        task.TaskRequirementPayloadHash,
			"verification_requirements":            task.Requirement.VerificationRequirements,
			"planner_requirement_plan_fingerprint": task.Requirement.PlanFingerprint,
		})
	}
	return out
}

func fingerprintEdges(edges []delivery.DependencyEdge) []any {
	out := make([]any, 0, len(edges))
	for _, edge := range edges {
		out = append(out, map[string]any{
			"edge_kind":    edge.EdgeKind,
			"from_task_id": edge.FromTaskID,
			"to_task_id":   edge.ToTaskID,
			"ordinal":      edge.Ordinal,
		})
	}
	return out
}

func verificationRequirements(tasks []TaskProposal) []any {
	out := []any{}
	for _, task := range tasks {
		for _, req := range task.Requirement.VerificationRequirements {
			out = append(out, map[string]any{
				"task_key":            task.Task.TaskKey,
				"verification_kind":   req.VerificationKind,
				"required_for_risk":   req.RequiredForRiskTiers,
				"independence_level":  req.IndependenceLevel,
				"permission_required": req.PermissionRequired,
				"output_contract":     req.OutputContract,
				"source":              req.Source,
			})
		}
	}
	return out
}

func approvedScope(tasks []TaskProposal) any {
	scopes := make([]any, 0, len(tasks))
	for _, task := range tasks {
		scopes = append(scopes, map[string]any{
			"task_key": task.Task.TaskKey,
			"scope":    json.RawMessage(task.Task.ScopeJSON),
		})
	}
	return map[string]any{"tasks": scopes}
}

func approvalRequirement(tasks []TaskProposal) string {
	for _, task := range tasks {
		if sideEffectRank(task.Task.SideEffectClass) > sideEffectRank(string(taskrequirements.SideEffectNone)) {
			return "required"
		}
	}
	return "not-required"
}

func normalizePlannerDecisions(decisions []PlannerDecision) []PlannerDecision {
	out := make([]PlannerDecision, 0, len(decisions))
	for _, decision := range decisions {
		decision.DecisionKey = strings.TrimSpace(decision.DecisionKey)
		decision.DecisionKind = strings.TrimSpace(decision.DecisionKind)
		decision.Summary = strings.TrimSpace(decision.Summary)
		decision.HeuristicReason = strings.TrimSpace(decision.HeuristicReason)
		if decision.DecisionKey == "" && decision.DecisionKind == "" && decision.Summary == "" {
			continue
		}
		out = append(out, decision)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].DecisionKey != out[j].DecisionKey {
			return out[i].DecisionKey < out[j].DecisionKey
		}
		if out[i].DecisionKind != out[j].DecisionKind {
			return out[i].DecisionKind < out[j].DecisionKind
		}
		return out[i].Summary < out[j].Summary
	})
	return out
}

func firstPositive(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

func knownEdgeKind(kind string) bool {
	switch kind {
	case "requires", "blocks", "orders-after":
		return true
	default:
		return false
	}
}

func knownSideEffect(value string) bool {
	return sideEffectRank(value) >= 0
}

func sideEffectRank(value string) int {
	switch value {
	case string(taskrequirements.SideEffectNone):
		return 0
	case string(taskrequirements.SideEffectLocalRead):
		return 1
	case string(taskrequirements.SideEffectLocalWrite):
		return 2
	case string(taskrequirements.SideEffectRepoWrite):
		return 3
	case string(taskrequirements.SideEffectGitRemoteWrite):
		return 4
	case string(taskrequirements.SideEffectGitHubWrite):
		return 5
	case string(taskrequirements.SideEffectProviderLaunch):
		return 6
	case string(taskrequirements.SideEffectExternalWrite):
		return 7
	default:
		return -1
	}
}

func stableID(prefix string, parts ...string) string {
	return prefix + "_" + digestBase32(parts...)[:32]
}

func digestBase32(parts ...string) string {
	sum := sha256.New()
	for _, part := range parts {
		sum.Write([]byte(part))
		sum.Write([]byte{0})
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum.Sum(nil)))
}

func taskErr(code taskrequirements.ErrorCode, format string, args ...any) error {
	return &taskrequirements.TypedError{Code: code, Message: fmt.Sprintf(format, args...)}
}
