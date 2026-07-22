// Package autoroute connects P4 route decision packages to the production
// loopcoder run path (V090-RB04 / #1315).
//
// Explicit provider+model pins are never overridden. Omitted route inputs or
// --auto-route evaluate a persisted evidence snapshot via eligibility +
// routedecision and either select one immutable winner or fail closed with
// typed reasons. Default inventory uses official fake adapters only (no live
// probes / no network in PR CI).
package autoroute
