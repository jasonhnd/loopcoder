# v0.9.0 Issue Drafts: P2 Visible Safe Runtime

Status: development-ready issue drafts; owner publication/assignment required

Publish only after P1 acceptance and owner approval. These issues consolidate
existing `agent`, `supervisedexec`, `progress`, `report`, `relay`, and host code
behind one runtime and one event truth. They must not create another supervisor
or claim user-visible delivery from a hidden database/stderr write.

## V090-012: Runtime facade over existing agent and supervised execution

**Metadata:** code, size S; depends on V090-011; labels `v0.9.0`, `runtime`,
`consolidation`; exclusive in the new runtime facade.

### Outcome and rationale

Define the one v0.9 runtime port used to start, observe, signal, and join a
top-level attempt while reusing proven process mechanics from `internal/agent`
and `internal/supervisedexec`. The facade prevents later work from calling five
provider runners and process helpers directly.

### Scope, constraints, and sequence

- Define immutable launch request, started identity, observation, signal, join,
  terminal evidence, and typed failure results.
- Adapt existing execution behind the port; add no provider-specific policy.
- Require injected clock/process observer/output sink and bounded contexts.
- Inventory every direct launch call and mark keep, compatibility-only, or later
  migration; do not migrate all callers in this issue.
- Implement fixture adapter first, then one existing generic command adapter.

### Acceptance criteria

1. A caller can launch, observe, signal, and join one fixture attempt through a
   provider-neutral interface with immutable launch inputs.
2. Success is returned only after terminal process evidence and join complete;
   provider prose cannot declare process completion.
3. Launch, observation, signal, and join failures have stable typed categories
   and preserve the strongest available evidence.
4. Existing low-level process mechanics are reused; no second process-group or
   PTY supervisor is introduced.
5. The PR includes a direct-launch disposition inventory for remaining callers.

### Verification, safety, and non-goals

Use fake executable modes from V090-003; focused runtime tests and remote race.
Failed launch owns no process; failed join remains nonterminal/attention-required.
Clear inherited provider/Git variables from fixtures. One test process, eight
children maximum. No real provider, resource policy, report scheduler, or CLI.
Done when P2 code can depend only on the facade.

---

## V090-013: Darwin process-tree identity and liveness

**Metadata:** code, size M; depends on V090-012; labels `v0.9.0`, `runtime`,
`darwin`, `process`; exclusive in process ownership/liveness.

### Outcome and rationale

Represent the actual owned process tree on macOS, distinguish PID reuse and
wrapper/child processes, and derive liveness from OS evidence. Provider output,
PTY activity, and heartbeats are observations, not execution authority.

### Scope, constraints, and sequence

- Persist root PID, process start identity, process-group/session identity,
  descendants, launch phase, observation time, and evidence confidence.
- Reuse Darwin guardian/process-group code where correct; remove assumptions
  that a wrapper PID is the durable worker identity.
- Detect child escape, PID reuse, missing permission, zombie, and unknown state.
- Define liveness states `not_started`, `starting`, `alive`, `exited`, and
  `unknown`; `unknown` never authorizes takeover or success.
- Add fixture trees before wiring the runtime facade.

### Acceptance criteria

1. Liveness rejects a reused PID whose start identity differs from the persisted
   launch evidence.
2. Wrapper exit with a still-owned descendant is not reported terminal.
3. An escaped or unobservable descendant produces attention-required evidence,
   not silent cleanup or automatic takeover.
4. Process-tree snapshots are bounded, ordered, and contain no command-line
   secrets in persisted/rendered form.
5. Fixture tests cover direct child, grandchild, wrapper exit, zombie, PID reuse,
   permission denied, and unknown observation.

### Verification, safety, and non-goals

Run Darwin fixture and remote race tests; no platform emulation. Observation
failure changes state to unknown but never kills an unproven process. Redact argv
and environment. No CPU/RSS policy, stop escalation, provider logic, or adoption.
Done when one process-tree identity feeds runtime events.

---

## V090-014: Bounded output capture and log lifecycle

**Metadata:** code, size S; depends on V090-012; labels `v0.9.0`, `runtime`,
`logs`, `security`; exclusive in attempt output capture.

### Outcome and rationale

Capture stdout/stderr without blocking a worker or exhausting disk/memory, while
keeping full local logs under the project payload root and emitting bounded,
redacted event excerpts for status.

### Scope, constraints, and sequence

- Define separate byte/rate/line limits, truncation markers, rotation/retention,
  flush, close, and terminal digest behavior.
- Stream to owner-only project logs; never to the customer repo/worktree.
- Apply structured redaction before event excerpts; preserve local raw logs only
  under explicit policy and owner-only permissions.
