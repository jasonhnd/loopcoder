package artifactqual

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SchemaRCManifest matches scripts/build-release-candidate.sh output.
const SchemaRCManifest = "loopcoder.release.candidate.v1"

// ReleaseCandidateDraftWorkflow is the Actions workflow that must produce the RC.
const ReleaseCandidateDraftWorkflow = "Release Candidate Draft"

// V090RCArtifactName is the exact Actions artifact name required for v0.9.0 RC.
const V090RCArtifactName = "v090-rc-darwin-arm64"

// RCManifest is evidence/rc-manifest.json inside an exact RC dist tree.
type RCManifest struct {
	Schema              string `json:"schema"`
	Version             string `json:"version"`
	CommitSHA           string `json:"commit_sha"`
	BuildDate           string `json:"build_date,omitempty"`
	Platform            string `json:"platform,omitempty"`
	BuildSource         string `json:"build_source"`
	Archive             string `json:"archive"`
	ArchiveDigestSHA256 string `json:"archive_digest_sha256"`
	SBOM                string `json:"sbom,omitempty"`
	Checksums           string `json:"checksums,omitempty"`
	PublicRelease       bool   `json:"public_release"`
	DraftOnly           bool   `json:"draft_only"`
	ActionsRunID        int64  `json:"actions_run_id,omitempty"`
	ActionsArtifactID   int64  `json:"actions_artifact_id,omitempty"`
	ActionsWorkflowName string `json:"actions_workflow_name,omitempty"`
	ActionsRepository   string `json:"actions_repository,omitempty"`
}

// RCProvenance binds an extracted/local RC tree to authoritative Actions identity.
type RCProvenance struct {
	ManifestPath string
	SumsPath     string
	Manifest     RCManifest
	// ObservedArchiveDigest is the hash of the archive file on disk.
	ObservedArchiveDigest string
	// ObservedSumsDigest is the first digest line from SHA256SUMS.
	ObservedSumsDigest string
}

// RCActionsBinding is the live/test Actions run that produced the RC artifact.
type RCActionsBinding struct {
	Repository      string `json:"repository"`
	WorkflowName    string `json:"workflow_name"`
	RunID           int64  `json:"run_id"`
	RunAttempt      int    `json:"run_attempt,omitempty"`
	ArtifactID      int64  `json:"artifact_id"`
	HeadSHA         string `json:"head_sha"`
	Conclusion      string `json:"conclusion"`
	Status          string `json:"status"`
	ArtifactName    string `json:"artifact_name,omitempty"`
	ArtifactExpired bool   `json:"artifact_expired,omitempty"`
}

// RCActionsVerifier fetches authoritative Release Candidate Draft run/artifact state.
type RCActionsVerifier interface {
	FetchRCBinding(ctx context.Context, repository string, runID, artifactID int64) (RCActionsBinding, error)
}

// LoadRCProvenanceFromDistDir loads evidence/rc-manifest.json and SHA256SUMS from
// a dist directory (or archive parent containing evidence/).
func LoadRCProvenanceFromDistDir(distDir string) (RCProvenance, error) {
	var out RCProvenance
	distDir = strings.TrimSpace(distDir)
	if distDir == "" {
		return out, fmt.Errorf("artifactqual: rc dist dir required")
	}
	manPath := filepath.Join(distDir, "evidence", "rc-manifest.json")
	sumsPath := filepath.Join(distDir, "SHA256SUMS")
	// Also accept evidence next to archive parent.
	if _, err := os.Stat(manPath); err != nil {
		// try distDir is OUT_DIR
		return out, fmt.Errorf("artifactqual: rc-manifest missing at %s: %w", manPath, err)
	}
	raw, err := os.ReadFile(manPath)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out.Manifest); err != nil {
		return out, fmt.Errorf("artifactqual: rc-manifest json: %w", err)
	}
	out.ManifestPath = manPath
	out.SumsPath = sumsPath
	sumsRaw, err := os.ReadFile(sumsPath)
	if err != nil {
		return out, fmt.Errorf("artifactqual: SHA256SUMS missing: %w", err)
	}
	// First field of first non-empty line.
	for _, line := range strings.Split(string(sumsRaw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 1 {
			out.ObservedSumsDigest = strings.ToLower(fields[0])
			break
		}
	}
	arch := filepath.Join(distDir, out.Manifest.Archive)
	if strings.TrimSpace(out.Manifest.Archive) == "" {
		return out, fmt.Errorf("artifactqual: rc-manifest archive field empty")
	}
	ab, err := os.ReadFile(arch)
	if err != nil {
		// Archive may be the qualify path itself — caller may pass archive path.
		return out, fmt.Errorf("artifactqual: rc archive %s: %w", arch, err)
	}
	sum := sha256.Sum256(ab)
	out.ObservedArchiveDigest = hex.EncodeToString(sum[:])
	return out, nil
}

