// Package scaffold initializes repository-local loopcoder files.
package scaffold

import (
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
)

const (
	DeliveryFilename = ".delivery.yml"
	RoadmapFilename  = "ROADMAP.md"
)

type Options struct {
	RepoPath       string
	Force          bool
	WorkerModel    string
	WorkerEffort   string
	VerifierModel  string
	VerifierEffort string
}

type Deps struct {
	FS     FileSystem
	GitHub GitHubRunner
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
	Files    []FileResult
	Labels   []LabelResult
	Warnings []string
}

type LabelSpec struct {
	Name        string
	Color       string
	Description string
}

var defaultLabels = []LabelSpec{
	{Name: "delivery:unit", Color: "0e8a16", Description: "loopcoder work unit"},
	{Name: "epic", Color: "5319e7", Description: "loopcoder epic issue"},
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
		FS:     osFileSystem{},
		GitHub: execGitHubRunner{},
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
	if strings.TrimSpace(opts.RepoPath) == "" {
		opts.RepoPath = "."
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
	b.WriteString("  gate: human-merge       # Humans choose what merges.\n")
	b.WriteString("worker:\n")
	b.WriteString("  # Optional. Absent = inherit the worker provider's global config. loopcoder never sets this on its own.\n")
	writeOptionalScalar(&b, "  ", "model", workerModel)
	b.WriteString("  # Optional. Absent = inherit the worker provider's global config. loopcoder never sets this on its own.\n")
	writeOptionalScalar(&b, "  ", "reasoning_effort", workerEffort)
	b.WriteString("  base_branch: main\n")
	b.WriteString("  command_hint: \"implement the issue, run relevant checks, commit\"\n")
	b.WriteString("environment:\n")
	b.WriteString("  pre_prod_branch: pre-prod # Tick auto-merges clean PRs here only; main remains human-only.\n")
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
	b.WriteString("#     # Optional. Absent = 15; expected worker heartbeat cadence in seconds.\n")
	b.WriteString("#     heartbeat_interval_seconds: 15\n")
	b.WriteString("#     # Optional. Absent = 120; mark progress stale after this many seconds without phase or log growth.\n")
	b.WriteString("#     stale_after_seconds: 120\n")
	b.WriteString("#     # Optional. Absent = 300; classify hung after stale progress or stale sidecar with no live process.\n")
	b.WriteString("#     hung_after_seconds: 300\n")
	b.WriteString("#     # Optional. Absent = 3; maximum attempts before blocking for a human decision.\n")
	b.WriteString("#     max_attempts: 3\n")
	b.WriteString("#     # Optional. Absent = [10, 30, 120]; retry backoff schedule between attempts.\n")
	b.WriteString("#     retry_backoff_seconds: [10, 30, 120]\n")
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
	b.WriteString("#   # Optional. Absent = 3.\n")
	b.WriteString("#   max_fix_passes: 3\n")
	b.WriteString("#   browser:\n")
	b.WriteString("#     # Optional. auto|always|never; absent = auto.\n")
	b.WriteString("#     enabled: auto\n")
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
- Use "## [epic] ..." for one epic issue that compile will not decompose.

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
Describe one large task here. compile will create exactly one epic issue.
`

func ensureLabels(ctx context.Context, repoPath string, runner GitHubRunner) ([]LabelResult, []string) {
	output, err := runner.Run(ctx, repoPath, "label", "list", "--limit", "1000", "--json", "name")
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
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}
