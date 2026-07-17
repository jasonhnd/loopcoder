package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestWaitQuotaResetRequiresUntil(t *testing.T) {
	var stderr bytes.Buffer
	exit := RunWithDeps([]string{"wait", "quota-reset"}, &bytes.Buffer{}, &stderr, Deps{
		Now: func() time.Time { return time.Date(2026, 7, 17, 16, 0, 0, 0, time.UTC) },
	})
	if exit != 2 {
		t.Fatalf("exit = %d, want 2; stderr=%s", exit, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--until is required") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestWaitQuotaResetRejectsPastUntil(t *testing.T) {
	var stderr bytes.Buffer
	past := time.Date(2026, 7, 17, 15, 0, 0, 0, time.UTC).Format(time.RFC3339)
	exit := RunWithDeps([]string{"wait", "quota-reset", "--until", past}, &bytes.Buffer{}, &stderr, Deps{
		Now: func() time.Time { return time.Date(2026, 7, 17, 16, 0, 0, 0, time.UTC) },
	})
	if exit != 2 {
		t.Fatalf("exit = %d, want 2; stderr=%s", exit, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not applicable") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