- Continue draining after the display bound is reached so child pipes cannot
  deadlock.
- Test flood, partial line, binary bytes, invalid UTF-8, secret patterns, cancel,
  and disk-write failure.

### Acceptance criteria

1. Output flood remains within configured memory/disk/rate bounds and cannot
   block process join because a display buffer filled.
2. Logs and temp files resolve only beneath the validated project payload root.
3. Persisted event excerpts are valid UTF-8, bounded, ordered, redacted, and
   explicitly marked when truncated or dropped.
4. Terminal evidence records per-stream byte counts, truncation/drop counts, and
   content digest after flush/join.
5. Log write failure becomes a typed runtime fault and does not falsely report a
   successful, fully observed attempt.

### Verification, safety, and non-goals

Use deterministic flood/binary/disk-error fixtures; focused tests and remote
race. Never print raw fixture secrets in assertion failures. No report timing,
UI delivery, log upload, or global retention service. Done when all runtime
output passes through this bounded sink.

---

## V090-015: CPU, RSS, and process-count sampling

**Metadata:** code, size S; depends on V090-013; labels `v0.9.0`, `runtime`,
`resources`, `darwin`; exclusive in process telemetry.

### Outcome and rationale

Measure the aggregate resources of the owned Darwin process tree so admission,
status, and termination decisions are based on host evidence rather than one
wrapper PID or provider self-report.

### Scope, constraints, and sequence

- Sample aggregate CPU time/rate, resident memory, process count, and observation
  quality for the persisted process-tree identity.
- Define unavailable/partial/stale states and monotonic counters.
- Bound sampling cost and frequency; never scan all system processes repeatedly
  when owned identities are known.
- Emit samples/events through injected clock and typed collector interface.
- Test descendant churn, wrapper exit, PID reuse, denied observation, and reset.

### Acceptance criteria

1. Aggregate metrics include all observed owned descendants and exclude a reused
   or unrelated PID.
2. Partial/unavailable observation is explicit and never rendered as zero use.
3. Sampling remains bounded at the maximum supported child count and does not
   itself create a busy loop.
4. CPU/RSS/process threshold crossings produce deterministic evidence once per
   transition, not repeated event floods.
5. Tests use fixture processes and injected time, not machine-specific baseline
   percentages or wall-clock margins.

### Verification, safety, and non-goals

Focused Darwin telemetry tests and remote race. Metrics omit argv/environment and
personal paths. Observation errors do not kill processes in this issue. No
admission, hard limits, routing, or UI. Done when V090-016 can consume one
aggregate sample contract.

---

## V090-016: Machine-global admission and resource reservations

**Metadata:** code, size M; depends on V090-007 and V090-015; labels `v0.9.0`,
`resources`, `admission`, `safety`; exclusive in machine resource authority.

### Outcome and rationale

Prevent one or several projects from exhausting the Mac. Admit work only when a
machine-level reservation fits configured worker, verifier, test, process, CPU,
and RSS budgets, and persist ownership so restart cannot forget active claims.

### Scope, constraints, and sequence

- Define reservation request, decision, generation, lease, renewal, release,
  observed use, and over-budget transition in `machine.db`.
- Default to one active worker, zero verifier while worker active, one local test,
  eight child processes, 2 GiB RSS, and 150 percent sustained CPU.
- Use atomic compare-and-claim; stale reservations require process evidence
  before release/reuse.
- Unknown liveness fails closed and requests human attention.
- Separate policy evaluation from enforcement and expose explainable reasons.

### Acceptance criteria

1. Concurrent admission requests cannot exceed any configured machine budget.
2. Reservations are generation-fenced, renewable, idempotently releasable, and
   linked to project/job/attempt identity.
3. An expired reservation with possibly live processes is not automatically
   reassigned; it becomes attention-required until authority is resolved.
4. Threshold violations produce one transition and an enforcement request with
   observed evidence; they do not directly declare attempt failure.
5. Decision output explains requested, reserved, available, denied, and unknown
   resource values without exposing other projects' private content.

### Verification, safety, and non-goals

Use two-project concurrent claim barriers, stale/renew/release fixtures, and
remote race. Rollback of a failed claim leaves no partial budget consumption.
No provider quota, scheduler waves, or process killing. Done when every P2/P3
launch requires an accepted reservation.

---

## V090-017: Stop, join, escalation, and guardian cleanup

**Metadata:** code, size M; depends on V090-012, V090-013, V090-016; labels
`v0.9.0`, `runtime`, `cancellation`, `safety`; exclusive in termination.

### Outcome and rationale

Provide one idempotent termination lifecycle that requests graceful stop, waits,
escalates within policy, joins every owned descendant, flushes output, releases
resources, and never reports terminal cleanup while a child remains.

