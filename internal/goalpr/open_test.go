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
	// seed so WriteFile parent exists + product files so not receipt-only
	_ = os.MkdirAll(filepath.Join(repo, ".git"), 0o700)
	_ = os.MkdirAll(filepath.Join(repo, "notes"), 0o700)
	_ = os.WriteFile(filepath.Join(repo, "notes/notes.go"), []byte("package notes\n"), 0o600)
	_ = os.WriteFile(filepath.Join(repo, "notes/notes_test.go"), []byte("package notes\n"), 0o600)
	_ = os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/n\n\ngo 1.22\n"), 0o600)

	g := &fakeGit{head: "deadbeefcafebabe"}
	h := &fakeHost{
		checks: []string{"product-tests", "product-build"},
		green:  true,
	}
	inst := true
	res, err := goalpr.Open(context.Background(), goalpr.Request{
		RepoPath: repo, ProjectID: "proj", RunID: "run_pr_1",
		GraphID: "g1", PlanDigest: "pd", SourceIssue: 1343, Actor: "owner",
		BaseRef: "main",
		Children: []workflowrun.ChildOutcome{
			{WorkItemID: "wi_implement", Terminal: "succeeded", AttemptID: "att-i", OutputEvidence: "sha256:aa",
				Provider: "antigravity", FilesTouched: []string{"notes/notes.go"}},
			{WorkItemID: "wi_tests", Terminal: "succeeded", AttemptID: "att-t", OutputEvidence: "sha256:bb",
				Provider: "antigravity", FilesTouched: []string{"notes/notes_test.go"}},
			{WorkItemID: "wi_verify", Terminal: "succeeded", AttemptID: "att-v", OutputEvidence: "sha256:verifdead",
				Provider: "codex", FilesTouched: []string{"review.md"}},
		},
		InstallMeaningfulCI: &inst,
		RequiredCheckNames:  goalpr.MeaningfulCheckNames(),
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
	if !g.pushed || !h.created {
		t.Fatalf("git/host steps: push=%v create=%v", g.pushed, h.created)
	}
	if h.mergeCalled {
		t.Fatal("must never call merge")
	}
	if res.ReceiptPath == "" {
		t.Fatal("missing receipt path")
	}
	if len(res.CIFiles) == 0 {
		t.Fatal("expected meaningful CI install")
	}
	if _, err := os.Stat(filepath.Join(repo, res.CIFiles[0])); err != nil {
		t.Fatalf("ci file: %v", err)
	}
	if !res.RequiredChecksGreen || len(res.RequiredChecks) < 2 {
		t.Fatalf("checks %+v", res)
	}
	// Verifier bound to codex verify child + head
	if res.VerifierProvider != "codex" || res.VerifierAttemptID != "att-v" {
		t.Fatalf("verifier bind %+v", res)
	}
	if !strings.Contains(res.VerifierEvidenceRef, "sha256:verifdead") || !strings.Contains(res.VerifierEvidenceRef, "@head:") {
		t.Fatalf("verifier ref %+v", res.VerifierEvidenceRef)
	}
	if strings.Contains(res.VerifierEvidenceRef, "pending") {
		t.Fatal("pending-live forbidden")
	}
	joined := strings.Join(res.Events, "\n")
	for _, frag := range []string{"git.push", "github.pr_create", "ci.install", "verifier.bind"} {
		if !strings.Contains(joined, frag) {
			t.Fatalf("missing event %s in %v", frag, res.Events)
		}
	}
}

func TestOpenRefusesPendingLiveVerifier(t *testing.T) {
	repo := t.TempDir()
	_ = os.MkdirAll(filepath.Join(repo, ".git"), 0o700)
	_ = os.WriteFile(filepath.Join(repo, "x.go"), []byte("package x\n"), 0o600)
	off := false
	_, err := goalpr.Open(context.Background(), goalpr.Request{
		RepoPath: repo, ProjectID: "p", RunID: "r",
		Children: []workflowrun.ChildOutcome{
			{WorkItemID: "a", Terminal: "succeeded", AttemptID: "x", OutputEvidence: "sha256:z", FilesTouched: []string{"x.go"}},
		},
		VerifierEvidence:    "sha256:pending-live",
		InstallMeaningfulCI: &off,
		Git:                 &fakeGit{}, Host: &fakeHost{},
	})
	if err == nil {
		t.Fatal("expected refuse pending-live")
	}
}

func TestFinalizeRequiresLiveChecks(t *testing.T) {
	h := &fakeHost{checks: []string{"product-tests", "product-build"}, green: true}
	base := goalpr.Result{
		URL: "https://github.com/o/r/pull/1", Number: 1, HeadOID: "abc",
		CreatedByLoopCoder: true, HumanMergeGate: true,
		IndependentVerifier: "codex", VerifierEvidenceRef: "sha256:v@head:abc",
	}
	fin, err := goalpr.FinalizePREvidence(context.Background(), base, goalpr.FinalizeRequest{
		PRNumber: 1, HeadOID: "abc", Host: h, RequiredMeaningfulOnly: true,
		VerifierEvidenceRef: "sha256:v", VerifierProvider: "codex", VerifierAttemptID: "att-v",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !fin.RequiredChecksGreen || !fin.OK {
		t.Fatalf("%+v", fin)
	}
}

func TestOpenNegativeNoEvidence(t *testing.T) {
	off := false
	_, err := goalpr.Open(context.Background(), goalpr.Request{
		RepoPath: t.TempDir(), ProjectID: "p", RunID: "r",
		Children: []workflowrun.ChildOutcome{
			{WorkItemID: "a", Terminal: "failed", AttemptID: "x", OutputEvidence: "e"},
		},
		InstallMeaningfulCI: &off,
		Git:                 &fakeGit{}, Host: &fakeHost{},
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
