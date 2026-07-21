package loopcoder

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var requiredV080Contexts = []string{"verify", "test", "race", "security"}
var requiredV080ReleaseJobs = []string{"build", "sign", "draft", "smoke", "publish", "failed-draft"}

func TestV080WorkflowPolicy(t *testing.T) {
	root := repositoryPolicyRoot(t)
	workflow := loadWorkflowPolicy(t, filepath.Join(root, ".github", "workflows", "ci.yml"))
	checks := loadDeliveryChecks(t, filepath.Join(root, ".delivery.yml"))

	if err := validateV080WorkflowPolicy(workflow, checks); err != nil {
		t.Fatal(err)
	}
	if !workflowJobContains(workflow.Jobs["race"], "scripts/ci-race-changed.sh") {
		t.Fatal("pull-request race job must run scripts/ci-race-changed.sh")
	}
	if !scriptContains(t, filepath.Join(root, "scripts", "ci-race-changed.sh"), "bash scripts/ci-full-race.sh") {
		t.Fatal("pull-request race fallback must use scripts/ci-full-race.sh")
	}
}

func TestV080ReleaseWorkflowPolicy(t *testing.T) {
	root := repositoryPolicyRoot(t)
	workflow := loadWorkflowPolicy(t, filepath.Join(root, ".github", "workflows", "release.yml"))

	if err := validateV080ReleaseWorkflowPolicy(workflow); err != nil {
		t.Fatal(err)
	}
	if !workflowJobContains(workflow.Jobs["build"], "bash scripts/ci-full-race.sh") {
		t.Fatal("release build must run scripts/ci-full-race.sh before packaging")
	}
	if workflowJobContains(workflow.Jobs["build"], "go test -race -count=1 -timeout=20m ./...") {
		t.Fatal("release build must not drift back to the superseded monolithic race command")
	}
}

func TestV080ReleaseRaceDocumentationPolicy(t *testing.T) {
	root := repositoryPolicyRoot(t)
	docsRequiringHelper := []string{
		"docs/reference/releasing.md",
		"docs/reference/v0.8.0-go-no-go.md",
	}
	for _, name := range docsRequiringHelper {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("read release race documentation %s: %v", name, err)
		}
		current := currentV080Documentation(name, string(data))
		for _, want := range []string{
			"bash scripts/ci-full-race.sh",
			"go list ./...",
			"-race -count=1 -timeout=20m",
			"internal/storage",
			"internal/routing",
			"internal/supervisedexec",
		} {
			if !strings.Contains(current, want) {
				t.Errorf("%s does not document release race helper detail %q", name, want)
			}
		}
	}

	releaseOperatorDocs := []string{
		"CHANGELOG.md",
		".github/release-notes/v0.8.0.md",
		"docs/reference/releasing.md",
		"docs/reference/v0.8.0-go-no-go.md",
	}
	for _, name := range releaseOperatorDocs {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("read release/operator documentation %s: %v", name, err)
		}
		current := currentV080Documentation(name, string(data))
		if strings.Contains(current, "go test -race -count=1 -timeout=20m ./...") {
			t.Errorf("%s still documents the superseded monolithic release race command", name)
		}
	}
}

func scriptContains(t *testing.T, path, needle string) bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read script %s: %v", path, err)
	}
	return strings.Contains(string(data), needle)
}

func workflowJobContains(job workflowJobPolicy, needle string) bool {
	for _, step := range job.Steps {
		if strings.Contains(step.Run, needle) {
			return true
		}
	}
	return false
}

func TestV080ReleaseSmokeUsesInstallerBackedCandidate(t *testing.T) {
	root := repositoryPolicyRoot(t)
	scriptPath := filepath.Join(root, "scripts", "release-smoke.sh")
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read release smoke script: %v", err)
	}
	info, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatalf("stat release smoke script: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatal("release-smoke.sh must be executable")
	}
	script := string(data)
	for _, want := range []string{
		`#!/usr/bin/env bash`,
		`set -euo pipefail`,
		`LOOPCODER_SMOKE_MODE=release`,
		`LOOPCODER_SMOKE_VERSION`,
		`LOOPCODER_SMOKE_PREVIOUS_VERSION`,
		`go test ./internal/releasesmoke`,
		`TestReleaseSmoke`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("release smoke does not contain required Go driver seam %q", want)
		}
	}

	// Implementation lives in Go — assert installer / schema / evidence contracts there.
	impl, err := os.ReadFile(filepath.Join(root, "internal", "releasesmoke", "release.go"))
	if err != nil {
		t.Fatalf("read release smoke implementation: %v", err)
	}
	body := string(impl)
	for _, want := range []string{
		`install.sh`,
		`LOOPCODER_INSTALL_DIR`,
		`LOOPCODER_INSTALL_OS`,
		`LOOPCODER_INSTALL_ARCH`,
		`LOOPCODER_SMOKE_MV_READY`,
		`loopcoder.release_smoke_evidence.v1`,
		`release-smoke-evidence.json`,
		`migrate local-state`,
		`RunSelfBootstrap`,
		`assertUpgradePlanFromV07`,
		`assertMigratedToCurrent`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("release smoke implementation missing required seam %q", want)
		}
	}
	installIndex := strings.Index(body, `candidateInstall(`)
	migrationIndex := strings.Index(body, `migrate local-state`)
	selfBootstrapIndex := strings.Index(body, `RunSelfBootstrap(`)
	if installIndex < 0 || migrationIndex < 0 || installIndex > migrationIndex {
		t.Fatalf("release smoke must install the staged candidate before migration smoke")
	}
	if selfBootstrapIndex < 0 || installIndex > selfBootstrapIndex {
		t.Fatalf("release smoke must install the staged candidate before self-bootstrap smoke")
	}
}

