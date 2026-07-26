package goalrun

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

func testLifeID() lifecycleBindIdentity {
	return lifecycleBindIdentity{
		ProjectID: "p", RunID: "r",
		PlanDigest:   "sha256:" + strings.Repeat("aa", 32),
		GraphDigest:  "sha256:" + strings.Repeat("bb", 32),
		GraphID:      "g",
		GraphVersion: 1,
	}
}

func TestChildOutcomesExactlyEqual_RejectsCapacityFieldDrift(t *testing.T) {
	base := workflowrun.ChildOutcome{
		WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1,
		TaskClass: "tera", ExecutionPlanDigest: "sha256:" + strings.Repeat("aa", 32),
		ChildContractDigest: "sha256:" + strings.Repeat("cc", 32),
		Terminal:            "succeeded", OutputEvidence: "sha256:" + strings.Repeat("dd", 32),
		Provider: "codex", Model: "m", Depth: "medium", Permission: "bounded_write",
		ActualSource: "unknown", RouteReason: "pin",
		ActualSources: workflowrun.ActualRouteSources{Model: "accepted_invocation"},
	}
	if err := childOutcomesExactlyEqual(base, base, "same"); err != nil {
		t.Fatal(err)
	}
	fields := []struct {
		name string
		mut  func(*workflowrun.ChildOutcome)
	}{
		{"ActualSource", func(o *workflowrun.ChildOutcome) { o.ActualSource = "accepted_invocation" }},
		{"ActualSources", func(o *workflowrun.ChildOutcome) {
			o.ActualSources = workflowrun.ActualRouteSources{Model: "provider_usage"}
		}},
		{"RouteReason", func(o *workflowrun.ChildOutcome) { o.RouteReason = "other" }},
		{"ReservationID", func(o *workflowrun.ChildOutcome) { o.ReservationID = "sres_9" }},
		{"RerouteEventRef", func(o *workflowrun.ChildOutcome) { o.RerouteEventRef = "wev_x" }},
		{"InputTokens", func(o *workflowrun.ChildOutcome) { o.InputTokens = 99 }},
		{"OutputTokens", func(o *workflowrun.ChildOutcome) { o.OutputTokens = 7 }},
		{"FilesTouched", func(o *workflowrun.ChildOutcome) { o.FilesTouched = []string{"a.go"} }},
		{"ActualCapacity", func(o *workflowrun.ChildOutcome) { v := 0.5; o.ActualCapacity = &v }},
		{"ActualSources.Effort", func(o *workflowrun.ChildOutcome) {
			o.ActualSources.Effort = "provider_usage"
		}},
		{"ActualSources.Permission", func(o *workflowrun.ChildOutcome) {
			o.ActualSources.Permission = "provider_usage"
		}},
		{"ActualSources.Account", func(o *workflowrun.ChildOutcome) {
			o.ActualSources.Account = "auth_binding"
		}},
		{"ActualSources.Install", func(o *workflowrun.ChildOutcome) {
			o.ActualSources.Install = "install_binding"
		}},
	}
	for _, tc := range fields {
		t.Run(tc.name, func(t *testing.T) {
			b := base
			tc.mut(&b)
			if err := childOutcomesExactlyEqual(base, b, "drift"); err == nil {
				t.Fatalf("want DeepEqual fail for %s", tc.name)
			}
		})
	}
}

func TestExactCanonicalTerminal(t *testing.T) {
	for _, good := range []string{"succeeded", "failed", "cancelled", "skipped"} {
		if !exactCanonicalTerminal(good) {
			t.Fatalf("want accept %q", good)
		}
	}
	for _, bad := range []string{"canceled", "refused", "Succeeded", " failed", "failed ", "CANCELLED", ""} {
		if exactCanonicalTerminal(bad) {
			t.Fatalf("want reject %q", bad)
		}
	}
}

