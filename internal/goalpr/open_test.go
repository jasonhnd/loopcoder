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
func (f *fakeGit) AddGoalPRReceipt(ctx context.Context, repo, rel string) error {
	f.addPath = rel
	return nil
}
func (f *fakeGit) Commit(ctx context.Context, repo, message string) error {
	if f.failCommit {
		return errors.New("commit fail")
	}
	f.committed = true
	// Preserve pre-set 40-hex head when tests supply a disposable PR head OID.
	if f.head == "" {
		f.head = strings.Repeat("d", 40)
	}
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
		f.head = strings.Repeat("d", 40)
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

func fullSHA256(seed string) string {
	// sha256: + 64 hex (deterministic pad; not a real hash)
	const hex = "0123456789abcdef"
	b := "sha256:"
	for i := 0; i < 64; i++ {
		if i < len(seed) {
			b += string(hex[int(seed[i])%16])
		} else {
			b += string(hex[i%16])
		}
	}
	return b
}

func TestOpenCreatesPRHumanGateNoAutoMerge(t *testing.T) {
	repo := t.TempDir()
	// seed so WriteFile parent exists + product files so not receipt-only
	_ = os.MkdirAll(filepath.Join(repo, ".git"), 0o700)
	_ = os.MkdirAll(filepath.Join(repo, "notes"), 0o700)
	_ = os.WriteFile(filepath.Join(repo, "notes/notes.go"), []byte("package notes\n"), 0o600)
	_ = os.WriteFile(filepath.Join(repo, "notes/notes_test.go"), []byte("package notes\n"), 0o600)
	_ = os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/n\n\ngo 1.22\n"), 0o600)

	headOID := strings.Repeat("d", 40)
	reviewedHead := strings.Repeat("c", 40)
	verEvid := fullSHA256("verify")
	g := &fakeGit{head: headOID}
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
			{WorkItemID: "wi_implement", TaskClass: "tera", Terminal: "succeeded", AttemptID: "att-i",
				OutputEvidence: fullSHA256("impl"), Provider: "antigravity", FilesTouched: []string{"notes/notes.go"}},
			{WorkItemID: "wi_tests", TaskClass: "tera", Terminal: "succeeded", AttemptID: "att-t",
				OutputEvidence: fullSHA256("tests"), Provider: "antigravity", FilesTouched: []string{"notes/notes_test.go"},
				IntegrateCommitSHA: reviewedHead},
			{WorkItemID: "wi_verify", TaskClass: "soul", Terminal: "succeeded", AttemptID: "att-v",
				OutputEvidence: verEvid, Provider: "codex", FilesTouched: []string{"review.md"},
				VerifierDecision:        workflowrun.VerifierDecisionPass,
				VerifierVerdictDigest:   fullSHA256("verdict"),
				VerifierReviewedHeadSHA: reviewedHead},
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
	wantRef := verEvid + "@head:" + headOID
	if res.VerifierEvidenceRef != wantRef {
		t.Fatalf("verifier ref got %q want %q", res.VerifierEvidenceRef, wantRef)
	}
	if res.HeadOID != headOID {
		t.Fatalf("head=%q", res.HeadOID)
	}
	joined := strings.Join(res.Events, "\n")
	for _, frag := range []string{"git.push", "github.pr_create", "ci.install", "verifier.bind"} {
		if !strings.Contains(joined, frag) {
			t.Fatalf("missing event %s in %v", frag, res.Events)
		}
	}
}

func TestOpenVerifierVerdictRefusalPrecedesAllMutatingSideEffects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*workflowrun.ChildOutcome)
	}{
		{"negative", func(v *workflowrun.ChildOutcome) { v.VerifierDecision = workflowrun.VerifierDecisionFail }},
		{"missing", func(v *workflowrun.ChildOutcome) { v.VerifierDecision = "" }},
		{"malformed_digest", func(v *workflowrun.ChildOutcome) { v.VerifierVerdictDigest = "sha256:short" }},
		{"reviewed_head_mismatch", func(v *workflowrun.ChildOutcome) { v.VerifierReviewedHeadSHA = strings.Repeat("e", 40) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			_ = os.MkdirAll(filepath.Join(repo, ".git"), 0o700)
			_ = os.WriteFile(filepath.Join(repo, "product.go"), []byte("package product\n"), 0o600)
			reviewedHead := strings.Repeat("c", 40)
			verify := workflowrun.ChildOutcome{
				WorkItemID: "wi_verify", TaskClass: "soul", Terminal: "succeeded",
				AttemptID: "att-v", Provider: "claude", OutputEvidence: fullSHA256("verify"),
				VerifierDecision:        workflowrun.VerifierDecisionPass,
				VerifierVerdictDigest:   fullSHA256("verdict"),
				VerifierReviewedHeadSHA: reviewedHead,
			}
			tc.mutate(&verify)
			git := &fakeGit{head: strings.Repeat("d", 40)}
			host := &fakeHost{}
			result, err := goalpr.Open(context.Background(), goalpr.Request{
				RepoPath: repo, ProjectID: "proj", RunID: "run-negative", GraphID: "g",
				BaseRef: "main",
				Children: []workflowrun.ChildOutcome{
					{WorkItemID: "wi_implement", TaskClass: "tera", Terminal: "succeeded",
						AttemptID: "att-i", Provider: "codex", OutputEvidence: fullSHA256("impl"),
						FilesTouched: []string{"product.go"}},
					{WorkItemID: "wi_tests", TaskClass: "tera", Terminal: "succeeded",
						AttemptID: "att-t", Provider: "codex", OutputEvidence: fullSHA256("tests"),
						IntegrateCommitSHA: reviewedHead, FilesTouched: []string{"product.go"}},
					verify,
				},
				Git: git, Host: host,
			})
			if err == nil || !errors.Is(err, goalpr.ErrNotReady) {
				t.Fatalf("expected not-ready: err=%v result=%+v", err, result)
			}
			if git.branch != "" || git.committed || git.pushed || git.addPath != "" ||
				host.created || len(result.Events) != 0 || result.ReceiptPath != "" ||
				len(result.CIFiles) != 0 {
				t.Fatalf("verdict refusal occurred after side effect: git=%+v host=%+v result=%+v", git, host, result)
			}
			for _, rel := range []string{".loopcoder", ".github"} {
				if _, statErr := os.Lstat(filepath.Join(repo, rel)); !os.IsNotExist(statErr) {
					t.Fatalf("%s created before verdict acceptance: %v", rel, statErr)
				}
			}
		})
	}
}