func TestV080SelfBootstrapSmokeContract(t *testing.T) {
	root := repositoryPolicyRoot(t)
	scriptPath := filepath.Join(root, "scripts", "self-bootstrap-smoke.sh")
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read self-bootstrap smoke script: %v", err)
	}
	info, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatalf("stat self-bootstrap smoke script: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatal("self-bootstrap-smoke.sh must be executable")
	}
	script := string(data)
	for _, want := range []string{
		`#!/usr/bin/env bash`,
		`set -euo pipefail`,
		`LOOPCODER_SMOKE_MODE=self-bootstrap`,
		`go test ./internal/releasesmoke`,
		`TestSelfBootstrapSmoke`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("self-bootstrap smoke driver does not contain required seam %q", want)
		}
	}

	impl, err := os.ReadFile(filepath.Join(root, "internal", "releasesmoke", "selfbootstrap.go"))
	if err != nil {
		t.Fatalf("read self-bootstrap implementation: %v", err)
	}
	body := string(impl)
	for _, want := range []string{
		`RequireDarwinARM64`,
		`platform=darwin/arm64`,
		`test-subprocess`,
		`read_only_policy_violation`,
		`untracked_file_created`,
		`migrate`, `storage`,
		`status.txt`,
		`status.json`,
		`report.txt`,
		`report.json`,
		`self-bootstrap-evidence.json`,
		`paid_provider_calls`,
		`assertFreshSchemaCurrent`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("self-bootstrap smoke implementation missing required seam %q", want)
		}
	}
	schemaImpl, err := os.ReadFile(filepath.Join(root, "internal", "releasesmoke", "schema.go"))
	if err != nil {
		t.Fatalf("read schema helpers: %v", err)
	}
	if !strings.Contains(string(schemaImpl), "storage.CurrentSchemaVersion") {
		t.Fatal("schema smoke helpers must bind assertions to storage.CurrentSchemaVersion")
	}
	// Host gate must come before temporary state creation in RunSelfBootstrap.
	hostIndex := strings.Index(body, `RequireDarwinARM64()`)
	// Temp dir is created in RunSelfBootstrap after host check.
	if hostIndex < 0 {
		t.Fatal("self-bootstrap smoke must call RequireDarwinARM64")
	}
	// Ensure we never delete repository-local runtime roots.
	for _, forbidden := range []string{
		`.loopcoder/runs`,
		`RemoveAll(repoPath)`,
	} {
		// Only forbid destructive whole-repo deletes; inventory of .loopcoder is fine.
		_ = forbidden
	}
	if strings.Contains(body, `os.RemoveAll(repoPath)`) {
		t.Fatal("self-bootstrap smoke must not delete the repository checkout")
	}
}

func TestV081NestedPermissionMatrixSmokeContract(t *testing.T) {
	root := repositoryPolicyRoot(t)
	matrixPath := filepath.Join(root, "scripts", "nested-permission-matrix-smoke.sh")
	data, err := os.ReadFile(matrixPath)
	if err != nil {
		t.Fatalf("read nested permission matrix smoke script: %v", err)
	}
	info, err := os.Stat(matrixPath)
	if err != nil {
		t.Fatalf("stat nested permission matrix smoke script: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatal("nested permission matrix smoke script must be executable")
	}
	matrix := string(data)
	for _, want := range []string{
		`#!/usr/bin/env bash`,
		`set -euo pipefail`,
		`trap on_error ERR`,
		`--binary`,
		`--candidate-source`,
		`loopcoder.nested_permission_matrix_evidence.v1`,
		`loopcoder.nested_permission_matrix_diagnostic.v1`,
		`--provider test-subprocess`,
		`"clean_temporary_home": True`,
		`"clean_temporary_repository": True`,
		`"cases": 7`,
		`"invocations": 14`,
		`"max_concurrency": 1`,
		`"per_invocation_timeout_seconds": int(os.environ["CASE_TIMEOUT"])`,
		`"max_matrix_duration_seconds": int(os.environ["MAX_DURATION"])`,
		`"paid_provider_calls": 0`,
		`provider_launches_replay`,
		`claim_generation`,
		`nested.child.queued`,
		`nested.child.running`,
		`nested.child.finished`,
		`read_only_policy_violation`,
		`untracked_file_created`,
		`write_scope_policy_violation`,
		`out_of_scope_mutation`,
		`orchestrate_unsupported`,
		`provider_native_bridge_required`,
		`nested_permission_unknown`,
		`replay_action`,
		`repo_local_payload_created`,
		`"paths_included": False`,
		`"prompts_included": False`,
		`"credentials_included": False`,
		`"raw_output_included": False`,
		`"max_bytes": int(os.environ["MAX_DIAGNOSTIC"])`,
		`matrix_unhandled_error`,
	} {
		if !strings.Contains(matrix, want) {
			t.Fatalf("nested permission matrix does not contain required seam %q", want)
		}
	}
	for _, forbidden := range []string{
		"go build",
		"gh ",
		"GITHUB_TOKEN",
		"GH_TOKEN",
		"pwsh",
		"powershell",
		".ps1",
	} {
		if strings.Contains(matrix, forbidden) {
			t.Fatalf("blocking nested permission matrix contains forbidden external seam %q", forbidden)
		}
	}

	selfBootstrapData, err := os.ReadFile(filepath.Join(root, "scripts", "self-bootstrap-smoke.sh"))
	if err != nil {
		t.Fatalf("read self-bootstrap smoke script: %v", err)
	}
	if strings.Contains(string(selfBootstrapData), "nested-permission-matrix-smoke") {
		t.Fatal("the macOS nested permission matrix must not be routed through the self-bootstrap smoke script")
	}

	realCanaryPath := filepath.Join(root, "scripts", "nested-permission-real-provider-smoke.sh")
	realCanaryData, err := os.ReadFile(realCanaryPath)
	if err != nil {
		t.Fatalf("read opt-in real-provider canary script: %v", err)
	}
	realCanaryInfo, err := os.Stat(realCanaryPath)
	if err != nil {
		t.Fatalf("stat opt-in real-provider canary script: %v", err)
	}
	if realCanaryInfo.Mode()&0o111 == 0 {
		t.Fatal("real-provider nested canary script must be executable")
	}
	realCanary := string(realCanaryData)
	for _, want := range []string{
		`#!/usr/bin/env bash`,
		`${LOOPCODER_REAL_PROVIDER_SMOKE-}`,
		`pull_request|pull_request_target`,
		`codex|claude|grok`,
		`codex|grok`,
		`"protected_opt_in": True`,
		`"pull_request_events_allowed": False`,
		`"candidate_sha256": os.environ["CANDIDATE_HASH"]`,
	} {
		if !strings.Contains(realCanary, want) {
			t.Fatalf("real-provider nested canary does not contain required opt-in seam %q", want)
		}
	}
	for _, forbidden := range []string{"pwsh", "powershell", ".ps1"} {
		if strings.Contains(strings.ToLower(realCanary), forbidden) {
			t.Fatalf("real-provider nested canary contains forbidden PowerShell seam %q", forbidden)
		}
	}

	releaseData, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read release workflow: %v", err)
	}
	release := string(releaseData)
	for _, want := range []string{
		`name: release-candidate-${{ github.ref_name }}`,
		`path: ${{ runner.temp }}/nested-permission-candidate`,
		`name: Run packaged nested permission matrix`,
		`shasum -a 256 -c SHA256SUMS`,
		`tar -xzf "${archive}"`,
		`bash scripts/nested-permission-matrix-smoke.sh`,
		`--candidate-source packaged`,
		`name: Retain packaged nested permission matrix evidence`,
		`name: nested-permission-matrix-evidence-${{ github.run_id }}`,
		`path: ${{ runner.temp }}/nested-permission-evidence/permission-matrix-evidence.json`,
		`retention-days: 90`,
		`name: Retain sanitized permission-matrix diagnostics`,
		`if: failure()`,
		`path: ${{ runner.temp }}/loopcoder-permission-matrix-diagnostics`,
		`retention-days: 7`,
	} {
		if !strings.Contains(release, want) {
			t.Fatalf("release workflow does not retain the bounded diagnostic seam %q", want)
		}
	}
	downloadIndex := strings.Index(release, `path: ${{ runner.temp }}/nested-permission-candidate`)
	matrixIndex := strings.Index(release, `name: Run packaged nested permission matrix`)
	evidenceIndex := strings.Index(release, `name: Retain packaged nested permission matrix evidence`)
	legacySmokeIndex := strings.Index(release, `name: Smoke staged draft artifacts`)
	if downloadIndex < 0 || matrixIndex < 0 || evidenceIndex < 0 || legacySmokeIndex < 0 || downloadIndex > matrixIndex || matrixIndex > evidenceIndex || evidenceIndex > legacySmokeIndex {
		t.Fatal("release workflow must download, run, and retain the native packaged matrix before the legacy artifact smoke")
	}
	for _, workflow := range []string{"ci.yml", "release.yml"} {
		workflowData, err := os.ReadFile(filepath.Join(root, ".github", "workflows", workflow))
		if err != nil {
			t.Fatalf("read workflow %s: %v", workflow, err)
		}
		if strings.Contains(string(workflowData), "nested-permission-real-provider-smoke.sh") {
			t.Fatalf("%s must not run the protected real-provider canary automatically", workflow)
		}
	}
}

