package workflowrun

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
)

const maxTestValidationReceiptBytes = 64 << 10

const (
	// TestValidationSchema is the durable, redacted production-owned receipt
	// written after a tests provider succeeds and before the child may integrate.
	TestValidationSchema = "loopcoder.test_validation.v1"

	testValidationStatusPassed = "passed"
	testValidationStatusFailed = "failed"

	defaultGoTestValidationHardCap = 10 * time.Minute
)

// TestValidationReceipt proves that LoopCoder, rather than provider prose,
// executed the fixed Go module and test contract in the exact child worktree.
// It contains no command output, prompt, credentials, or environment.
type TestValidationReceipt struct {
	Schema                  string                         `json:"schema"`
	ProjectID               string                         `json:"project_id"`
	RunID                   string                         `json:"run_id"`
	WorkItemID              string                         `json:"work_item_id"`
	AttemptID               string                         `json:"attempt_id"`
	HeadSHA                 string                         `json:"head_sha"`
	WorktreePathDigest      string                         `json:"worktree_path_digest"`
	Commands                [][]string                     `json:"commands"`
	CommandResults          []TestValidationCommandReceipt `json:"command_results"`
	CommandDigest           string                         `json:"command_digest"`
	Status                  string                         `json:"status"`
	FailureClass            string                         `json:"failure_class,omitempty"`
	ExitCode                int                            `json:"exit_code"`
	ProcessOutcome          string                         `json:"process_outcome"`
	StartedAt               time.Time                      `json:"started_at"`
	CompletedAt             time.Time                      `json:"completed_at"`
	DurationMS              int64                          `json:"duration_ms"`
	OutputDigest            string                         `json:"output_digest"`
	ProductDigestBefore     string                         `json:"product_digest_before"`
	ProductDigestAfter      string                         `json:"product_digest_after"`
	GeneratedDependencyFile bool                           `json:"generated_dependency_file"`
	ContentDigest           string                         `json:"content_digest"`
}

// TestValidationCommandReceipt is one fixed direct-argv command result. Output
// is represented only by a digest.
type TestValidationCommandReceipt struct {
	Command        []string `json:"command"`
	CommandDigest  string   `json:"command_digest"`
	ExitCode       int      `json:"exit_code"`
	ProcessOutcome string   `json:"process_outcome"`
	DurationMS     int64    `json:"duration_ms"`
	OutputDigest   string   `json:"output_digest"`
}

type testValidationResult struct {
	Applicable  bool
	Receipt     TestValidationReceipt
	ReceiptPath string
}

// lockedDigestWriter hashes unbounded command output without retaining it.
// Stdout and stderr may write concurrently.
type lockedDigestWriter struct {
	mu sync.Mutex
	h  hash.Hash
}

func newLockedDigestWriter() *lockedDigestWriter {
	return &lockedDigestWriter{h: sha256.New()}
}

func (w *lockedDigestWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.h.Write(p)
}

func (w *lockedDigestWriter) Digest() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return "sha256:" + hex.EncodeToString(w.h.Sum(nil))
}

