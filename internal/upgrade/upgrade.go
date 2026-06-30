package upgrade

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/home"
)

const (
	DefaultRepo       = "jasonhnd/loopcoder"
	DefaultAPIBaseURL = "https://api.github.com"
	DefaultBaseURL    = "https://github.com"

	EnvUpgradeRepo = "LOOPCODER_UPGRADE_REPO"
	EnvInstallRepo = "LOOPCODER_INSTALL_REPO"
	EnvAPIBaseURL  = "GITHUB_API_URL"
	EnvBaseURL     = "GITHUB_BASE_URL"
)

type Options struct {
	RequestedVersion string
	CurrentVersion   string
	RuntimeGOOS      string
	RuntimeGOARCH    string
}

type Result struct {
	CurrentPath       string
	CurrentVersion    string
	TargetVersion     string
	Platform          string
	AssetName         string
	AlreadyLatest     bool
	VersionBinaryPath string
	StableBinaryPath  string
	Deferred          bool
	PendingPath       string
	SkillRefresh      SkillRefreshResult
}

type SkillRefreshFileStatus string

const (
	SkillRefreshFileCreated     SkillRefreshFileStatus = "created"
	SkillRefreshFileUpdated     SkillRefreshFileStatus = "updated"
	SkillRefreshFileUnchanged   SkillRefreshFileStatus = "unchanged"
	SkillRefreshFileOverwritten SkillRefreshFileStatus = "overwritten"
)

type SkillRefreshFileResult struct {
	Path   string
	Status SkillRefreshFileStatus
}

type SkillRefreshResult struct {
	BinaryPath string
	Dir        string
	Files      []SkillRefreshFileResult
	Warning    string
}

type Deps struct {
	Getenv          func(string) string
	HomeLayout      func() (home.Layout, error)
	ExecutablePath  func() (string, error)
	HTTPGet         func(context.Context, string) ([]byte, error)
	ExtractBinary   func(archiveName string, data []byte, binaryName string) ([]byte, error)
	MkdirAll        func(string, fs.FileMode) error
	WriteFile       func(string, []byte, fs.FileMode) error
	Chmod           func(string, fs.FileMode) error
	Rename          func(string, string) error
	Remove          func(string) error
	ScheduleReplace func(source string, target string) error
	RunCommand      func(ctx context.Context, name string, args ...string) (CommandResult, error)
	RuntimeGOOS     string
	RuntimeGOARCH   string
}

type CommandResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

type releaseConfig struct {
	Repo       string
	APIBaseURL string
	BaseURL    string
}

type release struct {
	TagName    string         `json:"tag_name"`
	Draft      bool           `json:"draft"`
	Prerelease bool           `json:"prerelease"`
	Assets     []releaseAsset `json:"assets"`
}

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type installResult struct {
	VersionBinaryPath string
	StableBinaryPath  string
	Deferred          bool
	PendingPath       string
}

// DefaultDeps returns production dependencies for upgrade.
func DefaultDeps() Deps {
	client := &http.Client{Timeout: 60 * time.Second}
	return Deps{
		Getenv: os.Getenv,
		HomeLayout: func() (home.Layout, error) {
			return home.Resolve(home.DefaultDeps())
		},
		ExecutablePath: os.Executable,
		HTTPGet: func(ctx context.Context, rawURL string) ([]byte, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
			if err != nil {
				return nil, fmt.Errorf("create request: %w", err)
			}
			req.Header.Set("Accept", "application/vnd.github+json")
			req.Header.Set("User-Agent", "loopcoder-upgrade")

			resp, err := client.Do(req)
			if err != nil {
				return nil, fmt.Errorf("GET %s: %w", rawURL, err)
			}
			defer resp.Body.Close()

			data, readErr := io.ReadAll(resp.Body)
			if resp.StatusCode < 200 || resp.StatusCode > 299 {
				detail := strings.TrimSpace(string(data))
				if len(detail) > 240 {
					detail = detail[:240]
				}
				if detail == "" {
					detail = resp.Status
				}
				return nil, fmt.Errorf("GET %s: %s: %s", rawURL, resp.Status, detail)
			}
			if readErr != nil {
				return nil, fmt.Errorf("read response from %s: %w", rawURL, readErr)
			}
			return data, nil
		},
		ExtractBinary:   ExtractBinary,
		MkdirAll:        os.MkdirAll,
		WriteFile:       os.WriteFile,
		Chmod:           os.Chmod,
		Rename:          os.Rename,
		Remove:          os.Remove,
		ScheduleReplace: scheduleReplace,
		RunCommand:      execRunCommand,
		RuntimeGOOS:     runtime.GOOS,
		RuntimeGOARCH:   runtime.GOARCH,
	}
}

