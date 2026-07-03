package github

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseJSONOutputTrimsWarnings(t *testing.T) {
	var issues []Issue
	input := []byte("warning: ignored\n[{\"number\":93,\"title\":\"Ready\",\"labels\":[{\"name\":\"blocked-by:#1\"}],\"state\":\"OPEN\"}]\ntrailing")

	if err := parseJSONOutput(input, &issues); err != nil {
		t.Fatalf("parseJSONOutput returned error: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("parsed %d issues, want 1", len(issues))
	}
	if issues[0].Number != 93 || issues[0].Labels[0].Name != "blocked-by:#1" {
		t.Fatalf("parsed issue incorrectly: %#v", issues[0])
	}
}

func TestParseJSONOutputParsesObject(t *testing.T) {
	var payload struct {
		NameWithOwner string `json:"nameWithOwner"`
	}
	if err := parseJSONOutput([]byte("prefix {\"nameWithOwner\":\"owner/repo\"} suffix"), &payload); err != nil {
		t.Fatalf("parseJSONOutput returned error: %v", err)
	}
	if payload.NameWithOwner != "owner/repo" {
		t.Fatalf("NameWithOwner = %q, want owner/repo", payload.NameWithOwner)
	}
}

func TestParseGitHubRemote(t *testing.T) {
	cases := map[string]string{
		"git@github.com:owner/repo.git":      "owner/repo",
		"https://github.com/owner/repo.git":  "owner/repo",
		"https://github.com/owner/repo":      "owner/repo",
		"https://example.com/owner/repo.git": "",
	}
	for input, want := range cases {
		if got := parseGitHubRemote(input); got != want {
			t.Fatalf("parseGitHubRemote(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestExecRunnerCapturesOutputAndNonZeroExit(t *testing.T) {
	withTestGHCap(t, 2*time.Second)
	dir := t.TempDir()

	output, err := (ExecRunner{}).Run(context.Background(), dir, os.Args[0], "-test.run=TestGitHubExecHelper", "--", "stdout", "hello")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if string(output) != "hello\n" {
		t.Fatalf("output = %q, want hello newline", output)
	}

	output, err = (ExecRunner{}).Run(context.Background(), dir, os.Args[0], "-test.run=TestGitHubExecHelper", "--", "stderr-exit", "detail", "7")
	if err == nil {
		t.Fatal("Run error = nil, want non-zero exit error")
	}
	if output != nil {
		t.Fatalf("output = %q, want nil on error", output)
	}
	if !strings.Contains(err.Error(), "exit status 7") || !strings.Contains(err.Error(), "detail") {
		t.Fatalf("error = %q, want exit status and stderr detail", err.Error())
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("error = %v, want wrapped exec.ExitError exit 7", err)
	}
}

func TestExecRunnerTimesOut(t *testing.T) {
	withTestGHCap(t, 50*time.Millisecond)
	dir := t.TempDir()

	start := time.Now()
	_, err := (ExecRunner{}).Run(context.Background(), dir, os.Args[0], "-test.run=TestGitHubExecHelper", "--", "sleep", "5s")
	if err == nil {
		t.Fatal("Run error = nil, want timeout")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Run elapsed = %s, want bounded timeout", elapsed)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %q, want timeout", err.Error())
	}
}

func withTestGHCap(t *testing.T, hardCap time.Duration) {
	t.Helper()
	oldHardCap := ghHardCap
	ghHardCap = hardCap
	t.Setenv("GO_WANT_GITHUB_HELPER", "1")
	t.Cleanup(func() {
		ghHardCap = oldHardCap
	})
}

func TestGitHubExecHelper(t *testing.T) {
	if os.Getenv("GO_WANT_GITHUB_HELPER") != "1" {
		return
	}
	runExecHelper()
}

func runExecHelper() {
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		fmt.Fprintln(os.Stderr, "missing helper mode")
		os.Exit(2)
	}
	mode := os.Args[separator+1]
	args := os.Args[separator+2:]
	switch mode {
	case "stdout":
		fmt.Fprintln(os.Stdout, args[0])
	case "stderr-exit":
		fmt.Fprintln(os.Stderr, args[0])
		os.Exit(parseHelperInt(args[1]))
	case "sleep":
		time.Sleep(parseHelperDuration(args[0]))
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
		os.Exit(2)
	}
	os.Exit(0)
}

func parseHelperDuration(value string) time.Duration {
	duration, err := time.ParseDuration(value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse duration %q: %v\n", value, err)
		os.Exit(2)
	}
	return duration
}

func parseHelperInt(value string) int {
	var n int
	if _, err := fmt.Sscanf(value, "%d", &n); err != nil {
		fmt.Fprintf(os.Stderr, "parse int %q: %v\n", value, err)
		os.Exit(2)
	}
	return n
}

func TestCreatePRRunsGhPRCreate(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"repo\x00gh\x00pr\x00create\x00--head\x00loop/issue-101\x00--base\x00main\x00--title\x00Title\x00--body\x00Body": []byte("https://github.com/owner/repo/pull/101\n"),
		},
	}
	client := NewWithRunner("repo", runner)

	got, err := client.CreatePR(context.Background(), "loop/issue-101", "main", "Title", "Body")
	if err != nil {
		t.Fatalf("CreatePR returned error: %v", err)
	}
	if got != "https://github.com/owner/repo/pull/101" {
		t.Fatalf("CreatePR URL = %q", got)
	}

	want := [][]string{{"repo", "gh", "pr", "create", "--head", "loop/issue-101", "--base", "main", "--title", "Title", "--body", "Body"}}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
}