// LoadRCProvenanceForArchive validates RC files beside the archive (same OUT_DIR).
func LoadRCProvenanceForArchive(archivePath string) (RCProvenance, error) {
	archivePath = strings.TrimSpace(archivePath)
	if archivePath == "" {
		return RCProvenance{}, fmt.Errorf("artifactqual: archive path required")
	}
	dir := filepath.Dir(archivePath)
	p, err := LoadRCProvenanceFromDistDir(dir)
	if err != nil {
		return p, err
	}
	// Re-hash the exact archive path used for qualify.
	ab, err := os.ReadFile(archivePath)
	if err != nil {
		return p, err
	}
	sum := sha256.Sum256(ab)
	p.ObservedArchiveDigest = hex.EncodeToString(sum[:])
	base := filepath.Base(archivePath)
	if strings.TrimSpace(p.Manifest.Archive) != "" && base != filepath.Base(p.Manifest.Archive) {
		return p, fmt.Errorf("artifactqual: archive basename %q != manifest %q", base, p.Manifest.Archive)
	}
	return p, nil
}

// ValidateRCProvenance checks manifest + sums + identity bindings.
// Local archive + caller digest alone is NOT exact RC.
// expectRepository is the owner/repo that must exactly match the Actions binding.
func ValidateRCProvenance(p RCProvenance, expectSHA, expectDigest, binaryCommit, expectRepository string, binding *RCActionsBinding) (ok bool, reasons []string) {
	add := func(s string) { reasons = append(reasons, s) }
	m := p.Manifest
	if strings.TrimSpace(m.Schema) != SchemaRCManifest {
		add("rc_manifest_schema_mismatch")
	}
	if strings.TrimSpace(m.BuildSource) != "release-candidate" {
		add("rc_build_source_not_release_candidate")
	}
	if m.PublicRelease {
		add("rc_public_release_true")
	}
	if !m.DraftOnly {
		add("rc_draft_only_false")
	}
	wantSHA := strings.TrimSpace(expectSHA)
	if wantSHA == "" || strings.TrimSpace(m.CommitSHA) == "" || !strings.EqualFold(wantSHA, m.CommitSHA) {
		add("rc_commit_sha_mismatch")
	}
	bin := strings.ToLower(strings.TrimSpace(binaryCommit))
	if !isExact40Hex(bin) {
		add("rc_binary_commit_missing_or_invalid")
	} else if !strings.EqualFold(bin, m.CommitSHA) {
		add("rc_binary_commit_mismatch")
	}
	manDig := strings.ToLower(strings.TrimSpace(m.ArchiveDigestSHA256))
	obsDig := strings.ToLower(strings.TrimSpace(p.ObservedArchiveDigest))
	sumsDig := strings.ToLower(strings.TrimSpace(p.ObservedSumsDigest))
	expDig := strings.ToLower(strings.TrimSpace(expectDigest))
	if manDig == "" || obsDig == "" || manDig != obsDig {
		add("rc_archive_digest_mismatch_manifest")
	}
	if sumsDig == "" || sumsDig != obsDig {
		add("rc_sha256sums_mismatch")
	}
	if expDig != "" && expDig != obsDig {
		add("rc_expected_digest_mismatch")
	}
	// Authoritative Actions binding required for exact RC.
	if binding == nil {
		add("rc_actions_binding_missing")
	} else {
		wantRepo := strings.TrimSpace(expectRepository)
		gotRepo := strings.TrimSpace(binding.Repository)
		if wantRepo == "" {
			add("rc_actions_expect_repository_missing")
		} else if gotRepo == "" {
			add("rc_actions_repository_missing")
		} else if gotRepo != wantRepo {
			add("rc_actions_repository_mismatch")
		}
		if strings.TrimSpace(binding.WorkflowName) != ReleaseCandidateDraftWorkflow {
			add("rc_actions_workflow_name_mismatch")
		}
		if binding.RunID <= 0 {
			add("rc_actions_run_id_missing")
		}
		if binding.RunAttempt < 1 {
			add("rc_actions_run_attempt_missing")
		}
		if binding.ArtifactID <= 0 {
			add("rc_actions_artifact_id_missing")
		}
		if strings.TrimSpace(binding.ArtifactName) != V090RCArtifactName {
			add("rc_actions_artifact_name_mismatch")
		}
		if binding.ArtifactExpired {
			add("rc_actions_artifact_expired")
		}
		if !strings.EqualFold(strings.TrimSpace(binding.HeadSHA), wantSHA) {
			add("rc_actions_head_sha_mismatch")
		}
		if !strings.EqualFold(strings.TrimSpace(binding.Status), "completed") {
			add("rc_actions_run_not_completed")
		}
		if !strings.EqualFold(strings.TrimSpace(binding.Conclusion), "success") {
			add("rc_actions_run_not_success")
		}
		// Manifest may carry IDs when present — must match binding.
		if m.ActionsRunID > 0 && m.ActionsRunID != binding.RunID {
			add("rc_manifest_run_id_mismatch")
		}
		if m.ActionsArtifactID > 0 && m.ActionsArtifactID != binding.ArtifactID {
			add("rc_manifest_artifact_id_mismatch")
		}
	}
	ok = len(reasons) == 0
	return ok, reasons
}

// FormatRCProvenanceRef is a non-secret evidence reference.
func FormatRCProvenanceRef(p RCProvenance, b *RCActionsBinding) string {
	ref := "rc-manifest:" + shortSHA(p.ObservedArchiveDigest)
	if b != nil {
		ref += fmt.Sprintf(";actions_run:%d;artifact:%d", b.RunID, b.ArtifactID)
	}
	return ref
}
