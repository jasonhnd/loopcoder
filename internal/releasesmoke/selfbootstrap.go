package releasesmoke

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// SelfBootstrapOptions configures the self-bootstrap acceptance smoke.
type SelfBootstrapOptions struct {
	Repo          string
	Binary        string // empty => build from Repo
	Version       string
	KeepArtifacts bool
	ArtifactDir   string // optional override; when empty a temp dir is used
}

// RunSelfBootstrap executes the darwin/arm64 self-bootstrap acceptance checks.
func RunSelfBootstrap(opts SelfBootstrapOptions) error {
	if err := RequireDarwinARM64(); err != nil {
		return err
	}

	repoPath, err := filepath.Abs(opts.Repo)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(repoPath, ".git")); err != nil {
		return fmt.Errorf("self-bootstrap smoke must run against a git checkout; .git not found under %s", repoPath)
	}

	repoRuntimeBefore, err := repoRuntimeInventory(repoPath)
	if err != nil {
		return err
	}

	tmp, err := os.MkdirTemp("", "loopcoder-self-bootstrap-*")
	if err != nil {
		return err
	}
	keep := opts.KeepArtifacts
	defer func() {
		if !keep {
			_ = os.RemoveAll(tmp)
		}
	}()

	loopcoderHome := filepath.Join(tmp, "home")
	artifactDir := opts.ArtifactDir
	if artifactDir == "" {
		artifactDir = filepath.Join(tmp, "artifacts")
	}
	if err := os.MkdirAll(loopcoderHome, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		return err
	}

	return withEnv(map[string]string{"LOOPCODER_HOME": loopcoderHome}, func() error {
		return runSelfBootstrapBody(opts, repoPath, loopcoderHome, artifactDir, repoRuntimeBefore, &keep, tmp)
	})
}

