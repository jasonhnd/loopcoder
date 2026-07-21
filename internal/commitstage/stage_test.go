package commitstage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/commitstage"
)

func t0() time.Time { return time.Date(2026, 7, 23, 2, 0, 0, 0, time.UTC) }

func baseIntent() commitstage.Intent {
	return commitstage.Intent{
		AttemptID: "att1", IdempotencyKey: "idem-c1",
		OwnedPaths: []string{"internal/foo.go"}, ParentSHA: "parent01", BaseSHA: "parent01",
		TreeDigest: "td1", Message: "fix: thing\n\nCloses #1131",
		AuthorPolicy: "loopcoder_bot", VerificationDigest: "v1", RouteDigest: "r1",
		WorkerTerminal: true, VerificationOK: true,
	}
}

func TestCommitIdempotent(t *testing.T) {
	git := commitstage.NewFakeGit("parent01")
	git.SetDirty([]string{"internal/foo.go"})
	svc := &commitstage.Service{Store: commitstage.NewStore(t0), Git: git}
	in, err := svc.Freeze(baseIntent())
	if err != nil {
		t.Fatal(err)
	}
	r1, err := svc.CommitOrAdopt(context.Background(), in.IdempotencyKey)
	if err != nil || r1.CommitSHA == "" || r1.ParentSHA != "parent01" {
		t.Fatalf("%+v err=%v", r1, err)
	}
	r2, err := svc.CommitOrAdopt(context.Background(), in.IdempotencyKey)
	if err != nil || r2.CommitSHA != r1.CommitSHA {
		t.Fatalf("retry must adopt same commit: %+v", r2)
	}
}

func TestUnownedDriftBlocks(t *testing.T) {
	git := commitstage.NewFakeGit("parent01")
	git.SetDirty([]string{"internal/foo.go", "secret.env"})
	svc := &commitstage.Service{Store: commitstage.NewStore(t0), Git: git}
	_, _ = svc.Freeze(baseIntent())
	_, err := svc.CommitOrAdopt(context.Background(), "idem-c1")
	if !errors.Is(err, commitstage.ErrDrift) {
		t.Fatalf("err=%v", err)
	}
}

func TestNotReadyWithoutVerification(t *testing.T) {
	svc := &commitstage.Service{Store: commitstage.NewStore(t0), Git: commitstage.NewFakeGit("parent01")}
	in := baseIntent()
	in.VerificationOK = false
	if _, err := svc.Freeze(in); !errors.Is(err, commitstage.ErrNotReady) {
		t.Fatalf("err=%v", err)
	}
}

func TestHeadDrift(t *testing.T) {
	git := commitstage.NewFakeGit("otherhead")
	git.SetDirty([]string{"internal/foo.go"})
	svc := &commitstage.Service{Store: commitstage.NewStore(t0), Git: git}
	_, _ = svc.Freeze(baseIntent())
	_, err := svc.CommitOrAdopt(context.Background(), "idem-c1")
	if !errors.Is(err, commitstage.ErrDrift) {
		t.Fatalf("err=%v", err)
	}
}

func TestSanitizeSecretMessage(t *testing.T) {
	git := commitstage.NewFakeGit("parent01")
	git.SetDirty([]string{"internal/foo.go"})
	svc := &commitstage.Service{Store: commitstage.NewStore(t0), Git: git}
	in := baseIntent()
	in.Message = "add ghp_SECRETtoken"
	in.MessageDigest = ""
	frozen, err := svc.Freeze(in)
	if err != nil {
		t.Fatal(err)
	}
	if frozen.Message != "[redacted commit message]" {
		t.Fatalf("msg=%q", frozen.Message)
	}
}
