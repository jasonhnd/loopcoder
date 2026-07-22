// Package noauton removes autonomous compile, tick, trigger, and promotion
// entry points from v0.9 production reachability (V090-076 / #1190).
//
// Explicit bounded workflow scheduler and human/release gates remain. Deterministic
// zero-model watchers only through their accepted facade. Historical roadmap
// markers are inert documentation.
package noauton
