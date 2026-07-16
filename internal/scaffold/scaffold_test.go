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
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/gitlocal"
	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/registry"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestInitFreshRepoCreatesFilesAndMissingLabels(t *testing.T) {
	fsys := newFakeFileSystem()
	gh := &fakeGitHubRunner{
		listOutput: `[{"name":"status:ready"}]`,
	}

	result, err := Init(context.Background(), Options{RepoPath: "repo"}, scaffoldDepsForTest(t, fsys, gh))
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}

	assertFileStatus(t, result.Files, DeliveryFilename, FileCreated)
	assertFileStatus(t, result.Files, RoadmapFilename, FileCreated)
	if len(result.Warnings) != 0 {
		t.Fatalf("Warnings = %#v, want empty", result.Warnings)
	}

	delivery := string(fsys.read(t, filepath.Join("repo", DeliveryFilename)))
	for _, want := range []string{
		"version: 1",
		"work_items: github",
		"worker: codex",
		"verifier: claude",
		"gate: human-merge",
		"First-run safe default",
		"pre_prod_branch: pre-prod",
		"checks: []",
		"# evidence:",
		"preview_url: https://preview.example.com",
		"example_output: |",
		"test_results: go test ./...",
		"preview_build: dist/app-preview.zip",
		"# domain:",
		"prompt_budget_bytes: 4096",
		"review_packet_order:",
		"include_in_loopreview: true",
		"partial_work:",
		"liveness:",
		"# mcp:",
		"transport: stdio",
		"auth:",
		"# model:",
		"# reasoning_effort:",
	} {
		if !strings.Contains(delivery, want) {
			t.Fatalf(".delivery.yml missing %q:\n%s", want, delivery)
		}
	}
	if strings.Contains(delivery, "\n  model:") || strings.Contains(delivery, "\n  reasoning_effort:") {
		t.Fatalf(".delivery.yml contains uncommented model/effort without flags:\n%s", delivery)
	}

	roadmap := string(fsys.read(t, filepath.Join("repo", RoadmapFilename)))
	if !strings.Contains(roadmap, "## Example docs page") || !strings.Contains(roadmap, "- doc:") || !strings.Contains(roadmap, "## [epic] Example migration") {
		t.Fatalf("ROADMAP.md missing template examples:\n%s", roadmap)
	}

	if !gh.createdLabel("delivery:unit") {
		t.Fatalf("delivery:unit label was not created; calls=%#v", gh.calls)
	}
	if !gh.createdLabel("epic") {
		t.Fatalf("epic label was not created; calls=%#v", gh.calls)
	}
	if gh.createdLabel("status:ready") {
		t.Fatalf("status:ready already existed but was created; calls=%#v", gh.calls)
	}
}

func TestInitExistingFilesDoesNotClobber(t *testing.T) {
	fsys := newFakeFileSystem()
	fsys.mustWrite(filepath.Join("repo", DeliveryFilename), []byte("custom delivery"))
	fsys.mustWrite(filepath.Join("repo", RoadmapFilename), []byte("custom roadmap"))
	gh := &fakeGitHubRunner{listOutput: allLabelsJSON(t)}

	result, err := Init(context.Background(), Options{RepoPath: "repo"}, scaffoldDepsForTest(t, fsys, gh))
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}

	assertFileStatus(t, result.Files, DeliveryFilename, FileExists)
	assertFileStatus(t, result.Files, RoadmapFilename, FileExists)
	if got := string(fsys.read(t, filepath.Join("repo", DeliveryFilename))); got != "custom delivery" {
		t.Fatalf(".delivery.yml = %q, want original content", got)
	}
	if got := string(fsys.read(t, filepath.Join("repo", RoadmapFilename))); got != "custom roadmap" {
		t.Fatalf("ROADMAP.md = %q, want original content", got)
	}
	if gh.createCallCount() != 0 {
		t.Fatalf("label create calls = %d, want 0; calls=%#v", gh.createCallCount(), gh.calls)
	}
}

