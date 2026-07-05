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
	"time"

	loopcoder "github.com/jasonhnd/loopcoder"
	"github.com/jasonhnd/loopcoder/internal/audit"
	"github.com/jasonhnd/loopcoder/internal/claudehooks"
	"github.com/jasonhnd/loopcoder/internal/config"
	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
	"gopkg.in/yaml.v3"
)

const commandHardCapDefault = lcdefaults.DoctorCommandHardCap

var commandHardCap = commandHardCapDefault

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
	RepoPath   string
	BaseBranch string
	BuildInfo  BuildInfo
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
	baseBranch := strings.TrimSpace(opts.BaseBranch)
	if baseBranch == "" {
		baseBranch = lcdefaults.BaseBranch
	}
	build := normalizeBuildInfo(opts.BuildInfo)

	delivery := loadDelivery(ctx, repoPath, baseBranch, deps)
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
	checks = append(checks, checkAuditReadiness(repoPath, delivery, deps)...)
	checks = append(checks, checkInstalledSkill(deps))
	checks = append(checks, checkConductorHooks(repoPath, deps))
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
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, name, args...)
	if strings.TrimSpace(dir) != "" {
		cmd.Dir = dir
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	result := CommandResult{}
	runResult, err := supervisedexec.Run(ctx, cmd, supervisedexec.Options{HardCap: commandHardCap})
	result.Stdout = stdout.String()
	result.Stderr = stderr.String()
	if err != nil {
		return result, err
	}
	if runResult.Outcome == supervisedexec.OutcomeDeadline {
		return result, fmt.Errorf("%s timed out after %s", strings.Join(append([]string{name}, args...), " "), commandHardCap)
	}
	if runResult.ExitCode == 0 {
		return result, nil
	}
	result.ExitCode = runResult.ExitCode
	return result, nil
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
	Path        string
	Present     bool
	Valid       bool
	BaseBranch  string
	BasePresent bool
	Config      config.Config
	Meta        deliveryMetadata
	Err         error
}

type deliveryMetadata struct {
	Version             *int   `yaml:"version"`
	MinLoopcoderVersion string `yaml:"min_loopcoder_version"`
}

func loadDelivery(ctx context.Context, repoPath string, baseBranch string, deps Deps) deliveryState {
	path := filepath.Join(repoPath, ".delivery.yml")
	cfg, err := deps.LoadConfig(path)
	if err != nil {
		present := !errors.Is(err, os.ErrNotExist)
		return deliveryState{
			Path:        path,
			Present:     present,
			Valid:       false,
			BaseBranch:  baseBranch,
			BasePresent: !present && baseDeliveryConfigExists(ctx, repoPath, baseBranch, deps),
			Config:      config.Default(),
			Err:         err,
		}
	}
	data, err := deps.ReadFile(path)
	if err != nil {
		return deliveryState{
			Path:       path,
			Present:    true,
			Valid:      false,
			BaseBranch: baseBranch,
			Config:     cfg,
			Err:        fmt.Errorf("read delivery metadata: %w", err),
		}
	}
	var meta deliveryMetadata
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return deliveryState{
			Path:       path,
			Present:    true,
			Valid:      false,
			BaseBranch: baseBranch,
			Config:     cfg,
			Err:        fmt.Errorf("parse delivery metadata: %w", err),
		}
	}
	return deliveryState{
		Path:       path,
		Present:    true,
		Valid:      true,
		BaseBranch: baseBranch,
		Config:     cfg,
		Meta:       meta,
	}
}