### Scope, constraints, and sequence

- Define stop reason, state transitions, grace/hard deadlines, signal sequence,
  guardian role, join evidence, and unresolved-child outcome.
- Fence stop/join by attempt and process generation.
- Use an independent bounded cleanup context after caller cancellation.
- Persist each meaningful transition through the event writer.
- Test cooperative, ignoring, spawning-during-stop, escaped, already-exited, and
  repeated-stop fixtures.

### Acceptance criteria

1. Repeated stop requests are idempotent and cannot signal a process belonging
   to another generation.
2. Cooperative trees stop and join before the grace deadline; uncooperative
   owned trees follow the documented escalation sequence.
3. Terminal-clean state is impossible until output is flushed, all observable
   owned descendants are gone, and the resource reservation is released.
4. An escaped/unobservable child yields attention-required with retained
   evidence and does not falsely free ambiguous ownership.
5. Cancellation of the initiating context cannot skip bounded cleanup or poison
   later runtime operations.

### Verification, safety, and non-goals

Run explicit-barrier process fixtures and remote race. Tests must clean their own
children even on failure. Persist no argv/secrets. No adoption, report rendering,
or automatic retry. Done when runtime success/failure/cancel all end through this
single join path.

---

## V090-018: Recovery and adoption under ambiguous process authority

**Metadata:** code, size M; depends on V090-010, V090-013, V090-017; labels
`v0.9.0`, `recovery`, `runtime`, `safety`; exclusive in restart reconciliation.

### Outcome and rationale

On LoopCoder restart, reconcile persisted attempt/process evidence with the OS
without launching duplicate work. Adopt only an exactly proven owned process;
otherwise classify the attempt for cleanup or human attention.

### Scope, constraints, and sequence

- Define recovery decisions for never-started, exactly alive, exited-unrecorded,
  PID-reused, descendants-only, unknown, and terminal-clean cases.
- Require matching attempt generation, root/process start identity, and project
  authority before adoption.
- Rebuild runtime observation and report schedulers without rerunning provider
  work or GitHub delivery.
- Persist decision/evidence idempotently and expose explicit operator actions.
- Test crash windows before launch, after launch, during output, during stop, and
  after process exit before terminal event.

### Acceptance criteria

1. Exactly matching live process evidence is adopted without a second launch.
2. A proven never-started attempt may be relaunched only through a new explicit
   attempt decision; recovery itself does not silently execute.
3. PID reuse, uncertain descendants, or incomplete launch evidence becomes
   attention-required and never automatic takeover.
4. A proven exited process is joined/finalized from retained exit/output evidence
   without repeating provider work.
5. Repeating recovery after any crash window is idempotent and produces no
   duplicate terminal event or reservation release.

### Verification, safety, and non-goals

Use persisted fixture snapshots plus real short-lived Darwin processes and
explicit crash barriers. No broad process killing or secret-bearing snapshots.
No cross-Mac adoption, new routing, or delivery resume. Done when restart cannot
turn uncertainty into duplicate execution.

---

## V090-019: Evidence collectors for runtime and delivery progress

**Metadata:** code, size M; depends on V090-010, V090-012, V090-014, V090-015;
labels `v0.9.0`, `progress`, `events`, `observability`; exclusive in evidence
normalization.

### Outcome and rationale

Translate trustworthy runtime and delivery observations into one versioned event
vocabulary. Reports must describe concrete evidence such as process alive,
output advanced, resource sample, commit observed, PR created, or check changed;
they must not ask a model to narrate progress.

### Scope, constraints, and sequence

- Define evidence types, source, observed/recorded times, confidence, digest,
  causal identity, privacy class, and progress significance.
- Add collectors for process state, output movement, resource use, Git worktree/
  commit, GitHub delivery/check evidence, and operator actions.
- Deduplicate unchanged observations and distinguish heartbeat from progress.
- Reject provider prose as lifecycle authority while retaining bounded output as
  content evidence.
- Map current v0.8 progress/report sources to consolidate, compatibility-only,
  or remove-after-parity.

### Acceptance criteria

1. Every accepted observation identifies its source, subject, confidence,
   observed time, digest, and whether it is concrete progress.
2. Unchanged samples do not create unbounded event growth; state transitions and
   threshold crossings remain visible.
3. Heartbeat/liveness and concrete progress are separate fields and cannot be
   substituted for each other.
4. Provider text cannot set process, delivery, verification, or terminal state.
5. Runtime, Git, GitHub, and operator fixture evidence yields a deterministic,
   redacted event sequence.

### Verification, safety, and non-goals

