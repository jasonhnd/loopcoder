// Package securitypolicy defines the v0.9.0 security vocabulary for data
// classes and deny-by-default capabilities (V090-084 / #1093).
//
// It intentionally contains no credential storage, sandbox, or provider policy.
// Later issues (V090-085, V090-003, direct-run) must reference these identifiers
// instead of inventing parallel classification strings.
package securitypolicy

// DataClass is the durable classification for persisted, rendered, or exported
// fields. Unknown classes must be rejected by callers (fail closed).
type DataClass string

const (
	ClassCredential       DataClass = "credential"
	ClassPrivateContent   DataClass = "private_content"
	ClassOperatorReport   DataClass = "operator_report"
	ClassRawLog           DataClass = "raw_log"
	ClassPathLocal        DataClass = "path_local"
	ClassRouteEvidence    DataClass = "route_evidence"
	ClassQuotaObservation DataClass = "quota_observation"
	ClassPublicMetadata   DataClass = "public_metadata"
	ClassSyntheticFixture DataClass = "synthetic_fixture"
)

// DataClassSpec freezes allowed scope, retention owner, and default redaction.
type DataClassSpec struct {
	ID               DataClass
	AllowedScope     string
	RetentionOwner   string
	DefaultRedaction string
}

// DataClasses returns the authoritative class table for v0.9.
func DataClasses() []DataClassSpec {
	return []DataClassSpec{
		{ClassCredential, "never in LoopCoder storage or render surfaces", "operator/provider/OS keychain", "full suppress"},
		{ClassPrivateContent, "local project DB/logs under run ownership only", "project store + log retention", "summarize/drop for export; sanitize free text"},
		{ClassOperatorReport, "local UI stream + project events", "project events / UI ack ledger", "sanitize paths and secrets"},
		{ClassRawLog, "local log directory only", "project log retention", "never copy to PR/issue/commit"},
		{ClassPathLocal, "local diagnostics only", "ephemeral or hashed identity", "redact absolute paths"},
		{ClassRouteEvidence, "project events and status projections", "project store", "public codes only"},
		{ClassQuotaObservation, "machine authority snapshots", "machine store", "no secrets; preserve unknown"},
		{ClassPublicMetadata, "any surface including PR/release notes", "GitHub / release artifacts", "none beyond accuracy"},
		{ClassSyntheticFixture, "test temp dirs and fixtures only", "test process lifetime", "must not equal real secrets"},
	}
}

// KnownDataClass reports whether id is part of the v0.9 vocabulary.
func KnownDataClass(id DataClass) bool {
	for _, spec := range DataClasses() {
		if spec.ID == id {
			return true
		}
	}
	return false
}
