// Package scaffold initializes repository-local loopcoder files.
package scaffold

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/execresult"
	"github.com/jasonhnd/loopcoder/internal/gitlocal"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
)

const (
	DeliveryFilename = ".delivery.yml"
	RoadmapFilename  = "ROADMAP.md"

	ghHardCapDefault = lcdefaults.ScaffoldGitHubCommandCap
)

var (
	ghCommand = "gh"
	ghHardCap = ghHardCapDefault
)

type Options struct {
	RepoPath       string
	Force          bool
	Gate           string
	WorkerModel    string
	WorkerEffort   string
	VerifierModel  string
	VerifierEffort string
}

type Deps struct {
	FS                FileSystem
	GitHub            GitHubRunner
	ProtectLocalState func(context.Context, string) (gitlocal.ProtectResult, error)
}

type FileSystem interface {
	Stat(name string) (fs.FileInfo, error)
	WriteFile(name string, data []byte, perm fs.FileMode) error
}

type GitHubRunner interface {
	Run(ctx context.Context, dir string, args ...string) ([]byte, error)
}

type FileStatus string

const (
	FileCreated     FileStatus = "created"
	FileExists      FileStatus = "exists"
	FileOverwritten FileStatus = "overwritten"
)

type FileResult struct {
	Path   string
	Status FileStatus
}

type LabelStatus string

const (
	LabelCreated LabelStatus = "created"
	LabelExists  LabelStatus = "exists"
)

type LabelResult struct {
	Name   string
	Status LabelStatus
}

type Result struct {
	Files             []FileResult
	Labels            []LabelResult
	LocalStateExclude *LocalStateResult
	Warnings          []string
}

type LocalStateResult struct {
	Path   string
	Status gitlocal.ProtectStatus
}

type LabelSpec struct {
	Name        string
	Color       string
	Description string
}

var defaultLabels = []LabelSpec{
	{Name: "delivery:unit", Color: "0e8a16", Description: "loopcoder work unit"},
	{Name: "epic", Color: "5319e7", Description: "loopcoder epic work"},
	{Name: "status:ready", Color: "1d76db", Description: "ready for worker dispatch"},
	{Name: "status:implementing", Color: "fbca04", Description: "worker implementation in progress"},
	{Name: "status:in-review", Color: "5319e7", Description: "pull request awaiting verification"},
	{Name: "status:fixing", Color: "d93f0b", Description: "fix pass in progress"},
	{Name: "status:gated", Color: "c5def5", Description: "verified and waiting for human merge"},
	{Name: "gated", Color: "c5def5", Description: "human gate required"},
	{Name: "needs-human", Color: "b60205", Description: "human decision required"},
	{Name: "tier:1", Color: "bfd4f2", Description: "tier 1 work item"},
	{Name: "tier:2", Color: "d4c5f9", Description: "tier 2 work item"},
	{Name: "tier:3", Color: "ededed", Description: "tier 3 work item"},
	{Name: "risk:low", Color: "0e8a16", Description: "low risk work item"},
	{Name: "risk:med", Color: "fbca04", Description: "medium risk work item"},
	{Name: "risk:high", Color: "b60205", Description: "high risk work item"},
}

func DefaultLabels() []LabelSpec {
	labels := make([]LabelSpec, len(defaultLabels))
	copy(labels, defaultLabels)
	return labels
}

func DefaultDeps() Deps {
	return Deps{
		FS:                osFileSystem{},
		GitHub:            execGitHubRunner{},
		ProtectLocalState: gitlocal.ProtectLoopcoderState,
	}
}