func TestOpenVerifier_PinOnlyWithoutExactWiVerifyFails(t *testing.T) {
	repo := t.TempDir()
	_ = os.MkdirAll(filepath.Join(repo, ".git"), 0o700)
	_ = os.WriteFile(filepath.Join(repo, "x.go"), []byte("package x\n"), 0o600)
	off := false
	h := &fakeHost{}
	_, err := goalpr.Open(context.Background(), goalpr.Request{
		RepoPath: repo, ProjectID: "p", RunID: "r",
		Children: []workflowrun.ChildOutcome{
			{WorkItemID: "wi_implement", TaskClass: "tera", Terminal: "succeeded", AttemptID: "att-i",
				OutputEvidence: fullSHA256("impl"), Provider: "antigravity", FilesTouched: []string{"x.go"}},
		},
		IndependentVerifier: "codex",
		VerifierEvidence:    fullSHA256("pin-only"),
		InstallMeaningfulCI: &off,
		Git:                 &fakeGit{head: strings.Repeat("e", 40)}, Host: h,
	})
	if err == nil {
		t.Fatal("expected refuse pin-only without wi_verify")
	}
	if !errors.Is(err, goalpr.ErrNotReady) && !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("%v", err)
	}
	if h.created {
		t.Fatal("must not CreatePR")
	}
}

