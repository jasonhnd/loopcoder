package workflowrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	// CancelAfterIDs force TermCancelled after success write for matching ids
	// (simulates forced interrupt mid-child). Prefer FailIDs for hard fails.
	CancelAfterIDs map[string]bool
	// Calls records provider-exec invocations per work item (exactly-once tests).
	// When non-nil, each Execute increments Calls[WorkItemID].
	Calls map[string]int
	// HomeDir overrides layout root (tests).
	HomeDir string
	Now     func() time.Time
}

// Execute implements ChildExecutor for tests.
func (f FakeChildExecutor) Execute(ctx context.Context, in ChildExecInput) (ChildExecResult, error) {
	if f.Calls != nil {
		f.Calls[in.WorkItemID]++
	}
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
	wt, err := allocateChildWorktree(f.HomeDir, in.ProjectID, in.GraphID, in.WorkItemID, in.AttemptID, in.RepoPath)
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
	if f.CancelAfterIDs != nil && f.CancelAfterIDs[in.WorkItemID] {
		return ChildExecResult{
			Terminal: workgraph.TermCancelled, OutputEvidence: digest, WorktreePath: wt,
			ExitCode: 130, FailureClass: "forced_interrupt", Message: "fake interrupt " + in.WorkItemID,
			Provider: in.Route.Provider, Model: in.Route.Model, Depth: in.Route.Depth,
			FilesTouched: files, ActualSource: "unknown",
		}, context.Canceled
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

	wt, err := allocateChildWorktree(p.HomeDir, in.ProjectID, in.GraphID, in.WorkItemID, in.AttemptID, in.RepoPath)
	if err != nil {
		return ChildExecResult{
			Terminal: workgraph.TermFailed, FailureClass: "worktree", Message: err.Error(),
			Provider: prov, Model: model, Depth: depth, ActualSource: "unknown",
		}, err
	}
	// Snapshot parent/disposable root + durable project root before provider runs.
	// Root mutation during child execution is an isolation failure (fail closed).
	parentRoot := strings.TrimSpace(in.RepoPath)
	projectRoot := projectRootFromWorktree(wt)
	parentSnap := snapshotDirTree(parentRoot)
	projectSnap := snapshotDirTree(projectRoot)

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
	// Isolation fail-closed: product writes outside the assigned worktree, or any
	// mutation of the parent/disposable root (or durable project root outside the
	// worktree), abort success and cleanup/restore. Never relocate escapes into
	// the worktree and claim success (#1368 regression).
	if !in.ReadOnly {
		escaped := detectWorktreeEscapes(wt)
		parentMut := diffDirTree(parentSnap, snapshotDirTree(parentRoot), wt)
		projectMut := diffDirTree(projectSnap, snapshotDirTree(projectRoot), wt)
		if len(escaped) > 0 || len(parentMut) > 0 || len(projectMut) > 0 {
			// Cleanup: remove escaped product files; restore parent/project roots.
			cleanupIsolationViolation(escaped, parentMut, projectMut, parentSnap, projectSnap, parentRoot, projectRoot)
			all := append(append([]string{}, escaped...), parentMut...)
			all = append(all, projectMut...)
			msg := fmt.Sprintf("worktree isolation violation (fail closed, cleaned): %s",
				strings.Join(all, ", "))
			out := ChildExecResult{
				Terminal: workgraph.TermFailed, OutputEvidence: digest, WorktreePath: wt,
				ExitCode: 1, FailureClass: "isolation_violation", Message: msg,
				Provider: prov, Model: actualModel, Depth: actualDepth, FilesTouched: append(files, all...),
				ActualSource: "unknown",
			}
			out = attachUsage(out, res)
			return out, fmt.Errorf("workflowrun: %s", msg)
		}
	}
	out := ChildExecResult{
		Terminal: workgraph.TermSucceeded, OutputEvidence: digest, WorktreePath: wt,
		ExitCode: 0, Message: firstNonEmpty(res.Summary, "provider_ok"),
		Provider: prov, Model: actualModel, Depth: actualDepth, FilesTouched: files,
	}
	out = attachUsage(out, res)
	return out, nil
}

// detectWorktreeEscapes finds product files created under the durable project root
// but outside the assigned child worktree (common failure mode: provider writes to
// project root). Meta under runs/*/worktree is allowed.
func detectWorktreeEscapes(worktree string) []string {
	worktree = filepath.Clean(worktree)
	projectRoot := projectRootFromWorktree(worktree)
	if projectRoot == "" {
		return nil
	}
	var escaped []string
	_ = filepath.WalkDir(projectRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(worktree, path)
		if rerr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil // under worktree
		}
		base := filepath.Base(path)
		if strings.HasPrefix(base, ".") {
			return nil
		}
		if strings.Contains(path, string(filepath.Separator)+"runs"+string(filepath.Separator)) {
			return nil
		}
		// Skip durable meta dirs at project root.
		if base == "logs" || base == "tmp" || base == "recovery" {
			return nil
		}
		escaped = append(escaped, path)
		if len(escaped) >= 32 {
			return errors.New("stop")
		}
		return nil
	})
	return escaped
}

