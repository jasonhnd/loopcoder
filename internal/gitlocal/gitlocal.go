// Package gitlocal protects loopcoder's repo-local machine state from normal
// business-branch commits.
package gitlocal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/gitutil"
)

const (
	ManagedExcludeComment = "# loopcoder local state"
	ManagedExcludeEntry   = ".loopcoder/"
)

var ErrNotGitRepository = errors.New("not a git repository")

type ProtectStatus string

const (
	ProtectCreated   ProtectStatus = "created"
	ProtectUpdated   ProtectStatus = "updated"
	ProtectUnchanged ProtectStatus = "unchanged"
)

type ProtectResult struct {
	ExcludePath string
	Status      ProtectStatus
}

type Deps struct {
	Git       gitutil.Runner
	MkdirAll  func(string, fs.FileMode) error
	ReadFile  func(string) ([]byte, error)
	WriteFile func(string, []byte, fs.FileMode) error
}

func DefaultDeps() Deps {
	return Deps{
		Git:       gitutil.ExecRunner{},
		MkdirAll:  os.MkdirAll,
		ReadFile:  os.ReadFile,
		WriteFile: os.WriteFile,
	}
}

func ProtectLoopcoderState(ctx context.Context, repoPath string) (ProtectResult, error) {
	return ProtectLoopcoderStateWithDeps(ctx, repoPath, DefaultDeps())
}

func ProtectLoopcoderStateWithDeps(ctx context.Context, repoPath string, deps Deps) (ProtectResult, error) {
	deps = normalizeDeps(deps)
	excludePath, err := ResolveExcludePath(ctx, repoPath, deps.Git)
	if err != nil {
		return ProtectResult{}, err
	}

	status := ProtectUpdated
	current, err := deps.ReadFile(excludePath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return ProtectResult{}, fmt.Errorf("read git exclude %s: %w", excludePath, err)
		}
		status = ProtectCreated
	}
	if protectsLoopcoderState(current) {
		return ProtectResult{ExcludePath: excludePath, Status: ProtectUnchanged}, nil
	}

	next := appendManagedExcludeBlock(current)
	if err := deps.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return ProtectResult{}, fmt.Errorf("create git exclude directory %s: %w", filepath.Dir(excludePath), err)
	}
	if err := deps.WriteFile(excludePath, next, 0o644); err != nil {
		return ProtectResult{}, fmt.Errorf("write git exclude %s: %w", excludePath, err)
	}
	return ProtectResult{ExcludePath: excludePath, Status: status}, nil
}

func ResolveExcludePath(ctx context.Context, repoPath string, runner gitutil.Runner) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	repoPath = strings.TrimSpace(repoPath)
	if repoPath == "" {
		repoPath = "."
	}
	if runner == nil {
		runner = gitutil.ExecRunner{}
	}
	inside, err := runner.RunGit(ctx, repoPath, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrNotGitRepository, repoPath)
	}
	if strings.TrimSpace(string(inside)) != "true" {
		return "", fmt.Errorf("%w: %s", ErrNotGitRepository, repoPath)
	}
	out, err := runner.RunGit(ctx, repoPath, "rev-parse", "--path-format=absolute", "--git-path", "info/exclude")
	if err != nil {
		return "", fmt.Errorf("resolve git exclude path for %s: %w", repoPath, err)
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", fmt.Errorf("resolve git exclude path for %s: empty git output", repoPath)
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(repoPath, path)
	}
	return filepath.Clean(path), nil
}

func normalizeDeps(deps Deps) Deps {
	defaults := DefaultDeps()
	if deps.Git == nil {
		deps.Git = defaults.Git
	}
	if deps.MkdirAll == nil {
		deps.MkdirAll = defaults.MkdirAll
	}
	if deps.ReadFile == nil {
		deps.ReadFile = defaults.ReadFile
	}
	if deps.WriteFile == nil {
		deps.WriteFile = defaults.WriteFile
	}
	return deps
}

func protectsLoopcoderState(data []byte) bool {
	for _, rawLine := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		if strings.TrimSpace(rawLine) == ManagedExcludeEntry {
			return true
		}
	}
	return false
}

func ExcludesLoopcoderState(data []byte) bool {
	return protectsLoopcoderState(data)
}

func appendManagedExcludeBlock(current []byte) []byte {
	var next []byte
	next = append(next, current...)
	if len(bytes.TrimSpace(next)) > 0 && !bytes.HasSuffix(next, []byte("\n")) {
		next = append(next, '\n')
	}
	next = append(next, []byte(ManagedExcludeComment+"\n"+ManagedExcludeEntry+"\n")...)
	return next
}
