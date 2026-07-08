package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/migration"
)

func TestResolveReleaseLatestAndPinned(t *testing.T) {
	cfg := releaseConfig{
		Repo:       "owner/repo",
		APIBaseURL: "https://api.example.test",
		BaseURL:    "https://github.example.test",
	}
	tests := []struct {
		name      string
		requested string
		wantURL   string
	}{
		{
			name:    "latest",
			wantURL: "https://api.example.test/repos/owner/repo/releases/latest",
		},
		{
			name:      "pinned without v",
			requested: "0.3.2",
			wantURL:   "https://api.example.test/repos/owner/repo/releases/tags/v0.3.2",
		},
		{
			name:      "pinned with v",
			requested: "v0.3.1",
			wantURL:   "https://api.example.test/repos/owner/repo/releases/tags/v0.3.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotURL string
			rel, err := resolveRelease(context.Background(), tt.requested, cfg, func(_ context.Context, rawURL string) ([]byte, error) {
				gotURL = rawURL
				return releaseJSON(t, release{TagName: "v0.3.3"}), nil
			})
			if err != nil {
				t.Fatalf("resolveRelease returned error: %v", err)
			}
			if rel.TagName != "v0.3.3" {
				t.Fatalf("TagName = %q, want v0.3.3", rel.TagName)
			}
			if gotURL != tt.wantURL {
				t.Fatalf("URL = %q, want %q", gotURL, tt.wantURL)
			}
		})
	}
}

func TestVerifyChecksumPassAndFail(t *testing.T) {
	archiveName := "loopcoder_0.3.3_linux_amd64.tar.gz"
	archive := []byte("archive bytes")
	sum := sha256.Sum256(archive)
	good := []byte(hex.EncodeToString(sum[:]) + "  " + archiveName + "\n")

	if err := VerifyChecksum(good, archiveName, archive); err != nil {
		t.Fatalf("VerifyChecksum returned error for matching checksum: %v", err)
	}

	bad := []byte(strings.Repeat("0", 64) + "  " + archiveName + "\n")
	err := VerifyChecksum(bad, archiveName, archive)
	if err == nil {
		t.Fatal("VerifyChecksum returned nil error for mismatched checksum")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("error = %v, want checksum mismatch", err)
	}
}

