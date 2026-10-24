# Worklog: generalized stage-persona claim-then-spawn (owner directive 2026-10-24)

**Session:** Replace the dormant parallel auto-review assignment path
(#1832 item 5's ReviewAssigner — token-less direct dispatch) with ONE
generalized mechanism: when a task ENTERS a stage that has a persona
configured, the engine claims the row (existing lease + claim-token
semantics) and spawns a fresh session whose charter envelope carries
the configured AGENT PERSONA. Stage-generic and persona-generic,
config-driven — nothing hardwired to in_review or to "reviewer".

**Owner directive (verbatim):** "Delegate that work. Make it
expandable so it works on any arbitrary stage and can accept an agent
persona." — the claim-then-spawn-for-reviewers design recommendation
from the sub-orchestrator board test (epic "Fix npm publish leg",
2026-10-24), generalized.

## Validate-first map (on current origin/main)

- **(a) Live claim-then-spawn (reused verbatim):** claim sites
  `claimOnce`/`TaskStart` (engine.go — grant mints token+lease,
  attempts+1, DRR); spawn sites `spawnAfter`/`spawnSync` (guarded
  spawn-stamp: assignee + lease re-arm under token+stage match, loser
  aborted) and `spawnFor` (re-claim token delivery); charter envelope
  `boardCharterPrompt` (app/board_spawner.go) + Claim.VerdictChain;
  heartbeat = token + new-progress-evidence (#1847/#1832 WS4).
- **(b) Dormant parallel path (absorbed):** `maybeAutoAssignReview`
  (review_assign.go) fired post-commit on →in_review under
  reviewColumn=auto AND autoAssignReviewer=true — a manager-surface
  reviewer holding NO claim token, no lease, no liveness. Deleted;
  `effectiveConfigForProject` + the verdict-chain helpers moved to
  persona.go; app wiring's `newReviewAssigner` removed.
- **(c) #1832 WS4 stillborn coupling (not replicated):** Heartbeat's
  zero-turn-evidence guard refuses renewal of a spawn that never
  turned. The persona path COMPOSES with it: a dead persona worker's
  refused renewals lapse the lease, and the new RC1 persona pass
  re-pools a FRESH persona worker (#1813 accounting + doubling
  backoff + attempts-cap escalation). No Heartbeat semantics changed
  beyond admitting in_review claims.

## Mechanism

- **Config key `stagePersonas`** (object: stage → {name,
  instructions?}) on BOTH tiers (user defaults + project overrides;
  migration 000049 relaxes both CHECK constraints). Per-KEY merge: a
  set object replaces the inherited map wholesale; `{}` is the
  explicit opt-out (the composition lever for the auto-review
  defaults flip). Validation fail-closed: persona-eligible stages
  ONLY (in_progress, in_review — the stages a live claim can exist
  in; backlog is the claim pool, done terminal, pending_input keeps
  its suspended worker's token with human-only exits — the
  defaultWorkerSlots-at-project-tier "never display an inert
  effective-map" discipline), bounded name (64) / instructions
  (4096), DisallowUnknownFields, nulls never values.
- **Persona resolution:** explicit stagePersonas entry wins on any
  eligible stage; ELSE the legacy composition (reviewColumn=auto AND
  autoAssignReviewer=true) renders DefaultReviewerPersona for
  in_review — existing stored configs keep their meaning, and
  lane-autoreview's defaults flip composes as "auto-review on ==
  in_review has a reviewer persona via the generic config". Shipped
  defaults resolve NO persona → byte-identical behavior.
- **Entry mint (claim-before-spawn):** →in_review with a resolved
  persona applies today's forward-exit statement (token/lease
  cleared, #1813 success reset, identity retained for the bounce/RC2
  consumer) and then mints the persona claim in the SAME transaction
  (re-claim-class: fresh token+lease, last_progress_at=now, attempts
  UNCHANGED, provenance flag false) + a persona_claim audit event;
  the post-commit nudge spawns through the SAME guarded spawn-stamp
  (Claim.Persona/Stage ride the envelope; the charter renders the
  persona preamble, else today's prompt byte-identically).
- **in_progress personas** ride the EXISTING grant/bounce/exit spawns
  (spawnAfter/spawnFor resolve the row's stage persona); nothing new
  minted.
- **Heartbeat** admits in_review claims (token match required; an
  unclaimed in_review row still refuses — today's rule for
  no-persona rows). The stage machine's token-guard rule is
  untouched (writes FROM in_progress remain the gated class).
- **RC1 persona pass (dead-worker healing):** (a) lapse — an
  in_review persona claim with an expired lease gets #1813
  accounting (consecutive failure count, doubling backoff cooldown,
  claim cleared, deterministic reclaim event); a persona config
  removed mid-flight converges the row to today's unclaimed shape.
  (b) re-arm — unclaimed in_review rows past cooldown with a persona
  configured get a fresh mint + post-commit FRESH persona-worker
  spawn (spawnFor re-resolves; a vanished config spawns nothing);
  failure_count at the attempts cap escalates to
  pending_input/escalated (the human-only exit — a dead-persona
  loop never spins unbounded; persona mints never touch attempts).

## Tests (RED-first)

- Unit (run locally, green): persona_config_unit_test.go (validation
  matrix incl. ineligible stages/nulls/bounds/unknown fields, storage
  round-trip, per-KEY merge + {} opt-out, personaForStage resolution
  incl. legacy composition + explicit-wins + defaults-nil);
  TestCharterPrompt_PersonaRendering (reviewer default, arbitrary
  persona, persona-less byte-identical opening); config wire-shape
  pins extended.
- Integration (PG harness, CI): TestPersona_StageEntryClaimsThenSpawns
  (mint + spawn + persona + event + attempts unchanged);
  TestPersona_NoConfigByteIdenticalToday (the required back-compat
  pin); TestPersona_LegacyCompositionIsReviewerPersona (firing arm +
  the three dormant matrix tails);
  TestPersona_DeadWorkerLeaseLapseFreshReclaim (the required
  dead-persona-worker pin: lapse → #1813 mark → cooldown → fresh
  reclaim with persona charter → repeated death → cap escalation to
  pending_input); TestPersona_HeartbeatOnReviewClaim (extend under
  token+evidence; stale stamp no-extend; wrong token and unclaimed
  rows refuse); TestPersona_ImplementerPersonaRidesGrant;
  TestPersona_BounceAfterPersonaClaim (fail verdict on a
  persona-claimed row rides the existing bounce; mint preserved the
  identity); TestReview_1832_VerdictChainRidesNextClaim (carried over
  from the retired suite).

## Review rounds

**Round 1 (CHANGES_REQUESTED, head cb3663f06):** one blocking finding
— the project-tier Settings save built its PUT from the toggles only,
and the project PUT is REPLACE server-side (absent keys are DELETED):
saving reviewColumn silently wiped a stored stagePersonas override
(the PR body's "safe by construction" claim was wrong). Fixed + pinned
(frontend carries pc.overrides.stagePersonas verbatim; the pinning
test uses a project seeded with a stored auditor override). Non-
blocking findings fixed in the same round: (a) the {} opt-out now
OWNS the stage mapping outright (BoardConfig.StagePersonasSet — an
explicit key at any merged tier suppresses the legacy-composition
fallthrough; without this the defaults flip could never be opted out
per-project); (b) persona_claim events now SSE-publish (entry mint
rides the nudge's events; the re-arm rides commitPublishing); (c) the
escalation audit id is episode-unique nanos (failure_count resets on
the human re-arm — the count-keyed id collided across episodes); (d)
config-read failures in stagePersona log a warn (the observability
parity the absorbed path had). Also added the review's missing arm:
approve→done on a persona-claimed row (claim consumed, token/lease
cleared, Done guard applies regardless).

Declared NOT fixed (non-blocking, reviewer-discretionary): RC2 drift
paths move a persona-claimed in_review row without aborting the live
persona session (contained by the token guards; the orphan runs to
its timeout — the drift-abort seam is a natural follow-up alongside
#1847).

**Round 4 (rebase onto #1939):** the auto-review defaults flip
merged mid-flight; rebased and composed exactly as directed — the
flipped floor renders the reviewer persona for in_review through the
legacy-composition arm (auto-review on IS in_review having a reviewer
persona via the generic config; the parallel dispatch path is gone).
Pins reconciled: the byte-identical back-compat pin is now the
EXPLICIT {} opt-out; the flipped-no-config default FIRES (new
composition pin, TestPersona_FlippedDefaultFiresReviewerPersona);
the harness seeds a user-level {} opt-out to isolate the legacy
kill/reads/rearm suites from the now-firing floor (those pins own the
WORKER claim lifecycle); handler wire pins carry auto/true +
stagePersonas:{}.

**Round 2 on the re-cut #1941 (CHANGES_REQUESTED, head 3afeb1112):**
three blocking findings, all fixed. (1) The re-arm pass's SQL
prefilter required stored config rows — under the #1939 flipped floor
the persona fires with ZERO rows, so dead-worker healing/escalation
were unreachable in the shipped default posture. The prefilter is
GONE: the candidate scan is pause-gated (#1747 — finding 3: the
re-arm mint now predicates NOT paused at both the scan and the
guarded UPDATE) and Go's stagePersona resolution is the sole
authority; new pins: rearm-under-pure-default and
paused-row-refuses-rearm (+resume convergence). (2) The user-tier
Settings save materialized an explicit {} opt-out (the effective GET
carried no set-ness surface): stagePersonasSet now renders on the
wire and the tab carries the stored map back ONLY when set — never
materialize {}, never delete a stored map (frontend pins inverted +
a set-ness arm). Minor: projectFor warns on resolve failure; dup
comment and a `_ =` discard cleaned.

## Verification

go build/vet clean (incl. -tags=integration); `go test ./...` green
(known-unrelated outbox load-drill flake passes in isolation);
golangci-lint 0 issues (pkg/board, api/...); `make openapi-validate`
valid; frontend vitest 2489 passed + tsc clean + eslint clean on
touched files. No local PG — the integration suites run in the
secrets-integration CI lane (paths ^pkg/board/ and ^api/migrations/
both trigger).

## Follow-ups (out of scope, declared)

- A persona EDITING UI (this PR carries the frontend types + a
  carry-through so the user-tier REPLACE PUT never silently clears
  configured personas; the project tier's partial PUT is safe by
  construction).
- #1847's platform-derived liveness rework subsumes the WS4
  fail-open seam; the persona path already keys on its outcome.
