package doctor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	loopcoder "github.com/jasonhnd/loopcoder"
	"github.com/jasonhnd/loopcoder/internal/config"
	"gopkg.in/yaml.v3"
)

type Status string

const (
	StatusInfo Status = "info"
	StatusOK   Status = "ok"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
)

type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

type Options struct {
	RepoPath  string
	BuildInfo BuildInfo
}

type Deps struct {
	LookPath       func(file string) (string, error)
	RunCommand     func(ctx context.Context, dir string, name string, args ...string) (CommandResult, error)
	LoadConfig     func(path string) (config.Config, error)
	ReadFile       func(path string) ([]byte, error)
	ExecutablePath func() (string, error)
	UserHomeDir    func() (string, error)
	SkillMarkdown  func() ([]byte, error)
	AgentsMarkdown func() ([]byte, error)
}

type CommandResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

type Check struct {
	Name    string
	Status  Status
	Message string
	Hard    bool
}

type Report struct {
	Checks []Check
}

func (r Report) ExitCode() int {
	for _, check := range r.Checks {
		if check.Hard && check.Status == StatusFail {
			return 1
		}
	}
	return 0
}

func (r Report) Find(name string) (Check, bool) {
	for _, check := range r.Checks {
		if check.Name == name {
			return check, true
		}
	}
	return Check{}, false
}

func Render(w io.Writer, report Report) error {
	for _, check := range report.Checks {
		if _, err := fmt.Fprintf(w, "[%s] %s: %s\n", check.Status, check.Name, check.Message); err != nil {
			return err
		}
	}
	return nil
}

func DefaultDeps() Deps {
	return Deps{
		LookPath:       exec.LookPath,
		RunCommand:     execRunCommand,
		LoadConfig:     config.Load,
		ReadFile:       os.ReadFile,
		ExecutablePath: os.Executable,
		UserHomeDir:    os.UserHomeDir,
		SkillMarkdown:  loopcoder.SkillMarkdown,
		AgentsMarkdown: loopcoder.AgentsMarkdown,
	}
}

func Run(ctx context.Context, opts Options, deps Deps) Report {
	if ctx == nil {
		ctx = context.Background()
	}
	deps = normalizeDeps(deps)
	repoPath := strings.TrimSpace(opts.RepoPath)
	if repoPath == "" {
		repoPath = "."
	}
	build := normalizeBuildInfo(opts.BuildInfo)

	delivery := loadDelivery(repoPath, deps)
	checks := make([]Check, 0, 10)

	gitCheck, gitPresent := checkGit(deps)
	checks = append(checks, gitCheck)

	ghCheck, ghPresent := checkGH(ctx, deps)
	checks = append(checks, ghCheck)
	_ = ghPresent

	checks = append(checks, checkDeliveryConfig(delivery))
	checks = append(checks, checkProviders(deps, configuredProviders(delivery.Config))...)

	originCheck, originPresent := checkOrigin(ctx, deps, repoPath, gitPresent)
	checks = append(checks, originCheck)
	checks = append(checks, checkDefaultBranch(ctx, deps, repoPath, gitPresent, originPresent))

	checks = append(checks, checkBinary(build, deps))
	checks = append(checks, checkCompatibility(delivery, build))
	checks = append(checks, checkInstalledSkill(deps))
	checks = append(checks, Check{
		Name:    "conductor runtime",
		Status:  StatusOK,
		Message: "user-provided by the active Claude Code or Codex host; loopcoder does not ship it",
	})

	return Report{Checks: checks}
}

func normalizeDeps(deps Deps) Deps {
	defaults := DefaultDeps()
	if deps.LookPath == nil {
		deps.LookPath = defaults.LookPath
	}
	if deps.RunCommand == nil {
		deps.RunCommand = defaults.RunCommand
	}
	if deps.LoadConfig == nil {
		deps.LoadConfig = defaults.LoadConfig
	}
	if deps.ReadFile == nil {
		deps.ReadFile = defaults.ReadFile
	}
	if deps.ExecutablePath == nil {
		deps.ExecutablePath = defaults.ExecutablePath
	}
	if deps.UserHomeDir == nil {
		deps.UserHomeDir = defaults.UserHomeDir
	}
	if deps.SkillMarkdown == nil {
		deps.SkillMarkdown = defaults.SkillMarkdown
	}
	if deps.AgentsMarkdown == nil {
		deps.AgentsMarkdown = defaults.AgentsMarkdown
	}
	return deps
}

