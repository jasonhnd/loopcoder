package home

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jasonhnd/loopcoder/internal/authoritystore"
)

const (
	machineDBName   = "machine.db"
	projectDBName   = "project.db"
	runsDirName     = "runs"
	recoveryDirName = "recovery"
	ownerOnlyDir    = 0o700
)

// ErrInvalidProjectID is returned for malformed or escaping project IDs.
var ErrInvalidProjectID = errors.New("invalid project id")

// ErrUnsafePath is returned for symlink escapes, bad modes, or non-dir components.
var ErrUnsafePath = errors.New("unsafe loopcoder home path")

// ErrRepoRuntimeState is returned when a customer repo contains LoopCoder runtime files.
var ErrRepoRuntimeState = errors.New("repository contains loopcoder runtime state")

// V09Layout is the validated global home layout for v0.9 authority stores.
type V09Layout struct {
	HomeDir string
}

// ResolveV09 returns a layout rooted at LOOPCODER_HOME or ~/.loopcoder.
// It validates the home path but does not create directories.
func ResolveV09(deps Deps) (V09Layout, error) {
	homeDir, err := ResolveHomeDir(deps)
	if err != nil {
		return V09Layout{}, err
	}
	return NewV09(homeDir)
}

// NewV09 validates homeDir and returns a typed v0.9 layout (no creation).
func NewV09(homeDir string) (V09Layout, error) {
	homeDir = filepath.Clean(strings.TrimSpace(homeDir))
	if homeDir == "" || homeDir == "." {
		return V09Layout{}, fmt.Errorf("%w: empty home dir", ErrUnsafePath)
	}
	if !filepath.IsAbs(homeDir) {
		return V09Layout{}, fmt.Errorf("%w: home dir must be absolute", ErrUnsafePath)
	}
	return V09Layout{HomeDir: homeDir}, nil
}

// DataDir is $HOME/data.
func (l V09Layout) DataDir() string { return filepath.Join(l.HomeDir, dataDirName) }

// MachineDBPath is $HOME/data/machine.db.
func (l V09Layout) MachineDBPath() string { return filepath.Join(l.DataDir(), machineDBName) }

// ProjectsDir is $HOME/projects.
func (l V09Layout) ProjectsDir() string { return filepath.Join(l.HomeDir, projectsDirName) }

// ProjectDir is $HOME/projects/<id>.
func (l V09Layout) ProjectDir(projectID string) (string, error) {
	id, err := ValidateProjectID(projectID)
	if err != nil {
		return "", err
	}
	return filepath.Join(l.ProjectsDir(), id), nil
}

// ProjectDBPath is $HOME/projects/<id>/project.db.
func (l V09Layout) ProjectDBPath(projectID string) (string, error) {
	dir, err := l.ProjectDir(projectID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, projectDBName), nil
}

// ProjectRunsDir is $HOME/projects/<id>/runs.
func (l V09Layout) ProjectRunsDir(projectID string) (string, error) {
	dir, err := l.ProjectDir(projectID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, runsDirName), nil
}

// ProjectLogsDir is $HOME/projects/<id>/logs.
func (l V09Layout) ProjectLogsDir(projectID string) (string, error) {
	dir, err := l.ProjectDir(projectID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, logsDirName), nil
}

// ProjectTmpDir is $HOME/projects/<id>/tmp.
func (l V09Layout) ProjectTmpDir(projectID string) (string, error) {
	dir, err := l.ProjectDir(projectID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, tmpDirName), nil
}

// ProjectRecoveryDir is $HOME/projects/<id>/recovery.
func (l V09Layout) ProjectRecoveryDir(projectID string) (string, error) {
	dir, err := l.ProjectDir(projectID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, recoveryDirName), nil
}

