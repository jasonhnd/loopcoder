//go:build !unix

package process

var runPSIdentityCommand = func(args ...string) ([]byte, error) {
	return nil, nil
}

func processBirthIdentity(int) string {
	return ""
}

func processExecutableIdentity(int) string {
	return ""
}