func TestListHeadPRsRunsGhPRList(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"repo\x00gh\x00pr\x00list\x00--head\x00loop/issue-101\x00--json\x00number,url": []byte(`[{"number":5,"url":"https://github.com/owner/repo/pull/5"}]`),
		},
	}
	client := NewWithRunner("repo", runner)

	got, err := client.ListHeadPRs(context.Background(), "loop/issue-101")
	if err != nil {
		t.Fatalf("ListHeadPRs returned error: %v", err)
	}
	if len(got) != 1 || got[0].Number != 5 || got[0].URL != "https://github.com/owner/repo/pull/5" {
		t.Fatalf("ListHeadPRs = %#v", got)
	}
}

func TestMergeToPreProdRunsGitHubMergeAPI(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"repo\x00gh\x00pr\x00view\x00101\x00--json\x00number,title,body,url,headRefName,isDraft,closingIssuesReferences":                                                                []byte(`{"number":101,"headRefName":"loop/issue-101","url":"https://github.com/owner/repo/pull/101"}`),
			"repo\x00gh\x00repo\x00view\x00--json\x00nameWithOwner":                                                                                                                         []byte(`{"nameWithOwner":"owner/repo"}`),
			"repo\x00gh\x00api\x00--method\x00POST\x00repos/owner/repo/merges\x00-f\x00base=pre-prod\x00-f\x00head=loop/issue-101\x00-f\x00commit_message=loopcoder pre-prod merge PR #101": []byte(`{"sha":"abc123","html_url":"https://github.com/owner/repo/commit/abc123"}`),
		},
	}
	client := NewWithRunner("repo", runner)

	got, err := client.MergeToPreProd(context.Background(), 101, "pre-prod")
	if err != nil {
		t.Fatalf("MergeToPreProd returned error: %v", err)
	}
	if got.PRNumber != 101 || got.Branch != "pre-prod" || got.Head != "loop/issue-101" || got.SHA != "abc123" {
		t.Fatalf("MergeToPreProd result = %#v", got)
	}

	want := [][]string{
		{"repo", "gh", "pr", "view", "101", "--json", "number,title,body,url,headRefName,isDraft,closingIssuesReferences"},
		{"repo", "gh", "repo", "view", "--json", "nameWithOwner"},
		{"repo", "gh", "api", "--method", "POST", "repos/owner/repo/merges", "-f", "base=pre-prod", "-f", "head=loop/issue-101", "-f", "commit_message=loopcoder pre-prod merge PR #101"},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
}

func TestMergeToPreProdRejectsProductionBranches(t *testing.T) {
	client := NewWithRunner("repo", &fakeRunner{outputs: map[string][]byte{}})

	for _, branch := range []string{"main", "master", "prod", "production"} {
		_, err := client.MergeToPreProd(context.Background(), 101, branch)
		if err == nil {
			t.Fatalf("MergeToPreProd(%q) returned nil error, want rejection", branch)
		}
	}
}

func TestBranchChecksReadsHeadCheckRunsAndStatuses(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"repo\x00git\x00ls-remote\x00origin\x00refs/heads/pre-prod":                    []byte("abc123\trefs/heads/pre-prod\n"),
			"repo\x00gh\x00repo\x00view\x00--json\x00nameWithOwner":                        []byte(`{"nameWithOwner":"owner/repo"}`),
			"repo\x00gh\x00api\x00repos/owner/repo/commits/abc123/check-runs?per_page=100": []byte(`{"check_runs":[{"name":"verify","status":"completed","conclusion":"success"},{"name":"go","status":"completed","conclusion":"failure"}]}`),
			"repo\x00gh\x00api\x00repos/owner/repo/commits/abc123/status":                  []byte(`{"statuses":[{"context":"legacy","state":"success"}]}`),
		},
	}
	client := NewWithRunner("repo", runner)

	got, err := client.BranchChecks(context.Background(), "pre-prod")
	if err != nil {
		t.Fatalf("BranchChecks returned error: %v", err)
	}
	if got.Branch != "pre-prod" || got.HeadSHA != "abc123" {
		t.Fatalf("BranchChecks identity = %#v", got)
	}
	if !reflect.DeepEqual(got.Checks, []Check{
		{Name: "verify", State: "success", Bucket: "pass"},
		{Name: "go", State: "failure", Bucket: "fail"},
		{Name: "legacy", State: "success", Bucket: "pass"},
	}) {
		t.Fatalf("BranchChecks checks = %#v", got.Checks)
	}
}

