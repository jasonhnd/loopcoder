//go:build windows

package audit

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNativePermissionFindingSkipsSynthesizedWindowsModeBits(t *testing.T) {
	repo := t.TempDir()
	path := filepath.Join(repo, "prompt.txt")
	if err := os.WriteFile(path, []byte("sensitive prompt\n"), 0o644); err != nil {
		t.Fatalf("write prompt: %v", err)
	}

	if finding, ok := nativePermissionFinding(repo, "prompt.txt"); ok {
		t.Fatalf("nativePermissionFinding flagged synthesized Windows mode bits: %#v", finding)
	}
}

func TestNativeScansWindowsKeepsSensitiveWriteScanning(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "prompt.txt"), []byte("sensitive prompt\n"), 0o644); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	source := `package main

import "os"

func writePrompt(data []byte) error {
	return os.WriteFile("prompt.txt", data, 0o644)
}
`
	if err := os.WriteFile(filepath.Join(repo, "writer.go"), []byte(source), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}

	findings, err := RunNativeScans(repo, NativeConfig{FilePermissions: true, Include: []string{"**/*"}})
	if err != nil {
		t.Fatalf("RunNativeScans returned error: %v", err)
	}
	if hasRule(findings, "native:file-permission") {
		t.Fatalf("RunNativeScans emitted Windows mode-bit file-permission finding: %#v", findings)
	}
	if !hasRule(findings, "native:sensitive-write") {
		t.Fatalf("RunNativeScans did not keep sensitive-write source scanning: %#v", findings)
	}
}
