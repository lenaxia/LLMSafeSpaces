# Worklog: US-69.9 — typed actions op (union, capability negotiation, sole-writer serialization)

**Date:** 2026-08-31
**Session:** Epic 69 (#1134) US-69.9 (#1143, design 0055 M1 op 5): the frozen-union actions op (`Act`) goes live end-to-end — agentd executes the five verbs behind the authority's per-session single-flight (sole writer), capability declaration is measured at boot (probe, not assumption), the API exposes `POST /sessions/:sessionId/actions` behind `AGENTD_STATE_AUTHORITY`, and the US-69.6 matrix's no-exceptions decision + I7 are enforced by construction and golden tests.
**Status:** Complete (in-repo). The e2e-vs-pinned-pool AC (`union_member_e2e` against real opencode) rides the staged pool per the epic's staging (same disposition class as US-69.8's cluster ACs, cross-noted on #1147).

---

## Objective

Control ops stop being API→opencode side-channels: one typed union (`interrupt`/`switch_model`/`switch_agent`/`answer_question`/`compact`), agentd the sole writer of session mutations, serialized against delivery — harness differences are capability data, never API branches.

## Work Completed

### agentd — the Act op (`sessionstate`)
- **`actions.go`**: the `Actor` dialect seam (Admitter pattern; nil disables the op with typed `abi.actions` NotSupported) + the op core: union-member extraction → capability check against the boot report (undeclared verb → typed `action.<verb>` NotSupported BEFORE any harness call) → per-verb validation (InvalidArgument) → execution under the session's single-flight lock. `effect_seq` stays unset (the design-0055 open item — the effect event lands on the stream after the response).
- **Lock hoist**: the per-session single-flight map moved from `deliveryDriver` to `Authority.sessionLock` — admissions and actions share ONE domain (M1 sole writer). The driver's lock is injected; nil falls back to a private map (tests).
- **`driveAdmission` restructured to per-attempt locking**: the retry backoff now sleeps OUTSIDE the lock. Before, a failing admission held the lock across all 5 attempts (~52s worst) — an interrupt would queue behind a dying admission. Now an action waits at most ONE attempt (~10s). Invariant preserved (at most one admission in flight; state re-checked under the lock each attempt).
- **I7 by construction**: the Act path never touches ledger/entry state — no `superseded-by-interrupt` exists.

### agentd — the opencode actor (wiring layer)
- `opencodeActor`: the five verbs against localhost opencode — V1 `/session/:sid/abort` (the only interrupt route on pinned ≥1.18.10), V2 `/api/session/:sid/model` (`{"model":{...}}`), V2 `switchAgent` (boot-learned key), V1 question/permission reply (question `{"answers":[[...]]}` — the frontend's own input contract, worklog 0069's live capture; 404 → permission `{"reply":...}` — the adapter Resolve fallback pattern), V2 `/compact`.
- **Measured capability declaration**: the regression-pinned trio (interrupt/model/answer) declared unconditionally; `switchAgent` + `compact` boot-probed — typed 400 JSON = route present (and the 400's missing-key pointer teaches the switchAgent body key, the same trick that revealed the model id/modelID split); catch-all 204 = absent, stays undeclared. The 1.18.10 V2-interrupt removal is the precedent for never trusting route presence across pins.
- Harness 4xx → typed connect codes (400→InvalidArgument, 404→NotFound, 401→Unauthenticated), not generic 500s.

### API — the route
- `POST /api/v1/workspaces/:id/sessions/:sessionId/actions` (`proxy_actions.go`): flag off → typed 501 (`abi.actions` requires the authority flag, D4); on → union passthrough with the **path's sessionId authoritative** (a body sessionId never overrides), Connect-JSON to the pod's Act, connect codes mapped to HTTP (unimplemented→501, exhausted→429, etc.). `agentdEndpoint` factored out of the terminus's resolve (shared, both resume-safe); `agentdPortOverride` field for test stubs (the dev_preview pattern).

### Tests (the issue's plan, in-repo form)
- `sessionstate/actions_test.go`: union dispatch (5 verbs, wire-level), typed NotSupported (undeclared verb with detail capability; nil actor; unknown oneof), validation-before-lock, **golden serialization both directions** (action waits for in-flight admission; admission waits for in-flight action; no lost interrupt), **I7 race** (admission completing during a pending interrupt lands a PRESERVED admitted row).
- `sessionstate_actor_test.go`: wire shapes per verb (paths + exact bodies), probe discrimination (present vs catch-all 204), permission fallback, declaration sets, typed status mapping.
- `proxy_actions_test.go`: flag-off 501, path-authoritative sessionId + union passthrough, NotSupported→501 crossing the edge, non-object body 400.

## Key Decisions

1. **Generation of truth for capabilities = measurement**: the boot probe decides switchAgent/compact; nothing is declared on faith. Harness differences stay data.
2. **Per-attempt admission locking** (found by a deadlocking first draft of the I7 test): serialization must not let a retry chain starve interrupts. Sole-writer holds per attempt, not per row-lifetime.
3. **API edge stays dependency-light** (Connect-JSON by hand, the terminus discipline) — zero generated-code coupling in the API binary path.

## Tests Run

- `go test ./cmd/workspace-agentd/...` — green (186s + 19s, all prior suites)
- `go test -race -count=2 -run "Serializes|InterruptAdmission|SingleFlight|ExactlyOnce" ./cmd/workspace-agentd/sessionstate/` — green
- `go test ./api/internal/handlers/ ./api/internal/app/` — green (111s)
- `golangci-lint --new-from-merge-base ./cmd/workspace-agentd/... ./api/...` — 0 issues
- Schema untouched — `Act` was already declared on the frozen surface; the freeze gate is unaffected.

## Files Modified

- `cmd/workspace-agentd/sessionstate/{actions.go(new),actions_test.go(new),authority.go,ledger.go,service.go}`
- `cmd/workspace-agentd/{sessionstate_wiring.go,sessionstate_actor_test.go(new)}`
- `api/internal/handlers/{proxy_actions.go(new),proxy_actions_test.go(new),proxy.go,proxy_lifecycle.go}`
- `api/internal/server/router.go` (route)
