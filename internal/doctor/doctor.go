package doctor

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	loopcoder "github.com/jasonhnd/loopcoder"
	"github.com/jasonhnd/loopcoder/internal/audit"
	"github.com/jasonhnd/loopcoder/internal/claudehooks"
	"github.com/jasonhnd/loopcoder/internal/config"
	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/gitlocal"
	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/hostprofile"
	"github.com/jasonhnd/loopcoder/internal/localcleanup"
	"github.com/jasonhnd/loopcoder/internal/migration"
	"github.com/jasonhnd/loopcoder/internal/models"
	"github.com/jasonhnd/loopcoder/internal/provider"
	"github.com/jasonhnd/loopcoder/internal/registry"
	"github.com/jasonhnd/loopcoder/internal/reportquery"
	"github.com/jasonhnd/loopcoder/internal/runtimecap"
	"github.com/jasonhnd/loopcoder/internal/runtimepath"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
	"github.com/jasonhnd/loopcoder/internal/upgrade"
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
	Fix        bool
}

type Deps struct {
	LookPath           func(file string) (string, error)
	RunCommand         func(ctx context.Context, dir string, name string, args ...string) (CommandResult, error)
	LoadConfig         func(path string) (config.Config, error)
	Getenv             func(string) string
	ReadFile           func(path string) ([]byte, error)
	ExecutablePath     func() (string, error)
	UserHomeDir        func() (string, error)
	SkillMarkdown      func() ([]byte, error)
	AgentsMarkdown     func() ([]byte, error)
	CleanupPlan        func(localcleanup.Options) (localcleanup.Result, error)
	StorageHealth      func(context.Context, string) (storage.Health, error)
	StoragePermissions func(path string, fix bool) (storage.PermissionReport, error)
	ProjectShow        func(context.Context, registry.Options) (registry.ShowResult, error)
	ProjectDuplicates  func(context.Context, registry.Options) ([]registry.DuplicatePhysicalIdentity, error)
	ProjectRepair      func(context.Context, registry.Options) ([]registry.DuplicatePhysicalIdentity, error)
}

type CommandResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

type Check struct {
	Name       string
	Code       string
	Status     Status
	Message    string
	Hard       bool
	FixCommand string
}

type Report struct {
	RepoPath              string
	Version               string
	Commit                string
	Date                  string
	HostProfile           HostProfile
	Runtime               RuntimeHealth
	ProviderCompatibility []ProviderCompatibility
	Checks                []Check
}

type HostProfile struct {
	Name               string
	Source             string
	Selector           string
	InvocationStyle    string
	SupportsHooks      bool
	SupportsJSONOutput bool
	DetectedBy         []string
	KnownLimitations   []string
}

type ProviderCompatibility struct {
	Provider             string
	Host                 string
	Role                 string
	Support              string
	Status               Status
	Code                 string
	RequiredCapabilities []string
	MissingCapabilities  []string
	KnownLimitations     []string
}

type RuntimeHealth struct {
	HomeDir         string
	Database        RuntimeDatabase
	ProjectRegistry RuntimeProjectRegistry
	Migration       RuntimeMigration
	NestedRuns      RuntimeNestedRuns
}

type RuntimeDatabase struct {
	Path          string
	Exists        bool
	SchemaVersion int
	Status        Status
	Message       string
}

type RuntimeProjectRegistry struct {
	Status         Status
	Registered     bool
	Detached       bool
	ProjectID      string
	IdentitySource string
	PayloadRoot    string
	RunsRoot       string
	RelayRoot      string
	RecoveryRoot   string
	AuditRoot      string
	LogsRoot       string
	TmpRoot        string
	FallbackMode   string
	ConflictCount  int
	Message        string
}

type RuntimeMigration struct {
	Status         Status
	LegacySurfaces int
	Message        string
}

