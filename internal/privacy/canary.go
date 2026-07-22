package privacy

import (
	"encoding/json"
	"fmt"
)

// SurfaceBundle is a pure in-memory fixture of the destinations that must stay
// free of private markers after redaction. Used by the consumer canary.
type SurfaceBundle struct {
	MachineGlobalDB  map[string]any
	GlobalStatus     string
	UnrelatedProject map[string]any
	HostDiagnostics  string
	CIArtifact       string
	ReleaseManifest  map[string]any
	PRBody           string
	ErrorPath        string
	MachineSummary   map[string]any
	// Owning project surfaces may keep policy-authorized bounded content after
	// redaction of credentials; canary still requires markers redacted.
	ProjectEvents string
	ProjectLogs   string
}

// InjectedPrivate is a synthetic private payload used to contaminate raw
// inputs before redaction. Never written to real stores in CI.
type InjectedPrivate struct {
	IssueBody   string
	CodeSnippet string
	Prompt      string
	LocalPath   string
	AccountID   string
	Credential  string
	Output      string
}

// DefaultInjected returns the standard synthetic private payload.
func DefaultInjected() InjectedPrivate {
	return InjectedPrivate{
		IssueBody:   "issue mentions " + MarkerIssue,
		CodeSnippet: "code block " + MarkerCode,
		Prompt:      "prompt " + MarkerPrompt,
		LocalPath:   MarkerPath,
		AccountID:   MarkerAccount,
		Credential:  MarkerCredential,
		Output:      "model said " + MarkerOutput,
	}
}

// BuildContaminatedRaw builds raw (unredacted) surfaces that deliberately
// include private markers — simulating a bug that forgot redaction.
func BuildContaminatedRaw(inj InjectedPrivate) SurfaceBundle {
	return SurfaceBundle{
		MachineGlobalDB: map[string]any{
			"projects": []any{
				map[string]any{
					"project_id": "proj-1",
					"path":       inj.LocalPath,
					"issue_body": inj.IssueBody,
					"token":      inj.Credential,
				},
			},
		},
		GlobalStatus: "status path=" + inj.LocalPath + " account=" + inj.AccountID,
		UnrelatedProject: map[string]any{
			"leaked_prompt": inj.Prompt,
			"leaked_output": inj.Output,
		},
		HostDiagnostics: "diag code=" + inj.CodeSnippet + " cred=" + inj.Credential,
		CIArtifact:      "artifact " + inj.IssueBody + " " + inj.Output,
		ReleaseManifest: map[string]any{
			"notes": inj.IssueBody,
			"path":  inj.LocalPath,
		},
		PRBody:         "Closes private: " + inj.IssueBody + " path=" + inj.LocalPath,
		ErrorPath:      "error at " + inj.LocalPath + " secret=" + inj.Credential,
		MachineSummary: map[string]any{"path": inj.LocalPath, "account": inj.AccountID},
		ProjectEvents:  `{"event":"attempt","prompt":"` + inj.Prompt + `","token":"` + inj.Credential + `"}`,
		ProjectLogs:    "log line " + inj.Output + " " + inj.Credential,
	}
}

// RedactBundle applies RedactFor to every surface in the bundle.
func RedactBundle(raw SurfaceBundle) SurfaceBundle {
	return SurfaceBundle{
		MachineGlobalDB:  redactMap(DestMachineGlobalDB, raw.MachineGlobalDB),
		GlobalStatus:     RedactFor(DestGlobalStatus, raw.GlobalStatus),
		UnrelatedProject: redactMap(DestUnrelatedProject, raw.UnrelatedProject),
		HostDiagnostics:  RedactFor(DestHostDiagnostics, raw.HostDiagnostics),
		CIArtifact:       RedactFor(DestCIArtifact, raw.CIArtifact),
		ReleaseManifest:  redactMap(DestReleaseManifest, raw.ReleaseManifest),
		PRBody:           RedactFor(DestPRBody, raw.PRBody),
		ErrorPath:        RedactFor(DestErrorPath, raw.ErrorPath),
		MachineSummary:   redactMap(DestMachineSummary, raw.MachineSummary),
		ProjectEvents:    RedactFor(DestProjectEvents, raw.ProjectEvents),
		ProjectLogs:      RedactFor(DestProjectLogs, raw.ProjectLogs),
	}
}

