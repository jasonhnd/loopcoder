package workflowrun_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun/testspawn"
)

// TestSubprocessRestart_ExactReuseZeroInvocation uses two real OS processes
// (os/exec) with HOME set and LOOPCODER_HOME unset so ResolveDurableHome uses
// the stable ~/.loopcoder-under-HOME layout. Process 2 must not re-invoke.
//
// This is NOT a same-process dual-Service test.
func TestSubprocessRestart_ExactReuseZeroInvocation(t *testing.T) {
	if os.Getenv("LOOPCODER_SUBPROCESS_HELPER") == "1" {
		runSubprocessHelper(t)
		return
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	// Shared state files under root (visible to both processes).
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	countPath := filepath.Join(stateDir, "invocations.txt")
	resultPath := filepath.Join(stateDir, "result.txt")

	runHelper := func(phase string) {
		cmd := exec.Command(os.Args[0], "-test.run=TestSubprocessRestart_ExactReuseZeroInvocation", "-test.v")
		cmd.Env = append(os.Environ(),
			"LOOPCODER_SUBPROCESS_HELPER=1",
			"HOME="+home,
			"LOOPCODER_HOME=", // force unset for durable home resolution via HOME/.loopcoder
			"LOOPCODER_HELPER_PHASE="+phase,
			"LOOPCODER_HELPER_COUNT="+countPath,
			"LOOPCODER_HELPER_RESULT="+resultPath,
			"LOOPCODER_HELPER_ROOT="+root,
		)
		// Clear LOOPCODER_HOME properly.
		filtered := []string{}
		for _, e := range cmd.Env {
			if strings.HasPrefix(e, "LOOPCODER_HOME=") {
				continue
			}
			filtered = append(filtered, e)
		}
		cmd.Env = append(filtered, "HOME="+home,
			"LOOPCODER_SUBPROCESS_HELPER=1",
			"LOOPCODER_HELPER_PHASE="+phase,
			"LOOPCODER_HELPER_COUNT="+countPath,
			"LOOPCODER_HELPER_RESULT="+resultPath,
			"LOOPCODER_HELPER_ROOT="+root,
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("phase %s: %v\n%s", phase, err, out)
		}
	}

	runHelper("p1")
	raw1, _ := os.ReadFile(countPath)
	n1 := strings.Count(string(raw1), "\n")
	if n1 != 1 {
		t.Fatalf("process1 invocations want 1 got %d raw=%q", n1, raw1)
	}
	res1, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res1), "succeeded") {
		t.Fatalf("p1 result: %s", res1)
	}

	runHelper("p2")
	raw2, _ := os.ReadFile(countPath)
	n2 := strings.Count(string(raw2), "\n")
	if n2 != n1 {
		t.Fatalf("process2 must not invoke: count %d -> %d", n1, n2)
	}
	// Paths derived from same HOME must match.
	home1 := filepath.Join(home, ".loopcoder")
	// Durable home resolves to ~/.loopcoder under HOME.
	if _, err := os.Stat(filepath.Join(home1, "projects")); err != nil {
		// May use LOOPCODER_HOME empty → home.ResolveHomeDir → $HOME/.loopcoder
		t.Logf("projects under %s: %v", home1, err)
	}
}

func runSubprocessHelper(t *testing.T) {
	t.Helper()
	phase := os.Getenv("LOOPCODER_HELPER_PHASE")
	countPath := os.Getenv("LOOPCODER_HELPER_COUNT")
	resultPath := os.Getenv("LOOPCODER_HELPER_RESULT")
	// Durable home: unset LOOPCODER_HOME, HOME set by parent.
	// Service.HomeDir empty → ResolveDurableHome → $HOME/.loopcoder
	home, err := workflowrun.ResolveDurableHome("")
	if err != nil {
		t.Fatal(err)
	}
	svc := workflowrun.Service{
		Now:     t0,
		HomeDir: home,
		// Real spawn identity so process-2 recovery sees authority (no Fake exemption).
		Executor: testspawn.Executor{
			HomeDir:             home,
			Now:                 t0,
			InvocationCountPath: countPath,
		},
	}
	runID := "run_subproc_restart"
	project := "proj-subproc"
	req := withExpectedPlanDigest(t, workflowrun.Request{
		ProjectID: project, RunID: runID,
		Definition: workflowrun.OneNodeDefinition("g-sub", "impl"),
		Actor:      "owner",
		ChildRoutes: map[string]workflowrun.ChildRoute{
			"only": {
				Provider: "fixture", Model: "fixture-model", TaskClass: "tera", Depth: "medium",
				Permission: "bounded_write", AccountRef: "acct-f", WindowKind: "five_hour",
				ReservationID: "res-f", InstallRef: "install-f", RouteReason: "Winner: fixture/fixture-model restart",
			},
		},
	})
	res, err := svc.Execute(context.Background(), req)
	// Write result summary.
	msg := "status=" + res.Status
	if err != nil {
		msg += " err=" + err.Error()
	}
	for _, c := range res.Children {
		msg += " child=" + c.WorkItemID + " term=" + c.Terminal + " att=" + c.AttemptID
	}
	msg += " reuse=" + itoa(res.ReuseCount) + " launch=" + itoa(res.LaunchCount)
	_ = os.WriteFile(resultPath, []byte(msg+"\n"), 0o600)
	if phase == "p1" {
		if err != nil || res.Status != workflowrun.StatusHumanGate {
			t.Fatalf("p1 fail: %v %+v", err, res)
		}
		if res.LaunchCount != 1 {
			t.Fatalf("p1 launch=%d", res.LaunchCount)
		}
	}
	if phase == "p2" {
		// Must reuse durable terminal without PriorSucceeded and without launch.
		if err != nil {
			t.Fatalf("p2 fail: %v %+v", err, res)
		}
		if res.LaunchCount != 0 {
			t.Fatalf("p2 launch want 0 got %d", res.LaunchCount)
		}
		if res.ReuseCount < 1 {
			t.Fatalf("p2 reuse want >=1 got %d status=%s children=%+v", res.ReuseCount, res.Status, res.Children)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