func runGoModuleTestValidation(
	ctx context.Context,
	in ChildExecInput,
	worktree string,
	controlPlaneLogPath string,
	productDigestBefore string,
	hardCap time.Duration,
	now func() time.Time,
) (testValidationResult, error) {
	goModPath := filepath.Join(worktree, "go.mod")
	st, err := os.Lstat(goModPath)
	if err != nil {
		if os.IsNotExist(err) {
			return testValidationResult{}, nil
		}
		return testValidationResult{Applicable: true}, fmt.Errorf("workflowrun: inspect Go test contract: %w", err)
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return testValidationResult{Applicable: true}, fmt.Errorf("workflowrun: Go test contract requires regular non-symlink go.mod")
	}
	if now == nil {
		now = time.Now
	}
	if hardCap <= 0 {
		hardCap = defaultGoTestValidationHardCap
	}
	head, err := runGitRepo(ctx, worktree, "rev-parse", "HEAD")
	if err != nil || !isExactGitOID(head) {
		return testValidationResult{Applicable: true}, fmt.Errorf("workflowrun: Go test contract exact HEAD: %w", firstError(err, errors.New("invalid git HEAD")))
	}
	commands := [][]string{
		{"go", "mod", "tidy"},
		{"go", "test", "./..."},
	}
	commandSetDigest, err := canonicalJSONDigest(commands)
	if err != nil {
		return testValidationResult{Applicable: true}, fmt.Errorf("workflowrun: Go test contract command digest: %w", err)
	}
	worktreeSum := sha256.Sum256([]byte(filepath.Clean(worktree)))

	started := now().UTC()
	status := testValidationStatusPassed
	failureClass := ""
	processOutcome := "completed"
	exitCode := 0
	var duration time.Duration
	commandResults := make([]TestValidationCommandReceipt, 0, len(commands))
	if ctx == nil {
		ctx = context.Background()
	}
	runtimeRoot, err := os.MkdirTemp(filepath.Dir(controlPlaneLogPath), ".test-validation-runtime-")
	if err != nil {
		return testValidationResult{Applicable: true}, fmt.Errorf("workflowrun: create isolated Go test runtime: %w", err)
	}
	defer os.RemoveAll(runtimeRoot)
	commandEnv, err := isolatedGoTestEnvironment(runtimeRoot)
	if err != nil {
		return testValidationResult{Applicable: true}, fmt.Errorf("workflowrun: prepare isolated Go test runtime: %w", err)
	}
	gateCtx, cancel := context.WithTimeout(ctx, hardCap)
	defer cancel()
	for _, command := range commands {
		output := newLockedDigestWriter()
		cmd := exec.Command(command[0], command[1:]...)
		cmd.Dir = worktree
		cmd.Stdout = output
		cmd.Stderr = output
		cmd.Env = commandEnv
		run, runErr := supervisedexec.Run(gateCtx, cmd, supervisedexec.Options{
			HardCap:      hardCap,
			WorktreePath: worktree,
			RunID:        in.AttemptID,
			Role:         "workflow-test-validation",
		})
		duration += run.Elapsed
		commandOutcome := "completed"
		commandExit := run.ExitCode
		switch {
		case errors.Is(gateCtx.Err(), context.DeadlineExceeded) ||
			errors.Is(runErr, context.DeadlineExceeded) ||
			run.Outcome == supervisedexec.OutcomeDeadline:
			status = testValidationStatusFailed
			failureClass = "test_validation_timeout"
			processOutcome = "deadline"
			commandOutcome = "deadline"
			commandExit = -1
		case errors.Is(runErr, context.Canceled):
			status = testValidationStatusFailed
			failureClass = "test_validation_cancelled"
			processOutcome = "cancelled"
			commandOutcome = "cancelled"
			commandExit = -1
		case runErr != nil:
			status = testValidationStatusFailed
			failureClass = "test_validation_unavailable"
			processOutcome = "start_error"
			commandOutcome = "start_error"
			commandExit = -1
		case run.Outcome == supervisedexec.OutcomeStalled:
			status = testValidationStatusFailed
			failureClass = "test_validation_stalled"
			processOutcome = "stalled"
			commandOutcome = "stalled"
			commandExit = -1
		case run.ExitCode != 0:
			status = testValidationStatusFailed
			failureClass = "test_validation_failed"
		}
		commandResultDigest, digestErr := canonicalJSONDigest(command)
		if digestErr != nil {
			return testValidationResult{Applicable: true}, fmt.Errorf("workflowrun: Go test command result digest: %w", digestErr)
		}
		commandResults = append(commandResults, TestValidationCommandReceipt{
			Command: command, CommandDigest: commandResultDigest,
			ExitCode: commandExit, ProcessOutcome: commandOutcome,
			DurationMS: run.Elapsed.Milliseconds(), OutputDigest: output.Digest(),
		})
		exitCode = commandExit
		if status != testValidationStatusPassed {
			break
		}
	}
	completed := now().UTC()
	resultDigest, err := canonicalJSONDigest(commandResults)
	if err != nil {
		return testValidationResult{Applicable: true}, fmt.Errorf("workflowrun: Go test results digest: %w", err)
	}

	productDigestAfter, productFilesAfter, digestErr := productOutputDigest(worktree)
	if digestErr != nil {
		status = testValidationStatusFailed
		failureClass = FailureClassProductDigest
	}
	generatedDependency := false
	for _, rel := range productFilesAfter {
		if filepath.ToSlash(rel) == "go.sum" {
			generatedDependency = true
			break
		}
	}
	receipt := TestValidationReceipt{
		Schema:    TestValidationSchema,
		ProjectID: in.ProjectID, RunID: in.RunID,
		WorkItemID: in.WorkItemID, AttemptID: in.AttemptID,
		HeadSHA:            head,
		WorktreePathDigest: "sha256:" + hex.EncodeToString(worktreeSum[:]),
		Commands:           commands, CommandResults: commandResults,
		CommandDigest: commandSetDigest,
		Status:        status, FailureClass: failureClass, ExitCode: exitCode,
		ProcessOutcome: processOutcome,
		StartedAt:      started, CompletedAt: completed,
		DurationMS:              duration.Milliseconds(),
		OutputDigest:            resultDigest,
		ProductDigestBefore:     productDigestBefore,
		ProductDigestAfter:      productDigestAfter,
		GeneratedDependencyFile: generatedDependency,
	}
	contentDigest, err := testValidationReceiptDigest(receipt)
	if err != nil {
		return testValidationResult{Applicable: true, Receipt: receipt}, fmt.Errorf("workflowrun: Go test receipt digest: %w", err)
	}
	receipt.ContentDigest = contentDigest
	receiptPath, err := writeTestValidationReceipt(controlPlaneLogPath, receipt)
	if err != nil {
		return testValidationResult{Applicable: true, Receipt: receipt}, fmt.Errorf("workflowrun: Go test receipt persist: %w", err)
	}
	result := testValidationResult{Applicable: true, Receipt: receipt, ReceiptPath: receiptPath}
	if digestErr != nil {
		return result, fmt.Errorf("workflowrun: Go test post-validation product digest: %w", digestErr)
	}
	if status != testValidationStatusPassed {
		return result, fmt.Errorf(
			"workflowrun: Go test validation %s (exit=%d evidence=%s)",
			failureClass, exitCode, receipt.ContentDigest,
		)
	}
	return result, nil
}

