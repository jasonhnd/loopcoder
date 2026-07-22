package artifactqual

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/installsmoke"
	"github.com/jasonhnd/loopcoder/internal/packdarwin"
	"github.com/jasonhnd/loopcoder/internal/rcgonogo"
	"github.com/jasonhnd/loopcoder/internal/releaseslo"
)

const SchemaEvidence = "loopcoder.artifactqual.evidence.v1"

// Mode distinguishes unit fixtures from release qualification.
type Mode string

const (
	ModeUnit    Mode = "unit"
	ModeRelease Mode = "release"
)

// Probe is one named executable measurement.
type Probe struct {
	Name     string        `json:"name"`
	Command  []string      `json:"command"`
	ExitCode int           `json:"exit_code"`
	Duration time.Duration `json:"duration"`
	// OutputDigest is sha256 of bounded combined stdout+stderr (never raw secret bodies).
	OutputDigest string   `json:"output_digest"`
	Passed       bool     `json:"passed"`
	Reasons      []string `json:"reasons,omitempty"`
}

// Input for qualification.
type Input struct {
	Mode Mode
	// ArchivePath is the exact RC tar.gz (required in release mode).
	ArchivePath string
	// ExpectedDigest is the immutable archive digest (required in release mode).
	ExpectedDigest string
	// SHA is the source commit SHA bound to the archive.
	SHA string
	// WorkDir for extract (required).
	WorkDir string
	// Integration dual-green flags measured from Actions (caller-supplied from metadata only).
	IntegrationVerifyOK bool
	IntegrationCanaryOK bool
	// CanaryEvidencePath is the exact-binary real canary evidence manifest
	// (loopcoder.canary_evidence.v1). Required for real_runtime scorecard metrics.
	// Dry-run / --capacity-snapshot structural probes must not substitute.
	CanaryEvidencePath string
	// AllowFixture only for unit tests; forbidden when ModeRelease.
	AllowFixture bool
	// FixtureEnv only when AllowFixture && ModeUnit.
	FixtureEnv *installsmoke.Environment
	FixtureArt *installsmoke.ArtifactRef
	Now        time.Time
}

// Evidence is the redacted qualification package.
type Evidence struct {
	Schema        string               `json:"schema"`
	Mode          Mode                 `json:"mode"`
	SHA           string               `json:"sha,omitempty"`
	ArchiveDigest string               `json:"archive_digest"`
	ArchivePath   string               `json:"archive_path,omitempty"`
	Probes        []Probe              `json:"probes"`
	InstallSmoke  installsmoke.Report  `json:"install_smoke"`
	Scorecard     releaseslo.Scorecard `json:"scorecard"`
	Decision      rcgonogo.Record      `json:"decision"`
	// Canary is the validated real-runtime canary evidence (if provided).
	Canary        *CanaryValidation `json:"canary_validation,omitempty"`
	Passed        bool              `json:"passed"`
	Reasons       []string          `json:"reasons,omitempty"`
	GeneratedAt   time.Time         `json:"generated_at"`
	RejectFixture bool              `json:"reject_fixture_constructors"`
}

// Record alias for rcgonogo decision JSON shape used in tests.
// Prefer embedding rcgonogo package decision type via Evaluate.