func TestOpenVerifier_SubstringWorkItemFails(t *testing.T) {
	repo := t.TempDir()
	_ = os.MkdirAll(filepath.Join(repo, ".git"), 0o700)
	_ = os.WriteFile(filepath.Join(repo, "x.go"), []byte("package x\n"), 0o600)
	off := false
	h := &fakeHost{}
	_, err := goalpr.Open(context.Background(), goalpr.Request{
		RepoPath: repo, ProjectID: "p", RunID: "r",
		Children: []workflowrun.ChildOutcome{
			{WorkItemID: "wi_implement", TaskClass: "tera", Terminal: "succeeded", AttemptID: "att-i",
				OutputEvidence: fullSHA256("impl"), Provider: "antigravity", FilesTouched: []string{"x.go"}},
			{WorkItemID: "wi_verify_notes", TaskClass: "soul", Terminal: "succeeded", AttemptID: "att-v",
				OutputEvidence: fullSHA256("notes"), Provider: "codex", FilesTouched: []string{"x.go"}},
		},
		InstallMeaningfulCI: &off,
		Git:                 &fakeGit{head: strings.Repeat("e", 40)}, Host: h,
	})
	if err == nil {
		t.Fatal("expected refuse wi_verify_notes")
	}
	if h.created {
		t.Fatal("must not CreatePR")
	}
}

func TestOpenVerifier_ShortArbitrarySHA256Fails(t *testing.T) {
	repo := t.TempDir()
	_ = os.MkdirAll(filepath.Join(repo, ".git"), 0o700)
	_ = os.WriteFile(filepath.Join(repo, "x.go"), []byte("package x\n"), 0o600)
	off := false
	h := &fakeHost{}
	_, err := goalpr.Open(context.Background(), goalpr.Request{
		RepoPath: repo, ProjectID: "p", RunID: "r",
		Children: []workflowrun.ChildOutcome{
			{WorkItemID: "wi_implement", TaskClass: "tera", Terminal: "succeeded", AttemptID: "att-i",
				OutputEvidence: fullSHA256("impl"), Provider: "antigravity", FilesTouched: []string{"x.go"}},
			{WorkItemID: "wi_verify", TaskClass: "soul", Terminal: "succeeded", AttemptID: "att-v",
				OutputEvidence: "sha256:short", Provider: "codex", FilesTouched: []string{"x.go"}},
		},
		InstallMeaningfulCI: &off,
		Git:                 &fakeGit{head: strings.Repeat("e", 40)}, Host: h,
	})
	if err == nil {
		t.Fatal("expected refuse short evidence")
	}
	if h.created {
		t.Fatal("must not CreatePR")
	}
}

func TestOpenVerifier_SameImplementProviderFails(t *testing.T) {
	repo := t.TempDir()
	_ = os.MkdirAll(filepath.Join(repo, ".git"), 0o700)
	_ = os.WriteFile(filepath.Join(repo, "x.go"), []byte("package x\n"), 0o600)
	off := false
	h := &fakeHost{}
	_, err := goalpr.Open(context.Background(), goalpr.Request{
		RepoPath: repo, ProjectID: "p", RunID: "r",
		Children: []workflowrun.ChildOutcome{
			{WorkItemID: "wi_implement", TaskClass: "tera", Terminal: "succeeded", AttemptID: "att-i",
				OutputEvidence: fullSHA256("impl"), Provider: "codex", FilesTouched: []string{"x.go"}},
			{WorkItemID: "wi_verify", TaskClass: "soul", Terminal: "succeeded", AttemptID: "att-v",
				OutputEvidence: fullSHA256("verify"), Provider: "codex", FilesTouched: []string{"x.go"}},
		},
		InstallMeaningfulCI: &off,
		Git:                 &fakeGit{head: strings.Repeat("e", 40)}, Host: h,
	})
	if err == nil {
		t.Fatal("expected refuse same provider")
	}
	if h.created {
		t.Fatal("must not CreatePR")
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
