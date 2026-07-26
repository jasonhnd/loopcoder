// Package providerexec defines the minimal provider execution contract and
// reference adapters for the pre-router direct path (V090-095 / #1123).
//
// Adapters accept one immutable explicit request and return normalized
// process/outcome evidence. They do not discover providers, score routes,
// install tools, write project lifecycle, or persist credentials.
package providerexec
