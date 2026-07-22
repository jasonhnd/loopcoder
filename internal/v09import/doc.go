// Package v09import imports a neutral v0.8 export into machine/project v0.9
// stores transactionally with dry-run and migration reporting (V090-070 / #1184).
//
// Never imports running process, claim, resource reservation, auth, credential,
// or implicit route eligibility. Nonterminal/live v0.8 records become historical
// or attention-only. Same export is idempotent. One failed project does not
// corrupt machine store or other successful projects. Old state is never
// deleted/moved automatically; rollback requires restoring backups.
package v09import
