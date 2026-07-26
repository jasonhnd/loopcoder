package workflowrun

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

const (
	VerifierVerdictSchema       = "loopcoder.verifier_verdict.v1"
	VerifierDecisionPass        = "pass"
	VerifierDecisionFail        = "fail"
	VerifierDecisionNeedsHuman  = "needs_human"
	FailureClassVerifierInvalid = "verifier_verdict_invalid"
	FailureClassVerifierFailed  = "verifier_verdict_failed"
	FailureClassVerifierHuman   = "verifier_verdict_needs_human"
)

type VerifierFinding struct {
	Severity string `json:"severity"`
	Summary  string `json:"summary"`
}

// VerifierVerdict is the provider-machine-readable, allowlisted decision for
// the exact read-only verifier invocation. Identity fields are exact literals:
// callers never normalize or infer them from prose.
type VerifierVerdict struct {
	Schema              string            `json:"schema"`
	Decision            string            `json:"decision"`
	ProjectID           string            `json:"project_id"`
	RunID               string            `json:"run_id"`
	GraphID             string            `json:"graph_id"`
	ExecutionPlanDigest string            `json:"execution_plan_digest"`
	GraphDigest         string            `json:"graph_digest"`
	WorkItemID          string            `json:"work_item_id"`
	AttemptID           string            `json:"attempt_id"`
	ReviewedHeadSHA     string            `json:"reviewed_head_sha"`
	Summary             string            `json:"summary"`
	Findings            []VerifierFinding `json:"findings"`
}

func verifierReviewedHead(worktree string) (string, error) {
	out, err := exec.Command("git", "-C", worktree, "rev-parse", "--verify", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("workflowrun: verifier reviewed head: %w", err)
	}
	head := strings.TrimSuffix(string(out), "\n")
	if !isExactGitOID(head) {
		return "", fmt.Errorf("workflowrun: verifier reviewed head is not an exact git oid")
	}
	return head, nil
}

func isExactGitOID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func isExactSHA256Digest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for i := len("sha256:"); i < len(value); i++ {
		c := value[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func verifierVerdictSchema(in ChildExecInput, reviewedHead string) (string, error) {
	constString := func(value string) map[string]any {
		return map[string]any{"type": "string", "const": value}
	}
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required": []string{
			"schema", "decision", "project_id", "run_id", "graph_id",
			"execution_plan_digest", "graph_digest", "work_item_id",
			"attempt_id", "reviewed_head_sha", "summary", "findings",
		},
		"properties": map[string]any{
			"schema":                constString(VerifierVerdictSchema),
			"decision":              map[string]any{"type": "string", "enum": []string{VerifierDecisionPass, VerifierDecisionFail, VerifierDecisionNeedsHuman}},
			"project_id":            constString(in.ProjectID),
			"run_id":                constString(in.RunID),
			"graph_id":              constString(in.GraphID),
			"execution_plan_digest": constString(in.ExecutionPlanDigest),
			"graph_digest":          constString(in.GraphDigest),
			"work_item_id":          constString(in.WorkItemID),
			"attempt_id":            constString(in.AttemptID),
			"reviewed_head_sha":     constString(reviewedHead),
			"summary":               map[string]any{"type": "string", "minLength": 1, "maxLength": 4000},
			"findings": map[string]any{
				"type":     "array",
				"maxItems": 64,
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"severity", "summary"},
					"properties": map[string]any{
						"severity": map[string]any{"type": "string", "enum": []string{"info", "warning", "error", "critical"}},
						"summary":  map[string]any{"type": "string", "minLength": 1, "maxLength": 1000},
					},
				},
			},
		},
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func verifierPrompt(intent string, in ChildExecInput, reviewedHead string) string {
	return strings.TrimSpace(intent) + "\n\n" +
		"Review the exact integrated repository HEAD " + reviewedHead + " read-only and return only the required machine-readable verdict. " +
		"Use decision=pass only when no blocking defect remains. Use decision=fail for any error or critical defect. " +
		"Use decision=needs_human when required verification cannot be completed. " +
		"Bind every identity field exactly as required by the supplied JSON schema; do not include credentials, session identifiers, email, or raw provider output."
}

