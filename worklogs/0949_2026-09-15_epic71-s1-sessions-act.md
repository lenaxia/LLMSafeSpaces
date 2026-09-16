# Worklog 0945 — epic-71 / s1-sessions: the sessions-cluster write path through agentd Act (#1372)

**Date:** 2026-09-15
**Session:** Stream s1-sessions of epic #1314 — #1372: migrate the five sessions-cluster writes onto the agentd `Act` path in the authority regime (S1 completion beyond the input surface).
**Status:** in progress

---

## Objective

The five sites in `api/internal/handlers/proxy_handlers.go` make direct adapter-mediated mutating harness calls with no terminus gate: `CreateSession` :37, `Send` :136 (plus `syncSend` :898 — the same write), `Abort` :654, `DeleteSession` :681, `RenameSession` :734. Each gets the 4a two-regime pattern: reads stay adapter-mediated; in the authority regime (`h.agentdTerminus`) writes go through agentd `Act`; flag-off keeps the typed adapter methods unchanged. REST routes and response shapes stay IDENTICAL — only the write path moves.

## Rule-7 assumptions (stated, then validated)

| # | Assumption | Validation | Result |
|---|---|---|---|
| A1 | The Act vocabulary covers session create/send/abort/delete/rename | READ: `pkg/abi/llmsafespaces/abi/v1/action.proto` + `cmd/workspace-agentd/sessionstate/actions.go` (`actVerb`) | **FALSE for four verbs** — the union has interrupt/switch_model/switch_agent/answer_question/compact only. Abort == existing `InterruptAction` (the opencodeActor's interrupt posts V1 `/session/:id/abort`, the exact route `adapter.Abort` uses). Create/send/delete/rename are MISSING → additive proto extension (sanctioned by #1372's own assumption note: "extend the proto additively where a verb is missing"). |
| A2 | The outbox/delivery path's adapter-mediated sends (`proxy_lifecycle.go` ~:288/:314) interact correctly with Act-routed sends | READ `proxy_lifecycle.go:246-249`: `outboxDeliver` routes to `agentdTerminusDeliver` (the ABI `Deliver` ledger path) whenever `h.agentdTerminus` — the `adapter.SendAsync` (:288) and `adapter.Send` (:314) sites are flag-off-only, so in the authority regime the outbox never makes an adapter-mediated write. Interaction with the sync route's Act sends: both land on agentd, which serializes them pod-side via the per-session single-flight lock (`sessionstate/actions.go:77-79`) — strictly stronger ordering than the adapter path (which could interleave a sync send with a delivery directly at the harness). | **validated** — Send can migrate without touching the outbox (#1316's tree stays untouched) |
| A3 | Adding oneof members + enum values to the frozen schema passes `buf breaking` (FILE rules, armed via `abi/FROZEN` @ e58cecd) | Execution after the change (below) | validated — see Tests run |
| A4 | Response shapes can stay byte-identical across regimes | (a) CreateSession: the adapter stamps `workspaceID` via `ParseSessionWire`; the Act path's Actor cannot know the workspace id, so the handler stamps it post-conversion. (b) SendMessage: the adapter's FileChange enrichment (`a.differ`) is dead in production — `WithFileDiffProducer` has ZERO non-test callers (grep across repo), so the response is `translateMessage(om)` only, which the Actor can mirror via the same seam-local translation. (c) Error paths: every Act failure on the five sites maps to the route's EXISTING error body (502 family) — NOT `mapConnectError`, because these routes' wire shapes are pre-existing (unlike 4a's new input rows); the openapi rows pin 200/400/401/403/404 and the code's 502 bodies are the deployed contract. | **validated** (parity pinned by test) |
| A5 | `h.agentdTerminus` is off by default on main | `api/internal/app/app.go:225` — env `AGENTD_STATE_AUTHORITY`; authority-regime rows use the `SetAgentdTerminus(true)` seam (as `proxy_input_act_test.go` does) | **validated** |
| A6 | The session rate limiter + single-flight tolerate `session_id=""` (create has no session yet) | READ `sessionstate/service.go` limiter (bucket per key, "" is a key) + `authority.go:287 sessionLock` (map mutex) — creates serialize pod-wide on the "" lock; session creates are rare; documented | **validated** |
| A7 | abitest (the reference implementation) must declare + implement the new verbs for wire-pin tests | It declares 4 of 5 action types today (`Server.Capabilities`); extended additively | **validated** |

## Design (as decided pre-implementation)

- **D1 — Abort rides `InterruptAction`.** No new verb; the actor's interrupt IS the V1 abort. `AbortSession` returns 204 as today.
- **D2 — Four additive verbs**: `CreateSessionAction{title}` / `SendAction{text, model}` / `DeleteSessionAction{}` / `RenameSessionAction{title}` + results (`CreateSessionResult{session}`, `SendResult{message}`, `DeleteSessionResult{}`, `RenameSessionResult{}`); oneof members 7-10 on `ActionRequest` + `ActionResult`; `ActionType` values 6-9.
- **D3 — capability declaration is unconditional** for the four: their harness routes (POST /session, POST /session/:id/message, DELETE /session/:id, PATCH /session/:id) are the production adapter path's own V1 routes — the same confidence class as interrupt/switch_model/answer_question ("regression-pinned" per US-69.9); the boot-probe discipline is for unpinned V2 routes (switchAgent/compact).
- **D4 — error mapping: route-parity, not connect-parity.** All Act failures on the five sites surface the route's existing 502 bodies (incl. the SendMessage enrichment + syncSend 507 disk-full classification, which stays CRD-derived and path-independent). Deliberate divergence from the input surface's `mapConnectError` — recorded here; those rows were new spec rows in 4a, these are pinned.
- **D5 — parity converters both sides of the wire.** Agentd wiring: harness wire → `ParseSessionWire`/`ParseMessageWire` (seam-local, exported from `pkg/agent/opencode`) → contract → `abiv1` (new wiring converters beside `questionToABI`). API: protojson-decode `ActionResult` → `abiv1` → contract (new handlers converters beside `inputRequestFromABI`). A parity PIN asserts both regimes emit deep-equal REST bodies for the same harness fixture.
- **D6 — the per-prompt model split is shared**: `pkg/agent/opencode` exports the message-route object-wire builder; `Adapter.Send` and the actor's send consume the same function (single source of truth).
- **D7 — endpoint-unresolved is a plain error on these routes** (the adapter's resolution failures were 502s here too; no 409 — same rationale as D4).

## Out-of-scope findings (surfaced, not fixed here)

- `POST /workspaces/{id}/sessions/new` (router.go:1422) → `wsSvc.EnsureSession` → `createSessionOnWorkspace` (workspace_service.go:1604) makes a RAW opencode `POST /session` call — an S1 violation in the services layer, outside this stream's allowlist (api handlers + pkg/abi + agentd actor). Reported to the coordinator; needs its own issue.
- `sdks/openapi.yaml` rows for message/abort still carry `x-opencode-proxy` — 4b-c2's tree (#1304 owns the marker sweep).

## Work log (TDD — every surface red-first)

1. **ABI (RED → GREEN):** `pkg/abi/session_actions_roundtrip_test.go` written first — build failed on the missing verbs (red proof). Additive proto: `CreateSessionAction{title}` / `SendAction{text, model}` / `DeleteSessionAction` / `RenameSessionAction` + results (oneof members 7-10 on the request, 8-11 on the result — 7 collides with `effect_seq` on ActionResult, caught during design); `ActionType` values 6-9. `buf generate` regen (Go + TS). `surface_completeness_test`'s reviewed-surface list + ActionType pin extended (its red on the new messages was the review-surface gate working). abitest reference implements + declares the four (compact stays the NotSupported fixture); abitest's Act allows empty session_id ONLY for create.
2. **agentd authority:** union-dispatch/limiter/validation rows red-first (`actions_test.go`): actVerb's four new cases; `validateAction` (send requires session_id+text, delete/rename require session_id, rename requires title, create exempt); the rate limiter keys create on the pod-wide `"action.create_session"` bucket (the limiter's empty-key rejection was found by the red run — A6 corrected).
3. **agentd actor:** wire-shape rows red-first (`sessionstate_actor_test.go`) against the captured 1.18.10 fixtures (the schema_helper relative-path convention): create POSTs /session (title optional, `{}` body parity), send POSTs the V1 message route with the model OBJECT wire, delete DELETEs, rename PATCHes; the result translations round-trip via the SAME exported seams the adapter uses; capability declaration unconditional (D3). Transport generalized `post`→`do(method,…)` returning the body (64MB read cap mirroring the adapter's).
4. **shared seams (RED → GREEN):** `opencode.ParseMessageWire` (single-message parse — the adapter's Send refactored onto it, behavior-preserving) + `opencode.MessageModelOverrideWire` (the per-prompt model object wire; Adapter.Send refactored onto it). Pinned against the captured fixtures + the modelOverride split table.
5. **API handlers:** `proxy_sessions_act_test.go` rows red-first (the create row panicked on the ungated adapter call — red proof). `proxy_sessions_act.go`: actSessionAction (protojson decode, DiscardUnknown for mixed generations) + the five act wrappers + ABI→contract converters. The five sites gated in `proxy_handlers.go` (+syncSend). `abiAct` refactored into `abiActRaw` shared with the new `abiActProto`. Log strings regime-accurate (`…Act failed` vs `…adapter failed`) — the #817 pins stay green on the flag-off strings.

## Tests run (all green)

- `go test -timeout 900s -race ./api/internal/handlers/` — ok (17 new rows + full suite)
- `go test -timeout 600s -race ./pkg/abi/` + `./pkg/abi/...` — ok (new round-trip rows both codecs)
- `go test -timeout 600s -race ./cmd/workspace-agentd/ ./cmd/workspace-agentd/sessionstate/` — ok
- `go test -timeout 300s -race ./pkg/agent/...` `./pkg/session/` `./pkg/mcp/` — ok
- `buf breaking pkg/abi --against <worktree of abi/FROZEN baseline e58cecd>` — exit 0 (A3: additive oneof members + enum values pass the armed FILE gate)
- `golangci-lint run` (handlers + abi + agentd + opencode) — 0 issues; `make repolint` — all checks passed
- `make abi-check` freshness fails only against HEAD pre-commit (it diffs committed state); the same commands post-commit are clean

## Review-trap notes (from 0936/0941's history, applied)

- Bodyless-body pins: Abort/Delete assert empty bodies; SendMessage 502 bodies are byte-pinned (`{"error":"failed to send message"}` etc.).
- Regime-coverage pins: every route has terminus + flag-off + transport-error rows; the parity row asserts both regimes emit deep-equal REST JSON for the same fixture bytes (it caught two stub-builder gaps during development — the pin class works).
- No narrative claims without execution: every "validated" above names its proof.


## CI round 1 remediation — the MCP router fixture gap

The first CI run failed 4 `TestMCPClientSessionMessage_*` rows in `api/internal/server` (red on this branch, green on clean main — reproduced locally). Root cause: `newMCPRouterFixture` arms the agentd terminus for the contract-stream route, and the fixture's outbox is unset — so its `/prompt` flows hit `syncSend`, which now writes through Act against a stub pod that served only the adapter's V1 surface (locally the Act POST hit an unrelated listener: `act: status 401`; on CI it would be connection-refused). My handler-level rows could not catch this — it is ROUTER-level integration (real router + real MCP client + real proxy handler).

Fix (the fixture models the pod, and the pod now serves Act in the authority regime):
- `SetAgentdPortForTest` seam (the established *ForTest family) — the fixture points the ABI surface at its stub pod.
- The stub serves the Act op: the send arm answers with the same completed assistant message in ActionResult protojson (recorded into `promptGot`); the other verbs echo empty results.
- The promptGot assertion updated to the Act payload (`body["send"]["text"]` == the user's text) — the invariant (the text reaches the pod) is unchanged; the wire it rides is the new truth.

The passkey/uploads panics in the same CI run passed locally on both branches under `-short -cover` — runner flakes, not this change; re-verified by the rerun below.

## Tests run (CI round 1 remediation)

- `go test -timeout 600s ./api/internal/server/` — ok (4 rows red pre-fix → green post-fix; full package green)
- `go test -timeout 600s -short -cover ./api/internal/server/` — ok

## Review r1 remediation

**f1 (HIGH — the Act send rode the outbox's 3m30s-capped client):** `abiActRaw` used `agentdHTTPClient` (`Timeout: agentdDeliverInlineWindow`). A sync send is a full LLM turn; the adapter path deliberately has NO hard client timeout (pinned by `TestHTTPClient_NoHardTimeout` — the reviewer traced the intent). Fix: `sessionActHTTPClient = &http.Client{}` (request-context-bounded, the adapter's boundary semantics) + `sessionActBodyCap = 64 MiB` (f2 — the adapter's own Send bound; the input verbs keep their 1 MiB shared transport). Pinned red-first: `TestSessionActHTTPClient_NoHardTimeout` (the adapter pin's mirror) and `TestSessionsAct_SendMessage_LargeResult` (>1 MiB text part through the Act round trip — red: 502 on truncation).

**f3 (HIGH — #944's disk-pressure notice orphaned a third time):** the injector lives in `systemnotices.Wrap` around the ADAPTER; authority-regime sends bypass it (and the terminus OUTBOX path already did — pre-existing, same class). Fix at the funnels every authority-regime message write shares: the actor's send and the admitter's Admit prepend `systemnotices.Notice` (same tier/ratio/notice text — one source) from a pod-local `statfs` of the workspace volume (fresher than the API's CRD-status reader; `LLMSAFESPACES_WORKSPACE_DIR` override). Fail-open by construction; regimes disjoint → no double injection. `TestMain` pins the package's default disk posture so exact-body tests stay runner-independent; the notice rows stub the tiers.

**Minor (sessionId merge order):** the action map now merges BEFORE the `sessionId` injection — the caller's id is authoritative. Pinned red-first (`TestSessionsAct_SessionIdIsAuthoritative`, stray key overridden).

**f4 (overstated claim):** the PR description's headline now scopes S1 to the sessions cluster and names the `createSessionOnWorkspace` remainder; the first commit's "(S1 completion)" subject is corrected in the record here (history left intact for the reviewer's SHA stamping — squashing happens post-APPROVE per protocol).

**E2E:** the US-70 delivery pool dispatched on the branch (run 35031471918) — the pool arms `AGENTD_STATE_AUTHORITY`, so the delivery/walk-away legs exercise the Act-routed write path end-to-end.

## Tests run (r1)

- `go test -race` on handlers (900s), server, agentd + sessionstate, pkg/agent/..., pkg/abi — ok
- golangci-lint 0 issues; make repolint passed

## Mid-r2: main advanced under the branch — #1307 wedge interplay

Main's #1376/#1379 train landed the #1307 text-only-wedge classification (`agent.ErrImageInTextOnlyHistory` → 422) into the exact SendMessage/syncSend error paths this PR rewrote. The rebase auto-merged the classification in, with two defects found by execution:

1. **The merge wired SendMessage's check to a stale variable** (`err` — the body-parse error — instead of `sendErr`): the wedge classification could never fire on that route. Fixed via the shared helper.
2. **The authority regime had no wedge path at all** (Act failures are connect errors; the sentinel never rides): the marker is now defined ONCE on pkg/agent (`agent.TextOnlyWedgeMarker` + `agent.IsImageInTextOnlyHistoryMessage`), the opencode seam's `textOnlyWedgeMarker` aliases it (single source), and `isTextOnlyWedgeSendErr` classifies both regimes (sentinel wrap OR connect-message marker). Pinned `TestSessionsAct_SendMessage_TextOnlyWedge422` — RED proof captured with the connect probe removed (expected 422, got 502), green restored; main's own flag-off wedge rows stay green.

## Tests run (mid-r2)

- `-race` full: handlers, server, pkg/agent/..., agentd + sessionstate, pkg/abi — ok
- golangci-lint 0 issues; repolint passed

> History note: the mid-r2 push was `--force-with-lease` after rebasing
> onto origin/main (README Rule 10 scenario 3: this branch is mine alone,
> never pulled by others; the rebase replayed my three commits unchanged
> plus the new fix). No shared history was rewritten.

## Review r2 remediation

**f2 (HIGH — abort queued behind the in-flight turn):** the r2 review traced that `act` held the session single-flight across the actor's full execution while send performs a blocking full-turn POST — a mid-turn interrupt queued for minutes and aborted an already-finished turn. Fix: interrupt is EXEMPT from the session lock (`actions.go`, documented in place: it mutates no projected records — I7 — so the sole-writer rationale does not apply, and its purpose is to preempt the lock HOLDER's turn; the harness serializes the abort against its own turn state). Pinned red-first: `TestActOp_InterruptPreemptsInFlightSend` (RED proof: "interrupt queued behind the in-flight send", 2.1s fail pre-fix; green post-fix; the parked goroutine is cleanup-safe). The golden serialization row now uses compact (interrupt's queueing was never the property under test there — the matrix row is about MUTATING actions); `TestActOp_InterruptAdmissionRace` (the I7 preservation property) is unchanged and green.

**f3 (wedge over-classification):** the Act-path probe now requires `cce.code == "invalid_argument"` (the connect mapping of the harness 400 main gates on) — strict parity with flag-off for identical harness bytes. Pinned red-first: `TestSessionsAct_SendMessage_WedgeRequiresInvalidArgumentCode` (an internal-coded error carrying the marker stays 502; RED pre-fix).

**E2E gate (the required level):**
- `local/epic71-s1-sessions-act-e2e.sh` — cluster rows on the pool (which arms AGENTD_STATE_AUTHORITY, so the platform routes ARE the Act path): S1a happy sync send with the marker round trip; S1b abort PREEMPTS a slow mock turn (budget 10s < turn 45s — the cluster-level r2-f2 pin) and the session settles idle; S1c rename verified AGENT-side (non-circular truth); S1d delete verified by agent-side 404; S1e send/abort/delete on a dead session pin the byte-exact 502 bodies. Distinct WS_BASE (e2e71100-…), distinct mock (mock-llm-s1) so the AC-1d mock is untouched.
- Wired into `us-70-delivery-pool.yml` as a seam-inert leg AFTER the 3a walkaway rows and BEFORE the fault seam arms (the established ordering discipline); dispatched on the final head for the merge-gate evidence.
- `local/epic71_s1_sessions_script_test.go` pins the structure (bash syntax, lib sourcing, distinct base, the preempt-budget discriminator, agent-side truth asserts, the pinned bodies, the pool wiring + ordering).

**Reviewer's noted non-blocking items:** the create-session HANDLER has no cluster row (the route is not registered in the router — sessions/new is the unmigrated service path, disclosed; the Act create arm is pinned by agentd rows + handler rows); the E4 attachments Playwright flake is the documented known-flaky (0893) — the `--failed` rerun cleared it on this branch.

## Tests run (r2)

- `go test -race`: sessionstate (200s), handlers (900s) — ok; `./local/` — ok (new pin rows)
- golangci-lint 0 issues; make repolint passed; bash -n clean

### Pool dispatch history (the e2e gate's own record)

- Run 35031471918 (pre-rebase head c85f14e3): **success** — the delivery/walk-away legs green on the pre-remediation code (the s1 leg did not exist yet).
- Run 35042070419 (final head, dispatched concurrently with the prev-head run): **failure — cluster collision.** Both dispatches shared the fixed `llmsafespaces-ci` kind cluster/runner set; the newer run's legs died on apiserver/etcd health (`kc apply failed after 5 attempts` across every leg from 01:24). Re-dispatched serially.
- Run 35045727054 (serial, final head): the s1 leg ran for the first time. **S1a ✓ (Act send round trip), S1b abort-preempt ✓ (204 in 0s — the r2-f2 cluster pin held), S1c ✓ (agent-side rename), then two script defects:** the idle-settle assert was budgeted (30s) below the slow turn's own duration (45s — the abort stops the TURN, but the in-flight provider HTTP call runs to completion before the harness reaps it; the settle is turn-bounded, now S1B_SLOW_TURN_S + 60), and `http_code_of`'s third positional was unbound under `set -u` on the two-arg DELETE call (killed the script at S1d). Both fixed; re-dispatched.
- Run 35053181706 (fixed head 414141ee): the s1 leg died at script START — the reordered idle-budget default referenced `S1B_SLOW_TURN_S` before its declaration (unbound under `set -u`; `bash -n` cannot see that class). Fixed the declaration order and added `TestEpic71S1Script_BudgetDefaultsEvaluate` (evaluates the extracted default block under `set -euo pipefail`) so the class is pinned. Re-dispatched serially.
- (Correction of the r2-commit record: the budget pin initially committed red — the end anchor searched for WS_BASE, which PRECEDES the budget block in the script; a piped test masked the failure through the shell chain. Anchor fixed to the block's actual terminator; the test is green and the process lesson — never pipe a gate through tail in a && chain — recorded here because it happened.)
