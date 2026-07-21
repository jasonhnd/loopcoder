package outputcap

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jasonhnd/loopcoder/internal/sanitize"
)

// Capture is a dual-stream bounded sink for one attempt.
type Capture struct {
	mu sync.Mutex

	payloadRoot string
	limits      Limits
	stdout      *stream
	stderr      *stream
	excerpts    []Excerpt
	excerptSeq  int
	closed      bool
	fault       error
	now         func() time.Time
}

// Options configures Capture.
type Options struct {
	// PayloadRoot is the project payload root (e.g. projects/<id>).
	PayloadRoot string
	// AttemptID names log files under logs/<attemptID>/.
	AttemptID string
	Limits    Limits
	Now       func() time.Time
	// OpenFile allows tests to inject failing writers.
	OpenFile func(path string) (io.WriteCloser, error)
}

// New creates a Capture writing raw logs under payloadRoot/logs/<attemptID>/.
func New(opts Options) (*Capture, error) {
	if strings.TrimSpace(opts.PayloadRoot) == "" {
		return nil, ErrInvalidRoot
	}
	if strings.TrimSpace(opts.AttemptID) == "" {
		return nil, fmt.Errorf("%w: attempt id required", ErrInvalidRoot)
	}
	lim := opts.Limits.Normalize()
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	open := opts.OpenFile
	if open == nil {
		open = func(path string) (io.WriteCloser, error) {
			return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		}
	}
	c := &Capture{
		payloadRoot: filepath.Clean(opts.PayloadRoot),
		limits:      lim,
		now:         now,
	}
	var err error
	c.stdout, err = newStream(StreamStdout, c.payloadRoot, opts.AttemptID, lim, open)
	if err != nil {
		return nil, err
	}
	c.stderr, err = newStream(StreamStderr, c.payloadRoot, opts.AttemptID, lim, open)
	if err != nil {
		_ = c.stdout.Close()
		return nil, err
	}
	return c, nil
}

// StdoutWriter returns an io.Writer that never blocks join on display bounds.
func (c *Capture) StdoutWriter() io.Writer { return &streamWriter{c: c, s: c.stdout} }

// StderrWriter returns an io.Writer that never blocks join on display bounds.
func (c *Capture) StderrWriter() io.Writer { return &streamWriter{c: c, s: c.stderr} }

type streamWriter struct {
	c *Capture
	s *stream
}

func (w *streamWriter) Write(p []byte) (int, error) {
	return w.c.writeStream(w.s, p)
}

func (c *Capture) writeStream(s *stream, p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, ErrClosed
	}
	n, err := s.Write(p, c.now())
	// Always report full len(p) consumed for drain semantics unless hard fault.
	if err != nil {
		c.fault = err
		// Still count as drained from caller's perspective to avoid pipe deadlock:
		// return len(p) with error so some pumps stop; our contract is we don't
		// block. Prefer returning n==len(p) with ErrLogWrite so pumps can finish.
		return len(p), err
	}
	if s.displayTruncated || s.droppedLines > 0 {
		// Record excerpt update opportunistically.
	}
	return n, nil
}

// Excerpts returns ordered redacted display excerpts.
func (c *Capture) Excerpts() []Excerpt {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Excerpt, 0, 2)
	if e := c.stdout.excerpt(c.nextSeqLocked()); e.Text != "" || e.Truncated || e.Dropped {
		out = append(out, e)
	}
	if e := c.stderr.excerpt(c.nextSeqLocked()); e.Text != "" || e.Truncated || e.Dropped {
		out = append(out, e)
	}
	return out
}

func (c *Capture) nextSeqLocked() int {
	c.excerptSeq++
	return c.excerptSeq
}

// Flush forces log file sync if supported.
func (c *Capture) Flush() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var first error
	for _, s := range []*stream{c.stdout, c.stderr} {
		if err := s.Flush(); err != nil && first == nil {
			first = err
			c.fault = err
		}
	}
	return first
}

// Close flushes and closes log files; returns terminal evidence.
func (c *Capture) Close() (TerminalEvidence, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return c.terminalLocked(), c.fault
	}
	c.closed = true
	for _, s := range []*stream{c.stdout, c.stderr} {
		if err := s.Close(); err != nil && c.fault == nil {
			c.fault = err
		}
	}
	ev := c.terminalLocked()
	ev.ClosedAt = c.now().UTC()
	return ev, c.fault
}

func (c *Capture) terminalLocked() TerminalEvidence {
	return TerminalEvidence{
		Stdout:        c.stdout.stats(),
		Stderr:        c.stderr.stats(),
		FullyObserved: c.fault == nil,
		Fault:         c.fault,
	}
}

// Fault returns the first log write fault, if any.
func (c *Capture) Fault() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fault
}

type stream struct {
	name   StreamName
	limits Limits
	file   io.WriteCloser
	path   string

	hash hash.Hash

	bytesIn          int64
	bytesWrittenLog  int64
	display          []byte // ring-ish truncated buffer
	displayLines     int
	displayTruncated bool
	droppedBytes     int64
	droppedLines     int64
	rateWindowStart  time.Time
	rateWindowBytes  int
	diskFull         bool
}

