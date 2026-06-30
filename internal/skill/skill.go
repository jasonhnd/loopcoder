// Package skill installs and inspects the bundled loopcoder skill files.
package skill

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	loopcoder "github.com/jasonhnd/loopcoder"
)

const (
	FilenameSkill  = "SKILL.md"
	FilenameAgents = "AGENTS.md"
)

type InstallOptions struct {
	Dir   string
	Force bool
}

type InstallDeps struct {
	UserHomeDir    func() (string, error)
	Stat           func(string) (fs.FileInfo, error)
	MkdirAll       func(string, fs.FileMode) error
	ReadFile       func(string) ([]byte, error)
	WriteFile      func(string, []byte, fs.FileMode) error
	SkillMarkdown  func() ([]byte, error)
	AgentsMarkdown func() ([]byte, error)
}

type FileStatus string

const (
	FileCreated     FileStatus = "created"
	FileUpdated     FileStatus = "updated"
	FileUnchanged   FileStatus = "unchanged"
	FileOverwritten FileStatus = "overwritten"
)

type FileResult struct {
	Path   string
	Status FileStatus
}

type InstallResult struct {
	Dir   string
	Files []FileResult
}

type InspectStatus string

const (
	InspectCurrent      InspectStatus = "current"
	InspectStale        InspectStatus = "stale"
	InspectNotInstalled InspectStatus = "not-installed"
)

type InspectOptions struct {
	Dir string
}

type InspectResult struct {
	Dir        string
	Status     InspectStatus
	StaleFiles []string
	Missing    []string
}

type managedFile struct {
	name string
	data []byte
}

func DefaultInstallDeps() InstallDeps {
	return InstallDeps{
		UserHomeDir:    os.UserHomeDir,
		Stat:           os.Stat,
		MkdirAll:       os.MkdirAll,
		ReadFile:       os.ReadFile,
		WriteFile:      os.WriteFile,
		SkillMarkdown:  loopcoder.SkillMarkdown,
		AgentsMarkdown: loopcoder.AgentsMarkdown,
	}
}

func Install(ctx context.Context, opts InstallOptions, deps InstallDeps) (InstallResult, error) {
	_ = ctx
	deps = normalizeInstallDeps(deps)

	dir, err := ResolveInstallDir(opts.Dir, deps.UserHomeDir)
	if err != nil {
		return InstallResult{}, err
	}
	result := InstallResult{Dir: dir}

	if err := deps.MkdirAll(dir, 0o755); err != nil {
		return result, fmt.Errorf("create skill directory %s: %w", dir, err)
	}

	files, err := embeddedManagedFiles(deps)
	if err != nil {
		return result, err
	}
	for _, file := range files {
		fileResult, err := writeInstallFile(deps, filepath.Join(dir, file.name), file.data, opts.Force)
		if err != nil {
			return result, err
		}
		result.Files = append(result.Files, fileResult)
	}

	return result, nil
}

func InspectInstalled(opts InspectOptions, deps InstallDeps) (InspectResult, error) {
	deps = normalizeInstallDeps(deps)

	dir, err := ResolveInstallDir(opts.Dir, deps.UserHomeDir)
	if err != nil {
		return InspectResult{}, err
	}
	result := InspectResult{Dir: dir}

	files, err := embeddedManagedFiles(deps)
	if err != nil {
		return result, err
	}
	for _, file := range files {
		path := filepath.Join(dir, file.name)
		info, err := deps.Stat(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
				result.Missing = append(result.Missing, path)
				continue
			}
			return result, fmt.Errorf("stat %s: %w", path, err)
		}
		if info.IsDir() {
			result.StaleFiles = append(result.StaleFiles, path)
			continue
		}
		installed, err := deps.ReadFile(path)
		if err != nil {
			return result, fmt.Errorf("read %s: %w", path, err)
		}
		if !bytes.Equal(installed, file.data) {
			result.StaleFiles = append(result.StaleFiles, path)
		}
	}

	switch {
	case len(result.StaleFiles) > 0:
		result.Status = InspectStale
	case len(result.Missing) > 0:
		result.Status = InspectNotInstalled
	default:
		result.Status = InspectCurrent
	}
	return result, nil
}

func ResolveInstallDir(dir string, userHomeDir func() (string, error)) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir != "" {
		return filepath.Clean(dir), nil
	}
	homeDir, err := userHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve Claude skill directory: %w", err)
	}
	homeDir = strings.TrimSpace(homeDir)
	if homeDir == "" {
		return "", errors.New("resolve Claude skill directory: empty user home")
	}
	return filepath.Join(homeDir, ".claude", "skills", "loopcoder"), nil
}

func WriteInstallFile(deps InstallDeps, path string, data []byte, force bool) (FileResult, error) {
	deps = normalizeInstallDeps(deps)
	return writeInstallFile(deps, path, data, force)
}

func normalizeInstallDeps(deps InstallDeps) InstallDeps {
	defaults := DefaultInstallDeps()
	if deps.UserHomeDir == nil {
		deps.UserHomeDir = defaults.UserHomeDir
	}
	if deps.Stat == nil {
		deps.Stat = defaults.Stat
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
	if deps.SkillMarkdown == nil {
		deps.SkillMarkdown = defaults.SkillMarkdown
	}
	if deps.AgentsMarkdown == nil {
		deps.AgentsMarkdown = defaults.AgentsMarkdown
	}
	return deps
}

func embeddedManagedFiles(deps InstallDeps) ([]managedFile, error) {
	skill, err := readEmbeddedFile(FilenameSkill, deps.SkillMarkdown)
	if err != nil {
		return nil, err
	}
	agents, err := readEmbeddedFile(FilenameAgents, deps.AgentsMarkdown)
	if err != nil {
		return nil, err
	}
	return []managedFile{
		{name: FilenameSkill, data: skill},
		{name: FilenameAgents, data: agents},
	}, nil
}

func readEmbeddedFile(name string, read func() ([]byte, error)) ([]byte, error) {
	data, err := read()
	if err != nil {
		return nil, fmt.Errorf("read embedded %s: %w", name, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("embedded %s is empty", name)
	}
	return data, nil
}

func writeInstallFile(deps InstallDeps, path string, data []byte, force bool) (FileResult, error) {
	info, err := deps.Stat(path)
	if err == nil {
		if info.IsDir() {
			return FileResult{}, fmt.Errorf("%s is a directory", path)
		}
		if force {
			if err := deps.WriteFile(path, data, 0o644); err != nil {
				return FileResult{}, fmt.Errorf("write %s: %w", path, err)
			}
			return FileResult{Path: path, Status: FileOverwritten}, nil
		}
		installed, err := deps.ReadFile(path)
		if err != nil {
			return FileResult{}, fmt.Errorf("read %s: %w", path, err)
		}
		if bytes.Equal(installed, data) {
			return FileResult{Path: path, Status: FileUnchanged}, nil
		}
		if err := deps.WriteFile(path, data, 0o644); err != nil {
			return FileResult{}, fmt.Errorf("write %s: %w", path, err)
		}
		return FileResult{Path: path, Status: FileUpdated}, nil
	}
	if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, os.ErrNotExist) {
		return FileResult{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if err := deps.WriteFile(path, data, 0o644); err != nil {
		return FileResult{}, fmt.Errorf("write %s: %w", path, err)
	}
	return FileResult{Path: path, Status: FileCreated}, nil
}
