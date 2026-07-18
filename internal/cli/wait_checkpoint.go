package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/waitstate"
)

// fileWaitCheckpoint persists wait snapshots under a project-local directory so
// process restarts resume the same decision keys without duplicating wakes.
func fileWaitCheckpoint(dir string) (waitstate.CheckpointFunc, func(kind waitstate.Kind, waitID string) (waitstate.Snapshot, bool, error), error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, nil, fmt.Errorf("wait checkpoint directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	pathFor := func(kind waitstate.Kind, waitID string) string {
		safe := strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
				return r
			default:
				return '_'
			}
		}, string(kind)+"--"+waitID)
		return filepath.Join(dir, safe+".json")
	}
	checkpoint := func(_ context.Context, snapshot waitstate.Snapshot) error {
		path := pathFor(snapshot.Kind, snapshot.WaitID)
		tmp := path + ".tmp"
		data, err := json.MarshalIndent(snapshot, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			return err
		}
		return os.Rename(tmp, path)
	}
	load := func(kind waitstate.Kind, waitID string) (waitstate.Snapshot, bool, error) {
		data, err := os.ReadFile(pathFor(kind, waitID))
		if err != nil {
			if os.IsNotExist(err) {
				return waitstate.Snapshot{}, false, nil
			}
			return waitstate.Snapshot{}, false, err
		}
		var snapshot waitstate.Snapshot
		if err := json.Unmarshal(data, &snapshot); err != nil {
			return waitstate.Snapshot{}, false, err
		}
		return snapshot, true, nil
	}
	return checkpoint, load, nil
}
