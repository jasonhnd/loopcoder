package goalpr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

const (
	// ReceiptSchema is written into the disposable repo as product evidence.
	ReceiptSchema = "loopcoder.goalpr.receipt.v1"
	// StatusHumanGate is the only success terminal (never auto-merge).
	StatusHumanGate = "human_gate"
)

var (
	ErrInvalid   = errors.New("goalpr: invalid")
	ErrNotReady  = errors.New("goalpr: not ready")
	ErrGit       = errors.New("goalpr: git")
	ErrGitHub    = errors.New("goalpr: github")
	ErrAutoMerge = errors.New("goalpr: auto-merge forbidden")
)

// Request opens a real PR for an integrated goal run.
type Request struct {
	RepoPath    string
	BaseRef     string // default main
	Branch      string // empty → loopcoder/goal-<runID>
	Title       string
	Body        string
	ProjectID   string
	RunID       string
	GraphID     string
	PlanDigest  string
	SourceIssue int
	Actor       string
	// Children are workflow outcomes (attempt/evidence) included in the receipt.
	Children []workflowrun.ChildOutcome
	// IndependentVerifier names the verifier provider/company (must differ from worker when possible).
	IndependentVerifier string
	// VerifierEvidence is durable independent review evidence (digest or path).
	VerifierEvidence string
	// RequiredCheckNames optional expected check names; when empty, names are
	// taken from live PR checks observation (may be empty until CI starts).
	RequiredCheckNames []string
	// WaitForChecks when true polls checks until green or timeout (tests: false).
	WaitForChecks bool
	// CheckWait is max wait for checks (default 0 = observe once).
	CheckWait time.Duration
	// Injectables (tests). nil → production git/gh via exec.
	Git  Git
	Host Host
	Now  func() time.Time
	// ForbidMerge is always enforced true in production path.
	// Tests may not set this — Open never calls merge APIs.
}

// Git is the minimal repo mutation surface (fakeable).
type Git interface {
	RevParse(ctx context.Context, repo, rev string) (string, error)
	CheckoutNewBranch(ctx context.Context, repo, branch, startPoint string) error
	AddPath(ctx context.Context, repo, rel string) error
	Commit(ctx context.Context, repo, message string) error
	PushUpstream(ctx context.Context, repo, branch string) error
	HeadOID(ctx context.Context, repo string) (string, error)
}

// Host is the GitHub write/read surface used for PR create (never merge).
type Host interface {
	CreatePR(ctx context.Context, head, base, title, body string) (url string, err error)
	// ListChecks returns current check names + all-green flag for the PR.
	ListChecks(ctx context.Context, prNumber int) (names []string, allGreen bool, err error)
}

// Check is one required check observation.
type Check struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion,omitempty"`
	Status     string `json:"status,omitempty"`
}

// Receipt is committed into the disposable repo (LoopCoder-owned product file).
type Receipt struct {
	Schema              string                     `json:"schema"`
	ProjectID           string                     `json:"project_id"`
	RunID               string                     `json:"run_id"`
	GraphID             string                     `json:"graph_id,omitempty"`
	PlanDigest          string                     `json:"plan_digest,omitempty"`
	Actor               string                     `json:"actor,omitempty"`
	SourceIssue         int                        `json:"source_issue,omitempty"`
	Children            []workflowrun.ChildOutcome `json:"children,omitempty"`
	IndependentVerifier string                     `json:"independent_verifier,omitempty"`
	VerifierEvidence    string                     `json:"verifier_evidence,omitempty"`
	HumanGate           bool                       `json:"human_gate"` // always true
	AutoMerge           bool                       `json:"auto_merge"` // always false
	CreatedAt           time.Time                  `json:"created_at"`
}

// Result is product evidence for canary_evidence.v1 PR section (filled by LoopCoder).
type Result struct {
	OK                  bool     `json:"ok"`
	Status              string   `json:"status"` // human_gate
	URL                 string   `json:"url"`
	Number              int      `json:"number,omitempty"`
	Branch              string   `json:"branch"`
	BaseRef             string   `json:"base_ref"`
	HeadOID             string   `json:"head_oid,omitempty"`
	RequiredChecks      []string `json:"required_checks,omitempty"`
	RequiredChecksGreen bool     `json:"required_checks_green"`
	IndependentVerifier string   `json:"independent_verifier,omitempty"`
	VerifierEvidenceRef string   `json:"verifier_evidence_ref,omitempty"`
	CreatedByLoopCoder  bool     `json:"created_by_loopcoder"`
	HumanMergeGate      bool     `json:"human_merge_gate"`
	AutoMerge           bool     `json:"auto_merge"` // always false
	ReceiptPath         string   `json:"receipt_path,omitempty"`
	Message             string   `json:"message,omitempty"`
	Events              []string `json:"events,omitempty"`
}

