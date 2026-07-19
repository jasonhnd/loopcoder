package releasesmoke

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ReleaseOptions configures the full release smoke (install, migrate, self-bootstrap, upgrade).
type ReleaseOptions struct {
	Version         string
	PreviousVersion string
	Repo            string // github owner/name
	GitHubBaseURL   string
	GitHubAPIURL    string
	SourceRepo      string // local checkout
	KeepArtifacts   bool
}

// RunRelease executes the darwin/arm64 release acceptance smoke against a staged GitHub draft/release.
func RunRelease(opts ReleaseOptions) error {
	if err := RequireDarwinARM64(); err != nil {
		return err
	}
	for _, name := range []string{"go", "gh", "git", "cosign", "stat"} {
		if _, err := exec.LookPath(name); err != nil {
			return fmt.Errorf("%s is required for release smoke verification", name)
		}
	}

	tag := versionTag(opts.Version)
	plain := plainVersion(opts.Version)
	repo := opts.Repo
	if repo == "" {
		repo = "jasonhnd/loopcoder"
	}
	baseURL := opts.GitHubBaseURL
	if baseURL == "" {
		baseURL = "https://github.com"
	}
	apiURL := opts.GitHubAPIURL
	if apiURL == "" {
		apiURL = "https://api.github.com"
	}
	sourceRepo := opts.SourceRepo
	if sourceRepo == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		sourceRepo = wd
	}
	sourceRepo, err := filepath.Abs(sourceRepo)
	if err != nil {
		return err
	}

	tmp, err := os.MkdirTemp("", "loopcoder-release-smoke-*")
	if err != nil {
		return err
	}
	keep := opts.KeepArtifacts
	defer func() {
		if !keep {
			_ = os.RemoveAll(tmp)
		}
	}()
	artifactDir := filepath.Join(tmp, "artifacts")
	loopcoderHome := filepath.Join(tmp, "home")
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(loopcoderHome, 0o755); err != nil {
		return err
	}

	scriptRoot := filepath.Join(sourceRepo, "scripts")
	installScript := filepath.Join(scriptRoot, "install.sh")

	return withEnv(map[string]string{
		"LOOPCODER_HOME":        loopcoderHome,
		"LOOPCODER_UPGRADE_REPO": repo,
		"GITHUB_BASE_URL":       baseURL,
		"GITHUB_API_URL":        apiURL,
	}, func() error {
		release, err := downloadReleaseArchive(tmp, repo, plain, "candidate", true)
		if err != nil {
			return err
		}
		mock, err := StartMockReleaseAPI(repo, release)
		if err != nil {
			return err
		}
		defer mock.Close()

		extractDir := filepath.Join(tmp, "extract")
		if err := os.MkdirAll(extractDir, 0o755); err != nil {
			return err
		}
		if err := expandArchive(release.ArchivePath, extractDir); err != nil {
			return err
		}
		candidateBinary := filepath.Join(extractDir, "loopcoder")
		if _, err := os.Stat(candidateBinary); err != nil {
			return fmt.Errorf("archive did not contain loopcoder binary")
		}
		candidateHash, err := fileSHA256(candidateBinary)
		if err != nil {
			return err
		}

		installDir := filepath.Join(tmp, "fresh-install", "bin")
		installTmpDir := filepath.Join(tmp, "fresh-install", "tmp")
		if err := candidateInstall(mock, installScript, repo, tag, plain, installDir, installTmpDir, baseURL, "fresh install from staged candidate"); err != nil {
			return err
		}
		binary := filepath.Join(installDir, "loopcoder")
		if err := assertCandidateInstalled(binary, candidateHash, plain, "fresh install from staged candidate"); err != nil {
			return err
		}
		if err := interruptedInstallSeam(mock, installScript, repo, tag, plain, tmp, baseURL, candidateHash); err != nil {
			return err
		}

		tracked, err := exec.Command("git", "-C", sourceRepo, "ls-files", ".loopcoder").CombinedOutput()
		if err != nil {
			return fmt.Errorf("git ls-files .loopcoder: %w\n%s", err, tracked)
		}
		if strings.TrimSpace(string(tracked)) != "" {
			return fmt.Errorf("source checkout has tracked .loopcoder files:\n%s", tracked)
		}

		repoTmp := filepath.Join(tmp, "repo")
		if err := os.MkdirAll(repoTmp, 0o755); err != nil {
			return err
		}
		if out, err := exec.Command("git", "-C", repoTmp, "init", "-b", "main").CombinedOutput(); err != nil {
			return fmt.Errorf("initialize temporary git repo: %w\n%s", err, out)
		}
		if _, err := runChecked("loopcoder init", binary, nil, "init", "--repo", repoTmp); err != nil {
			return err
		}
		if _, err := runChecked("loopcoder skill install", binary, nil, "skill", "install", "--repo", repoTmp); err != nil {
			return err
		}
		regOut, err := runChecked("loopcoder projects register", binary, nil, "projects", "register", "--repo", repoTmp, "--format", "json")
		if err != nil {
			return err
		}
		var registered struct {
			Project struct {
				ProjectID string `json:"project_id"`
			} `json:"project"`
		}
		if err := decodeJSON("projects register", regOut, &registered); err != nil {
			return err
		}
		if registered.Project.ProjectID == "" {
			return fmt.Errorf("projects register did not return a project_id")
		}
		dbPath := filepath.Join(loopcoderHome, "data", "loopcoder.db")
		if _, err := os.Stat(dbPath); err != nil {
			return fmt.Errorf("projects register did not create %s", dbPath)
		}
		if err := assertOutsideRepo(dbPath, repoTmp, "database"); err != nil {
			return err
		}
		projectRoot := filepath.Join(loopcoderHome, "projects", registered.Project.ProjectID)
		for _, rel := range []string{"runs", "relay", "recovery", "audit"} {
			p := filepath.Join(projectRoot, rel)
			if _, err := os.Stat(p); err != nil {
				return fmt.Errorf("projects register did not create project payload directory %s", p)
			}
			if err := assertOutsideRepo(p, repoTmp, "project payload "+rel); err != nil {
				return err
			}
		}
		for _, rel := range []string{"logs", "tmp"} {
			p := filepath.Join(loopcoderHome, rel)
			if _, err := os.Stat(p); err != nil {
				return fmt.Errorf("projects register did not create home runtime directory %s", p)
			}
			if err := assertOutsideRepo(p, repoTmp, "home runtime "+rel); err != nil {
				return err
			}
		}

		showOut, err := runChecked("loopcoder projects show", binary, nil, "projects", "show", "--repo", repoTmp, "--format", "json")
		if err != nil {
			return err
		}
		var shown struct {
			Project struct {
				ProjectID string `json:"project_id"`
			} `json:"project"`
		}
		if err := decodeJSON("projects show", showOut, &shown); err != nil {
			return err
		}
		if shown.Project.ProjectID != registered.Project.ProjectID {
			return fmt.Errorf("projects show returned %s, want %s", shown.Project.ProjectID, registered.Project.ProjectID)
		}
		listOut, err := runChecked("loopcoder projects list", binary, nil, "projects", "list", "--format", "json")
		if err != nil {
			return err
		}
		if !strings.Contains(listOut, registered.Project.ProjectID) {
			return fmt.Errorf("projects list did not include registered project %s", registered.Project.ProjectID)
		}

		// legacy local-state fixture
		legacyWorkerDir := filepath.Join(repoTmp, ".loopcoder", "runs", "run-release-smoke", "workers")
		if err := os.MkdirAll(legacyWorkerDir, 0o755); err != nil {
			return err
		}
		legacy := map[string]any{
			"version": 1, "job_id": "job-release-smoke-1", "issue": 655, "attempt": 1,
			"provider": "codex", "status": "succeeded", "started_at": "2026-07-10T00:00:00Z",
			"report": map[string]any{
				"role": "worker", "provider": "codex", "model": "gpt-5.5", "model_source": "parsed",
				"effort": "high", "permission": "write", "action": "release smoke migration fixture",
				"exit_code": 0, "started_at": "2026-07-10T00:00:00Z", "ended_at": "2026-07-10T00:00:01Z",
				"duration_ms": 1000, "usage": map[string]any{"total_tokens": 655}, "verified": true,
				"work_id": "run-release-smoke", "issue": 655, "branch": "loop/issue-655",
			},
		}
		if err := writeJSONFile(filepath.Join(legacyWorkerDir, "job-release-smoke-1.attempt.json"), legacy); err != nil {
			return err
		}
		migOut, err := runChecked("migrate local-state dry run", binary, nil, "migrate", "local-state", "--repo", repoTmp, "--dry-run", "--format", "json")
		if err != nil {
			return err
		}
		var migration struct {
			DryRun       bool   `json:"dry_run"`
			Status       string `json:"status"`
			ScannedCount int    `json:"scanned_count"`
			ReportCount  int    `json:"report_count"`
		}
		if err := decodeJSON("migrate local-state --dry-run", migOut, &migration); err != nil {
			return err
		}
		if !migration.DryRun || !strings.HasPrefix(migration.Status, "dry-run") || migration.ScannedCount < 1 || migration.ReportCount < 1 {
			return fmt.Errorf("migration dry run did not report the fixture as importable: %s", migOut)
		}

		doctorOut, doctorCode, err := runCapture(binary, nil, "doctor", "--repo", repoTmp, "--format", "json")
		if err != nil {
			return err
		}
		var doctorPayload struct {
			ExitCode int `json:"exit_code"`
			Runtime  struct {
				Database struct {
					Path   string `json:"path"`
					Exists bool   `json:"exists"`
					Status string `json:"status"`
				} `json:"database"`
				ProjectRegistry struct {
					Registered   bool   `json:"registered"`
					ProjectID    string `json:"project_id"`
					PayloadRoot  string `json:"payload_root"`
					RunsRoot     string `json:"runs_root"`
					RelayRoot    string `json:"relay_root"`
					RecoveryRoot string `json:"recovery_root"`
					AuditRoot    string `json:"audit_root"`
					LogsRoot     string `json:"logs_root"`
					TmpRoot      string `json:"tmp_root"`
					FallbackMode string `json:"fallback_mode"`
				} `json:"project_registry"`
			} `json:"runtime"`
			ProviderCompatibility []struct {
				Provider string `json:"provider"`
				Role     string `json:"role"`
				Support  string `json:"support"`
				Code     string `json:"code"`
				Status   string `json:"status"`
			} `json:"provider_compatibility"`
			Checks []struct {
				Name   string `json:"name"`
				Code   string `json:"code"`
				Status string `json:"status"`
			} `json:"checks"`
		}
		if err := decodeJSON("doctor", doctorOut, &doctorPayload); err != nil {
			return err
		}
		if doctorPayload.ExitCode != 0 && doctorCode != 0 && doctorPayload.ExitCode != doctorCode {
			// prefer JSON exit_code when present
		}
		if doctorPayload.ExitCode != 0 {
			return fmt.Errorf("doctor JSON reported exit_code=%d", doctorPayload.ExitCode)
		}
		if doctorPayload.Runtime.Database.Path != filepath.ToSlash(dbPath) && doctorPayload.Runtime.Database.Path != dbPath {
			return fmt.Errorf("doctor JSON reported unexpected database path: %s", doctorPayload.Runtime.Database.Path)
		}
		if !doctorPayload.Runtime.Database.Exists || doctorPayload.Runtime.Database.Status != "ok" {
			return fmt.Errorf("doctor JSON did not report healthy storage")
		}
		if !doctorPayload.Runtime.ProjectRegistry.Registered || doctorPayload.Runtime.ProjectRegistry.ProjectID != registered.Project.ProjectID {
			return fmt.Errorf("doctor JSON did not report the registered smoke project")
		}
		if doctorPayload.Runtime.ProjectRegistry.FallbackMode != "registered-global" {
			return fmt.Errorf("doctor JSON reported fallback_mode=%s, want registered-global", doctorPayload.Runtime.ProjectRegistry.FallbackMode)
		}
		for _, runtimePath := range []string{
			doctorPayload.Runtime.ProjectRegistry.PayloadRoot,
			doctorPayload.Runtime.ProjectRegistry.RunsRoot,
			doctorPayload.Runtime.ProjectRegistry.RelayRoot,
			doctorPayload.Runtime.ProjectRegistry.RecoveryRoot,
			doctorPayload.Runtime.ProjectRegistry.AuditRoot,
			doctorPayload.Runtime.ProjectRegistry.LogsRoot,
			doctorPayload.Runtime.ProjectRegistry.TmpRoot,
		} {
			if strings.TrimSpace(runtimePath) == "" {
				return fmt.Errorf("doctor JSON omitted a runtime payload path")
			}
			if err := assertOutsideRepo(runtimePath, repoTmp, "doctor runtime payload path"); err != nil {
				return err
			}
		}
		foundCodex := false
		for _, pc := range doctorPayload.ProviderCompatibility {
			if pc.Provider == "codex" && pc.Role == "worker" {
				if err := assertSupportReady(pc.Support, pc.Code, pc.Status, "doctor JSON codex worker"); err != nil {
					return err
				}
				foundCodex = true
				break
			}
		}
		if !foundCodex {
			return fmt.Errorf("doctor JSON did not include default codex worker provider compatibility")
		}
		foundCheck := false
		for _, c := range doctorPayload.Checks {
			if c.Name == "provider compatibility codex worker" {
				if err := assertDoctorCheckReady(c.Code, c.Status, "doctor JSON codex worker check"); err != nil {
					return err
				}
				foundCheck = true
				break
			}
		}
		if !foundCheck {
			return fmt.Errorf("doctor JSON did not include selected default codex worker compatibility check")
		}

		reportOut, err := runChecked("loopcoder report", binary, nil, "report", "--repo", repoTmp, "--format", "json")
		if err != nil {
			return err
		}
		if !strings.Contains(reportOut, "run-release-smoke") {
			return fmt.Errorf("report JSON did not include the release smoke worker fixture")
		}

		if err := withMockReleaseEnv(mock, baseURL, repo, tag, func() error {
			upOut, err := runChecked("upgrade already-latest", binary, nil, "upgrade", "--version", plain)
			if err != nil {
				return err
			}
			low := strings.ToLower(upOut)
			if !strings.Contains(low, "already latest") && !strings.Contains(low, "no download needed") {
				return fmt.Errorf("upgrade did not recognize %s as already latest\n%s", plain, upOut)
			}
			return nil
		}); err != nil {
			return err
		}

		// self-bootstrap with installed candidate
		if err := RunSelfBootstrap(SelfBootstrapOptions{
			Repo:          sourceRepo,
			Binary:        binary,
			Version:       plain,
			KeepArtifacts: opts.KeepArtifacts,
		}); err != nil {
			return fmt.Errorf("self-bootstrap acceptance smoke: %w", err)
		}

		migrationEvidence := map[string]any{
			"attempted":                 false,
			"source_schema_version":     nil,
			"target_schema_version":     nil,
			"backup_verified":           false,
			"backup_sha256":             "",
			"rollback_opened_by_previous": false,
		}
		upgradeEvidence := map[string]any{
			"attempted":                 false,
			"from_version":              "",
			"to_version":                plain,
			"installed_candidate_sha256": "",
		}

		prevPlain := plainVersion(opts.PreviousVersion)
		if prevPlain != "" && prevPlain != plain {
			previous, err := downloadReleaseArchive(tmp, repo, prevPlain, "previous "+prevPlain, false)
			if err != nil {
				return err
			}
			previousExtract := filepath.Join(tmp, "previous-extract")
			if err := os.MkdirAll(previousExtract, 0o755); err != nil {
				return err
			}
			if err := expandArchive(previous.ArchivePath, previousExtract); err != nil {
				return err
			}
			previousBinary := filepath.Join(previousExtract, "loopcoder")
			if _, err := os.Stat(previousBinary); err != nil {
				return fmt.Errorf("previous archive did not contain loopcoder binary")
			}

			if previous.PlainVersion == "0.7.0" {
				migrationHome := filepath.Join(tmp, "v07-schema-migration-home")
				if err := os.MkdirAll(migrationHome, 0o755); err != nil {
					return err
				}
				if err := withEnv(map[string]string{"LOOPCODER_HOME": migrationHome}, func() error {
					v07Out, err := runChecked("v0.7.0 schema fixture registration", previousBinary, nil, "projects", "register", "--repo", repoTmp, "--format", "json")
					if err != nil {
						return err
					}
					var v07Reg struct {
						Project struct {
							ProjectID string `json:"project_id"`
						} `json:"project"`
					}
					if err := decodeJSON("v0.7.0 schema fixture registration", v07Out, &v07Reg); err != nil {
						return err
					}
					if v07Reg.Project.ProjectID == "" {
						return fmt.Errorf("v0.7.0 schema fixture registration omitted project_id")
					}
					migrationDatabase := filepath.Join(migrationHome, "data", "loopcoder.db")
					migrationBackupDir := filepath.Join(migrationHome, "data", "backups")
					if _, err := os.Stat(migrationDatabase); err != nil {
						return fmt.Errorf("v0.7.0 schema fixture did not create %s", migrationDatabase)
					}
					if _, err := os.Stat(migrationBackupDir); err == nil {
						return fmt.Errorf("v0.7.0 schema fixture unexpectedly created a v0.8 backup directory")
					}

					planOut, err := runChecked("candidate storage migration plan", binary, nil, "migrate", "storage", "--format", "json")
					if err != nil {
						return err
					}
					var plan storagePlanPayload
					if err := decodeJSON("candidate storage migration plan", planOut, &plan); err != nil {
						return err
					}
					if err := assertUpgradePlanFromV07(plan); err != nil {
						return err
					}
					if _, err := os.Stat(migrationBackupDir); err == nil {
						return fmt.Errorf("read-only storage migration plan created %s", migrationBackupDir)
					}
					target := plan.Plan.TargetSchemaVersion

					applyOut, err := runChecked("candidate storage migration apply", binary, nil, "migrate", "storage", "--apply", "--format", "json")
					if err != nil {
						return err
					}
					var apply storagePlanPayload
					if err := decodeJSON("candidate storage migration apply", applyOut, &apply); err != nil {
						return err
					}
					if err := assertMigratedToCurrent(apply, target); err != nil {
						return err
					}
					if apply.Backup == nil || !apply.Backup.Verified || apply.Backup.Path == "" {
						return fmt.Errorf("candidate storage migration did not return a verified backup")
					}
					if _, err := os.Stat(apply.Backup.Path); err != nil {
						return fmt.Errorf("candidate storage migration did not return a verified backup")
					}
					if apply.Rollback.BackupPath != apply.Backup.Path || apply.Rollback.BackupSHA256 != apply.Backup.SHA256 {
						return fmt.Errorf("candidate rollback metadata did not bind the verified backup")
					}
					mode, err := fileModeOctal(apply.Backup.Path)
					if err != nil {
						return err
					}
					if mode != "600" {
						return fmt.Errorf("candidate storage migration backup is not owner-only (mode=%s)", mode)
					}

					repeatOut, err := runChecked("repeated candidate storage migration", binary, nil, "migrate", "storage", "--apply", "--format", "json")
					if err != nil {
						return err
					}
					var repeat storagePlanPayload
					if err := decodeJSON("repeated candidate storage migration", repeatOut, &repeat); err != nil {
						return err
					}
					if repeat.Status != "no-op" || !repeat.Applied || repeat.Plan.Status != "current" {
						return fmt.Errorf("repeated candidate storage migration was not an idempotent no-op")
					}
					matches, _ := filepath.Glob(filepath.Join(migrationBackupDir, "schema-v9-*.db"))
					if len(matches) != 1 {
						return fmt.Errorf("repeated candidate storage migration did not preserve exactly one v0.7 backup")
					}

					rollbackHome := filepath.Join(tmp, "v07-schema-rollback-home")
					rollbackData := filepath.Join(rollbackHome, "data")
					if err := os.MkdirAll(rollbackData, 0o755); err != nil {
						return err
					}
					restoredDB := filepath.Join(rollbackData, "loopcoder.db")
					in, err := os.ReadFile(apply.Backup.Path)
					if err != nil {
						return err
					}
					if err := os.WriteFile(restoredDB, in, 0o600); err != nil {
						return err
					}
					if err := withEnv(map[string]string{"LOOPCODER_HOME": rollbackHome}, func() error {
						restoredOut, err := runChecked("restored v0.7.0 project", previousBinary, nil, "projects", "show", "--repo", repoTmp, "--format", "json")
						if err != nil {
							return err
						}
						var restored struct {
							Project struct {
								ProjectID string `json:"project_id"`
							} `json:"project"`
						}
						if err := decodeJSON("restored v0.7.0 project", restoredOut, &restored); err != nil {
							return err
						}
						if restored.Project.ProjectID != v07Reg.Project.ProjectID {
							return fmt.Errorf("restored v0.7.0 backup did not preserve project identity")
						}
						return nil
					}); err != nil {
						return err
					}

					migrationEvidence = map[string]any{
						"attempted":                   true,
						"source_schema_version":       plan.Plan.SourceSchemaVersion,
						"target_schema_version":       apply.Health.SchemaVersion,
						"backup_verified":             apply.Backup.Verified,
						"backup_sha256":               apply.Backup.SHA256,
						"rollback_opened_by_previous": true,
					}
					return nil
				}); err != nil {
					return err
				}
			}

			if err := withMockReleaseEnv(mock, baseURL, repo, tag, func() error {
				upOut, err := runChecked("upgrade from previous", previousBinary, nil, "upgrade", "--version", plain)
				if err != nil {
					return err
				}
				afterRe := regexp.MustCompile(`After: .*version=v?` + regexp.QuoteMeta(plain))
				if !afterRe.MatchString(upOut) && !strings.Contains(upOut, "Installed versioned binary:") {
					return fmt.Errorf("previous-version upgrade output did not show installation of %s\n%s", plain, upOut)
				}
				return nil
			}); err != nil {
				return err
			}
			upgradedStable := filepath.Join(loopcoderHome, "bin", "loopcoder")
			if err := assertCandidateInstalled(upgradedStable, candidateHash, plain, "previous-version upgrade from staged candidate"); err != nil {
				return err
			}
			upHash, _ := fileSHA256(upgradedStable)
			upgradeEvidence = map[string]any{
				"attempted":                  true,
				"from_version":               previous.PlainVersion,
				"to_version":                 plain,
				"installed_candidate_sha256": upHash,
			}
		}

		installedHash, _ := fileSHA256(binary)
		evidence := map[string]any{
			"schema_version":    "loopcoder.release_smoke_evidence.v1",
			"version":           plain,
			"previous_version":  prevPlain,
			"host_tuple":        HostTuple(),
			"candidate": map[string]any{
				"archive":                 release.Asset,
				"binary_sha256":           candidateHash,
				"installed_binary":        binary,
				"installed_binary_sha256": installedHash,
			},
			"self_bootstrap": map[string]any{
				"provider":            "test-subprocess",
				"paid_provider_calls": 0,
				"completed":           true,
			},
			"migration": migrationEvidence,
			"upgrade":   upgradeEvidence,
		}
		if err := writeJSONFile(filepath.Join(artifactDir, "release-smoke-evidence.json"), evidence); err != nil {
			return err
		}
		fmt.Println("release smoke evidence JSON written")
		fmt.Printf("release smoke verification passed for %s\n", tag)
		if keep {
			fmt.Println("retained release smoke artifacts:", artifactDir)
		}
		return nil
	})
}

