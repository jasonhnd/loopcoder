package goalrun_test

import (
	"encoding/json"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/goalrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

// buildMinimalProofBase constructs a genuinely valid nonnil proof fixture.
func buildMinimalProofBase() (goalrun.Result, []workflowrun.Event) {
	failedAtt := "att-only-abc-g0"
	retryAtt := "att-only-abc-g1"
	wi := "only"
	payload := func(m map[string]string) json.RawMessage {
		b, _ := json.Marshal(m)
		return b
	}
	events := []workflowrun.Event{
		{Schema: workflowrun.EventSchema, EventID: "e_mu", Kind: "model_unavailable", WorkItemID: wi, AttemptID: failedAtt,
			Terminal: "failed", Evidence: "ev-failed", Generation: 0,
			Payload: payload(map[string]string{
				"work_item_id": wi, "attempt_id": failedAtt, "provider": "antigravity", "model": "bad", "failure_class": "model_unavailable",
			})},
		{Schema: workflowrun.EventSchema, EventID: "e_ft", Kind: "terminal", WorkItemID: wi, AttemptID: failedAtt,
			Terminal: "failed", Evidence: "ev-failed", Generation: 0,
			Payload: payload(map[string]string{
				"work_item_id": wi, "attempt_id": failedAtt, "terminal": "failed", "output_evidence": "ev-failed", "failure_class": "model_unavailable",
			})},
		{Schema: workflowrun.EventSchema, EventID: "e_cl0", Kind: "claim", WorkItemID: wi, AttemptID: failedAtt, Generation: 0,
			Payload: payload(map[string]string{"work_item_id": wi, "attempt_id": failedAtt})},
		{Schema: workflowrun.EventSchema, EventID: "e_ln0", Kind: "launch", WorkItemID: wi, AttemptID: failedAtt, Generation: 0,
			Payload: payload(map[string]string{
				"work_item_id": wi, "attempt_id": failedAtt, "provider": "antigravity", "model": "bad",
				"depth": "medium", "permission": "bounded_write", "account_ref": "acct-ag", "window_kind": "five_hour", "reservation_id": "res-p",
			})},
		{Schema: workflowrun.EventSchema, EventID: "e_claim", Kind: "claim", WorkItemID: wi, AttemptID: retryAtt, Generation: 1,
			Payload: payload(map[string]string{
				"work_item_id": wi, "attempt_id": retryAtt, "supersedes_attempt_id": failedAtt, "retry_attempt_id": retryAtt,
			})},
		{Schema: workflowrun.EventSchema, EventID: "e_rr", Kind: "reroute", WorkItemID: wi, AttemptID: retryAtt, Generation: 1,
			Payload: payload(map[string]string{
				"work_item_id": wi, "supersedes_attempt_id": failedAtt, "retry_attempt_id": retryAtt,
				"alt_provider": "codex", "alt_model": "gpt-5.5", "depth": "medium", "permission": "bounded_write",
				"account_ref": "acct-codex", "window_kind": "weekly", "reservation_id": "res-a",
				"model_unavailable_event_id": "e_mu", "claim_event_id": "e_claim",
			})},
		{Schema: workflowrun.EventSchema, EventID: "e_ln", Kind: "launch", WorkItemID: wi, AttemptID: retryAtt, Generation: 1,
			Payload: payload(map[string]string{
				"work_item_id": wi, "retry_attempt_id": retryAtt, "reroute_event_id": "e_rr",
				"supersedes_attempt_id": failedAtt, "provider": "codex", "model": "gpt-5.5",
				"depth": "medium", "permission": "bounded_write", "account_ref": "acct-codex",
				"window_kind": "weekly", "reservation_id": "res-a",
			})},
		{Schema: workflowrun.EventSchema, EventID: "e_tm", Kind: "terminal", WorkItemID: wi, AttemptID: retryAtt, Generation: 1,
			Terminal: "succeeded", Evidence: "ev-ok",
			Payload: payload(map[string]string{
				"work_item_id": wi, "retry_attempt_id": retryAtt, "supersedes_attempt_id": failedAtt,
				"terminal": "succeeded", "output_evidence": "ev-ok",
			})},
	}
	res := goalrun.Result{
		Workflow: workflowrun.Result{
			Children: []workflowrun.ChildOutcome{
				{WorkItemID: wi, AttemptID: failedAtt, FailureClass: "model_unavailable", Terminal: "failed",
					Provider: "antigravity", Model: "bad", Depth: "medium", Permission: "bounded_write",
					AccountRef: "acct-ag", WindowKind: "five_hour", ReservationID: "res-p",
					OutputEvidence: "ev-failed"},
				{WorkItemID: wi, AttemptID: retryAtt, SupersedesAttemptID: failedAtt, Terminal: "succeeded",
					Provider: "codex", Model: "gpt-5.5", Depth: "medium", Permission: "bounded_write",
					AccountRef: "acct-codex", WindowKind: "weekly", ReservationID: "res-a",
					OutputEvidence:  "ev-ok",
					RerouteEventRef: "event_id=e_mu;event_id=e_claim;event_id=e_rr;event_id=e_ln;supersedes_attempt_id=" + failedAtt + ";retry_attempt_id=" + retryAtt},
			},
			CapacityTransitions: []workflowrun.CapacityTransition{
				{AttemptID: failedAtt, Role: "prior", State: "released", Provider: "antigravity", Model: "bad", Depth: "medium",
					Permission: "bounded_write", AccountRef: "acct-ag", WindowKind: "five_hour", ReservationID: "res-p"},
				{AttemptID: retryAtt, Role: "alternate", State: "released", Provider: "codex", Model: "gpt-5.5", Depth: "medium",
					Permission: "bounded_write", AccountRef: "acct-codex", WindowKind: "weekly", ReservationID: "res-a"},
			},
		},
	}
	return res, events
}

func TestProofFromResult_BaselineNonNilThenEveryFieldMutationFails(t *testing.T) {
	base, events := buildMinimalProofBase()
	baseline := goalrun.ProofFromResultForTest(base, events)
	if baseline == nil {
		t.Fatal("baseline proof must be non-nil before mutations")
	}

	type mut struct {
		name string
		fn   func(goalrun.Result, []workflowrun.Event) (goalrun.Result, []workflowrun.Event)
	}
	mutatePayload := func(idx int, key, val string) func(goalrun.Result, []workflowrun.Event) (goalrun.Result, []workflowrun.Event) {
		return func(r goalrun.Result, evs []workflowrun.Event) (goalrun.Result, []workflowrun.Event) {
			out := append([]workflowrun.Event(nil), evs...)
			var m map[string]string
			_ = json.Unmarshal(out[idx].Payload, &m)
			m[key] = val
			b, _ := json.Marshal(m)
			out[idx].Payload = b
			return r, out
		}
	}
	mutateCap := func(roleIdx int, field, val string) func(goalrun.Result, []workflowrun.Event) (goalrun.Result, []workflowrun.Event) {
		return func(r goalrun.Result, evs []workflowrun.Event) (goalrun.Result, []workflowrun.Event) {
			r2 := r
			tr := append([]workflowrun.CapacityTransition(nil), r.Workflow.CapacityTransitions...)
			switch field {
			case "provider":
				tr[roleIdx].Provider = val
			case "model":
				tr[roleIdx].Model = val
			case "depth":
				tr[roleIdx].Depth = val
			case "permission":
				tr[roleIdx].Permission = val
			case "account":
				tr[roleIdx].AccountRef = val
			case "window":
				tr[roleIdx].WindowKind = val
			case "reservation":
				tr[roleIdx].ReservationID = val
			}
			r2.Workflow.CapacityTransitions = tr
			return r2, evs
		}
	}
	mutateChild := func(idx int, field, val string) func(goalrun.Result, []workflowrun.Event) (goalrun.Result, []workflowrun.Event) {
		return func(r goalrun.Result, evs []workflowrun.Event) (goalrun.Result, []workflowrun.Event) {
			r2 := r
			kids := append([]workflowrun.ChildOutcome(nil), r.Workflow.Children...)
			switch field {
			case "provider":
				kids[idx].Provider = val
			case "model":
				kids[idx].Model = val
			case "depth":
				kids[idx].Depth = val
			case "permission":
				kids[idx].Permission = val
			case "account":
				kids[idx].AccountRef = val
			case "window":
				kids[idx].WindowKind = val
			case "reservation":
				kids[idx].ReservationID = val
			}
			r2.Workflow.Children = kids
			return r2, evs
		}
	}
	mutations := []mut{
		{"empty MU provider", mutatePayload(0, "provider", "")},
		{"wrong MU provider", mutatePayload(0, "provider", "wrong")},
		{"empty failed terminal payload terminal", mutatePayload(1, "terminal", "")},
		{"wrong failed terminal payload terminal", mutatePayload(1, "terminal", "succeeded")},
		{"empty failed terminal evidence", mutatePayload(1, "output_evidence", "")},
		{"empty reroute alt_provider", mutatePayload(5, "alt_provider", "")},
		{"wrong reroute alt_provider", mutatePayload(5, "alt_provider", "wrong")},
		{"empty reroute depth", mutatePayload(5, "depth", "")},
		{"wrong reroute depth", mutatePayload(5, "depth", "high")},
		{"empty reroute permission", mutatePayload(5, "permission", "")},
		{"wrong reroute permission", mutatePayload(5, "permission", "read-only")},
		{"empty reroute account", mutatePayload(5, "account_ref", "")},
		{"wrong reroute account", mutatePayload(5, "account_ref", "wrong")},
		{"empty reroute window", mutatePayload(5, "window_kind", "")},
		{"wrong reroute window", mutatePayload(5, "window_kind", "five_hour")},
		{"empty reroute reservation", mutatePayload(5, "reservation_id", "")},
		{"wrong reroute reservation", mutatePayload(5, "reservation_id", "wrong")},
		{"empty launch provider", mutatePayload(6, "provider", "")},
		{"wrong launch model", mutatePayload(6, "model", "wrong")},
		{"empty launch depth", mutatePayload(6, "depth", "")},
		{"empty launch permission", mutatePayload(6, "permission", "")},
		{"wrong launch permission", mutatePayload(6, "permission", "read-only")},
		{"empty launch account", mutatePayload(6, "account_ref", "")},
		{"empty launch window", mutatePayload(6, "window_kind", "")},
		{"empty launch reservation", mutatePayload(6, "reservation_id", "")},
		{"empty prior provider", mutateCap(0, "provider", "")},
		{"wrong prior model", mutateCap(0, "model", "wrong")},
		{"empty prior depth", mutateCap(0, "depth", "")},
		{"empty prior account", mutateCap(0, "account", "")},
		{"empty prior window", mutateCap(0, "window", "")},
		{"empty prior reservation", mutateCap(0, "reservation", "")},
		{"empty alt provider", mutateCap(1, "provider", "")},
		{"empty alt model", mutateCap(1, "model", "")},
		{"empty alt depth", mutateCap(1, "depth", "")},
		{"empty alt account", mutateCap(1, "account", "")},
		{"empty alt window", mutateCap(1, "window", "")},
		{"empty alt reservation", mutateCap(1, "reservation", "")},
		{"wrong retry account", mutateChild(1, "account", "wrong")},
		{"wrong retry window", mutateChild(1, "window", "five_hour")},
		{"wrong retry reservation", mutateChild(1, "reservation", "wrong")},
		{"wrong retry permission", mutateChild(1, "permission", "read-only")},
		{"capacity len!=2", func(r goalrun.Result, evs []workflowrun.Event) (goalrun.Result, []workflowrun.Event) {
			r2 := r
			r2.Workflow.CapacityTransitions = r2.Workflow.CapacityTransitions[:1]
			return r2, evs
		}},
		{"wrong supersedes in ref", func(r goalrun.Result, evs []workflowrun.Event) (goalrun.Result, []workflowrun.Event) {
			r2 := r
			kids := append([]workflowrun.ChildOutcome(nil), r.Workflow.Children...)
			kids[1].RerouteEventRef = "event_id=e_mu;event_id=e_claim;event_id=e_rr;event_id=e_ln;supersedes_attempt_id=WRONG;retry_attempt_id=att-only-abc-g1"
			r2.Workflow.Children = kids
			return r2, evs
		}},
	}
	for _, m := range mutations {
		r, evs := m.fn(base, events)
		if got := goalrun.ProofFromResultForTest(r, evs); got != nil {
			t.Fatalf("mutation %q must make proof nil", m.name)
		}
	}
}