func TestBranchHeadSHAReadsRef(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"repo\x00git\x00ls-remote\x00origin\x00refs/heads/main": []byte("main-head-sha\trefs/heads/main\n"),
		},
	}
	client := NewWithRunner("repo", runner)

	got, err := client.BranchHeadSHA(context.Background(), "main")
	if err != nil {
		t.Fatalf("BranchHeadSHA returned error: %v", err)
	}
	if got != "main-head-sha" {
		t.Fatalf("BranchHeadSHA = %q, want main-head-sha", got)
	}
}

func TestRevertOnPreProdUsesTemporaryWorktreeAndPushesOnlyPreProd(t *testing.T) {
	runner := &preProdRevertRunner{}
	client := NewWithRunner("repo", runner)

	got, err := client.RevertOnPreProd(context.Background(), 101, "pre-prod", "merge-sha")
	if err != nil {
		t.Fatalf("RevertOnPreProd returned error: %v", err)
	}
	if got.PRNumber != 101 || got.Branch != "pre-prod" || got.RevertedSHA != "merge-sha" || got.SHA != "revert-sha" {
		t.Fatalf("RevertOnPreProd result = %#v", got)
	}

	var sawFetch, sawRevert, sawPush bool
	for _, call := range runner.calls {
		joined := strings.Join(call, "\x00")
		if strings.Contains(strings.ToLower(joined), "main") {
			t.Fatalf("RevertOnPreProd touched main-shaped target: %#v", call)
		}
		if reflect.DeepEqual(call, []string{"repo", "git", "fetch", "origin", "+refs/heads/pre-prod:refs/remotes/origin/pre-prod"}) {
			sawFetch = true
		}
		if len(call) >= 7 && call[1] == "git" && call[2] == "revert" && reflect.DeepEqual(call[3:], []string{"-m", "1", "--no-edit", "merge-sha"}) {
			sawRevert = true
		}
		if len(call) == 5 && call[1] == "git" && reflect.DeepEqual(call[2:], []string{"push", "origin", "HEAD:pre-prod"}) {
			sawPush = true
		}
	}
	if !sawFetch || !sawRevert || !sawPush {
		t.Fatalf("calls missing fetch=%t revert=%t push=%t: %#v", sawFetch, sawRevert, sawPush, runner.calls)
	}
}

func TestRevertOnPreProdRejectsProductionBranches(t *testing.T) {
	client := NewWithRunner("repo", &fakeRunner{outputs: map[string][]byte{}})

	for _, branch := range []string{"main", "master", "prod", "production"} {
		_, err := client.RevertOnPreProd(context.Background(), 101, branch, "merge-sha")
		if err == nil {
			t.Fatalf("RevertOnPreProd(%q) returned nil error, want rejection", branch)
		}
	}
}

