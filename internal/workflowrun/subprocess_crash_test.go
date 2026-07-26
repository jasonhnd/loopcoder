package workflowrun_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/workclaim"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun/testspawn"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// TestSubprocessCrashAfterTerminal_Matrix exercises real OS process death after
// durable terminal fsync and before claim Close. Process 2 must be a fresh process
// that reconciles and launches zero times. Matrix: succeeded/failed/cancelled.
//
// p1 MUST os.Exit(nonzero) — not return an error (simulated unwind is rejected).
// Parent requires that exact abnormal exit and inspects durable claim/event files.
func TestSubprocessCrashAfterTerminal_Matrix(t *testing.T) {
	if os.Getenv("LOOPCODER_CRASH_HELPER") == "1" {
		runCrashHelper(t)
		return
	}
	for _, term := range []string{"succeeded", "failed", "cancelled"} {
		t.Run(term, func(t *testing.T) {
			root := t.TempDir()
			home := filepath.Join(root, "home")
			_ = os.MkdirAll(home, 0o700)
			state := filepath.Join(root, "state")
			_ = os.MkdirAll(state, 0o700)
			countPath := filepath.Join(state, "invocations.txt")
			claimStatePath := filepath.Join(state, "claim_state.txt")
			resultPath := filepath.Join(state, "result.txt")
			eventsPath := filepath.Join(state, "events_dump.txt")

			run := func(phase string) (exitCode int, out []byte) {
				filtered := []string{}
				for _, e := range os.Environ() {
					if strings.HasPrefix(e, "LOOPCODER_HOME=") {
						continue
					}
					filtered = append(filtered, e)
				}
				cmd := exec.Command(os.Args[0], "-test.run=TestSubprocessCrashAfterTerminal_Matrix/"+term+"$", "-test.v")
				cmd.Env = append(filtered,
					"HOME="+home,
					"LOOPCODER_HOME="+home,
					"LOOPCODER_CRASH_HELPER=1",
					"LOOPCODER_CRASH_PHASE="+phase,
					"LOOPCODER_CRASH_TERM="+term,
					"LOOPCODER_CRASH_COUNT="+countPath,
					"LOOPCODER_CRASH_CLAIM="+claimStatePath,
					"LOOPCODER_CRASH_RESULT="+resultPath,
					"LOOPCODER_CRASH_EVENTS="+eventsPath,
					"LOOPCODER_CRASH_EXIT=91",
				)
				out, err := cmd.CombinedOutput()
				if err == nil {
					return 0, out
				}
				if ee, ok := err.(*exec.ExitError); ok {
					return ee.ExitCode(), out
				}
				t.Fatalf("phase %s unexpected err type: %v\n%s", phase, err, out)
				return -1, out
			}

			code1, out1 := run("p1")
			if code1 != 91 {
				t.Fatalf("p1 must abnormal-exit 91 (os.Exit after terminal fsync), got %d\n%s", code1, out1)
			}
			// Parent inspects durable claim/event files itself (helper exited).
			rawClaim, err := os.ReadFile(claimStatePath)
			if err != nil {
				// Helper may have written just before exit; also inspect home store.
				csPath := filepath.Join(home, "projects", "proj-crash-sub", "runs", "run_crash_"+term, "workclaims.json")
				rawClaim, err = os.ReadFile(csPath)
				if err != nil {
					t.Fatalf("p1 did not leave durable claim state: %v\n%s", err, out1)
				}
				// Synthesize claim state line for assertions.
				state := "missing"
				if strings.Contains(string(rawClaim), `"state":"closed"`) || strings.Contains(string(rawClaim), `"state": "closed"`) {
					state = "closed"
				} else if strings.Contains(string(rawClaim), `"state":"claimed"`) || strings.Contains(string(rawClaim), `"state": "claimed"`) {
					state = "claimed"
				} else if strings.Contains(string(rawClaim), `"state":"running"`) || strings.Contains(string(rawClaim), `"state": "running"`) {
					state = "running"
				}
				rawClaim = []byte(fmt.Sprintf("after_p1 state=%s terminal_event=%s n_term=1\n", state, term))
				_ = os.WriteFile(claimStatePath, rawClaim, 0o600)
			}
			if !strings.Contains(string(rawClaim), "state=claimed") && !strings.Contains(string(rawClaim), "state=running") {
				t.Fatalf("after crash claim must still be open (not closed): %s", rawClaim)
			}
			if strings.Contains(string(rawClaim), "state=closed") {
				t.Fatalf("p1 must not Close claim before crash: %s", rawClaim)
			}

			// Inspect event log: exactly one terminal of the expected kind.
			project := "proj-crash-sub"
			runID := "run_crash_" + term
			elog, eerr := workflowrun.OpenEventLog(home, project, runID)
			if eerr != nil {
				t.Fatalf("open event log after p1: %v", eerr)
			}
			evs, eerr := elog.ReadAllForRun(project, runID)
			if eerr != nil {
				t.Fatalf("read events: %v", eerr)
			}
			nTerm := 0
			nLaunch := 0
			var lastTerm string
			var gen int
			var attempt string
			for _, e := range evs {
				switch strings.ToLower(e.Kind) {
				case "terminal":
					nTerm++
					lastTerm = e.Terminal
					gen = e.Generation
					attempt = e.AttemptID
				case "launch":
					nLaunch++
				}
			}
			if nTerm != 1 {
				t.Fatalf("p1 n_term want 1 got %d events=%d", nTerm, len(evs))
			}
			if lastTerm != term && !(term == "cancelled" && lastTerm == string(workgraph.TermCancelled)) {
				t.Fatalf("p1 terminal_event=%s want %s", lastTerm, term)
			}
			if gen <= 0 {
				t.Fatalf("p1 terminal generation must be positive got %d", gen)
			}
			if strings.TrimSpace(attempt) == "" {
				t.Fatalf("p1 terminal missing attempt_id")
			}
			if nLaunch != 1 {
				t.Fatalf("p1 launch count want 1 got %d", nLaunch)
			}

			raw1, _ := os.ReadFile(countPath)
			n1 := strings.Count(string(raw1), "\n")
			if n1 != 1 {
				t.Fatalf("p1 invocations want 1 got %d (%q)", n1, raw1)
			}

			// p2: fresh process — must exit 0, zero re-launch, claim closed.
			code2, out2 := run("p2")
			if code2 != 0 {
				t.Fatalf("p2 must exit 0 got %d\n%s", code2, out2)
			}
			raw2, _ := os.ReadFile(countPath)
			n2 := strings.Count(string(raw2), "\n")
			if n2 != n1 {
				t.Fatalf("p2 must not re-invoke: %d -> %d", n1, n2)
			}
			res2, err := os.ReadFile(resultPath)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(res2), "term="+term) &&
				!(term == "cancelled" && strings.Contains(string(res2), "term=cancelled")) {
				t.Fatalf("p2 must preserve terminal %s: %s", term, res2)
			}
			if !strings.Contains(string(res2), "launch=0") {
				t.Fatalf("p2 launch must be 0: %s", res2)
			}
			if !strings.Contains(string(res2), "n_term=1") {
				t.Fatalf("p2 must assert n_term=1: %s", res2)
			}
			if !strings.Contains(string(res2), "gen="+fmt.Sprintf("%d", gen)) {
				t.Fatalf("p2 must preserve generation %d: %s", gen, res2)
			}
			if !strings.Contains(string(res2), "attempt="+attempt) {
				t.Fatalf("p2 must preserve attempt %s: %s", attempt, res2)
			}

			// Claim closed after reconcile — parent re-reads durable store.
			csPath := filepath.Join(home, "projects", project, "runs", runID, "workclaims.json")
			rawClaim2, rerr := os.ReadFile(csPath)
			if rerr != nil {
				t.Fatalf("p2 claim store: %v", rerr)
			}
			if !strings.Contains(string(rawClaim2), `"state":"closed"`) && !strings.Contains(string(rawClaim2), `"state": "closed"`) {
				t.Fatalf("p2 claim must be closed after reconcile: %s", rawClaim2)
			}
			// Event count still exactly one terminal (no duplicate).
			elog2, _ := workflowrun.OpenEventLog(home, project, runID)
			evs2, _ := elog2.ReadAllForRun(project, runID)
			nTerm2 := 0
			for _, e := range evs2 {
				if strings.EqualFold(e.Kind, "terminal") {
					nTerm2++
				}
			}
			if nTerm2 != 1 {
				t.Fatalf("p2 n_term must stay 1 got %d", nTerm2)
			}
		})
	}
}

