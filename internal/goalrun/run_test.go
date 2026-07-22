package goalrun_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/goalrun"
)

func TestExecuteDecomposesAndReportsChildren(t *testing.T) {
	var reports bytes.Buffer
	res, err := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: "proj", Goal: "implement transparent multi-child routing",
		Issue: "1342", Actor: "owner", Owner: "worker",
		Provider: "fixture", Model: "fixture-model",
		ReportOut: &reports,
		Now:       func() time.Time { return time.Date(2026, 7, 22, 22, 0, 0, 0, time.UTC) },
	})
	if res.GraphID == "" || res.PlanDigest == "" {
		t.Fatalf("missing graph: %+v err=%v", res, err)
	}
	if len(res.Children) < 4 {
		t.Fatalf("children=%d", len(res.Children))
	}
	for _, c := range res.Children {
		if c.RouteRequirement == "" || c.ChildID == "" {
			t.Fatalf("%+v", c)
		}
		if strings.Contains(strings.ToLower(c.Intent), "provider_native") {
			t.Fatal("provider-native intent leak")
		}
	}
	if reports.Len() == 0 {
		t.Fatal("expected JSONL child reports")
	}
}