func TestSameReleaseVersionIgnoresOnlyOptionalLeadingV(t *testing.T) {
	tests := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{name: "tagged current", a: "v0.3.3", b: "0.3.3", want: true},
		{name: "tagged target", a: "0.3.3", b: "v0.3.3", want: true},
		{name: "uppercase tag", a: "V0.3.3", b: "0.3.3", want: true},
		{name: "same with whitespace", a: " 0.3.3 ", b: " v0.3.3 ", want: true},
		{name: "different patch", a: "0.3.3", b: "0.3.4", want: false},
		{name: "build metadata not loosened", a: "0.3.3", b: "v0.3.3+build", want: false},
		{name: "double v not loosened", a: "vv0.3.3", b: "v0.3.3", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sameReleaseVersion(tt.a, tt.b); got != tt.want {
				t.Fatalf("sameReleaseVersion(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestRunAlreadyLatestSkipsDownloadWithOptionalV(t *testing.T) {
	apiBase := "https://api.example.test"
	repo := "owner/repo"
	releaseURL := apiBase + "/repos/" + repo + "/releases/latest"
	calls := 0

	result, err := Run(context.Background(), Options{
		CurrentVersion: "0.3.3",
		RuntimeGOOS:    runtime.GOOS,
		RuntimeGOARCH:  runtime.GOARCH,
	}, Deps{
		Getenv: func(key string) string {
			switch key {
			case EnvAPIBaseURL:
				return apiBase
			case EnvUpgradeRepo:
				return repo
			default:
				return ""
			}
		},
		ExecutablePath: func() (string, error) {
			return filepath.Join("bin", wantBinaryFileName()), nil
		},
		HTTPGet: func(_ context.Context, rawURL string) ([]byte, error) {
			calls++
			if rawURL != releaseURL {
				t.Fatalf("unexpected URL %q", rawURL)
			}
			return releaseJSON(t, release{TagName: "v0.3.3"}), nil
		},
		RuntimeGOOS:   runtime.GOOS,
		RuntimeGOARCH: runtime.GOARCH,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("HTTPGet calls = %d, want only release lookup", calls)
	}
	if !result.AlreadyLatest {
		t.Fatalf("AlreadyLatest = false, result = %#v", result)
	}
	if result.TargetVersion != "v0.3.3" || result.CurrentVersion != "0.3.3" {
		t.Fatalf("result versions = %#v", result)
	}
	if result.AssetName != "" || result.VersionBinaryPath != "" || result.StableBinaryPath != "" {
		t.Fatalf("already-latest result should not include install fields: %#v", result)
	}
}

func TestRunFailsWhenAssetMissing(t *testing.T) {
	apiBase := "https://api.example.test"
	repo := "owner/repo"
	assetName := platformAssetName(t, "0.3.3")
	result, err := Run(context.Background(), Options{
		CurrentVersion: "v0.3.2",
		RuntimeGOOS:    runtime.GOOS,
		RuntimeGOARCH:  runtime.GOARCH,
	}, Deps{
		Getenv: func(key string) string {
			switch key {
			case EnvAPIBaseURL:
				return apiBase
			case EnvUpgradeRepo:
				return repo
			default:
				return ""
			}
		},
		ExecutablePath: func() (string, error) {
			return filepath.Join("bin", wantBinaryFileName()), nil
		},
		HTTPGet: func(_ context.Context, rawURL string) ([]byte, error) {
			if rawURL != apiBase+"/repos/"+repo+"/releases/latest" {
				t.Fatalf("unexpected URL %q", rawURL)
			}
			return releaseJSON(t, release{
				TagName: "v0.3.3",
				Assets:  []releaseAsset{{Name: "SHA256SUMS", BrowserDownloadURL: "https://download.example.test/SHA256SUMS"}},
			}), nil
		},
	})
	if err == nil {
		t.Fatal("Run returned nil error, want missing asset")
	}
	if !strings.Contains(err.Error(), "missing release asset "+assetName) {
		t.Fatalf("error = %v, want missing asset %s", err, assetName)
	}
	if result.CurrentVersion != "v0.3.2" || result.CurrentPath == "" {
		t.Fatalf("partial result did not report current selection: %#v", result)
	}
}

func TestRunFailsWhenChecksumSignatureMissing(t *testing.T) {
	apiBase := "https://api.example.test"
	repo := "owner/repo"
	assetName := platformAssetName(t, "0.3.3")
	releaseURL := apiBase + "/repos/" + repo + "/releases/latest"
	calls := 0

	_, err := Run(context.Background(), Options{
		CurrentVersion: "v0.3.2",
		RuntimeGOOS:    runtime.GOOS,
		RuntimeGOARCH:  runtime.GOARCH,
	}, Deps{
		Getenv: func(key string) string {
			switch key {
			case EnvAPIBaseURL:
				return apiBase
			case EnvUpgradeRepo:
				return repo
			default:
				return ""
			}
		},
		ExecutablePath: func() (string, error) {
			return filepath.Join("bin", wantBinaryFileName()), nil
		},
		HTTPGet: func(_ context.Context, rawURL string) ([]byte, error) {
			calls++
			if rawURL != releaseURL {
				t.Fatalf("unexpected URL %q", rawURL)
			}
			return releaseJSON(t, release{
				TagName: "v0.3.3",
				Assets: []releaseAsset{
					{Name: assetName, BrowserDownloadURL: "https://download.example.test/" + assetName},
					{Name: checksumAssetName, BrowserDownloadURL: "https://download.example.test/SHA256SUMS"},
				},
			}), nil
		},
		VerifySignature: func(context.Context, []byte, []byte, string, string) error {
			t.Fatal("VerifySignature should not run when the signature asset is missing")
			return nil
		},
		RuntimeGOOS:   runtime.GOOS,
		RuntimeGOARCH: runtime.GOARCH,
	})
	if err == nil {
		t.Fatal("Run returned nil error, want missing signature asset")
	}
	if !strings.Contains(err.Error(), "missing release asset "+checksumSignatureAssetName) {
		t.Fatalf("error = %v, want missing signature asset", err)
	}
	if calls != 1 {
		t.Fatalf("HTTPGet calls = %d, want only release lookup", calls)
	}
}

func TestRunFailsClosedWhenChecksumSignatureInvalid(t *testing.T) {
	archiveBytes := []byte("archive bytes")
	assetName := platformAssetName(t, "0.3.3")
	sum := sha256.Sum256(archiveBytes)
	checksums := []byte(hex.EncodeToString(sum[:]) + "  " + assetName + "\n")
	signatureBundle := []byte("bad sigstore bundle")
	releaseURL := "https://api.example.test/repos/owner/repo/releases/tags/v0.3.3"
	assetURL := "https://download.example.test/" + assetName
	sumsURL := "https://download.example.test/SHA256SUMS"
	signatureURL := "https://download.example.test/SHA256SUMS.sigstore"

	_, err := Run(context.Background(), Options{
		RequestedVersion: "v0.3.3",
		CurrentVersion:   "v0.3.2",
		RuntimeGOOS:      runtime.GOOS,
		RuntimeGOARCH:    runtime.GOARCH,
	}, Deps{
		Getenv: func(key string) string {
			switch key {
			case EnvAPIBaseURL:
				return "https://api.example.test"
			case EnvUpgradeRepo:
				return "owner/repo"
			default:
				return ""
			}
		},
		ExecutablePath: func() (string, error) {
			return filepath.Join("old", wantBinaryFileName()), nil
		},
		HTTPGet: mapGetter(t, map[string][]byte{
			releaseURL: releaseJSON(t, release{
				TagName: "v0.3.3",
				Assets: []releaseAsset{
					{Name: assetName, BrowserDownloadURL: assetURL},
					{Name: checksumAssetName, BrowserDownloadURL: sumsURL},
					{Name: checksumSignatureAssetName, BrowserDownloadURL: signatureURL},
				},
			}),
			sumsURL:      checksums,
			signatureURL: signatureBundle,
		}),
		VerifySignature: func(_ context.Context, artifact []byte, bundle []byte, identity string, issuer string) error {
			if !reflect.DeepEqual(artifact, checksums) || !reflect.DeepEqual(bundle, signatureBundle) {
				t.Fatalf("signature verifier inputs = %q/%q, want %q/%q", artifact, bundle, checksums, signatureBundle)
			}
			if identity != "https://github.com/owner/repo/.github/workflows/release.yml@refs/tags/v0.3.3" {
				t.Fatalf("identity = %q", identity)
			}
			if issuer != defaultCosignIssuer {
				t.Fatalf("issuer = %q", issuer)
			}
			return errors.New("wrong identity")
		},
		ExtractBinary: func(string, []byte, string) ([]byte, error) {
			t.Fatal("ExtractBinary should not run when signature verification fails")
			return nil, nil
		},
		RuntimeGOOS:   runtime.GOOS,
		RuntimeGOARCH: runtime.GOARCH,
	})
	if err == nil {
		t.Fatal("Run returned nil error, want signature failure")
	}
	if !strings.Contains(err.Error(), "verify SHA256SUMS signature") || !strings.Contains(err.Error(), "wrong identity") {
		t.Fatalf("error = %v, want signature failure with verifier detail", err)
	}
}

func TestRunKeepsChecksumVerificationAfterValidSignature(t *testing.T) {
	archiveBytes := []byte("archive bytes")
	assetName := platformAssetName(t, "0.3.3")
	checksums := []byte(strings.Repeat("0", 64) + "  " + assetName + "\n")
	signatureBundle := []byte("valid sigstore bundle")
	releaseURL := "https://api.example.test/repos/owner/repo/releases/tags/v0.3.3"
	assetURL := "https://download.example.test/" + assetName
	sumsURL := "https://download.example.test/SHA256SUMS"
	signatureURL := "https://download.example.test/SHA256SUMS.sigstore"

	_, err := Run(context.Background(), Options{
		RequestedVersion: "v0.3.3",
		CurrentVersion:   "v0.3.2",
		RuntimeGOOS:      runtime.GOOS,
		RuntimeGOARCH:    runtime.GOARCH,
	}, Deps{
		Getenv: func(key string) string {
			switch key {
			case EnvAPIBaseURL:
				return "https://api.example.test"
			case EnvUpgradeRepo:
				return "owner/repo"
			default:
				return ""
			}
		},
		ExecutablePath: func() (string, error) {
			return filepath.Join("old", wantBinaryFileName()), nil
		},
		HTTPGet: mapGetter(t, map[string][]byte{
			releaseURL: releaseJSON(t, release{
				TagName: "v0.3.3",
				Assets: []releaseAsset{
					{Name: assetName, BrowserDownloadURL: assetURL},
					{Name: checksumAssetName, BrowserDownloadURL: sumsURL},
					{Name: checksumSignatureAssetName, BrowserDownloadURL: signatureURL},
				},
			}),
			assetURL:     archiveBytes,
			sumsURL:      checksums,
			signatureURL: signatureBundle,
		}),
		VerifySignature: verifySignatureFixture(t, checksums, signatureBundle),
		ExtractBinary: func(string, []byte, string) ([]byte, error) {
			t.Fatal("ExtractBinary should not run when checksum verification fails")
			return nil, nil
		},
		RuntimeGOOS:   runtime.GOOS,
		RuntimeGOARCH: runtime.GOARCH,
	})
	if err == nil {
		t.Fatal("Run returned nil error, want checksum mismatch")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("error = %v, want checksum mismatch", err)
	}
}

func TestRunInstallsStagedAtomic(t *testing.T) {
	archiveBytes := []byte("archive bytes")
	binaryBytes := []byte("new loopcoder binary")
	assetName := platformAssetName(t, "0.3.3")
	sum := sha256.Sum256(archiveBytes)
	checksums := []byte(hex.EncodeToString(sum[:]) + "  " + assetName + "\n")
	signatureBundle := []byte("valid sigstore bundle")
	releaseURL := "https://api.example.test/repos/owner/repo/releases/tags/v0.3.3"
	assetURL := "https://download.example.test/" + assetName
	sumsURL := "https://download.example.test/SHA256SUMS"
	signatureURL := "https://download.example.test/SHA256SUMS.sigstore"
	fsys := newFakeFS()
	layout := home.New(filepath.Join("home", ".loopcoder"))
	stablePath := layout.StableBinaryPath()
	refreshCalled := false

	result, err := Run(context.Background(), Options{
		RequestedVersion: "v0.3.3",
		CurrentVersion:   "v0.3.2",
		RuntimeGOOS:      runtime.GOOS,
		RuntimeGOARCH:    runtime.GOARCH,
	}, Deps{
		Getenv: func(key string) string {
			switch key {
			case EnvAPIBaseURL:
				return "https://api.example.test"
			case EnvUpgradeRepo:
				return "owner/repo"
			default:
				return ""
			}
		},
		ExecutablePath: func() (string, error) {
			return filepath.Join("old", wantBinaryFileName()), nil
		},
		HomeLayout: func() (home.Layout, error) {
			return layout, nil
		},
		HTTPGet: mapGetter(t, map[string][]byte{
			releaseURL: releaseJSON(t, release{
				TagName: "v0.3.3",
				Assets: []releaseAsset{
					{Name: assetName, BrowserDownloadURL: assetURL},
					{Name: checksumAssetName, BrowserDownloadURL: sumsURL},
					{Name: checksumSignatureAssetName, BrowserDownloadURL: signatureURL},
				},
			}),
			assetURL:     archiveBytes,
			sumsURL:      checksums,
			signatureURL: signatureBundle,
		}),
		VerifySignature: verifySignatureFixture(t, checksums, signatureBundle),
		ExtractBinary: func(gotArchiveName string, data []byte, binaryName string) ([]byte, error) {
			if gotArchiveName != assetName {
				t.Fatalf("archiveName = %q, want %q", gotArchiveName, assetName)
			}
			if !reflect.DeepEqual(data, archiveBytes) {
				t.Fatalf("archive bytes = %q, want %q", data, archiveBytes)
			}
			if binaryName != wantBinaryFileName() {
				t.Fatalf("binaryName = %q, want %q", binaryName, wantBinaryFileName())
			}
			return binaryBytes, nil
		},
		MkdirAll:  fsys.MkdirAll,
		WriteFile: fsys.WriteFile,
		Chmod:     fsys.Chmod,
		Rename:    fsys.Rename,
		Remove:    fsys.Remove,
		RunCommand: func(_ context.Context, name string, args ...string) (CommandResult, error) {
			refreshCalled = true
			if name != stablePath {
				t.Fatalf("refresh binary = %q, want selected stable binary %q", name, stablePath)
			}
			if !reflect.DeepEqual(args, []string{"skill", "install"}) {
				t.Fatalf("refresh args = %#v, want skill install", args)
			}
			return CommandResult{
				Stdout: "loopcoder skill install complete\n" +
					"  directory " + filepath.Join("home", ".claude", "skills", "loopcoder") + "\n" +
					"  created " + filepath.Join("home", ".claude", "skills", "loopcoder", "SKILL.md") + "\n" +
					"  unchanged " + filepath.Join("home", ".claude", "skills", "loopcoder", "AGENTS.md") + "\n",
			}, nil
		},
		RuntimeGOOS:   runtime.GOOS,
		RuntimeGOARCH: runtime.GOARCH,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	versionPath, err := layout.VersionBinaryPath("v0.3.3")
	if err != nil {
		t.Fatalf("VersionBinaryPath returned error: %v", err)
	}
	if result.TargetVersion != "v0.3.3" || result.AssetName != assetName {
		t.Fatalf("result release fields = %#v", result)
	}
	if result.VersionBinaryPath != versionPath || result.StableBinaryPath != stablePath {
		t.Fatalf("result paths = %#v, want version=%q stable=%q", result, versionPath, stablePath)
	}
	if got := string(fsys.files[versionPath]); got != string(binaryBytes) {
		t.Fatalf("versioned binary bytes = %q, want %q", got, binaryBytes)
	}
	if got := string(fsys.files[stablePath]); got != string(binaryBytes) {
		t.Fatalf("stable binary bytes = %q, want %q", got, binaryBytes)
	}
	if !fsys.dirs[filepath.Dir(versionPath)] || !fsys.dirs[filepath.Dir(stablePath)] {
		t.Fatalf("expected version and bin dirs to be created: %#v", fsys.dirs)
	}
	if len(fsys.renames) != 1 || fsys.renames[0][1] != stablePath {
		t.Fatalf("renames = %#v, want one atomic rename to %q", fsys.renames, stablePath)
	}
	if !refreshCalled {
		t.Fatal("skill refresh command was not called")
	}
	if result.SkillRefresh.BinaryPath != stablePath {
		t.Fatalf("SkillRefresh.BinaryPath = %q, want %q", result.SkillRefresh.BinaryPath, stablePath)
	}
	if result.SkillRefresh.Warning != "" {
		t.Fatalf("SkillRefresh.Warning = %q, want empty", result.SkillRefresh.Warning)
	}
	assertSkillRefreshStatus(t, result.SkillRefresh.Files, filepath.Join("home", ".claude", "skills", "loopcoder", "SKILL.md"), SkillRefreshFileCreated)
	assertSkillRefreshStatus(t, result.SkillRefresh.Files, filepath.Join("home", ".claude", "skills", "loopcoder", "AGENTS.md"), SkillRefreshFileUnchanged)
}

func TestRunReportsSkillRefreshFailureAsWarning(t *testing.T) {
	archiveBytes := []byte("archive bytes")
	binaryBytes := []byte("new loopcoder binary")
	assetName := platformAssetName(t, "0.3.3")
	sum := sha256.Sum256(archiveBytes)
	checksums := []byte(hex.EncodeToString(sum[:]) + "  " + assetName + "\n")
	signatureBundle := []byte("valid sigstore bundle")
	releaseURL := "https://api.example.test/repos/owner/repo/releases/tags/v0.3.3"
	assetURL := "https://download.example.test/" + assetName
	sumsURL := "https://download.example.test/SHA256SUMS"
	signatureURL := "https://download.example.test/SHA256SUMS.sigstore"
	fsys := newFakeFS()
	layout := home.New(filepath.Join("home", ".loopcoder"))

	result, err := Run(context.Background(), Options{
		RequestedVersion: "v0.3.3",
		CurrentVersion:   "v0.3.2",
		RuntimeGOOS:      runtime.GOOS,
		RuntimeGOARCH:    runtime.GOARCH,
	}, Deps{
		Getenv: func(key string) string {
			switch key {
			case EnvAPIBaseURL:
				return "https://api.example.test"
			case EnvUpgradeRepo:
				return "owner/repo"
			default:
				return ""
			}
		},
		ExecutablePath: func() (string, error) {
			return filepath.Join("old", wantBinaryFileName()), nil
		},
		HomeLayout: func() (home.Layout, error) {
			return layout, nil
		},
		HTTPGet: mapGetter(t, map[string][]byte{
			releaseURL: releaseJSON(t, release{
				TagName: "v0.3.3",
				Assets: []releaseAsset{
					{Name: assetName, BrowserDownloadURL: assetURL},
					{Name: checksumAssetName, BrowserDownloadURL: sumsURL},
					{Name: checksumSignatureAssetName, BrowserDownloadURL: signatureURL},
				},
			}),
			assetURL:     archiveBytes,
			sumsURL:      checksums,
			signatureURL: signatureBundle,
		}),
		VerifySignature: verifySignatureFixture(t, checksums, signatureBundle),
		ExtractBinary: func(string, []byte, string) ([]byte, error) {
			return binaryBytes, nil
		},
		MkdirAll:      fsys.MkdirAll,
		WriteFile:     fsys.WriteFile,
		Chmod:         fsys.Chmod,
		Rename:        fsys.Rename,
		Remove:        fsys.Remove,
		RuntimeGOOS:   runtime.GOOS,
		RuntimeGOARCH: runtime.GOARCH,
		RunCommand: func(context.Context, string, ...string) (CommandResult, error) {
			return CommandResult{Stderr: "permission denied", ExitCode: 1}, nil
		},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.VersionBinaryPath == "" || result.StableBinaryPath == "" {
		t.Fatalf("binary upgrade did not succeed: %#v", result)
	}
	if result.SkillRefresh.Warning == "" {
		t.Fatalf("SkillRefresh.Warning is empty, want refresh warning")
	}
	if !strings.Contains(result.SkillRefresh.Warning, "permission denied") {
		t.Fatalf("SkillRefresh.Warning = %q, want permission detail", result.SkillRefresh.Warning)
	}
}

func TestRunRefreshesSkillFromNewBinaryAndUpdatesStaleFiles(t *testing.T) {
	helperPath := buildSkillInstallHelper(t)
	helperBytes, err := os.ReadFile(helperPath)
	if err != nil {
		t.Fatalf("read helper binary: %v", err)
	}

	archiveBytes := []byte("archive bytes")
	assetName := platformAssetName(t, "0.3.3")
	sum := sha256.Sum256(archiveBytes)
	checksums := []byte(hex.EncodeToString(sum[:]) + "  " + assetName + "\n")
	signatureBundle := []byte("valid sigstore bundle")
	releaseURL := "https://api.example.test/repos/owner/repo/releases/tags/v0.3.3"
	assetURL := "https://download.example.test/" + assetName
	sumsURL := "https://download.example.test/SHA256SUMS"
	signatureURL := "https://download.example.test/SHA256SUMS.sigstore"
	loopcoderHome := filepath.Join(t.TempDir(), ".loopcoder")
	layout := home.New(loopcoderHome)
	userHome := t.TempDir()
	skillDir := filepath.Join(userHome, ".claude", "skills", "loopcoder")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("create stale skill dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("old skill\n"), 0o644); err != nil {
		t.Fatalf("write stale skill: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "AGENTS.md"), []byte("old agents\n"), 0o644); err != nil {
		t.Fatalf("write stale agents: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "README.md"), []byte("user note\n"), 0o644); err != nil {
		t.Fatalf("write unrelated file: %v", err)
	}

	stablePath := layout.StableBinaryPath()
	result, err := Run(context.Background(), Options{
		RequestedVersion: "v0.3.3",
		CurrentVersion:   "v0.3.2",
		RuntimeGOOS:      runtime.GOOS,
		RuntimeGOARCH:    runtime.GOARCH,
	}, Deps{
		Getenv: func(key string) string {
			switch key {
			case EnvAPIBaseURL:
				return "https://api.example.test"
			case EnvUpgradeRepo:
				return "owner/repo"
			default:
				return ""
			}
		},
		ExecutablePath: func() (string, error) {
			return filepath.Join("old", wantBinaryFileName()), nil
		},
		HomeLayout: func() (home.Layout, error) {
			return layout, nil
		},
		HTTPGet: mapGetter(t, map[string][]byte{
			releaseURL: releaseJSON(t, release{
				TagName: "v0.3.3",
				Assets: []releaseAsset{
					{Name: assetName, BrowserDownloadURL: assetURL},
					{Name: checksumAssetName, BrowserDownloadURL: sumsURL},
					{Name: checksumSignatureAssetName, BrowserDownloadURL: signatureURL},
				},
			}),
			assetURL:     archiveBytes,
			sumsURL:      checksums,
			signatureURL: signatureBundle,
		}),
		VerifySignature: verifySignatureFixture(t, checksums, signatureBundle),
		ExtractBinary: func(string, []byte, string) ([]byte, error) {
			return helperBytes, nil
		},
		MkdirAll: os.MkdirAll,
		WriteFile: func(path string, data []byte, mode fs.FileMode) error {
			return os.WriteFile(path, data, mode)
		},
		Chmod:  os.Chmod,
		Rename: os.Rename,
		Remove: func(path string) error {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return nil
		},
		RuntimeGOOS:   runtime.GOOS,
		RuntimeGOARCH: runtime.GOARCH,
		RunCommand: func(ctx context.Context, name string, args ...string) (CommandResult, error) {
			if name != stablePath {
				t.Fatalf("refresh binary = %q, want newly selected stable binary %q", name, stablePath)
			}
			cmd := exec.CommandContext(ctx, name, args...)
			cmd.Env = append(os.Environ(), "HOME="+userHome, "USERPROFILE="+userHome)
			var stdout strings.Builder
			var stderr strings.Builder
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()
			result := CommandResult{Stdout: stdout.String(), Stderr: stderr.String()}
			if err == nil {
				return result, nil
			}
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				result.ExitCode = exitErr.ExitCode()
				return result, nil
			}
			return result, err
		},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.SkillRefresh.Warning != "" {
		t.Fatalf("SkillRefresh.Warning = %q, want empty", result.SkillRefresh.Warning)
	}
	assertSkillRefreshStatus(t, result.SkillRefresh.Files, filepath.Join(skillDir, "SKILL.md"), SkillRefreshFileUpdated)
	assertSkillRefreshStatus(t, result.SkillRefresh.Files, filepath.Join(skillDir, "AGENTS.md"), SkillRefreshFileUpdated)
	if got := readFileString(t, filepath.Join(skillDir, "SKILL.md")); got != "new skill content\n" {
		t.Fatalf("SKILL.md = %q, want new binary skill content", got)
	}
	if got := readFileString(t, filepath.Join(skillDir, "AGENTS.md")); got != "new agents content\n" {
		t.Fatalf("AGENTS.md = %q, want new binary agents content", got)
	}
	if got := readFileString(t, filepath.Join(skillDir, "README.md")); got != "user note\n" {
		t.Fatalf("unrelated file = %q, want preserved", got)
	}
}

func TestRunReports060MigrationStatusAfterUpgrade(t *testing.T) {
	archiveBytes := []byte("archive bytes")
	binaryBytes := []byte("new loopcoder binary")
	assetName := platformAssetName(t, "0.6.0")
	sum := sha256.Sum256(archiveBytes)
	checksums := []byte(hex.EncodeToString(sum[:]) + "  " + assetName + "\n")
	signatureBundle := []byte("valid sigstore bundle")
	releaseURL := "https://api.example.test/repos/owner/repo/releases/tags/v0.6.0"
	assetURL := "https://download.example.test/" + assetName
	sumsURL := "https://download.example.test/SHA256SUMS"
	signatureURL := "https://download.example.test/SHA256SUMS.sigstore"
	fsys := newFakeFS()
	layout := home.New(filepath.Join("home", ".loopcoder"))
	repo := t.TempDir()
	deliveryBody := "version: 1\nmin_loopcoder_version: 0.5.0\n" + migration.LegacyReportConfigRoot + ":\n  channel: chat\n"
	settingsBody := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"` + migration.LegacyReporterHookCommand + `","timeout":10}]}]}}`
	attemptBody := `{"issue":589,"attempt":1,"` + migration.LegacyReportStateKey + `":{"role":"worker","provider":"codex","model":"gpt-5"}}`
	writeTextFile(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeTextFile(t, filepath.Join(repo, ".delivery.yml"), deliveryBody)
	writeTextFile(t, filepath.Join(repo, ".claude", "settings.json"), settingsBody)
	writeTextFile(t, filepath.Join(repo, ".loopcoder", "hooks", migration.LegacyReporterHookName), "{}\n")
	writeTextFile(t, filepath.Join(repo, ".loopcoder", "runs", "run-20260708T000000Z-issue-589", "workers", "job.attempt.json"), attemptBody)

	result, err := Run(context.Background(), Options{
		RequestedVersion: "v0.6.0",
		CurrentVersion:   "v0.5.4",
		RuntimeGOOS:      runtime.GOOS,
		RuntimeGOARCH:    runtime.GOARCH,
	}, Deps{
		Getenv: func(key string) string {
			switch key {
			case EnvAPIBaseURL:
				return "https://api.example.test"
			case EnvUpgradeRepo:
				return "owner/repo"
			case migration.LegacyReporterScopeEnv:
				return "auto"
			default:
				return ""
			}
		},
		ExecutablePath: func() (string, error) {
			return filepath.Join("old", wantBinaryFileName()), nil
		},
		Getwd: func() (string, error) {
			return repo, nil
		},
		HomeLayout: func() (home.Layout, error) {
			return layout, nil
		},
		HTTPGet: mapGetter(t, map[string][]byte{
			releaseURL: releaseJSON(t, release{
				TagName: "v0.6.0",
				Assets: []releaseAsset{
					{Name: assetName, BrowserDownloadURL: assetURL},
					{Name: checksumAssetName, BrowserDownloadURL: sumsURL},
					{Name: checksumSignatureAssetName, BrowserDownloadURL: signatureURL},
				},
			}),
			assetURL:     archiveBytes,
			sumsURL:      checksums,
			signatureURL: signatureBundle,
		}),
		VerifySignature: func(_ context.Context, artifact []byte, bundle []byte, identity string, issuer string) error {
			if !reflect.DeepEqual(artifact, checksums) || !reflect.DeepEqual(bundle, signatureBundle) {
				t.Fatalf("signature verifier inputs = %q/%q, want %q/%q", artifact, bundle, checksums, signatureBundle)
			}
			if identity != "https://github.com/owner/repo/.github/workflows/release.yml@refs/tags/v0.6.0" {
				t.Fatalf("identity = %q, want v0.6.0 release workflow identity", identity)
			}
			if issuer != defaultCosignIssuer {
				t.Fatalf("issuer = %q, want %q", issuer, defaultCosignIssuer)
			}
			return nil
		},
		ExtractBinary: func(string, []byte, string) ([]byte, error) {
			return binaryBytes, nil
		},
		MkdirAll:  fsys.MkdirAll,
		WriteFile: fsys.WriteFile,
		Chmod:     fsys.Chmod,
		Rename:    fsys.Rename,
		Remove:    fsys.Remove,
		RunCommand: func(_ context.Context, name string, args ...string) (CommandResult, error) {
			return CommandResult{
				Stdout: "loopcoder skill install complete\n" +
					"  directory " + filepath.Join("home", ".claude", "skills", "loopcoder") + "\n" +
					"  updated " + filepath.Join("home", ".claude", "skills", "loopcoder", "SKILL.md") + "\n" +
					"  updated " + filepath.Join("home", ".claude", "skills", "loopcoder", "AGENTS.md") + "\n",
			}, nil
		},
		RuntimeGOOS:   runtime.GOOS,
		RuntimeGOARCH: runtime.GOARCH,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !result.VersionStatus.BreakingBoundary {
		t.Fatalf("BreakingBoundary = false, status = %#v", result.VersionStatus)
	}
	if !result.VersionStatus.CompatibilityAliasesActive {
		t.Fatalf("CompatibilityAliasesActive = false")
	}
	status := result.MigrationStatus
	if !status.RepoAvailable || status.RepoPath != repo {
		t.Fatalf("repo status = %#v, want available repo %s", status, repo)
	}
	if len(status.ConfigDiagnostics) != 2 {
		t.Fatalf("ConfigDiagnostics = %#v, want root and channel diagnostics", status.ConfigDiagnostics)
	}
	if len(status.EnvDiagnostics) != 1 {
		t.Fatalf("EnvDiagnostics = %#v, want legacy env diagnostic", status.EnvDiagnostics)
	}
	if len(status.HookDiagnostics) != 1 {
		t.Fatalf("HookDiagnostics = %#v, want legacy hook command diagnostic", status.HookDiagnostics)
	}
	if len(status.OldSurfaceDiagnostics) != 2 {
		t.Fatalf("OldSurfaceDiagnostics = %#v, want hook-state and state-key diagnostics", status.OldSurfaceDiagnostics)
	}
	if missing := missingManagedSkillFiles(result.SkillRefresh.Files); len(missing) > 0 {
		t.Fatalf("skill refresh missing managed files: %v", missing)
	}
}

func TestParseSkillInstallOutputRequiresBothManagedFiles(t *testing.T) {
	_, _, err := parseSkillInstallOutput("loopcoder skill install complete\n" +
		"  directory /home/.claude/skills/loopcoder\n" +
		"  updated /home/.claude/skills/loopcoder/SKILL.md\n")
	if err == nil {
		t.Fatal("parseSkillInstallOutput returned nil error, want missing AGENTS.md")
	}
	if !strings.Contains(err.Error(), "AGENTS.md") {
		t.Fatalf("error = %v, want missing AGENTS.md", err)
	}
}

func TestExecRunCommandCapturesOutputAndNonZeroExit(t *testing.T) {
	withTestCommandCap(t, 2*time.Second)

	result, err := execRunCommand(context.Background(), os.Args[0], "-test.run=TestUpgradeExecHelper", "--", "stdout", "ok")
	if err != nil {
		t.Fatalf("execRunCommand returned error: %v", err)
	}
	if result.ExitCode != 0 || result.Stdout != "ok\n" || result.Stderr != "" {
		t.Fatalf("result = %#v, want stdout ok exit 0", result)
	}

	result, err = execRunCommand(context.Background(), os.Args[0], "-test.run=TestUpgradeExecHelper", "--", "combined-exit", "stdout", "stderr", "7")
	if err != nil {
		t.Fatalf("execRunCommand returned error for non-zero exit: %v", err)
	}
	if result.ExitCode != 7 || result.Stdout != "stdout\n" || result.Stderr != "stderr\n" {
		t.Fatalf("result = %#v, want captured output and exit 7", result)
	}
}

func TestExecRunCommandTimesOut(t *testing.T) {
	withTestCommandCap(t, 50*time.Millisecond)

	start := time.Now()
	_, err := execRunCommand(context.Background(), os.Args[0], "-test.run=TestUpgradeExecHelper", "--", "sleep", "500ms")
	if err == nil {
		t.Fatal("execRunCommand error = nil, want timeout")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("execRunCommand elapsed = %s, want bounded timeout", elapsed)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %q, want timeout", err.Error())
	}
}

func withTestCommandCap(t *testing.T, hardCap time.Duration) {
	t.Helper()
	oldHardCap := commandHardCap
	commandHardCap = hardCap
	t.Setenv("GO_WANT_UPGRADE_HELPER", "1")
	t.Cleanup(func() {
		commandHardCap = oldHardCap
	})
}

func TestUpgradeExecHelper(t *testing.T) {
	if os.Getenv("GO_WANT_UPGRADE_HELPER") != "1" {
		return
	}
	runExecHelper()
}

func runExecHelper() {
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		fmt.Fprintln(os.Stderr, "missing helper mode")
		os.Exit(2)
	}
	mode := os.Args[separator+1]
	args := os.Args[separator+2:]
	switch mode {
	case "stdout":
		fmt.Fprintln(os.Stdout, args[0])
	case "combined-exit":
		fmt.Fprintln(os.Stdout, args[0])
		fmt.Fprintln(os.Stderr, args[1])
		os.Exit(parseHelperInt(args[2]))
	case "sleep":
		time.Sleep(parseHelperDuration(args[0]))
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
		os.Exit(2)
	}
	os.Exit(0)
}

func parseHelperDuration(value string) time.Duration {
	duration, err := time.ParseDuration(value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse duration %q: %v\n", value, err)
		os.Exit(2)
	}
	return duration
}

func parseHelperInt(value string) int {
	var n int
	if _, err := fmt.Sscanf(value, "%d", &n); err != nil {
		fmt.Fprintf(os.Stderr, "parse int %q: %v\n", value, err)
		os.Exit(2)
	}
	return n
}

func TestReplaceStableBinaryWindowsSchedulesDeferredReplaceWhenAtomicAndBackupRenamesFail(t *testing.T) {
	fsys := newFakeFS()
	stablePath := filepath.Join("home", ".loopcoder", "bin", "loopcoder.exe")
	tmpPath := filepath.Join("home", ".loopcoder", "bin", ".loopcoder.v0.3.3.tmp")
	backupPath := stablePath + ".old"
	pendingPath := stablePath + ".new"
	oldBinary := []byte("old running binary")
	newBinary := []byte("new loopcoder binary")
	if err := fsys.WriteFile(stablePath, oldBinary, 0o755); err != nil {
		t.Fatalf("WriteFile stable returned error: %v", err)
	}
	if err := fsys.WriteFile(tmpPath, newBinary, 0o755); err != nil {
		t.Fatalf("WriteFile tmp returned error: %v", err)
	}

	atomicErr := errors.New("running executable blocks atomic replace")
	backupErr := errors.New("running executable blocks backup rename")
	fsys.failRename(tmpPath, stablePath, atomicErr)
	fsys.failRename(stablePath, backupPath, backupErr)
	var scheduled [][2]string

	deferred, pending, err := replaceStableBinary(Deps{
		Rename: fsys.Rename,
		Remove: fsys.Remove,
		ScheduleReplace: func(source string, target string) error {
			scheduled = append(scheduled, [2]string{source, target})
			return nil
		},
		RuntimeGOOS: "windows",
	}, tmpPath, stablePath)
	if err != nil {
		t.Fatalf("replaceStableBinary returned error: %v", err)
	}
	if !deferred {
		t.Fatal("deferred = false, want true")
	}
	if pending != pendingPath {
		t.Fatalf("pending = %q, want %q", pending, pendingPath)
	}
	if got := string(fsys.files[stablePath]); got != string(oldBinary) {
		t.Fatalf("stable binary bytes = %q, want preserved old binary %q", got, oldBinary)
	}
	if got := string(fsys.files[pendingPath]); got != string(newBinary) {
		t.Fatalf("pending binary bytes = %q, want staged new binary %q", got, newBinary)
	}
	if _, ok := fsys.files[tmpPath]; ok {
		t.Fatalf("tmp binary %q still exists after staging pending replacement", tmpPath)
	}
	if _, ok := fsys.files[backupPath]; ok {
		t.Fatalf("backup binary %q exists even though backup rename failed", backupPath)
	}
	if !reflect.DeepEqual(scheduled, [][2]string{{pendingPath, stablePath}}) {
		t.Fatalf("scheduled replacements = %#v, want pending replacement %#v", scheduled, [][2]string{{pendingPath, stablePath}})
	}
	wantAttempts := [][2]string{
		{tmpPath, stablePath},
		{stablePath, backupPath},
		{tmpPath, pendingPath},
	}
	if !reflect.DeepEqual(fsys.renameAttempts, wantAttempts) {
		t.Fatalf("rename attempts = %#v, want %#v", fsys.renameAttempts, wantAttempts)
	}
	if !reflect.DeepEqual(fsys.renames, [][2]string{{tmpPath, pendingPath}}) {
		t.Fatalf("successful renames = %#v, want pending stage rename", fsys.renames)
	}
}

func releaseJSON(t *testing.T, rel release) []byte {
	t.Helper()
	data, err := json.Marshal(rel)
	if err != nil {
		t.Fatalf("marshal release JSON: %v", err)
	}
	return data
}

func mapGetter(t *testing.T, responses map[string][]byte) func(context.Context, string) ([]byte, error) {
	t.Helper()
	return func(_ context.Context, rawURL string) ([]byte, error) {
		data, ok := responses[rawURL]
		if !ok {
			t.Fatalf("unexpected URL %q", rawURL)
		}
		return data, nil
	}
}

func verifySignatureFixture(t *testing.T, wantArtifact []byte, wantBundle []byte) func(context.Context, []byte, []byte, string, string) error {
	t.Helper()
	return func(_ context.Context, artifact []byte, bundle []byte, identity string, issuer string) error {
		if !reflect.DeepEqual(artifact, wantArtifact) {
			t.Fatalf("signature artifact = %q, want %q", artifact, wantArtifact)
		}
		if !reflect.DeepEqual(bundle, wantBundle) {
			t.Fatalf("signature bundle = %q, want %q", bundle, wantBundle)
		}
		if identity != "https://github.com/owner/repo/.github/workflows/release.yml@refs/tags/v0.3.3" {
			t.Fatalf("signature identity = %q, want owner/repo release workflow identity", identity)
		}
		if issuer != defaultCosignIssuer {
			t.Fatalf("signature issuer = %q, want %q", issuer, defaultCosignIssuer)
		}
		return nil
	}
}

func platformAssetName(t *testing.T, version string) string {
	t.Helper()
	kind, err := archiveKind(runtime.GOOS)
	if err != nil {
		t.Fatalf("archiveKind returned error: %v", err)
	}
	if err := validateArch(runtime.GOARCH); err != nil {
		t.Fatalf("validateArch returned error: %v", err)
	}
	return "loopcoder_" + version + "_" + runtime.GOOS + "_" + runtime.GOARCH + "." + kind
}

func wantBinaryFileName() string {
	if runtime.GOOS == "windows" {
		return "loopcoder.exe"
	}
	return "loopcoder"
}

func assertSkillRefreshStatus(t *testing.T, files []SkillRefreshFileResult, path string, status SkillRefreshFileStatus) {
	t.Helper()
	clean := filepath.Clean(path)
	for _, file := range files {
		if filepath.Clean(file.Path) == clean {
			if file.Status != status {
				t.Fatalf("%s status = %s, want %s", path, file.Status, status)
			}
			return
		}
	}
	t.Fatalf("%s not found in skill refresh results: %#v", path, files)
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func writeTextFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func buildSkillInstallHelper(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "main.go")
	binaryPath := filepath.Join(dir, "loopcoder-helper"+executableSuffix())
	source := `package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) != 3 || os.Args[1] != "skill" || os.Args[2] != "install" {
		fmt.Fprintf(os.Stderr, "unexpected args: %v\n", os.Args[1:])
		os.Exit(2)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "user home: %v\n", err)
		os.Exit(1)
	}
	dir := filepath.Join(home, ".claude", "skills", "loopcoder")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("loopcoder skill install complete")
	fmt.Println("  directory " + dir)
	writeManaged(filepath.Join(dir, "SKILL.md"), []byte("new skill content\n"))
	writeManaged(filepath.Join(dir, "AGENTS.md"), []byte("new agents content\n"))
}

func writeManaged(path string, data []byte) {
	status := "updated"
	current, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			status = "created"
		} else {
			fmt.Fprintf(os.Stderr, "read %s: %v\n", path, err)
			os.Exit(1)
		}
	} else if bytes.Equal(current, data) {
		status = "unchanged"
	}
	if status != "unchanged" {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", path, err)
			os.Exit(1)
		}
	}
	fmt.Printf("  %s %s\n", status, path)
}
`
	if err := os.WriteFile(sourcePath, []byte(source), 0o644); err != nil {
		t.Fatalf("write helper source: %v", err)
	}
	cmd := exec.Command("go", "build", "-o", binaryPath, sourcePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build helper binary: %v\n%s", err, string(output))
	}
	return binaryPath
}

func executableSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

type fakeFS struct {
	dirs           map[string]bool
	files          map[string][]byte
	modes          map[string]fs.FileMode
	renameErrs     map[[2]string]error
	renameAttempts [][2]string
	renames        [][2]string
}

func newFakeFS() *fakeFS {
	return &fakeFS{
		dirs:       map[string]bool{},
		files:      map[string][]byte{},
		modes:      map[string]fs.FileMode{},
		renameErrs: map[[2]string]error{},
	}
}

func (f *fakeFS) MkdirAll(path string, _ fs.FileMode) error {
	f.dirs[path] = true
	return nil
}

func (f *fakeFS) WriteFile(path string, data []byte, mode fs.FileMode) error {
	copied := make([]byte, len(data))
	copy(copied, data)
	f.files[path] = copied
	f.modes[path] = mode
	return nil
}

func (f *fakeFS) Chmod(path string, mode fs.FileMode) error {
	if _, ok := f.files[path]; !ok {
		return os.ErrNotExist
	}
	f.modes[path] = mode
	return nil
}

func (f *fakeFS) failRename(oldPath string, newPath string, err error) {
	if err == nil {
		err = errors.New("rename failed")
	}
	f.renameErrs[[2]string{oldPath, newPath}] = err
}

func (f *fakeFS) Rename(oldPath string, newPath string) error {
	f.renameAttempts = append(f.renameAttempts, [2]string{oldPath, newPath})
	if err, ok := f.renameErrs[[2]string{oldPath, newPath}]; ok {
		return err
	}
	data, ok := f.files[oldPath]
	if !ok {
		return errors.New("missing source")
	}
	f.files[newPath] = data
	f.modes[newPath] = f.modes[oldPath]
	delete(f.files, oldPath)
	delete(f.modes, oldPath)
	f.renames = append(f.renames, [2]string{oldPath, newPath})
	return nil
}

func (f *fakeFS) Remove(path string) error {
	delete(f.files, path)
	delete(f.modes, path)
	return nil
}
