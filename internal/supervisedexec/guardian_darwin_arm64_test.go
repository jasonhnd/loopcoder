//go:build darwin && arm64

package supervisedexec

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/process"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestDarwinGuardianSupervisorEOFRejectsAuthorityMutationAfterReadiness(t *testing.T) {
	cmd, authority := startGuardianAuthorityProcess(t)
	defer cmd.Wait()
	defer terminateProcessGroup(cmd.Process.Pid)

	cfg := guardianConfigForAuthority(t, authority)
	readFile, writeFile, readyRead, readyWrite := guardianTestPipes(t)
	defer readFile.Close()
	defer writeFile.Close()
	defer readyRead.Close()

	var loadCalls atomic.Int32
	loader := func(context.Context, guardianConfig) (storage.ProviderExecutionAuthority, error) {
		if loadCalls.Add(1) == 1 {
			return authority, nil
		}
		mutated := authority
		mutated.OwnerID = "worker:run-guardian:job-guardian:recovery"
		mutated.ClaimGeneration++
		return mutated, nil
	}
	var killedPGID atomic.Int64
	done := make(chan int, 1)
	go func() {
		done <- runGuardianProcessWithFiles(cfg, readFile, readyWrite, loader, func(pgid int) error {
			killedPGID.Store(int64(pgid))
			return nil
		})
	}()

	waitGuardianReadyToken(t, readyRead)
	if err := writeFile.Close(); err != nil {
		t.Fatalf("close supervisor liveness pipe: %v", err)
	}
	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("guardian exit code = %d, want 1", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("guardian did not finish after supervisor EOF")
	}
	if got := killedPGID.Load(); got != 0 {
		t.Fatalf("killed PGID = %d, want no kill", got)
	}
	if got := loadCalls.Load(); got < 2 {
		t.Fatalf("authority loader calls = %d, want fresh EOF reload", got)
	}
	if !process.Alive(authority.ProviderPID) {
		t.Fatalf("provider pid %d was killed after authority mutation", authority.ProviderPID)
	}
	assertGuardianDiagnostic(t, cfg.DiagnosticPath, "skip")
}

