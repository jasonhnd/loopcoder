package supervisedexec

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/process"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

const guardianSchema = "loopcoder.macos_provider_guardian.v1"

// GuardianOptions configures the macOS supervisor-death guardian. It is inert
// off darwin/arm64.
type GuardianOptions struct {
	Enabled        bool
	StorePath      string
	DiagnosticPath string

	ProjectID       string
	RunID           string
	AttemptID       string
	OwnerID         string
	ClaimGeneration int64

	OnStart func(GuardianProcess) error
}

// GuardianProcess describes the helper process watching parent liveness.
type GuardianProcess struct {
	PID int
}

type guardianHandle interface {
	Release() error
}

type guardianNoop struct{}

func (guardianNoop) Release() error { return nil }

type guardianConfig struct {
	SchemaVersion   string `json:"schema_version"`
	StorePath       string `json:"store_path"`
	DiagnosticPath  string `json:"diagnostic_path"`
	ProjectID       string `json:"project_id"`
	RunID           string `json:"run_id"`
	AttemptID       string `json:"attempt_id"`
	OwnerID         string `json:"owner_id"`
	ClaimGeneration int64  `json:"claim_generation"`
}

type guardianEvent struct {
	SchemaVersion   string `json:"schema_version"`
	Event           string `json:"event"`
	At              string `json:"at"`
	GuardianPID     int    `json:"guardian_pid,omitempty"`
	ProjectID       string `json:"project_id,omitempty"`
	RunID           string `json:"run_id,omitempty"`
	AttemptID       string `json:"attempt_id,omitempty"`
	OwnerID         string `json:"owner_id,omitempty"`
	ClaimGeneration int64  `json:"claim_generation,omitempty"`
	ProviderPID     int    `json:"provider_pid,omitempty"`
	ProviderPGID    int    `json:"provider_pgid,omitempty"`
	Reason          string `json:"reason,omitempty"`
	Error           string `json:"error,omitempty"`
}

type guardianAuthorityLoader func(context.Context, guardianConfig) (storage.ProviderExecutionAuthority, error)
type guardianGroupKiller func(int) error

func normalizeGuardianOptions(opts GuardianOptions) GuardianOptions {
	opts.StorePath = strings.TrimSpace(opts.StorePath)
	opts.DiagnosticPath = strings.TrimSpace(opts.DiagnosticPath)
	opts.ProjectID = strings.TrimSpace(opts.ProjectID)
	opts.RunID = strings.TrimSpace(opts.RunID)
	opts.AttemptID = strings.TrimSpace(opts.AttemptID)
	opts.OwnerID = strings.TrimSpace(opts.OwnerID)
	return opts
}

func validateGuardianOptions(opts GuardianOptions) error {
	if !opts.Enabled {
		return nil
	}
	if opts.StorePath == "" || opts.DiagnosticPath == "" {
		return fmt.Errorf("supervisedexec: guardian store path and diagnostic path are required")
	}
	if opts.ProjectID == "" || opts.RunID == "" || opts.AttemptID == "" || opts.OwnerID == "" || opts.ClaimGeneration <= 0 {
		return fmt.Errorf("supervisedexec: complete guardian authority fence is required")
	}
	return nil
}

func guardianConfigFromOptions(opts GuardianOptions) guardianConfig {
	return guardianConfig{
		SchemaVersion:   guardianSchema,
		StorePath:       opts.StorePath,
		DiagnosticPath:  opts.DiagnosticPath,
		ProjectID:       opts.ProjectID,
		RunID:           opts.RunID,
		AttemptID:       opts.AttemptID,
		OwnerID:         opts.OwnerID,
		ClaimGeneration: opts.ClaimGeneration,
	}
}

func verifyGuardianAuthority(authority storage.ProviderExecutionAuthority, cfg guardianConfig) error {
	if authority.ProjectID != cfg.ProjectID || authority.RunID != cfg.RunID || authority.AttemptID != cfg.AttemptID ||
		authority.OwnerID != cfg.OwnerID || authority.ClaimGeneration != cfg.ClaimGeneration {
		return fmt.Errorf("authority fence mismatch")
	}
	if strings.TrimSpace(authority.CompletedAt) != "" {
		return fmt.Errorf("authority already completed")
	}
	if authority.IdentityAmbiguous {
		return fmt.Errorf("authority identity is ambiguous: %s", strings.TrimSpace(authority.AmbiguityReason))
	}
	identity := process.Identity{
		PID:                  authority.ProviderPID,
		PGID:                 authority.ProviderPGID,
		ProcessBirthIdentity: authority.ProcessBirthIdentity,
		ExecutableIdentity:   authority.ExecutableIdentity,
		Ambiguous:            authority.IdentityAmbiguous,
	}
	return process.VerifySnapshot(identity)
}

func guardianVerifyAndKill(ctx context.Context, cfg guardianConfig, load guardianAuthorityLoader, kill guardianGroupKiller) guardianEvent {
	event := guardianEvent{
		SchemaVersion:   guardianSchema,
		Event:           "skip",
		At:              time.Now().UTC().Format(time.RFC3339Nano),
		ProjectID:       cfg.ProjectID,
		RunID:           cfg.RunID,
		AttemptID:       cfg.AttemptID,
		OwnerID:         cfg.OwnerID,
		ClaimGeneration: cfg.ClaimGeneration,
	}
	if load == nil {
		event.Reason = "authority-loader-missing"
		return event
	}
	if kill == nil {
		event.Reason = "group-killer-missing"
		return event
	}
	authority, err := load(ctx, cfg)
	if err != nil {
		event.Reason = "authority-load-failed"
		event.Error = err.Error()
		return event
	}
	event.ProviderPID = authority.ProviderPID
	event.ProviderPGID = authority.ProviderPGID
	if err := verifyGuardianAuthority(authority, cfg); err != nil {
		event.Reason = "authority-verification-failed"
		event.Error = err.Error()
		return event
	}
	if err := kill(authority.ProviderPGID); err != nil {
		event.Event = "kill-failed"
		event.Error = err.Error()
		return event
	}
	event.Event = "killed"
	event.Reason = "supervisor-liveness-channel-closed"
	return event
}

func loadGuardianAuthority(ctx context.Context, cfg guardianConfig) (storage.ProviderExecutionAuthority, error) {
	store, err := storage.Open(ctx, storage.Options{Path: cfg.StorePath, BusyTimeout: 100 * time.Millisecond})
	if err != nil {
		return storage.ProviderExecutionAuthority{}, err
	}
	defer store.Close()
	return storage.LoadProviderExecutionAuthority(ctx, store, cfg.ProjectID, cfg.RunID, cfg.AttemptID)
}

func writeGuardianEvent(path string, event guardianEvent) {
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = file.Write(append(data, '\n'))
	_ = file.Close()
}