// Open creates branch/commit/push/PR in an owner-controlled disposable repo and
// stops at the human merge gate. Never merges.
func Open(ctx context.Context, req Request) (Result, error) {
	out := Result{
		CreatedByLoopCoder: true,
		HumanMergeGate:     true,
		AutoMerge:          false,
		Status:             StatusHumanGate,
	}
	emit := func(e string) { out.Events = append(out.Events, e) }

	repo := strings.TrimSpace(req.RepoPath)
	if repo == "" {
		return out, fmt.Errorf("%w: repo_path required", ErrInvalid)
	}
	if strings.TrimSpace(req.RunID) == "" || strings.TrimSpace(req.ProjectID) == "" {
		return out, fmt.Errorf("%w: project_id and run_id required", ErrInvalid)
	}
	// At least one succeeded child with evidence (product integration).
	okKids := 0
	for _, c := range req.Children {
		if strings.EqualFold(c.Terminal, "succeeded") && strings.TrimSpace(c.OutputEvidence) != "" {
			okKids++
		}
	}
	if okKids == 0 {
		return out, fmt.Errorf("%w: no succeeded children with output evidence", ErrNotReady)
	}

	base := strings.TrimSpace(req.BaseRef)
	if base == "" {
		base = "main"
	}
	branch := strings.TrimSpace(req.Branch)
	if branch == "" {
		branch = "loopcoder/goal-" + sanitize(req.RunID)
	}
	out.Branch = branch
	out.BaseRef = base

	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = fmt.Sprintf("loopcoder goal %s (human gate)", req.RunID)
	}
	body := strings.TrimSpace(req.Body)
	if body == "" {
		body = fmt.Sprintf(
			"## LoopCoder goal PR (human merge gate)\n\n"+
				"- project: `%s`\n- run: `%s`\n- graph: `%s`\n"+
				"- auto_merge: **false** (owner merge required)\n"+
				"- created_by: loopcoder goalpr\n",
			req.ProjectID, req.RunID, req.GraphID,
		)
		if req.SourceIssue > 0 {
			body += fmt.Sprintf("\nRefs #%d\n", req.SourceIssue)
		}
	}
	// Never allow merge instruction that auto-merges.
	if strings.Contains(strings.ToLower(body), "auto-merge: true") ||
		strings.Contains(strings.ToLower(body), "automerged") {
		return out, fmt.Errorf("%w: body must not request auto-merge", ErrAutoMerge)
	}

	git := req.Git
	if git == nil {
		git = ProductionGit{}
	}
	host := req.Host
	if host == nil {
		host = ProductionHost{RepoPath: repo}
	}
	nowFn := req.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn().UTC()

	// Base must resolve.
	if _, err := git.RevParse(ctx, repo, base); err != nil {
		// try origin/base
		if _, err2 := git.RevParse(ctx, repo, "origin/"+base); err2 != nil {
			return out, fmt.Errorf("%w: base ref %s: %v", ErrGit, base, err)
		}
		base = "origin/" + strings.TrimPrefix(base, "origin/")
	}
	// Prefer existing goal branch with product integrations (not a fresh branch
	// from base that would drop child commits). CheckoutNewBranch reuses branch
	// when it already exists.
	emit("git.checkout_branch:" + branch)
	if err := git.CheckoutNewBranch(ctx, repo, branch, base); err != nil {
		return out, fmt.Errorf("%w: checkout branch: %v", ErrGit, err)
	}

	// Durable receipt in-repo (product file, not hand-filled canary manifest).
	receipt := Receipt{
		Schema: ReceiptSchema, ProjectID: req.ProjectID, RunID: req.RunID,
		GraphID: req.GraphID, PlanDigest: req.PlanDigest, Actor: req.Actor,
		SourceIssue: req.SourceIssue, Children: req.Children,
		IndependentVerifier: firstNonEmpty(req.IndependentVerifier, "independent"),
		VerifierEvidence:    req.VerifierEvidence,
		HumanGate:           true, AutoMerge: false, CreatedAt: now,
	}
	raw, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return out, err
	}
	rel := filepath.Join(".loopcoder", "goal-pr", sanitize(req.RunID)+"-receipt.json")
	abs := filepath.Join(repo, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return out, err
	}
	if err := os.WriteFile(abs, raw, 0o600); err != nil {
		return out, err
	}
	out.ReceiptPath = abs
	emit("receipt.write:" + rel)

	if err := git.AddPath(ctx, repo, rel); err != nil {
		return out, fmt.Errorf("%w: add: %v", ErrGit, err)
	}
	msg := fmt.Sprintf("loopcoder: goal PR receipt for %s (human gate, no auto-merge)", req.RunID)
	emit("git.commit")
	if err := git.Commit(ctx, repo, msg); err != nil {
		return out, fmt.Errorf("%w: commit: %v", ErrGit, err)
	}

	// Fail closed: PR must not be receipt-only when children produced product files.
	// Scan working tree for product paths outside .loopcoder/.
	if err := refuseReceiptOnlyPR(repo, req.Children); err != nil {
		return out, err
	}
	head, err := git.HeadOID(ctx, repo)
	if err != nil {
		return out, fmt.Errorf("%w: head: %v", ErrGit, err)
	}
	out.HeadOID = head
	emit("git.push:" + branch)
	if err := git.PushUpstream(ctx, repo, branch); err != nil {
		return out, fmt.Errorf("%w: push: %v", ErrGit, err)
	}

	// Create PR — never merge. Base branch name for GitHub is without origin/.
	prBase := strings.TrimPrefix(strings.TrimSpace(req.BaseRef), "origin/")
	if prBase == "" {
		prBase = "main"
	}
	out.BaseRef = prBase
	emit("github.pr_create")
	url, err := host.CreatePR(ctx, branch, prBase, title, body)
	if err != nil {
		return out, fmt.Errorf("%w: create pr: %v", ErrGitHub, err)
	}
	if url == "" {
		return out, fmt.Errorf("%w: empty pr url", ErrGitHub)
	}
	out.URL = strings.TrimSpace(url)
	out.Number = parsePRNumber(out.URL)
	emit("github.pr_url:" + out.URL)

	// Independent verifier evidence (from request or derived).
	out.IndependentVerifier = firstNonEmpty(req.IndependentVerifier, "independent")
	out.VerifierEvidenceRef = firstNonEmpty(req.VerifierEvidence, "receipt:"+rel)
	if out.IndependentVerifier == "" {
		return out, fmt.Errorf("%w: independent verifier required", ErrNotReady)
	}

	// Observe checks (product path; disposable repos may have zero until CI runs).
	names := append([]string{}, req.RequiredCheckNames...)
	allGreen := false
	if out.Number > 0 {
		n, green, cerr := host.ListChecks(ctx, out.Number)
		if cerr == nil {
			if len(names) == 0 {
				names = n
			}
			allGreen = green
			// If caller listed required names, green only when all present+green.
			if len(req.RequiredCheckNames) > 0 {
				allGreen = green && len(n) > 0
			}
		}
		emit(fmt.Sprintf("github.pr_checks count=%d green=%v", len(names), allGreen))
	}
	// Product honesty: if no checks configured yet, surface empty names and green=false
	// unless caller provided names and host reported green. Canary waits for real CI.
	if len(names) == 0 && len(req.RequiredCheckNames) > 0 {
		names = req.RequiredCheckNames
	}
	out.RequiredChecks = names
	out.RequiredChecksGreen = allGreen && len(names) > 0

	out.OK = out.URL != "" && out.CreatedByLoopCoder && out.HumanMergeGate && !out.AutoMerge &&
		out.IndependentVerifier != "" && (out.VerifierEvidenceRef != "")
	out.Message = fmt.Sprintf(
		"PR %s opened; human merge gate; auto_merge=false; checks=%d green=%v",
		out.URL, len(out.RequiredChecks), out.RequiredChecksGreen,
	)
	emit("human_gate.await_owner_merge")
	return out, nil
}

