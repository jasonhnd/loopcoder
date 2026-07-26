package outputcap

import (
	"errors"
	"time"
)

// Defaults for memory display and disk retention.
const (
	DefaultMaxDisplayBytes = 64 * 1024
	DefaultMaxDisplayLines = 200
	DefaultMaxDiskBytes    = 32 * 1024 * 1024
	DefaultMaxLineBytes    = 8 * 1024
	DefaultRateBytesPerSec = 4 * 1024 * 1024
	TruncationMarker       = "\n…[truncated]…\n"
	DropMarker             = "\n…[dropped]…\n"
)

var (
	// ErrOutsidePayloadRoot means a log path escaped the project payload root.
	ErrOutsidePayloadRoot = errors.New("outputcap: path outside project payload root")
	// ErrLogWrite is a typed runtime fault for disk failures.
	ErrLogWrite = errors.New("outputcap: log write failed")
	// ErrClosed means the capture was closed.
	ErrClosed = errors.New("outputcap: closed")
	// ErrInvalidRoot means payload root is empty or not absolute.
	ErrInvalidRoot = errors.New("outputcap: invalid payload root")
)

// StreamName identifies stdout or stderr.
type StreamName string

const (
	StreamStdout StreamName = "stdout"
	StreamStderr StreamName = "stderr"
)

// Limits configure capture bounds.
type Limits struct {
	MaxDisplayBytes int
	MaxDisplayLines int
	MaxDiskBytes    int64
	MaxLineBytes    int
	// RateBytesPerSec bounds average write rate into display retention
	// (disk still drains; excess is dropped from display only).
	RateBytesPerSec int
}

// Normalize applies defaults for zero fields.
func (l Limits) Normalize() Limits {
	if l.MaxDisplayBytes <= 0 {
		l.MaxDisplayBytes = DefaultMaxDisplayBytes
	}
	if l.MaxDisplayLines <= 0 {
		l.MaxDisplayLines = DefaultMaxDisplayLines
	}
	if l.MaxDiskBytes <= 0 {
		l.MaxDiskBytes = DefaultMaxDiskBytes
	}
	if l.MaxLineBytes <= 0 {
		l.MaxLineBytes = DefaultMaxLineBytes
	}
	if l.RateBytesPerSec <= 0 {
		l.RateBytesPerSec = DefaultRateBytesPerSec
	}
	return l
}

// StreamStats is per-stream terminal evidence.
type StreamStats struct {
	Name            StreamName
	BytesIn         int64
	BytesWrittenLog int64
	BytesDisplay    int64
	Truncated       bool
	DroppedBytes    int64
	DroppedLines    int64
	// Digest is sha256 of all bytes accepted on the stream (pre-redaction raw).
	Digest string
	// LogPath is relative to payload root or basenamed for redaction.
	LogPath string
}

// TerminalEvidence aggregates capture after flush/close.
type TerminalEvidence struct {
	Stdout        StreamStats
	Stderr        StreamStats
	FullyObserved bool
	// Fault is set when log write failed; attempt must not claim full observation success.
	Fault    error
	ClosedAt time.Time
}

// Excerpt is a bounded redacted event payload fragment.
type Excerpt struct {
	Stream    StreamName
	Text      string // valid UTF-8, redacted
	Truncated bool
	Dropped   bool
	Seq       int // order of emission
}
