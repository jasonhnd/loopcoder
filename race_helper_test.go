package loopcoder

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFullRaceHelperCoversEveryPackageExactlyOnce(t *testing.T) {
	inventory := []string{
		"github.com/jasonhnd/loopcoder",
		"github.com/jasonhnd/loopcoder/internal/example",
		"github.com/jasonhnd/loopcoder/internal/storage",
		"github.com/jasonhnd/loopcoder/internal/routing",
		"github.com/jasonhnd/loopcoder/internal/supervisedexec",
	}

	result := runFullRaceHelper(t, inventory, fullRaceFakeOptions{})
	if result.err != nil {
		t.Fatalf("ci-full-race.sh failed: %v\nstdout:\n%s\nstderr:\n%s", result.err, result.stdout, result.stderr)
	}
	if len(result.calls) != 5 {
		t.Fatalf("go calls = %d, want 5: %#v", len(result.calls), result.calls)
	}
	assertGoCall(t, result.calls[0], "list", "./...")

	ordinaryCall := result.calls[1]
	assertGoCallPrefix(t, ordinaryCall, "test")
	for _, want := range []string{"-race", "-count=1", "-timeout=20m", "-p=2"} {
		if !containsToken(ordinaryCall, want) {
			t.Fatalf("ordinary race call missing %q: %#v", want, ordinaryCall)
		}
	}
	for _, isolated := range inventory[2:] {
		if containsToken(ordinaryCall, isolated) {
			t.Fatalf("ordinary race call included isolated package %q: %#v", isolated, ordinaryCall)
		}
	}

	isolatedPackages := []string{
		"github.com/jasonhnd/loopcoder/internal/storage",
		"github.com/jasonhnd/loopcoder/internal/routing",
		"github.com/jasonhnd/loopcoder/internal/supervisedexec",
	}
	for index, isolated := range isolatedPackages {
		call := result.calls[index+2]
		assertGoCallPrefix(t, call, "test")
		for _, want := range []string{"-race", "-count=1", "-timeout=20m", "-p=1", isolated} {
			if !containsToken(call, want) {
				t.Fatalf("isolated race call for %s missing %q: %#v", isolated, want, call)
			}
		}
		if len(raceCallPackages(call)) != 1 {
			t.Fatalf("isolated race call for %s included extra packages: %#v", isolated, call)
		}
	}

	coverage := map[string]int{}
	for _, call := range result.calls[1:] {
		for _, pkg := range raceCallPackages(call) {
			coverage[pkg]++
		}
	}
	for _, pkg := range inventory {
		if coverage[pkg] != 1 {
			t.Fatalf("package %s race coverage count = %d, want 1; all coverage: %#v", pkg, coverage[pkg], coverage)
		}
	}
}

func TestFullRaceHelperFailsClosed(t *testing.T) {
	baseInventory := []string{
		"github.com/jasonhnd/loopcoder",
		"github.com/jasonhnd/loopcoder/internal/storage",
		"github.com/jasonhnd/loopcoder/internal/routing",
		"github.com/jasonhnd/loopcoder/internal/supervisedexec",
	}

	tests := []struct {
		name        string
		inventory   []string
		options     fullRaceFakeOptions
		wantStderr  string
		wantExit    int
		wantNoMatch string
	}{
		{
			name:       "inventory command failure",
			inventory:  baseInventory,
			options:    fullRaceFakeOptions{listExit: "12"},
			wantStderr: "go list ./... failed",
			wantExit:   1,
		},
		{
			name:       "duplicate inventory",
			inventory:  append(baseInventory, "github.com/jasonhnd/loopcoder/internal/routing"),
			wantStderr: "duplicate package github.com/jasonhnd/loopcoder/internal/routing",
			wantExit:   1,
		},
		{
			name: "missing isolated package",
			inventory: []string{
				"github.com/jasonhnd/loopcoder",
				"github.com/jasonhnd/loopcoder/internal/storage",
				"github.com/jasonhnd/loopcoder/internal/supervisedexec",
			},
			wantStderr: "missing required isolated race package internal/routing",
			wantExit:   1,
		},
		{
			name:        "package failure propagates",
			inventory:   baseInventory,
			options:     fullRaceFakeOptions{failPackageMatch: "internal/routing", testExit: "23"},
			wantExit:    23,
			wantNoMatch: "internal/supervisedexec",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := runFullRaceHelper(t, tt.inventory, tt.options)
			if result.err == nil {
				t.Fatal("ci-full-race.sh succeeded, want failure")
			}
			if got := exitCode(result.err); got != tt.wantExit {
				t.Fatalf("exit code = %d, want %d\nstdout:\n%s\nstderr:\n%s", got, tt.wantExit, result.stdout, result.stderr)
			}
			if tt.wantStderr != "" && !strings.Contains(result.stderr, tt.wantStderr) {
				t.Fatalf("stderr = %q, want substring %q", result.stderr, tt.wantStderr)
			}
			if tt.wantNoMatch != "" && strings.Contains(strings.Join(flattenCalls(result.calls), "\n"), tt.wantNoMatch) {
				t.Fatalf("go calls contained %q after failure: %#v", tt.wantNoMatch, result.calls)
			}
		})
	}
}