func newStream(name StreamName, root, attemptID string, lim Limits, open func(string) (io.WriteCloser, error)) (*stream, error) {
	rel := filepath.Join("logs", attemptID, string(name)+".log")
	path, err := ResolveLogPath(root, rel)
	if err != nil {
		return nil, err
	}
	f, err := open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: open %s: %v", ErrLogWrite, filepath.Base(path), err)
	}
	return &stream{
		name:   name,
		limits: lim,
		file:   f,
		path:   path,
		hash:   sha256.New(),
	}, nil
}

func (s *stream) Write(p []byte, now time.Time) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.bytesIn += int64(len(p))
	s.hash.Write(p)

	// Always attempt disk write until disk cap; continue draining on failure after fault.
	if !s.diskFull && s.file != nil {
		toWrite := p
		remain := s.limits.MaxDiskBytes - s.bytesWrittenLog
		if remain <= 0 {
			s.diskFull = true
			s.displayTruncated = true
		} else {
			if int64(len(toWrite)) > remain {
				toWrite = toWrite[:remain]
				s.diskFull = true
				s.displayTruncated = true
			}
			n, err := s.file.Write(toWrite)
			s.bytesWrittenLog += int64(n)
			if err != nil {
				return len(p), fmt.Errorf("%w: %v", ErrLogWrite, err)
			}
		}
	}

	// Display buffer: rate + size + line bounds; never block.
	s.appendDisplay(p, now)
	return len(p), nil
}

func (s *stream) appendDisplay(p []byte, now time.Time) {
	if s.rateWindowStart.IsZero() || now.Sub(s.rateWindowStart) >= time.Second {
		s.rateWindowStart = now
		s.rateWindowBytes = 0
	}
	// Rate limit applies to display retention only.
	room := s.limits.RateBytesPerSec - s.rateWindowBytes
	if room <= 0 {
		s.droppedBytes += int64(len(p))
		s.droppedLines++
		s.displayTruncated = true
		return
	}
	chunk := p
	if len(chunk) > room {
		s.droppedBytes += int64(len(chunk) - room)
		chunk = chunk[:room]
		s.displayTruncated = true
	}
	s.rateWindowBytes += len(chunk)

	// Line length bound.
	if s.limits.MaxLineBytes > 0 && len(chunk) > s.limits.MaxLineBytes {
		s.droppedBytes += int64(len(chunk) - s.limits.MaxLineBytes)
		chunk = chunk[:s.limits.MaxLineBytes]
		s.displayTruncated = true
	}

	s.display = append(s.display, chunk...)
	// Enforce display byte cap (keep tail).
	if len(s.display) > s.limits.MaxDisplayBytes {
		overflow := len(s.display) - s.limits.MaxDisplayBytes
		s.droppedBytes += int64(overflow)
		s.display = s.display[overflow:]
		s.displayTruncated = true
	}
	// Line cap (keep last N lines).
	lines := countLines(s.display)
	if lines > s.limits.MaxDisplayLines {
		s.display = tailLines(s.display, s.limits.MaxDisplayLines)
		s.displayTruncated = true
		s.droppedLines += int64(lines - s.limits.MaxDisplayLines)
	}
	s.displayLines = countLines(s.display)
}

func (s *stream) excerpt(seq int) Excerpt {
	raw := string(s.display)
	// Ensure valid UTF-8.
	if !utf8.ValidString(raw) {
		raw = strings.ToValidUTF8(raw, "\uFFFD")
		s.displayTruncated = true
	}
	text := sanitize.Text(raw)
	if s.displayTruncated && text != "" && !strings.Contains(text, "truncated") {
		text = text + TruncationMarker
	}
	if s.droppedBytes > 0 && !strings.Contains(text, "dropped") {
		text = text + DropMarker
	}
	return Excerpt{
		Stream:    s.name,
		Text:      text,
		Truncated: s.displayTruncated,
		Dropped:   s.droppedBytes > 0,
		Seq:       seq,
	}
}

func (s *stream) stats() StreamStats {
	return StreamStats{
		Name:            s.name,
		BytesIn:         s.bytesIn,
		BytesWrittenLog: s.bytesWrittenLog,
		BytesDisplay:    int64(len(s.display)),
		Truncated:       s.displayTruncated,
		DroppedBytes:    s.droppedBytes,
		DroppedLines:    s.droppedLines,
		Digest:          "sha256:" + hex.EncodeToString(s.hash.Sum(nil)),
		LogPath:         filepath.Base(s.path),
	}
}

func (s *stream) Flush() error {
	if s.file == nil {
		return nil
	}
	if f, ok := s.file.(*os.File); ok {
		return f.Sync()
	}
	return nil
}

func (s *stream) Close() error {
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	if err != nil {
		return fmt.Errorf("%w: close: %v", ErrLogWrite, err)
	}
	return nil
}

func countLines(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := 1
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	// trailing newline doesn't add empty line for cap purposes — approximate ok
	return n
}

func tailLines(b []byte, max int) []byte {
	if max <= 0 || len(b) == 0 {
		return nil
	}
	// Find start index of last `max` lines.
	lines := 0
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == '\n' {
			lines++
			if lines >= max {
				return b[i+1:]
			}
		}
	}
	return b
}
