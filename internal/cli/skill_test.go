package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/claudehooks"
)

func TestSkillInstallHelpDocumentsFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := Run([]string{"skill", "install", "--help"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("Run returned exit code %d, want 0", exitCode)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	help := stdout.String()
	for _, want := range []string{"loopcoder skill install", "--dir", "~/.claude/skills/loopcoder", "--repo", "--force"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q:\n%s", want, help)
		}
	}
}

func TestSkillInstallRunsWithInjectedDepsAndAliases(t *testing.T) {
	var stdout, stderr bytes.Buffer
	target := filepath.Join("home", ".claude", "skills", "loopcoder")
	project := filepath.Join("work", "repo")
	called := false

	exitCode := RunWithDeps([]string{
		"skill",
		"install",
		"-Dir", target,
		"-Repo", project,
		"-Force",
	}, &stdout, &stderr, Deps{
		SkillInstall: func(_ context.Context, opts SkillInstallOptions) (SkillInstallResult, error) {
			called = true
			if opts.Dir != target || opts.ProjectDir != project || !opts.Force {
				t.Fatalf("skill install opts = %#v", opts)
			}
			return SkillInstallResult{
				Dir: target,
				Files: []SkillInstallFileResult{
					{Path: filepath.Join(target, skillFilename), Status: SkillInstallFileOverwritten},
					{Path: filepath.Join(target, agentsFilename), Status: SkillInstallFileUpdated},
				},
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if !called {
		t.Fatal("SkillInstall dependency was not called")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	for _, want := range []string{
		"loopcoder skill install complete",
		"directory " + target,
		"overwritten " + filepath.Join(target, skillFilename),
		"updated " + filepath.Join(target, agentsFilename),
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestInstallSkillWritesDefaultClaudeSkillDir(t *testing.T) {
	fsys := newSkillFakeFS()

	result, err := InstallSkill(context.Background(), SkillInstallOptions{}, skillDepsForTest(fsys))
	if err != nil {
		t.Fatalf("InstallSkill returned error: %v", err)
	}

	wantDir := filepath.Join("home", ".claude", "skills", "loopcoder")
	if result.Dir != wantDir {
		t.Fatalf("Dir = %q, want %q", result.Dir, wantDir)
	}
	if !fsys.dirs[wantDir] {
		t.Fatalf("directory %q was not created; dirs=%#v", wantDir, fsys.dirs)
	}
	assertSkillInstallStatus(t, result.Files, filepath.Join(wantDir, skillFilename), SkillInstallFileCreated)
	assertSkillInstallStatus(t, result.Files, filepath.Join(wantDir, agentsFilename), SkillInstallFileCreated)
	if got := string(fsys.read(t, filepath.Join(wantDir, skillFilename))); got != "skill content\n" {
		t.Fatalf("SKILL.md = %q, want embedded skill content", got)
	}
	if got := string(fsys.read(t, filepath.Join(wantDir, agentsFilename))); got != "agents content\n" {
		t.Fatalf("AGENTS.md = %q, want embedded agents content", got)
	}
	if result.HookSettings == nil {
		t.Fatal("HookSettings is nil, want project hook settings result")
	}
	if result.HookSettings.Path != claudehooks.SettingsPath(".") {
		t.Fatalf("HookSettings.Path = %q, want default project settings", result.HookSettings.Path)
	}
}

func TestInstallSkillMergesHookSettingsFreshAndIdempotent(t *testing.T) {
	fsys := newSkillFakeFS()
	project := "repo"
	settingsPath := claudehooks.SettingsPath(project)

	result, err := InstallSkill(context.Background(), SkillInstallOptions{ProjectDir: project}, skillDepsForTest(fsys))
	if err != nil {
		t.Fatalf("InstallSkill returned error: %v", err)
	}
	if result.HookSettings == nil {
		t.Fatal("HookSettings is nil")
	}
	if result.HookSettings.Path != settingsPath || result.HookSettings.Status != SkillInstallFileCreated {
		t.Fatalf("HookSettings = %#v, want created %s", result.HookSettings, settingsPath)
	}
	first := fsys.read(t, settingsPath)
	assertNoMissingRequiredHooks(t, first)
	assertHookCommandCounts(t, first, 2)

	result, err = InstallSkill(context.Background(), SkillInstallOptions{ProjectDir: project}, skillDepsForTest(fsys))
	if err != nil {
		t.Fatalf("second InstallSkill returned error: %v", err)
	}
	if result.HookSettings == nil || result.HookSettings.Status != SkillInstallFileUnchanged {
		t.Fatalf("second HookSettings = %#v, want unchanged", result.HookSettings)
	}
	second := fsys.read(t, settingsPath)
	if !bytes.Equal(first, second) {
		t.Fatalf("settings changed on idempotent re-run:\nfirst=%s\nsecond=%s", first, second)
	}
	assertHookCommandCounts(t, second, 2)
}

func TestInstallSkillPreservesExistingClaudeSettings(t *testing.T) {
	fsys := newSkillFakeFS()
	project := "repo"
	settingsPath := claudehooks.SettingsPath(project)
	fsys.mustWrite(settingsPath, []byte(`{
  "permissions": {
    "allow": ["Bash(git status)"]
  },
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "node hooks/user-hook.js",
            "timeout": 3
          }
        ]
      }
    ],
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "node hooks/pre-existing.js",
            "timeout": 5
          }
        ]
      }
    ]
  }
}`))

	result, err := InstallSkill(context.Background(), SkillInstallOptions{ProjectDir: project}, skillDepsForTest(fsys))
	if err != nil {
		t.Fatalf("InstallSkill returned error: %v", err)
	}
	if result.HookSettings == nil || result.HookSettings.Status != SkillInstallFileUpdated {
		t.Fatalf("HookSettings = %#v, want updated", result.HookSettings)
	}

	data := fsys.read(t, settingsPath)
	assertNoMissingRequiredHooks(t, data)
	assertHookCommandCounts(t, data, 2)
	if !strings.Contains(string(data), "Bash(git status)") {
		t.Fatalf("settings lost unrelated permissions:\n%s", data)
	}
	if !strings.Contains(string(data), "node hooks/user-hook.js") || !strings.Contains(string(data), "node hooks/pre-existing.js") {
		t.Fatalf("settings lost unrelated hooks:\n%s", data)
	}
}

func TestInstallSkillRejectsMalformedClaudeSettings(t *testing.T) {
	fsys := newSkillFakeFS()
	project := "repo"
	settingsPath := claudehooks.SettingsPath(project)
	fsys.mustWrite(settingsPath, []byte(`{"hooks":`))

	_, err := InstallSkill(context.Background(), SkillInstallOptions{ProjectDir: project}, skillDepsForTest(fsys))
	if err == nil {
		t.Fatal("InstallSkill returned nil error, want malformed settings failure")
	}
	if !strings.Contains(err.Error(), "merge Claude Code settings") || !strings.Contains(err.Error(), "parse Claude Code settings JSON") {
		t.Fatalf("error = %v, want clear settings parse failure", err)
	}
	if got := string(fsys.read(t, settingsPath)); got != `{"hooks":` {
		t.Fatalf("malformed settings were rewritten: %q", got)
	}
}

func TestInstallSkillUpdatesStaleFilesWithoutForce(t *testing.T) {
	fsys := newSkillFakeFS()
	target := filepath.Join("skills", "loopcoder")
	fsys.mustWrite(filepath.Join(target, skillFilename), []byte("custom skill"))
	fsys.mustWrite(filepath.Join(target, agentsFilename), []byte("custom agents"))
	fsys.mustWrite(filepath.Join(target, "README.md"), []byte("user note"))

	result, err := InstallSkill(context.Background(), SkillInstallOptions{Dir: target}, skillDepsForTest(fsys))
	if err != nil {
		t.Fatalf("InstallSkill returned error: %v", err)
	}

	assertSkillInstallStatus(t, result.Files, filepath.Join(target, skillFilename), SkillInstallFileUpdated)
	assertSkillInstallStatus(t, result.Files, filepath.Join(target, agentsFilename), SkillInstallFileUpdated)
	if got := string(fsys.read(t, filepath.Join(target, skillFilename))); got != "skill content\n" {
		t.Fatalf("SKILL.md = %q, want embedded skill content", got)
	}
	if got := string(fsys.read(t, filepath.Join(target, agentsFilename))); got != "agents content\n" {
		t.Fatalf("AGENTS.md = %q, want embedded agents content", got)
	}
	if got := string(fsys.read(t, filepath.Join(target, "README.md"))); got != "user note" {
		t.Fatalf("unrelated file = %q, want preserved user note", got)
	}
	if len(result.Files) != 2 {
		t.Fatalf("file results = %#v, want only managed files", result.Files)
	}
}

func TestInstallSkillReportsUnchangedWhenManagedFilesMatch(t *testing.T) {
	fsys := newSkillFakeFS()
	target := filepath.Join("skills", "loopcoder")
	fsys.mustWrite(filepath.Join(target, skillFilename), []byte("skill content\n"))
	fsys.mustWrite(filepath.Join(target, agentsFilename), []byte("agents content\n"))

	result, err := InstallSkill(context.Background(), SkillInstallOptions{Dir: target}, skillDepsForTest(fsys))
	if err != nil {
		t.Fatalf("InstallSkill returned error: %v", err)
	}

	assertSkillInstallStatus(t, result.Files, filepath.Join(target, skillFilename), SkillInstallFileUnchanged)
	assertSkillInstallStatus(t, result.Files, filepath.Join(target, agentsFilename), SkillInstallFileUnchanged)
}

func TestInstallSkillForceOverwritesExistingFiles(t *testing.T) {
	fsys := newSkillFakeFS()
	target := filepath.Join("skills", "loopcoder")
	fsys.mustWrite(filepath.Join(target, skillFilename), []byte("custom skill"))
	fsys.mustWrite(filepath.Join(target, agentsFilename), []byte("custom agents"))

	result, err := InstallSkill(context.Background(), SkillInstallOptions{Dir: target, Force: true}, skillDepsForTest(fsys))
	if err != nil {
		t.Fatalf("InstallSkill returned error: %v", err)
	}

	assertSkillInstallStatus(t, result.Files, filepath.Join(target, skillFilename), SkillInstallFileOverwritten)
	assertSkillInstallStatus(t, result.Files, filepath.Join(target, agentsFilename), SkillInstallFileOverwritten)
	if got := string(fsys.read(t, filepath.Join(target, skillFilename))); got != "skill content\n" {
		t.Fatalf("SKILL.md = %q, want embedded skill content", got)
	}
	if got := string(fsys.read(t, filepath.Join(target, agentsFilename))); got != "agents content\n" {
		t.Fatalf("AGENTS.md = %q, want embedded agents content", got)
	}
}

func TestInstallSkillReportsDirectoryCreateError(t *testing.T) {
	fsys := newSkillFakeFS()
	fsys.mkdirErr = errors.New("permission denied")
	target := filepath.Join("skills", "loopcoder")

	result, err := InstallSkill(context.Background(), SkillInstallOptions{Dir: target}, skillDepsForTest(fsys))
	if err == nil {
		t.Fatal("InstallSkill returned nil error, want mkdir failure")
	}
	if result.Dir != target {
		t.Fatalf("Dir = %q, want partial target %q", result.Dir, target)
	}
	for _, want := range []string{"create skill directory", target, "permission denied"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q: %v", want, err)
		}
	}
}

func TestInstallSkillRejectsEmptyEmbeddedContent(t *testing.T) {
	fsys := newSkillFakeFS()
	deps := skillDepsForTest(fsys)
	deps.SkillMarkdown = func() ([]byte, error) {
		return []byte(" \n\t"), nil
	}

	_, err := InstallSkill(context.Background(), SkillInstallOptions{Dir: "target"}, deps)
	if err == nil {
		t.Fatal("InstallSkill returned nil error, want empty embedded file failure")
	}
	if !strings.Contains(err.Error(), "embedded SKILL.md is empty") {
		t.Fatalf("error = %v, want empty embedded file message", err)
	}
	if len(fsys.files) != 0 {
		t.Fatalf("files were written despite empty embedded content: %#v", fsys.files)
	}
}

func skillDepsForTest(fsys *skillFakeFS) SkillInstallDeps {
	return SkillInstallDeps{
		UserHomeDir: func() (string, error) {
			return "home", nil
		},
		Stat:      fsys.Stat,
		MkdirAll:  fsys.MkdirAll,
		ReadFile:  fsys.ReadFile,
		WriteFile: fsys.WriteFile,
		SkillMarkdown: func() ([]byte, error) {
			return []byte("skill content\n"), nil
		},
		AgentsMarkdown: func() ([]byte, error) {
			return []byte("agents content\n"), nil
		},
	}
}

func assertSkillInstallStatus(t *testing.T, files []SkillInstallFileResult, path string, status SkillInstallFileStatus) {
	t.Helper()
	clean := filepath.Clean(path)
	for _, file := range files {
		if filepath.Clean(file.Path) == clean {
			if file.Status != status {
				t.Fatalf("%s status = %s, want %s", path, file.Status, status)
			}
			return
		}
	}
	t.Fatalf("%s not found in file results: %#v", path, files)
}

func assertNoMissingRequiredHooks(t *testing.T, data []byte) {
	t.Helper()
	missing, err := claudehooks.MissingHooks(data)
	if err != nil {
		t.Fatalf("MissingHooks returned error: %v\n%s", err, data)
	}
	if len(missing) != 0 {
		t.Fatalf("missing hooks: %s\n%s", claudehooks.FormatMissing(missing), data)
	}
}

func assertHookCommandCounts(t *testing.T, data []byte, wantEach int) {
	t.Helper()
	for _, command := range []string{
		"loopcoder hook conductor-attest",
		"loopcoder hook conductor-relay-guard",
	} {
		if got := strings.Count(string(data), command); got != wantEach {
			t.Fatalf("%s count = %d, want %d\n%s", command, got, wantEach, data)
		}
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("settings are not valid JSON: %v\n%s", err, data)
	}
}

type skillFakeFS struct {
	dirs     map[string]bool
	files    map[string][]byte
	modes    map[string]fs.FileMode
	mkdirErr error
}

func newSkillFakeFS() *skillFakeFS {
	return &skillFakeFS{
		dirs:  map[string]bool{},
		files: map[string][]byte{},
		modes: map[string]fs.FileMode{},
	}
}

func (f *skillFakeFS) Stat(path string) (fs.FileInfo, error) {
	clean := filepath.Clean(path)
	if f.dirs[clean] {
		return skillFakeInfo{name: filepath.Base(clean), dir: true}, nil
	}
	data, ok := f.files[clean]
	if !ok {
		return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
	}
	return skillFakeInfo{name: filepath.Base(clean), size: int64(len(data))}, nil
}

func (f *skillFakeFS) MkdirAll(path string, _ fs.FileMode) error {
	if f.mkdirErr != nil {
		return f.mkdirErr
	}
	f.dirs[filepath.Clean(path)] = true
	return nil
}

func (f *skillFakeFS) ReadFile(path string) ([]byte, error) {
	data, ok := f.files[filepath.Clean(path)]
	if !ok {
		return nil, &fs.PathError{Op: "read", Path: path, Err: fs.ErrNotExist}
	}
	return append([]byte(nil), data...), nil
}

func (f *skillFakeFS) WriteFile(path string, data []byte, mode fs.FileMode) error {
	clean := filepath.Clean(path)
	copied := append([]byte(nil), data...)
	f.files[clean] = copied
	f.modes[clean] = mode
	return nil
}

func (f *skillFakeFS) mustWrite(path string, data []byte) {
	if err := f.WriteFile(path, data, 0o644); err != nil {
		panic(err)
	}
}

func (f *skillFakeFS) read(t *testing.T, path string) []byte {
	t.Helper()
	data, ok := f.files[filepath.Clean(path)]
	if !ok {
		t.Fatalf("file %q not found; files=%#v", path, f.files)
	}
	return append([]byte(nil), data...)
}

type skillFakeInfo struct {
	name string
	size int64
	dir  bool
}

func (f skillFakeInfo) Name() string       { return f.name }
func (f skillFakeInfo) Size() int64        { return f.size }
func (f skillFakeInfo) Mode() fs.FileMode  { return 0o644 }
func (f skillFakeInfo) ModTime() time.Time { return time.Time{} }
func (f skillFakeInfo) IsDir() bool        { return f.dir }
func (f skillFakeInfo) Sys() any           { return nil }
