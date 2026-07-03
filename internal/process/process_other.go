//go:build !unix && !windows

package process

func Alive(pid int) bool {
	return pid > 0
}

// KillTree is a best-effort no-op on platforms without process control.
func KillTree(pid int) error {
	return nil
}