type RuntimeNestedRuns struct {
	Status       Status
	RunCount     int
	ParentEdges  int
	ChildEdges   int
	ProblemCount int
	Message      string
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

func RenderJSON(w io.Writer, report Report) error {
	type renderedCheck struct {
		Name       string `json:"name"`
		Code       string `json:"code,omitempty"`
		Status     Status `json:"status"`
		Hard       bool   `json:"hard"`
		Message    string `json:"message"`
		FixCommand string `json:"fix_command"`
	}
	type renderedHostProfile struct {
		Name               string   `json:"name"`
		Source             string   `json:"source"`
		Selector           string   `json:"selector,omitempty"`
		InvocationStyle    string   `json:"invocation_style"`
		SupportsHooks      bool     `json:"supports_hooks"`
		SupportsJSONOutput bool     `json:"supports_json_output"`
		DetectedBy         []string `json:"detected_by,omitempty"`
		KnownLimitations   []string `json:"known_limitations,omitempty"`
	}
	type renderedProviderCompatibility struct {
		Provider             string   `json:"provider"`
		Host                 string   `json:"host"`
		Role                 string   `json:"role"`
		Support              string   `json:"support"`
		Status               Status   `json:"status"`
		Code                 string   `json:"code"`
		RequiredCapabilities []string `json:"required_capabilities,omitempty"`
		MissingCapabilities  []string `json:"missing_capabilities,omitempty"`
		KnownLimitations     []string `json:"known_limitations,omitempty"`
	}
	type renderedRuntime struct {
		HomeDir  string `json:"home_dir,omitempty"`
		Database struct {
			Path          string `json:"path,omitempty"`
			Exists        bool   `json:"exists"`
			SchemaVersion int    `json:"schema_version"`
			Status        Status `json:"status"`
			Message       string `json:"message"`
		} `json:"database"`
		ProjectRegistry struct {
			Status         Status `json:"status"`
			Registered     bool   `json:"registered"`
			Detached       bool   `json:"detached"`
			ProjectID      string `json:"project_id,omitempty"`
			IdentitySource string `json:"identity_source,omitempty"`
			PayloadRoot    string `json:"payload_root,omitempty"`
			RunsRoot       string `json:"runs_root,omitempty"`
			RelayRoot      string `json:"relay_root,omitempty"`
			RecoveryRoot   string `json:"recovery_root,omitempty"`
			AuditRoot      string `json:"audit_root,omitempty"`
			LogsRoot       string `json:"logs_root,omitempty"`
			TmpRoot        string `json:"tmp_root,omitempty"`
			FallbackMode   string `json:"fallback_mode,omitempty"`
			ConflictCount  int    `json:"conflict_count"`
			Message        string `json:"message"`
		} `json:"project_registry"`
		Migration struct {
			Status         Status `json:"status"`
			LegacySurfaces int    `json:"legacy_surfaces"`
			Message        string `json:"message"`
		} `json:"migration"`
		NestedRuns struct {
			Status       Status `json:"status"`
			RunCount     int    `json:"run_count"`
			ParentEdges  int    `json:"parent_edges"`
			ChildEdges   int    `json:"child_edges"`
			ProblemCount int    `json:"problem_count"`
			Message      string `json:"message"`
		} `json:"nested_runs"`
	}
	checks := make([]renderedCheck, 0, len(report.Checks))
	for _, check := range report.Checks {
		checks = append(checks, renderedCheck{
			Name:       check.Name,
			Code:       check.Code,
			Status:     check.Status,
			Hard:       check.Hard,
			Message:    check.Message,
			FixCommand: check.FixCommand,
		})
	}
	compatibility := make([]renderedProviderCompatibility, 0, len(report.ProviderCompatibility))
	for _, entry := range report.ProviderCompatibility {
		compatibility = append(compatibility, renderedProviderCompatibility{
			Provider:             entry.Provider,
			Host:                 entry.Host,
			Role:                 entry.Role,
			Support:              entry.Support,
			Status:               entry.Status,
			Code:                 entry.Code,
			RequiredCapabilities: append([]string(nil), entry.RequiredCapabilities...),
			MissingCapabilities:  append([]string(nil), entry.MissingCapabilities...),
			KnownLimitations:     append([]string(nil), entry.KnownLimitations...),
		})
	}
	payload := struct {
		RepoPath              string                          `json:"repo_path"`
		Version               string                          `json:"version"`
		Commit                string                          `json:"commit"`
		Date                  string                          `json:"date"`
		ExitCode              int                             `json:"exit_code"`
		Host                  renderedHostProfile             `json:"host_profile"`
		Runtime               renderedRuntime                 `json:"runtime"`
		ProviderCompatibility []renderedProviderCompatibility `json:"provider_compatibility"`
		Checks                []renderedCheck                 `json:"checks"`
	}{
		RepoPath: report.RepoPath,
		Version:  report.Version,
		Commit:   report.Commit,
		Date:     report.Date,
		ExitCode: report.ExitCode(),
		Host: renderedHostProfile{
			Name:               report.HostProfile.Name,
			Source:             report.HostProfile.Source,
			Selector:           report.HostProfile.Selector,
			InvocationStyle:    report.HostProfile.InvocationStyle,
			SupportsHooks:      report.HostProfile.SupportsHooks,
			SupportsJSONOutput: report.HostProfile.SupportsJSONOutput,
			DetectedBy:         append([]string(nil), report.HostProfile.DetectedBy...),
			KnownLimitations:   append([]string(nil), report.HostProfile.KnownLimitations...),
		},
		ProviderCompatibility: compatibility,
		Checks:                checks,
	}
	payload.Runtime.HomeDir = filepath.ToSlash(report.Runtime.HomeDir)
	payload.Runtime.Database.Path = filepath.ToSlash(report.Runtime.Database.Path)
	payload.Runtime.Database.Exists = report.Runtime.Database.Exists
	payload.Runtime.Database.SchemaVersion = report.Runtime.Database.SchemaVersion
	payload.Runtime.Database.Status = report.Runtime.Database.Status
	payload.Runtime.Database.Message = report.Runtime.Database.Message
	payload.Runtime.ProjectRegistry.Status = report.Runtime.ProjectRegistry.Status
	payload.Runtime.ProjectRegistry.Registered = report.Runtime.ProjectRegistry.Registered
	payload.Runtime.ProjectRegistry.Detached = report.Runtime.ProjectRegistry.Detached
	payload.Runtime.ProjectRegistry.ProjectID = report.Runtime.ProjectRegistry.ProjectID
	payload.Runtime.ProjectRegistry.IdentitySource = report.Runtime.ProjectRegistry.IdentitySource
	payload.Runtime.ProjectRegistry.PayloadRoot = filepath.ToSlash(report.Runtime.ProjectRegistry.PayloadRoot)
	payload.Runtime.ProjectRegistry.RunsRoot = filepath.ToSlash(report.Runtime.ProjectRegistry.RunsRoot)
	payload.Runtime.ProjectRegistry.RelayRoot = filepath.ToSlash(report.Runtime.ProjectRegistry.RelayRoot)
	payload.Runtime.ProjectRegistry.RecoveryRoot = filepath.ToSlash(report.Runtime.ProjectRegistry.RecoveryRoot)
	payload.Runtime.ProjectRegistry.AuditRoot = filepath.ToSlash(report.Runtime.ProjectRegistry.AuditRoot)
	payload.Runtime.ProjectRegistry.LogsRoot = filepath.ToSlash(report.Runtime.ProjectRegistry.LogsRoot)
	payload.Runtime.ProjectRegistry.TmpRoot = filepath.ToSlash(report.Runtime.ProjectRegistry.TmpRoot)
	payload.Runtime.ProjectRegistry.FallbackMode = report.Runtime.ProjectRegistry.FallbackMode
	payload.Runtime.ProjectRegistry.ConflictCount = report.Runtime.ProjectRegistry.ConflictCount
	payload.Runtime.ProjectRegistry.Message = report.Runtime.ProjectRegistry.Message
	payload.Runtime.Migration.Status = report.Runtime.Migration.Status
	payload.Runtime.Migration.LegacySurfaces = report.Runtime.Migration.LegacySurfaces
	payload.Runtime.Migration.Message = report.Runtime.Migration.Message
	payload.Runtime.NestedRuns.Status = report.Runtime.NestedRuns.Status
	payload.Runtime.NestedRuns.RunCount = report.Runtime.NestedRuns.RunCount
	payload.Runtime.NestedRuns.ParentEdges = report.Runtime.NestedRuns.ParentEdges
	payload.Runtime.NestedRuns.ChildEdges = report.Runtime.NestedRuns.ChildEdges
	payload.Runtime.NestedRuns.ProblemCount = report.Runtime.NestedRuns.ProblemCount
	payload.Runtime.NestedRuns.Message = report.Runtime.NestedRuns.Message
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(payload)
}

func WithMetadata(report Report, repoPath string, build BuildInfo) Report {
	build = normalizeBuildInfo(build)
	if strings.TrimSpace(report.RepoPath) == "" {
		report.RepoPath = repoPath
	}
	if strings.TrimSpace(report.Version) == "" {
		report.Version = build.Version
	}
	if strings.TrimSpace(report.Commit) == "" {
		report.Commit = build.Commit
	}
	if strings.TrimSpace(report.Date) == "" {
		report.Date = build.Date
	}
	if strings.TrimSpace(report.HostProfile.Name) == "" && strings.TrimSpace(report.HostProfile.Source) == "" {
		if resolved, err := hostprofile.Resolve(hostprofile.Options{Getenv: func(string) string { return "" }}); err == nil {
			report.HostProfile = renderHostProfile(resolved)
		}
	}
	return report
}

func DefaultDeps() Deps {
	return Deps{
		LookPath:       exec.LookPath,
		RunCommand:     execRunCommand,
		LoadConfig:     config.Load,
		Getenv:         os.Getenv,
		ReadFile:       os.ReadFile,
		ExecutablePath: os.Executable,
		UserHomeDir:    os.UserHomeDir,
		SkillMarkdown:  loopcoder.SkillMarkdown,
		AgentsMarkdown: loopcoder.AgentsMarkdown,
		CleanupPlan:    localcleanup.Plan,
		StorageHealth:  storage.CheckHealth,
		StoragePermissions: func(path string, fix bool) (storage.PermissionReport, error) {
			if fix {
				return storage.RepairPermissions(path)
			}
			return storage.CheckPermissions(path)
		},
		ProjectShow: func(ctx context.Context, opts registry.Options) (registry.ShowResult, error) {
			return registry.Show(ctx, opts, registry.DefaultDeps())
		},
		ProjectDuplicates: func(ctx context.Context, opts registry.Options) ([]registry.DuplicatePhysicalIdentity, error) {
			return registry.DuplicatePhysicalIdentities(ctx, opts, registry.DefaultDeps())
		},
		ProjectRepair: func(ctx context.Context, opts registry.Options) ([]registry.DuplicatePhysicalIdentity, error) {
			return registry.RepairDuplicatePhysicalIdentities(ctx, opts, registry.DefaultDeps())
		},
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

	if opts.Fix {
		return runFix(ctx, repoPath, baseBranch, build, deps)
	}

	delivery := loadDelivery(ctx, repoPath, baseBranch, deps)
	checks := make([]Check, 0, 10)
	host, hostCheck := resolveHostProfile(delivery, deps)

	gitCheck, gitPresent := checkGit(deps)
	checks = append(checks, gitCheck)

	ghCheck, ghPresent := checkGH(ctx, deps)
	checks = append(checks, ghCheck)
	_ = ghPresent
	checks = append(checks, checkLocalStateExclude(ctx, repoPath, gitPresent, deps))
	checks = append(checks, checkTrackedLoopcoderState(ctx, repoPath, gitPresent, deps))

	checks = append(checks, checkDeliveryConfig(delivery))
	checks = append(checks, hostCheck)
	checks = append(checks, checkModelSelections(delivery))
	checks = append(checks, checkProviders(ctx, deps, configuredProviders(delivery.Config))...)
	checks = append(checks, checkProviderCompatibility(delivery.Config, host)...)

	originCheck, originPresent := checkOrigin(ctx, deps, repoPath, gitPresent)
	checks = append(checks, originCheck)
	checks = append(checks, checkDefaultBranch(ctx, deps, repoPath, gitPresent, originPresent))

	checks = append(checks, checkBinary(build, deps))
	checks = append(checks, checkCompatibility(delivery, build))
	checks = append(checks, checkVersionStatus(build, repoPath, deps))
	checks = append(checks, checkAuditReadiness(repoPath, delivery, deps)...)
	checks = append(checks, checkInstalledSkill(deps))
	checks = append(checks, checkConductorHooks(repoPath, deps))
	checks = append(checks, checkReportQuery(repoPath))
	checks = append(checks, checkStoragePermissions(deps))
	checks = append(checks, checkStorageHealth(ctx, deps))
	checks = append(checks, checkProjectRegistry(ctx, repoPath, deps))
	checks = append(checks, checkLocalStateImport(ctx, repoPath, deps))
	checks = append(checks, checkMigrationStatus(repoPath, deps))
	checks = append(checks, checkNestedRunHealth(repoPath))
	checks = append(checks, checkStaleState(repoPath, deps))
	checks = append(checks, Check{
		Name:    "conductor runtime",
		Status:  StatusOK,
		Message: "user-provided by the active Claude Code or Codex host; loopcoder does not ship it",
	})

	return WithMetadata(Report{
		HostProfile:           host,
		Runtime:               runtimeHealth(ctx, repoPath, deps),
		ProviderCompatibility: renderProviderCompatibility(provider.SmokeMatrix(runtimecap.DefaultContract())),
		Checks:                checks,
	}, repoPath, build)
}

func runFix(ctx context.Context, repoPath, baseBranch string, build BuildInfo, deps Deps) Report {
	checks := []Check{
		fixStoragePermissions(deps),
		fixProjectRegistryDuplicates(ctx, repoPath, deps),
		fixDeliveryConfig(repoPath, deps),
		fixConductorHookSettings(repoPath, deps),
		fixConductorHookState(repoPath),
		fixLegacyStateKeys(repoPath),
		fixStaleState(repoPath),
	}
	status := scanMigrationStatus(repoPath, deps)
	remaining := migrationLegacyCount(status)
	if remaining > 0 {
		checks = append(checks, Check{
			Name:    "post-upgrade repair",
			Status:  StatusWarn,
			Message: fmt.Sprintf("repair completed with %d legacy surface(s) still reported; env vars must be changed in the shell and unreadable state may need manual repair", remaining),
		})
	} else {
		checks = append(checks, Check{
			Name:    "post-upgrade repair",
			Status:  StatusOK,
			Message: "repair scan found no remaining legacy surfaces",
		})
	}

	readOnly := Run(ctx, Options{
		RepoPath:   repoPath,
		BaseBranch: baseBranch,
		BuildInfo:  build,
	}, deps)
	checks = append(checks, readOnly.Checks...)
	return WithMetadata(Report{
		Runtime:               readOnly.Runtime,
		HostProfile:           readOnly.HostProfile,
		ProviderCompatibility: readOnly.ProviderCompatibility,
		Checks:                checks,
	}, repoPath, build)
}

func fixDeliveryConfig(repoPath string, deps Deps) Check {
	path := filepath.Join(repoPath, ".delivery.yml")
	data, err := deps.ReadFile(path)
	if err != nil {
		if isNotExist(err) {
			return Check{Name: "fix .delivery.yml", Status: StatusOK, Message: "unchanged; .delivery.yml not present"}
		}
		return Check{Name: "fix .delivery.yml", Status: StatusFail, Message: fmt.Sprintf("could not read .delivery.yml: %v", err), Hard: true}
	}
	result, err := config.MigrateDeliveryYAML(data)
	if err != nil {
		return Check{Name: "fix .delivery.yml", Status: StatusFail, Message: err.Error(), Hard: true}
	}
	if !result.Changed {
		return Check{Name: "fix .delivery.yml", Status: StatusOK, Message: "unchanged; no legacy config keys found"}
	}
	if err := writeFileAtomic(path, result.Data, 0o644); err != nil {
		return Check{Name: "fix .delivery.yml", Status: StatusFail, Message: fmt.Sprintf("migration rendered but write failed: %v", err), Hard: true}
	}
	return Check{
		Name:    "fix .delivery.yml",
		Status:  StatusOK,
		Message: fmt.Sprintf("changed; migrated %d legacy config key diagnostic(s) to report keys", len(result.Diagnostics)),
	}
}

func fixConductorHookSettings(repoPath string, deps Deps) Check {
	path := claudehooks.SettingsPath(repoPath)
	data, err := deps.ReadFile(path)
	if err != nil && !isNotExist(err) {
		return Check{Name: "fix conductor hooks", Status: StatusFail, Message: fmt.Sprintf("could not read Claude Code settings: %v", err), Hard: true}
	}
	created := isNotExist(err)
	merged, changed, err := claudehooks.MergeSettings(data)
	if err != nil {
		return Check{Name: "fix conductor hooks", Status: StatusFail, Message: err.Error(), Hard: true}
	}
	if !changed && !created {
		return Check{Name: "fix conductor hooks", Status: StatusOK, Message: "unchanged; conductor hook settings already use current commands"}
	}
	if err := writeFileAtomic(path, merged, 0o644); err != nil {
		return Check{Name: "fix conductor hooks", Status: StatusFail, Message: fmt.Sprintf("could not write Claude Code settings: %v", err), Hard: true}
	}
	status := "changed"
	if created {
		status = "created"
	}
	return Check{Name: "fix conductor hooks", Status: StatusOK, Message: status + "; wrote current conductor hook commands"}
}

func fixConductorHookState(repoPath string) Check {
	oldPath := filepath.Join(repoPath, ".loopcoder", "hooks", migration.LegacyReporterHookName)
	newPath := filepath.Join(repoPath, ".loopcoder", "hooks", migration.ReporterHookName)
	return migrateHookStateDir(oldPath, newPath, "fix hook state")
}

func migrateHookStateDir(oldPath, newPath, name string) Check {
	oldInfo, oldErr := os.Lstat(oldPath)
	if oldErr != nil {
		if isNotExist(oldErr) {
			return Check{Name: name, Status: StatusOK, Message: "unchanged; legacy hook state not present"}
		}
		return Check{Name: name, Status: StatusFail, Message: fmt.Sprintf("could not inspect legacy hook state: %v", oldErr), Hard: true}
	}
	if oldInfo.Mode()&os.ModeSymlink != 0 || !oldInfo.IsDir() {
		return Check{Name: name, Status: StatusFail, Message: "legacy hook state is not a regular directory; refusing to migrate", Hard: true}
	}
	if newInfo, err := os.Lstat(newPath); err == nil {
		if newInfo.Mode()&os.ModeSymlink != 0 || !newInfo.IsDir() {
			return Check{Name: name, Status: StatusFail, Message: "current hook state path exists but is not a regular directory; refusing to merge", Hard: true}
		}
		moved, err := moveDirContents(oldPath, newPath)
		if err != nil {
			return Check{Name: name, Status: StatusFail, Message: fmt.Sprintf("could not merge legacy hook state: %v", err), Hard: true}
		}
		if err := os.Remove(oldPath); err != nil {
			return Check{Name: name, Status: StatusWarn, Message: fmt.Sprintf("changed; moved %d legacy state file(s), but could not remove old directory: %v", moved, err)}
		}
		return Check{Name: name, Status: StatusOK, Message: fmt.Sprintf("changed; moved %d legacy state file(s) into current hook state", moved)}
	} else if !isNotExist(err) {
		return Check{Name: name, Status: StatusFail, Message: fmt.Sprintf("could not inspect current hook state: %v", err), Hard: true}
	}
	if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
		return Check{Name: name, Status: StatusFail, Message: fmt.Sprintf("could not create hook state parent: %v", err), Hard: true}
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		return Check{Name: name, Status: StatusFail, Message: fmt.Sprintf("could not move legacy hook state: %v", err), Hard: true}
	}
	return Check{Name: name, Status: StatusOK, Message: "changed; moved legacy hook state to current label"}
}

func moveDirContents(oldPath, newPath string) (int, error) {
	entries, err := os.ReadDir(oldPath)
	if err != nil {
		return 0, err
	}
	moved := 0
	for _, entry := range entries {
		source := filepath.Join(oldPath, entry.Name())
		target := filepath.Join(newPath, entry.Name())
		if entry.Type()&os.ModeSymlink != 0 {
			return moved, fmt.Errorf("refusing to move symlink %s", source)
		}
		if _, err := os.Lstat(target); err == nil {
			continue
		} else if !isNotExist(err) {
			return moved, err
		}
		if err := os.Rename(source, target); err != nil {
			return moved, err
		}
		moved++
	}
	return moved, nil
}

func fixLegacyStateKeys(repoPath string) Check {
	roots := []string{
		filepath.Join(repoPath, ".loopcoder", "runs"),
		filepath.Join(repoPath, ".loopcoder", "relay", "pending"),
	}
	changed := 0
	var diagnostics []string
	for _, root := range roots {
		n, err := rewriteLegacyStateKeys(root)
		changed += n
		if err != nil {
			diagnostics = append(diagnostics, err.Error())
		}
	}
	if len(diagnostics) > 0 {
		return Check{Name: "fix state keys", Status: StatusWarn, Message: fmt.Sprintf("changed %d file(s); skipped some state: %s", changed, strings.Join(diagnostics, "; "))}
	}
	if changed == 0 {
		return Check{Name: "fix state keys", Status: StatusOK, Message: "unchanged; no legacy report state keys found"}
	}
	return Check{Name: "fix state keys", Status: StatusOK, Message: fmt.Sprintf("changed; rewrote %d local state file(s) from legacy report key to current key", changed)}
}

func rewriteLegacyStateKeys(root string) (int, error) {
	if !pathExists(root) {
		return 0, nil
	}
	changed := 0
	var skipped []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			skipped = append(skipped, filepath.ToSlash(path)+": "+walkErr.Error())
			return nil
		}
		if path == root {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		didChange, err := rewriteLegacyStateKeyFile(path)
		if err != nil {
			skipped = append(skipped, filepath.ToSlash(path)+": "+err.Error())
			return nil
		}
		if didChange {
			changed++
		}
		return nil
	})
	if err != nil {
		skipped = append(skipped, err.Error())
	}
	if len(skipped) > 0 {
		return changed, errors.New(strings.Join(skipped, "; "))
	}
	return changed, nil
}

