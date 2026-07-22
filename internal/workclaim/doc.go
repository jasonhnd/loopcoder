// Package workclaim implements atomic WorkItem claims and generation-fenced
// close (V090-059).
//
// One eligible WorkItem is owned by one attempt generation at a time. Stale or
// losing generations cannot renew, close, or emit terminal success. Ambiguous
// expired live execution is needs-human and is not auto-reclaimed. No provider
// launch or scheduler logic lives here.
package workclaim
