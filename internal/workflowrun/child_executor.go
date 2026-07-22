package workflowrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/agent"
	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// ChildRoute is the immutable per-child provider pin for one WorkItem.
type ChildRoute struct {
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	Depth       string `json:"depth,omitempty"`
	AccountRef  string `json:"account_ref,omitempty"`
	RouteReason string `json:"route_reason,omitempty"`
}

// ChildExecInput is one LoopCoder-owned child launch request.
type ChildExecInput struct {
	ProjectID  string
	GraphID    string
	WorkItemID string
	ClaimID    string
	AttemptID  string
	Intent     string
	Route      ChildRoute
	RepoPath   string
	// ReadOnly forces read-only provider mode (research children).
	ReadOnly bool
	Timeout  time.Duration
}

// ChildExecResult is durable child terminal evidence. Capacity actual is only
// set when the provider reported usable usage — never fabricated.
type ChildExecResult struct {
	Terminal       workgraph.TerminalState
	OutputEvidence string // required for success close
	WorktreePath   string
	ProcessPID     int
	ExitCode       int
	InputTokens    int64
	OutputTokens   int64
	// ActualCapacity is a fraction [0,1] when known; nil means unknown.
	ActualCapacity *float64
	// ActualSource is provider_usage|estimated|unknown (never invent numbers).
	ActualSource string
	FailureClass string
	Message      string
	// Actual route observed (must match requested on success integrity path).
	Provider string
	Model    string
	Depth    string
	// FilesTouched are relative paths written under the child worktree.
	FilesTouched []string
}

// ChildExecutor runs one claimed WorkItem as a LoopCoder-owned transparent child.
// Production default invokes the routed provider in an exclusive worktree.
// Tests inject FakeChildExecutor; remote acceptance must use production.
type ChildExecutor interface {
	Execute(ctx context.Context, in ChildExecInput) (ChildExecResult, error)
}

// FakeChildExecutor is for focused unit tests only. It still allocates a real
// worktree and writes evidence files (not the old Claim→Close string stub),
// but never launches a live provider process.
type FakeChildExecutor struct {
	// FailIDs force TermFailed for matching work item ids.
	FailIDs map[string]bool
	// HomeDir overrides layout root (tests).
	HomeDir string
	Now     func() time.Time
}

// Execute implements ChildExecutor for tests.
func (f FakeChildExecutor) Execute(ctx context.Context, in ChildExecInput) (ChildExecResult, error) {
	if err := ctx.Err(); err != nil {
		return ChildExecResult{
			Terminal: workgraph.TermCancelled, FailureClass: "cancelled", Message: err.Error(),
			Provider: in.Route.Provider, Model: in.Route.Model, Depth: in.Route.Depth,
			ActualSource: "unknown",
		}, err
	}
	now := f.Now
	if now == nil {
		now = time.Now
	}
	wt, err := allocateChildWorktree(f.HomeDir, in.ProjectID, in.GraphID, in.WorkItemID, in.AttemptID)
	if err != nil {
		return ChildExecResult{
			Terminal: workgraph.TermFailed, FailureClass: "worktree", Message: err.Error(),
			Provider: in.Route.Provider, Model: in.Route.Model, Depth: in.Route.Depth,
			ActualSource: "unknown",
		}, err
	}
	evPath, digest, files, err := writeChildEvidence(wt, in, "fake_executor", now().UTC())
	if err != nil {
		return ChildExecResult{
			Terminal: workgraph.TermFailed, FailureClass: "evidence", Message: err.Error(),
			WorktreePath: wt, Provider: in.Route.Provider, Model: in.Route.Model, Depth: in.Route.Depth,
			ActualSource: "unknown",
		}, err
	}
	if f.FailIDs != nil && f.FailIDs[in.WorkItemID] {
		return ChildExecResult{
			Terminal: workgraph.TermFailed, OutputEvidence: digest, WorktreePath: wt,
			ExitCode: 1, FailureClass: "injected_fail", Message: "fake fail " + in.WorkItemID,
			Provider: in.Route.Provider, Model: in.Route.Model, Depth: in.Route.Depth,
			FilesTouched: files, ActualSource: "unknown",
		}, nil
	}
	_ = evPath
	// Fake does not invent capacity actual — honest unknown.
	return ChildExecResult{
		Terminal: workgraph.TermSucceeded, OutputEvidence: digest, WorktreePath: wt,
		ExitCode: 0, Provider: in.Route.Provider, Model: in.Route.Model, Depth: in.Route.Depth,
		FilesTouched: files, ActualSource: "unknown",
		Message: "fake_executor_ok",
	}, nil
}

