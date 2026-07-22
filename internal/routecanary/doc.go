// Package routecanary is the P4 smart-routing end-to-end acceptance canary
// (V090-055 / #1166).
//
// It exercises explicit pins, automatic winners, no-route, and authorized
// successors across Codex, Claude, Gemini, Antigravity, and Grok using only
// deterministic fixtures. No live provider calls, no busy polling, no residual
// child processes or repo-local state. Optional real observation smoke is out of
// band and never required for PR CI.
package routecanary
