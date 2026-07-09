package home

import (
	"errors"
	"io/fs"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestResolveHomeDirHonorsLoopcoderHomeOverride(t *testing.T) {
	override := filepath.Join("custom", "loopcoder-home", ".")

	got, err := ResolveHomeDir(Deps{
		Getenv: func(key string) string {
			if key != EnvHome {
				t.Fatalf("Getenv key = %q, want %s", key, EnvHome)
			}
			return override
		},
		UserHomeDir: func() (string, error) {
			t.Fatal("UserHomeDir should not be called when override is set")
			return "", nil
		},
	})
	if err != nil {
		t.Fatalf("ResolveHomeDir returned error: %v", err)
	}

	if want := filepath.Clean(override); got != want {
		t.Fatalf("ResolveHomeDir() = %q, want %q", got, want)
	}
}

func TestResolveHomeDirDefaultsToDotLoopcoderUnderUserHome(t *testing.T) {
	userHome := filepath.Join("Users", "owner")

	got, err := ResolveHomeDir(Deps{
		Getenv: func(string) string {
			return ""
		},
		UserHomeDir: func() (string, error) {
			return userHome, nil
		},
	})
	if err != nil {
		t.Fatalf("ResolveHomeDir returned error: %v", err)
	}

	if want := filepath.Join(userHome, ".loopcoder"); got != want {
		t.Fatalf("ResolveHomeDir() = %q, want %q", got, want)
	}
}

func TestResolveHomeDirReturnsUserHomeError(t *testing.T) {
	wantErr := errors.New("no home")

	_, err := ResolveHomeDir(Deps{
		Getenv: func(string) string {
			return ""
		},
		UserHomeDir: func() (string, error) {
			return "", wantErr
		},
	})
	if err == nil {
		t.Fatal("ResolveHomeDir returned nil error, want user home error")
	}
	if !strings.Contains(err.Error(), "resolve user home") || !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want wrapping %v", err, wantErr)
	}
}

func TestLayoutPaths(t *testing.T) {
	root := filepath.Join("home", ".loopcoder")
	layout := New(root)

	if got, want := layout.BinDir(), filepath.Join(root, "bin"); got != want {
		t.Fatalf("BinDir() = %q, want %q", got, want)
	}
	if got, want := layout.DataDir(), filepath.Join(root, "data"); got != want {
		t.Fatalf("DataDir() = %q, want %q", got, want)
	}
	if got, want := layout.DatabasePath(), filepath.Join(root, "data", "loopcoder.db"); got != want {
		t.Fatalf("DatabasePath() = %q, want %q", got, want)
	}
	if got, want := layout.VersionsDir(), filepath.Join(root, "versions"); got != want {
		t.Fatalf("VersionsDir() = %q, want %q", got, want)
	}
	if got, want := layout.SkillsDir(), filepath.Join(root, "skills"); got != want {
		t.Fatalf("SkillsDir() = %q, want %q", got, want)
	}
	if got, want := layout.StablePlaybookPath(), filepath.Join(root, "skills", "SKILL.md"); got != want {
		t.Fatalf("StablePlaybookPath() = %q, want %q", got, want)
	}
	if got, want := layout.StableBinaryPath(), filepath.Join(root, "bin", wantBinaryFileName()); got != want {
		t.Fatalf("StableBinaryPath() = %q, want %q", got, want)
	}

	versionDir, err := layout.VersionDir(" v0.3.1 ")
	if err != nil {
		t.Fatalf("VersionDir returned error: %v", err)
	}
	if want := filepath.Join(root, "versions", "v0.3.1"); versionDir != want {
		t.Fatalf("VersionDir() = %q, want %q", versionDir, want)
	}

	versionBinary, err := layout.VersionBinaryPath("0.3.1")
	if err != nil {
		t.Fatalf("VersionBinaryPath returned error: %v", err)
	}
	if want := filepath.Join(root, "versions", "0.3.1", wantBinaryFileName()); versionBinary != want {
		t.Fatalf("VersionBinaryPath() = %q, want %q", versionBinary, want)
	}
}

func TestVersionPathsRejectInvalidVersionSegments(t *testing.T) {
	layout := New("home")
	tests := []string{"", " ", ".", "..", "v0/3/1", `v0\3\1`}

	for _, tt := range tests {
		t.Run(tt, func(t *testing.T) {
			if _, err := layout.VersionDir(tt); err == nil {
				t.Fatal("VersionDir returned nil error, want invalid version")
			}
			if _, err := layout.VersionBinaryPath(tt); err == nil {
				t.Fatal("VersionBinaryPath returned nil error, want invalid version")
			}
		})
	}
}

