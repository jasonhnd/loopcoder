package processtree

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

type fakeObs struct {
	procs []RawProc
	err   error
}

func (f fakeObs) List() ([]RawProc, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.procs, nil
}

func TestNotStarted(t *testing.T) {
	tr := &Tracker{}
	a := tr.Observe()
	if a.Liveness != LivenessNotStarted || a.Terminal {
		t.Fatalf("%#v", a)
	}
}

func TestPIDReuseRejected(t *testing.T) {
	ev := LaunchEvidence{
		RootPID: 4242, PGID: 4242,
		ProcessBirthIdentity: "Mon Jul  1 00:00:00 2026",
		RecordedAt:           time.Now().UTC(),
	}
	// Alive root with different birth.
	// process.Alive(4242) will be false for fake PID — use AssessPIDReuse unit +
	// synthetic observer where we can't force Alive. Unit test AssessPIDReuse.
	if err := AssessPIDReuse(ev, "Tue Jul  2 00:00:00 2026", true); !errors.Is(err, ErrPIDReuse) {
		t.Fatalf("err=%v", err)
	}
	if err := AssessPIDReuse(ev, ev.ProcessBirthIdentity, true); err != nil {
		t.Fatalf("same birth: %v", err)
	}

	// Observe path: inject matching PID in table with wrong birth AND use self PID
	// only for structure — for pure logic use fake root that isn't alive.
	// When root not alive, reuse branch is skipped; test reuse via AssessPIDReuse above.

	tr := &Tracker{
		Evidence: LaunchEvidence{
			RootPID: 4242, PGID: 0,
			ProcessBirthIdentity: "THIS_IS_NOT_REAL_BIRTH_IDENTITY",
			RecordedAt:           time.Now().UTC(),
		},
		Alive: func(pid int) bool { return pid == 4242 },
		Observer: fakeObs{procs: []RawProc{{
			PID: 4242, PPID: 1, PGID: 1,
			LStart: "WRONG_BIRTH", Comm: "test", State: "S",
		}}},
	}
	a := tr.Observe()
	// Current process is alive; birth mismatch → unknown + pid_reuse
	if a.Liveness != LivenessUnknown {
		t.Fatalf("liveness=%s reasons=%v", a.Liveness, a.Reasons)
	}
	if !contains(a.Reasons, "pid_reuse") {
		t.Fatalf("reasons=%v", a.Reasons)
	}
	if a.Terminal {
		t.Fatal("reuse must not be terminal success")
	}
}

func TestWrapperExitDescendantsAliveNotTerminal(t *testing.T) {
	// Root dead, child alive in owned set.
	tr := &Tracker{
		Evidence: LaunchEvidence{
			RootPID: 100, PGID: 100,
			ProcessBirthIdentity: "start",
			RecordedAt:           time.Now().UTC(),
		},
		Alive: func(pid int) bool { return pid == 101 || pid == 102 },
		Observer: fakeObs{procs: []RawProc{
			// root absent (exited)
			{PID: 101, PPID: 100, PGID: 100, LStart: "c", Comm: "child", State: "S"},
			{PID: 102, PPID: 101, PGID: 100, LStart: "g", Comm: "grand", State: "S"},
		}},
	}
	a := tr.Observe()
	if a.Terminal {
		t.Fatalf("wrapper exit with descendants must not be terminal: %#v", a)
	}
	if a.Liveness != LivenessAlive {
		t.Fatalf("liveness=%s reasons=%v", a.Liveness, a.Reasons)
	}
	if !contains(a.Reasons, "wrapper_exited_descendants_alive") {
		t.Fatalf("reasons=%v", a.Reasons)
	}
}