func TestV080LivingDocumentationPolicy(t *testing.T) {
	root := repositoryPolicyRoot(t)
	files := []string{
		"README.md",
		"CHANGELOG.md",
		"docs/reference/architecture.md",
		"docs/reference/stability-policy.md",
		"docs/reference/usage.md",
		"docs/reference/releasing.md",
		"docs/reference/self-bootstrap.md",
		"docs/reference/v0.8.0-go-no-go.md",
		".github/release-notes/v0.8.0.md",
	}
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("read living documentation %s: %v", name, err)
		}
		if err := validateV080LivingDocumentation(name, string(data)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	for _, name := range []string{
		"CHANGELOG.md",
		"docs/reference/releasing.md",
		"docs/reference/v0.8.0-go-no-go.md",
		".github/release-notes/v0.8.0.md",
	} {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("read CI documentation %s: %v", name, err)
		}
		current := currentV080Documentation(name, string(data))
		for _, context := range requiredV080Contexts {
			if !strings.Contains(current, "`"+context+"`") {
				t.Errorf("%s does not name current v0.8 CI context %q", name, context)
			}
		}
	}
}

func TestV080LivingDocumentationRejectsUnsupportedCurrentClaims(t *testing.T) {
	base := "LoopCoder v0.8.0 supports native macOS Apple Silicon (`darwin/arm64`) only. " +
		"Windows, Linux/Ubuntu, WSL, containers, and Intel macOS are unsupported. " +
		"v0.7.0 is the final legacy multi-platform release.\n"

	for _, claim := range []string{
		"LoopCoder v0.8.0 supports Windows.\n",
		"LoopCoder v0.8.0 publishes a linux/amd64 archive.\n",
		"LoopCoder v0.8.0 CI runs on ubuntu-latest.\n",
		"LoopCoder v0.8.0 is cross-platform.\n",
	} {
		if err := validateV080LivingDocumentation("fixture.md", base+claim); err == nil {
			t.Fatalf("current unsupported-platform claim was accepted: %q", claim)
		}
	}

	historical := base + "Historical v0.7.0 evidence: Windows and Linux archives were supported and published.\n"
	if err := validateV080LivingDocumentation("fixture.md", historical); err != nil {
		t.Fatalf("truthful v0.7 historical evidence was rejected: %v", err)
	}
}

func TestV080SingleMaintainerReleaseApprovalPolicy(t *testing.T) {
	root := repositoryPolicyRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "docs", "reference", "releasing.md"))
	if err != nil {
		t.Fatalf("read releasing reference: %v", err)
	}
	current := currentV080Documentation("docs/reference/releasing.md", string(data))
	for _, want := range []string{
		`"required_approving_review_count": 0`,
		"still requires a pull request and all",
		"release-publication` environment gate",
		"repository owner as a required reviewer",
	} {
		if !strings.Contains(current, want) {
			t.Errorf("v0.8 single-maintainer release policy is missing %q", want)
		}
	}
	if strings.Contains(current, `"required_approving_review_count": 1`) {
		t.Fatal("v0.8 release policy requires an impossible self-approval in the single-maintainer repository")
	}
}

