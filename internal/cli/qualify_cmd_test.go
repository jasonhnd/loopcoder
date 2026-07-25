package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestQualifyCommandRequiredActionsEvidence proves usage code 2 before Qualify
// when any required Actions authority flag is missing/zero. No archive/gh/network.
func TestQualifyCommandRequiredActionsEvidence(t *testing.T) {
	base := []string{
		"--archive", "/tmp/does-not-matter.tgz",
		"--digest", strings.Repeat("a", 64),
		"--sha", strings.Repeat("b", 40),
		"--repository", "jasonhnd/loopcoder",
		"--integration-run-id", "99",
		"--integration-run-attempt", "1",
		"--rc-run-id", "77",
		"--rc-artifact-id", "88",
		"--canary-evidence", "/tmp/canary-evidence.json",
		"--canary-project-id", "disp-canary-1",
		"--canary-run-id", "run_canary_1",
		"--canary-pr-head", strings.Repeat("c", 40),
	}
	cases := []struct {
		name    string
		mutate  func([]string) []string
		wantSub string
	}{
		{
			name: "missing_archive",
			mutate: func(a []string) []string {
				return dropFlag(a, "--archive")
			},
			wantSub: "--archive",
		},
		{
			name: "missing_digest",
			mutate: func(a []string) []string {
				return dropFlag(a, "--digest")
			},
			wantSub: "--digest",
		},
		{
			name: "missing_sha",
			mutate: func(a []string) []string {
				return dropFlag(a, "--sha")
			},
			wantSub: "--sha",
		},
		{
			name: "missing_repository",
			mutate: func(a []string) []string {
				return dropFlag(a, "--repository")
			},
			wantSub: "--repository",
		},
		{
			name: "zero_integration_run_id",
			mutate: func(a []string) []string {
				return setFlag(a, "--integration-run-id", "0")
			},
			wantSub: "--integration-run-id",
		},
		{
			name: "zero_integration_run_attempt",
			mutate: func(a []string) []string {
				return setFlag(a, "--integration-run-attempt", "0")
			},
			wantSub: "--integration-run-attempt",
		},
		{
			name: "zero_rc_run_id",
			mutate: func(a []string) []string {
				return setFlag(a, "--rc-run-id", "0")
			},
			wantSub: "--rc-run-id",
		},
		{
			name: "zero_rc_artifact_id",
			mutate: func(a []string) []string {
				return setFlag(a, "--rc-artifact-id", "0")
			},
			wantSub: "--rc-artifact-id",
		},
		{
			name: "empty_repository",
			mutate: func(a []string) []string {
				return setFlag(a, "--repository", "")
			},
			wantSub: "--repository",
		},
		{
			name: "missing_canary_evidence",
			mutate: func(a []string) []string {
				return dropFlag(a, "--canary-evidence")
			},
			wantSub: "--canary-evidence",
		},
		{
			name: "empty_canary_evidence",
			mutate: func(a []string) []string {
				return setFlag(a, "--canary-evidence", "")
			},
			wantSub: "--canary-evidence",
		},
		{
			name: "missing_canary_project_id",
			mutate: func(a []string) []string {
				return dropFlag(a, "--canary-project-id")
			},
			wantSub: "--canary-project-id",
		},
		{
			name: "empty_canary_project_id",
			mutate: func(a []string) []string {
				return setFlag(a, "--canary-project-id", "")
			},
			wantSub: "--canary-project-id",
		},
		{
			name: "missing_canary_run_id",
			mutate: func(a []string) []string {
				return dropFlag(a, "--canary-run-id")
			},
			wantSub: "--canary-run-id",
		},
		{
			name: "empty_canary_run_id",
			mutate: func(a []string) []string {
				return setFlag(a, "--canary-run-id", "")
			},
			wantSub: "--canary-run-id",
		},
		{
			name: "missing_canary_pr_head",
			mutate: func(a []string) []string {
				return dropFlag(a, "--canary-pr-head")
			},
			wantSub: "--canary-pr-head",
		},
		{
			name: "empty_canary_pr_head",
			mutate: func(a []string) []string {
				return setFlag(a, "--canary-pr-head", "")
			},
			wantSub: "--canary-pr-head",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := tc.mutate(append([]string(nil), base...))
			var stdout, stderr bytes.Buffer
			code := runQualify(args, &stdout, &stderr, Deps{})
			if code != 2 {
				t.Fatalf("code=%d want 2 stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), "missing required flags") {
				t.Fatalf("stderr=%q", stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.wantSub) {
				t.Fatalf("want %q in stderr=%q", tc.wantSub, stderr.String())
			}
			// Must not have attempted artifact qualify (no digest/passed output).
			if strings.Contains(stdout.String(), "passed=") || strings.Contains(stdout.String(), "digest=") {
				t.Fatalf("must not run Qualify: stdout=%q", stdout.String())
			}
		})
	}
}

func dropFlag(args []string, name string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == name {
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				i++ // skip value
			}
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func setFlag(args []string, name, value string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == name {
			out = append(out, name, value)
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				i++
			}
			continue
		}
		out = append(out, args[i])
	}
	return out
}