func checkDeliveryConfig(delivery deliveryState) Check {
	if !delivery.Present {
		if delivery.BasePresent {
			return Check{
				Name:    ".delivery.yml",
				Status:  StatusWarn,
				Message: fmt.Sprintf("absent from working tree but present on %s; run from base or use --config-from-base", delivery.BaseBranch),
			}
		}
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

func baseDeliveryConfigExists(ctx context.Context, repoPath string, baseBranch string, deps Deps) bool {
	result, err := deps.RunCommand(ctx, repoPath, "git", "show", strings.TrimSpace(baseBranch)+":.delivery.yml")
	return err == nil && result.ExitCode == 0
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

func checkAuditReadiness(repoPath string, delivery deliveryState, deps Deps) []Check {
	if !delivery.Valid {
		return []Check{{
			Name:    "audit config",
			Status:  StatusFail,
			Message: "cannot evaluate audit readiness because .delivery.yml is invalid",
		}}
	}
	plan, err := audit.BuildPlan(repoPath, delivery.Config.Audit, audit.Options{})
	if err != nil {
		return []Check{{
			Name:    "audit config",
			Status:  StatusFail,
			Message: fmt.Sprintf("invalid audit config: %v", err),
		}}
	}
	checks := []Check{
		checkAuditConfig(plan),
		checkAuditTools(plan, deps),
		checkAuditParsers(plan),
		checkAuditRubric(repoPath, delivery.Config.Audit.Review.RubricPath, deps),
		checkAuditBaseline(repoPath, delivery.Config.Audit.Baseline.Path, deps),
		checkAuditCICheck(repoPath, delivery.Config, deps),
		checkAuditLLMProvider(delivery.Config, deps),
	}
	return checks
}

func checkAuditConfig(plan audit.Plan) Check {
	native := []string{}
	if plan.Native.Secrets {
		native = append(native, "secrets")
	}
	if plan.Native.FilePermissions {
		native = append(native, "file_permissions")
	}
	if len(native) == 0 {
		native = append(native, "disabled")
	}
	commandIDs := make([]string, 0, len(plan.Commands))
	for _, command := range plan.Commands {
		commandIDs = append(commandIDs, command.ID)
	}
	commandText := strings.Join(commandIDs, ", ")
	if commandText == "" {
		commandText = "none"
	}
	return Check{
		Name:    "audit config",
		Status:  StatusOK,
		Message: fmt.Sprintf("threshold=%s; sast_commands=%s; native=%s", plan.Threshold, commandText, strings.Join(native, ",")),
	}
}

func checkAuditTools(plan audit.Plan, deps Deps) Check {
	if len(plan.Commands) == 0 {
		return Check{
			Name:    "audit tools",
			Status:  StatusInfo,
			Message: "no language SAST commands configured; native audit scans still run when enabled",
		}
	}
	missing := []string{}
	found := []string{}
	for _, command := range plan.Commands {
		if len(command.Argv) == 0 {
			missing = append(missing, command.ID+"=(empty argv)")
			continue
		}
		path, err := deps.LookPath(command.Argv[0])
		if err != nil || strings.TrimSpace(path) == "" {
			missing = append(missing, command.ID+"="+command.Argv[0])
			continue
		}
		found = append(found, command.ID+"="+path)
	}
	if len(missing) > 0 {
		return Check{
			Name:    "audit tools",
			Status:  StatusFail,
			Message: fmt.Sprintf("missing required SAST tools on PATH: %s; found: %s", strings.Join(missing, ", "), firstNonEmpty(strings.Join(found, ", "), "none")),
		}
	}
	return Check{
		Name:    "audit tools",
		Status:  StatusOK,
		Message: "required SAST tools found on PATH: " + strings.Join(found, ", "),
	}
}

func checkAuditParsers(plan audit.Plan) Check {
	if len(plan.Commands) == 0 {
		return Check{
			Name:    "audit parsers",
			Status:  StatusInfo,
			Message: "no configured SAST command parsers to check",
		}
	}
	recognized := []string{}
	unrecognized := []string{}
	for _, command := range plan.Commands {
		if audit.KnownParser(command.Parser) {
			recognized = append(recognized, command.ID+"="+command.Parser)
		} else {
			unrecognized = append(unrecognized, command.ID+"="+command.Parser)
		}
	}
	if len(unrecognized) > 0 {
		return Check{
			Name:    "audit parsers",
			Status:  StatusFail,
			Message: "unrecognized parser(s): " + strings.Join(unrecognized, ", "),
		}
	}
	return Check{
		Name:    "audit parsers",
		Status:  StatusOK,
		Message: "recognized parser(s): " + strings.Join(recognized, ", "),
	}
}

func checkAuditRubric(repoPath, rawPath string, deps Deps) Check {
	if strings.TrimSpace(rawPath) == "" {
		return Check{
			Name:    "audit rubric",
			Status:  StatusInfo,
			Message: "no configured rubric path; Layer 2 uses the built-in threat model only",
		}
	}
	clean, err := safeRepoRelativePath(rawPath)
	if err != nil {
		return Check{
			Name:    "audit rubric",
			Status:  StatusFail,
			Message: fmt.Sprintf("configured rubric path is invalid: %v", err),
		}
	}
	data, err := deps.ReadFile(filepath.Join(repoPath, filepath.FromSlash(clean)))
	if err != nil {
		return Check{
			Name:    "audit rubric",
			Status:  StatusFail,
			Message: fmt.Sprintf("configured rubric %s is not readable: %v", clean, err),
		}
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return Check{
			Name:    "audit rubric",
			Status:  StatusFail,
			Message: fmt.Sprintf("configured rubric %s is empty", clean),
		}
	}
	return Check{
		Name:    "audit rubric",
		Status:  StatusOK,
		Message: fmt.Sprintf("configured rubric exists at %s", clean),
	}
}

func checkAuditBaseline(repoPath, rawPath string, deps Deps) Check {
	if strings.TrimSpace(rawPath) == "" {
		return Check{
			Name:    "audit baseline",
			Status:  StatusInfo,
			Message: "no audit baseline configured",
		}
	}
	clean, err := safeRepoRelativePath(rawPath)
	if err != nil {
		return Check{
			Name:    "audit baseline",
			Status:  StatusFail,
			Message: fmt.Sprintf("configured baseline path is invalid: %v", err),
		}
	}
	data, err := deps.ReadFile(filepath.Join(repoPath, filepath.FromSlash(clean)))
	if err != nil {
		return Check{
			Name:    "audit baseline",
			Status:  StatusFail,
			Message: fmt.Sprintf("configured baseline %s is not readable: %v", clean, err),
		}
	}
	var baseline audit.BaselineFile
	if err := yaml.Unmarshal(data, &baseline); err != nil {
		return Check{
			Name:    "audit baseline",
			Status:  StatusFail,
			Message: fmt.Sprintf("parse audit baseline %s: %v", clean, err),
		}
	}
	validation := audit.ValidateBaseline(baseline, time.Now())
	if len(validation.Errors) > 0 {
		return Check{
			Name:    "audit baseline",
			Status:  StatusFail,
			Message: "invalid baseline waiver(s): " + strings.Join(validation.Errors, "; "),
		}
	}
	if len(validation.Expired) > 0 {
		ids := make([]string, 0, len(validation.Expired))
		for _, waiver := range validation.Expired {
			ids = append(ids, waiver.ID)
		}
		return Check{
			Name:    "audit baseline",
			Status:  StatusFail,
			Message: "expired baseline waiver(s): " + strings.Join(ids, ", "),
		}
	}
	return Check{
		Name:    "audit baseline",
		Status:  StatusOK,
		Message: fmt.Sprintf("baseline parsed with %d waiver(s); no expired or broad waivers", len(baseline.Waivers)),
	}
}

func checkAuditCICheck(repoPath string, cfg config.Config, deps Deps) Check {
	inDelivery := false
	for _, check := range cfg.CI.Checks {
		if strings.TrimSpace(check) == "audit" {
			inDelivery = true
			break
		}
	}
	workflowHasJob, workflowErr := workflowHasAuditJob(repoPath, deps)
	switch {
	case inDelivery && workflowHasJob:
		return Check{
			Name:    "audit ci check",
			Status:  StatusOK,
			Message: "ci.checks includes audit and workflow job audit exists",
		}
	case !inDelivery && workflowHasJob:
		return Check{
			Name:    "audit ci check",
			Status:  StatusFail,
			Message: "workflow job audit exists but .delivery.yml ci.checks does not include audit",
		}
	case inDelivery && workflowErr != nil:
		return Check{
			Name:    "audit ci check",
			Status:  StatusFail,
			Message: fmt.Sprintf(".delivery.yml ci.checks includes audit but workflow job could not be verified: %v", workflowErr),
		}
	case inDelivery:
		return Check{
			Name:    "audit ci check",
			Status:  StatusFail,
			Message: ".delivery.yml ci.checks includes audit but no workflow job named audit was found",
		}
	default:
		if workflowErr != nil {
			return Check{
				Name:    "audit ci check",
				Status:  StatusInfo,
				Message: fmt.Sprintf("audit is not required in ci.checks; workflow job lookup skipped after error: %v", workflowErr),
			}
		}
		return Check{
			Name:    "audit ci check",
			Status:  StatusInfo,
			Message: "audit is not required in ci.checks",
		}
	}
}

func workflowHasAuditJob(repoPath string, deps Deps) (bool, error) {
	data, err := deps.ReadFile(filepath.Join(repoPath, ".github", "workflows", "ci.yml"))
	if err != nil {
		return false, err
	}
	var workflow struct {
		Jobs map[string]any `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		return false, err
	}
	_, ok := workflow.Jobs["audit"]
	return ok, nil
}

func checkAuditLLMProvider(cfg config.Config, deps Deps) Check {
	provider := strings.TrimSpace(cfg.Adapters.Verifier)
	if provider == "" {
		provider = "claude"
	}
	if _, err := config.MCPServersForInvocation(cfg.MCP, config.MCPInvocationOptions{
		Role:           "verifier",
		ReadOnly:       true,
		ReadOnlyPolicy: config.MCPReadOnlyFilter,
		RequireRole:    true,
		ErrorPrefix:    "invalid delivery config: ",
	}); err != nil {
		return Check{
			Name:    "audit llm provider",
			Status:  StatusFail,
			Message: fmt.Sprintf("read-only verifier MCP selection failed for Layer 2: %v", err),
		}
	}
	path, err := deps.LookPath(provider)
	if err != nil || strings.TrimSpace(path) == "" {
		return Check{
			Name:    "audit llm provider",
			Status:  StatusWarn,
			Message: fmt.Sprintf("Layer 2 verifier provider %q could not be resolved on PATH", provider),
		}
	}
	return Check{
		Name:    "audit llm provider",
		Status:  StatusOK,
		Message: fmt.Sprintf("Layer 2 read-only verifier provider %q resolves at %s", provider, path),
	}
}

func safeRepoRelativePath(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("path is required")
	}
	if filepath.IsAbs(raw) {
		return "", errors.New("absolute paths are not allowed")
	}
	clean := filepath.Clean(filepath.FromSlash(raw))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("path must stay inside the repository")
	}
	return strings.ReplaceAll(clean, string(filepath.Separator), "/"), nil
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

func checkConductorHooks(repoPath string, deps Deps) Check {
	path := claudehooks.SettingsPath(repoPath)
	data, err := deps.ReadFile(path)
	if err != nil {
		if isNotExist(err) {
			return Check{
				Name:    "conductor hooks",
				Status:  StatusWarn,
				Message: fmt.Sprintf("active Claude Code settings not found at %s; missing loopcoder conductor hooks: %s; run: loopcoder skill install", path, claudehooks.FormatMissing(claudehooks.RequiredHooks())),
			}
		}
		return Check{
			Name:    "conductor hooks",
			Status:  StatusWarn,
			Message: fmt.Sprintf("could not inspect active Claude Code settings at %s: %v; run: loopcoder skill install", path, err),
		}
	}
	missing, err := claudehooks.MissingHooks(data)
	if err != nil {
		return Check{
			Name:    "conductor hooks",
			Status:  StatusWarn,
			Message: fmt.Sprintf("could not inspect active Claude Code hooks at %s: %v; run: loopcoder skill install", path, err),
		}
	}
	if len(missing) > 0 {
		return Check{
			Name:    "conductor hooks",
			Status:  StatusWarn,
			Message: fmt.Sprintf("active Claude Code settings at %s are missing loopcoder conductor hooks: %s; run: loopcoder skill install", path, claudehooks.FormatMissing(missing)),
		}
	}
	// The hooks are registered, but Claude Code runs them as `loopcoder hook
	// <name>`, so a healthy install also requires the loopcoder binary to resolve
	// on PATH. Only checking that the command string is present in settings is
	// exactly the false-positive that hid the earlier broken install.
	if _, err := deps.LookPath("loopcoder"); err != nil {
		return Check{
			Name:    "conductor hooks",
			Status:  StatusWarn,
			Message: fmt.Sprintf("active Claude Code settings at %s include loopcoder conductor hooks, but the loopcoder binary is not on PATH, so Claude Code cannot run them; install loopcoder on PATH", path),
		}
	}
	return Check{
		Name:    "conductor hooks",
		Status:  StatusOK,
		Message: fmt.Sprintf("active Claude Code settings include loopcoder conductor hooks at %s and the loopcoder binary resolves on PATH", path),
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
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