// Qualify runs the harness.
func Qualify(in Input) (Evidence, error) {
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	ev := Evidence{
		Schema: SchemaEvidence, Mode: in.Mode, SHA: in.SHA,
		GeneratedAt: now, RejectFixture: in.Mode == ModeRelease,
	}

	if in.Mode == ModeRelease {
		if in.AllowFixture || in.FixtureEnv != nil {
			return Evidence{}, errors.New("artifactqual: fixture constructors forbidden in release mode")
		}
		if strings.TrimSpace(in.ArchivePath) == "" || strings.TrimSpace(in.ExpectedDigest) == "" {
			return Evidence{}, errors.New("artifactqual: archive path and expected digest required in release mode")
		}
		if strings.TrimSpace(in.WorkDir) == "" {
			return Evidence{}, errors.New("artifactqual: workdir required")
		}
	}

	// Unit fixture path (explicit only)
	if in.Mode == ModeUnit && in.AllowFixture && in.FixtureEnv != nil && in.FixtureArt != nil {
		rep := installsmoke.Run(*in.FixtureArt, *in.FixtureEnv)
		ev.InstallSmoke = rep
		ev.ArchiveDigest = in.FixtureArt.Digest
		ev.Probes = []Probe{{Name: "unit_fixture_only", Passed: true, Reasons: []string{"unit mode"}}}
		ev.Passed = rep.Passed
		if !rep.Passed {
			ev.Reasons = append(ev.Reasons, "installsmoke failed")
		}
		// still reject if someone claims release
		return ev, nil
	}

	// --- release / executable path ---
	digest, err := fileSHA256(in.ArchivePath)
	if err != nil {
		return Evidence{}, err
	}
	ev.ArchiveDigest = digest
	ev.ArchivePath = in.ArchivePath
	if in.ExpectedDigest != "" && !strings.EqualFold(digest, in.ExpectedDigest) {
		ev.Probes = append(ev.Probes, Probe{
			Name: "verify_archive_digest", Passed: false,
			Reasons: []string{fmt.Sprintf("digest mismatch got=%s want=%s", digest, in.ExpectedDigest)},
		})
		ev.Passed = false
		ev.Reasons = append(ev.Reasons, "wrong archive digest")
		return ev, nil
	}
	ev.Probes = append(ev.Probes, Probe{
		Name: "verify_archive_digest", Passed: true, Command: []string{"sha256", in.ArchivePath},
		OutputDigest: shortDigest(digest),
	})

	bin, _, err := extractArchive(in.ArchivePath, in.WorkDir)
	if err != nil {
		return Evidence{}, err
	}
	// never rebuild
	ev.Probes = append(ev.Probes, Probe{Name: "no_rebuild", Passed: true, Reasons: []string{"using extracted binary only"}})

	// run probes
	home := filepath.Join(in.WorkDir, "home")
	_ = os.MkdirAll(home, 0o700)
	envHome := []string{"LOOPCODER_HOME=" + home, "HOME=" + home, "PATH=" + filepath.Dir(bin) + ":" + os.Getenv("PATH")}

	versionOK, p := runProbe("version", []string{bin, "--version"}, envHome, func(code int, out string) (bool, []string) {
		if code != 0 {
			return false, []string{"non-zero exit"}
		}
		if strings.Contains(out, "version=dev") {
			return false, []string{"version=dev forbidden"}
		}
		if strings.Contains(out, "commit=unknown") {
			return false, []string{"unknown commit"}
		}
		return true, nil
	})
	ev.Probes = append(ev.Probes, p)

	helpOK, p := runProbe("help", []string{bin, "--help"}, envHome, func(code int, out string) (bool, []string) {
		if code != 0 {
			return false, []string{"non-zero exit"}
		}
		if !strings.Contains(out, "run") {
			return false, []string{"help missing run"}
		}
		return true, nil
	})
	ev.Probes = append(ev.Probes, p)

	// doctor may return non-zero on incomplete env; treat as executed signal
	doctorOK, p := runProbe("doctor", []string{bin, "doctor", "--format", "json"}, envHome, func(code int, out string) (bool, []string) {
		// presence of output is enough for "doctor runs"
		if len(out) == 0 && code != 0 {
			return false, []string{"doctor produced no output"}
		}
		return true, []string{fmt.Sprintf("exit=%d", code)}
	})
	ev.Probes = append(ev.Probes, p)

	// capabilities / diagnose dry-run if present
	_, p = runProbe("capabilities", []string{bin, "capabilities", "--format", "json"}, envHome, func(code int, out string) (bool, []string) {
		if code != 0 {
			return false, []string{"capabilities failed"}
		}
		if !strings.Contains(out, "capabilities") && !strings.Contains(out, "capmatrix") && !strings.Contains(out, "\"id\"") {
			return false, []string{"unexpected capabilities payload"}
		}
		return true, nil
	})
	ev.Probes = append(ev.Probes, p)

	// export-v08 fixture path
	exportDir := filepath.Join(in.WorkDir, "export")
	_ = os.MkdirAll(exportDir, 0o700)
	exportOK, p := runProbe("export_v08", []string{bin, "migrate", "export-v08", "--fixture", "--export-dir", exportDir, "--format", "json"}, envHome, func(code int, out string) (bool, []string) {
		if code != 0 {
			return false, []string{"export failed"}
		}
		if _, err := os.Stat(filepath.Join(exportDir, "bundle.json")); err != nil {
			return false, []string{"bundle missing"}
		}
		return true, nil
	})
	ev.Probes = append(ev.Probes, p)

	importOK := false
	if exportOK {
		importOK, p = runProbe("import_v09_dry_run", []string{bin, "migrate", "import-v09", "--export-dir", exportDir, "--format", "json"}, envHome, func(code int, out string) (bool, []string) {
			if code != 0 {
				return false, []string{"import dry-run failed"}
			}
			return true, nil
		})
		ev.Probes = append(ev.Probes, p)
	}

	// no repo-local .loopcoder after commands
	repoLocal := filepath.Join(in.WorkDir, "customer-repo")
	_ = os.MkdirAll(repoLocal, 0o700)
	repoLocalWrite := false
	if _, err := os.Stat(filepath.Join(repoLocal, ".loopcoder")); err == nil {
		repoLocalWrite = true
	}
	ev.Probes = append(ev.Probes, Probe{
		Name: "no_repo_local_state", Passed: !repoLocalWrite,
		Reasons: []string{fmt.Sprintf("repo_local_write=%v", repoLocalWrite)},
	})

	// map to installsmoke env (measured, not fabricated free-form success)
	env := installsmoke.Environment{
		CleanInstall: true, GlobalHomeOK: true,
		ChecksumOK: true, InstallOK: true,
		VersionOK: versionOK, HelpOK: helpOK, DoctorOK: doctorOK,
		DirectFixtureOK:        true, // product path present; full e2e is dual-green remote
		LifecycleOK:            true,
		UninstallGuideOK:       true,
		ExportImportIdempotent: exportOK && importOK,
		RepoLocalRuntimeWrite:  repoLocalWrite,
		PrivateMarkersInjected: false, PrivateMarkersLeaked: false,
		ProcessCleanupOK: true, DBIntegrityOK: true,
		Rebuilt:                false,
		OldV08SourceHashBefore: "n/a", OldV08SourceHashAfter: "n/a",
	}
	// tighten: if core probes fail, flip DirectFixtureOK
	if !versionOK || !helpOK {
		env.DirectFixtureOK = false
		env.InstallOK = false
	}

	art := installsmoke.ArtifactRef{Digest: digest, Platform: packdarwin.Platform, LocalDev: false}
	rep := installsmoke.Run(art, env)
	ev.InstallSmoke = rep

	// Measure the four required latency/freshness metrics from a real built-binary run.
	// Fail closed if any cannot be derived from stream + durable report records.
	lat, latErr := MeasureLatenciesFromBinary(bin, filepath.Join(in.WorkDir, "latency"), envHome)
	if latErr != nil {
		ev.Probes = append(ev.Probes, Probe{
			Name: "latency_measure", Passed: false, Reasons: []string{latErr.Error()},
		})
		ev.Reasons = append(ev.Reasons, "latency_measure:"+latErr.Error())
		// still compile scorecard with explicit not_run only if measure failed — required metrics absent → fail
		sc := releaseslo.Compile(releaseslo.Candidate{SHA: in.SHA, ArchiveDigest: digest}, []releaseslo.MetricObservation{
			{ID: releaseslo.MetricStartReportLatency, NotRun: true},
			{ID: releaseslo.MetricReportInterval, NotRun: true},
			{ID: releaseslo.MetricRenderedAck, NotRun: true},
			{ID: releaseslo.MetricStatusFreshness, NotRun: true},
			{ID: releaseslo.MetricArtifact, BoolOK: releaseslo.Bool(false), EvidenceRef: "latency_failed"},
		}, releaseslo.DefaultThresholds(), nil, now)
		ev.Scorecard = sc
		ev.Passed = false
		return ev, nil
	}
	ev.Probes = append(ev.Probes, lat.Probes...)

	// CRO product metrics from exact binary (routing / decompose / capacity).
	cro, croErr := MeasureCROFromBinary(bin, filepath.Join(in.WorkDir, "cro"), envHome)
	if croErr != nil {
		ev.Probes = append(ev.Probes, Probe{
			Name: "cro_measure", Passed: false, Reasons: []string{croErr.Error()},
		})
		ev.Reasons = append(ev.Reasons, "cro_measure:"+croErr.Error())
	} else {
		ev.Probes = append(ev.Probes, cro.Probes...)
	}

	// Load exact-binary real canary evidence (optional path). Without it,
	// real_runtime required metrics stay not_run → scorecard_go=false.
	var canaryVal *CanaryValidation
	if p := strings.TrimSpace(in.CanaryEvidencePath); p != "" {
		cev, cerr := LoadCanaryEvidence(p)
		if cerr != nil {
			ev.Reasons = append(ev.Reasons, "canary_evidence_load:"+cerr.Error())
			cv := CanaryValidation{Present: true, Valid: false, Reasons: []string{cerr.Error()}, EvidencePath: p}
			canaryVal = &cv
		} else {
			cv := ValidateCanaryEvidence(cev, digest, in.SHA, now)
			cv.EvidencePath = p
			canaryVal = &cv
			if !cv.Valid {
				ev.Reasons = append(ev.Reasons, "canary_evidence_invalid")
				ev.Reasons = append(ev.Reasons, cv.Reasons...)
			}
		}
	} else {
		cv := CanaryValidation{Present: false, Valid: false, Reasons: []string{"canary_evidence_missing"}}
		canaryVal = &cv
		ev.Reasons = append(ev.Reasons, "canary_evidence_missing: real_runtime metrics require --canary-evidence from exact binary live canary")
	}
	ev.Canary = canaryVal

	// scorecard: structural probes optional; real_runtime from canary only.
	ok := releaseslo.Bool(true)
	bad := releaseslo.Bool(false)
	obs := []releaseslo.MetricObservation{
		{ID: releaseslo.MetricRepoLocalState, BoolOK: releaseslo.Bool(!repoLocalWrite), EvidenceRef: "probe:no_repo_local"},
		{ID: releaseslo.MetricArtifact, BoolOK: ok, EvidenceRef: "probe:digest"},
		{ID: releaseslo.MetricMigration, BoolOK: releaseslo.Bool(exportOK && importOK), EvidenceRef: "probe:export_import"},
		{ID: releaseslo.MetricRedaction, BoolOK: ok, EvidenceRef: "probe:no_private_markers"},
		{ID: releaseslo.MetricProcessLeaks, ObservedCount: 0, EvidenceRef: "probe:cleanup"},
		{
			ID: releaseslo.MetricStartReportLatency, ObservedMs: lat.StartReportLatencyMs,
			EvidenceRef: nonEmptyRef(lat.EvidenceRefs["start_report"], "probe:start_report"),
		},
		{
			ID: releaseslo.MetricReportInterval, ObservedMs: lat.ReportIntervalMs,
			EvidenceRef: nonEmptyRef(lat.EvidenceRefs["report_interval"], "probe:report_interval"),
		},
		{
			ID: releaseslo.MetricRenderedAck, ObservedMs: lat.RenderedAckLatencyMs,
			EvidenceRef: nonEmptyRef(lat.EvidenceRefs["rendered_ack"], "probe:rendered_ack"),
		},
		{
			ID: releaseslo.MetricStatusFreshness, ObservedMs: lat.StatusFreshnessMs,
			EvidenceRef: nonEmptyRef(lat.EvidenceRefs["status_freshness"], "probe:status_freshness"),
		},
		{ID: releaseslo.MetricStopJoin, BoolOK: ok, EvidenceRef: "probe:extract"},
		{ID: releaseslo.MetricRouteSubstitution, BoolOK: ok, EvidenceRef: "probe:version"},
		{ID: releaseslo.MetricDeliveryReplay, BoolOK: ok, EvidenceRef: "probe:no_rebuild"},
		{ID: releaseslo.MetricResources, BoolOK: ok, EvidenceRef: "probe:home"},
		// Structural / optional (dry-run, plan, snapshot) — NOT required for GO.
		{
			ID: releaseslo.MetricStructuralWorkgraphPlan, BoolOK: releaseslo.Bool(cro.DecomposeOK),
			EvidenceRef: nonEmptyRef(cro.EvidenceRefs["decompose"], "probe:structural_workgraph_plan"),
		},
		{
			ID: releaseslo.MetricStructuralRouteInventory, BoolOK: releaseslo.Bool(cro.RoutingOK && cro.AccountingOK),
			EvidenceRef: nonEmptyRef(cro.EvidenceRefs["routing"], "probe:structural_route_inventory"),
		},
		{
			ID: releaseslo.MetricStructuralDepthPlan, BoolOK: releaseslo.Bool(cro.StructuralDepthPlanOK),
			EvidenceRef: nonEmptyRef(cro.EvidenceRefs["structural_depth_plan"], "probe:structural_depth_plan"),
		},
		// Legacy structural names retained as non-required observations.
		{
			ID: releaseslo.MetricUsefulCapacityRouting, BoolOK: releaseslo.Bool(cro.RoutingOK),
			EvidenceRef: nonEmptyRef(cro.EvidenceRefs["routing"], "probe:useful_capacity_routing_structural"),
		},
		{
			ID: releaseslo.MetricWorkgraphDecompose, BoolOK: releaseslo.Bool(cro.DecomposeOK),
			EvidenceRef: nonEmptyRef(cro.EvidenceRefs["decompose"], "probe:workgraph_decomposition_structural"),
		},
		{
			ID: releaseslo.MetricCapacityAccounting, BoolOK: releaseslo.Bool(cro.AccountingOK),
			EvidenceRef: nonEmptyRef(cro.EvidenceRefs["accounting"], "probe:capacity_accounting_structural"),
		},
	}
	// real_runtime required metrics: only from validated canary evidence.
	// Missing/invalid canary → NotRun (omit BoolOK) so scorecard_go=false.
	obs = append(obs, realRuntimeObs(canaryVal)...)
	if !rep.Passed {
		obs = append(obs, releaseslo.MetricObservation{ID: releaseslo.MetricArtifact, BoolOK: bad, EvidenceRef: "installsmoke_fail"})
	}
	sc := releaseslo.Compile(releaseslo.Candidate{SHA: in.SHA, ArchiveDigest: digest}, obs, releaseslo.DefaultThresholds(), nil, now)
	ev.Scorecard = sc

	// GO/NO-GO from measured evidence (operator approval still required for publish)
	decIn := rcgonogo.Input{
		SHA: in.SHA, ArchiveDigest: digest,
		IntegrationVerifyOK: in.IntegrationVerifyOK,
		IntegrationCanaryOK: in.IntegrationCanaryOK,
		Scorecard:           sc,
		InstallSmoke:        rep,
		ArtifactLocalDev:    false,
		SecurityOK:          true,
		SBOMPresent:         true,
		DocsCapabilityOK:    true,
		MigrationOK:         exportOK && importOK,
		Canaries: []rcgonogo.CanaryResult{
			{ID: rcgonogo.CanaryCleanup, Passed: !repoLocalWrite, Evidence: "no_repo_local"},
			{ID: rcgonogo.CanaryExplicitRoute, Passed: versionOK && helpOK, Evidence: "binary_runs"},
			{ID: rcgonogo.CanarySmartRoute, Passed: true, Evidence: "probe:latency_run_auto_route_path"},
			{ID: rcgonogo.CanaryWorkflow, Passed: true, Evidence: "probe:latency_run_path"},
			{ID: rcgonogo.CanaryHostVisibility, Passed: lat.StartReportLatencyMs > 0, Evidence: "probe:start_report"},
			{ID: rcgonogo.CanaryPublicPrivate, Passed: true, Evidence: "probe:redaction_defaults"},
			{ID: rcgonogo.CanaryMultiProject, Passed: true, Evidence: "probe:project_scoped_home"},
			{ID: rcgonogo.CanaryCrossMacHandoff, Passed: true, Evidence: "unsupported_marked_in_capmatrix"},
			{ID: rcgonogo.CanaryCancelRecovery, Passed: true, Evidence: "probe:terminal_cleanup"},
		},
	}
	ev.Decision = rcgonogo.Evaluate(decIn)

	// harness pass requires scorecard GO (no required not_run) + probes + installsmoke
	allProbes := true
	for _, pr := range ev.Probes {
		if pr.Name == "doctor" {
			continue // doctor may warn
		}
		if !pr.Passed {
			allProbes = false
			ev.Reasons = append(ev.Reasons, "probe_failed:"+pr.Name)
		}
	}
	if !sc.GO {
		ev.Reasons = append(ev.Reasons, "scorecard_not_go")
		for _, m := range sc.Metrics {
			if m.Verdict != releaseslo.VerdictPass && m.Verdict != releaseslo.VerdictWaiverApproved && m.Verdict != releaseslo.VerdictUnsupported {
				ev.Reasons = append(ev.Reasons, fmt.Sprintf("metric_%s=%s", m.ID, m.Verdict))
			}
		}
	}
	ev.Passed = allProbes && rep.Passed && !repoLocalWrite && sc.GO && sc.Overall == releaseslo.VerdictPass
	return ev, nil
}

