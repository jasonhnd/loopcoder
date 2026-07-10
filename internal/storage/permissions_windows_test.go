//go:build windows

package storage

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsCheckPermissionsReportsUnsupportedACLHardening(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "loopcoder.db")

	report, err := CheckPermissions(path)
	if err != nil {
		t.Fatalf("CheckPermissions returned error: %v", err)
	}
	if report.Supported || report.Secure {
		t.Fatalf("report = %#v, want unsupported insecure warning", report)
	}
	for _, want := range []string{"Windows", "ACL", "not implemented"} {
		if !strings.Contains(report.Message, want) {
			t.Fatalf("message = %q, want containing %q", report.Message, want)
		}
	}
}

func TestWindowsRepairPermissionsDoesNotClaimACLRepair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "loopcoder.db")

	report, err := RepairPermissions(path)
	if err != nil {
		t.Fatalf("RepairPermissions returned error: %v", err)
	}
	if report.Supported || report.Secure || report.Repaired {
		t.Fatalf("report = %#v, want unsupported unrepaired warning", report)
	}
	for _, want := range []string{"cannot repair Windows ACLs", "not implemented"} {
		if !strings.Contains(report.Message, want) {
			t.Fatalf("message = %q, want containing %q", report.Message, want)
		}
	}
}
