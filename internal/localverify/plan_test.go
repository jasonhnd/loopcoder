package localverify_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/localverify"
)

func TestPlanFromGoChangesFocused(t *testing.T) {
	p, err := localverify.BuildPlan([]string{
		"internal/foo/bar.go",
		"internal/foo/bar_test.go",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Digest == "" || len(p.Included) == 0 {
		t.Fatalf("%+v", p)
	}
	for _, c := range p.Included {
		line := strings.Join(c.Argv, " ")
		if strings.Contains(line, "./...") || strings.Contains(line, "-race") {
			t.Fatalf("denied command in plan: %s", line)
		}
		if c.Budgets.HardDeadline <= 0 || c.Budgets.MaxOutputB <= 0 {
			t.Fatalf("missing budgets: %+v", c)
		}
	}
	// excluded explains full suite
	joined := strings.Join(p.Excluded, " ")
	if !strings.Contains(joined, "go test ./...") {
		t.Fatal(p.Excluded)
	}
}

func TestDocsOnlySkipsHeavyTests(t *testing.T) {
	p, err := localverify.BuildPlan([]string{"docs/reference/usage.md", "README.md"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range p.Included {
		if c.Name == "focused_go_test" {
			t.Fatal("docs should not force go test")
		}
	}
}

func TestDeniedDetection(t *testing.T) {
	if !localverify.IsDenied([]string{"go", "test", "./..."}) {
		t.Fatal("expected deny ./...")
	}
	if !localverify.IsDenied([]string{"go", "test", "-race", "./internal/foo"}) {
		t.Fatal("expected deny race")
	}
	if localverify.IsDenied([]string{"go", "test", "./internal/foo", "-count=1"}) {
		t.Fatal("focused ok")
	}
}

func TestResultBlocksDelivery(t *testing.T) {
	cmd := localverify.Command{Name: "x", Argv: []string{"true"}, Scope: ".", Budgets: localverify.DefaultBudgets()}
	cmd.Digest = "d"
	ok := localverify.RecordResult(cmd, 0, time.Millisecond, []byte("ok"), false)
	fail := localverify.RecordResult(cmd, 1, time.Millisecond, []byte("fail"), false)
	if localverify.PlanBlocksDelivery([]localverify.Result{ok}) {
		t.Fatal("ok should not block")
	}
	if !localverify.PlanBlocksDelivery([]localverify.Result{ok, fail}) {
		t.Fatal("fail should block")
	}
}

func TestDeterministicDigest(t *testing.T) {
	files := []string{"a.go", "internal/b/c.go"}
	p1, _ := localverify.BuildPlan(files)
	p2, _ := localverify.BuildPlan(files)
	if p1.Digest != p2.Digest {
		t.Fatal("nondeterministic")
	}
}
