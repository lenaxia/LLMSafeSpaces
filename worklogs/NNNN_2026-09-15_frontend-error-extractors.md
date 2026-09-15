# Worklog NNNN — 2026-09-15 — epic-71 / 4b-c1: delete legacy opencode nested-error-envelope extractors (#1303)

Stream: epic-71 / 4b-c1 · Issue #1303 · Branch `fix/epic71-4b-frontend-error-extractors` (from origin/main @ 2e839cd6).
Constraints honored: frontend/ + its tests + docs only. No sdks/openapi.yaml (#1304's), no api/ Go, no pkg/ (the repolint lexicon gate is #1305's lane).

## Evidence re-verification (the code moved since the 2026-09-08 leak review)

All four evidence pointers re-checked against current main before any edit:

| Issue pointer | Verified at | Drift since 2026-09-08 |
|---|---|---|
| `agentErrorRef.ts` shape-2 docs + nested fallback | lines 9-11 (docs), 46-73 (`extractOpencodeField`), nested read at 64-70 | none — intact as described |
| `ChatHistoryErrorBanner.test.tsx` raw-envelope fixtures | lines 10-19 header, 25-28 `OpencodeErrorEnvelope`, 46-76 the "#486 EXACT shape" row | none — intact |
| `types.ts:94` `AgentSession` "Shape returned by the opencode agent GET /session/:id (proxied through)" | line 94 | none — intact; also found dead `parentID`/`share` fields (opencode casing / opencode share; contract has `parentId`, no `share`) and stale `"retry"` status value (contract Status has none) |
| `useSessionTitle.ts` opencode/proxy comments | lines 7, 12, 29, 35 | none — intact |

Blockers: #828 merged — `fetchUpstreamHistory` and the raw ≥400 `c.Data` passthrough are gone from `proxy_handlers.go` (grep-verified, zero hits). Question/permission REST migration merged via #1302 (#1363 + #1371, landed 2026-09-14 — these also touched the four frontend files, InputRequest contract work only; the extractor leak survived them, which is why #1303 exists).

Live error-body shapes on the message routes (read from current `proxy_handlers.go` + `proxy_chat_enrichment.go`):
- GET history failure → API-authored 502 `{"error":"failed to fetch history"}` (GetHistory).
- POST prompt/async failures → fixed `{"error":"failed to send message"}`, optionally enriched by `EnrichChatErrorBody` (allowlist `_tag, message, kind, field, resource, service, status, operation, ref, providerID, modelID, suggestions, sessionID` + refresh hints). Both call sites feed the fixed body today, but the allowlist promotion path is the surviving mechanism the issue keeps extracted (top-level `ref`/`message`).
- No nested `{name, data:{...}}` envelope can reach the client anywhere.

## Assumptions (Rule 7) + validation

1. **Only `ChatHistoryErrorBanner.tsx` consumes the extractors.** Validated: repo-wide grep — the only non-test import.
2. **Nothing else parses nested error envelopes** (`body.data`/`body?.data`). Validated: grep across frontend/src — zero hits (the SSE event-envelope `.data` in ChatPage.tsx:863 is a different, live seam).
3. **`parentID`/`share` on `AgentSession` are dead.** Validated: grep — declared, never read; contract `session.Session` (contract_gen.go) uses `parentId`, has no `share`.
4. **Making `AgentSession.status` required + contract-valued breaks no consumer.** Validated: only reads are `session?.status === "busy"` (useChatStream) and title reads; `tsc --noEmit` clean.
5. **Issue's "ChatPage.historyError.test.tsx … unchanged and green" is impossible verbatim**: its row-1 fixture IS the dead envelope. Validated by execution: post-deletion the row fails (`Unable to find /Unexpected server error/`). Resolution: row-1 fixture rewritten to the API-authored 502 body preserving the #490/#491 intent (message renders, no "undefined"); rows 2-4 byte-identical; `ChatPage.autorename.test.tsx` untouched and green. Disclosed here and in the PR.
6. **The vitest rows themselves pin the dead shape** (nested → undefined), and the triage's requested grep-gate adds a source-scan pin (mutation-verified below).

## TDD record (red-first)

1. RED: `agentErrorRef.test.ts` rewritten — nested rows now expect `undefined` + new `extractAgentErrorMessage` block (0 rows before). Run: **3 failed** (nested ref, nested message, `data.message` in empty/non-string row) — exactly the deleted behavior.
2. GREEN: `agentErrorRef.ts` — nested `data.*` fallback deleted; helper renamed `extractOpencodeField` → `extractErrorField`; docs rewritten to the two live shapes (top-level allowlisted, API's own `{error}` with caller fallback). Run: 11/11.
3. `ChatHistory.historyError` row-1 failed against the dead fixture (see A5), then green on the API-authored body.

## Changes

1. `frontend/src/api/agentErrorRef.ts` — nested fallback deleted; docs rewritten (no "raw opencode envelope", no stale `proxy_handlers.go:155` reference). Exported names kept (`extractAgentErrorRef`/`extractAgentErrorMessage` — the #488 regression intent).
2. `frontend/src/api/agentErrorRef.test.ts` — nested → `undefined` rows; full `extractAgentErrorMessage` coverage (top-level, nested-undefined, `{error}`-undefined, non-objects never throw).
3. `frontend/src/components/chat/ChatHistoryErrorBanner.test.tsx` — fixtures to API-authored shapes only: GET-history 502 `{error}` row carries the #486 regression intent; NEW graceful-fallback row pins that a stray legacy envelope extracts nothing and falls through to the placeholder; flat-allowlisted, 503 recovery, placeholder, and #491 empty-message rows kept; `OpencodeErrorEnvelope` type deleted; stale header rewritten.
4. `frontend/src/components/chat/ChatHistoryErrorBanner.tsx` — doc comment only (extraction hierarchy now: top-level allowlisted → `body.error` → `err.message` → placeholder). Component logic unchanged.
5. `frontend/src/pages/ChatPage.historyError.test.tsx` — row-1 fixture to the API-authored 502 body (see A5); rows 2-4 unchanged.
6. `frontend/src/api/types.ts` — `AgentSession` documents the contract `Session` (pkg/session contract_gen.go): `workspaceId`/`parentId` (contract casing), dead `parentID`/`share` deleted, `status` required with contract values (`unknown|idle|busy|error|compacting|archived`; `"retry"` never existed in the contract). #796 cross-link: #796 is OPEN and not in flight (no PR references it), so `AgentSession` was fixed here per the issue's "fold … if in flight" conditional.
7. `frontend/src/hooks/useSessionTitle.ts` — comments de-opencoded ("session contract endpoint", "the agent generates a title", "may not exist on the agent yet"). Behavior unchanged.
8. NEW `frontend/src/components/chat/ChatHistoryErrorBanner.wiring.test.tsx` — integration leg: REAL client (`getRaw` → fetch stub → `ApiClientError`) with the EXACT API-authored 502/503 bodies → REAL banner; asserts URL construction, status/body propagation, message render, no invented ref.
9. NEW `frontend/src/components/chat/chatErrorSeamGate.test.ts` — grep-gate (2026-09-15 triage ask): guards `agentErrorRef.ts`, `types.ts`, `useSessionTitle.ts` against `record.data|body.data`, `extractOpencodeField`, `OpencodeErrorEnvelope`, `proxied through|passed through verbatim`; fails loudly if a guarded file moves (anti-vacuous row). **Mutation-verified**: appending `record.data` to agentErrorRef.ts → gate fails; reverted → 4/4 green.
10. NEW `frontend/tests/e2e/history-error-banner.spec.ts` — Playwright leg: mocked 502 `{error}` and 503 recovery bodies → banner renders API fields only, zero `Ref:` rows, no "undefined".

## Test gates (executed)

- `npx vitest run` (frontend/): baseline 1823 passed (166 files) → **1835 passed** (net +12: +5 extractor rows, +1 banner row, +2 wiring, +4 gate). Zero failures.
- `npx tsc --noEmit` (frontend/): **0 errors** (baseline 0).
- `npx playwright test`: **147 passed / 1 failed / 13 skipped**; the 1 failure (`attachments.spec.ts` E3 oversize) passes 7/7 in isolation — pre-existing contention flake in the 9-minute full run, untouched by this diff (no attachments-path change). New spec: 2/2.
- Go pin tests: grepped `pkg/repolint` + `local/` for source pins on the touched files — none exist (only `AgentSessionStatus`, an unrelated CRD Go type). No Go gate applies.

## Deferred (disclosed, not silent)

- **Kind-cluster e2e leg** (kill/suspend the agent pod mid-session → load history → assert the API-authored banner against the real API): NOT run from this agent's worktree container — `command -v` verified docker, kind, and kubectl are all absent there (re-confirmed post-r4; the review runner does carry the full cluster toolchain, so this is an agent-environment limit, not a repo limit). Closest runnable form on a provisioned cluster: `./local/test.sh` (kind bootstrap + smoke), then suspend the e2e workspace and open `/chat/<ws>/<session>` expecting the rows asserted in `history-error-banner.spec.ts`. The mocked Playwright leg + the wiring test cover the same seam assertion (API fields only, no nested fallback) short of a live pod.
- **Live-router integration against `dev_preview.go`**: the issue's phrasing targets the API dev-preview router; no live API is runnable here (stateless deps need DB/Redis/K8s). Adapted to the repo's established real-client wiring pattern (`workspaces.getSession.test.ts` lineage) with the exact API-authored bodies quoted from `proxy_handlers.go`. 
- **repolint lexicon lint** for the deleted shape: #1305's scope (its two rules cover the guardrail class); the frontend grep-gate above holds the seam until then.

## Adversarial self-review (Rule 11)

- f1 "Issue said ChatPage.historyError unchanged" — real, resolved by A5 disclosure + minimal rewrite (1 row, intent preserved).
- f2 "grep-gate could be vacuous" — real risk; disproved by mutation run (gate fails on injected `record.data`).
- f3 "AgentSession status required could break mocks" — false alarm: tsc 0 errors; runtime reads are optional-chained.
- f4 "wiring test isn't the dev_preview router" — real limitation; disclosed above, not worked around silently.
- f5 "`SessionStatusEvent.status` still has `"retry"`" — inspected: backend-emitted SSE tracker event (types.ts:260), one of the four issue locations is not it; out of scope, noted for #796's sweep.

## Review r1 remediation (PR #1378, REQUEST_CHANGES → fixed)

Findings validated against source before fixing:

- **r1-f1 (mislabeled 503 fixtures)** — REAL. The only 503 the **GET history route** emits is `{"error":"workspace not ready","phase":…,"retryAfter":…}` (`proxy_adapter_crosscutting.go` resolveWorkspaceForAdapter, quoted at line 59); no handler authors `workspace connection failed`, `reason`, or a 503 `message` — and "The agent is not responding…" is the frontend's own fallback string (`useChatStream.ts:192-196`, a different component). (The POST message routes have a second, distinct 503: the fail-closed quota gate's `{"error":"quota check unavailable, please retry"}`, `proxy.go:379` — never a GET-history body.) Fixed: wiring test + Playwright 503 rows now use the exact not-ready body and assert the red error state (message from `body.error`, no reason branch taken); header comments quote both real bodies with their source files.
- **r1-f2 (carried-over dead-shape rows)** — REAL. The "Real API shape" 503 rows in `ChatHistoryErrorBanner.test.tsx` and the `ChatPage.historyError.test.tsx` Retry row pinned the dead `workspace connection failed` body. Fixed: unit row → the real not-ready body; Retry row → the real not-ready body.
- **r1-f3 (reason-keyed `isRecovering` branch has no API producer)** — noted by the second-pass reviewer. Branch KEPT (frontend display logic, outside #1303's four locations); its two coverage rows relabeled as defensive-branch-only with the no-producer fact and the #796 cross-reference, matching the `"retry"` flag treatment. Also flagged for #796: `useChatStream.ts`'s 503 `err.body?.message` read (same no-producer family).

Gates re-run post-remediation: vitest **1835/1835** (168 files), tsc **0**, Playwright `history-error-banner.spec.ts` **2/2**.

## Review r2 remediation (PR #1378, round-2 REQUEST_CHANGES → fixed)

- **r2-F1 (phantom shapes in freshly written source docs)** — REAL. `ChatHistoryErrorBanner.tsx` hierarchy doc said "e.g. 503 recovery bodies" for the structured `message` (no 503 body carries `message`; the only structured-`message` body is the 507 disk-full at `proxy_handlers.go:916-924` — verified) and its inline comment claimed `message` "always present on 503s" (false); `agentErrorRef.ts` carried the same phantom plus the dead `workspace connection failed` example (zero Go producers — verified by grep). Fixed: docs now cite only real bodies (507 disk-full for structured `message`; GET-history 502 `failed to fetch history` and the 503 `workspace not ready` for `body.error`); inline comment corrected.
- **Seam gate extended per the review's suggestion**: `ChatHistoryErrorBanner.tsx` added to the guarded files; new forbidden pattern `workspace connection failed` (the string that demonstrably resurfaced). Mutation-verified again: injecting the string into the banner → gate fails 1/5; reverted → green. Suite: vitest **1836/1836** (168 files), tsc **0**.

## Review r3 remediation (PR #1378, round-3 REQUEST_CHANGES → fixed)

- **r3-F1 (phantom-shape debt in `types.ts` `ApiError` docs)** — REAL. `ApiError.reason` documented a producerless 503 reason enum and `ApiError.message` claimed "Always present on 503s" (word-for-word the r2 phantom; only the 507 disk-full carries structured `message`) — in a file this PR edits and gate-guards, contradicting the PR's own defensive-branch labels. Fixed: `reason` doc now states no producer exists on the message routes (real 503 body cited) + #796 cross-ref; `message` doc cites the 507 as its only carrier; `retryAfter`'s "workspace restarting" wording tightened to the real not-ready semantics.
- **Non-blocking notes addressed while in there:** defensive fixtures no longer reuse the dead string (`agent did not respond` placeholder); seam-gate extraction pattern now matches optional chaining (`body?.data`); e2e `getSession` mock carries `status` (fidelity with the now-required contract field). Left as carried per the review's own disposition: `useChatStream.ts:187-196` phantom (pre-existing, #796-flagged).
- Gates: vitest **1836/1836** (168 files), tsc **0**, Playwright `history-error-banner.spec.ts` **2/2**. Frontend-scope grep (this PR's surface): the dead string survives only in the #796-flagged pre-existing `useChatStream.ts` comment and the gate's own pattern table. Outside this PR's scope it also appears in `sdks/go/client_test.go`, `sdks/typescript/tests/client.test.ts`, `sdks/python/tests/test_client.py` — SDK test fixtures, #1304's lane.

## Review r4 remediation (PR #1378, round-4 REQUEST_CHANGES → worklog corrections only)

The r4 review verified all code/test remediations sound; its blocking finding was three worklog verification claims that did not survive validation. Each re-validated first-hand before correcting:

1. **"no kind/docker/kubectl in this environment"** — TRUE of this agent's worktree container (`command -v` re-run: all three absent, docker daemon unreachable) but FALSE as a statement about the repo's execution environments: the review runner carries docker 28.0.4, kind v0.33.0, kubectl v1.37.0. The deferral is an agent-environment limit, not a repo limit — the kind leg is executable anywhere with the cluster toolchain. The Deferred section's rationale is corrected accordingly; the deferral itself stands per the r1–r3 disposition.
2. **"the only 503 the message routes emit"** — over-broad. Corrected to GET-history-scoped (the wiring test was already scoped correctly); the POST routes' fail-closed quota 503 (`proxy.go:379`) noted.
3. **"repo-wide grep"** — the grep was frontend-scoped only. Corrected; the three `sdks/` occurrences recorded for #1304.

No code or test changes in this round.
