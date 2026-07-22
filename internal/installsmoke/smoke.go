package installsmoke

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/packdarwin"
	"github.com/jasonhnd/loopcoder/internal/privacy"
)

// StepID is one smoke step.
type StepID string

const (
	StepVerifyChecksum   StepID = "verify_checksum_signature"
	StepInstall          StepID = "install"
	StepVersionHelp      StepID = "version_help"
	StepDoctor           StepID = "doctor"
	StepGlobalLayout     StepID = "global_layout"
	StepDirectFixture    StepID = "clean_direct_fixture"
	StepStatusEvents     StepID = "status_events_cancel_resume"
	StepUninstallGuide   StepID = "uninstall_guidance"
	StepV08ExportImport  StepID = "v08_export_import_idempotent"
	StepNoRepoState      StepID = "no_repo_local_state"
	StepPrivateRedaction StepID = "private_redaction_markers"
	StepProcessCleanup   StepID = "process_resource_cleanup"
	StepDBIntegrity      StepID = "db_integrity_after_interrupt"
)

// StepResult is one step outcome.
type StepResult struct {
	ID      StepID   `json:"id"`
	Passed  bool     `json:"passed"`
	Reasons []string `json:"reasons,omitempty"`
}

// Report is the smoke report bound to an exact archive digest.
type Report struct {
	Schema        string `json:"schema"`
	ArchiveDigest string `json:"archive_digest"`
	Platform      string `json:"platform"`
	// RebuiltDuringSmoke must be false.
	RebuiltDuringSmoke bool         `json:"rebuilt_during_smoke"`
	Steps              []StepResult `json:"steps"`
	Passed             bool         `json:"passed"`
}

// SchemaReport id.
const SchemaReport = "loopcoder.installsmoke.report.v1"

// ArtifactRef is the exact V090-081 archive under test.
type ArtifactRef struct {
	Digest   string
	Platform string
	// LocalDev forbidden.
	LocalDev bool
}

// Environment is the consumer env under test.
type Environment struct {
	// CleanInstall true for green-field home.
	CleanInstall bool
	// UpgradeFromV08 true when migrating fixtures.
	UpgradeFromV08 bool
	// Home layout present.
	GlobalHomeOK bool
	// RepoLocalRuntimeWrite attempted (must fail).
	RepoLocalRuntimeWrite bool
	// OldV08SourceHash before/after export.
	OldV08SourceHashBefore string
	OldV08SourceHashAfter  string
	// PrivateMarkersInjected for redaction check.
	PrivateMarkersInjected bool
	PrivateMarkersLeaked   bool
	// ProcessCleanupOK after interrupt.
	ProcessCleanupOK bool
	// DBIntegrityOK after abrupt interrupt.
	DBIntegrityOK bool
	// Commands observed.
	VersionOK, HelpOK, DoctorOK bool
	// Direct fixture run.
	DirectFixtureOK bool
	// Status/events/cancel/resume.
	LifecycleOK bool
	// Checksum/signature verified against digest.
	ChecksumOK bool
	// Install succeeded for exact digest.
	InstallOK bool
	// Uninstall guidance present.
	UninstallGuideOK bool
	// Export/import idempotent.
	ExportImportIdempotent bool
	// Rebuilt binary during smoke (forbidden).
	Rebuilt bool
}

