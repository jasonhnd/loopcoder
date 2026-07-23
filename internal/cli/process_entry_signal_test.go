package cli

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/goalrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

// Probe modes re-exec this test binary as a real process entry (fresh
// processEntryOnce + signal state). Mirrors cmd/loopcoder/main.go wiring.
const (
	probeEnvKey               = "LOOPCODER_SIGNAL_PROBE"
	probeMainRunWithBuildInfo = "main-runwithbuildinfo"
	probeWorkflowHang         = "workflow-hang-child"
)

func TestMain(m *testing.M) {
	switch os.Getenv(probeEnvKey) {
	case probeMainRunWithBuildInfo:
		// Exact main-equivalent path: RunWithBuildInfo (not RunWithDeps).
		// Block long enough for the parent to deliver external SIGTERM.
		until := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)
		code := RunWithBuildInfo([]string{
			"wait", "quota-reset",
			"--until", until,
			"--format", "json",
		}, os.Stdout, os.Stderr, BuildInfo{
			Version: "probe-signal-entry",
			Commit:  "probe",
			Date:    "probe",
		})
		os.Exit(code)
	case probeWorkflowHang:
		// Shared entry setup (same as Run/RunWithBuildInfo) + CommandContext
		// into goalrun with a real mid-flight hang child. External SIGTERM must
		// cancel the context so workflowrun appends kind=interrupt and durable
		// partial — never hand-written events.
		ensureProcessEntry(os.Stderr)
		home := os.Getenv("LOOPCODER_HOME")
		projectID := os.Getenv("LOOPCODER_PROBE_PROJECT")
		runID := os.Getenv("LOOPCODER_PROBE_RUN")
		if home == "" || projectID == "" || runID == "" {
			os.Stderr.WriteString("workflow-hang probe: missing LOOPCODER_HOME/project/run\n")
			os.Exit(2)
		}
		res, err := goalrun.Execute(CommandContext(), goalrun.Request{
			ProjectID: projectID,
			RunID:     runID,
			Goal:      "implement hang probe for durable signal interrupt ledger",
			Issue:     "1397",
			Actor:     "owner",
			Provider:  "fixture",
			Model:     "fixture-model",
			HomeDir:   home,
			// Hang first graph child until CommandContext is cancelled by SIGTERM.
			Executor: workflowrun.FakeChildExecutor{
				HomeDir: home,
				HangIDs: map[string]bool{"wi_research": true},
			},
		})
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
		if err != nil {
			os.Exit(4)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestRunWithBuildInfo_MainEquivalent_SIGTERM_InstallsHandler proves the
// production main path (RunWithBuildInfo) installs the signal handler: external
// SIGTERM yields handler banner + exit 130 (os.Exit), not default disposition
// (Unix: ProcessState.ExitCode()==-1 with WaitStatus.Signaled SIGTERM).
func TestRunWithBuildInfo_MainEquivalent_SIGTERM_InstallsHandler(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal disposition probe is POSIX")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunWithBuildInfo_MainEquivalent_SIGTERM_InstallsHandler$", "-test.v=false")
	cmd.Env = append(os.Environ(), probeEnvKey+"="+probeMainRunWithBuildInfo)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start probe: %v", err)
	}
	// Allow RunWithBuildInfo → wait to enter the blocking path.
	time.Sleep(300 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("SIGTERM: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("probe did not exit after SIGTERM; stderr=%q stdout=%q", stderr.String(), stdout.String())
	}
	if cmd.ProcessState == nil {
		t.Fatal("missing process state after wait")
	}
	// Default SIGTERM disposition: kernel kills the process. On Unix,
	// ProcessState.ExitCode() is -1 and WaitStatus.Signaled() is true with
	// Signal()==SIGTERM. Do not treat numeric 15 as ExitCode — that is the
	// signal number, not the wait exit code Go reports for a clean exit().
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		t.Fatalf("SIGTERM used default disposition (signaled by %v, ExitCode=%d); handler not installed. stderr=%q",
			ws.Signal(), cmd.ProcessState.ExitCode(), stderr.String())
	}
	// Primary assertions: handler banner + os.Exit(130) from installShutdownOnSignal.
	if !strings.Contains(stderr.String(), "[loopcoder] interrupted") {
		t.Fatalf("handler did not print interrupt banner; ExitCode=%d stderr=%q stdout=%q",
			cmd.ProcessState.ExitCode(), stderr.String(), stdout.String())
	}
	if got := cmd.ProcessState.ExitCode(); got != 130 {
		t.Fatalf("ExitCode=%d want 130 (handler os.Exit); stderr=%q", got, stderr.String())
	}
}

