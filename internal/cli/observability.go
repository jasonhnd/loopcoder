package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/detachedrun"
	"github.com/jasonhnd/loopcoder/internal/observability"
	"github.com/jasonhnd/loopcoder/internal/orchestration"
	"github.com/jasonhnd/loopcoder/internal/recovery"
	"github.com/jasonhnd/loopcoder/internal/worker"
)

func renderCanonicalHuman(w io.Writer, doc observability.Document, deps Deps) error {
	_, err := io.WriteString(w, observability.RenderHuman(doc, canonicalCapabilities(w, deps)))
	return err
}

func renderCanonicalMachine(w io.Writer, doc observability.Document, format string) error {
	switch format {
	case "json":
		return observability.RenderJSON(w, doc)
	case "jsonl":
		return observability.RenderJSONL(w, doc)
	default:
		return renderCanonicalHuman(w, doc, Deps{})
	}
}

func canonicalCapabilities(w io.Writer, deps Deps) observability.Capabilities {
	if deps.IsTerminal == nil {
		deps.IsTerminal = DefaultDeps().IsTerminal
	}
	interactive := deps.IsTerminal(w)
	width := 80
	if deps.TerminalWidth != nil {
		if injected := deps.TerminalWidth(w); injected > 0 {
			width = injected
		}
	}
	plain := plainPrettyForced()
	return observability.Capabilities{
		Width:      width,
		Color:      interactive && !plain,
		Unicode:    interactive && !plain,
		Redirected: !interactive,
	}
}

