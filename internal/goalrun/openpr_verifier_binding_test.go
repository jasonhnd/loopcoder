package goalrun

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

func fullSHA256Seed(seed string) string {
	const hex = "0123456789abcdef"
	b := "sha256:"
	for i := 0; i < 64; i++ {
		if i < len(seed) {
			b += string(hex[int(seed[i])%16])
		} else {
			b += string(hex[i%16])
		}
	}
	return b
}

func TestBindOpenPRVerifierFromChildren(t *testing.T) {
	verEvid := fullSHA256Seed("verify-ok")
	implEvid := fullSHA256Seed("impl-ok")
	goodVerify := workflowrun.ChildOutcome{
		WorkItemID: "wi_verify", TaskClass: "soul", Terminal: "succeeded",
		Provider: "codex", AttemptID: "att-v-1", OutputEvidence: verEvid,
	}
	goodImpl := workflowrun.ChildOutcome{
		WorkItemID: "wi_implement", TaskClass: "tera", Terminal: "succeeded",
		Provider: "grok", AttemptID: "att-i-1", OutputEvidence: implEvid,
	}

	t.Run("positive_exact", func(t *testing.T) {
		prov, evid, ok := bindOpenPRVerifierFromChildren([]workflowrun.ChildOutcome{goodImpl, goodVerify}, nil)
		if !ok {
			t.Fatal("want ok")
		}
		if prov != "codex" || evid != verEvid {
			t.Fatalf("prov=%q evid=%q", prov, evid)
		}
	})

	t.Run("wi_verify_notes_fails", func(t *testing.T) {
		notes := goodVerify
		notes.WorkItemID = "wi_verify_notes"
		_, _, ok := bindOpenPRVerifierFromChildren([]workflowrun.ChildOutcome{goodImpl, notes}, nil)
		if ok {
			t.Fatal("substring id must fail")
		}
	})

	t.Run("short_digest_fails", func(t *testing.T) {
		v := goodVerify
		v.OutputEvidence = "sha256:short"
		_, _, ok := bindOpenPRVerifierFromChildren([]workflowrun.ChildOutcome{goodImpl, v}, nil)
		if ok {
			t.Fatal("short digest must fail")
		}
	})

	t.Run("same_provider_case_insensitive_fails", func(t *testing.T) {
		v := goodVerify
		v.Provider = "GROK"
		_, _, ok := bindOpenPRVerifierFromChildren([]workflowrun.ChildOutcome{goodImpl, v}, nil)
		if ok {
			t.Fatal("same provider must fail")
		}
	})

	t.Run("duplicate_exact_verify_fails", func(t *testing.T) {
		v2 := goodVerify
		v2.AttemptID = "att-v-2"
		_, _, ok := bindOpenPRVerifierFromChildren([]workflowrun.ChildOutcome{goodImpl, goodVerify, v2}, nil)
		if ok {
			t.Fatal("duplicate verify must fail")
		}
	})

	t.Run("missing_verify_attempt_fails", func(t *testing.T) {
		v := goodVerify
		v.AttemptID = ""
		_, _, ok := bindOpenPRVerifierFromChildren([]workflowrun.ChildOutcome{goodImpl, v}, nil)
		if ok {
			t.Fatal("missing attempt must fail")
		}
	})

	t.Run("wrong_class_fails", func(t *testing.T) {
		v := goodVerify
		v.TaskClass = "tera"
		_, _, ok := bindOpenPRVerifierFromChildren([]workflowrun.ChildOutcome{goodImpl, v}, nil)
		if ok {
			t.Fatal("wrong class must fail")
		}
	})

	t.Run("whitespace_work_item_id_fails", func(t *testing.T) {
		v := goodVerify
		v.WorkItemID = " wi_verify"
		_, _, ok := bindOpenPRVerifierFromChildren([]workflowrun.ChildOutcome{goodImpl, v}, nil)
		if ok {
			t.Fatal("whitespace WorkItemID must fail")
		}
		v.WorkItemID = "wi_verify "
		_, _, ok = bindOpenPRVerifierFromChildren([]workflowrun.ChildOutcome{goodImpl, v}, nil)
		if ok {
			t.Fatal("trailing whitespace WorkItemID must fail")
		}
	})

	t.Run("invalid_exact_verify_alone_fails", func(t *testing.T) {
		// Exact wi_verify id but failed terminal — fail closed.
		v := goodVerify
		v.Terminal = "failed"
		_, _, ok := bindOpenPRVerifierFromChildren([]workflowrun.ChildOutcome{goodImpl, v}, nil)
		if ok {
			t.Fatal("failed exact verify must fail")
		}
	})

	// Sanity: forged pin strings are not inputs to the helper at all.
	_ = strings.Contains("sha256:forged-pin", "sha256")
}

