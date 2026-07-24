package prstage

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// GHClient implements GitHub via the official gh CLI (no synthetic PR numbers).
// Production only — tests inject FakeGitHub explicitly.
type GHClient struct {
	// RepoDir optional cwd so gh uses local remote context.
	RepoDir string
}

// NewGHClient returns a real GitHub port. Requires gh on PATH and authenticated session.
func NewGHClient(repoDir string) *GHClient {
	return &GHClient{RepoDir: strings.TrimSpace(repoDir)}
}

func (c *GHClient) run(args ...string) (string, error) {
	cmd := exec.Command("gh", args...)
	if c.RepoDir != "" {
		cmd.Dir = c.RepoDir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (c *GHClient) FindCompatible(owner, name, baseRef, headRef string) (PRView, bool, error) {
	owner, name = strings.TrimSpace(owner), strings.TrimSpace(name)
	baseRef, headRef = strings.TrimSpace(baseRef), strings.TrimSpace(headRef)
	if owner == "" || name == "" || headRef == "" {
		return PRView{}, false, fmt.Errorf("%w: owner/name/head required", ErrInvalid)
	}
	repo := owner + "/" + name
	out, err := c.run("pr", "list", "--repo", repo, "--head", headRef, "--base", baseRef,
		"--state", "open", "--json", "number,title,body,url,baseRefName,headRefName,headRefOid,baseRefOid",
		"--limit", "5")
	if err != nil {
		return PRView{}, false, err
	}
	var rows []struct {
		Number      int    `json:"number"`
		Title       string `json:"title"`
		Body        string `json:"body"`
		URL         string `json:"url"`
		BaseRefName string `json:"baseRefName"`
		HeadRefName string `json:"headRefName"`
		HeadRefOid  string `json:"headRefOid"`
		BaseRefOid  string `json:"baseRefOid"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return PRView{}, false, fmt.Errorf("%w: pr list json: %v", ErrInvalid, err)
	}
	if len(rows) == 0 {
		return PRView{}, false, nil
	}
	r := rows[0]
	return PRView{
		Number: r.Number, RepoOwner: owner, RepoName: name,
		BaseRef: r.BaseRefName, BaseOID: r.BaseRefOid,
		HeadRef: r.HeadRefName, HeadOID: r.HeadRefOid,
		Title: r.Title, Body: r.Body, URL: r.URL, Open: true,
	}, true, nil
}

func (c *GHClient) CreatePR(owner, name, baseRef, headRef, title, body string) (PRView, error) {
	owner, name = strings.TrimSpace(owner), strings.TrimSpace(name)
	baseRef, headRef = strings.TrimSpace(baseRef), strings.TrimSpace(headRef)
	if owner == "" || name == "" || baseRef == "" || headRef == "" {
		return PRView{}, fmt.Errorf("%w: owner/name/base/head required", ErrInvalid)
	}
	repo := owner + "/" + name
	out, err := c.run("pr", "create", "--repo", repo, "--base", baseRef, "--head", headRef,
		"--title", title, "--body", body, "--json", "number,url,baseRefName,headRefName,headRefOid,baseRefOid,title,body")
	if err != nil {
		// Fallback: some gh versions lack --json on create; parse URL/number from text.
		out2, err2 := c.run("pr", "create", "--repo", repo, "--base", baseRef, "--head", headRef,
			"--title", title, "--body", body)
		if err2 != nil {
			msg := strings.ToLower(err.Error() + err2.Error())
			if strings.Contains(msg, "permission") || strings.Contains(msg, "403") {
				return PRView{}, ErrPermission
			}
			if strings.Contains(msg, "rate") {
				return PRView{}, ErrRateLimited
			}
			return PRView{}, err2
		}
		return c.parseCreateText(owner, name, baseRef, headRef, title, body, out2)
	}
	var row struct {
		Number      int    `json:"number"`
		URL         string `json:"url"`
		BaseRefName string `json:"baseRefName"`
		HeadRefName string `json:"headRefName"`
		HeadRefOid  string `json:"headRefOid"`
		BaseRefOid  string `json:"baseRefOid"`
		Title       string `json:"title"`
		Body        string `json:"body"`
	}
	if err := json.Unmarshal([]byte(out), &row); err != nil {
		return c.parseCreateText(owner, name, baseRef, headRef, title, body, out)
	}
	return PRView{
		Number: row.Number, RepoOwner: owner, RepoName: name,
		BaseRef: firstNonEmptyStr(row.BaseRefName, baseRef), BaseOID: row.BaseRefOid,
		HeadRef: firstNonEmptyStr(row.HeadRefName, headRef), HeadOID: row.HeadRefOid,
		Title: firstNonEmptyStr(row.Title, title), Body: firstNonEmptyStr(row.Body, body),
		URL: row.URL, Open: true,
	}, nil
}

func (c *GHClient) parseCreateText(owner, name, baseRef, headRef, title, body, text string) (PRView, error) {
	// Expect a URL like https://github.com/o/n/pull/123
	text = strings.TrimSpace(text)
	num := 0
	url := ""
	for _, field := range strings.Fields(text) {
		if strings.Contains(field, "/pull/") {
			url = field
			parts := strings.Split(field, "/pull/")
			if len(parts) == 2 {
				n, _ := strconv.Atoi(strings.TrimRight(parts[1], "/"))
				if n > 0 {
					num = n
				}
			}
		}
	}
	if num <= 0 {
		return PRView{}, fmt.Errorf("%w: cannot parse pr number from %q", ErrInvalid, text)
	}
	return PRView{
		Number: num, RepoOwner: owner, RepoName: name,
		BaseRef: baseRef, HeadRef: headRef, Title: title, Body: body, URL: url, Open: true,
	}, nil
}

func (c *GHClient) ReadPR(owner, name string, number int) (PRView, error) {
	if number <= 0 {
		return PRView{}, fmt.Errorf("%w: pr number", ErrInvalid)
	}
	repo := strings.TrimSpace(owner) + "/" + strings.TrimSpace(name)
	out, err := c.run("pr", "view", strconv.Itoa(number), "--repo", repo,
		"--json", "number,title,body,url,baseRefName,headRefName,headRefOid,baseRefOid,state")
	if err != nil {
		return PRView{}, err
	}
	var row struct {
		Number      int    `json:"number"`
		Title       string `json:"title"`
		Body        string `json:"body"`
		URL         string `json:"url"`
		BaseRefName string `json:"baseRefName"`
		HeadRefName string `json:"headRefName"`
		HeadRefOid  string `json:"headRefOid"`
		BaseRefOid  string `json:"baseRefOid"`
		State       string `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &row); err != nil {
		return PRView{}, fmt.Errorf("%w: pr view json: %v", ErrInvalid, err)
	}
	return PRView{
		Number: row.Number, RepoOwner: owner, RepoName: name,
		BaseRef: row.BaseRefName, BaseOID: row.BaseRefOid,
		HeadRef: row.HeadRefName, HeadOID: row.HeadRefOid,
		Title: row.Title, Body: row.Body, URL: row.URL,
		Open: strings.EqualFold(row.State, "OPEN") || strings.EqualFold(row.State, "open"),
	}, nil
}

func (c *GHClient) RateLimited() bool { return false }

func (c *GHClient) Authorized() bool {
	_, err := c.run("auth", "status")
	return err == nil
}

// ObserveChecks returns required-check conclusions for a PR head via gh.
// Empty/missing required checks stay unknown — never auto-green.
// Binds exact head SHA: reads PR headRefOid before checks and rejects moved head.
func ObserveChecks(repoDir, owner, name string, pr int, head string, required []string) ([]struct {
	Name       string
	Conclusion string
	Required   bool
}, error) {
	if pr <= 0 || strings.TrimSpace(head) == "" {
		return nil, fmt.Errorf("%w: pr/head required", ErrInvalid)
	}
	repo := strings.TrimSpace(owner) + "/" + strings.TrimSpace(name)
	// Exact head binding: fail closed on every error/empty OID/non-open state.
	// Never skip comparison.
	headBefore, err := readPRHeadOID(repoDir, repo, pr)
	if err != nil {
		return nil, err
	}
	if headBefore != strings.TrimSpace(head) {
		return nil, fmt.Errorf("%w: pr head moved before checks got=%s expected=%s", ErrConflict, headBefore, head)
	}
	cmd := exec.Command("gh", "pr", "checks", strconv.Itoa(pr), "--repo", repo, "--json", "name,state,bucket")
	if strings.TrimSpace(repoDir) != "" {
		cmd.Dir = repoDir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh pr checks: %w: %s", err, strings.TrimSpace(string(out)))
	}
	headAfter, aerr := readPRHeadOID(repoDir, repo, pr)
	if aerr != nil {
		return nil, aerr
	}
	if headAfter != strings.TrimSpace(head) {
		return nil, fmt.Errorf("%w: pr head moved after checks got=%s expected=%s", ErrConflict, headAfter, head)
	}
	var rows []struct {
		Name   string `json:"name"`
		State  string `json:"state"`
		Bucket string `json:"bucket"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("%w: checks json: %v", ErrInvalid, err)
	}
	byName := map[string]string{}
	for _, r := range rows {
		// Map gh bucket/state to ciwatch conclusions.
		conc := strings.ToLower(strings.TrimSpace(r.Bucket))
		if conc == "" {
			conc = strings.ToLower(strings.TrimSpace(r.State))
		}
		switch conc {
		case "pass", "success", "completed":
			conc = "success"
		case "fail", "failure", "failed":
			conc = "failure"
		case "pending", "running", "queued", "in_progress":
			conc = "pending"
		default:
			// keep raw lower
		}
		byName[strings.TrimSpace(r.Name)] = conc
	}
	var outChecks []struct {
		Name       string
		Conclusion string
		Required   bool
	}
	for _, n := range required {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		conc, ok := byName[n]
		if !ok {
			// Try case-insensitive match.
			for k, v := range byName {
				if strings.EqualFold(k, n) {
					conc, ok = v, true
					break
				}
			}
		}
		if !ok {
			conc = "missing"
		}
		outChecks = append(outChecks, struct {
			Name       string
			Conclusion string
			Required   bool
		}{Name: n, Conclusion: conc, Required: true})
	}
	return outChecks, nil
}

func firstNonEmptyStr(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// readPRHeadOID returns the current headRefOid for a PR (exact SHA binding).
// Fail closed: gh errors, malformed JSON, empty OID, or non-open state all error.
// Never returns empty,nil (that previously skipped head comparison).
func readPRHeadOID(repoDir, repo string, pr int) (string, error) {
	return readPRHeadOIDCmd(repoDir, repo, pr, nil)
}

// readPRHeadOIDCmd allows tests to inject a fake runner (moved-head/error/malformed).
// When run is nil, executes real `gh pr view`.
func readPRHeadOIDCmd(repoDir, repo string, pr int, run func(dir string, argv ...string) ([]byte, error)) (string, error) {
	argv := []string{"pr", "view", strconv.Itoa(pr), "--repo", repo, "--json", "headRefOid,state"}
	var out []byte
	var err error
	if run != nil {
		out, err = run(repoDir, append([]string{"gh"}, argv...)...)
	} else {
		cmd := exec.Command("gh", argv...)
		if strings.TrimSpace(repoDir) != "" {
			cmd.Dir = repoDir
		}
		out, err = cmd.CombinedOutput()
	}
	if err != nil {
		return "", fmt.Errorf("%w: gh pr view head: %v: %s", ErrInvalid, err, strings.TrimSpace(string(out)))
	}
	var row struct {
		HeadRefOid string `json:"headRefOid"`
		State      string `json:"state"`
	}
	if json.Unmarshal(out, &row) != nil {
		return "", fmt.Errorf("%w: gh pr view head: malformed json", ErrInvalid)
	}
	oid := strings.TrimSpace(row.HeadRefOid)
	if oid == "" {
		return "", fmt.Errorf("%w: gh pr view head: empty headRefOid", ErrInvalid)
	}
	state := strings.ToLower(strings.TrimSpace(row.State))
	if state != "" && state != "open" {
		return "", fmt.Errorf("%w: pr state %q is not open", ErrConflict, row.State)
	}
	if state == "" {
		// Some gh versions omit state; require explicit open when present.
		// Empty state is fail-closed: we cannot prove OPEN.
		return "", fmt.Errorf("%w: gh pr view head: empty state (cannot prove OPEN)", ErrInvalid)
	}
	return oid, nil
}

// SetReadPRHeadOIDForTest injects a test runner for readPRHeadOID (tests only).
// Prefer calling readPRHeadOIDCmd directly from tests.
var testReadPRHeadOIDRun func(dir string, argv ...string) ([]byte, error)

func init() {
	// Wire optional test hook without changing production path signature.
	_ = testReadPRHeadOIDRun
}
