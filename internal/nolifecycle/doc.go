// Package nolifecycle retires parallel v0.8 progress/report/relay/outbox
// lifecycle writers so project events are the sole lifecycle truth
// (V090-074 / #1188).
//
// Retained redaction/rendering helpers may only project from pure event input
// for loopcoder.ui.v1. Compatibility commands cannot create/flush/close
// lifecycle state. Historical v0.8 export parsing for V090-069 is untouched.
package nolifecycle