func platformAssetName(version string) string {
	return fmt.Sprintf("loopcoder_%s_darwin_arm64.tar.gz", plainVersion(version))
}

func downloadReleaseArchive(tmp, repo, selectedVersion, label string, requireExact bool) (ReleaseAssets, error) {
	selectedTag := versionTag(selectedVersion)
	selectedPlain := plainVersion(selectedVersion)
	selectedAsset := platformAssetName(selectedPlain)
	identity := fmt.Sprintf("https://github.com/%s/.github/workflows/release.yml@refs/tags/%s", repo, selectedTag)
	issuer := "https://token.actions.githubusercontent.com"
	releaseDir := filepath.Join(tmp, "release-"+selectedPlain)
	if err := os.MkdirAll(releaseDir, 0o755); err != nil {
		return ReleaseAssets{}, err
	}
	if out, err := exec.Command("gh", "release", "view", selectedTag, "--repo", repo).CombinedOutput(); err != nil {
		return ReleaseAssets{}, fmt.Errorf("check %s release exists: %w\n%s", label, err, out)
	}
	if requireExact {
		if err := assertRemoteReleaseAssetInventory(repo, selectedTag, selectedAsset, label); err != nil {
			return ReleaseAssets{}, err
		}
	}
	archivePath := filepath.Join(releaseDir, selectedAsset)
	sumsPath := filepath.Join(releaseDir, "SHA256SUMS")
	signaturePath := filepath.Join(releaseDir, "SHA256SUMS.sigstore")
	dl := exec.Command("gh", "release", "download", selectedTag, "--repo", repo, "--dir", releaseDir, "--clobber",
		"--pattern", selectedAsset, "--pattern", "SHA256SUMS", "--pattern", "SHA256SUMS.sigstore")
	if out, err := dl.CombinedOutput(); err != nil {
		return ReleaseAssets{}, fmt.Errorf("download %s release assets: %w\n%s", label, err, out)
	}
	for _, p := range []string{archivePath, sumsPath, signaturePath} {
		if _, err := os.Stat(p); err != nil {
			return ReleaseAssets{}, fmt.Errorf("downloaded %s release did not include %s", label, filepath.Base(p))
		}
	}
	if err := assertLocalReleaseAssetInventory(releaseDir, selectedAsset, label); err != nil {
		return ReleaseAssets{}, err
	}
	verify := exec.Command("cosign", "verify-blob", sumsPath, "--bundle", signaturePath,
		"--certificate-identity", identity, "--certificate-oidc-issuer", issuer)
	if out, err := verify.CombinedOutput(); err != nil {
		return ReleaseAssets{}, fmt.Errorf("verify %s SHA256SUMS signature: %w\n%s", label, err, out)
	}
	expectedHash, err := expectedSumForAsset(sumsPath, selectedAsset)
	if err != nil {
		return ReleaseAssets{}, err
	}
	actualHash, err := fileSHA256(archivePath)
	if err != nil {
		return ReleaseAssets{}, err
	}
	if actualHash != expectedHash {
		return ReleaseAssets{}, fmt.Errorf("checksum mismatch for %s: expected %s, got %s", selectedAsset, expectedHash, actualHash)
	}
	return ReleaseAssets{
		Tag:           selectedTag,
		PlainVersion:  selectedPlain,
		Asset:         selectedAsset,
		ArchivePath:   archivePath,
		SumsPath:      sumsPath,
		SignaturePath: signaturePath,
	}, nil
}

