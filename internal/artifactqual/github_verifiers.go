package artifactqual

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// GHCommandRunner executes `gh` argv (no shell). Tests inject fakes.
type GHCommandRunner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// ProductionGHCommandRunner runs `gh` via exec.CommandContext.
type ProductionGHCommandRunner struct{}

// Run executes gh with the given args under ctx. Stdout is returned; stderr is not.
func (ProductionGHCommandRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("gh exit=%d", ee.ExitCode())
		}
		return nil, fmt.Errorf("gh invoke failed")
	}
	return out, nil
}

// DefaultGHTimeout is the default bound for GitHub evidence fetches.
const DefaultGHTimeout = 30 * time.Second

// GitHubEvidenceVerifier implements PreProdActionsVerifier and RCActionsVerifier
// via `gh api` JSON fetches. Never trusts caller-supplied receipt schema.
type GitHubEvidenceVerifier struct {
	Runner  GHCommandRunner
	Timeout time.Duration
}

// ownerRepoPattern is owner/repo with no path segments or spaces.
// Each side is a non-empty GitHub name; ".." / leading dots rejected separately.
var ownerRepoPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func validateOwnerRepo(repository string) (string, error) {
	repo := strings.TrimSpace(repository)
	if repo == "" || strings.Count(repo, "/") != 1 || !ownerRepoPattern.MatchString(repo) {
		return "", fmt.Errorf("artifactqual: invalid repository")
	}
	owner, name, _ := strings.Cut(repo, "/")
	if owner == "." || owner == ".." || name == "." || name == ".." ||
		strings.Contains(owner, "..") || strings.Contains(name, "..") {
		return "", fmt.Errorf("artifactqual: invalid repository")
	}
	return repo, nil
}

func (v *GitHubEvidenceVerifier) runner() GHCommandRunner {
	if v != nil && v.Runner != nil {
		return v.Runner
	}
	return ProductionGHCommandRunner{}
}

func (v *GitHubEvidenceVerifier) timeout() time.Duration {
	if v != nil && v.Timeout > 0 {
		return v.Timeout
	}
	return DefaultGHTimeout
}

func (v *GitHubEvidenceVerifier) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, v.timeout())
}

func (v *GitHubEvidenceVerifier) ghAPI(ctx context.Context, path string) ([]byte, error) {
	out, err := v.runner().Run(ctx, "api", path)
	if err != nil {
		// Never wrap or surface runner err text (may contain tokens/raw stderr).
		return nil, fmt.Errorf("artifactqual: gh api %s failed", redactAPIPath(path))
	}
	return out, nil
}

// redactAPIPath keeps only structural segments (no query secrets).
func redactAPIPath(path string) string {
	// Drop query string if any.
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	return path
}

// FetchRun implements PreProdActionsVerifier.
func (v *GitHubEvidenceVerifier) FetchRun(ctx context.Context, repository string, runID int64, attempt int) (PreProdActionsReceipt, error) {
	var zero PreProdActionsReceipt
	repo, err := validateOwnerRepo(repository)
	if err != nil {
		return zero, err
	}
	if runID <= 0 {
		return zero, fmt.Errorf("artifactqual: invalid run_id")
	}
	if attempt < 1 {
		return zero, fmt.Errorf("artifactqual: invalid attempt")
	}

	ctx, cancel := v.withTimeout(ctx)
	defer cancel()

	runPath := fmt.Sprintf("repos/%s/actions/runs/%d", repo, runID)
	runRaw, err := v.ghAPI(ctx, runPath)
	if err != nil {
		return zero, fmt.Errorf("artifactqual: fetch preprod run: %w", err)
	}
	var run ghActionsRunJSON
	if err := json.Unmarshal(runRaw, &run); err != nil {
		return zero, fmt.Errorf("artifactqual: parse preprod run json failed")
	}
	if run.ID != runID {
		return zero, fmt.Errorf("artifactqual: preprod run id mismatch")
	}
	if run.RunAttempt != attempt {
		return zero, fmt.Errorf("artifactqual: preprod run attempt mismatch")
	}
	gotRepo := strings.TrimSpace(run.Repository.FullName)
	if gotRepo == "" || !strings.EqualFold(gotRepo, repo) {
		return zero, fmt.Errorf("artifactqual: preprod run repository mismatch")
	}

	jobsPath := fmt.Sprintf("repos/%s/actions/runs/%d/attempts/%d/jobs?per_page=100", repo, runID, attempt)
	jobsRaw, err := v.ghAPI(ctx, jobsPath)
	if err != nil {
		return zero, fmt.Errorf("artifactqual: fetch preprod jobs: %w", err)
	}
	var jobsWrap ghActionsJobsJSON
	if err := json.Unmarshal(jobsRaw, &jobsWrap); err != nil {
		return zero, fmt.Errorf("artifactqual: parse preprod jobs json failed")
	}

	jobs := make([]PreProdActionsJob, 0, len(jobsWrap.Jobs))
	for _, j := range jobsWrap.Jobs {
		jobs = append(jobs, PreProdActionsJob{
			Name:       j.Name,
			Status:     j.Status,
			Conclusion: j.Conclusion,
		})
	}

	return PreProdActionsReceipt{
		Schema:       SchemaPreProdActionsReceipt,
		Repository:   gotRepo,
		WorkflowPath: strings.TrimSpace(run.Path),
		RunID:        run.ID,
		Attempt:      run.RunAttempt,
		Event:        strings.TrimSpace(run.Event),
		HeadBranch:   strings.TrimSpace(run.HeadBranch),
		HeadSHA:      strings.TrimSpace(run.HeadSHA),
		Status:       strings.TrimSpace(run.Status),
		Conclusion:   strings.TrimSpace(run.Conclusion),
		Jobs:         jobs,
	}, nil
}

