package runstatus

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/state"
)

func loadRunTree(repoPath, selectedRunID, projectID string, now time.Time) RunTree {
	selectedRunID = strings.TrimSpace(selectedRunID)
	if selectedRunID == "" {
		return RunTree{Nodes: []RunTreeNode{}}
	}
	rootID := rootRunID(repoPath, selectedRunID)
	nodesByID := map[string]*RunTreeNode{}
	visiting := map[string]bool{}
	collectRunTreeNode(repoPath, rootID, "", projectID, 0, now, nodesByID, visiting)
	nodes := make([]RunTreeNode, 0, len(nodesByID))
	for _, node := range nodesByID {
		sort.Strings(node.ChildRunIDs)
		nodes = append(nodes, *node)
	}
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Depth != nodes[j].Depth {
			return nodes[i].Depth < nodes[j].Depth
		}
		return nodes[i].RunID < nodes[j].RunID
	})
	return RunTree{
		RootRunID:     rootID,
		SelectedRunID: selectedRunID,
		Nodes:         nodes,
		Summary:       summarizeRunTree(nodes),
	}
}

func rootRunID(repoPath, runID string) string {
	root := strings.TrimSpace(runID)
	seen := map[string]bool{}
	for root != "" && !seen[root] {
		seen[root] = true
		parent := runParent(repoPath, root)
		if strings.TrimSpace(parent) == "" {
			break
		}
		root = strings.TrimSpace(parent)
	}
	return root
}

func collectRunTreeNode(repoPath, runID, parentRunID, projectID string, depth int, now time.Time, nodes map[string]*RunTreeNode, visiting map[string]bool) {
	runID = strings.TrimSpace(runID)
	if runID == "" || visiting[runID] {
		return
	}
	visiting[runID] = true
	defer delete(visiting, runID)

	lifecycle, lifecycleErr := state.LoadLifecycle(repoPath, runID)
	eventChildren, eventParent := runEdgesFromEvents(repoPath, runID)
	if strings.TrimSpace(parentRunID) == "" {
		parentRunID = firstNonEmpty(lifecycle.ParentRunID, eventParent)
	}
	var nodeProblems []string
	if strings.TrimSpace(parentRunID) == runID {
		nodeProblems = append(nodeProblems, "graph inconsistency: run references itself as parent")
	}
	children := map[string]bool{}
	for _, child := range lifecycle.ChildRunIDs {
		child = strings.TrimSpace(child)
		if child != "" {
			children[child] = true
			if child == runID {
				nodeProblems = append(nodeProblems, "graph inconsistency: run references itself as child")
			}
		}
	}
	for _, child := range eventChildren {
		child = strings.TrimSpace(child)
		if child != "" {
			children[child] = true
			if child == runID {
				nodeProblems = append(nodeProblems, "graph inconsistency: run references itself as child")
			}
		}
	}

	node := RunTreeNode{
		ProjectID:       projectID,
		RunID:           runID,
		ParentRunID:     strings.TrimSpace(parentRunID),
		ChildRunIDs:     sortedStringSet(children),
		Depth:           depth,
		LifecycleStatus: strings.TrimSpace(string(lifecycle.State)),
		LifecycleSource: lifecycle.Source,
	}
	if lifecycleErr != nil {
		node.LifecycleStatus = "unknown"
		node.LifecycleSource = "unreadable"
		node.LastError = lifecycleErr.Error()
	} else if node.LifecycleStatus == "" {
		node.LifecycleStatus = "unknown"
		if node.LifecycleSource == "" {
			node.LifecycleSource = "none"
		}
	}
	applyLifecycleTimestamps(&node, lifecycle.History)
	applyRunNodeDetails(repoPath, runID, now, &node)
	if len(nodeProblems) > 0 {
		if strings.TrimSpace(node.LastError) != "" {
			nodeProblems = append(nodeProblems, node.LastError)
		}
		node.LastError = strings.Join(dedupeStrings(nodeProblems), "; ")
	}
	nodes[runID] = &node

	for _, child := range node.ChildRunIDs {
		collectRunTreeNode(repoPath, child, runID, projectID, depth+1, now, nodes, visiting)
	}
}

func runParent(repoPath, runID string) string {
	lifecycle, err := state.LoadLifecycle(repoPath, runID)
	if err == nil && strings.TrimSpace(lifecycle.ParentRunID) != "" {
		return strings.TrimSpace(lifecycle.ParentRunID)
	}
	_, parent := runEdgesFromEvents(repoPath, runID)
	return parent
}

