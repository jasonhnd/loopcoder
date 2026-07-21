// Package outputcap implements bounded attempt stdout/stderr capture and log
// lifecycle (V090-014). Child pipes are always drained so join cannot deadlock
// when display buffers fill. Event excerpts are redacted UTF-8; raw logs stay
// under the validated project payload root with owner-only permissions.
package outputcap