func parseVerifierVerdict(raw string, in ChildExecInput, reviewedHead string) (VerifierVerdict, string, error) {
	var out VerifierVerdict
	if len(raw) == 0 || len(raw) > 64*1024 {
		return out, "", fmt.Errorf("workflowrun: verifier verdict missing or oversized")
	}
	dec := json.NewDecoder(bytes.NewBufferString(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return VerifierVerdict{}, "", fmt.Errorf("workflowrun: verifier verdict JSON: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err == nil {
		return VerifierVerdict{}, "", fmt.Errorf("workflowrun: verifier verdict has trailing value")
	} else if err != io.EOF {
		return VerifierVerdict{}, "", fmt.Errorf("workflowrun: verifier verdict trailing JSON: %w", err)
	}
	if out.Schema != VerifierVerdictSchema ||
		out.ProjectID != in.ProjectID ||
		out.RunID != in.RunID ||
		out.GraphID != in.GraphID ||
		out.ExecutionPlanDigest != in.ExecutionPlanDigest ||
		out.GraphDigest != in.GraphDigest ||
		out.WorkItemID != in.WorkItemID ||
		out.AttemptID != in.AttemptID ||
		out.ReviewedHeadSHA != reviewedHead {
		return VerifierVerdict{}, "", fmt.Errorf("workflowrun: verifier verdict identity mismatch")
	}
	if strings.TrimSpace(out.Summary) == "" || out.Summary != strings.TrimSpace(out.Summary) ||
		len(out.Summary) > 4000 {
		return VerifierVerdict{}, "", fmt.Errorf("workflowrun: verifier verdict summary empty or padded")
	}
	if out.Findings == nil || len(out.Findings) > 64 {
		return VerifierVerdict{}, "", fmt.Errorf("workflowrun: verifier findings missing or oversized")
	}
	blocking := false
	for _, finding := range out.Findings {
		if finding.Summary == "" || finding.Summary != strings.TrimSpace(finding.Summary) ||
			len(finding.Summary) > 1000 {
			return VerifierVerdict{}, "", fmt.Errorf("workflowrun: verifier finding summary empty or padded")
		}
		switch finding.Severity {
		case "info", "warning":
		case "error", "critical":
			blocking = true
		default:
			return VerifierVerdict{}, "", fmt.Errorf("workflowrun: verifier finding severity invalid")
		}
	}
	switch out.Decision {
	case VerifierDecisionPass:
		if blocking {
			return VerifierVerdict{}, "", fmt.Errorf("workflowrun: pass verdict contains blocking finding")
		}
	case VerifierDecisionFail:
		if !blocking {
			return VerifierVerdict{}, "", fmt.Errorf("workflowrun: fail verdict requires error or critical finding")
		}
	case VerifierDecisionNeedsHuman:
	default:
		return VerifierVerdict{}, "", fmt.Errorf("workflowrun: verifier decision invalid")
	}
	canonical, err := json.Marshal(out)
	if err != nil {
		return VerifierVerdict{}, "", err
	}
	sum := sha256.Sum256(canonical)
	return out, "sha256:" + hex.EncodeToString(sum[:]), nil
}

func materializeStructuredVerifierVerdict(worktree string, verdict VerifierVerdict) error {
	raw, err := json.MarshalIndent(verdict, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := writeProductFileSecurely(worktree, "verdict.json", string(raw), "structured verifier verdict"); err != nil {
		return err
	}
	var body strings.Builder
	body.WriteString("# Verification verdict\n\n")
	body.WriteString("Decision: **" + verdict.Decision + "**\n\n")
	body.WriteString("Reviewed HEAD: `" + verdict.ReviewedHeadSHA + "`\n\n")
	body.WriteString("## Summary\n\n" + verdict.Summary + "\n")
	if len(verdict.Findings) > 0 {
		body.WriteString("\n## Findings\n")
		for _, finding := range verdict.Findings {
			body.WriteString("\n- " + finding.Severity + ": " + finding.Summary)
		}
		body.WriteString("\n")
	}
	return writeProductFileSecurely(worktree, "verdict.md", body.String(), "verifier verdict")
}

func verifierVerdictPayloadFields(outcome *ChildOutcome) map[string]string {
	if outcome == nil || outcome.WorkItemID != "wi_verify" {
		return nil
	}
	return map[string]string{
		"verifier_decision":          outcome.VerifierDecision,
		"verifier_verdict_digest":    outcome.VerifierVerdictDigest,
		"verifier_reviewed_head_sha": outcome.VerifierReviewedHeadSHA,
	}
}
