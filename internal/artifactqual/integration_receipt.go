package artifactqual

import (
	"context"
	"fmt"
	"strings"
)

// SchemaPreProdActionsReceipt is the structured same-run dual-green Actions evidence.
// Distinct from internal/integrationreceipt SchemaReceipt (loopcoder.integration.receipt.v1).
const SchemaPreProdActionsReceipt = "loopcoder.preprod_actions_receipt.v1"

// Required pre-prod integration workflow path, branch, event, and job names.
const (
	PreProdIntegrationWorkflow = ".github/workflows/pre-prod-integration.yml"
	PreProdHeadBranch          = "pre-prod"
	PreProdEventPush           = "push"
	JobIntegrationVerify       = "integration-verify"
	JobIntegrationCanary       = "integration-canary"
)

// PreProdActionsJob is one job conclusion in a single Actions run/attempt.
type PreProdActionsJob struct {
	Name       string `json:"name"`
	Status     string `json:"status"`     // completed
	Conclusion string `json:"conclusion"` // success
}

// PreProdActionsReceipt is authoritative same-run dual-green evidence fetched from
// GitHub Actions. Caller booleans or unfetched JSON cannot substitute.
type PreProdActionsReceipt struct {
	Schema       string              `json:"schema"`
	Repository   string              `json:"repository"` // owner/repo
	WorkflowPath string              `json:"workflow_path"`
	RunID        int64               `json:"run_id"`
	Attempt      int                 `json:"attempt"` // run_attempt ≥1
	Event        string              `json:"event"`   // push
	HeadBranch   string              `json:"head_branch"`
	HeadSHA      string              `json:"head_sha"`
	Conclusion   string              `json:"conclusion"` // success
	Status       string              `json:"status"`     // completed
	Jobs         []PreProdActionsJob `json:"jobs"`
}

// PreProdActionsVerifier fetches authoritative pre-prod Actions state (production: GitHub API).
// Tests inject fakes; production uses a live implementation.
type PreProdActionsVerifier interface {
	// FetchRun returns the receipt for an exact Actions run ID and attempt.
	FetchRun(ctx context.Context, repository string, runID int64, attempt int) (PreProdActionsReceipt, error)
}

// ValidatePreProdActionsReceipt checks same-run dual-green against expected SHA/repo.
// Schema, repository, head_branch=pre-prod, event=push, workflow path, positive
// run/attempt, exact SHA, completed+success, and both named jobs in THIS receipt
// are mandatory. Any mismatch fails closed (neither job greens).
func ValidatePreProdActionsReceipt(r PreProdActionsReceipt, expectSHA, expectRepo string) (verifyOK, canaryOK bool, reasons []string) {
	add := func(s string) { reasons = append(reasons, s) }
	if strings.TrimSpace(r.Schema) != SchemaPreProdActionsReceipt {
		add("preprod_actions_receipt_schema_mismatch")
	}
	gotRepo := strings.TrimSpace(r.Repository)
	wantRepo := strings.TrimSpace(expectRepo)
	if gotRepo == "" {
		add("preprod_actions_receipt_repository_missing")
	} else if wantRepo != "" && !strings.EqualFold(gotRepo, wantRepo) {
		add("preprod_actions_receipt_repository_mismatch")
	}
	if strings.TrimSpace(r.WorkflowPath) != PreProdIntegrationWorkflow {
		add("preprod_actions_receipt_workflow_path_mismatch")
	}
	if r.RunID <= 0 {
		add("preprod_actions_receipt_run_id_missing")
	}
	if r.Attempt < 1 {
		add("preprod_actions_receipt_attempt_missing")
	}
	if strings.TrimSpace(r.HeadBranch) != PreProdHeadBranch {
		add("preprod_actions_receipt_head_branch_mismatch")
	}
	if !strings.EqualFold(strings.TrimSpace(r.Event), PreProdEventPush) {
		add("preprod_actions_receipt_event_not_push")
	}
	wantSHA := strings.TrimSpace(expectSHA)
	gotSHA := strings.TrimSpace(r.HeadSHA)
	if wantSHA == "" || gotSHA == "" || !strings.EqualFold(wantSHA, gotSHA) {
		add("preprod_actions_receipt_head_sha_mismatch")
	}
	if !strings.EqualFold(strings.TrimSpace(r.Status), "completed") {
		add("preprod_actions_receipt_run_not_completed")
	}
	if !strings.EqualFold(strings.TrimSpace(r.Conclusion), "success") {
		add("preprod_actions_receipt_run_not_success")
	}

	byName := map[string]PreProdActionsJob{}
	for _, j := range r.Jobs {
		name := strings.TrimSpace(j.Name)
		if name == "" {
			continue
		}
		byName[name] = j
	}
	checkJob := func(name string) bool {
		j, ok := byName[name]
		if !ok {
			add("preprod_actions_job_missing:" + name)
			return false
		}
		if !strings.EqualFold(strings.TrimSpace(j.Status), "completed") {
			add("preprod_actions_job_not_completed:" + name)
			return false
		}
		if !strings.EqualFold(strings.TrimSpace(j.Conclusion), "success") {
			add("preprod_actions_job_not_success:" + name)
			return false
		}
		return true
	}
	verifyOK = checkJob(JobIntegrationVerify)
	canaryOK = checkJob(JobIntegrationCanary)
	if len(reasons) > 0 {
		verifyOK, canaryOK = false, false
	}
	return verifyOK, canaryOK, reasons
}

// PreProdDualGreenFromReceipt returns dual-green only when the receipt fully validates.
func PreProdDualGreenFromReceipt(r *PreProdActionsReceipt, expectSHA, expectRepo string) (verifyOK, canaryOK bool, reasons []string) {
	if r == nil {
		return false, false, []string{"preprod_actions_receipt_missing"}
	}
	return ValidatePreProdActionsReceipt(*r, expectSHA, expectRepo)
}

// FormatPreProdActionsReceiptRef builds a non-secret evidence reference.
func FormatPreProdActionsReceiptRef(r PreProdActionsReceipt) string {
	return fmt.Sprintf("actions_run:%d/attempt:%d/workflow:%s/sha:%s",
		r.RunID, r.Attempt, r.WorkflowPath, shortSHA(r.HeadSHA))
}

func shortSHA(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