// ValidateProjectID rejects empty, absolute, traversal, and separator IDs.
func ValidateProjectID(projectID string) (string, error) {
	id := strings.TrimSpace(projectID)
	if id == "" {
		return "", fmt.Errorf("%w: empty", ErrInvalidProjectID)
	}
	if id == "." || id == ".." {
		return "", fmt.Errorf("%w: %q", ErrInvalidProjectID, id)
	}
	if filepath.IsAbs(id) {
		return "", fmt.Errorf("%w: absolute path", ErrInvalidProjectID)
	}
	if strings.ContainsAny(id, `/\`) {
		return "", fmt.Errorf("%w: path separators not allowed", ErrInvalidProjectID)
	}
	if strings.Contains(id, "..") {
		return "", fmt.Errorf("%w: traversal", ErrInvalidProjectID)
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return "", fmt.Errorf("%w: illegal character in %q", ErrInvalidProjectID, id)
	}
	return id, nil
}

// EnsureBase creates the minimum global directories under the layout, owner-only.
func (l V09Layout) EnsureBase() error {
	for _, dir := range []string{l.HomeDir, l.DataDir(), l.ProjectsDir()} {
		if _, err := ensureOwnerOnlyDir(dir); err != nil {
			return err
		}
	}
	return nil
}

// EnsureProject creates the project payload directories (not the database file).
func (l V09Layout) EnsureProject(projectID string) error {
	if err := l.EnsureBase(); err != nil {
		return err
	}
	root, err := l.ProjectDir(projectID)
	if err != nil {
		return err
	}
	runs, err := l.ProjectRunsDir(projectID)
	if err != nil {
		return err
	}
	logs, err := l.ProjectLogsDir(projectID)
	if err != nil {
		return err
	}
	tmp, err := l.ProjectTmpDir(projectID)
	if err != nil {
		return err
	}
	recovery, err := l.ProjectRecoveryDir(projectID)
	if err != nil {
		return err
	}
	for _, dir := range []string{root, runs, logs, tmp, recovery} {
		if _, err := ensureOwnerOnlyDir(dir); err != nil {
			return err
		}
	}
	return nil
}

// EnsureMinimumLayout is a first-run helper: base dirs and optional project tree.
func EnsureMinimumLayout(homeDir, projectID string) (V09Layout, error) {
	layout, err := NewV09(homeDir)
	if err != nil {
		return V09Layout{}, err
	}
	if err := layout.EnsureBase(); err != nil {
		return V09Layout{}, err
	}
	if projectID != "" {
		if err := layout.EnsureProject(projectID); err != nil {
			return V09Layout{}, err
		}
	}
	return layout, nil
}

// OpenMachine opens machine.db through authoritystore after ensuring base layout.
func (l V09Layout) OpenMachine(ctx context.Context, now func() time.Time) (*authoritystore.MachineStore, error) {
	if err := l.EnsureBase(); err != nil {
		return nil, err
	}
	return authoritystore.OpenMachine(ctx, authoritystore.OpenOptions{
		Path: l.MachineDBPath(),
		Now:  now,
	})
}

// OpenProject opens project.db through authoritystore after ensuring project layout.
func (l V09Layout) OpenProject(ctx context.Context, projectID string, now func() time.Time) (*authoritystore.ProjectStore, error) {
	if err := l.EnsureProject(projectID); err != nil {
		return nil, err
	}
	path, err := l.ProjectDBPath(projectID)
	if err != nil {
		return nil, err
	}
	return authoritystore.OpenProject(ctx, authoritystore.OpenOptions{
		Path: path,
		Now:  now,
	})
}

// ContainsPath reports whether candidate is the home or a path beneath it.
func (l V09Layout) ContainsPath(candidate string) bool {
	candidate = filepath.Clean(candidate)
	home := l.HomeDir
	if candidate == home {
		return true
	}
	prefix := home + string(os.PathSeparator)
	return strings.HasPrefix(candidate, prefix)
}

// AssertNotUnderRepo fails if any layout path resolves under repoRoot.
func (l V09Layout) AssertNotUnderRepo(repoRoot string) error {
	repoRoot = filepath.Clean(repoRoot)
	if repoRoot == "" {
		return nil
	}
	check := []string{l.HomeDir, l.DataDir(), l.ProjectsDir(), l.MachineDBPath()}
	for _, p := range check {
		if p == repoRoot || strings.HasPrefix(p, repoRoot+string(os.PathSeparator)) {
			return fmt.Errorf("%w: layout path resolves under customer repo", ErrUnsafePath)
		}
	}
	return nil
}

func ensureOwnerOnlyDir(path string) (created bool, err error) {
	path = filepath.Clean(path)
	info, err := os.Lstat(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
		if err := os.MkdirAll(path, ownerOnlyDir); err != nil {
			return false, fmt.Errorf("%w: mkdir %s: %v", ErrUnsafePath, filepath.Base(path), err)
		}
		if err := os.Chmod(path, ownerOnlyDir); err != nil {
			return true, fmt.Errorf("%w: chmod: %v", ErrUnsafePath, err)
		}
		return true, validateOwnerOnlyDir(path)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("%w: symlink at layout component", ErrUnsafePath)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("%w: path component is not a directory", ErrUnsafePath)
	}
	return false, validateOwnerOnlyDir(path)
}

func validateOwnerOnlyDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: symlink", ErrUnsafePath)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: not a directory", ErrUnsafePath)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%w: directory mode %04o is not owner-only", ErrUnsafePath, perm)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		if int(stat.Uid) != os.Getuid() {
			return fmt.Errorf("%w: directory owner is not current user", ErrUnsafePath)
		}
	}
	return nil
}

// ScanRepoForRuntimeState fails if repoRoot contains LoopCoder runtime artifacts.
func ScanRepoForRuntimeState(repoRoot string) error {
	repoRoot = filepath.Clean(strings.TrimSpace(repoRoot))
	if repoRoot == "" {
		return fmt.Errorf("scan repo: empty root")
	}
	forbiddenNames := map[string]struct{}{
		".loopcoder": {},
		"machine.db": {},
		"project.db": {},
	}
	return filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(repoRoot, path)
		if relErr != nil {
			return relErr
		}
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(os.PathSeparator)) {
			if d.IsDir() && rel == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if _, ok := forbiddenNames[name]; ok {
			return fmt.Errorf("%w: found %s", ErrRepoRuntimeState, name)
		}
		if strings.HasPrefix(name, ".loopcoder") {
			return fmt.Errorf("%w: found %s", ErrRepoRuntimeState, name)
		}
		return nil
	})
}