func Init(ctx context.Context, opts Options, deps Deps) (Result, error) {
	defaults := DefaultDeps()
	if deps.FS == nil {
		deps.FS = defaults.FS
	}
	if deps.GitHub == nil {
		deps.GitHub = defaults.GitHub
	}
	if deps.ProtectLocalState == nil {
		deps.ProtectLocalState = defaults.ProtectLocalState
	}
	if strings.TrimSpace(opts.RepoPath) == "" {
		opts.RepoPath = "."
	}
	if err := validateGate(opts.Gate); err != nil {
		return Result{}, err
	}

	var result Result
	deliveryPath := filepath.Join(opts.RepoPath, DeliveryFilename)
	deliveryResult, err := writeFile(deps.FS, deliveryPath, []byte(DeliveryTemplate(opts)), opts.Force)
	if err != nil {
		return Result{}, err
	}
	result.Files = append(result.Files, deliveryResult)

	roadmapPath := filepath.Join(opts.RepoPath, RoadmapFilename)
	roadmapResult, err := writeFile(deps.FS, roadmapPath, []byte(RoadmapTemplate), opts.Force)
	if err != nil {
		return Result{}, err
	}
	result.Files = append(result.Files, roadmapResult)

	labels, warnings := ensureLabels(ctx, opts.RepoPath, deps.GitHub)
	result.Labels = labels
	result.Warnings = append(result.Warnings, warnings...)

	localState, err := deps.ProtectLocalState(ctx, opts.RepoPath)
	if err != nil {
		if errors.Is(err, gitlocal.ErrNotGitRepository) {
			result.Warnings = append(result.Warnings, fmt.Sprintf("local .loopcoder/ exclude was not installed for %s: %v", displayRepoPath(opts.RepoPath), err))
		} else {
			return Result{}, fmt.Errorf("protect local loopcoder state: %w", err)
		}
	} else {
		result.LocalStateExclude = &LocalStateResult{
			Path:   localState.ExcludePath,
			Status: localState.Status,
		}
	}
	return result, nil
}

