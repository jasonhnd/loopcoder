package workflowrun

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/process"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
	"github.com/jasonhnd/loopcoder/internal/workclaim"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// AuthorityOwnerFromClaimID binds authority/guardian fence owner to durable claim identity.
// Never a global constant that could collide across attempts.
func AuthorityOwnerFromClaimID(claimID string) string {
	claimID = strings.TrimSpace(claimID)
	if claimID == "" {
		return ""
	}
	return "workflowrun:claim:" + claimID
}

// AuthorityStorePath is the run-scoped SQLite store for ProviderExecutionAuthority
// (same schema as storage.ProviderExecutionAuthority — not a parallel identity store).
func AuthorityStorePath(homeDir, projectID, runID string) (string, error) {
	dir, err := RunDurableDir(homeDir, projectID, runID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "execution-authority.sqlite"), nil
}

// GuardianDiagnosticPath is the append-only guardian diagnostic JSONL for one attempt.
func GuardianDiagnosticPath(homeDir, projectID, runID, attemptID string) (string, error) {
	dir, err := RunDurableDir(homeDir, projectID, runID)
	if err != nil {
		return "", err
	}
	safe := sanitizeBranch(attemptID)
	return filepath.Join(dir, "guardian-"+safe+".jsonl"), nil
}

// OpenAuthorityStore opens/creates the run-scoped execution-authority store and
// ensures the project row exists (FK for provider_execution_authorities).
func OpenAuthorityStore(ctx context.Context, homeDir, projectID, runID string, now func() time.Time) (storage.Store, error) {
	path, err := AuthorityStorePath(homeDir, projectID, runID)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	store, err := storage.Open(ctx, storage.Options{Path: path, Now: now})
	if err != nil {
		return nil, err
	}
	if err := ensureAuthorityProject(ctx, store, projectID, homeDir, now); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

func ensureAuthorityProject(ctx context.Context, store storage.Store, projectID, localPath string, now func() time.Time) error {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return fmt.Errorf("workflowrun: authority project_id required")
	}
	if now == nil {
		now = time.Now
	}
	ts := now().UTC().Format(time.RFC3339Nano)
	localPath = strings.TrimSpace(localPath)
	if localPath == "" {
		localPath = "/workflowrun/" + projectID
	}
	return store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO projects(
				id, local_path, created_at, updated_at, local_path_canonical, git_root, identity_source
			) VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO NOTHING`,
			projectID, localPath, ts, ts, localPath, localPath, "workflowrun")
		return err
	})
}

// PersistChildExecutionAuthority writes the same spawn identity used for the pid event.
func PersistChildExecutionAuthority(
	ctx context.Context,
	store storage.Store,
	projectID, runID, attemptID, ownerID string,
	claimGen int64,
	ps ProcessStart,
	worktreePath, logPath string,
	at time.Time,
) (storage.ProviderExecutionAuthority, error) {
	if store == nil {
		return storage.ProviderExecutionAuthority{}, fmt.Errorf("workflowrun: authority store required")
	}
	if err := ValidateProcessStart(ps); err != nil {
		return storage.ProviderExecutionAuthority{}, err
	}
	if claimGen <= 0 {
		return storage.ProviderExecutionAuthority{}, fmt.Errorf("workflowrun: claim generation required for authority")
	}
	ownerID = strings.TrimSpace(ownerID)
	if ownerID == "" {
		return storage.ProviderExecutionAuthority{}, fmt.Errorf("workflowrun: authority owner_id required (claim-bound)")
	}
	if strings.TrimSpace(worktreePath) == "" || strings.TrimSpace(logPath) == "" {
		return storage.ProviderExecutionAuthority{}, fmt.Errorf("workflowrun: worktree and log path required for authority")
	}
	if at.IsZero() {
		at = ps.ObservedAt
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	auth := storage.ProviderExecutionAuthority{
		ProjectID:            projectID,
		RunID:                runID,
		AttemptID:            attemptID,
		ProviderPID:          ps.PID,
		ProviderPGID:         ps.PGID,
		ProcessBirthIdentity: ps.ProcessBirthIdentity,
		ExecutableIdentity:   ps.ExecutableIdentity,
		OwnerID:              ownerID,
		ClaimGeneration:      claimGen,
		WorktreePath:         worktreePath,
		LogPath:              logPath,
		IdentityAmbiguous:    ps.IdentityAmbiguous,
		AmbiguityReason:      ps.IdentityAmbiguityNote,
		// Typed in the same row/tx as create — not a later event append.
		SpawnPhase: storage.SpawnPhaseAuthorityPersisted,
	}
	return storage.PersistProviderExecutionAuthority(ctx, store, auth, at)
}

// TransitionChildSpawnPhase advances authority.spawn_phase under claim fence.
func TransitionChildSpawnPhase(
	ctx context.Context,
	store storage.Store,
	projectID, runID, attemptID, ownerID string,
	claimGen int64,
	toPhase string,
	at time.Time,
) error {
	if store == nil {
		return fmt.Errorf("workflowrun: authority store required for spawn_phase transition")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	fence := storage.ProviderExecutionAuthorityFence{
		ProjectID: projectID, RunID: runID, AttemptID: attemptID,
		OwnerID: ownerID, ClaimGeneration: claimGen,
	}
	return storage.TransitionProviderExecutionSpawnPhase(ctx, store, fence, at, toPhase)
}

// CompleteChildExecutionAuthority marks authority terminal via the normal API
// (requires spawn_phase=pid_event_persisted). Errors are never swallowed.
// Pre-PID recovery must use CompleteChildExecutionAuthorityPrePIDRecovery.
func CompleteChildExecutionAuthority(
	ctx context.Context,
	store storage.Store,
	projectID, runID, attemptID, ownerID string,
	claimGen int64,
	terminal string,
	at time.Time,
) error {
	return completeChildExecutionAuthority(ctx, store, projectID, runID, attemptID, ownerID, claimGen, terminal, at, false)
}

// CompleteChildExecutionAuthorityPrePIDRecovery completes authority only when
// spawn_phase is authority_persisted or pid_event_failed (typed pre-PID recovery).
func CompleteChildExecutionAuthorityPrePIDRecovery(
	ctx context.Context,
	store storage.Store,
	projectID, runID, attemptID, ownerID string,
	claimGen int64,
	terminal string,
	at time.Time,
) error {
	return completeChildExecutionAuthority(ctx, store, projectID, runID, attemptID, ownerID, claimGen, terminal, at, true)
}

func completeChildExecutionAuthority(
	ctx context.Context,
	store storage.Store,
	projectID, runID, attemptID, ownerID string,
	claimGen int64,
	terminal string,
	at time.Time,
	prePID bool,
) error {
	if store == nil {
		return fmt.Errorf("workflowrun: authority store required for complete")
	}
	ownerID = strings.TrimSpace(ownerID)
	if ownerID == "" {
		return fmt.Errorf("workflowrun: authority owner required for complete")
	}
	fence := storage.ProviderExecutionAuthorityFence{
		ProjectID: projectID, RunID: runID, AttemptID: attemptID,
		OwnerID: ownerID, ClaimGeneration: claimGen,
	}
	existing, err := storage.LoadProviderExecutionAuthority(ctx, store, projectID, runID, attemptID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("workflowrun: authority missing for complete %s/%s/%s: %w", projectID, runID, attemptID, err)
		}
		return fmt.Errorf("workflowrun: authority load for complete: %w", err)
	}
	if strings.TrimSpace(existing.CompletedAt) != "" {
		if strings.TrimSpace(existing.TerminalState) == strings.TrimSpace(terminal) {
			return nil // idempotent same terminal
		}
		return fmt.Errorf("workflowrun: authority already completed as %q want %q", existing.TerminalState, terminal)
	}
	phase := strings.TrimSpace(existing.SpawnPhase)
	if phase == "" || phase == storage.SpawnPhaseLegacyUnknown {
		return fmt.Errorf("workflowrun: authority complete rejected for spawn_phase %q", phase)
	}
	if prePID {
		return storage.CompleteProviderExecutionAuthorityPrePIDRecovery(ctx, store, fence, at, terminal)
	}
	// Crash window: authority_persisted + durable PID path must transition first.
	// Callers/recovery perform TransitionChildSpawnPhase before complete when needed.
	return storage.CompleteProviderExecutionAuthority(ctx, store, fence, at, terminal)
}

// BuildChildGuardianOptions configures macOS supervisor-death guardian for one child attempt.
func BuildChildGuardianOptions(
	storePath, diagnosticPath, projectID, runID, attemptID, ownerID string,
	claimGen int64,
	authorityHolder *storage.ProviderExecutionAuthority,
) supervisedexec.GuardianOptions {
	if strings.TrimSpace(storePath) == "" || strings.TrimSpace(diagnosticPath) == "" {
		return supervisedexec.GuardianOptions{}
	}
	if strings.TrimSpace(ownerID) == "" || claimGen <= 0 {
		return supervisedexec.GuardianOptions{}
	}
	return supervisedexec.GuardianOptions{
		Enabled:         true,
		StorePath:       storePath,
		DiagnosticPath:  diagnosticPath,
		ProjectID:       projectID,
		RunID:           runID,
		AttemptID:       attemptID,
		OwnerID:         ownerID,
		ClaimGeneration: claimGen,
		ProviderAuthority: func() (storage.ProviderExecutionAuthority, bool) {
			if authorityHolder == nil || authorityHolder.ProviderPID <= 0 {
				return storage.ProviderExecutionAuthority{}, false
			}
			return *authorityHolder, true
		},
	}
}

// ValidateAuthorityMatchesSpawn fails closed when authority and pid event disagree,
// or when the authority row is not a fresh authority_persisted create (legacy/advanced).
// Called before durable PID append.
func ValidateAuthorityMatchesSpawn(auth storage.ProviderExecutionAuthority, ps ProcessStart, claimGen int64, attemptID, ownerID string) error {
	if strings.TrimSpace(auth.SchemaVersion) != storage.ProviderExecutionAuthoritySchema {
		return fmt.Errorf("authority schema_version %q != %q", auth.SchemaVersion, storage.ProviderExecutionAuthoritySchema)
	}
	if strings.TrimSpace(auth.AuthorityID) == "" {
		return fmt.Errorf("authority authority_id required nonempty")
	}
	// Pre-PID authority_persisted must be the create row at exactly RecordVersion=1.
	if auth.RecordVersion != 1 {
		return fmt.Errorf("authority record_version %d want exactly 1 before PID append", auth.RecordVersion)
	}
	phase := strings.TrimSpace(auth.SpawnPhase)
	if phase == "" {
		phase = storage.SpawnPhaseLegacyUnknown
	}
	if phase == storage.SpawnPhaseLegacyUnknown {
		return fmt.Errorf("authority spawn_phase legacy_unknown rejected before PID append")
	}
	if phase != storage.SpawnPhaseAuthorityPersisted {
		return fmt.Errorf("authority spawn_phase %q want authority_persisted before PID append", phase)
	}
	if strings.TrimSpace(auth.CompletedAt) != "" {
		return fmt.Errorf("authority already completed before PID append")
	}
	if strings.TrimSpace(auth.TerminalState) != "" {
		return fmt.Errorf("authority TerminalState %q contradictory before PID append", auth.TerminalState)
	}
	if auth.ProviderPID != ps.PID {
		return fmt.Errorf("authority pid %d != spawn pid %d", auth.ProviderPID, ps.PID)
	}
	if auth.ProviderPGID != ps.PGID {
		return fmt.Errorf("authority pgid %d != spawn pgid %d", auth.ProviderPGID, ps.PGID)
	}
	if strings.TrimSpace(auth.ProcessBirthIdentity) != strings.TrimSpace(ps.ProcessBirthIdentity) {
		return fmt.Errorf("authority process_birth_identity mismatch")
	}
	if strings.TrimSpace(auth.ExecutableIdentity) != strings.TrimSpace(ps.ExecutableIdentity) {
		return fmt.Errorf("authority executable_identity mismatch")
	}
	if auth.ClaimGeneration != claimGen {
		return fmt.Errorf("authority claim_generation %d != %d", auth.ClaimGeneration, claimGen)
	}
	if strings.TrimSpace(auth.AttemptID) != strings.TrimSpace(attemptID) {
		return fmt.Errorf("authority attempt_id mismatch")
	}
	if strings.TrimSpace(ownerID) == "" || strings.TrimSpace(auth.OwnerID) != strings.TrimSpace(ownerID) {
		return fmt.Errorf("authority owner_id mismatch want %q got %q", ownerID, auth.OwnerID)
	}
	if strings.TrimSpace(auth.WorktreePath) == "" || strings.TrimSpace(auth.WorktreePath) != strings.TrimSpace(ps.WorktreePath) {
		return fmt.Errorf("authority worktree_path missing or mismatch")
	}
	if strings.TrimSpace(auth.LogPath) == "" || strings.TrimSpace(auth.LogPath) != strings.TrimSpace(ps.LogPath) {
		return fmt.Errorf("authority log_path missing or mismatch")
	}
	if auth.IdentityAmbiguous {
		return fmt.Errorf("authority identity ambiguous: %s", auth.AmbiguityReason)
	}
	if strings.TrimSpace(auth.StartedAt) == "" {
		return fmt.Errorf("authority started_at required")
	}
	return nil
}

// NextAttemptGenerationFromEvents selects the 0-indexed generation this run should
// claim or reuse for each work item (durable source of truth when Request map is nil).
//
// Latest generation by event order of max gen wins:
//  1. Latest effective terminal is succeeded gN → select gN (exact attempt reuse, zero launch).
//  2. Latest is authoritative hard-recovery cancelled/failed gN with no later attempt → gN+1.
//  3. Never prefer older success/cancel over a newer generation's state.
func NextAttemptGenerationFromEvents(events []Event) map[string]int {
	type genSnap struct {
		terminal  string // succeeded|cancelled|failed|""
		hardRecov bool
		launched  bool
	}
	// workItem → gen → snap
	by := map[string]map[int]*genSnap{}
	ensure := func(id string, g int) *genSnap {
		if by[id] == nil {
			by[id] = map[int]*genSnap{}
		}
		if by[id][g] == nil {
			by[id][g] = &genSnap{}
		}
		return by[id][g]
	}
	for _, ev := range events {
		id := strings.TrimSpace(ev.WorkItemID)
		if id == "" {
			continue
		}
		g := ParseAttemptGeneration(ev.AttemptID)
		if g < 0 {
			continue
		}
		s := ensure(id, g)
		switch ev.Kind {
		case "launch":
			s.launched = true
		case "reuse", "integrate":
			s.terminal = string(workgraph.TermSucceeded)
		case "terminal":
			term := strings.TrimSpace(ev.Terminal)
			s.terminal = term
			if isAuthoritativeHardRecoveryEvent(ev) {
				s.hardRecov = true
			}
		case "interrupt":
			if isAuthoritativeHardRecoveryEvent(ev) {
				s.hardRecov = true
			}
		}
	}
	out := map[string]int{}
	for id, gens := range by {
		maxG := -1
		for g := range gens {
			if g > maxG {
				maxG = g
			}
		}
		if maxG < 0 {
			continue
		}
		st := gens[maxG]
		if strings.EqualFold(st.terminal, string(workgraph.TermSucceeded)) {
			// Reuse exact succeeded attempt gN.
			out[id] = maxG
			continue
		}
		if st.hardRecov && (strings.EqualFold(st.terminal, string(workgraph.TermCancelled)) ||
			strings.EqualFold(st.terminal, string(workgraph.TermFailed))) {
			// Authoritative recovery finished gN → claim gN+1.
			out[id] = maxG + 1
			continue
		}
		// No durable selection (open in-flight handled by recovery first).
	}
	return out
}

// isAuthoritativeHardRecoveryEvent accepts only the complete structured markers
// this package emits — never Message/prose and never partial OR acceptance.
// Requires recovery=authoritative AND failure_class=hard_kill_recovery AND
// interrupt_class=hard_kill_recovery. Kind must be interrupt or terminal;
// terminal requires cancelled|failed.
func isAuthoritativeHardRecoveryEvent(ev Event) bool {
	kind := strings.TrimSpace(ev.Kind)
	if kind != "interrupt" && kind != "terminal" {
		return false
	}
	if len(ev.Payload) == 0 {
		return false
	}
	var m map[string]string
	if err := json.Unmarshal(ev.Payload, &m); err != nil {
		return false
	}
	if m["recovery"] != "authoritative" {
		return false
	}
	if m["failure_class"] != "hard_kill_recovery" {
		return false
	}
	if strings.TrimSpace(m["interrupt_class"]) != InterruptClassHardKillRecovery {
		return false
	}
	if kind == "terminal" {
		term := strings.TrimSpace(firstNonEmpty(ev.Terminal, m["terminal"]))
		if !strings.EqualFold(term, string(workgraph.TermCancelled)) &&
			!strings.EqualFold(term, string(workgraph.TermFailed)) {
			return false
		}
	}
	return true
}

// newInterruptID returns a durable interrupt pairing id (stable for attempt+gen).
func newInterruptID(attemptID string, gen int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("interrupt|%s|%d|%d", attemptID, gen, time.Now().UnixNano())))
	return "iint_" + hex.EncodeToString(sum[:12])
}

// processProofClass classifies provider process observability for recovery.
type processProofClass int

const (
	processProofDead processProofClass = iota
	processProofExactLive
	processProofObservableReused // pid alive but identity no longer matches
	processProofUnobservable     // alive and identity cannot be proven either way
)

func (c processProofClass) String() string {
	switch c {
	case processProofDead:
		return "dead"
	case processProofExactLive:
		return "exact_live"
	case processProofObservableReused:
		return "observable_reused"
	case processProofUnobservable:
		return "unobservable"
	default:
		return "unknown"
	}
}

// classifyProviderProcess uses typed process.ClassifySnapshot — never string-parses errors.
// Only dead or observable-reused may continue without kill; unobservable fails closed.
func classifyProviderProcess(auth storage.ProviderExecutionAuthority) processProofClass {
	ident := process.Identity{
		PID: auth.ProviderPID, PGID: auth.ProviderPGID,
		ProcessBirthIdentity: auth.ProcessBirthIdentity,
		ExecutableIdentity:   auth.ExecutableIdentity,
	}
	switch process.ClassifySnapshot(ident) {
	case process.VerifyDead:
		return processProofDead
	case process.VerifyExactLive:
		return processProofExactLive
	case process.VerifyMismatch:
		return processProofObservableReused
	default:
		return processProofUnobservable
	}
}

// Spawn-phase constants are storage-row fields (same write as Persist), not events.
// Re-export storage names for workflowrun call sites.
const (
	SpawnPhaseLegacyUnknown      = storage.SpawnPhaseLegacyUnknown
	SpawnPhaseAuthorityPersisted = storage.SpawnPhaseAuthorityPersisted
	SpawnPhasePIDEventPersisted  = storage.SpawnPhasePIDEventPersisted
	SpawnPhasePIDEventFailed     = storage.SpawnPhasePIDEventFailed
)

// RecoverOptions configures authoritative hard-kill recovery.
type RecoverOptions struct {
	HomeDir   string
	ProjectID string
	RunID     string
	// Now is for authority timestamps only — death waits use real time.Now.
	Now func() time.Time
	// WaitAlive max wait for guardian/process death before identity-safe kill.
	WaitAlive time.Duration
	// KillAfterVerify when true and still alive after wait, KillGroup after VerifySnapshot.
	KillAfterVerify bool
	// FailAfter crash-window: after_interrupt|after_terminal|after_claim_close|after_authority_complete.
	// Failpoint fires AFTER the named action succeeds.
	FailAfter string
	// Plan/graph identity for recovery appends (required when appending events).
	PlanDigest  string
	GraphDigest string
	// OnKillGroup optional spy for tests (defaults to process.KillGroup).
	OnKillGroup func(pgid int) error
}

// recoverMode selects mutation policy for one attempt.
type recoverMode int

const (
	// recoverModeNormal: exact terminal already exists — preserve terminal+evidence only.
	recoverModeNormal recoverMode = iota
	// recoverModeHard: launch+pid, no terminal — authoritative interrupt+hard terminal.
	recoverModeHard
	// recoverModePrePID: authority persisted, pid append never landed (typed exception).
	recoverModePrePID
)

type recoverCandidate struct {
	workItemID string
	attemptID  string
	gen        int
	auth       storage.ProviderExecutionAuthority
	claim      *workclaim.Claim
	pidEv      Event // required except recoverModePrePID
	hasPID     bool
	mode       recoverMode
	// Frozen process proof from phase 1 (before any mutation).
	proofClass processProofClass
	// Identity copied from canonical launch/claim (no empty opts fallback).
	taskClass string
	ccd       string
	planDig   string
	graphDig  string
	graphID   string
	graphVer  int
	// Non-secret route fields from launch payload (all required nonempty).
	route map[string]string
	// needInterrupt / needTerminal / needClaimClose / needAuthComplete
	needInterrupt        bool
	needTerminal         bool
	needClaimClose       bool
	needAuthComplete     bool
	needPhaseToPersisted bool // authority_persisted+PID: fenced transition before complete
	// Exact terminal + evidence to converge claim/authority to (byte-for-byte preserve).
	finalTerminal string
	finalEvidence string // may be empty for failed/cancelled when durable event had empty
	interruptID   string // durable pair key for hard/service interrupt ↔ terminal
	// alreadyDone when all required steps complete.
	alreadyDone bool
}

// RecoverOpenLaunchInterruptsAuthoritative is two-phase:
//  1. Load+validate ALL open/recoverable attempts (no side effects).
//  2. Apply death-proof recovery mutations only if every candidate is valid.
//
// Idempotent: interrupt/terminal/claim/authority complete are each at most once per attempt.
func RecoverOpenLaunchInterruptsAuthoritative(elog *EventLog, opts RecoverOptions) (int, error) {
	if elog == nil {
		return 0, nil
	}
	projectID := strings.TrimSpace(opts.ProjectID)
	runID := strings.TrimSpace(opts.RunID)
	if projectID == "" || runID == "" {
		return 0, fmt.Errorf("workflowrun: recover requires project_id and run_id")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	wait := opts.WaitAlive
	if wait <= 0 {
		wait = 8 * time.Second
	}

	events, err := elog.ReadAllForRun(projectID, runID)
	if err != nil {
		return 0, err
	}
	for i, ev := range events {
		if err := ValidateChildEventIdentity(ev); err != nil {
			return 0, fmt.Errorf("workflowrun: recover pre-append identity line %d: %w", i, err)
		}
	}
	if err := ValidateEventStreamInvariants(events); err != nil {
		return 0, err
	}

	ctx := context.Background()
	if strings.TrimSpace(opts.HomeDir) == "" {
		// No durable home: nothing to recover authoritatively.
		if len(OpenLaunchesWithoutTerminal(events)) > 0 {
			return 0, fmt.Errorf("workflowrun: recover requires durable home for authority")
		}
		return 0, nil
	}

	store, oerr := OpenAuthorityStore(ctx, opts.HomeDir, projectID, runID, now)
	if oerr != nil {
		return 0, fmt.Errorf("workflowrun: open authority store for recover: %w", oerr)
	}
	defer store.Close()

	var cs *workclaim.Store
	runDir, rerr := RunDurableDir(opts.HomeDir, projectID, runID)
	if rerr != nil {
		return 0, rerr
	}
	claimPath := filepath.Join(runDir, "workclaims.json")
	if _, serr := os.Stat(claimPath); serr == nil {
		opened, cerr := workclaim.OpenPath(claimPath, now)
		if cerr != nil {
			return 0, fmt.Errorf("workflowrun: open claim store for recover: %w", cerr)
		}
		cs = opened
	}

	// Phase 1: collect + fully validate + classify ALL candidates — zero mutations.
	// Any unrecoverable candidate fails the entire recovery before any kill/append/close.
	candidates, verr := validateRecoverCandidates(ctx, events, store, cs, projectID, runID, opts.KillAfterVerify)
	if verr != nil {
		return 0, verr
	}
	if len(candidates) == 0 {
		return 0, nil
	}

	// Phase 2a: resolve process death for every candidate that still needs it,
	// using frozen phase-1 proof classes — still zero durable event/claim mutations.
	n := 0
	killFn := opts.OnKillGroup
	if killFn == nil {
		killFn = process.KillGroup
	}
	for i := range candidates {
		c := &candidates[i]
		if c.alreadyDone {
			continue
		}
		if err := ensureProviderDeadWithClass(c.auth, c.proofClass, wait, opts.KillAfterVerify && c.mode == recoverModeHard, killFn); err != nil {
			return 0, err // zero durable mutations yet
		}
		// Re-classify after wait/kill; any unobservable/exact-live remaining fails before durable work.
		c.proofClass = classifyProviderProcess(c.auth)
		switch c.proofClass {
		case processProofDead, processProofObservableReused:
			// ok
		default:
			return 0, fmt.Errorf("workflowrun: recover %s: process still %s after wait/kill (fail before durable mutation)", c.attemptID, c.proofClass)
		}
	}

	// Phase 2b: durable mutations only after every candidate process-proof is dead/reused.
	for _, c := range candidates {
		if c.alreadyDone {
			continue
		}
		termState := c.finalTerminal
		evidence := c.finalEvidence // exact preserve; may be empty for failed/cancelled
		if termState == "" {
			return n, fmt.Errorf("workflowrun: recover %s: empty final terminal", c.attemptID)
		}
		if strings.EqualFold(termState, string(workgraph.TermSucceeded)) && evidence == "" {
			return n, fmt.Errorf("workflowrun: recover %s: succeeded requires evidence", c.attemptID)
		}

		// Canonical launch identity only — no opts.PlanDigest fallback for stamps.
		planDig := strings.TrimSpace(c.planDig)
		graphDig := strings.TrimSpace(c.graphDig)
		if planDig == "" || c.taskClass == "" || c.ccd == "" {
			return n, fmt.Errorf("workflowrun: recover %s: missing canonical launch identity plan/class/ccd", c.attemptID)
		}
		validateRecoveryEv := func(ev Event) error {
			if err := ValidateChildEventIdentity(ev); err != nil {
				return err
			}
			itemOK := map[string]bool{c.workItemID: true}
			if err := ValidateChildEventIdentityForPlan(ev, planDig, runID, itemOK); err != nil {
				return err
			}
			return nil
		}

		// Crash window: authority_persisted + PID → one legal fenced phase transition first.
		if c.needPhaseToPersisted && strings.TrimSpace(c.auth.CompletedAt) == "" {
			if err := TransitionChildSpawnPhase(ctx, store, projectID, runID, c.attemptID, c.auth.OwnerID, c.auth.ClaimGeneration, SpawnPhasePIDEventPersisted, now()); err != nil {
				return n, fmt.Errorf("workflowrun: recover phase transition %s: %w", c.attemptID, err)
			}
			c.auth.SpawnPhase = SpawnPhasePIDEventPersisted
		}

		intID := strings.TrimSpace(c.interruptID)
		if c.needInterrupt {
			if c.mode != recoverModeHard {
				return n, fmt.Errorf("workflowrun: recover %s: interrupt only allowed in hard-recovery mode", c.attemptID)
			}
			if intID == "" {
				intID = newInterruptID(c.attemptID, c.gen)
			}
			payload := map[string]string{
				"pid":                    fmt.Sprintf("%d", c.auth.ProviderPID),
				"pgid":                   fmt.Sprintf("%d", c.auth.ProviderPGID),
				"process_birth_identity": c.auth.ProcessBirthIdentity,
				"executable_identity":    c.auth.ExecutableIdentity,
				"worktree_path":          c.auth.WorktreePath,
				"log_path":               c.auth.LogPath,
				"recovery":               "authoritative",
				"owner_id":               c.auth.OwnerID,
				"failure_class":          "hard_kill_recovery",
				"interrupt_class":        InterruptClassHardKillRecovery,
				"interrupt_id":           intID,
				"terminal":               termState,
			}
			for k, v := range c.route {
				if v != "" {
					payload[k] = v
				}
			}
			payload = stampChildIdentityPayload(payload, projectID, runID, c.graphID, c.graphVer,
				c.workItemID, c.attemptID, c.gen, planDig, graphDig, c.taskClass, c.ccd)
			ev := Event{
				ProjectID: projectID, RunID: runID,
				Kind: "interrupt", WorkItemID: c.workItemID, AttemptID: c.attemptID, Generation: c.gen,
				GraphID: c.graphID, GraphVersion: c.graphVer,
				PID: c.auth.ProviderPID, Terminal: termState,
				ExecutionPlanDigest: planDig, GraphDigest: graphDig,
				TaskClass: c.taskClass, ChildContractDigest: c.ccd,
				FailureClass: "hard_kill_recovery",
				Message:      "authoritative hard-kill recovery",
				Payload:      eventJSONPayload(payload),
			}
			if err := validateRecoveryEv(ev); err != nil {
				return n, fmt.Errorf("recover interrupt identity: %w", err)
			}
			if _, err := elog.Append(ev); err != nil {
				return n, err
			}
			var rerr error
			events, rerr = elog.ReadAllForRun(projectID, runID)
			if rerr != nil {
				return n, rerr
			}
			if err := ValidateEventStreamInvariants(events); err != nil {
				return n, fmt.Errorf("recover after interrupt stream: %w", err)
			}
			if opts.FailAfter == "after_interrupt" {
				return n, fmt.Errorf("workflowrun: test failpoint after_interrupt")
			}
		}

		if c.needTerminal {
			if c.mode == recoverModeNormal {
				return n, fmt.Errorf("workflowrun: recover %s: must not append terminal in normal convergence", c.attemptID)
			}
			payload := map[string]string{
				"terminal": termState, "output_evidence": evidence,
			}
			if c.mode == recoverModeHard {
				if intID == "" {
					intID = newInterruptID(c.attemptID, c.gen)
				}
				payload["failure_class"] = "hard_kill_recovery"
				payload["recovery"] = "authoritative"
				payload["interrupt_class"] = InterruptClassHardKillRecovery
				payload["interrupt_id"] = intID
			} else if c.mode == recoverModePrePID {
				payload["failure_class"] = "pid_event_failed"
			}
			for k, v := range c.route {
				if v != "" {
					payload[k] = v
				}
			}
			payload = stampChildIdentityPayload(payload, projectID, runID, c.graphID, c.graphVer,
				c.workItemID, c.attemptID, c.gen, planDig, graphDig, c.taskClass, c.ccd)
			msg := "hard_kill_recovery"
			if c.mode == recoverModePrePID {
				msg = "pid_event_failed"
			}
			fc := payload["failure_class"]
			ev := Event{
				ProjectID: projectID, RunID: runID,
				Kind: "terminal", WorkItemID: c.workItemID, AttemptID: c.attemptID, Generation: c.gen,
				GraphID: c.graphID, GraphVersion: c.graphVer,
				Terminal: termState, Evidence: evidence,
				ExecutionPlanDigest: planDig, GraphDigest: graphDig,
				TaskClass: c.taskClass, ChildContractDigest: c.ccd,
				FailureClass: fc,
				Message:      msg,
				Payload:      eventJSONPayload(payload),
			}
			if err := validateRecoveryEv(ev); err != nil {
				return n, fmt.Errorf("recover terminal identity: %w", err)
			}
			if _, err := elog.Append(ev); err != nil {
				return n, err
			}
			var rerr error
			events, rerr = elog.ReadAllForRun(projectID, runID)
			if rerr != nil {
				return n, rerr
			}
			if err := ValidateEventStreamInvariants(events); err != nil {
				return n, fmt.Errorf("recover after terminal stream: %w", err)
			}
			if opts.FailAfter == "after_terminal" {
				return n, fmt.Errorf("workflowrun: test failpoint after_terminal")
			}
		}

		if c.needClaimClose && cs != nil && c.claim != nil &&
			(c.claim.State == workclaim.StateClaimed || c.claim.State == workclaim.StateRunning) {
			if _, err := cs.Close(workclaim.CloseRequest{
				ClaimID: c.claim.ClaimID, Generation: c.claim.Generation,
				ExecutorID: firstNonEmpty(c.claim.ExecutorID, "workflowrun"), AttemptID: c.attemptID,
				Terminal: workgraph.TerminalState(termState), OutputEvidence: evidence,
			}); err != nil {
				return n, fmt.Errorf("workflowrun: recover claim close %s: %w", c.attemptID, err)
			}
			if opts.FailAfter == "after_claim_close" {
				return n, fmt.Errorf("workflowrun: test failpoint after_claim_close")
			}
		}

		if c.needAuthComplete && strings.TrimSpace(c.auth.CompletedAt) == "" {
			var aerr error
			if c.mode == recoverModePrePID {
				aerr = CompleteChildExecutionAuthorityPrePIDRecovery(ctx, store, projectID, runID, c.attemptID, c.auth.OwnerID, c.auth.ClaimGeneration, termState, now())
			} else {
				aerr = CompleteChildExecutionAuthority(ctx, store, projectID, runID, c.attemptID, c.auth.OwnerID, c.auth.ClaimGeneration, termState, now())
			}
			if aerr != nil {
				return n, fmt.Errorf("workflowrun: recover authority complete %s: %w", c.attemptID, aerr)
			}
			if opts.FailAfter == "after_authority_complete" {
				return n, fmt.Errorf("workflowrun: test failpoint after_authority_complete")
			}
		}
		n++
	}

	// Re-read and validate full stream after convergence.
	finalEvents, ferr := elog.ReadAllForRun(projectID, runID)
	if ferr != nil {
		return n, ferr
	}
	if err := ValidateEventStreamInvariants(finalEvents); err != nil {
		return n, fmt.Errorf("workflowrun: recover post-convergence stream: %w", err)
	}
	return n, nil
}

func hasKindForAttempt(events []Event, kind, workItemID, attemptID string) bool {
	for _, ev := range events {
		if ev.Kind == kind && ev.WorkItemID == workItemID && ev.AttemptID == attemptID {
			return true
		}
	}
	return false
}

func validateRecoverCandidates(
	ctx context.Context,
	events []Event,
	store storage.Store,
	cs *workclaim.Store,
	projectID, runID string,
	killAfterVerify bool,
) ([]recoverCandidate, error) {
	type key struct{ id, att string }
	keySet := map[string]key{}
	addKey := func(id, att string) {
		if id == "" || att == "" {
			return
		}
		keySet[attemptKey(id, att)] = key{id, att}
	}
	for id, att := range OpenLaunchesWithoutTerminal(events) {
		addKey(id, att)
	}
	pidByAttempt := map[string]Event{}
	launchByAttempt := map[string]Event{}
	termByAttempt := map[string]Event{}
	intByAttempt := map[string]Event{}
	for _, ev := range events {
		if ev.WorkItemID == "" || ev.AttemptID == "" {
			continue
		}
		k := attemptKey(ev.WorkItemID, ev.AttemptID)
		switch ev.Kind {
		case "pid":
			pidByAttempt[k] = ev
		case "launch":
			launchByAttempt[k] = ev
		case "terminal":
			termByAttempt[k] = ev
			addKey(ev.WorkItemID, ev.AttemptID) // any terminal may need claim/auth convergence
		case "interrupt":
			intByAttempt[k] = ev
			if isAuthoritativeHardRecoveryEvent(ev) {
				addKey(ev.WorkItemID, ev.AttemptID)
			}
		}
	}
	claimByAttempt := map[string]workclaim.Claim{}
	if cs != nil {
		for _, c := range cs.AllClaims() {
			if c.WorkItemID != "" && c.AttemptID != "" {
				k := attemptKey(c.WorkItemID, c.AttemptID)
				claimByAttempt[k] = c
				// Open claims and closed claims with incomplete authority are candidates.
				addKey(c.WorkItemID, c.AttemptID)
			}
		}
	}

	auths, aerr := storage.ListProviderExecutionAuthorities(ctx, store, projectID, runID)
	if aerr != nil {
		return nil, fmt.Errorf("workflowrun: recover list authorities: %w", aerr)
	}
	workItemByAttempt := map[string]string{}
	for _, ev := range events {
		if ev.AttemptID != "" && ev.WorkItemID != "" {
			workItemByAttempt[ev.AttemptID] = ev.WorkItemID
		}
	}
	for _, c := range claimByAttempt {
		if c.AttemptID != "" && c.WorkItemID != "" {
			workItemByAttempt[c.AttemptID] = c.WorkItemID
		}
	}
	authByAttempt := map[string]storage.ProviderExecutionAuthority{}
	for _, a := range auths {
		att := strings.TrimSpace(a.AttemptID)
		if att == "" {
			continue
		}
		authByAttempt[att] = a
		wi := workItemByAttempt[att]
		if wi == "" {
			if strings.TrimSpace(a.CompletedAt) == "" {
				return nil, fmt.Errorf("workflowrun: recover incomplete authority attempt %q: missing work_item_id binding", att)
			}
			continue
		}
		addKey(wi, att)
	}

	if len(keySet) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var out []recoverCandidate
	for _, k := range keys {
		pair := keySet[k]
		id, att := pair.id, pair.att
		gen, gerr := ClaimGenerationFromAttemptID(att)
		if gerr != nil {
			return nil, fmt.Errorf("workflowrun: recover validate %s: %w", id, gerr)
		}

		auth, hasAuth := authByAttempt[att]
		if !hasAuth {
			loaded, lerr := storage.LoadProviderExecutionAuthority(ctx, store, projectID, runID, att)
			if lerr != nil {
				if !errors.Is(lerr, sql.ErrNoRows) {
					return nil, fmt.Errorf("workflowrun: recover validate %s/%s: authority load: %w", id, att, lerr)
				}
				// Durable launch/claim without authority is ambiguous corruption — not a Fake exemption.
				// Fail closed before any mutation; never select gN+1 for this state.
				return nil, fmt.Errorf("workflowrun: recover validate %s/%s: missing authority for durable launch/claim (ambiguous; fail closed before mutation; diagnostic=no_authority)", id, att)
			}
			auth = loaded
		}
		if err := validateAuthorityForRecover(auth, projectID, runID, att, int64(gen)); err != nil {
			return nil, fmt.Errorf("workflowrun: recover validate %s/%s: %w", id, att, err)
		}

		le, hasLaunch := launchByAttempt[k]
		if !hasLaunch {
			return nil, fmt.Errorf("workflowrun: recover validate %s: missing required launch event", att)
		}
		planDig := strings.TrimSpace(le.ExecutionPlanDigest)
		graphDig := strings.TrimSpace(le.GraphDigest)
		taskClass := strings.TrimSpace(le.TaskClass)
		ccd := strings.TrimSpace(le.ChildContractDigest)
		if planDig == "" || graphDig == "" || taskClass == "" || ccd == "" {
			return nil, fmt.Errorf("workflowrun: recover validate %s: launch missing required plan/graph/class/ccd", att)
		}
		// Canonical attempt binding.
		wantAtt := AttemptID(id, planDig, runID, ParseAttemptGeneration(att))
		if att != wantAtt {
			return nil, fmt.Errorf("workflowrun: recover validate %s: attempt_id %q != canonical %q", id, att, wantAtt)
		}
		route, rerr := requireLaunchRoutePayload(le)
		if rerr != nil {
			return nil, fmt.Errorf("workflowrun: recover validate %s: %w", att, rerr)
		}

		// Claim: required and fully compared (all identity fields nonempty).
		c, hasClaim := claimByAttempt[k]
		if !hasClaim {
			return nil, fmt.Errorf("workflowrun: recover validate %s: missing claim", att)
		}
		if strings.TrimSpace(c.ClaimID) == "" {
			return nil, fmt.Errorf("workflowrun: recover validate %s: missing claim_id", att)
		}
		if strings.TrimSpace(c.ProjectID) == "" || strings.TrimSpace(c.ProjectID) != strings.TrimSpace(projectID) {
			return nil, fmt.Errorf("workflowrun: recover validate %s: claim project required exact match %q", att, projectID)
		}
		if strings.TrimSpace(c.GraphID) == "" {
			return nil, fmt.Errorf("workflowrun: recover validate %s: claim graph_id required", att)
		}
		if c.GraphVersion <= 0 {
			return nil, fmt.Errorf("workflowrun: recover validate %s: claim graph_version required", att)
		}
		if strings.TrimSpace(c.PlanDigest) == "" || c.PlanDigest != planDig {
			return nil, fmt.Errorf("workflowrun: recover validate %s: claim plan_digest %q != launch %q", att, c.PlanDigest, planDig)
		}
		if strings.TrimSpace(c.GraphDigest) == "" || c.GraphDigest != graphDig {
			return nil, fmt.Errorf("workflowrun: recover validate %s: claim graph_digest %q != launch %q", att, c.GraphDigest, graphDig)
		}
		if c.WorkItemID != id || c.AttemptID != att {
			return nil, fmt.Errorf("workflowrun: recover validate %s: claim work/attempt mismatch", att)
		}
		if c.Generation != int64(gen) {
			return nil, fmt.Errorf("workflowrun: recover validate %s: claim gen %d != attempt gen %d", att, c.Generation, gen)
		}
		if strings.TrimSpace(c.ExecutorID) != WorkflowrunExecutorID {
			return nil, fmt.Errorf("workflowrun: recover validate %s: claim executor %q want exact %q", att, c.ExecutorID, WorkflowrunExecutorID)
		}
		if c.State != workclaim.StateClaimed && c.State != workclaim.StateRunning && c.State != workclaim.StateClosed {
			return nil, fmt.Errorf("workflowrun: recover validate %s: unknown claim state %q", att, c.State)
		}
		if strings.TrimSpace(c.TaskClass) == "" || c.TaskClass != taskClass {
			return nil, fmt.Errorf("workflowrun: recover validate %s: claim task_class required exact match", att)
		}
		if strings.TrimSpace(c.ChildContractDigest) == "" || c.ChildContractDigest != ccd {
			return nil, fmt.Errorf("workflowrun: recover validate %s: claim child_contract_digest required exact match", att)
		}
		wantOwner := AuthorityOwnerFromClaimID(c.ClaimID)
		if wantOwner == "" || strings.TrimSpace(auth.OwnerID) != wantOwner {
			return nil, fmt.Errorf("workflowrun: recover validate %s: owner %q != %q", att, auth.OwnerID, wantOwner)
		}
		if auth.ClaimGeneration != c.Generation {
			return nil, fmt.Errorf("workflowrun: recover validate %s: authority gen %d != claim gen %d", att, auth.ClaimGeneration, c.Generation)
		}

		pe, hasPID := pidByAttempt[k]
		te, hasTerm := termByAttempt[k]
		ie, hasInt := intByAttempt[k]
		authDone := strings.TrimSpace(auth.CompletedAt) != ""
		claimOpen := c.State == workclaim.StateClaimed || c.State == workclaim.StateRunning
		claimClosed := c.State == workclaim.StateClosed

		// Closed claim without event terminal is corruption even if claim carries a terminal.
		if claimClosed && !hasTerm {
			return nil, fmt.Errorf("workflowrun: recover validate %s: closed claim without matching event terminal (corruption; fail before mutation)", att)
		}
		// Completed authority without matching event terminal is corruption.
		if authDone && !hasTerm {
			return nil, fmt.Errorf("workflowrun: recover validate %s: completed authority without matching event terminal (corruption; fail before mutation)", att)
		}
		if claimClosed && strings.TrimSpace(string(c.Terminal)) == "" {
			return nil, fmt.Errorf("workflowrun: recover validate %s: closed claim without claim terminal", att)
		}

		// Mode selection (state machine A–D). Never infer prePID solely from missing PID.
		// Empty/legacy_unknown is NEVER recoverable.
		var mode recoverMode
		var finalTerm, finalEvidence string
		needInt, needTerm, needClaim, needAuth := false, false, claimOpen, !authDone
		needPhaseToPersisted := false
		phase := strings.TrimSpace(auth.SpawnPhase)
		if phase == "" {
			phase = SpawnPhaseLegacyUnknown
		}
		// Exact spawn-phase combinations (fail closed on contradiction).
		if phase == SpawnPhaseLegacyUnknown {
			return nil, fmt.Errorf("workflowrun: recover validate %s: spawn_phase=legacy_unknown always fails closed", att)
		}
		if phase == SpawnPhasePIDEventFailed && hasPID {
			return nil, fmt.Errorf("workflowrun: recover validate %s: pid_event_failed with PID event is contradiction", att)
		}
		if phase == SpawnPhasePIDEventPersisted && !hasPID {
			return nil, fmt.Errorf("workflowrun: recover validate %s: pid_event_persisted without PID is contradiction", att)
		}
		// authority_persisted + PID = crash between PID append and phase transition —
		// one legal fenced phase transition must run before any completion.
		if phase == SpawnPhaseAuthorityPersisted && hasPID {
			needPhaseToPersisted = true
		}
		// Typed pre-PID: only exact authority_persisted or pid_event_failed, and no durable pid.
		typedPrePID := !hasPID && (phase == SpawnPhaseAuthorityPersisted || phase == SpawnPhasePIDEventFailed)

		switch {
		case hasTerm && !isAuthoritativeHardRecoveryEvent(te):
			// B: NORMAL CONVERGENCE — preserve exact terminal + evidence byte-for-byte.
			mode = recoverModeNormal
			finalTerm = strings.TrimSpace(te.Terminal)
			finalEvidence = te.Evidence // exact, including empty for failed/cancelled
			if finalTerm == "" {
				return nil, fmt.Errorf("workflowrun: recover validate %s: normal terminal empty", att)
			}
			if strings.EqualFold(finalTerm, string(workgraph.TermSucceeded)) && strings.TrimSpace(finalEvidence) == "" {
				return nil, fmt.Errorf("workflowrun: recover validate %s: succeeded terminal missing evidence", att)
			}
			needInt = false
			needTerm = false
			// Cross-store exact agreement for closed claim / completed authority.
			if claimClosed {
				if strings.TrimSpace(string(c.Terminal)) != finalTerm {
					return nil, fmt.Errorf("workflowrun: recover validate %s: closed claim terminal %q != event %q", att, c.Terminal, finalTerm)
				}
				if c.OutputEvidence != finalEvidence {
					return nil, fmt.Errorf("workflowrun: recover validate %s: closed claim evidence %q != event %q", att, c.OutputEvidence, finalEvidence)
				}
			}
			if authDone {
				if strings.TrimSpace(auth.TerminalState) != finalTerm {
					return nil, fmt.Errorf("workflowrun: recover validate %s: completed authority terminal %q != event %q", att, auth.TerminalState, finalTerm)
				}
			}
			if hasInt && isAuthoritativeHardRecoveryEvent(ie) {
				return nil, fmt.Errorf("workflowrun: recover validate %s: hard-recovery interrupt after normal terminal", att)
			}

		case hasTerm && isAuthoritativeHardRecoveryEvent(te):
			// Hard-recovery terminal MUST have prior authoritative interrupt (D).
			if !hasInt || !isAuthoritativeHardRecoveryEvent(ie) {
				return nil, fmt.Errorf("workflowrun: recover validate %s: hard-recovery terminal without prior authoritative interrupt (corruption)", att)
			}
			// Existing hard terminal with empty evidence is corruption — never synthesize.
			if strings.TrimSpace(te.Evidence) == "" {
				return nil, fmt.Errorf("workflowrun: recover validate %s: hard-recovery terminal empty evidence (corruption; never synthesize)", att)
			}
			// interrupt_id must pair interrupt ↔ terminal.
			if err := validateInterruptTerminalPair(ie, te); err != nil {
				return nil, fmt.Errorf("workflowrun: recover validate %s: %w", att, err)
			}
			mode = recoverModeHard
			finalTerm = strings.TrimSpace(te.Terminal)
			finalEvidence = te.Evidence // exact preserve; no synthesis
			needInt = false
			needTerm = false
			// Hard-recovery terminal must exact-match closed claim and completed authority.
			if claimClosed {
				if strings.TrimSpace(string(c.Terminal)) != finalTerm {
					return nil, fmt.Errorf("workflowrun: recover validate %s: closed claim terminal %q != hard event %q", att, c.Terminal, finalTerm)
				}
				if c.OutputEvidence != finalEvidence {
					return nil, fmt.Errorf("workflowrun: recover validate %s: closed claim evidence %q != hard event %q", att, c.OutputEvidence, finalEvidence)
				}
			}
			if authDone {
				if strings.TrimSpace(auth.TerminalState) != finalTerm {
					return nil, fmt.Errorf("workflowrun: recover validate %s: completed authority terminal %q != hard event %q", att, auth.TerminalState, finalTerm)
				}
			}

		case !hasTerm && hasPID:
			// C: HARD RECOVERY — launch+pid+authority, no terminal.
			// Contradictory phase: pid_event_failed+PID already rejected above.
			mode = recoverModeHard
			finalTerm = string(workgraph.TermCancelled)
			finalEvidence = "failed:hard_kill_recovery:" + id
			needInt = !hasInt
			needTerm = true
			if hasInt && !isAuthoritativeHardRecoveryEvent(ie) {
				return nil, fmt.Errorf("workflowrun: recover validate %s: non-authoritative interrupt without terminal", att)
			}
			if hasInt {
				needInt = false
				// Existing hard interrupt must carry complete structured identity (incl interrupt_id).
				if strings.TrimSpace(eventPayloadString(ie, "interrupt_id")) == "" {
					return nil, fmt.Errorf("workflowrun: recover validate %s: hard interrupt missing interrupt_id", att)
				}
			}

		case !hasTerm && !hasPID && typedPrePID:
			// Sole typed exception: authority_persisted or pid_event_failed, no PID.
			mode = recoverModePrePID
			finalTerm = string(workgraph.TermFailed)
			finalEvidence = "failed:pid_event:" + id
			needInt = false
			needTerm = true

		case !hasTerm && !hasPID:
			return nil, fmt.Errorf("workflowrun: recover validate %s: missing pid without typed authority spawn_phase=%q (fail before mutation)", att, phase)

		default:
			return nil, fmt.Errorf("workflowrun: recover validate %s: unclassifiable state", att)
		}

		// Do not complete authority in a contradictory/incomplete phase.
		if needAuth {
			switch phase {
			case SpawnPhasePIDEventPersisted:
				// normal complete
			case SpawnPhaseAuthorityPersisted:
				if hasPID {
					// crash window: require phase transition before complete
					needPhaseToPersisted = true
				} else if mode != recoverModePrePID {
					return nil, fmt.Errorf("workflowrun: recover validate %s: cannot complete authority_persisted outside pre-PID recovery", att)
				}
			case SpawnPhasePIDEventFailed:
				if mode != recoverModePrePID {
					return nil, fmt.Errorf("workflowrun: recover validate %s: cannot complete pid_event_failed outside pre-PID recovery", att)
				}
			default:
				return nil, fmt.Errorf("workflowrun: recover validate %s: cannot complete spawn_phase %q", att, phase)
			}
		}

		// PID required for all modes except typed PrePID — full identity compare.
		if mode != recoverModePrePID {
			if !hasPID {
				return nil, fmt.Errorf("workflowrun: recover validate %s: missing required pid event", att)
			}
			if err := ValidatePIDEventPayload(pe); err != nil {
				return nil, fmt.Errorf("workflowrun: recover validate %s pid payload: %w", att, err)
			}
			var m map[string]string
			if err := json.Unmarshal(pe.Payload, &m); err != nil {
				return nil, fmt.Errorf("workflowrun: recover validate %s pid payload json: %w", att, err)
			}
			if pe.PID != auth.ProviderPID || m["pid"] != fmt.Sprintf("%d", auth.ProviderPID) {
				return nil, fmt.Errorf("workflowrun: recover validate %s: pid mismatch event/authority", att)
			}
			if m["pgid"] != fmt.Sprintf("%d", auth.ProviderPGID) {
				return nil, fmt.Errorf("workflowrun: recover validate %s: pgid mismatch", att)
			}
			if strings.TrimSpace(m["process_birth_identity"]) != strings.TrimSpace(auth.ProcessBirthIdentity) {
				return nil, fmt.Errorf("workflowrun: recover validate %s: birth mismatch", att)
			}
			if strings.TrimSpace(m["executable_identity"]) != strings.TrimSpace(auth.ExecutableIdentity) {
				return nil, fmt.Errorf("workflowrun: recover validate %s: executable mismatch", att)
			}
			if strings.TrimSpace(m["observed_at"]) == "" {
				return nil, fmt.Errorf("workflowrun: recover validate %s: observed_at missing", att)
			}
			// worktree/log required exact match to authority.
			wp := strings.TrimSpace(m["worktree_path"])
			if wp == "" {
				return nil, fmt.Errorf("workflowrun: recover validate %s: pid worktree_path required", att)
			}
			if wp != strings.TrimSpace(auth.WorktreePath) {
				return nil, fmt.Errorf("workflowrun: recover validate %s: worktree_path mismatch", att)
			}
			lp := strings.TrimSpace(m["log_path"])
			if lp == "" {
				return nil, fmt.Errorf("workflowrun: recover validate %s: pid log_path required", att)
			}
			if lp != strings.TrimSpace(auth.LogPath) {
				return nil, fmt.Errorf("workflowrun: recover validate %s: log_path mismatch", att)
			}
			// Cross-event identity: full top-level + structured payload + launch route.
			if err := requireChildEventIdentityMatch(pe, projectID, runID, c.GraphID, c.GraphVersion, id, att, gen, planDig, graphDig, taskClass, ccd); err != nil {
				return nil, fmt.Errorf("workflowrun: recover validate %s pid identity: %w", att, err)
			}
			if err := requireEventRouteMatch(pe, route); err != nil {
				return nil, fmt.Errorf("workflowrun: recover validate %s pid route: %w", att, err)
			}
		}
		if err := requireChildEventIdentityMatch(le, projectID, runID, c.GraphID, c.GraphVersion, id, att, gen, planDig, graphDig, taskClass, ccd); err != nil {
			return nil, fmt.Errorf("workflowrun: recover validate %s launch identity: %w", att, err)
		}
		// Launch route already validated as requireLaunchRoutePayload; re-check completeness.
		if err := requireEventRouteMatch(le, route); err != nil {
			return nil, fmt.Errorf("workflowrun: recover validate %s launch route: %w", att, err)
		}
		if hasTerm {
			if err := requireChildEventIdentityMatch(te, projectID, runID, c.GraphID, c.GraphVersion, id, att, gen, planDig, graphDig, taskClass, ccd); err != nil {
				return nil, fmt.Errorf("workflowrun: recover validate %s terminal identity: %w", att, err)
			}
			if err := requireEventRouteMatch(te, route); err != nil {
				return nil, fmt.Errorf("workflowrun: recover validate %s terminal route: %w", att, err)
			}
			if err := requireEventPayloadSelfConsistent(te); err != nil {
				return nil, fmt.Errorf("workflowrun: recover validate %s terminal payload self: %w", att, err)
			}
		}
		if hasInt {
			if err := requireChildEventIdentityMatch(ie, projectID, runID, c.GraphID, c.GraphVersion, id, att, gen, planDig, graphDig, taskClass, ccd); err != nil {
				return nil, fmt.Errorf("workflowrun: recover validate %s interrupt identity: %w", att, err)
			}
			if err := requireEventRouteMatch(ie, route); err != nil {
				return nil, fmt.Errorf("workflowrun: recover validate %s interrupt route: %w", att, err)
			}
			if err := requireEventPayloadSelfConsistent(ie); err != nil {
				return nil, fmt.Errorf("workflowrun: recover validate %s interrupt payload self: %w", att, err)
			}
		}
		// Launch top-level GraphID/Version must equal claim.
		if strings.TrimSpace(le.GraphID) != strings.TrimSpace(c.GraphID) || le.GraphVersion != c.GraphVersion {
			return nil, fmt.Errorf("workflowrun: recover validate %s: launch graph %s/%d != claim %s/%d",
				att, le.GraphID, le.GraphVersion, c.GraphID, c.GraphVersion)
		}

		// Freeze process proof for EVERY candidate that still needs work (phase-1 whole-state).
		proof := classifyProviderProcess(auth)
		if needInt || needTerm || needClaim || needAuth {
			switch proof {
			case processProofDead, processProofObservableReused:
				// ok
			case processProofExactLive:
				// Only hard recovery may kill exact-live, and only when KillAfterVerify is set.
				if !(mode == recoverModeHard && killAfterVerify) {
					return nil, fmt.Errorf("workflowrun: recover validate %s: process exact-live (class frozen; fail before any mutation)", att)
				}
			case processProofUnobservable:
				return nil, fmt.Errorf("workflowrun: recover validate %s: process unobservable (fail before any mutation)", att)
			default:
				return nil, fmt.Errorf("workflowrun: recover validate %s: process class unknown", att)
			}
		}

		// Authority schema/paths completeness.
		if strings.TrimSpace(auth.SchemaVersion) == "" {
			return nil, fmt.Errorf("workflowrun: recover validate %s: authority schema_version required", att)
		}
		if strings.TrimSpace(auth.WorktreePath) == "" || strings.TrimSpace(auth.LogPath) == "" {
			return nil, fmt.Errorf("workflowrun: recover validate %s: authority worktree/log required", att)
		}

		// Completed authority with open claim: only if terminal values will agree.
		if authDone && claimOpen {
			if strings.TrimSpace(auth.TerminalState) == "" {
				return nil, fmt.Errorf("workflowrun: recover validate %s: completed authority missing terminal", att)
			}
			if hasTerm && strings.TrimSpace(auth.TerminalState) != strings.TrimSpace(te.Terminal) {
				return nil, fmt.Errorf("workflowrun: recover validate %s: completed authority terminal != event", att)
			}
			// Ensure we close with auth terminal when event matches.
			if finalTerm != "" && strings.TrimSpace(auth.TerminalState) != finalTerm {
				return nil, fmt.Errorf("workflowrun: recover validate %s: completed authority terminal conflict", att)
			}
		}
		// Completed authority without matching terminal when terminal exists.
		if authDone && hasTerm && strings.TrimSpace(auth.TerminalState) != strings.TrimSpace(te.Terminal) {
			return nil, fmt.Errorf("workflowrun: recover validate %s: completed authority terminal %q != event %q", att, auth.TerminalState, te.Terminal)
		}

		if !needInt && !needTerm && !needClaim && !needAuth {
			continue
		}

		// Exact-live completed authority is corruption if still our process.
		if authDone {
			switch classifyProviderProcess(auth) {
			case processProofExactLive:
				return nil, fmt.Errorf("workflowrun: recover validate %s: completed authority but process still exact-live", att)
			case processProofUnobservable:
				return nil, fmt.Errorf("workflowrun: recover validate %s: completed authority process unobservable (fail before mutation)", att)
			}
		}

		var claimPtr *workclaim.Claim
		if claimOpen {
			cp := c
			claimPtr = &cp
		}
		intID := ""
		if hasInt {
			intID = strings.TrimSpace(eventPayloadString(ie, "interrupt_id"))
		}
		out = append(out, recoverCandidate{
			workItemID: id, attemptID: att, gen: gen, auth: auth, claim: claimPtr,
			pidEv: pe, hasPID: hasPID, mode: mode, proofClass: proof,
			taskClass: taskClass, ccd: ccd, planDig: planDig, graphDig: graphDig,
			graphID: c.GraphID, graphVer: c.GraphVersion, route: route,
			needInterrupt: needInt, needTerminal: needTerm, needClaimClose: needClaim, needAuthComplete: needAuth,
			needPhaseToPersisted: needPhaseToPersisted,
			finalTerminal:        finalTerm, finalEvidence: finalEvidence, interruptID: intID, alreadyDone: false,
		})
	}
	return out, nil
}

// requireChildEventIdentityMatch enforces complete top-level + structured payload
// identity on durable recovery-relevant events. Empty/optional fields are corruption.
func requireChildEventIdentityMatch(ev Event, projectID, runID, graphID string, graphVer int, workItemID, attemptID string, gen int, planDig, graphDig, taskClass, ccd string) error {
	if strings.TrimSpace(ev.ProjectID) == "" || strings.TrimSpace(ev.ProjectID) != strings.TrimSpace(projectID) {
		return fmt.Errorf("project_id missing or mismatch event=%q want=%q", ev.ProjectID, projectID)
	}
	if strings.TrimSpace(ev.RunID) == "" || strings.TrimSpace(ev.RunID) != strings.TrimSpace(runID) {
		return fmt.Errorf("run_id missing or mismatch event=%q want=%q", ev.RunID, runID)
	}
	if strings.TrimSpace(ev.GraphID) == "" || strings.TrimSpace(ev.GraphID) != strings.TrimSpace(graphID) {
		return fmt.Errorf("graph_id missing or mismatch event=%q want=%q", ev.GraphID, graphID)
	}
	if ev.GraphVersion <= 0 || ev.GraphVersion != graphVer {
		return fmt.Errorf("graph_version missing or mismatch event=%d want=%d", ev.GraphVersion, graphVer)
	}
	if strings.TrimSpace(ev.WorkItemID) == "" || strings.TrimSpace(ev.WorkItemID) != workItemID {
		return fmt.Errorf("work_item_id missing or mismatch")
	}
	if strings.TrimSpace(ev.AttemptID) == "" || strings.TrimSpace(ev.AttemptID) != attemptID {
		return fmt.Errorf("attempt_id missing or mismatch")
	}
	if ev.Generation <= 0 || ev.Generation != gen {
		return fmt.Errorf("generation %d != %d", ev.Generation, gen)
	}
	if strings.TrimSpace(ev.ExecutionPlanDigest) == "" || ev.ExecutionPlanDigest != planDig {
		return fmt.Errorf("execution_plan_digest missing or mismatch")
	}
	if strings.TrimSpace(ev.GraphDigest) == "" || ev.GraphDigest != graphDig {
		return fmt.Errorf("graph_digest missing or mismatch")
	}
	if strings.TrimSpace(ev.TaskClass) == "" || ev.TaskClass != taskClass {
		return fmt.Errorf("task_class missing or mismatch")
	}
	if strings.TrimSpace(ev.ChildContractDigest) == "" || ev.ChildContractDigest != ccd {
		return fmt.Errorf("child_contract_digest missing or mismatch")
	}
	if err := requireStructuredChildIdentityPayload(ev, projectID, runID, graphID, graphVer, workItemID, attemptID, gen, planDig, graphDig, taskClass, ccd); err != nil {
		return err
	}
	return nil
}

// requireStructuredChildIdentityPayload requires complete structured identity keys
// exact to top-level/claim. Missing is corruption.
func requireStructuredChildIdentityPayload(ev Event, projectID, runID, graphID string, graphVer int, workItemID, attemptID string, gen int, planDig, graphDig, taskClass, ccd string) error {
	if len(ev.Payload) == 0 {
		return fmt.Errorf("structured identity payload required")
	}
	var m map[string]string
	if err := json.Unmarshal(ev.Payload, &m); err != nil {
		return fmt.Errorf("structured identity payload malformed: %w", err)
	}
	want := map[string]string{
		"project_id":            strings.TrimSpace(projectID),
		"run_id":                strings.TrimSpace(runID),
		"graph_id":              strings.TrimSpace(graphID),
		"graph_version":         fmt.Sprintf("%d", graphVer),
		"work_item_id":          strings.TrimSpace(workItemID),
		"attempt_id":            strings.TrimSpace(attemptID),
		"generation":            fmt.Sprintf("%d", gen),
		"execution_plan_digest": strings.TrimSpace(planDig),
		"graph_digest":          strings.TrimSpace(graphDig),
		"task_class":            strings.TrimSpace(taskClass),
		"child_contract_digest": strings.TrimSpace(ccd),
	}
	for k, w := range want {
		got := strings.TrimSpace(m[k])
		if got == "" {
			return fmt.Errorf("payload %s required nonempty", k)
		}
		if got != w {
			return fmt.Errorf("payload %s %q != %q", k, got, w)
		}
	}
	return nil
}

// requireEventRouteMatch requires every requiredRouteKeys field exact to canonical launch route.
func requireEventRouteMatch(ev Event, route map[string]string) error {
	if len(ev.Payload) == 0 {
		return fmt.Errorf("route payload required")
	}
	var m map[string]string
	if err := json.Unmarshal(ev.Payload, &m); err != nil {
		return fmt.Errorf("route payload malformed: %w", err)
	}
	for _, k := range requiredRouteKeys {
		want := strings.TrimSpace(route[k])
		if want == "" {
			return fmt.Errorf("canonical route field %q empty", k)
		}
		got := strings.TrimSpace(m[k])
		if got == "" {
			return fmt.Errorf("payload route field %q required", k)
		}
		if got != want {
			return fmt.Errorf("payload route field %q %q != launch %q", k, got, want)
		}
	}
	return nil
}

// requireEventPayloadSelfConsistent requires payload work_item/attempt/generation
// and terminal/class fields match top-level Event fields.
// Failure class rules (canonical = top-level FailureClass when set, else payload
// failure_class; never Message):
//   - succeeded terminal: class must be empty on both
//   - failed/cancelled terminal: class must be nonempty and top-level==payload
//   - typed interrupt: class must be nonempty and consistent with interrupt typing
func requireEventPayloadSelfConsistent(ev Event) error {
	if len(ev.Payload) == 0 {
		return fmt.Errorf("payload required for self-consistency")
	}
	var m map[string]string
	if err := json.Unmarshal(ev.Payload, &m); err != nil {
		return fmt.Errorf("payload malformed: %w", err)
	}
	if strings.TrimSpace(m["work_item_id"]) != strings.TrimSpace(ev.WorkItemID) {
		return fmt.Errorf("payload work_item_id != event")
	}
	if strings.TrimSpace(m["attempt_id"]) != strings.TrimSpace(ev.AttemptID) {
		return fmt.Errorf("payload attempt_id != event")
	}
	if strings.TrimSpace(m["generation"]) != fmt.Sprintf("%d", ev.Generation) {
		return fmt.Errorf("payload generation != event")
	}
	kind := strings.TrimSpace(ev.Kind)
	payloadFC := strings.TrimSpace(m["failure_class"])
	topFC := strings.TrimSpace(ev.FailureClass)
	if kind == "terminal" || kind == "interrupt" {
		if pt := strings.TrimSpace(m["terminal"]); pt != "" {
			if !strings.EqualFold(pt, strings.TrimSpace(ev.Terminal)) {
				return fmt.Errorf("payload terminal %q != event %q", pt, ev.Terminal)
			}
		} else if strings.TrimSpace(ev.Terminal) != "" {
			return fmt.Errorf("payload terminal required when event terminal set")
		}
	}
	if kind == "terminal" {
		term := strings.TrimSpace(ev.Terminal)
		if strings.EqualFold(term, string(workgraph.TermSucceeded)) {
			if topFC != "" || payloadFC != "" {
				return fmt.Errorf("succeeded terminal must have empty failure_class (top=%q payload=%q)", topFC, payloadFC)
			}
			return nil
		}
		if strings.EqualFold(term, string(workgraph.TermFailed)) ||
			strings.EqualFold(term, string(workgraph.TermCancelled)) {
			// Canonical class: prefer top-level; payload must match when both set.
			if topFC == "" && payloadFC == "" {
				return fmt.Errorf("%s terminal requires nonempty failure_class", term)
			}
			if topFC != "" && payloadFC != "" && topFC != payloadFC {
				return fmt.Errorf("failure_class top %q != payload %q", topFC, payloadFC)
			}
			if topFC != "" && payloadFC == "" {
				return fmt.Errorf("payload failure_class required to match top-level %q", topFC)
			}
			if topFC == "" && payloadFC != "" {
				// Payload-only is allowed as canonical when top-level omitted on older lines
				// only if we treat payload as sole source — require top-level for new rules.
				return fmt.Errorf("top-level FailureClass required for %s terminal (payload=%q)", term, payloadFC)
			}
			return nil
		}
	}
	if kind == "interrupt" {
		fc := eventFailureClass(ev)
		if fc == "" {
			return fmt.Errorf("interrupt requires nonempty failure_class")
		}
		if topFC != "" && payloadFC != "" && topFC != payloadFC {
			return fmt.Errorf("interrupt failure_class top %q != payload %q", topFC, payloadFC)
		}
		if payloadFC == "" {
			return fmt.Errorf("interrupt payload failure_class required")
		}
	}
	return nil
}

// stampChildIdentityPayload fills complete structured identity keys (required for recovery).
func stampChildIdentityPayload(m map[string]string, projectID, runID, graphID string, graphVer int, workItemID, attemptID string, gen int, planDig, graphDig, taskClass, ccd string) map[string]string {
	if m == nil {
		m = map[string]string{}
	}
	m["project_id"] = strings.TrimSpace(projectID)
	m["run_id"] = strings.TrimSpace(runID)
	m["graph_id"] = strings.TrimSpace(graphID)
	m["graph_version"] = fmt.Sprintf("%d", graphVer)
	m["work_item_id"] = strings.TrimSpace(workItemID)
	m["attempt_id"] = strings.TrimSpace(attemptID)
	m["generation"] = fmt.Sprintf("%d", gen)
	m["execution_plan_digest"] = strings.TrimSpace(planDig)
	m["graph_digest"] = strings.TrimSpace(graphDig)
	m["task_class"] = strings.TrimSpace(taskClass)
	m["child_contract_digest"] = strings.TrimSpace(ccd)
	return m
}

// validateInterruptTerminalPair requires identical interrupt_id/class/work/attempt/gen
// and matching terminal semantics for a typed interrupt → terminal pair.
// interrupt_id pairing is necessary but not sufficient — payload/top-level must agree.
func validateInterruptTerminalPair(ie, te Event) error {
	intID := strings.TrimSpace(eventPayloadString(ie, "interrupt_id"))
	termID := strings.TrimSpace(eventPayloadString(te, "interrupt_id"))
	if intID == "" || termID == "" {
		return fmt.Errorf("interrupt_id required on both interrupt and terminal")
	}
	if intID != termID {
		return fmt.Errorf("interrupt_id mismatch interrupt=%q terminal=%q", intID, termID)
	}
	if strings.TrimSpace(ie.WorkItemID) != strings.TrimSpace(te.WorkItemID) ||
		strings.TrimSpace(ie.AttemptID) != strings.TrimSpace(te.AttemptID) ||
		ie.Generation != te.Generation {
		return fmt.Errorf("interrupt/terminal work_item/attempt/generation mismatch")
	}
	if strings.TrimSpace(eventPayloadString(ie, "work_item_id")) != strings.TrimSpace(eventPayloadString(te, "work_item_id")) ||
		strings.TrimSpace(eventPayloadString(ie, "attempt_id")) != strings.TrimSpace(eventPayloadString(te, "attempt_id")) ||
		strings.TrimSpace(eventPayloadString(ie, "generation")) != strings.TrimSpace(eventPayloadString(te, "generation")) {
		return fmt.Errorf("interrupt/terminal payload work_item/attempt/generation mismatch")
	}
	if err := requireEventPayloadSelfConsistent(ie); err != nil {
		return fmt.Errorf("interrupt self: %w", err)
	}
	if err := requireEventPayloadSelfConsistent(te); err != nil {
		return fmt.Errorf("terminal self: %w", err)
	}
	ic := childInterruptClass(ie)
	tc := childInterruptClass(te)
	if ic == "" || tc == "" || ic != tc {
		return fmt.Errorf("interrupt/terminal class mismatch %q vs %q", ic, tc)
	}
	if ic == InterruptClassHardKillRecovery {
		if !isAuthoritativeHardRecoveryEvent(ie) || !isAuthoritativeHardRecoveryEvent(te) {
			return fmt.Errorf("hard pair requires complete authoritative structured payload on both")
		}
	}
	if ic == InterruptClassServiceForced {
		if !isServiceForcedInterruptEvent(ie) || !isServiceForcedInterruptEvent(te) {
			return fmt.Errorf("service forced pair requires complete structured payload on both")
		}
		if !strings.EqualFold(strings.TrimSpace(te.Terminal), string(workgraph.TermCancelled)) {
			return fmt.Errorf("service forced terminal must be cancelled")
		}
	}
	return nil
}

// requiredRouteKeys are non-secret launch route fields that must be present and nonempty.
var requiredRouteKeys = []string{
	"provider", "model", "depth", "permission",
	"account_ref", "install_ref", "window_kind", "reservation_id", "route_reason",
}

func requireLaunchRoutePayload(le Event) (map[string]string, error) {
	if len(le.Payload) == 0 {
		return nil, fmt.Errorf("launch route payload required")
	}
	var m map[string]string
	if err := json.Unmarshal(le.Payload, &m); err != nil {
		return nil, fmt.Errorf("launch route payload malformed: %w", err)
	}
	out := map[string]string{}
	for _, k := range requiredRouteKeys {
		v := strings.TrimSpace(m[k])
		if v == "" {
			return nil, fmt.Errorf("launch route field %q required nonempty", k)
		}
		out[k] = v
	}
	return out, nil
}

func validateAuthorityForRecover(auth storage.ProviderExecutionAuthority, projectID, runID, attemptID string, claimGen int64) error {
	if strings.TrimSpace(auth.SchemaVersion) != storage.ProviderExecutionAuthoritySchema {
		return fmt.Errorf("authority schema_version %q != %q", auth.SchemaVersion, storage.ProviderExecutionAuthoritySchema)
	}
	if strings.TrimSpace(auth.AuthorityID) == "" {
		return fmt.Errorf("authority authority_id required nonempty")
	}
	if auth.RecordVersion <= 0 {
		return fmt.Errorf("authority record_version must be positive")
	}
	if strings.TrimSpace(auth.ProjectID) != strings.TrimSpace(projectID) ||
		strings.TrimSpace(auth.RunID) != strings.TrimSpace(runID) ||
		strings.TrimSpace(auth.AttemptID) != strings.TrimSpace(attemptID) {
		return fmt.Errorf("authority project/run/attempt fence mismatch")
	}
	if auth.ClaimGeneration != claimGen {
		return fmt.Errorf("authority gen %d != attempt gen %d", auth.ClaimGeneration, claimGen)
	}
	if strings.TrimSpace(auth.OwnerID) == "" {
		return fmt.Errorf("authority owner_id missing")
	}
	if auth.ProviderPID <= 0 || auth.ProviderPGID <= 0 {
		return fmt.Errorf("authority pid/pgid incomplete")
	}
	if strings.TrimSpace(auth.ProcessBirthIdentity) == "" {
		return fmt.Errorf("authority process_birth_identity missing")
	}
	if strings.TrimSpace(auth.ExecutableIdentity) == "" {
		return fmt.Errorf("authority executable_identity missing")
	}
	if strings.TrimSpace(auth.StartedAt) == "" {
		return fmt.Errorf("authority started_at missing")
	}
	if strings.TrimSpace(auth.WorktreePath) == "" || strings.TrimSpace(auth.LogPath) == "" {
		return fmt.Errorf("authority worktree_path and log_path required")
	}
	if auth.IdentityAmbiguous {
		return fmt.Errorf("authority identity ambiguous: %s", auth.AmbiguityReason)
	}
	return nil
}

// ensureProviderDead uses real monotonic time.Now for wait deadlines — never a
// frozen event clock (opts.Now / t0).
func ensureProviderDead(auth storage.ProviderExecutionAuthority, wait time.Duration, killAfter bool, killFn func(int) error) error {
	return ensureProviderDeadWithClass(auth, classifyProviderProcess(auth), wait, killAfter, killFn)
}

// ensureProviderDeadWithClass applies a frozen phase-1 proof class then rechecks
// with wall clock. Unobservable never kills; exact-live waits then optional kill.
func ensureProviderDeadWithClass(auth storage.ProviderExecutionAuthority, frozen processProofClass, wait time.Duration, killAfter bool, killFn func(int) error) error {
	pid, pgid := auth.ProviderPID, auth.ProviderPGID
	if pid <= 0 {
		return fmt.Errorf("ensure dead: invalid pid")
	}
	switch frozen {
	case processProofDead, processProofObservableReused:
		return nil
	case processProofUnobservable:
		return fmt.Errorf("workflowrun: provider pid=%d unobservable (fail before mutation; zero kill)", pid)
	case processProofExactLive:
		// wait then optional kill
	default:
		return fmt.Errorf("workflowrun: provider pid=%d unknown process class", pid)
	}

	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		switch classifyProviderProcess(auth) {
		case processProofDead, processProofObservableReused:
			return nil
		case processProofUnobservable:
			return fmt.Errorf("workflowrun: provider pid=%d became unobservable during wait", pid)
		}
		time.Sleep(25 * time.Millisecond)
	}
	switch classifyProviderProcess(auth) {
	case processProofDead, processProofObservableReused:
		return nil
	case processProofUnobservable:
		return fmt.Errorf("workflowrun: provider pid=%d unobservable after wait", pid)
	}
	if !killAfter {
		return fmt.Errorf("workflowrun: provider still exact-live pid=%d (wait for guardian)", pid)
	}
	if killFn == nil {
		killFn = process.KillGroup
	}
	if err := killFn(pgid); err != nil {
		return fmt.Errorf("workflowrun: kill group %d: %w", pgid, err)
	}
	deadDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadDeadline) {
		switch classifyProviderProcess(auth) {
		case processProofDead, processProofObservableReused:
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	switch classifyProviderProcess(auth) {
	case processProofDead, processProofObservableReused:
		return nil
	default:
		return fmt.Errorf("workflowrun: provider pid %d still not dead after group kill (class=%s)", pid, classifyProviderProcess(auth))
	}
}