// Run resolves, verifies, extracts, and installs a release asset.
func Run(ctx context.Context, opts Options, deps Deps) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	deps = normalizeDeps(deps)

	currentPath, err := deps.ExecutablePath()
	if err != nil || strings.TrimSpace(currentPath) == "" {
		currentPath = "(unknown)"
	}
	currentVersion := strings.TrimSpace(opts.CurrentVersion)
	if currentVersion == "" {
		currentVersion = "unknown"
	}
	result := Result{
		CurrentPath:    currentPath,
		CurrentVersion: currentVersion,
	}

	goos := firstNonEmpty(opts.RuntimeGOOS, deps.RuntimeGOOS)
	goarch := firstNonEmpty(opts.RuntimeGOARCH, deps.RuntimeGOARCH)
	archiveKind, err := archiveKind(goos)
	if err != nil {
		return result, err
	}
	if err := validateArch(goarch); err != nil {
		return result, err
	}
	result.Platform = goos + "/" + goarch

	cfg := releaseConfigFromEnv(deps)
	rel, err := resolveRelease(ctx, opts.RequestedVersion, cfg, deps.HTTPGet)
	if err != nil {
		return result, err
	}
	if strings.TrimSpace(rel.TagName) == "" {
		return result, errors.New("release response did not include tag_name")
	}
	result.TargetVersion = rel.TagName

	if sameReleaseVersion(currentVersion, rel.TagName) {
		result.AlreadyLatest = true
		return result, nil
	}

	assetVersion := assetVersion(rel.TagName)
	if assetVersion == "" {
		return result, fmt.Errorf("version %q resolves to an empty asset version", rel.TagName)
	}
	assetName := fmt.Sprintf("loopcoder_%s_%s_%s.%s", assetVersion, goos, goarch, archiveKind)
	result.AssetName = assetName

	asset, ok := findAsset(rel, assetName)
	if !ok {
		return result, fmt.Errorf("missing release asset %s for %s", assetName, result.Platform)
	}
	sums, ok := findAsset(rel, "SHA256SUMS")
	if !ok {
		return result, errors.New("missing release asset SHA256SUMS")
	}

	archiveBytes, err := deps.HTTPGet(ctx, assetURL(asset, cfg, rel.TagName, assetName))
	if err != nil {
		return result, fmt.Errorf("download %s: %w", assetName, err)
	}
	checksums, err := deps.HTTPGet(ctx, assetURL(sums, cfg, rel.TagName, "SHA256SUMS"))
	if err != nil {
		return result, fmt.Errorf("download SHA256SUMS: %w", err)
	}
	if err := VerifyChecksum(checksums, assetName, archiveBytes); err != nil {
		return result, err
	}

	binaryBytes, err := deps.ExtractBinary(assetName, archiveBytes, binaryName(goos))
	if err != nil {
		return result, err
	}
	layout, err := deps.HomeLayout()
	if err != nil {
		return result, fmt.Errorf("resolve loopcoder home: %w", err)
	}
	installed, err := installBinary(deps, layout, rel.TagName, binaryBytes)
	if err != nil {
		return result, err
	}

	result.VersionBinaryPath = installed.VersionBinaryPath
	result.StableBinaryPath = installed.StableBinaryPath
	result.Deferred = installed.Deferred
	result.PendingPath = installed.PendingPath
	result.SkillRefresh = refreshSkill(ctx, deps, skillRefreshBinaryPath(installed))
	return result, nil
}