func execRunCommand(ctx context.Context, dir string, name string, args ...string) (CommandResult, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if strings.TrimSpace(dir) != "" {
		cmd.Dir = dir
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	result := CommandResult{}
	err := cmd.Run()
	result.Stdout = stdout.String()
	result.Stderr = stderr.String()
	if err == nil {
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	return result, err
}

func checkGit(deps Deps) (Check, bool) {
	path, err := deps.LookPath("git")
	if err != nil || strings.TrimSpace(path) == "" {
		return Check{
			Name:    "git",
			Status:  StatusFail,
			Message: "git was not found on PATH",
			Hard:    true,
		}, false
	}
	return Check{
		Name:    "git",
		Status:  StatusOK,
		Message: fmt.Sprintf("found at %s", path),
	}, true
}

func checkGH(ctx context.Context, deps Deps) (Check, bool) {
	path, err := deps.LookPath("gh")
	if err != nil || strings.TrimSpace(path) == "" {
		return Check{
			Name:    "gh",
			Status:  StatusFail,
			Message: "GitHub CLI gh was not found on PATH",
			Hard:    true,
		}, false
	}
	result, runErr := deps.RunCommand(ctx, "", "gh", "auth", "status")
	if runErr != nil {
		return Check{
			Name:    "gh",
			Status:  StatusFail,
			Message: fmt.Sprintf("found at %s but gh auth status could not run: %v", path, runErr),
			Hard:    true,
		}, true
	}
	if result.ExitCode != 0 {
		detail := commandDetail(result)
		if detail == "" {
			detail = fmt.Sprintf("exit code %d", result.ExitCode)
		}
		return Check{
			Name:    "gh",
			Status:  StatusFail,
			Message: fmt.Sprintf("found at %s but gh auth status failed: %s", path, detail),
			Hard:    true,
		}, true
	}
	return Check{
		Name:    "gh",
		Status:  StatusOK,
		Message: fmt.Sprintf("found at %s and authenticated", path),
	}, true
}

type deliveryState struct {
	Path    string
	Present bool
	Valid   bool
	Config  config.Config
	Meta    deliveryMetadata
	Err     error
}

type deliveryMetadata struct {
	Version             *int   `yaml:"version"`
	MinLoopcoderVersion string `yaml:"min_loopcoder_version"`
}

func loadDelivery(repoPath string, deps Deps) deliveryState {
	path := filepath.Join(repoPath, ".delivery.yml")
	cfg, err := deps.LoadConfig(path)
	if err != nil {
		return deliveryState{
			Path:    path,
			Present: !errors.Is(err, os.ErrNotExist),
			Valid:   false,
			Config:  config.Default(),
			Err:     err,
		}
	}
	data, err := deps.ReadFile(path)
	if err != nil {
		return deliveryState{
			Path:    path,
			Present: true,
			Valid:   false,
			Config:  cfg,
			Err:     fmt.Errorf("read delivery metadata: %w", err),
		}
	}
	var meta deliveryMetadata
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return deliveryState{
			Path:    path,
			Present: true,
			Valid:   false,
			Config:  cfg,
			Err:     fmt.Errorf("parse delivery metadata: %w", err),
		}
	}
	return deliveryState{
		Path:    path,
		Present: true,
		Valid:   true,
		Config:  cfg,
		Meta:    meta,
	}
}

func checkDeliveryConfig(delivery deliveryState) Check {
	if !delivery.Present {
		return Check{
			Name:    ".delivery.yml",
			Status:  StatusWarn,
			Message: "absent; documented defaults apply",
		}
	}
	if !delivery.Valid {
		return Check{
			Name:    ".delivery.yml",
			Status:  StatusFail,
			Message: fmt.Sprintf("present but invalid: %v", delivery.Err),
		}
	}
	return Check{
		Name:    ".delivery.yml",
		Status:  StatusOK,
		Message: "present and valid",
	}
}

type providerSpec struct {
	Name  string
	Roles []string
}

func configuredProviders(cfg config.Config) []providerSpec {
	worker := strings.TrimSpace(cfg.Adapters.Worker)
	if worker == "" {
		worker = "codex"
	}
	verifier := strings.TrimSpace(cfg.Adapters.Verifier)
	if verifier == "" {
		verifier = "claude"
	}

	providers := make([]providerSpec, 0, 2)
	add := func(name, role string) {
		for index := range providers {
			if providers[index].Name == name {
				providers[index].Roles = append(providers[index].Roles, role)
				return
			}
		}
		providers = append(providers, providerSpec{Name: name, Roles: []string{role}})
	}
	add(worker, "worker")
	add(verifier, "verifier")
	return providers
}

func checkProviders(deps Deps, providers []providerSpec) []Check {
	checks := make([]Check, 0, len(providers))
	for _, provider := range providers {
		roleText := strings.Join(provider.Roles, ", ")
		path, err := deps.LookPath(provider.Name)
		if err != nil || strings.TrimSpace(path) == "" {
			checks = append(checks, Check{
				Name:    "provider " + provider.Name,
				Status:  StatusWarn,
				Message: fmt.Sprintf("configured for %s but CLI %q was not found on PATH", roleText, provider.Name),
			})
			continue
		}
		checks = append(checks, Check{
			Name:    "provider " + provider.Name,
			Status:  StatusOK,
			Message: fmt.Sprintf("configured for %s; found at %s; authentication not checked by a stable cheap probe", roleText, path),
		})
	}
	return checks
}

func checkOrigin(ctx context.Context, deps Deps, repoPath string, gitPresent bool) (Check, bool) {
	if !gitPresent {
		return Check{
			Name:    "repository origin",
			Status:  StatusFail,
			Message: "cannot check origin remote because git is missing",
		}, false
	}
	result, err := deps.RunCommand(ctx, repoPath, "git", "remote", "get-url", "origin")
	if err != nil {
		return Check{
			Name:    "repository origin",
			Status:  StatusFail,
			Message: fmt.Sprintf("could not inspect origin remote: %v", err),
		}, false
	}
	if result.ExitCode != 0 {
		detail := commandDetail(result)
		if detail == "" {
			detail = fmt.Sprintf("exit code %d", result.ExitCode)
		}
		return Check{
			Name:    "repository origin",
			Status:  StatusFail,
			Message: fmt.Sprintf("origin remote is not configured: %s", detail),
		}, false
	}
	remoteURL := firstNonEmptyLine(result.Stdout)
	if remoteURL == "" {
		remoteURL = "(configured)"
	}
	return Check{
		Name:    "repository origin",
		Status:  StatusOK,
		Message: fmt.Sprintf("origin remote configured as %s", remoteURL),
	}, true
}

func checkDefaultBranch(ctx context.Context, deps Deps, repoPath string, gitPresent bool, originPresent bool) Check {
	if !gitPresent {
		return Check{
			Name:    "default branch",
			Status:  StatusFail,
			Message: "cannot detect origin default branch because git is missing",
		}
	}
	if !originPresent {
		return Check{
			Name:    "default branch",
			Status:  StatusFail,
			Message: "cannot detect origin default branch because origin remote is missing",
		}
	}

	result, err := deps.RunCommand(ctx, repoPath, "git", "remote", "show", "origin")
	if err == nil && result.ExitCode == 0 {
		if branch, ok := parseRemoteHeadBranch(result.Stdout); ok {
			return Check{
				Name:    "default branch",
				Status:  StatusOK,
				Message: fmt.Sprintf("origin default branch is %s", branch),
			}
		}
	}

	result, err = deps.RunCommand(ctx, repoPath, "git", "symbolic-ref", "refs/remotes/origin/HEAD", "--short")
	if err == nil && result.ExitCode == 0 {
		branch := strings.TrimSpace(result.Stdout)
		branch = strings.TrimPrefix(branch, "origin/")
		if branch != "" {
			return Check{
				Name:    "default branch",
				Status:  StatusOK,
				Message: fmt.Sprintf("origin default branch is %s (from local origin/HEAD)", branch),
			}
		}
	}

	return Check{
		Name:    "default branch",
		Status:  StatusWarn,
		Message: "origin default branch could not be detected; no branch was assumed",
	}
}

func checkBinary(build BuildInfo, deps Deps) Check {
	path, err := deps.ExecutablePath()
	status := StatusOK
	if err != nil || strings.TrimSpace(path) == "" {
		status = StatusWarn
		path = "(unknown)"
	}
	return Check{
		Name:   "loopcoder binary",
		Status: status,
		Message: fmt.Sprintf(
			"path=%s version=%s commit=%s date=%s track=%s",
			path,
			build.Version,
			build.Commit,
			build.Date,
			selectedTrack(build.Version),
		),
	}
}

func checkCompatibility(delivery deliveryState, build BuildInfo) Check {
	if !delivery.Present {
		return Check{
			Name:    "version compatibility",
			Status:  StatusWarn,
			Message: ".delivery.yml absent; defaults apply and no min_loopcoder_version is declared",
		}
	}
	if !delivery.Valid {
		return Check{
			Name:    "version compatibility",
			Status:  StatusFail,
			Message: "cannot evaluate because .delivery.yml is invalid",
		}
	}

	status := StatusOK
	parts := make([]string, 0, 3)
	if delivery.Meta.Version == nil || *delivery.Meta.Version == 0 {
		status = StatusFail
		parts = append(parts, ".delivery.yml schema version is missing")
	} else if *delivery.Meta.Version != 1 {
		status = StatusFail
		parts = append(parts, fmt.Sprintf(".delivery.yml schema version=%d is unsupported; supported version is 1", *delivery.Meta.Version))
	} else {
		parts = append(parts, ".delivery.yml schema version=1")
	}

	minimum := strings.TrimSpace(delivery.Meta.MinLoopcoderVersion)
	if minimum == "" {
		parts = append(parts, fmt.Sprintf("no min_loopcoder_version declared; selected loopcoder version=%s", build.Version))
		return Check{Name: "version compatibility", Status: status, Message: strings.Join(parts, "; ")}
	}

	minVersion, ok := parseSemver(minimum)
	if !ok {
		status = StatusFail
		parts = append(parts, fmt.Sprintf("min_loopcoder_version=%s is not a valid semantic version", minimum))
		return Check{Name: "version compatibility", Status: status, Message: strings.Join(parts, "; ")}
	}
	buildVersion, ok := parseSemver(build.Version)
	if !ok {
		if status != StatusFail {
			status = StatusWarn
		}
		parts = append(parts, fmt.Sprintf("min_loopcoder_version=%s cannot be compared with selected version=%s", minimum, build.Version))
		return Check{Name: "version compatibility", Status: status, Message: strings.Join(parts, "; ")}
	}
	if compareSemver(buildVersion, minVersion) < 0 {
		status = StatusFail
		parts = append(parts, fmt.Sprintf("selected loopcoder version=%s is older than min_loopcoder_version=%s", build.Version, minimum))
		return Check{Name: "version compatibility", Status: status, Message: strings.Join(parts, "; ")}
	}

	parts = append(parts, fmt.Sprintf("min_loopcoder_version=%s is satisfied by selected loopcoder version=%s", minimum, build.Version))
	return Check{Name: "version compatibility", Status: status, Message: strings.Join(parts, "; ")}
}

func checkInstalledSkill(deps Deps) Check {
	dir, err := defaultSkillDir(deps.UserHomeDir)
	if err != nil {
		return Check{
			Name:    "loopcoder skill",
			Status:  StatusWarn,
			Message: fmt.Sprintf("could not resolve installed skill directory: %v; run: loopcoder skill install", err),
		}
	}

	managed := []struct {
		name string
		read func() ([]byte, error)
	}{
		{name: "SKILL.md", read: deps.SkillMarkdown},
		{name: "AGENTS.md", read: deps.AgentsMarkdown},
	}

	missing := make([]string, 0, len(managed))
	stale := make([]string, 0, len(managed))
	for _, file := range managed {
		embedded, err := readEmbeddedSkill(file.name, file.read)
		if err != nil {
			return Check{
				Name:    "loopcoder skill",
				Status:  StatusWarn,
				Message: fmt.Sprintf("could not read embedded %s: %v; run: loopcoder skill install", file.name, err),
			}
		}
		path := filepath.Join(dir, file.name)
		installed, err := deps.ReadFile(path)
		if err != nil {
			if isNotExist(err) {
				missing = append(missing, file.name)
				continue
			}
			return Check{
				Name:    "loopcoder skill",
				Status:  StatusWarn,
				Message: fmt.Sprintf("could not inspect installed %s: %v; run: loopcoder skill install", file.name, err),
			}
		}
		if !bytes.Equal(installed, embedded) {
			stale = append(stale, file.name)
		}
	}

	if len(missing) == len(managed) {
		return Check{
			Name:    "loopcoder skill",
			Status:  StatusInfo,
			Message: fmt.Sprintf("not installed at %s; run: loopcoder skill install", dir),
		}
	}
	if len(missing) > 0 || len(stale) > 0 {
		details := make([]string, 0, 2)
		if len(stale) > 0 {
			details = append(details, "stale "+strings.Join(stale, ", "))
		}
		if len(missing) > 0 {
			details = append(details, "missing "+strings.Join(missing, ", "))
		}
		return Check{
			Name:    "loopcoder skill",
			Status:  StatusWarn,
			Message: fmt.Sprintf("installed loopcoder skill is stale or partial compared with selected binary embedded content (%s); run: loopcoder skill install", strings.Join(details, "; ")),
		}
	}

	return Check{
		Name:    "loopcoder skill",
		Status:  StatusOK,
		Message: fmt.Sprintf("installed managed files match selected binary embedded content at %s", dir),
	}
}

func defaultSkillDir(userHomeDir func() (string, error)) (string, error) {
	homeDir, err := userHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	homeDir = strings.TrimSpace(homeDir)
	if homeDir == "" {
		return "", errors.New("empty user home")
	}
	return filepath.Join(homeDir, ".claude", "skills", "loopcoder"), nil
}

func readEmbeddedSkill(name string, read func() ([]byte, error)) ([]byte, error) {
	data, err := read()
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("embedded content is empty")
	}
	return data, nil
}

