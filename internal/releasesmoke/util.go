package releasesmoke

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func assertOutsideRepo(path, repoPath, label string) error {
	fullPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("%s: resolve path: %w", label, err)
	}
	fullRepo, err := filepath.Abs(repoPath)
	if err != nil {
		return fmt.Errorf("%s: resolve repo: %w", label, err)
	}
	fullRepo = strings.TrimRight(fullRepo, string(filepath.Separator))
	if fullPath == fullRepo || strings.HasPrefix(fullPath, fullRepo+string(filepath.Separator)) {
		return fmt.Errorf("%s must live outside the repository: %s is under %s", label, fullPath, fullRepo)
	}
	return nil
}

func runCapture(bin string, env []string, args ...string) (stdout string, exitCode int, err error) {
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	out, runErr := cmd.CombinedOutput()
	stdout = string(out)
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			return stdout, ee.ExitCode(), nil
		}
		return stdout, -1, runErr
	}
	return stdout, 0, nil
}

func runChecked(label, bin string, env []string, args ...string) (string, error) {
	stdout, code, err := runCapture(bin, env, args...)
	if err != nil {
		return stdout, fmt.Errorf("%s: %w\n%s", label, err, stdout)
	}
	if code != 0 {
		return stdout, fmt.Errorf("%s failed with exit code %d\n%s", label, code, stdout)
	}
	return stdout, nil
}

func decodeJSON(label, text string, dest any) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("%s produced empty JSON output", label)
	}
	if err := json.Unmarshal([]byte(text), dest); err != nil {
		return fmt.Errorf("%s produced invalid JSON: %w\n%s", label, err, text)
	}
	return nil
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func withEnv(extra map[string]string, fn func() error) error {
	previous := make(map[string]*string, len(extra))
	for key, value := range extra {
		if cur, ok := os.LookupEnv(key); ok {
			c := cur
			previous[key] = &c
		} else {
			previous[key] = nil
		}
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	defer func() {
		for key, prev := range previous {
			if prev == nil {
				_ = os.Unsetenv(key)
				continue
			}
			_ = os.Setenv(key, *prev)
		}
	}()
	return fn()
}

func processEnvWith(overrides map[string]string) []string {
	base := os.Environ()
	if len(overrides) == 0 {
		return base
	}
	index := make(map[string]int, len(base))
	for i, kv := range base {
		if eq := strings.IndexByte(kv, '='); eq > 0 {
			index[kv[:eq]] = i
		}
	}
	out := append([]string(nil), base...)
	for key, value := range overrides {
		entry := key + "=" + value
		if i, ok := index[key]; ok {
			out[i] = entry
			continue
		}
		out = append(out, entry)
	}
	return out
}

func treeInventory(root string) ([]string, error) {
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return []string{"<absent>"}, nil
		}
		return nil, err
	}
	entries := []string{".|directory"}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			entries = append(entries, rel+"|directory")
			return nil
		}
		sum, err := fileSHA256(path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		entries = append(entries, fmt.Sprintf("%s|file|%d|%s", rel, info.Size(), sum))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func repoRuntimeInventory(repoPath string) ([]string, error) {
	var entries []string
	for _, relativeRoot := range []string{
		".loopcoder/runs",
		".loopcoder/logs",
		".loopcoder/recovery",
		".loopcoder/relay",
	} {
		inv, err := treeInventory(filepath.Join(repoPath, relativeRoot))
		if err != nil {
			return nil, err
		}
		for _, entry := range inv {
			entries = append(entries, relativeRoot+"|"+entry)
		}
	}
	return entries, nil
}

func assertInventoryUnchanged(before, after []string, label string) error {
	if len(before) != len(after) {
		return fmt.Errorf("%s changed unexpectedly: len before=%d after=%d", label, len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			return fmt.Errorf("%s changed unexpectedly at index %d: %q -> %q", label, i, before[i], after[i])
		}
	}
	return nil
}

func fileModeOctal(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%o", info.Mode().Perm()), nil
}

func plainVersion(version string) string {
	return strings.TrimPrefix(strings.TrimSpace(version), "v")
}

func versionTag(version string) string {
	v := strings.TrimSpace(version)
	if strings.HasPrefix(v, "v") {
		return v
	}
	return "v" + v
}
