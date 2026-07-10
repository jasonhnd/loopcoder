package storage

import "io/fs"

// PermissionReport describes the storage permission state without exposing
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

// PermissionItem describes one inspected storage path.
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