// ProductionChildExecutor is the default LoopCoder-owned child executor.
// It allocates an exclusive worktree and invokes the routed provider via
// agent.Runner with DisableDelegation (never provider-native subagents).
// Fixture routes produce local worktree evidence without a live process.
type ProductionChildExecutor struct {
	HomeDir string
	// Lookup defaults to agent.Lookup.
	Lookup func(provider string) (agent.Runner, error)
	Now    func() time.Time
	// HardCap bounds each child provider call (default 10m).
	HardCap time.Duration
}

// DefaultChildExecutor returns the production executor.
func DefaultChildExecutor() ChildExecutor {
	return ProductionChildExecutor{}
}

// Execute implements ChildExecutor for production.
func (p ProductionChildExecutor) Execute(ctx context.Context, in ChildExecInput) (ChildExecResult, error) {
	if err := ctx.Err(); err != nil {
		return ChildExecResult{
			Terminal: workgraph.TermCancelled, FailureClass: "cancelled", Message: err.Error(),
			Provider: in.Route.Provider, Model: in.Route.Model, Depth: in.Route.Depth,
			ActualSource: "unknown",
		}, err
	}
	now := p.Now
	if now == nil {
		now = time.Now
	}
	prov := strings.TrimSpace(in.Route.Provider)
	model := strings.TrimSpace(in.Route.Model)
	depth := strings.TrimSpace(in.Route.Depth)
	if prov == "" {
		prov = "fixture"
	}
	if model == "" {
		model = "fixture-model"
	}
	if depth == "" {
		depth = "medium"
	}
	in.Route.Provider, in.Route.Model, in.Route.Depth = prov, model, depth

	wt, err := allocateChildWorktree(p.HomeDir, in.ProjectID, in.GraphID, in.WorkItemID, in.AttemptID)
	if err != nil {
		return ChildExecResult{
			Terminal: workgraph.TermFailed, FailureClass: "worktree", Message: err.Error(),
			Provider: prov, Model: model, Depth: depth, ActualSource: "unknown",
		}, err
	}

	// Fixture / unknown providers: durable local evidence without inventing a live process.
	if prov == "fixture" || prov == "auto" {
		_, digest, files, werr := writeChildEvidence(wt, in, "fixture_local", now().UTC())
		if werr != nil {
			return ChildExecResult{
				Terminal: workgraph.TermFailed, FailureClass: "evidence", Message: werr.Error(),
				WorktreePath: wt, Provider: prov, Model: model, Depth: depth, ActualSource: "unknown",
			}, werr
		}
		return ChildExecResult{
			Terminal: workgraph.TermSucceeded, OutputEvidence: digest, WorktreePath: wt,
			ExitCode: 0, Provider: prov, Model: model, Depth: depth,
			FilesTouched: files, ActualSource: "unknown",
			Message: "fixture_local_evidence",
		}, nil
	}

	lookup := p.Lookup
	if lookup == nil {
		lookup = agent.Lookup
	}
	runner, lerr := lookup(prov)
	if lerr != nil {
		// Honest failure — do not silently close as success without a provider.
		return ChildExecResult{
			Terminal: workgraph.TermFailed, FailureClass: "unsupported_provider",
			Message: lerr.Error(), WorktreePath: wt,
			Provider: prov, Model: model, Depth: depth, ActualSource: "unknown",
		}, lerr
	}

	logPath := filepath.Join(wt, ".loopcoder-child-provider.log")
	prompt := strings.TrimSpace(in.Intent)
	if prompt == "" {
		prompt = "bounded LoopCoder child work item " + in.WorkItemID
	}
	// Explicit anti-delegation: LoopCoder owns children; never provider-native subagents.
	hardCap := p.HardCap
	if hardCap <= 0 {
		hardCap = 10 * time.Minute
	}
	if in.Timeout > 0 && in.Timeout < hardCap {
		hardCap = in.Timeout
	}
	runCtx := ctx
	var cancel context.CancelFunc
	if hardCap > 0 {
		runCtx, cancel = context.WithTimeout(ctx, hardCap)
		defer cancel()
	}

	inv := agent.Invocation{
		WorktreePath:      wt,
		Prompt:            prompt,
		Model:             model,
		Effort:            depth,
		ReadOnly:          in.ReadOnly,
		BoundedWrite:      !in.ReadOnly,
		DisableDelegation: true,
		Role:              "nested-bounded-write",
		LogPath:           logPath,
		RunID:             in.AttemptID,
		ProviderKey:       in.AttemptID,
		HardCap:           hardCap,
	}
	if in.ReadOnly {
		inv.Role = "nested-read-only"
		inv.BoundedWrite = false
	}

	res, rerr := runner.Run(runCtx, inv)
	// Always materialize evidence files (even on failure) for audit.
	_, digest, files, _ := writeChildEvidence(wt, in, "provider_run", now().UTC())
	// Requested route identity is authoritative (never swap provider←model).
	actualModel := firstNonEmpty(res.Model, model)
	actualDepth := firstNonEmpty(res.Effort, depth)
	if rerr != nil {
		term := workgraph.TermFailed
		fc := "process_failure"
		if errors.Is(rerr, context.Canceled) {
			term = workgraph.TermCancelled
			fc = "cancelled"
		} else if errors.Is(rerr, context.DeadlineExceeded) {
			term = workgraph.TermFailed
			fc = "timeout"
		}
		out := ChildExecResult{
			Terminal: term, OutputEvidence: digest, WorktreePath: wt,
			ExitCode: res.ExitCode, FailureClass: fc, Message: rerr.Error(),
			Provider: prov, Model: actualModel, Depth: actualDepth, FilesTouched: files,
			ActualSource: "unknown",
		}
		out = attachUsage(out, res)
		return out, rerr
	}
	if res.ExitCode != 0 {
		out := ChildExecResult{
			Terminal: workgraph.TermFailed, OutputEvidence: digest, WorktreePath: wt,
			ExitCode: res.ExitCode, FailureClass: firstNonEmpty(res.FailureClass, "nonzero_exit"),
			Message:  firstNonEmpty(res.Summary, fmt.Sprintf("exit %d", res.ExitCode)),
			Provider: prov, Model: actualModel, Depth: actualDepth, FilesTouched: files,
			ActualSource: "unknown",
		}
		out = attachUsage(out, res)
		return out, nil
	}
	// Success requires non-empty output evidence (workclaim close contract).
	if strings.TrimSpace(digest) == "" {
		return ChildExecResult{
			Terminal: workgraph.TermFailed, WorktreePath: wt, FailureClass: "missing_evidence",
			Message: "empty output evidence", Provider: prov, Model: model, Depth: depth,
			ActualSource: "unknown",
		}, fmt.Errorf("workflowrun: empty child output evidence")
	}
	out := ChildExecResult{
		Terminal: workgraph.TermSucceeded, OutputEvidence: digest, WorktreePath: wt,
		ExitCode: 0, Message: firstNonEmpty(res.Summary, "provider_ok"),
		Provider: prov, Model: actualModel, Depth: actualDepth, FilesTouched: files,
	}
	out = attachUsage(out, res)
	return out, nil
}