Golden event fixtures, dedup/property tests, malformed evidence, and remote race.
Redaction occurs before persistence for event excerpts. No scheduler, UI client,
real GitHub/provider, or parallel report store. Done when one evidence vocabulary
feeds status and timed reports.

---

## V090-020: Five-minute report scheduler and no-progress policy

**Metadata:** code, size S; depends on V090-019; labels `v0.9.0`, `progress`,
`scheduler`, `slo`; exclusive in progress timing.

### Outcome and rationale

Guarantee a start report, immediate state-change/blocker/terminal reports, and a
bounded five-minute status receipt while active, without provider calls or busy
polling. Detect repeated no-progress intervals and return control.

### Scope, constraints, and sequence

- Implement injected-clock scheduling from persisted attempt state/evidence.
- Emit one due receipt with stage, elapsed, last concrete progress, process count,
  resource state, remote gate, blocker, next timeout, and next action.
- Persist next due time and deduplicate across restart.
- After two intervals without concrete progress, request stop/detach/attention per
  policy; do not continue indefinite orchestration.
- Waiting performs zero model/provider calls.

### Acceptance criteria

1. Active attempts produce a start receipt and no gap longer than five minutes
   between due receipts under injected-time fixtures.
2. State change, blocker, resource breach, and terminal transitions emit promptly
   and reset only the appropriate report clock.
3. Restart at any point does not duplicate a receipt or lose the next due time.
4. Two consecutive intervals without concrete progress produce one documented
   no-progress action instead of silent continuation.
5. A structural test proves the scheduler has no provider runner dependency and
   uses bounded timer/wake operations rather than a busy loop.

### Verification, safety, and non-goals

Injected-clock boundary/restart tests, duplicate wake tests, and remote race. A
scheduler error leaves the attempt active but attention-required; it never
declares success. No rendering, host callback, or model-based summary. Done when
V090-024 can advance 12 simulated minutes deterministically.

---

## V090-021: Current status projection and cursor-based event follow

**Metadata:** code, size M; depends on V090-010, V090-019, V090-020; labels
`v0.9.0`, `status`, `events`, `cli`; exclusive in status projection/follow core.

### Outcome and rationale

Build one compact current-status projection and a resumable event-follow API so
terminal and UI clients display the same truth after disconnect or restart.

### Scope, constraints, and sequence

- Define reducer output for project/job/attempt stage, liveness, progress,
  resources, delivery gate, blocker, next action/time, and final-mile status.
- Expose snapshot plus `events --after <cursor> --follow` through a bounded core
  interface; CLI rendering may be minimal here.
- Rebuild projections from events and persist versioned checkpoints.
- Coalesce samples for current status without deleting audit events.
- Define stale/unknown fields explicitly; never render them as healthy/zero.

### Acceptance criteria

1. Current status is reproducible from the event log and its digest matches a
   full rebuild after reopen.
2. Follow returns a snapshot cursor then each later accepted event once and in
   order across disconnect/reconnect.
3. Status distinguishes heartbeat, concrete progress, provider process, delivery
   gate, and host final-mile stage.
4. Unknown/stale resource or process evidence is visible and cannot become
   success by omission.
5. Snapshot and individual event payloads stay within documented size bounds and
   redact private content by default.

### Verification, safety, and non-goals

Golden reducer, rebuild, pagination, reconnect, slow-consumer, and remote race
tests. Slow readers use bounded buffers and resume cursors; they cannot block the
event writer. No UI framework, host callback, or old report table writes. Done
when all presentation reads this projection/event stream.

---

## V090-022: UI-neutral report envelope and human view model

**Metadata:** code/docs, size M; depends on V090-021; labels `v0.9.0`, `ui`,
`reporting`, `protocol`; exclusive in `loopcoder.ui.v1` report projection.

### Outcome and rationale

Define the public, UI-independent report envelope and one compact human view
model. Terminal, Paseo, Codex, Claude Code, browser tools, desktop applications,
and future UIs consume the same schema; no UI becomes runtime authority.

### Scope, constraints, and sequence

- Implement the normative contract in
  `docs/architecture/v0.9.0-ui-report-protocol.md` and versioned JSON schemas.
- Project start, state change, periodic, attention, blocker, and terminal events
  into bounded report envelopes with stable IDs, sequence, digest, and privacy.
- Define one compact human view containing stage, elapsed time, actual route,
  concrete evidence, resources, blocker/attention, next action, and deadline.
- Add golden narrow-width/mobile and desktop render models without embedding an
  actual UI framework or transport.
- Keep raw logs, prompts, issue bodies, credentials, and absolute paths outside
  default report content.

### Acceptance criteria

1. Every required report kind has a versioned, bounded envelope derived only from
   accepted events and an immutable route/policy snapshot.
