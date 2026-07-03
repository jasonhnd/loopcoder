package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/execresult"
	"github.com/jasonhnd/loopcoder/internal/kickback"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
)

const ghHardCapDefault = 60 * time.Second

var ghHardCap = ghHardCapDefault

// Reader is the GitHub read surface used by orchestration. Tests can inject a
// fake implementation so no real gh credentials are required.
type Reader interface {
	RepoName(ctx context.Context) (string, error)
	ListIssues(ctx context.Context, state string) ([]Issue, error)
	ViewIssue(ctx context.Context, number int) (Issue, error)
	ListOpenPRs(ctx context.Context) ([]PullRequest, error)
	PRChecks(ctx context.Context, number int) ([]Check, error)
}

type Writer interface {
	CreatePR(ctx context.Context, head, base, title, body string) (string, error)
	ListHeadPRs(ctx context.Context, branch string) ([]PullRequestReference, error)
	MergeToPreProd(ctx context.Context, prNumber int, preProdBranch string) (PreProdMergeResult, error)
	RevertOnPreProd(ctx context.Context, prNumber int, preProdBranch, mergeSHA string) (PreProdRevertResult, error)
}

// ProductionWriter is the human-only production promotion write surface. It is
// intentionally separate from Writer so unattended tick dependencies cannot gain
// a main-merge method by interface widening.
type ProductionWriter interface {
	BranchHeadSHA(ctx context.Context, branch string) (string, error)
	BranchChecks(ctx context.Context, branch string) (BranchChecksResult, error)
	KickBackFromPreProd(ctx context.Context, item, preProdBranch string) (PreProdKickBackResult, error)
	RouteKickBackToNeedsHuman(ctx context.Context, prNumber int) (NeedsHumanRouteResult, error)
	PromotePreProdToMain(ctx context.Context, preProdBranch string) (MainPromotionResult, error)
	RevertProductionMerge(ctx context.Context, mainBranch, mergeCommit, priorStableCommit string) (ProductionRevertResult, error)
	SyncPreProdFromMain(ctx context.Context, preProdBranch string) (PreProdSyncResult, error)
}

// IssueWriter is the GitHub issue mutation surface used by compile. Tests can
// inject a fake implementation so no real gh credentials are required.
type IssueWriter interface {
	RepoName(ctx context.Context) (string, error)
	ListIssues(ctx context.Context, state string) ([]Issue, error)
	CreateIssue(ctx context.Context, title, body string, labels []string) (Issue, error)
	UpdateIssue(ctx context.Context, number int, title, body string, addLabels, removeLabels []string) (Issue, error)
	CloseIssue(ctx context.Context, number int) error
}

type Runner interface {
	Run(ctx context.Context, dir, name string, args ...string) ([]byte, error)
}

type CLI struct {
	repoPath string
	runner   Runner
}

type Label struct {
	Name string `json:"name"`
}

type PullRequestReference struct {
	Number   int    `json:"number,omitempty"`
	Title    string `json:"title,omitempty"`
	URL      string `json:"url,omitempty"`
	State    string `json:"state,omitempty"`
	MergedAt string `json:"mergedAt,omitempty"`
	Merged   bool   `json:"merged,omitempty"`
}

type IssueReference struct {
	Number int    `json:"number"`
	Title  string `json:"title,omitempty"`
	State  string `json:"state,omitempty"`
}

type Issue struct {
	Number                         int                    `json:"number"`
	Title                          string                 `json:"title"`
	Body                           string                 `json:"body"`
	State                          string                 `json:"state"`
	StateReason                    string                 `json:"stateReason"`
	Labels                         []Label                `json:"labels"`
	ClosedByPullRequestsReferences []PullRequestReference `json:"closedByPullRequestsReferences"`
}

type PullRequest struct {
	Number                  int              `json:"number"`
	Title                   string           `json:"title"`
	Body                    string           `json:"body"`
	URL                     string           `json:"url"`
	HeadRefName             string           `json:"headRefName"`
	IsDraft                 bool             `json:"isDraft"`
	Labels                  []Label          `json:"labels"`
	ClosingIssuesReferences []IssueReference `json:"closingIssuesReferences"`
}

type Check struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Bucket string `json:"bucket"`
}

type BranchChecksResult struct {
	Branch  string  `json:"branch"`
	HeadSHA string  `json:"head_sha"`
	Checks  []Check `json:"checks"`
}

type PreProdMergeResult struct {
	PRNumber int    `json:"pr_number"`
	Branch   string `json:"branch"`
	Head     string `json:"head"`
	SHA      string `json:"sha,omitempty"`
	URL      string `json:"url,omitempty"`
}

type PreProdRevertResult struct {
	PRNumber    int    `json:"pr_number"`
	Branch      string `json:"branch"`
	RevertedSHA string `json:"reverted_sha"`
	SHA         string `json:"sha,omitempty"`
	URL         string `json:"url,omitempty"`
}

type ProductionRevertResult struct {
	Branch            string `json:"branch"`
	RevertedSHA       string `json:"reverted_sha"`
	MergeCommit       string `json:"merge_commit,omitempty"`
	PriorStableCommit string `json:"prior_stable_commit,omitempty"`
	SHA               string `json:"sha,omitempty"`
	URL               string `json:"url,omitempty"`
}

type PreProdKickBackResult struct {
	Item        string `json:"item"`
	PRNumber    int    `json:"pr_number,omitempty"`
	Branch      string `json:"branch"`
	RevertedSHA string `json:"reverted_sha"`
	SHA         string `json:"sha,omitempty"`
	URL         string `json:"url,omitempty"`
}