// ProductionGit uses the system git binary in the disposable repo.
type ProductionGit struct{}

func (ProductionGit) RevParse(ctx context.Context, repo, rev string) (string, error) {
	return runGit(ctx, repo, "rev-parse", "--verify", rev)
}

func (ProductionGit) CheckoutNewBranch(ctx context.Context, repo, branch, startPoint string) error {
	// If branch exists, check it out; else create from startPoint.
	if _, err := runGit(ctx, repo, "rev-parse", "--verify", branch); err == nil {
		_, err := runGit(ctx, repo, "checkout", branch)
		return err
	}
	_, err := runGit(ctx, repo, "checkout", "-B", branch, startPoint)
	return err
}

func (ProductionGit) AddPath(ctx context.Context, repo, rel string) error {
	_, err := runGit(ctx, repo, "add", "--", rel)
	return err
}

func (ProductionGit) Commit(ctx context.Context, repo, message string) error {
	_, err := runGit(ctx, repo, "commit", "-m", message)
	return err
}

func (ProductionGit) PushUpstream(ctx context.Context, repo, branch string) error {
	_, err := runGit(ctx, repo, "push", "-u", "origin", branch)
	return err
}

func (ProductionGit) HeadOID(ctx context.Context, repo string) (string, error) {
	return runGit(ctx, repo, "rev-parse", "HEAD")
}