// FetchRCBinding implements RCActionsVerifier.
func (v *GitHubEvidenceVerifier) FetchRCBinding(ctx context.Context, repository string, runID, artifactID int64) (RCActionsBinding, error) {
	var zero RCActionsBinding
	repo, err := validateOwnerRepo(repository)
	if err != nil {
		return zero, err
	}
	if runID <= 0 {
		return zero, fmt.Errorf("artifactqual: invalid run_id")
	}
	if artifactID <= 0 {
		return zero, fmt.Errorf("artifactqual: invalid artifact_id")
	}

	ctx, cancel := v.withTimeout(ctx)
	defer cancel()

	runPath := fmt.Sprintf("repos/%s/actions/runs/%d", repo, runID)
	runRaw, err := v.ghAPI(ctx, runPath)
	if err != nil {
		return zero, fmt.Errorf("artifactqual: fetch rc run: %w", err)
	}
	var run ghActionsRunJSON
	if err := json.Unmarshal(runRaw, &run); err != nil {
		return zero, fmt.Errorf("artifactqual: parse rc run json failed")
	}
	if run.ID != runID {
		return zero, fmt.Errorf("artifactqual: rc run id mismatch")
	}
	gotRepo := strings.TrimSpace(run.Repository.FullName)
	if gotRepo == "" || !strings.EqualFold(gotRepo, repo) {
		return zero, fmt.Errorf("artifactqual: rc run repository mismatch")
	}

	artPath := fmt.Sprintf("repos/%s/actions/runs/%d/artifacts?per_page=100", repo, runID)
	artRaw, err := v.ghAPI(ctx, artPath)
	if err != nil {
		return zero, fmt.Errorf("artifactqual: fetch rc artifacts: %w", err)
	}
	var arts ghActionsArtifactsJSON
	if err := json.Unmarshal(artRaw, &arts); err != nil {
		return zero, fmt.Errorf("artifactqual: parse rc artifacts json failed")
	}

	var found *ghActionsArtifactJSON
	for i := range arts.Artifacts {
		if arts.Artifacts[i].ID == artifactID {
			found = &arts.Artifacts[i]
			break
		}
	}
	if found == nil {
		return zero, fmt.Errorf("artifactqual: rc artifact id not found")
	}
	if found.WorkflowRun.ID != 0 && found.WorkflowRun.ID != runID {
		return zero, fmt.Errorf("artifactqual: rc artifact workflow_run mismatch")
	}

	wfName := strings.TrimSpace(run.Name)
	if wfName == "" {
		wfName = strings.TrimSpace(run.DisplayTitle)
	}

	return RCActionsBinding{
		Repository:      gotRepo,
		WorkflowName:    wfName,
		RunID:           run.ID,
		RunAttempt:      run.RunAttempt,
		ArtifactID:      found.ID,
		ArtifactName:    strings.TrimSpace(found.Name),
		ArtifactExpired: found.Expired,
		HeadSHA:         strings.TrimSpace(run.HeadSHA),
		Status:          strings.TrimSpace(run.Status),
		Conclusion:      strings.TrimSpace(run.Conclusion),
	}, nil
}

