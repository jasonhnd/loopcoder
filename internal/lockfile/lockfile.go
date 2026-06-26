package lockfile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

const lockDirName = "loopcoder-locks"

var localLocks sync.Map

type Lock struct {
	key      string
	path     string
	fileLock *flock.Flock
	local    chan struct{}

	mu       sync.Mutex
	released bool
}

// Acquire locks the git worktree-add critical section for repoPath.
func Acquire(repoPath string, timeout time.Duration) (*Lock, error) {
	key, err := repoKey(repoPath)
	if err != nil {
		return nil, err
	}
	path := lockFilePath(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}

	slot := localLock(path)
	start := time.Now()
	if !acquireLocal(slot, timeout) {
		return nil, timeoutError(key, timeout)
	}

	fileLock := flock.New(path)
	if timeout <= 0 {
		locked, err := fileLock.TryLock()
		if err != nil {
			releaseLocal(slot)
			return nil, fmt.Errorf("acquire worktree lock for repo key %s: %w", key, err)
		}
		if !locked {
			releaseLocal(slot)
			return nil, timeoutError(key, timeout)
		}
		return &Lock{key: key, path: path, fileLock: fileLock, local: slot}, nil
	}

	remaining := timeout - time.Since(start)
	if remaining <= 0 {
		releaseLocal(slot)
		return nil, timeoutError(key, timeout)
	}

	ctx, cancel := context.WithTimeout(context.Background(), remaining)
	defer cancel()
	locked, err := fileLock.TryLockContext(ctx, retryDelay(remaining))
	if err != nil {
		releaseLocal(slot)
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return nil, timeoutError(key, timeout)
		}
		return nil, fmt.Errorf("acquire worktree lock for repo key %s: %w", key, err)
	}
	if !locked {
		releaseLocal(slot)
		return nil, timeoutError(key, timeout)
	}

	return &Lock{key: key, path: path, fileLock: fileLock, local: slot}, nil
}

// Release releases a lock returned by Acquire.
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}

	l.mu.Lock()
	if l.released {
		l.mu.Unlock()
		return nil
	}
	l.released = true
	fileLock := l.fileLock
	slot := l.local
	key := l.key
	l.mu.Unlock()

	var err error
	if fileLock != nil {
		if unlockErr := fileLock.Unlock(); unlockErr != nil {
			err = fmt.Errorf("release worktree lock for repo key %s: %w", key, unlockErr)
		}
	}
	if slot != nil {
		releaseLocal(slot)
	}
	return err
}

func repoKey(repoPath string) (string, error) {
	canonical, err := canonicalRepoPath(repoPath)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:]), nil
}

func canonicalRepoPath(repoPath string) (string, error) {
	repoPath = strings.TrimSpace(repoPath)
	if repoPath == "" {
		return "", errors.New("repo path is required")
	}

	abs, err := filepath.Abs(repoPath)
	if err != nil {
		return "", fmt.Errorf("canonicalize repo path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}

	return normalizeCanonicalPath(filepath.Clean(abs)), nil
}

func lockFilePath(key string) string {
	return filepath.Join(os.TempDir(), lockDirName, "worktree-add-"+key+".lock")
}

func localLock(path string) chan struct{} {
	slot := make(chan struct{}, 1)
	slot <- struct{}{}
	actual, _ := localLocks.LoadOrStore(path, slot)
	return actual.(chan struct{})
}

func acquireLocal(slot chan struct{}, timeout time.Duration) bool {
	if timeout <= 0 {
		select {
		case <-slot:
			return true
		default:
			return false
		}
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-slot:
		return true
	case <-timer.C:
		return false
	}
}

func releaseLocal(slot chan struct{}) {
	select {
	case slot <- struct{}{}:
	default:
	}
}

func retryDelay(timeout time.Duration) time.Duration {
	delay := 10 * time.Millisecond
	if timeout > 0 && timeout < delay {
		return timeout
	}
	return delay
}

func timeoutError(key string, timeout time.Duration) error {
	return fmt.Errorf("timed out acquiring worktree lock for repo key %s after %s", key, timeout)
}
