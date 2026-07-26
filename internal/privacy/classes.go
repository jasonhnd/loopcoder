package privacy

// DataClass is a sensitivity class for content that may appear in LoopCoder
// surfaces. Classes are exclusive for conformance: a value is classified once
// and destination policy is evaluated against that class only.
type DataClass string

const (
	// ClassPublicIdentity is durable public identity (project_id, short name,
	// owner login, provider family name). Safe for machine-global summaries.
	ClassPublicIdentity DataClass = "public_identity"

	// ClassProjectPrivateMeta is project-local metadata (issue numbers already
	// public on that repo, bounded status codes, attempt ids). Stays in the
	// owning project store/events only.
	ClassProjectPrivateMeta DataClass = "project_private_metadata"

	// ClassCodePromptOutput is issue body text, code, prompts, model output,
	// full local paths, branch names that embed private content. Never leaves
	// the owning project payload without redaction.
	ClassCodePromptOutput DataClass = "code_prompt_output"

	// ClassCredentials is tokens, API keys, keychain material, auth headers.
	// Never persisted or rendered on any surface.
	ClassCredentials DataClass = "credentials"

	// ClassQuotaAccount is provider quota/account identifiers and soft-mode
	// reasons that may include account-scoped strings. Machine summary may
	// carry only aggregated counts, never account identifiers.
	ClassQuotaAccount DataClass = "quota_account"

	// ClassDiagnostics is host diagnostics, support bundles, CI artifacts,
	// release evidence manifests. Must be redacted to public identity +
	// aggregate counts only.
	ClassDiagnostics DataClass = "diagnostics"
)

// Destination is a sink that may receive content.
type Destination string

const (
	DestMachineGlobalDB  Destination = "machine_global_db"
	DestGlobalStatus     Destination = "global_status"
	DestUnrelatedProject Destination = "unrelated_project"
	DestHostDiagnostics  Destination = "host_diagnostics"
	DestCIArtifact       Destination = "ci_artifact"
	DestReleaseManifest  Destination = "release_manifest"
	DestProjectEvents    Destination = "project_events"
	DestProjectLogs      Destination = "project_logs"
	DestPRBody           Destination = "pr_body"
	DestErrorPath        Destination = "error_path"
	DestMachineSummary   Destination = "machine_summary"
	DestJSONHumanOutput  Destination = "json_human_output"
)

// AllDataClasses returns the closed set of data classes.
func AllDataClasses() []DataClass {
	return []DataClass{
		ClassPublicIdentity,
		ClassProjectPrivateMeta,
		ClassCodePromptOutput,
		ClassCredentials,
		ClassQuotaAccount,
		ClassDiagnostics,
	}
}

// AllDestinations returns the closed set of destinations under conformance.
func AllDestinations() []Destination {
	return []Destination{
		DestMachineGlobalDB,
		DestGlobalStatus,
		DestUnrelatedProject,
		DestHostDiagnostics,
		DestCIArtifact,
		DestReleaseManifest,
		DestProjectEvents,
		DestProjectLogs,
		DestPRBody,
		DestErrorPath,
		DestMachineSummary,
		DestJSONHumanOutput,
	}
}

// Allowed reports whether class may appear unredacted at dest.
// Credentials never allowed anywhere. Code/prompt/output never allowed on
// machine-global, host, CI, release, unrelated project, or PR body surfaces.
func Allowed(class DataClass, dest Destination) bool {
	if class == ClassCredentials {
		return false
	}
	switch dest {
	case DestMachineGlobalDB, DestGlobalStatus, DestUnrelatedProject,
		DestHostDiagnostics, DestCIArtifact, DestReleaseManifest,
		DestMachineSummary:
		return class == ClassPublicIdentity
	case DestPRBody, DestErrorPath, DestJSONHumanOutput:
		// Public identity + project-private meta only (issue numbers, status).
		return class == ClassPublicIdentity || class == ClassProjectPrivateMeta
	case DestProjectEvents, DestProjectLogs:
		// Owning project may retain authorized bounded content; credentials
		// already rejected above. Code/prompt/output only when policy-bounded
		// (caller still must apply size/retention caps).
		return class != ClassCredentials
	default:
		// Fail closed on unknown destinations.
		return false
	}
}

// PolicyTable returns a stable map class → allowed destinations for docs/tests.
func PolicyTable() map[DataClass][]Destination {
	out := make(map[DataClass][]Destination, len(AllDataClasses()))
	for _, c := range AllDataClasses() {
		var allowed []Destination
		for _, d := range AllDestinations() {
			if Allowed(c, d) {
				allowed = append(allowed, d)
			}
		}
		out[c] = allowed
	}
	return out
}