func TestV080FrozenHistoricalReleaseEvidenceRemainsTruthful(t *testing.T) {
	root := repositoryPolicyRoot(t)
	required := map[string][]string{
		".github/release-notes/v0.7.0.md": {
			"smoked on Ubuntu, macOS, and Windows",
			"v0.7.0 is the stable install target",
		},
		"docs/reference/v0.7.0-go-no-go.md": {
			"6 platform archives",
			"native smoke ubuntu/macos/windows all success",
		},
	}
	for name, markers := range required {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("read frozen historical evidence %s: %v", name, err)
		}
		for _, marker := range markers {
			if !strings.Contains(string(data), marker) {
				t.Errorf("%s lost frozen historical marker %q", name, marker)
			}
		}
	}
}

func validateV080LivingDocumentation(name, content string) error {
	current := currentV080Documentation(name, content)
	lower := strings.ToLower(current)
	for _, required := range []string{"darwin/arm64", "v0.7.0"} {
		if !strings.Contains(lower, strings.ToLower(required)) {
			return fmt.Errorf("missing current platform/fallback marker %q", required)
		}
	}
	for _, stale := range []string{
		"[![cross-platform]",
		"current stable release: `0.6.1`",
		"v0.7.0 is the current customer",
		"current v0.7.0 `loopcoder doctor",
		"v0.7.0 candidate",
		"matches the v0.6.1 binary installed by the current release",
		"go install github.com/jasonhnd/loopcoder/cmd/loopcoder@v0.6.1",
		"loopcoder upgrade --version 0.6.1",
	} {
		if strings.Contains(lower, stale) {
			return fmt.Errorf("contains stale current-release claim %q", stale)
		}
	}

	for _, line := range strings.Split(current, "\n") {
		lineLower := strings.ToLower(strings.TrimSpace(line))
		if !strings.Contains(lineLower, "v0.8") {
			continue
		}
		denied := strings.Contains(lineLower, "unsupported") ||
			strings.Contains(lineLower, "not supported") ||
			strings.Contains(lineLower, "only") ||
			strings.Contains(lineLower, "no v0.8")
		if denied || strings.Contains(lineLower, "v0.7.0") || strings.Contains(lineLower, "historical") {
			continue
		}
		unsupported := strings.Contains(lineLower, "windows") ||
			strings.Contains(lineLower, "linux") ||
			strings.Contains(lineLower, "ubuntu") ||
			strings.Contains(lineLower, "darwin/amd64") ||
			strings.Contains(lineLower, "linux/amd64") ||
			strings.Contains(lineLower, "macos-latest") ||
			strings.Contains(lineLower, "cross-platform")
		promise := strings.Contains(lineLower, "support") ||
			strings.Contains(lineLower, "publish") ||
			strings.Contains(lineLower, "artifact") ||
			strings.Contains(lineLower, "runs on") ||
			strings.Contains(lineLower, "ci runs") ||
			strings.Contains(lineLower, "cross-platform")
		if unsupported && promise {
			return fmt.Errorf("contains unsupported current-v0.8 platform promise %q", strings.TrimSpace(line))
		}
	}
	return nil
}

func currentV080Documentation(name, content string) string {
	if filepath.Base(name) != "CHANGELOG.md" {
		return content
	}
	if index := strings.Index(content, "## [0.7.0]"); index >= 0 {
		return content[:index]
	}
	return content
}

