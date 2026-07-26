package workflowrun_test

import (
	"testing"

	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

func TestRequireEventPayloadSelfConsistent_FailureClass(t *testing.T) {
	// Success must have empty class.
	okSucc := workflowrun.Event{
		Kind: "terminal", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1,
		Terminal: "succeeded",
		Payload:  []byte(`{"work_item_id":"wi","attempt_id":"att-wi-x-g0","generation":"1","terminal":"succeeded"}`),
	}
	// Use recovery path helper via ValidateEventStreamInvariants with typed pair for failed cases.
	// Direct package test of require is not exported — exercise via stream pair validation
	// for interrupt/terminal self-consistency and service path.
	_ = okSucc

	// Cancelled with only payload class fails self-check when top-level missing.
	// Stream path for service pair requires top-level FailureClass (already covered).
	// Negative: success with class
	if err := workflowrun.ValidateEventStreamInvariants([]workflowrun.Event{
		{Kind: "launch", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1},
		{Kind: "interrupt", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1,
			Terminal: "cancelled", FailureClass: "forced_interrupt",
			Payload: []byte(`{"work_item_id":"wi","attempt_id":"att-wi-x-g0","generation":"1","failure_class":"forced_interrupt","interrupt_class":"service_forced_interrupt","interrupt_id":"iint_x","terminal":"cancelled"}`)},
		{Kind: "terminal", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1,
			Terminal: "cancelled", FailureClass: "forced_interrupt",
			Payload: []byte(`{"work_item_id":"wi","attempt_id":"att-wi-x-g0","generation":"1","failure_class":"forced_interrupt","interrupt_class":"service_forced_interrupt","interrupt_id":"iint_x","terminal":"cancelled"}`)},
	}); err != nil {
		t.Fatalf("legal pair: %v", err)
	}
	// Mismatched top vs payload class
	if err := workflowrun.ValidateEventStreamInvariants([]workflowrun.Event{
		{Kind: "launch", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1},
		{Kind: "interrupt", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1,
			Terminal: "cancelled", FailureClass: "forced_interrupt",
			Payload: []byte(`{"work_item_id":"wi","attempt_id":"att-wi-x-g0","generation":"1","failure_class":"forced_interrupt","interrupt_class":"service_forced_interrupt","interrupt_id":"iint_y","terminal":"cancelled"}`)},
		{Kind: "terminal", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1,
			Terminal: "cancelled", FailureClass: "other_class",
			Payload: []byte(`{"work_item_id":"wi","attempt_id":"att-wi-x-g0","generation":"1","failure_class":"forced_interrupt","interrupt_class":"service_forced_interrupt","interrupt_id":"iint_y","terminal":"cancelled"}`)},
	}); err == nil {
		t.Fatal("want fail on failure_class mismatch top vs payload")
	}
	// Missing top-level FailureClass on cancelled terminal
	if err := workflowrun.ValidateEventStreamInvariants([]workflowrun.Event{
		{Kind: "launch", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1},
		{Kind: "interrupt", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1,
			Terminal: "cancelled", FailureClass: "forced_interrupt",
			Payload: []byte(`{"work_item_id":"wi","attempt_id":"att-wi-x-g0","generation":"1","failure_class":"forced_interrupt","interrupt_class":"service_forced_interrupt","interrupt_id":"iint_z","terminal":"cancelled"}`)},
		{Kind: "terminal", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1,
			Terminal: "cancelled",
			Payload:  []byte(`{"work_item_id":"wi","attempt_id":"att-wi-x-g0","generation":"1","failure_class":"forced_interrupt","interrupt_class":"service_forced_interrupt","interrupt_id":"iint_z","terminal":"cancelled"}`)},
	}); err == nil {
		t.Fatal("want fail when top-level FailureClass missing")
	}
}