func TestInstalledVersionsListsCleanDirectoriesSorted(t *testing.T) {
	layout := New(filepath.Join("home", ".loopcoder"))
	var readPath string

	got, err := layout.InstalledVersions(Deps{
		ReadDir: func(path string) ([]fs.DirEntry, error) {
			readPath = path
			return []fs.DirEntry{
				fakeDirEntry{name: "0.3.1", dir: true},
				fakeDirEntry{name: "README.txt", dir: false},
				fakeDirEntry{name: "0.3.0", dir: true},
				fakeDirEntry{name: "nested/version", dir: true},
				fakeDirEntry{name: "0.3.0-rc.1", dir: true},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("InstalledVersions returned error: %v", err)
	}

	if readPath != layout.VersionsDir() {
		t.Fatalf("ReadDir path = %q, want %q", readPath, layout.VersionsDir())
	}
	want := []string{"0.3.0-rc.1", "0.3.0", "0.3.1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("InstalledVersions() = %v, want %v", got, want)
	}
}

func TestInstalledVersionsOrdersSemverAware(t *testing.T) {
	layout := New(filepath.Join("home", ".loopcoder"))
	tests := []struct {
		name    string
		entries []fs.DirEntry
		want    []string
	}{
		{
			name: "multi-digit patch",
			entries: []fs.DirEntry{
				fakeDirEntry{name: "0.3.10", dir: true},
				fakeDirEntry{name: "0.3.2", dir: true},
				fakeDirEntry{name: "0.3.9", dir: true},
			},
			want: []string{"0.3.2", "0.3.9", "0.3.10"},
		},
		{
			name: "multi-digit minor",
			entries: []fs.DirEntry{
				fakeDirEntry{name: "0.10.0", dir: true},
				fakeDirEntry{name: "0.9.12", dir: true},
				fakeDirEntry{name: "0.3.0", dir: true},
				fakeDirEntry{name: "0.9.5", dir: true},
			},
			want: []string{"0.3.0", "0.9.5", "0.9.12", "0.10.0"},
		},
		{
			name: "leading v prefix",
			entries: []fs.DirEntry{
				fakeDirEntry{name: "v0.3.10", dir: true},
				fakeDirEntry{name: "0.3.9", dir: true},
				fakeDirEntry{name: "v0.3.2", dir: true},
			},
			want: []string{"v0.3.2", "0.3.9", "v0.3.10"},
		},
		{
			name: "malformed entries sort before semver",
			entries: []fs.DirEntry{
				fakeDirEntry{name: "0.3.10", dir: true},
				fakeDirEntry{name: "not-semver", dir: true},
				fakeDirEntry{name: "0.3.2", dir: true},
				fakeDirEntry{name: "0.3.0-rc.1", dir: true},
			},
			want: []string{"0.3.0-rc.1", "not-semver", "0.3.2", "0.3.10"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := layout.InstalledVersions(Deps{
				ReadDir: func(string) ([]fs.DirEntry, error) {
					return tt.entries, nil
				},
			})
			if err != nil {
				t.Fatalf("InstalledVersions returned error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("InstalledVersions() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestInstalledVersionsReturnsEmptyWhenVersionsDirIsMissing(t *testing.T) {
	layout := New("home")

	got, err := layout.InstalledVersions(Deps{
		ReadDir: func(string) ([]fs.DirEntry, error) {
			return nil, fs.ErrNotExist
		},
	})
	if err != nil {
		t.Fatalf("InstalledVersions returned error: %v", err)
	}
	if got != nil {
		t.Fatalf("InstalledVersions() = %v, want nil", got)
	}
}

func TestInstalledVersionsWrapsReadDirErrors(t *testing.T) {
	layout := New("home")
	wantErr := errors.New("permission denied")

	_, err := layout.InstalledVersions(Deps{
		ReadDir: func(string) ([]fs.DirEntry, error) {
			return nil, wantErr
		},
	})
	if err == nil {
		t.Fatal("InstalledVersions returned nil error, want read error")
	}
	if !strings.Contains(err.Error(), "read installed versions") || !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want wrapping %v", err, wantErr)
	}
}

func wantBinaryFileName() string {
	if runtime.GOOS == "windows" {
		return "loopcoder.exe"
	}
	return "loopcoder"
}

type fakeDirEntry struct {
	name string
	dir  bool
}

func (f fakeDirEntry) Name() string {
	return f.name
}

func (f fakeDirEntry) IsDir() bool {
	return f.dir
}

func (f fakeDirEntry) Type() fs.FileMode {
	if f.dir {
		return fs.ModeDir
	}
	return 0
}

func (f fakeDirEntry) Info() (fs.FileInfo, error) {
	return nil, errors.New("not implemented")
}
