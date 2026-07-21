// Package attention implements durable attention lifecycle and the authorized
// operator action API (V090-090 / #1118).
//
// Needs-human is product state: open → acknowledged → resolved|superseded.
// UI clients submit bounded, idempotent actions with client/session identity,
// expected run revision, and action-specific authorization. Accepted actions
// append evidence before effect and never mutate route pins, forge completion,
// bypass admission, or signal unowned processes.
package attention
