package cli

import (
	"context"
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

	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/orchestration"
	"github.com/jasonhnd/loopcoder/internal/pathid"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/worker"
)

const (
	nestedTestSubprocessProvider = "test-subprocess"
	nestedPromptBudgetBytes      = 16 * 1024
)

type nestedRunOptions struct {
	RepoPath         string
	PlanPath         string
	BaseBranch       string
	Provider         string
	Model            string
	Effort           string
	ParentPermission string
	Format           string
	Timeout          time.Duration
	ConfigFromBase   bool
	Strict           bool
}

type nestedChildMetadata struct {
	IssueBody      string `json:"issue_body,omitempty"`
	Prompt         string `json:"prompt,omitempty"`
	Branch         string `json:"branch,omitempty"`
	Provider       string `json:"provider,omitempty"`
	Model          string `json:"model,omitempty"`
	Effort         string `json:"effort,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
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
	fmt.Fprintln(w, "Submit a v1 child plan, persist the durable graph, and execute children through loopcoder-owned scheduling.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	fmt.Fprintln(w, "  --repo string                 repository path (required)")
	fmt.Fprintln(w, "  --plan string                 child plan JSON file (required)")
	fmt.Fprintln(w, "  --base-branch string          base branch for worker children (default \"main\")")
	fmt.Fprintln(w, "  --provider string             worker provider (default from .delivery.yml or \"codex\")")
	fmt.Fprintln(w, "  --model string                optional worker model override for this run")
	fmt.Fprintln(w, "  --effort string               optional worker reasoning effort override for this run")
	fmt.Fprintln(w, "  --parent-permission string    parent permission ceiling: read-only, write, or orchestrate (default \"orchestrate\")")
	fmt.Fprintln(w, "  --format string               output format: text or json (default \"text\")")
	fmt.Fprintln(w, "  --timeout duration            optional timeout for the nested run, for example 30s or 5m")
	fmt.Fprintln(w, "  --config-from-base            read .delivery.yml from base branch when absent from working tree")
	fmt.Fprintln(w, "  --strict                      reject invalid model/depth selections instead of warning")
	fmt.Fprintln(w, "  --help                        show nested command help")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  provider %q is reserved for deterministic local smoke tests and executes scope.commands as subprocesses.\n", nestedTestSubprocessProvider)
}

func runNestedRun(args []string, stdout, stderr io.Writer, deps Deps) int {
	defaults := DefaultDeps()
	if deps.Dispatch == nil {
		deps.Dispatch = defaults.Dispatch
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
	case "text", "json":
	default:
		fmt.Fprintf(stderr, "nested run: invalid --format %q; want text or json\n", opts.Format)
		return 2
	}
	warnings := stderr
	if opts.Format == "json" {
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
		fmt.Fprintf(stderr, "nested run: %v\n", err)
		return 2
	}
	if err := enforceNestedPlanScope(resolvedRepo, parentPermission, &plan); err != nil {
		fmt.Fprintf(stderr, "nested run: %v\n", err)
		return 2
	}

	cfg, err := loadDeliveryConfig(resolvedRepo, opts.BaseBranch, opts.ConfigFromBase)
	if err != nil {
		fmt.Fprintf(stderr, "nested run: %v\n", err)
		return 1
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
	} else {
		opts.Model = firstNonEmptyNested(opts.Model, "deterministic-subprocess")
		opts.Effort = firstNonEmptyNested(opts.Effort, "none")
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

	executor := nestedDispatchExecutor(opts, deps, warnings)
	if opts.Provider == nestedTestSubprocessProvider {
		executor = nestedSubprocessExecutor(opts, deps, warnings)
	}
	progressRecorder, stopProgress := progressSupervisorForRegisteredRepo(context.Background(), resolvedRepo, plan.RootRunID, deps.Now, stderr)
	defer func() {
		if err := stopProgress(); err != nil {
			fmt.Fprintf(stderr, "nested run: progress receipt shutdown: %v\n", err)
		}
	}()

	report, err := orchestration.ScheduleNestedRuns(ctx, orchestration.NestedScheduleOptions{
		RepoPath:         resolvedRepo,
		BaseBranch:       opts.BaseBranch,
		Plan:             &plan,
		Store:            store,
		Budget:           cfg.Guardrails.Budget,
		CircuitBreaker:   cfg.Guardrails.CircuitBreaker,
		ConcurrencyLimit: plan.MaxConcurrency,
		MaxDepth:         plan.MaxDepth,
		Progress:         progressRecorder,
		Execute:          executor,
	})
	if err != nil {
		if nestedReportHasContent(report) {
			if renderErr := renderNestedRun(stdout, opts.Format, report); renderErr != nil {
				fmt.Fprintf(stderr, "nested run: write output after failure: %v\n", renderErr)
			}
		}
		if opts.Format != "json" || !nestedReportHasContent(report) {
			fmt.Fprintf(stderr, "nested run: %v\n", err)
		}
		return 1
	}
	if err := renderNestedRun(stdout, opts.Format, report); err != nil {
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

func nestedDispatchExecutor(opts nestedRunOptions, deps Deps, stderr io.Writer) orchestration.ChildRunExecutor {
	return func(ctx context.Context, child orchestration.ChildRunPlan) (orchestration.ChildRunResult, error) {
		if existing, ok, err := completedNestedAttempt(opts.RepoPath, child); err != nil {
			return orchestration.ChildRunResult{}, err
		} else if ok {
			return existing, nil
		}
		if child.Permission == string(reporter.PermissionWrite) || child.Permission == string(reporter.PermissionOrchestrate) {
			return orchestration.ChildRunResult{}, fmt.Errorf("provider dispatch executor cannot enforce scoped writes for child %q with permission %q", child.ChildKey, child.Permission)
		}
		if child.Issue <= 0 {
			return orchestration.ChildRunResult{}, fmt.Errorf("child %q requires a positive issue or scope issue for worker dispatch", child.ChildKey)
		}
		metadata, err := decodeNestedChildMetadata(child.Metadata)
		if err != nil {
			return orchestration.ChildRunResult{}, err
		}
		dispatchCtx := ctx
		cancel := func() {}
		if metadata.TimeoutSeconds > 0 {
			dispatchCtx, cancel = context.WithTimeout(ctx, time.Duration(metadata.TimeoutSeconds)*time.Second)
		}
		defer cancel()

		provider := firstNonEmptyNested(metadata.Provider, opts.Provider)
		result, err := deps.Dispatch(dispatchCtx, worker.Options{
			RepoPath:        opts.RepoPath,
			IssueNumber:     child.Issue,
			IssueTitle:      child.Title,
			IssueBody:       nestedChildIssueBody(opts, child, metadata),
			BaseBranch:      opts.BaseBranch,
			Branch:          firstNonEmptyNested(metadata.Branch, nestedChildBranch(child)),
			RunID:           child.RunID,
			ProviderKey:     child.ProviderKey,
			Attempt:         1,
			Provider:        provider,
			Model:           firstNonEmptyNested(metadata.Model, opts.Model),
			Effort:          firstNonEmptyNested(metadata.Effort, opts.Effort),
			ConfigFromBase:  opts.ConfigFromBase,
			RecoveryContext: "",
			Stderr:          stderr,
		})
		if err != nil {
			return orchestration.ChildRunResult{}, err
		}
		return childResultFromWorker(child, result), nil
	}
}

func nestedSubprocessExecutor(opts nestedRunOptions, deps Deps, stderr io.Writer) orchestration.ChildRunExecutor {
	return func(ctx context.Context, child orchestration.ChildRunPlan) (orchestration.ChildRunResult, error) {
		if existing, ok, err := completedNestedAttempt(opts.RepoPath, child); err != nil {
			return orchestration.ChildRunResult{}, err
		} else if ok {
			return existing, nil
		}
		if len(child.Scope.Commands) == 0 {
			return orchestration.ChildRunResult{}, fmt.Errorf("test-subprocess child %q requires at least one scope.commands entry", child.ChildKey)
		}
		metadata, err := decodeNestedChildMetadata(child.Metadata)
		if err != nil {
			return orchestration.ChildRunResult{}, err
		}
		runCtx := ctx
		cancel := func() {}
		if metadata.TimeoutSeconds > 0 {
			runCtx, cancel = context.WithTimeout(ctx, time.Duration(metadata.TimeoutSeconds)*time.Second)
		}
		defer cancel()

		started := deps.Now().UTC()
		status := orchestration.NestedStatusSucceeded
		exitCode := 0
		var output strings.Builder
		for _, command := range child.Scope.Commands {
			command = strings.TrimSpace(command)
			if command == "" {
				continue
			}
			cmd := shellCommand(runCtx, command)
			cmd.Dir = opts.RepoPath
			out, err := cmd.CombinedOutput()
			output.Write(out)
			if err != nil {
				status = normalizeExecutorFailureStatus(err)
				exitCode = commandExitCode(err)
				if exitCode == 0 {
					exitCode = 1
				}
				break
			}
		}
		ended := deps.Now().UTC()
		summary := strings.TrimSpace(output.String())
		if summary == "" {
			summary = "test subprocess completed"
		}
		if len(summary) > nestedPromptBudgetBytes {
			summary = summary[:nestedPromptBudgetBytes] + "\n[loopcoder] subprocess output truncated"
		}
		reportRecord := reporter.Report{
			WorkID:      child.RunID,
			Issue:       child.Issue,
			Role:        reporter.RoleWorker,
			Provider:    nestedTestSubprocessProvider,
			Model:       firstNonEmptyNested(opts.Model, "deterministic-subprocess"),
			ModelSource: reporter.ModelSourceSelfReported,
			Effort:      firstNonEmptyNested(opts.Effort, "none"),
			Permission:  reporter.Permission(child.Permission),
			Action:      "execute nested child " + child.ChildKey,
			ExitCode:    exitCode,
			StartedAt:   state.FormatTimestamp(started),
			EndedAt:     state.FormatTimestamp(ended),
			DurationMS:  ended.Sub(started).Milliseconds(),
			Usage: reporter.Usage{
				TotalTokens: int64Ptr(0),
			},
			Verified: true,
		}
		attemptStatus := status
		errText := ""
		if status != orchestration.NestedStatusSucceeded {
			errText = summary
		}
		attemptPath, writeErr := writeNestedAttempt(opts.RepoPath, child, reportRecord, attemptStatus, exitCode, summary, errText, deps.Now)
		if writeErr != nil {
			fmt.Fprintf(stderr, "[loopcoder] warning: failed to write nested subprocess attempt: %v\n", writeErr)
		}
		result := orchestration.ChildRunResult{
			ID:              child.ID,
			ChildKey:        child.ChildKey,
			Title:           child.Title,
			Role:            child.Role,
			RunID:           child.RunID,
			Issue:           child.Issue,
			Scope:           child.Scope,
			Permission:      child.Permission,
			DependsOn:       append([]string(nil), child.DependsOn...),
			Aggregation:     child.Aggregation,
			Required:        child.Required,
			Optional:        child.Optional,
			Ordinal:         child.Ordinal,
			Depth:           child.Depth,
			Status:          status,
			ReplayAction:    child.ReplayAction,
			StartedAt:       reportRecord.StartedAt,
			FinishedAt:      reportRecord.EndedAt,
			AttemptPath:     attemptPath,
			ProviderKey:     child.ProviderKey,
			ProviderReceipt: attemptPath,
			Report:          &reportRecord,
		}
		if status != orchestration.NestedStatusSucceeded {
			result.Error = summary
			result.Reason = summary
			result.NextAction = "inspect the failed nested child and rerun the same plan after recovery"
			return result, errors.New(summary)
		}
		return result, nil
	}
}

func completedNestedAttempt(repoPath string, child orchestration.ChildRunPlan) (orchestration.ChildRunResult, bool, error) {
	attempts, err := state.LoadAttempts(repoPath, child.RunID)
	if err != nil {
		return orchestration.ChildRunResult{}, false, err
	}
	for i := len(attempts) - 1; i >= 0; i-- {
		attempt := attempts[i]
		status := state.NormalizeStatus(attempt.Status)
		if status != state.StatusSucceeded {
			continue
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
			Reason:              attempt.Error,
			AttemptPath:         attempt.Path,
			ProviderKey:         child.ProviderKey,
			ProviderReceipt:     attempt.Path,
			RecoveryContextPath: attempt.RecoveryContextPath,
			Report:              attempt.Report,
		}
		return result, true, nil
	}
	return orchestration.ChildRunResult{}, false, nil
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

func renderNestedRun(w io.Writer, format string, report orchestration.NestedScheduleReport) error {
	if format == "json" {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		_, err = w.Write(append(data, '\n'))
		return err
	}
	_, err := fmt.Fprint(w, renderNestedText(report))
	return err
}

func renderNestedText(report orchestration.NestedScheduleReport) string {
	var b strings.Builder
	fmt.Fprintln(&b, "NESTED RUN")
	fmt.Fprintf(&b, "ParentRunId: %s\n", report.ParentRunID)
	fmt.Fprintf(&b, "Status: %s\n", report.Status)
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

func childResultFromWorker(child orchestration.ChildRunPlan, result worker.Result) orchestration.ChildRunResult {
	status := normalizeExecutorStatus(result.Status)
	if status == "" {
		if result.OK {
			status = orchestration.NestedStatusSucceeded
		} else {
			status = orchestration.NestedStatusFailed
		}
	}
	runID := firstNonEmptyNested(result.RunID, child.RunID)
	childResult := orchestration.ChildRunResult{
		ID:              child.ID,
		ChildKey:        child.ChildKey,
		Title:           child.Title,
		Role:            child.Role,
		RunID:           runID,
		Issue:           result.Issue,
		Scope:           child.Scope,
		Permission:      child.Permission,
		DependsOn:       append([]string(nil), child.DependsOn...),
		Aggregation:     child.Aggregation,
		Required:        child.Required,
		Optional:        child.Optional,
		Ordinal:         child.Ordinal,
		Depth:           child.Depth,
		Status:          status,
		ReplayAction:    child.ReplayAction,
		Error:           "",
		Reason:          result.Reason,
		NextAction:      result.NextAction,
		AttemptPath:     result.AttemptPath,
		ProviderKey:     child.ProviderKey,
		ProviderReceipt: firstNonEmptyNested(result.PR, result.AttemptPath),
		Report:          result.Report,
	}
	if status != orchestration.NestedStatusSucceeded {
		receipt := reporter.NormalizeDecision(reporter.DecisionInput{
			Status:             status,
			ExplicitReason:     childResult.Reason,
			ConcreteError:      result.Summary,
			ExplicitNextAction: childResult.NextAction,
		})
		childResult.Reason = receipt.Reason
		childResult.NextAction = receipt.NextAction
	}
	return childResult
}

func writeNestedAttempt(repoPath string, child orchestration.ChildRunPlan, record reporter.Report, status string, exitCode int, summary, errText string, now func() time.Time) (string, error) {
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
		Version:        1,
		JobID:          "job-" + child.ChildKey + "-test-subprocess",
		Issue:          child.Issue,
		Attempt:        1,
		Provider:       nestedTestSubprocessProvider,
		PID:            os.Getpid(),
		Phase:          "nested_subprocess_exited",
		Status:         status,
		Branch:         nestedChildBranch(child),
		StartedAt:      record.StartedAt,
		HeartbeatAt:    at,
		LastProgressAt: at,
		LogBytes:       int64(len(summary)),
		ExitCode:       &exitCode,
		Error:          errPtr,
		Usage:          &record.Usage,
		Report:         &record,
	})
}

func nestedChildIssueBody(opts nestedRunOptions, child orchestration.ChildRunPlan, metadata nestedChildMetadata) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Nested child generated by loopcoder.\n\nBase branch: %s\nChild key: %s\nPermission: %s\nScope: %s\n\n",
		opts.BaseBranch,
		child.ChildKey,
		child.Permission,
		childScopeSummary(child.Scope),
	)
	if strings.TrimSpace(metadata.IssueBody) != "" {
		b.WriteString(boundedNestedText(metadata.IssueBody))
	} else if strings.TrimSpace(metadata.Prompt) != "" {
		b.WriteString(boundedNestedText(metadata.Prompt))
	} else {
		fmt.Fprintf(&b, "Implement the nested child task: %s\n", child.Title)
	}
	if child.Permission == string(reporter.PermissionOrchestrate) {
		fmt.Fprintf(&b, "\n\nNested policy: loopcoder owns child identity, spawning, permission, persistence, budget, and recovery. Do not use provider-native sub-agents outside a loopcoder child plan; max_depth=%d.\n", lcdefaults.NestedSchedulerMaxDepth)
	}
	_ = opts
	return b.String()
}

func childScopeSummary(scope orchestration.ChildScope) string {
	data, err := json.Marshal(scope)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func decodeNestedChildMetadata(raw json.RawMessage) (nestedChildMetadata, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nestedChildMetadata{}, nil
	}
	var metadata nestedChildMetadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nestedChildMetadata{}, fmt.Errorf("decode child metadata: %w", err)
	}
	metadata.IssueBody = boundedNestedText(metadata.IssueBody)
	metadata.Prompt = boundedNestedText(metadata.Prompt)
	return metadata, nil
}

func boundedNestedText(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= nestedPromptBudgetBytes {
		return value
	}
	return value[:nestedPromptBudgetBytes] + "\n[loopcoder] nested child context truncated"
}

func nestedChildBranch(child orchestration.ChildRunPlan) string {
	issue := child.Issue
	if issue <= 0 {
		issue = 1
	}
	return fmt.Sprintf("loop/issue-%d-%s", issue, safeBranchSegment(child.ChildKey))
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