type NeedsHumanRouteResult struct {
	PRNumber     int    `json:"pr_number,omitempty"`
	IssueNumbers []int  `json:"issue_numbers,omitempty"`
	PRLabeled    bool   `json:"pr_labeled,omitempty"`
	Label        string `json:"label,omitempty"`
}

type MainPromotionResult struct {
	PreProdBranch   string `json:"pre_prod_branch"`
	MainBranch      string `json:"main_branch"`
	Head            string `json:"head"`
	SHA             string `json:"sha,omitempty"`
	URL             string `json:"url,omitempty"`
	AlreadyUpToDate bool   `json:"already_up_to_date,omitempty"`
}

type PreProdSyncResult struct {
	PreProdBranch string `json:"pre_prod_branch"`
	MainBranch    string `json:"main_branch"`
	SHA           string `json:"sha,omitempty"`
	URL           string `json:"url,omitempty"`
}

// New returns a gh-backed reader rooted at repoPath.
func New(repoPath string) *CLI {
	return NewWithRunner(repoPath, ExecRunner{})
}

// NewWithRunner returns a gh-backed client using runner.
func NewWithRunner(repoPath string, runner Runner) *CLI {
	if runner == nil {
		runner = ExecRunner{}
	}
	return &CLI{repoPath: repoPath, runner: runner}
}

// ExecRunner runs external commands through exec.CommandContext.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	result, err := supervisedexec.Run(ctx, cmd, supervisedexec.Options{HardCap: ghHardCap})
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if detail != "" {
			return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, detail)
		}
		return nil, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	if result.Outcome == supervisedexec.OutcomeDeadline {
		return nil, fmt.Errorf("%s %s timed out after %s", name, strings.Join(args, " "), ghHardCap)
	}
	if result.ExitCode != 0 {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		err := execresult.CommandExitError(cmd, result.ExitCode)
		if detail != "" {
			return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, detail)
		}
		return nil, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return stdout.Bytes(), nil
}

func (c *CLI) RepoName(ctx context.Context) (string, error) {
	var payload struct {
		NameWithOwner string `json:"nameWithOwner"`
	}
	if err := c.runJSON(ctx, []string{"repo", "view", "--json", "nameWithOwner"}, &payload); err == nil {
		if strings.TrimSpace(payload.NameWithOwner) != "" {
			return payload.NameWithOwner, nil
		}
	}

	remote, err := c.gitRemote(ctx)
	if err == nil {
		if parsed := parseGitHubRemote(remote); parsed != "" {
			return parsed, nil
		}
		if strings.TrimSpace(remote) != "" {
			return strings.TrimSpace(remote), nil
		}
	}

	return c.repoPath, nil
}

func (c *CLI) ListIssues(ctx context.Context, state string) ([]Issue, error) {
	if strings.TrimSpace(state) == "" {
		state = "open"
	}
	var issues []Issue
	err := c.runJSON(ctx, []string{
		"issue", "list",
		"--state", state,
		"--limit", "1000",
		"--json", "number,title,body,labels,state,stateReason",
	}, &issues)
	return issues, err
}

func (c *CLI) ViewIssue(ctx context.Context, number int) (Issue, error) {
	var issue Issue
	err := c.runJSON(ctx, []string{
		"issue", "view", fmt.Sprintf("%d", number),
		"--json", "number,title,body,state,stateReason,labels,closedByPullRequestsReferences",
	}, &issue)
	return issue, err
}

func (c *CLI) ListOpenPRs(ctx context.Context) ([]PullRequest, error) {
	var prs []PullRequest
	err := c.runJSON(ctx, []string{
		"pr", "list",
		"--state", "open",
		"--limit", "1000",
		"--json", "number,title,url,headRefName,isDraft,labels,closingIssuesReferences",
	}, &prs)
	return prs, err
}

func (c *CLI) ViewPR(ctx context.Context, number int) (PullRequest, error) {
	var pr PullRequest
	err := c.runJSON(ctx, []string{
		"pr", "view", fmt.Sprintf("%d", number),
		"--json", "number,title,body,url,headRefName,isDraft,closingIssuesReferences",
	}, &pr)
	return pr, err
}

func (c *CLI) PRDiff(ctx context.Context, number int) (string, error) {
	output, err := c.run(ctx, "gh", "pr", "diff", fmt.Sprintf("%d", number))
	if err != nil {
		return "", err
	}
	return string(output), nil
}

func (c *CLI) PRDiffNameOnly(ctx context.Context, number int) ([]string, error) {
	output, err := c.run(ctx, "gh", "pr", "diff", fmt.Sprintf("%d", number), "--name-only")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.ReplaceAll(string(output), "\r\n", "\n"), "\n")
	files := make([]string, 0, len(lines))
	for _, line := range lines {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			files = append(files, trimmed)
		}
	}
	return files, nil
}

func (c *CLI) PRChecks(ctx context.Context, number int) ([]Check, error) {
	var checks []Check
	err := c.runJSON(ctx, []string{
		"pr", "checks", fmt.Sprintf("%d", number),
		"--json", "name,state,bucket",
	}, &checks)
	if err != nil {
		return nil, err
	}
	return checks, nil
}

