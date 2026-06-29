package home

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

const (
	// EnvHome overrides the default loopcoder home directory.
	EnvHome = "LOOPCODER_HOME"

	homeDirName     = ".loopcoder"
	binDirName      = "bin"
	versionsDirName = "versions"
	skillsDirName   = "skills"
	playbookName    = "SKILL.md"
)

// Deps contains the filesystem and environment operations used by this package.
type Deps struct {
	Getenv      func(string) string
	UserHomeDir func() (string, error)
	ReadDir     func(string) ([]fs.DirEntry, error)
}

// Layout describes the paths inside a resolved loopcoder home directory.
type Layout struct {
	HomeDir string
}

// DefaultDeps returns the real process dependencies.
func DefaultDeps() Deps {
	return Deps{
		Getenv:      os.Getenv,
		UserHomeDir: os.UserHomeDir,
		ReadDir: func(path string) ([]fs.DirEntry, error) {
			return os.ReadDir(path)
		},
	}
}

// Resolve returns a Layout rooted at LOOPCODER_HOME or ~/.loopcoder.
func Resolve(deps Deps) (Layout, error) {
	homeDir, err := ResolveHomeDir(deps)
	if err != nil {
		return Layout{}, err
	}
	return New(homeDir), nil
}

// ResolveHomeDir returns LOOPCODER_HOME when set, otherwise ~/.loopcoder.
func ResolveHomeDir(deps Deps) (string, error) {
	deps = normalizeDeps(deps)
	if override := strings.TrimSpace(deps.Getenv(EnvHome)); override != "" {
		return filepath.Clean(override), nil
	}

	userHome, err := deps.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	userHome = strings.TrimSpace(userHome)
	if userHome == "" {
		return "", errors.New("resolve user home: empty path")
	}
	return filepath.Join(userHome, homeDirName), nil
}

// New returns a Layout rooted at homeDir.
func New(homeDir string) Layout {
	return Layout{HomeDir: filepath.Clean(homeDir)}
}

// BinDir returns the directory containing the stable selected binary.
func (l Layout) BinDir() string {
	return filepath.Join(l.HomeDir, binDirName)
}

// VersionsDir returns the directory containing versioned binary installs.
func (l Layout) VersionsDir() string {
	return filepath.Join(l.HomeDir, versionsDirName)
}

// SkillsDir returns the directory containing the stable playbook copy.
func (l Layout) SkillsDir() string {
	return filepath.Join(l.HomeDir, skillsDirName)
}

// StablePlaybookPath returns the main stable playbook copy path.
func (l Layout) StablePlaybookPath() string {
	return filepath.Join(l.SkillsDir(), playbookName)
}

// StableBinaryPath returns the home-store stable selected binary path.
//
// Final binary selection remains LOOPCODER_BIN > PATH > home store and will be
// wired by later CLI work. This package only provides the home-store path.
func (l Layout) StableBinaryPath() string {
	return filepath.Join(l.BinDir(), binaryFileName())
}

// VersionDir returns the directory for a single installed version.
func (l Layout) VersionDir(version string) (string, error) {
	version, err := cleanVersion(version)
	if err != nil {
		return "", err
	}
	return filepath.Join(l.VersionsDir(), version), nil
}

// VersionBinaryPath returns the home-store binary path for version.
func (l Layout) VersionBinaryPath(version string) (string, error) {
	versionDir, err := l.VersionDir(version)
	if err != nil {
		return "", err
	}
	return filepath.Join(versionDir, binaryFileName()), nil
}

// InstalledVersions lists installed version directories in sorted order.
func (l Layout) InstalledVersions(deps Deps) ([]string, error) {
	deps = normalizeDeps(deps)
	entries, err := deps.ReadDir(l.VersionsDir())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read installed versions: %w", err)
	}

	versions := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := strings.TrimSpace(entry.Name())
		if _, err := cleanVersion(name); err != nil {
			continue
		}
		versions = append(versions, name)
	}
	sort.Strings(versions)
	return versions, nil
}

func normalizeDeps(deps Deps) Deps {
	defaults := DefaultDeps()
	if deps.Getenv == nil {
		deps.Getenv = defaults.Getenv
	}
	if deps.UserHomeDir == nil {
		deps.UserHomeDir = defaults.UserHomeDir
	}
	if deps.ReadDir == nil {
		deps.ReadDir = defaults.ReadDir
	}
	return deps
}

func cleanVersion(version string) (string, error) {
	version = strings.TrimSpace(version)
	if version == "" {
		return "", errors.New("version is required")
	}
	if version == "." || version == ".." ||
		strings.Contains(version, "/") ||
		strings.Contains(version, `\`) {
		return "", fmt.Errorf("invalid version path segment %q", version)
	}
	return version, nil
}

func binaryFileName() string {
	if runtime.GOOS == "windows" {
		return "loopcoder.exe"
	}
	return "loopcoder"
}