func TestPromotePreProdToMainMergesAndSyncsPreProd(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"repo\x00gh\x00repo\x00view\x00--json\x00nameWithOwner": []byte(`{"nameWithOwner":"owner/repo"}`),
			"repo\x00gh\x00api\x00--method\x00POST\x00repos/owner/repo/merges\x00-f\x00base=main\x00-f\x00head=pre-prod\x00-f\x00commit_message=loopcoder promote pre-prod to main": []byte(`{"sha":"main-merge-sha","html_url":"https://github.com/owner/repo/commit/main-merge-sha"}`),
			"repo\x00git\x00fetch\x00origin\x00+refs/heads/main:refs/remotes/origin/main":                                                                                           nil,
			"repo\x00git\x00rev-parse\x00refs/remotes/origin/main":                                                                                                                  []byte("main-head-sha\n"),
			"repo\x00git\x00push\x00origin\x00refs/remotes/origin/main:refs/heads/pre-prod":                                                                                         nil,
		},
	}
	client := NewWithRunner("repo", runner)

	promoted, err := client.PromotePreProdToMain(context.Background(), "pre-prod")
	if err != nil {
		t.Fatalf("PromotePreProdToMain returned error: %v", err)
	}
	if promoted.PreProdBranch != "pre-prod" || promoted.MainBranch != "main" || promoted.Head != "pre-prod" || promoted.SHA != "main-merge-sha" {
		t.Fatalf("PromotePreProdToMain result = %#v", promoted)
	}
	synced, err := client.SyncPreProdFromMain(context.Background(), "pre-prod")
	if err != nil {
		t.Fatalf("SyncPreProdFromMain returned error: %v", err)
	}
	if synced.PreProdBranch != "pre-prod" || synced.MainBranch != "main" || synced.SHA != "main-head-sha" {
		t.Fatalf("SyncPreProdFromMain result = %#v", synced)
	}

	want := [][]string{
		{"repo", "gh", "repo", "view", "--json", "nameWithOwner"},
		{"repo", "gh", "api", "--method", "POST", "repos/owner/repo/merges", "-f", "base=main", "-f", "head=pre-prod", "-f", "commit_message=loopcoder promote pre-prod to main"},
		{"repo", "git", "fetch", "origin", "+refs/heads/main:refs/remotes/origin/main"},
		{"repo", "git", "rev-parse", "refs/remotes/origin/main"},
		{"repo", "git", "push", "origin", "refs/remotes/origin/main:refs/heads/pre-prod"},
		{"repo", "gh", "repo", "view", "--json", "nameWithOwner"},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
}

func TestPromotePreProdToMainEmptyMergeResponseIsAlreadyUpToDate(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"repo\x00gh\x00repo\x00view\x00--json\x00nameWithOwner": []byte(`{"nameWithOwner":"owner/repo"}`),
			"repo\x00gh\x00api\x00--method\x00POST\x00repos/owner/repo/merges\x00-f\x00base=main\x00-f\x00head=pre-prod\x00-f\x00commit_message=loopcoder promote pre-prod to main": nil,
		},
	}
	client := NewWithRunner("repo", runner)

	promoted, err := client.PromotePreProdToMain(context.Background(), "pre-prod")
	if err != nil {
		t.Fatalf("PromotePreProdToMain returned error: %v", err)
	}
	if promoted.PreProdBranch != "pre-prod" || promoted.MainBranch != "main" || promoted.Head != "pre-prod" {
		t.Fatalf("PromotePreProdToMain identity = %#v", promoted)
	}
	if !promoted.AlreadyUpToDate || promoted.SHA != "" || promoted.URL != "" {
		t.Fatalf("PromotePreProdToMain empty body = %#v, want already-up-to-date without SHA", promoted)
	}
}

func TestKickBackFromPreProdResolvesPRMergeAndReverts(t *testing.T) {
	runner := &preProdKickBackRunner{}
	client := NewWithRunner("repo", runner)

	got, err := client.KickBackFromPreProd(context.Background(), "#101", "pre-prod")
	if err != nil {
		t.Fatalf("KickBackFromPreProd returned error: %v", err)
	}
	if got.Item != "#101" || got.PRNumber != 101 || got.Branch != "pre-prod" || got.RevertedSHA != "merge-sha" || got.SHA != "revert-sha" {
		t.Fatalf("KickBackFromPreProd result = %#v", got)
	}

	var sawLog, sawRevert, sawPush bool
	for _, call := range runner.calls {
		if reflect.DeepEqual(call, []string{"repo", "git", "log", "--format=%H%x00%s", "refs/remotes/origin/pre-prod"}) {
			sawLog = true
		}
		if len(call) >= 7 && call[1] == "git" && call[2] == "revert" && reflect.DeepEqual(call[3:], []string{"-m", "1", "--no-edit", "merge-sha"}) {
			sawRevert = true
		}
		if len(call) == 5 && call[1] == "git" && reflect.DeepEqual(call[2:], []string{"push", "origin", "HEAD:pre-prod"}) {
			sawPush = true
		}
	}
	if !sawLog || !sawRevert || !sawPush {
		t.Fatalf("calls missing log=%t revert=%t push=%t: %#v", sawLog, sawRevert, sawPush, runner.calls)
	}
}

func TestKickBackFromPreProdResolvesCollidingPRNumberBoundary(t *testing.T) {
	runner := &preProdKickBackCollisionRunner{}
	client := NewWithRunner("repo", runner)

	got, err := client.KickBackFromPreProd(context.Background(), "#35", "pre-prod")
	if err != nil {
		t.Fatalf("KickBackFromPreProd returned error: %v", err)
	}
	if got.PRNumber != 35 || got.RevertedSHA != "merge-35" {
		t.Fatalf("KickBackFromPreProd result = %#v, want PR #35 merge", got)
	}

	for _, call := range runner.calls {
		if len(call) >= 7 && call[1] == "git" && call[2] == "revert" {
			if !reflect.DeepEqual(call[3:], []string{"-m", "1", "--no-edit", "merge-35"}) {
				t.Fatalf("revert call = %#v, want merge-35 not a colliding PR", call)
			}
			return
		}
	}
	t.Fatalf("missing git revert call: %#v", runner.calls)
}