func projectRootFromWorktree(worktree string) string {
	runsDir := filepath.Dir(filepath.Dir(filepath.Clean(worktree))) // .../runs
	if filepath.Base(runsDir) != "runs" {
		return ""
	}
	return filepath.Dir(runsDir)
}

// dirSnap is a relative-path → content hash map for isolation snapshots.
type dirSnap map[string]string

func snapshotDirTree(root string) dirSnap {
	out := dirSnap{}
	root = strings.TrimSpace(root)
	if root == "" {
		return out
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return out
	}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		// Skip other child worktrees under runs/ so concurrent children don't
		// look like parent mutations.
		if strings.Contains(path, string(filepath.Separator)+"runs"+string(filepath.Separator)) {
			return nil
		}
		base := filepath.Base(path)
		if base == "logs" || strings.HasPrefix(base, ".") {
			// Skip meta/dotfiles at snapshot roots (not child worktree product).
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil || strings.HasPrefix(rel, "..") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		sum := sha256.Sum256(b)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:8])
		return nil
	})
	return out
}

// diffDirTree returns new/changed paths in after not present (or different) in
// before, excluding anything under excludeRoot (the assigned worktree).
func diffDirTree(before, after dirSnap, excludeRoot string) []string {
	var mut []string
	excludeRoot = filepath.Clean(excludeRoot)
	for rel, h := range after {
		if before[rel] == h {
			continue
		}
		// If excludeRoot is under the snap root we can't map absolute paths here;
		// caller snapshots only parent/project roots, not the worktree tree.
		_ = excludeRoot
		mut = append(mut, rel)
	}
	return mut
}

// cleanupIsolationViolation removes escaped product files and restores parent
// / project roots to the pre-run snapshot (delete new files; never invent).
func cleanupIsolationViolation(escaped, parentMut, projectMut []string, parentSnap, projectSnap dirSnap, parentRoot, projectRoot string) {
	for _, p := range escaped {
		_ = os.RemoveAll(p)
	}
	// Delete newly created relative paths under parent/project roots.
	for _, rel := range parentMut {
		if parentSnap[rel] == "" {
			_ = os.RemoveAll(filepath.Join(parentRoot, filepath.FromSlash(rel)))
		}
	}
	for _, rel := range projectMut {
		if projectSnap[rel] == "" {
			_ = os.RemoveAll(filepath.Join(projectRoot, filepath.FromSlash(rel)))
		}
	}
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

func allocateChildWorktree(homeDir, projectID, graphID, workItemID, attemptID, repoPath string) (string, error) {
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
	// Isolated provider workspace: prefer a real git worktree/clone of the
	// parent repo so AG/Codex project context is the child path only.
	if err := materializeIsolatedGitWorkspace(root, repoPath); err != nil {
		return "", err
	}
	_ = os.Chmod(root, 0o700)
	marker := fmt.Sprintf("graph=%s\nwork_item=%s\nattempt=%s\nrepo=%s\n", graphID, workItemID, attemptID, strings.TrimSpace(repoPath))
	if err := os.WriteFile(filepath.Join(root, ".loopcoder-owned-worktree"), []byte(marker), 0o600); err != nil {
		return "", err
	}
	return root, nil
}

// materializeIsolatedGitWorkspace creates an exclusive git checkout at dest
// from repoPath (git worktree when possible, else clone, else empty git init).
// The provider must treat dest as its sole project context.
func materializeIsolatedGitWorkspace(dest, repoPath string) error {
	if err := os.RemoveAll(dest); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	repoPath = strings.TrimSpace(repoPath)
	if repoPath != "" {
		if abs, aerr := filepath.Abs(repoPath); aerr == nil {
			repoPath = abs
		}
		if st, err := os.Stat(filepath.Join(repoPath, ".git")); err == nil && (st.IsDir() || st.Mode().IsRegular()) {
			// Prefer linked worktree (cheap, independent index/worktree).
			cmd := exec.Command("git", "-C", repoPath, "worktree", "add", "--detach", dest, "HEAD")
			if out, err := cmd.CombinedOutput(); err == nil {
				return nil
			} else {
				// Fall back to local clone if worktree fails (e.g. bare/invalid).
				_ = out
				cmd2 := exec.Command("git", "clone", "--local", "--no-hardlinks", repoPath, dest)
				if out2, err2 := cmd2.CombinedOutput(); err2 == nil {
					return nil
				} else {
					_ = out2
				}
			}
		}
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return err
	}
	// Empty isolated git project so providers still have a repo identity.
	init := exec.Command("git", "init")
	init.Dir = dest
	_, _ = init.CombinedOutput()
	return nil
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
