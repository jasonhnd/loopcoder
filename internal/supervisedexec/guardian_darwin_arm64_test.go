//go:build darwin && arm64

package supervisedexec

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
		mutated := authority
		mutated.OwnerID = "worker:run-guardian:job-guardian:recovery"
		mutated.ClaimGeneration++
		loadCalls.Add(1)
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
	if got := loadCalls.Load(); got != 1 {
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
		case 1, 2:
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
	if got := loadCalls.Load(); got < 3 {
		t.Fatalf("authority loader calls = %d, want transient EOF retries before kill", got)
	}
	assertGuardianDiagnostic(t, cfg.DiagnosticPath, "killed")
}

func TestDarwinGuardianReadinessDeliveryFailureAfterAuthorityRetentionStillKillsOnSupervisorEOF(t *testing.T) {
	cmd, authority := startGuardianAuthorityProcess(t)
	defer cmd.Wait()
	defer terminateProcessGroup(cmd.Process.Pid)

	cfg := guardianConfigForAuthority(t, authority)
	readFile, writeFile, readyRead, readyWrite := guardianTestPipes(t)
	defer readFile.Close()
	defer readyWrite.Close()
	if err := readyRead.Close(); err != nil {
		t.Fatalf("close readiness reader: %v", err)
	}

	var loadCalls atomic.Int32
	loader := func(context.Context, guardianConfig) (storage.ProviderExecutionAuthority, error) {
		loadCalls.Add(1)
		return authority, nil
	}
	var killedPGID atomic.Int64
	done := make(chan int, 1)
	go func() {
		done <- runGuardianProcessWithFiles(cfg, readFile, readyWrite, loader, func(pgid int) error {
			killedPGID.Store(int64(pgid))
			return nil
		})
	}()

	if err := writeFile.Close(); err != nil {
		t.Fatalf("close supervisor liveness pipe: %v", err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("guardian exit code = %d, want 0", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("guardian did not finish after readiness failure and supervisor EOF")
	}
	if got := killedPGID.Load(); got != int64(authority.ProviderPGID) {
		t.Fatalf("killed PGID = %d, want %d", got, authority.ProviderPGID)
	}
	if got := loadCalls.Load(); got != 1 {
		t.Fatalf("authority loader calls = %d, want fresh EOF reload after readiness failure", got)
	}
	assertGuardianDiagnostic(t, cfg.DiagnosticPath, "killed")
	assertGuardianDiagnostic(t, cfg.DiagnosticPath, "readiness-failed")
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
		loadCalls.Add(1)
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
	if guardianReadySignalTimeout >= 2*time.Second {
		t.Fatalf("guardian ready signal timeout = %s, must stay below 2s", guardianReadySignalTimeout)
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

func TestDarwinGuardianStartupRejectsInvalidRetainedAuthorityWithoutLoading(t *testing.T) {
	cfg := guardianConfig{
		SchemaVersion:  guardianSchema,
		DiagnosticPath: filepath.Join(t.TempDir(), "guardian.jsonl"),
	}
	readFile, writeFile, readyRead, readyWrite := guardianTestPipes(t)
	defer readFile.Close()
	defer writeFile.Close()
	defer readyRead.Close()

	var killed atomic.Bool
	var loadCalls atomic.Int32
	done := make(chan int, 1)
	go func() {
		done <- runGuardianProcessWithFiles(cfg, readFile, readyWrite, func(ctx context.Context, _ guardianConfig) (storage.ProviderExecutionAuthority, error) {
			loadCalls.Add(1)
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
			t.Fatal("guardian signaled readiness without retained authority")
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
		t.Fatal("guardian killed provider before retained authority validation")
	}
	if got := loadCalls.Load(); got != 0 {
		t.Fatalf("authority loader calls = %d, want no startup load", got)
	}
	assertGuardianDiagnostic(t, cfg.DiagnosticPath, "startup-failed")
}

func TestDarwinGuardianConfigRoundTripsRetainedAuthority(t *testing.T) {
	cmd, authority := startGuardianAuthorityProcess(t)
	defer cmd.Wait()
	defer terminateProcessGroup(cmd.Process.Pid)

	cfg := guardianConfigForAuthority(t, authority)
	encoded, err := encodeGuardianConfig(cfg)
	if err != nil {
		t.Fatalf("encode guardian config: %v", err)
	}
	decoded, err := decodeGuardianConfig(encoded)
	if err != nil {
		t.Fatalf("decode guardian config: %v", err)
	}
	retained := decoded.RetainedAuthority.providerExecutionAuthority()
	if err := verifyGuardianAuthority(retained, decoded); err != nil {
		t.Fatalf("retained authority validation after round trip: %v", err)
	}
	if retained.ProviderPID != authority.ProviderPID || retained.ProviderPGID != authority.ProviderPGID ||
		retained.ProcessBirthIdentity != authority.ProcessBirthIdentity || retained.ExecutableIdentity != authority.ExecutableIdentity {
		t.Fatalf("retained authority changed after round trip: got %#v want %#v", retained, authority)
	}
}

func TestDarwinGuardianReadinessDoesNotDependOnStartupLoader(t *testing.T) {
	cmd, authority := startGuardianAuthorityProcess(t)
	defer cmd.Wait()
	defer terminateProcessGroup(cmd.Process.Pid)

	cfg := guardianConfigForAuthority(t, authority)
	readFile, writeFile, readyRead, readyWrite := guardianTestPipes(t)
	defer readFile.Close()
	defer writeFile.Close()
	defer readyRead.Close()

	var loadCalls atomic.Int32
	var killed atomic.Bool
	done := make(chan int, 1)
	go func() {
		done <- runGuardianProcessWithFiles(cfg, readFile, readyWrite, func(context.Context, guardianConfig) (storage.ProviderExecutionAuthority, error) {
			loadCalls.Add(1)
			select {}
		}, func(int) error {
			killed.Store(true)
			return nil
		})
	}()

	select {
	case err := <-readGuardianReadyToken(readyRead):
		if err != nil {
			t.Fatalf("guardian readiness: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("guardian readiness depended on startup authority loader")
	}
	if got := loadCalls.Load(); got != 0 {
		t.Fatalf("authority loader calls before release = %d, want none", got)
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
	if got := loadCalls.Load(); got != 0 {
		t.Fatalf("authority loader calls after clean release = %d, want none", got)
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
	var loadCalls atomic.Int32
	done := make(chan int, 1)
	go func() {
		done <- runGuardianProcessWithFiles(cfg, readFile, readyWrite, func(context.Context, guardianConfig) (storage.ProviderExecutionAuthority, error) {
			loadCalls.Add(1)
			return storage.ProviderExecutionAuthority{}, fmt.Errorf("unexpected load")
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
	if got := loadCalls.Load(); got != 0 {
		t.Fatalf("authority loader calls after clean release = %d, want none", got)
	}
	assertGuardianDiagnostic(t, cfg.DiagnosticPath, "released")
}

func guardianConfigForAuthority(t *testing.T, authority storage.ProviderExecutionAuthority) guardianConfig {
	t.Helper()
	return guardianConfig{
		SchemaVersion:     guardianSchema,
		DiagnosticPath:    filepath.Join(t.TempDir(), "guardian.jsonl"),
		ProjectID:         authority.ProjectID,
		RunID:             authority.RunID,
		AttemptID:         authority.AttemptID,
		OwnerID:           authority.OwnerID,
		ClaimGeneration:   authority.ClaimGeneration,
		RetainedAuthority: guardianAuthoritySnapshotFromStorage(authority),
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
