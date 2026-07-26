package workflowrun

import (
	"encoding/json"
	"strings"
	"testing"
)

func verifierTestInput() ChildExecInput {
	return ChildExecInput{
		ProjectID: "proj-verdict", RunID: "run-verdict", GraphID: "g-verdict",
		ExecutionPlanDigest: "sha256:" + strings.Repeat("a", 64),
		GraphDigest:         "sha256:" + strings.Repeat("b", 64),
		WorkItemID:          "wi_verify",
		AttemptID:           "att-wi_verify-verdict-g0",
	}
}

func verifierTestVerdict(in ChildExecInput, decision string) VerifierVerdict {
	findings := []VerifierFinding{}
	if decision == VerifierDecisionFail {
		findings = []VerifierFinding{{Severity: "critical", Summary: "The exact reviewed tree does not compile."}}
	}
	return VerifierVerdict{
		Schema: VerifierVerdictSchema, Decision: decision,
		ProjectID: in.ProjectID, RunID: in.RunID, GraphID: in.GraphID,
		ExecutionPlanDigest: in.ExecutionPlanDigest, GraphDigest: in.GraphDigest,
		WorkItemID: in.WorkItemID, AttemptID: in.AttemptID,
		ReviewedHeadSHA: strings.Repeat("c", 40),
		Summary:         "Independent review completed against the exact integrated head.",
		Findings:        findings,
	}
}

func marshalVerifierTestVerdict(t *testing.T, verdict any) string {
	t.Helper()
	raw, err := json.Marshal(verdict)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestParseVerifierVerdictAllowlistAndIdentity(t *testing.T) {
	in := verifierTestInput()
	head := strings.Repeat("c", 40)
	for _, decision := range []string{VerifierDecisionPass, VerifierDecisionFail, VerifierDecisionNeedsHuman} {
		t.Run(decision, func(t *testing.T) {
			verdict := verifierTestVerdict(in, decision)
			got, digest, err := parseVerifierVerdict(marshalVerifierTestVerdict(t, verdict), in, head)
			if err != nil {
				t.Fatal(err)
			}
			if got.Decision != decision || !isExactSHA256Digest(digest) {
				t.Fatalf("got=%+v digest=%q", got, digest)
			}
		})
	}

	base := verifierTestVerdict(in, VerifierDecisionPass)
	tests := []struct {
		name string
		raw  func() string
	}{
		{"empty", func() string { return "" }},
		{"malformed", func() string { return `{"schema":` }},
		{"trailing", func() string { return marshalVerifierTestVerdict(t, base) + `{}` }},
		{"unknown_field", func() string {
			var m map[string]any
			_ = json.Unmarshal([]byte(marshalVerifierTestVerdict(t, base)), &m)
			m["provider_prose"] = "not allowlisted"
			return marshalVerifierTestVerdict(t, m)
		}},
		{"null_findings", func() string {
			var m map[string]any
			_ = json.Unmarshal([]byte(marshalVerifierTestVerdict(t, base)), &m)
			m["findings"] = nil
			return marshalVerifierTestVerdict(t, m)
		}},
		{"attempt_mismatch", func() string {
			v := base
			v.AttemptID = "att-other-g0"
			return marshalVerifierTestVerdict(t, v)
		}},
		{"head_mismatch", func() string {
			v := base
			v.ReviewedHeadSHA = strings.Repeat("d", 40)
			return marshalVerifierTestVerdict(t, v)
		}},
		{"padded_summary", func() string {
			v := base
			v.Summary = " padded"
			return marshalVerifierTestVerdict(t, v)
		}},
		{"pass_with_blocker", func() string {
			v := base
			v.Findings = []VerifierFinding{{Severity: "error", Summary: "blocking"}}
			return marshalVerifierTestVerdict(t, v)
		}},
		{"fail_without_blocker", func() string {
			v := base
			v.Decision = VerifierDecisionFail
			return marshalVerifierTestVerdict(t, v)
		}},
		{"decision_alias", func() string {
			v := base
			v.Decision = "PASS"
			return marshalVerifierTestVerdict(t, v)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := parseVerifierVerdict(tc.raw(), in, head); err == nil {
				t.Fatal("mutation must fail closed")
			}
		})
	}
}

func TestVerifierVerdictSchemaPinsExactIdentity(t *testing.T) {
	in := verifierTestInput()
	head := strings.Repeat("c", 40)
	raw, err := verifierVerdictSchema(in, head)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		AdditionalProperties bool `json:"additionalProperties"`
		Properties           map[string]struct {
			Const string `json:"const"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(raw), &schema); err != nil {
		t.Fatal(err)
	}
	if schema.AdditionalProperties {
		t.Fatal("schema must reject unknown fields")
	}
	for key, want := range map[string]string{
		"project_id": in.ProjectID, "run_id": in.RunID, "graph_id": in.GraphID,
		"execution_plan_digest": in.ExecutionPlanDigest, "graph_digest": in.GraphDigest,
		"work_item_id": in.WorkItemID, "attempt_id": in.AttemptID,
		"reviewed_head_sha": head,
	} {
		if got := schema.Properties[key].Const; got != want {
			t.Fatalf("%s const=%q want=%q", key, got, want)
		}
	}
}
