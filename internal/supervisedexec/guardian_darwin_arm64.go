//go:build darwin && arm64

package supervisedexec

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/jasonhnd/loopcoder/internal/storage"
)

const (
	guardianEnvActive = "LOOPCODER_MACOS_GUARDIAN"
	guardianEnvConfig = "LOOPCODER_MACOS_GUARDIAN_CONFIG"
	guardianArg       = "__loopcoder_macos_guardian"
	guardianReadFD    = uintptr(3)
	guardianReadyFD   = uintptr(4)

	guardianStartupAuthorityLoadTimeout = 1 * time.Second
	guardianAuthorityLoadTimeout        = 8 * time.Second
	guardianReadySignalTimeout          = 1500 * time.Millisecond
)

type darwinGuardianHandle struct {
	write *os.File
	cmd   *exec.Cmd
}

func init() {
	if os.Getenv(guardianEnvActive) != "1" {
		return
	}
	if len(os.Args) < 2 || os.Args[1] != guardianArg {
		return
	}
	os.Exit(runGuardianProcess())
}

func startGuardian(opts GuardianOptions) (guardianHandle, error) {
	opts = normalizeGuardianOptions(opts)
	if !opts.Enabled {
		return guardianNoop{}, nil
	}
	if err := validateGuardianOptions(opts); err != nil {
		return nil, err
	}
	readFile, writeFile, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("start guardian liveness pipe: %w", err)
	}
	closeRead := true
	defer func() {
		if closeRead {
			_ = readFile.Close()
		}
	}()
	closeWrite := true
	defer func() {
		if closeWrite {
			_ = writeFile.Close()
		}
	}()
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("start guardian readiness pipe: %w", err)
	}
	closeReadyRead := true
	defer func() {
		if closeReadyRead {
			_ = readyRead.Close()
		}
	}()
	closeReadyWrite := true
	defer func() {
		if closeReadyWrite {
			_ = readyWrite.Close()
		}
	}()

	cfg := guardianConfigFromOptions(opts)
	encoded, err := encodeGuardianConfig(cfg)
	if err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve guardian executable: %w", err)
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open guardian devnull: %w", err)
	}
	defer devNull.Close()

	cmd := exec.Command(exe, guardianArg)
	cmd.Env = append(os.Environ(), guardianEnvActive+"=1", guardianEnvConfig+"="+encoded)
	cmd.ExtraFiles = []*os.File{readFile, readyWrite}
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start guardian: %w", err)
	}
	closeRead = false
	_ = readFile.Close()
	closeReadyWrite = false
	_ = readyWrite.Close()
	if err := waitGuardianReadySignal(cmd, readyRead); err != nil {
		_ = writeFile.Close()
		return nil, err
	}
	closeReadyRead = false
	_ = readyRead.Close()
	closeWrite = false
	handle := &darwinGuardianHandle{write: writeFile, cmd: cmd}
	writeGuardianEvent(cfg.DiagnosticPath, guardianEvent{
		SchemaVersion:   guardianSchema,
		Event:           "started",
		At:              time.Now().UTC().Format(time.RFC3339Nano),
		GuardianPID:     cmd.Process.Pid,
		ProjectID:       cfg.ProjectID,
		RunID:           cfg.RunID,
		AttemptID:       cfg.AttemptID,
		OwnerID:         cfg.OwnerID,
		ClaimGeneration: cfg.ClaimGeneration,
	})
	if opts.OnStart != nil {
		if err := opts.OnStart(GuardianProcess{PID: cmd.Process.Pid}); err != nil {
			_ = handle.Release()
			return nil, err
		}
	}
	return handle, nil
}