func TestInitForceOverwritesExistingFiles(t *testing.T) {
	fsys := newFakeFileSystem()
	fsys.mustWrite(filepath.Join("repo", DeliveryFilename), []byte("custom delivery"))
	fsys.mustWrite(filepath.Join("repo", RoadmapFilename), []byte("custom roadmap"))

	result, err := Init(context.Background(), Options{
		RepoPath: "repo",
		Force:    true,
	}, scaffoldDepsForTest(t, fsys, &fakeGitHubRunner{listOutput: allLabelsJSON(t)}))
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}

	assertFileStatus(t, result.Files, DeliveryFilename, FileOverwritten)
	assertFileStatus(t, result.Files, RoadmapFilename, FileOverwritten)
	if got := string(fsys.read(t, filepath.Join("repo", DeliveryFilename))); !strings.Contains(got, "version: 1") {
		t.Fatalf(".delivery.yml was not overwritten with template:\n%s", got)
	}
	if got := string(fsys.read(t, filepath.Join("repo", RoadmapFilename))); !strings.Contains(got, "Template for loopcoder work units.") {
		t.Fatalf("ROADMAP.md was not overwritten with template:\n%s", got)
	}
}

func TestInitModelFlagsPersistRoleValues(t *testing.T) {
	fsys := newFakeFileSystem()

	_, err := Init(context.Background(), Options{
		RepoPath:       "repo",
		WorkerModel:    "gpt-5",
		WorkerEffort:   "high",
		VerifierModel:  "claude-sonnet-4-5",
		VerifierEffort: "max",
	}, scaffoldDepsForTest(t, fsys, &fakeGitHubRunner{listOutput: allLabelsJSON(t)}))
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}

	cfg, err := config.Parse(fsys.read(t, filepath.Join("repo", DeliveryFilename)))
	if err != nil {
		t.Fatalf("config.Parse returned error: %v", err)
	}
	if cfg.Worker.Model != "gpt-5" || cfg.Worker.ReasoningEffort != "high" {
		t.Fatalf("Worker config = %#v, want model and effort from flags", cfg.Worker)
	}
	if cfg.Verifier.Model != "claude-sonnet-4-5" || cfg.Verifier.ReasoningEffort != "max" {
		t.Fatalf("Verifier config = %#v, want model and effort from flags", cfg.Verifier)
	}
}

func TestInitGateAutoGeneratesAutoGate(t *testing.T) {
	fsys := newFakeFileSystem()

	_, err := Init(context.Background(), Options{
		RepoPath: "repo",
		Gate:     "auto",
	}, scaffoldDepsForTest(t, fsys, &fakeGitHubRunner{listOutput: allLabelsJSON(t)}))
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}

	delivery := string(fsys.read(t, filepath.Join("repo", DeliveryFilename)))
	if !strings.Contains(delivery, "gate: auto") {
		t.Fatalf(".delivery.yml missing explicit auto gate:\n%s", delivery)
	}
	if strings.Contains(delivery, "gate: human-merge") {
		t.Fatalf(".delivery.yml contains human gate for explicit auto:\n%s", delivery)
	}
}

func TestInitRejectsInvalidGateBeforeWrites(t *testing.T) {
	fsys := newFakeFileSystem()

	_, err := Init(context.Background(), Options{
		RepoPath: "repo",
		Gate:     "bogus",
	}, scaffoldDepsForTest(t, fsys, &fakeGitHubRunner{listOutput: allLabelsJSON(t)}))
	if err == nil || !strings.Contains(err.Error(), "allowed values: human-merge, auto") {
		t.Fatalf("Init error = %v, want invalid gate", err)
	}
	if len(fsys.files) != 0 {
		t.Fatalf("files were written despite invalid gate: %#v", fsys.files)
	}
}

func TestInitProtectsLocalLoopcoderState(t *testing.T) {
	fsys := newFakeFileSystem()
	called := false

	result, err := Init(context.Background(), Options{RepoPath: "repo"}, Deps{
		FS:     fsys,
		GitHub: &fakeGitHubRunner{listOutput: allLabelsJSON(t)},
		Getenv: func(key string) string {
			if key == home.EnvHome {
				return t.TempDir()
			}
			return ""
		},
		UserHomeDir: func() (string, error) { return t.TempDir(), nil },
		RunGit: func(context.Context, string, ...string) (string, error) {
			return "", nil
		},
		InspectLocalState: func(context.Context, string) (*LocalStateResult, *Mutation, error) {
			return &LocalStateResult{Path: filepath.Join("repo", ".git", "info", "exclude"), Status: gitlocal.ProtectUnchanged}, nil, nil
		},
		ResolveProject: fakeResolveProject,
		RegisterProject: func(ctx context.Context, opts registry.Options) (registry.RegisterResult, error) {
			project, err := fakeResolveProject(ctx, opts)
			return registry.RegisterResult{Project: project, Created: true}, err
		},
		ProtectLocalState: func(_ context.Context, repoPath string) (gitlocal.ProtectResult, error) {
			called = true
			if repoPath != "repo" {
				t.Fatalf("ProtectLocalState repoPath = %q, want repo", repoPath)
			}
			return gitlocal.ProtectResult{ExcludePath: filepath.Join("repo", ".git", "info", "exclude"), Status: gitlocal.ProtectUpdated}, nil
		},
	})
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}
	if !called {
		t.Fatal("ProtectLocalState was not called")
	}
	if result.LocalStateExclude == nil || result.LocalStateExclude.Path != filepath.Join("repo", ".git", "info", "exclude") || result.LocalStateExclude.Status != gitlocal.ProtectUpdated {
		t.Fatalf("LocalStateExclude = %#v", result.LocalStateExclude)
	}
}

