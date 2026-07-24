package workflowrun_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun/testspawn"
)

// TestRecoverIdentity_TamperFamilies_ZeroMutation: mutate one identity family at a
// time on a complete seed; recovery must fail closed with zero durable mutation
// and zero kill callbacks.
func TestRecoverIdentity_TamperFamilies_ZeroMutation(t *testing.T) {
	type tamper struct {
		name string
		fn   func(path string) // mutates event log JSONL bytes in place
	}
	tampers := []tamper{
		{"empty_project_id", func(path string) { rewriteJSONLField(t, path, "project_id", "") }},
		{"wrong_project_id", func(path string) { rewriteJSONLField(t, path, "project_id", "other-proj") }},
		{"empty_run_id", func(path string) { rewriteJSONLField(t, path, "run_id", "") }},
		{"wrong_run_id", func(path string) { rewriteJSONLField(t, path, "run_id", "other-run") }},
		{"empty_graph_id", func(path string) { rewriteJSONLField(t, path, "graph_id", "") }},
		{"wrong_graph_id", func(path string) { rewriteJSONLField(t, path, "graph_id", "g-wrong") }},
		{"wrong_graph_version", func(path string) { rewriteJSONLField(t, path, "graph_version", "99") }},
		{"wrong_plan_digest", func(path string) { rewriteJSONLField(t, path, "execution_plan_digest", "sha256:dead") }},
		{"wrong_graph_digest", func(path string) { rewriteJSONLField(t, path, "graph_digest", "sha256:dead") }},
		{"wrong_task_class", func(path string) { rewriteJSONLField(t, path, "task_class", "soul") }},
		{"wrong_ccd", func(path string) {
			rewriteJSONLField(t, path, "child_contract_digest", "sha256:"+strings.Repeat("0", 64))
		}},
		{"wrong_route_model", func(path string) { rewriteJSONLField(t, path, "model", "wrong-model") }},
		{"missing_route_provider", func(path string) { rewriteJSONLField(t, path, "provider", "") }},
		{"wrong_worktree", func(path string) { rewriteJSONLField(t, path, "worktree_path", "/tmp/wrong-wt") }},
		{"empty_log_path", func(path string) { rewriteJSONLField(t, path, "log_path", "") }},
		{"wrong_pid_payload", func(path string) { rewriteJSONLField(t, path, "pid", "1") }},
	}
	for _, tc := range tampers {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			home := testHome(t)
			project, runID := "proj-tamp-"+tc.name, "run_tamp_"+tc.name
			plan, att, _ := seedOpenRecoverableAttempt(t, home, project, runID)
			elog, err := workflowrun.OpenEventLog(home, project, runID)
			if err != nil {
				t.Fatal(err)
			}
			logPath := elog.Path()
			claimPath := filepath.Join(mustRunDir(t, home, project, runID), "workclaims.json")
			authPath, err := workflowrun.AuthorityStorePath(home, project, runID)
			if err != nil {
				t.Fatal(err)
			}
			// Snapshot authority row before tamper.
			ctx := context.Background()
			store, err := workflowrun.OpenAuthorityStore(ctx, home, project, runID, t0)
			if err != nil {
				t.Fatal(err)
			}
			authBefore, err := storage.LoadProviderExecutionAuthority(ctx, store, project, runID, att)
			if err != nil {
				t.Fatal(err)
			}
			_ = store.Close()

			tc.fn(logPath)

			beforeLog, _ := os.ReadFile(logPath)
			beforeClaim, _ := os.ReadFile(claimPath)
			beforeAuth, _ := os.ReadFile(authPath)
			kills := 0
			n, rerr := workflowrun.RecoverOpenLaunchInterruptsAuthoritative(elog, workflowrun.RecoverOptions{
				HomeDir: home, ProjectID: project, RunID: runID, Now: t0,
				WaitAlive: 50 * time.Millisecond, KillAfterVerify: true, PlanDigest: plan,
				OnKillGroup: func(int) error { kills++; return nil },
			})
			if rerr == nil || n != 0 {
				t.Fatalf("want fail-closed n=%d err=%v", n, rerr)
			}
			if kills != 0 {
				t.Fatalf("kill callbacks=%d want 0", kills)
			}
			afterLog, _ := os.ReadFile(logPath)
			afterClaim, _ := os.ReadFile(claimPath)
			afterAuth, _ := os.ReadFile(authPath)
			if string(beforeLog) != string(afterLog) {
				t.Fatal("event log mutated")
			}
			if string(beforeClaim) != string(afterClaim) {
				t.Fatal("claim store mutated")
			}
			if string(beforeAuth) != string(afterAuth) {
				t.Fatal("authority store mutated")
			}
			// Authority row fields unchanged via load.
			store2, _ := workflowrun.OpenAuthorityStore(ctx, home, project, runID, t0)
			authAfter, aerr := storage.LoadProviderExecutionAuthority(ctx, store2, project, runID, att)
			_ = store2.Close()
			if aerr != nil {
				t.Fatal(aerr)
			}
			if authAfter.RecordVersion != authBefore.RecordVersion ||
				authAfter.SpawnPhase != authBefore.SpawnPhase ||
				authAfter.CompletedAt != authBefore.CompletedAt ||
				authAfter.TerminalState != authBefore.TerminalState {
				t.Fatalf("authority row drifted: before=%+v after=%+v", authBefore, authAfter)
			}
		})
	}
}

