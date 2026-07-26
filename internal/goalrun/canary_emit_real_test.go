package goalrun

import (
	"testing"

	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

// Failed and fake rows must emit RealProviderExecuted=false (same gate as collectUsage).
func TestCanaryChildrenFromReports_FailedAndFakeNotRealExecuted(t *testing.T) {
	res := Result{
		Children: []ChildReport{
			{
				ChildID: "wi_fake", AttemptID: "att-fake", Provider: "codex", Model: "gpt-5.5", Depth: "low",
				Terminal: "succeeded", Stage: "integrated",
				OutputEvidence: "fake-ok", WorktreePath: "/tmp/wt/fake",
				// No ArgvDigest / ActualSources — FakeChildExecutor shape.
			},
			{
				ChildID: "wi_failed", AttemptID: "att-fail", Provider: "grok", Model: "grok-4.5", Depth: "medium",
				Terminal: "failed", Stage: "terminal",
				OutputEvidence: "failed:executor_error", WorktreePath: "/tmp/wt/fail",
				ArgvDigest: "sha256:deadbeef",
				ActualSources: workflowrun.ActualRouteSources{
					Model: "accepted_invocation", Effort: "accepted_invocation", Permission: "accepted_invocation",
				},
			},
			{
				ChildID: "wi_auth_only", AttemptID: "att-auth", Provider: "codex", Model: "gpt-5.5", Depth: "high",
				Terminal: "failed", Stage: "terminal",
				OutputEvidence: "failed:pre-spawn", WorktreePath: "/tmp/wt/auth",
				ActualSources: workflowrun.ActualRouteSources{
					Account: "auth_binding", Install: "install_binding",
				},
			},
			successProof("wi_real", "codex", "gpt-5.5", "low"),
		},
	}
	// successProof needs attempt/output for canary child emission path fields.
	res.Children[3].AttemptID = "att-real"
	res.Children[3].OutputEvidence = "ok"
	res.Children[3].WorktreePath = "/tmp/wt/real"

	children := canaryChildrenFromReports(res)
	byID := map[string]bool{}
	for _, c := range children {
		byID[c.ChildID] = c.RealProviderExecuted
	}
	if byID["wi_fake"] {
		t.Fatal("fake integrated success without ArgvDigest must emit RealProviderExecuted=false")
	}
	if byID["wi_failed"] {
		t.Fatal("failed terminal must emit RealProviderExecuted=false even with ArgvDigest")
	}
	if byID["wi_auth_only"] {
		t.Fatal("auth/install-binding-only failed row must emit RealProviderExecuted=false")
	}
	if !byID["wi_real"] {
		t.Fatal("real accepted-invocation success must emit RealProviderExecuted=true")
	}
}

// Four codex successes + failed grok must not yield multi-provider via canary children proof.
func TestCanaryChildrenFromReports_FourSuccessOneFailedProviderNotMulti(t *testing.T) {
	children := []ChildReport{
		successProof("wi_a", "codex", "gpt-5.5", "low"),
		successProof("wi_b", "codex", "gpt-5.5", "medium"),
		successProof("wi_c", "codex", "gpt-5.5", "medium"),
		successProof("wi_d", "codex", "gpt-5.5", "high"),
		{
			ChildID: "wi_e", Provider: "grok", Model: "grok-4.5", Depth: "medium",
			Terminal: "failed", Stage: "terminal", AttemptID: "att-e",
			OutputEvidence: "failed", ArgvDigest: "sha256:x",
			ActualSources: workflowrun.ActualRouteSources{
				Model: "accepted_invocation", Effort: "accepted_invocation", Permission: "accepted_invocation",
			},
		},
	}
	for i := range children {
		if children[i].AttemptID == "" {
			children[i].AttemptID = "att-" + children[i].ChildID
		}
		if children[i].OutputEvidence == "" {
			children[i].OutputEvidence = "ok"
		}
	}
	provs, _, _ := collectUsage(children)
	if len(provs) != 1 || provs[0] != "codex" {
		t.Fatalf("collectUsage providers=%v want only codex", provs)
	}
	cc := canaryChildrenFromReports(Result{Children: children})
	realProvs := map[string]bool{}
	for _, c := range cc {
		if c.RealProviderExecuted {
			realProvs[c.Provider] = true
		}
	}
	if len(realProvs) != 1 || !realProvs["codex"] {
		t.Fatalf("canary RealProviderExecuted providers=%v want only codex", realProvs)
	}
}

// Four medium successes + failed high depth must not yield multi-depth via collectUsage.
func TestCollectUsage_FailedSecondDepthNotMultiDepth(t *testing.T) {
	children := []ChildReport{
		successProof("wi_a", "codex", "gpt-5.5", "medium"),
		successProof("wi_b", "antigravity", "gpt-oss", "medium"),
		successProof("wi_c", "codex", "gpt-5.5", "medium"),
		successProof("wi_d", "antigravity", "gpt-oss", "medium"),
		{
			ChildID: "wi_e", Provider: "codex", Model: "gpt-5.5", Depth: "high",
			Terminal: "failed", Stage: "terminal",
			ArgvDigest: "sha256:x",
			ActualSources: workflowrun.ActualRouteSources{
				Model: "accepted_invocation", Effort: "accepted_invocation", Permission: "accepted_invocation",
			},
		},
	}
	provs, _, depths := collectUsage(children)
	if len(provs) != 2 {
		t.Fatalf("providers=%v want multi-provider from successes", provs)
	}
	if len(depths) != 1 || depths[0] != "medium" {
		t.Fatalf("depths=%v want only medium (failed high must not count)", depths)
	}
}

// Structured WorkItemID/TaskClass/OutputEvidence must propagate from ChildReport.
func TestCanaryChildrenFromReports_StructuredVerifierIdentity(t *testing.T) {
	evid := "sha256:verify-product-digest-00112233445566778899aabbccddeeff"
	res := Result{
		Children: []ChildReport{
			{
				ChildID: "wi_verify", AttemptID: "att-v-1", Provider: "codex", Model: "gpt-5.5",
				Depth: "high", TaskClass: "soul", Terminal: "succeeded", Stage: "integrated",
				OutputEvidence: evid, WorktreePath: "/tmp/wt/verify",
				ArgvDigest: "sha256:argv-wi_verify",
				ActualSources: workflowrun.ActualRouteSources{
					Model: "provider_stream", Effort: "accepted_invocation", Permission: "accepted_invocation",
					Account: "auth_binding", Install: "install_binding",
				},
			},
		},
	}
	children := canaryChildrenFromReports(res)
	if len(children) != 1 {
		t.Fatalf("children=%d", len(children))
	}
	c := children[0]
	if c.WorkItemID != "wi_verify" {
		t.Fatalf("WorkItemID=%q", c.WorkItemID)
	}
	if c.ChildID != c.WorkItemID {
		t.Fatalf("ChildID=%q WorkItemID=%q want equal", c.ChildID, c.WorkItemID)
	}
	if c.TaskClass != "soul" {
		t.Fatalf("TaskClass=%q", c.TaskClass)
	}
	if c.OutputEvidence != evid {
		t.Fatalf("OutputEvidence=%q want %q", c.OutputEvidence, evid)
	}
}