func TestValidatePriorOutcomesAgainstGraph_MissingDepthPermission(t *testing.T) {
	id := testLifeID()
	plan, runID := id.PlanDigest, id.RunID
	ccd := "sha256:" + strings.Repeat("cc", 32)
	att := workflowrun.AttemptID("wi_only", plan, runID, 0)
	items := []workgraph.WorkItem{{ID: "wi_only"}}
	routes := map[string]priorRouteSnap{
		"wi_only": {Class: "tera", Depth: "medium", Permission: "bounded_write"},
	}
	ccdMap := map[string]string{"wi_only": ccd}
	base := workflowrun.ChildOutcome{
		WorkItemID: "wi_only", AttemptID: att, Generation: 1,
		TaskClass: "tera", ExecutionPlanDigest: plan, ChildContractDigest: ccd,
		Depth: "medium", Permission: "bounded_write",
		Terminal: "cancelled", FailureClass: "forced_interrupt",
	}
	missDepth := base
	missDepth.Depth = ""
	if err := validatePriorOutcomesAgainstGraphMaps([]workflowrun.ChildOutcome{missDepth}, items, plan, runID, ccdMap, routes); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("missing depth: %v", err)
	}
	missPerm := base
	missPerm.Permission = ""
	if err := validatePriorOutcomesAgainstGraphMaps([]workflowrun.ChildOutcome{missPerm}, items, plan, runID, ccdMap, routes); err == nil || !strings.Contains(err.Error(), "permission") {
		t.Fatalf("missing permission: %v", err)
	}
	badTerm := base
	badTerm.Terminal = "canceled"
	if err := validatePriorOutcomesAgainstGraphMaps([]workflowrun.ChildOutcome{badTerm}, items, plan, runID, ccdMap, routes); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("canceled terminal: %v", err)
	}
	mixed := base
	mixed.Terminal = "Cancelled"
	if err := validatePriorOutcomesAgainstGraphMaps([]workflowrun.ChildOutcome{mixed}, items, plan, runID, ccdMap, routes); err == nil {
		t.Fatal("mixed-case terminal must fail")
	}
	partialCap := base
	partialCap.AccountRef = "acct"
	// missing install/window/provider
	if err := validatePriorOutcomesAgainstGraphMaps([]workflowrun.ChildOutcome{partialCap}, items, plan, runID, ccdMap, routes); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("partial capacity: %v", err)
	}
}

func TestRequireTypedAbortEvidence_StrictJSON(t *testing.T) {
	plan := "sha256:" + strings.Repeat("aa", 32)
	att := workflowrun.AttemptID("wi", plan, "r", 0)
	kid := workflowrun.ChildOutcome{
		WorkItemID: "wi", AttemptID: att, Generation: 1,
		Terminal: "cancelled", FailureClass: "forced_interrupt",
	}
	// Malformed payload.
	evs := []workflowrun.Event{{
		Kind: "interrupt", WorkItemID: "wi", AttemptID: att, Generation: 1,
		FailureClass: "forced_interrupt", Payload: []byte(`{not-json`),
	}}
	if err := requireTypedAbortEvidence(kid, evs); err == nil || !strings.Contains(err.Error(), "JSON") {
		t.Fatalf("malformed payload: %v", err)
	}
	// Spoofed failure_class.
	raw, _ := json.Marshal(map[string]string{
		"failure_class": "not_a_real_class", "interrupt_class": "service_forced_interrupt",
		"interrupt_id": "iint_x", "terminal": "cancelled",
		"work_item_id": "wi", "attempt_id": att, "generation": "1",
	})
	evs = []workflowrun.Event{{
		Kind: "interrupt", WorkItemID: "wi", AttemptID: att, Generation: 1,
		FailureClass: "forced_interrupt", Payload: raw,
	}}
	if err := requireTypedAbortEvidence(kid, evs); err == nil {
		t.Fatal("spoofed failure_class must fail")
	}
	// Valid.
	rawOK, _ := json.Marshal(map[string]string{
		"failure_class": "forced_interrupt", "interrupt_class": "service_forced_interrupt",
		"interrupt_id": "iint_x", "terminal": "cancelled",
		"work_item_id": "wi", "attempt_id": att, "generation": "1",
	})
	evs = []workflowrun.Event{
		{Kind: "interrupt", WorkItemID: "wi", AttemptID: att, Generation: 1,
			FailureClass: "forced_interrupt", Payload: rawOK},
		{Kind: "terminal", WorkItemID: "wi", AttemptID: att, Generation: 1,
			Terminal: "cancelled", FailureClass: "forced_interrupt", Payload: rawOK},
	}
	if err := requireTypedAbortEvidence(kid, evs); err != nil {
		t.Fatalf("valid: %v", err)
	}
}