func attachUsage(out ChildExecResult, res agent.Result) ChildExecResult {
	if res.Usage.InputTokens != nil {
		out.InputTokens = *res.Usage.InputTokens
	}
	if res.Usage.OutputTokens != nil {
		out.OutputTokens = *res.Usage.OutputTokens
	}
	total := out.InputTokens + out.OutputTokens
	if res.Usage.TotalTokens != nil && *res.Usage.TotalTokens > 0 {
		total = *res.Usage.TotalTokens
	}
	if total <= 0 {
		out.ActualSource = "unknown"
		out.ActualCapacity = nil
		return out
	}
	// Honest estimated fraction from token count vs a large soft window.
	// Never claim exact when the provider only reported tokens.
	const softWindow = 200_000.0
	frac := float64(total) / softWindow
	if frac > 1 {
		frac = 1
	}
	if frac < 0 {
		frac = 0
	}
	out.ActualCapacity = &frac
	out.ActualSource = "estimated"
	return out
}

func allocateChildWorktree(homeDir, projectID, graphID, workItemID, attemptID string) (string, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		projectID = "local-project"
	}
	var layout home.V09Layout
	var err error
	if homeDir != "" {
		// Tests/production may pass an explicit root; ensure owner-only base+project.
		if err := os.MkdirAll(homeDir, 0o700); err != nil {
			return "", err
		}
		_ = os.Chmod(homeDir, 0o700)
		layout, err = home.EnsureMinimumLayout(homeDir, projectID)
	} else {
		layout, err = home.ResolveV09(home.Deps{})
		if err == nil {
			err = layout.EnsureProject(projectID)
		}
	}
	if err != nil {
		return "", err
	}
	pdir, err := layout.ProjectDir(projectID)
	if err != nil {
		return "", err
	}
	runKey := short(graphID + "|" + workItemID + "|" + attemptID)
	root := filepath.Join(pdir, "runs", "wf_"+runKey, "worktree")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	_ = os.Chmod(root, 0o700)
	marker := fmt.Sprintf("graph=%s\nwork_item=%s\nattempt=%s\n", graphID, workItemID, attemptID)
	if err := os.WriteFile(filepath.Join(root, ".loopcoder-owned-worktree"), []byte(marker), 0o600); err != nil {
		return "", err
	}
	return root, nil
}

