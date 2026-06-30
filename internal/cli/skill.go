package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/jasonhnd/loopcoder/internal/skill"
)

const (
	skillFilename  = skill.FilenameSkill
	agentsFilename = skill.FilenameAgents
)

type SkillInstallOptions = skill.InstallOptions
type SkillInstallDeps = skill.InstallDeps
type SkillInstallFileStatus = skill.FileStatus

const (
	SkillInstallFileCreated     SkillInstallFileStatus = skill.FileCreated
	SkillInstallFileUpdated     SkillInstallFileStatus = skill.FileUpdated
	SkillInstallFileUnchanged   SkillInstallFileStatus = skill.FileUnchanged
	SkillInstallFileExists      SkillInstallFileStatus = "exists"
	SkillInstallFileOverwritten SkillInstallFileStatus = skill.FileOverwritten
)

type SkillInstallFileResult = skill.FileResult
type SkillInstallResult = skill.InstallResult

func DefaultSkillInstallDeps() SkillInstallDeps {
	return skill.DefaultInstallDeps()
}

func InstallSkill(ctx context.Context, opts SkillInstallOptions, deps SkillInstallDeps) (SkillInstallResult, error) {
	return skill.Install(ctx, opts, deps)
}

func writeSkillInstallFile(deps SkillInstallDeps, path string, data []byte, force bool) (SkillInstallFileResult, error) {
	return skill.WriteInstallFile(deps, path, data, force)
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
		renderSkillInstallFileResult(w, file)
	}
}

func renderSkillInstallFileResult(w io.Writer, file SkillInstallFileResult) {
	switch file.Status {
	case SkillInstallFileCreated:
		fmt.Fprintf(w, "  created %s\n", file.Path)
	case SkillInstallFileUpdated:
		fmt.Fprintf(w, "  updated %s\n", file.Path)
	case SkillInstallFileUnchanged:
		fmt.Fprintf(w, "  unchanged %s\n", file.Path)
	case SkillInstallFileOverwritten:
		fmt.Fprintf(w, "  overwritten %s\n", file.Path)
	case SkillInstallFileExists:
		fmt.Fprintf(w, "  exists %s\n", file.Path)
	default:
		fmt.Fprintf(w, "  %s %s\n", file.Status, file.Path)
	}
}
