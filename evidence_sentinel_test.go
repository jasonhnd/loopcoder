package loopcoder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEvidenceSentinel is the deterministic local-focused gate used by
// scripts/pre-push-sentinel.sh. It must stay fast and free of repository-wide
// go test ./... or race work.
func TestEvidenceSentinel(t *testing.T) {
	root := repositoryPolicyRoot(t)

	docPath := filepath.Join(root, "docs", "reference", "evidence-tiers.md")
	doc, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read evidence tiers doc: %v", err)
	}
	body := string(doc)
	for _, want := range []string{
		"local-focused",
		"pull-request",
		"merge-sha",
		"release-artifact",
		"consumer-canary",
		"Greptile",
		"tested_commit_sha",
		"archive_digest_sha256",
		"scripts/pre-push-sentinel.sh",
		"`verify`",
		"`test`",
		"`race`",
		"`security`",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("evidence-tiers.md missing %q", want)
		}
	}

	scriptPath := filepath.Join(root, "scripts", "pre-push-sentinel.sh")
	scriptBytes, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read pre-push sentinel: %v", err)
	}
	info, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatalf("stat pre-push sentinel: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatal("scripts/pre-push-sentinel.sh must be executable")
	}
	script := string(scriptBytes)
	for _, want := range []string{
		"evidence_tier=local-focused",
		"tested_commit_sha=",
		"gofmt -l",
		"git diff --check",
		"TestEvidenceSentinel",
		"BUDGET_SECONDS",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("pre-push sentinel missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"go test ./...",
		"go test -race ./...",
		"ci-full-race.sh",
		"gh pr checks",
		"govulncheck",
		"staticcheck",
	} {
		if strings.Contains(script, forbidden) {
			t.Errorf("pre-push sentinel must not contain heavy command %q", forbidden)
		}
	}

	hookPath := filepath.Join(root, "hooks", "pre-push")
	hookBytes, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read hooks/pre-push: %v", err)
	}
	hookInfo, err := os.Stat(hookPath)
	if err != nil {
		t.Fatalf("stat hooks/pre-push: %v", err)
	}
	if hookInfo.Mode()&0o111 == 0 {
		t.Fatal("hooks/pre-push must be executable")
	}
	if !strings.Contains(string(hookBytes), "scripts/pre-push-sentinel.sh") {
		t.Fatal("hooks/pre-push must invoke scripts/pre-push-sentinel.sh")
	}

	// CI must record the tested commit SHA on every required PR job.
	ciPath := filepath.Join(root, ".github", "workflows", "ci.yml")
	ciBytes, err := os.ReadFile(ciPath)
	if err != nil {
		t.Fatalf("read ci workflow: %v", err)
	}
	ci := string(ciBytes)
	for _, want := range []string{
		"tested_commit_sha=",
		"GITHUB_SHA",
		"evidence_tier=pull-request",
	} {
		if !strings.Contains(ci, want) {
			t.Errorf("ci.yml missing SHA evidence marker %q", want)
		}
	}

	// Release must record archive digest for artifact stages.
	releasePath := filepath.Join(root, ".github", "workflows", "release.yml")
	releaseBytes, err := os.ReadFile(releasePath)
	if err != nil {
		t.Fatalf("read release workflow: %v", err)
	}
	release := string(releaseBytes)
	for _, want := range []string{
		"tested_commit_sha=",
		"archive_digest_sha256=",
		"evidence_tier=release-artifact",
	} {
		if !strings.Contains(release, want) {
			t.Errorf("release.yml missing archive evidence marker %q", want)
		}
	}
}