func assertRemoteReleaseAssetInventory(repo, tag, selectedAsset, label string) error {
	out, err := exec.Command("gh", "release", "view", tag, "--repo", repo, "--json", "assets", "--jq", ".assets[].name").CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to inspect %s release asset inventory: %w\n%s", label, err, out)
	}
	var names []string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		if n := strings.TrimSpace(sc.Text()); n != "" {
			names = append(names, n)
		}
	}
	want := []string{selectedAsset, "SHA256SUMS", "SHA256SUMS.sigstore"}
	return assertAssetNameSet(names, want, label+" remote")
}

func assertLocalReleaseAssetInventory(releaseDir, selectedAsset, label string) error {
	entries, err := os.ReadDir(releaseDir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return assertAssetNameSet(names, []string{selectedAsset, "SHA256SUMS", "SHA256SUMS.sigstore"}, label+" local")
}

func assertAssetNameSet(actual, want []string, label string) error {
	if len(actual) != len(want) {
		return fmt.Errorf("%s asset inventory contains %d files; want %d: %v", label, len(actual), len(want), actual)
	}
	set := map[string]bool{}
	for _, n := range actual {
		set[n] = true
	}
	for _, w := range want {
		if !set[w] {
			return fmt.Errorf("%s asset inventory mismatch; got %v, want %v", label, actual, want)
		}
	}
	return nil
}

func expectedSumForAsset(sumsPath, asset string) (string, error) {
	f, err := os.Open(sumsPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[len(fields)-1] == asset {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("SHA256SUMS does not contain %s", asset)
}

func expandArchive(archive, dest string) error {
	if !strings.HasSuffix(archive, ".tar.gz") {
		return fmt.Errorf("unsupported release archive format: %s", filepath.Base(archive))
	}
	cmd := exec.Command("tar", "-xzf", archive, "-C", dest)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to extract %s: %w\n%s", archive, err, out)
	}
	return nil
}

func candidateInstallEnv(server *MockReleaseAPI, installDir, installTmpDir, repo, tag, baseURL, pathPrefix string) map[string]string {
	releaseIdentity := fmt.Sprintf("%s/%s/.github/workflows/release.yml@refs/tags/%s", baseURL, repo, tag)
	installHome := filepath.Join(filepath.Dir(installTmpDir), "home")
	pathValue := installDir + ":" + os.Getenv("PATH")
	if pathPrefix != "" {
		pathValue = pathPrefix + ":" + pathValue
	}
	return map[string]string{
		"GITHUB_BASE_URL":         server.URL,
		"GITHUB_API_URL":          server.URL,
		"LOOPCODER_COSIGN_IDENTITY": releaseIdentity,
		"LOOPCODER_INSTALL_REPO":  repo,
		"LOOPCODER_UPGRADE_REPO":  repo,
		"LOOPCODER_INSTALL_DIR":   installDir,
		"LOOPCODER_INSTALL_OS":    "darwin",
		"LOOPCODER_INSTALL_ARCH":  "arm64",
		"TMPDIR":                  installTmpDir,
		"HOME":                    installHome,
		"PATH":                    pathValue,
	}
}

func candidateInstall(server *MockReleaseAPI, installScript, repo, tag, plain, installDir, installTmpDir, baseURL, label string) error {
	if _, err := os.Stat(installScript); err != nil {
		return fmt.Errorf("install.sh not found at %s", installScript)
	}
	if err := os.MkdirAll(installTmpDir, 0o755); err != nil {
		return err
	}
	envMap := candidateInstallEnv(server, installDir, installTmpDir, repo, tag, baseURL, "")
	return withEnv(envMap, func() error {
		cmd := exec.Command("/bin/sh", installScript, "--version", plain)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s install.sh failed: %w", label, err)
		}
		return nil
	})
}

func assertCandidateInstalled(binaryPath, expectedHash, expectedVersion, label string) error {
	if _, err := os.Stat(binaryPath); err != nil {
		return fmt.Errorf("%s binary was not installed at %s", label, binaryPath)
	}
	actualHash, err := fileSHA256(binaryPath)
	if err != nil {
		return err
	}
	if actualHash != expectedHash {
		return fmt.Errorf("%s binary hash mismatch: expected staged candidate hash %s, got %s", label, expectedHash, actualHash)
	}
	mode, err := fileModeOctal(binaryPath)
	if err != nil {
		return err
	}
	if mode != "755" {
		return fmt.Errorf("%s binary mode is %s, want 755", label, mode)
	}
	out, err := runChecked(label+" version", binaryPath, nil, "version")
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		return fmt.Errorf("%s binary emitted %d version lines; want exactly one", label, len(lines))
	}
	text := lines[0]
	if !regexp.MustCompile(`version=v?` + regexp.QuoteMeta(expectedVersion)).MatchString(text) {
		return fmt.Errorf("%s binary did not report %s with non-placeholder commit/date: %s", label, expectedVersion, text)
	}
	if regexp.MustCompile(`(commit|date)=unknown`).MatchString(text) {
		return fmt.Errorf("%s binary did not report %s with non-placeholder commit/date: %s", label, expectedVersion, text)
	}
	return nil
}