func TestInitGitHubUnavailableWarnsWithoutFailing(t *testing.T) {
	fsys := newFakeFileSystem()
	gh := &fakeGitHubRunner{
		listErr: errors.New("exec: gh not found"),
	}

	result, err := Init(context.Background(), Options{RepoPath: "repo"}, scaffoldDepsForTest(t, fsys, gh))
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}

	assertFileStatus(t, result.Files, DeliveryFilename, FileCreated)
	assertFileStatus(t, result.Files, RoadmapFilename, FileCreated)
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "gh label setup skipped") {
		t.Fatalf("Warnings = %#v, want gh skipped warning", result.Warnings)
	}
	if gh.createCallCount() != 0 {
		t.Fatalf("label create calls = %d, want 0; calls=%#v", gh.createCallCount(), gh.calls)
	}
}

func TestPreviewDirtyRepositoryWarnsWithoutMutation(t *testing.T) {
	fsys := newFakeFileSystem()
	deps := scaffoldDepsForTest(t, fsys, &fakeGitHubRunner{listOutput: allLabelsJSON(t)})
	deps.RunGit = func(_ context.Context, _ string, args ...string) (string, error) {
		if reflect.DeepEqual(args, []string{"status", "--porcelain"}) {
			return " M tracked.go\n?? untracked.txt\n", nil
		}
		return "", nil
	}

	result, err := Preview(context.Background(), Options{RepoPath: "repo"}, deps)
	if err != nil {
		t.Fatalf("Preview returned error: %v", err)
	}
	if result.Dirty == nil || !result.Dirty.Dirty || !strings.Contains(result.Dirty.Porcelain, "tracked.go") {
		t.Fatalf("Dirty = %#v, want porcelain warning", result.Dirty)
	}
	if !containsWarning(result.Warnings, "repository has uncommitted changes") {
		t.Fatalf("Warnings = %#v, want dirty warning", result.Warnings)
	}
	if len(fsys.files) != 0 {
		t.Fatalf("preview wrote files: %#v", fsys.files)
	}
}

func TestInitRuntimePermissionFailureBlocksBeforeRegistryOrFiles(t *testing.T) {
	fsys := newFakeFileSystem()
	homeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(homeDir, "projects"), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write projects blocker: %v", err)
	}
	registerCalled := false
	deps := scaffoldDepsForTest(t, fsys, &fakeGitHubRunner{listOutput: allLabelsJSON(t)})
	deps.Getenv = func(key string) string {
		if key == home.EnvHome {
			return homeDir
		}
		return ""
	}
	deps.UserHomeDir = func() (string, error) { return homeDir, nil }
	deps.RegisterProject = func(context.Context, registry.Options) (registry.RegisterResult, error) {
		registerCalled = true
		return registry.RegisterResult{}, errors.New("register should not be called")
	}

	result, err := Init(context.Background(), Options{RepoPath: "repo"}, deps)
	var blocked *BlockedError
	if !errors.As(err, &blocked) || blocked.Code != "runtime-root-permission" {
		t.Fatalf("Init error = %v, want runtime-root-permission BlockedError", err)
	}
	if result.Outcome != OutcomeBlocked || result.Blocked == nil || result.Blocked.Code != "runtime-root-permission" {
		t.Fatalf("blocked result = %#v", result.Blocked)
	}
	if registerCalled {
		t.Fatal("RegisterProject was called after runtime dir failure")
	}
	if len(fsys.files) != 0 {
		t.Fatalf("files were written after blocked runtime setup: %#v", fsys.files)
	}
	if _, err := os.Stat(filepath.Join(homeDir, "data", "loopcoder.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("database stat err = %v, want not exist", err)
	}
}

