//go:build !unix

package process

func processGroup(int) (int, bool) {
	return 0, false
}