func (c *CLI) BranchChecks(ctx context.Context, branch string) (BranchChecksResult, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return BranchChecksResult{}, fmt.Errorf("branch is required")
	}
	sha, err := c.branchHeadSHA(ctx, branch)
	if err != nil {
		return BranchChecksResult{}, err
	}
	repo, err := c.RepoName(ctx)
	if err != nil {
		return BranchChecksResult{}, err
	}
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return BranchChecksResult{}, fmt.Errorf("repository name is required")
	}

	checks, err := c.checkRunsForCommit(ctx, repo, sha)
	if err != nil {
		return BranchChecksResult{}, err
	}
	statuses, err := c.statusChecksForCommit(ctx, repo, sha)
	if err != nil {
		return BranchChecksResult{}, err
	}
	checks = append(checks, statuses...)
	return BranchChecksResult{
		Branch:  branch,
		HeadSHA: sha,
		Checks:  checks,
	}, nil
}

func (c *CLI) BranchHeadSHA(ctx context.Context, branch string) (string, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return "", fmt.Errorf("branch is required")
	}
	return c.branchHeadSHA(ctx, branch)
}

func (c *CLI) CreatePR(ctx context.Context, head, base, title, body string) (string, error) {
	output, err := c.run(ctx, "gh", "pr", "create", "--head", head, "--base", base, "--title", title, "--body", body)
	if err != nil {
		return "", err
	}
	url := strings.TrimSpace(string(output))
	if url == "" {
		return "", fmt.Errorf("gh pr create returned an empty URL")
	}
	return url, nil
}

func (c *CLI) ListHeadPRs(ctx context.Context, branch string) ([]PullRequestReference, error) {
	var prs []PullRequestReference
	err := c.runJSON(ctx, []string{
		"pr", "list",
		"--head", branch,
		"--json", "number,url",
	}, &prs)
	return prs, err
}

func (c *CLI) MergeToPreProd(ctx context.Context, prNumber int, preProdBranch string) (PreProdMergeResult, error) {
	preProdBranch = strings.TrimSpace(preProdBranch)
	if prNumber <= 0 {
		return PreProdMergeResult{}, fmt.Errorf("pull request number is required")
	}
	if preProdBranch == "" {
		return PreProdMergeResult{}, fmt.Errorf("pre-prod branch is required")
	}
	if isReservedProductionBranch(preProdBranch) {
		return PreProdMergeResult{}, fmt.Errorf("pre-prod branch %q is reserved for human promotion", preProdBranch)
	}

	pr, err := c.ViewPR(ctx, prNumber)
	if err != nil {
		return PreProdMergeResult{}, err
	}
	head := strings.TrimSpace(pr.HeadRefName)
	if head == "" {
		return PreProdMergeResult{}, fmt.Errorf("pull request #%d has no head branch", prNumber)
	}
	repo, err := c.RepoName(ctx)
	if err != nil {
		return PreProdMergeResult{}, err
	}
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return PreProdMergeResult{}, fmt.Errorf("repository name is required")
	}

	var payload struct {
		SHA     string `json:"sha"`
		HTMLURL string `json:"html_url"`
	}
	if err := c.runJSON(ctx, []string{
		"api",
		"--method", "POST",
		"repos/" + repo + "/merges",
		"-f", "base=" + preProdBranch,
		"-f", "head=" + head,
		"-f", fmt.Sprintf("commit_message=loopcoder pre-prod merge PR #%d", prNumber),
	}, &payload); err != nil {
		return PreProdMergeResult{}, err
	}
	return PreProdMergeResult{
		PRNumber: prNumber,
		Branch:   preProdBranch,
		Head:     head,
		SHA:      payload.SHA,
		URL:      payload.HTMLURL,
	}, nil
}

func (c *CLI) RevertOnPreProd(ctx context.Context, prNumber int, preProdBranch, mergeSHA string) (PreProdRevertResult, error) {
	preProdBranch = strings.TrimSpace(preProdBranch)
	mergeSHA = strings.TrimSpace(mergeSHA)
	if prNumber <= 0 {
		return PreProdRevertResult{}, fmt.Errorf("pull request number is required")
	}
	if preProdBranch == "" {
		return PreProdRevertResult{}, fmt.Errorf("pre-prod branch is required")
	}
	if mergeSHA == "" {
		return PreProdRevertResult{}, fmt.Errorf("merge commit SHA is required")
	}
	return c.revertPreProdCommit(ctx, prNumber, preProdBranch, mergeSHA)
}

func (c *CLI) KickBackFromPreProd(ctx context.Context, item, preProdBranch string) (PreProdKickBackResult, error) {
	item = strings.TrimSpace(item)
	preProdBranch = strings.TrimSpace(preProdBranch)
	if item == "" {
		return PreProdKickBackResult{}, fmt.Errorf("kick-back item is required")
	}
	if preProdBranch == "" {
		return PreProdKickBackResult{}, fmt.Errorf("pre-prod branch is required")
	}
	if isReservedProductionBranch(preProdBranch) {
		return PreProdKickBackResult{}, fmt.Errorf("pre-prod branch %q is reserved for human promotion", preProdBranch)
	}

	mergeSHA, prNumber, err := c.resolvePreProdKickBackCommit(ctx, item, preProdBranch)
	if err != nil {
		return PreProdKickBackResult{}, err
	}
	reverted, err := c.revertPreProdCommit(ctx, prNumber, preProdBranch, mergeSHA)
	if err != nil {
		return PreProdKickBackResult{}, err
	}
	return PreProdKickBackResult{
		Item:        item,
		PRNumber:    prNumber,
		Branch:      reverted.Branch,
		RevertedSHA: reverted.RevertedSHA,
		SHA:         reverted.SHA,
		URL:         reverted.URL,
	}, nil
}