2. Machine JSON and the human view have the same semantic fields and content
   digest; pretty text is never parsed back as authority.
3. A report distinguishes process liveness, semantic progress, delivery stage,
   and product status rather than collapsing them into "working".
4. Golden views remain legible on narrow and desktop surfaces and never hide a
   blocker, required action, actual model, or next-report deadline.
5. Redaction tests exclude credentials, private source/issue bodies, prompts,
   absolute paths, and raw output unless a named policy explicitly permits a
   bounded field.

### Verification, safety, and non-goals

Golden schema/version/reducer/render/redaction tests and remote race for shared
projection readers. No transport, acknowledgement store, UI framework, live UI,
or host detection. Done when any later adapter can render the same report without
editing runtime, provider, router, or storage lifecycle code.

---

## V090-023: Durable UI subscription, cursor, and acknowledgement ledger

**Metadata:** code, size M; depends on V090-010 and V090-022; labels `v0.9.0`,
`ui`, `delivery`, `cursor`; exclusive in per-client delivery authority.

### Outcome and rationale

Provide one UI-neutral subscription and acknowledgement service over project
reports. A database write or bytes handed to a transport must never be reported
as operator-visible; only an identified client acknowledgement advances the
proven delivery stage.

### Scope, constraints, and sequence

- Register bounded client/session identity, capability set, required/optional
  status, subscription filters, and last accepted cursor.
- Replay ordered reports then follow new reports with bounded per-client queues.
- Persist monotonic `persisted`, `streamed`, `accepted`, `rendered`, and optional
  `seen` acknowledgement evidence keyed by client, event, sequence, and digest.
- Reject stale, regressive, cross-project, wrong-digest, or unsupported-stage
  acknowledgements.
- Isolate slow/disconnected clients from event append and process supervision;
  reconnect starts after the last accepted cursor.

### Acceptance criteria

1. A new client receives every report after its cursor in sequence and can move
   each report only through supported monotonic acknowledgement stages.
2. Reconnect and duplicate transport delivery produce no lost report, duplicate
   semantic rendering obligation, provider restart, or lifecycle side effect.
3. Acknowledgement evidence names the client/session/adapter version, event,
   digest, stage, and time; environment detection alone grants no capability.
4. Slow or unavailable clients cannot block event commit, signal/join, terminal
   persistence, or another client; overflow closes replayably at a known cursor.
5. Project isolation and redaction prevent one UI subscription or machine-global
   log from receiving another project's private report content.

### Verification, safety, and non-goals

Use deterministic multi-client replay, duplicate, wrong-digest, reconnect,
overflow, cancellation, and remote race tests. No network server, terminal
renderer, real UI, host credential, or operator-action mutation. Done when
reference transports can share one durable delivery ledger.

---

## V090-088: Terminal reference UI and bounded human/JSONL rendering

**Metadata:** code, size S; depends on V090-022 and V090-023; labels `v0.9.0`,
`ui`, `terminal`, `reporting`; exclusive in the reference UI client.

### Outcome and rationale

Make the terminal a complete protocol client rather than an accidental stderr
side effect. It is the universal fallback for shells, coding-agent hosts, and
automation, and it defines the minimum behavior every richer UI must match.

### Scope and constraints

- Implement replay-then-follow for human and JSONL modes using the same cursor.
- Keep machine command stdout separate from human reports; broken pipes and
  partial writes are explicit delivery failures.
- Render every mandatory report kind and submit `rendered` only after the full
  bounded report reaches the configured operator stream.
- Support noninteractive snapshot mode and attached follow mode.
- Do not parse rendered text, inspect host environment to invent capability, or
  dump raw provider logs into reports.

### Acceptance criteria

1. Terminal human and JSONL clients consume identical report sequences/digests
   and advance only their own durable acknowledgement cursor.
2. Start, state, periodic, attention, blocker, and terminal reports match the
   accepted narrow/desktop golden views without truncating required actions.
3. Broken pipe, partial write, closed stream, and slow output never produce a
   false `rendered` acknowledgement and remain replayable.
4. Strict JSON commands receive no human text on stdout; report JSONL uses an
   explicit dedicated stream or subcommand.
5. Following reports makes zero provider calls and exits cleanly on cancel,
   terminal state, client interrupt, or bounded transport failure.

### Verification and boundaries

Golden output, pipe-close, slow-writer, replay, cursor, and remote race tests.
No HTTP server, external UI, notifications, host hooks, or UI-specific code.
Done when an ordinary user can always inspect a run through public CLI surfaces.

---

## V090-089: Local HTTP/SSE UI bridge and capability handshake

