//go:build !darwin

package processtree

import "fmt"

// DarwinPS is unavailable off Darwin; List always fails closed.
type DarwinPS struct{}

// List implements Observer.
func (DarwinPS) List() ([]RawProc, error) {
	return nil, fmt.Errorf("processtree: DarwinPS only supported on darwin")
}