func writeFile(fsys FileSystem, path string, data []byte, force bool) (FileResult, error) {
	info, err := fsys.Stat(path)
	if err == nil {
		if info.IsDir() {
			return FileResult{}, fmt.Errorf("%s is a directory", path)
		}
		if !force {
			return FileResult{Path: filepath.Base(path), Status: FileExists}, nil
		}
		if err := fsys.WriteFile(path, data, 0o644); err != nil {
			return FileResult{}, fmt.Errorf("write %s: %w", path, err)
		}
		return FileResult{Path: filepath.Base(path), Status: FileOverwritten}, nil
	}
	if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, os.ErrNotExist) {
		return FileResult{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if err := fsys.WriteFile(path, data, 0o644); err != nil {
		return FileResult{}, fmt.Errorf("write %s: %w", path, err)
	}
	return FileResult{Path: filepath.Base(path), Status: FileCreated}, nil
}

func DeliveryTemplate(opts Options) string {
	gate := normalizeScaffoldGate(opts.Gate)
	workerModel := strings.TrimSpace(opts.WorkerModel)
	workerEffort := strings.TrimSpace(opts.WorkerEffort)
	verifierModel := strings.TrimSpace(opts.VerifierModel)
	verifierEffort := strings.TrimSpace(opts.VerifierEffort)
	hasVerifier := verifierModel != "" || verifierEffort != ""

	var b strings.Builder
	b.WriteString("version: 1\n")
	b.WriteString("adapters:\n")
	b.WriteString("  work_items: github      # Work items are GitHub issues.\n")
	b.WriteString("  workspace: git-worktree # Work happens in git worktrees.\n")
	b.WriteString("  conductor: codex-cli    # Transparency only: the human session that conducts.\n")
	b.WriteString("  worker: codex           # Default worker provider.\n")
	b.WriteString("  vcs: github             # GitHub hosts PRs and checks.\n")
	b.WriteString("  verifier: claude        # Should differ from worker; provider registry key.\n")
	if gate == "auto" {
		b.WriteString("  gate: auto              # Explicit first-run opt-in to automatic production promotion when all gates pass.\n")
	} else {
		b.WriteString("  gate: human-merge       # First-run safe default: humans choose what promotes to production.\n")
	}
	b.WriteString("worker:\n")
	b.WriteString("  # Optional. Absent = inherit the worker provider's global config. loopcoder never sets this on its own.\n")
	writeOptionalScalar(&b, "  ", "model", workerModel)
	b.WriteString("  # Optional. Absent = inherit the worker provider's global config. loopcoder never sets this on its own.\n")
	writeOptionalScalar(&b, "  ", "reasoning_effort", workerEffort)
	fmt.Fprintf(&b, "  base_branch: %s\n", lcdefaults.BaseBranch)
	b.WriteString("  command_hint: \"implement the issue, run relevant checks, commit\"\n")
	b.WriteString("environment:\n")
	fmt.Fprintf(&b, "  pre_prod_branch: %s # Tick auto-merges clean PRs here only; promote is the separate production step.\n", lcdefaults.PreProdBranch)
	b.WriteString("# evidence:\n")
	b.WriteString("#   # Optional. Tick copies configured evidence onto dispatched, pending, and pre-prod report items.\n")
	b.WriteString("#   website:\n")
	b.WriteString("#     preview_url: https://preview.example.com\n")
	b.WriteString("#   cli:\n")
	b.WriteString("#     example_output: |\n")
	b.WriteString("#       $ loopcoder --version\n")
	b.WriteString("#       version=dev commit=unknown date=unknown\n")
	b.WriteString("#   library:\n")
	b.WriteString("#     test_results: go test ./...\n")
	b.WriteString("#   app:\n")
	b.WriteString("#     preview_build: dist/app-preview.zip\n")
	b.WriteString("# domain:\n")
	b.WriteString("#   # Optional. Absent = current code profile; later 0.5.0 slices consume these plug points.\n")
	b.WriteString("#   name: docs\n")
	b.WriteString("#   description: corporate IR document production\n")
	b.WriteString("#   skills:\n")
	b.WriteString("#     # Optional. Ordered repo-relative skill file globs.\n")
	b.WriteString("#     paths:\n")
	b.WriteString("#       - .claude/skills/*/SKILL.md\n")
	b.WriteString("#     machine_library:\n")
	b.WriteString("#       # Optional. Repo-relative machine-readable skill metadata globs.\n")
	b.WriteString("#       paths:\n")
	b.WriteString("#         - .loopcoder/skill-library/**/*.md\n")
	b.WriteString("#     # Optional. Skill names, path stems, or tags to include.\n")
	b.WriteString("#     select:\n")
	b.WriteString("#       - governance\n")
	b.WriteString("#     # Optional. Absent = current default prompt budget.\n")
	b.WriteString("#     prompt_budget_bytes: 4096\n")
	b.WriteString("#   verification:\n")
	b.WriteString("#     rubric:\n")
	b.WriteString("#       # Optional. Repo files copied into the review rubric packet.\n")
	b.WriteString("#       paths:\n")
	b.WriteString("#         - governance/qa-checklist.md\n")
	b.WriteString("#       # Optional. Inline rubric checklist items.\n")
	b.WriteString("#       checklist:\n")
	b.WriteString("#         - Rendered artifact matches the approved spec.\n")
	b.WriteString("#     # Optional. Top-level loopreview packet section order.\n")
	b.WriteString("#     review_packet_order:\n")
	b.WriteString("#       - rendered_artifact\n")
	b.WriteString("#       - rubric\n")
	b.WriteString("#       - changed_files\n")
	b.WriteString("#       - diff\n")
	b.WriteString("#       - issue\n")
	b.WriteString("#       - spec\n")
	b.WriteString("#   evidence:\n")
	b.WriteString("#     producer:\n")
	b.WriteString("#       # Optional. Command run in the PR worktree before loopreview packet construction.\n")
	b.WriteString("#       command: make render\n")
	b.WriteString("#       outputs:\n")
	b.WriteString("#         - out/report.pdf\n")
	b.WriteString("#       timeout_seconds: 300\n")
	b.WriteString("#       include_in_loopreview: true\n")
	b.WriteString("#   red_lines:\n")
	b.WriteString("#     - category: disclosure-compliance\n")
	b.WriteString("#       detail: unresolved disclosure or legal approval requirement\n")
	b.WriteString("#       path_globs:\n")
	b.WriteString("#         - disclosure/**\n")
	b.WriteString("#   partial_work:\n")
	b.WriteString("#     # Optional. harvest-needs-human|report-only; absent = current behavior.\n")
	b.WriteString("#     mode: harvest-needs-human\n")
	b.WriteString("#   liveness:\n")
	b.WriteString("#     # Optional. worktree-mtime|log-only|custom; absent = current behavior.\n")
	b.WriteString("#     mode: worktree-mtime\n")
	b.WriteString("# mcp:\n")
	b.WriteString("#   # Optional. MCP servers are declared only; provider wiring lands in later 0.5.0 slices.\n")
	b.WriteString("#   servers:\n")
	b.WriteString("#     - name: governance-index\n")
	b.WriteString("#       transport: stdio\n")
	b.WriteString("#       command: ./tools/governance-mcp\n")
	b.WriteString("#       args: [\"--root\", \".\"]\n")
	b.WriteString("#       roles: [worker, verifier]\n")
	b.WriteString("#       read_only: true\n")
	b.WriteString("#     - name: disclosure-system\n")
	b.WriteString("#       transport: http\n")
	b.WriteString("#       url: https://mcp.example.com/disclosure\n")
	b.WriteString("#       auth:\n")
	b.WriteString("#         header: Authorization\n")
	b.WriteString("#         env: DISCLOSURE_MCP_TOKEN\n")
	b.WriteString("#       roles: [worker]\n")
	b.WriteString("#       read_only: false\n")
	if hasVerifier {
		b.WriteString("verifier:\n")
		b.WriteString("  # Optional. Absent = inherit the verifier provider's global config. loopcoder never sets this on its own.\n")
		writeOptionalScalar(&b, "  ", "model", verifierModel)
		b.WriteString("  # Optional. Absent = inherit the verifier provider's global config. loopcoder never sets this on its own.\n")
		writeOptionalScalar(&b, "  ", "reasoning_effort", verifierEffort)
	} else {
		b.WriteString("# verifier:\n")
		b.WriteString("#   # Optional. Absent = inherit the verifier provider's global config. loopcoder never sets this on its own.\n")
		b.WriteString("#   # model:\n")
		b.WriteString("#   # Optional. Absent = inherit the verifier provider's global config. loopcoder never sets this on its own.\n")
		b.WriteString("#   # reasoning_effort:\n")
	}
	b.WriteString("# resilience:\n")
	b.WriteString("#   worker:\n")
	fmt.Fprintf(&b, "#     # Optional. Absent = %d; expected worker heartbeat cadence in seconds.\n", lcdefaults.WorkerHeartbeatIntervalSeconds)
	fmt.Fprintf(&b, "#     heartbeat_interval_seconds: %d\n", lcdefaults.WorkerHeartbeatIntervalSeconds)
	fmt.Fprintf(&b, "#     # Optional. Absent = %d; mark progress stale after this many seconds without phase or log growth.\n", lcdefaults.WorkerStaleAfterSeconds)
	fmt.Fprintf(&b, "#     stale_after_seconds: %d\n", lcdefaults.WorkerStaleAfterSeconds)
	fmt.Fprintf(&b, "#     # Optional. Absent = %d; classify hung after stale progress or stale sidecar with no live process.\n", lcdefaults.WorkerHungAfterSeconds)
	fmt.Fprintf(&b, "#     hung_after_seconds: %d\n", lcdefaults.WorkerHungAfterSeconds)
	fmt.Fprintf(&b, "#     # Optional. Absent = %d; maximum attempts before blocking for a human decision.\n", lcdefaults.WorkerMaxAttempts)
	fmt.Fprintf(&b, "#     max_attempts: %d\n", lcdefaults.WorkerMaxAttempts)
	fmt.Fprintf(&b, "#     # Optional. Absent = %s; retry backoff schedule between attempts.\n", formatInlineInts(lcdefaults.WorkerRetryBackoffSeconds()))
	fmt.Fprintf(&b, "#     retry_backoff_seconds: %s\n", formatInlineInts(lcdefaults.WorkerRetryBackoffSeconds()))
	b.WriteString("ci:\n")
	b.WriteString("  checks: []\n")
	b.WriteString("  # Optional. Absent = no local test command gate.\n")
	b.WriteString("  # tests:\n")
	b.WriteString("  #   - go test ./...\n")
	b.WriteString("  # Optional. Absent = no local typecheck command gate.\n")
	b.WriteString("  # typecheck:\n")
	b.WriteString("  #   - go vet ./...\n")
	b.WriteString("  # Optional. Absent = no local build command gate.\n")
	b.WriteString("  # build:\n")
	b.WriteString("  #   - go build ./...\n")
	b.WriteString("# verification:\n")
	b.WriteString("#   # Optional. Absent = true for loopcoder code PRs.\n")
	b.WriteString("#   spec_required: true\n")
	fmt.Fprintf(&b, "#   # Optional. Absent = %d.\n", lcdefaults.VerificationMaxFixPasses)
	fmt.Fprintf(&b, "#   max_fix_passes: %d\n", lcdefaults.VerificationMaxFixPasses)
	b.WriteString("#   browser:\n")
	fmt.Fprintf(&b, "#     # Optional. auto|always|never; absent = %s.\n", lcdefaults.VerificationBrowserMode)
	fmt.Fprintf(&b, "#     enabled: %s\n", lcdefaults.VerificationBrowserMode)
	b.WriteString("#     # Optional. Changed-path patterns that trigger browser verification in auto mode.\n")
	b.WriteString("#     globs:\n")
	b.WriteString("#       - web/**\n")
	b.WriteString("#       - app/**\n")
	b.WriteString("#       - \"**/*.css\"\n")
	b.WriteString("#       - \"**/*.tsx\"\n")
	b.WriteString("report:\n")
	b.WriteString("  channel: chat\n")
	return b.String()
}

func formatInlineInts(values []int) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, strconv.Itoa(value))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func normalizeScaffoldGate(gate string) string {
	gate = strings.TrimSpace(gate)
	if gate == "" {
		return "human-merge"
	}
	return gate
}

