package store

import (
	"fmt"
	"os"
	"time"
)

// openMarkerPath returns the unclean-open sidecar for a database path.
// Sidecar avoids schema migration while still detecting abrupt process death.
func openMarkerPath(dbPath string) string {
	return dbPath + ".open"
}

// readUncleanMarker reports whether a previous open left a marker.
func readUncleanMarker(dbPath string) bool {
	_, err := os.Stat(openMarkerPath(dbPath))
	return err == nil
}

// writeOpenMarker records that this process holds an open handle.
func writeOpenMarker(dbPath string, now time.Time) error {
	content := fmt.Sprintf("pid=%d\nopened_at=%s\n", os.Getpid(), now.UTC().Format(time.RFC3339Nano))
	path := openMarkerPath(dbPath)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write open marker: %w", err)
	}
	return hardenSQLiteSidecars(dbPath)
}

// clearOpenMarker removes the unclean-open sidecar after a clean close.
func clearOpenMarker(dbPath string) error {
	path := openMarkerPath(dbPath)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear open marker: %w", err)
	}
	return nil
}