func runEdgesFromEvents(repoPath, runID string) ([]string, string) {
	path := filepath.Join(state.RunPathForRead(repoPath, runID), "events.jsonl")
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() > maxRunStatusEventBytes {
		return nil, ""
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, ""
	}
	defer file.Close()

	children := map[string]bool{}
	parent := ""
	limited := &io.LimitedReader{R: file, N: maxRunStatusEventBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 1024), maxRunStatusEventLineBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event struct {
			Details json.RawMessage `json:"details"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil || len(event.Details) == 0 {
			continue
		}
		var details struct {
			ParentRunID string `json:"parent_run_id"`
			Child       struct {
				RunID string `json:"run_id"`
			} `json:"child"`
			Result struct {
				RunID string `json:"run_id"`
			} `json:"result"`
		}
		if err := json.Unmarshal(event.Details, &details); err != nil {
			continue
		}
		if strings.TrimSpace(details.ParentRunID) != "" && details.ParentRunID != runID {
			parent = strings.TrimSpace(details.ParentRunID)
		}
		if strings.TrimSpace(details.ParentRunID) == runID {
			children[details.Child.RunID] = true
			children[details.Result.RunID] = true
			delete(children, "")
		}
	}
	return sortedStringSet(children), parent
}

func applyLifecycleTimestamps(node *RunTreeNode, history []state.LifecycleTransition) {
	for i, transition := range history {
		ts := strings.TrimSpace(transition.Timestamp)
		if ts == "" {
			continue
		}
		if i == 0 {
			node.StartedAt = ts
		}
		node.UpdatedAt = ts
		if strings.TrimSpace(transition.Reason) != "" && isProblemLifecycleState(string(transition.State)) {
			node.LastError = transition.Reason
		}
	}
	if isTerminalLifecycleState(node.LifecycleStatus) {
		node.EndedAt = node.UpdatedAt
	}
}

func applyRunNodeDetails(repoPath, runID string, now time.Time, node *RunTreeNode) {
	runPath := state.RunPathForRead(repoPath, runID)
	if info, err := os.Stat(runPath); err != nil || !info.IsDir() {
		return
	}
	var diagnostics runStatusDiagnostics
	scanRunStatusDir(runPath, "workers", isWorkerAttemptRecord, maxRunStatusRecordBytes, now, nil, &diagnostics)
	if !diagnostics.empty() {
		node.LastError = diagnostics.Error()
		return
	}
	attempts, err := state.LoadAttempts(repoPath, runID)
	if err != nil {
		node.LastError = err.Error()
		return
	}
	metadata, verifiers := runNodeMetadata(repoPath, runID, now)
	applyClaimMetadata(repoPath, runID, node)
	var latest *state.Attempt
	for i := range attempts {
		if latest == nil || attemptAfter(attempts[i], *latest) {
			latest = &attempts[i]
		}
	}
	if latest != nil {
		applyAttemptToNode(node, *latest, metadata)
	}
	if latest != nil {
		if verifier := matchingVerifier(*latest, metadataPR(*latest, metadata), verifiers); verifier != nil && verifier.Report != nil {
			applyReportToNode(node, verifier.Report)
			node.Role = string(verifier.Report.Role)
			if strings.TrimSpace(node.PR) == "" {
				node.PR = verifier.PR
			}
		}
	}
	if node.ReportSummary == "" {
		for _, record := range metadata {
			if strings.TrimSpace(record.Summary) != "" {
				node.ReportSummary = strings.TrimSpace(record.Summary)
				break
			}
		}
	}
}

func applyClaimMetadata(repoPath, runID string, node *RunTreeNode) {
	path := filepath.Join(state.RunPathForRead(repoPath, runID), "events.jsonl")
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() > maxRunStatusEventBytes {
		return
	}
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()

	limited := &io.LimitedReader{R: file, N: maxRunStatusEventBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 1024), maxRunStatusEventLineBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event struct {
			Details json.RawMessage `json:"details"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil || len(event.Details) == 0 {
			continue
		}
		var details struct {
			Result struct {
				RunID           string `json:"run_id"`
				ClaimOutcome    string `json:"claim_outcome"`
				ClaimOwner      string `json:"claim_owner"`
				ClaimGeneration int64  `json:"claim_generation"`
				LeaseExpiresAt  string `json:"lease_expires_at"`
				ClaimPhase      string `json:"claim_phase"`
				ProviderKey     string `json:"provider_idempotency_key"`
			} `json:"result"`
		}
		if err := json.Unmarshal(event.Details, &details); err != nil {
			continue
		}
		if strings.TrimSpace(details.Result.RunID) != runID {
			continue
		}
		if strings.TrimSpace(details.Result.ClaimOutcome) != "" {
			node.ClaimOutcome = strings.TrimSpace(details.Result.ClaimOutcome)
		}
		if strings.TrimSpace(details.Result.ClaimOwner) != "" {
			node.ClaimOwner = strings.TrimSpace(details.Result.ClaimOwner)
		}
		if details.Result.ClaimGeneration > 0 {
			node.ClaimGeneration = details.Result.ClaimGeneration
		}
		if strings.TrimSpace(details.Result.LeaseExpiresAt) != "" {
			node.LeaseExpiresAt = strings.TrimSpace(details.Result.LeaseExpiresAt)
		}
		if strings.TrimSpace(details.Result.ClaimPhase) != "" {
			node.ClaimPhase = strings.TrimSpace(details.Result.ClaimPhase)
		}
		if strings.TrimSpace(details.Result.ProviderKey) != "" {
			node.ProviderKey = strings.TrimSpace(details.Result.ProviderKey)
		}
	}
}