func runCrashHelper(t *testing.T) {
	t.Helper()
	phase := os.Getenv("LOOPCODER_CRASH_PHASE")
	termWant := os.Getenv("LOOPCODER_CRASH_TERM")
	countPath := os.Getenv("LOOPCODER_CRASH_COUNT")
	claimStatePath := os.Getenv("LOOPCODER_CRASH_CLAIM")
	resultPath := os.Getenv("LOOPCODER_CRASH_RESULT")
	exitCode := 91
	if v := os.Getenv("LOOPCODER_CRASH_EXIT"); v != "" {
		fmt.Sscanf(v, "%d", &exitCode)
	}

	home, err := workflowrun.ResolveDurableHome("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "home: %v\n", err)
		os.Exit(1)
	}
	runID := "run_crash_" + termWant
	project := "proj-crash-sub"

	writeClaimState := func(label string) {
		csPath := filepath.Join(home, "projects", project, "runs", runID, "workclaims.json")
		raw, _ := os.ReadFile(csPath)
		state := "missing"
		if len(raw) > 0 {
			if strings.Contains(string(raw), `"state": "closed"`) || strings.Contains(string(raw), `"state":"closed"`) {
				state = "closed"
			} else if strings.Contains(string(raw), `"state": "claimed"`) || strings.Contains(string(raw), `"state":"claimed"`) {
				state = "claimed"
			} else if strings.Contains(string(raw), `"state": "running"`) || strings.Contains(string(raw), `"state":"running"`) {
				state = "running"
			}
		}
		elog, _ := workflowrun.OpenEventLog(home, project, runID)
		nTerm := 0
		lastTerm := ""
		gen := 0
		attempt := ""
		if elog != nil {
			if evs, eerr := elog.ReadAllForRun(project, runID); eerr == nil {
				for _, e := range evs {
					if e.Kind == "terminal" {
						nTerm++
						lastTerm = e.Terminal
						gen = e.Generation
						attempt = e.AttemptID
					}
				}
			}
		}
		_ = os.WriteFile(claimStatePath, []byte(fmt.Sprintf(
			"%s state=%s terminal_event=%s n_term=%d gen=%d attempt=%s\n",
			label, state, lastTerm, nTerm, gen, attempt)), 0o600)
	}

	route := workflowrun.ChildRoute{
		Provider: "fixture", Model: "fixture-model", TaskClass: "tera", Depth: "medium",
		Permission: "bounded_write", AccountRef: "acct-f", WindowKind: "five_hour",
		ReservationID: "res-f", InstallRef: "install-f", RouteReason: "Winner: fixture/fixture-model crash-matrix",
	}

	if phase == "p1" {
		ex := testspawn.Executor{HomeDir: home, Now: t0, InvocationCountPath: countPath}
		switch termWant {
		case "failed":
			ex.FailIDs = map[string]bool{"only": true}
		case "cancelled":
			ex.CancelAfterIDs = map[string]bool{"only": true}
		}
		svc := workflowrun.Service{
			Now: t0, HomeDir: home, Executor: ex,
			TestCrashAfterTerminal: func(attemptID, terminal string) error {
				match := terminal == termWant ||
					(termWant == "cancelled" && terminal == string(workgraph.TermCancelled))
				if !match {
					return nil
				}
				// Persist claim/event snapshot for parent, then abrupt process death
				// BEFORE Close/defer cleanup (real crash window).
				writeClaimState("after_p1")
				os.Exit(exitCode)
				return fmt.Errorf("unreachable")
			},
		}
		_, _ = svc.Execute(context.Background(), withExpectedPlanDigest(t, workflowrun.Request{
			ProjectID: project, RunID: runID,
			Definition:  workflowrun.OneNodeDefinition("g-crash", "impl"),
			Actor:       "owner",
			ChildRoutes: map[string]workflowrun.ChildRoute{"only": route},
		}))
		// Must not reach here for matching terminal — if we do, fail closed.
		writeClaimState("after_p1_no_crash")
		os.Exit(1)
	}

	// p2: fresh process — reopen, reconcile, zero launch.
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: testspawn.Executor{
			HomeDir: home, Now: t0, InvocationCountPath: countPath,
		},
	}
	res, err := svc.Execute(context.Background(), withExpectedPlanDigest(t, workflowrun.Request{
		ProjectID: project, RunID: runID,
		Definition:  workflowrun.OneNodeDefinition("g-crash", "impl"),
		Actor:       "owner",
		ChildRoutes: map[string]workflowrun.ChildRoute{"only": route},
	}))
	writeClaimState("after_p2")

	// Exact non-vacuous assertions for all three terminal cases.
	elog, _ := workflowrun.OpenEventLog(home, project, runID)
	nTerm, nLaunch := 0, 0
	gen, attempt, evidence := 0, "", ""
	if elog != nil {
		if evs, eerr := elog.ReadAllForRun(project, runID); eerr == nil {
			for _, e := range evs {
				switch strings.ToLower(e.Kind) {
				case "terminal":
					nTerm++
					gen = e.Generation
					attempt = e.AttemptID
					evidence = e.Evidence
					if e.Terminal != termWant && !(termWant == "cancelled" && e.Terminal == string(workgraph.TermCancelled)) {
						t.Fatalf("event terminal=%s want %s", e.Terminal, termWant)
					}
				case "launch":
					nLaunch++
					// Succeeded capacity reuse requires full binding on launch payload.
					if termWant == "succeeded" {
						var m map[string]string
						if json.Unmarshal(e.Payload, &m) != nil {
							t.Fatalf("launch payload malformed")
						}
						for _, k := range []string{"provider", "model", "depth", "permission", "account_ref", "window_kind", "reservation_id"} {
							if strings.TrimSpace(m[k]) == "" {
								t.Fatalf("succeeded launch missing %s", k)
							}
						}
					}
				}
			}
		}
	}
	if nTerm != 1 {
		t.Fatalf("n_term want 1 got %d", nTerm)
	}
	if nLaunch != 1 {
		t.Fatalf("n_launch want 1 got %d", nLaunch)
	}
	if gen <= 0 || attempt == "" {
		t.Fatalf("gen/attempt required gen=%d attempt=%q", gen, attempt)
	}
	if termWant == "succeeded" && strings.TrimSpace(evidence) == "" {
		t.Fatalf("succeeded requires output evidence")
	}

	found := false
	for _, c := range res.Children {
		if c.WorkItemID != "only" {
			continue
		}
		found = true
		if c.Terminal != termWant && !(termWant == "cancelled" && c.Terminal == string(workgraph.TermCancelled)) {
			t.Fatalf("child terminal=%s want %s reuse=%d", c.Terminal, termWant, res.ReuseCount)
		}
	}
	if !found {
		t.Fatalf("child 'only' not found in p2 result children=%+v", res.Children)
	}
	if res.LaunchCount != 0 {
		t.Fatalf("p2 launch=%d want 0", res.LaunchCount)
	}

	cs, csErr := workclaim.OpenPath(filepath.Join(home, "projects", project, "runs", runID, "workclaims.json"), t0)
	if csErr != nil {
		t.Fatalf("open claim store: %v", csErr)
	}
	for _, c := range cs.AllClaims() {
		if c.State != workclaim.StateClosed {
			t.Fatalf("claim still open: %+v", c)
		}
		if string(c.Terminal) != termWant && !(termWant == "cancelled" && c.Terminal == workgraph.TermCancelled) {
			t.Fatalf("claim terminal=%s want %s", c.Terminal, termWant)
		}
		if int64(gen) != c.Generation {
			t.Fatalf("claim gen %d != event gen %d", c.Generation, gen)
		}
	}

	msg := fmt.Sprintf("err=%v status=%s launch=%d reuse=%d n_term=%d gen=%d attempt=%s evidence=%s",
		err, res.Status, res.LaunchCount, res.ReuseCount, nTerm, gen, attempt, evidence)
	for _, c := range res.Children {
		msg += fmt.Sprintf(" child=%s term=%s att=%s", c.WorkItemID, c.Terminal, c.AttemptID)
	}
	_ = os.WriteFile(resultPath, []byte(msg+"\n"), 0o600)
}
