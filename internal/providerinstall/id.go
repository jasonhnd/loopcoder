// Package providerinstall exports the production ProviderInstallationID algorithm
// so runners can affirm the same pinst_* as inventory discovery.
package providerinstall

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ComputeInstallationID returns the same "pinst_"+base32 identity used by
// providerinventory for the exact executable path that will be launched.
func ComputeInstallationID(adapterID, execPath string) (string, error) {
	adapterID = strings.TrimSpace(adapterID) // preserve case — inventory does not lower-case
	execPath = strings.TrimSpace(execPath)
	if adapterID == "" || execPath == "" {
		return "", fmt.Errorf("providerinstall: adapter and path required")
	}
	// Must match providerinventory inspectCandidate: GOOS + "-" + GOARCH
	platform := runtime.GOOS + "-" + runtime.GOARCH
	canonical := filepath.Clean(execPath)
	if abs, err := filepath.Abs(canonical); err == nil {
		canonical = abs
	}
	basename := filepath.Base(canonical)
	pathHash := "sha256:" + hashHex(canonical)
	// Exact match of providerinventory.installationID:
	// pinst_ + hashBase32(adapterID, Basename, PathHash, hashHex(path), platform)[:32]
	return "pinst_" + hashBase32(adapterID, basename, pathHash, hashHex(canonical), platform)[:32], nil
}

// ComputeFromCommand resolves command via PATH then ComputeInstallationID.
// Returns (installID, absoluteExecutablePath, error).
func ComputeFromCommand(adapterID, command string) (installID, absPath string, err error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", "", fmt.Errorf("providerinstall: command empty")
	}
	p, err := exec.LookPath(command)
	if err != nil {
		return "", "", err
	}
	if abs, aerr := filepath.Abs(p); aerr == nil {
		p = abs
	}
	id, err := ComputeInstallationID(adapterID, p)
	return id, p, err
}

// RedactedExecutableEvidence is a non-secret path fingerprint for reports.
func RedactedExecutableEvidence(absPath string) string {
	absPath = strings.TrimSpace(absPath)
	if absPath == "" {
		return ""
	}
	base := filepath.Base(absPath)
	return base + "#sha256:" + hashHex(absPath)[:16]
}

func hashHex(parts ...string) string {
	sum := sha256.New()
	for _, part := range parts {
		sum.Write([]byte(part))
		sum.Write([]byte{0})
	}
	return hex.EncodeToString(sum.Sum(nil))
}

func hashBase32(parts ...string) string {
	sum := sha256.New()
	for _, part := range parts {
		sum.Write([]byte(part))
		sum.Write([]byte{0})
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum.Sum(nil)))
}

// EnsureLookPath is used only for tests that need PATH isolation.
var _ = os.Getenv
