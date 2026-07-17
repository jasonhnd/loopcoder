package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/agent"
	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/gitutil"
	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/hostprofile"
	"github.com/jasonhnd/loopcoder/internal/orchestration"
	"github.com/jasonhnd/loopcoder/internal/pathid"
	"github.com/jasonhnd/loopcoder/internal/readonlyexec"
	"github.com/jasonhnd/loopcoder/internal/recovery"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/runtimecap"
	"github.com/jasonhnd/loopcoder/internal/runtimepath"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
	"github.com/jasonhnd/loopcoder/internal/writeexec"
)

const (
	nestedTestSubprocessProvider = "test-subprocess"
	nestedPromptBudgetBytes      = 16 * 1024
)

type nestedRunOptions struct {
	RepoPath         string
	PlanPath         string
	BaseBranch       string
	HostProfile      string
	Provider         string
	Model            string
	Effort           string
	ParentPermission string
	Format           string
	Timeout          time.Duration
	ConfigFromBase   bool
	Strict           bool
	SchedulerRunIDs  []string
}

type nestedChildMetadata struct {
	IssueBody      string `json:"issue_body,omitempty"`
	Prompt         string `json:"prompt,omitempty"`
	Branch         string `json:"branch,omitempty"`
	Provider       string `json:"provider,omitempty"`
	Model          string `json:"model,omitempty"`
	Effort         string `json:"effort,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	AdapterID      string `json:"adapter_id,omitempty"`
}

func runNested(args []string, stdout, stderr io.Writer, deps Deps) int {
	if len(args) == 0 || isHelp(args[0]) {
		printNestedHelp(stdout)
		return 0
	}
	switch args[0] {
	case "run":
		return runNestedRun(args[1:], stdout, stderr, deps)
	default:
		fmt.Fprintf(stderr, "nested: unknown subcommand %q\n\n", args[0])
		printNestedHelp(stderr)
		return 2
	}
}

func printNestedHelp(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  loopcoder nested run --repo <path> --plan <file.json> [flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Submit a v1 child plan, persist the durable graph, and execute read-only or isolated bounded-write children through loopcoder-owned scheduling.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	fmt.Fprintln(w, "  --repo string                 repository path (required)")
	fmt.Fprintln(w, "  --plan string                 child plan JSON file (required)")
	fmt.Fprintln(w, "  --base-branch string          base branch associated with the nested run (default \"main\")")
	fmt.Fprintln(w, "  --provider string             child provider: codex or grok for bounded write; claude is read-only (default from .delivery.yml or \"codex\")")
	fmt.Fprintln(w, "  --model string                optional child model override for this run")
	fmt.Fprintln(w, "  --effort string               optional child reasoning effort override for this run")
	fmt.Fprintln(w, "  --parent-permission string    parent permission ceiling: read-only, write, or orchestrate (default \"orchestrate\")")
	fmt.Fprintln(w, "  --format string               output format: text, json, or jsonl (default \"text\")")
	fmt.Fprintln(w, "  --timeout duration            optional timeout for the nested run, for example 30s or 5m")
	fmt.Fprintln(w, "  --config-from-base            read .delivery.yml from base branch when absent from working tree")
	fmt.Fprintln(w, "  --strict                      reject invalid model/depth selections instead of warning")
	fmt.Fprintln(w, "  --help                        show nested command help")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  provider %q is reserved for deterministic local smoke tests; its scope.commands still pass through read-only or bounded-write post-run enforcement.\n", nestedTestSubprocessProvider)
}

func runNestedRun(args []string, stdout, stderr io.Writer, deps Deps) int {
	defaults := DefaultDeps()
	if deps.AgentLookup == nil {
		deps.AgentLookup = defaults.AgentLookup
	}
	if deps.Now == nil {
		deps.Now = defaults.Now
	}

	fs := flag.NewFlagSet("nested run", flag.ContinueOnError)
	fs.SetOutput(stderr)

	opts := nestedRunOptions{
		BaseBranch:       lcdefaults.BaseBranch,
		ParentPermission: string(reporter.PermissionOrchestrate),
		Format:           "text",
	}
	var repoAlias string
	var planAlias string
	var baseBranchAlias string
	var providerAlias string
	var modelAlias string
	var effortAlias string
	var parentPermissionAlias string
	var formatAlias string
	var timeoutAlias time.Duration
	var configFromBaseAlias bool
	var strictAlias bool

	fs.StringVar(&opts.RepoPath, "repo", "", "repository path")
	fs.StringVar(&repoAlias, "Repo", "", "repository path")
	fs.StringVar(&opts.PlanPath, "plan", "", "child plan JSON path")
	fs.StringVar(&planAlias, "Plan", "", "child plan JSON path")
	fs.StringVar(&opts.BaseBranch, "base-branch", lcdefaults.BaseBranch, "base branch")
	fs.StringVar(&baseBranchAlias, "BaseBranch", "", "base branch")
	fs.StringVar(&opts.Provider, "provider", "", "provider")
	fs.StringVar(&providerAlias, "Provider", "", "provider")
	fs.StringVar(&opts.Model, "model", "", "model")
	fs.StringVar(&modelAlias, "Model", "", "model")
	fs.StringVar(&opts.Effort, "effort", "", "effort")
	fs.StringVar(&effortAlias, "Effort", "", "effort")
	fs.StringVar(&opts.ParentPermission, "parent-permission", string(reporter.PermissionOrchestrate), "parent permission ceiling")
	fs.StringVar(&parentPermissionAlias, "ParentPermission", "", "parent permission ceiling")
	fs.StringVar(&opts.Format, "format", "text", "output format")
	fs.StringVar(&formatAlias, "Format", "", "output format")
	fs.DurationVar(&opts.Timeout, "timeout", 0, "nested run timeout")
	fs.DurationVar(&timeoutAlias, "Timeout", 0, "nested run timeout")
	fs.BoolVar(&opts.ConfigFromBase, "config-from-base", false, "read .delivery.yml from base branch when absent from working tree")
	fs.BoolVar(&configFromBaseAlias, "ConfigFromBase", false, "read .delivery.yml from base branch when absent from working tree")
	fs.BoolVar(&opts.Strict, "strict", false, "reject invalid model/depth selections instead of warning")
	fs.BoolVar(&strictAlias, "Strict", false, "reject invalid model/depth selections instead of warning")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if opts.RepoPath == "" {
		opts.RepoPath = repoAlias
	}
	if opts.PlanPath == "" {
		opts.PlanPath = planAlias
	}
	if baseBranchAlias != "" {
		opts.BaseBranch = baseBranchAlias
	}
	if providerAlias != "" {
		opts.Provider = providerAlias
	}
	if modelAlias != "" {
		opts.Model = modelAlias
	}
	if effortAlias != "" {
		opts.Effort = effortAlias
	}
	if parentPermissionAlias != "" {
		opts.ParentPermission = parentPermissionAlias
	}
	if formatAlias != "" {
		opts.Format = formatAlias
	}
	if timeoutAlias > 0 {
		opts.Timeout = timeoutAlias
	}
	opts.ConfigFromBase = opts.ConfigFromBase || configFromBaseAlias
	opts.Strict = opts.Strict || strictAlias

	if strings.TrimSpace(opts.RepoPath) == "" {
		fmt.Fprintln(stderr, "nested run: --repo is required")
		return 2
	}
	if strings.TrimSpace(opts.PlanPath) == "" {
		fmt.Fprintln(stderr, "nested run: --plan is required")
		return 2
	}
	switch opts.Format {
	case "text", "json", "jsonl":
	default:
		fmt.Fprintf(stderr, "nested run: invalid --format %q; want text, json, or jsonl\n", opts.Format)
		return 2
	}
	warnings := stderr
	if opts.Format == "json" || opts.Format == "jsonl" {
		warnings = io.Discard
	}
	parentPermission := normalizeNestedPermission(opts.ParentPermission)
	if !validNestedParentPermission(parentPermission) {
		fmt.Fprintf(stderr, "nested run: invalid --parent-permission %q; want read-only, write, or orchestrate\n", opts.ParentPermission)
		return 2
	}

	resolvedRepo, err := resolveRepo(opts.RepoPath)
	if err != nil {
		fmt.Fprintf(stderr, "nested run: %v\n", err)
		return 2
	}
	opts.RepoPath = resolvedRepo
	if exitCode, blocked := checkRelayGate(resolvedRepo, stdout, stderr); blocked {
		return exitCode
	}

	planData, err := os.ReadFile(opts.PlanPath)
	if err != nil {
		fmt.Fprintf(stderr, "nested run: read plan: %v\n", err)
		return 2
	}
	plan, err := orchestration.ParseChildPlanJSON(planData)
	if err != nil {
		var permissionErr *orchestration.PermissionNotEnforceableError
		if errors.As(err, &permissionErr) {
			return renderNestedPermissionRefusal(stdout, stderr, opts, plan, nestedExecutorCapability(opts), permissionErr, deps)
		}
		fmt.Fprintf(stderr, "nested run: %v\n", err)
		return 2
	}
	if err := enforceNestedPlanScope(resolvedRepo, parentPermission, &plan); err != nil {
		fmt.Fprintf(stderr, "nested run: %v\n", err)
		return 2
	}
	if err := orchestration.PrepareNestedPlanForExecution(&plan, 0); err != nil {
		fmt.Fprintf(stderr, "nested run: %v\n", err)
		return 1
	}

	cfg, err := loadDeliveryConfig(resolvedRepo, opts.BaseBranch, opts.ConfigFromBase)
	if err != nil {
		fmt.Fprintf(stderr, "nested run: %v\n", err)
		return 1
	}
	opts.HostProfile = cfg.Host.Profile
	capabilityOpts := opts
	if capabilityOpts.Provider != nestedTestSubprocessProvider {
		capabilityOpts.Provider = firstNonEmptyNested(capabilityOpts.Provider, cfg.Adapters.Worker, defaultProviderForRole("worker"))
	}
	capability := nestedExecutorCapability(capabilityOpts)
	if err := orchestration.CheckNestedExecutorPermissions(&plan, capability); err != nil {
		var permissionErr *orchestration.PermissionNotEnforceableError
		if errors.As(err, &permissionErr) {
			return renderNestedPermissionRefusal(stdout, stderr, capabilityOpts, plan, capability, permissionErr, deps)
		}
		fmt.Fprintf(stderr, "nested run: permission preflight: %v\n", err)
		return 1
	}
	opts.Provider = capabilityOpts.Provider
	opts.SchedulerRunIDs = append(opts.SchedulerRunIDs, plan.ParentRunID)
	for _, child := range plan.Items {
		opts.SchedulerRunIDs = append(opts.SchedulerRunIDs, child.RunID)
	}

	if opts.Provider != nestedTestSubprocessProvider {
		selection, ok := resolveAndValidateRoleSelection(roleSelectionInput{
			Role:           "worker",
			Provider:       opts.Provider,
			Model:          opts.Model,
			Effort:         opts.Effort,
			ConfigProvider: cfg.Adapters.Worker,
			ConfigModel:    cfg.Worker.Model,
			ConfigEffort:   cfg.Worker.ReasoningEffort,
			Strict:         cfg.Models.Strict || opts.Strict,
			Warnings:       warnings,
		})
		if !ok {
			return 1
		}
		opts.Provider = selection.Provider
		opts.Model = selection.Model
		opts.Effort = selection.Effort
		if err := preflightNestedPlanRoute(plan, opts.Provider, opts.HostProfile); err != nil {
			fmt.Fprintf(stderr, "nested run: child route preflight: %v\n", err)
			return 1
		}
	} else {
		opts.Model = firstNonEmptyNested(opts.Model, "deterministic-subprocess")
		opts.Effort = firstNonEmptyNested(opts.Effort, "none")
	}
	if err := validateNestedPlanProviders(plan, opts.Provider); err != nil {
		fmt.Fprintf(stderr, "nested run: provider preflight: %v\n", err)
		return 1
	}

	layout, err := home.Resolve(home.DefaultDeps())
	if err != nil {
		fmt.Fprintf(stderr, "nested run: %v\n", err)
		return 1
	}
	store, err := storage.Open(context.Background(), storage.Options{Path: layout.DatabasePath(), Now: deps.Now})
	if err != nil {
		fmt.Fprintf(stderr, "nested run: %v\n", err)
		return 1
	}
	defer store.Close()

	ctx := context.Background()
	cancel := func() {}
	if opts.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
	}
	defer cancel()

	executor := nestedExecutor(opts, deps, warnings)
	progressRecorder, stopProgress := progressSupervisorForRegisteredRepo(context.Background(), resolvedRepo, plan.RootRunID, deps.Now, stderr)
	defer func() {
		if err := stopProgress(); err != nil {
			fmt.Fprintf(stderr, "nested run: progress receipt shutdown: %v\n", err)
		}
	}()

	report, err := orchestration.ScheduleNestedRuns(ctx, orchestration.NestedScheduleOptions{
		RepoPath:                 resolvedRepo,
		BaseBranch:               opts.BaseBranch,
		Plan:                     &plan,
		Store:                    store,
		Budget:                   cfg.Guardrails.Budget,
		CircuitBreaker:           cfg.Guardrails.CircuitBreaker,
		ConcurrencyLimit:         plan.MaxConcurrency,
		MaxDepth:                 plan.MaxDepth,
		Progress:                 progressRecorder,
		AllowUnbudgetedLocalTest: opts.Provider == nestedTestSubprocessProvider,
		Execute:                  executor,
	})
	if err != nil {
		if nestedReportHasContent(report) {
			if renderErr := renderNestedRun(stdout, opts.Format, report, deps); renderErr != nil {
				fmt.Fprintf(stderr, "nested run: write output after failure: %v\n", renderErr)
			}
		}
		if opts.Format != "json" || !nestedReportHasContent(report) {
			fmt.Fprintf(stderr, "nested run: %v\n", err)
		}
		return 1
	}
	if err := renderNestedRun(stdout, opts.Format, report, deps); err != nil {
		fmt.Fprintf(stderr, "nested run: write output: %v\n", err)
		return 1
	}
	if report.Status == orchestration.NestedStatusFailed || report.Status == orchestration.NestedStatusNeedsHuman {
		return 1
	}
	return 0
}

func nestedReportHasContent(report orchestration.NestedScheduleReport) bool {
	return report.Version != 0 || strings.TrimSpace(report.ParentRunID) != "" || len(report.Children) > 0
}

func nestedExecutorCapability(opts nestedRunOptions) orchestration.NestedExecutorCapability {
	executorID := "nested-child"
	registrationID := ""
	if opts.Provider == nestedTestSubprocessProvider {
		executorID = nestedTestSubprocessProvider
	}
	permissions := []string{}
	if nestedReadOnlyProviderSupported(opts.Provider) {
		registrationID = "builtin:nested-child:" + strings.TrimSpace(opts.Provider) + ":v2"
		permissions = []string{string(reporter.PermissionReadOnly)}
		if nestedBoundedWriteProviderSupported(opts.Provider) {
			permissions = append(permissions, string(reporter.PermissionWrite))
		}
	}
	return orchestration.NestedExecutorCapability{
		ExecutorID:             executorID,
		RegistrationID:         registrationID,
		Provider:               strings.TrimSpace(opts.Provider),
		EnforceablePermissions: permissions,
		ProviderNative:         false,
	}
}

func nestedBoundedWriteProviderSupported(provider string) bool {
	switch strings.TrimSpace(provider) {
	case "codex", "grok", nestedTestSubprocessProvider:
		return true
	default:
		return false
	}
}

func nestedReadOnlyProviderSupported(provider string) bool {
	switch strings.TrimSpace(provider) {
	case "codex", "claude", "grok", nestedTestSubprocessProvider:
		return true
	default:
		return false
	}
}

func preflightNestedReadOnlyRoute(provider, hostProfile string) error {
	provider = strings.TrimSpace(provider)
	if !nestedReadOnlyProviderSupported(provider) || provider == nestedTestSubprocessProvider {
		if provider == nestedTestSubprocessProvider {
			return nil
		}
		return fmt.Errorf("provider %q has no registered nested read-only adapter", provider)
	}
	if err := runtimecap.RequireProviderCapability(provider, runtimecap.ProviderReadOnly); err != nil {
		return err
	}
	host, err := hostprofile.Resolve(hostprofile.Options{Profile: hostProfile, Getenv: os.Getenv})
	if err != nil {
		return err
	}
	compatibility := runtimecap.EvaluateCompatibility(provider, host.Name, runtimecap.RoleVerifier)
	if compatibility.Support == runtimecap.SupportUnsupported {
		return fmt.Errorf("provider %q and host %q cannot enforce the read-only child contract (%s)", provider, host.Name, compatibility.Code)
	}
	return nil
}

func preflightNestedWriteRoute(provider, hostProfile string) error {
	provider = strings.TrimSpace(provider)
	if !nestedBoundedWriteProviderSupported(provider) || provider == nestedTestSubprocessProvider {
		if provider == nestedTestSubprocessProvider {
			return nil
		}
		return fmt.Errorf("provider %q has no registered nested bounded-write adapter", provider)
	}
	if err := runtimecap.RequireProviderCapability(provider, runtimecap.ProviderBoundedWrite); err != nil {
		return err
	}
	host, err := hostprofile.Resolve(hostprofile.Options{Profile: hostProfile, Getenv: os.Getenv})
	if err != nil {
		return err
	}
	compatibility := runtimecap.EvaluateCompatibility(provider, host.Name, runtimecap.RoleWorker)
	if compatibility.Support == runtimecap.SupportUnsupported {
		return fmt.Errorf("provider %q and host %q cannot enforce the bounded-write child contract (%s)", provider, host.Name, compatibility.Code)
	}
	return nil
}

func preflightNestedPlanRoute(plan orchestration.ChildPlan, provider, hostProfile string) error {
	seen := map[string]bool{}
	for _, child := range plan.Items {
		permission := normalizeNestedPermission(child.Permission)
		if seen[permission] {
			continue
		}
		seen[permission] = true
		switch permission {
		case string(reporter.PermissionReadOnly):
			if err := preflightNestedReadOnlyRoute(provider, hostProfile); err != nil {
				return err
			}
		case string(reporter.PermissionWrite):
			if err := preflightNestedWriteRoute(provider, hostProfile); err != nil {
				return err
			}
		default:
			return fmt.Errorf("child %q permission %q has no nested execution adapter", child.ChildKey, child.Permission)
		}
	}
	return nil
}

func validateNestedPlanProviders(plan orchestration.ChildPlan, selectedProvider string) error {
	selectedProvider = strings.TrimSpace(selectedProvider)
	for _, child := range plan.Items {
		var metadata nestedChildMetadata
		if len(child.Metadata) > 0 {
			if err := json.Unmarshal(child.Metadata, &metadata); err != nil {
				return fmt.Errorf("child %q provider metadata is invalid: %w", child.ChildKey, err)
			}
		}
		for _, declaration := range []struct {
			field string
			value string
		}{
			{field: "provider", value: metadata.Provider},
			{field: "adapter_id", value: metadata.AdapterID},
		} {
			field := declaration.field
			requested := strings.TrimSpace(declaration.value)
			if selectedProvider != nestedTestSubprocessProvider && requested != "" && requested != selectedProvider {
				return fmt.Errorf("child %q %s %q does not match executor registration %q", child.ChildKey, field, requested, selectedProvider)
			}
		}
		if !nestedReadOnlyProviderSupported(selectedProvider) {
			return fmt.Errorf("child %q provider %q has no registered nested adapter", child.ChildKey, selectedProvider)
		}
		if normalizeNestedPermission(child.Permission) == string(reporter.PermissionWrite) && !nestedBoundedWriteProviderSupported(selectedProvider) {
			return fmt.Errorf("child %q provider %q has no registered nested bounded-write adapter", child.ChildKey, selectedProvider)
		}
	}
	return nil
}

func validateNestedPlanReadOnlyProviders(plan orchestration.ChildPlan, selectedProvider string) error {
	return validateNestedPlanProviders(plan, selectedProvider)
}

func nestedExecutor(opts nestedRunOptions, deps Deps, stderr io.Writer) orchestration.ChildRunExecutor {
	readOnly := nestedReadOnlyExecutor(opts, deps, stderr)
	boundedWrite := nestedWriteExecutor(opts, deps, stderr)
	return func(ctx context.Context, request orchestration.ChildExecutionRequest) (orchestration.ChildRunResult, error) {
		switch normalizeNestedPermission(request.Permission) {
		case string(reporter.PermissionReadOnly):
			return readOnly(ctx, request)
		case string(reporter.PermissionWrite):
			return boundedWrite(ctx, request)
		default:
			base := nestedResultFromRequest(request)
			return base, &writeexec.PolicyViolationError{Phase: "pre-launch", Reason: "the executor received a permission without a registered nested adapter"}
		}
	}
}

func renderNestedPermissionRefusal(stdout, stderr io.Writer, opts nestedRunOptions, plan orchestration.ChildPlan, capability orchestration.NestedExecutorCapability, permissionErr *orchestration.PermissionNotEnforceableError, deps Deps) int {
	report := orchestration.NestedPermissionRefusalReport(opts.RepoPath, opts.BaseBranch, plan, capability, permissionErr, deps.Now())
	if err := renderNestedRun(stdout, opts.Format, report, deps); err != nil {
		fmt.Fprintf(stderr, "nested run: write permission refusal: %v\n", err)
		return 1
	}
	return 1
}

func nestedReadOnlyExecutor(opts nestedRunOptions, deps Deps, stderr io.Writer) orchestration.ChildRunExecutor {
	return func(ctx context.Context, request orchestration.ChildExecutionRequest) (orchestration.ChildRunResult, error) {
		child := request.ChildRunPlan()
		base := nestedResultFromRequest(request)
		if request.Permission != string(reporter.PermissionReadOnly) {
			return base, &readonlyexec.PolicyViolationError{Phase: "pre-launch", Reason: "the executor received a non-read-only immutable contract"}
		}
		provider := opts.Provider
		if provider != nestedTestSubprocessProvider {
			provider = firstNonEmptyNested(request.Work.Provider, request.ProviderDecision.AdapterID, opts.Provider)
		}
		if provider != opts.Provider {
			return base, &readonlyexec.PolicyViolationError{Phase: "pre-launch", Reason: "the persisted provider route does not match the registered read-only adapter"}
		}
		if err := preflightNestedReadOnlyRoute(provider, opts.HostProfile); err != nil {
			return base, &readonlyexec.PolicyViolationError{Phase: "pre-launch", Reason: "the persisted provider route is not supported for read-only execution"}
		}

		roots, err := runtimepath.Resolve(ctx, opts.RepoPath)
		if err != nil {
			return base, err
		}
		projectKey, err := nestedPrivateProjectKey(opts.RepoPath, roots.ProjectID)
		if err != nil {
			return base, fmt.Errorf("resolve private nested read-only project identity: %w", err)
		}
		evidenceDir := filepath.Join(roots.LogsRoot, "nested-read-only", projectKey, nestedPrivateRunKey(child.RunID))
		if err := os.MkdirAll(evidenceDir, 0o700); err != nil {
			return base, fmt.Errorf("prepare private nested read-only evidence: %w", err)
		}
		evidencePath := filepath.Join(evidenceDir, "enforcement.json")
		projectStatePaths := []string{filepath.Join(opts.RepoPath, ".loopcoder")}
		excludedStatePaths := make([]string, 0, len(opts.SchedulerRunIDs))
		for _, runID := range opts.SchedulerRunIDs {
			if strings.TrimSpace(runID) == "" || strings.TrimSpace(runID) == child.RunID {
				continue
			}
			excludedStatePaths = append(excludedStatePaths, state.RunPath(opts.RepoPath, runID))
		}
		if roots.Registered && strings.TrimSpace(roots.ProjectRoot) != "" {
			projectStatePaths = append(projectStatePaths, roots.ProjectRoot)
		}
		enforcementOpts := readonlyexec.Options{
			RepoPath:            opts.RepoPath,
			EvidencePath:        evidencePath,
			ContractFingerprint: request.ContractFingerprint,
			ClaimGeneration:     request.ClaimGeneration,
			ProjectStatePaths:   projectStatePaths,
			ExcludedPaths:       excludedStatePaths,
		}
		if existing, ok, err := completedNestedAttempt(opts.RepoPath, child, provider, enforcementOpts); err != nil {
			return base, err
		} else if ok {
			return existing, nil
		}
		session, baselineAudit, err := readonlyexec.Begin(ctx, enforcementOpts)
		base.ReadOnlyEnforcement = nestedReadOnlyAudit(baselineAudit)
		if err != nil {
			return base, err
		}

		started := deps.Now().UTC()
		runCtx := ctx
		cancel := func() {}
		if request.Work.TimeoutSeconds > 0 {
			runCtx, cancel = context.WithTimeout(ctx, time.Duration(request.Work.TimeoutSeconds)*time.Second)
		}
		defer cancel()
		providerResult, runErr := runNestedReadOnlyProvider(runCtx, opts, request, provider, deps, evidenceDir, stderr)
		ended := deps.Now().UTC()
		status := nestedAgentStatus(providerResult, runErr)
		summary := strings.TrimSpace(recovery.Scrub(providerResult.Summary))
		if summary == "" {
			if runErr == nil && status == orchestration.NestedStatusSucceeded {
				summary = "read-only child completed"
			} else if runErr == nil {
				summary = "read-only child did not complete successfully"
			} else {
				summary = strings.TrimSpace(recovery.Scrub(runErr.Error()))
			}
		}
		summary = boundedNestedText(summary)

		verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 30*time.Second)
		audit, enforcementErr := session.Finish(verifyCtx, status)
		verifyCancel()
		auditRecord := nestedReadOnlyAudit(audit)
		base.ReadOnlyEnforcement = auditRecord
		if enforcementErr != nil {
			status = orchestration.NestedStatusNeedsHuman
		}
		exitCode := providerResult.ExitCode
		if exitCode == 0 && (runErr != nil || enforcementErr != nil) {
			exitCode = 1
		}
		reportRecord := reporter.Report{
			WorkID:      child.RunID,
			Issue:       child.Issue,
			Role:        reporter.RoleWorker,
			Provider:    provider,
			Model:       firstNonEmptyNested(providerResult.Model, request.Work.Model, opts.Model),
			ModelSource: reporter.ModelSourceForProvider(provider),
			Effort:      firstNonEmptyNested(providerResult.Effort, request.Work.Effort, opts.Effort),
			Permission:  reporter.PermissionReadOnly,
			Action:      "execute read-only nested child " + child.ChildKey,
			ExitCode:    exitCode,
			StartedAt:   firstNonEmptyNested(providerResult.StartedAt, state.FormatTimestamp(started)),
			EndedAt:     firstNonEmptyNested(providerResult.EndedAt, state.FormatTimestamp(ended)),
			DurationMS:  maxNestedDuration(providerResult.DurationMS, ended.Sub(started).Milliseconds()),
			Usage:       providerResult.Usage,
			Verified:    enforcementErr == nil && runErr == nil && status == orchestration.NestedStatusSucceeded,
		}
		errText := ""
		if enforcementErr != nil {
			errText = enforcementErr.Error()
		} else if status != orchestration.NestedStatusSucceeded {
			errText = summary
		}
		attemptPath, writeErr := writeNestedAttempt(opts.RepoPath, child, provider, reportRecord, status, exitCode, summary, errText, auditRecord, deps.Now)
		if writeErr != nil {
			fmt.Fprintf(stderr, "[loopcoder] warning: failed to write nested read-only attempt: %v\n", writeErr)
			attemptPath = ""
		}
		base.Status = status
		base.StartedAt = reportRecord.StartedAt
		base.FinishedAt = reportRecord.EndedAt
		base.AttemptPath = attemptPath
		base.ProviderReceipt = attemptPath
		base.Report = &reportRecord
		base.ReadOnlyEnforcement = auditRecord
		if enforcementErr != nil {
			base.Error = enforcementErr.Error()
			base.Reason = "read-only enforcement detected or could not rule out a guarded state change"
			base.NextAction = "inspect the preserved enforcement audit before replaying this child"
			return base, enforcementErr
		}
		if runErr != nil {
			base.Error = summary
			base.Reason = summary
			base.NextAction = "inspect the failed read-only provider attempt and rerun the unchanged plan after recovery"
			return base, sanitizeNestedProviderError(runErr)
		}
		base.Reason = summary
		if status != orchestration.NestedStatusSucceeded {
			base.Error = summary
			base.NextAction = "inspect the read-only provider attempt before replaying the unchanged plan"
		}
		return base, nil
	}
}

func nestedWriteExecutor(opts nestedRunOptions, deps Deps, stderr io.Writer) orchestration.ChildRunExecutor {
	return func(ctx context.Context, request orchestration.ChildExecutionRequest) (orchestration.ChildRunResult, error) {
		child := request.ChildRunPlan()
		base := nestedResultFromRequest(request)
		if request.Permission != string(reporter.PermissionWrite) {
			return base, &writeexec.PolicyViolationError{Phase: "pre-launch", Reason: "the bounded-write executor received a non-write immutable contract"}
		}
		provider := opts.Provider
		if provider != nestedTestSubprocessProvider {
			provider = firstNonEmptyNested(request.Work.Provider, request.ProviderDecision.AdapterID, opts.Provider)
		}
		if provider != opts.Provider {
			return base, &writeexec.PolicyViolationError{Phase: "pre-launch", Reason: "the persisted provider route does not match the registered bounded-write adapter"}
		}
		if err := preflightNestedWriteRoute(provider, opts.HostProfile); err != nil {
			return base, &writeexec.PolicyViolationError{Phase: "pre-launch", Reason: "the persisted provider route is not supported for bounded-write execution"}
		}
		if err := validateNestedWriteContract(request); err != nil {
			return base, err
		}

		enforcementOpts, evidenceDir, err := nestedWriteEnforcementOptions(ctx, opts, request)
		if err != nil {
			var policyErr *writeexec.PolicyViolationError
			if errors.As(err, &policyErr) {
				return base, err
			}
			return base, &writeexec.PolicyViolationError{Phase: "pre-launch", Reason: "the private bounded-write authority could not be prepared"}
		}
		base.WorktreePath = enforcementOpts.WorktreePath
		if existing, ok, err := completedNestedWriteAttempt(opts.RepoPath, child, provider, enforcementOpts); err != nil {
			return base, err
		} else if ok {
			return existing, nil
		}
		environment, err := prepareNestedWriteEnvironment(evidenceDir, request.ClaimGeneration)
		if err != nil {
			return base, &writeexec.PolicyViolationError{Phase: "pre-launch", Reason: "the private Git deny hooks could not be prepared"}
		}

		session, baselineAudit, err := writeexec.Begin(ctx, enforcementOpts)
		base.MutationManifest = nestedMutationManifest(baselineAudit)
		if err != nil {
			return base, err
		}
		started := deps.Now().UTC()
		runCtx := ctx
		cancel := func() {}
		if request.Work.TimeoutSeconds > 0 {
			runCtx, cancel = context.WithTimeout(ctx, time.Duration(request.Work.TimeoutSeconds)*time.Second)
		}
		defer cancel()
		providerResult, runErr := runNestedWriteProvider(runCtx, opts, request, provider, deps, evidenceDir, enforcementOpts.WorktreePath, environment, stderr)
		ended := deps.Now().UTC()
		status := nestedAgentStatus(providerResult, runErr)
		summary := strings.TrimSpace(recovery.Scrub(providerResult.Summary))
		if summary == "" {
			if runErr == nil && status == orchestration.NestedStatusSucceeded {
				summary = "bounded-write child completed"
			} else if runErr == nil {
				summary = "bounded-write child did not complete successfully"
			} else {
				summary = strings.TrimSpace(recovery.Scrub(runErr.Error()))
			}
		}
		summary = boundedNestedText(summary)

		verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 30*time.Second)
		audit, enforcementErr := session.Finish(verifyCtx, status)
		verifyCancel()
		auditRecord := nestedMutationManifest(audit)
		base.MutationManifest = auditRecord
		if enforcementErr != nil {
			status = orchestration.NestedStatusNeedsHuman
		}
		exitCode := providerResult.ExitCode
		if exitCode == 0 && (runErr != nil || enforcementErr != nil) {
			exitCode = 1
		}
		reportRecord := reporter.Report{
			WorkID:      child.RunID,
			Issue:       child.Issue,
			Role:        reporter.RoleWorker,
			Provider:    provider,
			Model:       firstNonEmptyNested(providerResult.Model, request.Work.Model, opts.Model),
			ModelSource: reporter.ModelSourceForProvider(provider),
			Effort:      firstNonEmptyNested(providerResult.Effort, request.Work.Effort, opts.Effort),
			Permission:  reporter.PermissionWrite,
			Action:      "execute bounded-write nested child " + child.ChildKey,
			ExitCode:    exitCode,
			StartedAt:   firstNonEmptyNested(providerResult.StartedAt, state.FormatTimestamp(started)),
			EndedAt:     firstNonEmptyNested(providerResult.EndedAt, state.FormatTimestamp(ended)),
			DurationMS:  maxNestedDuration(providerResult.DurationMS, ended.Sub(started).Milliseconds()),
			Usage:       providerResult.Usage,
			Verified:    enforcementErr == nil && runErr == nil && status == orchestration.NestedStatusSucceeded,
		}
		errText := ""
		if enforcementErr != nil {
			errText = enforcementErr.Error()
		} else if status != orchestration.NestedStatusSucceeded {
			errText = summary
		}
		attemptPath, writeErr := writeNestedWriteAttempt(opts.RepoPath, child, request.ClaimGeneration, provider, reportRecord, status, exitCode, summary, errText, auditRecord, enforcementOpts.WorktreePath, deps.Now)
		var persistenceErr error
		if writeErr != nil {
			fmt.Fprintf(stderr, "[loopcoder] warning: failed to write nested bounded-write attempt: %v\n", writeErr)
			attemptPath = ""
			status = orchestration.NestedStatusNeedsHuman
			reportRecord.Verified = false
			if reportRecord.ExitCode == 0 {
				reportRecord.ExitCode = 1
			}
			persistenceErr = &writeexec.PolicyViolationError{Phase: "attempt-persistence", Reason: "the verified mutation manifest could not be persisted to the durable child attempt"}
		}
		base.Status = status
		base.StartedAt = reportRecord.StartedAt
		base.FinishedAt = reportRecord.EndedAt
		base.AttemptPath = attemptPath
		base.ProviderReceipt = attemptPath
		base.Report = &reportRecord
		base.MutationManifest = auditRecord
		base.WorktreePath = enforcementOpts.WorktreePath
		if persistenceErr != nil {
			base.Error = persistenceErr.Error()
			base.Reason = "bounded-write completion was withheld because its durable attempt could not be persisted"
			base.NextAction = "inspect the preserved private manifest and repair local attempt storage before reconciliation"
			return base, persistenceErr
		}
		if enforcementErr != nil {
			base.Error = enforcementErr.Error()
			base.Reason = "bounded-write enforcement detected or could not rule out a forbidden state change"
			base.NextAction = "inspect the preserved mutation manifest and isolated worktree before replaying this child"
			return base, enforcementErr
		}
		if runErr != nil {
			base.Error = summary
			base.Reason = summary
			base.NextAction = "inspect the preserved bounded-write attempt and isolated worktree before recovery"
			return base, sanitizeNestedWriteProviderError(runErr)
		}
		base.Reason = summary
		if status != orchestration.NestedStatusSucceeded {
			base.Error = summary
			base.NextAction = "inspect the bounded-write attempt and isolated worktree before replaying the unchanged plan"
		}
		return base, nil
	}
}

func validateNestedWriteContract(request orchestration.ChildExecutionRequest) error {
	if request.ClaimGeneration <= 0 || len(request.MutationScope.Paths) == 0 {
		return &writeexec.PolicyViolationError{Phase: "pre-launch", Reason: "bounded-write execution requires a fenced claim and a non-empty concrete path scope"}
	}
	if len(request.MutationScope.PullRequests) > 0 || len(request.MutationScope.Data) > 0 {
		return &writeexec.PolicyViolationError{Phase: "pre-launch", Reason: "bounded-write execution supports local path mutations only"}
	}
	if len(request.Capabilities.Network) > 0 || len(request.Capabilities.Delegation) > 0 {
		return &writeexec.PolicyViolationError{Phase: "pre-launch", Reason: "bounded-write execution cannot receive network or delegation capability"}
	}
	if strings.TrimSpace(request.Work.Branch) != "" {
		return &writeexec.PolicyViolationError{Phase: "pre-launch", Reason: "bounded-write execution cannot receive branch authority"}
	}
	return nil
}

func nestedWriteEnforcementOptions(ctx context.Context, opts nestedRunOptions, request orchestration.ChildExecutionRequest) (writeexec.Options, string, error) {
	roots, err := runtimepath.Resolve(ctx, opts.RepoPath)
	if err != nil {
		return writeexec.Options{}, "", err
	}
	projectKey, err := nestedPrivateProjectKey(opts.RepoPath, roots.ProjectID)
	if err != nil {
		return writeexec.Options{}, "", fmt.Errorf("resolve private nested write project identity: %w", err)
	}
	child := request.ChildRunPlan()
	evidenceDir := filepath.Join(roots.LogsRoot, "nested-write", projectKey, nestedPrivateRunKey(child.RunID))
	if err := os.MkdirAll(evidenceDir, 0o700); err != nil {
		return writeexec.Options{}, "", fmt.Errorf("prepare private nested write evidence: %w", err)
	}
	requestedBase, _ := gitutil.New().RevParse(ctx, opts.RepoPath, "origin/"+strings.TrimSpace(opts.BaseBranch))
	baseRevision, err := writeexec.ResolveAuthority(filepath.Join(evidenceDir, "authority.json"), request.ContractFingerprint, requestedBase)
	if err != nil {
		return writeexec.Options{}, "", err
	}
	claimKey := fmt.Sprintf("claim-%d", request.ClaimGeneration)
	worktreePath := filepath.Join(roots.TmpRoot, "nested-write", projectKey, nestedPrivateRunKey(child.RunID), claimKey, "worktree")
	projectStatePaths := []string{filepath.Join(opts.RepoPath, ".loopcoder")}
	excludedStatePaths := make([]string, 0, len(opts.SchedulerRunIDs))
	for _, runID := range opts.SchedulerRunIDs {
		if strings.TrimSpace(runID) == "" || strings.TrimSpace(runID) == child.RunID {
			continue
		}
		excludedStatePaths = append(excludedStatePaths, state.RunPath(opts.RepoPath, runID))
	}
	if roots.Registered && strings.TrimSpace(roots.ProjectRoot) != "" {
		projectStatePaths = append(projectStatePaths, roots.ProjectRoot)
	}
	return writeexec.Options{
		RepoPath:            opts.RepoPath,
		WorktreePath:        worktreePath,
		EvidencePath:        filepath.Join(evidenceDir, claimKey+"-enforcement.json"),
		ContractFingerprint: request.ContractFingerprint,
		ClaimGeneration:     request.ClaimGeneration,
		BaseRevision:        baseRevision,
		AllowedPaths:        append([]string(nil), request.MutationScope.Paths...),
		ProjectStatePaths:   projectStatePaths,
		ExcludedPaths:       excludedStatePaths,
	}, evidenceDir, nil
}

func prepareNestedWriteEnvironment(evidenceDir string, claimGeneration int64) (map[string]string, error) {
	hooksDir := filepath.Join(evidenceDir, fmt.Sprintf("claim-%d-hooks", claimGeneration))
	if err := os.MkdirAll(hooksDir, 0o700); err != nil {
		return nil, err
	}
	denyHook := []byte("#!/bin/sh\nexit 1\n")
	for _, name := range []string{"pre-commit", "commit-msg", "pre-merge-commit", "pre-rebase", "pre-push"} {
		path := filepath.Join(hooksDir, name)
		if err := os.WriteFile(path, denyHook, 0o700); err != nil {
			return nil, err
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return nil, err
		}
	}
	return map[string]string{
		"GIT_OPTIONAL_LOCKS":  "0",
		"GIT_ALLOW_PROTOCOL":  "",
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_ASKPASS":         "false",
		"GCM_INTERACTIVE":     "never",
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_COUNT":    "1",
		"GIT_CONFIG_KEY_0":    "core.hooksPath",
		"GIT_CONFIG_VALUE_0":  hooksDir,
	}, nil
}

func runNestedWriteProvider(ctx context.Context, opts nestedRunOptions, request orchestration.ChildExecutionRequest, provider string, deps Deps, evidenceDir, worktreePath string, environment map[string]string, stderr io.Writer) (agent.Result, error) {
	if provider == nestedTestSubprocessProvider {
		return runNestedTestSubprocess(ctx, opts, request, worktreePath, environment)
	}
	runner, err := deps.AgentLookup(provider)
	if err != nil {
		return agent.Result{}, err
	}
	promptData, err := json.MarshalIndent(request, "", "  ")
	if err != nil {
		return agent.Result{}, fmt.Errorf("encode immutable child contract: %w", err)
	}
	prompt := "Execute this LoopCoder child only inside the current detached isolated worktree. Modify only the relative equivalents of allowed_mutation_scope.paths. Do not stage, commit, create or move refs, merge, rebase, tag, push, publish, change Git configuration/remotes/hooks/credentials, access the parent checkout, use network or MCP tools, or delegate work. Stop after the scoped file edits and return a concise summary.\n\nImmutable execution contract:\n" + string(promptData)
	return runner.Run(ctx, agent.Invocation{
		WorktreePath: worktreePath,
		Prompt:       prompt,
		Model:        firstNonEmptyNested(request.Work.Model, opts.Model),
		Effort:       firstNonEmptyNested(request.Work.Effort, opts.Effort),
		BoundedWrite: true,
		Environment:  environment,
		LogPath:      filepath.Join(evidenceDir, fmt.Sprintf("claim-%d-provider.log", request.ClaimGeneration)),
		Stderr:       stderr,
		HardCap:      nestedProviderHardCap(request.Work.TimeoutSeconds),
		RunID:        request.RunID,
		Role:         "nested-bounded-write",
		ProviderKey:  request.IdempotencyKey,
	})
}

func runNestedReadOnlyProvider(ctx context.Context, opts nestedRunOptions, request orchestration.ChildExecutionRequest, provider string, deps Deps, evidenceDir string, stderr io.Writer) (agent.Result, error) {
	if provider == nestedTestSubprocessProvider {
		return runNestedReadOnlyTestSubprocess(ctx, opts, request)
	}
	runner, err := deps.AgentLookup(provider)
	if err != nil {
		return agent.Result{}, err
	}
	promptData, err := json.MarshalIndent(request, "", "  ")
	if err != nil {
		return agent.Result{}, fmt.Errorf("encode immutable child contract: %w", err)
	}
	prompt := "Execute this LoopCoder nested child in strictly read-only mode. Do not modify files, Git state, hooks, configuration, worktree metadata, or LoopCoder project state. Return concise findings only.\n\nImmutable execution contract:\n" + string(promptData)
	return runner.Run(ctx, agent.Invocation{
		WorktreePath: filepath.FromSlash(request.ScopedRepositoryIdentity),
		Prompt:       prompt,
		Model:        firstNonEmptyNested(request.Work.Model, opts.Model),
		Effort:       firstNonEmptyNested(request.Work.Effort, opts.Effort),
		ReadOnly:     true,
		LogPath:      filepath.Join(evidenceDir, fmt.Sprintf("claim-%d-provider.log", request.ClaimGeneration)),
		Stderr:       stderr,
		HardCap:      nestedProviderHardCap(request.Work.TimeoutSeconds),
		RunID:        request.RunID,
		Role:         "nested-read-only",
		ProviderKey:  request.IdempotencyKey,
	})
}

func runNestedReadOnlyTestSubprocess(ctx context.Context, opts nestedRunOptions, request orchestration.ChildExecutionRequest) (agent.Result, error) {
	return runNestedTestSubprocess(ctx, opts, request, opts.RepoPath, map[string]string{"GIT_OPTIONAL_LOCKS": "0"})
}

func runNestedTestSubprocess(ctx context.Context, opts nestedRunOptions, request orchestration.ChildExecutionRequest, worktreePath string, environment map[string]string) (agent.Result, error) {
	child := request.ChildRunPlan()
	if len(child.Scope.Commands) == 0 {
		return agent.Result{}, fmt.Errorf("test-subprocess child %q requires at least one scope.commands entry", child.ChildKey)
	}
	started := time.Now().UTC()
	result := agent.Result{Model: firstNonEmptyNested(opts.Model, "deterministic-subprocess"), Effort: firstNonEmptyNested(opts.Effort, "none")}
	var output strings.Builder
	executed := false
	for _, command := range child.Scope.Commands {
		command = strings.TrimSpace(command)
		if command == "" {
			continue
		}
		executed = true
		cmd := shellCommand(context.Background(), command)
		cmd.Dir = worktreePath
		cmd.Env = environmentWithOverridesCLI(os.Environ(), environment)
		cmd.Stdout = &output
		cmd.Stderr = &output
		execResult, err := supervisedexec.Run(ctx, cmd, supervisedexec.Options{
			HardCap: nestedSubprocessHardCap(nestedChildMetadata{TimeoutSeconds: request.Work.TimeoutSeconds}),
			RunID:   child.RunID,
			Role:    "nested-test-subprocess",
		})
		result.ExitCode = execResult.ExitCode
		if err != nil {
			result.ExitCode = commandExitCode(err)
			if result.ExitCode == 0 {
				result.ExitCode = 1
			}
			result.Summary = output.String()
			result.StartedAt = state.FormatTimestamp(started)
			result.EndedAt = state.FormatTimestamp(time.Now().UTC())
			return result, err
		}
		if execResult.Killed {
			result.ExitCode = 1
			result.Hung = true
			result.HungReason = "test subprocess exceeded its execution boundary"
			break
		}
		if execResult.ExitCode != 0 {
			break
		}
	}
	if !executed {
		return result, fmt.Errorf("test-subprocess child %q has no non-empty scope.commands entry", child.ChildKey)
	}
	ended := time.Now().UTC()
	result.Summary = output.String()
	result.StartedAt = state.FormatTimestamp(started)
	result.EndedAt = state.FormatTimestamp(ended)
	result.DurationMS = ended.Sub(started).Milliseconds()
	result.Usage.TotalTokens = int64Ptr(0)
	if result.Hung {
		return result, context.DeadlineExceeded
	}
	if result.ExitCode != 0 {
		return result, fmt.Errorf("test subprocess exited with code %d", result.ExitCode)
	}
	return result, nil
}

func nestedResultFromRequest(request orchestration.ChildExecutionRequest) orchestration.ChildRunResult {
	child := request.ChildRunPlan()
	return orchestration.ChildRunResult{
		ID: child.ID, ChildKey: child.ChildKey, Title: child.Title, Role: child.Role,
		RunID: child.RunID, Issue: child.Issue, Scope: child.Scope, Permission: child.Permission,
		DependsOn: append([]string(nil), child.DependsOn...), Aggregation: child.Aggregation,
		Required: child.Required, Optional: child.Optional, Ordinal: child.Ordinal, Depth: child.Depth,
		ReplayAction: child.ReplayAction, ProviderKey: request.IdempotencyKey,
		ContractSchema: request.SchemaVersion, ContractFingerprint: request.ContractFingerprint,
	}
}

func nestedReadOnlyAudit(audit readonlyexec.Audit) *state.ReadOnlyEnforcementAudit {
	violations := make([]state.ReadOnlyEnforcementViolation, 0, len(audit.Violations))
	for _, violation := range audit.Violations {
		violations = append(violations, state.ReadOnlyEnforcementViolation{
			Code: violation.Code, Surface: violation.Surface, TargetID: violation.TargetID,
			BeforeHash: violation.BeforeHash, AfterHash: violation.AfterHash,
		})
	}
	return &state.ReadOnlyEnforcementAudit{
		Mode: audit.Mode, Verification: audit.Verification,
		BaselineFingerprint: audit.BaselineFingerprint, PostRunFingerprint: audit.PostRunFingerprint,
		Recovered: audit.Recovered, Violations: violations,
	}
}

func nestedMutationManifest(audit writeexec.Audit) *state.MutationManifestAudit {
	changes := make([]state.MutationManifestChange, 0, len(audit.Changes))
	for _, change := range audit.Changes {
		changes = append(changes, state.MutationManifestChange{
			Path: change.Path, Kind: change.Kind, BeforeHash: change.BeforeHash, AfterHash: change.AfterHash,
		})
	}
	violations := make([]state.MutationManifestViolation, 0, len(audit.Violations))
	for _, violation := range audit.Violations {
		violations = append(violations, state.MutationManifestViolation{
			Code: violation.Code, Surface: violation.Surface, TargetID: violation.TargetID,
			BeforeHash: violation.BeforeHash, AfterHash: violation.AfterHash,
		})
	}
	return &state.MutationManifestAudit{
		Mode: audit.Mode, Verification: audit.Verification, WorktreeID: audit.WorktreeID,
		BaseRevision: audit.BaseRevision, BaselineFingerprint: audit.BaselineFingerprint,
		PostRunFingerprint: audit.PostRunFingerprint, ManifestFingerprint: audit.ManifestFingerprint,
		Recovered: audit.Recovered, Changes: changes, Violations: violations,
	}
}

func nestedAgentStatus(result agent.Result, err error) string {
	if err != nil {
		return normalizeExecutorFailureStatus(err)
	}
	if result.Hung {
		return orchestration.NestedStatusNeedsHuman
	}
	if result.ExitCode != 0 {
		return orchestration.NestedStatusFailed
	}
	return orchestration.NestedStatusSucceeded
}

func nestedProviderHardCap(timeoutSeconds int) time.Duration {
	if timeoutSeconds <= 0 {
		return supervisedexec.DefaultHardCap
	}
	return time.Duration(timeoutSeconds) * time.Second
}

func nestedPrivateRunKey(runID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(runID)))
	return safeBranchSegment(runID) + "-" + fmt.Sprintf("%x", sum[:6])
}

func nestedPrivateProjectKey(repoPath, projectID string) (string, error) {
	identity := strings.TrimSpace(projectID)
	if identity == "" {
		canonical, err := pathid.Canonicalize(repoPath)
		if err != nil {
			return "", err
		}
		identity = canonical.Identity
	}
	if identity == "" {
		return "", errors.New("project identity is empty")
	}
	sum := sha256.Sum256([]byte(identity))
	return "project-" + fmt.Sprintf("%x", sum[:12]), nil
}

func maxNestedDuration(values ...int64) int64 {
	var maximum int64
	for _, value := range values {
		if value > maximum {
			maximum = value
		}
	}
	return maximum
}

func sanitizeNestedProviderError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("nested read-only provider canceled: %w", context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("nested read-only provider timed out: %w", context.DeadlineExceeded)
	}
	message := boundedNestedText(recovery.Scrub(err.Error()))
	if strings.TrimSpace(message) == "" {
		message = "nested read-only provider failed"
	}
	return errors.New(message)
}

func sanitizeNestedWriteProviderError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("nested bounded-write provider canceled: %w", context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("nested bounded-write provider timed out: %w", context.DeadlineExceeded)
	}
	message := boundedNestedText(recovery.Scrub(err.Error()))
	if strings.TrimSpace(message) == "" {
		message = "nested bounded-write provider failed"
	}
	return errors.New(message)
}

func environmentWithOverride(environ []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(environ)+1)
	for _, entry := range environ {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return append(out, prefix+value)
}

func environmentWithOverridesCLI(environ []string, overrides map[string]string) []string {
	result := append([]string(nil), environ...)
	for key, value := range overrides {
		result = environmentWithOverride(result, key, value)
	}
	return result
}

func completedNestedAttempt(repoPath string, child orchestration.ChildRunPlan, provider string, enforcementOpts readonlyexec.Options) (orchestration.ChildRunResult, bool, error) {
	attempts, err := state.LoadAttempts(repoPath, child.RunID)
	if err != nil {
		return orchestration.ChildRunResult{}, false, err
	}
	for i := len(attempts) - 1; i >= 0; i-- {
		attempt := attempts[i]
		status := state.NormalizeStatus(attempt.Status)
		if status != state.StatusSucceeded || strings.TrimSpace(attempt.Provider) != strings.TrimSpace(provider) || attempt.ReadOnlyEnforcement == nil || attempt.ReadOnlyEnforcement.Mode != readonlyexec.EnforcementMode || attempt.ReadOnlyEnforcement.Verification != readonlyexec.VerificationPassed {
			continue
		}
		verified, err := readonlyexec.ReconcileVerified(enforcementOpts)
		if err != nil {
			return orchestration.ChildRunResult{}, false, err
		}
		verifiedAudit := nestedReadOnlyAudit(verified)
		if !sameNestedReadOnlyAudit(attempt.ReadOnlyEnforcement, verifiedAudit) {
			return orchestration.ChildRunResult{}, false, &readonlyexec.PolicyViolationError{Phase: "reconciliation", Reason: "the successful attempt does not match its private read-only enforcement evidence"}
		}
		result := orchestration.ChildRunResult{
			ID:                  child.ID,
			ChildKey:            child.ChildKey,
			Title:               child.Title,
			Role:                child.Role,
			RunID:               child.RunID,
			Issue:               child.Issue,
			Scope:               child.Scope,
			Permission:          child.Permission,
			DependsOn:           append([]string(nil), child.DependsOn...),
			Aggregation:         child.Aggregation,
			Required:            child.Required,
			Optional:            child.Optional,
			Ordinal:             child.Ordinal,
			Depth:               child.Depth,
			Status:              normalizeExecutorStatus(status),
			ReplayAction:        child.ReplayAction,
			StartedAt:           attempt.StartedAt,
			FinishedAt:          attempt.HeartbeatAt,
			Error:               attempt.Error,
			Reason:              firstNonEmptyNested(attempt.Summary, attempt.Error),
			AttemptPath:         attempt.Path,
			ProviderKey:         child.ProviderKey,
			ProviderReceipt:     attempt.Path,
			RecoveryContextPath: attempt.RecoveryContextPath,
			Report:              attempt.Report,
			ReadOnlyEnforcement: verifiedAudit,
		}
		return result, true, nil
	}
	return orchestration.ChildRunResult{}, false, nil
}

func completedNestedWriteAttempt(repoPath string, child orchestration.ChildRunPlan, provider string, enforcementOpts writeexec.Options) (orchestration.ChildRunResult, bool, error) {
	attempts, err := state.LoadAttempts(repoPath, child.RunID)
	if err != nil {
		return orchestration.ChildRunResult{}, false, err
	}
	for i := len(attempts) - 1; i >= 0; i-- {
		attempt := attempts[i]
		status := state.NormalizeStatus(attempt.Status)
		if status != state.StatusSucceeded || strings.TrimSpace(attempt.Provider) != strings.TrimSpace(provider) || attempt.MutationManifest == nil || attempt.MutationManifest.Mode != writeexec.EnforcementMode || attempt.MutationManifest.Verification != writeexec.VerificationPassed || strings.TrimSpace(attempt.WorktreePath) != strings.TrimSpace(enforcementOpts.WorktreePath) {
			continue
		}
		verified, err := writeexec.ReconcileVerified(context.Background(), enforcementOpts)
		if err != nil {
			return orchestration.ChildRunResult{}, false, err
		}
		verifiedAudit := nestedMutationManifest(verified)
		if !sameNestedMutationManifest(attempt.MutationManifest, verifiedAudit) {
			return orchestration.ChildRunResult{}, false, &writeexec.PolicyViolationError{Phase: "reconciliation", Reason: "the successful attempt does not match its private bounded-write enforcement evidence"}
		}
		result := orchestration.ChildRunResult{
			ID: child.ID, ChildKey: child.ChildKey, Title: child.Title, Role: child.Role,
			RunID: child.RunID, Issue: child.Issue, Scope: child.Scope, Permission: child.Permission,
			DependsOn: append([]string(nil), child.DependsOn...), Aggregation: child.Aggregation,
			Required: child.Required, Optional: child.Optional, Ordinal: child.Ordinal, Depth: child.Depth,
			Status: normalizeExecutorStatus(status), ReplayAction: child.ReplayAction,
			StartedAt: attempt.StartedAt, FinishedAt: attempt.HeartbeatAt,
			Error: attempt.Error, Reason: firstNonEmptyNested(attempt.Summary, attempt.Error),
			AttemptPath: attempt.Path, ProviderKey: child.ProviderKey, ProviderReceipt: attempt.Path,
			RecoveryContextPath: attempt.RecoveryContextPath, Report: attempt.Report,
			MutationManifest: verifiedAudit, WorktreePath: enforcementOpts.WorktreePath,
		}
		return result, true, nil
	}
	return orchestration.ChildRunResult{}, false, nil
}

func sameNestedReadOnlyAudit(left, right *state.ReadOnlyEnforcementAudit) bool {
	if left == nil || right == nil || left.Mode != right.Mode || left.Verification != right.Verification || left.BaselineFingerprint != right.BaselineFingerprint || left.PostRunFingerprint != right.PostRunFingerprint || left.Recovered != right.Recovered || len(left.Violations) != len(right.Violations) {
		return false
	}
	for index := range left.Violations {
		if left.Violations[index] != right.Violations[index] {
			return false
		}
	}
	return true
}

func sameNestedMutationManifest(left, right *state.MutationManifestAudit) bool {
	if left == nil || right == nil || left.Mode != right.Mode || left.Verification != right.Verification || left.WorktreeID != right.WorktreeID || left.BaseRevision != right.BaseRevision || left.BaselineFingerprint != right.BaselineFingerprint || left.PostRunFingerprint != right.PostRunFingerprint || left.ManifestFingerprint != right.ManifestFingerprint || left.Recovered != right.Recovered || len(left.Changes) != len(right.Changes) || len(left.Violations) != len(right.Violations) {
		return false
	}
	for index := range left.Changes {
		if left.Changes[index] != right.Changes[index] {
			return false
		}
	}
	for index := range left.Violations {
		if left.Violations[index] != right.Violations[index] {
			return false
		}
	}
	return true
}

func enforceNestedPlanScope(repoPath, parentPermission string, plan *orchestration.ChildPlan) error {
	parentRepo, err := pathid.Canonicalize(repoPath)
	if err != nil {
		return fmt.Errorf("parent repo: %w", err)
	}
	for i := range plan.Items {
		child := &plan.Items[i]
		if !permissionWithin(parentPermission, child.Permission) {
			return fmt.Errorf("child %q permission %q exceeds parent permission %q", child.ChildKey, child.Permission, parentPermission)
		}
		childRepo, err := resolveNestedChildRepo(parentRepo.Display, child.Scope.Repo)
		if err != nil {
			return fmt.Errorf("child %q scope.repo: %w", child.ChildKey, err)
		}
		if absolutePathOnDifferentVolume(parentRepo.Display, childRepo) {
			return fmt.Errorf("child %q scope.repo %q escapes parent repo %s", child.ChildKey, child.Scope.Repo, repoPath)
		}
		childRepoID, err := pathid.Canonicalize(childRepo)
		if err != nil {
			return fmt.Errorf("child %q scope.repo: %w", child.ChildKey, err)
		}
		if !pathWithin(parentRepo.Identity, childRepoID.Identity) {
			return fmt.Errorf("child %q scope.repo %q escapes parent repo %s", child.ChildKey, child.Scope.Repo, repoPath)
		}
		for _, scopedPath := range child.Scope.Paths {
			resolvedPath, err := resolveNestedScopedPath(childRepoID.Display, scopedPath)
			if err != nil {
				return fmt.Errorf("child %q scope.paths %q: %w", child.ChildKey, scopedPath, err)
			}
			if absolutePathOnDifferentVolume(childRepoID.Display, resolvedPath) || absolutePathOnDifferentVolume(parentRepo.Display, resolvedPath) {
				return fmt.Errorf("child %q scope.paths %q escapes approved repo scope", child.ChildKey, scopedPath)
			}
			scopedID, err := pathid.Canonicalize(resolvedPath)
			if err != nil {
				return fmt.Errorf("child %q scope.paths %q: %w", child.ChildKey, scopedPath, err)
			}
			if !pathWithin(childRepoID.Identity, scopedID.Identity) || !pathWithin(parentRepo.Identity, scopedID.Identity) {
				return fmt.Errorf("child %q scope.paths %q escapes approved repo scope", child.ChildKey, scopedPath)
			}
		}
	}
	return nil
}

func renderNestedRun(w io.Writer, format string, report orchestration.NestedScheduleReport, deps Deps) error {
	if format == "json" {
		data, err := json.MarshalIndent(nestedJSONPayload(report), "", "  ")
		if err != nil {
			return err
		}
		_, err = w.Write(append(data, '\n'))
		return err
	}
	if format == "jsonl" {
		return renderCanonicalMachine(w, nestedObservability(report), "jsonl")
	}
	if _, err := fmt.Fprint(w, renderNestedText(report)); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	return renderCanonicalHuman(w, nestedObservability(report), deps)
}

func renderNestedText(report orchestration.NestedScheduleReport) string {
	var b strings.Builder
	fmt.Fprintln(&b, "NESTED RUN")
	fmt.Fprintf(&b, "ParentRunId: %s\n", report.ParentRunID)
	fmt.Fprintf(&b, "Status: %s\n", report.Status)
	if report.Outcome != "" {
		fmt.Fprintf(&b, "Outcome: %s\n", report.Outcome)
	}
	if report.ExecutorCapability != nil {
		fmt.Fprintf(&b, "Executor: %s registration=%s provider=%s enforceable_permissions=%s provider_native=%t\n",
			reporter.BoundDecisionText(report.ExecutorCapability.ExecutorID),
			reporter.BoundDecisionText(report.ExecutorCapability.RegistrationID),
			reporter.BoundDecisionText(report.ExecutorCapability.Provider),
			reporter.BoundDecisionText(strings.Join(report.ExecutorCapability.EnforceablePermissions, ",")),
			report.ExecutorCapability.ProviderNative,
		)
	}
	fmt.Fprintf(&b, "Children: %d (required=%d optional=%d succeeded=%d failed=%d needs-human=%d cancelled=%d skipped=%d)\n",
		len(report.Children),
		report.Summary.RequiredCount,
		report.Summary.OptionalCount,
		report.Summary.SucceededCount,
		report.Summary.FailedCount,
		report.Summary.NeedsHumanCount,
		report.Summary.CancelledCount,
		report.Summary.SkippedCount,
	)
	for _, child := range report.Children {
		line := fmt.Sprintf("- %s %s %s", child.ChildKey, child.RunID, child.Status)
		if child.ReplayAction != "" {
			line += " action=" + child.ReplayAction
		}
		if child.Outcome != "" {
			line += " outcome=" + child.Outcome
		}
		if child.ClaimOutcome != "" {
			line += " claim=" + child.ClaimOutcome
		}
		if child.ClaimOwner != "" {
			line += " owner=" + child.ClaimOwner
		}
		if child.ClaimGeneration > 0 {
			line += fmt.Sprintf(" generation=%d", child.ClaimGeneration)
		}
		if child.LeaseExpiresAt != "" {
			line += " lease_expires_at=" + child.LeaseExpiresAt
		}
		if child.ClaimPhase != "" {
			line += " phase=" + child.ClaimPhase
		}
		if child.ProviderKey != "" {
			line += " provider_key=" + child.ProviderKey
		}
		if child.ContractSchema != "" {
			line += " contract=" + child.ContractSchema
			if child.ContractFingerprint != "" {
				line += "@" + child.ContractFingerprint
			}
		}
		if child.ReadOnlyEnforcement != nil {
			line += " read_only_mode=" + reporter.BoundDecisionText(child.ReadOnlyEnforcement.Mode)
			line += " read_only_verification=" + reporter.BoundDecisionText(child.ReadOnlyEnforcement.Verification)
			line += " baseline=" + reporter.BoundDecisionText(child.ReadOnlyEnforcement.BaselineFingerprint)
			if child.ReadOnlyEnforcement.PostRunFingerprint != "" {
				line += " post_run=" + reporter.BoundDecisionText(child.ReadOnlyEnforcement.PostRunFingerprint)
			}
			line += fmt.Sprintf(" read_only_violations=%d", len(child.ReadOnlyEnforcement.Violations))
		}
		if child.MutationManifest != nil {
			line += " write_mode=" + reporter.BoundDecisionText(child.MutationManifest.Mode)
			line += " write_verification=" + reporter.BoundDecisionText(child.MutationManifest.Verification)
			line += " base_revision=" + reporter.BoundDecisionText(child.MutationManifest.BaseRevision)
			if child.MutationManifest.ManifestFingerprint != "" {
				line += " manifest=" + reporter.BoundDecisionText(child.MutationManifest.ManifestFingerprint)
			}
			line += fmt.Sprintf(" changes=%d write_violations=%d", len(child.MutationManifest.Changes), len(child.MutationManifest.Violations))
		}
		if child.WorktreePath != "" {
			line += " worktree=" + reporter.BoundDecisionText(child.WorktreePath)
		}
		if child.Error != "" {
			line += " error=" + reporter.BoundDecisionText(child.Error)
		}
		if child.Reason != "" {
			line += " reason=" + reporter.BoundDecisionText(child.Reason)
		}
		if child.NextAction != "" {
			line += " next_action=" + reporter.BoundDecisionText(child.NextAction)
		}
		fmt.Fprintln(&b, line)
		if child.ReadOnlyEnforcement != nil {
			for _, violation := range child.ReadOnlyEnforcement.Violations {
				fmt.Fprintf(&b, "  policy_violation code=%s surface=%s target=%s before=%s after=%s\n",
					reporter.BoundDecisionText(violation.Code), reporter.BoundDecisionText(violation.Surface),
					reporter.BoundDecisionText(violation.TargetID), reporter.BoundDecisionText(violation.BeforeHash),
					reporter.BoundDecisionText(violation.AfterHash))
			}
		}
		if child.MutationManifest != nil {
			for _, change := range child.MutationManifest.Changes {
				fmt.Fprintf(&b, "  mutation path=%s kind=%s before=%s after=%s\n",
					reporter.BoundDecisionText(change.Path), reporter.BoundDecisionText(change.Kind),
					reporter.BoundDecisionText(change.BeforeHash), reporter.BoundDecisionText(change.AfterHash))
			}
			for _, violation := range child.MutationManifest.Violations {
				fmt.Fprintf(&b, "  write_policy_violation code=%s surface=%s target=%s before=%s after=%s\n",
					reporter.BoundDecisionText(violation.Code), reporter.BoundDecisionText(violation.Surface),
					reporter.BoundDecisionText(violation.TargetID), reporter.BoundDecisionText(violation.BeforeHash),
					reporter.BoundDecisionText(violation.AfterHash))
			}
		}
	}
	for _, refusal := range report.Refusals {
		fmt.Fprintf(&b, "Refusal: child=%s code=%s permission=%s capability_result=%s provider_native=%t next_action=%s\n",
			reporter.BoundDecisionText(refusal.ChildKey),
			reporter.BoundDecisionText(refusal.Code),
			reporter.BoundDecisionText(refusal.RequestedPermission),
			reporter.BoundDecisionText(refusal.CapabilityResult),
			refusal.ProviderNativeRequested,
			reporter.BoundDecisionText(refusal.NextAction),
		)
	}
	switch report.Status {
	case orchestration.NestedStatusSucceeded:
		fmt.Fprintln(&b, "Next: review child reports and continue parent aggregation.")
	case orchestration.NestedStatusNeedsHuman:
		fmt.Fprintln(&b, "Next: inspect needs-human child records before resuming the parent.")
	default:
		fmt.Fprintln(&b, "Next: inspect failed child records, then rerun the same plan_id to reuse succeeded children and resume/retry durable children without duplicating them.")
	}
	return b.String()
}

func writeNestedAttempt(repoPath string, child orchestration.ChildRunPlan, provider string, record reporter.Report, status string, exitCode int, summary, errText string, enforcement *state.ReadOnlyEnforcementAudit, now func() time.Time) (string, error) {
	if now == nil {
		now = time.Now
	}
	at := state.FormatTimestamp(now())
	var errPtr *string
	if strings.TrimSpace(errText) != "" {
		errCopy := strings.TrimSpace(errText)
		errPtr = &errCopy
	}
	return state.WriteAttempt(repoPath, child.RunID, state.AttemptRecord{
		Version:             1,
		JobID:               "job-" + child.ChildKey + "-nested-read-only",
		Issue:               child.Issue,
		Attempt:             1,
		Provider:            strings.TrimSpace(provider),
		PID:                 os.Getpid(),
		Phase:               "nested_read_only_verified",
		Status:              status,
		Branch:              "",
		StartedAt:           record.StartedAt,
		HeartbeatAt:         at,
		LastProgressAt:      at,
		LogBytes:            int64(len(summary)),
		ExitCode:            &exitCode,
		Error:               errPtr,
		Summary:             strings.TrimSpace(summary),
		Usage:               &record.Usage,
		Report:              &record,
		ReadOnlyEnforcement: enforcement,
	})
}

func writeNestedWriteAttempt(repoPath string, child orchestration.ChildRunPlan, claimGeneration int64, provider string, record reporter.Report, status string, exitCode int, summary, errText string, manifest *state.MutationManifestAudit, worktreePath string, now func() time.Time) (string, error) {
	if now == nil {
		now = time.Now
	}
	at := state.FormatTimestamp(now())
	var errPtr *string
	if strings.TrimSpace(errText) != "" {
		errCopy := strings.TrimSpace(errText)
		errPtr = &errCopy
	}
	return state.WriteAttempt(repoPath, child.RunID, state.AttemptRecord{
		Version:          1,
		JobID:            fmt.Sprintf("job-%s-nested-write-claim-%d", child.ChildKey, claimGeneration),
		Issue:            child.Issue,
		Attempt:          1,
		Provider:         strings.TrimSpace(provider),
		PID:              os.Getpid(),
		Phase:            "nested_write_verified",
		Status:           status,
		Branch:           "",
		StartedAt:        record.StartedAt,
		HeartbeatAt:      at,
		LastProgressAt:   at,
		LogBytes:         int64(len(summary)),
		ExitCode:         &exitCode,
		Error:            errPtr,
		Summary:          strings.TrimSpace(summary),
		Usage:            &record.Usage,
		Report:           &record,
		MutationManifest: manifest,
		WorktreePath:     strings.TrimSpace(worktreePath),
	})
}

func boundedNestedText(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= nestedPromptBudgetBytes {
		return value
	}
	return value[:nestedPromptBudgetBytes] + "\n[loopcoder] nested child context truncated"
}

func safeBranchSegment(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if valid {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if b.Len() == 0 || lastDash {
			continue
		}
		b.WriteByte('-')
		lastDash = true
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "child"
	}
	if len(out) > 48 {
		out = strings.TrimRight(out[:48], "-")
	}
	if out == "" {
		return "child"
	}
	return out
}

func normalizeNestedPermission(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "read":
		return string(reporter.PermissionReadOnly)
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func validNestedParentPermission(value string) bool {
	switch reporter.Permission(normalizeNestedPermission(value)) {
	case reporter.PermissionReadOnly, reporter.PermissionWrite, reporter.PermissionOrchestrate:
		return true
	default:
		return false
	}
}

func permissionWithin(parent, child string) bool {
	return permissionRank(normalizeNestedPermission(child)) <= permissionRank(normalizeNestedPermission(parent))
}

func permissionRank(value string) int {
	switch reporter.Permission(value) {
	case reporter.PermissionReadOnly:
		return 1
	case reporter.PermissionWrite:
		return 2
	case reporter.PermissionOrchestrate:
		return 3
	default:
		return 99
	}
}

func resolveNestedChildRepo(repoPath, childRepo string) (string, error) {
	childRepo = strings.TrimSpace(childRepo)
	if childRepo == "" || childRepo == "." {
		return filepath.Abs(repoPath)
	}
	if filepath.IsAbs(childRepo) {
		return filepath.Abs(childRepo)
	}
	return filepath.Abs(filepath.Join(repoPath, childRepo))
}

func resolveNestedScopedPath(childRepo, scopedPath string) (string, error) {
	scopedPath = strings.TrimSpace(scopedPath)
	if scopedPath == "" {
		return "", fmt.Errorf("path is required")
	}
	if filepath.IsAbs(scopedPath) {
		return filepath.Abs(scopedPath)
	}
	return filepath.Abs(filepath.Join(childRepo, scopedPath))
}

func pathWithin(parent, child string) bool {
	parent = filepath.Clean(parent)
	child = filepath.Clean(child)
	if runtime.GOOS == "windows" {
		parent = strings.ToLower(parent)
		child = strings.ToLower(child)
	}
	if parent == child {
		return true
	}
	return strings.HasPrefix(child, parent+string(filepath.Separator))
}

func absolutePathOnDifferentVolume(base, candidate string) bool {
	if runtime.GOOS != "windows" || !filepath.IsAbs(candidate) {
		return false
	}
	baseVolume := filepath.VolumeName(filepath.Clean(base))
	candidateVolume := filepath.VolumeName(filepath.Clean(candidate))
	return baseVolume != "" && candidateVolume != "" && !strings.EqualFold(baseVolume, candidateVolume)
}

func shellCommand(ctx context.Context, command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", command)
	}
	return exec.CommandContext(ctx, "sh", "-c", command)
}

func normalizeExecutorFailureStatus(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return orchestration.NestedStatusCancelled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return orchestration.NestedStatusTimedOut
	}
	return orchestration.NestedStatusFailed
}

func nestedSubprocessHardCap(metadata nestedChildMetadata) time.Duration {
	if metadata.TimeoutSeconds <= 0 {
		return supervisedexec.DefaultHardCap
	}
	return time.Duration(metadata.TimeoutSeconds+1) * time.Second
}

func normalizeExecutorStatus(status string) string {
	switch state.NormalizeStatus(status) {
	case state.StatusSucceeded:
		return orchestration.NestedStatusSucceeded
	case state.StatusCancelled:
		return orchestration.NestedStatusCancelled
	case state.StatusTimedOut:
		return orchestration.NestedStatusTimedOut
	case state.StatusNeedsHuman, state.StatusHung:
		return orchestration.NestedStatusNeedsHuman
	case state.StatusFailed:
		return orchestration.NestedStatusFailed
	default:
		return strings.TrimSpace(status)
	}
}

func commandExitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}

func int64Ptr(value int64) *int64 {
	return &value
}

func firstNonEmptyNested(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