func runNodeMetadata(repoPath, runID string, now time.Time) ([]metadataRecord, []verifierRecord) {
	eventMetadata, eventVerifiers, _, err := loadEventRecords(repoPath, runID, now)
	if err != nil {
		return nil, nil
	}
	runPath := state.RunPathForRead(repoPath, runID)
	jsonMetadata, jsonVerifiers, err := scanRunJSONRecords(runPath, now)
	if err != nil {
		return eventMetadata, dedupeVerifierRecords(eventVerifiers)
	}
	verifiers := append(eventVerifiers, jsonVerifiers...)
	verifiers = dedupeVerifierRecords(verifiers)
	sortVerifierRecords(verifiers)
	return append(eventMetadata, jsonMetadata...), verifiers
}

func applyAttemptToNode(node *RunTreeNode, attempt state.Attempt, metadata []metadataRecord) {
	if attempt.Issue > 0 {
		node.Issue = attempt.Issue
	}
	node.PR = firstNonEmpty(node.PR, metadataPR(attempt, metadata))
	if strings.TrimSpace(attempt.StartedAt) != "" && strings.TrimSpace(node.StartedAt) == "" {
		node.StartedAt = strings.TrimSpace(attempt.StartedAt)
	}
	node.UpdatedAt = firstNonEmpty(attempt.LastProgressAt, attempt.HeartbeatAt, node.UpdatedAt)
	if strings.TrimSpace(attempt.Error) != "" {
		node.LastError = strings.TrimSpace(attempt.Error)
	}
	if state.IsTerminalStatus(attempt.Status) && strings.TrimSpace(node.EndedAt) == "" {
		node.EndedAt = node.UpdatedAt
	}
	if attempt.Report != nil {
		applyReportToNode(node, attempt.Report)
	}
	if node.Provider == "" {
		node.Provider = strings.TrimSpace(attempt.Provider)
	}
}

func applyReportToNode(node *RunTreeNode, record *reporter.Report) {
	if record == nil {
		return
	}
	node.Role = strings.TrimSpace(string(record.Role))
	node.Provider = strings.TrimSpace(record.Provider)
	node.Model = strings.TrimSpace(record.Model)
	node.Effort = strings.TrimSpace(record.Effort)
	node.Permission = strings.TrimSpace(string(record.Permission))
	if record.Issue > 0 {
		node.Issue = record.Issue
	}
	if strings.TrimSpace(record.StartedAt) != "" && strings.TrimSpace(node.StartedAt) == "" {
		node.StartedAt = strings.TrimSpace(record.StartedAt)
	}
	if strings.TrimSpace(record.EndedAt) != "" {
		node.EndedAt = strings.TrimSpace(record.EndedAt)
		node.UpdatedAt = strings.TrimSpace(record.EndedAt)
	}
	if strings.TrimSpace(record.Action) != "" {
		node.ReportSummary = strings.TrimSpace(record.Action)
	}
	if record.ExitCode != 0 && strings.TrimSpace(node.LastError) == "" {
		node.LastError = "report exit code " + strconv.Itoa(record.ExitCode)
	}
}

func attemptAfter(left, right state.Attempt) bool {
	if !left.LastWriteUTC.IsZero() || !right.LastWriteUTC.IsZero() {
		if !left.LastWriteUTC.Equal(right.LastWriteUTC) {
			return left.LastWriteUTC.After(right.LastWriteUTC)
		}
	}
	if left.Attempt != right.Attempt {
		return left.Attempt > right.Attempt
	}
	return left.JobID > right.JobID
}

func summarizeRunTree(nodes []RunTreeNode) RunTreeSummary {
	summary := RunTreeSummary{RunCount: len(nodes)}
	for _, node := range nodes {
		if isTerminalLifecycleState(node.LifecycleStatus) {
			summary.TerminalRuns++
		} else {
			summary.InterruptedRuns++
		}
		switch state.LifecycleState(strings.ToLower(strings.TrimSpace(node.LifecycleStatus))) {
		case state.StateFailed, state.StateCancelled, state.StateAbandoned:
			summary.FailedRuns++
		case state.StateNeedsHuman:
			summary.NeedsHumanRuns++
		}
	}
	return summary
}

func isTerminalLifecycleState(value string) bool {
	switch state.LifecycleState(strings.ToLower(strings.TrimSpace(value))) {
	case state.StateSucceeded, state.StateFailed, state.StateCancelled, state.StateAbandoned, state.StateNeedsHuman:
		return true
	default:
		return false
	}
}

func isProblemLifecycleState(value string) bool {
	switch state.LifecycleState(strings.ToLower(strings.TrimSpace(value))) {
	case state.StateFailed, state.StateCancelled, state.StateAbandoned, state.StateNeedsHuman:
		return true
	default:
		return false
	}
}

func sortedStringSet(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, strings.TrimSpace(value))
		}
	}
	sort.Strings(out)
	return out
}

func dedupeStrings(values []string) []string {
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