func (c *CLI) RouteKickBackToNeedsHuman(ctx context.Context, prNumber int) (NeedsHumanRouteResult, error) {
	if prNumber <= 0 {
		return NeedsHumanRouteResult{}, fmt.Errorf("pull request number is required")
	}
	pr, err := c.ViewPR(ctx, prNumber)
	if err != nil {
		return NeedsHumanRouteResult{}, err
	}
	result := NeedsHumanRouteResult{
		PRNumber: prNumber,
		Label:    needsHumanLabel,
	}
	issueNumbers := linkedIssueNumbers(pr)
	if err := c.ensureLabels(ctx, []string{needsHumanLabel}); err != nil {
		return result, err
	}
	if len(issueNumbers) == 0 {
		if _, err := c.run(ctx, "gh", "pr", "edit", fmt.Sprintf("%d", prNumber), "--add-label", needsHumanLabel); err != nil {
			return result, err
		}
		result.PRLabeled = true
		return result, nil
	}
	for _, issueNumber := range issueNumbers {
		if _, err := c.run(ctx, "gh", "issue", "edit", fmt.Sprintf("%d", issueNumber), "--add-label", needsHumanLabel); err != nil {
			return result, err
		}
	}
	result.IssueNumbers = issueNumbers
	return result, nil
}

func (c *CLI) PromotePreProdToMain(ctx context.Context, preProdBranch string) (MainPromotionResult, error) {
	preProdBranch = strings.TrimSpace(preProdBranch)
	if preProdBranch == "" {
		return MainPromotionResult{}, fmt.Errorf("pre-prod branch is required")
	}
	if isReservedProductionBranch(preProdBranch) {
		return MainPromotionResult{}, fmt.Errorf("pre-prod branch %q is reserved for human promotion", preProdBranch)
	}
	repo, err := c.RepoName(ctx)
	if err != nil {
		return MainPromotionResult{}, err
	}
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return MainPromotionResult{}, fmt.Errorf("repository name is required")
	}

	var payload struct {
		SHA     string `json:"sha"`
		HTMLURL string `json:"html_url"`
	}
	if err := c.runJSON(ctx, []string{
		"api",
		"--method", "POST",
		"repos/" + repo + "/merges",
		"-f", "base=main",
		"-f", "head=" + preProdBranch,
		"-f", "commit_message=loopcoder promote " + preProdBranch + " to main",
	}, &payload); err != nil {
		return MainPromotionResult{}, err
	}
	return MainPromotionResult{
		PreProdBranch:   preProdBranch,
		MainBranch:      "main",
		Head:            preProdBranch,
		SHA:             payload.SHA,
		URL:             payload.HTMLURL,
		AlreadyUpToDate: strings.TrimSpace(payload.SHA) == "",
	}, nil
}

func (c *CLI) RevertProductionMerge(ctx context.Context, mainBranch, mergeCommit, priorStableCommit string) (ProductionRevertResult, error) {
	mainBranch = strings.TrimSpace(mainBranch)
	mergeCommit = strings.TrimSpace(mergeCommit)
	priorStableCommit = strings.TrimSpace(priorStableCommit)
	if mainBranch == "" {
		return ProductionRevertResult{}, fmt.Errorf("main branch is required")
	}
	if !isReservedProductionBranch(mainBranch) {
		return ProductionRevertResult{}, fmt.Errorf("branch %q is not a production branch", mainBranch)
	}
	if mergeCommit == "" {
		return ProductionRevertResult{}, fmt.Errorf("merge commit SHA is required")
	}
	if priorStableCommit == "" {
		return ProductionRevertResult{}, fmt.Errorf("prior stable commit SHA is required")
	}

	return c.revertProductionCommit(ctx, mainBranch, mergeCommit, priorStableCommit)
}

func (c *CLI) SyncPreProdFromMain(ctx context.Context, preProdBranch string) (PreProdSyncResult, error) {
	preProdBranch = strings.TrimSpace(preProdBranch)
	if preProdBranch == "" {
		return PreProdSyncResult{}, fmt.Errorf("pre-prod branch is required")
	}
	if isReservedProductionBranch(preProdBranch) {
		return PreProdSyncResult{}, fmt.Errorf("pre-prod branch %q is reserved for human promotion", preProdBranch)
	}
	if _, err := c.run(ctx, "git", "fetch", "origin", "+refs/heads/main:refs/remotes/origin/main"); err != nil {
		return PreProdSyncResult{}, err
	}
	shaBytes, err := c.run(ctx, "git", "rev-parse", "refs/remotes/origin/main")
	if err != nil {
		return PreProdSyncResult{}, err
	}
	sha := strings.TrimSpace(string(shaBytes))
	if sha == "" {
		return PreProdSyncResult{}, fmt.Errorf("main branch sync produced an empty commit SHA")
	}
	if _, err := c.run(ctx, "git", "push", "origin", "refs/remotes/origin/main:refs/heads/"+preProdBranch); err != nil {
		return PreProdSyncResult{}, err
	}
	return PreProdSyncResult{
		PreProdBranch: preProdBranch,
		MainBranch:    "main",
		SHA:           sha,
		URL:           commitURL(ctx, c, sha),
	}, nil
}