func rewriteLegacyStateKeyFile(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	if !bytes.Contains(data, []byte(`"`+migration.LegacyReportStateKey+`"`)) {
		return false, nil
	}
	var out []byte
	if strings.HasSuffix(path, ".jsonl") {
		lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
		changed := false
		rendered := make([]string, len(lines))
		for i, line := range lines {
			if strings.TrimSpace(line) == "" {
				rendered[i] = line
				continue
			}
			lineData, didChange, err := rewriteLegacyStateKeyJSON([]byte(line), true)
			if err != nil {
				return false, err
			}
			changed = changed || didChange
			rendered[i] = strings.TrimSuffix(string(lineData), "\n")
		}
		if !changed {
			return false, nil
		}
		out = []byte(strings.Join(rendered, "\n"))
	} else {
		var changed bool
		out, changed, err = rewriteLegacyStateKeyJSON(data, false)
		if err != nil || !changed {
			return false, err
		}
	}
	return true, writeFileAtomic(path, out, 0o644)
}

func rewriteLegacyStateKeyJSON(data []byte, compact bool) ([]byte, bool, error) {
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, false, err
	}
	legacy, ok := object[migration.LegacyReportStateKey]
	if !ok {
		return data, false, nil
	}
	if _, hasCurrent := object[migration.ReportStateKey]; !hasCurrent {
		object[migration.ReportStateKey] = legacy
	}
	delete(object, migration.LegacyReportStateKey)
	var out []byte
	var err error
	if compact {
		out, err = json.Marshal(object)
	} else {
		out, err = json.MarshalIndent(object, "", "  ")
	}
	if err != nil {
		return nil, false, err
	}
	out = append(out, '\n')
	return out, true, nil
}

