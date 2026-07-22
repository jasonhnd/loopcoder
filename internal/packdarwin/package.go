package packdarwin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Platform is the only product platform for v0.9.
const Platform = "darwin/arm64"

// ArchiveName pattern for the single product archive.
const ArchiveNamePattern = "loopcoder_%s_darwin_arm64.tar.gz"

// RequiredMembers of the archive.
func RequiredMembers() []string {
	return []string{
		"loopcoder", // binary
		"LICENSE",
		"README.md",
		"docs/reference/v0.9.0-quickstart.md",
		"VERSION",
		"COMMIT",
	}
}

// ForbiddenMembers must not appear (unsupported platforms / junk).
func ForbiddenMembers() []string {
	return []string{
		"loopcoder_windows", "loopcoder_linux", "loopcoder_amd64",
		"*.exe", "windows/", "linux/",
	}
}

// BuildIdentity binds the build to protected commit and clean host.
type BuildIdentity struct {
	// CommitSHA exact protected commit.
	CommitSHA string `json:"commit_sha"`
	// ProtectedBranch e.g. pre-prod or release tag commit.
	ProtectedBranch string `json:"protected_branch"`
	// CleanHosted true when built in clean CI/hosted env (not local dev machine promote).
	CleanHosted bool `json:"clean_hosted"`
	// Version semver without leading v optionally.
	Version string `json:"version"`
}

// Artifact is one built archive record.
type Artifact struct {
	Schema    string `json:"schema"`
	Name      string `json:"name"`
	Platform  string `json:"platform"`
	Version   string `json:"version"`
	CommitSHA string `json:"commit_sha"`
	// Digest is SHA-256 of archive bytes.
	Digest string `json:"sha256"`
	// Members listed in archive.
	Members []string `json:"members"`
	// LocalDev false required for promotion.
	LocalDev bool `json:"local_dev"`
}

// SchemaArtifact id.
const SchemaArtifact = "loopcoder.packdarwin.artifact.v1"

// SchemaDraft id for draft release metadata.
const SchemaDraft = "loopcoder.packdarwin.draft_release.v1"

// SchemaSBOM id.
const SchemaSBOM = "loopcoder.packdarwin.sbom.v1"

// ChecksumFile is SHA256SUMS content lines.
type ChecksumFile struct {
	Lines []string `json:"lines"`
}