func TestInitAlreadyConfiguredSkipsRegistryUpdate(t *testing.T) {
	fsys := newFakeFileSystem()
	fsys.mustWrite(filepath.Join("repo", DeliveryFilename), []byte("custom delivery"))
	fsys.mustWrite(filepath.Join("repo", RoadmapFilename), []byte("custom roadmap"))
	homeDir := t.TempDir()
	dbPath := filepath.Join(homeDir, "data", "loopcoder.db")
	store, err := storage.Open(context.Background(), storage.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	project := registry.Project{
		ProjectID:          "proj_test",
		DisplayName:        "repo",
		LocalPath:          filepath.Clean("repo"),
		LocalPathCanonical: filepath.Clean("repo"),
		IdentitySource:     registry.IdentityLocalPath,
	}
	if err := store.WithTx(context.Background(), func(tx storage.Tx) error {
		_, err := tx.Exec(context.Background(), `INSERT INTO projects(id, display_name, local_path, local_path_canonical, git_root, default_branch, remote_url, remote_url_normalized, github_owner, github_name, identity_source, created_at, updated_at, detached_at) VALUES (?, ?, ?, ?, '', '', '', '', '', '', 'local-path', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '')`,
			project.ProjectID, project.DisplayName, project.LocalPath, project.LocalPathCanonical)
		return err
	}); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	store.Close()
	layout := home.New(homeDir)
	for _, dir := range []string{
		filepath.Join(layout.ProjectDir(project.ProjectID), "runs"),
		filepath.Join(layout.ProjectDir(project.ProjectID), "relay"),
		filepath.Join(layout.ProjectDir(project.ProjectID), "recovery"),
		filepath.Join(layout.ProjectDir(project.ProjectID), "audit"),
		layout.LogsDir(),
		layout.TmpDir(),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	registerCalled := false
	deps := scaffoldDepsForTest(t, fsys, &fakeGitHubRunner{listOutput: allLabelsJSON(t)})
	deps.Getenv = func(key string) string {
		if key == home.EnvHome {
			return homeDir
		}
		return ""
	}
	deps.UserHomeDir = func() (string, error) { return homeDir, nil }
	deps.RegisterProject = func(context.Context, registry.Options) (registry.RegisterResult, error) {
		registerCalled = true
		return registry.RegisterResult{}, nil
	}

	result, err := Init(context.Background(), Options{RepoPath: "repo"}, deps)
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}
	if result.Outcome != OutcomeAlreadyConfigured || len(result.Mutations) != 0 {
		t.Fatalf("result outcome=%q mutations=%#v, want already-configured with no mutations", result.Outcome, result.Mutations)
	}
	if registerCalled {
		t.Fatal("RegisterProject was called for already-registered setup")
	}
}

func TestPreviewMultiCheckoutCanonicalConflictIsDeterministic(t *testing.T) {
	fsys := newFakeFileSystem()
	homeDir := t.TempDir()
	dbPath := filepath.Join(homeDir, "data", "loopcoder.db")
	store, err := storage.Open(context.Background(), storage.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	canonical := filepath.Join(t.TempDir(), "repo")
	if err := store.WithTx(context.Background(), func(tx storage.Tx) error {
		for _, id := range []string{"proj_a", "proj_b"} {
			if _, err := tx.Exec(context.Background(), `INSERT INTO projects(id, display_name, local_path, local_path_canonical, git_root, default_branch, remote_url, remote_url_normalized, github_owner, github_name, identity_source, created_at, updated_at, detached_at) VALUES (?, ?, ?, ?, ?, '', '', '', '', '', 'local-path', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '')`,
				id, id, canonical, canonical, canonical); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("insert projects: %v", err)
	}
	store.Close()

	deps := scaffoldDepsForTest(t, fsys, &fakeGitHubRunner{listOutput: allLabelsJSON(t)})
	deps.Getenv = func(key string) string {
		if key == home.EnvHome {
			return homeDir
		}
		return ""
	}
	deps.UserHomeDir = func() (string, error) { return homeDir, nil }
	deps.ResolveProject = func(context.Context, registry.Options) (registry.Project, error) {
		return registry.Project{
			ProjectID:          "proj_current",
			DisplayName:        "repo",
			LocalPath:          canonical,
			LocalPathCanonical: canonical,
			GitRoot:            canonical,
			IdentitySource:     registry.IdentityLocalPath,
		}, nil
	}

	result, err := Preview(context.Background(), Options{RepoPath: "repo"}, deps)
	if err != nil {
		t.Fatalf("Preview returned error: %v", err)
	}
	if result.Outcome != OutcomeBlocked || result.Blocked == nil || result.Blocked.Code != "project-registry-conflict" {
		t.Fatalf("Outcome/block = %q/%#v, want registry conflict", result.Outcome, result.Blocked)
	}
	if gotIDs := conflictIDs(result.Conflicts); !reflect.DeepEqual(gotIDs, []string{"proj_a", "proj_b"}) {
		t.Fatalf("conflict IDs = %#v, want deterministic proj_a/proj_b", gotIDs)
	}
	if len(fsys.files) != 0 {
		t.Fatalf("preview wrote files: %#v", fsys.files)
	}
}

func scaffoldDepsForTest(t *testing.T, fsys FileSystem, gh GitHubRunner) Deps {
	t.Helper()
	homeDir := t.TempDir()
	return Deps{
		FS:     fsys,
		GitHub: gh,
		Getenv: func(key string) string {
			if key == home.EnvHome {
				return homeDir
			}
			return ""
		},
		UserHomeDir: func() (string, error) { return homeDir, nil },
		RunGit: func(context.Context, string, ...string) (string, error) {
			return "", nil
		},
		InspectLocalState: func(context.Context, string) (*LocalStateResult, *Mutation, error) {
			return &LocalStateResult{Path: filepath.Join("repo", ".git", "info", "exclude"), Status: gitlocal.ProtectUnchanged}, nil, nil
		},
		ResolveProject: fakeResolveProject,
		RegisterProject: func(ctx context.Context, opts registry.Options) (registry.RegisterResult, error) {
			project, err := fakeResolveProject(ctx, opts)
			return registry.RegisterResult{Project: project, Created: true}, err
		},
		ProtectLocalState: func(context.Context, string) (gitlocal.ProtectResult, error) {
			return gitlocal.ProtectResult{ExcludePath: filepath.Join("repo", ".git", "info", "exclude"), Status: gitlocal.ProtectUnchanged}, nil
		},
	}
}

func fakeResolveProject(_ context.Context, _ registry.Options) (registry.Project, error) {
	return registry.Project{
		ProjectID:          "proj_test",
		DisplayName:        "repo",
		LocalPath:          filepath.Clean("repo"),
		LocalPathCanonical: filepath.Clean("repo"),
		IdentitySource:     registry.IdentityLocalPath,
	}, nil
}

func containsWarning(warnings []string, want string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, want) {
			return true
		}
	}
	return false
}

func conflictIDs(projects []registry.Project) []string {
	out := make([]string, 0, len(projects))
	for _, project := range projects {
		out = append(out, project.ProjectID)
	}
	return out
}

func TestExecGitHubRunnerCombinedOutputAndNonZeroExit(t *testing.T) {
	withTestGHCommand(t, 2*time.Second)
	dir := t.TempDir()

	output, err := (execGitHubRunner{}).Run(context.Background(), dir, "-test.run=TestScaffoldExecHelper", "--", "combined-exit", "stdout", "stderr", "7")
	if err == nil {
		t.Fatal("Run error = nil, want non-zero exit error")
	}
	text := string(output)
	if !strings.Contains(text, "stdout") || !strings.Contains(text, "stderr") {
		t.Fatalf("output = %q, want combined stdout and stderr", text)
	}
	if err.Error() != "exit status 7" {
		t.Fatalf("error = %q, want exit status 7", err.Error())
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("error = %v, want exec.ExitError exit 7", err)
	}
}

func TestExecGitHubRunnerTimesOut(t *testing.T) {
	withTestGHCommand(t, 50*time.Millisecond)
	dir := t.TempDir()

	start := time.Now()
	output, err := (execGitHubRunner{}).Run(context.Background(), dir, "-test.run=TestScaffoldExecHelper", "--", "sleep", "500ms")
	if err == nil {
		t.Fatal("Run error = nil, want timeout")
	}
	if len(output) != 0 {
		t.Fatalf("output = %q, want no output", output)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Run elapsed = %s, want bounded timeout", elapsed)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %q, want timeout", err.Error())
	}
}

func withTestGHCommand(t *testing.T, hardCap time.Duration) {
	t.Helper()
	oldCommand := ghCommand
	oldHardCap := ghHardCap
	ghCommand = os.Args[0]
	ghHardCap = hardCap
	t.Setenv("GO_WANT_SCAFFOLD_HELPER", "1")
	t.Cleanup(func() {
		ghCommand = oldCommand
		ghHardCap = oldHardCap
	})
}

func TestScaffoldExecHelper(t *testing.T) {
	if os.Getenv("GO_WANT_SCAFFOLD_HELPER") != "1" {
		return
	}
	runExecHelper()
}

func runExecHelper() {
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		fmt.Fprintln(os.Stderr, "missing helper mode")
		os.Exit(2)
	}
	mode := os.Args[separator+1]
	args := os.Args[separator+2:]
	switch mode {
	case "combined-exit":
		fmt.Fprintln(os.Stdout, args[0])
		fmt.Fprintln(os.Stderr, args[1])
		os.Exit(parseHelperInt(args[2]))
	case "sleep":
		time.Sleep(parseHelperDuration(args[0]))
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
		os.Exit(2)
	}
	os.Exit(0)
}