**Metadata:** code, size M; depends on V090-022 and V090-023; labels `v0.9.0`,
`ui`, `api`, `sse`; exclusive in the generic local UI transport.

### Outcome and rationale

Provide a cross-language local transport that desktop, browser-based, Electron,
and future UIs can consume without a LoopCoder core change. The bridge is an
explicitly owned process, not an always-on service and not a Paseo API.

### Scope and constraints

- Expose version/capability handshake, report replay/follow over SSE, status, and
  acknowledgement JSON endpoints on an ephemeral loopback listener.
- Print a bounded machine-readable startup handshake containing address,
  protocol version, owner identity, expiry, and capability-token delivery method.
- Require a short-lived scoped bearer capability, strict loopback binding,
  origin policy, body/connection/rate limits, and idle shutdown.
- Reuse V090-023 subscriptions; keep provider credentials and raw stores
  inaccessible.
- Let the launching UI or explicit run own process lifetime; no login item,
  background daemon, LAN bind, discovery broadcast, or cloud relay.

### Acceptance criteria

1. A generic fixture UI can negotiate `loopcoder.ui.v1`, subscribe after a
   cursor, receive SSE reports, and submit valid acknowledgement evidence.
2. Non-loopback bind, absent/wrong/expired token, forbidden origin, oversized
   input, excess clients, and unsupported protocol fail before data disclosure.
3. Disconnect/reconnect resumes from the acknowledged cursor and never restarts
   provider work or duplicates semantic rendering obligations.
4. Slow clients and malformed requests remain within CPU/RSS/connection/output
   limits and cannot block event append, terminal UI, or process cleanup.
5. Owner exit, idle expiry, or explicit shutdown closes listeners and goroutines
   within a bounded join period and leaves no background process.

### Verification and boundaries

Black-box loopback HTTP/SSE tests with injected tokens/clocks, malformed clients,
reconnect, slow-reader, CORS/origin, shutdown, and remote race. No real browser,
Paseo, TLS, remote access, account system, or persistent daemon.

---

## V090-090: Attention lifecycle and authorized operator action API

**Metadata:** code, size M; depends on V090-010 and V090-022; labels `v0.9.0`,
`attention`, `ui`, `control`; exclusive in attention state and operator actions.

### Outcome and rationale

Turn `needs-human` from scattered status prose into durable, actionable product
state. Every UI can show the same pending attention and submit the same bounded
authorized action without becoming execution authority.

### Scope and constraints

- Define attention identity, kind, severity, reason, allowed actions, deadline,
  evidence, and open/acknowledged/resolved/superseded transitions.
- Support acknowledge, bounded input, named permission approve/deny, cancel,
  explicit detach, provider-free retry, and documented recovery selection.
- Require client/session identity, expected run revision, idempotency key, and
  action-specific authorization.
- Project current attention and a machine-wide redacted project index without
  copying private bodies into `machine.db`.
- Reject actions that mutate route pins, forge completion, bypass admission, or
  signal an unowned process.

### Acceptance criteria

1. Repeating the same action is idempotent; stale-revision, conflicting,
   unauthorized, unsupported, or already-resolved actions fail typed.
2. Every accepted action appends evidence before effect and maps to one existing
   runtime/delivery transition rather than a UI-specific side channel.
3. Pending attention survives UI/core restart, is visible from terminal and the
   generic bridge, and resolves only with accepted evidence.
4. Private content remains project-scoped and default summaries expose only
   project identity, kind, severity, age, and safe remediation.
5. No UI action can change immutable route, permission, base, issue, policy, or
   process authority without the defined successor/approval transition.

### Verification and boundaries

State-machine/property tests, duplicate/stale/forged client vectors, cancel and
permission fixtures, projection rebuild, and remote race. No rich UI, provider
prompting, autonomous approval, or cross-project workflow mutation.

---

## V090-091: Required report-client gate, delivery degradation, and fallback policy

**Metadata:** code, size M; depends on V090-088, V090-089, and V090-090; labels
`v0.9.0`, `reporting`, `policy`, `safety`; exclusive in mandatory delivery policy.

### Outcome and rationale

Make "the user must receive reports" executable. A run cannot launch into a
requested UI mode unless one required client proves the start report was
rendered, and a running attempt cannot remain silently invisible indefinitely.

### Scope and constraints

- Freeze required/optional clients, allowed fallbacks, acknowledgement deadline,
  missed-report policy, and detach/stop behavior in the run snapshot.
- Gate provider launch on at least one required `start:rendered` acknowledgement.
- Emit `delivery_degraded` after one missed mandatory acknowledgement and apply
  the explicit stop/detach policy after two consecutive intervals.
- Keep report generation/replay active during outage and keep process cleanup
  independent from UI availability.
