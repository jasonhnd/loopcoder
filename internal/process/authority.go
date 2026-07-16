package process

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Authority returns a durable-enough local process identity for loopcoder-owned
// detached supervisors. Platform files add process-group evidence when
// available; callers must verify the returned string before signalling.
func Authority(pid int, observedAt time.Time) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("pid must be positive")
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	parts := []string{
		"kind=loopcoder-detached-supervisor",
		"pid=" + strconv.Itoa(pid),
		"observed_at=" + observedAt.UTC().Format(time.RFC3339Nano),
		"executable=" + exe,
	}
	if group, ok := processGroup(pid); ok {
		parts = append(parts, "process_group="+strconv.Itoa(group))
	}
	return strings.Join(parts, "\x1f"), nil
}

// VerifyAuthority fails closed when the stored process authority cannot still
// be matched to the observable process identity.
func VerifyAuthority(pid int, authority string) error {
	fields := authorityFields(authority)
	if fields["kind"] != "loopcoder-detached-supervisor" {
		return fmt.Errorf("unsupported process authority")
	}
	if fields["pid"] != strconv.Itoa(pid) {
		return fmt.Errorf("process authority pid mismatch")
	}
	if !Alive(pid) {
		return fmt.Errorf("process is not alive")
	}
	if expected := strings.TrimSpace(fields["executable"]); expected != "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if exe != expected {
			return fmt.Errorf("process authority executable mismatch")
		}
	}
	if expected := strings.TrimSpace(fields["process_group"]); expected != "" {
		group, ok := processGroup(pid)
		if !ok {
			return fmt.Errorf("process group unavailable")
		}
		if strconv.Itoa(group) != expected {
			return fmt.Errorf("process authority group mismatch")
		}
	}
	return nil
}

func authorityFields(authority string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(authority, "\x1f") {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return out
}
