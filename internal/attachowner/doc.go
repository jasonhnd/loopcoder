// Package attachowner implements foreground and explicit-detach supervisor
// ownership (V090-094 / #1122).
//
// Foreground attachment is the default: the invoking process owns the run until
// terminal, explicit detach, or failure. Explicit --detach is the only path to
// a per-run background supervisor. UI disconnect alone neither kills nor adopts
// provider work; the frozen delivery policy decides. One run owns one
// supervisor generation; stale generations cannot signal or release another's
// resources. No login item, global daemon, or silent auto-detach.
package attachowner