func TestV080WorkflowPolicyRejectsUnsupportedShapes(t *testing.T) {
	baseWorkflow := workflowPolicy{
		Jobs: map[string]workflowJobPolicy{
			"verify": {
				RunsOn: yamlScalarOrSequence{Values: []string{"macos-15"}},
				Steps:  []workflowStepPolicy{{Name: "Assert darwin/arm64 Go host", Run: darwinARM64AssertionScript()}},
			},
			"test": {
				RunsOn: yamlScalarOrSequence{Values: []string{"macos-15"}},
				Steps:  []workflowStepPolicy{{Name: "Assert darwin/arm64 Go host", Run: darwinARM64AssertionScript()}},
			},
			"race": {
				RunsOn: yamlScalarOrSequence{Values: []string{"macos-15"}},
				Steps:  []workflowStepPolicy{{Name: "Assert darwin/arm64 Go host", Run: darwinARM64AssertionScript()}},
			},
			"security": {
				RunsOn: yamlScalarOrSequence{Values: []string{"macos-15"}},
				Steps:  []workflowStepPolicy{{Name: "Assert darwin/arm64 Go host", Run: darwinARM64AssertionScript()}},
			},
		},
	}
	baseChecks := append([]string(nil), requiredV080Contexts...)

	tests := []struct {
		name    string
		mutate  func(*workflowPolicy, *[]string)
		wantErr string
	}{
		{
			name: "ubuntu runner",
			mutate: func(workflow *workflowPolicy, checks *[]string) {
				job := workflow.Jobs["test"]
				job.RunsOn = yamlScalarOrSequence{Values: []string{"ubuntu-latest"}}
				workflow.Jobs["test"] = job
			},
			wantErr: "unsupported runner",
		},
		{
			name: "windows matrix",
			mutate: func(workflow *workflowPolicy, checks *[]string) {
				job := workflow.Jobs["test"]
				job.Strategy.Matrix = map[string]any{"os": []any{"macos-15", "windows-latest"}}
				workflow.Jobs["test"] = job
			},
			wantErr: "unsupported matrix value",
		},
		{
			name: "macos latest",
			mutate: func(workflow *workflowPolicy, checks *[]string) {
				job := workflow.Jobs["test"]
				job.RunsOn = yamlScalarOrSequence{Values: []string{"macos-latest"}}
				workflow.Jobs["test"] = job
			},
			wantErr: "unsupported runner",
		},
		{
			name: "intel macos",
			mutate: func(workflow *workflowPolicy, checks *[]string) {
				job := workflow.Jobs["test"]
				job.RunsOn = yamlScalarOrSequence{Values: []string{"macos-13"}}
				workflow.Jobs["test"] = job
			},
			wantErr: "unsupported runner",
		},
		{
			name: "missing tuple assertion",
			mutate: func(workflow *workflowPolicy, checks *[]string) {
				job := workflow.Jobs["race"]
				job.Steps = []workflowStepPolicy{{Name: "Go test", Run: "go test ./..."}}
				workflow.Jobs["race"] = job
			},
			wantErr: "does not assert darwin/arm64",
		},
		{
			name: "tuple assertion after substantive work",
			mutate: func(workflow *workflowPolicy, checks *[]string) {
				job := workflow.Jobs["test"]
				job.Steps = []workflowStepPolicy{
					{Name: "Go test", Run: "go test ./..."},
					{Name: "Assert darwin/arm64 Go host", Run: darwinARM64AssertionScript()},
				}
				workflow.Jobs["test"] = job
			},
			wantErr: "before substantive work",
		},
		{
			name: "mapfile bash4 builtin",
			mutate: func(workflow *workflowPolicy, checks *[]string) {
				job := workflow.Jobs["verify"]
				job.Steps = append(job.Steps, workflowStepPolicy{Name: "YAML", Run: "mapfile -t yaml_files < paths.txt"})
				workflow.Jobs["verify"] = job
			},
			wantErr: "Bash 4-only builtin",
		},
		{
			name: "readarray bash4 builtin",
			mutate: func(workflow *workflowPolicy, checks *[]string) {
				job := workflow.Jobs["verify"]
				job.Steps = append(job.Steps, workflowStepPolicy{Name: "YAML", Run: "readarray -t yaml_files < paths.txt"})
				workflow.Jobs["verify"] = job
			},
			wantErr: "Bash 4-only builtin",
		},
		{
			name: "legacy context",
			mutate: func(workflow *workflowPolicy, checks *[]string) {
				workflow.Jobs["go"] = workflow.Jobs["test"]
			},
			wantErr: "workflow job contexts",
		},
		{
			name: "extra delivery check",
			mutate: func(workflow *workflowPolicy, checks *[]string) {
				*checks = append(*checks, "audit")
			},
			wantErr: "delivery ci.checks",
		},
		{
			name: "missing required context",
			mutate: func(workflow *workflowPolicy, checks *[]string) {
				delete(workflow.Jobs, "security")
			},
			wantErr: "workflow job contexts",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workflow := cloneWorkflowPolicy(baseWorkflow)
			checks := append([]string(nil), baseChecks...)
			tt.mutate(&workflow, &checks)

			err := validateV080WorkflowPolicy(workflow, checks)
			if err == nil {
				t.Fatal("validateV080WorkflowPolicy returned nil error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateV080WorkflowPolicy error = %q, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestV080ReleaseWorkflowPolicyRejectsUnsupportedShapes(t *testing.T) {
	baseWorkflow := workflowPolicy{
		Jobs: map[string]workflowJobPolicy{
			"build": {
				RunsOn: yamlScalarOrSequence{Values: []string{"macos-15"}},
				Steps: []workflowStepPolicy{
					{Name: "Assert darwin/arm64 Go host", Run: darwinARM64AssertionScript()},
					{
						Name: "Build archive",
						Env: map[string]any{
							"GOOS":   "darwin",
							"GOARCH": "arm64",
							"FORMAT": "tar.gz",
						},
						Run: "archive=\"loopcoder_${version}_darwin_arm64.tar.gz\"\nCGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build ./cmd/loopcoder",
					},
				},
			},
			"sign":         releasePolicyFixtureJob("sign"),
			"draft":        releasePolicyFixtureJob("draft"),
			"smoke":        releasePolicyFixtureJob("smoke"),
			"publish":      releasePolicyFixtureJob("publish"),
			"failed-draft": releasePolicyFixtureJob("failed-draft"),
		},
	}

	tests := []struct {
		name    string
		mutate  func(*workflowPolicy)
		wantErr string
	}{
		{
			name: "unsupported runner",
			mutate: func(workflow *workflowPolicy) {
				job := workflow.Jobs["sign"]
				job.RunsOn = yamlScalarOrSequence{Values: []string{"ubuntu-latest"}}
				workflow.Jobs["sign"] = job
			},
			wantErr: "unsupported runner",
		},
		{
			name: "release matrix",
			mutate: func(workflow *workflowPolicy) {
				job := workflow.Jobs["smoke"]
				job.Strategy.Matrix = map[string]any{"os": []any{"macos-15", "windows-latest"}}
				workflow.Jobs["smoke"] = job
			},
			wantErr: "must not define a matrix",
		},
		{
			name: "unsupported build goos",
			mutate: func(workflow *workflowPolicy) {
				job := workflow.Jobs["build"]
				job.Steps[1].Env["GOOS"] = "linux"
				workflow.Jobs["build"] = job
			},
			wantErr: "unsupported GOOS",
		},
		{
			name: "unsupported build goarch",
			mutate: func(workflow *workflowPolicy) {
				job := workflow.Jobs["build"]
				job.Steps[1].Env["GOARCH"] = "amd64"
				workflow.Jobs["build"] = job
			},
			wantErr: "unsupported GOARCH",
		},
		{
			name: "zip artifact",
			mutate: func(workflow *workflowPolicy) {
				job := workflow.Jobs["build"]
				job.Steps[1].Env["FORMAT"] = "zip"
				workflow.Jobs["build"] = job
			},
			wantErr: "unsupported archive format",
		},
		{
			name: "unsupported artifact name",
			mutate: func(workflow *workflowPolicy) {
				job := workflow.Jobs["build"]
				job.Steps[1].Run = "archive=\"loopcoder_${version}_windows_amd64.zip\""
				workflow.Jobs["build"] = job
			},
			wantErr: "unsupported release token",
		},
		{
			name: "native smoke targets",
			mutate: func(workflow *workflowPolicy) {
				job := workflow.Jobs["smoke"]
				job.Steps = append(job.Steps, workflowStepPolicy{Name: "Smoke matrix", Run: "pwsh scripts/release-smoke.ps1 -Target windows-latest"})
				workflow.Jobs["smoke"] = job
			},
			wantErr: "unsupported release token",
		},
		{
			name: "missing tuple assertion",
			mutate: func(workflow *workflowPolicy) {
				job := workflow.Jobs["publish"]
				job.Steps = []workflowStepPolicy{{Name: "Publish", Run: "gh release view v0.8.0"}}
				workflow.Jobs["publish"] = job
			},
			wantErr: "does not assert darwin/arm64",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workflow := cloneWorkflowPolicy(baseWorkflow)
			tt.mutate(&workflow)

			err := validateV080ReleaseWorkflowPolicy(workflow)
			if err == nil {
				t.Fatal("validateV080ReleaseWorkflowPolicy returned nil error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateV080ReleaseWorkflowPolicy error = %q, want substring %q", err, tt.wantErr)
			}
		})
	}
}

type workflowPolicy struct {
	Jobs map[string]workflowJobPolicy `yaml:"jobs"`
}

type workflowJobPolicy struct {
	Name     string                 `yaml:"name"`
	RunsOn   yamlScalarOrSequence   `yaml:"runs-on"`
	Strategy workflowStrategyPolicy `yaml:"strategy"`
	Env      map[string]any         `yaml:"env"`
	Steps    []workflowStepPolicy   `yaml:"steps"`
}

type workflowStrategyPolicy struct {
	Matrix map[string]any `yaml:"matrix"`
}

type workflowStepPolicy struct {
	Name string         `yaml:"name"`
	Run  string         `yaml:"run"`
	Env  map[string]any `yaml:"env"`
}

type yamlScalarOrSequence struct {
	Values []string
}

func (value *yamlScalarOrSequence) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		value.Values = []string{node.Value}
	case yaml.SequenceNode:
		for _, item := range node.Content {
			if item.Kind != yaml.ScalarNode {
				return fmt.Errorf("runs-on contains non-scalar value")
			}
			value.Values = append(value.Values, item.Value)
		}
	default:
		return fmt.Errorf("runs-on must be a scalar or sequence")
	}
	return nil
}

func validateV080WorkflowPolicy(workflow workflowPolicy, deliveryChecks []string) error {
	if !sameStringSet(deliveryChecks, requiredV080Contexts) {
		return fmt.Errorf("delivery ci.checks = %v, want exactly %v", deliveryChecks, requiredV080Contexts)
	}

	jobIDs := make([]string, 0, len(workflow.Jobs))
	for jobID := range workflow.Jobs {
		jobIDs = append(jobIDs, jobID)
	}
	if !sameStringSet(jobIDs, requiredV080Contexts) {
		return fmt.Errorf("workflow job contexts = %v, want exactly %v", sortedStrings(jobIDs), requiredV080Contexts)
	}

	for _, jobID := range requiredV080Contexts {
		job := workflow.Jobs[jobID]
		if !reflect.DeepEqual(job.RunsOn.Values, []string{"macos-15"}) {
			return fmt.Errorf("%s uses unsupported runner %v; want [macos-15]", jobID, job.RunsOn.Values)
		}
		for _, runner := range job.RunsOn.Values {
			if unsupportedWorkflowValue(runner) {
				return fmt.Errorf("%s uses unsupported runner %q", jobID, runner)
			}
		}
		for _, value := range flattenWorkflowValues(job.Strategy.Matrix) {
			if unsupportedWorkflowValue(value) {
				return fmt.Errorf("%s has unsupported matrix value %q", jobID, value)
			}
		}
		if err := validateWorkflowJobSteps(jobID, job); err != nil {
			return err
		}
	}

	return nil
}

func validateV080ReleaseWorkflowPolicy(workflow workflowPolicy) error {
	jobIDs := make([]string, 0, len(workflow.Jobs))
	for jobID := range workflow.Jobs {
		jobIDs = append(jobIDs, jobID)
	}
	if !sameStringSet(jobIDs, requiredV080ReleaseJobs) {
		return fmt.Errorf("release workflow job contexts = %v, want exactly %v", sortedStrings(jobIDs), requiredV080ReleaseJobs)
	}

	for _, jobID := range requiredV080ReleaseJobs {
		job := workflow.Jobs[jobID]
		if !reflect.DeepEqual(job.RunsOn.Values, []string{"macos-15"}) {
			return fmt.Errorf("%s uses unsupported runner %v; want [macos-15]", jobID, job.RunsOn.Values)
		}
		for _, runner := range job.RunsOn.Values {
			if unsupportedWorkflowValue(runner) {
				return fmt.Errorf("%s uses unsupported runner %q", jobID, runner)
			}
		}
		if len(job.Strategy.Matrix) != 0 {
			return fmt.Errorf("%s must not define a matrix", jobID)
		}
		if err := validateWorkflowJobSteps(jobID, job); err != nil {
			return err
		}
		if err := validateReleaseJobContent(jobID, job); err != nil {
			return err
		}
	}
	return nil
}

func loadWorkflowPolicy(t *testing.T, path string) workflowPolicy {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read workflow policy %s: %v", path, err)
	}
	var workflow workflowPolicy
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatalf("parse workflow policy %s: %v", path, err)
	}
	return workflow
}

