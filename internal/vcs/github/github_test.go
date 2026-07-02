package github

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
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
			"repo\x00gh\x00label\x00create\x00epic\x00--color\x005319e7\x00--description\x00loopcoder epic issue":                                              nil,
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
