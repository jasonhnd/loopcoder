package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/audit"
	"github.com/jasonhnd/loopcoder/internal/recovery"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/worker"
)

func TestObservabilityJSONLCommandAdaptersEmitMachineRecordsOnly(t *testing.T) {
	repoState := t.TempDir()
	record := validDispatchReport()
	record.WorkID = "run-jsonl"
	result := validDispatchResult(record)
	result.AttemptPath = filepath.Join(string(os.PathSeparator), "tmp", "repo", ".loopcoder", "runs", "run-jsonl", "workers", "job-864.attempt.json")
	result.RunID = "run-jsonl"
	result.Issue = 864
	result.Status = "succeeded"
	if err := state.AppendLifecycleTransition(repoState, state.LifecycleTransition{
		Timestamp: "2026-07-14T00:00:00Z",
		RunID:     result.RunID,
		State:     state.StatePlanned,
	}); err != nil {
		t.Fatalf("append lifecycle: %v", err)
	}
	if _, err := state.WriteAttempt(repoState, result.RunID, state.AttemptRecord{
		Version:        1,
		JobID:          "job-864",
		Issue:          864,
		Attempt:        1,
		Provider:       "codex",
		Status:         "succeeded",
		StartedAt:      "2026-07-14T00:00:00Z",
		HeartbeatAt:    "2026-07-14T00:00:01Z",
		LastProgressAt: "2026-07-14T00:00:01Z",
		Report:         &record,
	}); err != nil {
		t.Fatalf("write attempt: %v", err)
	}

	cases := []struct {
		name string
		args []string
		deps Deps
	}{
		{
			name: "dispatch",
			args: []string{"dispatch", "--repo", t.TempDir(), "--issue-number", "864", "--issue-title", "Observability", "--run-id", result.RunID, "--format", "jsonl"},
			deps: Deps{
				Dispatch: func(context.Context, worker.Options) (worker.Result, error) {
					return result, nil
				},
			},
		},
		{
			name: "audit",
			args: []string{"audit", "--repo", t.TempDir(), "--format", "jsonl"},
			deps: Deps{
				Audit: func(context.Context, audit.Options) (audit.Result, error) {
					out := audit.NewResult("repo-audit", []string{audit.LayerSAST}, audit.SeverityMedium)
					out.Report = &record
					return audit.Finalize(out), nil
				},
			},
		},
		{
			name: "recover",
			args: []string{"recover", "--repo", t.TempDir(), "--issue-number", "864", "--issue-title", "Observability", "--run-id", result.RunID, "--format", "jsonl"},
			deps: Deps{
				Recover: func(context.Context, recovery.Options) (recovery.Result, error) {
					return recovery.Result{
						Action: recovery.ActionRetry,
						DispatchResult: &recovery.DispatchResult{
							OK:          true,
							Issue:       864,
							RunID:       result.RunID,
							AttemptPath: result.AttemptPath,
							Status:      "succeeded",
							Report:      &record,
						},
					}, nil
				},
			},
		},
		{
			name: "status",
			args: []string{"status", "--repo", repoState, "--run", result.RunID, "--format", "jsonl"},
		},
		{
			name: "report",
			args: []string{"report", "--repo", repoState, "--run", result.RunID, "--format", "jsonl"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			tc.deps.Now = fixedCLINow
			exitCode := RunWithDeps(tc.args, &stdout, &stderr, tc.deps)
			if exitCode != 0 {
				t.Fatalf("RunWithDeps exit=%d stderr=%q stdout=%q", exitCode, stderr.String(), stdout.String())
			}
			assertCanonicalJSONLOnly(t, stdout.String(), tc.name)
			if strings.Contains(stdout.String(), "warning:") || strings.Contains(stdout.String(), "OBSERVABILITY ") {
				t.Fatalf("machine stdout contains human/diagnostic text:\n%s", stdout.String())
			}
		})
	}
}

func TestObservabilityHumanAdapterUsesInjectedTerminalCapabilities(t *testing.T) {
	clearPrettyEnv(t)
	var stdout, stderr bytes.Buffer
	record := validDispatchReport()
	result := validDispatchResult(record)
	exitCode := RunWithDeps([]string{"dispatch", "--repo", t.TempDir(), "--issue-number", "864", "--issue-title", "Observability"}, &stdout, &stderr, Deps{
		IsTerminal:    func(io.Writer) bool { return true },
		TerminalWidth: func(io.Writer) int { return 48 },
		Dispatch: func(context.Context, worker.Options) (worker.Result, error) {
			return result, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps exit=%d stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "\x1b[32m") || !strings.Contains(stderr.String(), "OBSERVABILITY dispatch") {
		t.Fatalf("terminal capabilities did not reach canonical renderer:\n%s", stderr.String())
	}
}

func assertCanonicalJSONLOnly(t *testing.T, output, command string) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 0 || strings.TrimSpace(output) == "" {
		t.Fatalf("%s emitted no JSONL", command)
	}
	for _, line := range lines {
		var payload struct {
			SchemaVersion string `json:"schema_version"`
			Command       string `json:"command"`
		}
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			t.Fatalf("%s JSONL line did not parse: %v\n%s", command, err, output)
		}
		if payload.SchemaVersion != "loopcoder.observability_render.v1" || payload.Command == "" {
			t.Fatalf("%s JSONL line is not canonical observability: %#v\n%s", command, payload, output)
		}
	}
}
