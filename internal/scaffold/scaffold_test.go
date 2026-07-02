package scaffold

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
)

func TestInitFreshRepoCreatesFilesAndMissingLabels(t *testing.T) {
	fsys := newFakeFileSystem()
	gh := &fakeGitHubRunner{
		listOutput: `[{"name":"status:ready"}]`,
	}

	result, err := Init(context.Background(), Options{RepoPath: "repo"}, Deps{
		FS:     fsys,
		GitHub: gh,
	})
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
		"checks: []",
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

	result, err := Init(context.Background(), Options{RepoPath: "repo"}, Deps{
		FS:     fsys,
		GitHub: gh,
	})
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
	}, Deps{
		FS:     fsys,
		GitHub: &fakeGitHubRunner{listOutput: allLabelsJSON(t)},
	})
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
	}, Deps{
		FS:     fsys,
		GitHub: &fakeGitHubRunner{listOutput: allLabelsJSON(t)},
	})
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

func TestInitGitHubUnavailableWarnsWithoutFailing(t *testing.T) {
	fsys := newFakeFileSystem()
	gh := &fakeGitHubRunner{
		listErr: errors.New("exec: gh not found"),
	}

	result, err := Init(context.Background(), Options{RepoPath: "repo"}, Deps{
		FS:     fsys,
		GitHub: gh,
	})
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
