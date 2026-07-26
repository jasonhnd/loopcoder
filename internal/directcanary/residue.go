package directcanary

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Forbidden residual names/paths that must not appear under a consumer checkout.
var residualMarkers = []string{
	".loopcoder",
	"loopcoder-runtime",
	"loopcoder.session",
	"delivery-receipt.json",
	"worker-attempt.json",
	"route-pin.json",
}

// ScanResidue walks root and reports LoopCoder runtime residue.
// It never follows symlinks outside root and ignores .git contents except for
// checking that no loopcoder metadata was written into git-managed paths.
func ScanResidue(root string) ([]string, error) {
	if root == "" {
		return nil, fmt.Errorf("directcanary: residue root required")
	}
	var hits []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		// Skip .git objects entirely for content; still flag marker names if present.
		base := d.Name()
		for _, m := range residualMarkers {
			if base == m || strings.Contains(rel, string(filepath.Separator)+m+string(filepath.Separator)) || strings.HasSuffix(rel, m) {
				hits = append(hits, rel)
			}
		}
		if d.IsDir() && base == ".git" {
			// do not descend into objects; still scanned the dir name itself above
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return hits, err
	}
	return hits, nil
}
