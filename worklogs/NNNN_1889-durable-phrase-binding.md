# Worklog: #1889 — durable phrase binding (F1), unresolved-ref no-raw-500 (F2), reaped-with-phrase observability (F3)

**Date:** 2026-10-24
**Branch:** `fix/1889-durable-phrase-binding`
**Status:** PR opening (r1)

---

## Objective

Issue #1889 (follow-up to #1850, scope split from PR #1881): three
adjacent-hardening items around the session-phrase binding. Per the
VALIDATE-FIRST ruling, every sub-item was reproduced (or ruled
already-solved) against CURRENT origin/main `85b121e91` (v0.41.0) —
the sessionindex/phrase area took #1881 (write-once short_id,
phrase-bearing births, spawn grace), #1865 (reap by-ID confirmation +
replica merge), #1887/#1882 (delete propagation + deleted_at death
marks), and #1914/#1879 (archive durability) this week.

## Validation-first findings (what STILL reproduces vs. landed work)

**F1 — durable phrase binding: REPRODUCES (structurally).** No ledger
or any phrase history keyed independently of the row exists on main:
`EnsureSessionRegistered` and `execWithMintedPhrase` mint fresh with no
consult (short_session_ids.go). After ANY row death the binding dies
with the row; the resurrection vector (late title/message/context
upserts, re-Register, backfill sweep) mints a FRESH phrase. NOTE the
trigger set NARROWED vs. the issue's snapshot: #1865's by-ID probe
vetoes the reconcile from reaping a LIVE quiet count-zero session
(`reap_vetoed_live`), so the "live quiet >15min" trigger is closed;
what still exercises the defect class: hard delete → in-flight late
events (the #1901 drain tombstone only covers QUEUED RecordMessage
drains, not title/context/last-seen upserts or Register), post-grace
reaps of genuinely-gone rows later re-born, and the #1881-documented
residual. Red evidence: register → DeleteSessionTree → re-register
mints a random new phrase (unit pins + the integration loop).

**F2 — unresolved-ref no-raw-500: REPRODUCES ON THE REST SEND PATH;
the agentd MCP path is already typed (STOP-coding class for that
half).** Landed work covers the MCP send_message tool: the
SessionExists pre-validation (mcp_tools.go, the #1465/#1663/#1667
belts) answers a typed not-found + did-you-mean for BOTH orphaned-
phrase shapes (replica-known dead mapping, and unknown-phrase
passthrough). The REST API send surfaces were NOT covered:
`SendMessage` and `syncSend` wrap EVERY send failure in a raw
502 `{"error":"failed to send message"}` — an orphaned phrase (or any
unknown id) mid-sentenced a 5xx with no typed verdict, no did-you-mean,
and no reap (the read paths' `reapSessionIfGone` treatment never
applied to sends). Red evidence: handler pins answering 410+
suggestions on main's 502.

**F3 — reaped-with-phrase observability: REPRODUCES.** No
`reaped_with_phrase` (or any phrase-orphan) disposition exists
anywhere; all three delete sites (read-path reap, reconcile reap,
ApplySessionDeleteEffects) delete phrase-bearing rows silently. The
#1887 `deleted_at` death-mark is a read-path filter signal, not an
orphaning record. Red evidence: outcome counter never fires.

## The fix

### F1 — migration 000047 `session_phrase_ledger` (api + byte-identical helm mirror)

One row per (workspace_id, session_id) holding the phrase the row was
born under, keyed INDEPENDENTLY of session_index (the indestructible
binding). Backfilled from every live minted row at cutover (a claim on
a pre-ledger row can never outrank its real phrase). Design decisions
for the issue's "design work needed" items, in the migration header:
PK (workspace_id, session_id) ONLY — a (workspace_id, short_id) unique
index is deliberately ABSENT (after row death the phrase is free for a
new session to mint; ledger-level uniqueness would mis-refuse the
legitimate re-mint); ledger rows survive reaps/hard deletes (that IS
the feature); retention = workspace lifetime (DeleteSessionIndex, the
workspace-reset path, clears the ledger alongside the rows).