func TestDarwinGuardianSupervisorEOFRetriesCurrentAuthorityBeforeKill(t *testing.T) {
	cmd, authority := startGuardianAuthorityProcess(t)
	defer cmd.Wait()
	defer terminateProcessGroup(cmd.Process.Pid)

	cfg := guardianConfigForAuthority(t, authority)
	readFile, writeFile, readyRead, readyWrite := guardianTestPipes(t)
	defer readFile.Close()
	defer writeFile.Close()
	defer readyRead.Close()

	var loadCalls atomic.Int32
	loader := func(context.Context, guardianConfig) (storage.ProviderExecutionAuthority, error) {
		switch loadCalls.Add(1) {
		case 1:
			return authority, nil
		case 2, 3:
			return storage.ProviderExecutionAuthority{}, fmt.Errorf("configure sqlite: %w", context.DeadlineExceeded)
		default:
			return authority, nil
		}
	}
	var killedPGID atomic.Int64
	done := make(chan int, 1)
	go func() {
		done <- runGuardianProcessWithFiles(cfg, readFile, readyWrite, loader, func(pgid int) error {
			killedPGID.Store(int64(pgid))
			return nil
		})
	}()

	waitGuardianReadyToken(t, readyRead)
	if err := writeFile.Close(); err != nil {
		t.Fatalf("close supervisor liveness pipe: %v", err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("guardian exit code = %d, want 0", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("guardian did not finish after supervisor EOF")
	}
	if got := killedPGID.Load(); got != int64(authority.ProviderPGID) {
		t.Fatalf("killed PGID = %d, want %d", got, authority.ProviderPGID)
	}
	if got := loadCalls.Load(); got < 4 {
		t.Fatalf("authority loader calls = %d, want transient EOF retries before kill", got)
	}
	assertGuardianDiagnostic(t, cfg.DiagnosticPath, "killed")
}

func TestDarwinGuardianSupervisorEOFFailsClosedWhenCurrentAuthorityUnreadable(t *testing.T) {
	cmd, authority := startGuardianAuthorityProcess(t)
	defer cmd.Wait()
	defer terminateProcessGroup(cmd.Process.Pid)

	cfg := guardianConfigForAuthority(t, authority)
	readFile, writeFile, readyRead, readyWrite := guardianTestPipes(t)
	defer readFile.Close()
	defer writeFile.Close()
	defer readyRead.Close()

	var loadCalls atomic.Int32
	loader := func(context.Context, guardianConfig) (storage.ProviderExecutionAuthority, error) {
		if loadCalls.Add(1) == 1 {
			return authority, nil
		}
		return storage.ProviderExecutionAuthority{}, fmt.Errorf("configure sqlite: %w", context.DeadlineExceeded)
	}
	var killedPGID atomic.Int64
	done := make(chan int, 1)
	go func() {
		done <- runGuardianProcessWithFiles(cfg, readFile, readyWrite, loader, func(pgid int) error {
			killedPGID.Store(int64(pgid))
			return nil
		})
	}()

	waitGuardianReadyToken(t, readyRead)
	if err := writeFile.Close(); err != nil {
		t.Fatalf("close supervisor liveness pipe: %v", err)
	}
	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("guardian exit code = %d, want 1", code)
		}
	case <-time.After(guardianAuthorityLoadTimeout + 2*time.Second):
		t.Fatal("guardian did not finish after supervisor EOF")
	}
	if got := killedPGID.Load(); got != 0 {
		t.Fatalf("killed PGID = %d, want no kill", got)
	}
	if got := loadCalls.Load(); got < 2 {
		t.Fatalf("authority loader calls = %d, want bounded EOF reload attempts", got)
	}
	if !process.Alive(authority.ProviderPID) {
		t.Fatalf("provider pid %d was killed when authority was unreadable", authority.ProviderPID)
	}
	assertGuardianDiagnostic(t, cfg.DiagnosticPath, "skip")
}

func TestDarwinGuardianStartupDeadlinesStayBelowTwoSeconds(t *testing.T) {
	if guardianStartupAuthorityLoadTimeout >= 2*time.Second {
		t.Fatalf("guardian startup authority timeout = %s, must stay below 2s", guardianStartupAuthorityLoadTimeout)
	}
	if guardianReadySignalTimeout >= 2*time.Second {
		t.Fatalf("guardian ready signal timeout = %s, must stay below 2s", guardianReadySignalTimeout)
	}
	if guardianReadySignalTimeout <= guardianStartupAuthorityLoadTimeout {
		t.Fatalf("guardian ready signal timeout = %s, must exceed startup authority timeout %s", guardianReadySignalTimeout, guardianStartupAuthorityLoadTimeout)
	}
	if guardianAuthorityLoadTimeout <= guardianReadySignalTimeout {
		t.Fatalf("guardian EOF authority timeout = %s, must exceed startup readiness timeout %s", guardianAuthorityLoadTimeout, guardianReadySignalTimeout)
	}
}

func TestDarwinGuardianLateReadinessFailsWithinStartupDeadline(t *testing.T) {
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("readiness pipe: %v", err)
	}
	defer readyRead.Close()
	defer readyWrite.Close()

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start placeholder guardian process: %v", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	writeDone := make(chan error, 1)
	go func() {
		time.Sleep(guardianReadySignalTimeout + 100*time.Millisecond)
		_, err := readyWrite.Write([]byte{'1'})
		writeDone <- err
	}()

	start := time.Now()
	err = waitGuardianReadySignal(cmd, readyRead)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("guardian readiness succeeded after startup deadline")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("guardian readiness error = %v, want timeout", err)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("guardian readiness blocked for %s, want under 2s", elapsed)
	}
	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("late readiness token write unexpectedly succeeded")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("readiness writer did not finish")
	}
}