func (c *CLI) revertProductionCommit(ctx context.Context, mainBranch, mergeCommit, priorStableCommit string) (ProductionRevertResult, error) {
	refspec := "+refs/heads/" + mainBranch + ":refs/remotes/origin/" + mainBranch
	if _, err := c.run(ctx, "git", "fetch", "origin", refspec); err != nil {
		return ProductionRevertResult{}, err
	}

	if revertSHA, err := c.findExistingProductionRevert(ctx, mainBranch, mergeCommit); err != nil {
		return ProductionRevertResult{}, err
	} else if revertSHA != "" {
		if err := c.validateCommitTreesMatch(ctx, c.repoPath, revertSHA, priorStableCommit); err != nil {
			return ProductionRevertResult{}, err
		}
		return ProductionRevertResult{
			Branch:            mainBranch,
			RevertedSHA:       mergeCommit,
			MergeCommit:       mergeCommit,
			PriorStableCommit: priorStableCommit,
			SHA:               revertSHA,
			URL:               commitURL(ctx, c, revertSHA),
		}, nil
	}

	branchHead, err := c.remoteBranchHead(ctx, mainBranch)
	if err != nil {
		return ProductionRevertResult{}, err
	}
	if !sameGitSHA(branchHead, mergeCommit) {
		return ProductionRevertResult{}, fmt.Errorf("%s head %s is not the just-promoted merge commit %s", mainBranch, branchHead, mergeCommit)
	}

	worktree, err := os.MkdirTemp("", "loopcoder-production-revert-*")
	if err != nil {
		return ProductionRevertResult{}, fmt.Errorf("create temporary production revert worktree: %w", err)
	}
	if err := os.RemoveAll(worktree); err != nil {
		return ProductionRevertResult{}, fmt.Errorf("prepare temporary production revert worktree path: %w", err)
	}
	defer os.RemoveAll(worktree)

	addedWorktree := false
	defer func() {
		if addedWorktree {
			_, _ = c.run(ctx, "git", "worktree", "remove", "--force", worktree)
		}
	}()

	if _, err := c.run(ctx, "git", "worktree", "add", "--detach", worktree, "refs/remotes/origin/"+mainBranch); err != nil {
		return ProductionRevertResult{}, err
	}
	addedWorktree = true

	if _, err := c.runner.Run(ctx, worktree, "git", "revert", "-m", "1", "--no-edit", mergeCommit); err != nil {
		return ProductionRevertResult{}, err
	}
	revertSHABytes, err := c.runner.Run(ctx, worktree, "git", "rev-parse", "HEAD")
	if err != nil {
		return ProductionRevertResult{}, err
	}
	revertSHA := strings.TrimSpace(string(revertSHABytes))
	if revertSHA == "" {
		return ProductionRevertResult{}, fmt.Errorf("production revert produced an empty commit SHA")
	}
	if err := c.validateCommitTreesMatch(ctx, worktree, "HEAD", priorStableCommit); err != nil {
		return ProductionRevertResult{}, err
	}
	if _, err := c.runner.Run(ctx, worktree, "git", "push", "origin", "HEAD:"+mainBranch); err != nil {
		return ProductionRevertResult{}, err
	}

	return ProductionRevertResult{
		Branch:            mainBranch,
		RevertedSHA:       mergeCommit,
		MergeCommit:       mergeCommit,
		PriorStableCommit: priorStableCommit,
		SHA:               revertSHA,
		URL:               commitURL(ctx, c, revertSHA),
	}, nil
}

func (c *CLI) resolvePreProdKickBackCommit(ctx context.Context, item, preProdBranch string) (string, int, error) {
	parsed := kickback.ParseItem(item)
	if parsed.SHA != "" {
		return parsed.SHA, 0, nil
	}
	prNumber := parsed.PRNumber
	if prNumber <= 0 {
		return "", 0, fmt.Errorf("kick-back item %q must be a PR number or merge commit SHA", item)
	}
	refspec := "+refs/heads/" + preProdBranch + ":refs/remotes/origin/" + preProdBranch
	if _, err := c.run(ctx, "git", "fetch", "origin", refspec); err != nil {
		return "", 0, err
	}
	output, err := c.run(ctx, "git", "log", "--format=%H%x00%s", "refs/remotes/origin/"+preProdBranch)
	if err != nil {
		return "", 0, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		parts := strings.SplitN(line, "\x00", 2)
		if len(parts) != 2 {
			continue
		}
		if preProdMergeSubjectPRNumber(parts[1]) == prNumber {
			sha := strings.TrimSpace(parts[0])
			if sha == "" {
				return "", 0, fmt.Errorf("pre-prod merge commit for PR #%d had an empty SHA", prNumber)
			}
			return sha, prNumber, nil
		}
	}
	return "", 0, fmt.Errorf("pre-prod merge commit for PR #%d was not found on %s", prNumber, preProdBranch)
}