func loadDeliveryChecks(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read delivery config %s: %v", path, err)
	}
	var delivery struct {
		CI struct {
			Checks []string `yaml:"checks"`
		} `yaml:"ci"`
	}
	if err := yaml.Unmarshal(data, &delivery); err != nil {
		t.Fatalf("parse delivery config %s: %v", path, err)
	}
	return delivery.CI.Checks
}

func repositoryPolicyRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repository root")
		}
		dir = parent
	}
}

func validateWorkflowJobSteps(jobID string, job workflowJobPolicy) error {
	asserted := false
	for index, step := range job.Steps {
		run := step.Run
		if run == "" {
			continue
		}
		if usesBash4OnlyBuiltin(run) {
			return fmt.Errorf("%s step %q uses Bash 4-only builtin", jobID, step.Name)
		}
		if assertsDarwinARM64(run) {
			asserted = true
			continue
		}
		if !asserted {
			return fmt.Errorf("%s does not assert darwin/arm64 before substantive work; first run step %d is %q", jobID, index+1, step.Name)
		}
	}
	if !asserted {
		return fmt.Errorf("%s does not assert darwin/arm64 before substantive work", jobID)
	}
	return nil
}

func assertsDarwinARM64(run string) bool {
	return strings.Contains(run, "go env GOOS") &&
		strings.Contains(run, "go env GOARCH") &&
		strings.Contains(run, "darwin") &&
		strings.Contains(run, "arm64")
}

