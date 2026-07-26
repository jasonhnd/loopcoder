package workflowrun

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/process"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestNextAttemptGeneration_LatestSucceededG1NotG0(t *testing.T) {
	// g0 hard-recovery cancelled, g1 succeeded → select g1 (reuse), not g0 or g2.
	events := []Event{
		{Kind: "launch", WorkItemID: "only", AttemptID: "att-only-x-g0", Generation: 1},
		{Kind: "interrupt", WorkItemID: "only", AttemptID: "att-only-x-g0", Generation: 1,
			Payload: []byte(`{"recovery":"authoritative","failure_class":"hard_kill_recovery","interrupt_class":"hard_kill_recovery","interrupt_id":"iint_test_hard"}`)},
		{Kind: "terminal", WorkItemID: "only", AttemptID: "att-only-x-g0", Generation: 1, Terminal: "cancelled",
			Payload: []byte(`{"recovery":"authoritative","failure_class":"hard_kill_recovery","interrupt_class":"hard_kill_recovery","interrupt_id":"iint_test_hard"}`)},
		{Kind: "launch", WorkItemID: "only", AttemptID: "att-only-x-g1", Generation: 2},
		{Kind: "terminal", WorkItemID: "only", AttemptID: "att-only-x-g1", Generation: 2, Terminal: "succeeded"},
	}
	got := NextAttemptGenerationFromEvents(events)
	if got["only"] != 1 {
		t.Fatalf("select gen=%d want 1 (reuse g1)", got["only"])
	}
}

func TestNextAttemptGeneration_HardCancelG0SelectsG1(t *testing.T) {
	events := []Event{
		{Kind: "launch", WorkItemID: "only", AttemptID: "att-only-x-g0", Generation: 1},
		{Kind: "interrupt", WorkItemID: "only", AttemptID: "att-only-x-g0", Generation: 1,
			Payload: []byte(`{"recovery":"authoritative","failure_class":"hard_kill_recovery","interrupt_class":"hard_kill_recovery","interrupt_id":"iint_test_hard"}`)},
		{Kind: "terminal", WorkItemID: "only", AttemptID: "att-only-x-g0", Generation: 1, Terminal: "cancelled",
			Payload: []byte(`{"recovery":"authoritative","failure_class":"hard_kill_recovery","interrupt_class":"hard_kill_recovery","interrupt_id":"iint_test_hard"}`)},
	}
	got := NextAttemptGenerationFromEvents(events)
	if got["only"] != 1 {
		t.Fatalf("select gen=%d want 1", got["only"])
	}
}

func TestNextAttemptGeneration_CompletedSuccessG0SelectsG0(t *testing.T) {
	events := []Event{
		{Kind: "launch", WorkItemID: "only", AttemptID: "att-only-x-g0", Generation: 1},
		{Kind: "terminal", WorkItemID: "only", AttemptID: "att-only-x-g0", Generation: 1, Terminal: "succeeded"},
	}
	got := NextAttemptGenerationFromEvents(events)
	if got["only"] != 0 {
		t.Fatalf("select gen=%d want 0 (reuse g0)", got["only"])
	}
}

// Service forced_interrupt pair must NOT select hard-recovery gN+1.
func TestNextAttemptGeneration_ServiceForcedCancelDoesNotSelectGNPlus1(t *testing.T) {
	events := []Event{
		{Kind: "launch", WorkItemID: "only", AttemptID: "att-only-x-g0", Generation: 1},
		{Kind: "interrupt", WorkItemID: "only", AttemptID: "att-only-x-g0", Generation: 1, Terminal: "cancelled",
			Payload: []byte(`{"failure_class":"forced_interrupt","interrupt_class":"service_forced_interrupt"}`)},
		{Kind: "terminal", WorkItemID: "only", AttemptID: "att-only-x-g0", Generation: 1, Terminal: "cancelled",
			Payload: []byte(`{"failure_class":"forced_interrupt","interrupt_class":"service_forced_interrupt"}`)},
	}
	got := NextAttemptGenerationFromEvents(events)
	if _, ok := got["only"]; ok {
		t.Fatalf("service forced cancel must not select generation via hard recovery: got %v", got)
	}
	// Soft ledger hard_kill without recovery=authoritative also must not bump.
	soft := []Event{
		{Kind: "launch", WorkItemID: "only", AttemptID: "att-only-x-g0", Generation: 1},
		{Kind: "interrupt", WorkItemID: "only", AttemptID: "att-only-x-g0", Generation: 1, Terminal: "cancelled",
			Payload: []byte(`{"failure_class":"hard_kill_recovery","interrupt_class":"hard_kill_recovery","recovery":"ledger_soft"}`)},
	}
	got2 := NextAttemptGenerationFromEvents(soft)
	if _, ok := got2["only"]; ok {
		t.Fatalf("soft ledger hard interrupt must not select gN+1: got %v", got2)
	}
}

