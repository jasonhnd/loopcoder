package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/attestation"
	"github.com/jasonhnd/loopcoder/internal/audit"
)

func TestAuditLLMRelayWriteFailureReturnsNeedsHuman(t *testing.T) {
	repo := t.TempDir()
	writeCLIAuditConfig(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".loopcoder"), []byte("not a directory\n"), 0o600); err != nil {
		t.Fatalf("write .loopcoder file: %v", err)
	}

	record := validCLIAuditAttestation()
	var stdout, stderr bytes.Buffer
	exitCode := RunWithDeps([]string{
		"audit",
		"--repo", repo,
		"--layer", "llm",
		"--format", "json",
		"--no-pretty",
	}, &stdout, &stderr, Deps{
		Now: func() time.Time { return time.Date(2026, 7, 5, 1, 2, 3, 0, time.UTC) },
		Audit: func(_ context.Context, opts audit.Options) (audit.Result, error) {
			if opts.Provider != "codex" {
				t.Fatalf("Provider = %q, want codex from verifier config", opts.Provider)
			}
			result := audit.NewResult(repo, []string{audit.LayerLLM}, audit.SeverityMedium)
			result.Attestation = &record
			return audit.Finalize(result), nil
		},
	})
	if exitCode != 2 {
		t.Fatalf("exit code = %d, want 2; stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"verdict": "needs-human"`) || !strings.Contains(stdout.String(), "write audit review relay record") {
		t.Fatalf("stdout missing relay needs-human verdict:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), `"attestation"`) {
		t.Fatalf("audit JSON serialized local-only attestation:\n%s", stdout.String())
	}
}

func TestAuditLLMRelayWritesLocalPrettyBlock(t *testing.T) {
	repo := t.TempDir()
	writeCLIAuditConfig(t, repo)
	record := validCLIAuditAttestation()

	var stdout, stderr bytes.Buffer
	exitCode := RunWithDeps([]string{
		"audit",
		"--repo", repo,
		"--layer", "llm",
		"--format", "json",
		"--pretty",
	}, &stdout, &stderr, Deps{
		Now: func() time.Time { return time.Date(2026, 7, 5, 1, 2, 3, 0, time.UTC) },
		Audit: func(context.Context, audit.Options) (audit.Result, error) {
			result := audit.NewResult(repo, []string{audit.LayerLLM}, audit.SeverityMedium)
			result.Attestation = &record
			return audit.Finalize(result), nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "attestation verified") {
		t.Fatalf("stderr missing pretty attestation:\n%s", stderr.String())
	}
	if strings.Contains(stdout.String(), `"attestation"`) {
		t.Fatalf("audit JSON serialized local-only attestation:\n%s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(repo, ".loopcoder", "relay")); err != nil {
		t.Fatalf("relay state was not written locally: %v", err)
	}
}

func writeCLIAuditConfig(t *testing.T, repo string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte(`
adapters:
  verifier: codex
verifier:
  model: test-model
  reasoning_effort: high
`), 0o600); err != nil {
		t.Fatalf("write .delivery.yml: %v", err)
	}
}

func validCLIAuditAttestation() attestation.AttestationRecord {
	started := time.Date(2026, 7, 5, 1, 2, 3, 0, time.UTC)
	ended := started.Add(time.Second)
	total := int64(9)
	return attestation.AttestationRecord{
		Role:        attestation.RoleVerifier,
		Provider:    "codex",
		Model:       "test-model",
		ModelSource: attestation.ModelSourceParsed,
		Effort:      "high",
		Permission:  attestation.PermissionReadOnly,
		Action:      "audit LLM security review",
		ExitCode:    0,
		StartedAt:   started.Format(time.RFC3339Nano),
		EndedAt:     ended.Format(time.RFC3339Nano),
		DurationMS:  1000,
		Usage: attestation.Usage{
			TotalTokens: &total,
		},
		Verified: true,
	}
}
