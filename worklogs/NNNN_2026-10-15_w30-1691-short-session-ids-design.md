# Worklog: #1691 — design 0066: harness-abstracted short session IDs + keyword alias layer + did-you-mean resolution

**Date:** 2026-10-15
**Session:** w30 design lane (worktree `wt-1691`, branch `design/1691-short-session-ids`)
**Status:** Complete — PR open, iterating review

---

## Objective

Produce `design/0066_2026-10-15_short-session-ids-alias-layer.md` for issue #1691: elaborate HOW the eight owner-settled rulings become a system — mint point and ownership, persistence mapping, additive ABI migration, alias-layer semantics, did-you-mean spec, security notes, rollout sequencing. DESIGN ONLY: no implementation, no ABI changes, no schema changes.

## Work Completed

### design/0066_2026-10-15_short-session-ids-alias-layer.md (new)

- **§2** Rulings transcribed verbatim from the issue (all 8; normative source for §4).
- **§3** Verified today-map: `session_index` schema + the five row-creating UPSERT shapes; the #1667 suggest layer (`session_suggest.go`); ABI `Session`/`ActionResult` field numbers (next free 13/12); armed freeze gate (`abi/FROZEN`, Makefile targets); frontend addressing (`/chat/:ws/:sessionId`); titles-are-caller-owned fact (in-tree adjective-noun generator mints *workspace* names, `names.ts` — the platform mints no session title).
- **§4.1** Format `oc-` + 8×[0-9a-z], prefix registry (oc assigned; cc/cx/pi reserved), prefix = inert mint-time metadata; collision arithmetic matching ruling 3's ~0.002% (36⁸ ≈ 2.82e12; C(10⁴,2)/N ≈ 1.8e-5).
- **§4.2** Mint ownership nested in #1643's S1–S4 single-owner frame: three load-bearing rules — one owner for the mapping fact (the API registration seam, NOT the volatile sessionstate authority); no sixth parallel store (agentd artifact is a replica with a convergence path, #1340 discipline); registration ≠ state-watching (cannot regress S1–S4). `EnsureSessionRegistered` seam with check-and-retry; callers enumerated (sessions/new, proxy row-creating paths with in-process LRU, agentd create_session via control plane, convergence sweep). Honest record: the #1643-referenced "design/0064 session-state" doc is absent from the repo (verified); frame cited from the issue body.
- **§4.3** API DB = durable truth (nullable column + partial unique index, two-phase NOT NULL flip; Go-side backfill — no SQL RNG, no deterministic hashing per ruling 6); internal joins stay on harness id (edge-alias principle). Agentd replica = JSON file (dev-preview-headers pattern), boot pull + on-miss pull + push; agentd never mints.
- **§4.4** Resolution precedence (short ci > legacy exact > unambiguous workspace-scoped alias); aliases computed never stored; rename flips resolution live; no alias history.
- **§4.5** Additive ABI (Session.short_id=13, ActionResult.short_id=12), freeze-gate compliance argument (buf permits tail additions; regen freshness incl. TS), REST/SDK additive fields, URL migration = value swap with legacy links immortal; Phase C end-state documented, unscheduled.
- **§4.6** Did-you-mean spec: trigger (404-class only), scoring (bounded Levenshtein ≤2 reusing `editDistanceBounded`, prefix/token on titles, ranking + lastActive tiebreak, top-3), verbatim response shape `{title, shortId, workspace, lastActive}`, workspace-scoped-first over authorized workspaces only, never-auto-resolve, surface table (API/list/agentd/SDK).
- **§4.7** Enumeration: 41.4 bits is fine because authz scopes everything; did-you-mean reveals no row the caller can't already list; 403s never suggest; per-workspace uniqueness kills cross-tenant existence signals.
- **§7** Rollout: implementation AFTER #1643 S1–S4 lanes + current SSE/UX train (#1686/#1687, #1665/#1666 same-surface); M1 mint+persist (no behavior change) → M2 additive surfacing → M3 addressing flips + did-you-mean → M4 docs; each phase revertible.
- **§8** Two open questions only (suggestion `workspace` field id-vs-name; reserved prefix spellings confirm) — neither blocks M1.

## Key Decisions

- Mint owner = API registration seam (single-writer), agentd = replica. Kept out of the sessionstate authority deliberately — the #1643 incident class is parallel stores; identity is not state.
- Legacy ids stay valid aliases until a major version (ruling 7); "immortal old links" is the whole URL migration story.
- No SQL-side mint/backfill RNG: the alphabet and entropy live in Go, in one place.

## Blockers

None.

## Tests Run

None — design document only. Every code claim verified by direct source read at 9b1f317c (see §3 citations; line refs re-checked post-write).

## Files Modified

- design/0066_2026-10-15_short-session-ids-alias-layer.md (new)
- worklogs/NNNN_2026-10-15_w30-1691-short-session-ids-design.md (this file)
- COORDINATE.md (claim row)