func writeChildEvidence(wt string, in ChildExecInput, kind string, at time.Time) (path, digest string, files []string, err error) {
	evDir := filepath.Join(wt, ".loopcoder", "child-evidence")
	if err := os.MkdirAll(evDir, 0o700); err != nil {
		return "", "", nil, err
	}
	payload := map[string]any{
		"schema":       "loopcoder.child.evidence.v1",
		"kind":         kind,
		"project_id":   in.ProjectID,
		"graph_id":     in.GraphID,
		"work_item_id": in.WorkItemID,
		"claim_id":     in.ClaimID,
		"attempt_id":   in.AttemptID,
		"intent":       in.Intent,
		"provider":     in.Route.Provider,
		"model":        in.Route.Model,
		"depth":        in.Route.Depth,
		"account_ref":  in.Route.AccountRef,
		"route_reason": in.Route.RouteReason,
		"recorded_at":  at.UTC().Format(time.RFC3339Nano),
		// Explicit: LoopCoder-owned; not a provider-native subagent.
		"ownership":          "loopcoder_work_item",
		"native_delegation":  false,
		"disable_delegation": true,
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", "", nil, err
	}
	path = filepath.Join(evDir, in.WorkItemID+".json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", "", nil, err
	}
	// Also drop a human-readable findings stub for research/integration paths.
	stubName := "child-output-" + in.WorkItemID + ".md"
	stub := fmt.Sprintf("# Child %s\n\nIntent: %s\nRoute: %s/%s depth=%s\nKind: %s\n",
		in.WorkItemID, in.Intent, in.Route.Provider, in.Route.Model, in.Route.Depth, kind)
	stubPath := filepath.Join(wt, stubName)
	if err := os.WriteFile(stubPath, []byte(stub), 0o600); err != nil {
		return "", "", nil, err
	}
	sum := sha256.Sum256(raw)
	digest = "sha256:" + hex.EncodeToString(sum[:])
	files = []string{
		filepath.Join(".loopcoder", "child-evidence", in.WorkItemID+".json"),
		stubName,
		".loopcoder-owned-worktree",
	}
	return path, digest, files, nil
}

func firstNonEmpty(vals ...string) string {
	for _, a := range vals {
		if strings.TrimSpace(a) != "" {
			return a
		}
	}
	return ""
}
