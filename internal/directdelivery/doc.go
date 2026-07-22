// Package directdelivery wires post-worker delivery stages onto the
// authoritative loopcoder run path (V090-RB03 / #1314).
//
// After directrun reaches cleanup-terminal, this package connects localverify,
// commitstage, hookpolicy, pushstage, prstage, ciwatch, mergegate, and
// deliveryresume using injectable ports (deterministic fakes by default).
// Production Git/GitHub adapters may be injected via Deps; the default path
// never opens network sockets or launches models during CI watch.
//
// The default terminal for a successful delivery is human_gate (await owner
// merge decision). Auto-merge remains false.
package directdelivery