func parseHelperDuration(value string) time.Duration {
	duration, err := time.ParseDuration(value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse duration %q: %v\n", value, err)
		os.Exit(2)
	}
	return duration
}

func parseHelperInt(value string) int {
	var n int
	if _, err := fmt.Sscanf(value, "%d", &n); err != nil {
		fmt.Fprintf(os.Stderr, "parse int %q: %v\n", value, err)
		os.Exit(2)
	}
	return n
}

func assertFileStatus(t *testing.T, files []FileResult, path string, status FileStatus) {
	t.Helper()
	for _, file := range files {
		if file.Path == path {
			if file.Status != status {
				t.Fatalf("%s status = %s, want %s", path, file.Status, status)
			}
			return
		}
	}
	t.Fatalf("%s not found in file results: %#v", path, files)
}

func allLabelsJSON(t *testing.T) string {
	t.Helper()
	type label struct {
		Name string `json:"name"`
	}
	labels := make([]label, 0, len(defaultLabels))
	for _, spec := range defaultLabels {
		labels = append(labels, label{Name: spec.Name})
	}
	data, err := json.Marshal(labels)
	if err != nil {
		t.Fatalf("json.Marshal labels: %v", err)
	}
	return string(data)
}

type fakeFileSystem struct {
	files map[string][]byte
}

