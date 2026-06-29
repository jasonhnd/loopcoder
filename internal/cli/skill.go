package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	loopcoder "github.com/jasonhnd/loopcoder"
)

const (
	skillFilename  = "SKILL.md"
	agentsFilename = "AGENTS.md"
)

type SkillInstallOptions struct {
	Dir   string
	Force bool
}

type SkillInstallDeps struct {
	UserHomeDir    func() (string, error)
	Stat           func(string) (fs.FileInfo, error)
	MkdirAll       func(string, fs.FileMode) error
	WriteFile      func(string, []byte, fs.FileMode) error
	SkillMarkdown  func() ([]byte, error)
	AgentsMarkdown func() ([]byte, error)
}

type SkillInstallFileStatus string

const (
	SkillInstallFileCreated     SkillInstallFileStatus = "created"
	SkillInstallFileExists      SkillInstallFileStatus = "exists"
	SkillInstallFileOverwritten SkillInstallFileStatus = "overwritten"
)

type SkillInstallFileResult struct {
	Path   string
	Status SkillInstallFileStatus
}

type SkillInstallResult struct {
	Dir   string
	Files []SkillInstallFileResult
}

func DefaultSkillInstallDeps() SkillInstallDeps {
	return SkillInstallDeps{
		UserHomeDir:    os.UserHomeDir,
		Stat:           os.Stat,
		MkdirAll:       os.MkdirAll,
		WriteFile:      os.WriteFile,
		SkillMarkdown:  loopcoder.SkillMarkdown,
		AgentsMarkdown: loopcoder.AgentsMarkdown,
	}
}

func InstallSkill(ctx context.Context, opts SkillInstallOptions, deps SkillInstallDeps) (SkillInstallResult, error) {
	_ = ctx
	deps = normalizeSkillInstallDeps(deps)

	dir, err := resolveSkillInstallDir(opts.Dir, deps.UserHomeDir)
	if err != nil {
		return SkillInstallResult{}, err
	}
	result := SkillInstallResult{Dir: dir}

	if err := deps.MkdirAll(dir, 0o755); err != nil {
		return result, fmt.Errorf("create skill directory %s: %w", dir, err)
	}

	skill, err := readEmbeddedSkillFile(skillFilename, deps.SkillMarkdown)
	if err != nil {
		return result, err
	}
	agents, err := readEmbeddedSkillFile(agentsFilename, deps.AgentsMarkdown)
	if err != nil {
		return result, err
	}

	for _, file := range []struct {
		name string
		data []byte
	}{
		{name: skillFilename, data: skill},
		{name: agentsFilename, data: agents},
	} {
		fileResult, err := writeSkillInstallFile(deps, filepath.Join(dir, file.name), file.data, opts.Force)
		if err != nil {
			return result, err
		}
		result.Files = append(result.Files, fileResult)
	}

	return result, nil
}

func normalizeSkillInstallDeps(deps SkillInstallDeps) SkillInstallDeps {
	defaults := DefaultSkillInstallDeps()
	if deps.UserHomeDir == nil {
		deps.UserHomeDir = defaults.UserHomeDir
	}
	if deps.Stat == nil {
		deps.Stat = defaults.Stat
	}
	if deps.MkdirAll == nil {
		deps.MkdirAll = defaults.MkdirAll
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

func resolveSkillInstallDir(dir string, userHomeDir func() (string, error)) (string, error) {
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

func readEmbeddedSkillFile(name string, read func() ([]byte, error)) ([]byte, error) {
	data, err := read()
	if err != nil {
		return nil, fmt.Errorf("read embedded %s: %w", name, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("embedded %s is empty", name)
	}
	return data, nil
}

func writeSkillInstallFile(deps SkillInstallDeps, path string, data []byte, force bool) (SkillInstallFileResult, error) {
	info, err := deps.Stat(path)
	if err == nil {
		if info.IsDir() {
			return SkillInstallFileResult{}, fmt.Errorf("%s is a directory", path)
		}
		if !force {
			return SkillInstallFileResult{Path: path, Status: SkillInstallFileExists}, nil
		}
		if err := deps.WriteFile(path, data, 0o644); err != nil {
			return SkillInstallFileResult{}, fmt.Errorf("write %s: %w", path, err)
		}
		return SkillInstallFileResult{Path: path, Status: SkillInstallFileOverwritten}, nil
	}
	if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, os.ErrNotExist) {
		return SkillInstallFileResult{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if err := deps.WriteFile(path, data, 0o644); err != nil {
		return SkillInstallFileResult{}, fmt.Errorf("write %s: %w", path, err)
	}
	return SkillInstallFileResult{Path: path, Status: SkillInstallFileCreated}, nil
}

func runSkill(args []string, stdout, stderr io.Writer, deps Deps) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "skill: expected subcommand")
		printSkillHelp(stderr)
		return 2
	}
	if isHelp(args[0]) {
		printSkillHelp(stdout)
		return 0
	}
	if args[0] != "install" {
		fmt.Fprintf(stderr, "skill: unknown subcommand %q\n", args[0])
		printSkillHelp(stderr)
		return 2
	}
	return runSkillInstall(args[1:], stdout, stderr, deps)
}

func runSkillInstall(args []string, stdout, stderr io.Writer, deps Deps) int {
	if deps.SkillInstall == nil {
		deps.SkillInstall = DefaultDeps().SkillInstall
	}

	fs := flag.NewFlagSet("skill install", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var opts SkillInstallOptions
	var dirAlias string
	var forceAlias bool
	fs.StringVar(&opts.Dir, "dir", "", "Claude Code loopcoder skill directory")
	fs.StringVar(&dirAlias, "Dir", "", "Claude Code loopcoder skill directory")
	fs.BoolVar(&opts.Force, "force", false, "overwrite existing skill files")
	fs.BoolVar(&forceAlias, "Force", false, "overwrite existing skill files")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if dirAlias != "" {
		opts.Dir = dirAlias
	}
	if forceAlias {
		opts.Force = true
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "skill install: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	result, err := deps.SkillInstall(context.Background(), opts)
	if err != nil {
		fmt.Fprintf(stderr, "skill install: %v\n", err)
		return 1
	}
	renderSkillInstallResult(stdout, result)
	return 0
}

func printSkillHelp(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  loopcoder skill install [flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Install the bundled loopcoder playbook skill files.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	fmt.Fprintln(w, "  --dir string    Claude Code loopcoder skill directory (default \"~/.claude/skills/loopcoder\")")
	fmt.Fprintln(w, "  --force         overwrite existing skill files")
	fmt.Fprintln(w, "  --help          show command help")
}

func renderSkillInstallResult(w io.Writer, result SkillInstallResult) {
	fmt.Fprintln(w, "loopcoder skill install complete")
	fmt.Fprintf(w, "  directory %s\n", result.Dir)
	for _, file := range result.Files {
		switch file.Status {
		case SkillInstallFileCreated:
			fmt.Fprintf(w, "  created %s\n", file.Path)
		case SkillInstallFileOverwritten:
			fmt.Fprintf(w, "  overwritten %s\n", file.Path)
		case SkillInstallFileExists:
			fmt.Fprintf(w, "  exists %s\n", file.Path)
		default:
			fmt.Fprintf(w, "  %s %s\n", file.Status, file.Path)
		}
	}
}
