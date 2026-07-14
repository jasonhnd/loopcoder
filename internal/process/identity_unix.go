//go:build unix

package process

import "os/exec"

var runPSIdentityCommand = func(args ...string) ([]byte, error) {
	return exec.Command("ps", args...).Output()
}

func processBirthIdentity(pid int) string {
	return psIdentityField(pid, "-o", "lstart=")
}

func processExecutableIdentity(pid int) string {
	return psIdentityField(pid, "-o", "comm=")
}
