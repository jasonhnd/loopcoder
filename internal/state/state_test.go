package state

import (
	"testing"
	"time"
)

func TestFormatTimestampUsesUTCRFC3339(t *testing.T) {
	jst := time.FixedZone("JST", 9*60*60)
	input := time.Date(2026, 6, 26, 12, 34, 56, 987654321, jst)

	got := FormatTimestamp(input)
	want := "2026-06-26T03:34:56Z"
	if got != want {
		t.Fatalf("FormatTimestamp() = %q, want %q", got, want)
	}
}

func TestParseTimestampAcceptsPowerShellRoundTripFormat(t *testing.T) {
	got, err := ParseTimestamp("2026-06-26T12:34:56.1234567+09:00")
	if err != nil {
		t.Fatalf("ParseTimestamp returned error: %v", err)
	}

	want := time.Date(2026, 6, 26, 3, 34, 56, 123456700, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("ParseTimestamp() = %s, want %s", got, want)
	}
}

func TestTimestampFormatParseRoundTrip(t *testing.T) {
	input := time.Date(2026, 6, 26, 12, 34, 56, 0, time.FixedZone("offset", -7*60*60))

	formatted := FormatTimestamp(input)
	parsed, err := ParseTimestamp(formatted)
	if err != nil {
		t.Fatalf("ParseTimestamp(%q) returned error: %v", formatted, err)
	}

	if !parsed.Equal(input.UTC()) {
		t.Fatalf("parsed timestamp = %s, want %s", parsed, input.UTC())
	}
}

func TestRunIDForIssueUsesDocumentedShape(t *testing.T) {
	got := RunIDForIssue(91, time.Date(2026, 6, 26, 12, 0, 0, 0, time.FixedZone("JST", 9*60*60)))
	want := "run-20260626T030000Z-issue-91"
	if got != want {
		t.Fatalf("RunIDForIssue() = %q, want %q", got, want)
	}
	if !IsRunID(got) {
		t.Fatalf("IsRunID(%q) = false, want true", got)
	}
}
