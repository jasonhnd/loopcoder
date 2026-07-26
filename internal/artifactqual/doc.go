// Package artifactqual is the executable exact-artifact qualification harness
// (V090-RB09 / #1320).
//
// It derives installsmoke Environment facts from named subprocess/file probes
// against an immutable RC archive. FixtureEnvironment boolean constructors are
// rejected in release mode. Outputs feed releaseslo and rcgonogo.
package artifactqual
