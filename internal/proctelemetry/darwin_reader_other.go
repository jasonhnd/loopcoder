//go:build !darwin

package proctelemetry

import "fmt"

// DarwinReader is unavailable off Darwin.
type DarwinReader struct{}

// Read implements ResourceReader.
func (DarwinReader) Read([]int) (map[int]ProcResources, error) {
	return nil, fmt.Errorf("proctelemetry: DarwinReader only supported on darwin")
}
