//go:build windows

package doctor

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestCheckStoragePermissionsWarnsOnWindowsUnsupportedACLHardening(t *testing.T) {
	homeDir := t.TempDir()
	dbPath := filepath.Join(homeDir, "data", "loopcoder.db")

	check := checkStoragePermissions(Deps{
		Getenv:      func(string) string { return homeDir },
		UserHomeDir: func() (string, error) { return "unused", nil },
		StoragePermissions: func(path string, fix bool) (storage.PermissionReport, error) {
			if path != dbPath {
				t.Fatalf("storage path = %q, want %q", path, dbPath)
			}
			return storage.CheckPermissions(path)
		},
	})

	if check.Status != StatusWarn {
		t.Fatalf("status = %s, want warn (%s)", check.Status, check.Message)
	}
	for _, want := range []string{"permissions=unsupported", "Windows", "ACL"} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("message = %q, want containing %q", check.Message, want)
		}
	}
}
