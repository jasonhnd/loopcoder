// Package retention defines bounded local lifecycle policy for events, logs,
// runtime files, and related expendable classes, plus dry-run inventory and
// explicit archive/delete planning (V090-087 / #1182).
//
// Append-only does not mean unlimited disk growth. GC never deletes customer
// repos, branches, commits, PRs, provider credentials, or unknown files, and
// never runs implicitly during fragile recovery. Active, attention, migration,
// ambiguous, and unacknowledged records are held regardless of age/size.
package retention
