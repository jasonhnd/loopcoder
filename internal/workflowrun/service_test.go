package workflowrun_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/workflowdef"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

func t0() time.Time { return time.Date(2026, 7, 22, 20, 0, 0, 0, time.UTC) }

func TestOneNodeReachesHumanGate(t *testing.T) {
	svc := workflowrun.Service{Now: t0}
	res, err := svc.Execute(context.Background(), workflowrun.Request{
		ProjectID: "proj", Definition: workflowrun.OneNodeDefinition("g1", "docs"),
		Actor: "owner",
	})
	if err != nil {
		t.Fatalf("%v %+v", err, res)
	}
	if res.Status != workflowrun.StatusHumanGate {
		t.Fatalf("%+v", res)
	}
	if res.ClaimCount != 1 || res.LaunchCount != 1 {
		t.Fatalf("claims/launches %+v", res)
	}
	if res.AutoMerge {
		t.Fatal("auto_merge")
	}
	if !strings.Contains(strings.Join(res.Events, "\n"), "human_gate.await_owner") {
		t.Fatalf("events %v", res.Events)
	}
}

func TestThreeNodeChainClaimOnce(t *testing.T) {
	svc := workflowrun.Service{Now: t0}
	res, err := svc.Execute(context.Background(), workflowrun.Request{
		ProjectID: "proj", Definition: workflowrun.ChainDefinition("g3"),
		Actor: "owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ClaimCount != 3 || res.LaunchCount != 3 {
		t.Fatalf("%+v", res)
	}
	if len(res.Integrated) != 3 {
		t.Fatalf("integrated %v", res.Integrated)
	}
	// deterministic order a,b,c
	if strings.Join(res.Integrated, ",") != "a,b,c" {
		t.Fatalf("order %v", res.Integrated)
	}
}

func TestCyclicCreatesNoClaims(t *testing.T) {
	svc := workflowrun.Service{Now: t0}
	def := workflowdef.Definition{
		SchemaVersion: 1, GraphID: "bad", Source: "explicit_definition",
		Items: []workflowdef.DefItem{
			{ID: "a", Intent: "A", Status: "required", IntegrationOrder: 1},
			{ID: "b", Intent: "B", Status: "required", IntegrationOrder: 2},
		},
		Deps: []workflowdef.DefDep{
			{From: "a", To: "b", Kind: "finish_to_start"},
			{From: "b", To: "a", Kind: "finish_to_start"},
		},
	}
	res, err := svc.Execute(context.Background(), workflowrun.Request{
		ProjectID: "proj", Definition: def, Actor: "owner",
	})
	if err == nil {
		t.Fatalf("expected error: %+v", res)
	}
	if res.ClaimCount != 0 || res.LaunchCount != 0 {
		t.Fatalf("side effects: %+v", res)
	}
	if res.Status != workflowrun.StatusInvalid {
		t.Fatalf("status %s", res.Status)
	}
}