// Run executes the pure smoke policy against environment facts.
func Run(art ArtifactRef, env Environment) Report {
	rep := Report{
		Schema: SchemaReport, ArchiveDigest: art.Digest,
		Platform: packdarwin.Platform, RebuiltDuringSmoke: env.Rebuilt,
	}
	if art.Platform != "" && art.Platform != packdarwin.Platform {
		rep.Steps = []StepResult{{ID: StepVerifyChecksum, Passed: false, Reasons: []string{"platform not darwin/arm64"}}}
		return rep
	}
	if art.LocalDev {
		rep.Steps = []StepResult{{ID: StepInstall, Passed: false, Reasons: []string{"local_dev artifact forbidden"}}}
		return rep
	}
	if art.Digest == "" {
		rep.Steps = []StepResult{{ID: StepVerifyChecksum, Passed: false, Reasons: []string{"archive digest required"}}}
		return rep
	}

	steps := []struct {
		id StepID
		fn func() (bool, []string)
	}{
		{StepVerifyChecksum, func() (bool, []string) {
			if !env.ChecksumOK {
				return false, []string{"checksum/signature verification failed"}
			}
			return true, nil
		}},
		{StepInstall, func() (bool, []string) {
			if env.Rebuilt {
				return false, []string{"must not rebuild during smoke; use exact V090-081 archive"}
			}
			if !env.InstallOK {
				return false, []string{"install failed"}
			}
			return true, nil
		}},
		{StepVersionHelp, func() (bool, []string) {
			if !env.VersionOK || !env.HelpOK {
				return false, []string{"version/help failed"}
			}
			return true, nil
		}},
		{StepDoctor, func() (bool, []string) {
			if !env.DoctorOK {
				return false, []string{"doctor failed"}
			}
			return true, nil
		}},
		{StepGlobalLayout, func() (bool, []string) {
			if !env.GlobalHomeOK {
				return false, []string{"global home layout missing"}
			}
			return true, nil
		}},
		{StepDirectFixture, func() (bool, []string) {
			if !env.DirectFixtureOK {
				return false, []string{"direct fixture run failed"}
			}
			return true, nil
		}},
		{StepStatusEvents, func() (bool, []string) {
			if !env.LifecycleOK {
				return false, []string{"status/events/cancel/resume failed"}
			}
			return true, nil
		}},
		{StepUninstallGuide, func() (bool, []string) {
			if !env.UninstallGuideOK {
				return false, []string{"uninstall guidance missing"}
			}
			return true, nil
		}},
		{StepV08ExportImport, func() (bool, []string) {
			if env.UpgradeFromV08 {
				if env.OldV08SourceHashBefore == "" || env.OldV08SourceHashBefore != env.OldV08SourceHashAfter {
					return false, []string{"v0.8 source hash changed during export"}
				}
				if !env.ExportImportIdempotent {
					return false, []string{"export/import not idempotent"}
				}
			}
			return true, nil
		}},
		{StepNoRepoState, func() (bool, []string) {
			if env.RepoLocalRuntimeWrite {
				return false, []string{"production wrote repo-local runtime state"}
			}
			return true, nil
		}},
		{StepPrivateRedaction, func() (bool, []string) {
			if env.PrivateMarkersInjected && env.PrivateMarkersLeaked {
				return false, []string{"private markers leaked"}
			}
			return true, nil
		}},
		{StepProcessCleanup, func() (bool, []string) {
			if !env.ProcessCleanupOK {
				return false, []string{"process/resource cleanup failed"}
			}
			return true, nil
		}},
		{StepDBIntegrity, func() (bool, []string) {
			if !env.DBIntegrityOK {
				return false, []string{"database integrity failed after interrupt"}
			}
			return true, nil
		}},
	}

	all := true
	for _, s := range steps {
		ok, reasons := s.fn()
		rep.Steps = append(rep.Steps, StepResult{ID: s.id, Passed: ok, Reasons: reasons})
		if !ok {
			all = false
		}
	}
	rep.Passed = all && !env.Rebuilt
	return rep
}

// FixtureEnvironment returns a fully green fixture for unit tests.
func FixtureEnvironment(digest string) (ArtifactRef, Environment) {
	return ArtifactRef{Digest: digest, Platform: packdarwin.Platform}, Environment{
		CleanInstall: true, GlobalHomeOK: true, ChecksumOK: true, InstallOK: true,
		VersionOK: true, HelpOK: true, DoctorOK: true, DirectFixtureOK: true,
		LifecycleOK: true, UninstallGuideOK: true, ProcessCleanupOK: true, DBIntegrityOK: true,
		UpgradeFromV08: true, OldV08SourceHashBefore: "abc", OldV08SourceHashAfter: "abc",
		ExportImportIdempotent: true, PrivateMarkersInjected: true, PrivateMarkersLeaked: false,
	}
}

// AssertNoMarkerLeak scans a sample output string.
func AssertNoMarkerLeak(sample string) error {
	if privacy.ContainsAnyMarker(sample) {
		return fmt.Errorf("marker leak in smoke output")
	}
	// also check common secret shapes via redaction residual
	out := privacy.RedactFor(privacy.DestCIArtifact, sample)
	if strings.Contains(out, "ghp_") {
		return fmt.Errorf("token residual")
	}
	return nil
}

// StepIDs returns stable ordered step ids.
func StepIDs() []StepID {
	ids := []StepID{
		StepVerifyChecksum, StepInstall, StepVersionHelp, StepDoctor, StepGlobalLayout,
		StepDirectFixture, StepStatusEvents, StepUninstallGuide, StepV08ExportImport,
		StepNoRepoState, StepPrivateRedaction, StepProcessCleanup, StepDBIntegrity,
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