func (c *CLI) revertPreProdCommit(ctx context.Context, prNumber int, preProdBranch, mergeSHA string) (PreProdRevertResult, error) {
	if isReservedProductionBranch(preProdBranch) {
		return PreProdRevertResult{}, fmt.Errorf("pre-prod branch %q is reserved for human promotion", preProdBranch)
	}

	refspec := "+refs/heads/" + preProdBranch + ":refs/remotes/origin/" + preProdBranch
	if _, err := c.run(ctx, "git", "fetch", "origin", refspec); err != nil {
		return PreProdRevertResult{}, err
	}

	if revertSHA, err := c.findExistingPreProdRevert(ctx, preProdBranch, mergeSHA); err != nil {
		return PreProdRevertResult{}, err
	} else if revertSHA != "" {
		return PreProdRevertResult{
			PRNumber:    prNumber,
			Branch:      preProdBranch,
			RevertedSHA: mergeSHA,
			SHA:         revertSHA,
			URL:         commitURL(ctx, c, revertSHA),
		}, nil
	}

	worktree, err := os.MkdirTemp("", "loopcoder-preprod-revert-*")
	if err != nil {
		return PreProdRevertResult{}, fmt.Errorf("create temporary pre-prod worktree: %w", err)
	}
	if err := os.RemoveAll(worktree); err != nil {
		return PreProdRevertResult{}, fmt.Errorf("prepare temporary pre-prod worktree path: %w", err)
	}
	defer os.RemoveAll(worktree)

	addedWorktree := false
	defer func() {
		if addedWorktree {
			_, _ = c.run(ctx, "git", "worktree", "remove", "--force", worktree)
		}
	}()

	if _, err := c.run(ctx, "git", "worktree", "add", "--detach", worktree, "refs/remotes/origin/"+preProdBranch); err != nil {
		return PreProdRevertResult{}, err
	}
	addedWorktree = true

	if _, err := c.runner.Run(ctx, worktree, "git", "revert", "-m", "1", "--no-edit", mergeSHA); err != nil {
		return PreProdRevertResult{}, err
	}
	revertSHABytes, err := c.runner.Run(ctx, worktree, "git", "rev-parse", "HEAD")
	if err != nil {
		return PreProdRevertResult{}, err
	}
	revertSHA := strings.TrimSpace(string(revertSHABytes))
	if revertSHA == "" {
		return PreProdRevertResult{}, fmt.Errorf("pre-prod revert produced an empty commit SHA")
	}
	if _, err := c.runner.Run(ctx, worktree, "git", "push", "origin", "HEAD:"+preProdBranch); err != nil {
		return PreProdRevertResult{}, err
	}

	return PreProdRevertResult{
		PRNumber:    prNumber,
		Branch:      preProdBranch,
		RevertedSHA: mergeSHA,
		SHA:         revertSHA,
		URL:         commitURL(ctx, c, revertSHA),
	}, nil
}

func (c *CLI) findExistingPreProdRevert(ctx context.Context, preProdBranch, mergeSHA string) (string, error) {
	output, err := c.run(ctx, "git", "log", "--format=%H%x00%B%x1e", "refs/remotes/origin/"+preProdBranch)
	if err != nil {
		return "", err
	}
	return existingRevertSHAForCommit(output, mergeSHA), nil
}

func (c *CLI) findExistingProductionRevert(ctx context.Context, mainBranch, mergeSHA string) (string, error) {
	output, err := c.run(ctx, "git", "log", "--format=%H%x00%B%x1e", "refs/remotes/origin/"+mainBranch)
	if err != nil {
		return "", err
	}
	return existingRevertSHAForCommit(output, mergeSHA), nil
}

func existingRevertSHAForCommit(output []byte, mergeSHA string) string {
	for _, record := range strings.Split(string(output), "\x1e") {
		record = strings.TrimSpace(record)
		if record == "" {
			continue
		}
		parts := strings.SplitN(record, "\x00", 2)
		if len(parts) != 2 {
			continue
		}
		if revertMessageTargetsCommit(parts[1], mergeSHA) {
			return strings.TrimSpace(parts[0])
		}
	}
	return ""
}

func revertMessageTargetsCommit(message, mergeSHA string) bool {
	const prefix = "This reverts commit "
	for _, line := range strings.Split(message, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		remainder := strings.TrimSpace(line[len(prefix):])
		fields := strings.Fields(remainder)
		if len(fields) == 0 {
			continue
		}
		candidate := strings.Trim(fields[0], ".,")
		if sameGitSHA(candidate, mergeSHA) {
			return true
		}
	}
	return false
}

func (c *CLI) remoteBranchHead(ctx context.Context, branch string) (string, error) {
	output, err := c.run(ctx, "git", "rev-parse", "refs/remotes/origin/"+branch)
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(output))
	if sha == "" {
		return "", fmt.Errorf("branch %q resolved to an empty SHA", branch)
	}
	return sha, nil
}

func (c *CLI) validateCommitTreesMatch(ctx context.Context, dir, revertedCommit, priorStableCommit string) error {
	revertedTree, err := c.commitTree(ctx, dir, revertedCommit)
	if err != nil {
		return fmt.Errorf("read production revert tree: %w", err)
	}
	priorTree, err := c.commitTree(ctx, dir, priorStableCommit)
	if err != nil {
		return fmt.Errorf("read prior stable tree: %w", err)
	}
	if !sameGitSHA(revertedTree, priorTree) {
		return fmt.Errorf("production revert tree %s does not match prior stable tree %s", revertedTree, priorTree)
	}
	return nil
}

func (c *CLI) commitTree(ctx context.Context, dir, rev string) (string, error) {
	output, err := c.runner.Run(ctx, dir, "git", "rev-parse", strings.TrimSpace(rev)+"^{tree}")
	if err != nil {
		return "", err
	}
	tree := strings.TrimSpace(string(output))
	if tree == "" {
		return "", fmt.Errorf("commit %q resolved to an empty tree", rev)
	}
	return tree, nil
}

func (c *CLI) CreateIssue(ctx context.Context, title, body string, labels []string) (Issue, error) {
	labels = normalizeLabelArgs(labels)
	if err := c.ensureLabels(ctx, labels); err != nil {
		return Issue{}, err
	}

	args := []string{"issue", "create", "--title", title, "--body", body}
	for _, label := range labels {
		args = append(args, "--label", label)
	}
	output, err := c.run(ctx, "gh", args...)
	if err != nil {
		return Issue{}, err
	}
	number := issueNumberFromURL(strings.TrimSpace(string(output)))
	if number <= 0 {
		return Issue{}, fmt.Errorf("gh issue create returned no issue number: %s", strings.TrimSpace(string(output)))
	}
	issue, viewErr := c.ViewIssue(ctx, number)
	if viewErr != nil {
		return Issue{
			Number: number,
			Title:  title,
			Body:   body,
			State:  "OPEN",
			Labels: labelsFromNames(labels),
		}, nil
	}
	return issue, nil
}

