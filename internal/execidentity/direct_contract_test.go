package execidentity_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/execidentity"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

func baseInput() execidentity.DirectContractInput {
	return execidentity.DirectContractInput{
		IssueTitle:     "Implement direct-run identity",
		IssueBody:      "Body with acceptance criteria.",
		BaseSHA:        "deadbeefcafebabe0123456789abcdef01234567",
		TaskClass:      "tera",
		Depth:          "medium",
		Permission:     "bounded_write",
		OutputContract: execidentity.DirectRunOutputContract,
		Actor:          "owner",
		ProjectID:      "proj-direct",
		Now:            time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC),
	}
}

func TestBuildDirectContractStableAndFullSHA256(t *testing.T) {
	in := baseInput()
	c1, err := execidentity.BuildDirectContract(in)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := execidentity.BuildDirectContract(in)
	if err != nil {
		t.Fatal(err)
	}
	if c1.PlanDigest == "" || c1.GraphDigest == "" || c1.ChildContractDigest == "" {
		t.Fatalf("empty digests: %+v", c1)
	}
	if c1.PlanDigest != c2.PlanDigest || c1.GraphDigest != c2.GraphDigest || c1.ChildContractDigest != c2.ChildContractDigest {
		t.Fatalf("unstable serialization:\n%+v\n%+v", c1, c2)
	}
	if c1.PlanDigest == c1.GraphDigest {
		t.Fatal("plan must not equal graph digest")
	}
	if c1.Graph.Source != workgraph.SourceDirectMaterialize {
		t.Fatalf("source=%q want %q", c1.Graph.Source, workgraph.SourceDirectMaterialize)
	}
	const p = "sha256:"
	if !strings.HasPrefix(c1.ChildContractDigest, p) {
		t.Fatalf("ccd prefix: %q", c1.ChildContractDigest)
	}
	if len(strings.TrimPrefix(c1.ChildContractDigest, p)) != 64 {
		t.Fatalf("ccd must be full 64-hex: %q", c1.ChildContractDigest)
	}
	for _, dig := range []string{c1.PlanDigest, c1.GraphDigest, c1.ChildContractDigest} {
		if strings.Contains(dig, "direct-plan") || strings.Contains(dig, "direct-graph") || strings.Contains(dig, "direct-ccd") {
			t.Fatalf("counterfeit digest forbidden: %q", dig)
		}
	}
}

func TestBuildDirectContractMutationMatrix(t *testing.T) {
	base, err := execidentity.BuildDirectContract(baseInput())
	if err != nil {
		t.Fatal(err)
	}
	// CCD includes ExecutionPlanDigest, so plan-changing inputs must change CCD too.
	muts := []struct {
		name string
		fn   func(*execidentity.DirectContractInput)
	}{
		{"title", func(i *execidentity.DirectContractInput) { i.IssueTitle = "Other title" }},
		{"body", func(i *execidentity.DirectContractInput) { i.IssueBody = "Completely different body text." }},
		{"base", func(i *execidentity.DirectContractInput) { i.BaseSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" }},
		{"class", func(i *execidentity.DirectContractInput) { i.TaskClass = "soul" }},
		{"depth", func(i *execidentity.DirectContractInput) { i.Depth = "high" }},
		{"permission", func(i *execidentity.DirectContractInput) { i.Permission = "read-only" }},
		{"output", func(i *execidentity.DirectContractInput) { i.OutputContract = "test_pass" }},
	}
	for _, m := range muts {
		t.Run(m.name, func(t *testing.T) {
			in := baseInput()
			m.fn(&in)
			got, err := execidentity.BuildDirectContract(in)
			if err != nil {
				t.Fatal(err)
			}
			if got.PlanDigest == base.PlanDigest {
				t.Fatalf("%s must change PlanDigest", m.name)
			}
			if got.GraphDigest == base.GraphDigest {
				t.Fatalf("%s must change GraphDigest", m.name)
			}
			// CCD always includes ExecutionPlanDigest → any plan mutation changes CCD.
			if got.ChildContractDigest == base.ChildContractDigest {
				t.Fatalf("%s must change ChildContractDigest (includes ExecutionPlanDigest)", m.name)
			}
		})
	}
}

func TestBuildDirectContractFailClosedNoDefaults(t *testing.T) {
	cases := []struct {
		name string
		fn   func(*execidentity.DirectContractInput)
	}{
		{"empty_class", func(i *execidentity.DirectContractInput) { i.TaskClass = "" }},
		{"needs_human", func(i *execidentity.DirectContractInput) { i.TaskClass = "needs_human" }},
		{"empty_depth", func(i *execidentity.DirectContractInput) { i.Depth = "" }},
		{"bad_depth", func(i *execidentity.DirectContractInput) { i.Depth = "xhigh" }},
		{"empty_perm", func(i *execidentity.DirectContractInput) { i.Permission = "" }},
		{"alias_perm", func(i *execidentity.DirectContractInput) { i.Permission = "ro" }},
		{"empty_base", func(i *execidentity.DirectContractInput) { i.BaseSHA = "" }},
		{"empty_output", func(i *execidentity.DirectContractInput) { i.OutputContract = "" }},
		{"empty_issue", func(i *execidentity.DirectContractInput) { i.IssueTitle = ""; i.IssueBody = "" }},
		{"empty_actor", func(i *execidentity.DirectContractInput) { i.Actor = "" }},
		{"empty_project", func(i *execidentity.DirectContractInput) { i.ProjectID = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput()
			tc.fn(&in)
			if _, err := execidentity.BuildDirectContract(in); err == nil {
				t.Fatalf("expected fail for %s", tc.name)
			}
		})
	}
}
