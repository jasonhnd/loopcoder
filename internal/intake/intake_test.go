package intake_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/intake"
)

func t0() time.Time { return time.Date(2026, 7, 22, 22, 0, 0, 0, time.UTC) }

func seedOpen(gh *intake.FakeGitHub, n int, body string) {
	gh.Put(intake.IssueSource{
		NodeID: "I_node_1", Number: n, State: "open", Title: "Fix thing",
		Labels: []string{"bug"}, Assignees: []string{"alice"},
		UpdatedAt: t0(), URL: "https://example.test/i/1",
		RepoOwner: "acme", RepoName: "app", AuthOK: true,
	}, body)
}

func svc(gh *intake.FakeGitHub) *intake.Service {
	return &intake.Service{
		Store:  intake.NewStore(t0),
		GitHub: gh,
		Policy: intake.StaticPolicy{Base: "pre-prod"},
		Now:    t0,
	}
}

func TestIntakeImmutableAndIdempotent(t *testing.T) {
	gh := intake.NewFakeGitHub()
	seedOpen(gh, 42, "private body secret path")
	s := svc(gh)
	opts := intake.IntakeOptions{
		ProjectID: "proj", Ref: intake.IssueRef{RepoOwner: "acme", RepoName: "app", Number: 42},
		BaseBranch: "pre-prod", ExpectedRepoOwner: "acme", ExpectedRepoName: "app",
	}
	r1, err := s.Intake(context.Background(), opts)
	if err != nil || !r1.OK || r1.Request == nil {
		t.Fatalf("%+v err=%v", r1, err)
	}
	id := r1.Request.RequestID
	if r1.Request.Issue.BodyDigest == "" || r1.Request.Policy.Digest == "" {
		t.Fatal("missing digests")
	}
	// private body project-scoped on store get, not public view
	got, _ := s.Store.Get(id)
	if got.PrivateBody == "" {
		t.Fatal("expected private body in store")
	}
	pub := intake.PublicView(got)
	if pub.PrivateBody != "" {
		t.Fatal("public view must strip body")
	}
	// retry identical
	r2, err := s.Intake(context.Background(), opts)
	if err != nil || !r2.Duplicate || r2.Request.RequestID != id {
		t.Fatalf("%+v err=%v", r2, err)
	}
}

func TestEditDoesNotOverwrite(t *testing.T) {
	gh := intake.NewFakeGitHub()
	seedOpen(gh, 7, "v1")
	s := svc(gh)
	opts := intake.IntakeOptions{
		ProjectID: "p", Ref: intake.IssueRef{RepoOwner: "acme", RepoName: "app", Number: 7},
		ExpectedRepoOwner: "acme", ExpectedRepoName: "app",
	}
	r1, _ := s.Intake(context.Background(), opts)
	// edit body
	gh.Put(intake.IssueSource{
		NodeID: "I_node_1", Number: 7, State: "open", Title: "Fix thing",
		UpdatedAt: t0().Add(time.Minute), URL: "https://example.test/i/7",
		RepoOwner: "acme", RepoName: "app", AuthOK: true,
	}, "v2 edited")
	r2, err := s.Intake(context.Background(), opts)
	if !errors.Is(err, intake.ErrConflict) || r2.Drift == nil {
		t.Fatalf("%+v err=%v", r2, err)
	}
	if r2.Drift.RequiresAction != "restart" {
		t.Fatal(r2.Drift)
	}
	// active still v1 digest
	got, _ := s.Store.Get(r1.Request.RequestID)
	if got.SourceRevision != r1.Request.SourceRevision {
		t.Fatal("active overwritten")
	}
	// explicit restart
	r3, err := s.ExplicitRestart(context.Background(), opts)
	if err != nil || !r3.OK {
		t.Fatalf("%+v err=%v", r3, err)
	}
	if r3.Request.SourceRevision == r1.Request.SourceRevision {
		t.Fatal("restart should new revision")
	}
}