func TestKickBackFromPreProdSkipsAlreadyRevertedCommit(t *testing.T) {
	runner := &preProdKickBackAlreadyRevertedRunner{}
	client := NewWithRunner("repo", runner)

	got, err := client.KickBackFromPreProd(context.Background(), "#101", "pre-prod")
	if err != nil {
		t.Fatalf("KickBackFromPreProd returned error: %v", err)
	}
	if got.PRNumber != 101 || got.RevertedSHA != "merge-sha" || got.SHA != "existing-revert-sha" {
		t.Fatalf("KickBackFromPreProd result = %#v, want existing revert", got)
	}
	for _, call := range runner.calls {
		if len(call) >= 3 && call[1] == "git" && (call[2] == "revert" || call[2] == "push" || call[2] == "worktree") {
			t.Fatalf("already-reverted kick-back should not mutate pre-prod, saw call %#v", call)
		}
	}
}

func TestResolvePreProdKickBackCommitAcceptsOddPRPrefix(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"repo\x00git\x00fetch\x00origin\x00+refs/heads/pre-prod:refs/remotes/origin/pre-prod": nil,
			"repo\x00git\x00log\x00--format=%H%x00%s\x00refs/remotes/origin/pre-prod":             []byte("merge-sha\x00loopcoder pre-prod merge PR #101\n"),
		},
	}
	client := NewWithRunner("repo", runner)

	sha, prNumber, err := client.resolvePreProdKickBackCommit(context.Background(), "pr:#101", "pre-prod")
	if err != nil {
		t.Fatalf("resolvePreProdKickBackCommit returned error: %v", err)
	}
	if sha != "merge-sha" || prNumber != 101 {
		t.Fatalf("resolved sha=%q pr=%d, want PR #101 merge", sha, prNumber)
	}
}

func TestResolvePreProdKickBackCommitTreatsShortBareNumericAsPRNumber(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"repo\x00git\x00fetch\x00origin\x00+refs/heads/pre-prod:refs/remotes/origin/pre-prod": nil,
			"repo\x00git\x00log\x00--format=%H%x00%s\x00refs/remotes/origin/pre-prod":             []byte("numeric-merge-sha\x00loopcoder pre-prod merge PR #123\n"),
		},
	}
	client := NewWithRunner("repo", runner)

	sha, prNumber, err := client.resolvePreProdKickBackCommit(context.Background(), "123", "pre-prod")
	if err != nil {
		t.Fatalf("resolvePreProdKickBackCommit returned error: %v", err)
	}
	if sha != "numeric-merge-sha" || prNumber != 123 {
		t.Fatalf("resolved sha=%q pr=%d, want numeric PR lookup", sha, prNumber)
	}
	want := [][]string{
		{"repo", "git", "fetch", "origin", "+refs/heads/pre-prod:refs/remotes/origin/pre-prod"},
		{"repo", "git", "log", "--format=%H%x00%s", "refs/remotes/origin/pre-prod"},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want PR-resolution calls %#v", runner.calls, want)
	}
}

func TestResolvePreProdKickBackCommitTreatsLongBareNumericAsSHAWithoutLogLookup(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{}}
	client := NewWithRunner("repo", runner)

	sha, prNumber, err := client.resolvePreProdKickBackCommit(context.Background(), "1234567", "pre-prod")
	if err != nil {
		t.Fatalf("resolvePreProdKickBackCommit returned error: %v", err)
	}
	if sha != "1234567" || prNumber != 0 {
		t.Fatalf("resolved sha=%q pr=%d, want direct numeric SHA branch", sha, prNumber)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("numeric SHA branch should not fetch or scan pre-prod log, calls=%#v", runner.calls)
	}
}

func TestResolvePreProdKickBackCommitAcceptsSHAWithoutLogLookup(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{}}
	client := NewWithRunner("repo", runner)

	sha, prNumber, err := client.resolvePreProdKickBackCommit(context.Background(), "abc1234", "pre-prod")
	if err != nil {
		t.Fatalf("resolvePreProdKickBackCommit returned error: %v", err)
	}
	if sha != "abc1234" || prNumber != 0 {
		t.Fatalf("resolved sha=%q pr=%d, want direct SHA branch", sha, prNumber)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("SHA branch should not fetch or scan pre-prod log, calls=%#v", runner.calls)
	}
}