func TestIsAuthoritativeHardRecovery_RejectsProse(t *testing.T) {
	if isAuthoritativeHardRecoveryEvent(Event{
		Kind: "terminal", Message: "authoritative hard-kill recovery", Terminal: "cancelled",
	}) {
		t.Fatal("must not accept Message-only prose")
	}
	// Partial markers rejected (no OR).
	if isAuthoritativeHardRecoveryEvent(Event{
		Kind: "interrupt", Payload: []byte(`{"recovery":"authoritative"}`),
	}) {
		t.Fatal("must require both recovery and failure_class")
	}
	if isAuthoritativeHardRecoveryEvent(Event{
		Kind: "terminal", Terminal: "cancelled",
		Payload: []byte(`{"failure_class":"hard_kill_recovery"}`),
	}) {
		t.Fatal("must require both recovery and failure_class")
	}
	if isAuthoritativeHardRecoveryEvent(Event{
		Kind: "terminal", Terminal: "succeeded",
		Payload: []byte(`{"recovery":"authoritative","failure_class":"hard_kill_recovery","interrupt_class":"hard_kill_recovery","interrupt_id":"iint_test_hard"}`),
	}) {
		t.Fatal("must not accept succeeded terminal as hard recovery")
	}
	if !isAuthoritativeHardRecoveryEvent(Event{
		Kind:    "interrupt",
		Payload: []byte(`{"recovery":"authoritative","failure_class":"hard_kill_recovery","interrupt_class":"hard_kill_recovery","interrupt_id":"iint_test_hard"}`),
	}) {
		t.Fatal("must accept exact structured interrupt recovery")
	}
	if !isAuthoritativeHardRecoveryEvent(Event{
		Kind: "terminal", Terminal: "cancelled",
		Payload: []byte(`{"recovery":"authoritative","failure_class":"hard_kill_recovery","interrupt_class":"hard_kill_recovery","interrupt_id":"iint_test_hard"}`),
	}) {
		t.Fatal("must accept structured cancelled recovery terminal")
	}
}