func (h *darwinGuardianHandle) Release() error {
	if h == nil || h.write == nil {
		return nil
	}
	_, writeErr := h.write.Write([]byte{'r'})
	closeErr := h.write.Close()
	h.write = nil
	if h.cmd != nil && h.cmd.Process != nil {
		done := make(chan error, 1)
		go func() { done <- h.cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func runGuardianProcess() int {
	cfg, err := decodeGuardianConfig(os.Getenv(guardianEnvConfig))
	if err != nil {
		return 2
	}
	readFile := os.NewFile(guardianReadFD, "loopcoder-guardian-liveness")
	if readFile == nil {
		writeGuardianEvent(cfg.DiagnosticPath, guardianEvent{
			SchemaVersion: guardianSchema,
			Event:         "startup-failed",
			At:            time.Now().UTC().Format(time.RFC3339Nano),
			Error:         "missing liveness fd " + strconv.Itoa(int(guardianReadFD)),
		})
		return 2
	}
	defer readFile.Close()
	readyFile := os.NewFile(guardianReadyFD, "loopcoder-guardian-ready")
	if readyFile == nil {
		writeGuardianEvent(cfg.DiagnosticPath, guardianEvent{
			SchemaVersion: guardianSchema,
			Event:         "startup-failed",
			At:            time.Now().UTC().Format(time.RFC3339Nano),
			Error:         "missing readiness fd " + strconv.Itoa(int(guardianReadyFD)),
		})
		return 2
	}
	return runGuardianProcessWithFiles(cfg, readFile, readyFile, loadGuardianAuthority, killDarwinProcessGroup)
}

func runGuardianProcessWithFiles(cfg guardianConfig, readFile, readyFile *os.File, load guardianAuthorityLoader, kill guardianGroupKiller) int {
	defer readyFile.Close()
	authorityCache := newGuardianAuthorityCache()
	loadCtx, cancelLoad := context.WithTimeout(context.Background(), guardianStartupAuthorityLoadTimeout)
	_, loadErr := retryGuardianAuthorityLoad(loadCtx, cfg, load, authorityCache)
	cancelLoad()
	if loadErr != nil {
		writeGuardianEvent(cfg.DiagnosticPath, guardianEvent{
			SchemaVersion: guardianSchema,
			Event:         "startup-failed",
			At:            time.Now().UTC().Format(time.RFC3339Nano),
			Error:         "retain authority before readiness: " + loadErr.Error(),
		})
		return 2
	}
	if _, err := readyFile.Write([]byte{'1'}); err != nil {
		writeGuardianEvent(cfg.DiagnosticPath, guardianEvent{
			SchemaVersion:   guardianSchema,
			Event:           "readiness-failed",
			At:              time.Now().UTC().Format(time.RFC3339Nano),
			ProjectID:       cfg.ProjectID,
			RunID:           cfg.RunID,
			AttemptID:       cfg.AttemptID,
			OwnerID:         cfg.OwnerID,
			ClaimGeneration: cfg.ClaimGeneration,
			Error:           "signal readiness: " + err.Error(),
		})
	}

	var token [1]byte
	_, err := readFile.Read(token[:])
	switch {
	case err == nil && token[0] == 'r':
		writeGuardianEvent(cfg.DiagnosticPath, guardianEvent{
			SchemaVersion:   guardianSchema,
			Event:           "released",
			At:              time.Now().UTC().Format(time.RFC3339Nano),
			ProjectID:       cfg.ProjectID,
			RunID:           cfg.RunID,
			AttemptID:       cfg.AttemptID,
			OwnerID:         cfg.OwnerID,
			ClaimGeneration: cfg.ClaimGeneration,
		})
		return 0
	case err == io.EOF:
		ctx, cancel := context.WithTimeout(context.Background(), guardianAuthorityLoadTimeout)
		defer cancel()
		event := guardianVerifyAndKill(ctx, cfg, func(ctx context.Context, cfg guardianConfig) (storage.ProviderExecutionAuthority, error) {
			return retryGuardianAuthorityLoad(ctx, cfg, load, nil)
		}, kill)
		writeGuardianEvent(cfg.DiagnosticPath, event)
		if event.Event == "killed" {
			return 0
		}
		return 1
	default:
		writeGuardianEvent(cfg.DiagnosticPath, guardianEvent{
			SchemaVersion:   guardianSchema,
			Event:           "channel-error",
			At:              time.Now().UTC().Format(time.RFC3339Nano),
			ProjectID:       cfg.ProjectID,
			RunID:           cfg.RunID,
			AttemptID:       cfg.AttemptID,
			OwnerID:         cfg.OwnerID,
			ClaimGeneration: cfg.ClaimGeneration,
			Error:           fmt.Sprint(err),
		})
		return 1
	}
}

func waitGuardianReadySignal(cmd *exec.Cmd, readyRead *os.File) error {
	done := make(chan error, 1)
	go func() {
		var token [1]byte
		n, err := readyRead.Read(token[:])
		if err != nil {
			done <- fmt.Errorf("start guardian readiness: %w", err)
			return
		}
		if n != 1 || token[0] != '1' {
			done <- fmt.Errorf("start guardian readiness: unexpected token %q", token[:n])
			return
		}
		done <- nil
	}()

	select {
	case err := <-done:
		if err != nil {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = cmd.Wait()
		}
		return err
	case <-time.After(guardianReadySignalTimeout):
		_ = readyRead.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		return fmt.Errorf("start guardian readiness: timed out")
	}
}

func killDarwinProcessGroup(pgid int) error {
	if pgid <= 0 {
		return fmt.Errorf("provider pgid must be positive")
	}
	return syscall.Kill(-pgid, syscall.SIGKILL)
}

func encodeGuardianConfig(cfg guardianConfig) (string, error) {
	data, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

func decodeGuardianConfig(encoded string) (guardianConfig, error) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return guardianConfig{}, err
	}
	var cfg guardianConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return guardianConfig{}, err
	}
	if cfg.SchemaVersion != guardianSchema {
		return guardianConfig{}, fmt.Errorf("unsupported guardian schema %q", cfg.SchemaVersion)
	}
	return cfg, nil
}
