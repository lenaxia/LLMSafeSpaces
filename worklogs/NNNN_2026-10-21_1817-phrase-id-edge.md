# Worklog: #1817 — piece 1: user-stream session events carry the phrase (#1821 rapid slice)

**Date:** 2026-10-21
**Session:** continuation worker (orchestrator rapid-fix dispatch after the #1821 root-cause ruling). Worktree `wt-1817`, branch `fix/1817-streaming-busy-regression` off main @ 644446fa.
**Status:** user-broker half complete; the contract-stream half scoped for ruling (see Next).

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
  integration/e2e test levels land.

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
  PARTIALLY addressed until those land.

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
  PARTIALLY addressed until those land.
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
RED-first pin: `TestEmissionTranslator_WarmDedupsToSinglePull` (20 emissions
while the pull is blocked → the pull runs ~1×, not 20×).

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