func (c *CLI) UpdateIssue(ctx context.Context, number int, title, body string, addLabels, removeLabels []string) (Issue, error) {
	addLabels = normalizeLabelArgs(addLabels)
	removeLabels = normalizeLabelArgs(removeLabels)
	if err := c.ensureLabels(ctx, addLabels); err != nil {
		return Issue{}, err
	}

	args := []string{"issue", "edit", fmt.Sprintf("%d", number), "--title", title, "--body", body}
	for _, label := range addLabels {
		args = append(args, "--add-label", label)
	}
	for _, label := range removeLabels {
		args = append(args, "--remove-label", label)
	}
	if _, err := c.run(ctx, "gh", args...); err != nil {
		return Issue{}, err
	}
	issue, err := c.ViewIssue(ctx, number)
	if err != nil {
		return Issue{
			Number: number,
			Title:  title,
			Body:   body,
			State:  "OPEN",
			Labels: labelsFromNames(addLabels),
		}, nil
	}
	return issue, nil
}

func (c *CLI) CloseIssue(ctx context.Context, number int) error {
	_, err := c.run(ctx, "gh", "issue", "close", fmt.Sprintf("%d", number), "--reason", "not planned")
	return err
}

func (c *CLI) runJSON(ctx context.Context, args []string, target any) error {
	output, err := c.run(ctx, "gh", args...)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(output)) == 0 {
		return nil
	}
	return parseJSONOutput(output, target)
}

func (c *CLI) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if c == nil || c.runner == nil {
		return nil, fmt.Errorf("github client is not configured")
	}
	return c.runner.Run(ctx, c.repoPath, name, args...)
}