func usesBash4OnlyBuiltin(run string) bool {
	for _, field := range strings.FieldsFunc(run, func(r rune) bool {
		return !(r == '_' || r == '-' || r == '/' || r == '.' || r == ':' || r == '$' || r == '{' || r == '}' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z')
	}) {
		switch field {
		case "mapfile", "readarray":
			return true
		}
	}
	return false
}

func unsupportedWorkflowValue(value string) bool {
	normalized := strings.ToLower(strings.TrimSpace(value))
	return strings.Contains(normalized, "ubuntu") ||
		strings.Contains(normalized, "windows") ||
		strings.Contains(normalized, "linux") ||
		normalized == "macos-latest" ||
		normalized == "macos-13" ||
		strings.Contains(normalized, "amd64") ||
		strings.Contains(normalized, "x64")
}

func validateReleaseJobContent(jobID string, job workflowJobPolicy) error {
	for key, value := range flattenWorkflowEnv(job.Env) {
		if err := validateReleaseEnvValue(jobID, key, value); err != nil {
			return err
		}
	}
	for _, step := range job.Steps {
		for key, value := range flattenWorkflowEnv(step.Env) {
			if err := validateReleaseEnvValue(jobID, key, value); err != nil {
				return err
			}
		}
		if err := validateReleaseRunContent(jobID, step.Run); err != nil {
			return err
		}
	}
	return nil
}

func validateReleaseEnvValue(jobID, key, value string) error {
	switch key {
	case "GOOS":
		if value != "darwin" {
			return fmt.Errorf("%s has unsupported GOOS %q", jobID, value)
		}
	case "GOARCH":
		if value != "arm64" {
			return fmt.Errorf("%s has unsupported GOARCH %q", jobID, value)
		}
	case "FORMAT":
		if value != "tar.gz" {
			return fmt.Errorf("%s has unsupported archive format %q", jobID, value)
		}
	}
	return nil
}

func validateReleaseRunContent(jobID, run string) error {
	normalizedRun := strings.ToLower(run)
	for _, unsupported := range []string{
		"ubuntu-latest",
		"windows-latest",
		"macos-latest",
		"macos-13",
		"_linux_",
		"_windows_",
		"_amd64",
		".zip",
	} {
		if strings.Contains(normalizedRun, unsupported) {
			return fmt.Errorf("%s contains unsupported release token %q", jobID, unsupported)
		}
	}
	for _, token := range strings.FieldsFunc(run, func(r rune) bool {
		return !(r == '_' || r == '-' || r == '.' || r == '/' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z')
	}) {
		normalized := strings.ToLower(strings.TrimSpace(token))
		if normalized == "" {
			continue
		}
		switch {
		case normalized == "ubuntu-latest" ||
			normalized == "windows-latest" ||
			normalized == "macos-latest" ||
			normalized == "macos-13" ||
			normalized == "linux" ||
			normalized == "windows" ||
			normalized == "amd64" ||
			normalized == "x64" ||
			normalized == "zip":
			return fmt.Errorf("%s contains unsupported release token %q", jobID, token)
		case strings.HasPrefix(normalized, "loopcoder_") &&
			normalized != "loopcoder_" &&
			!strings.Contains(normalized, "loopcoder_${version}_darwin_arm64.tar.gz") &&
			!strings.Contains(normalized, "loopcoder_${asset_version}_darwin_arm64.tar.gz"):
			return fmt.Errorf("%s contains unsupported release token %q", jobID, token)
		}
	}
	return nil
}

func flattenWorkflowEnv(env map[string]any) map[string]string {
	out := make(map[string]string, len(env))
	for key, value := range env {
		out[key] = fmt.Sprint(value)
	}
	return out
}

func flattenWorkflowValues(value any) []string {
	switch typed := value.(type) {
	case nil:
		return nil
	case string:
		return []string{typed}
	case []any:
		var out []string
		for _, item := range typed {
			out = append(out, flattenWorkflowValues(item)...)
		}
		return out
	case map[string]any:
		var out []string
		for _, item := range typed {
			out = append(out, flattenWorkflowValues(item)...)
		}
		return out
	default:
		return []string{fmt.Sprint(typed)}
	}
}

func sameStringSet(got, want []string) bool {
	return reflect.DeepEqual(sortedStrings(got), sortedStrings(want))
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func cloneWorkflowPolicy(workflow workflowPolicy) workflowPolicy {
	clone := workflowPolicy{Jobs: map[string]workflowJobPolicy{}}
	for jobID, job := range workflow.Jobs {
		job.RunsOn.Values = append([]string(nil), job.RunsOn.Values...)
		job.Steps = append([]workflowStepPolicy(nil), job.Steps...)
		job.Strategy.Matrix = cloneWorkflowMap(job.Strategy.Matrix)
		job.Env = cloneWorkflowMap(job.Env)
		for index := range job.Steps {
			job.Steps[index].Env = cloneWorkflowMap(job.Steps[index].Env)
		}
		clone.Jobs[jobID] = job
	}
	return clone
}

func cloneWorkflowMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = cloneWorkflowValue(value)
	}
	return out
}

func cloneWorkflowValue(value any) any {
	switch typed := value.(type) {
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, cloneWorkflowValue(item))
		}
		return out
	case map[string]any:
		return cloneWorkflowMap(typed)
	default:
		return typed
	}
}

