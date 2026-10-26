# Worklog — #1959c: re-dispatch phrase resolution at the V1 seam

Lane: `fix/1959c-phrase-resolution` (charter lane-1959c). Issue: #1959 root
cause 2 — comments 6425743005 (ROOT CAUSE 2, read first) and 6420872512
(root cause 1 / #1963 context), both read in full before starting.

## 1. The defect (why #1963 was not enough)

#1963 moved the bounce re-dispatch + halt/resume directives onto the LIVE V1
route (`adapter.Send`, `POST /session/:id/message`). The second live probe on
0.44.3 (tsk-fragrant-brochure-reprints) STILL fell back to a fresh spawn:
the engine's `assignee_session`/`implementer_session` store the platform
PHRASE (durable identity, #1850/#1816 — the platform resolves phrases at ITS
layer), opencode's own routes key on RAW `ses_` ids, and the adapter posts
the id VERBATIM with zero phrase-resolution code (adapter.go:359 — confirmed
by grep). So the V1 POST was `/session/florid-teapot-verifies/message` → 404
→ the 10s fast-fail returned it → pkg/board's by-design fresh-worker
fallback. The #1963 wiring pins could not see this: their stub opencode
ACCEPTED any session id on V1.

## 2. Mechanism check (where phrase→raw actually lives)

Traced the full identity chain before choosing the fix surface:

- Platform registry: `session_index` (`workspace_id`, `short_id`=phrase) →
  `session_id` = raw harness id. Read by the route-edge middleware
  (`SessionRefResolution` → `FindSessionByShortID`,
  api/internal/middleware/session_ref.go) and PULLED by the pod-side replica
  (`cmd/workspace-agentd/session_shortids.go`, `resolveShortID`) — the layer
  `llmsafespaces_send_message` resolves through, live-proven.
- Spawn/stamp chain: `newBoardSpawner` returns agentd's node-exec
  `sessionId`; `stampAssignee` writes it; the forward exit captures
  `implementer_session=assignee_session` (stage.go); the bounce mints
  `assignee_session=<implementer>` IN the re-claim and nudges the
  re-dispatch with the SAME value (bounceTx/bounceImplementer) — live
  evidence (0.44.3) is that this stored value is the PHRASE.
- The stored-rows-canonical posture (#1922: raw stays canonical in rows,
  translation at the read edge) means BOTH forms must keep working: the seam
  resolves when the ref is phrase-shaped, passes raw through untouched.

## 3. Charter's preferred-fix check — honest finding

The charter sketched resolution "via a session-list lookup —
pkg/agent/opencode already has list+ParseSessionListWire". CHECKED, and the
production opencode session list (GET /session, translate.go's
ParseSessionListWire input) carries NO phrase field — the list is raw-keyed
ids + titles; the phrase is platform-layer identity (#1816) that only the
platform registry and the pod replica know. A list-based resolution is
structurally impossible against real opencode — it would have been a second
accept-anything fiction. The charter's alternative — "the platform's own
message machinery" — was evaluated: the full HTTP route hop (own API +
SessionRefResolution middleware) is reachable but changes liveness semantics
(self-call plumbing + auth; the proxied send waits the whole turn — no
fast-fail window control). The RESOLUTION HALF of that machinery
(session_index, the exact `FindSessionByShortID` query) IS reachable from
the api side as a direct consult with identical authority. Fix surface:
resolve at the delivery seam via the platform registry, V1 Send the raw id,
liveness semantics untouched.

## 4. Validate-first (born RED, commit 0fe64e764)

`api/internal/app/board_worker_comms_phrase_test.go` — a REAL-SHAPE stub
(`stubRealOpencode`): `POST /session/:id/message` and `/abort` answer 404
for every id outside the live RAW session set (phrase ids are NEVER
members); `GET /session` serves the 1.18.x bare-array list with no phrase
field. The phrase→raw binding lives ONLY in the injected registry stand-in
(`stubPhraseRegistry`, the session_index seam — the same injection pattern
as stubScope for the pool), so the SEAM's resolution is the only path to
green.

Born RED (captured verbatim — the live 0.44.3 signature):

```
Error: "agent http status: POST /session/florid-teapot-verifies/message returned 404: {\"error\":\"session not found\"}"
Messages: a live phrase-addressed implementer must receive the re-dispatch ...
--- FAIL: TestBoardWorkerComms_PhraseResolution_RedispatchPostsToRawID
--- FAIL: TestBoardWorkerComms_PhraseResolution_UnknownPhrase_FastFailKeepsFallback   (delivery attempted: 1 post)
--- FAIL: TestBoardWorkerComms_PhraseResolution_RegistryFailure_IsFallbackError       (lookup never consulted)
--- FAIL: TestBoardWorkerComms_PhraseResolution_DirectivesResolveToo                  (halt never landed)
--- FAIL: TestBoardWorkerComms_PhraseResolution_AborterResolvesToo                    (POST /session/florid-teapot-verifies/abort 404)
```

`RawRefsStillDeliver` green before AND after (the raw regression guard).
Red commit carried the #1963-precedent enabler only (wrapper type +
aborter constructor shape + app.go wiring; delivery logic UNCHANGED — the
pins prove it by failing).