func fixStaleState(repoPath string) Check {
	result, err := localcleanup.Cleanup(localcleanup.Options{
		RepoPath: repoPath,
		Apply:    true,
	})
	if err != nil {
		return Check{Name: "fix stale local state", Status: StatusWarn, Message: fmt.Sprintf("could not apply stale cleanup: %v", err)}
	}
	if len(result.Planned) == 0 {
		return Check{Name: "fix stale local state", Status: StatusOK, Message: "unchanged; no cleanup-eligible items found"}
	}
	message := fmt.Sprintf("changed; removed %d of %d cleanup-eligible item(s)", len(result.Removed), len(result.Planned))
	if len(result.Diagnostics) > 0 {
		return Check{Name: "fix stale local state", Status: StatusWarn, Message: message + "; diagnostics: " + strings.Join(result.Diagnostics, "; ")}
	}
	return Check{Name: "fix stale local state", Status: StatusOK, Message: message}
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
	if deps.Getenv == nil {
		deps.Getenv = defaults.Getenv
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
	if deps.CleanupPlan == nil {
		deps.CleanupPlan = defaults.CleanupPlan
	}
	if deps.StorageHealth == nil {
		deps.StorageHealth = defaults.StorageHealth
	}
	if deps.StoragePermissions == nil {
		deps.StoragePermissions = defaults.StoragePermissions
	}
	if deps.ProjectShow == nil {
		deps.ProjectShow = defaults.ProjectShow
	}
	if deps.ProjectDuplicates == nil {
		deps.ProjectDuplicates = defaults.ProjectDuplicates
	}
	if deps.ProjectRepair == nil {
		deps.ProjectRepair = defaults.ProjectRepair
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

type depsGitRunner struct {
	deps Deps
}

func (r depsGitRunner) RunGit(ctx context.Context, repoPath string, args ...string) ([]byte, error) {
	result, err := r.deps.RunCommand(ctx, repoPath, "git", args...)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		detail := commandDetail(result)
		if detail == "" {
			detail = fmt.Sprintf("exit code %d", result.ExitCode)
		}
		return nil, errors.New(detail)
	}
	return []byte(result.Stdout), nil
}

func checkLocalStateExclude(ctx context.Context, repoPath string, gitPresent bool, deps Deps) Check {
	const fixCommand = "loopcoder skill install --repo ."
	if !gitPresent {
		return Check{
			Name:    "local-state exclude",
			Status:  StatusWarn,
			Message: "cannot verify .loopcoder/ exclude protection because git is missing",
		}
	}
	excludePath, err := gitlocal.ResolveExcludePath(ctx, repoPath, depsGitRunner{deps: deps})
	if err != nil {
		return Check{
			Name:    "local-state exclude",
			Status:  StatusWarn,
			Message: fmt.Sprintf("could not resolve .git/info/exclude for .loopcoder/ protection: %v", err),
		}
	}
	data, err := deps.ReadFile(excludePath)
	if err != nil {
		if isNotExist(err) {
			return Check{
				Name:       "local-state exclude",
				Status:     StatusWarn,
				Message:    fmt.Sprintf(".loopcoder/ is not protected because %s does not exist; run: %s", excludePath, fixCommand),
				FixCommand: fixCommand,
			}
		}
		return Check{
			Name:    "local-state exclude",
			Status:  StatusWarn,
			Message: fmt.Sprintf("could not read %s for .loopcoder/ protection: %v", excludePath, err),
		}
	}
	if !gitlocal.ExcludesLoopcoderState(data) {
		return Check{
			Name:       "local-state exclude",
			Status:     StatusWarn,
			Message:    fmt.Sprintf(".loopcoder/ is not protected by %s; run: %s", excludePath, fixCommand),
			FixCommand: fixCommand,
		}
	}
	return Check{
		Name:    "local-state exclude",
		Status:  StatusOK,
		Message: fmt.Sprintf(".loopcoder/ is protected by %s", excludePath),
	}
}

func checkTrackedLoopcoderState(ctx context.Context, repoPath string, gitPresent bool, deps Deps) Check {
	const fixCommand = "git rm -r --cached .loopcoder && echo .loopcoder/ >> .git/info/exclude"
	if !gitPresent {
		return Check{
			Name:    "tracked .loopcoder",
			Status:  StatusWarn,
			Message: "cannot inspect tracked .loopcoder files because git is missing",
		}
	}
	result, err := deps.RunCommand(ctx, repoPath, "git", "ls-files", ".loopcoder")
	if err != nil {
		return Check{
			Name:    "tracked .loopcoder",
			Status:  StatusWarn,
			Message: fmt.Sprintf("could not inspect tracked .loopcoder files: %v", err),
		}
	}
	if result.ExitCode != 0 {
		detail := commandDetail(result)
		if detail == "" {
			detail = fmt.Sprintf("exit code %d", result.ExitCode)
		}
		return Check{
			Name:    "tracked .loopcoder",
			Status:  StatusWarn,
			Message: fmt.Sprintf("could not inspect tracked .loopcoder files: %s", detail),
		}
	}
	tracked := nonEmptyLines(result.Stdout)
	if len(tracked) > 0 {
		return Check{
			Name:       "tracked .loopcoder",
			Status:     StatusFail,
			Message:    fmt.Sprintf("found %d tracked .loopcoder file(s); run: %s", len(tracked), fixCommand),
			Hard:       true,
			FixCommand: fixCommand,
		}
	}
	return Check{
		Name:    "tracked .loopcoder",
		Status:  StatusOK,
		Message: "no tracked .loopcoder files found",
	}
}

func checkReportQuery(repoPath string) Check {
	records, err := reportquery.List(reportquery.Options{RepoPath: repoPath, Limit: 1})
	if err != nil {
		return Check{
			Name:    "report query",
			Status:  StatusWarn,
			Message: fmt.Sprintf("could not read local report records: %v", err),
		}
	}
	return Check{
		Name:    "report query",
		Status:  StatusOK,
		Message: fmt.Sprintf("local report records are readable (%d diagnostic sample record(s))", len(records)),
	}
}

func checkStoragePermissions(deps Deps) Check {
	deps = normalizeDeps(deps)
	path, err := resolvedStoragePath(deps)
	if err != nil {
		return Check{
			Name:    "storage permissions",
			Status:  StatusWarn,
			Message: fmt.Sprintf("could not resolve loopcoder home for storage permissions: %v", err),
		}
	}
	report, err := deps.StoragePermissions(path, false)
	if err != nil {
		return Check{
			Name:    "storage permissions",
			Status:  StatusFail,
			Message: fmt.Sprintf("path=%s permissions=fail: %v", path, err),
			Hard:    true,
		}
	}
	if !report.Supported {
		return Check{
			Name:       "storage permissions",
			Status:     StatusWarn,
			Message:    fmt.Sprintf("path=%s permissions=unsupported platform=%s: %s", path, report.Platform, firstNonEmpty(report.Message, "owner-only ACL hardening is not implemented")),
			FixCommand: "loopcoder doctor --repo . --fix",
		}
	}
	if report.Secure {
		return Check{
			Name:    "storage permissions",
			Status:  StatusOK,
			Message: fmt.Sprintf("path=%s permissions=owner-only: %s", path, firstNonEmpty(report.Message, "ok")),
		}
	}
	return Check{
		Name:       "storage permissions",
		Status:     StatusWarn,
		Message:    fmt.Sprintf("path=%s permissions=insecure: %s", path, storagePermissionDetails(report)),
		FixCommand: "loopcoder doctor --repo . --fix",
	}
}

func checkStorageHealth(ctx context.Context, deps Deps) Check {
	deps = normalizeDeps(deps)
	path, err := resolvedStoragePath(deps)
	if err != nil {
		return Check{
			Name:    "storage",
			Status:  StatusWarn,
			Message: fmt.Sprintf("could not resolve loopcoder home for storage health: %v", err),
		}
	}
	health, err := deps.StorageHealth(ctx, path)
	if err != nil {
		return Check{
			Name:    "storage",
			Status:  StatusFail,
			Message: fmt.Sprintf("path=%s health=fail: %v", path, err),
		}
	}
	if !health.Exists {
		return Check{
			Name:    "storage",
			Status:  StatusInfo,
			Message: fmt.Sprintf("path=%s schema_version=0 health=not-created", path),
		}
	}
	if !health.OK {
		message := strings.TrimSpace(health.Message)
		if message == "" {
			message = "unhealthy"
		}
		return Check{
			Name:    "storage",
			Status:  StatusFail,
			Message: fmt.Sprintf("path=%s schema_version=%d health=%s", path, health.SchemaVersion, message),
		}
	}
	return Check{
		Name:    "storage",
		Status:  StatusOK,
		Message: fmt.Sprintf("path=%s schema_version=%d health=ok", path, health.SchemaVersion),
	}
}

func fixStoragePermissions(deps Deps) Check {
	deps = normalizeDeps(deps)
	path, err := resolvedStoragePath(deps)
	if err != nil {
		return Check{
			Name:    "fix storage permissions",
			Status:  StatusWarn,
			Message: fmt.Sprintf("could not resolve loopcoder home for storage permissions: %v", err),
		}
	}
	before, beforeErr := deps.StoragePermissions(path, false)
	if beforeErr != nil {
		return Check{
			Name:    "fix storage permissions",
			Status:  StatusFail,
			Message: fmt.Sprintf("path=%s before=unreadable: %v", path, beforeErr),
			Hard:    true,
		}
	}
	after, err := deps.StoragePermissions(path, true)
	if err != nil {
		return Check{
			Name:    "fix storage permissions",
			Status:  StatusFail,
			Message: fmt.Sprintf("path=%s repair=failed: %v", path, err),
			Hard:    true,
		}
	}
	if !after.Supported {
		return Check{
			Name:    "fix storage permissions",
			Status:  StatusWarn,
			Message: fmt.Sprintf("path=%s unchanged: %s", path, firstNonEmpty(after.Message, "owner-only ACL hardening is not implemented on this platform")),
		}
	}
	if !after.Secure {
		return Check{
			Name:    "fix storage permissions",
			Status:  StatusWarn,
			Message: fmt.Sprintf("path=%s repair incomplete; before=%s after=%s", path, storagePermissionDetails(before), storagePermissionDetails(after)),
		}
	}
	if after.Repaired {
		return Check{
			Name:    "fix storage permissions",
			Status:  StatusOK,
			Message: fmt.Sprintf("path=%s changed; before=%s after=%s", path, storagePermissionDetails(before), storagePermissionDetails(after)),
		}
	}
	return Check{
		Name:    "fix storage permissions",
		Status:  StatusOK,
		Message: fmt.Sprintf("path=%s unchanged; %s", path, firstNonEmpty(after.Message, "storage permissions are owner-only")),
	}
}

func fixProjectRegistryDuplicates(ctx context.Context, repoPath string, deps Deps) Check {
	deps = normalizeDeps(deps)
	path, err := resolvedStoragePath(deps)
	if err != nil {
		return Check{Name: "fix project registry duplicates", Status: StatusWarn, Message: fmt.Sprintf("could not resolve storage path: %v", err)}
	}
	health, err := deps.StorageHealth(ctx, path)
	if err != nil || !health.Exists || !health.OK {
		return Check{Name: "fix project registry duplicates", Status: StatusInfo, Message: "skipped; healthy project registry storage is not available"}
	}
	repaired, err := deps.ProjectRepair(ctx, registry.Options{RepoPath: repoPath, DatabasePath: path})
	if err != nil {
		return Check{Name: "fix project registry duplicates", Status: StatusWarn, Message: fmt.Sprintf("could not repair duplicate physical project identities: %v", err)}
	}
	if len(repaired) == 0 {
		return Check{Name: "fix project registry duplicates", Status: StatusOK, Message: "no duplicate physical project identities found"}
	}
	return Check{Name: "fix project registry duplicates", Status: StatusOK, Message: fmt.Sprintf("reconciled %d duplicate physical project identity group(s)", len(repaired))}
}

func checkProjectRegistry(ctx context.Context, repoPath string, deps Deps) Check {
	deps = normalizeDeps(deps)
	layout, err := home.Resolve(home.Deps{
		Getenv:      deps.Getenv,
		UserHomeDir: deps.UserHomeDir,
	})
	if err != nil {
		return Check{
			Name:    "project registry",
			Status:  StatusWarn,
			Message: fmt.Sprintf("could not resolve loopcoder home for project registry: %v", err),
		}
	}
	path := layout.DatabasePath()
	health, err := deps.StorageHealth(ctx, path)
	if err != nil {
		return Check{
			Name:    "project registry",
			Status:  StatusWarn,
			Message: fmt.Sprintf("could not inspect project registry because storage health failed: %v", err),
		}
	}
	if !health.Exists {
		return Check{
			Name:    "project registry",
			Status:  StatusInfo,
			Message: "project is not registered; global registry database has not been created",
		}
	}
	if !health.OK {
		return Check{
			Name:    "project registry",
			Status:  StatusWarn,
			Message: fmt.Sprintf("could not inspect project registry because storage is unhealthy: %s", firstNonEmpty(health.Message, "unhealthy")),
		}
	}
	duplicates, err := deps.ProjectDuplicates(ctx, registry.Options{RepoPath: repoPath, DatabasePath: path})
	if err != nil {
		return Check{
			Name:    "project registry",
			Status:  StatusWarn,
			Message: fmt.Sprintf("could not inspect duplicate physical project identities: %v", err),
		}
	}
	if len(duplicates) > 0 {
		return Check{
			Name:    "project registry",
			Status:  StatusWarn,
			Message: fmt.Sprintf("found %d duplicate physical project identity group(s); run: loopcoder doctor --repo . --fix", len(duplicates)),
		}
	}
	result, err := deps.ProjectShow(ctx, registry.Options{RepoPath: repoPath, DatabasePath: path})
	if err != nil {
		return Check{
			Name:    "project registry",
			Status:  StatusWarn,
			Message: fmt.Sprintf("could not inspect project registry: %v", err),
		}
	}
	if len(result.Conflicts) > 0 {
		ids := make([]string, 0, len(result.Conflicts))
		for _, conflict := range result.Conflicts {
			ids = append(ids, conflict.ProjectID)
		}
		sort.Strings(ids)
		return Check{
			Name:    "project registry",
			Status:  StatusWarn,
			Message: fmt.Sprintf("project identity is ambiguous; local path also matches registered project(s): %s; run: loopcoder projects show --repo .", strings.Join(ids, ", ")),
		}
	}
	if !result.Registered {
		if result.Detached {
			return Check{
				Name:    "project registry",
				Status:  StatusInfo,
				Message: fmt.Sprintf("project is detached; preserved project_id=%s identity=%s; run: loopcoder projects register --repo . to reactivate", result.Project.ProjectID, result.Project.IdentitySource),
			}
		}
		return Check{
			Name:    "project registry",
			Status:  StatusInfo,
			Message: fmt.Sprintf("project is not registered; fallback=unregistered-repo-local candidate=%s identity=%s; run: loopcoder projects register --repo .", result.Project.ProjectID, result.Project.IdentitySource),
		}
	}
	roots, _ := runtimepath.Resolve(ctx, repoPath)
	return Check{
		Name:    "project registry",
		Status:  StatusOK,
		Message: fmt.Sprintf("registered project_id=%s identity=%s payload_root=%s fallback=%s path=%s", result.Project.ProjectID, result.Project.IdentitySource, filepath.ToSlash(roots.ProjectRoot), roots.FallbackMode, result.Project.LocalPath),
	}
}

func runtimeHealth(ctx context.Context, repoPath string, deps Deps) RuntimeHealth {
	deps = normalizeDeps(deps)
	runtime := RuntimeHealth{
		Migration:  runtimeMigration(repoPath, deps),
		NestedRuns: runtimeNestedRuns(repoPath),
	}
	layout, err := home.Resolve(home.Deps{
		Getenv:      deps.Getenv,
		UserHomeDir: deps.UserHomeDir,
	})
	if err != nil {
		runtime.Database = RuntimeDatabase{
			Status:  StatusWarn,
			Message: fmt.Sprintf("could not resolve loopcoder home: %v", err),
		}
		runtime.ProjectRegistry = RuntimeProjectRegistry{
			Status:  StatusWarn,
			Message: fmt.Sprintf("could not resolve loopcoder home: %v", err),
		}
		return runtime
	}
	runtime.HomeDir = layout.HomeDir
	runtime.Database = runtimeDatabase(ctx, layout.DatabasePath(), deps)
	runtime.ProjectRegistry = runtimeProjectRegistry(ctx, repoPath, layout.DatabasePath(), runtime.Database, deps)
	return runtime
}

func resolvedStoragePath(deps Deps) (string, error) {
	layout, err := home.Resolve(home.Deps{
		Getenv:      deps.Getenv,
		UserHomeDir: deps.UserHomeDir,
	})
	if err != nil {
		return "", err
	}
	return layout.DatabasePath(), nil
}

func storagePermissionDetails(report storage.PermissionReport) string {
	var details []string
	for _, item := range report.Items {
		if !item.Exists || item.Secure && !item.Repaired {
			continue
		}
		switch {
		case item.Repaired:
			details = append(details, fmt.Sprintf("%s %s %04o->%04o", item.Kind, item.Path, item.BeforeMode, item.AfterMode))
		case item.Unsafe:
			details = append(details, fmt.Sprintf("%s %s unsafe=%s", item.Kind, item.Path, item.Message))
		default:
			details = append(details, fmt.Sprintf("%s %s %s", item.Kind, item.Path, item.Message))
		}
	}
	if len(details) == 0 {
		return firstNonEmpty(report.Message, "none")
	}
	return strings.Join(details, "; ")
}

func runtimeDatabase(ctx context.Context, path string, deps Deps) RuntimeDatabase {
	health, err := deps.StorageHealth(ctx, path)
	if err != nil {
		return RuntimeDatabase{
			Path:          path,
			Exists:        health.Exists,
			SchemaVersion: health.SchemaVersion,
			Status:        StatusFail,
			Message:       err.Error(),
		}
	}
	if !health.Exists {
		return RuntimeDatabase{
			Path:    path,
			Status:  StatusInfo,
			Message: firstNonEmpty(health.Message, "database has not been created"),
		}
	}
	if !health.OK {
		return RuntimeDatabase{
			Path:          path,
			Exists:        true,
			SchemaVersion: health.SchemaVersion,
			Status:        StatusFail,
			Message:       firstNonEmpty(health.Message, "unhealthy"),
		}
	}
	return RuntimeDatabase{
		Path:          path,
		Exists:        true,
		SchemaVersion: health.SchemaVersion,
		Status:        StatusOK,
		Message:       firstNonEmpty(health.Message, "storage database is healthy"),
	}
}

func runtimeProjectRegistry(ctx context.Context, repoPath, dbPath string, database RuntimeDatabase, deps Deps) RuntimeProjectRegistry {
	if database.Status == StatusInfo && !database.Exists {
		return RuntimeProjectRegistry{
			Status:  StatusInfo,
			Message: "global registry database has not been created",
		}
	}
	if database.Status == StatusFail {
		return RuntimeProjectRegistry{
			Status:  StatusWarn,
			Message: "storage is unhealthy; registry could not be inspected",
		}
	}
	duplicates, err := deps.ProjectDuplicates(ctx, registry.Options{RepoPath: repoPath, DatabasePath: dbPath})
	if err != nil {
		return RuntimeProjectRegistry{
			Status:  StatusWarn,
			Message: err.Error(),
		}
	}
	if len(duplicates) > 0 {
		return RuntimeProjectRegistry{
			Status:        StatusWarn,
			ConflictCount: len(duplicates),
			Message:       "duplicate physical project identities found",
		}
	}
	result, err := deps.ProjectShow(ctx, registry.Options{RepoPath: repoPath, DatabasePath: dbPath})
	if err != nil {
		return RuntimeProjectRegistry{
			Status:  StatusWarn,
			Message: err.Error(),
		}
	}
	registry := RuntimeProjectRegistry{
		Registered:     result.Registered,
		Detached:       result.Detached,
		ProjectID:      result.Project.ProjectID,
		IdentitySource: string(result.Project.IdentitySource),
		ConflictCount:  len(result.Conflicts),
	}
	if roots, err := runtimepath.Resolve(ctx, repoPath); err == nil {
		registry.PayloadRoot = roots.ProjectRoot
		registry.RunsRoot = roots.RunsRoot
		registry.RelayRoot = roots.RelayRoot
		registry.RecoveryRoot = roots.RecoveryRoot
		registry.AuditRoot = roots.AuditRoot
		registry.LogsRoot = roots.LogsRoot
		registry.TmpRoot = roots.TmpRoot
		registry.FallbackMode = roots.FallbackMode
	}
	switch {
	case len(result.Conflicts) > 0:
		registry.Status = StatusWarn
		registry.Message = "project identity is ambiguous"
	case result.Detached:
		registry.Status = StatusInfo
		registry.Message = "project registry identity is detached"
	case !result.Registered:
		registry.Status = StatusInfo
		registry.Message = "project is not registered; runtime uses explicit repo-local fallback"
	default:
		registry.Status = StatusOK
		registry.Message = "project registry identity is registered; runtime payloads use global project root"
	}
	return registry
}

func runtimeMigration(repoPath string, deps Deps) RuntimeMigration {
	status := scanMigrationStatus(repoPath, deps)
	legacyCount := migrationLegacyCount(status)
	if legacyCount > 0 {
		return RuntimeMigration{
			Status:         StatusWarn,
			LegacySurfaces: legacyCount,
			Message:        "legacy surfaces require explicit doctor --fix migration",
		}
	}
	if strings.TrimSpace(status.ScanWarning) != "" {
		return RuntimeMigration{
			Status:  StatusWarn,
			Message: status.ScanWarning,
		}
	}
	return RuntimeMigration{
		Status:  StatusOK,
		Message: "no legacy surfaces found",
	}
}

func checkNestedRunHealth(repoPath string) Check {
	health := runtimeNestedRuns(repoPath)
	return Check{
		Name:    "nested runs",
		Status:  health.Status,
		Message: health.Message,
	}
}

func runtimeNestedRuns(repoPath string) RuntimeNestedRuns {
	var entries []os.DirEntry
	foundRoot := ""
	for _, runsRoot := range state.RunsRootsForRead(repoPath) {
		readEntries, err := os.ReadDir(runsRoot)
		if err != nil {
			if isNotExist(err) {
				continue
			}
			return RuntimeNestedRuns{
				Status:       StatusWarn,
				ProblemCount: 1,
				Message:      fmt.Sprintf("could not inspect run tree state: %v", err),
			}
		}
		entries = readEntries
		foundRoot = runsRoot
		if len(entries) > 0 {
			break
		}
	}
	if foundRoot == "" {
		return RuntimeNestedRuns{
			Status:  StatusOK,
			Message: "no runtime run tree state found",
		}
	}
	if len(entries) > lcdefaults.RunStatusMaxDirectoryEntries {
		return RuntimeNestedRuns{
			Status:       StatusWarn,
			ProblemCount: 1,
			Message:      fmt.Sprintf("run directory entry limit exceeded (%d > %d)", len(entries), lcdefaults.RunStatusMaxDirectoryEntries),
		}
	}

	runIDs := map[string]bool{}
	lifecycles := map[string]state.Lifecycle{}
	var problems []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		runID := strings.TrimSpace(entry.Name())
		if !state.IsRunID(runID) {
			problems = append(problems, fmt.Sprintf("%s is not a valid run id", runID))
			continue
		}
		runIDs[runID] = true
		lifecycle, err := state.LoadLifecycle(repoPath, runID)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s lifecycle unreadable: %v", runID, err))
			continue
		}
		lifecycles[runID] = lifecycle
	}

	parentEdges := 0
	childEdges := 0
	for runID, lifecycle := range lifecycles {
		eventChildren, eventParent := doctorRunEdgesFromEvents(repoPath, runID)
		parent := strings.TrimSpace(firstNonEmptyDoctor(lifecycle.ParentRunID, eventParent))
		if parent != "" {
			parentEdges++
			if parent == runID {
				problems = append(problems, fmt.Sprintf("%s references itself as parent", runID))
			} else if !runIDs[parent] {
				problems = append(problems, fmt.Sprintf("%s references missing parent %s", runID, parent))
			}
		}
		children := map[string]bool{}
		for _, child := range lifecycle.ChildRunIDs {
			children[strings.TrimSpace(child)] = true
		}
		for _, child := range eventChildren {
			children[strings.TrimSpace(child)] = true
		}
		delete(children, "")
		for child := range children {
			child = strings.TrimSpace(child)
			if child == "" {
				continue
			}
			childEdges++
			if child == runID {
				problems = append(problems, fmt.Sprintf("%s references itself as child", runID))
			} else if !runIDs[child] {
				problems = append(problems, fmt.Sprintf("%s references missing child %s", runID, child))
			}
		}
	}

	runCount := len(runIDs)
	if len(problems) > 0 {
		detail := strings.Join(problems, "; ")
		if len(problems) > 3 {
			detail = strings.Join(problems[:3], "; ") + fmt.Sprintf("; plus %d more", len(problems)-3)
		}
		return RuntimeNestedRuns{
			Status:       StatusWarn,
			RunCount:     runCount,
			ParentEdges:  parentEdges,
			ChildEdges:   childEdges,
			ProblemCount: len(problems),
			Message:      fmt.Sprintf("run tree has %d problem(s): %s; inspect with: loopcoder status --repo . --format json", len(problems), detail),
		}
	}
	if parentEdges == 0 && childEdges == 0 {
		return RuntimeNestedRuns{
			Status:      StatusOK,
			RunCount:    runCount,
			ParentEdges: parentEdges,
			ChildEdges:  childEdges,
			Message:     fmt.Sprintf("run tree readable; %d run(s), no nested edges", runCount),
		}
	}
	return RuntimeNestedRuns{
		Status:      StatusOK,
		RunCount:    runCount,
		ParentEdges: parentEdges,
		ChildEdges:  childEdges,
		Message:     fmt.Sprintf("run tree readable; %d run(s), %d parent edge(s), %d child edge(s)", runCount, parentEdges, childEdges),
	}
}