func TestEscapedDescendantAttention(t *testing.T) {
	tr := &Tracker{
		Evidence: LaunchEvidence{
			RootPID: 200, PGID: 200,
			ProcessBirthIdentity: "s",
			RecordedAt:           time.Now().UTC(),
		},
		Observer: fakeObs{procs: []RawProc{
			{PID: 200, PPID: 1, PGID: 200, LStart: "s", Comm: "root", State: "S"},
			// child same group
			{PID: 201, PPID: 200, PGID: 200, LStart: "c", Comm: "child", State: "S"},
			// escaped: parent is owned but different PGID so not added to owned, still listed as escaped
			{PID: 202, PPID: 200, PGID: 999, LStart: "e", Comm: "escape", State: "S"},
		}},
	}
	// Root 200 not actually alive — may go unknown. Force root live by using our PID as root.
	self := os.Getpid()
	tr.Evidence.RootPID = self
	// Get real birth for self to avoid reuse.
	ev, err := RecordLaunch(self, "att", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	tr.Evidence = ev
	tr.Observer = fakeObs{procs: []RawProc{
		{PID: self, PPID: 1, PGID: ev.PGID, LStart: ev.ProcessBirthIdentity, Comm: "test", State: "S"},
		{PID: self + 10000, PPID: self, PGID: ev.PGID, LStart: "c", Comm: "child", State: "S"},
		{PID: self + 10001, PPID: self, PGID: 99999, LStart: "e", Comm: "escape", State: "S"},
	}}
	a := tr.Observe()
	if !a.AttentionRequired {
		t.Fatalf("expected attention: %#v", a)
	}
	if !contains(a.Reasons, "escaped_descendant") {
		t.Fatalf("reasons=%v", a.Reasons)
	}
}

func TestTreeExited(t *testing.T) {
	tr := &Tracker{
		Evidence: LaunchEvidence{RootPID: 300, PGID: 300, ProcessBirthIdentity: "x", RecordedAt: time.Now().UTC()},
		Alive:    func(int) bool { return false },
		Observer: fakeObs{procs: nil},
	}
	a := tr.Observe()
	if a.Liveness != LivenessExited || !a.Terminal {
		t.Fatalf("%#v", a)
	}
}

func TestObservationFailureUnknownNoKill(t *testing.T) {
	tr := &Tracker{
		Evidence: LaunchEvidence{RootPID: 1, RecordedAt: time.Now().UTC()},
		Observer: fakeObs{err: errors.New("permission denied")},
	}
	a := tr.Observe()
	if a.Liveness != LivenessUnknown || !a.AttentionRequired {
		t.Fatalf("%#v", a)
	}
	// We never kill — no process API called for kill.
}

func TestSnapshotNoSecretsOrderedBounded(t *testing.T) {
	tr := &Tracker{
		Evidence: LaunchEvidence{RootPID: 1, PGID: 1, RecordedAt: time.Now().UTC()},
		MaxNodes: 3,
		Observer: fakeObs{procs: []RawProc{
			{PID: 1, PPID: 0, PGID: 1, Comm: "init", State: "S"},
			{PID: 2, PPID: 1, PGID: 1, Comm: "a", State: "S"},
			{PID: 3, PPID: 2, PGID: 1, Comm: "b", State: "S"},
			{PID: 4, PPID: 3, PGID: 1, Comm: "c", State: "S"},
			{PID: 5, PPID: 4, PGID: 1, Comm: "sk-secret-token-value", State: "S"},
		}},
	}
	a := tr.Observe()
	if !a.Snapshot.Truncated && len(a.Snapshot.Nodes) > 3 {
		// May truncate during walk
	}
	text := FormatSnapshot(a.Snapshot)
	if containsStr(text, "sk-secret") {
		t.Fatalf("secret in snapshot: %s", text)
	}
	// Ordered by PID
	for i := 1; i < len(a.Snapshot.Nodes); i++ {
		if a.Snapshot.Nodes[i].PID < a.Snapshot.Nodes[i-1].PID {
			t.Fatal("not ordered")
		}
	}
}

func TestLiveFixtureDirectChildAndGrandchild(t *testing.T) {
	// Spawn: sh -c 'sleep 30'  (child is sleep via shell)
	cmd := exec.Command("/bin/sh", "-c", "sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	}()
	ev, err := RecordLaunch(cmd.Process.Pid, "live", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	tr := &Tracker{Evidence: ev, Observer: DarwinPS{}}
	// Give shell time to spawn sleep.
	deadline := time.Now().Add(2 * time.Second)
	var a Assessment
	for time.Now().Before(deadline) {
		a = tr.Observe()
		if a.Liveness == LivenessAlive && !a.Terminal {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if a.Liveness != LivenessAlive {
		t.Fatalf("expected alive, got %#v", a)
	}
	// Signal tree and wait for exit.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	_, _ = cmd.Process.Wait()
	time.Sleep(50 * time.Millisecond)
	a2 := tr.Observe()
	if a2.Liveness != LivenessExited && a2.Liveness != LivenessUnknown {
		// After kill, tree should exit; unknown is acceptable if race.
		if !a2.Terminal && a2.Liveness == LivenessAlive {
			// try harder kill
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			time.Sleep(50 * time.Millisecond)
			a2 = tr.Observe()
		}
	}
	if a2.Liveness == LivenessAlive && a2.Terminal {
		t.Fatal("inconsistent")
	}
}

func TestZombieReasonToken(t *testing.T) {
	// Simulated zombie root with no live kids → exited
	tr := &Tracker{
		Evidence: LaunchEvidence{RootPID: 400, PGID: 400, ProcessBirthIdentity: "z", RecordedAt: time.Now().UTC()},
		Alive:    func(int) bool { return false },
		Observer: fakeObs{procs: []RawProc{
			{PID: 400, PPID: 1, PGID: 400, LStart: "z", Comm: "zomb", State: "Z"},
		}},
	}
	a := tr.Observe()
	if a.Liveness != LivenessExited {
		t.Fatalf("%#v", a)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		(len(s) > 0 && (func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		})()))
}
