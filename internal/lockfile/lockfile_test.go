package lockfile

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRepoKeyDerivationIsDeterministic(t *testing.T) {
	repo := t.TempDir()

	key, err := repoKey(repo)
	if err != nil {
		t.Fatalf("repoKey returned error: %v", err)
	}
	sameKey, err := repoKey(filepath.Join(repo, "."))
	if err != nil {
		t.Fatalf("repoKey returned error for equivalent path: %v", err)
	}
	if key != sameKey {
		t.Fatalf("repoKey differs for equivalent paths: %q != %q", key, sameKey)
	}
	if len(key) != 64 {
		t.Fatalf("repoKey length = %d, want 64", len(key))
	}

	otherKey, err := repoKey(t.TempDir())
	if err != nil {
		t.Fatalf("repoKey returned error for other repo: %v", err)
	}
	if key == otherKey {
		t.Fatalf("repoKey matched for different repo paths: %q", key)
	}

	path := lockFilePath(key)
	if !strings.Contains(filepath.Base(path), key) {
		t.Fatalf("lock file path %q does not include repo key %q", path, key)
	}
}

func TestAcquireReleaseReacquire(t *testing.T) {
	repo := t.TempDir()

	first, err := Acquire(repo, time.Second)
	if err != nil {
		t.Fatalf("Acquire returned error: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("Release returned error: %v", err)
	}

	second, err := Acquire(repo, time.Second)
	if err != nil {
		t.Fatalf("Acquire after Release returned error: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatalf("second Release returned error: %v", err)
	}
}

func TestAcquireTimeout(t *testing.T) {
	repo := t.TempDir()

	first, err := Acquire(repo, time.Second)
	if err != nil {
		t.Fatalf("Acquire returned error: %v", err)
	}
	defer first.Release()

	_, err = Acquire(repo, 20*time.Millisecond)
	if err == nil {
		t.Fatal("Acquire while locked succeeded, want timeout")
	}
	key, keyErr := repoKey(repo)
	if keyErr != nil {
		t.Fatalf("repoKey returned error: %v", keyErr)
	}
	if !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), key) {
		t.Fatalf("timeout error = %q, want repo key %q", err.Error(), key)
	}
}
