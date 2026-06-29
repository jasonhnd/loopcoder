package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/home"
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

func TestRunInstallsStagedAtomic(t *testing.T) {
	archiveBytes := []byte("archive bytes")
	binaryBytes := []byte("new loopcoder binary")
	assetName := platformAssetName(t, "0.3.3")
	sum := sha256.Sum256(archiveBytes)
	checksums := []byte(hex.EncodeToString(sum[:]) + "  " + assetName + "\n")
	releaseURL := "https://api.example.test/repos/owner/repo/releases/tags/v0.3.3"
	assetURL := "https://download.example.test/" + assetName
	sumsURL := "https://download.example.test/SHA256SUMS"
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
					{Name: "SHA256SUMS", BrowserDownloadURL: sumsURL},
				},
			}),
			assetURL: archiveBytes,
			sumsURL:  checksums,
		}),
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
		MkdirAll:      fsys.MkdirAll,
		WriteFile:     fsys.WriteFile,
		Chmod:         fsys.Chmod,
		Rename:        fsys.Rename,
		Remove:        fsys.Remove,
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
	stablePath := layout.StableBinaryPath()
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