func TestDarwinGuardianStartupAuthorityLoadFailsWithinTwoSeconds(t *testing.T) {
	cfg := guardianConfig{
		SchemaVersion:  guardianSchema,
		DiagnosticPath: filepath.Join(t.TempDir(), "guardian.jsonl"),
	}
	readFile, writeFile, readyRead, readyWrite := guardianTestPipes(t)
	defer readFile.Close()
	defer writeFile.Close()
	defer readyRead.Close()

	var killed atomic.Bool
	done := make(chan int, 1)
	go func() {
		done <- runGuardianProcessWithFiles(cfg, readFile, readyWrite, func(ctx context.Context, _ guardianConfig) (storage.ProviderExecutionAuthority, error) {
			<-ctx.Done()
			return storage.ProviderExecutionAuthority{}, ctx.Err()
		}, func(int) error {
			killed.Store(true)
			return nil
		})
	}()

	start := time.Now()
	select {
	case err := <-readGuardianReadyToken(readyRead):
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("guardian signaled readiness after startup authority load failure")
		}
		if elapsed >= 2*time.Second {
			t.Fatalf("guardian startup readiness failure took %s, want under 2s", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("guardian startup readiness failure timed out")
	}
	select {
	case code := <-done:
		if code != 2 {
			t.Fatalf("guardian exit code = %d, want 2", code)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("guardian did not finish after startup authority load failure")
	}
	if killed.Load() {
		t.Fatal("guardian killed provider before startup authority retention")
	}
	assertGuardianDiagnostic(t, cfg.DiagnosticPath, "startup-failed")
}

func TestDarwinGuardianReadinessWaitsForAuthorityRetention(t *testing.T) {
	cmd, authority := startGuardianAuthorityProcess(t)
	defer cmd.Wait()
	defer terminateProcessGroup(cmd.Process.Pid)

	cfg := guardianConfigForAuthority(t, authority)
	readFile, writeFile, readyRead, readyWrite := guardianTestPipes(t)
	defer readFile.Close()
	defer writeFile.Close()
	defer readyRead.Close()

	loadStarted := make(chan struct{})
	allowLoad := make(chan struct{})
	var closeAllowLoad sync.Once
	defer closeAllowLoad.Do(func() { close(allowLoad) })
	var loadCalls atomic.Int32
	var killed atomic.Bool
	done := make(chan int, 1)
	go func() {
		done <- runGuardianProcessWithFiles(cfg, readFile, readyWrite, func(context.Context, guardianConfig) (storage.ProviderExecutionAuthority, error) {
			if loadCalls.Add(1) == 1 {
				close(loadStarted)
			}
			<-allowLoad
			return authority, nil
		}, func(int) error {
			killed.Store(true)
			return nil
		})
	}()

	select {
	case <-loadStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("guardian did not start retaining authority")
	}
	readyDone := readGuardianReadyToken(readyRead)
	select {
	case err := <-readyDone:
		t.Fatalf("guardian signaled readiness before authority retention completed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	closeAllowLoad.Do(func() { close(allowLoad) })
	select {
	case err := <-readyDone:
		if err != nil {
			t.Fatalf("guardian readiness: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("guardian did not signal readiness after authority retention completed")
	}
	if _, err := writeFile.Write([]byte{'r'}); err != nil {
		t.Fatalf("write supervisor release: %v", err)
	}
	if err := writeFile.Close(); err != nil {
		t.Fatalf("close supervisor liveness pipe: %v", err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("guardian exit code = %d, want 0", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("guardian did not finish after clean release")
	}
	if killed.Load() {
		t.Fatal("guardian killed provider before supervisor EOF")
	}
}

func TestDarwinGuardianReadinessFailsWhenAuthorityCannotBeRetained(t *testing.T) {
	cfg := guardianConfig{
		SchemaVersion:  guardianSchema,
		DiagnosticPath: filepath.Join(t.TempDir(), "guardian.jsonl"),
	}
	readFile, writeFile, readyRead, readyWrite := guardianTestPipes(t)
	defer readFile.Close()
	defer writeFile.Close()
	defer readyRead.Close()

	var killed atomic.Bool
	done := make(chan int, 1)
	go func() {
		done <- runGuardianProcessWithFiles(cfg, readFile, readyWrite, nil, func(int) error {
			killed.Store(true)
			return nil
		})
	}()

	select {
	case err := <-readGuardianReadyToken(readyRead):
		if err == nil {
			t.Fatal("guardian signaled readiness without retained authority")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("guardian readiness failure timed out")
	}
	select {
	case code := <-done:
		if code != 2 {
			t.Fatalf("guardian exit code = %d, want 2", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("guardian did not finish after authority retention failure")
	}
	if killed.Load() {
		t.Fatal("guardian killed provider after authority retention failure")
	}
	assertGuardianDiagnostic(t, cfg.DiagnosticPath, "startup-failed")
}

func TestDarwinGuardianCleanReleaseDoesNotKillRetainedAuthority(t *testing.T) {
	cmd, authority := startGuardianAuthorityProcess(t)
	defer cmd.Wait()
	defer terminateProcessGroup(cmd.Process.Pid)

	cfg := guardianConfigForAuthority(t, authority)
	readFile, writeFile, readyRead, readyWrite := guardianTestPipes(t)
	defer readFile.Close()
	defer writeFile.Close()
	defer readyRead.Close()

	var killed atomic.Bool
	done := make(chan int, 1)
	go func() {
		done <- runGuardianProcessWithFiles(cfg, readFile, readyWrite, func(context.Context, guardianConfig) (storage.ProviderExecutionAuthority, error) {
			return authority, nil
		}, func(int) error {
			killed.Store(true)
			return nil
		})
	}()

	waitGuardianReadyToken(t, readyRead)
	if _, err := writeFile.Write([]byte{'r'}); err != nil {
		t.Fatalf("write supervisor release: %v", err)
	}
	if err := writeFile.Close(); err != nil {
		t.Fatalf("close supervisor liveness pipe: %v", err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("guardian exit code = %d, want 0", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("guardian did not finish after clean release")
	}
	if killed.Load() {
		t.Fatal("guardian killed provider after clean release")
	}
	if !process.Alive(authority.ProviderPID) {
		t.Fatalf("provider pid %d is not alive after clean release", authority.ProviderPID)
	}
	assertGuardianDiagnostic(t, cfg.DiagnosticPath, "released")
}

func guardianConfigForAuthority(t *testing.T, authority storage.ProviderExecutionAuthority) guardianConfig {
	t.Helper()
	return guardianConfig{
		SchemaVersion:   guardianSchema,
		DiagnosticPath:  filepath.Join(t.TempDir(), "guardian.jsonl"),
		ProjectID:       authority.ProjectID,
		RunID:           authority.RunID,
		AttemptID:       authority.AttemptID,
		OwnerID:         authority.OwnerID,
		ClaimGeneration: authority.ClaimGeneration,
	}
}

func guardianTestPipes(t *testing.T) (*os.File, *os.File, *os.File, *os.File) {
	t.Helper()
	readFile, writeFile, err := os.Pipe()
	if err != nil {
		t.Fatalf("liveness pipe: %v", err)
	}
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		_ = readFile.Close()
		_ = writeFile.Close()
		t.Fatalf("readiness pipe: %v", err)
	}
	return readFile, writeFile, readyRead, readyWrite
}

func waitGuardianReadyToken(t *testing.T, readyRead *os.File) {
	t.Helper()
	done := readGuardianReadyToken(readyRead)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("guardian readiness: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("guardian readiness timed out")
	}
}

func readGuardianReadyToken(readyRead *os.File) <-chan error {
	done := make(chan error, 1)
	go func() {
		var token [1]byte
		n, err := readyRead.Read(token[:])
		if err != nil {
			done <- err
			return
		}
		if n != 1 || token[0] != '1' {
			done <- fmt.Errorf("unexpected readiness token %q", token[:n])
			return
		}
		done <- nil
	}()
	return done
}
