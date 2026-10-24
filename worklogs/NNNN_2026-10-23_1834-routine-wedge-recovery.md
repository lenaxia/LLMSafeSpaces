# Worklog: #1834 — trigger routine path must recover post-OOM (wedge class)

**Date:** 2026-10-23
**Branch:** `fix/1834-routine-wedge-recovery`
**Status:** PR #1909 r1 remediation pushed — review r1 (REQUEST_CHANGES @ 0171e45) findings addressed: workflow-shape residue gate + reconciler pins, auth-first pin, e2e R11 rows (a/b/c), closure re-scoped to "addresses the finding-3 amplifier"

---

## Objective

Issue #1834: the 2026-10-21 routine-path outage (01:20–22:54Z,
multi-window, multi-workspace) — trigger-fired routines wedged after an
OOM-class restart. INVESTIGATE-then-FIX lane: reconstruct the wedge
from the issue's evidence, pin it minimally, fix smallest-correct.

## Investigation — the wedge reconstructed (evidence → mechanism)

The issue's three findings map as follows:

**Finding 2 (script nodes structurally unavailable in sidecar mode,
#1455):** superseded for this lane — #1885 landed sidecar script parity
today (the `script_exec` control-socket leg, engine sessionless
early-return, e2e contract flip). Not re-decided here.

**Finding 3 (failure accounting — `script_failed` never counted):**
verified at the DEPLOYED commit (c6f4e77, pre-#1885): the pre-script
leg tested only `err != nil` — agentd's HTTP-200 error envelopes
(ErrorCode set, err nil) fell through, so a failed script_path leg
landed `delivered` + counter reset (pr-monitor-sweep's ~30 failures,
cf=0; only `agent call timed out` / status failed counted). #1885's
review F1 closed exactly that leg (fails loud + accounts). My
outage-shape pin (`TestExecuteRoutine_AgentLegOpencode500Envelope_FailsLoudAndCounts`)
verifies the AGENT leg holds the same contract for the retry-exhausted
envelope — born green on main, kept as the belt.

**Finding 1 (the wedge):** the headless path is engine → agentd
`/v1/workflow/node/execute` → `execAgentNode` → opencode :4096 (session
create + message POST). The signature "15-25s = opencode starting then
500ing" with session creates succeeding means opencode served its API
but the message leg (the provider-carrying turn) failed internally —
the credential/spawn chain the turn depends on was stale, not missing.
Where could stale state survive opencode restarts but not an agentd
restart? In agentd/sidecar-held spawn+credential state, by design:

- the supervisor's spawn pulls the secrets-env delta from the sidecar
  (/v1/spawn-env, US-70.1) with a LAST-GOOD fallback — an opencode
  restart re-consumes whatever the sidecar serves, stale included;
- the boot materializer's W2 apply-guard (`runMaterializeCommand`)
  early-returns on `seq <= anchor.AppliedSeq` — its own comment admits
  "the repair lands only on pod recreation or a batch-seq bump, not
  'the next pass'";
- `applySecretsBatch`'s live push (`StageCredentials`) failure is
  swallowed with a Warn: credentials stay on disk but "in-memory
  provider state will not pick them up until the next explicit reload
  or pod restart" (secrets.go:1248) — nothing retries the live push;
- the deferred restart (busy-session maintenance window) can defer
  that credential restart unboundedly while sessions read as
  progressing.

So the 06:19Z/20:31Z restarts (opencode-level) respawned the child onto
the same stale agentd-held view; the 22:41Z secrets-resync AGENTD
restart re-ran boot materialization from the durable batch file and the
22:45–22:54Z recovery followed. The deployed-log confirmation of that
window (agentd restart logs 22:41–22:54Z) is owner-surface and is
NOTED as the issue's outstanding ask — this lane's code-level mechanism
is complete and pinned without it.

**The amplifier that made it 21 hours (MY LANE'S DEFECT, pinned):**
auto-disable never disarmed WEBHOOK triggers. `ClaimDueCronTriggers`
filters `enabled = true`, but `HandleWebhook` never checked
`trigger.Enabled` — every signed delivery of an auto-disabled trigger
minted an executing fire — and `processPendingRoutineFire` (the pending
drain) didn't re-check either. The platform's own circuit breaker could
not stop the hammering it exists to stop: post-#1885 accounting counts
the failures and flips `enabled=false`, and the webhook route kept
executing anyway.

## Fix design (smallest correct)

0. **Review r1 addition — the workflow shape's residue gate.** r1 F1:
   the disarm invariant was half-delivered — `ClaimQueuedRuns` joins
   nothing and the Reconciler never consulted `Enabled`, so a run
   minted before the disable still executed post-disable (reachable in
   exactly the outage shape: the reconciler's own failure accounting
   flips `enabled=false` mid-queue; wedge-class backpressure piles
   queued runs). `Reconciler.executeRun` now re-checks the owning
   trigger at execution: a CONFIRMED disabled trigger cancels the run
   BEFORE workspace activation (named reason `trigger_disabled`, run
   `canceled` + the closed error-code enum's existing member, audit
   mirrored onto the run's fire row as `skipped`). Fetch errors and
   gone triggers are NOT disable verdicts — the run executes (the
   #1473 platform-problem discipline, pinned as the belt).
   Mutation-verified: gate deleted → pin RED (`must cancel, got
   "succeeded"`); restored → green.

1. **Receiver gate** (`webhook_receiver.go`): after HMAC/skew/dedup
   (auth-first — no unauthenticated oracle for trigger state) and after
   the envelope build, a disabled trigger records ONE self-explaining
   `skipped` fire (`{"reason":"trigger_disabled", "hint": ...}`,
   CompletedAt set — terminal at mint) and answers `202
   {"status":"skipped"}`. 2xx is deliberate: a non-2xx would make
   GitHub-style senders retry-storm a trigger the platform just
   disarmed (the validation_error precedent). ActionType mirrors the
   trigger's target (run_workflow vs routine) so the audit reads true.
2. **Drain belt** (`engine.go processPendingRoutineFire`): a fire
   minted before the disable (delivery racing the auto-disable,
   pre-fix residue) is marked `skipped` with the same payload, never
   executed. Gate-closed discipline: absorbing is not failing — no
   accounting, no counter movement.
3. **Outage-shape accounting belt** (test-only): the retry-exhausted
   `script_failed`/"opencode returned 500" envelope fails loud +
   counts once — pins the #1885 family at the exact outage shape.

NOT changed (documented choice): the serialized tick + missed-tick skip
(the multi-window "routines stopped firing" mechanism — slow fires
stretch ticks, stale next_fire_at renders 'skipped' rows) is observable
by design and out of the smallest-correct budget; the agentd-side
credential-chain findings above are recorded here as the issue-1
mechanism, with their hardening left to the owner's #1455-adjacent
surface decisions.

## Red-first evidence (both pins failing on main @ 4a7cbc27)

- `TestProcessPendingRoutineFire_DisabledTrigger_SkipsExecution`:
  `a disabled trigger's pending fire must be skipped, got "delivered"`
  (both direct and tick-drain drives — the fire EXECUTED the agent leg).
- `TestWebhookReceiver_DisabledTrigger_AbsorbedSkippedNeverExecutes`:
  status assertions failed — the receiver minted a `fired` row (no
  skip, no CompletedAt, no trigger_disabled payload) and the
  workflow-targeted shape minted a RUN.

## Test matrix (green post-fix)

- `api/internal/workflows` full suite (84s) — including the new pins:
  drain skip × 2 drives (skipped + self-explaining + zero dispatches +
  no accounting); agent-leg envelope (failed + code+detail verbatim +
  exactly-once accounting + exactly 3 attempts + no reset); reconciler
  residue × 3 subtests (disabled → canceled+mirrored+zero nodes+zero
  agentd calls+no accounting; gone row → executes; enabled →
  executes).
- `api/internal/handlers` full suite — receiver pin × 2 target shapes
  (202 + one terminal skip row + no runs + no increments) + the
  auth-first regression pin (disabled trigger + invalid signature →
  401, nothing recorded).
- `local/` full suite (the ExecuteSmoke traversal under shims +
  structural needles) — R11a (receiver gate, routine shape: 202+
  skipped body, self-explaining skip fire, zero executing fires, cf
  unchanged), R11b (workflow shape: 202, zero runs minted), R11c (the
  drain belt with R10's tick-race retry pattern: pre-disable pending
  fire → skipped trigger_disabled, cf==0, race-lost detection via the
  dummy workspace's activation failure).
- `go vet` clean; `gofmt` clean; `repolint` all checks passed; worklog
  dup-check empty.

## Closure scope (per review r1's issue-closure verification)

The PR says "Addresses the #1834 finding-3 amplifier" — NOT Closes:
finding 1's literal ask (agentd restart logs 22:41–22:54Z) is
owner-surface and stays open with the code-level mechanism recorded
below; finding 2 was superseded by #1885; finding 3's per-leg
accounting was #1885's — this PR contributes the amplifier fix (the
webhook disarm across receiver, drain, and reconciler paths) plus the
outage-shape belt pins.

## Mutation-verify suggestion

- Delete the receiver's `!trigger.Enabled` block →
  `TestWebhookReceiver_DisabledTrigger_AbsorbedSkippedNeverExecutes`
  fails (fired row minted).
- Delete the drain's `!trigger.Enabled` block →
  `TestProcessPendingRoutineFire_DisabledTrigger_SkipsExecution` fails
  (delivered, agent leg dispatched).
- Change the drain's skip to `accountTriggerFailure` → the
  no-increments assertion fails (absorbing is not failing).

## Residuals / owner-surface asks

- The issue's literal ask for finding 1 — agentd restart logs
  22:41–22:54Z — needs production log access (owner-surface). The
  code-level mechanism this lane reconstructed (spawn-env last-good +
  W2 anchor guard + swallowed StageCredentials + deferrable credential
  restart) predicts: those logs show the 06:19Z/20:31Z restarts as
  opencode-child (supervisor) restarts and the 22:41Z one as a sidecar/
  agentd process restart with a fresh materialize pass.
- agentd-side hardening (live-push retry / anchor self-heal /
  spawn-freshness) is a design decision on the #1455-adjacent surface —
  recorded, not changed.
- The skipped-fire rows minted per delivery for a disabled trigger are
  bounded by the sender's own rate (plus the receiver's rate limiter);
  a permanently-disabled trigger's webhook could be deleted by the
  owner to stop the audit rows entirely.