Mint seams (`session_phrase_ledger.go` + `short_session_ids.go`):
`phraseBaseFor` consults the ledger (hot path is ONE extra SELECT);
on miss mints fresh and CLAIMS via
`INSERT … ON CONFLICT (workspace_id, session_id) DO UPDATE SET
short_id = session_phrase_ledger.short_id RETURNING short_id` — the
no-op-update trick so racing claimants converge on the winner (the
registration seam's check-and-retry one level down) in one round trip.
Both `EnsureSessionRegistered` and `execWithMintedPhrase` (signature
extended to carry workspace/session; all six upsert call sites
updated) use the ledger phrase as the mint BASE — a resurrected row is
BORN under its original phrase (write-once trigger untouched:
INSERTs only). The 23505 suffix ladder climbs from the ledger phrase;
a suffixed birth corrects the ledger to the phrase the row actually
holds (recordPhraseLedger). EVERY ledger failure degrades to today's
fresh-mint semantics — convergence is best-effort, addressing never
fails (the pre-existing sqlmock pins pass UNCHANGED because the
unexpected-query errors are swallowed by the degrade).

### F2 — the send path's typed not-found (handlers)

`sendTargetGone`: after a send failure, ONE by-ID probe
(`probeSessionAliveByID` — the #1850 precedent) asks the READ route
for the definitive verdict. The adapter's message-POST 404 semantics
stay unclassified (adapter_helpers' ruling: non-session routes can 404
for other meanings) — the PROBE arbitrates, never the send error.
Definitive not-found → `reapSessionIfGone` answers the typed 410
session_gone + §4.6 did-you-mean and reaps the guard-admitted ghost
row (byte-consistent with the read paths); alive (busy/mid-turn
failure) or indeterminate (transport) → false, the caller keeps its
wedge/507/502 answer — uncertainty never answers the gone surface.
Detached context: the probe must survive a client that disconnected
during the failed send. Wired into BOTH `SendMessage` and `syncSend`
(after the wedge classification, before the disk-full/502 legs).

Existing pins updated (each disclosed): the four transport-failure
pins gained a probe-answering-alive `getSessionFn` (their 502/507
assertions unchanged); `TestSendPromptAsync_AdapterPath_SessionNotFound_
Returns404` — which pinned the raw 502 ("session not found error maps
to 502 via Send path"), i.e. documented exactly the class #1889 closes
— is now `…ReturnsTyped410`; the sessionsAct env's empty mockAdapter
became lenient (the probe runs in both regimes — the reconcile
precedent).

### F3 — `reaped_with_phrase` disposition

`observePhraseOrphaned` (one counter + one Warn log with
workspace/session/phrase/origin) at all three delete sites:
read-path reap (`read_path_reap`), reconcile reap (`reconcile_reap`,
row lookup via the byInternal map, built once and shared with the
truthfulness split), and ApplySessionDeleteEffects (`delete_effects`,
phrase captured pre-delete via a best-effort list filter; a failed
lookup skips the disposition, never the delete). Help text updated.
Phrase-less rows orphan nothing — the outcome is the phrase-orphaning
signal, not a delete counter (mirror pins). The F1 ledger doubles as
the durable tombstone audit (binding recoverable forever after);
this outcome makes the ORPHANING EVENT dashboard-visible. Workspace
reset is a documented non-goal (the workspace's dashboard relevance
dies with it; the ledger is cleared there regardless).

## Red-first evidence (captured on unmodified main, branch base)

- database (sqlmock): `TestEnsureSessionRegistered_RowDeathRebirthConvergesOnLedgerPhrase`,
  `TestUpsertSessionTitle_RebirthCarriesLedgerPhrase`,
  `TestEnsureSessionRegistered_FreshBirthClaimsLedger`,
  `TestEnsureSessionRegistered_LedgerUnavailableStillRegisters` —
  the first three FAIL on main (no ledger consult; random-mint INSERT
  args), the last is the degrade guard.
- handlers: `TestSendMessage_TargetGone_ReturnsTyped410WithSuggestions`,
  `TestSendMessage_PhraseTargetGone_ReturnsTyped410`,
  `TestSyncSend_TargetGone_ReturnsTyped410` — FAIL on main (502);
  `TestReconcile_ReapedRowWithPhrase_IsObservable`,
  `TestReadPathReap_RowWithPhrase_IsObservable`,
  `TestApplySessionDeleteEffects_PhraseBearingRow_IsObservable` —
  FAIL on main (counter never fires). Veto mirrors (alive→502,
  indeterminate→502, phraseless→no outcome) PASS on main — guards,
  not regressions.

## Verification

- `go test ./api/... ./pkg/... ./cmd/... -count=1` — green, with one
  PRE-EXISTING exception (see disclosure).
- `-count=3` stress over database + sessionindex + the send/reap/
  reconcile/delete-effects handler families — green.
- `go vet` (unit + `-tags integration`) on the three touched packages
  — clean.
- `make repolint` — all checks passed (migration sequence 47, helm
  mirror byte-identical).
- Integration tier (`-tags integration`, TestIntegration_PhraseLedger_*:
  resurrection convergence, upsert-leg rebirth, migration backfill,
  distinct-phrases): skips in this pod — no local Postgres server
  binary (client only); rides CI's provisioned Postgres per the house
  contract, the #1881 lane's exact posture.
- **Pre-existing failure disclosed:** `pkg/repolint
  TestLive_Worklogs_NoDuplicates` fails on UNMODIFIED main (verified
  via stash) — the tracked `worklogs/NNNNN_2026-10-20_1788-sidebar-
  gesture-e2e-flake.md` sentinel (from #1788's r18 merge commit
  f0c10b612) is unnumbered on main; `make repolint` itself reports it
  as WARN (non-gating) and passes. Not this lane's breakage.

## Scope / surfaces

- No fleet no-touch areas: pkg/board, the frontend provider, and
  api/internal/workflows engine are untouched (the engine's
  ApplySessionDeleteEffects INTERFACE declaration is engine-side and
  unchanged; the implementation change lives in handlers).
- MCP tool surfaces: unchanged (no docs-parity delta — the agentd MCP
  send path was already typed; validated, not re-fixed). API surface:
  the send routes' FAILURE answer changes class for the
  definitively-absent-target subset (502 → typed 410 session_gone,
  the read paths' existing body contract); no openapi delta (error
  responses only).
- Metrics: one new `session_index_reconcile_outcomes_total` label
  value (`reaped_with_phrase`). Storage: migration 000047 (api + helm
  mirror, byte-identical).