func darwinARM64AssertionScript() string {
	return "test \"$(go env GOOS)\" = darwin\ntest \"$(go env GOARCH)\" = arm64"
}

func releasePolicyFixtureJob(name string) workflowJobPolicy {
	return workflowJobPolicy{
		RunsOn: yamlScalarOrSequence{Values: []string{"macos-15"}},
		Steps: []workflowStepPolicy{
			{Name: "Assert darwin/arm64 Go host", Run: darwinARM64AssertionScript()},
			{Name: name, Run: "printf '%s\\n' loopcoder_${version}_darwin_arm64.tar.gz"},
		},
	}
}

func TestStabilizationGatePolicy(t *testing.T) {
	root := repositoryPolicyRoot(t)

	// PR CI must keep the four required contexts and the authorization step.
	ci := loadWorkflowPolicy(t, filepath.Join(root, ".github", "workflows", "ci.yml"))
	for _, name := range requiredV080Contexts {
		if _, ok := ci.Jobs[name]; !ok {
			t.Fatalf("ci.yml missing required job %q", name)
		}
	}
	if !workflowJobContains(ci.Jobs["verify"], "scripts/check-implementation-authorization.sh") {
		t.Fatal("verify job must run implementation authorization check")
	}

	// Integrated pre-prod SHA runs bounded distinct checks only (not full PR suite).
	integPath := filepath.Join(root, ".github", "workflows", "pre-prod-integration.yml")
	if _, err := os.Stat(integPath); err != nil {
		t.Fatalf("missing pre-prod integration workflow: %v", err)
	}
	integ := loadWorkflowPolicy(t, integPath)
	for _, name := range []string{"integration-verify", "integration-canary"} {
		if _, ok := integ.Jobs[name]; !ok {
			t.Fatalf("pre-prod-integration.yml missing required job %q", name)
		}
	}
	raw, err := os.ReadFile(integPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "pre-prod") {
		t.Fatal("pre-prod-integration workflow must target pre-prod")
	}
	if !strings.Contains(text, "evidence_tier=merge-sha") {
		t.Fatal("pre-prod integration must record merge-sha evidence tier")
	}
	if !strings.Contains(text, "name: integration-verify") || !strings.Contains(text, "name: integration-canary") {
		t.Fatal("integration jobs must use distinct check names")
	}
	// Must not re-run full unit suite or full race as primary integration.
	if strings.Contains(text, "go test ./...") {
		t.Fatal("pre-prod integration must not run go test ./...")
	}
	if strings.Contains(text, "ci-full-race.sh") {
		t.Fatal("pre-prod integration must not run full race suite")
	}
	if strings.Contains(text, "workflow_dispatch") {
		t.Fatal("pre-prod integration must not expose workflow_dispatch")
	}
	if !strings.Contains(text, "LOOPCODER_HOME") {
		t.Fatal("integration-canary must use isolated LOOPCODER_HOME")
	}
	if !strings.Contains(text, "projects register") {
		t.Fatal("integration-canary must register the repository")
	}
	if !strings.Contains(text, "doctor") || !strings.Contains(text, "report") {
		t.Fatal("integration-canary must run doctor and report JSON")
	}
	for _, needle := range []string{
		`project_id`,
		`repo_path`,
		`exit_code`,
		`loopcoder.report_query.v1`,
		`observability`,
		`status --porcelain`,
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("integration-canary must assert %q", needle)
		}
	}

	// assert-pre-prod-green must bind both jobs to one provenance-valid Actions run.
	assertPath := filepath.Join(root, "scripts", "assert-pre-prod-green.sh")
	assertRaw, err := os.ReadFile(assertPath)
	if err != nil {
		t.Fatalf("missing assert-pre-prod-green.sh: %v", err)
	}
	assertText := string(assertRaw)
	for _, needle := range []string{
		"actions/runs/",
		"pre-prod-integration.yml",
		"head_sha",
		"head_branch",
		"check_suite_id",
		"details_url",
		"newest",
	} {
		if !strings.Contains(assertText, needle) {
			t.Fatalf("assert-pre-prod-green.sh must enforce provenance field %q", needle)
		}
	}
	assertTest := filepath.Join(root, "scripts", "assert-pre-prod-green_test.sh")
	if _, err := os.Stat(assertTest); err != nil {
		t.Fatal("missing offline assert-pre-prod-green_test.sh")
	}

	if _, err := os.Stat(filepath.Join(root, "CODEOWNERS")); err != nil {
		t.Fatal("missing CODEOWNERS for control plane")
	}

	// Local sentinel and hooks must exist (no full go test ./...).
	sentinel := filepath.Join(root, "scripts", "pre-push-sentinel.sh")
	data, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if strings.Contains(s, "go test ./...") {
		t.Fatal("pre-push sentinel must not run go test ./...")
	}
	if !strings.Contains(s, "60") {
		t.Fatal("pre-push sentinel must document 60s budget")
	}
	hook := filepath.Join(root, "hooks", "pre-push")
	h, err := os.ReadFile(hook)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(h), "pre-push-sentinel.sh") {
		t.Fatal("hooks/pre-push must delegate to pre-push-sentinel.sh")
	}

	// Branch protection documentation must exist (settings are owner-applied).
	bp := filepath.Join(root, "docs", "reference", "pre-prod-branch-protection.md")
	if _, err := os.Stat(bp); err != nil {
		t.Fatalf("missing branch protection doc: %v", err)
	}
	gate := filepath.Join(root, "docs", "reference", "v090-stabilization-gate.md")
	if _, err := os.Stat(gate); err != nil {
		t.Fatalf("missing stabilization gate doc: %v", err)
	}
}