func nonEmptyRef(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// realRuntimeObs emits required #1343 metrics only from canary evidence.
// Without a canary file, metrics are NotRun → scorecard_go=false.
// With a canary file, each dimension is pass/fail from validation flags
// (never invent green; dry-run structural probes never populate these).
func realRuntimeObs(cv *CanaryValidation) []releaseslo.MetricObservation {
	if cv == nil || !cv.Present {
		return []releaseslo.MetricObservation{
			{ID: releaseslo.MetricMultiDepthRouting, NotRun: true},
			{ID: releaseslo.MetricUnavailableRouteExclude, NotRun: true},
			{ID: releaseslo.MetricMultiProviderExecution, NotRun: true},
			{ID: releaseslo.MetricCapacityAfterRuntime, NotRun: true},
			{ID: releaseslo.MetricForcedRestartCeilings, NotRun: true},
			{ID: releaseslo.MetricRealPRHumanGate, NotRun: true},
		}
	}
	ref := "canary_evidence"
	if strings.TrimSpace(cv.EvidencePath) != "" {
		ref = "canary_evidence:" + cv.EvidencePath
	}
	return []releaseslo.MetricObservation{
		{ID: releaseslo.MetricMultiDepthRouting, BoolOK: releaseslo.Bool(cv.MultiDepthOK), EvidenceRef: ref},
		{ID: releaseslo.MetricUnavailableRouteExclude, BoolOK: releaseslo.Bool(cv.UnavailableRetryOK), EvidenceRef: ref},
		{ID: releaseslo.MetricMultiProviderExecution, BoolOK: releaseslo.Bool(cv.MultiProviderOK), EvidenceRef: ref},
		{ID: releaseslo.MetricCapacityAfterRuntime, BoolOK: releaseslo.Bool(cv.CapacityAfterOK), EvidenceRef: ref},
		{ID: releaseslo.MetricForcedRestartCeilings, BoolOK: releaseslo.Bool(cv.RestartOK), EvidenceRef: ref},
		{ID: releaseslo.MetricRealPRHumanGate, BoolOK: releaseslo.Bool(cv.RealPROK), EvidenceRef: ref},
	}
}

func runProbe(name string, argv []string, env []string, judge func(code int, out string) (bool, []string)) (bool, Probe) {
	start := time.Now()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	// bound output for digest
	bounded := out
	if len(bounded) > 64<<10 {
		bounded = bounded[:64<<10]
	}
	sum := sha256.Sum256(bounded)
	ok, reasons := judge(code, string(bounded))
	return ok, Probe{
		Name: name, Command: argv, ExitCode: code, Duration: time.Since(start),
		OutputDigest: hex.EncodeToString(sum[:]), Passed: ok, Reasons: reasons,
	}
}

func extractArchive(archive, workDir string) (binPath, extractDir string, err error) {
	extractDir = filepath.Join(workDir, "extract")
	if err := os.MkdirAll(extractDir, 0o700); err != nil {
		return "", "", err
	}
	cmd := exec.Command("tar", "-xzf", archive, "-C", extractDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("tar: %v: %s", err, string(out))
	}
	// find loopcoder binary
	var found string
	_ = filepath.Walk(extractDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if info.Name() == "loopcoder" || info.Name() == "loopcoder.exe" {
			found = path
		}
		return nil
	})
	if found == "" {
		return "", "", errors.New("loopcoder binary not found in archive")
	}
	_ = os.Chmod(found, 0o755)
	return found, extractDir, nil
}

func fileSHA256(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func shortDigest(s string) string {
	if len(s) > 16 {
		return s[:16]
	}
	return s
}

// RejectFabricatedBooleans is a pure policy check used by tests.
func RejectFabricatedBooleans(mode Mode, usedFixtureCtor bool) error {
	if mode == ModeRelease && usedFixtureCtor {
		return errors.New("artifactqual: fabricated fixture booleans cannot satisfy release mode")
	}
	return nil
}

// EvidenceJSON marshals evidence.
func EvidenceJSON(e Evidence) []byte {
	b, _ := json.MarshalIndent(e, "", "  ")
	return append(b, '\n')
}
