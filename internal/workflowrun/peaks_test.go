package workflowrun

import "testing"

// TestConcurrentPeaks_MaxConcurrentNotConstant exercises the production tracker
// (not a hand-rolled test double). Overlapping enters raise peak; leave returns
// active to 0 without changing peak.
func TestConcurrentPeaks_MaxConcurrentNotConstant(t *testing.T) {
	var p concurrentPeaks
	p.enterProcess()
	if p.ProcessPeak != 1 || p.activeProcess != 1 {
		t.Fatalf("after enter: peak=%d active=%d", p.ProcessPeak, p.activeProcess)
	}
	p.enterProcess()
	if p.ProcessPeak != 2 || p.activeProcess != 2 {
		t.Fatalf("overlap: peak=%d active=%d want 2/2", p.ProcessPeak, p.activeProcess)
	}
	p.leaveProcess()
	if p.ProcessPeak != 2 || p.activeProcess != 1 {
		t.Fatalf("after leave: peak=%d active=%d want 2/1", p.ProcessPeak, p.activeProcess)
	}
	p.leaveProcess()
	if p.activeProcess != 0 {
		t.Fatalf("active=%d want 0", p.activeProcess)
	}
	if p.ProcessPeak != 2 {
		t.Fatalf("peak should remain max=%d", p.ProcessPeak)
	}

	// Worktree same contract.
	p.enterWorktree()
	p.enterWorktree()
	if p.WorktreePeak != 2 {
		t.Fatalf("worktree peak=%d", p.WorktreePeak)
	}
	p.leaveWorktree()
	p.leaveWorktree()
	if p.activeWorktree != 0 {
		t.Fatalf("worktree active=%d", p.activeWorktree)
	}
}

func TestConcurrentPeaks_LeaveDoesNotUnderflow(t *testing.T) {
	var p concurrentPeaks
	p.leaveProcess()
	p.leaveWorktree()
	if p.activeProcess != 0 || p.activeWorktree != 0 {
		t.Fatalf("underflow active p=%d w=%d", p.activeProcess, p.activeWorktree)
	}
}
