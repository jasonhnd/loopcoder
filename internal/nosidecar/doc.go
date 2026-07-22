// Package nosidecar forbids production writes of .loopcoder / runtime sidecars
// inside customer repositories and maps legacy repo-local paths to global or
// read-only export dispositions (V090-072 / #1186).
//
// New-path runtime never chooses <repo>/.loopcoder. Legacy state is opened only
// by the read-only exporter. Existing repo-local files are never auto-deleted.
package nosidecar
