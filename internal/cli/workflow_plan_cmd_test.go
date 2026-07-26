package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

func TestWorkflowPlanDecomposesFourChildren(t *testing.T) {
	deps := DefaultDeps()
	deps.Now = func() time.Time { return time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC) }
	var stdout, stderr bytes.Buffer
	code := runWorkflow([]string{"plan", "--goal", "implement multi-provider routing", "--issue", "1341", "--format", "json"}, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var g workgraph.Graph
	if err := json.Unmarshal(stdout.Bytes(), &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Items) < 4 {
		t.Fatalf("items=%d", len(g.Items))
	}
	if g.Source != workgraph.SourceGoalDecompose {
		t.Fatalf("source=%s", g.Source)
	}
	if !strings.Contains(stdout.String(), "wi_implement") {
		t.Fatal(stdout.String())
	}
}