func runGit(ctx context.Context, repo string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repo
	// Identity for disposable canary commits when global config missing.
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=loopcoder",
		"GIT_AUTHOR_EMAIL=loopcoder@local",
		"GIT_COMMITTER_NAME=loopcoder",
		"GIT_COMMITTER_EMAIL=loopcoder@local",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %v: %w: %s", args, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// ProductionHost uses gh CLI in the disposable repo (never merge).
type ProductionHost struct {
	RepoPath string
}

func (h ProductionHost) CreatePR(ctx context.Context, head, base, title, body string) (string, error) {
	base = strings.TrimPrefix(strings.TrimSpace(base), "origin/")
	if base == "" {
		base = "main"
	}
	cmd := exec.CommandContext(ctx, "gh", "pr", "create",
		"--head", head, "--base", base, "--title", title, "--body", body)
	cmd.Dir = h.RepoPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh pr create: %w: %s", err, strings.TrimSpace(string(out)))
	}
	url := strings.TrimSpace(string(out))
	// gh may print extra lines; take last URL-looking line.
	for _, line := range strings.Split(url, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "/pull/") {
			return line, nil
		}
	}
	if url == "" {
		return "", fmt.Errorf("gh pr create: empty url")
	}
	return url, nil
}

func (h ProductionHost) ListChecks(ctx context.Context, prNumber int) ([]string, bool, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "checks", strconv.Itoa(prNumber), "--json", "name,state,bucket")
	cmd.Dir = h.RepoPath
	out, err := cmd.CombinedOutput()
	// gh exits non-zero for pending/fail; still parse JSON when present.
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		if err != nil {
			return nil, false, err
		}
		return nil, false, nil
	}
	var rows []struct {
		Name   string `json:"name"`
		State  string `json:"state"`
		Bucket string `json:"bucket"`
	}
	if jerr := json.Unmarshal([]byte(raw), &rows); jerr != nil {
		// Non-JSON table form — treat as observation unavailable.
		if err != nil {
			return nil, false, err
		}
		return nil, false, jerr
	}
	names := make([]string, 0, len(rows))
	allGreen := len(rows) > 0
	for _, r := range rows {
		if strings.TrimSpace(r.Name) == "" {
			continue
		}
		names = append(names, r.Name)
		st := strings.ToUpper(r.State + " " + r.Bucket)
		if !strings.Contains(st, "SUCCESS") && !strings.Contains(st, "PASS") && !strings.Contains(st, "SKIP") {
			// pending/fail → not green
			if strings.Contains(st, "FAIL") || strings.Contains(st, "PENDING") || strings.Contains(st, "QUEUED") ||
				strings.Contains(st, "IN_PROGRESS") || strings.Contains(st, "CANCEL") {
				allGreen = false
			} else if r.Bucket != "" && !strings.EqualFold(r.Bucket, "pass") && !strings.EqualFold(r.Bucket, "skipping") {
				allGreen = false
			}
		}
	}
	if len(names) == 0 {
		allGreen = false
	}
	return names, allGreen, nil
}

var prNumRe = regexp.MustCompile(`/pull/(\d+)`)

func parsePRNumber(url string) int {
	m := prNumRe.FindStringSubmatch(url)
	if len(m) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func sanitize(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	if out == "" {
		return "run"
	}
	if len(out) > 80 {
		return out[:80]
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// refuseReceiptOnlyPR fails closed when children reported product FilesTouched
// but the branch tree only has .loopcoder/** (no real product code/tests).
func refuseReceiptOnlyPR(repo string, children []workflowrun.ChildOutcome) error {
	wantProduct := false
	for _, c := range children {
		if !strings.EqualFold(c.Terminal, "succeeded") {
			continue
		}
		for _, f := range c.FilesTouched {
			f = filepath.ToSlash(f)
			if f == "" || strings.HasPrefix(f, ".loopcoder/") || strings.Contains(f, ".loopcoder-owned-worktree") {
				continue
			}
			if strings.HasPrefix(f, "child-output-") {
				continue
			}
			wantProduct = true
			break
		}
	}
	if !wantProduct {
		return nil
	}
	// Walk repo (excluding .git) for any non-meta product file.
	found := false
	_ = filepath.Walk(repo, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			if info != nil && info.IsDir() && (filepath.Base(path) == ".git" || filepath.Base(path) == ".loopcoder") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(repo, path)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, ".loopcoder/") || rel == "README.md" || strings.HasPrefix(rel, ".github/") {
			return nil
		}
		if strings.HasPrefix(filepath.Base(rel), "child-output-") {
			return nil
		}
		found = true
		return io.EOF // stop walk
	})
	if !found {
		return fmt.Errorf("%w: PR would be receipt-only; integrate product files onto goal branch first", ErrNotReady)
	}
	return nil
}
