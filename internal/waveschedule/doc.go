// Package waveschedule plans bounded serial/parallel waves from a ready set
// (V090-061). It persists wave plans before claims and emits immutable
// completion candidates for later integration (V090-100). It never integrates,
// merges, closes WorkItems, or makes model calls.
package waveschedule
