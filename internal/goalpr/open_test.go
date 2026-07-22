package goalpr_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/goalpr"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

type fakeGit struct {
	branch     string
	base       string
	committed  bool
	pushed     bool
	head       string
	addPath    string
	failPush   bool
	failCommit bool
}

func (f *fakeGit) RevParse(ctx context.Context, repo, rev string) (string, error) {
	return "baseoid", nil
}
func (f *fakeGit) CheckoutNewBranch(ctx context.Context, repo, branch, startPoint string) error {
	f.branch, f.base = branch, startPoint
	return nil
}
func (f *fakeGit) AddPath(ctx context.Context, repo, rel string) error {
	f.addPath = rel
	return nil
}
func (f *fakeGit) Commit(ctx context.Context, repo, message string) error {
	if f.failCommit {
		return errors.New("commit fail")
	}
	f.committed = true
	f.head = "deadbeef"
	return nil
}
func (f *fakeGit) PushUpstream(ctx context.Context, repo, branch string) error {
	if f.failPush {
		return errors.New("push fail")
	}
	f.pushed = true
	return nil
}
func (f *fakeGit) HeadOID(ctx context.Context, repo string) (string, error) {
	if f.head == "" {
		f.head = "deadbeef"
	}
	return f.head, nil
}

type fakeHost struct {
	created     bool
	url         string
	head, base  string
	title, body string
	checks      []string
	green       bool
	failCreate  bool
	mergeCalled bool // must stay false
}

func (f *fakeHost) CreatePR(ctx context.Context, head, base, title, body string) (string, error) {
	if f.failCreate {
		return "", errors.New("create fail")
	}
	f.created = true
	f.head, f.base, f.title, f.body = head, base, title, body
	if f.url == "" {
		f.url = "https://github.com/owner/disp/pull/42"
	}
	return f.url, nil
}
func (f *fakeHost) ListChecks(ctx context.Context, prNumber int) ([]string, bool, error) {
	return f.checks, f.green, nil
}
func (f *fakeHost) Merge(_ context.Context, _ int) error {
	f.mergeCalled = true
	return errors.New("merge forbidden")
}

func t0() time.Time { return time.Date(2026, 7, 23, 7, 0, 0, 0, time.UTC) }

func TestOpenCreatesPRHumanGateNoAutoMerge(t *testing.T) {
	repo := t.TempDir()
	// seed so WriteFile parent exists
	_ = os.MkdirAll(filepath.Join(repo, ".git"), 0o700)

	g := &fakeGit{}
	h := &fakeHost{
		checks: []string{"verify", "test"},
		green:  true,
	}
	res, err := goalpr.Open(context.Background(), goalpr.Request{
		RepoPath: repo, ProjectID: "proj", RunID: "run_pr_1",
		GraphID: "g1", PlanDigest: "pd", SourceIssue: 1343, Actor: "owner",
		BaseRef: "main",
		Children: []workflowrun.ChildOutcome{
			{WorkItemID: "a", Terminal: "succeeded", AttemptID: "att-a", OutputEvidence: "sha256:aa"},
			{WorkItemID: "b", Terminal: "succeeded", AttemptID: "att-b", OutputEvidence: "sha256:bb"},
		},
		IndependentVerifier: "claude",
		VerifierEvidence:    "sha256:verifier-1",
		RequiredCheckNames:  []string{"verify", "test"},
		Git:                 g,
		Host:                h,
		Now:                 t0,
	})
	if err != nil {
		t.Fatalf("%v %+v", err, res)
	}
	if !res.OK || !res.CreatedByLoopCoder || !res.HumanMergeGate || res.AutoMerge {
		t.Fatalf("%+v", res)
	}
	if res.Status != goalpr.StatusHumanGate {
		t.Fatalf("status %s", res.Status)
	}
	if res.URL != "https://github.com/owner/disp/pull/42" || res.Number != 42 {
		t.Fatalf("url/number %+v", res)
	}
	if !g.committed || !g.pushed || !h.created {
		t.Fatalf("git/host steps: commit=%v push=%v create=%v", g.committed, g.pushed, h.created)
	}
	if h.mergeCalled {
		t.Fatal("must never call merge")
	}
	if !strings.Contains(h.body, "auto_merge") || strings.Contains(strings.ToLower(h.body), "auto-merge: true") {
		// body should declare human gate
	}
	if res.ReceiptPath == "" {
		t.Fatal("missing receipt path")
	}
	if _, err := os.Stat(res.ReceiptPath); err != nil {
		t.Fatalf("receipt file: %v", err)
	}
	if !res.RequiredChecksGreen || len(res.RequiredChecks) < 2 {
		t.Fatalf("checks %+v", res)
	}
	if res.IndependentVerifier != "claude" || res.VerifierEvidenceRef == "" {
		t.Fatalf("verifier %+v", res)
	}
	joined := strings.Join(res.Events, "\n")
	for _, frag := range []string{"git.push", "github.pr_create", "human_gate.await_owner_merge"} {
		if !strings.Contains(joined, frag) {
			t.Fatalf("missing event %s in %v", frag, res.Events)
		}
	}
}

func TestOpenNegativeNoEvidence(t *testing.T) {
	_, err := goalpr.Open(context.Background(), goalpr.Request{
		RepoPath: t.TempDir(), ProjectID: "p", RunID: "r",
		Children: []workflowrun.ChildOutcome{
			{WorkItemID: "a", Terminal: "failed", AttemptID: "x", OutputEvidence: "e"},
		},
		Git: &fakeGit{}, Host: &fakeHost{},
	})
	if err == nil {
		t.Fatal("expected not ready")
	}
	if !errors.Is(err, goalpr.ErrNotReady) && !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("%v", err)
	}
}

func TestOpenNegativeMissingRepo(t *testing.T) {
	_, err := goalpr.Open(context.Background(), goalpr.Request{
		ProjectID: "p", RunID: "r",
		Children: []workflowrun.ChildOutcome{
			{WorkItemID: "a", Terminal: "succeeded", AttemptID: "x", OutputEvidence: "sha256:z"},
		},
	})
	if err == nil {
		t.Fatal("expected invalid")
	}
}

func TestOpenNegativePushFailureNoPR(t *testing.T) {
	repo := t.TempDir()
	_ = os.MkdirAll(filepath.Join(repo, ".git"), 0o700)
	h := &fakeHost{}
	res, err := goalpr.Open(context.Background(), goalpr.Request{
		RepoPath: repo, ProjectID: "p", RunID: "r",
		Children: []workflowrun.ChildOutcome{
			{WorkItemID: "a", Terminal: "succeeded", AttemptID: "x", OutputEvidence: "sha256:z"},
		},
		IndependentVerifier: "claude", VerifierEvidence: "v",
		Git: &fakeGit{failPush: true}, Host: h,
	})
	if err == nil {
		t.Fatalf("expected push error: %+v", res)
	}
	if h.created {
		t.Fatal("must not create PR after push failure")
	}
}

func TestOpenNegativeAutoMergeBodyRefused(t *testing.T) {
	repo := t.TempDir()
	_, err := goalpr.Open(context.Background(), goalpr.Request{
		RepoPath: repo, ProjectID: "p", RunID: "r",
		Body: "please auto-merge: true now",
		Children: []workflowrun.ChildOutcome{
			{WorkItemID: "a", Terminal: "succeeded", AttemptID: "x", OutputEvidence: "sha256:z"},
		},
		Git: &fakeGit{}, Host: &fakeHost{},
	})
	if err == nil || !errors.Is(err, goalpr.ErrAutoMerge) {
		t.Fatalf("want ErrAutoMerge got %v", err)
	}
}