// ScanBundle scans every surface; returns all findings.
func ScanBundle(b SurfaceBundle) []Finding {
	var out []Finding
	out = append(out, ScanMap(DestMachineGlobalDB, "machine_global_db", b.MachineGlobalDB)...)
	out = append(out, ScanText(DestGlobalStatus, "global_status", b.GlobalStatus)...)
	out = append(out, ScanMap(DestUnrelatedProject, "unrelated_project", b.UnrelatedProject)...)
	out = append(out, ScanText(DestHostDiagnostics, "host_diagnostics", b.HostDiagnostics)...)
	out = append(out, ScanText(DestCIArtifact, "ci_artifact", b.CIArtifact)...)
	out = append(out, ScanMap(DestReleaseManifest, "release_manifest", b.ReleaseManifest)...)
	out = append(out, ScanText(DestPRBody, "pr_body", b.PRBody)...)
	out = append(out, ScanText(DestErrorPath, "error_path", b.ErrorPath)...)
	out = append(out, ScanMap(DestMachineSummary, "machine_summary", b.MachineSummary)...)
	out = append(out, ScanLines(DestProjectEvents, "project_events", b.ProjectEvents)...)
	out = append(out, ScanLines(DestProjectLogs, "project_logs", b.ProjectLogs)...)
	return out
}

// RunConsumerCanary builds contaminated surfaces, redacts them, and asserts
// no synthetic private marker remains on any scanned surface. Also verifies
// fail-closed GitHub access for unknown visibility and wrong repo.
func RunConsumerCanary() error {
	raw := BuildContaminatedRaw(DefaultInjected())
	// Contaminated raw must be dirty (canary self-check).
	if dirty := ScanBundle(raw); len(dirty) == 0 {
		return fmt.Errorf("consumer canary self-check failed: raw bundle had no markers")
	}
	clean := RedactBundle(raw)
	if err := AssertClean(ScanBundle(clean)); err != nil {
		return fmt.Errorf("consumer canary redaction failed: %w", err)
	}
	// GitHub fail-closed cases.
	unknown := EvaluateRepoAccess(RepoAccessRequest{
		Owner: "acme", Name: "private-app", Visibility: VisibilityUnknown,
		Authorized: true, Requested: LeastPermissions(),
	})
	if unknown.Allowed {
		return fmt.Errorf("consumer canary: unknown visibility must fail closed")
	}
	unauth := EvaluateRepoAccess(RepoAccessRequest{
		Owner: "acme", Name: "private-app", Visibility: VisibilityPrivate,
		Authorized: false, Requested: LeastPermissions(),
	})
	if unauth.Allowed {
		return fmt.Errorf("consumer canary: unauthorized repo must fail closed")
	}
	forbidden := EvaluateRepoAccess(RepoAccessRequest{
		Owner: "acme", Name: "private-app", Visibility: VisibilityPrivate,
		Authorized: true, Requested: []GitHubPermission{PermAdmin},
	})
	if forbidden.Allowed {
		return fmt.Errorf("consumer canary: admin permission must fail closed")
	}
	wrong := WrongRepoAccess("acme", "private-app", "acme", "other-app")
	if wrong.Allowed {
		return fmt.Errorf("consumer canary: wrong repo must fail closed")
	}
	ok := EvaluateRepoAccess(RepoAccessRequest{
		Owner: "acme", Name: "private-app", Visibility: VisibilityPrivate,
		Authorized: true, Requested: LeastPermissions(), ExpectPrivate: true,
	})
	if !ok.Allowed {
		return fmt.Errorf("consumer canary: least-privilege private access should be allowed: %v", ok.Reasons)
	}
	// Credentials never allowed anywhere in policy table.
	for _, d := range AllDestinations() {
		if Allowed(ClassCredentials, d) {
			return fmt.Errorf("consumer canary: credentials allowed at %s", d)
		}
	}
	return nil
}

// CanaryReport is a redacted machine-readable canary result for evidence.
type CanaryReport struct {
	Schema  string   `json:"schema"`
	Passed  bool     `json:"passed"`
	Reasons []string `json:"reasons,omitempty"`
}

// SchemaCanary is the report schema id.
const SchemaCanary = "loopcoder.privacy.consumer_canary.v1"

// ReportCanary runs the canary and returns a JSON-serializable report that
// never embeds synthetic marker values.
func ReportCanary() CanaryReport {
	if err := RunConsumerCanary(); err != nil {
		// Strip any accidental marker residue from the error string.
		msg := RedactFor(DestCIArtifact, err.Error())
		return CanaryReport{Schema: SchemaCanary, Passed: false, Reasons: []string{msg}}
	}
	return CanaryReport{Schema: SchemaCanary, Passed: true, Reasons: []string{"all privacy surfaces clean; github fail-closed ok"}}
}

// ReportCanaryJSON is the stable JSON encoding of ReportCanary.
func ReportCanaryJSON() ([]byte, error) {
	return json.Marshal(ReportCanary())
}

func redactMap(dest Destination, m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = redactAny(dest, v)
	}
	return out
}

func redactAny(dest Destination, v any) any {
	switch t := v.(type) {
	case string:
		return RedactFor(dest, t)
	case map[string]any:
		return redactMap(dest, t)
	case []any:
		out := make([]any, len(t))
		for i, el := range t {
			out[i] = redactAny(dest, el)
		}
		return out
	default:
		return v
	}
}
