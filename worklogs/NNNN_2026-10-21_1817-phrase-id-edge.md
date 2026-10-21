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
