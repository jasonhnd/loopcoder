//go:build !unix && !windows

package process

func Alive(pid int) bool {
	return pid > 0
}
