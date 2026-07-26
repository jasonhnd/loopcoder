package projectschema

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// CursorFormatVersion is the opaque cursor encoding version.
const CursorFormatVersion = 1

// ErrInvalidCursor is returned for malformed, wrong-project, or future cursors.
var ErrInvalidCursor = errors.New("projectschema: invalid cursor")

// Cursor is an opaque replay position (project + last accepted sequence).
type Cursor struct {
	FormatVersion int    `json:"v"`
	ProjectID     string `json:"project_id"`
	Sequence      int64  `json:"seq"`
}

// EncodeCursor returns a path-free opaque cursor string.
func EncodeCursor(c Cursor) (string, error) {
	if c.FormatVersion == 0 {
		c.FormatVersion = CursorFormatVersion
	}
	if strings.TrimSpace(c.ProjectID) == "" {
		return "", fmt.Errorf("%w: project_id required", ErrInvalidCursor)
	}
	if c.Sequence < 0 {
		return "", fmt.Errorf("%w: sequence must be non-negative", ErrInvalidCursor)
	}
	// Never include paths or payloads.
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// DecodeCursor parses an opaque cursor without applying project checks.
func DecodeCursor(raw string) (Cursor, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Cursor{}, fmt.Errorf("%w: empty", ErrInvalidCursor)
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return Cursor{}, fmt.Errorf("%w: decode", ErrInvalidCursor)
	}
	var c Cursor
	if err := json.Unmarshal(b, &c); err != nil {
		return Cursor{}, fmt.Errorf("%w: json", ErrInvalidCursor)
	}
	if c.FormatVersion == 0 {
		return Cursor{}, fmt.Errorf("%w: missing format version", ErrInvalidCursor)
	}
	if c.FormatVersion > CursorFormatVersion {
		return Cursor{}, fmt.Errorf("%w: future format version %d", ErrInvalidCursor, c.FormatVersion)
	}
	if c.FormatVersion < 1 {
		return Cursor{}, fmt.Errorf("%w: unsupported format version %d", ErrInvalidCursor, c.FormatVersion)
	}
	if strings.TrimSpace(c.ProjectID) == "" {
		return Cursor{}, fmt.Errorf("%w: project_id required", ErrInvalidCursor)
	}
	if c.Sequence < 0 {
		return Cursor{}, fmt.Errorf("%w: negative sequence", ErrInvalidCursor)
	}
	return c, nil
}

// ValidateCursorForProject rejects wrong-project and future-version cursors.
// It does not silently fall back to sequence zero.
func ValidateCursorForProject(c Cursor, projectID string) error {
	if c.FormatVersion > CursorFormatVersion {
		return fmt.Errorf("%w: future format version %d", ErrInvalidCursor, c.FormatVersion)
	}
	if c.ProjectID != projectID {
		return fmt.Errorf("%w: wrong project", ErrInvalidCursor)
	}
	return nil
}

// ZeroCursor returns a cursor before the first event for a project.
func ZeroCursor(projectID string) Cursor {
	return Cursor{FormatVersion: CursorFormatVersion, ProjectID: projectID, Sequence: 0}
}

// ErrRebuildRequired signals corrupt/missing projection data; events are untouched.
var ErrRebuildRequired = errors.New("projectschema: projection rebuild required")

// ErrCheckpointConflict is returned when CAS expected revision does not match.
var ErrCheckpointConflict = errors.New("projectschema: checkpoint conflict")
