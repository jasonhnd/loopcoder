package github

import (
	"context"
	"errors"
	"reflect"
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
