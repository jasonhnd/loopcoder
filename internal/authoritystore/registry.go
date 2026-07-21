package authoritystore

import (
	"fmt"
	"path/filepath"
	"sync"
)

// openRegistry tracks live handles by cleaned path so one file cannot be opened
// as both machine and project authority in-process.
type openRegistry struct {
	mu     sync.Mutex
	byPath map[string]Role
}

var globalOpen = &openRegistry{byPath: map[string]Role{}}

func (r *openRegistry) claim(path string, role Role) error {
	path = filepath.Clean(path)
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.byPath[path]; ok && existing != role {
		return fmt.Errorf("%w: path already open as %s, cannot open as %s", ErrRoleMismatch, existing, role)
	}
	// Allow re-open same role (concurrent handles of same role are permitted).
	// Track first role only.
	if _, ok := r.byPath[path]; !ok {
		r.byPath[path] = role
	}
	return nil
}

func (r *openRegistry) release(path string) {
	path = filepath.Clean(path)
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byPath, path)
}

// resetOpenRegistryForTest clears the process registry (tests only).
func resetOpenRegistryForTest() {
	globalOpen.mu.Lock()
	defer globalOpen.mu.Unlock()
	globalOpen.byPath = map[string]Role{}
}