func doctorRunEdgesFromEvents(repoPath, runID string) ([]string, string) {
	path := filepath.Join(state.RunPathForRead(repoPath, runID), "events.jsonl")
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() > lcdefaults.RunStatusMaxEventBytes {
		return nil, ""
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, ""
	}
	defer file.Close()

	children := map[string]bool{}
	parent := ""
	limited := &io.LimitedReader{R: file, N: lcdefaults.RunStatusMaxEventBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 1024), lcdefaults.RunStatusMaxEventLineBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event struct {
			Details json.RawMessage `json:"details"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil || len(event.Details) == 0 {
			continue
		}
		var details struct {
			ParentRunID string `json:"parent_run_id"`
			Child       struct {
				RunID string `json:"run_id"`
			} `json:"child"`
			Result struct {
				RunID string `json:"run_id"`
			} `json:"result"`
		}
		if err := json.Unmarshal(event.Details, &details); err != nil {
			continue
		}
		if strings.TrimSpace(details.ParentRunID) != "" && details.ParentRunID != runID {
			parent = strings.TrimSpace(details.ParentRunID)
		}
		if strings.TrimSpace(details.ParentRunID) == "" || strings.TrimSpace(details.ParentRunID) == runID {
			children[strings.TrimSpace(details.Child.RunID)] = true
			children[strings.TrimSpace(details.Result.RunID)] = true
			delete(children, "")
		}
	}
	out := make([]string, 0, len(children))
	for child := range children {
		out = append(out, child)
	}
	sort.Strings(out)
	return out, parent
}

func firstNonEmptyDoctor(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func checkLocalStateImport(ctx context.Context, repoPath string, deps Deps) Check {
	deps = normalizeDeps(deps)
	if !legacyLocalStatePresent(repoPath) {
		return Check{
			Name:    "local state import",
			Status:  StatusOK,
			Message: "no repo-local .loopcoder history found for storage import",
		}
	}
	layout, err := home.Resolve(home.Deps{
		Getenv:      deps.Getenv,
		UserHomeDir: deps.UserHomeDir,
	})
	if err != nil {
		return Check{
			Name:    "local state import",
			Status:  StatusWarn,
			Message: fmt.Sprintf("repo-local .loopcoder history exists, but loopcoder home could not be resolved: %v; run: loopcoder migrate local-state --repo .", err),
		}
	}
	path := layout.DatabasePath()
	health, err := deps.StorageHealth(ctx, path)
	if err != nil || !health.Exists || !health.OK {
		return Check{
			Name:    "local state import",
			Status:  StatusWarn,
			Message: "repo-local .loopcoder history exists and has not been imported into healthy storage; run: loopcoder migrate local-state --repo .",
		}
	}
	result, err := deps.ProjectShow(ctx, registry.Options{RepoPath: repoPath, DatabasePath: path})
	if err != nil {
		return Check{
			Name:    "local state import",
			Status:  StatusWarn,
			Message: "repo-local .loopcoder history exists but this project has no import status; run: loopcoder migrate local-state --repo .",
		}
	}
	if result.Detached {
		return Check{
			Name:    "local state import",
			Status:  StatusInfo,
			Message: fmt.Sprintf("repo-local .loopcoder history import status is preserved for detached project_id=%s; run: loopcoder projects register --repo . to reactivate", result.Project.ProjectID),
		}
	}
	if !result.Registered {
		return Check{
			Name:    "local state import",
			Status:  StatusWarn,
			Message: "repo-local .loopcoder history exists but this project has no import status; run: loopcoder migrate local-state --repo .",
		}
	}
	status, malformed, err := readLocalStateImportStatus(ctx, path, result.Project.ProjectID)
	if err != nil {
		return Check{
			Name:    "local state import",
			Status:  StatusWarn,
			Message: fmt.Sprintf("could not inspect local state import status: %v; run: loopcoder migrate local-state --repo .", err),
		}
	}
	if strings.TrimSpace(status) == "" {
		return Check{
			Name:    "local state import",
			Status:  StatusWarn,
			Message: "repo-local .loopcoder history exists but no import status is recorded for this project; run: loopcoder migrate local-state --repo .",
		}
	}
	if malformed > 0 {
		return Check{
			Name:    "local state import",
			Status:  StatusWarn,
			Message: fmt.Sprintf("last local state import status=%s with %d malformed record(s); fix records or rerun: loopcoder migrate local-state --repo .", status, malformed),
		}
	}
	return Check{
		Name:    "local state import",
		Status:  StatusOK,
		Message: fmt.Sprintf("repo-local .loopcoder history import recorded for project_id=%s status=%s", result.Project.ProjectID, status),
	}
}

func legacyLocalStatePresent(repoPath string) bool {
	for _, rel := range []string{
		filepath.Join(".loopcoder", "runs"),
		filepath.Join(".loopcoder", "relay"),
	} {
		info, err := os.Stat(filepath.Join(repoPath, rel))
		if err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

func readLocalStateImportStatus(ctx context.Context, dbPath, projectID string) (string, int, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return "", 0, err
	}
	defer db.Close()
	var status string
	var malformed int
	err = db.QueryRowContext(ctx, `SELECT status, malformed_count FROM legacy_import_status WHERE project_id = ?`, projectID).Scan(&status, &malformed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	return status, malformed, nil
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

func resolveHostProfile(delivery deliveryState, deps Deps) (HostProfile, Check) {
	configProfile := ""
	if delivery.Valid || !delivery.Present {
		configProfile = delivery.Config.Host.Profile
	}
	resolved, err := hostprofile.Resolve(hostprofile.Options{
		Profile: configProfile,
		Getenv:  deps.Getenv,
	})
	if err != nil {
		selector := "host.profile"
		if strings.TrimSpace(deps.Getenv(hostprofile.EnvName)) != "" {
			selector = hostprofile.EnvName
		}
		return HostProfile{Source: "error", Selector: selector}, Check{
			Name:    "host profile",
			Status:  StatusFail,
			Message: err.Error(),
			Hard:    true,
		}
	}
	profile := renderHostProfile(resolved)
	status := StatusOK
	if resolved.Source == hostprofile.SourceFallback {
		status = StatusWarn
	}
	message := fmt.Sprintf("profile=%s source=%s", profile.Name, profile.Source)
	if profile.Selector != "" {
		message += " selector=" + profile.Selector
	}
	if profile.InvocationStyle != "" {
		message += "; " + profile.InvocationStyle
	}
	if len(profile.KnownLimitations) > 0 {
		message += "; limitations: " + strings.Join(profile.KnownLimitations, "; ")
	}
	return profile, Check{
		Name:    "host profile",
		Status:  status,
		Message: message,
	}
}

func renderHostProfile(resolved hostprofile.Resolved) HostProfile {
	return HostProfile{
		Name:               resolved.Name,
		Source:             string(resolved.Source),
		Selector:           resolved.Selector,
		InvocationStyle:    resolved.Runtime.InvocationStyle,
		SupportsHooks:      resolved.Runtime.SupportsHooks,
		SupportsJSONOutput: resolved.Runtime.SupportsJSONOutput,
		DetectedBy:         append([]string(nil), resolved.DetectedBy...),
		KnownLimitations:   append([]string(nil), resolved.Runtime.KnownLimitations...),
	}
}

func checkModelSelections(delivery deliveryState) Check {
	if !delivery.Valid && delivery.Present {
		return Check{
			Name:    "model selection",
			Status:  StatusWarn,
			Message: "cannot evaluate model/depth selections because .delivery.yml is invalid or unavailable",
		}
	}
	cfg := delivery.Config
	strict := cfg.Models.Strict
	results := []models.ValidationResult{
		models.ValidateSelection(models.Selection{
			Role:     "worker",
			Provider: firstNonEmpty(cfg.Adapters.Worker, "codex"),
			Model:    strings.TrimSpace(cfg.Worker.Model),
			Depth:    strings.TrimSpace(cfg.Worker.ReasoningEffort),
		}, models.ValidationOptions{Strict: strict}),
		models.ValidateSelection(models.Selection{
			Role:     "verifier",
			Provider: firstNonEmpty(cfg.Adapters.Verifier, "claude"),
			Model:    strings.TrimSpace(cfg.Verifier.Model),
			Depth:    strings.TrimSpace(cfg.Verifier.ReasoningEffort),
		}, models.ValidationOptions{Strict: strict}),
	}

	status := StatusOK
	hard := false
	messages := []string{}
	for _, result := range results {
		for _, diagnostic := range result.Diagnostics {
			messages = append(messages, diagnostic.Message)
			if diagnostic.Severity == models.SeverityReject {
				status = StatusFail
				hard = true
			} else if status != StatusFail {
				status = StatusWarn
			}
		}
	}
	if len(messages) > 0 {
		return Check{
			Name:    "model selection",
			Status:  status,
			Message: strings.Join(messages, "; "),
			Hard:    hard,
		}
	}
	summaries := make([]string, 0, len(results))
	for _, result := range results {
		summaries = append(summaries, formatModelSelection(result.Selection))
	}
	return Check{
		Name:    "model selection",
		Status:  StatusOK,
		Message: strings.Join(summaries, "; "),
	}
}

func formatModelSelection(selection models.Selection) string {
	model := firstNonEmpty(selection.Model, "(none)")
	depth := firstNonEmpty(selection.Depth, "(none)")
	return fmt.Sprintf("%s provider=%s model=%s depth=%s", selection.Role, selection.Provider, model, depth)
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

func checkProviders(ctx context.Context, deps Deps, providers []providerSpec) []Check {
	checks := make([]Check, 0, len(providers))
	for _, provider := range providers {
		roleText := strings.Join(provider.Roles, ", ")
		cliName := providerCLIName(provider.Name)
		path, err := deps.LookPath(cliName)
		if err != nil || strings.TrimSpace(path) == "" {
			status := StatusWarn
			hard := false
			fix := ""
			if provider.Name == "antigravity" {
				status = StatusFail
				hard = true
				fix = "; install Google Antigravity CLI and run: agy login"
			}
			checks = append(checks, Check{
				Name:    "provider " + provider.Name,
				Code:    "missing_executable",
				Status:  status,
				Message: fmt.Sprintf("configured for %s but CLI %q was not found on PATH%s", roleText, cliName, fix),
				Hard:    hard,
			})
			continue
		}
		if provider.Name == "antigravity" {
			checks = append(checks, checkAntigravityOAuth(ctx, deps, roleText, path))
			continue
		}
		checks = append(checks, Check{
			Name:    "provider " + provider.Name,
			Code:    "provider_ready",
			Status:  StatusOK,
			Message: fmt.Sprintf("configured for %s; CLI %q found at %s; authentication not checked by a stable cheap probe", roleText, cliName, path),
		})
	}
	return checks
}

func checkProviderCompatibility(cfg config.Config, host HostProfile) []Check {
	hostName := firstNonEmpty(host.Name, "generic-local")
	configured := configuredProviders(cfg)
	checks := make([]Check, 0, len(configured)*2)
	for _, spec := range configured {
		for _, role := range spec.Roles {
			compatRole := runtimecap.RoleWorker
			if role == "verifier" {
				compatRole = runtimecap.RoleVerifier
			}
			entry := provider.Check(runtimecap.DefaultContract(), spec.Name, hostName, compatRole)
			checks = append(checks, checkFromCompatibilityEntry(entry, true))
		}
	}

	worker := strings.TrimSpace(cfg.Adapters.Worker)
	if worker == "" {
		worker = "codex"
	}
	nested := provider.Check(runtimecap.DefaultContract(), worker, hostName, runtimecap.RoleNestedSubagents)
	nestedCheck := checkFromCompatibilityEntry(nested, false)
	nestedCheck.Name = "provider compatibility " + worker + " nested-subagents"
	checks = append(checks, nestedCheck)
	return checks
}

func checkFromCompatibilityEntry(entry runtimecap.CompatibilityEntry, selected bool) Check {
	status := compatibilityStatus(entry.Support)
	hard := selected && entry.Support == runtimecap.SupportUnsupported
	if !selected && entry.Support == runtimecap.SupportUnsupported {
		status = StatusInfo
	}
	missing := formatCapabilities(entry.MissingCapabilities)
	required := formatCapabilities(entry.RequiredCapabilities)
	parts := []string{
		fmt.Sprintf("provider=%s host=%s role=%s support=%s", entry.Provider, entry.Host, entry.Role, entry.Support),
	}
	if required != "" {
		parts = append(parts, "requires="+required)
	}
	if missing != "" {
		parts = append(parts, "missing="+missing)
	}
	if len(entry.KnownLimitations) > 0 {
		parts = append(parts, "limitations: "+strings.Join(entry.KnownLimitations, "; "))
	}
	return Check{
		Name:    fmt.Sprintf("provider compatibility %s %s", entry.Provider, entry.Role),
		Code:    entry.Code,
		Status:  status,
		Message: strings.Join(parts, "; "),
		Hard:    hard,
	}
}

func compatibilityStatus(support runtimecap.SupportLevel) Status {
	switch support {
	case runtimecap.SupportSupported:
		return StatusOK
	case runtimecap.SupportExperimental:
		return StatusWarn
	default:
		return StatusFail
	}
}

func formatCapabilities(capabilities []runtimecap.ProviderCapability) string {
	if len(capabilities) == 0 {
		return ""
	}
	out := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		out = append(out, string(capability))
	}
	return strings.Join(out, ",")
}

func renderProviderCompatibility(entries []runtimecap.CompatibilityEntry) []ProviderCompatibility {
	out := make([]ProviderCompatibility, 0, len(entries))
	for _, entry := range entries {
		out = append(out, ProviderCompatibility{
			Provider:             entry.Provider,
			Host:                 entry.Host,
			Role:                 string(entry.Role),
			Support:              string(entry.Support),
			Status:               compatibilityStatus(entry.Support),
			Code:                 entry.Code,
			RequiredCapabilities: formatCompatibilityCapabilities(entry.RequiredCapabilities),
			MissingCapabilities:  formatCompatibilityCapabilities(entry.MissingCapabilities),
			KnownLimitations:     append([]string(nil), entry.KnownLimitations...),
		})
	}
	return out
}

func formatCompatibilityCapabilities(capabilities []runtimecap.ProviderCapability) []string {
	if len(capabilities) == 0 {
		return nil
	}
	out := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		out = append(out, string(capability))
	}
	return out
}

func providerCLIName(provider string) string {
	provider = strings.TrimSpace(provider)
	if registered, ok := models.LookupProvider(provider); ok && strings.TrimSpace(registered.CLI) != "" {
		return strings.TrimSpace(registered.CLI)
	}
	return provider
}

func checkAntigravityOAuth(ctx context.Context, deps Deps, roleText, path string) Check {
	result, err := deps.RunCommand(ctx, "", "agy", "models")
	if err != nil {
		detail := commandDetail(result)
		if detail != "" {
			detail = ": " + detail
		}
		return Check{
			Name:    "provider antigravity",
			Code:    "unauthenticated_provider",
			Status:  StatusFail,
			Message: fmt.Sprintf("configured for %s; CLI \"agy\" found at %s but agy models could not run: %v%s; run: agy login", roleText, path, err, detail),
			Hard:    true,
		}
	}
	if result.ExitCode != 0 {
		detail := commandDetail(result)
		if detail == "" {
			detail = fmt.Sprintf("exit code %d", result.ExitCode)
		}
		return Check{
			Name:    "provider antigravity",
			Code:    "unauthenticated_provider",
			Status:  StatusFail,
			Message: fmt.Sprintf("configured for %s; CLI \"agy\" found at %s but agy models failed: %s; run: agy login", roleText, path, detail),
			Hard:    true,
		}
	}
	if antigravityAuthProbeLooksFailed(result.Stdout + "\n" + result.Stderr) {
		detail := commandDetail(result)
		return Check{
			Name:    "provider antigravity",
			Code:    "unauthenticated_provider",
			Status:  StatusFail,
			Message: fmt.Sprintf("configured for %s; CLI \"agy\" found at %s but agy models reported an authentication problem: %s; run: agy login", roleText, path, firstNonEmpty(detail, "authentication required")),
			Hard:    true,
		}
	}
	return Check{
		Name:    "provider antigravity",
		Code:    "provider_authenticated",
		Status:  StatusOK,
		Message: fmt.Sprintf("configured for %s; CLI \"agy\" found at %s; agy models OAuth probe succeeded", roleText, path),
	}
}

func antigravityAuthProbeLooksFailed(text string) bool {
	lower := strings.ToLower(text)
	authSignal := strings.Contains(lower, "oauth") ||
		strings.Contains(lower, "login") ||
		strings.Contains(lower, "auth")
	failureSignal := strings.Contains(lower, "error") ||
		strings.Contains(lower, "failed") ||
		strings.Contains(lower, "required") ||
		strings.Contains(lower, "unauthorized") ||
		strings.Contains(lower, "not logged")
	return authSignal && failureSignal
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

func checkVersionStatus(build BuildInfo, repoPath string, deps Deps) Check {
	const transitionTarget = "0.6.0"

	versionStatus := upgrade.ClassifyVersionStatus(build.Version, transitionTarget)
	migrationStatus := scanMigrationStatus(repoPath, deps)
	legacyCount := migrationLegacyCount(migrationStatus)

	status := StatusOK
	parts := []string{
		fmt.Sprintf("selected version=%s classification=%s", build.Version, versionStatus.CurrentClassification),
		fmt.Sprintf("upgrade target=%s classification=%s", transitionTarget, versionStatus.TargetClassification),
	}
	if strings.TrimSpace(migrationStatus.MinLoopcoderVersion) != "" {
		parts = append(parts, fmt.Sprintf(".delivery.yml min_loopcoder_version=%s", migrationStatus.MinLoopcoderVersion))
	}
	if versionStatus.CompatibilityAliasesActive {
		parts = append(parts, "0.6.0 compatibility aliases are active for the transition window")
	}
	if versionStatus.BreakingBoundary {
		status = StatusWarn
		parts = append(parts, "selected version is before the 0.6.0 breaking boundary; run: loopcoder upgrade --version 0.6.0")
	}
	if versionStatus.CurrentClassification == upgrade.VersionUnknown {
		status = StatusWarn
		parts = append(parts, "selected version cannot be classified for the 0.6.0 transition")
	}
	if legacyCount > 0 {
		status = StatusWarn
		parts = append(parts, fmt.Sprintf("migration scan found %d legacy surface(s); run: loopcoder doctor --repo . --fix", legacyCount))
	} else {
		parts = append(parts, "migration scan found no legacy surfaces")
	}
	if strings.TrimSpace(migrationStatus.ScanWarning) != "" {
		if status != StatusWarn {
			status = StatusWarn
		}
		parts = append(parts, migrationStatus.ScanWarning)
	}
	return Check{
		Name:    "version status",
		Status:  status,
		Message: strings.Join(parts, "; "),
	}
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
	cliName := providerCLIName(provider)
	path, err := deps.LookPath(cliName)
	if err != nil || strings.TrimSpace(path) == "" {
		return Check{
			Name:    "audit llm provider",
			Status:  StatusWarn,
			Message: fmt.Sprintf("Layer 2 verifier provider %q CLI %q could not be resolved on PATH", provider, cliName),
		}
	}
	return Check{
		Name:    "audit llm provider",
		Status:  StatusOK,
		Message: fmt.Sprintf("Layer 2 read-only verifier provider %q CLI %q resolves at %s", provider, cliName, path),
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

func nonEmptyLines(text string) []string {
	var lines []string
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return lines
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

func checkMigrationStatus(repoPath string, deps Deps) Check {
	status := scanMigrationStatus(repoPath, deps)
	legacyCount := migrationLegacyCount(status)

	if legacyCount > 0 {
		return Check{
			Name:    "migration status",
			Status:  StatusWarn,
			Message: fmt.Sprintf("found %d legacy surface(s) requiring migration; run: loopcoder doctor --repo . --fix", legacyCount),
		}
	}
	return Check{
		Name:    "migration status",
		Status:  StatusOK,
		Message: "no legacy surfaces found",
	}
}

func scanMigrationStatus(repoPath string, deps Deps) upgrade.MigrationStatus {
	deps = normalizeDeps(deps)
	udeps := upgrade.DefaultDeps()
	udeps.Getwd = func() (string, error) { return repoPath, nil }
	udeps.Getenv = deps.Getenv
	udeps.ReadFile = deps.ReadFile
	return upgrade.ScanMigrationStatus(udeps)
}

func migrationLegacyCount(status upgrade.MigrationStatus) int {
	return len(status.EnvDiagnostics) + len(status.HookDiagnostics) + len(status.OldSurfaceDiagnostics) + len(status.ConfigDiagnostics)
}

func checkStaleState(repoPath string, deps Deps) Check {
	deps = normalizeDeps(deps)
	opts := localcleanup.Options{
		RepoPath: repoPath,
	}
	result, err := deps.CleanupPlan(opts)
	if err != nil {
		return Check{
			Name:    "stale local state",
			Status:  StatusWarn,
			Message: fmt.Sprintf("could not scan local state: %v; after fixing local .loopcoder permissions, rerun: loopcoder doctor --repo .", err),
		}
	}

	if len(result.Planned) > 0 {
		return Check{
			Name:    "stale local state",
			Status:  StatusWarn,
			Message: fmt.Sprintf("found %d cleanup-eligible item(s); run: loopcoder doctor --repo . --fix", len(result.Planned)),
		}
	}
	return Check{
		Name:    "stale local state",
		Status:  StatusOK,
		Message: "no cleanup-eligible items found",
	}
}

func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
