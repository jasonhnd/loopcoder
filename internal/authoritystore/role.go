package authoritystore

import (
	"errors"
	"fmt"
)

// Role is the authority role for a v0.9 compact store file.
type Role string

const (
	RoleMachine Role = "machine"
	RoleProject Role = "project"
)

// Format identities are independent so a file opened under one role cannot be
// reused as the other without failing format validation.
const (
	MachineFormatIdentity = "loopcoder.machine.v1"
	ProjectFormatIdentity = "loopcoder.project.v1"
)

// ErrRoleMismatch is returned when a path is already open under a different role
// or when metadata does not match the requested role.
var ErrRoleMismatch = errors.New("authority store role mismatch")

// ErrInvalidRole is returned when Open is called without machine or project.
var ErrInvalidRole = errors.New("authority store role is required")

// ErrCrossDBTransaction is returned if a caller attempts unsupported cross-DB work.
var ErrCrossDBTransaction = errors.New("authority store does not support cross-database transactions")

func formatIdentityForRole(role Role) (string, error) {
	switch role {
	case RoleMachine:
		return MachineFormatIdentity, nil
	case RoleProject:
		return ProjectFormatIdentity, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrInvalidRole, role)
	}
}
