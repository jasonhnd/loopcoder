// Package projectschema defines the compact project.db domain tables for v0.9
// project authority (V090-008 / #1100).
//
// Events are the immutable lifecycle truth. This package owns schema shape only;
// append/replay APIs are V090-009. No provider credentials, machine-global
// inventory, or cross-database foreign keys are stored here.
package projectschema