func TestRouteKickBackToNeedsHumanLabelsLinkedIssues(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"repo\x00gh\x00pr\x00view\x00101\x00--json\x00number,title,body,url,headRefName,isDraft,closingIssuesReferences": []byte(`{"number":101,"title":"Kick back item","headRefName":"loop/issue-35","closingIssuesReferences":[{"number":35}]}`),
			"repo\x00gh\x00label\x00create\x00needs-human\x00--color\x00b60205\x00--description\x00human decision required":  nil,
			"repo\x00gh\x00issue\x00edit\x0035\x00--add-label\x00needs-human":                                                nil,
		},
	}
	client := NewWithRunner("repo", runner)

	got, err := client.RouteKickBackToNeedsHuman(context.Background(), 101)
	if err != nil {
		t.Fatalf("RouteKickBackToNeedsHuman returned error: %v", err)
	}
	if got.PRNumber != 101 || got.PRLabeled || !reflect.DeepEqual(got.IssueNumbers, []int{35}) || got.Label != "needs-human" {
		t.Fatalf("route result = %#v, want issue #35 label", got)
	}

	want := [][]string{
		{"repo", "gh", "pr", "view", "101", "--json", "number,title,body,url,headRefName,isDraft,closingIssuesReferences"},
		{"repo", "gh", "label", "create", "needs-human", "--color", "b60205", "--description", "human decision required"},
		{"repo", "gh", "issue", "edit", "35", "--add-label", "needs-human"},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
}

func TestRouteKickBackToNeedsHumanLabelsPRWhenNoLinkedIssue(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"repo\x00gh\x00pr\x00view\x00101\x00--json\x00number,title,body,url,headRefName,isDraft,closingIssuesReferences": []byte(`{"number":101,"title":"Kick back item","headRefName":"feature"}`),
			"repo\x00gh\x00label\x00create\x00needs-human\x00--color\x00b60205\x00--description\x00human decision required":  nil,
			"repo\x00gh\x00pr\x00edit\x00101\x00--add-label\x00needs-human":                                                  nil,
		},
	}
	client := NewWithRunner("repo", runner)

	got, err := client.RouteKickBackToNeedsHuman(context.Background(), 101)
	if err != nil {
		t.Fatalf("RouteKickBackToNeedsHuman returned error: %v", err)
	}
	if got.PRNumber != 101 || !got.PRLabeled || len(got.IssueNumbers) != 0 {
		t.Fatalf("route result = %#v, want PR label fallback", got)
	}
}

func TestProductionPromotionMethodsRejectProductionBranches(t *testing.T) {
	methods := map[string]func(context.Context, *CLI, string) error{
		"KickBackFromPreProd": func(ctx context.Context, client *CLI, branch string) error {
			_, err := client.KickBackFromPreProd(ctx, "#101", branch)
			return err
		},
		"PromotePreProdToMain": func(ctx context.Context, client *CLI, branch string) error {
			_, err := client.PromotePreProdToMain(ctx, branch)
			return err
		},
		"SyncPreProdFromMain": func(ctx context.Context, client *CLI, branch string) error {
			_, err := client.SyncPreProdFromMain(ctx, branch)
			return err
		},
	}
	for name, method := range methods {
		for _, branch := range []string{"main", "master", "prod", "production"} {
			t.Run(name+"/"+branch, func(t *testing.T) {
				runner := &fakeRunner{outputs: map[string][]byte{}}
				client := NewWithRunner("repo", runner)

				err := method(context.Background(), client, branch)
				if err == nil || !strings.Contains(err.Error(), "reserved for human promotion") {
					t.Fatalf("%s(%q) error = %v, want reserved rejection", name, branch, err)
				}
				if len(runner.calls) != 0 {
					t.Fatalf("%s(%q) calls = %#v, want none", name, branch, runner.calls)
				}
			})
		}
	}
}

func TestWriterHasNoMergeToMainMethod(t *testing.T) {
	writer := reflect.TypeOf((*Writer)(nil)).Elem()
	for i := 0; i < writer.NumMethod(); i++ {
		method := writer.Method(i)
		name := strings.ToLower(method.Name)
		if strings.Contains(name, "main") {
			t.Fatalf("github Writer exposes merge-to-main shaped method: %s", method.Name)
		}
		if strings.Contains(name, "merge") && !strings.Contains(name, "preprod") {
			t.Fatalf("github Writer merge method is not pre-prod scoped: %s", method.Name)
		}
	}
}