// SBOM is a minimal software bill of materials record.
type SBOM struct {
	Schema    string `json:"schema"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	Platform  string `json:"platform"`
	CommitSHA string `json:"commit_sha"`
	// Components are top-level only (binary + notices).
	Components []string `json:"components"`
}

// Provenance binds signature/provenance to archive digest.
type Provenance struct {
	ArchiveDigest string `json:"archive_digest"`
	CommitSHA     string `json:"commit_sha"`
	// SignatureRef is a placeholder record id (not a real signature blob).
	SignatureRef string `json:"signature_ref"`
	// Tool e.g. sigstore/cosign record.
	Tool string `json:"tool"`
}

// DraftRelease is unpublished release metadata.
type DraftRelease struct {
	Schema        string `json:"schema"`
	Tag           string `json:"tag"`
	Draft         bool   `json:"draft"`
	Published     bool   `json:"published"`
	ArchiveDigest string `json:"archive_digest"`
	Platform      string `json:"platform"`
	// No multi-platform assets.
	Assets     []string   `json:"assets"`
	Checksums  []string   `json:"checksums"`
	SBOMDigest string     `json:"sbom_digest,omitempty"`
	Provenance Provenance `json:"provenance"`
	// UnsupportedClaims must be empty for GO.
	UnsupportedClaims []string `json:"unsupported_claims,omitempty"`
}

// ValidateBuildIdentity fails if commit missing or not hosted clean.
func ValidateBuildIdentity(id BuildIdentity) error {
	if len(id.CommitSHA) < 40 {
		return fmt.Errorf("exact commit sha required")
	}
	if strings.TrimSpace(id.Version) == "" {
		return fmt.Errorf("version required")
	}
	if !id.CleanHosted {
		return fmt.Errorf("build must be clean hosted environment; local developer artifacts cannot be promoted")
	}
	return nil
}

// NewArtifact constructs an artifact record after member validation.
func NewArtifact(id BuildIdentity, archiveBytes []byte, members []string) (Artifact, error) {
	if err := ValidateBuildIdentity(id); err != nil {
		return Artifact{}, err
	}
	if err := ValidateMembers(members); err != nil {
		return Artifact{}, err
	}
	sum := sha256.Sum256(archiveBytes)
	name := fmt.Sprintf(ArchiveNamePattern, strings.TrimPrefix(id.Version, "v"))
	return Artifact{
		Schema: SchemaArtifact, Name: name, Platform: Platform,
		Version: id.Version, CommitSHA: id.CommitSHA,
		Digest: hex.EncodeToString(sum[:]), Members: append([]string(nil), members...),
		LocalDev: false,
	}, nil
}

// ValidateMembers ensures required present and forbidden absent.
func ValidateMembers(members []string) error {
	set := map[string]bool{}
	for _, m := range members {
		set[m] = true
	}
	for _, req := range RequiredMembers() {
		if !set[req] {
			return fmt.Errorf("missing required member %s", req)
		}
	}
	for _, m := range members {
		lm := strings.ToLower(m)
		if strings.Contains(lm, "windows") || strings.Contains(lm, "linux") || strings.HasSuffix(lm, ".exe") {
			return fmt.Errorf("forbidden unsupported-platform member %s", m)
		}
	}
	return nil
}

// Checksums builds SHA256SUMS lines for the archive.
func Checksums(a Artifact) ChecksumFile {
	return ChecksumFile{Lines: []string{a.Digest + "  " + a.Name}}
}

// BuildSBOM creates a minimal SBOM for the archive.
func BuildSBOM(a Artifact) SBOM {
	comps := append([]string(nil), a.Members...)
	sort.Strings(comps)
	return SBOM{
		Schema: SchemaSBOM, Name: a.Name, Version: a.Version,
		Platform: a.Platform, CommitSHA: a.CommitSHA, Components: comps,
	}
}

// BindProvenance ties signature ref to archive digest + commit.
func BindProvenance(a Artifact, signatureRef, tool string) (Provenance, error) {
	if a.Digest == "" || a.CommitSHA == "" {
		return Provenance{}, fmt.Errorf("artifact digest and commit required")
	}
	if strings.TrimSpace(signatureRef) == "" {
		return Provenance{}, fmt.Errorf("signature ref required")
	}
	if tool == "" {
		tool = "sigstore"
	}
	return Provenance{
		ArchiveDigest: a.Digest, CommitSHA: a.CommitSHA,
		SignatureRef: signatureRef, Tool: tool,
	}, nil
}

// NewDraftRelease creates unpublished metadata for one archive.
func NewDraftRelease(a Artifact, p Provenance, sbom SBOM) (DraftRelease, error) {
	if a.LocalDev {
		return DraftRelease{}, fmt.Errorf("local developer artifact cannot be promoted")
	}
	if a.Platform != Platform {
		return DraftRelease{}, fmt.Errorf("only %s supported", Platform)
	}
	if p.ArchiveDigest != a.Digest {
		return DraftRelease{}, fmt.Errorf("provenance digest mismatch")
	}
	raw, _ := json.Marshal(sbom)
	sum := sha256.Sum256(raw)
	tag := "v" + strings.TrimPrefix(a.Version, "v")
	return DraftRelease{
		Schema: SchemaDraft, Tag: tag, Draft: true, Published: false,
		ArchiveDigest: a.Digest, Platform: a.Platform,
		Assets: []string{a.Name}, Checksums: Checksums(a).Lines,
		SBOMDigest: hex.EncodeToString(sum[:]), Provenance: p,
	}, nil
}

// ApprovePublication flips draft→published only when draft is complete.
func ApprovePublication(d DraftRelease) (DraftRelease, error) {
	if !d.Draft {
		return d, fmt.Errorf("not a draft")
	}
	if d.ArchiveDigest == "" || len(d.Assets) != 1 {
		return d, fmt.Errorf("incomplete draft")
	}
	if d.Platform != Platform {
		return d, fmt.Errorf("unsupported platform claim")
	}
	if len(d.UnsupportedClaims) > 0 {
		return d, fmt.Errorf("unsupported claims present: %v", d.UnsupportedClaims)
	}
	d.Draft = false
	d.Published = true
	return d, nil
}

// RejectLocalPromotion documents that local artifacts cannot become release.
func RejectLocalPromotion(localDev bool) error {
	if localDev {
		return fmt.Errorf("local developer artifact may not be promoted")
	}
	return nil
}