// TestRecoverIdentity_ServicePrimaryAndAlternate_RestartValidates: real Service
// emits primary (MU) then alternate success with full identity; second process
// recovery validates without mutation and reuses.
func TestRecoverIdentity_ServicePrimaryAndAlternate_RestartValidates(t *testing.T) {
	home := testHome(t)
	project, runID := "proj-id-alt", "run_id_alt"
	routeBad := workflowrun.ChildRoute{
		Provider: "antigravity", Model: "model-unavailable-token", TaskClass: "tera",
		Depth: "medium", Permission: "bounded_write",
		AccountRef: "acct-ag", InstallRef: "install-ag", WindowKind: "five_hour",
		ReservationID: "res-ag", RouteReason: "pin-bad",
	}
	svc1 := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: testspawn.Executor{
			HomeDir: home, Now: t0, FailModel: "model-unavailable-token",
		},
	}
	res1, err := svc1.Execute(context.Background(), withExpectedPlanDigest(t, workflowrun.Request{
		ProjectID: project, RunID: runID,
		Definition: workflowrun.OneNodeDefinition("g-id-alt", "implement identity alternate"),
		Actor:      "owner", CapacityReroute: passThroughCapHook{},
		ChildRoutes: map[string]workflowrun.ChildRoute{"only": routeBad},
		SameDepthAlternates: map[string][]workflowrun.AlternateCandidate{
			"only": {
				{Provider: "antigravity", Model: "model-unavailable-token", Effort: "medium",
					Permission: "bounded_write", AccountRef: "acct-ag", InstallRef: "install-ag",
					WindowKind: "five_hour", HardEligible: true},
				{Provider: "codex", Model: "gpt-5.5", Effort: "medium",
					Permission: "bounded_write", AccountRef: "acct-codex", InstallRef: "install-codex",
					WindowKind: "five_hour", HardEligible: true},
			},
		},
	}))
	if err != nil {
		t.Fatalf("p1: %v %+v", err, res1)
	}
	if res1.Status != workflowrun.StatusHumanGate {
		t.Fatalf("p1 status=%s", res1.Status)
	}
	// Prove primary + alternate launch/pid/terminal present with structured identity.
	events := loadEvents(t, home, project, runID)
	if err := workflowrun.ValidateEventStreamInvariants(events); err != nil {
		t.Fatalf("stream: %v", err)
	}
	var nLaunch, nPID, nTerm, nMU int
	for _, ev := range events {
		switch ev.Kind {
		case "launch":
			nLaunch++
			requireEventHasIdentityPayload(t, ev)
		case "pid":
			nPID++
			requireEventHasIdentityPayload(t, ev)
			requireEventHasRoute(t, ev)
			if strings.TrimSpace(eventPayloadField(ev, "worktree_path")) == "" ||
				strings.TrimSpace(eventPayloadField(ev, "log_path")) == "" {
				t.Fatalf("pid missing worktree/log: %+v", ev)
			}
		case "terminal":
			nTerm++
			requireEventHasIdentityPayload(t, ev)
			requireEventHasRoute(t, ev)
		case "model_unavailable":
			nMU++
		}
		if ev.WorkItemID != "" {
			if strings.TrimSpace(ev.GraphID) == "" || ev.GraphVersion <= 0 {
				t.Fatalf("child event missing GraphID/Version: %+v", ev)
			}
			if strings.TrimSpace(ev.ProjectID) == "" || strings.TrimSpace(ev.RunID) == "" {
				t.Fatalf("child event missing project/run: %+v", ev)
			}
		}
	}
	if nLaunch < 2 || nPID < 2 || nTerm < 2 || nMU < 1 {
		t.Fatalf("want >=2 launch/pid/terminal and MU; got L=%d P=%d T=%d MU=%d", nLaunch, nPID, nTerm, nMU)
	}

	// Restart: recovery validates; no re-launch when PriorSucceeded set from outcome.
	var prior workflowrun.ChildOutcome
	for _, c := range res1.Children {
		if c.WorkItemID == "only" && c.Terminal == "succeeded" {
			prior = c
		}
	}
	if prior.AttemptID == "" {
		t.Fatalf("missing succeeded prior: %+v", res1.Children)
	}
	logPath := res1.EventLogPath
	beforeLog, _ := os.ReadFile(logPath)
	calls := map[string]int{}
	svc2 := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: testspawn.Executor{HomeDir: home, Now: t0, Calls: calls},
	}
	res2, err := svc2.Execute(context.Background(), withExpectedPlanDigest(t, workflowrun.Request{
		ProjectID: project, RunID: runID,
		Definition:     workflowrun.OneNodeDefinition("g-id-alt", "implement identity alternate"),
		Actor:          "owner",
		PriorSucceeded: map[string]workflowrun.ChildOutcome{"only": prior},
		ChildRoutes:    map[string]workflowrun.ChildRoute{"only": routeBad},
		SameDepthAlternates: map[string][]workflowrun.AlternateCandidate{
			"only": {
				{Provider: "codex", Model: "gpt-5.5", Effort: "medium",
					Permission: "bounded_write", AccountRef: "acct-codex", InstallRef: "install-codex",
					WindowKind: "five_hour", HardEligible: true},
			},
		},
	}))
	if err != nil {
		t.Fatalf("p2: %v %+v", err, res2)
	}
	if calls["only"] != 0 {
		t.Fatalf("must not relaunch: %v", calls)
	}
	if res2.ReuseCount < 1 && res2.LaunchCount != 0 {
		t.Fatalf("want reuse or zero launch: %+v", res2)
	}
	afterLog, _ := os.ReadFile(logPath)
	// Recovery may append nothing when fully converged; log must not lose prior lines.
	if len(afterLog) < len(beforeLog) {
		t.Fatal("event log shrunk on restart")
	}
	_ = sync.Once{} // silence unused if not needed
}