func (c *CLI) branchHeadSHA(ctx context.Context, branch string) (string, error) {
	output, err := c.run(ctx, "git", "ls-remote", "origin", "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(output))
	if line == "" {
		return "", fmt.Errorf("branch %q was not found on origin", branch)
	}
	fields := strings.Fields(line)
	if len(fields) == 0 || strings.TrimSpace(fields[0]) == "" {
		return "", fmt.Errorf("could not resolve head SHA for branch %q", branch)
	}
	return strings.TrimSpace(fields[0]), nil
}

func (c *CLI) checkRunsForCommit(ctx context.Context, repo, sha string) ([]Check, error) {
	var payload struct {
		CheckRuns []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if err := c.runJSON(ctx, []string{
		"api",
		"repos/" + repo + "/commits/" + sha + "/check-runs?per_page=100",
	}, &payload); err != nil {
		return nil, err
	}
	checks := make([]Check, 0, len(payload.CheckRuns))
	for _, run := range payload.CheckRuns {
		name := strings.TrimSpace(run.Name)
		if name == "" {
			continue
		}
		checks = append(checks, Check{
			Name:   name,
			State:  firstNonEmptyString(run.Conclusion, run.Status),
			Bucket: checkRunBucket(run.Status, run.Conclusion),
		})
	}
	return checks, nil
}

func (c *CLI) statusChecksForCommit(ctx context.Context, repo, sha string) ([]Check, error) {
	var payload struct {
		Statuses []struct {
			Context string `json:"context"`
			State   string `json:"state"`
		} `json:"statuses"`
	}
	if err := c.runJSON(ctx, []string{
		"api",
		"repos/" + repo + "/commits/" + sha + "/status",
	}, &payload); err != nil {
		return nil, err
	}
	checks := make([]Check, 0, len(payload.Statuses))
	for _, status := range payload.Statuses {
		name := strings.TrimSpace(status.Context)
		if name == "" {
			continue
		}
		checks = append(checks, Check{
			Name:   name,
			State:  strings.TrimSpace(status.State),
			Bucket: statusBucket(status.State),
		})
	}
	return checks, nil
}

func (c *CLI) ensureLabels(ctx context.Context, labels []string) error {
	for _, label := range labels {
		output, err := c.run(ctx, "gh", "label", "create", label, "--color", labelColor(label), "--description", labelDescription(label))
		if err != nil && !labelAlreadyExists(err, output) {
			return fmt.Errorf("ensure label %q: %w", label, err)
		}
	}
	return nil
}

func (c *CLI) gitRemote(ctx context.Context) (string, error) {
	output, err := c.run(ctx, "git", "remote", "get-url", "origin")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func parseJSONOutput(output []byte, target any) error {
	cleaned := trimToJSON(output)
	if len(cleaned) == 0 {
		return fmt.Errorf("no JSON object or array found")
	}
	if err := json.Unmarshal(cleaned, target); err != nil {
		return fmt.Errorf("parse gh JSON: %w", err)
	}
	return nil
}

func trimToJSON(output []byte) []byte {
	text := strings.TrimSpace(string(output))
	if text == "" {
		return nil
	}

	arrayStart := strings.Index(text, "[")
	objectStart := strings.Index(text, "{")
	start := -1
	switch {
	case arrayStart >= 0 && objectStart >= 0 && arrayStart < objectStart:
		start = arrayStart
	case arrayStart >= 0 && objectStart >= 0:
		start = objectStart
	case arrayStart >= 0:
		start = arrayStart
	case objectStart >= 0:
		start = objectStart
	}
	if start < 0 {
		return nil
	}

	text = strings.TrimSpace(text[start:])
	if strings.HasPrefix(text, "[") {
		if end := strings.LastIndex(text, "]"); end >= 0 {
			text = text[:end+1]
		}
	} else if strings.HasPrefix(text, "{") {
		if end := strings.LastIndex(text, "}"); end >= 0 {
			text = text[:end+1]
		}
	}
	return []byte(strings.TrimSpace(text))
}

var githubRemotePattern = regexp.MustCompile(`github\.com[:/]([^/]+)/(.+?)(?:\.git)?$`)
var issueURLPattern = regexp.MustCompile(`/issues/(\d+)(?:\D*)$`)
var preProdMergePRPattern = regexp.MustCompile(`(?i)\bPR\s*#(\d+)(?:\D|$)`)
var branchIssuePattern = regexp.MustCompile(`(?i)(?:^|[/_-])issue[/_-]?(\d+)(?:\D|$)`)
var hashIssuePattern = regexp.MustCompile(`#(\d+)`)

const needsHumanLabel = "needs-human"

func parseGitHubRemote(remote string) string {
	matches := githubRemotePattern.FindStringSubmatch(strings.TrimSpace(remote))
	if len(matches) != 3 {
		return ""
	}
	return matches[1] + "/" + matches[2]
}

func issueNumberFromURL(url string) int {
	matches := issueURLPattern.FindStringSubmatch(strings.TrimSpace(url))
	if len(matches) != 2 {
		return 0
	}
	var number int
	if _, err := fmt.Sscanf(matches[1], "%d", &number); err != nil {
		return 0
	}
	return number
}

func normalizeLabelArgs(labels []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		trimmed := strings.TrimSpace(label)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	return out
}

func labelsFromNames(names []string) []Label {
	labels := make([]Label, 0, len(names))
	for _, name := range names {
		if strings.TrimSpace(name) != "" {
			labels = append(labels, Label{Name: name})
		}
	}
	return labels
}

func labelAlreadyExists(err error, output []byte) bool {
	text := strings.ToLower(err.Error() + "\n" + string(output))
	return strings.Contains(text, "already exists") || strings.Contains(text, "name has already been taken")
}

func labelColor(label string) string {
	lower := strings.ToLower(strings.TrimSpace(label))
	switch {
	case lower == "delivery:unit":
		return "0e8a16"
	case lower == "epic":
		return "5319e7"
	case lower == needsHumanLabel:
		return "b60205"
	case strings.HasPrefix(lower, "blocked-by:#"):
		return "fbca04"
	default:
		return "ededed"
	}
}

func labelDescription(label string) string {
	lower := strings.ToLower(strings.TrimSpace(label))
	switch {
	case lower == "delivery:unit":
		return "loopcoder work unit"
	case lower == "epic":
		return "loopcoder epic work"
	case lower == needsHumanLabel:
		return "human decision required"
	case strings.HasPrefix(lower, "blocked-by:#"):
		return "loopcoder dependency edge"
	default:
		return "loopcoder compile label"
	}
}

func checkRunBucket(status, conclusion string) string {
	status = strings.ToLower(strings.TrimSpace(status))
	conclusion = strings.ToLower(strings.TrimSpace(conclusion))
	if status != "completed" {
		if status == "" {
			return "pending"
		}
		return "pending"
	}
	switch conclusion {
	case "success":
		return "pass"
	case "neutral", "skipped":
		return "skipping"
	case "cancelled":
		return "cancel"
	case "failure", "timed_out", "action_required", "startup_failure", "stale":
		return "fail"
	default:
		return "pending"
	}
}

func statusBucket(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "success":
		return "pass"
	case "failure", "error":
		return "fail"
	case "pending":
		return "pending"
	default:
		return "unknown"
	}
}

func commitURL(ctx context.Context, c *CLI, sha string) string {
	repo, err := c.RepoName(ctx)
	if err != nil {
		return ""
	}
	repo = strings.TrimSpace(repo)
	if repo == "" || strings.TrimSpace(sha) == "" {
		return ""
	}
	return "https://github.com/" + repo + "/commit/" + strings.TrimSpace(sha)
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func preProdMergeSubjectPRNumber(subject string) int {
	matches := preProdMergePRPattern.FindStringSubmatch(subject)
	if len(matches) != 2 {
		return 0
	}
	number, err := strconv.Atoi(matches[1])
	if err != nil || number <= 0 {
		return 0
	}
	return number
}

func sameGitSHA(a, b string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	if len(a) < 7 || len(b) < 7 {
		return false
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

func linkedIssueNumbers(pr PullRequest) []int {
	seen := map[int]bool{}
	for _, ref := range pr.ClosingIssuesReferences {
		if ref.Number > 0 {
			seen[ref.Number] = true
		}
	}
	if number := linkedIssueNumberFromText(pr.HeadRefName); number > 0 {
		seen[number] = true
	}
	out := make([]int, 0, len(seen))
	for number := range seen {
		out = append(out, number)
	}
	sort.Ints(out)
	return out
}

func linkedIssueNumberFromText(text string) int {
	if strings.TrimSpace(text) == "" {
		return 0
	}
	if matches := branchIssuePattern.FindStringSubmatch(text); len(matches) == 2 {
		number, _ := strconv.Atoi(matches[1])
		return number
	}
	if matches := hashIssuePattern.FindStringSubmatch(text); len(matches) == 2 {
		number, _ := strconv.Atoi(matches[1])
		return number
	}
	return 0
}

func isReservedProductionBranch(branch string) bool {
	switch strings.ToLower(strings.TrimSpace(branch)) {
	case "main", "master", "prod", "production":
		return true
	default:
		return false
	}
}
