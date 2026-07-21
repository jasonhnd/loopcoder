// Package deliverygate implements the required report-client gate, delivery
// degradation, and fallback policy (V090-091 / #1119).
//
// A run cannot launch a provider into a requested UI mode unless a required
// client proves start:rendered. Missed mandatory acknowledgements degrade
// delivery and eventually apply the frozen stop/detach policy. Fallbacks must
// be named connected clients with their own rendered acknowledgements — never
// invented from host detection. Report generation and process cleanup stay
// independent of UI availability.
package deliverygate
