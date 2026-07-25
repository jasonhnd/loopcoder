package workflowrun

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fullPartialResult(projectID, runID string) Result {
	plan := "sha256:" + strings.Repeat("ab", 32)
	graph := "sha256:" + strings.Repeat("cd", 32)
	return Result{
		Status:     StatusBlocked,
		PlanDigest: plan, GraphDigest: graph,
		GraphID: "g_partial_dur", GraphVersion: 1,
		RunID: runID, Interrupted: true,
		Children: []ChildOutcome{{
			WorkItemID: "wi_only", AttemptID: AttemptID("wi_only", plan, runID, 0),
			Generation: 1, TaskClass: "tera", ExecutionPlanDigest: plan,
			ChildContractDigest: "sha256:" + strings.Repeat("ef", 32),
			Terminal: "cancelled", FailureClass: "forced_interrupt",
			Permission: "bounded_write",
		}},
		AbortedAttempts: map[string]string{
			"wi_only": AttemptID("wi_only", plan, runID, 0),
		},
	}
}

func TestWritePartialPrior_IdentityFailClosed(t *testing.T) {
	home := t.TempDir()
	projectID, runID := "proj-id", "run_id"
	base := fullPartialResult(projectID, runID)

	cases := []struct {
		name string
		mut  func(*Result)
		want string
	}{
		{"empty_plan", func(r *Result) { r.PlanDigest = "" }, "plan_digest"},
		{"empty_graph_digest", func(r *Result) { r.GraphDigest = "" }, "graph_digest"},
		{"empty_graph_id", func(r *Result) { r.GraphID = "" }, "graph_id"},
		{"zero_graph_version", func(r *Result) { r.GraphVersion = 0 }, "graph_version"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Isolate each case under its own home so a prior successful write
			// cannot mask identity failure.
			h := t.TempDir()
			r := base
			tc.mut(&r)
			err := writePartialPrior(h, projectID, runID, r)
			if err == nil {
				t.Fatal("expected fail closed")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want substring %q", err, tc.want)
			}
			// Identity is refused before OpenEventLog/write — partial must not exist.
			path := filepath.Join(h, "projects", projectID, "runs", runID, "workflow-partial.json")
			_, serr := os.Stat(path)
			if serr == nil {
				t.Fatalf("partial must not exist after identity fail: path=%s writeErr=%v", path, err)
			}
			if !os.IsNotExist(serr) {
				t.Fatalf("want os.IsNotExist after identity fail, got stat err=%v writeErr=%v", serr, err)
			}
		})
	}
	// Empty project/run.
	if err := writePartialPrior(home, "", runID, base); err == nil || !strings.Contains(err.Error(), "project_id") {
		t.Fatalf("empty project: %v", err)
	}
	if err := writePartialPrior(home, projectID, "", base); err == nil || !strings.Contains(err.Error(), "run_id") {
		t.Fatalf("empty run: %v", err)
	}
}

func TestWritePartialPrior_SuccessPersistsFullKidsAndIdentity(t *testing.T) {
	home := t.TempDir()
	projectID, runID := "proj-ok", "run_ok"
	r := fullPartialResult(projectID, runID)
	if err := writePartialPrior(home, projectID, runID, r); err != nil {
		t.Fatal(err)
	}
	cp, err := LoadPartialPrior(home, projectID, runID)
	if err != nil {
		t.Fatal(err)
	}
	if err := requirePartialDocumentIdentity(cp); err != nil {
		t.Fatal(err)
	}
	if len(cp.WorkflowKids) != 1 {
		t.Fatalf("WorkflowKids=%d want 1", len(cp.WorkflowKids))
	}
	if cp.WorkflowKids[0].AttemptID != r.Children[0].AttemptID {
		t.Fatalf("kid attempt=%s", cp.WorkflowKids[0].AttemptID)
	}
	if cp.PlanDigest != cp.ExecutionPlanDigest || cp.PlanDigest != r.PlanDigest {
		t.Fatalf("plan digests: %+v", cp)
	}
	if cp.GraphID != r.GraphID || cp.GraphVersion != r.GraphVersion {
		t.Fatalf("graph identity: %+v", cp)
	}
}

