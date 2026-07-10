package home

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

const (
	// EnvHome overrides the default loopcoder home directory.
	EnvHome = "LOOPCODER_HOME"

	homeDirName     = ".loopcoder"
	binDirName      = "bin"
	dataDirName     = "data"
	databaseName    = "loopcoder.db"
	projectsDirName = "projects"
	logsDirName     = "logs"
	tmpDirName      = "tmp"
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

// DataDir returns the directory containing machine-local runtime data.
func (l Layout) DataDir() string {
	return filepath.Join(l.HomeDir, dataDirName)
}

// DatabasePath returns the default SQLite database path.
func (l Layout) DatabasePath() string {
	return filepath.Join(l.DataDir(), databaseName)
}

// ProjectsDir returns the directory containing per-project runtime payloads.
func (l Layout) ProjectsDir() string {
	return filepath.Join(l.HomeDir, projectsDirName)
}

// ProjectDir returns the machine-local payload root for a registered project.
func (l Layout) ProjectDir(projectID string) string {
	return filepath.Join(l.ProjectsDir(), filepath.Clean(projectID))
}

// LogsDir returns the machine-local runtime log directory.
func (l Layout) LogsDir() string {
	return filepath.Join(l.HomeDir, logsDirName)
}

// TmpDir returns the machine-local runtime temporary directory.
func (l Layout) TmpDir() string {
	return filepath.Join(l.HomeDir, tmpDirName)
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

// InstalledVersions lists installed version directories in semver-aware order.
//
// Directory names that are clean path segments but not MAJOR.MINOR.PATCH
// versions, with an optional leading v, sort lexically before valid semver
// entries. That keeps ordering deterministic without letting malformed entries
// become the latest version by taking the final position in the returned list.
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
	sort.Slice(versions, func(i, j int) bool {
		return compareInstalledVersion(versions[i], versions[j]) < 0
	})
	return versions, nil
}

type installedSemver struct {
	major int
	minor int
	patch int
}

func compareInstalledVersion(a, b string) int {
	av, aOK := parseInstalledSemver(a)
	bv, bOK := parseInstalledSemver(b)
	switch {
	case aOK && bOK:
		if cmp := compareInt(av.major, bv.major); cmp != 0 {
			return cmp
		}
		if cmp := compareInt(av.minor, bv.minor); cmp != 0 {
			return cmp
		}
		if cmp := compareInt(av.patch, bv.patch); cmp != 0 {
			return cmp
		}
		return strings.Compare(a, b)
	case aOK:
		return 1
	case bOK:
		return -1
	default:
		return strings.Compare(a, b)
	}
}

func parseInstalledSemver(version string) (installedSemver, bool) {
	version = strings.TrimPrefix(version, "v")

	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return installedSemver{}, false
	}

	major, ok := parseSemverNumber(parts[0])
	if !ok {
		return installedSemver{}, false
	}
	minor, ok := parseSemverNumber(parts[1])
	if !ok {
		return installedSemver{}, false
	}
	patch, ok := parseSemverNumber(parts[2])
	if !ok {
		return installedSemver{}, false
	}
	return installedSemver{major: major, minor: minor, patch: patch}, true
}

func parseSemverNumber(part string) (int, bool) {
	if part == "" || (len(part) > 1 && part[0] == '0') {
		return 0, false
	}
	for _, r := range part {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	value, err := strconv.Atoi(part)
	if err != nil {
		return 0, false
	}
	return value, true
}

func compareInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
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
