package workflowrun

// concurrentPeaks tracks true simultaneous process/worktree occupancy peaks.
// Launch-count increments are not peaks.
type concurrentPeaks struct {
	activeProcess  int
	activeWorktree int
	ProcessPeak    int
	WorktreePeak   int
}

func (p *concurrentPeaks) enterProcess() {
	p.activeProcess++
	if p.activeProcess > p.ProcessPeak {
		p.ProcessPeak = p.activeProcess
	}
}

func (p *concurrentPeaks) leaveProcess() {
	if p.activeProcess > 0 {
		p.activeProcess--
	}
}

func (p *concurrentPeaks) enterWorktree() {
	p.activeWorktree++
	if p.activeWorktree > p.WorktreePeak {
		p.WorktreePeak = p.activeWorktree
	}
}

func (p *concurrentPeaks) leaveWorktree() {
	if p.activeWorktree > 0 {
		p.activeWorktree--
	}
}