func TestCreateIssueRunsGhIssueCreate(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"repo\x00gh\x00label\x00create\x00delivery:unit\x00--color\x000e8a16\x00--description\x00loopcoder work unit":                             nil,
			"repo\x00gh\x00label\x00create\x00blocked-by:#1\x00--color\x00fbca04\x00--description\x00loopcoder dependency edge":                       nil,
			"repo\x00gh\x00issue\x00create\x00--title\x00Code: Add feature\x00--body\x00Body\x00--label\x00delivery:unit\x00--label\x00blocked-by:#1": []byte("https://github.com/owner/repo/issues/7\n"),
			"repo\x00gh\x00issue\x00view\x007\x00--json\x00number,title,body,state,stateReason,labels,closedByPullRequestsReferences":                 []byte(`{"number":7,"title":"Code: Add feature","body":"Body","state":"OPEN","labels":[{"name":"delivery:unit"},{"name":"blocked-by:#1"}]}`),
		},
	}
	client := NewWithRunner("repo", runner)

	created, err := client.CreateIssue(context.Background(), "Code: Add feature", "Body", []string{"delivery:unit", "blocked-by:#1"})
	if err != nil {
		t.Fatalf("CreateIssue returned error: %v", err)
	}
	if created.Number != 7 || len(created.Labels) != 2 {
		t.Fatalf("created issue = %#v", created)
	}
}

func TestUpdateIssueAndCloseIssueRunGhCommands(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"repo\x00gh\x00label\x00create\x00epic\x00--color\x005319e7\x00--description\x00loopcoder epic work":                                               nil,
			"repo\x00gh\x00issue\x00edit\x007\x00--title\x00Epic: Add feature\x00--body\x00New body\x00--add-label\x00epic\x00--remove-label\x00blocked-by:#1": nil,
			"repo\x00gh\x00issue\x00view\x007\x00--json\x00number,title,body,state,stateReason,labels,closedByPullRequestsReferences":                          []byte(`{"number":7,"title":"Epic: Add feature","body":"New body","state":"OPEN","labels":[{"name":"delivery:unit"},{"name":"epic"}]}`),
			"repo\x00gh\x00issue\x00close\x007\x00--reason\x00not planned":                                                                                     nil,
		},
	}
	client := NewWithRunner("repo", runner)

	updated, err := client.UpdateIssue(context.Background(), 7, "Epic: Add feature", "New body", []string{"epic"}, []string{"blocked-by:#1"})
	if err != nil {
		t.Fatalf("UpdateIssue returned error: %v", err)
	}
	if updated.Title != "Epic: Add feature" || len(updated.Labels) != 2 {
		t.Fatalf("updated issue = %#v", updated)
	}
	if err := client.CloseIssue(context.Background(), 7); err != nil {
		t.Fatalf("CloseIssue returned error: %v", err)
	}
}

func TestViewPRAndDiffRunGhCommands(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"repo\x00gh\x00pr\x00view\x00152\x00--json\x00number,title,body,url,headRefName,isDraft,closingIssuesReferences": []byte(`{"number":152,"title":"PR","body":"Body","url":"https://github.com/owner/repo/pull/152","headRefName":"loop/issue-152","closingIssuesReferences":[{"number":152}]}`),
			"repo\x00gh\x00pr\x00diff\x00152":                []byte("diff --git a/a.go b/a.go\n"),
			"repo\x00gh\x00pr\x00diff\x00152\x00--name-only": []byte("a.go\r\nb.go\n"),
		},
	}
	client := NewWithRunner("repo", runner)

	pr, err := client.ViewPR(context.Background(), 152)
	if err != nil {
		t.Fatalf("ViewPR returned error: %v", err)
	}
	if pr.Number != 152 || pr.Title != "PR" || pr.Body != "Body" || pr.HeadRefName != "loop/issue-152" || len(pr.ClosingIssuesReferences) != 1 {
		t.Fatalf("ViewPR parsed incorrectly: %#v", pr)
	}
	diff, err := client.PRDiff(context.Background(), 152)
	if err != nil {
		t.Fatalf("PRDiff returned error: %v", err)
	}
	if diff != "diff --git a/a.go b/a.go\n" {
		t.Fatalf("diff = %q", diff)
	}
	files, err := client.PRDiffNameOnly(context.Background(), 152)
	if err != nil {
		t.Fatalf("PRDiffNameOnly returned error: %v", err)
	}
	if !reflect.DeepEqual(files, []string{"a.go", "b.go"}) {
		t.Fatalf("files = %#v, want a.go and b.go", files)
	}

	want := [][]string{
		{"repo", "gh", "pr", "view", "152", "--json", "number,title,body,url,headRefName,isDraft,closingIssuesReferences"},
		{"repo", "gh", "pr", "diff", "152"},
		{"repo", "gh", "pr", "diff", "152", "--name-only"},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
}

