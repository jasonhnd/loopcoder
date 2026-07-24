package workflowrun

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/home"
)

// ResolveDurableHome returns the process-independent durable root for event
// logs and claim stores. Order:
//  1. explicit homeDir (tests inject t.TempDir())
//  2. LOOPCODER_HOME via home.ResolveHomeDir
//  3. ~/.loopcoder via home.ResolveHomeDir
//
// Never returns a PID-scoped temp path — restart must see the same durable root.
func ResolveDurableHome(explicit string) (string, error) {
	if e := strings.TrimSpace(explicit); e != "" {
		return filepath.Clean(e), nil
	}
	dir, err := home.ResolveHomeDir(home.DefaultDeps())
	if err != nil {
		return "", fmt.Errorf("resolve durable home: %w", err)
	}
	if strings.TrimSpace(dir) == "" {
		return "", fmt.Errorf("resolve durable home: empty path")
	}
	return filepath.Clean(dir), nil
}

// RunDurableDir is <home>/projects/<project>/runs/<runID>.
func RunDurableDir(homeDir, projectID, runID string) (string, error) {
	root, err := ResolveDurableHome(homeDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "projects", strings.TrimSpace(projectID), "runs", sanitizeBranch(runID)), nil
}

// eventLogPathIfExists returns the workflow-events.jsonl path when the file
// already exists, or ("", nil) when absent. Never creates directories/files.
func eventLogPathIfExists(homeDir, projectID, runID string) (string, error) {
	dir, err := RunDurableDir(homeDir, projectID, runID)
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, "workflow-events.jsonl")
	if _, err := os.Stat(p); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return p, nil
}