func validateGate(gate string) error {
	switch normalizeScaffoldGate(gate) {
	case "human-merge", "auto":
		return nil
	default:
		return fmt.Errorf("invalid adapters.gate %q; allowed values: human-merge, auto", strings.TrimSpace(gate))
	}
}

func displayRepoPath(repoPath string) string {
	repoPath = strings.TrimSpace(repoPath)
	if repoPath == "" {
		return "."
	}
	return repoPath
}

func writeOptionalScalar(b *strings.Builder, indent, key, value string) {
	if value == "" {
		fmt.Fprintf(b, "%s# %s:\n", indent, key)
		return
	}
	fmt.Fprintf(b, "%s%s: %s\n", indent, key, strconv.Quote(value))
}

const RoadmapTemplate = `# ROADMAP

<!--
Template for loopcoder work units.

Format:
- Each ## heading is one topic or unit.
- Each "- doc:" or "- code:" list item is one slice and becomes one issue.
- code slices depend on the doc slices in the same unit unless "(needs: ...)" is set.
- Slice refs are <unit-slug>/<kind>-<n>; within the same unit, <kind>-<n> works.
- Use "## [epic] ..." for a slice DAG; add "- doc:" / "- code:" lines for explicit slices.

The example below is illustrative only, not a real roadmap.
-->

## Example docs page
Create a short documentation page for one workflow.

- doc: Design the example docs page
- code: Add the example docs page

## Example checks
Add a lightweight check that verifies the docs page is linked.

- code: Add docs link check (needs: example-docs-page/code-1)

## [epic] Example migration
Describe one large task here. compile will emit an epic slice DAG.

- doc: Design the migration slice plan
- code: Add the first isolated migration slice
`