func normalizeBuildInfo(build BuildInfo) BuildInfo {
	if strings.TrimSpace(build.Version) == "" {
		build.Version = "dev"
	}
	if strings.TrimSpace(build.Commit) == "" {
		build.Commit = "unknown"
	}
	if strings.TrimSpace(build.Date) == "" {
		build.Date = "unknown"
	}
	return build
}

func selectedTrack(version string) string {
	if _, ok := parseSemver(version); ok {
		return "release"
	}
	return "development source build"
}

func commandDetail(result CommandResult) string {
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	return detail
}

func isNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist)
}

func firstNonEmptyLine(text string) string {
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func parseRemoteHeadBranch(output string) (string, bool) {
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		const prefix = "head branch:"
		if !strings.HasPrefix(lower, prefix) {
			continue
		}
		branch := strings.TrimSpace(trimmed[len(prefix):])
		if branch == "" || branch == "(unknown)" {
			return "", false
		}
		return branch, true
	}
	return "", false
}

type semver struct {
	major      int
	minor      int
	patch      int
	prerelease string
}

func parseSemver(value string) (semver, bool) {
	trimmed := strings.TrimSpace(value)
	trimmed = strings.TrimPrefix(trimmed, "v")
	if trimmed == "" {
		return semver{}, false
	}

	core := trimmed
	prerelease := ""
	if index := strings.Index(core, "+"); index >= 0 {
		core = core[:index]
	}
	if index := strings.Index(core, "-"); index >= 0 {
		prerelease = core[index+1:]
		core = core[:index]
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	major, ok := parseSemverPart(parts[0])
	if !ok {
		return semver{}, false
	}
	minor, ok := parseSemverPart(parts[1])
	if !ok {
		return semver{}, false
	}
	patch, ok := parseSemverPart(parts[2])
	if !ok {
		return semver{}, false
	}
	return semver{major: major, minor: minor, patch: patch, prerelease: prerelease}, true
}

func parseSemverPart(value string) (int, bool) {
	if value == "" {
		return 0, false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

func compareSemver(a, b semver) int {
	for _, pair := range [][2]int{
		{a.major, b.major},
		{a.minor, b.minor},
		{a.patch, b.patch},
	} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	if a.prerelease == b.prerelease {
		return 0
	}
	if a.prerelease == "" {
		return 1
	}
	if b.prerelease == "" {
		return -1
	}
	return strings.Compare(a.prerelease, b.prerelease)
}
