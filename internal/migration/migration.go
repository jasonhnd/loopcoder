// Package migration owns one-version compatibility names for breaking-release
// transitions.
package migration

import (
	"fmt"
	"strings"
)

type Surface string

const (
	SurfaceEnv         Surface = "env"
	SurfaceHookCommand Surface = "hook-command"
	SurfaceConfigKey   Surface = "config-key"
)

const (
	ReporterScopeEnv       = "LOOPCODER_CONDUCTOR_REPORTER_SCOPE"
	LegacyReporterScopeEnv = "LOOPCODER_CONDUCTOR_ATTEST_SCOPE"

	ReporterStateDirEnv       = "LOOPCODER_CONDUCTOR_REPORTER_STATE_DIR"
	LegacyReporterStateDirEnv = "LOOPCODER_CONDUCTOR_ATTEST_STATE_DIR"

	ReporterHookName       = "conductor-reporter"
	LegacyReporterHookName = "conductor-attest"

	ReporterHookCommand       = "loopcoder hook " + ReporterHookName
	LegacyReporterHookCommand = "loopcoder hook " + LegacyReporterHookName

	ReportConfigRoot          = "report"
	LegacyReportConfigRoot    = "attestation"
	ReportConfigChannel       = ReportConfigRoot + ".channel"
	LegacyReportConfigChannel = LegacyReportConfigRoot + ".channel"

	ReportStateKey       = ReportConfigRoot
	LegacyReportStateKey = LegacyReportConfigRoot
)

type Entry struct {
	Surface    Surface
	Legacy     string
	Current    string
	FixCommand string
}

type Diagnostic struct {
	Surface    Surface
	Legacy     string
	Current    string
	FixCommand string
	Conflict   bool
	Detail     string
}

var reporterRenameEntries = []Entry{
	{
		Surface:    SurfaceEnv,
		Legacy:     LegacyReporterScopeEnv,
		Current:    ReporterScopeEnv,
		FixCommand: "set LOOPCODER_CONDUCTOR_REPORTER_SCOPE to the same value and unset LOOPCODER_CONDUCTOR_ATTEST_SCOPE",
	},
	{
		Surface:    SurfaceEnv,
		Legacy:     LegacyReporterStateDirEnv,
		Current:    ReporterStateDirEnv,
		FixCommand: "set LOOPCODER_CONDUCTOR_REPORTER_STATE_DIR to the same value and unset LOOPCODER_CONDUCTOR_ATTEST_STATE_DIR",
	},
	{
		Surface:    SurfaceHookCommand,
		Legacy:     LegacyReporterHookCommand,
		Current:    ReporterHookCommand,
		FixCommand: "loopcoder skill install --repo .",
	},
	{
		Surface:    SurfaceConfigKey,
		Legacy:     LegacyReportConfigRoot,
		Current:    ReportConfigRoot,
		FixCommand: "loopcoder doctor --repo . --fix",
	},
	{
		Surface:    SurfaceConfigKey,
		Legacy:     LegacyReportConfigChannel,
		Current:    ReportConfigChannel,
		FixCommand: "loopcoder doctor --repo . --fix",
	},
}

func ReporterRenameEntries() []Entry {
	return append([]Entry(nil), reporterRenameEntries...)
}

func ReporterRenameEntry(legacy string) (Entry, bool) {
	for _, entry := range reporterRenameEntries {
		if entry.Legacy == legacy {
			return entry, true
		}
	}
	return Entry{}, false
}

func NewDiagnostic(entry Entry, conflict bool, detail string) Diagnostic {
	return Diagnostic{
		Surface:    entry.Surface,
		Legacy:     entry.Legacy,
		Current:    entry.Current,
		FixCommand: entry.FixCommand,
		Conflict:   conflict,
		Detail:     strings.TrimSpace(detail),
	}
}

func (d Diagnostic) String() string {
	status := "legacy"
	if d.Conflict {
		status = "conflicting legacy"
	}
	message := fmt.Sprintf("%s %s %q accepted as %q", status, d.Surface, d.Legacy, d.Current)
	if d.Conflict {
		message += "; new name wins"
	}
	if d.Detail != "" {
		message += "; " + d.Detail
	}
	if d.FixCommand != "" {
		message += "; fix: " + d.FixCommand
	}
	return message
}

func ResolveReporterScopeEnv(env func(string) string) (string, []Diagnostic) {
	return ResolveEnv(env, ReporterScopeEnv, LegacyReporterScopeEnv)
}

func ResolveReporterStateDirEnv(env func(string) string) (string, []Diagnostic) {
	return ResolveEnv(env, ReporterStateDirEnv, LegacyReporterStateDirEnv)
}

func ResolveEnv(env func(string) string, currentKey string, legacyKey string) (string, []Diagnostic) {
	if env == nil {
		env = func(string) string { return "" }
	}
	currentValue := strings.TrimSpace(env(currentKey))
	legacyValue := strings.TrimSpace(env(legacyKey))
	if currentValue == "" && legacyValue == "" {
		return "", nil
	}

	entry, ok := ReporterRenameEntry(legacyKey)
	if !ok {
		entry = Entry{Surface: SurfaceEnv, Legacy: legacyKey, Current: currentKey}
	}
	if legacyValue == "" {
		return currentValue, nil
	}
	if currentValue != "" {
		return currentValue, []Diagnostic{NewDiagnostic(entry, currentValue != legacyValue, "")}
	}
	return legacyValue, []Diagnostic{NewDiagnostic(entry, false, "")}
}

func ReporterEnvDiagnostics(env func(string) string) []Diagnostic {
	var diagnostics []Diagnostic
	if _, found := ResolveReporterScopeEnv(env); len(found) > 0 {
		diagnostics = append(diagnostics, found...)
	}
	if _, found := ResolveReporterStateDirEnv(env); len(found) > 0 {
		diagnostics = append(diagnostics, found...)
	}
	return diagnostics
}
