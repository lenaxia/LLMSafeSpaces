# Worklog: 0a — entry-level admission idempotency (S2 rewritten)

**Date:** 2026-09-11
**Session:** epic-71 / 0a (#1315): root-cause the ~180s re-admission loop (16 transcript copies, attempts never incremented) and land the S2 write-site fix (agent: opencode-vesper)
**Status:** In Review (PR pending; merge gate = incident repro green + pool rows/#1308 pins)

---

## Objective

Retire the re-admission duplicator class: at most ONE transcript user message per outbox entry, across all attempts and re-admissions, enforced where the write happens.

---

## Work Completed

### Root cause (validated against main)

- The ~180s cadence is the **3-minute `Admit` ctx** (`sessionstate/ledger.go`), not any outbox clock. `driveAdmission`'s ladder (5 attempts, 200ms·2ⁱ backoff) re-POSTs whenever the prior POST's synchronous V1 turn doesn't return — and the V1 POST body carried **no dedupe key** (`{parts}` only), so every POST was an unconditional transcript append.
- The incident loop: hung turn → client ctx abort → row stays LEDGERED → ladder re-POST (5×) → `markFailed` → outbox terminus re-arms attempt+1 → fresh row → 5 more → `replayUnresolved` adds cycles ⇒ 16 copies. `admittedAnywhere` never fired because ADMITTED is only recorded when `Admit` RETURNS — "reached the harness but the client timed out" was the blind spot. The outbox recorded only its own 5 attempts (`context deadline exceeded`, ctx 50s-1min < the 3m30s inline window) — the duplicating loop was agentd's, invisible to it.

### G1 gate — live probe on the pinned opencode 1.18.15 (this agent's own harness)

- `messageID` is a validated V1 body field: non-`msg`-prefixed → `400 "Expected a string starting with \"msg\""`.
- A `msg`-prefixed key becomes the user message's **store ID verbatim** (assistant `parentID` = the key). Full 38-char entry-shaped keys pass.
- **Same-ID re-POST is a harness-side UPSERT**: returns the existing exchange, no new user/assistant message.
- By-ID point lookup: 200 present / 404 absent.

### Fix (TDD)

- `Admitter.Admit` carries the entry-derived key `harnessMessageID(entryID) = "msg_"+entryID` (attempt-independent); `opencodeAdmitter` rides it in the V1 POST body. Upsert-by-key bounds transcript cardinality **by construction**.
- `attemptAdmission` gains the pre-POST **store-evidence short-circuit** (reuses 1b's `StoreReader.MessagePresence` seam — one seam, two consumers): message present ⇒ `markAdmitted`-by-evidence, no harness write. Errors are not absence (seam contract) — fail open to the keyed POST.
- `Config.AdmitterTimeout` (0 ⇒ 3min default) — injectable so the repro drives the hung-turn window in test time.
- 1b precedence (per comment 5629147296): FAILED+evidence resolves ADMITTED-by-evidence on any re-drive (evidence check sits after `admittedAnywhere`, before the POST) — consistent with #1311's store-evidence convergence; the sweep's FAILED→attempt+1 re-arm now re-drives into the evidence check instead of a fresh write.

### Tests

- Unit (red-first): `TestDeliver_AdmitCarriesEntryDedupeKey`; `TestDeliver_EvidenceShortCircuitsReAdmission` (the incident regression — would-fail admitter never called, row resolves admitted with the key); `TestDeliver_EvidenceAbsentStillPosts`; `TestDeliver_EvidenceErrorStillPosts`.
- Wiring: `TestOpencodeAdmitter_UsesV1MessagePath` extended — body `messageID` field pin.
- **Incident repro**: `TestAdmissionIncidentRepro_HungTurnExactlyOneTranscriptMessage` — full production path (wire Deliver → ledger → ladder → real `opencodeAdmitter` → hung harness with G1-probed semantics) asserts exactly ONE transcript copy with the entry key and an ADMITTED-by-evidence row. Pre-fix mechanism reds are the unit tests above; the incident itself (ses_f73747f8) is the field evidence.
- Full `cmd/workspace-agentd/...` green (232s); `-race` green on root (273s) + sessionstate (55s); repro green under `-race`; gofmt/vet clean.

## Key Decisions

- **Key in the POST body vs text-embedded marker**: body field is harness-native (G1), invisible to users, and gives the by-ID point lookup. Text markers were the fallback if G1 failed.
- **Reuse `MessagePresence`** instead of a new by-ID seam method: zero interface churn; the sweep and the write path share one evidence definition. A by-ID GET optimization can come later if paging shows up in profiles.
- **Kind repro → deterministic integration repro**: a real kind leg cannot deterministically freeze a live LLM turn (flaky-by-design); the repro test reproduces the G1-probed harness semantics exactly and drives the real wiring. Deviation from the epic table's "kind repro" wording noted in the PR for the owner's call.

## Assumptions stated and validated (Rule 7)

1. Each ladder attempt POSTs unconditionally pre-fix — validated in code + the incident's 16 copies at the 3-min cadence.
2. Harness honors client `messageID` — **live-probed (G1)**, not assumed.
3. Evidence-coupled store semantics match production — production `MessagePresence` pages the harness store; the repro couples directly.
4. `admittedAnywhere` excludes FAILED — validated (1b's comment + code).

## Blockers

None. Open coordination: 1b to sanity-check the FAILED+evidence precedence (comment 5629147296 ask).

## Tests Run

`go test ./cmd/workspace-agentd/...` (green, 232s); `-race` root + sessionstate (green); repro `-race` (green); `gofmt`/`go vet` clean. Pool rows + #1308 pins ride CI/pool on the PR branch (merge gate).

## Next Steps

- PR → automated review cycle; pool dispatch ask (as with #1321).
- 0a remainder per #1315: fault legs 7-8 assertions in the #1312 harness when 2b lands the knobs (transcript cardinality per entry is now assertable via the key).

## Files Modified

- `cmd/workspace-agentd/sessionstate/ledger.go` — key derivation, evidence short-circuit, AdmitterTimeout, Admitter doc
- `cmd/workspace-agentd/sessionstate/authority.go` — Config.AdmitterTimeout
- `cmd/workspace-agentd/sessionstate_wiring.go` — Admit signature + body messageID
- `cmd/workspace-agentd/admission_incident_repro_test.go` — new: the incident repro
- `cmd/workspace-agentd/sessionstate/ledger_test.go` — 4 new tests + fake arity
- `cmd/workspace-agentd/sessionstate_wiring_test.go` — body-field pin + arity
- `cmd/workspace-agentd/{admitter_mcp_e2e,admitter_v1_mcp,sessionstate_metrics_test,sessionstate/actions_test,sessionstate/reconcile_test,sessionstate/delivery_op_test}.go` — Admit arity updates
- `worklogs/NNNN_2026-09-11_entry-level-admission-idempotency.md` (this file)