func TestWrongRepoClosedAuthRateLimit(t *testing.T) {
	gh := intake.NewFakeGitHub()
	seedOpen(gh, 1, "b")
	s := svc(gh)
	// wrong repo expectation
	r, err := s.Intake(context.Background(), intake.IntakeOptions{
		ProjectID: "p", Ref: intake.IssueRef{RepoOwner: "acme", RepoName: "app", Number: 1},
		ExpectedRepoOwner: "other", ExpectedRepoName: "app",
	})
	if r.Failure != intake.FailWrongRepo {
		t.Fatalf("%+v err=%v", r, err)
	}
	// closed
	gh.Put(intake.IssueSource{
		NodeID: "I_c", Number: 2, State: "closed", Title: "x",
		RepoOwner: "acme", RepoName: "app", AuthOK: true, UpdatedAt: t0(),
	}, "b")
	r, _ = s.Intake(context.Background(), intake.IntakeOptions{
		ProjectID: "p", Ref: intake.IssueRef{RepoOwner: "acme", RepoName: "app", Number: 2},
		ExpectedRepoOwner: "acme", ExpectedRepoName: "app",
	})
	if r.Failure != intake.FailClosedIssue {
		t.Fatalf("%+v", r)
	}
	// unauthorized
	gh2 := intake.NewFakeGitHub()
	gh2.Auth = false
	gh2.Put(intake.IssueSource{
		NodeID: "I_u", Number: 3, State: "open", Title: "x",
		RepoOwner: "acme", RepoName: "app", UpdatedAt: t0(),
	}, "b")
	s2 := svc(gh2)
	r, err = s2.Intake(context.Background(), intake.IntakeOptions{
		ProjectID: "p", Ref: intake.IssueRef{RepoOwner: "acme", RepoName: "app", Number: 3},
		ExpectedRepoOwner: "acme", ExpectedRepoName: "app",
	})
	if r.Failure != intake.FailUnauthorized && !errors.Is(err, intake.ErrUnauthorized) {
		t.Fatalf("%+v err=%v", r, err)
	}
	// rate limit
	gh3 := intake.NewFakeGitHub()
	gh3.Limited = true
	s3 := svc(gh3)
	r, err = s3.Intake(context.Background(), intake.IntakeOptions{
		ProjectID: "p", Ref: intake.IssueRef{RepoOwner: "acme", RepoName: "app", Number: 1},
		ExpectedRepoOwner: "acme", ExpectedRepoName: "app",
	})
	if r.Failure != intake.FailRateLimit {
		t.Fatalf("%+v err=%v", r, err)
	}
}

func TestTransferAndTimeout(t *testing.T) {
	gh := intake.NewFakeGitHub()
	gh.Put(intake.IssueSource{
		NodeID: "I_t", Number: 9, State: "open", Title: "x",
		RepoOwner: "acme", RepoName: "app", AuthOK: true, UpdatedAt: t0(),
		Extra: map[string]string{"transferred": "1"},
	}, "b")
	s := svc(gh)
	r, _ := s.Intake(context.Background(), intake.IntakeOptions{
		ProjectID: "p", Ref: intake.IssueRef{RepoOwner: "acme", RepoName: "app", Number: 9},
		ExpectedRepoOwner: "acme", ExpectedRepoName: "app",
	})
	if r.Failure != intake.FailTransferred {
		t.Fatalf("%+v", r)
	}

	gh2 := intake.NewFakeGitHub()
	seedOpen(gh2, 1, "b")
	gh2.Delay = 50 * time.Millisecond
	s2 := svc(gh2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	r, err := s2.Intake(ctx, intake.IntakeOptions{
		ProjectID: "p", Ref: intake.IssueRef{RepoOwner: "acme", RepoName: "app", Number: 1},
		ExpectedRepoOwner: "acme", ExpectedRepoName: "app",
	})
	if r.Failure != intake.FailTimeout && !errors.Is(err, intake.ErrTimeout) {
		t.Fatalf("%+v err=%v", r, err)
	}
}

func TestParseIssueRef(t *testing.T) {
	ref, err := intake.ParseIssueRef("acme/app#12", "", "")
	if err != nil || ref.Number != 12 || ref.RepoOwner != "acme" {
		t.Fatalf("%+v err=%v", ref, err)
	}
	ref, err = intake.ParseIssueRef("12", "acme", "app")
	if err != nil || ref.Number != 12 {
		t.Fatal(err)
	}
}

func TestRedactsSecretsInTitle(t *testing.T) {
	gh := intake.NewFakeGitHub()
	gh.Put(intake.IssueSource{
		NodeID: "I_s", Number: 5, State: "open", Title: "leak ghp_ABCDEFSECRET",
		RepoOwner: "acme", RepoName: "app", AuthOK: true, UpdatedAt: t0(),
	}, "body")
	s := svc(gh)
	r, err := s.Intake(context.Background(), intake.IntakeOptions{
		ProjectID: "p", Ref: intake.IssueRef{RepoOwner: "acme", RepoName: "app", Number: 5},
		ExpectedRepoOwner: "acme", ExpectedRepoName: "app",
	})
	if err != nil || !r.OK {
		t.Fatal(err)
	}
	if r.Request.Issue.Title != "[redacted]" {
		t.Fatalf("title=%q", r.Request.Issue.Title)
	}
}