func TestCreatePRPropagatesRunnerError(t *testing.T) {
	wantErr := errors.New("gh failed")
	client := NewWithRunner("repo", &fakeRunner{err: wantErr})

	_, err := client.CreatePR(context.Background(), "head", "base", "title", "body")
	if !errors.Is(err, wantErr) {
		t.Fatalf("CreatePR error = %v, want %v", err, wantErr)
	}
}

type fakeRunner struct {
	calls   [][]string
	outputs map[string][]byte
	err     error
}

func (f *fakeRunner) Run(_ context.Context, dir, name string, args ...string) ([]byte, error) {
	call := append([]string{dir, name}, args...)
	f.calls = append(f.calls, call)
	if f.err != nil {
		return nil, f.err
	}
	key := dir + "\x00" + name
	for _, arg := range args {
		key += "\x00" + arg
	}
	return f.outputs[key], nil
}

type preProdRevertRunner struct {
	calls [][]string
}

func (r *preProdRevertRunner) Run(_ context.Context, dir, name string, args ...string) ([]byte, error) {
	call := append([]string{dir, name}, args...)
	r.calls = append(r.calls, call)
	if name == "git" && len(args) >= 2 && args[0] == "rev-parse" && args[1] == "HEAD" {
		return []byte("revert-sha\n"), nil
	}
	if name == "gh" && reflect.DeepEqual(args, []string{"repo", "view", "--json", "nameWithOwner"}) {
		return []byte(`{"nameWithOwner":"owner/repo"}`), nil
	}
	return nil, nil
}

type preProdKickBackRunner struct {
	calls [][]string
}

func (r *preProdKickBackRunner) Run(_ context.Context, dir, name string, args ...string) ([]byte, error) {
	call := append([]string{dir, name}, args...)
	r.calls = append(r.calls, call)
	if dir == "repo" && name == "git" && reflect.DeepEqual(args, []string{"log", "--format=%H%x00%s", "refs/remotes/origin/pre-prod"}) {
		return []byte("other-sha\x00loopcoder pre-prod merge PR #100\nmerge-sha\x00loopcoder pre-prod merge PR #101\n"), nil
	}
	if name == "git" && len(args) >= 2 && args[0] == "rev-parse" && args[1] == "HEAD" {
		return []byte("revert-sha\n"), nil
	}
	if name == "gh" && reflect.DeepEqual(args, []string{"repo", "view", "--json", "nameWithOwner"}) {
		return []byte(`{"nameWithOwner":"owner/repo"}`), nil
	}
	return nil, nil
}

type preProdKickBackCollisionRunner struct {
	calls [][]string
}

func (r *preProdKickBackCollisionRunner) Run(_ context.Context, dir, name string, args ...string) ([]byte, error) {
	call := append([]string{dir, name}, args...)
	r.calls = append(r.calls, call)
	if dir == "repo" && name == "git" && reflect.DeepEqual(args, []string{"log", "--format=%H%x00%s", "refs/remotes/origin/pre-prod"}) {
		return []byte(strings.Join([]string{
			"merge-352\x00loopcoder pre-prod merge PR #352",
			"merge-350\x00loopcoder pre-prod merge PR #350",
			"merge-35\x00loopcoder pre-prod merge PR #35",
		}, "\n") + "\n"), nil
	}
	if name == "git" && len(args) >= 2 && args[0] == "rev-parse" && args[1] == "HEAD" {
		return []byte("revert-sha\n"), nil
	}
	if name == "gh" && reflect.DeepEqual(args, []string{"repo", "view", "--json", "nameWithOwner"}) {
		return []byte(`{"nameWithOwner":"owner/repo"}`), nil
	}
	return nil, nil
}

type preProdKickBackAlreadyRevertedRunner struct {
	calls [][]string
}

func (r *preProdKickBackAlreadyRevertedRunner) Run(_ context.Context, dir, name string, args ...string) ([]byte, error) {
	call := append([]string{dir, name}, args...)
	r.calls = append(r.calls, call)
	if dir == "repo" && name == "git" && reflect.DeepEqual(args, []string{"log", "--format=%H%x00%s", "refs/remotes/origin/pre-prod"}) {
		return []byte("merge-sha\x00loopcoder pre-prod merge PR #101\n"), nil
	}
	if dir == "repo" && name == "git" && reflect.DeepEqual(args, []string{"log", "--format=%H%x00%B%x1e", "refs/remotes/origin/pre-prod"}) {
		return []byte("existing-revert-sha\x00Revert \"loopcoder pre-prod merge PR #101\"\n\nThis reverts commit merge-sha.\x1e"), nil
	}
	if name == "gh" && reflect.DeepEqual(args, []string{"repo", "view", "--json", "nameWithOwner"}) {
		return []byte(`{"nameWithOwner":"owner/repo"}`), nil
	}
	return nil, nil
}
