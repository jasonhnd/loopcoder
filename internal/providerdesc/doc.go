// Package providerdesc defines the versioned provider descriptor registry and
// conformance harness (V090-037 / #1140).
//
// One adapter registers one versioned descriptor exposing only capabilities it
// can prove. Discovery, catalog, quota, and invocation share normalized
// provenance/confidence/freshness/diagnostic envelopes. Adapters never read or
// write route decisions, project lifecycle, GitHub delivery, or raw credentials.
// A fake adapter plus reusable conformance suite covers success and typed
// failure modes without network or real providers.
package providerdesc
