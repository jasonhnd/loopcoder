// Package capacityledger owns useful-capacity routing policy defaults and
// durable soft-reservation accounting for production loopcoder run
// (V090-CRO-007 / #1340).
//
// Policy order for the owner product goal:
//  1. quality/safety floors (eligibility / task class) already applied upstream
//  2. useful paid capacity before reset (default ModeBurnBeforeReset)
//
// Also exposes balanced and quality-first (preserve premium) policies.
//
// Accounting is soft-fraction based, never fabricates exact tokens from
// unknown/stale observations, and never stores credentials.
package capacityledger
