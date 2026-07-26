package effectivepolicy

// SchemaVersion is the effective-policy document schema. Incompatible versions
// fail closed before any side effect.
const SchemaVersion = 1

// Source identifies where an effective value came from.
type Source string

const (
	SourceAbsent        Source = "absent"
	SourceExplicitCLI   Source = "explicit_cli"
	SourceRunRequest    Source = "approved_run_request"
	SourceProjectPolicy Source = "project_policy"
	SourceUserLocal     Source = "user_local"
	SourceDefault       Source = "compiled_default"
	SourceCompatibility Source = "compatibility"
	SourceInvalid       Source = "invalid"
)

// Rank returns merge precedence. Higher rank wins. Explicit CLI is highest.
func (s Source) Rank() int {
	switch s {
	case SourceExplicitCLI:
		return 50
	case SourceRunRequest:
		return 40
	case SourceProjectPolicy:
		return 30
	case SourceUserLocal:
		return 20
	case SourceDefault:
		return 10
	case SourceCompatibility:
		return 5
	default:
		return 0
	}
}

// Field keys present in an effective-policy snapshot.
const (
	FieldProvider          = "provider"
	FieldModel             = "model"
	FieldEffort            = "effort"
	FieldPermission        = "permission"
	FieldReportClient      = "report_client"
	FieldBaseBranch        = "base_branch"
	FieldMaxChildProcesses = "max_child_processes"
	FieldMaxRSSMiB         = "max_rss_mib"
	FieldRetentionDays     = "retention_days"
	FieldProjectPolicyPath = "project_policy_path"
	FieldNativeSubagents   = "native_subagents"
)

// PinFields are route pins that environment/host/routing must never override
// once set by a higher-rank source.
var PinFields = []string{
	FieldProvider,
	FieldModel,
	FieldEffort,
	FieldPermission,
	FieldReportClient,
	FieldBaseBranch,
	FieldNativeSubagents,
}