func normalizeDeps(deps Deps) Deps {
	defaults := DefaultDeps()
	if deps.Getenv == nil {
		deps.Getenv = defaults.Getenv
	}
	if deps.HomeLayout == nil {
		deps.HomeLayout = defaults.HomeLayout
	}
	if deps.ExecutablePath == nil {
		deps.ExecutablePath = defaults.ExecutablePath
	}
	if deps.HTTPGet == nil {
		deps.HTTPGet = defaults.HTTPGet
	}
	if deps.ExtractBinary == nil {
		deps.ExtractBinary = defaults.ExtractBinary
	}
	if deps.MkdirAll == nil {
		deps.MkdirAll = defaults.MkdirAll
	}
	if deps.WriteFile == nil {
		deps.WriteFile = defaults.WriteFile
	}
	if deps.Chmod == nil {
		deps.Chmod = defaults.Chmod
	}
	if deps.Rename == nil {
		deps.Rename = defaults.Rename
	}
	if deps.Remove == nil {
		deps.Remove = defaults.Remove
	}
	if deps.ScheduleReplace == nil {
		deps.ScheduleReplace = defaults.ScheduleReplace
	}
	if deps.RunCommand == nil {
		deps.RunCommand = defaults.RunCommand
	}
	if strings.TrimSpace(deps.RuntimeGOOS) == "" {
		deps.RuntimeGOOS = defaults.RuntimeGOOS
	}
	if strings.TrimSpace(deps.RuntimeGOARCH) == "" {
		deps.RuntimeGOARCH = defaults.RuntimeGOARCH
	}
	return deps
}

func execRunCommand(ctx context.Context, name string, args ...string) (CommandResult, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	result := CommandResult{}
	err := cmd.Run()
	result.Stdout = stdout.String()
	result.Stderr = stderr.String()
	if err == nil {
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	return result, err
}

func releaseConfigFromEnv(deps Deps) releaseConfig {
	repo := strings.TrimSpace(deps.Getenv(EnvUpgradeRepo))
	if repo == "" {
		repo = strings.TrimSpace(deps.Getenv(EnvInstallRepo))
	}
	if repo == "" {
		repo = DefaultRepo
	}
	apiBaseURL := strings.TrimRight(strings.TrimSpace(deps.Getenv(EnvAPIBaseURL)), "/")
	if apiBaseURL == "" {
		apiBaseURL = DefaultAPIBaseURL
	}
	baseURL := strings.TrimRight(strings.TrimSpace(deps.Getenv(EnvBaseURL)), "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return releaseConfig{
		Repo:       repo,
		APIBaseURL: apiBaseURL,
		BaseURL:    baseURL,
	}
}

func resolveRelease(ctx context.Context, requested string, cfg releaseConfig, get func(context.Context, string) ([]byte, error)) (release, error) {
	requested = strings.TrimSpace(requested)
	var endpoint string
	pinned := requested != "" && !strings.EqualFold(requested, "latest")
	if pinned {
		endpoint = fmt.Sprintf("%s/repos/%s/releases/tags/%s", cfg.APIBaseURL, cfg.Repo, url.PathEscape(normalizeTag(requested)))
	} else {
		endpoint = fmt.Sprintf("%s/repos/%s/releases/latest", cfg.APIBaseURL, cfg.Repo)
	}

	data, err := get(ctx, endpoint)
	if err != nil {
		return release{}, err
	}
	var rel release
	if err := json.Unmarshal(data, &rel); err != nil {
		return release{}, fmt.Errorf("parse release JSON: %w", err)
	}
	if !pinned && (rel.Draft || rel.Prerelease) {
		return release{}, fmt.Errorf("latest release %s is not stable", rel.TagName)
	}
	return rel, nil
}

func normalizeTag(version string) string {
	version = strings.TrimSpace(version)
	if strings.HasPrefix(version, "v") || strings.HasPrefix(version, "V") {
		return version
	}
	return "v" + version
}

func sameReleaseVersion(a string, b string) bool {
	return releaseVersionKey(a) == releaseVersionKey(b)
}

func releaseVersionKey(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return ""
	}
	if strings.HasPrefix(version, "v") || strings.HasPrefix(version, "V") {
		return version[1:]
	}
	return version
}

func assetVersion(tag string) string {
	return strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(tag), "v"), "V")
}

func archiveKind(goos string) (string, error) {
	switch goos {
	case "windows":
		return "zip", nil
	case "darwin", "linux":
		return "tar.gz", nil
	default:
		return "", fmt.Errorf("unsupported OS %q", goos)
	}
}

func validateArch(goarch string) error {
	switch goarch {
	case "amd64", "arm64":
		return nil
	default:
		return fmt.Errorf("unsupported architecture %q", goarch)
	}
}

func binaryName(goos string) string {
	if goos == "windows" {
		return "loopcoder.exe"
	}
	return "loopcoder"
}

func findAsset(rel release, name string) (releaseAsset, bool) {
	for _, asset := range rel.Assets {
		if asset.Name == name {
			return asset, true
		}
	}
	return releaseAsset{}, false
}