func TestWritePartialPrior_InjectedDirOpenBlocks(t *testing.T) {
	home := t.TempDir()
	projectID, runID := "proj-diropen", "run_diropen"
	r := fullPartialResult(projectID, runID)
	orig := partialOpenDir
	t.Cleanup(func() { partialOpenDir = orig })
	partialOpenDir = func(name string) (*os.File, error) {
		return nil, fmt.Errorf("injected dir open fail")
	}
	err := writePartialPrior(home, projectID, runID, r)
	if err == nil || !strings.Contains(err.Error(), "open dir") {
		t.Fatalf("want open dir error, got %v", err)
	}
	if !strings.Contains(err.Error(), "injected dir open fail") {
		t.Fatalf("want injected cause: %v", err)
	}
}

func TestWritePartialPrior_InjectedDirSyncBlocks(t *testing.T) {
	home := t.TempDir()
	projectID, runID := "proj-dirsync", "run_dirsync"
	r := fullPartialResult(projectID, runID)
	origS, origC, origO := partialSyncFile, partialCloseFile, partialOpenDir
	t.Cleanup(func() {
		partialSyncFile, partialCloseFile, partialOpenDir = origS, origC, origO
	})
	// Let file path succeed; fail only on directory Sync.
	// openDir returns real dir; Sync fails when name is the run dir after rename.
	// Simpler: first Sync calls (tmp+final) succeed; dir Sync fails.
	var syncN int
	partialSyncFile = func(f *os.File) error {
		syncN++
		// tmp fsync, final fsync, then dir fsync — fail the 3rd.
		if syncN >= 3 {
			return fmt.Errorf("injected dir sync fail")
		}
		return f.Sync()
	}
	err := writePartialPrior(home, projectID, runID, r)
	if err == nil || !strings.Contains(err.Error(), "fsync dir") {
		t.Fatalf("want fsync dir error, got %v", err)
	}
	if !strings.Contains(err.Error(), "injected dir sync fail") {
		t.Fatalf("want injected cause: %v", err)
	}
}

func TestWritePartialPrior_InjectedDirCloseBlocks(t *testing.T) {
	home := t.TempDir()
	projectID, runID := "proj-dirclose", "run_dirclose"
	r := fullPartialResult(projectID, runID)
	origC, origO := partialCloseFile, partialOpenDir
	t.Cleanup(func() {
		partialCloseFile, partialOpenDir = origC, origO
	})
	var closeN int
	partialCloseFile = func(f *os.File) error {
		closeN++
		// tmp close, final close, dir close — fail 3rd.
		if closeN >= 3 {
			_ = f.Close() // still close underlying
			return fmt.Errorf("injected dir close fail")
		}
		return f.Close()
	}
	err := writePartialPrior(home, projectID, runID, r)
	if err == nil || !strings.Contains(err.Error(), "close dir") {
		t.Fatalf("want close dir error, got %v", err)
	}
	if !strings.Contains(err.Error(), "injected dir close fail") {
		t.Fatalf("want injected cause: %v", err)
	}
}

// TestFailBlockedJoin_PartialErrorRetainsPrimaryAndRunsCleanup: injected partial
// durability failure is joined with primary cause; cleanup still runs.
func TestFailBlockedJoin_PartialErrorRetainsPrimaryAndRunsCleanup(t *testing.T) {
	home := t.TempDir()
	projectID, runID := "proj-join", "run_join"
	r := fullPartialResult(projectID, runID)
	// Incomplete identity so writePartialPrior fails without needing FS inject.
	r.GraphID = ""
	cleanupRan := false
	cleanupErr := errors.New("cleanup boom")
	out, err := failBlockedJoin(r, home, projectID, runID, "primary cause boom", func() error {
		cleanupRan = true
		return cleanupErr
	})
	if err == nil {
		t.Fatal("expected joined error")
	}
	if !cleanupRan {
		t.Fatal("cleanup must run even when partial fails")
	}
	msg := err.Error()
	if !strings.Contains(msg, "primary cause boom") {
		t.Fatalf("must retain primary: %v", err)
	}
	if !strings.Contains(msg, "partial_checkpoint") {
		t.Fatalf("must join partial_checkpoint: %v", err)
	}
	if !strings.Contains(msg, "graph_id") {
		t.Fatalf("must surface partial identity cause: %v", err)
	}
	if !strings.Contains(msg, "cleanup") {
		t.Fatalf("must join cleanup: %v", err)
	}
	if out.Status != StatusBlocked {
		t.Fatalf("status=%s", out.Status)
	}
	_ = time.Now()
}
