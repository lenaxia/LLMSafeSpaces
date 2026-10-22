# Worklog: #1817 — piece 1: user-stream session events carry the phrase (#1821 rapid slice)

**Date:** 2026-10-21
**Session:** continuation worker (orchestrator rapid-fix dispatch after the #1821 root-cause ruling). Worktree `wt-1817`, branch `fix/1817-streaming-busy-regression` off main @ 644446fa.
**Status:** complete for pieces 1–3 (contract-stream half shipped under Ruling A); r1–r7 findings addressed; r8 merge-hygiene minors; r9 observable-alias/hot-path frontend work and lifecycle/bounds agentd fixes; r10 provider activity re-key + batched snapshot learning and test-determinism/retention corrections. r11 frontend fixes applied in the companion commit on this branch (see the r9/r10 frontend section at the end).

## Root cause (adopted from #1821, investigator file:line-verified)
SSE session.status legs keyed by HARNESS id (proxy_usagestream.go:182) vs the frontend identity flipped to PHRASE (sessionindex rows) — the join misses for every minted row. One root cause, both symptoms: sporadic busy + dead live streaming (ChatPage.tsx:720 vs fold.ts:39 — same join, the contract-stream consumer). E2E mocks injected ONE id into both surfaces — every suite blind.

## Work Completed
### (a) RED-first pins (first commit)
`proxy_usagestream_phrase_test.go` — T0.1 (session.status / agent.question / agent.question.resolved each must carry the phrase + additive internal_session_id; RED on main with the exact root-cause predictions quoted in the commit message) + the degraded-passthrough pin (green on main — never-worse pinned) + T1.6 (the cross-surface join tripwire: list row keyed phrase + ABI event keyed harness → ONE joinable wire key). Fixture class: mockSessionIndex.phrases (dual-id).

### (b) Implementation (second commit)
`ProxyHandler.translateSessionIDToPhrase` (proxy_usagestream.go): reverse-resolve via the sessionIndex Register seam (idempotent, LRU-cached, degraded-safe) at each of the three emission sites; wire id = phrase; harness id additive as internal_session_id (map-typed Data both shapes); passthrough on no-phrase.

## Tests Run
- `go test ./api/internal/handlers/ -count=1` — FULL suite green (82s).
- golangci --new-from-merge-base: 0 issues.

## Next (scope ruling requested)
The STREAMING half of the acceptance: the contract-stream frames (the fold's key source) still carry harness ids — sessionstate/projection.go:417 `SessionId: id`. Two seams: (A) agentd emission translation (agentd natively knows both ids — cmd/workspace-agentd/session_ref machinery; ships fast via the OnRefresh overlay policy) — an ABI frame id-space change needing the freeze-gate's additive judgment; (B) api-side rewriting at the contractstream raw-forward boundary (parse→rewrite→remarshal per frame — hot-path cost, no ABI change). Recommendation: (A).

## Ruling A executed (the streaming half)

The orchestrator ruled seam A (agentd emission, additive internal field). Shipped:
- ABI additive: Event.internal_session_id=13, SessionSnapshot.internal_session_id=7 (freeze-gate judged additive; consumers must not require them — absent on ingest paths and un-refreshed emitters). Codegen redone with the PINNED toolchain after an initial wrong-plugin regen (buf v1.72.0 / protoc-gen-go v1.36.11 / connect v1.20.0 / es 2.14.0; the freshness gate now passes).
- sessionstate: Config.IDTranslator (nil-safe); Authority.wireEvent (proto.Clone — govet copylocks bars the struct copy; the clone also detaches nested state) on the live-event frame; podSnapshotsLocked translates the subscribe-snapshot rows. State()/statusz and the internal map stay HARNESS-keyed (the API's usagestream consumer joins there — its own piece-1 translation handles that surface).
- Wiring: emissionIDTranslator = currentSessionShortIDs().shortIDFor (the same TTL-cached registry the MCP surfaces read).
- HEAL WINDOW (ops note): un-refreshed pods keep emitting harness-keyed frames until their agentd overlay refreshes (#1746 OnRefresh) — consumers must not require the phrase; bounce long-lived workspaces if needed.
- Red-first: emission pins committed RED (both arms; output quoted), heal-window pin green throughout; agentd + sessionstate suites green; golangci 0.

## Piece 2 — the T0.1 agent.question pin update

Piece 2 (f1a73ab9b) fixed the clobber: `translateSessionIDToPhrase`'s
default branch had replaced `agent.question`/`agent.permission` Data
(a `session.InputRequest` struct) with a synthetic
`{"internal_session_id": harness}` map. The dedicated
`case session.InputRequest` now preserves the struct payload intact,
and `consultSessionID` keys the busy consult by the internal (harness)
id.

The T0.1 `agent.question_carries_the_phrase` subtest still asserted the
OLD clobber behavior — that Data is a map carrying
`internal_session_id` = harness. That assertion is now wrong: the
contract shape is the `session.InputRequest` struct (contract_gen.go),
which has NO `internal_session_id` field. The harness id is preserved
ADDITIVELY on the struct's own `SessionID` field (set by
`inputRequestFromABI`, proxy_usagestream.go:306), not on a map key.

Pin updated to assert the NEW contract (not gutted): (a) wire
`evt.SessionID == phrase`; (b) `evt.Data` is the `session.InputRequest`
struct (payload preserved, not clobbered); (c) `ir.SessionID ==
harness` (the additive-id contract on the wire); (d) `ir.Question`
survives. Sibling subtests (session.status, agent.question.resolved)
are untouched — they still cover the additive internal_session_id map
injection. The `TestTranslatePreservesInputRequestPayload` pin (piece 2's
own red-first) continues to cover the full struct-field preservation.

Tests Run:
- `go test ./api/internal/handlers/ -run TestUserStreamSessionEventsCarryPhraseID` — PASS (the previously-failing subtest now green).
- `go test ./api/internal/handlers/ -count=1` — FULL suite green (91s).
- `go test ./api/internal/services/usagestream/ -count=1` — green.
- `go build ./...` — clean.

## Correction (Rule 7 — disproved assumption, note the fix)

The "Ruling A executed" section above recorded: "State()/statusz and
the internal map stay HARNESS-keyed (the API's usagestream consumer
joins there — its own piece-1 translation handles that surface)." That
assumption is DISPROVED by the r1 review (4db41b0a's review, finding 1):
the consumer's usagestream half does NOT merely join — it re-translates.
On refreshed pods agentd emits the phrase on the wire + the harness id
additive; the consumer passes the phrase to the bridge, which runs
`translateSessionIDToPhrase` again. That calls `Register(workspace,
phrase)` → `EnsureSessionRegistered`, whose INSERT keys on `session_id`
but a phrase lives in `short_id` — so the call MINTS a new short-id row
(phantom row), re-broken the busy join, and wrote `context_used` to the
ghost row. The piece-1 translation was NOT the consumer's join; it was a
second, unguarded translation. This section (piece 3) fixes it.

## Piece 3 — the integration contract (r1 review, findings 1–3)

The r1 review (on 4db41b0a) found the two halves DOUBLE-TRANSLATE on
refreshed pods and that the lifecycle clears were still harness-keyed.
Three critical defects, all fixed here with TDD red-first pins:

### Finding 1 — double translation / phantom row (CRITICAL)
`translateSessionIDToPhrase` now guards: it reads the additive
`internal_session_id` off the event Data; when present and the wire id
is already the phrase (resolvePhrase(harness)==phrase), it passes the
phrase through WITHOUT re-registering — no phantom row, no third wire
id. A harness-keyed frame (heal window/legacy, no internal) still
translates normally. The consumer routes the busy emission through a new
`BridgeWithInternal` seam (`emitSessionStatus`) carrying
`consultSessionID(evt)` (the harness id) so the bridge can seed the
additive id onto the event; bridges that don't implement it fall back to
the plain `SessionStatus`. `SessionStatusWithInternal` is the additive
seam on the bridge.

### Finding 2 — gate latches busy (CRITICAL)
`onState` now keys the fold's `g.derived` baseline by the INTERNAL
(harness) id (`s.GetInternalSessionId()`, fallback to the wire key),
consistent with `consultBusy` (which records under the harness key).
Before: a phrase-keyed busy snapshot seeded a phrase entry that the
harness-keyed idle clear never removed → `busyNow` OR'd true forever,
the idle-drop never fired. Now snapshot + consult are the same session.

### Finding 3 — lifecycle clears harness-keyed (CRITICAL)
Added `translateSessionIDToPhrase` to the six harness-keyed clear
emission sites: idle reconcile (proxy_events.go), delete
(proxy_handlers.go), archive/unarchive (proxy_lifecycle.go), admin abort
(admin_session.go), pending-input snapshot (proxy_input.go), inbox
re-present (proxy_inbox.go). A phrase-keyed clear now matches the
phrase-keyed busy flip (the stuck-busy's lifecycle half).

### Also fixed
- `cloneSessionSnapshot` (pkg/abi/abiclient/client.go) now preserves
  `InternalSessionId` — the additive harness id was dropped by the fold,
  contradicting the "never dropped" contract and the finding-2 keying.
- `wireEvent` comment (authority.go) said "Shallow struct copy" while
  using proto.Clone — corrected.

### Red-first pins
- `TestTranslateNoDoubleTranslation_OnRefreshedPod` (handlers) — an
  already-phrase frame (phrase on wire, harness additive) must not be
  re-registered: wire stays the single phrase, harness stays additive.
- `TestGateNoLatch_PhraseKeyedSnapshotThenIdle` (usagestream) — a
  phrase-keyed busy snapshot (harness additive) then a harness-keyed
  idle must leave the gate NOT busy (no stale phrase entry).
- `TestLifecycleClearIsPhraseKeyed` (handlers) — a harness-keyed idle
  clear resolves to the phrase on the wire.

### Out of scope (documented as follow-ups, not this PR)
- Finding 4 (the mock Register returns "" for unseeded vs production
  mints) and finding 5 (no negative caching → 2s DB call per emission
  under a degraded DB) are architectural seams the r1 review lists as
  prior findings; addressing them (a read-only resolve seam, a
  sessionindex negative cache) is a separate slice. #1817 remains
  PARTIALLY addressed until the e2e pins (E2.1/E2.6) and the
  integration/e2e test levels land. (Updated: integration levels landed in
  r6; the genuinely red-first E2.1/E2.6 pins and the frontend heal-window
  seam landed in `4d07a8fc7` — see Correction (iv).)

Tests Run:
- `go test ./api/internal/handlers/ -count=1` — FULL suite green (89s), incl. the three new pins.
- `go test ./api/internal/services/usagestream/ -count=1` — green, incl. the gate-latch pin.
- `go test ./pkg/abi/abiclient/ -count=1` — green (the clone fix).
- `go build ./...` — clean.

## Piece 3 (continued) — r2 review (2026-10-21, findings 1–4)

The r2 reviewer re-opened the double-translation surface beyond the
SessionStatus leg: every OTHER emission that the consumer routes through
the translation seam can ALSO carry the additive internal id (refreshed
pods translate EVERY frame, not just status), so each needs the same
guard OR a key-by-internal fix. Plus a new defect on the delete tail.

### Finding 1 — ContextUsed double-translates (the ghost-row occupancy)
`emitContextUsed` carried `evt.GetSessionId()` (the phrase) to
`Bridge.ContextUsed`, which did `Register(workspace, phrase)` →
EnsureSessionRegistered INSERTs a phantom row and wrote context occupancy
onto the ghost. Fix: additive seam `ContextUsedWithInternal` + the consumer
passes `consultSessionID(evt)` (the harness id); the bridge keys
Register/UpsertContextUsed by the internal when present.

### Finding 2 — InputResolved double-translates (prompt never removed)
`emitInputResolved` same shape — the resolved event targeted a third id so
the frontend never cleared the prompt. Fix: additive seam
`InputResolvedWithInternal` + the bridge seeds `internal_session_id` on the
Data map so the existing translate guard passes the phrase through.

### Finding 3 — reconnect corrections double-translate
`reconcileBridgeLocked` emitted `SessionStatus(workspaceID, corr.sessionID,
busy)` with the phrase as sessionID and NO internal → the bridge minted a
phantom row on every reconcile. Fix: `bridgeCorrection` gained `internalID`
(populated from `s.GetInternalSessionId()`), emitted via
`SessionStatusWithInternal`.

### Finding 4 — delete-tail ghost row (NEW)
The delete tail (`removeActiveSessionEvents`) ran `DeleteSession` (row
removed) then the MINTING `translateSessionIDToPhrase` → Register INSERTed
a ghost row for the just-deleted session, and the `deleted` clear carried a
freshly-minted phrase the frontend does not recognize. Fix: a READ-ONLY
`SessionIndexService.ResolveShortID(workspaceID, sessionID)` (LRU-only, no
mint — the LRU survives DeleteSession which does not evict it) + a
`translateSessionIDToPhraseNoMint` twin used by the delete tail. The clear
is still phrase-keyed (joins the busy flip) but never mints.

### Finding 5 — guard downgrade under degraded index
The first r2 draft of the guard only passed through when `Register(internal)`
returned a non-empty phrase; a degraded index (Register→"") would FLIP a
healthy refreshed-pod frame back to harness space. Fixed: if the additive
internal id is present, `setInternalSessionID` + return UNCONDITIONALLY —
never downgrade a correct phrase, "never worse" holds for the refreshed-pod
shape too.

### Finding 6 — flip-leg dedup keyed on the wire (phrase) id
The `PART_START/END/MESSAGE_START/ERROR` flip-only candidates read
`g.derived[evt.GetSessionId()]` (the phrase), but consultBusy/onState record
under the CONSULT key (internal/harness id). On refreshed pods the phrase
key always missed (prev=false) → re-emitted busy on every candidate,
breaking the flip-only contract. Fixed: read `g.derived[consultSessionID(evt)]`.

### Red-first pins (r2)
- `mockSessionIndex` gained `mint`/`registerCalls`/`registeredCalls()` to
  model production's EnsureSessionRegistered minting; the double-translation
  pin asserts the phrase is NEVER registered (mint mode → no phantom row).
- `TestDeleteClearIsPhraseKeyedWithoutMint` — the delete clear is phrase-keyed
  (joins the busy flip) AND Register is never called (no ghost row for a
  deleted session).
- `ResolveShortID` no-op stubs added to every SessionIndexService test mock
  (handlers ×4, server, workspace, workflows ×2, backfill, shortid seam).

### Out of scope (still follow-ups, not this PR)
- e2e pins (E2.1/E2.6) and the integration/e2e test levels. #1817 remains
  PARTIALLY addressed until those land. (Integration levels landed in r6;
  the red-first E2.1/E2.6 pins and the heal-window seam landed in
  `4d07a8fc7`.)

Tests Run (r2):
- `go build ./...` — clean.
- `gofmt -l api/ cmd/ pkg/` — clean.
- `golangci-lint run --new-from-rev origin/main` (all 7 changed pkgs) — 0 issues.
- `go test ./api/internal/{handlers,services/sessionindex,services/usagestream,server,services/workspace,workflows}/ -count=1` — all green.
- `go test ./api/... -count=1` — green EXCEPT `outbox.TestRollbackDrill_UnderLoad`, a pre-existing FLAKY load test (count drifts 38/40/49 per run, machine-load dependent) — reproduced FAILING on the pre-change baseline (stashed diff), so unrelated to this PR.

## Piece 3 (continued) — r3 review (2026-10-21, findings 1–3)

The r3 reviewer re-opened the additive-seam design itself: the consumer's
`emitSessionStatus/emitInputResolved/emitContextUsed` passed
`consultSessionID(evt)` as the additive internal id. But `consultSessionID`
FALLS BACK to the wire session id when `internal_session_id` is empty — so a
legacy / heal-window / agentd-unresolved frame (wire = harness, no internal)
reached the bridge with `internalID == sessionID`, the guard read that as
"already translated" and SKIPPED the phrase translation. A real regression
the piece-1 comment ("legacy still translates normally") had asserted but the
r2 consumer fallback disproved.

### Finding 1 — CRITICAL: legacy / heal-window path stops translating (regression)
Fix: the three `emit*` seams now pass the RAW `evt.GetInternalSessionID()`
(empty when absent) as the additive id, and keep `consultSessionID(evt)` ONLY
as the busy-consult key. A legacy frame therefore carries `internalID=""`
through the seam and the bridge's guard passes through to the normal
harness→phrase translation. RED-first pin: `TestAdditiveSeamPassesRawInternalID`
(the recordedBridge fake now implements `BridgeWithInternal` and records the
exact internal threaded through the seam — with the bug, a legacy frame carries
`internal=s1` (the wire id) and the bridge guard would misread it; the pin
asserts `internal=""` for legacy and the harness id for refreshed).

### Finding 2 — HIGH: ContextUsed checks the tombstone in the wrong id space
`ContextUsedWithInternal` guarded on `isSessionDeleted(workspaceID, sessionID)`
(the WIRE phrase), but delete tombstones are recorded under the HARNESS id
(proxy_handlers.go delete path). A late context-usage event on a refreshed pod
(phrase on the wire, harness additive) passed the phrase check, then
`Register(rowID=harness)` re-created the just-deleted index row — the
delete-tail ghost-row class r2 finding 4 closed, re-opening on the
context-usage path. Fix: guard against BOTH id spaces (`rowID` and
`sessionID`) before the write. RED-first pin:
`TestContextUsedNoWrite_AfterDelete_TombstoneHarnessKeyed` (deletes the
harness, fires a phrase-keyed context event, asserts no Register on the
harness id).

### Finding 3 — agent.question payload ids (documented follow-up, NOT this PR)
The `agent.question`/`agent.permission` payload's `SessionID`/`RootSessionID`
are the harness ids (`inputRequestFromABI` / `inputRequestFromRecord`); the
frontend's `pendingQuestionsForSession` matches `q.sessionId`/`q.rootSessionId`
against the route phrase, so on refreshed pods the payload ids do not join the
phrase. The r3 reviewer marks this **not a new regression** (the pre-PR
envelope was harness too) — it is a separate cross-surface prompt-identity
slice. Translating the payload's ids (vs the envelope) is a different
contract change; doing it here risks the `session.InputRequest` struct
contract and is out of the T0.1 scope ruling. #1817's "agent.question joins
in the phrase space" claim is therefore still PARTIAL and is recorded as such.

### Style (r3)
- Merged the two consecutive stale doc blocks on
  `translateSessionIDToPhrase` into one.
- Corrected the `ResolveShortID` doc (interfaces + service) to say "the delete
  tail" (its only caller) instead of the over-broad "delete/abort/archive".

### Rule-7 correction (disproved assumption)
The r2 worklog still recorded "a harness-keyed/legacy frame still translates
normally" as a property of the piece-1 seam. The r2 consumer fallback
(passing `consultSessionID`) DISPROVED it — that was finding 1. The fix
above restores the property by passing the RAW internal id; the worklog's
"never worse" claim was only true for the refreshed-pod shape, not the legacy
one, until this fix.

Tests Run (r3):
- `go build ./...` — clean; `gofmt -l api/ cmd/ pkg/` — clean.
- `golangci-lint run --new-from-rev origin/main` (all 7 changed pkgs) — 0 issues.
- `go test ./api/internal/{handlers,services/usagestream,services/sessionindex,server,services/workspace,workflows}/ -count=1` — all green, incl. the two new RED-first pins.
- RED-first verified for both new pins by reverting each fix and confirming the
  pin fails (finding 1: legacy frame carries `internal=s1`; finding 2: Register
  called once on the deleted harness id).

## Piece 3 (continued) — r4 review (2026-10-21, findings 1–3 + minor 5)

r4 verified the r3 fixes are genuine but found the agentd-replica staleness
that defeats the streaming half for UI-created sessions.

### Finding 1 — HIGH / blocking: UI-created sessions never warm the replica
The emission translator (`emissionIDTranslator`) is a PURE replica read
(`shortIDFor`). The replica is populated only by (a) the boot pull, (b) the
on-miss pull invoked ONLY from MCP ref resolution, and (c) the agentd-MCP
create_session round trip. The NORMAL frontend path (POST /sessions/new →
EnsureSession → createSessionOnWorkspace) registers only in the API's
session_index and never pushes to the pod (the platform registry endpoint is
pull-only). So a UI-created session after agentd boot is unknown to the
replica: every contract-stream frame emitted the harness id and the fold join
this PR fixes STILL missed — the headline symptom persisted for the common
path. The "same TTL-cached registry the MCP surfaces read" claim was true of
the MCP path but NOT emission (no pull on emission).
Fix: warm on miss — an unresolved id in the LIVE emission path schedules a
detached, TTL-rate-limited replica pull (async, because wireEvent runs under
the authority lock; the store's put/putAll + the pull's rate-limit mutex are
independent of that lock, so the off-lock goroutine is safe). The current
frame still passes the harness id through (heal window — never worse); the
pull converges the replica so subsequent frames carry the phrase.
RED-first pin: `TestEmissionTranslator_WarmsOnMiss` (first call for an
unknown session returns "" = heal window; the async warm populates the
replica from the registry; a second call returns the phrase). Verified RED by
removing the warm (the replica never learns the UI-created session).

### Finding 2 — MEDIUM: publishInputResolved harness-keyed
The single seam for API-originated `agent.*.resolved` events built a
harness-keyed WorkspaceSSEEvent and published it directly, skipping the
translation every other user-stream session-keyed event gets. Fix: add
`translateSessionIDToPhrase` so the envelope is phrase-keyed (the #1821
piece-1 contract; impact was bounded because the frontend removes resolved
prompts by request_id, but the mixed-id-space emission violated the
contract). RED-first pin: `TestPublishInputResolvedIsPhraseKeyed` (drives the
real `publishInputResolved` with a minted session; the published envelope is
phrase-keyed, the payload keeps its own harness session_id). Verified RED by
removing the translate.

### Finding 3 — agent.question payload ids (still the documented follow-up)
r4 re-confirmed the `agent.question`/`agent.permission` payload `SessionID`/
`RootSessionID` stay harness-space (the envelope is phrase-keyed; the
payload-internal prompt-identity join is a separate cross-surface slice).
Kept in the PR body's Remaining list; the T0.1 scope ruling does not cover
it.

### Minor 5 — applyEvent + TS clone dropped InternalSessionId
The Go `applyEvent` synthesized a fresh snapshot (a session observed before
any status event) WITHOUT `InternalSessionId`, contradicting the "never
dropped" contract the clone fix honored. Fix: carry
`evt.GetInternalSessionId()` into the synthesized snapshot. The TS fold
clone (`frontend/src/session/fold.ts`) also omitted it.
RULE-7 CORRECTION (disproved claim): the r4 worklog/PR claim "the frontend
schema has no `internalSessionId` field" is FALSE — r5 caught it.
`frontend/src/abi/llmsafespaces/abi/v1/abi_pb.ts:247-249` DOES define
`internalSessionId: string` (field 7, regenerated by this PR); the actual
gap was ONLY that `cloneSessionSnapshot` enumerated fields and dropped it.
Fixed here: the TS clone now carries `s.internalSessionId`. The frontend
does not READ the field in app code today (consumers will in the future),
so the fix is contract-preserving, not behavior-changing.

### Out of scope (follow-ups, not this PR)
- E2.1/E2.6 e2e pins and the integration test level (translated authority
  stream → consumer → real bridge → real sessionindex/DB). #1817 remains
  PARTIALLY addressed until those land. (Integration level landed in r6;
  the red-first E2.1/E2.6 pins and the heal-window seam landed in
  `4d07a8fc7`.)
- TS fold clone `internalSessionId` (finding 5 frontend half).

Tests Run (r4):
- `go build ./...` — clean; `gofmt -l api/ cmd/ pkg/` — clean.
- `golangci-lint run --new-from-rev origin/main` (handlers, sessionindex,
  usagestream, workspace-agentd, abiclient) — 0 issues.
- `go test ./api/internal/{handlers,services/usagestream,services/sessionindex,server,services/workspace,workflows}/ + ./pkg/abi/abiclient/ -count=1` — all green, incl. the two new API pins.
- `go test ./cmd/workspace-agentd/ -run 'TestEmissionTranslator_WarmsOnMiss' -race` — green (the async warm has no data race).
- The full `cmd/workspace-agentd` main-package suite times out (>300s) both WITH and WITHOUT this diff — pre-existing slowness (last-running test before the baseline timeout is a relay-liveness test, unrelated); the sub-packages (sessionstate, faultmatrix) pass.
- RED-first verified for all three new r4 pins by reverting each fix.

## Piece 3 (continued) — r5 review (2026-10-21, findings 1–3 + style + Rule 7)

r5 verified the r4 fixes are real (the async warm populates the replica,
publishInputResolved is phrase-keyed, applyEvent carries the additive id)
but found the r4 warm design still broke back-to-back UI-created sessions.

### Finding 1 — HIGH: the shared 30s TTL defeats the next UI-created session
`emissionIDTranslator` scheduled the warm via `pullSessionShortIDsRateLimited`
(gated by the ONE process-wide `shortIDLastPull` 30s TTL). Flow: create
session A via the UI after agentd boot → A's first unresolved emission
triggers the one pull and sets the TTL → create session B within 30s → B is
not in the replica and every emission for B's first turn is suppressed by
A's pull, so B streams harness-keyed — the #1817 dead-streaming symptom on
the common path. Also raced the production ordering (`EnsureSession` creates
the opencode session BEFORE minting the phrase via `sessionIndex.Register`),
so an early warm could pull a registry without the new row then lock out
retries.
Fix: a DIFFERENT gate for the emission warm — `pullSessionShortIDSWarm`,
gated by an in-flight singleflight (`warmPullInFlight` atomic flag) instead
of the fixed TTL. A back-to-back session's warm proceeds once the in-flight
one completes. The MCP ref-resolution path KEEPS the fixed TTL (that
legitimate use is typo-loop throttling).
RED-first pin: `TestEmissionTranslator_WarmNotLockedByPriorPull` (A warms;
B is added to the registry after A's pull; B's emission must still warm —
verified RED by reverting the warm to the fixed-TTL gate, where B is
suppressed).

### Finding 2 — MEDIUM: unthrottled goroutine per unresolved emission
The r4 `go warmSessionShortIDsOnMiss()` fired per emission frame (wireEvent
per event, podSnapshotsLocked per snapshot row); for a session the replica
never resolves, every frame forked a goroutine that only took the global
mutex. Fix: the singleflight flag (finding 1's mechanism) coalesces to ONE
concurrent warm pull — a second unresolved emission while a warm is in
flight returns without forking.
This r5 dedup pin was later found vacuous and retired; the r6 worker moved
the bound to one long-lived worker and r7 replaced the pin with a real
live-goroutine peak measurement (see Corrections (i)–(ii) and the r7
section).

### Finding 3 — MINOR: publishInputResolved used the request context
`translateSessionIDToPhrase(c.Request.Context(), …)` — a browser
abort/navigation mid-reply would cancel the Register seam and re-emit the
envelope harness-keyed. Fix: detached `context.Background()` (matching the
other user-stream emitters).

### Style
- (The two near-duplicate guard comment blocks on
  translateSessionIDToPhrase were already merged in an earlier round; r5
  re-noted them — see the single merged block now in place.)

### Tests Run (r5):
- `go build ./...` — clean; `gofmt -l api/ cmd/ pkg/` — clean.
- `go test ./cmd/workspace-agentd/ -run 'TestEmissionTranslator'` — all 3
  pins green (warm-on-miss, cross-session-not-locked, singleflight-dedup),
  -race clean.
- RED-first verified for the cross-session pin by reverting the warm to the
  fixed-TTL gate (B suppressed). The dedup pin is structurally RED against
  the r4 per-emission fork (20 pulls vs ~1).
- Frontend: `fold.ts` cloneSessionSnapshot now carries `internalSessionId`
  (type-correct — field 7 on the SessionSnapshot schema type); node_modules
  is not installed in this worktree so `tsc` is not run locally (the CI
  Frontend job typechecks).

## Piece 3 (continued) — r6 review (2026-10-21, findings 1–4,6)

r6 verified the r5 convergence story but found the emission warm itself was
still the wrong shape: it fired a goroutine per unresolved emission and its
"dedup" pin did not measure what it claimed. The contract-stream half was
already shipped (Ruling A); the remaining work was bounding the replica
warm and telling the truth about its test coverage.

### Finding 1/3/4 — one worker, queued pending ids, bounded pulls
Replaced the per-emission `go warmSessionShortIDsOnMiss(...)` (and its
in-flight CAS, which DROPPED overlapping ids) with ONE long-lived worker
started via `sync.Once` (`session_ref.go`). Misses now only enqueue the id
and signal a bounded wake channel; the worker coalesces the pending set into
one pull. An id that arrives while a pull is in flight stays pending and is
pulled on the next loop — never dropped. Unresolved ids stay pending under
per-id exponential backoff (1s, doubling, 30s cap), with idle-entry pruning
(10m) and a hard map ceiling (4096), so a hot stream neither forks a
goroutine per frame nor pulls once per frame. At the r6 commit the worker
was started with `bgCtx`, but it was NOT registered on `bgWg` and
`runShutdown` waited only on `bgWg` — so the "shutdown joins the worker"
claim was not yet true. r7 fixes the join (see the r7 section below).

### Correction (i) — "no goroutine-per-emission" was FALSE before this commit
The r5 worklog claimed the singleflight replaced the per-emission fork. It
did not: `warmSessionShortIDsOnMiss` still ran `go ...` per emission, and the
singleflight only coalesced the *pull*, not the goroutine. That claim is true
ONLY after the r6 worker commit (56afc7251); the earlier wording is annotated
here as disproved rather than silently rewritten.

### Correction (ii) — both dedup pins were vacuous (r7)
The r5 dedup pin counted PULLS while the pull was blocked, not goroutines; the old implementation already
coalesced pulls, so it passed against the code it was meant to reject.
The r6 replacement counted worker STARTS via the test-only
`shortIDWarmWorkerStarts`. That counter stays at 1 even if a per-emission
`go` is reintroduced, so it was vacuous too, and the counter has been
REMOVED from production. r7 replaces it with a real live-goroutine pin:
the registry pull blocks in `httptest`, 50 distinct unresolved ids are
emitted, and a peak sampler plus a blocked-pull barrier assert the
goroutine delta stays <= baseline+8. Mutation evidence (run, not assumed):
- reintroducing `go warmSessionShortIDsOnMiss(harnessID)` makes the peak
  sample well above the +8 bound (observed 18–27 goroutines on the audit
  host) → the pin FAILS;
- the single `sync.Once` worker stays inside the bound → the pin PASSES.

### Correction (iii) — the contract-stream half DID ship
The header previously scoped it "for ruling"; Ruling A executed it in piece
3 (agentd `IDTranslator` + `wireEvent` + snapshot translation). The header
is corrected; the historical ruling-request section is retained as history,
not as current status.

### Correction (iv) — integration levels, the genuinely red-first E2E, and the heal-window seam (r8)
The earlier "integration levels are a follow-up" notes are now closed for
this slice by the Postgres-backed tests below. The GENUINELY red-first
E2.1/E2.6 pins and the frontend heal-window id seam land in `4d07a8fc7`
(not `bce1579c`). `bce1579c`'s E2E was post-fix phrase-keyed and passed on
`origin/main`, so it did not reproduce the divergence; `4d07a8fc7` instead
drives PRE-FIX wire shapes on the surface under test and is verified red on
`origin/main` (tests only, no seam) at the predicted assertions, then green
on the branch.

The seam (`frontend/src/session/sessionAliases.ts`, consumed by
`useContractStream.ts` and `SessionActivityProvider.tsx`):
- learns per-workspace (harness → phrase) pairs from ADDITIVE ids
  (`data.internal_session_id` on user-stream events, `internalSessionId` on
  contract frames);
- is bounded at 512 aliases per workspace with FIFO eviction;
- is the IDENTITY when no pair is known (never worse than pre-fix);
- refuses an equal-id "pair" so a same-id decoy cannot alias onto the phrase
  session;
- late-re-keys fold rows that landed under the harness key before the pair
  was learned (`rekeySessionMap`), publishing only when the map changed.

Honest limitation: if BOTH streams are fully pre-fix — no additive id
anywhere — no mapping exists, so a refresh/bounce is still required. The
seam covers the real skew cases (API-new/agentd-old and
API-old/agentd-new); it does not invent a mapping from nothing.

The Postgres-backed test levels:
- `api/internal/handlers/usagestream_phrase_integration_test.go` — real
  Postgres, real `sessionindex`, real `sessionstate.Authority`, real
  `abiclient`, real `usagestream.Consumer`, real `usageBridge`, real
  `UserEventBroker`: phrase-keyed user event + additive harness id,
  context_used=115 on the HARNESS row, zero phrase-keyed phantom, the
  three-surface join, the reconnect correction minting nothing, and the
  dead-index legacy/translated guard.
- `api/internal/handlers/session_registry_integration_test.go` — POST
  `/internal/v1/session-register` mints, GET `/internal/v1/session-registry`
  returns `{sessionID: harness, shortID: phrase}`, and the index row joins.
- `cmd/workspace-agentd/session_warm_integration_test.go` — the real
  replica + warm worker against an `httptest` registry contract over real
  Postgres: heal window → worker pull → phrase; a 500 leaves the replica
  cold with no panic.
- These run in `secrets-integration.yml` alongside the board integration
  steps; the unit-level worker pins remain in `session_ref_test.go`.

### Tests Run (r6):
- `go build ./...` — clean.
- `go test -tags=integration -count=1 -race -run 'TestIntegrationUsageStream' ./api/internal/handlers/` — PASS.
- `go test -tags=integration -count=1 -race -run 'TestIntegrationSessionRegistry' ./api/internal/handlers/` — PASS.
- `go test -tags=integration -count=1 -race -run 'TestIntegrationEmissionWarm' ./cmd/workspace-agentd/` — PASS.
- `go test ./cmd/workspace-agentd/ -run 'TestEmissionTranslator|TestWarm|TestShortIDStore' -count=1` — green.
- `go test ./api/internal/handlers/ -count=1` — green.


## Piece 3 (continued) — r7 review (2026-10-21, Go-side CI + findings 3–7)

r7 closed the automated reviewer's Go-side findings and the two red CI
checks.

### CI 1 — contextcheck (agentd)
`ensureShortIDWarmWorker` no longer takes a nil/`context.Background()`
fallback: the worker is bound once to the process `bgCtx` at boot and the
context is passed as a parameter through the goroutine closure. The miss
path (`warmSessionShortIDsOnMiss`) only enqueues + signals — it never
starts the worker, so there is no non-inherited `Background` context.
Tests start the worker explicitly with `t.Context()` / join via
`warmWorkerWg`.

### CI 2 — reconnect test flake (`TestReconnectSeedReconcilesBridgeTruth`)
Root cause was in the `fakeClient.Stream` test double, not the production
consumer: `requireOpen` returns as soon as `connected` is signalled, but the
fake created its `kill` channel AFTER that signal. If the test's
`killStream()` won that race it saw a nil channel and silently did nothing —
no reconnect, and the 5s `Eventually` failed. The fake now (a) creates the
kill channel and snapshots the seed BEFORE signalling `connected`, and
(b) signals only AFTER applying the initial snapshot, so `requireOpen` also
guarantees the initial `onState` finished. No timeout was raised; the
synchronisation is deterministic. `-race -count=10` on the package is green.

### Finding 3 — real goroutine-per-emission pin
See Correction (ii): the vacuous `shortIDWarmWorkerStarts` counter and its
test are gone; the replacement measures live goroutines under a blocked
pull and fails against the reintroduced per-emission `go`.

### Finding 4 — worker cancelled and bounded at shutdown
The warm worker is now started at the TOP of `startBackgroundLoops`,
before any emitter goroutine, via
`ensureShortIDWarmWorker(bgCtx, bgWg)`: `bgWg.Add(1)` + `Done` in the
worker goroutine, so `runShutdown`'s `bgCancel` + `bgWg.Wait` cancels it
and waits on it for up to 5s; the pull's own 10s context bounds the
remainder. Source comments were updated to match.

### Finding 5 — global emission-path pull-rate bound
Per-id backoff bounds a repeated id, but N DISTINCT newly-seen ids are each
immediately due and caused ~N pulls. Added
`shortIDWarmMinPullInterval = 1s`: a global floor between warm pulls that
batches distinct ids while still letting a new session warm within a
second (NOT the retired 30s cross-session lockout). New pin:
`TestEmissionTranslator_GlobalPullRateBound` (with an injected hour-long
floor, 25 distinct ids yield exactly one pull; with a zero floor,
sequentially emitted distinct ids pull once each). Mutation evidence:
removing the worker's floor wait makes the floor subtest fail; the
zero-floor subtest still passes.

### Finding 6 — legacy-frame translation pin at the live-DB level
Added `TestIntegrationUsageStream_LegacyFrameTranslatesToPhrase`: a real
Postgres `sessionindex`, real authority/consumer/bridge, and a LEGACY
harness-keyed frame with NO additive internal id. The user-stream event's
`session_id` must become the PHRASE, with the harness id preserved
additively and no phantom row. Mutation evidence: deleting
`evt.SessionID = phrase` in `proxy_usagestream.go` makes the test fail
(the user event never arrives phrase-keyed); restoring it passes.

### Finding 7 — CI change gate
`secrets-integration.yml`'s diff gate now includes
`api/internal/handlers/pod_session_registry.go` (guarded registry test),
plus `pkg/shortid/` (transitively imported by the guarded sessionindex
tests), `pkg/abi/v1/` (the phrase integration test's event types), and
`api/internal/handlers/proxy_handlers.go` (part of the guarded handlers
package). Workflow edits count as changed, so the workflow validates itself.

### Mutation coverage — what is and is not pinned
- Per-emission goroutine pin: mutation-verified (see above).
- Global pull-rate floor: mutation-verified (interval 0 → red).
- Legacy phrase translation: mutation-verified (line removal → red).
- Per-id exponential backoff: pinned behaviorally by
  `TestEmissionTranslator_ThrottlesUnresolvedPulls`, but r7 did NOT
  mutation-verify that pin.
- Prune / ceilings (`shortIDWarmPruneLocked`): r7 had no pin. r9 adds
  `TestShortIDWarm_Prune` (expired keys leave both maps, live entries
  survive, the oldest surplus is evicted at the 4096 backoff ceiling). It
  pins correctness; the single-pass rewrite is structural (no
  timing-based pin asserts the absence of repeated scans).
- Pending-set hard cap (`shortIDWarmMaxPending`): r9 pin
  `TestShortIDWarm_PendingCap`, mutation-verified (cap removed → red).
- Shutdown join: r9 pin `TestShortIDWarm_ShutdownJoinsWorker`,
  mutation-verified (`Done` moved ahead of the loop → red).

### Tests Run (r7)
- `go build ./...` — clean; `go vet` on the changed packages — clean.
- `golangci-lint run ./cmd/workspace-agentd/... ./api/internal/...` — 0 issues.
- `go test -race -timeout 300s ./api/internal/services/usagestream/ -count=10` — green.
- `go test -timeout 180s ./cmd/workspace-agentd/ -run 'TestEmissionTranslator|TestWarm|TestShortIDStore' -count=1` — green.
- `go test -tags=integration -race -count=1 -run 'TestIntegrationUsageStream' ./api/internal/handlers/` (real Postgres) — green.

## Piece 3 (continued) — r8 review (2026-10-21, merge-hygiene minors)

r8 was the docs/PR-body accuracy round plus three minimal findings. The
E2E ownership correction is in Correction (iv); the PR body was rewritten to
describe the current design and no longer claims there are no E2E pins, an
"unexercised" integration level, or the retired dedup pin.

### Minor 3a — lifecycle clears pinned at the CALL SITES
`TestLifecycleClearIsPhraseKeyed` only called the translator directly, so
deleting a call site would not fail anything. `TestLifecycleClearCallSites-
ArePhraseKeyed` now drives the REAL paths — `reconcileSessionState` (idle),
`removeActiveSessionEvents` (delete, and asserts no mint), and
`PublishSessionArchived` (archive) — plus the admin force-abort HTTP route,
asserting the event that reaches the workspace broker is phrase-keyed.
Mutation evidence (run, not assumed): removing the translate call from each
site makes its subtest fail with the harness id on the wire
(`ses_clearsite_1817` instead of `november-foxtrot-2`). The pending-input /
inbox re-present prompts are covered by the existing phrase pins for those
emissions; they are prompts, not lifecycle clears.

### Minor 3b — T1.6 covers the third surface
`TestUserStreamSessionEvents_JoinableWithSessionList` now builds a real
`sessionstate.Authority` with an `IDTranslator`, ingests a harness-keyed
busy event, and asserts the subscribe SNAPSHOT carries the same join key
(`sessionId` = phrase, `internalSessionId` = harness). The tripwire therefore
covers all three surfaces: list row, user stream, contract projection.
Mutation evidence: disabling the translation in `podSnapshotsLocked` makes
the test fail ("the contract snapshot must contain the session join row").

### Minor 3c — `session_id_deprecated` on a translation miss (#1821 degrade rule)
On a translation miss the harness id still passes through (never worse) but
map-shaped `session.status` / `agent.*.resolved` payloads now carry
`session_id_deprecated: "true"` (`markDeprecatedSessionID`, matching the
existing MCP `session_id_deprecated` string convention). Successful
translations carry no flag. The struct-shaped `agent.question` /
`agent.permission` `session.InputRequest` payload is the documented
EXCEPTION: it is never clobbered to carry a marker (the same clobber the
piece-2 fix preserves). Pins: `TestTranslationMissFlagsSessionIDDeprecated`
(miss flagged for status + resolved; healthy translation unflagged; struct
payload intact). RED first: both miss subtests failed before the helper was
added.

### Deliberately deferred
- The `agent.question` / `agent.permission` payload ids
  (`SessionID`/`RootSessionID`) remain harness-space; only the ENVELOPE is
  phrase-keyed. The payload-internal join is a separate cross-surface contract
  change (PR body "Remaining").
- Delete-clear against a cold/warm-LRU miss passes the harness id through
  (now flagged deprecated) — it never mints a ghost row by design.

### Tests Run (r8)
- `go build ./...` — clean.
- `golangci-lint run ./api/internal/... ./cmd/workspace-agentd/...` — 0 issues.
- `go test -timeout 180s ./api/internal/handlers/ -count=1` — green (97s).
- `go test -race -timeout 300s ./api/internal/services/usagestream/ -count=1` — green.
- Postgres integration (`TEST_DATABASE_URL`, tags=integration): usage-stream
  (translated, legacy, reconnect, dead-index) + registry round trip + agentd
  emission warm — all PASS.
- `make repolint` — all checks passed (the NNNN sentinel warning is the
  expected post-merge numbering note).
- Mutation evidence for every new pin is recorded in the findings above.

## Piece 3 (continued) — r9 review (2026-10-21, shutdown join + pending bounds)

r9 audited the r8 head and found two verified defects in the warm worker
plus a false status claim. Both defects are fixed here; the claims in r7's
Finding 4 and the CHANGELOG are now true in code, not aspirational.

### Finding 1 — shutdown join released before the worker ran
`ensureShortIDWarmWorker` registered `lifecycle.Add(1)` but called
`lifecycle.Done()` at the TOP of the goroutine, before
`shortIDWarmWorker(ctx)`. `bgWg.Wait()` therefore returned immediately at
shutdown while the worker kept running — the opposite of the comment,
worklog, and CHANGELOG. Fixed by deferring `lifecycle.Done()` alongside
`defer warmWorkerWg.Done()`, so release happens when the loop exits.

Pin: `TestShortIDWarm_ShutdownJoinsWorker` holds a registry pull in flight
and the replica write lock, cancels the worker context, then asserts
`lifecycle.Wait()` stays blocked while the worker is alive and returns only
after both are released (3s timeout; `warmWorkerWg` independently confirms
exit). Mutation evidence (run, not assumed): moving `lifecycle.Done()` back
ahead of `shortIDWarmWorker` makes the pin fail with "lifecycle.Wait
returned while the warm worker was still in flight".

### Finding 2 — pending set unbounded; prune O(n²)
`warmPending` was an uncapped map (the 4096 ceiling only bounded
`warmBackoff`), and the ceiling loop rescanned the whole map once per
deletion while holding `warmMu`.

- `shortIDWarmMaxPending = 4096`. `shortIDWarmAddPendingLocked` enforces it
  on every enqueue (including settle re-enqueues): an id already pending is
  a no-op, and on overflow the NEWEST id is dropped so the existing pending
  backlog survives. A dropped id re-enqueues only on its next emission;
  per-id backoff alone never re-enqueues it.
- `shortIDWarmPruneLocked` is now one pass: collect expired keys and, when
  over the backoff ceiling, the excess oldest by `lastSeen`; delete the
  collected victims in one loop. It never clears `warmPending` wholesale.

Pins: `TestShortIDWarm_PendingCap` (3x the cap of distinct ids never
exceeds it; a queued id survives the overflow while the newest is refused)
and
`TestShortIDWarm_Prune` (expired removed from both maps, live preserved,
oldest surplus evicted). Mutation evidence: removing the cap check makes
the pending pin fail (warmPending grows to 12288, want exactly 4096).

### Finding 3 — stale status wording
The header claimed "r1–r8 review rounds closed"; it now says r1–r7 findings
addressed with r8/r9/r10 fixes below, which matches the record. The r7
mutation-coverage bullet no longer calls the prune UNTESTED.

### Tests Run (r9)
- `go build ./...` — clean.
- `golangci-lint run ./cmd/workspace-agentd/...` — 0 issues.
- `go test -race -timeout 180s ./cmd/workspace-agentd/ -run 'TestEmissionTranslator|TestWarm|TestShortIDStore|TestShortIDWarm' -count=5` — green.
- `go test -timeout 600s ./cmd/workspace-agentd/ -count=1` — green (411s;
  the 180s cap is below the package's pre-existing full-suite runtime, not a
  regression from this change).
- `make repolint` — all checks passed (the NNNN sentinel warning is the
  expected post-merge numbering note).

## Piece 3 (continued) — r10 review (2026-10-21, barrier safety + retention)

r10 found three remaining robustness defects in the r9 head: agentd tests
could hang their own cleanup, pending overflow discarded the retry backlog,
and the global pull-floor pin still raced wall-clock sleeps.

### Finding 1 — barrier release must survive `t.Fatal`
Several blocked-handler tests closed their release channel only on the
success path. A `t.Fatal` before that point ran `t.Cleanup`, whose
`httptest.Server.Close` waits for the handler, which waited forever on the
unclosed channel — so a failure became a package timeout. Every such
barrier now uses `sync.OnceFunc` + `defer` (the release is idempotent and
runs on both success and Goexit): `agentz_test.go`,
`control_socket_test.go`, `control_socket_concurrency_test.go`,
`mcp_secrets_resync_test.go`, `readyz_d4_test.go`, and both blocked-pull
tests in `session_ref_test.go`. Verified by forcing an early failure after
the pull barrier in a scratch worktree: the failing test process cleaned up
in 0.04s instead of hanging to the timeout.

### Finding 2 — pending overflow must keep the retry backlog
The r9 overflow path replaced the whole `warmPending` map, dropping every
id already queued (the reverse of what the code comment claimed). It now
enqueues only when there is room: an id already pending is a no-op, and on
overflow the NEWEST id is refused while the existing backlog survives. A
refused id re-enqueues only on its next emission — `warmBackoff` alone never
re-enqueues it. `shortIDWarmPruneLocked` no longer clears `warmPending`
wholesale (enqueue owns the cap). `TestShortIDWarm_PendingCap` now asserts
an existing id survives, the newest is refused, and the refused id requeues
once room frees; `TestShortIDWarm_Prune` no longer expects the old reset.

### Finding 3 — deterministic pull-floor pin
`TestEmissionTranslator_GlobalPullRateBound` slept 10ms per id and 100ms
afterwards, racing the 1s floor. The floor is now an injectable variable
(`shortIDWarmMinPullInterval`, still 1s in production) with a test-only
`warmFloorWaitNotify` observer. The test waits on positive signals: with an
injected hour-long floor, 25 distinct ids yield exactly one pull; with a
zero floor, sequentially emitted distinct ids pull once each. No wall-clock
sleeps remain in the assertion.

### Tests Run (r10)
- `gofmt -l` on changed files — empty.
- `go build ./...` — clean.
- `golangci-lint run ./cmd/workspace-agentd/... ./api/internal/...` — 0 issues.
- `go test -race -timeout 300s ./cmd/workspace-agentd/ -run 'TestShortIDWarm|TestEmissionTranslator|TestWarm|TestShortIDStore' -count=5` — green.
- `go test -timeout 600s ./cmd/workspace-agentd/ -count=1` — green (416s).
- `go test -race -timeout 300s ./api/internal/services/usagestream/ -count=1` — green.
- `make repolint` — all checks passed (same NNNN sentinel note).
- Postgres integration: skipped (no database available in this environment).

## Piece 3 (continued) — r9/r10 frontend: observable alias graph + activity re-key (2026-10-21)

The r9/r10 sections above document only the agentd worker/pending/prune/
pull-floor work. This section is the frontend half of those two rounds.
Commit split: `9ac402f0` is r9 (observable alias learning + hot-path guard)
and `ef9fdb52` is r10 (provider activity re-key + batched snapshot
learning). The r11 companion re-key/purity fixes are called out separately
at the end because they are a sibling commit, not part of r9/r10.

### `frontend/src/session/sessionAliases.ts` — the observable alias graph

- **Additive-pair learning.** `translateSessionIds` accepts the pair shape
  emitted by the fixed API/agentd (wire id = phrase, additive internal id =
  harness) and teaches it through `learnSessionAlias`. A legacy frame with
  no additive id resolves through the learned table and carries the harness
  id additively, so downstream consumers never lose it.
- **Per-workspace bounded map.** `aliasesByWorkspace` is keyed
  workspace → (harness → phrase), insertion-ordered. Each workspace holds at
  most 512 aliases with FIFO eviction (`MAX_ALIASES_PER_WORKSPACE`), and at
  most 1024 workspaces with oldest-workspace eviction (`MAX_WORKSPACES`).
  One workspace's ids can never resolve in another.
- **Input guards.** `isBoundedId` rejects empty ids, ids over 256 chars
  (`MAX_SESSION_ID_LENGTH`), and ids containing whitespace or control
  characters (including DEL), because frame fields are untrusted input.
- **Equal-id decoy guard.** A "pair" whose wire and internal ids are equal
  teaches nothing. If that id is a known harness, the frame is left alone so
  it cannot alias a stale/decoy frame onto the phrase session.
- **Identity when no pair is known.** With no additive id and no learned
  mapping, translation is the identity — exactly the pre-fix behavior, never
  worse.
- **Monotonic version + listeners.** `learnSessionAlias` returns `changed`,
  bumps the process-wide `sessionAliasVersion()`, and notifies
  `subscribeSessionAliases` listeners synchronously with the changed
  workspace. Re-learning the same pair, or a malformed/equal pair, is a
  no-op and does not bump the version.
- **Batched learning.** `beginSessionAliasBatch` / `endSessionAliasBatch`
  collapse per-pair listener notifications into one notification per changed
  workspace at the outermost close (nested batches flush only at depth 0).
  The version still advances per learned pair.
- **Late fold re-key.** `rekeySessionMap` moves a legacy row from its
  harness key to the phrase when a pair is learned afterwards, preserving the
  harness id on the snapshot. It skips the same-id decoy and does not
  overwrite an existing phrase row (that row is the newer projection). It
  returns whether the map changed, and callers publish only on true.

### `frontend/src/hooks/useContractStream.ts` — applied-only learning + hot path

- **Applied frames only.** The event path checks `seq <= fold.seq` BEFORE
  translating, so a stale or duplicate frame can never overwrite a newer
  harness→phrase mapping. Snapshot rows are translated only when the
  snapshot is applied.
- **Alias-version hot-path guard.** `rekeyIfAliasesChanged` compares
  `sessionAliasVersion()` against `aliasVersionRef`; the O(all sessions)
  walk runs only when the version moved. PART_DELTA and other alias-free
  frames therefore do not re-key. The subscriber also consumes unrelated
  workspace changes without touching this workspace's fold.
- **Snapshot ordering.** r9 re-keyed the fold before applying the snapshot.
  r10 changed this to: translate all snapshot rows inside one alias batch,
  apply the snapshot, then run at most one re-key and publish one state —
  the subscriber is suppressed while `applyingSnapshot` is true, so a
  multi-pair snapshot no longer re-keys/publishes once per pair. The batch
  is closed in `finally`, so a translation throw cannot leave it open.
- **Single publish.** The snapshot path calls `publish()` once after the
  batch/re-key and then `onSnapshot`; it does not publish per learned pair.

### `frontend/src/providers/SessionActivityProvider.tsx` — late-learn activity re-key

r10 subscribes to `subscribeSessionAliases`. When the changed workspace
matches, it re-keys the provider's session-keyed activity projections
through that workspace's alias table:

- `busySessions` and `pendingUnread` (`rekeyWorkspaceValueMap`, scoped by
  the map's workspace value; an existing phrase key wins);
- `abortedSessions`;
- `pendingActions`, including `pendingActionWsRef` and
  `requestToSessionRef` (request-id sets merge into the phrase key);
- `pendingQuestionContent` and `pendingPermissionContent`
  (`rekeyPendingContent` resolves both `sessionId` and `rootSessionId`, and
  refuses to move a request known to belong to another workspace);
- `clearedRef` entries for that workspace.

### r11 companion commit — StrictMode purity, deleted/staging re-key

The r11 frontend review found two gaps in the r10 shape: some provider refs
were mutated inside `setState` updaters (which React StrictMode may invoke
twice), and `deletedSessions` was not re-keyed. The code fix is a sibling
commit, **r11 frontend fixes applied in the companion commit on this
branch** (this docs commit changes no code):

- ref mutations are hoisted OUT of the state updaters — the next map/set and
  any ref updates are computed from refs first, then the updater only
  returns state, so a double-invoked updater stays pure/idempotent;
- `deletedSessions` is re-keyed on a late-learned pair;
- per-flight and legacy staging maps are resolved at commit before their
  rows are handed to the pending-action updater.

### Tests / pins

- `sessionAliases.test.ts`: additive learning, legacy translation,
  identity-before-pair, same-id decoy, empty/equal rejection, workspace
  scoping, re-mint, the 512/1024 bounds, version/listener semantics, input
  guards, late `rekeySessionMap` (including decoy/existing-phrase), and the
  hot-path re-key counter.
- `useContractStream.test.tsx`: a late user-stream learn re-keys and
  publishes the fold; PART_DELTA runs no re-key when the version is
  unchanged; a multi-pair snapshot produces one alias notification and at
  most one re-key; a stale duplicate cannot overwrite the mapping.
- `SessionActivityProvider.aliasRekey.test.tsx`: harness-keyed busy moves to
  the phrase and idle then clears it; unread/aborted/pending-action keys
  move too; another workspace's alias does not move the key.
- E2E E2.1/E2.6 remain the genuinely red-first pins recorded under r8: they
  drive pre-fix wire shapes on the surface under test and were verified
  failing on `origin/main` (tests only, no seam) at the predicted
  assertions, then passing on the branch. This docs change did not re-run
  them.
- r11 pins reported by the companion commit: StrictMode pending-action,
  deleted-session re-key, staging/commit ordering, fold reseed. They are
  attributed to that commit and were not independently run by this docs
  change.

No mutation verification is claimed for the frontend pins in this section.
The agentd mutation evidence recorded in the r9/r10 sections above stands.

### Sidebar-only healing limitation

Learning happens wherever a translator observes the pair. The contract
stream is mounted by ChatPage only; sidebar-only pages run just the user
stream. With a pre-fix API (harness-keyed user events, no additive id) and a
post-fix agentd (phrase-keyed contract rows carrying `internalSessionId`), a
sidebar-only page has no source for the pair, so it cannot heal until a chat
view has been opened once in this tab (the alias map is module-level and
outlives the page). Healing sidebar-only pages requires hoisting the single
contract-stream connection to the shell — not a second SSE connection — an
architectural change outside this seam.