func isolatedGoTestEnvironment(root string) ([]string, error) {
	dirs := map[string]string{
		"HOME":        filepath.Join(root, "home"),
		"USERPROFILE": filepath.Join(root, "home"),
		"TMPDIR":      filepath.Join(root, "tmp"),
		"TMP":         filepath.Join(root, "tmp"),
		"TEMP":        filepath.Join(root, "tmp"),
		"GOCACHE":     filepath.Join(root, "go-build"),
		"GOPATH":      filepath.Join(root, "gopath"),
		"GOMODCACHE":  filepath.Join(root, "gopath", "pkg", "mod"),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	env := make([]string, 0, len(dirs)+10)
	for key, value := range dirs {
		env = append(env, key+"="+value)
	}
	for _, key := range []string{
		"PATH", "LANG", "LC_ALL", "SystemRoot", "ComSpec", "PATHEXT",
		"SSL_CERT_FILE", "SSL_CERT_DIR",
	} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return append(env,
		"GOENV=off",
		"GOFLAGS=-modcacherw",
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=never",
	), nil
}

func testValidationReceiptDigest(receipt TestValidationReceipt) (string, error) {
	receipt.ContentDigest = ""
	raw, err := json.Marshal(receipt)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func canonicalJSONDigest(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func writeTestValidationReceipt(controlPlaneLogPath string, receipt TestValidationReceipt) (string, error) {
	dir := filepath.Dir(controlPlaneLogPath)
	if err := requireNonSymlinkDir(dir); err != nil {
		return "", err
	}
	raw, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".test-validation-*.tmp")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := requireRegularNonSymlinkFile(tmpPath); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, "test-validation.json")
	if err := os.Rename(tmpPath, dest); err != nil {
		return "", err
	}
	cleanup = false
	if err := requireRegularNonSymlinkFile(dest); err != nil {
		return "", err
	}
	return dest, nil
}

func testValidationPayloadFields(outcome *ChildOutcome) map[string]string {
	if outcome == nil {
		return nil
	}
	return map[string]string{
		"test_validation_status":         outcome.TestValidationStatus,
		"test_validation_evidence":       outcome.TestValidationEvidence,
		"test_validation_command_digest": outcome.TestValidationCommandDigest,
		"test_validation_head_sha":       outcome.TestValidationHeadSHA,
		"test_validation_receipt_path":   outcome.TestValidationReceiptPath,
	}
}

func validatePassedGoTestReceipt(
	ctx context.Context,
	homeDir, projectID, runID, workItemID, attemptID, worktree string,
	out ChildExecResult,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	goModPath := filepath.Join(worktree, "go.mod")
	st, err := os.Lstat(goModPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect go.mod: %w", err)
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return fmt.Errorf("go.mod must be a regular non-symlink file")
	}
	if out.TestValidationStatus != testValidationStatusPassed ||
		!isExactSHA256Digest(out.TestValidationEvidence) ||
		!isExactSHA256Digest(out.TestValidationCommandDigest) ||
		!isExactGitOID(out.TestValidationHeadSHA) ||
		out.TestValidationReceiptPath == "" {
		return fmt.Errorf("go tests child missing exact passed validation binding")
	}
	logPath, err := providerControlPlaneLogPath(homeDir, projectID, runID, attemptID)
	if err != nil {
		return fmt.Errorf("resolve expected validation receipt: %w", err)
	}
	expectedPath := filepath.Join(filepath.Dir(logPath), "test-validation.json")
	if filepath.Clean(out.TestValidationReceiptPath) != filepath.Clean(expectedPath) {
		return fmt.Errorf("validation receipt path mismatch")
	}
	receipt, err := readTestValidationReceipt(expectedPath)
	if err != nil {
		return err
	}
	if receipt.ProjectID != projectID || receipt.RunID != runID ||
		receipt.WorkItemID != workItemID || receipt.AttemptID != attemptID {
		return fmt.Errorf("validation receipt identity mismatch")
	}
	worktreeSum := sha256.Sum256([]byte(filepath.Clean(worktree)))
	if receipt.WorktreePathDigest != "sha256:"+hex.EncodeToString(worktreeSum[:]) {
		return fmt.Errorf("validation receipt worktree binding mismatch")
	}
	head, err := runGitRepo(ctx, worktree, "rev-parse", "HEAD")
	if err != nil || head != receipt.HeadSHA {
		return fmt.Errorf("validation receipt HEAD binding mismatch")
	}
	if receipt.Status != testValidationStatusPassed || receipt.FailureClass != "" ||
		receipt.ExitCode != 0 || receipt.ProcessOutcome != "completed" {
		return fmt.Errorf("validation receipt is not an exact pass")
	}
	if len(receipt.Commands) != 2 ||
		len(receipt.Commands[0]) != 3 ||
		receipt.Commands[0][0] != "go" ||
		receipt.Commands[0][1] != "mod" ||
		receipt.Commands[0][2] != "tidy" ||
		len(receipt.Commands[1]) != 3 ||
		receipt.Commands[1][0] != "go" ||
		receipt.Commands[1][1] != "test" ||
		receipt.Commands[1][2] != "./..." ||
		len(receipt.CommandResults) != 2 {
		return fmt.Errorf("validation receipt command mismatch")
	}
	for i, commandResult := range receipt.CommandResults {
		if commandResult.ExitCode != 0 ||
			commandResult.ProcessOutcome != "completed" ||
			!isExactSHA256Digest(commandResult.CommandDigest) ||
			!isExactSHA256Digest(commandResult.OutputDigest) ||
			commandResult.DurationMS < 0 ||
			len(commandResult.Command) != len(receipt.Commands[i]) {
			return fmt.Errorf("validation receipt command result mismatch")
		}
		for j := range commandResult.Command {
			if commandResult.Command[j] != receipt.Commands[i][j] {
				return fmt.Errorf("validation receipt command result identity mismatch")
			}
		}
		digest, err := canonicalJSONDigest(commandResult.Command)
		if err != nil || digest != commandResult.CommandDigest {
			return fmt.Errorf("validation receipt command result digest mismatch")
		}
	}
	commandDigest, err := canonicalJSONDigest(receipt.Commands)
	if err != nil || commandDigest != receipt.CommandDigest {
		return fmt.Errorf("validation receipt command digest mismatch")
	}
	resultsDigest, err := canonicalJSONDigest(receipt.CommandResults)
	if err != nil || resultsDigest != receipt.OutputDigest {
		return fmt.Errorf("validation receipt results digest mismatch")
	}
	if receipt.CommandDigest != out.TestValidationCommandDigest ||
		receipt.ContentDigest != out.TestValidationEvidence ||
		receipt.HeadSHA != out.TestValidationHeadSHA ||
		receipt.ProductDigestAfter != out.OutputEvidence {
		return fmt.Errorf("validation receipt evidence binding mismatch")
	}
	if !isExactSHA256Digest(receipt.OutputDigest) ||
		!isExactSHA256Digest(receipt.ProductDigestBefore) ||
		!isExactSHA256Digest(receipt.ProductDigestAfter) ||
		receipt.CompletedAt.Before(receipt.StartedAt) ||
		receipt.DurationMS < 0 {
		return fmt.Errorf("validation receipt evidence is malformed")
	}
	digest, err := testValidationReceiptDigest(receipt)
	if err != nil {
		return err
	}
	if digest != receipt.ContentDigest {
		return fmt.Errorf("validation receipt content digest mismatch")
	}
	return nil
}

func readTestValidationReceipt(path string) (TestValidationReceipt, error) {
	if err := requireRegularNonSymlinkFile(path); err != nil {
		return TestValidationReceipt{}, fmt.Errorf("validation receipt: %w", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return TestValidationReceipt{}, err
	}
	defer f.Close()
	raw, err := ioReadAllLimit(f, maxTestValidationReceiptBytes)
	if err != nil {
		return TestValidationReceipt{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var receipt TestValidationReceipt
	if err := dec.Decode(&receipt); err != nil {
		return TestValidationReceipt{}, fmt.Errorf("validation receipt JSON: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return TestValidationReceipt{}, fmt.Errorf("validation receipt has trailing JSON")
	}
	if receipt.Schema != TestValidationSchema {
		return TestValidationReceipt{}, fmt.Errorf("validation receipt schema mismatch")
	}
	return receipt, nil
}

func ioReadAllLimit(f *os.File, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("validation receipt exceeds %d bytes", limit)
	}
	return raw, nil
}

func firstError(primary, fallback error) error {
	if primary != nil {
		return primary
	}
	return fallback
}
