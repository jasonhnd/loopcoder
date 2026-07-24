package routecontract_test

import (
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/capclass"
	"github.com/jasonhnd/loopcoder/internal/routecontract"
)

func TestParseRouteRequirementStrict(t *testing.T) {
	pr, err := routecontract.ParseRouteRequirement("class=soul,depth=high,permission=read-only")
	if err != nil || pr.Class != capclass.ClassSoul || pr.Depth != "high" || pr.Permission != "read-only" {
		t.Fatalf("soul: %+v %v", pr, err)
	}
	// Token order independence for parse values.
	pr2, err := routecontract.ParseRouteRequirement("permission=bounded_write,depth=medium,class=tera")
	if err != nil || pr2.Class != capclass.ClassTera || pr2.Depth != "medium" || pr2.Permission != "bounded_write" {
		t.Fatalf("reordered: %+v %v", pr2, err)
	}
	for _, bad := range []string{
		"",
		"depth=high,permission=read-only",
		",class=soul,depth=high,permission=read-only",
		"class=soul,depth=high,permission=read-only,",
		"class=soul,,depth=high,permission=read-only",
		"class=soul,depth=high,permission=readonly",
		"class=soul,depth=high,permission=ro",
		"class=soul,depth=high,permission=bounded-write",
		"class=needs_human,depth=high,permission=read-only",
	} {
		if _, err := routecontract.ParseRouteRequirement(bad); err == nil {
			t.Fatalf("expected fail for %q", bad)
		}
	}
}

func TestChildContractDigestRequiresFieldsAndOrderIndependentParse(t *testing.T) {
	base := routecontract.ChildAssignment{
		ExecutionPlanDigest: "sha256:plan1",
		WorkItemID:          "wi_a",
		TaskClass:           "tera",
		Depth:               "medium",
		Permission:          "bounded_write",
		OutputContract:      "branch+diff",
	}
	d1, err := routecontract.ChildContractDigest(base)
	if err != nil || d1 == "" {
		t.Fatalf("digest: %v %q", err, d1)
	}
	// Full sha256 hex: "sha256:" + 64 lowercase hex chars (not truncated).
	const prefix = "sha256:"
	if !strings.HasPrefix(d1, prefix) {
		t.Fatalf("digest must start with %q: %q", prefix, d1)
	}
	hexPart := strings.TrimPrefix(d1, prefix)
	if len(hexPart) != 64 {
		t.Fatalf("digest must be full sha256 hex (64 chars), got len=%d: %q", len(hexPart), d1)
	}
	for _, c := range hexPart {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Fatalf("digest hex must be lowercase [0-9a-f]: %q", d1)
		}
	}
	// Same values → same digest.
	d2, err := routecontract.ChildContractDigest(base)
	if err != nil || d1 != d2 {
		t.Fatalf("stable: %q vs %q err=%v", d1, d2, err)
	}
	// Each field mutates digest.
	muts := []struct {
		name string
		fn   func(*routecontract.ChildAssignment)
	}{
		{"class", func(a *routecontract.ChildAssignment) { a.TaskClass = "soul" }},
		{"depth", func(a *routecontract.ChildAssignment) { a.Depth = "high" }},
		{"permission", func(a *routecontract.ChildAssignment) { a.Permission = "read-only" }},
		{"output", func(a *routecontract.ChildAssignment) { a.OutputContract = "test_pass" }},
		{"item", func(a *routecontract.ChildAssignment) { a.WorkItemID = "wi_b" }},
		{"plan", func(a *routecontract.ChildAssignment) { a.ExecutionPlanDigest = "sha256:plan2" }},
	}
	for _, m := range muts {
		a := base
		m.fn(&a)
		d, err := routecontract.ChildContractDigest(a)
		if err != nil {
			t.Fatalf("%s: %v", m.name, err)
		}
		if d == d1 {
			t.Fatalf("%s must change digest", m.name)
		}
	}
	// Missing fields fail (never hash empties).
	for _, a := range []routecontract.ChildAssignment{
		{WorkItemID: "x", TaskClass: "tera", Depth: "medium", Permission: "bounded_write", OutputContract: "o"},
		{ExecutionPlanDigest: "p", TaskClass: "tera", Depth: "medium", Permission: "bounded_write", OutputContract: "o"},
		{ExecutionPlanDigest: "p", WorkItemID: "x", Depth: "medium", Permission: "bounded_write", OutputContract: "o"},
		{ExecutionPlanDigest: "p", WorkItemID: "x", TaskClass: "tera", Permission: "bounded_write", OutputContract: "o"},
		{ExecutionPlanDigest: "p", WorkItemID: "x", TaskClass: "tera", Depth: "medium", OutputContract: "o"},
		{ExecutionPlanDigest: "p", WorkItemID: "x", TaskClass: "tera", Depth: "medium", Permission: "bounded_write"},
		{ExecutionPlanDigest: "p", WorkItemID: "x", TaskClass: "needs_human", Depth: "medium", Permission: "bounded_write", OutputContract: "o"},
		{ExecutionPlanDigest: "p", WorkItemID: "x", TaskClass: "tera", Depth: "medium", Permission: "ro", OutputContract: "o"},
	} {
		if _, err := routecontract.ChildContractDigest(a); err == nil {
			t.Fatalf("expected fail for %+v", a)
		}
	}
	_ = strings.Contains
}

func TestValidateRouteMatchesParsed(t *testing.T) {
	pr, err := routecontract.ParseRouteRequirement("class=tera,depth=medium,permission=bounded_write")
	if err != nil {
		t.Fatal(err)
	}
	if err := routecontract.ValidateRouteMatchesParsed(pr, "tera", "medium", "bounded_write"); err != nil {
		t.Fatal(err)
	}
	if err := routecontract.ValidateRouteMatchesParsed(pr, "soul", "medium", "bounded_write"); err == nil {
		t.Fatal("class mismatch must fail")
	}
	if err := routecontract.ValidateRouteMatchesParsed(pr, "tera", "high", "bounded_write"); err == nil {
		t.Fatal("depth mismatch must fail")
	}
	if err := routecontract.ValidateRouteMatchesParsed(pr, "tera", "medium", "read-only"); err == nil {
		t.Fatal("permission mismatch must fail")
	}
}
