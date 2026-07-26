package wtclaim_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/wtclaim"
)

func t0() time.Time { return time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC) }

func intent() wtclaim.Intent {
	return wtclaim.Intent{
		ProjectID: "p", RunID: "run1", AttemptID: "a1",
		BranchName: "ordinary/issue-1128", BaseSHA: "abc1234deadbeef",
		OwnerID: "owner1", RuntimeRoot: "/tmp/runtime-p",
	}
}

func TestFirstClaimAndIdempotentRetry(t *testing.T) {
	git := wtclaim.NewFakeGit()
	s := &wtclaim.Service{Store: wtclaim.NewStore(t0), Git: git, Now: t0}
	r1, err := s.ClaimOrReuse(context.Background(), intent())
	if err != nil || !r1.OK || r1.Reused || r1.Claim == nil {
		t.Fatalf("%+v err=%v", r1, err)
	}
	path := r1.Claim.WorktreePath
	r2, err := s.ClaimOrReuse(context.Background(), intent())
	if err != nil || !r2.OK || !r2.Reused {
		t.Fatalf("%+v err=%v", r2, err)
	}
	if r2.Claim.ClaimID != r1.Claim.ClaimID || r2.Claim.WorktreePath != path {
		t.Fatal("must reuse same claim")
	}
}

func TestConflictsPreserveState(t *testing.T) {
	git := wtclaim.NewFakeGit()
	s := &wtclaim.Service{Store: wtclaim.NewStore(t0), Git: git, Now: t0}
	// pre-existing unrelated branch
	_ = git.CreateWorktree("ordinary/issue-1128", "/other/path", "abc1234deadbeef")
	_ = git.SetOwnerMeta("/other/path", "someone-else", 9)
	r, err := s.ClaimOrReuse(context.Background(), intent())
	if !errors.Is(err, wtclaim.ErrConflict) || r.Failure != wtclaim.FailUnrelated {
		t.Fatalf("%+v err=%v", r, err)
	}
}

func TestDirtyAndMoved(t *testing.T) {
	git := wtclaim.NewFakeGit()
	s := &wtclaim.Service{Store: wtclaim.NewStore(t0), Git: git, Now: t0}
	r1, _ := s.ClaimOrReuse(context.Background(), intent())
	git.SetDirty(r1.Claim.WorktreePath, true)
	r2, err := s.ClaimOrReuse(context.Background(), intent())
	if !errors.Is(err, wtclaim.ErrDirty) || r2.Failure != wtclaim.FailDirty {
		t.Fatalf("%+v err=%v", r2, err)
	}
	git.SetDirty(r1.Claim.WorktreePath, false)
	git.DeletePath(r1.Claim.WorktreePath)
	r3, err := s.ClaimOrReuse(context.Background(), intent())
	if r3.Failure != wtclaim.FailMoved {
		t.Fatalf("%+v err=%v", r3, err)
	}
}

func TestTimeoutAdoptsCompletedSideEffect(t *testing.T) {
	git := wtclaim.NewFakeGit()
	git.FailCreateWith = wtclaim.ErrTimeout
	s := &wtclaim.Service{Store: wtclaim.NewStore(t0), Git: git, Now: t0}
	r, err := s.ClaimOrReuse(context.Background(), intent())
	// Create returns timeout but leaves tree; service should adopt
	if err != nil || !r.OK {
		t.Fatalf("%+v err=%v", r, err)
	}
}

func TestCleanupOnlyOwnedClean(t *testing.T) {
	git := wtclaim.NewFakeGit()
	s := &wtclaim.Service{Store: wtclaim.NewStore(t0), Git: git, Now: t0}
	r, _ := s.ClaimOrReuse(context.Background(), intent())
	git.SetDirty(r.Claim.WorktreePath, true)
	if err := s.CleanupOwned(r.Claim.ClaimID); !errors.Is(err, wtclaim.ErrDirty) {
		t.Fatalf("err=%v", err)
	}
	git.SetDirty(r.Claim.WorktreePath, false)
	if err := s.CleanupOwned(r.Claim.ClaimID); err != nil {
		t.Fatal(err)
	}
}

func TestScrubEnv(t *testing.T) {
	in := []string{"PATH=/bin", "GIT_DIR=/evil", "HOME=/u", "GIT_WORK_TREE=/w", "FOO=1"}
	out := wtclaim.ScrubEnv(in)
	joined := strings.Join(out, ",")
	if strings.Contains(joined, "GIT_DIR") || strings.Contains(joined, "GIT_WORK_TREE") {
		t.Fatal(out)
	}
	if !strings.Contains(joined, "PATH=") || !strings.Contains(joined, "HOME=") {
		t.Fatal(out)
	}
}
