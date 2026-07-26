package store

import "io/fs"

// PermissionReport describes store path permission state without exposing
// database contents.
type PermissionReport struct {
	Path      string
	Platform  string
	Supported bool
	Secure    bool
	Repaired  bool
	Message   string
	Items     []PermissionItem
}

// PermissionItem describes one inspected store path.
type PermissionItem struct {
	Path       string
	Kind       string
	Exists     bool
	BeforeMode fs.FileMode
	AfterMode  fs.FileMode
	Secure     bool
	Repaired   bool
	Unsafe     bool
	Message    string
}

func firstUnsafePermissionItem(report PermissionReport) *PermissionItem {
	for i := range report.Items {
		if report.Items[i].Unsafe {
			return &report.Items[i]
		}
	}
	return nil
}
