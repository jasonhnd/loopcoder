//go:build darwin && arm64

package supervisedexec

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/process"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestDarwinGuardianRetainedAuthorityKillsAfterSupervisorEOFLoaderContention(t *testing.T) {
	cmd, authority := startGuardianAuthorityProcess(t)
	defer cmd.Wait()
	defer terminateProcessGroup(cmd.Process.Pid)

	cfg := guardianConfigForAuthority(t, authority)
	readFile, writeFile, readyRead, readyWrite := guardianTestPipes(t)
	defer readFile.Close()
	defer writeFile.Close()
	defer readyRead.Close()

	var loadCalls atomic.Int32
	authorityLoaded := make(chan struct{})
	loader := func(context.Context, guardianConfig) (storage.ProviderExecutionAuthority, error) {
		if loadCalls.Add(1) == 1 {
			close(authorityLoaded)
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
	select {
	case <-authorityLoaded:
	case <-time.After(2 * time.Second):
		t.Fatal("guardian did not retain authority before supervisor EOF")
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
		t.Fatal("guardian did not finish after supervisor EOF")
	}
	if got := killedPGID.Load(); got != int64(authority.ProviderPGID) {
		t.Fatalf("killed PGID = %d, want %d", got, authority.ProviderPGID)
	}
	if got := loadCalls.Load(); got != 1 {
		t.Fatalf("authority loader calls = %d, want cached kill without EOF reload", got)
	}
	assertGuardianDiagnostic(t, cfg.DiagnosticPath, "killed")
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
		done <- runGuardianProcessWithFiles(cfg, readFile, readyWrite, func(ctx context.Context, _ guardianConfig) (storage.ProviderExecutionAuthority, error) {
			<-ctx.Done()
			return storage.ProviderExecutionAuthority{}, ctx.Err()
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
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("guardian readiness: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("guardian readiness timed out")
	}
}
