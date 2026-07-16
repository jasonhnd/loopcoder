//go:build !darwin || !arm64

package supervisedexec

func startGuardian(opts GuardianOptions) (guardianHandle, error) {
	_ = opts
	return guardianNoop{}, nil
}