- Never invent a fallback from host detection; the fallback must have a real
  connected client and its own acknowledgement.

### Acceptance criteria

1. Missing requested UI, failed handshake, or unrendered start report launches
   no provider and produces a typed pre-launch decision with remediation.
2. One missed report creates visible attention; two consecutive missed required
   intervals perform the frozen stop/detach policy and return control.
3. A valid terminal or generic-bridge fallback satisfies policy only after its
   own `rendered` acknowledgement and is named in reports.
4. Delivery outage cannot prevent signal/join, terminal persistence, reservation
   release, or descriptor closure, but it can block a clean product verdict.
5. Reconnect replays obligations and clears degradation only after matching
   acknowledgement evidence; no model narrates or repairs delivery.

### Verification and boundaries

Injected-clock start-gate, missed-interval, disconnect, fallback, reconnect,
cancel, cleanup, and remote race tests. No UI-specific implementation, OS
notification assumption, provider retry, or indefinite background waiter.

---

## V090-092: Generic UI conformance runner and golden transcripts

**Metadata:** test/docs, size M; depends on V090-088, V090-089, V090-090, and
V090-091; labels
`v0.9.0`, `ui`, `conformance`, `protocol`; exclusive in public UI qualification.

### Outcome and rationale

Give any UI developer an executable way to prove compatibility without access to
LoopCoder internals. A product claim is based on a black-box protocol transcript,
not a mocked host name or environment variable.

### Scope and constraints

- Publish schemas, capability profiles, golden transcripts, malformed vectors,
  and a black-box adapter command contract.
- Exercise required report kinds, replay, exact semantic deduplication,
  acknowledgement honesty, attention actions, slow client, and unavailable UI.
- Produce a redacted conformance manifest tied to LoopCoder and adapter versions.
- Define `full`, `degraded`, and `unsupported` profiles by demonstrated behavior.
- Keep adapter implementation and researched UI source outside the fixtures.

### Acceptance criteria

1. A third-party fixture adapter can pass the full profile using only published
   schemas/transcripts and the terminal or HTTP/SSE transport documentation.
2. A lying adapter that acknowledges unrendered, wrong-digest, skipped, or
   out-of-order reports fails with a precise reproducible vector.
3. Reconnect across a mandatory report proves no loss, no duplicate semantic
   rendering, and no provider/worker restart.
4. Conformance runs have fixed process/time/output limits and leave no listener,
   client, fixture process, or private data behind.
5. The resulting manifest states proven delivery stages/actions and cannot turn
   fixture-only evidence into a real-host support claim.

### Verification and boundaries

Run the suite against the terminal client, generic bridge fixture, intentionally
broken adapters, and remote race. No real provider, private host conversation,
UI source reuse, or release support claim before a real-adapter smoke.

---

## V090-093: Paseo reference adapter and real public-surface smoke

**Metadata:** code/test, size M; depends on V090-092; labels `v0.9.0`, `ui`,
`paseo`, `integration`; exclusive in the first external UI adapter.

### Outcome and rationale

Prove that one real external UI can consume the generic protocol. Paseo is the
first reference client because it exposed the original visibility failure, but
it receives no privileged core API and is not required by other UIs.

### Scope and constraints

- Revalidate Paseo's current public inbound, activity, notification, terminal,
  WebSocket, and MCP surfaces at the implementation base.
- Implement only the adapter behavior those public surfaces can prove; use the
  generic bridge or terminal transport where appropriate.
- Report a precise capability profile for accepted/rendered/seen, reconnect,
  attention, and cancel; unsupported rich in-chat behavior remains unsupported.
- Add a short opt-in real smoke using synthetic project/report content.
- Maintain strict AGPL separation: no Paseo source, schema, test, or prose is
  copied, translated, linked, or compiled into LoopCoder.
- If Paseo's public surface cannot provide a truthful `rendered`
  acknowledgement, stop with a bounded interface-gap record. Any Paseo-side
  change requires a separately approved issue and PR in the Paseo repository;
  it must not be hidden inside this LoopCoder issue.

### Acceptance criteria

1. The adapter passes every claimed generic conformance capability and reports
   only the highest final-mile stage backed by a real Paseo acknowledgement.
2. Start, periodic, attention, and terminal reports appear in the documented
   operator-visible surface; hidden stderr or host detection is insufficient.
3. Paseo restart/reconnect resumes by cursor without report loss, duplicate
   semantic rendering, or provider/worker restart.
4. Missing/changed Paseo capability degrades to a named generic transport or
   fails closed when Paseo was required; LoopCoder core remains unchanged.
5. License/provenance review and diff inspection prove independent protocol
   implementation with synthetic fixtures and no private conversation data.