func renderCanonicalJSONLine(w io.Writer, value any) error {
	data, err := delivery.CanonicalJSON(value)
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

func detachedRecoverJSONPayload(result detachedrun.StatusResult) any {
	return struct {
		SchemaVersion string                 `json:"schema_version"`
		Observability observability.Document `json:"observability"`
		detachedrun.StatusResult
	}{
		SchemaVersion: "loopcoder.recover_detached_result.v1",
		Observability: detachedRecoverObservability(result),
		StatusResult:  result,
	}
}

func detachedRecoverObservability(result detachedrun.StatusResult) observability.Document {
	return observability.NewDocument("recover", observability.Correlation{
		DeliveryRunID: result.Record.RunID,
		Source:        "recover",
	}, []observability.RenderItem{{
		ID:         result.Record.RunID,
		Kind:       "detached-recovery",
		Status:     result.Record.Status,
		Reason:     result.Reason,
		NextAction: result.ReplayAction,
		Confidence: "exact",
		Freshness:  "durable",
		SourceRefs: []observability.SourceRef{{
			Table:         "detached_runs",
			RecordID:      result.Record.RunID,
			DeliveryRunID: result.Record.RunID,
			Provenance:    "detached-recover",
		}},
	}}, nil)
}

func dispatchJSONPayload(result worker.Result) any {
	result = dispatchResultForOutput(result)
	return struct {
		SchemaVersion string                 `json:"schema_version"`
		Observability observability.Document `json:"observability"`
		worker.Result
	}{
		SchemaVersion: "loopcoder.dispatch_result.v1",
		Observability: dispatchObservability(result),
		Result:        result,
	}
}

func dispatchObservability(result worker.Result) observability.Document {
	return observability.NewDocument("dispatch", observability.Correlation{
		DeliveryRunID: result.RunID,
		Source:        "dispatch",
	}, dispatchObservabilityItems(result), nil)
}

func dispatchObservabilityItems(result worker.Result) []observability.RenderItem {
	if result.Report == nil {
		return []observability.RenderItem{}
	}
	return []observability.RenderItem{
		observability.ItemFromReport("issue-"+strconv.Itoa(result.Issue), "worker", result.Status, result.Reason, result.NextAction, *result.Report, []observability.SourceRef{{
			Table:         "worker_attempts",
			RecordID:      result.AttemptPath,
			DeliveryRunID: result.RunID,
			Provenance:    "durable-local-attempt",
		}}),
	}
}

func dispatchResultForOutput(result worker.Result) worker.Result {
	if strings.TrimSpace(result.AttemptPath) != "" {
		result.AttemptPath = observability.StableRecordID(result.AttemptPath)
	}
	return result
}

func recoveryResultForOutput(result recovery.Result) recovery.Result {
	if result.DispatchResult != nil {
		dispatch := recoveryDispatchResultForOutput(*result.DispatchResult)
		result.DispatchResult = &dispatch
	}
	if len(result.RecoveryAttempts) > 0 {
		result.RecoveryAttempts = append([]recovery.AttemptRecord(nil), result.RecoveryAttempts...)
		for i := range result.RecoveryAttempts {
			if strings.TrimSpace(result.RecoveryAttempts[i].RecoveryContextPath) != "" {
				result.RecoveryAttempts[i].RecoveryContextPath = observability.StableRecordID(result.RecoveryAttempts[i].RecoveryContextPath)
			}
			if result.RecoveryAttempts[i].DispatchResult != nil {
				dispatch := recoveryDispatchResultForOutput(*result.RecoveryAttempts[i].DispatchResult)
				result.RecoveryAttempts[i].DispatchResult = &dispatch
			}
		}
	}
	return result
}

func recoveryDispatchResultForOutput(result recovery.DispatchResult) recovery.DispatchResult {
	if strings.TrimSpace(result.AttemptPath) != "" {
		result.AttemptPath = observability.StableRecordID(result.AttemptPath)
	}
	return result
}

func nestedJSONPayload(report orchestration.NestedScheduleReport) any {
	return struct {
		SchemaVersion string                 `json:"schema_version"`
		Observability observability.Document `json:"observability"`
		orchestration.NestedScheduleReport
	}{
		SchemaVersion:        "loopcoder.nested_run_result.v1",
		Observability:        nestedObservability(report),
		NestedScheduleReport: report,
	}
}

func nestedObservability(report orchestration.NestedScheduleReport) observability.Document {
	items := make([]observability.RenderItem, 0, len(report.Children))
	evidence := make([]observability.Evidence, 0)
	for _, child := range report.Children {
		if audit := child.ReadOnlyEnforcement; audit != nil {
			severity := "info"
			if audit.Verification != "passed" {
				severity = "error"
			}
			evidence = append(evidence, observability.Evidence{
				Type: "read-only-enforcement", Code: audit.Verification, Severity: severity,
				Section: "nested", Kind: audit.Mode,
				Message: fmt.Sprintf("baseline=%s post_run=%s recovered=%t", audit.BaselineFingerprint, audit.PostRunFingerprint, audit.Recovered),
				SourceRefs: []observability.SourceRef{{
					Table: "read_only_enforcement", RecordID: child.RunID,
					DeliveryRunID: child.RunID, Provenance: "durable-read-only-audit",
				}},
			})
			for _, violation := range audit.Violations {
				evidence = append(evidence, observability.Evidence{
					Type: "read-only-policy-violation", Code: violation.Code, Severity: "error",
					Section: "nested", Kind: violation.Surface,
					Message: fmt.Sprintf("target=%s before=%s after=%s", violation.TargetID, violation.BeforeHash, violation.AfterHash),
					SourceRefs: []observability.SourceRef{{
						Table: "read_only_enforcement", RecordID: violation.TargetID,
						DeliveryRunID: child.RunID, Field: violation.Surface, Provenance: "durable-read-only-audit",
					}},
				})
			}
		}
		if manifest := child.MutationManifest; manifest != nil {
			severity := "info"
			if manifest.Verification != "passed" {
				severity = "error"
			}
			evidence = append(evidence, observability.Evidence{
				Type: "bounded-write-manifest", Code: manifest.Verification, Severity: severity,
				Section: "nested", Kind: manifest.Mode,
				Message: fmt.Sprintf("base=%s manifest=%s changes=%d violations=%d recovered=%t", manifest.BaseRevision, manifest.ManifestFingerprint, len(manifest.Changes), len(manifest.Violations), manifest.Recovered),
				SourceRefs: []observability.SourceRef{{
					Table: "mutation_manifest", RecordID: child.RunID,
					DeliveryRunID: child.RunID, Provenance: "durable-bounded-write-audit",
				}},
			})
			for _, violation := range manifest.Violations {
				evidence = append(evidence, observability.Evidence{
					Type: "bounded-write-policy-violation", Code: violation.Code, Severity: "error",
					Section: "nested", Kind: violation.Surface,
					Message: fmt.Sprintf("target=%s before=%s after=%s", violation.TargetID, violation.BeforeHash, violation.AfterHash),
					SourceRefs: []observability.SourceRef{{
						Table: "mutation_manifest", RecordID: violation.TargetID,
						DeliveryRunID: child.RunID, Field: violation.Surface, Provenance: "durable-bounded-write-audit",
					}},
				})
			}
		}
		if report.Outcome == orchestration.NestedOutcomePermissionNotEnforceable {
			provider := ""
			if report.ExecutorCapability != nil {
				provider = report.ExecutorCapability.Provider
			}
			items = append(items, observability.RenderItem{
				ID:         firstNonEmptyNested(child.ChildKey, child.ID, "nested-permission-refusal"),
				Kind:       "nested-permission-preflight",
				Status:     child.Status,
				Provider:   provider,
				Permission: child.Permission,
				Reason:     child.Reason,
				NextAction: child.NextAction,
				Confidence: "exact",
				Freshness:  "preflight",
				SourceRefs: []observability.SourceRef{{
					Table:         "nested_plan_input",
					RecordID:      firstNonEmptyNested(child.ID, child.ChildKey, "nested-permission-refusal"),
					DeliveryRunID: report.ParentRunID,
					Field:         "permission",
					Provenance:    "ephemeral_preflight",
				}},
			})
			continue
		}
		if child.Report != nil {
			items = append(items, observability.ItemFromReport(child.RunID, "nested-child", child.Status, child.Reason, child.NextAction, *child.Report, []observability.SourceRef{{
				Table:         "run_edges",
				RecordID:      child.ID,
				DeliveryRunID: child.RunID,
				Provenance:    "durable-nested-run",
			}}))
			continue
		}
		items = append(items, observability.RenderItem{
			ID:         child.RunID,
			Kind:       "nested-child",
			Status:     child.Status,
			Reason:     child.Reason,
			NextAction: child.NextAction,
			Confidence: "exact",
			Freshness:  "durable",
			SourceRefs: []observability.SourceRef{{
				Table:         "run_edges",
				RecordID:      child.ID,
				DeliveryRunID: child.RunID,
				Provenance:    "durable-nested-run",
			}},
		})
	}
	return observability.NewDocument("nested", observability.Correlation{
		DeliveryRunID: report.ParentRunID,
		Source:        "nested",
	}, items, evidence)
}

func recoveryObservability(opts recovery.Options, result recovery.Result) observability.Document {
	items := make([]observability.RenderItem, 0, 1+len(result.RecoveryAttempts))
	if result.DispatchResult != nil && result.DispatchResult.Report != nil {
		dispatch := result.DispatchResult
		items = append(items, observability.ItemFromReport("issue-"+strconv.Itoa(dispatch.Issue), "recovery-dispatch", dispatch.Status, dispatch.Reason, dispatch.NextAction, *dispatch.Report, []observability.SourceRef{{
			Table:         "recovery_attempts",
			RecordID:      dispatch.AttemptPath,
			DeliveryRunID: dispatch.RunID,
			Provenance:    "recover",
		}}))
	}
	for _, attempt := range result.RecoveryAttempts {
		id := "issue-" + strconv.Itoa(attempt.Issue) + "-attempt-" + strconv.Itoa(attempt.Attempt)
		items = append(items, observability.RenderItem{
			ID:         id,
			Kind:       "recovery-attempt",
			Status:     attempt.Status,
			Provider:   firstNonEmptyCLI(opts.Provider, "not-reported"),
			Model:      attempt.Model,
			Effort:     attempt.Effort,
			Reason:     attempt.Error,
			NextAction: string(result.Action),
			Confidence: "exact",
			Freshness:  "durable",
			SourceRefs: []observability.SourceRef{{
				Table:         "recovery_attempts",
				RecordID:      firstNonEmptyCLI(attempt.RecoveryContextPath, id),
				DeliveryRunID: attempt.RunID,
				Provenance:    "recover",
			}},
		})
	}
	if len(items) == 0 {
		items = append(items, observability.RenderItem{
			ID:         "issue-" + strconv.Itoa(opts.IssueNumber),
			Kind:       "recovery",
			Status:     string(result.Action),
			Provider:   firstNonEmptyCLI(opts.Provider, "not-reported"),
			Model:      opts.Model,
			Effort:     opts.Effort,
			NextAction: string(result.Action),
			Confidence: "exact",
			Freshness:  "durable",
			SourceRefs: []observability.SourceRef{{
				Table:         "recovery",
				RecordID:      "issue-" + strconv.Itoa(opts.IssueNumber),
				DeliveryRunID: opts.RunID,
				Provenance:    "recover",
			}},
		})
	}
	return observability.NewDocument("recover", observability.Correlation{DeliveryRunID: opts.RunID, Source: "recover"}, items, nil)
}
