// Package projectid resolves stable v0.9 project identities and registers them
// under the machine authority store (V090-006 / #1098).
//
// GitHub owner/repo identity is preferred when a normalized remote exists.
// Local-only repositories get a stable path-derived ID. Short repository names
// alone never merge two projects.
package projectid