func withMockReleaseEnv(server *MockReleaseAPI, baseURL, repo, tag string, fn func() error) error {
	identity := fmt.Sprintf("%s/%s/.github/workflows/release.yml@refs/tags/%s", baseURL, repo, tag)
	return withEnv(map[string]string{
		"GITHUB_BASE_URL":           server.URL,
		"GITHUB_API_URL":            server.URL,
		"LOOPCODER_COSIGN_IDENTITY": identity,
	}, fn)
}

func assertSupportReady(support, code, status, label string) error {
	if support != "supported" && support != "experimental" {
		return fmt.Errorf("%s support must be supported or experimental, got %s", label, support)
	}
	if code != "supported" && code != "experimental" {
		return fmt.Errorf("%s code must be supported or experimental, got %s", label, code)
	}
	if status != "ok" && status != "warn" {
		return fmt.Errorf("%s status must be ok or warn, got %s", label, status)
	}
	return nil
}

func assertDoctorCheckReady(code, status, label string) error {
	if code != "supported" && code != "experimental" {
		return fmt.Errorf("%s code must be supported or experimental, got %s", label, code)
	}
	if status != "ok" && status != "warn" {
		return fmt.Errorf("%s status must be ok or warn, got %s", label, status)
	}
	return nil
}

func interruptedInstallSeam(server *MockReleaseAPI, installScript, repo, tag, plain, tmp, baseURL, expectedHash string) error {
	caseDir := filepath.Join(tmp, "interrupted-install")
	installDir := filepath.Join(caseDir, "bin")
	installTmp := filepath.Join(caseDir, "tmp")
	shimDir := filepath.Join(caseDir, "shim")
	for _, d := range []string{installDir, installTmp, shimDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	preexisting := filepath.Join(installDir, "loopcoder")
	preexistingContent := "#!/bin/sh\n" +
		"if [ \"${1:-}\" = \"version\" ] || [ \"${1:-}\" = \"--version\" ]; then\n" +
		"  printf '%s\\n' 'version=0.7.0 commit=preexisting date=2026-07-01T00:00:00Z'\n" +
		"  exit 0\n" +
		"fi\n" +
		"printf '%s\\n' 'preexisting loopcoder still usable'\n"
	if err := os.WriteFile(preexisting, []byte(preexistingContent), 0o755); err != nil {
		return err
	}
	preexistingHash, err := fileSHA256(preexisting)
	if err != nil {
		return err
	}
	mvReady := filepath.Join(caseDir, "mv.ready")
	mvPidFile := filepath.Join(caseDir, "mv.pid")
	mvShim := "#!/bin/sh\nset -eu\n" +
		"printf '%s\\n' \"$$\" >\"$LOOPCODER_SMOKE_MV_PID\"\n" +
		"touch \"$LOOPCODER_SMOKE_MV_READY\"\n" +
		"while :; do\n  sleep 1\ndone\n"
	if err := os.WriteFile(filepath.Join(shimDir, "mv"), []byte(mvShim), 0o755); err != nil {
		return err
	}

	envMap := candidateInstallEnv(server, installDir, installTmp, repo, tag, baseURL, shimDir)
	envMap["LOOPCODER_SMOKE_MV_READY"] = mvReady
	envMap["LOOPCODER_SMOKE_MV_PID"] = mvPidFile

	stdoutPath := filepath.Join(caseDir, "install.stdout")
	stderrPath := filepath.Join(caseDir, "install.stderr")
	stdoutF, err := os.Create(stdoutPath)
	if err != nil {
		return err
	}
	stderrF, err := os.Create(stderrPath)
	if err != nil {
		_ = stdoutF.Close()
		return err
	}

	err = withEnv(envMap, func() error {
		cmd := exec.Command("/bin/sh", installScript, "--version", plain)
		cmd.Stdout = stdoutF
		cmd.Stderr = stderrF
		if err := cmd.Start(); err != nil {
			return err
		}
		waitCh := make(chan error, 1)
		go func() { waitCh <- cmd.Wait() }()

		deadline := time.Now().Add(20 * time.Second)
		for {
			if _, err := os.Stat(mvReady); err == nil {
				break
			}
			select {
			case waitErr := <-waitCh:
				_ = stdoutF.Close()
				_ = stderrF.Close()
				stdout, _ := os.ReadFile(stdoutPath)
				stderr, _ := os.ReadFile(stderrPath)
				return fmt.Errorf("interrupted install exited before replacement seam was reached: %v\nstdout:\n%s\nstderr:\n%s", waitErr, stdout, stderr)
			default:
			}
			if time.Now().After(deadline) {
				_ = cmd.Process.Kill()
				<-waitCh
				return fmt.Errorf("interrupted install did not reach the atomic replacement seam")
			}
			time.Sleep(100 * time.Millisecond)
		}

		mvPidBytes, err := os.ReadFile(mvPidFile)
		if err != nil {
			_ = cmd.Process.Kill()
			<-waitCh
			return err
		}
		mvPid := strings.TrimSpace(string(mvPidBytes))
		if !regexp.MustCompile(`^[0-9]+$`).MatchString(mvPid) {
			_ = cmd.Process.Kill()
			<-waitCh
			return fmt.Errorf("interruption mv shim pid was not numeric: %s", mvPid)
		}
		if out, err := exec.Command("kill", mvPid).CombinedOutput(); err != nil {
			_ = cmd.Process.Kill()
			<-waitCh
			return fmt.Errorf("failed to kill mv shim pid %s: %w\n%s", mvPid, err, out)
		}

		select {
		case waitErr := <-waitCh:
			if waitErr == nil {
				return fmt.Errorf("interrupted install unexpectedly succeeded")
			}
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-waitCh
			return fmt.Errorf("interrupted install did not exit after replacement process was terminated")
		}
		return nil
	})
	_ = stdoutF.Close()
	_ = stderrF.Close()
	if err != nil {
		return err
	}

	afterHash, err := fileSHA256(preexisting)
	if err != nil {
		return err
	}
	if afterHash != preexistingHash {
		return fmt.Errorf("interrupted install changed the pre-existing binary")
	}
	preOut, code, err := runCapture(preexisting, nil, "version")
	if err != nil || code != 0 || !strings.Contains(preOut, "version=0.7.0") {
		return fmt.Errorf("pre-existing binary was not usable after interrupted install")
	}
	entries, _ := os.ReadDir(installDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".loopcoder.tmp.") {
			return fmt.Errorf("interrupted install exposed a partial replacement in %s", installDir)
		}
	}
	entries, _ = os.ReadDir(installTmp)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "loopcoder-install.") {
			return fmt.Errorf("interrupted install left installer temporary artifacts in %s", installTmp)
		}
	}
	if err := candidateInstall(server, installScript, repo, tag, plain, installDir, installTmp, baseURL, "retry after interrupted install"); err != nil {
		return err
	}
	return assertCandidateInstalled(filepath.Join(installDir, "loopcoder"), expectedHash, plain, "retry after interrupted install")
}