func runSelfBootstrapBody(opts SelfBootstrapOptions, repoPath, loopcoderHome, artifactDir string, repoRuntimeBefore []string, keep *bool, tmp string) error {
	usingStaged := strings.TrimSpace(opts.Binary) != ""
	binaryPath := strings.TrimSpace(opts.Binary)
	if binaryPath == "" {
		binaryPath = filepath.Join(tmp, "loopcoder")
		cmd := exec.Command("go", "build", "-o", binaryPath, "./cmd/loopcoder")
		cmd.Dir = repoPath
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("build local loopcoder binary: %w\n%s", err, out)
		}
	} else {
		abs, err := filepath.Abs(binaryPath)
		if err != nil {
			return err
		}
		binaryPath = abs
	}

	binaryHash, err := fileSHA256(binaryPath)
	if err != nil {
		return err
	}
	versionText, err := runChecked("record selected loopcoder binary identity", binaryPath, nil, "version")
	if err != nil {
		return err
	}
	versionText = strings.TrimSpace(versionText)
	_ = writeFile(filepath.Join(artifactDir, "candidate-version.txt"), versionText+"\n")
	_ = writeFile(filepath.Join(artifactDir, "candidate-sha256.txt"), binaryHash+"\n")
	if !regexp.MustCompile(`(^|\s)platform=darwin/arm64(\s|$)`).MatchString(versionText) {
		return fmt.Errorf("selected binary is not a darwin/arm64 binary: %s", versionText)
	}
	plain := plainVersion(opts.Version)
	if usingStaged {
		versionPattern := regexp.MustCompile(`(^|\s)version=v?` + regexp.QuoteMeta(plain) + `(\s|$)`)
		if !versionPattern.MatchString(versionText) {
			return fmt.Errorf("staged binary did not report requested version %s: %s", plain, versionText)
		}
		if regexp.MustCompile(`(^|\s)(commit|date)=unknown(\s|$)`).MatchString(versionText) {
			return fmt.Errorf("staged binary must report non-placeholder commit and date: %s", versionText)
		}
	}

	const (
		parentRun         = "run-20260709T000000Z-wave"
		childRun          = "run-20260709T000001Z-child-0-self-bootstrap-alpha"
		childRunBeta      = "run-20260709T000001Z-child-1-self-bootstrap-beta"
		childRunGamma     = "run-20260709T000001Z-child-2-self-bootstrap-gamma"
		mutationParentRun = "run-20260709T000002Z-wave-read-only-mutation"
		mutationChildRun  = "run-20260709T000003Z-child-0-read-only-mutation"
		writeParentRun    = "run-20260709T000004Z-wave-bounded-write"
		writeChildRun     = "run-20260709T000005Z-child-0-bounded-write"
	)
	boundedWriteWorktree := ""
	defer func() {
		if boundedWriteWorktree != "" {
			_ = exec.Command("git", "-C", repoPath, "worktree", "remove", "--force", boundedWriteWorktree).Run()
		}
	}()

	registerOut, err := runChecked("register loopcoder checkout", binaryPath, nil, "projects", "register", "--repo", repoPath, "--format", "json")
	if err != nil {
		return err
	}
	var registered struct {
		Project struct {
			ProjectID   string `json:"project_id"`
			DisplayName string `json:"display_name"`
		} `json:"project"`
	}
	if err := decodeJSON("projects register", registerOut, &registered); err != nil {
		return err
	}
	if registered.Project.ProjectID == "" || registered.Project.DisplayName != "loopcoder" {
		return fmt.Errorf("project registry did not resolve the loopcoder checkout: %s", registerOut)
	}

	dbPath := filepath.Join(loopcoderHome, "data", "loopcoder.db")
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Errorf("project registration did not create %s", dbPath)
	}
	if err := assertOutsideRepo(dbPath, repoPath, "v0.8.0 database"); err != nil {
		return err
	}

	databaseHashBefore, err := fileSHA256(dbPath)
	if err != nil {
		return err
	}
	backupRoot := filepath.Join(loopcoderHome, "data", "backups")
	backupBefore, err := treeInventory(backupRoot)
	if err != nil {
		return err
	}
	storagePlanOut, err := runChecked("render read-only fresh-schema storage migration plan", binaryPath, nil, "migrate", "storage", "--format", "json")
	if err != nil {
		return err
	}
	_ = writeFile(filepath.Join(artifactDir, "storage-plan.json"), storagePlanOut)
	var storagePlan storagePlanPayload
	if err := decodeJSON("migrate storage", storagePlanOut, &storagePlan); err != nil {
		return err
	}
	if err := assertFreshSchemaCurrent(storagePlan); err != nil {
		return err
	}
	databaseHashAfter, err := fileSHA256(dbPath)
	if err != nil {
		return err
	}
	if databaseHashAfter != databaseHashBefore {
		return fmt.Errorf("read-only fresh-schema migration plan modified the database")
	}
	backupAfter, err := treeInventory(backupRoot)
	if err != nil {
		return err
	}
	if err := assertInventoryUnchanged(backupBefore, backupAfter, "fresh-schema backup inventory"); err != nil {
		return err
	}

	planPath := filepath.Join(artifactDir, "child-plan.json")
	childPlan := map[string]any{
		"schema_version":   "loopcoder.child_plan.v1",
		"plan_id":          "plan-" + parentRun + "-self-bootstrap",
		"parent_run_id":    parentRun,
		"root_run_id":      parentRun,
		"parent_depth":     0,
		"max_depth":        2,
		"max_concurrency":  2,
		"created_at":       "2026-07-09T00:00:00Z",
		"items": []map[string]any{
			childPlanItem("self-bootstrap-alpha", childRun, 654, []string{"scripts/self-bootstrap-smoke.sh"}, nil, true),
			childPlanItem("self-bootstrap-beta", childRunBeta, 655, []string{"docs/reference/self-bootstrap.md"}, nil, true),
			childPlanItem("self-bootstrap-gamma", childRunGamma, 656, []string{"docs/reference/usage.md"}, []string{"self-bootstrap-alpha", "self-bootstrap-beta"}, false),
		},
	}
	if err := writeJSONFile(planPath, childPlan); err != nil {
		return err
	}

	nestedOut, err := runChecked("execute nested child plan", binaryPath, nil,
		"nested", "run", "--repo", repoPath, "--plan", planPath, "--provider", "test-subprocess", "--format", "json")
	if err != nil {
		return err
	}
	_ = writeFile(filepath.Join(artifactDir, "nested-run.json"), nestedOut)
	var nested nestedResult
	if err := decodeJSON("nested run", nestedOut, &nested); err != nil {
		return err
	}
	if nested.Status != "succeeded" || len(nested.Children) != 3 {
		return fmt.Errorf("nested run did not execute the expected three child processes: %s", nestedOut)
	}
	alpha := findChild(nested.Children, childRun)
	if alpha == nil || alpha.Status != "succeeded" || alpha.AttemptPath == "" {
		return fmt.Errorf("nested run did not produce a successful durable alpha child attempt")
	}
	if err := assertOutsideRepo(alpha.AttemptPath, repoPath, "nested child attempt"); err != nil {
		return err
	}

	// Read-only mutation fixture
	mutationMarkerName := ".loopcoder-read-only-mutation-fixture"
	mutationMarkerPath := filepath.Join(repoPath, mutationMarkerName)
	mutationPlanPath := filepath.Join(artifactDir, "read-only-mutation-plan.json")
	mutationPlan := map[string]any{
		"schema_version":  "loopcoder.child_plan.v1",
		"plan_id":         "plan-" + mutationParentRun,
		"parent_run_id":   mutationParentRun,
		"root_run_id":     mutationParentRun,
		"parent_depth":    0,
		"max_depth":       2,
		"max_concurrency": 1,
		"created_at":      "2026-07-09T00:00:02Z",
		"items": []map[string]any{
			{
				"child_key":  "read-only-mutation",
				"title":      "read-only-mutation",
				"role":       "worker",
				"run_id":     mutationChildRun,
				"issue":      1006,
				"scope": map[string]any{
					"repo":     ".",
					"paths":    []string{"scripts/self-bootstrap-smoke.sh"},
					"issues":   []int{1006},
					"commands": []string{"printf mutation > " + mutationMarkerName},
				},
				"permission": "read-only",
				"depends_on": []string{},
				"aggregation": map[string]any{
					"mode":           "collect",
					"required":       true,
					"include_report": true,
				},
			},
		},
	}
	if err := writeJSONFile(mutationPlanPath, mutationPlan); err != nil {
		return err
	}
	fmt.Println("==> verify read-only mutation fixture fails closed")
	mutationOut, mutationCode, err := runCapture(binaryPath, nil, "nested", "run", "--repo", repoPath, "--plan", mutationPlanPath, "--provider", "test-subprocess", "--format", "json")
	if err != nil {
		return err
	}
	_ = writeFile(filepath.Join(artifactDir, "read-only-mutation-result.json"), mutationOut)
	if mutationCode == 0 {
		return fmt.Errorf("read-only mutation fixture unexpectedly succeeded")
	}
	var mutationResult nestedResult
	if err := decodeJSON("read-only mutation fixture", mutationOut, &mutationResult); err != nil {
		return err
	}
	if len(mutationResult.Children) < 1 {
		return fmt.Errorf("read-only mutation fixture produced no children")
	}
	mutationChild := mutationResult.Children[0]
	if mutationResult.Status != "needs-human" || mutationChild.Status != "needs-human" || mutationChild.Outcome != "read_only_policy_violation" {
		return fmt.Errorf("read-only mutation fixture did not produce the typed needs-human policy outcome")
	}
	if mutationChild.ReadOnlyEnforcement == nil || mutationChild.ReadOnlyEnforcement.Verification != "policy-violation" {
		return fmt.Errorf("read-only mutation fixture omitted policy-violation enforcement evidence")
	}
	if !hasViolationCode(mutationChild.ReadOnlyEnforcement.Violations, "untracked_file_created") {
		return fmt.Errorf("read-only mutation fixture omitted the untracked-file violation code")
	}
	if _, err := os.Stat(mutationMarkerPath); err != nil {
		return fmt.Errorf("read-only executor remediated the mutation instead of preserving evidence")
	}
	_ = os.Remove(mutationMarkerPath)

	// Bounded write
	writeTargetRelative := "docs/reference/self-bootstrap.md"
	writeTargetPath := filepath.Join(repoPath, writeTargetRelative)
	writeTargetHashBefore, err := fileSHA256(writeTargetPath)
	if err != nil {
		return err
	}
	worktreeCountBefore, err := countWorktrees(repoPath)
	if err != nil {
		return err
	}
	writePlanPath := filepath.Join(artifactDir, "bounded-write-plan.json")
	writePlan := map[string]any{
		"schema_version":  "loopcoder.child_plan.v1",
		"plan_id":         "plan-" + writeParentRun,
		"parent_run_id":   writeParentRun,
		"root_run_id":     writeParentRun,
		"parent_depth":    0,
		"max_depth":       2,
		"max_concurrency": 1,
		"created_at":      "2026-07-09T00:00:04Z",
		"items": []map[string]any{
			{
				"child_key":  "bounded-write",
				"title":      "bounded-write",
				"role":       "worker",
				"run_id":     writeChildRun,
				"issue":      1007,
				"scope": map[string]any{
					"repo":     ".",
					"paths":    []string{writeTargetRelative},
					"issues":   []int{1007},
					"commands": []string{"printf 'bounded write smoke\\n' >> " + writeTargetRelative},
				},
				"permission": "write",
				"depends_on": []string{},
				"aggregation": map[string]any{
					"mode":           "collect",
					"required":       true,
					"include_report": true,
				},
			},
		},
	}
	if err := writeJSONFile(writePlanPath, writePlan); err != nil {
		return err
	}
	writeOut, err := runChecked("execute packaged bounded-write child smoke", binaryPath, nil,
		"nested", "run", "--repo", repoPath, "--plan", writePlanPath, "--provider", "test-subprocess", "--base-branch", "main", "--format", "json")
	if err != nil {
		return err
	}
	_ = writeFile(filepath.Join(artifactDir, "bounded-write-result.json"), writeOut)
	var writeResult nestedResult
	if err := decodeJSON("bounded-write nested run", writeOut, &writeResult); err != nil {
		return err
	}
	if len(writeResult.Children) < 1 {
		return fmt.Errorf("bounded-write packaged smoke produced no children")
	}
	writeChild := writeResult.Children[0]
	if writeResult.Status != "succeeded" || writeChild.Status != "succeeded" {
		return fmt.Errorf("bounded-write packaged smoke did not succeed")
	}
	if writeChild.MutationManifest == nil || writeChild.MutationManifest.Verification != "passed" || !hasChangePath(writeChild.MutationManifest.Changes, writeTargetRelative) {
		return fmt.Errorf("bounded-write packaged smoke omitted the allowed mutation manifest")
	}
	if writeChild.WorktreePath == "" || writeChild.AttemptPath == "" {
		return fmt.Errorf("bounded-write packaged smoke omitted its preserved worktree or attempt path")
	}
	if err := assertOutsideRepo(writeChild.WorktreePath, repoPath, "bounded-write child worktree"); err != nil {
		return err
	}
	if err := assertOutsideRepo(writeChild.AttemptPath, repoPath, "bounded-write child attempt"); err != nil {
		return err
	}
	boundedWriteWorktree = writeChild.WorktreePath
	worktreeCountAfter, err := countWorktrees(repoPath)
	if err != nil || worktreeCountAfter != worktreeCountBefore+1 {
		return fmt.Errorf("bounded-write packaged smoke did not register exactly one isolated worktree")
	}
	writeTargetHashAfter, err := fileSHA256(writeTargetPath)
	if err != nil {
		return err
	}
	if writeTargetHashAfter != writeTargetHashBefore {
		return fmt.Errorf("bounded-write packaged smoke modified the parent checkout")
	}
	writeChildTarget := filepath.Join(writeChild.WorktreePath, writeTargetRelative)
	childHash, err := fileSHA256(writeChildTarget)
	if err != nil || childHash == writeTargetHashBefore {
		return fmt.Errorf("bounded-write packaged smoke did not preserve the child mutation")
	}
	if _, err := runChecked("remove packaged bounded-write smoke worktree", "git", nil, "-C", repoPath, "worktree", "remove", "--force", writeChild.WorktreePath); err != nil {
		return err
	}
	boundedWriteWorktree = ""
	worktreeCountCleanup, err := countWorktrees(repoPath)
	if err != nil || worktreeCountCleanup != worktreeCountBefore {
		return fmt.Errorf("bounded-write packaged smoke cleanup left a registered worktree")
	}

	// status / report / doctor
	statusText, err := runChecked("render status run tree (human)", binaryPath, nil, "status", "--repo", repoPath, "--run", childRun, "--format", "text")
	if err != nil {
		return err
	}
	_ = writeFile(filepath.Join(artifactDir, "status.txt"), statusText)
	if !strings.Contains(statusText, childRun) {
		return fmt.Errorf("status human output did not include selected child run %s", childRun)
	}
	statusJSON, err := runChecked("render status run tree (JSON)", binaryPath, nil, "status", "--repo", repoPath, "--run", childRun, "--format", "json")
	if err != nil {
		return err
	}
	_ = writeFile(filepath.Join(artifactDir, "status.json"), statusJSON)
	var statusPayload struct {
		RunTree struct {
			RootRunID     string `json:"root_run_id"`
			SelectedRunID string `json:"selected_run_id"`
			Summary       struct {
				RunCount int `json:"run_count"`
			} `json:"summary"`
			Nodes []struct {
				RunID       string `json:"run_id"`
				ParentRunID string `json:"parent_run_id"`
				Issue       int    `json:"issue"`
				Role        string `json:"role"`
			} `json:"nodes"`
		} `json:"run_tree"`
	}
	if err := decodeJSON("status", statusJSON, &statusPayload); err != nil {
		return err
	}
	if statusPayload.RunTree.RootRunID != parentRun || statusPayload.RunTree.SelectedRunID != childRun || statusPayload.RunTree.Summary.RunCount != 4 {
		return fmt.Errorf("status JSON did not expose the expected parent/child run tree")
	}
	var childNodeOK bool
	for _, n := range statusPayload.RunTree.Nodes {
		if n.RunID == childRun && n.ParentRunID == parentRun && n.Issue == 654 && n.Role == "worker" {
			childNodeOK = true
			break
		}
	}
	if !childNodeOK {
		return fmt.Errorf("status JSON child node is missing issue/report metadata")
	}

	reportText, err := runChecked("render report run tree (human)", binaryPath, nil, "report", "--repo", repoPath, "--run", childRun, "--format", "text")
	if err != nil {
		return err
	}
	_ = writeFile(filepath.Join(artifactDir, "report.txt"), reportText)
	if !strings.Contains(reportText, childRun) {
		return fmt.Errorf("report human output did not include selected child run %s", childRun)
	}
	reportJSON, err := runChecked("render report run tree (JSON)", binaryPath, nil, "report", "--repo", repoPath, "--run", childRun, "--format", "json")
	if err != nil {
		return err
	}
	_ = writeFile(filepath.Join(artifactDir, "report.json"), reportJSON)
	var reportPayload struct {
		RunTree struct {
			RootRunID string `json:"root_run_id"`
			Nodes     []any  `json:"nodes"`
		} `json:"run_tree"`
		Records []struct {
			RunID  string `json:"run_id"`
			Report struct {
				WorkID string `json:"work_id"`
				Role   string `json:"role"`
			} `json:"report"`
		} `json:"records"`
	}
	if err := decodeJSON("report", reportJSON, &reportPayload); err != nil {
		return err
	}
	if reportPayload.RunTree.RootRunID != parentRun || len(reportPayload.RunTree.Nodes) != 4 {
		return fmt.Errorf("report JSON did not include the parent/child run tree")
	}
	foundWorker := false
	for _, rec := range reportPayload.Records {
		if (rec.RunID == childRun || rec.Report.WorkID == childRun) && rec.Report.Role == "worker" {
			foundWorker = true
			break
		}
	}
	if !foundWorker {
		return fmt.Errorf("report JSON did not include the child worker report record")
	}

	doctorOut, doctorCode, err := runCapture(binaryPath, nil, "doctor", "--repo", repoPath, "--format", "json")
	if err != nil {
		return err
	}
	_ = writeFile(filepath.Join(artifactDir, "doctor.json"), doctorOut)
	var doctor struct {
		Runtime struct {
			Database struct {
				Path   string `json:"path"`
				Exists bool   `json:"exists"`
				Status string `json:"status"`
			} `json:"database"`
			ProjectRegistry struct {
				Registered   bool   `json:"registered"`
				ProjectID    string `json:"project_id"`
				PayloadRoot  string `json:"payload_root"`
				RunsRoot     string `json:"runs_root"`
				RelayRoot    string `json:"relay_root"`
				RecoveryRoot string `json:"recovery_root"`
				AuditRoot    string `json:"audit_root"`
				LogsRoot     string `json:"logs_root"`
				TmpRoot      string `json:"tmp_root"`
			} `json:"project_registry"`
			NestedRuns struct {
				ParentEdges int    `json:"parent_edges"`
				ChildEdges  int    `json:"child_edges"`
				Status      string `json:"status"`
			} `json:"nested_runs"`
		} `json:"runtime"`
		ProviderCompatibility []struct {
			Provider string `json:"provider"`
			Role     string `json:"role"`
		} `json:"provider_compatibility"`
	}
	if err := decodeJSON("doctor", doctorOut, &doctor); err != nil {
		return err
	}
	dbPathNorm := filepath.ToSlash(dbPath)
	if doctor.Runtime.Database.Path != dbPathNorm && doctor.Runtime.Database.Path != dbPath {
		return fmt.Errorf("doctor JSON reported unexpected database path: %s", doctor.Runtime.Database.Path)
	}
	if !doctor.Runtime.Database.Exists || doctor.Runtime.Database.Status != "ok" {
		return fmt.Errorf("doctor JSON did not report healthy storage")
	}
	if !doctor.Runtime.ProjectRegistry.Registered || doctor.Runtime.ProjectRegistry.ProjectID != registered.Project.ProjectID {
		return fmt.Errorf("doctor JSON did not report the registered loopcoder project")
	}
	for _, runtimePath := range []string{
		doctor.Runtime.ProjectRegistry.PayloadRoot,
		doctor.Runtime.ProjectRegistry.RunsRoot,
		doctor.Runtime.ProjectRegistry.RelayRoot,
		doctor.Runtime.ProjectRegistry.RecoveryRoot,
		doctor.Runtime.ProjectRegistry.AuditRoot,
		doctor.Runtime.ProjectRegistry.LogsRoot,
		doctor.Runtime.ProjectRegistry.TmpRoot,
	} {
		if strings.TrimSpace(runtimePath) == "" {
			return fmt.Errorf("doctor JSON omitted a registered runtime payload path")
		}
		if err := assertOutsideRepo(runtimePath, repoPath, "doctor runtime payload path"); err != nil {
			return err
		}
	}
	if doctor.Runtime.NestedRuns.ParentEdges < 1 || doctor.Runtime.NestedRuns.ChildEdges < 1 || doctor.Runtime.NestedRuns.Status != "ok" {
		return fmt.Errorf("doctor JSON did not report healthy nested run edges")
	}
	codexWorker := false
	for _, pc := range doctor.ProviderCompatibility {
		if pc.Provider == "codex" && pc.Role == "worker" {
			codexWorker = true
			break
		}
	}
	if !codexWorker {
		return fmt.Errorf("doctor JSON did not expose provider compatibility for the codex worker")
	}
	if doctorCode != 0 {
		fmt.Printf("doctor exited %d because external readiness checks may fail without gh/provider auth; runtime self-bootstrap assertions passed.\n", doctorCode)
	}

	repoRuntimeAfter, err := repoRuntimeInventory(repoPath)
	if err != nil {
		return err
	}
	if err := assertInventoryUnchanged(repoRuntimeBefore, repoRuntimeAfter, "repository-local runtime payload inventory"); err != nil {
		return err
	}

	evidence := map[string]any{
		"schema_version":     "loopcoder.self_bootstrap_evidence.v1",
		"requested_version":  plain,
		"staged_candidate":   usingStaged,
		"host_tuple":         HostTuple(),
		"binary": map[string]any{
			"path":           binaryPath,
			"sha256":         binaryHash,
			"version_output": versionText,
		},
		"provider":                         "test-subprocess",
		"paid_provider_calls":              0,
		"project_id":                       registered.Project.ProjectID,
		"database_path":                    dbPath,
		"database_outside_repo":            true,
		"registered_payload_root":          doctor.Runtime.ProjectRegistry.PayloadRoot,
		"registered_payload_outside_repo":  true,
		"repository_runtime_unchanged":     true,
		"storage_plan": map[string]any{
			"path":                  filepath.Join(artifactDir, "storage-plan.json"),
			"source_schema_version": storagePlan.Plan.SourceSchemaVersion,
			"target_schema_version": storagePlan.Plan.TargetSchemaVersion,
			"status":                storagePlan.Plan.Status,
			"database_unchanged":    true,
			"backup_created":        false,
		},
		"runs": map[string]any{
			"parent":                 parentRun,
			"children":               []string{childRun, childRunBeta, childRunGamma},
			"status":                 nested.Status,
			"mutation_fixture_child": mutationChildRun,
			"mutation_fixture_status": mutationChild.Status,
			"bounded_write_child":    writeChildRun,
			"bounded_write_status":   writeChild.Status,
			"bounded_write_manifest": writeChild.MutationManifest.ManifestFingerprint,
		},
		"artifacts": map[string]any{
			"status_human":       filepath.Join(artifactDir, "status.txt"),
			"status_json":        filepath.Join(artifactDir, "status.json"),
			"report_human":       filepath.Join(artifactDir, "report.txt"),
			"report_json":        filepath.Join(artifactDir, "report.json"),
			"doctor_json":        filepath.Join(artifactDir, "doctor.json"),
			"bounded_write_json": filepath.Join(artifactDir, "bounded-write-result.json"),
		},
	}
	evidenceBytes, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return err
	}
	_ = writeFile(filepath.Join(artifactDir, "self-bootstrap-evidence.json"), string(evidenceBytes)+"\n")
	human := []string{
		fmt.Sprintf("self-bootstrap smoke passed for v%s", plain),
		"host: " + HostTuple(),
		"candidate: " + binaryPath,
		"candidate_sha256: " + binaryHash,
		"provider: test-subprocess (paid calls: 0)",
		"repo: " + repoPath,
		"loopcoder_home: " + loopcoderHome,
		"database: " + dbPath,
		"project_id: " + registered.Project.ProjectID,
		"parent_run: " + parentRun,
		fmt.Sprintf("child_runs: %s, %s, %s", childRun, childRunBeta, childRunGamma),
		"bounded_write_child: " + writeChildRun,
		"bounded_write_manifest: " + writeChild.MutationManifest.ManifestFingerprint,
		"bounded_write_parent_unchanged: true",
		"repository_runtime_unchanged: true",
		"artifacts: " + artifactDir,
	}
	_ = writeFile(filepath.Join(artifactDir, "self-bootstrap-evidence.txt"), strings.Join(human, "\n")+"\n")
	for _, line := range human {
		fmt.Println(line)
	}
	fmt.Println("self-bootstrap evidence JSON:")
	fmt.Println(string(evidenceBytes))
	if *keep {
		fmt.Println("retained artifacts:", artifactDir)
	}
	return nil
}