### Verification and boundaries

Generic conformance plus one bounded opt-in real Paseo smoke on the supported
macOS artifact. No Paseo UI redesign, private API assumption, hard dependency,
credential storage, or claim that other UIs are supported without conformance.
The issue remains blocked rather than weakening `rendered` if a companion Paseo
change is required but unavailable.

---

## V090-094: Foreground and explicit-detach supervisor ownership

**Metadata:** code, size M; depends on V090-017, V090-018, V090-023, and V090-091;
labels `v0.9.0`, `runtime`, `detach`, `ui`; exclusive in attachment ownership.

### Outcome and rationale

Define exactly what happens when the invoking UI remains connected, disconnects,
or explicitly requests detached execution. Reuse `internal/detachedrun` recovery
mechanics without introducing an always-on daemon or letting a UI own the worker.

### Scope and constraints

- Make foreground attachment the default and explicit `--detach` the only path
  to a per-run background supervisor.
- Persist supervisor process identity, generation, report clients/policy,
  cancellation endpoint, lease/heartbeat, and terminal join evidence.
- Require a configured required report client/fallback for detached launch.
- Define host close, core crash, supervisor crash, stale owner, reconnect, cancel,
  and terminal handoff behavior.
- One run owns one supervisor; no login item, global daemon, silent auto-detach,
  or cross-computer process adoption.

### Acceptance criteria

1. Foreground run returns only after terminal/explicit detach/failure and leaves
   no unowned child, listener, timer, or descriptor.
2. Explicit detach returns a stable run ID only after supervisor identity,
   cancellation, report policy, and ownership evidence are durable.
3. UI disconnect alone neither kills nor adopts provider work; the frozen
   delivery policy determines replay, attention, stop, or explicit detach.
4. Stale supervisor generation cannot signal, renew, report terminal, or release
   another generation's resources; ambiguous authority requires attention.
5. `status`, `events`, `attach`, and `cancel` operate through durable run
   authority and never require the original UI process or provider narration.

### Verification and boundaries

Real short-lived Darwin process fixtures plus kill/restart/stale-generation/UI
disconnect tests and remote race. No persistent daemon, launch agent, cloud
control plane, cross-Mac live continuation, or automatic provider retry.

---

## V090-024: Twelve-minute silent-worker multi-UI visibility and cleanup canary

**Metadata:** test, size M; depends on V090-016, V090-017, V090-018, V090-019,
V090-020, V090-021, V090-036, V090-092, and V090-094; labels
`v0.9.0`, `acceptance`, `runtime`,
`visibility`, `ui`; exclusive hardened-visibility checkpoint.

### Outcome and rationale

Prove the production direct path with a deterministic worker that emits no
provider output for 12 minutes, then completes or is cancelled. Terminal, the
generic bridge client, and an independent black-box conformance client receive
the same mandatory report truth while the runtime remains bounded and
provider-free report scheduling continues.

### Scope, constraints, and sequence

- Run the V090-003 silent fixture through the accepted direct-run path, runtime,
  event writer, report projection, subscriptions, required-client policy, terminal
  UI, generic bridge, and black-box conformance client.
- Use injected time for report intervals while retaining real short-lived Darwin
  process ownership/cleanup evidence.
- Cover completion, cancellation, each UI disconnect/reconnect, core/supervisor
  restart, required-client outage, resource breach, and ambiguous child variants.
- Emit an evidence manifest tied to the tested merge SHA.
- Make this the hardened visibility gate before automatic routing and workflows.

### Acceptance criteria

1. The canary emits and renders start, five-minute, ten-minute, and terminal or
   blocker reports with no provider/model polling and no wall-clock correctness
   dependency.
2. Current status throughout the silent interval shows process liveness, last
   concrete progress, resources, next report time, and final-mile stage honestly.
3. Terminal, generic bridge, and the independent conformance client consume the
   same report digests; every disconnect/reconnect replays from cursor without
   lost or duplicate semantic rendering or worker restart.
4. Completion and cancellation join every owned child, flush logs/events, and
   release the machine reservation; ambiguous escape is attention-required.
5. CPU, RSS, process, output, event, and UI-client-buffer bounds stay within policy and
   the evidence manifest contains no machine-identifying data.

### Verification, failure, and non-goals

Run all variants in hosted Darwin acceptance CI with deterministic barriers and
bounded real processes. Any flake blocks automatic routing; do not mask it with
reruns or timing margins. The real Paseo smoke remains an independent V090-093
release-evidence item and cannot block this core canary. No
self-bootstrap, auto-routing, workflow, or model-generated progress. Done when
the exact merge SHA has an archived multi-UI manifest and owner acceptance.
