package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

func TestWorkflowRunOneNodeHumanGate(t *testing.T) {
	deps := DefaultDeps()
	deps.Now = func() time.Time { return time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC) }
	var stdout, stderr bytes.Buffer
	code := runWorkflow([]string{"run", "--fixture", "one", "--format", "json"}, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var res workflowrun.Result
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Status != workflowrun.StatusHumanGate {
		t.Fatalf("%+v", res)
	}
	if res.ClaimCount != 1 || res.LaunchCount != 1 {
		t.Fatalf("%+v", res)
	}
	if res.AutoMerge {
		t.Fatal("auto_merge")
	}
}

func TestWorkflowRunChain(t *testing.T) {
	deps := DefaultDeps()
	deps.Now = func() time.Time { return time.Date(2026, 7, 22, 21, 1, 0, 0, time.UTC) }
	var stdout, stderr bytes.Buffer
	code := runWorkflow([]string{"run", "--fixture", "chain", "--format", "json"}, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var res workflowrun.Result
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.ClaimCount != 3 {
		t.Fatalf("%+v", res)
	}
	if strings.Join(res.Integrated, ",") != "a,b,c" {
		t.Fatalf("integrated %v", res.Integrated)
	}
}

func TestWorkflowRegisteredInCommands(t *testing.T) {
	found := false
	for _, c := range Commands() {
		if c.Name == "workflow" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("workflow command missing")
	}
}
