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
	cmd.ExtraFiles = []*os.File{readFile}
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start guardian: %w", err)
	}
	closeRead = false
	_ = readFile.Close()
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

	var token [1]byte
	_, err = readFile.Read(token[:])
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
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		event := guardianVerifyAndKill(ctx, cfg, retryLoadGuardianAuthority, killDarwinProcessGroup)
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

func retryLoadGuardianAuthority(ctx context.Context, cfg guardianConfig) (authority storage.ProviderExecutionAuthority, err error) {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		authority, err = loadGuardianAuthority(ctx, cfg)
		if err == nil {
			return authority, nil
		}
		select {
		case <-ctx.Done():
			return storage.ProviderExecutionAuthority{}, err
		case <-ticker.C:
		}
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