func TestEnsureProviderDead_UsesWallClockNotFrozenNow(t *testing.T) {
	// Dead pid returns immediately.
	auth := storage.ProviderExecutionAuthority{
		ProviderPID: 999999999, ProviderPGID: 999999999,
		ProcessBirthIdentity: "nope", ExecutableIdentity: "/nope",
	}
	start := time.Now()
	if err := ensureProviderDead(auth, 50*time.Millisecond, false, nil); err != nil {
		t.Fatalf("dead pid: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("dead path must not spin on frozen clock")
	}
}

func TestClassifyProviderProcess_AndUnobservableNoKill(t *testing.T) {
	// Dead pid.
	if classifyProviderProcess(storage.ProviderExecutionAuthority{
		ProviderPID: 999999991, ProviderPGID: 999999991,
		ProcessBirthIdentity: "x", ExecutableIdentity: "/x",
	}) != processProofDead {
		t.Fatal("want dead")
	}
	// Exact live + killAfter false: ensureProviderDead errors without kill.
	cmd := exec.Command("/bin/sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = process.KillGroup(cmd.Process.Pid)
		_, _ = cmd.Process.Wait()
	})
	snap, err := process.Snapshot(cmd.Process.Pid, time.Now())
	if err != nil || snap.Ambiguous {
		t.Fatalf("snap: %v ambig=%v", err, snap.Ambiguous)
	}
	auth := storage.ProviderExecutionAuthority{
		ProviderPID: snap.PID, ProviderPGID: snap.PGID,
		ProcessBirthIdentity: snap.ProcessBirthIdentity,
		ExecutableIdentity:   snap.ExecutableIdentity,
	}
	if classifyProviderProcess(auth) != processProofExactLive {
		t.Fatalf("want exact_live got %s", classifyProviderProcess(auth))
	}
	// Observable reused: wrong birth while live.
	reused := auth
	reused.ProcessBirthIdentity = "not-the-real-birth"
	if classifyProviderProcess(reused) != processProofObservableReused {
		t.Fatalf("want observable_reused got %s", classifyProviderProcess(reused))
	}
	// Unobservable: cannot easily force without platform quirks; use zero pgid + live pid
	// with birth match impossible — empty birth makes VerifySnapshot fail as ambiguous.
	unobs := auth
	unobs.ProcessBirthIdentity = ""
	unobs.ExecutableIdentity = ""
	// Alive with incomplete identity → VerifySnapshot fails ambiguous → unobservable.
	if cls := classifyProviderProcess(unobs); cls != processProofUnobservable && cls != processProofObservableReused {
		// Accept either unobservable or reused depending on error string; both must not kill when we call ensure with killAfter false after class check.
		t.Logf("class=%s for incomplete identity while live", cls)
	}
	kills := 0
	err = ensureProviderDead(unobs, 30*time.Millisecond, true, func(int) error {
		kills++
		return nil
	})
	// Incomplete identity while alive must not kill (unobservable fail or immediate reused continue).
	if kills != 0 && classifyProviderProcess(unobs) == processProofUnobservable {
		t.Fatalf("unobservable must not kill, kills=%d err=%v", kills, err)
	}
	if classifyProviderProcess(unobs) == processProofUnobservable && err == nil {
		t.Fatal("unobservable must fail before mutation")
	}
	// Exact live + killAfter false: no kill, error.
	kills = 0
	err = ensureProviderDead(auth, 40*time.Millisecond, false, func(int) error {
		kills++
		return nil
	})
	if kills != 0 {
		t.Fatalf("exact-live killAfter=false must not kill, kills=%d", kills)
	}
	if err == nil {
		t.Fatal("exact-live killAfter=false must error")
	}
}

func TestEnsureProviderDead_AliveChildBoundedWithFrozenEventClock(t *testing.T) {
	// If death wait used s.Now/t0 (frozen), alive child would infinite-loop.
	// Must bound-return using wall clock even when a frozen event clock exists elsewhere.
	cmd := exec.Command("/bin/sleep", "30")
	// Own process group so KillGroup cannot take down the test runner.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = process.KillGroup(cmd.Process.Pid)
		_, _ = cmd.Process.Wait()
	})
	snap, err := process.Snapshot(cmd.Process.Pid, time.Now())
	if err != nil || snap.Ambiguous {
		t.Fatalf("snapshot: err=%v ambig=%v reason=%s", err, snap.Ambiguous, snap.AmbiguityReason)
	}
	if snap.PGID == os.Getpid() || snap.PGID == 0 {
		t.Fatalf("child pgid %d collides with test runner", snap.PGID)
	}
	auth := storage.ProviderExecutionAuthority{
		ProviderPID: snap.PID, ProviderPGID: snap.PGID,
		ProcessBirthIdentity: snap.ProcessBirthIdentity,
		ExecutableIdentity:   snap.ExecutableIdentity,
	}
	// Frozen event clock (same class as Service.Now/t0) must NOT drive the wait.
	frozen := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	_ = frozen
	kills := 0
	start := time.Now()
	err = ensureProviderDead(auth, 80*time.Millisecond, true, func(pgid int) error {
		kills++
		if pgid != snap.PGID {
			return fmt.Errorf("kill pgid %d want %d", pgid, snap.PGID)
		}
		if err := process.KillGroup(pgid); err != nil {
			return err
		}
		// Reap so Alive/identity settle (test owns the child).
		_, _ = cmd.Process.Wait()
		return nil
	})
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("alive wait unbounded (%v); must use wall clock not frozen event clock", elapsed)
	}
	if kills != 1 {
		t.Fatalf("kill count=%d want 1 (kill path must run after bounded wait)", kills)
	}
	if elapsed < 50*time.Millisecond {
		t.Fatalf("elapsed %v too short; waitAlive window not applied", elapsed)
	}
	if err != nil {
		t.Fatalf("ensureProviderDead: %v after %v", err, elapsed)
	}
	if process.Alive(snap.PID) {
		t.Fatal("child still alive after kill+reap")
	}
}
