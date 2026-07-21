// Package evidencecollect normalizes runtime and delivery observations into a
// versioned evidence vocabulary (V090-019 / #1111).
//
// Collectors produce redacted, digests-stable events. Heartbeat/liveness is
// distinct from concrete progress. Provider prose cannot set process, delivery,
// verification, or terminal lifecycle state. Unchanged samples are deduplicated.
package evidencecollect
