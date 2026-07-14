//go:build unix

package process

import "syscall"

func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// KillTree terminates pid and its whole descendant tree. loopcoder workers lead
// their own process group (Setpgid), so a group-directed signal reaps the whole
// subtree; a bare pid is the fallback.
func KillTree(pid int) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err == nil {
		return nil
	}
	return syscall.Kill(pid, syscall.SIGKILL)
}

func KillGroup(pgid int) error {
	if pgid <= 0 {
		return nil
	}
	return syscall.Kill(-pgid, syscall.SIGKILL)
}
