//go:build unix

package process

import "syscall"

func processGroup(pid int) (int, bool) {
	group, err := syscall.Getpgid(pid)
	return group, err == nil
}
