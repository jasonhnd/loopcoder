// Package cliprune prunes legacy CLI commands and superseded specifications so
// users only see supported v0.9 commands plus explicit migration/compat ports
// (V090-078 / #1192).
//
// Commands are not removed until their owner deletion issue has replacement
// evidence. Historical architecture is non-authoritative.
package cliprune
