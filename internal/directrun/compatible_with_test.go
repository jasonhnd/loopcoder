package directrun

import (
	"strings"
	"testing"
)

// TestCompatibleWith_RawExactIdentity rejects padded/noncanonical plan, graph,
// task_class, and CCD. Digests and class are compared byte-exact without TrimSpace.
// TaskClass must already be canonical lowercase on both sides.
func TestCompatibleWith_RawExactIdentity(t *testing.T) {
	const (
		plan  = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		graph = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		class = "tera"
		ccd   = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
		obj   = "sha256:objective"
		base  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	good := StageReceipt{
		Objective: obj, BaseSHA: base,
		PlanDigest: plan, GraphDigest: graph, TaskClass: class, ChildContractDigest: ccd,
		Provider: "codex", Model: "gpt-5.5", Effort: "medium",
		AccountRef: "acct-1", InstallRef: "install-1",
	}
	req := Request{
		PlanDigest: plan, GraphDigest: graph, TaskClass: class, ChildContractDigest: ccd,
		Provider: "codex", Model: "gpt-5.5", Effort: "medium",
		AccountRef: "acct-1", InstallRef: "install-1",
	}
	if err := good.compatibleWith(req, base, obj); err != nil {
		t.Fatalf("exact match must pass: %v", err)
	}

	// Whitespace padding on identity fields must fail closed (no trim-before-compare).
	padCases := []struct {
		name string
		mut  func(*StageReceipt, *Request)
		sub  string
	}{
		{"plan_receipt_pad", func(r *StageReceipt, q *Request) { r.PlanDigest = " " + plan }, "plan_digest"},
		{"plan_req_pad", func(r *StageReceipt, q *Request) { q.PlanDigest = plan + " " }, "plan_digest"},
		{"graph_receipt_pad", func(r *StageReceipt, q *Request) { r.GraphDigest = "\t" + graph }, "graph_digest"},
		{"graph_req_pad", func(r *StageReceipt, q *Request) { q.GraphDigest = graph + "\n" }, "graph_digest"},
		{"class_receipt_pad", func(r *StageReceipt, q *Request) { r.TaskClass = " " + class }, "task_class"},
		{"class_req_pad", func(r *StageReceipt, q *Request) { q.TaskClass = class + " " }, "task_class"},
		{"ccd_receipt_pad", func(r *StageReceipt, q *Request) { r.ChildContractDigest = " " + ccd }, "child_contract_digest"},
		{"ccd_req_pad", func(r *StageReceipt, q *Request) { q.ChildContractDigest = ccd + " " }, "child_contract_digest"},
		// Case: TaskClass must be exact lowercase; upper/mixed fails.
		{"class_case_receipt", func(r *StageReceipt, q *Request) { r.TaskClass = "Tera" }, "task_class"},
		{"class_case_req", func(r *StageReceipt, q *Request) { q.TaskClass = "TERA" }, "task_class"},
		// Byte mismatch without padding.
		{"plan_mismatch", func(r *StageReceipt, q *Request) {
			r.PlanDigest = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
		}, "plan_digest"},
	}
	for _, tc := range padCases {
		t.Run(tc.name, func(t *testing.T) {
			r := good
			q := req
			tc.mut(&r, &q)
			err := r.compatibleWith(q, base, obj)
			if err == nil {
				t.Fatalf("expected fail closed for %s", tc.name)
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.sub)) {
				t.Fatalf("error %q should mention %q", err.Error(), tc.sub)
			}
		})
	}
}

// TestRequireExecutionIdentity_RejectsPaddedAndNoncanonical rejects request
// identity that is padded or not lowercase TaskClass before spend.
func TestRequireExecutionIdentity_RejectsPaddedAndNoncanonical(t *testing.T) {
	const (
		plan  = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		graph = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		ccd   = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	)
	base := Request{
		PlanDigest: plan, GraphDigest: graph, TaskClass: "tera", ChildContractDigest: ccd,
	}
	if err := requireExecutionIdentity(base); err != nil {
		t.Fatalf("canonical base must pass: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*Request)
		sub  string
	}{
		{"plan_pad", func(r *Request) { r.PlanDigest = " " + plan }, "plan_digest"},
		{"graph_pad", func(r *Request) { r.GraphDigest = graph + " " }, "graph_digest"},
		{"class_pad", func(r *Request) { r.TaskClass = " tera" }, "task_class"},
		{"class_upper", func(r *Request) { r.TaskClass = "Tera" }, "task_class"},
		{"class_mixed", func(r *Request) { r.TaskClass = "Soul" }, "task_class"},
		{"ccd_pad", func(r *Request) { r.ChildContractDigest = " " + ccd }, "child_contract_digest"},
		{"class_empty", func(r *Request) { r.TaskClass = "" }, "task_class"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			tc.mut(&r)
			err := requireExecutionIdentity(r)
			if err == nil {
				t.Fatalf("expected reject for %s", tc.name)
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.sub)) {
				t.Fatalf("error %q should mention %q", err.Error(), tc.sub)
			}
		})
	}
}
