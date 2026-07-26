// Package compatshim classifies legacy v0.8 commands and enforces old/new writer
// isolation so legacy and v0.9 paths cannot both mutate the same project
// (V090-071 / #1185).
//
// Compatibility output is clearly prefixed and excluded from v0.9 status/gates.
// Once a project accepts v0.9 writes, old mutation is refused; old reads remain
// isolated. Deprecation/removal schedule is explicit in the support matrix.
package compatshim