type nestedResult struct {
	Status   string         `json:"status"`
	Children []nestedChild  `json:"children"`
}

type nestedChild struct {
	RunID               string `json:"run_id"`
	Status              string `json:"status"`
	Outcome             string `json:"outcome"`
	AttemptPath         string `json:"attempt_path"`
	WorktreePath        string `json:"worktree_path"`
	ReadOnlyEnforcement *struct {
		Verification string `json:"verification"`
		Violations   []struct {
			Code string `json:"code"`
		} `json:"violations"`
	} `json:"read_only_enforcement"`
	MutationManifest *struct {
		Verification         string `json:"verification"`
		ManifestFingerprint  string `json:"manifest_fingerprint"`
		Changes              []struct {
			Path string `json:"path"`
		} `json:"changes"`
	} `json:"mutation_manifest"`
}

func childPlanItem(key, runID string, issue int, paths, dependsOn []string, required bool) map[string]any {
	if dependsOn == nil {
		dependsOn = []string{}
	}
	return map[string]any{
		"child_key": key,
		"title":     key,
		"role":      "worker",
		"run_id":    runID,
		"issue":     issue,
		"scope": map[string]any{
			"repo":     ".",
			"paths":    paths,
			"issues":   []int{issue},
			"commands": []string{"git status --short"},
		},
		"permission": "read-only",
		"depends_on": dependsOn,
		"aggregation": map[string]any{
			"mode":           "collect",
			"required":       required,
			"include_report": true,
		},
	}
}

func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, string(b)+"\n")
}

func findChild(children []nestedChild, runID string) *nestedChild {
	for i := range children {
		if children[i].RunID == runID {
			return &children[i]
		}
	}
	return nil
}

func hasViolationCode(violations []struct {
	Code string `json:"code"`
}, code string) bool {
	for _, v := range violations {
		if v.Code == code {
			return true
		}
	}
	return false
}

func hasChangePath(changes []struct {
	Path string `json:"path"`
}, path string) bool {
	for _, c := range changes {
		if c.Path == path {
			return true
		}
	}
	return false
}

func countWorktrees(repoPath string) (int, error) {
	out, err := exec.Command("git", "-C", repoPath, "worktree", "list", "--porcelain").CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("git worktree list: %w\n%s", err, out)
	}
	count := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "worktree ") {
			count++
		}
	}
	return count, nil
}
