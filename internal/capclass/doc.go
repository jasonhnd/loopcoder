// Package capclass defines provider-neutral capability classes (Luna, Tera,
// Soul) and deterministic task-risk classification for route policy (V090-050).
//
// Classification is pure and explainable: every risk input and reason is listed.
// Unknown evidence never silently selects a weaker/cheaper class. Owner
// overrides are append-only records and cannot mutate an active attempt route.
// Model→class mappings are data/tests only; the classifier has no scheduler
// coupling and does not hard-code company marketing model names as policy.
package capclass
