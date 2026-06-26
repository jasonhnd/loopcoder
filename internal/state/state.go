// Package state contains helpers for loopcoder run state.
package state

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

const runIDTimeLayout = "20060102T150405Z"

var runIDPattern = regexp.MustCompile(`^run-\d{8}T\d{6}Z-issue-[1-9]\d*$`)

var ErrLatestRunSelectionNotImplemented = errors.New("latest run selection not yet implemented; see docs/go-migration.md")

// FormatTimestamp formats timestamps in UTC RFC3339 for loopcoder sidecars.
func FormatTimestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// ParseTimestamp parses RFC3339 timestamps, including PowerShell ToString("o") values.
func ParseTimestamp(value string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", value, err)
	}
	return t.UTC(), nil
}

// RunIDForIssue returns a run id in the run-<utc-compact>-issue-<n> shape.
func RunIDForIssue(issueNumber int, at time.Time) string {
	return fmt.Sprintf("run-%s-issue-%d", at.UTC().Format(runIDTimeLayout), issueNumber)
}

// IsRunID reports whether value matches the documented run id shape.
func IsRunID(value string) bool {
	return runIDPattern.MatchString(value)
}

// LatestRunID will select the newest local run in a later migration phase.
func LatestRunID(_ string) (string, error) {
	return "", ErrLatestRunSelectionNotImplemented
}