func newFakeFileSystem() *fakeFileSystem {
	return &fakeFileSystem{files: make(map[string][]byte)}
}

func (f *fakeFileSystem) Stat(name string) (fs.FileInfo, error) {
	clean := filepath.Clean(name)
	data, ok := f.files[clean]
	if !ok {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
	}
	return fakeFileInfo{name: filepath.Base(name), size: int64(len(data))}, nil
}

func (f *fakeFileSystem) WriteFile(name string, data []byte, _ fs.FileMode) error {
	clean := filepath.Clean(name)
	copied := append([]byte(nil), data...)
	f.files[clean] = copied
	return nil
}

func (f *fakeFileSystem) mustWrite(name string, data []byte) {
	if err := f.WriteFile(name, data, 0o644); err != nil {
		panic(err)
	}
}

func (f *fakeFileSystem) read(t *testing.T, name string) []byte {
	t.Helper()
	data, ok := f.files[filepath.Clean(name)]
	if !ok {
		t.Fatalf("file %q not found; files=%#v", name, f.files)
	}
	return append([]byte(nil), data...)
}

type fakeFileInfo struct {
	name string
	size int64
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return f.size }
func (f fakeFileInfo) Mode() fs.FileMode  { return 0o644 }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return false }
func (f fakeFileInfo) Sys() any           { return nil }

type fakeGitHubRunner struct {
	listOutput string
	listErr    error
	calls      [][]string
}

func (f *fakeGitHubRunner) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	if reflect.DeepEqual(args, []string{"label", "list", "--limit", "1000", "--json", "name"}) {
		if f.listErr != nil {
			return nil, f.listErr
		}
		if f.listOutput == "" {
			return []byte("[]"), nil
		}
		return []byte(f.listOutput), nil
	}
	if len(args) >= 3 && args[0] == "label" && args[1] == "create" {
		return []byte(""), nil
	}
	return nil, errors.New("unexpected gh call: " + strings.Join(args, " "))
}

func (f *fakeGitHubRunner) createdLabel(name string) bool {
	for _, call := range f.calls {
		if len(call) >= 3 && call[0] == "label" && call[1] == "create" && call[2] == name {
			return true
		}
	}
	return false
}

func (f *fakeGitHubRunner) createCallCount() int {
	count := 0
	for _, call := range f.calls {
		if len(call) >= 2 && call[0] == "label" && call[1] == "create" {
			count++
		}
	}
	return count
}