// FetchPR implements PRLiveVerifier via gh api (pull + check-runs at head).
func (v *GitHubEvidenceVerifier) FetchPR(ctx context.Context, repository string, number int) (PRLiveState, error) {
	var zero PRLiveState
	repo, err := validateOwnerRepo(repository)
	if err != nil {
		return zero, err
	}
	if number <= 0 {
		return zero, fmt.Errorf("artifactqual: invalid pr number")
	}

	ctx, cancel := v.withTimeout(ctx)
	defer cancel()

	prPath := fmt.Sprintf("repos/%s/pulls/%d", repo, number)
	prRaw, err := v.ghAPI(ctx, prPath)
	if err != nil {
		return zero, fmt.Errorf("artifactqual: fetch pr failed")
	}
	var pr ghPullJSON
	if err := json.Unmarshal(prRaw, &pr); err != nil {
		return zero, fmt.Errorf("artifactqual: parse pr json failed")
	}
	if pr.Number != number {
		return zero, fmt.Errorf("artifactqual: pr number mismatch")
	}
	gotRepo := strings.TrimSpace(pr.Base.Repo.FullName)
	if gotRepo == "" || !strings.EqualFold(gotRepo, repo) {
		return zero, fmt.Errorf("artifactqual: pr repository mismatch")
	}
	headOID := strings.TrimSpace(pr.Head.SHA)
	if !isExact40Hex(headOID) {
		return zero, fmt.Errorf("artifactqual: pr head oid invalid")
	}

	checksPath := fmt.Sprintf("repos/%s/commits/%s/check-runs?per_page=100", repo, headOID)
	checksRaw, err := v.ghAPI(ctx, checksPath)
	if err != nil {
		return zero, fmt.Errorf("artifactqual: fetch pr check-runs failed")
	}
	var checksWrap ghCheckRunsJSON
	if err := json.Unmarshal(checksRaw, &checksWrap); err != nil {
		return zero, fmt.Errorf("artifactqual: parse pr check-runs json failed")
	}

	checks := make([]PRCheck, 0, len(checksWrap.CheckRuns))
	var required []string
	seenName := map[string]bool{}
	allGreen := len(checksWrap.CheckRuns) > 0
	for _, c := range checksWrap.CheckRuns {
		name := strings.TrimSpace(c.Name)
		status := strings.TrimSpace(c.Status)
		conclusion := strings.TrimSpace(c.Conclusion)
		checks = append(checks, PRCheck{Name: name, Status: status, Conclusion: conclusion})
		if name != "" && !seenName[strings.ToLower(name)] {
			seenName[strings.ToLower(name)] = true
			required = append(required, name)
		}
		if !strings.EqualFold(status, "completed") || !strings.EqualFold(conclusion, "success") {
			allGreen = false
		}
	}
	if len(checksWrap.CheckRuns) == 0 {
		allGreen = false
	}

	autoMergeEnabled := jsonObjectPresent(pr.AutoMerge)
	state := strings.TrimSpace(pr.State)
	return PRLiveState{
		Repository:          gotRepo,
		Number:              pr.Number,
		URL:                 strings.TrimSpace(pr.HTMLURL),
		BaseRef:             strings.TrimSpace(pr.Base.Ref),
		HeadOID:             strings.ToLower(headOID),
		State:               state,
		AutoMergeEnabled:    autoMergeEnabled,
		RequiredChecks:      required,
		RequiredChecksGreen: allGreen,
		ChecksAtHead:        checks,
		HumanMergeGate:      state == "open" && !autoMergeEnabled,
	}, nil
}

// jsonObjectPresent is true when raw is a non-null JSON value (object/array/etc).
func jsonObjectPresent(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null"
}

// Ensure interface compliance.
var (
	_ PreProdActionsVerifier = (*GitHubEvidenceVerifier)(nil)
	_ RCActionsVerifier      = (*GitHubEvidenceVerifier)(nil)
	_ PRLiveVerifier         = (*GitHubEvidenceVerifier)(nil)
)

// --- GitHub Actions API JSON shapes (minimal fields) ---

type ghActionsRunJSON struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	DisplayTitle string `json:"display_title"`
	Path         string `json:"path"`
	Event        string `json:"event"`
	HeadBranch   string `json:"head_branch"`
	HeadSHA      string `json:"head_sha"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	RunAttempt   int    `json:"run_attempt"`
	Repository   struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

type ghActionsJobsJSON struct {
	Jobs []struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	} `json:"jobs"`
}

type ghActionsArtifactJSON struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Expired     bool   `json:"expired"`
	WorkflowRun struct {
		ID int64 `json:"id"`
	} `json:"workflow_run"`
}

type ghActionsArtifactsJSON struct {
	Artifacts []ghActionsArtifactJSON `json:"artifacts"`
}

// ghPullJSON is a minimal GitHub pull object.
type ghPullJSON struct {
	Number    int             `json:"number"`
	HTMLURL   string          `json:"html_url"`
	State     string          `json:"state"`
	AutoMerge json.RawMessage `json:"auto_merge"` // non-nil / non-null object → auto-merge enabled
	Base      struct {
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
	Head struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

type ghCheckRunsJSON struct {
	CheckRuns []struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	} `json:"check_runs"`
}