func TestBindOpenPRVerifierFromChildren_ExactForcedInterruptRetry(t *testing.T) {
	const (
		plan = "sha256:11caa5995e91f476519af265"
		ccd  = "sha256:082d671819fd02259f3fae32d1e99214d25bdac24d7262873ded497c453df302"
	)
	route := workflowrun.ChildOutcome{
		WorkItemID: "wi_implement", TaskClass: "tera",
		Provider: "codex", Model: "gpt-5.3-codex-spark", Depth: "medium",
		Permission: "bounded_write", AccountRef: "acct-codex", InstallRef: "pinst-codex",
		ExecutionPlanDigest: plan, ChildContractDigest: ccd,
	}
	failed := route
	failed.AttemptID = "att-wi_implement-exact-g0"
	failed.Generation = 1
	failed.Terminal = "cancelled"
	failed.FailureClass = "forced_interrupt"
	failed.OutputEvidence = "failed:executor_error:wi_implement"
	retry := route
	retry.AttemptID = "att-wi_implement-exact-g1"
	retry.Generation = 2
	retry.Terminal = "succeeded"
	retry.OutputEvidence = fullSHA256Seed("retry-success")
	retry.IntegrateCommitSHA = strings.Repeat("a", 40)
	verify := workflowrun.ChildOutcome{
		WorkItemID: "wi_verify", TaskClass: "soul", Terminal: "succeeded",
		Provider: "claude", AttemptID: "att-wi_verify-exact-g0",
		OutputEvidence: fullSHA256Seed("verify-success"),
	}

	payload := func(child workflowrun.ChildOutcome, extra map[string]string) json.RawMessage {
		m := map[string]string{
			"work_item_id": child.WorkItemID, "attempt_id": child.AttemptID,
			"generation": strconv.Itoa(child.Generation),
			"task_class": child.TaskClass, "execution_plan_digest": child.ExecutionPlanDigest,
			"child_contract_digest": child.ChildContractDigest,
			"provider":              child.Provider, "model": child.Model, "depth": child.Depth,
			"permission": child.Permission, "account_ref": child.AccountRef,
			"install_ref": child.InstallRef,
		}
		for k, v := range extra {
			m[k] = v
		}
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	event := func(kind string, child workflowrun.ChildOutcome, extra map[string]string) workflowrun.Event {
		return workflowrun.Event{
			Schema: workflowrun.EventSchema, EventID: kind + "-" + child.AttemptID,
			ProjectID: "proj", RunID: "run", Kind: kind,
			WorkItemID: child.WorkItemID, AttemptID: child.AttemptID,
			Generation: child.Generation, TaskClass: child.TaskClass,
			ExecutionPlanDigest: child.ExecutionPlanDigest,
			ChildContractDigest: child.ChildContractDigest,
			Payload:             payload(child, extra),
		}
	}
	events := []workflowrun.Event{
		event("claim", failed, nil),
		event("launch", failed, nil),
		event("pid", failed, nil),
		event("interrupt", failed, map[string]string{
			"terminal": "cancelled", "failure_class": "forced_interrupt",
			"interrupt_class": workflowrun.InterruptClassServiceForced, "interrupt_id": "iint_exact",
		}),
		event("terminal", failed, map[string]string{
			"terminal": "cancelled", "failure_class": "forced_interrupt",
			"interrupt_class": workflowrun.InterruptClassServiceForced, "interrupt_id": "iint_exact",
		}),
		event("claim", retry, nil),
		event("launch", retry, nil),
		event("pid", retry, nil),
		event("integrate", retry, map[string]string{"commit_sha": retry.IntegrateCommitSHA}),
		event("terminal", retry, map[string]string{
			"terminal": "succeeded", "output_evidence": retry.OutputEvidence,
		}),
	}
	events[3].Terminal, events[3].FailureClass = "cancelled", "forced_interrupt"
	events[4].Terminal, events[4].FailureClass = "cancelled", "forced_interrupt"
	events[4].Evidence = failed.OutputEvidence
	events[8].CommitSHA = retry.IntegrateCommitSHA
	events[9].Terminal = "succeeded"
	events[9].Evidence = retry.OutputEvidence

	kids := []workflowrun.ChildOutcome{failed, retry, verify}
	if provider, evidence, ok := bindOpenPRVerifierFromChildren(kids, events); !ok {
		t.Fatal("exact forced-interrupt retry must bind")
	} else if provider != "claude" || evidence != verify.OutputEvidence {
		t.Fatalf("provider=%q evidence=%q", provider, evidence)
	}

	cloneEvents := func() []workflowrun.Event {
		out := append([]workflowrun.Event(nil), events...)
		for i := range out {
			out[i].Payload = append(json.RawMessage(nil), events[i].Payload...)
		}
		return out
	}
	mutatePayload := func(in []workflowrun.Event, index int, key, value string, remove bool) {
		var m map[string]string
		if err := json.Unmarshal(in[index].Payload, &m); err != nil {
			t.Fatal(err)
		}
		if remove {
			delete(m, key)
		} else {
			m[key] = value
		}
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		in[index].Payload = raw
	}
	tests := []struct {
		name   string
		mutate func(*[]workflowrun.ChildOutcome, *[]workflowrun.Event)
	}{
		{"missing_interrupt_class", func(_ *[]workflowrun.ChildOutcome, evs *[]workflowrun.Event) {
			mutatePayload(*evs, 3, "interrupt_class", "", true)
		}},
		{"padded_interrupt_class", func(_ *[]workflowrun.ChildOutcome, evs *[]workflowrun.Event) {
			mutatePayload(*evs, 3, "interrupt_class", " "+workflowrun.InterruptClassServiceForced, false)
		}},
		{"mismatched_interrupt_id", func(_ *[]workflowrun.ChildOutcome, evs *[]workflowrun.Event) {
			mutatePayload(*evs, 4, "interrupt_id", "iint_other", false)
		}},
		{"retry_route_mismatch", func(k *[]workflowrun.ChildOutcome, _ *[]workflowrun.Event) {
			(*k)[1].AccountRef = "acct-other"
		}},
		{"retry_generation_gap", func(k *[]workflowrun.ChildOutcome, _ *[]workflowrun.Event) {
			(*k)[1].Generation = 3
		}},
		{"unrelated_duplicate_implement", func(k *[]workflowrun.ChildOutcome, _ *[]workflowrun.Event) {
			extra := retry
			extra.AttemptID = "att-wi_implement-extra-g2"
			*k = append(*k, extra)
		}},
		{"missing_raw_events", func(_ *[]workflowrun.ChildOutcome, evs *[]workflowrun.Event) {
			*evs = nil
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotKids := append([]workflowrun.ChildOutcome(nil), kids...)
			gotEvents := cloneEvents()
			tc.mutate(&gotKids, &gotEvents)
			if _, _, ok := bindOpenPRVerifierFromChildren(gotKids, gotEvents); ok {
				t.Fatal("mutation must fail closed")
			}
		})
	}
}
