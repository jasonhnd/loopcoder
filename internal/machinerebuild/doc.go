// Package machinerebuild rebuilds machine-authority inventory and reconciles
// resource/quota reservations when machine.db is missing or corrupt
// (V090-086 / #1181).
//
// Recovery is conservative and current-Mac only:
//   - scan validated $LOOPCODER_HOME/projects children
//   - reject symlink/path/file anomalies and wrong-owner/duplicates
//   - rebuild a new machine store beside the damaged one (never silent overwrite)
//   - preserve damaged DB read-only for diagnostics/backup
//   - reconcile reservations against live process evidence; unknown ownership
//     becomes attention, never automatic release/adoption
//
// No database merge, cross-Mac state copy, credential recovery, or automatic
// deletion of damaged data. Provider facts are refreshed with provenance only;
// stale serialized snapshots are not treated as current truth.
package machinerebuild