func assetURL(asset releaseAsset, cfg releaseConfig, tag string, name string) string {
	if strings.TrimSpace(asset.BrowserDownloadURL) != "" {
		return asset.BrowserDownloadURL
	}
	return fmt.Sprintf("%s/%s/releases/download/%s/%s", cfg.BaseURL, cfg.Repo, url.PathEscape(tag), url.PathEscape(name))
}

// VerifyChecksum verifies archive against its SHA256SUMS entry.
func VerifyChecksum(checksums []byte, archiveName string, archive []byte) error {
	expected, err := expectedChecksum(checksums, archiveName)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(archive)
	actual := hex.EncodeToString(sum[:])
	if !strings.EqualFold(expected, actual) {
		return fmt.Errorf("checksum mismatch for %s: expected %s, got %s", archiveName, strings.ToLower(expected), actual)
	}
	return nil
}

func expectedChecksum(checksums []byte, archiveName string) (string, error) {
	scanner := bufio.NewScanner(bytes.NewReader(checksums))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		hash := strings.ToLower(fields[0])
		fileName := strings.TrimPrefix(fields[1], "*")
		fileName = strings.TrimPrefix(fileName, "./")
		if fileName != archiveName {
			continue
		}
		if len(hash) != 64 || !isHex(hash) {
			return "", fmt.Errorf("invalid SHA256SUMS hash for %s", archiveName)
		}
		return hash, nil
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read SHA256SUMS: %w", err)
	}
	return "", fmt.Errorf("SHA256SUMS does not contain %s", archiveName)
}

func isHex(value string) bool {
	for _, r := range value {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
			continue
		}
		return false
	}
	return true
}

// ExtractBinary extracts binaryName from a release archive.
func ExtractBinary(archiveName string, data []byte, binaryName string) ([]byte, error) {
	if strings.HasSuffix(archiveName, ".zip") {
		return extractZipBinary(archiveName, data, binaryName)
	}
	if strings.HasSuffix(archiveName, ".tar.gz") {
		return extractTarGzBinary(archiveName, data, binaryName)
	}
	return nil, fmt.Errorf("unsupported archive format %s", archiveName)
}

func extractZipBinary(archiveName string, data []byte, binaryName string) ([]byte, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", archiveName, err)
	}
	for _, file := range reader.File {
		if file.FileInfo().IsDir() || filepath.Base(filepath.Clean(file.Name)) != binaryName {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("open %s in %s: %w", binaryName, archiveName, err)
		}
		defer rc.Close()
		return io.ReadAll(rc)
	}
	return nil, fmt.Errorf("%s did not contain %s", archiveName, binaryName)
}

func extractTarGzBinary(archiveName string, data []byte, binaryName string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", archiveName, err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", archiveName, err)
		}
		if header.Typeflag != tar.TypeReg || filepath.Base(filepath.Clean(header.Name)) != binaryName {
			continue
		}
		return io.ReadAll(tr)
	}
	return nil, fmt.Errorf("%s did not contain %s", archiveName, binaryName)
}

func installBinary(deps Deps, layout home.Layout, version string, binary []byte) (installResult, error) {
	if len(binary) == 0 {
		return installResult{}, errors.New("extracted loopcoder binary is empty")
	}

	versionPath, err := layout.VersionBinaryPath(version)
	if err != nil {
		return installResult{}, err
	}
	stablePath := layout.StableBinaryPath()
	result := installResult{
		VersionBinaryPath: versionPath,
		StableBinaryPath:  stablePath,
	}

	if err := deps.MkdirAll(filepath.Dir(versionPath), 0o755); err != nil {
		return result, fmt.Errorf("create version directory: %w", err)
	}
	if err := deps.WriteFile(versionPath, binary, 0o755); err != nil {
		return result, fmt.Errorf("write versioned binary: %w", err)
	}
	if err := deps.Chmod(versionPath, 0o755); err != nil {
		return result, fmt.Errorf("chmod versioned binary: %w", err)
	}

	if err := deps.MkdirAll(filepath.Dir(stablePath), 0o755); err != nil {
		return result, fmt.Errorf("create bin directory: %w", err)
	}
	tmpPath := filepath.Join(filepath.Dir(stablePath), "."+filepath.Base(stablePath)+"."+safePathSegment(version)+".tmp")
	_ = deps.Remove(tmpPath)
	if err := deps.WriteFile(tmpPath, binary, 0o755); err != nil {
		return result, fmt.Errorf("write stable binary candidate: %w", err)
	}
	if err := deps.Chmod(tmpPath, 0o755); err != nil {
		return result, fmt.Errorf("chmod stable binary candidate: %w", err)
	}

	deferred, pendingPath, err := replaceStableBinary(deps, tmpPath, stablePath)
	if err != nil {
		return result, err
	}
	result.Deferred = deferred
	result.PendingPath = pendingPath
	return result, nil
}

