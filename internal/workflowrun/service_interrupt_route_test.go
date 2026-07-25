package workflowrun

import (
	"strings"
	"testing"
)

func TestBindServiceInterruptedOutcomeRoute_PartialExactBinding(t *testing.T) {
	selected := ChildRoute{
		Provider: "claude", Model: "claude-opus-4-8", Depth: "high",
		Permission: "read-only", TaskClass: "soul",
		AccountRef: "acct-claude", InstallRef: "install-claude",
		WindowKind: "five_hour", ReservationID: "res-claude",
		RouteReason: "winner=claude",
	}
	invoked := ChildRoute{
		Provider: selected.Provider,
		Model:    selected.Model,
		Depth:    selected.Depth,
	}
	var outcome ChildOutcome
	if err := bindServiceInterruptedOutcomeRoute(&outcome, selected, invoked); err != nil {
		t.Fatalf("bind partial interrupted route: %v", err)
	}
	if outcome.Provider != selected.Provider ||
		outcome.Model != selected.Model ||
		outcome.Depth != selected.Depth ||
		outcome.Permission != selected.Permission ||
		outcome.AccountRef != selected.AccountRef ||
		outcome.InstallRef != selected.InstallRef ||
		outcome.WindowKind != selected.WindowKind ||
		outcome.ReservationID != selected.ReservationID ||
		outcome.RouteReason != selected.RouteReason {
		t.Fatalf("outcome route not exact selected route: got=%+v want=%+v", outcome, selected)
	}
}

func TestBindServiceInterruptedOutcomeRoute_EachNonemptyMismatchFailsClosed(t *testing.T) {
	selected := ChildRoute{
		Provider: "claude", Model: "claude-opus-4-8", Depth: "high",
		Permission: "read-only", TaskClass: "soul",
		AccountRef: "acct-claude", InstallRef: "install-claude",
		WindowKind: "five_hour", ReservationID: "res-claude",
	}
	tests := []struct {
		name   string
		mutate func(*ChildRoute)
	}{
		{"provider", func(r *ChildRoute) { r.Provider = "Claude" }},
		{"model", func(r *ChildRoute) { r.Model = "claude-opus-4-8 " }},
		{"depth", func(r *ChildRoute) { r.Depth = "HIGH" }},
		{"permission", func(r *ChildRoute) { r.Permission = " read-only" }},
		{"task_class", func(r *ChildRoute) { r.TaskClass = "Soul" }},
		{"account_ref", func(r *ChildRoute) { r.AccountRef = "acct-other" }},
		{"install_ref", func(r *ChildRoute) { r.InstallRef = "install-other" }},
		{"window_kind", func(r *ChildRoute) { r.WindowKind = "weekly" }},
		{"reservation_id", func(r *ChildRoute) { r.ReservationID = "res-other" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			invoked := selected
			tc.mutate(&invoked)
			var outcome ChildOutcome
			err := bindServiceInterruptedOutcomeRoute(&outcome, selected, invoked)
			if err == nil {
				t.Fatalf("%s mismatch accepted: invoked=%+v", tc.name, invoked)
			}
			if !strings.Contains(err.Error(), tc.name) {
				t.Fatalf("%s mismatch error=%q", tc.name, err)
			}
			if outcome.Permission != "" || outcome.Provider != "" {
				t.Fatalf("mismatch partially bound outcome: %+v", outcome)
			}
		})
	}
}

func TestBindServiceInterruptedOutcomeRoute_SelectedCapacityMustBeComplete(t *testing.T) {
	selected := ChildRoute{
		Provider: "claude", Model: "claude-opus-4-8", Depth: "high",
		Permission: "read-only", TaskClass: "soul",
		AccountRef: "acct-claude", InstallRef: "install-claude",
		WindowKind: "five_hour",
		// A capacity-bound interruption without the selected reservation is not
		// safe to resume from a reconstructed outcome.
		ReservationID: "",
	}
	var outcome ChildOutcome
	err := bindServiceInterruptedOutcomeRoute(&outcome, selected, ChildRoute{})
	if err == nil || !strings.Contains(err.Error(), "reservation_id") {
		t.Fatalf("incomplete selected capacity route must fail closed: %v", err)
	}
}
