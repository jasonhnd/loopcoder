package authoritystore

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/store"
)

// OpenOptions controls authority-store opening. Role is required.
type OpenOptions struct {
	Role Role
	Path string
	Now  func() time.Time

	BusyTimeout time.Duration
}

// MachineStore is a machine-scoped compact store handle.
type MachineStore struct {
	inner *store.Store
}

// ProjectStore is a project-scoped compact store handle.
type ProjectStore struct {
	inner *store.Store
}

// Open opens a compact store for the requested authority role.
func Open(ctx context.Context, opts OpenOptions) (role Role, machine *MachineStore, project *ProjectStore, err error) {
	role = opts.Role
	format, err := formatIdentityForRole(role)
	if err != nil {
		return "", nil, nil, err
	}
	path := strings.TrimSpace(opts.Path)
	if path == "" {
		return "", nil, nil, fmt.Errorf("open authority store: path is required")
	}
	path = filepath.Clean(path)
	if err := globalOpen.claim(path, role); err != nil {
		return "", nil, nil, err
	}
	inner, err := store.Open(ctx, store.Options{
		Path:           path,
		Now:            opts.Now,
		BusyTimeout:    opts.BusyTimeout,
		FormatIdentity: format,
		AuthorityRole:  string(role),
	})
	if err != nil {
		globalOpen.release(path)
		return "", nil, nil, err
	}
	// Defend against identity mismatch if an existing file was created under
	// another role (format validation should already fail; double-check).
	if got := inner.ExpectedFormatIdentity(); got != format {
		_ = inner.Close()
		globalOpen.release(path)
		return "", nil, nil, fmt.Errorf("%w: format identity %q does not match role %s", ErrRoleMismatch, got, role)
	}
	switch role {
	case RoleMachine:
		return role, &MachineStore{inner: inner}, nil, nil
	case RoleProject:
		return role, nil, &ProjectStore{inner: inner}, nil
	default:
		_ = inner.Close()
		globalOpen.release(path)
		return "", nil, nil, fmt.Errorf("%w: %q", ErrInvalidRole, role)
	}
}

// OpenMachine opens a machine authority store.
func OpenMachine(ctx context.Context, opts OpenOptions) (*MachineStore, error) {
	opts.Role = RoleMachine
	_, m, _, err := Open(ctx, opts)
	return m, err
}

// OpenProject opens a project authority store.
func OpenProject(ctx context.Context, opts OpenOptions) (*ProjectStore, error) {
	opts.Role = RoleProject
	_, _, p, err := Open(ctx, opts)
	return p, err
}

// Close releases the machine store handle.
func (s *MachineStore) Close() error {
	if s == nil || s.inner == nil {
		return nil
	}
	path := s.inner.Path()
	err := s.inner.Close()
	globalOpen.release(path)
	return err
}

// Close releases the project store handle.
func (s *ProjectStore) Close() error {
	if s == nil || s.inner == nil {
		return nil
	}
	path := s.inner.Path()
	err := s.inner.Close()
	globalOpen.release(path)
	return err
}

// Path returns the database path.
func (s *MachineStore) Path() string {
	if s == nil || s.inner == nil {
		return ""
	}
	return s.inner.Path()
}

// Path returns the database path.
func (s *ProjectStore) Path() string {
	if s == nil || s.inner == nil {
		return ""
	}
	return s.inner.Path()
}

// Role returns RoleMachine.
func (s *MachineStore) Role() Role { return RoleMachine }

// Role returns RoleProject.
func (s *ProjectStore) Role() Role { return RoleProject }

// FormatIdentity returns the durable format identity.
func (s *MachineStore) FormatIdentity() string { return MachineFormatIdentity }

// FormatIdentity returns the durable format identity.
func (s *ProjectStore) FormatIdentity() string { return ProjectFormatIdentity }

// Foundation returns the underlying compact store for later schema work.
// Callers must not open the same path under a different role.
func (s *MachineStore) Foundation() *store.Store {
	if s == nil {
		return nil
	}
	return s.inner
}

// Foundation returns the underlying compact store for later schema work.
func (s *ProjectStore) Foundation() *store.Store {
	if s == nil {
		return nil
	}
	return s.inner
}

// Metadata returns foundation metadata.
func (s *MachineStore) Metadata(ctx context.Context) (store.Metadata, error) {
	if s == nil || s.inner == nil {
		return store.Metadata{}, fmt.Errorf("machine store is nil")
	}
	return s.inner.Metadata(ctx)
}

// Metadata returns foundation metadata.
func (s *ProjectStore) Metadata(ctx context.Context) (store.Metadata, error) {
	if s == nil || s.inner == nil {
		return store.Metadata{}, fmt.Errorf("project store is nil")
	}
	return s.inner.Metadata(ctx)
}

// CheckIntegrity runs foundation integrity checks.
func (s *MachineStore) CheckIntegrity(ctx context.Context) error {
	if s == nil || s.inner == nil {
		return fmt.Errorf("machine store is nil")
	}
	return s.inner.CheckIntegrity(ctx)
}

// CheckIntegrity runs foundation integrity checks.
func (s *ProjectStore) CheckIntegrity(ctx context.Context) error {
	if s == nil || s.inner == nil {
		return fmt.Errorf("project store is nil")
	}
	return s.inner.CheckIntegrity(ctx)
}

// BeginCrossDBTransaction is intentionally unsupported.
func BeginCrossDBTransaction(_ *MachineStore, _ *ProjectStore) error {
	return ErrCrossDBTransaction
}