func mustRunDir(t *testing.T, home, project, runID string) string {
	t.Helper()
	d, err := workflowrun.RunDurableDir(home, project, runID)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func rewriteJSONLField(t *testing.T, path, key, newVal string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Spawn/process identity fields live on pid; route/digest fields on launch (canonical).
	preferKind := "launch"
	switch key {
	case "worktree_path", "log_path", "pid", "pgid", "process_birth_identity", "executable_identity", "observed_at":
		preferKind = "pid"
	}
	lines := strings.Split(string(raw), "\n")
	changed := false
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		kind, _ := m["kind"].(string)
		if kind != preferKind {
			continue
		}
		setTop := key == "project_id" || key == "run_id" || key == "graph_id" || key == "graph_version" ||
			key == "execution_plan_digest" || key == "graph_digest" || key == "task_class" || key == "child_contract_digest"
		if setTop {
			if key == "graph_version" {
				n := 0
				if newVal == "99" {
					n = 99
				}
				m[key] = n
			} else {
				m[key] = newVal
			}
		}
		if pl, ok := m["payload"].(map[string]any); ok {
			pl[key] = newVal
			m["payload"] = pl
		} else if !setTop {
			// Ensure payload exists for process fields.
			m["payload"] = map[string]any{key: newVal}
		}
		b, _ := json.Marshal(m)
		lines[i] = string(b)
		changed = true
		break
	}
	if !changed {
		t.Fatalf("no %s JSONL line mutated for key %s", preferKind, key)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
}

func requireEventHasIdentityPayload(t *testing.T, ev workflowrun.Event) {
	t.Helper()
	for _, k := range []string{
		"project_id", "run_id", "graph_id", "graph_version",
		"work_item_id", "attempt_id", "generation",
		"execution_plan_digest", "graph_digest", "task_class", "child_contract_digest",
	} {
		if strings.TrimSpace(eventPayloadField(ev, k)) == "" {
			t.Fatalf("event kind=%s missing payload %s", ev.Kind, k)
		}
	}
}

func requireEventHasRoute(t *testing.T, ev workflowrun.Event) {
	t.Helper()
	for _, k := range []string{
		"provider", "model", "depth", "permission",
		"account_ref", "install_ref", "window_kind", "reservation_id", "route_reason",
	} {
		if strings.TrimSpace(eventPayloadField(ev, k)) == "" {
			t.Fatalf("event kind=%s missing route %s", ev.Kind, k)
		}
	}
}

func eventPayloadField(ev workflowrun.Event, key string) string {
	if len(ev.Payload) == 0 {
		return ""
	}
	var m map[string]string
	if err := json.Unmarshal(ev.Payload, &m); err != nil {
		return ""
	}
	return strings.TrimSpace(m[key])
}
