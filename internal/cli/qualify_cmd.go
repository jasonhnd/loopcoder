package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/artifactqual"
)

func runQualify(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("qualify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	archive := fs.String("archive", "", "path to exact RC archive (.tar.gz)")
	digest := fs.String("digest", "", "expected sha256 of archive")
	sha := fs.String("sha", "", "source commit SHA")
	work := fs.String("work-dir", "", "scratch directory (default temp)")
	format := fs.String("format", "text", "text|json")
	iv := fs.Bool("integration-verify-ok", false, "diagnostic only: legacy integration-verify flag (does not qualify)")
	ic := fs.Bool("integration-canary-ok", false, "diagnostic only: legacy integration-canary flag (does not qualify)")
	repo := fs.String("repository", "", "owner/repo for pre-prod and RC Actions evidence")
	intRunID := fs.Int64("integration-run-id", 0, "GitHub Actions run ID for pre-prod dual-green")
	intAttempt := fs.Int("integration-run-attempt", 0, "GitHub Actions run attempt for pre-prod dual-green (≥1)")
	rcRunID := fs.Int64("rc-run-id", 0, "GitHub Actions run ID for Release Candidate Draft")
	rcArtID := fs.Int64("rc-artifact-id", 0, "GitHub Actions artifact ID for v090-rc-darwin-arm64")
	canary := fs.String("canary-evidence", "", "exact-binary real canary evidence JSON (loopcoder.canary_evidence.v1); required")
	canaryProject := fs.String("canary-project-id", "", "expected disposable canary project_id (anti-reuse)")
	canaryRun := fs.String("canary-run-id", "", "expected disposable canary run_id (anti-reuse)")
	canaryPRHead := fs.String("canary-pr-head", "", "disposable canary PR head OID (not LoopCoder RC SHA)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	var missing []string
	if strings.TrimSpace(*archive) == "" {
		missing = append(missing, "--archive")
	}
	if strings.TrimSpace(*digest) == "" {
		missing = append(missing, "--digest")
	}
	if strings.TrimSpace(*sha) == "" {
		missing = append(missing, "--sha")
	}
	if strings.TrimSpace(*repo) == "" {
		missing = append(missing, "--repository")
	}
	if *intRunID <= 0 {
		missing = append(missing, "--integration-run-id")
	}
	if *intAttempt < 1 {
		missing = append(missing, "--integration-run-attempt")
	}
	if *rcRunID <= 0 {
		missing = append(missing, "--rc-run-id")
	}
	if *rcArtID <= 0 {
		missing = append(missing, "--rc-artifact-id")
	}
	if strings.TrimSpace(*canary) == "" {
		missing = append(missing, "--canary-evidence")
	}
	if strings.TrimSpace(*canaryProject) == "" {
		missing = append(missing, "--canary-project-id")
	}
	if strings.TrimSpace(*canaryRun) == "" {
		missing = append(missing, "--canary-run-id")
	}
	if strings.TrimSpace(*canaryPRHead) == "" {
		missing = append(missing, "--canary-pr-head")
	}
	if len(missing) > 0 {
		fmt.Fprintf(stderr, "qualify: missing required flags: %s\n", strings.Join(missing, ", "))
		return 2
	}

	wd := strings.TrimSpace(*work)
	if wd == "" {
		var err error
		wd, err = os.MkdirTemp("", "loopcoder-qualify-*")
		if err != nil {
			fmt.Fprintf(stderr, "qualify: temp: %v\n", err)
			return 4
		}
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	ghVerifier := &artifactqual.GitHubEvidenceVerifier{}
	ev, err := artifactqual.Qualify(artifactqual.Input{
		Mode:        artifactqual.ModeRelease,
		ArchivePath: *archive, ExpectedDigest: *digest, SHA: *sha,
		WorkDir: wd, IntegrationVerifyOK: *iv, IntegrationCanaryOK: *ic,
		Repository:              strings.TrimSpace(*repo),
		IntegrationVerifier:     ghVerifier,
		IntegrationRunID:        *intRunID,
		IntegrationRunAttempt:   *intAttempt,
		RCActionsVerifier:       ghVerifier,
		RCRunID:                 *rcRunID,
		RCArtifactID:            *rcArtID,
		PRLiveVerifier:          ghVerifier,
		CanaryEvidencePath:      strings.TrimSpace(*canary),
		ExpectedCanaryProjectID: strings.TrimSpace(*canaryProject),
		ExpectedCanaryRunID:     strings.TrimSpace(*canaryRun),
		ExpectedPRHeadOID:       strings.TrimSpace(*canaryPRHead),
		Now:                     now().UTC(),
	})
	if err != nil {
		fmt.Fprintf(stderr, "qualify: %v\n", err)
		return 4
	}
	if strings.ToLower(*format) == "json" {
		_, _ = stdout.Write(artifactqual.EvidenceJSON(ev))
	} else {
		fmt.Fprintf(stdout, "passed=%v digest=%s probes=%d install_smoke=%v decision=%s\n",
			ev.Passed, ev.ArchiveDigest, len(ev.Probes), ev.InstallSmoke.Passed, ev.Decision.Decision)
		if len(ev.Reasons) > 0 {
			fmt.Fprintf(stdout, "reasons=%s\n", strings.Join(ev.Reasons, ";"))
		}
	}
	// write evidence beside work dir when possible
	_ = os.WriteFile(filepath.Join(wd, "qualification-evidence.json"), artifactqual.EvidenceJSON(ev), 0o600)
	if !ev.Passed {
		return 4
	}
	return 0
}
