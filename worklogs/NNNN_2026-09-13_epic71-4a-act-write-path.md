# Worklog NNNN — epic-71 / 4a increment 1: input writes through Act (+ contract deltas D1/D2)

**Date:** 2026-09-13
**Session:** Stream 4a of epic #1314 — implement #1302's write-path half: replies/rejects go through agentd `Act` (S1), the SessionAction disposition hook lands (3a's deferral), and the permission-reply wire contract is pinned fixture-first.

## Gates executed before implementation

- **Batch-3 prerequisite:** #1357 merged 2026-09-13 (the issue's churn warning honored — no shared-file work before it landed).
- **Fixture-first (the issue's own Rule-7 rule):** the opencode permission-reply wire contract captured empirically on the pinned 1.18.15 local serve — `once` is per-call (an immediately-following command matching the same pattern raised a NEW ask); `always` is pattern-scoped persistent (a third command ran straight to completed, ZERO asks in a 60s window); reject from G1. Recorded as `permission_reply_wire_contract` in `ask_terminal_states_1_18_15.json`.
- **Consumer grep:** `sdks/canary` parses no response shapes (status-only asserts; one `202` window on permission-reply to widen in the one-shape PR); no in-tree vscode SDK.
- **Design:** `design/stories/epic-71-input-delivery-robustness/US-71.4a-act-migration.md` with the post-batch-3 remaining-work map and decisions D1–D3.

## Work completed (TDD — every suite red-first)

1. **Contract (D2):** `AnswerInputAction.message = 5` (deny feedback; only valid WITH `reply` — validated agentd-side with new rows). Full regen: `make abi-generate` (Go + TS + sessiongen).
2. **Agentd routing (D1):** `reply="reject"` on a question id routes question-reject-FIRST (the dismiss exit — the mirror of the answer path's question-first probe), permission-reply fallback on 404; `message` rides the permission reply body verbatim. 3 new actor tests + 2 validation rows.
3. **API write routes through Act (S1):** `QuestionReply`/`QuestionReject`/`PermissionReply` forward `AnswerInputAction` via the existing `abiAct` seam in the authority regime (`agentdTerminus`); the ask's session resolves live-pending-first, inbox-record-second (a dead ask's Act still lands — agentd's resolve-by-absence, 1a). The batch-3 adapter path survives verbatim under flag-off (D4 single regime per flag). Connect errors map through `mapConnectError` — never a silent no-op. 7 red-first handler tests (happy paths asserting the exact action payloads, dead-ask-404, connect-error mapping, flag-off regression pin).
4. **SessionAction disposition hook (3a's deferral landed):** a successful `answerQuestion` through the generic actions route terminalizes the inbox record (answered; `reply=reject` → dismissed) — the MCP/SDK reply path now clears whileAway re-presentations like the REST one. Pinned.
5. **Pre-existing in-platform test failure fixed (Rule 5):** `TestCallMCPTool_DevPreviewURL_*` failed on any in-platform `go test` run — the sandbox's real agentd env (`LLMSAFESPACE_API_PUBLIC_URL`) outranks the tests' `LLMSAFESPACE_API_URL` pin (branch 1 of `mcpPublicAPIOrigin`) and shadowed it. Green in CI, red in-platform; the tests now pin the public-origin knob empty (hermetic everywhere). Root-caused via a pristine-main worktree — NOT introduced by this stream.

## Key decisions (in the design doc)

- **D1:** question-reject rides the EXISTING `reply` vocabulary ("reject" was already in the proto comment) — no contract change for the dismiss exit.
- **D2:** `message` is one additive field; disjointness rules unchanged (message requires reply).
- Regime split: Act in the authority regime, adapter under flag-off — the flag stays the single-regime switch (D4).

## Tests run

- `go test ./cmd/workspace-agentd/ ./cmd/workspace-agentd/sessionstate/` — ok (incl. the DevPreview hermetic fix; full package 241s)
- `go test -race -count=1 ./api/internal/handlers/` — ok (7 new Act rows)
- `golangci-lint run` (handlers + agentd) — 0 issues; `make abi-generate` idempotent

## Remaining for #1302 (increment 2)

- The one-shape `InputRequest` cutover (REST lists + SSE emitter + envelope `whileAway` (D3) + frontend retypes + Playwright stubs)
- Prefix validation behind the dialect seam (generic request-ID contract)
- OpenAPI `InputRequest` schema + retype + drop `x-opencode-proxy`; canary `2xx` widen
- e2e: the S6 stale-click leg + L2 ≤2s clear

## Blockers

- None.
