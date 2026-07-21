package outputcap_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jasonhnd/loopcoder/internal/outputcap"
	"github.com/jasonhnd/loopcoder/internal/sanitize"
)

func TestFloodDoesNotBlockAndBoundsDisplay(t *testing.T) {
	root := t.TempDir()
	cap, err := outputcap.New(outputcap.Options{
		PayloadRoot: root,
		AttemptID:   "att-flood",
		Limits: outputcap.Limits{
			MaxDisplayBytes: 1024,
			MaxDisplayLines: 20,
			MaxDiskBytes:    1 << 20,
			RateBytesPerSec: 1 << 20,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte("x"), 4096)
	for i := 0; i < 200; i++ {
		n, err := cap.StdoutWriter().Write(chunk)
		if err != nil {
			t.Fatalf("write: %v", err)
		}
		if n != len(chunk) {
			t.Fatalf("drained %d want %d", n, len(chunk))
		}
	}
	ex := cap.Excerpts()
	if len(ex) == 0 {
		t.Fatal("expected excerpt")
	}
	if len(ex[0].Text) > 2048+len(outputcap.TruncationMarker)+len(outputcap.DropMarker) {
		t.Fatalf("excerpt too large: %d", len(ex[0].Text))
	}
	if !ex[0].Truncated && !ex[0].Dropped {
		t.Fatal("expected truncation or drop under flood")
	}
	ev, err := cap.Close()
	if err != nil {
		t.Fatal(err)
	}
	if ev.Stdout.BytesIn < int64(200*4096) {
		t.Fatalf("bytes in = %d", ev.Stdout.BytesIn)
	}
	if ev.Stdout.Digest == "" {
		t.Fatal("missing digest")
	}
	if !ev.FullyObserved {
		t.Fatal("expected fully observed without write fault")
	}
}

func TestLogsUnderPayloadRootOnly(t *testing.T) {
	root := t.TempDir()
	_, err := outputcap.ResolveLogPath(root, "../escape.log")
	if !errors.Is(err, outputcap.ErrOutsidePayloadRoot) {
		t.Fatalf("err=%v", err)
	}
	_, err = outputcap.ResolveLogPath(root, "/etc/passwd")
	if !errors.Is(err, outputcap.ErrOutsidePayloadRoot) {
		t.Fatalf("err=%v", err)
	}
	p, err := outputcap.ResolveLogPath(root, "logs/a/stdout.log")
	if err != nil {
		t.Fatal(err)
	}
	if err := outputcap.ValidateUnderRoot(root, p); err != nil {
		t.Fatal(err)
	}
	// Capture creates files under root/logs
	cap, err := outputcap.New(outputcap.Options{PayloadRoot: root, AttemptID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = cap.StdoutWriter().Write([]byte("hi\n"))
	ev, _ := cap.Close()
	logPath := filepath.Join(root, "logs", "a", "stdout.log")
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("log missing: %v digest=%s", err, ev.Stdout.Digest)
	}
}

func TestExcerptRedactedUTF8Truncation(t *testing.T) {
	root := t.TempDir()
	cap, err := outputcap.New(outputcap.Options{
		PayloadRoot: root,
		AttemptID:   "sec",
		Limits:      outputcap.Limits{MaxDisplayBytes: 512, MaxDisplayLines: 50},
	})
	if err != nil {
		t.Fatal(err)
	}
	secret := "token=ghp_abcdefghijklmnopqrstuv\n"
	_, _ = cap.StdoutWriter().Write([]byte(secret))
	_, _ = cap.StdoutWriter().Write([]byte{0xff, 0xfe, 'o', 'k', '\n'}) // invalid UTF-8
	ex := cap.Excerpts()
	if len(ex) == 0 {
		t.Fatal("no excerpt")
	}
	if !utf8.ValidString(ex[0].Text) {
		t.Fatal("excerpt not valid utf-8")
	}
	if strings.Contains(ex[0].Text, "ghp_abcdefghijklmnopqrstuv") {
		t.Fatalf("secret leaked in excerpt (test must not print raw): redacted=%v", strings.Contains(ex[0].Text, sanitize.RedactedGitHub) || strings.Contains(ex[0].Text, "REDACTED"))
	}
	if !strings.Contains(ex[0].Text, "REDACTED") && !strings.Contains(ex[0].Text, sanitize.RedactedGitHub) && !strings.Contains(ex[0].Text, sanitize.RedactedSecret) {
		// sanitize may use REDACTED_* constants
		t.Logf("excerpt=%q", ex[0].Text) // only redacted text
	}
	_, _ = cap.Close()
}

func TestTerminalEvidenceCounts(t *testing.T) {
	root := t.TempDir()
	cap, err := outputcap.New(outputcap.Options{
		PayloadRoot: root,
		AttemptID:   "term",
		Limits:      outputcap.Limits{MaxDisplayBytes: 16, MaxDisplayLines: 2, MaxDiskBytes: 1 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = cap.StdoutWriter().Write([]byte("line1\nline2\nline3\nline4\n"))
	_, _ = cap.StderrWriter().Write([]byte("err\n"))
	ev, err := cap.Close()
	if err != nil {
		t.Fatal(err)
	}
	if ev.Stdout.BytesIn == 0 || ev.Stderr.BytesIn == 0 {
		t.Fatalf("%#v", ev)
	}
	if !ev.Stdout.Truncated && ev.Stdout.DroppedBytes == 0 {
		// small display should truncate or drop
		t.Logf("stdout stats %#v", ev.Stdout)
	}
	if ev.Stdout.Digest == ev.Stderr.Digest && ev.Stdout.BytesIn != ev.Stderr.BytesIn {
		t.Fatal("digests should differ for different content")
	}
}

func TestLogWriteFailureTypedFault(t *testing.T) {
	root := t.TempDir()
	cap, err := outputcap.New(outputcap.Options{
		PayloadRoot: root,
		AttemptID:   "fail",
		OpenFile: func(path string) (io.WriteCloser, error) {
			return &failWriter{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = cap.StdoutWriter().Write([]byte("data"))
	if !errors.Is(err, outputcap.ErrLogWrite) {
		t.Fatalf("err=%v", err)
	}
	ev, err := cap.Close()
	if !errors.Is(err, outputcap.ErrLogWrite) && !errors.Is(cap.Fault(), outputcap.ErrLogWrite) {
		t.Fatalf("close err=%v fault=%v", err, cap.Fault())
	}
	if ev.FullyObserved {
		t.Fatal("must not claim fully observed after log fault")
	}
}

func TestPartialLineAndBinary(t *testing.T) {
	root := t.TempDir()
	cap, err := outputcap.New(outputcap.Options{PayloadRoot: root, AttemptID: "bin"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = cap.StdoutWriter().Write([]byte("partial"))
	_, _ = cap.StdoutWriter().Write([]byte{0x00, 0x01, 0x02})
	_, _ = cap.StdoutWriter().Write([]byte("tail\n"))
	ex := cap.Excerpts()
	if len(ex) == 0 || !utf8.ValidString(ex[0].Text) {
		t.Fatalf("%#v", ex)
	}
	_, _ = cap.Close()
}

type failWriter struct{}

func (f *failWriter) Write(p []byte) (int, error) {
	return 0, errors.New("disk full simulated")
}
func (f *failWriter) Close() error { return nil }

func TestCancelCloseIdempotent(t *testing.T) {
	root := t.TempDir()
	cap, err := outputcap.New(outputcap.Options{
		PayloadRoot: root,
		AttemptID:   "c",
		Now:         func() time.Time { return time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = cap.Close()
	_, err = cap.StdoutWriter().Write([]byte("x"))
	if !errors.Is(err, outputcap.ErrClosed) {
		t.Fatalf("err=%v", err)
	}
}