// TestWorkflowGoal_HangChild_SIGTERM_WritesInterruptLedger is a real subprocess:
// shared process entry + CommandContext + hanging child. After external SIGTERM
// the event ledger must contain kind=interrupt and durable partial/cancelled
// attempt evidence — never hand-written events.
func TestWorkflowGoal_HangChild_SIGTERM_WritesInterruptLedger(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal disposition probe is POSIX")
	}
	home := filepath.Join(t.TempDir(), "loopcoder-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	projectID := "proj-signal-hang"
	runID := "run_signal_hang_1"

	cmd := exec.Command(os.Args[0], "-test.run=^TestWorkflowGoal_HangChild_SIGTERM_WritesInterruptLedger$", "-test.v=false")
	cmd.Env = append(os.Environ(),
		probeEnvKey+"="+probeWorkflowHang,
		"LOOPCODER_HOME="+home,
		"LOOPCODER_PROBE_PROJECT="+projectID,
		"LOOPCODER_PROBE_RUN="+runID,
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start hang probe: %v", err)
	}

	// Wait until launch (or claim) is durable in the ledger, then SIGTERM.
	ledgerPath := ""
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		found := findWorkflowEvents(home, projectID, runID)
		if found == "" {
			continue
		}
		body, err := os.ReadFile(found)
		if err != nil {
			continue
		}
		if strings.Contains(string(body), `"kind":"launch"`) || strings.Contains(string(body), `"kind":"claim"`) {
			ledgerPath = found
			break
		}
	}
	if ledgerPath == "" {
		_ = cmd.Process.Kill()
		t.Fatalf("no launch/claim in ledger before SIGTERM; stderr=%q stdout=%q home=%s",
			stderr.String(), stdout.String(), home)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("SIGTERM: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("hang probe did not exit after SIGTERM; stderr=%q", stderr.String())
	}

	// Re-read ledger after process exit — must include real interrupt event.
	body, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("read ledger %s: %v", ledgerPath, err)
	}
	lines := signalProbeNonEmptyLines(string(body))
	if len(lines) == 0 {
		t.Fatal("empty ledger after SIGTERM")
	}
	hasInterrupt := false
	for _, line := range lines {
		var ev map[string]any
		if jerr := json.Unmarshal([]byte(line), &ev); jerr != nil {
			t.Fatalf("ledger line not JSON: %q err=%v", line, jerr)
		}
		kind, _ := ev["kind"].(string)
		if kind == "interrupt" {
			hasInterrupt = true
			break
		}
	}
	if !hasInterrupt {
		t.Fatalf("FAIL: no interrupt event in ledger after SIGTERM (must be real, not hand-written).\nledger=%s\nbody:\n%s\nstderr=%q\nstdout=%q",
			ledgerPath, body, stderr.String(), stdout.String())
	}

	// Attempt cancelled / durable partial for forced-kill recovery.
	partialPath := filepath.Join(filepath.Dir(ledgerPath), "workflow-partial.json")
	if st, err := os.Stat(partialPath); err != nil || st.Size() == 0 {
		// goal-checkpoint is also acceptable durable evidence after graceful cancel path.
		cpPath, _ := goalrun.CheckpointPath(home, projectID, runID)
		if _, cerr := os.Stat(cpPath); cerr != nil {
			t.Fatalf("want durable partial or goal-checkpoint after interrupt; partial=%v checkpoint=%v\nledger:\n%s",
				err, cerr, body)
		}
	}

	// Handler banner proves entry setup ran in this subprocess.
	if !strings.Contains(stderr.String(), "[loopcoder] interrupted") {
		// Grace exit may race with goalrun return; interrupt ledger is authoritative.
		// Still log for diagnosis when banner is missing.
		t.Logf("note: interrupt banner missing (ledger still required); stderr=%q", stderr.String())
	}
}

func findWorkflowEvents(home, projectID, runID string) string {
	// Production layout: $HOME/projects/<project>/runs/<run>/workflow-events.jsonl
	candidate := filepath.Join(home, "projects", projectID, "runs", runID, "workflow-events.jsonl")
	if st, err := os.Stat(candidate); err == nil && st.Size() > 0 {
		return candidate
	}
	// Walk fallback if layout nesting differs slightly.
	root := filepath.Join(home, "projects", projectID)
	var found string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if info.Name() == "workflow-events.jsonl" && strings.Contains(path, runID) {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

func signalProbeNonEmptyLines(s string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(s))
	// Ledger lines can be long JSON.
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}