type fullRaceFakeOptions struct {
	listExit         string
	failPackageMatch string
	testExit         string
}

type fullRaceHelperResult struct {
	stdout string
	stderr string
	calls  [][]string
	err    error
}

func runFullRaceHelper(t *testing.T, inventory []string, options fullRaceFakeOptions) fullRaceHelperResult {
	t.Helper()
	root := repositoryPolicyRoot(t)
	tempDir := t.TempDir()
	binDir := filepath.Join(tempDir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("mkdir fake bin: %v", err)
	}
	fakeGo := filepath.Join(binDir, "go")
	if err := os.WriteFile(fakeGo, []byte(fakeGoScript), 0o755); err != nil {
		t.Fatalf("write fake go: %v", err)
	}
	logPath := filepath.Join(tempDir, "go.log")

	cmd := exec.Command("bash", filepath.Join(root, "scripts", "ci-full-race.sh"))
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_GO_LIST="+strings.Join(inventory, "\n"),
		"FAKE_GO_LOG="+logPath,
		"FAKE_GO_LIST_EXIT="+options.listExit,
		"FAKE_GO_FAIL_MATCH="+options.failPackageMatch,
		"FAKE_GO_TEST_EXIT="+options.testExit,
		"LOOPCODER_RACE_ORDINARY_PARALLELISM=2",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	return fullRaceHelperResult{
		stdout: stdout.String(),
		stderr: stderr.String(),
		calls:  readFakeGoCalls(t, logPath),
		err:    err,
	}
}

const fakeGoScript = `#!/usr/bin/env bash
set -euo pipefail

{
  printf '%s' "${1:-}"
  for arg in "${@:2}"; do
    printf '\t%s' "$arg"
  done
  printf '\n'
} >>"${FAKE_GO_LOG}"

if [[ "${1:-}" = "list" && "${2:-}" = "./..." ]]; then
  if [[ -n "${FAKE_GO_LIST_EXIT:-}" && "${FAKE_GO_LIST_EXIT}" != "0" ]]; then
    exit "${FAKE_GO_LIST_EXIT}"
  fi
  printf '%s\n' "${FAKE_GO_LIST}"
  exit 0
fi

if [[ "${1:-}" = "test" ]]; then
  for arg in "$@"; do
    if [[ -n "${FAKE_GO_FAIL_MATCH:-}" && "${arg}" = *"${FAKE_GO_FAIL_MATCH}"* ]]; then
      exit "${FAKE_GO_TEST_EXIT:-1}"
    fi
  done
  exit 0
fi

exit 97
`

func readFakeGoCalls(t *testing.T, path string) [][]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read fake go log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	calls := make([][]string, 0, len(lines))
	for _, line := range lines {
		calls = append(calls, strings.Split(line, "\t"))
	}
	return calls
}

func assertGoCall(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("go call = %#v, want %#v", got, want)
	}
}

func assertGoCallPrefix(t *testing.T, got []string, prefix string) {
	t.Helper()
	if len(got) == 0 || got[0] != prefix {
		t.Fatalf("go call = %#v, want prefix %q", got, prefix)
	}
}

func containsToken(tokens []string, want string) bool {
	for _, token := range tokens {
		if token == want {
			return true
		}
	}
	return false
}

func raceCallPackages(tokens []string) []string {
	var packages []string
	for _, token := range tokens {
		if strings.HasPrefix(token, "github.com/jasonhnd/loopcoder") {
			packages = append(packages, token)
		}
	}
	return packages
}

func exitCode(err error) int {
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	return -1
}

func flattenCalls(calls [][]string) []string {
	var out []string
	for _, call := range calls {
		out = append(out, call...)
	}
	return out
}
