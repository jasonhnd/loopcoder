// Package acceptharness provides deterministic disposable fixtures for v0.9.0
// ordinary-development acceptance tests (V090-003 / #1095).
//
// The harness never contacts GitHub, model providers, keychains, browsers, or
// the network. Time advances only through an injected clock or explicit
// barriers. All identities and tokens are synthetic.
//
// Public helpers are intended for later P1+ packages to reuse instead of
// inventing one-off fakes.
package acceptharness