func skillRefreshBinaryPath(installed installResult) string {
	if strings.TrimSpace(installed.StableBinaryPath) != "" && !installed.Deferred {
		return installed.StableBinaryPath
	}
	if strings.TrimSpace(installed.VersionBinaryPath) != "" {
		return installed.VersionBinaryPath
	}
	if strings.TrimSpace(installed.PendingPath) != "" {
		return installed.PendingPath
	}
	return installed.StableBinaryPath
}

func refreshSkill(ctx context.Context, deps Deps, binaryPath string) SkillRefreshResult {
	result := SkillRefreshResult{BinaryPath: binaryPath}
	if strings.TrimSpace(binaryPath) == "" {
		result.Warning = "selected binary path is empty"
		return result
	}

	commandResult, err := deps.RunCommand(ctx, binaryPath, "skill", "install")
	if err != nil {
		result.Warning = fmt.Sprintf("run %s skill install: %v", binaryPath, err)
		return result
	}
	if commandResult.ExitCode != 0 {
		detail := strings.TrimSpace(commandResult.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(commandResult.Stdout)
		}
		if detail == "" {
			detail = fmt.Sprintf("exit code %d", commandResult.ExitCode)
		}
		result.Warning = fmt.Sprintf("%s skill install failed: %s", binaryPath, detail)
		return result
	}

	dir, files, err := parseSkillInstallOutput(commandResult.Stdout)
	if err != nil {
		result.Warning = fmt.Sprintf("parse %s skill install output: %v", binaryPath, err)
		return result
	}
	result.Dir = dir
	result.Files = files
	return result
}

func parseSkillInstallOutput(output string) (string, []SkillRefreshFileResult, error) {
	var dir string
	var files []SkillRefreshFileResult
	for _, rawLine := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || line == "loopcoder skill install complete" {
			continue
		}
		if strings.HasPrefix(line, "directory ") {
			dir = strings.TrimSpace(strings.TrimPrefix(line, "directory "))
			continue
		}
		status, path, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		status = strings.TrimSpace(status)
		path = strings.TrimSpace(path)
		switch SkillRefreshFileStatus(status) {
		case SkillRefreshFileCreated, SkillRefreshFileUpdated, SkillRefreshFileUnchanged, SkillRefreshFileOverwritten:
			files = append(files, SkillRefreshFileResult{Path: path, Status: SkillRefreshFileStatus(status)})
		}
	}
	if dir == "" {
		return "", nil, errors.New("missing directory line")
	}
	if len(files) == 0 {
		return "", nil, errors.New("missing managed file status lines")
	}
	return dir, files, nil
}

func replaceStableBinary(deps Deps, tmpPath string, stablePath string) (bool, string, error) {
	if err := deps.Rename(tmpPath, stablePath); err == nil {
		return false, "", nil
	} else if deps.RuntimeGOOS != "windows" {
		return false, "", fmt.Errorf("replace stable binary: %w", err)
	} else {
		return replaceStableBinaryWindows(deps, tmpPath, stablePath, err)
	}
}

func replaceStableBinaryWindows(deps Deps, tmpPath string, stablePath string, originalErr error) (bool, string, error) {
	backupPath := stablePath + ".old"
	_ = deps.Remove(backupPath)
	if err := deps.Rename(stablePath, backupPath); err == nil {
		if err := deps.Rename(tmpPath, stablePath); err == nil {
			_ = deps.Remove(backupPath)
			return false, "", nil
		} else {
			_ = deps.Rename(backupPath, stablePath)
			return false, "", fmt.Errorf("replace stable binary after backup: %w", err)
		}
	}

	pendingPath := stablePath + ".new"
	_ = deps.Remove(pendingPath)
	if err := deps.Rename(tmpPath, pendingPath); err != nil {
		return false, "", fmt.Errorf("replace stable binary: %w; also failed to stage pending Windows binary: %v", originalErr, err)
	}
	if err := deps.ScheduleReplace(pendingPath, stablePath); err != nil {
		return false, "", fmt.Errorf("replace stable binary: %w; pending binary staged at %s but deferred replacement failed: %v", originalErr, pendingPath, err)
	}
	return true, pendingPath, nil
}

func safePathSegment(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
