package pushstage

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// LocalRemote implements Remote against a real git worktree via git CLI.
// Production only — tests inject FakeRemote explicitly.
type LocalRemote struct {
	Worktree string
}

// NewLocalRemote returns a fail-closed real remote port.
func NewLocalRemote(worktree string) (*LocalRemote, error) {
	worktree = strings.TrimSpace(worktree)
	if worktree == "" {
		return nil, fmt.Errorf("%w: empty worktree", ErrInvalid)
	}
	abs, err := filepath.Abs(worktree)
	if err != nil {
		return nil, fmt.Errorf("%w: abs: %v", ErrInvalid, err)
	}
	return &LocalRemote{Worktree: abs}, nil
}

func (r *LocalRemote) run(args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", r.Worktree}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (r *LocalRemote) ReadRef(remote, branch string) (oid string, exists bool, err error) {
	remote = strings.TrimSpace(remote)
	branch = strings.TrimSpace(branch)
	if remote == "" || branch == "" {
		return "", false, fmt.Errorf("%w: remote/branch required", ErrInvalid)
	}
	out, err := r.run("ls-remote", "--heads", remote, "refs/heads/"+branch)
	if err != nil {
		msg := err.Error()
		if strings.Contains(strings.ToLower(msg), "auth") || strings.Contains(msg, "403") {
			return "", false, ErrAuth
		}
		return "", false, err
	}
	if out == "" {
		return "", false, nil
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", false, nil
	}
	return fields[0], true, nil
}

func (r *LocalRemote) RateLimited() bool { return false }

// PushNonForce pushes newOID only when the remote tip matches expectedOld
// (empty expectedOld means the branch must not exist yet). Never uses force or
// force-with-lease — those can rewrite non-FF history and violate the contract.
func (r *LocalRemote) PushNonForce(remote, branch, expectedOld, newOID string) error {
	remote = strings.TrimSpace(remote)
	branch = strings.TrimSpace(branch)
	newOID = strings.TrimSpace(newOID)
	expectedOld = strings.TrimSpace(expectedOld)
	if remote == "" || branch == "" || newOID == "" {
		return fmt.Errorf("%w: remote/branch/newOID required", ErrInvalid)
	}
	// Exact remote OID check before any push.
	gotOID, exists, err := r.ReadRef(remote, branch)
	if err != nil {
		return err
	}
	if expectedOld == "" {
		if exists {
			return fmt.Errorf("%w: branch %s already exists at %s (expected create)", ErrConflict, branch, gotOID)
		}
	} else {
		if !exists {
			return fmt.Errorf("%w: branch %s missing (expected old %s)", ErrConflict, branch, expectedOld)
		}
		if gotOID != expectedOld {
			return fmt.Errorf("%w: remote moved got=%s expected_old=%s", ErrConflict, gotOID, expectedOld)
		}
	}
	// Plain non-force push only.
	args := []string{"push", remote, newOID + ":refs/heads/" + branch}
	cmd := exec.Command("git", append([]string{"-C", r.Worktree}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.ToLower(string(out) + err.Error())
		switch {
		case strings.Contains(msg, "non-fast-forward"), strings.Contains(msg, "rejected"),
			strings.Contains(msg, "failed to push"):
			return ErrConflict
		case strings.Contains(msg, "auth"), strings.Contains(msg, "403"), strings.Contains(msg, "permission"):
			return ErrAuth
		case strings.Contains(msg, "rate"):
			return ErrRateLimited
		default:
			return fmt.Errorf("git push: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	// Read-back: tip must equal newOID.
	tip, tipExists, rerr := r.ReadRef(remote, branch)
	if rerr != nil {
		return rerr
	}
	if !tipExists || tip != newOID {
		return fmt.Errorf("%w: read-back tip=%s exists=%v want=%s", ErrConflict, tip, tipExists, newOID)
	}
	return nil
}