## 5. Fix (commit d29350587)

`api/internal/app/board_worker_comms.go`:

- **`deliverBoardTextV1` resolves inside the detached goroutine, before
  `Send`**: the ref → `sender.ResolveSessionRef` → RAW id → V1 POST.
  Resolution rides the same fast-fail window: unresolvable phrase (reaped/
  archived original) or registry outage = the seam's synchronous liveness
  error (pkg/board's fallback input); a hanging consult can never block the
  verdict request past the window; a resolved send keeps every #1963
  semantic (detached, WithoutCancel, 30m budget, fastFail=0 directives).
- **`boardV1Sender` GROWN — the narrowing discipline strengthened**:
  `ResolveSessionRef` (+`Abort` for the interrupt leg — same disease: a
  verbatim-phrase abort 404s and the in-flight claim's interrupt silently
  never lands). The pre-#1959c assertion `var _ boardV1Sender =
  agent.Adapter(nil)` is DELIBERATELY GONE: a bare adapter no longer
  satisfies the seam — unresolved wiring is a COMPILE ERROR (demonstrated
  live: the #1963 test file needed rewiring to compile).
- **`boardResolvingSender`** — the only production implementation
  (`var _ boardV1Sender = (*boardResolvingSender)(nil)`): adapter V1 legs +
  `boardPhraseLookupFor(pool)` (session_index consult, FindSessionByShortID's
  exact query). Phrase gate = `shortid.IsShortIDForm` (case-insensitive);
  RAW/legacy refs pass through byte-identical (the #1663 lesson — resolution
  never makes a live session unaddressable; raw consults are free). Registry
  miss → `phrase resolves to no live session`; registry error → wrapped
  lookup error (never a silent verbatim POST).
- **`newBoardAborter(scope, sender)`**: resolve → abort; also refactored to
  the injected-scope constructor shape (the #1963 enabler pattern).
- app.go: `boardSender := newBoardResolvingSender(agentAdapter,
  boardPhraseLookupFor(boardPool))` wired into all three seams.

Deliberately untouched: pkg/board's decision tree (bounceTx, nudge,
fallback), the fast-fail window/budget knobs, #1958 RunID, the #1922
read-edge translation, the #1963 route pins (rewired through the wrapper,
assertions unchanged).

## 6. Mutation-verified, then restored

- **Deliver-path bypass** (`raw := sessionID` — post the ref verbatim):
  RedispatchPostsToRawID / UnknownPhrase / RegistryFailure / Directives RED;
  suite green after restore.
- **Aborter bypass** (`raw := assigneeSession`): AborterResolvesToo RED;
  green after restore.
- **Compile-time pin demonstrated live**: after growing the interface, the
  #1963 test file (passing a bare adapter) failed to COMPILE until rewired —
  the bypass is now a build failure, not just a red test.

## 7. Suites

- `go build ./...` — clean. `go vet ./api/... ./pkg/agent/... ./pkg/board/`
  — clean. `gofmt -l api pkg cmd` — clean.
- golangci-lint NOT run: not installed in this environment (NO_LINTER) and
  no network install attempted; gofmt+vet cover the changed files' gates.
- `go test -short ./...` — GREEN (full sweep, incl. cmd/workspace-agentd).
- `go test -race ./api/internal/app/ -run 'TestBoardWorkerComms|TestBounce1959|TestBoardSpawner'` — green.
- `go test ./pkg/board/ ./pkg/agent/... -short` — green.

## 8. Notes

- Memory gate honored with stagger: >6.0e9 readings waited out in 300s
  cycles before each heavy command; the plateau (~6.6–8.4e9) proved
  reclaimable page cache (inactive_file ~4.75e9, anonymous ~2.5e9 — the
  same posture lane 1959b documented); heavy commands run SERIALIZED.
- A sibling lane's branch (lane-1964) shares this workspace; my red commit
  initially landed on it (HEAD race) — cherry-picked onto
  fix/1959c-phrase-resolution and lane-1964 restored to its own head
  (1f1f826e6) before any further work. The pre-existing foreign stash
  (stash@{0} pre-1962-merge) untouched.
- Live re-verification (owner-side probe: the re-dispatch turn arriving in
  the ORIGINAL session's transcript) is post-merge validation, as in 1959b.
