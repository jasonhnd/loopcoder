// Package runtimefacade is the v0.9 provider-neutral runtime port (V090-012).
//
// Callers launch, observe, signal, and join top-level attempts through Runtime
// and Handle only. Process completion is proven by OS join evidence, never by
// provider prose. Low-level process-group and PTY supervision remain in
// internal/supervisedexec; this package does not introduce a second supervisor.
//
// See docs/reference/runtime-facade.md for the direct-launch disposition
// inventory of remaining callers.
package runtimefacade