func ensureLabels(ctx context.Context, repoPath string, runner GitHubRunner) ([]LabelResult, []string) {
	output, err := runner.Run(ctx, repoPath, "label", "list", "--limit", strconv.Itoa(lcdefaults.GitHubListLimit), "--json", "name")
	if err != nil {
		return nil, []string{"gh label setup skipped: " + commandError(err, output)}
	}

	var labels []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(output, &labels); err != nil {
		return nil, []string{fmt.Sprintf("gh label setup skipped: parse label list: %v", err)}
	}

	existing := make(map[string]bool, len(labels))
	for _, label := range labels {
		if strings.TrimSpace(label.Name) != "" {
			existing[label.Name] = true
		}
	}

	results := make([]LabelResult, 0, len(defaultLabels))
	var warnings []string
	for _, label := range defaultLabels {
		if existing[label.Name] {
			results = append(results, LabelResult{Name: label.Name, Status: LabelExists})
			continue
		}
		output, err := runner.Run(ctx, repoPath, "label", "create", label.Name, "--color", label.Color, "--description", label.Description)
		if err != nil {
			if labelAlreadyExists(err, output) {
				results = append(results, LabelResult{Name: label.Name, Status: LabelExists})
				continue
			}
			warnings = append(warnings, fmt.Sprintf("gh label create %q failed: %s", label.Name, commandError(err, output)))
			continue
		}
		results = append(results, LabelResult{Name: label.Name, Status: LabelCreated})
	}
	return results, warnings
}

func labelAlreadyExists(err error, output []byte) bool {
	text := strings.ToLower(err.Error() + "\n" + string(output))
	return strings.Contains(text, "already exists") || strings.Contains(text, "name has already been taken")
}

func commandError(err error, output []byte) string {
	text := strings.TrimSpace(string(output))
	if text == "" {
		return err.Error()
	}
	return err.Error() + ": " + text
}

type osFileSystem struct{}

func (osFileSystem) Stat(name string) (fs.FileInfo, error) {
	return os.Stat(name)
}

func (osFileSystem) WriteFile(name string, data []byte, perm fs.FileMode) error {
	return os.WriteFile(name, data, perm)
}

type execGitHubRunner struct{}

func (execGitHubRunner) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, ghCommand, args...)
	cmd.Dir = dir

	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output

	result, err := supervisedexec.Run(ctx, cmd, supervisedexec.Options{HardCap: ghHardCap})
	if err != nil {
		return output.Bytes(), err
	}
	if result.Outcome == supervisedexec.OutcomeDeadline {
		return output.Bytes(), fmt.Errorf("gh %s timed out after %s", strings.Join(args, " "), ghHardCap)
	}
	if result.ExitCode != 0 {
		return output.Bytes(), execresult.CommandExitError(cmd, result.ExitCode)
	}
	return output.Bytes(), nil
}