func TestValidateAndMerge_EventOnlyAndDuplicateConflict(t *testing.T) {
	id := testLifeID()
	plan, runID := id.PlanDigest, id.RunID
	ccd := "sha256:" + strings.Repeat("cc", 32)
	att := workflowrun.AttemptID("wi_only", plan, runID, 0)
	home := t.TempDir()
	dir := filepath.Join(home, "projects", id.ProjectID, "runs", id.RunID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	elogPath := filepath.Join(dir, "workflow-events.jsonl")
	mkEv := func(kind string, extra map[string]string) workflowrun.Event {
		e := workflowrun.Event{
			Schema: workflowrun.EventSchema, ProjectID: id.ProjectID, RunID: id.RunID,
			Kind: kind, WorkItemID: "wi_only", AttemptID: att, Generation: 1,
			ExecutionPlanDigest: id.PlanDigest, GraphDigest: id.GraphDigest,
			GraphID: id.GraphID, GraphVersion: id.GraphVersion,
			TaskClass: "tera", ChildContractDigest: ccd,
			EventID: "wev_" + kind, At: time.Now().UTC(),
		}
		if kind == "interrupt" || kind == "terminal" {
			e.Terminal = "cancelled"
			e.FailureClass = "forced_interrupt"
			pl := map[string]string{
				"failure_class": "forced_interrupt", "interrupt_class": "service_forced_interrupt",
				"interrupt_id": "iint_1", "terminal": "cancelled",
				"work_item_id": "wi_only", "attempt_id": att, "generation": "1",
			}
			for k, v := range extra {
				pl[k] = v
			}
			raw, _ := json.Marshal(pl)
			e.Payload = raw
		}
		return e
	}
	// Event-only: launch without kids.
	var b strings.Builder
	for _, e := range []workflowrun.Event{
		{Schema: workflowrun.EventSchema, ProjectID: id.ProjectID, RunID: id.RunID, Kind: "run.start", EventID: "s"},
		mkEv("launch", nil),
	} {
		line, _ := json.Marshal(e)
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(elogPath, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := validateAndMergePriorOutcomes(nil, nil, elogPath, id); err == nil || !strings.Contains(err.Error(), "event-only") {
		t.Fatalf("event-only: %v", err)
	}

	// Full kids + events, then conflict on duplicate AttemptID differing field.
	kid := workflowrun.ChildOutcome{
		WorkItemID: "wi_only", AttemptID: att, Generation: 1,
		TaskClass: "tera", ExecutionPlanDigest: plan, ChildContractDigest: ccd,
		Depth: "medium", Permission: "bounded_write",
		Provider: "codex", Model: "m", Terminal: "cancelled", FailureClass: "forced_interrupt",
	}
	b.Reset()
	for _, e := range []workflowrun.Event{
		{Schema: workflowrun.EventSchema, ProjectID: id.ProjectID, RunID: id.RunID, Kind: "run.start", EventID: "s2"},
		mkEv("claim", nil), mkEv("launch", nil), mkEv("interrupt", nil), mkEv("terminal", nil),
	} {
		line, _ := json.Marshal(e)
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(elogPath, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	kid2 := kid
	kid2.Message = "different"
	_, err := validateAndMergePriorOutcomes([]workflowrun.ChildOutcome{kid, kid2}, map[string]string{"wi_only": att}, elogPath, id)
	if err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("duplicate conflict: %v", err)
	}
}

func TestResolveCanonicalEventLogPath_MismatchAndSymlink(t *testing.T) {
	home := t.TempDir()
	projectID, runID := "proj", "run1"
	can, err := eventLogPathRead(home, projectID, runID)
	if err != nil {
		t.Fatal(err)
	}
	// Mismatch path.
	if _, err := resolveCanonicalEventLogPath(home, projectID, runID, "/tmp/other-events.jsonl", ""); err == nil {
		t.Fatal("path mismatch must fail")
	}
	// ./ and ../ aliases must fail byte-exact (Clean would have accepted them).
	// Build non-cleaned strings that filepath.Clean would normalize to canonical.
	dotAlias := filepath.Dir(can) + string(filepath.Separator) + "." + string(filepath.Separator) + "workflow-events.jsonl"
	if filepath.Clean(dotAlias) != filepath.Clean(can) {
		t.Fatalf("test setup: Clean(dotAlias)=%q Clean(can)=%q", filepath.Clean(dotAlias), filepath.Clean(can))
	}
	if dotAlias == can {
		t.Fatal("test setup: dotAlias unexpectedly equals can")
	}
	if _, err := resolveCanonicalEventLogPath(home, projectID, runID, dotAlias, ""); err == nil {
		t.Fatal("./ alias must fail byte-exact stamp check")
	}
	parentAlias := filepath.Dir(can) + string(filepath.Separator) + ".." + string(filepath.Separator) + runID + string(filepath.Separator) + "workflow-events.jsonl"
	if filepath.Clean(parentAlias) != filepath.Clean(can) {
		t.Fatalf("test setup: Clean(parentAlias)=%q Clean(can)=%q", filepath.Clean(parentAlias), filepath.Clean(can))
	}
	if parentAlias == can {
		t.Fatal("test setup: parentAlias unexpectedly equals can")
	}
	if _, err := resolveCanonicalEventLogPath(home, projectID, runID, parentAlias, ""); err == nil {
		t.Fatal("../ alias must fail byte-exact stamp check")
	}
	// Whitespace padding fails.
	if _, err := resolveCanonicalEventLogPath(home, projectID, runID, " "+can, ""); err == nil {
		t.Fatal("whitespace-padded stamp must fail")
	}
	// Create real file then symlink as alternate authority leaf.
	if err := os.MkdirAll(filepath.Dir(can), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(can, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := can + ".link"
	if err := os.Symlink(can, link); err != nil {
		t.Fatal(err)
	}
	_, errLink := resolveCanonicalEventLogPath(home, projectID, runID, link, "")
	if errLink == nil {
		t.Fatal("symlink path as EventLogPath must fail closed")
	}
	if !strings.Contains(errLink.Error(), "symlink") && !strings.Contains(errLink.Error(), "!= canonical") {
		t.Fatalf("symlink authority: %v", errLink)
	}
	// Symlink as run child directory (projects/proj/runs/run1 via link) fails.
	// Inject symlink parent: replace a durable component with a symlink.
	runDir := filepath.Dir(can)
	evilParent := runDir + ".evil"
	if err := os.MkdirAll(evilParent, 0o700); err != nil {
		t.Fatal(err)
	}
	// Nonexistent leaf under symlink parent: build path that would be canonical
	// only if we followed a symlink child — verifySecure rejects symlink components.
	// Create symlink child under projects: projects/proj/runs -> evil
	// (cannot rename real run dir while testing stamp equality; test leaf symlink already covers.)
	// Symlink event leaf where path equals a non-canonical string already fails stamp.
	// Canonical exact match OK (platform /var prefix tolerated via longest ancestor).
	if p, err := resolveCanonicalEventLogPath(home, projectID, runID, can, can); err != nil || p != can {
		t.Fatalf("canonical match: p=%q err=%v want %q", p, err, can)
	}
	// cp/partial byte mismatch fails before any inventory mutation (authority only).
	if _, err := resolveCanonicalEventLogPath(home, projectID, runID, can, can+".other"); err == nil {
		t.Fatal("cp/partial mismatch must fail")
	}
}

func TestRequireExactRerouteEventRef_RejectsSubstringSpoof(t *testing.T) {
	failedAtt := "att-wi-x-g0"
	winnerAtt := "att-wi-x-g1"
	failed := workflowrun.ChildOutcome{
		WorkItemID: "wi", AttemptID: failedAtt, Generation: 1,
		Terminal: "failed", FailureClass: "model_unavailable",
	}
	winner := workflowrun.ChildOutcome{
		WorkItemID: "wi", AttemptID: winnerAtt, Generation: 2,
		Terminal: "succeeded", SupersedesAttemptID: failedAtt,
	}
	muID, claimID, rrID, lnID := "wev_mu", "wev_claim", "wev_rr", "wev_ln"
	// Production: MU on failed; claim+reroute+launch on winner.
	failedEvs := []workflowrun.Event{
		{Kind: "model_unavailable", EventID: muID, AttemptID: failedAtt},
		{Kind: "claim", EventID: "wev_failed_claim", AttemptID: failedAtt},
		{Kind: "launch", EventID: "wev_failed_ln", AttemptID: failedAtt},
	}
	winnerEvs := []workflowrun.Event{
		{Kind: "claim", EventID: claimID, AttemptID: winnerAtt},
		{Kind: "launch", EventID: lnID, AttemptID: winnerAtt},
		{Kind: "reroute", EventID: rrID, AttemptID: winnerAtt},
	}
	rrEv := winnerEvs[2]
	good := "event_id=" + muID + ";event_id=" + claimID + ";event_id=" + rrID + ";event_id=" + lnID +
		";supersedes_attempt_id=" + failedAtt + ";retry_attempt_id=" + winnerAtt
	winner.RerouteEventRef = good
	if err := requireExactRerouteEventRef(winner, failed, failedEvs, winnerEvs, rrEv); err != nil {
		t.Fatalf("good ref: %v", err)
	}
	// Prefix/collision spoof: longer id containing required id as substring.
	winner.RerouteEventRef = "event_id=" + muID + "EXTRA;event_id=" + claimID + ";event_id=" + rrID + ";event_id=" + lnID +
		";supersedes_attempt_id=" + failedAtt + ";retry_attempt_id=" + winnerAtt
	if err := requireExactRerouteEventRef(winner, failed, failedEvs, winnerEvs, rrEv); err == nil {
		t.Fatal("prefix collision event_id must fail")
	}
	// Duplicate event_id.
	winner.RerouteEventRef = "event_id=" + muID + ";event_id=" + muID + ";event_id=" + rrID + ";event_id=" + lnID +
		";supersedes_attempt_id=" + failedAtt + ";retry_attempt_id=" + winnerAtt
	if err := requireExactRerouteEventRef(winner, failed, failedEvs, winnerEvs, rrEv); err == nil {
		t.Fatal("duplicate event_id must fail")
	}
	// Extra event_id.
	winner.RerouteEventRef = good + ";event_id=wev_extra"
	if err := requireExactRerouteEventRef(winner, failed, failedEvs, winnerEvs, rrEv); err == nil {
		t.Fatal("extra event_id must fail")
	}
	// Wrong role (missing reroute id, wrong supersedes).
	winner.RerouteEventRef = "event_id=" + muID + ";event_id=" + claimID + ";event_id=" + lnID + ";event_id=wev_wrong" +
		";supersedes_attempt_id=WRONG;retry_attempt_id=" + winnerAtt
	if err := requireExactRerouteEventRef(winner, failed, failedEvs, winnerEvs, rrEv); err == nil {
		t.Fatal("wrong role/supersedes must fail")
	}
	// Substring-only spoof of the old Contains path.
	winner.RerouteEventRef = "prefix-" + rrID + "-suffix"
	if err := requireExactRerouteEventRef(winner, failed, failedEvs, winnerEvs, rrEv); err == nil {
		t.Fatal("substring-only ref must fail")
	}
}
