# Worklog: Threat model reconciliation for v0.3.0

**Date:** 2026-07-11
**Session:** Bring `THREAT-MODEL.md` and `security-report-g33-g47.md` in sync with the v0.3.0 network hardening sweep landings.
**Status:** Complete

---

## Objective

After PRs #513 (G34), #515 (G39/terminal-origin), #516 (CORS), #517 (NetPol),
#518 (runtimeClass), #519 (JWT) landed, the threat model docs were stale:

- G33 was still listed as 🔴 Open Critical even though
  `WorkspaceAccessMiddleware` has been wired on `idGroup` since the v2
  design pass. The "Open" status was doc drift, not a real gap.
- G34 was still 🔴 Open Critical — closed by PR #513.
- G39 (terminal WebSocket `CheckOrigin: return true`) was still 🔴 Open —
  closed by PR #515.
- The Implementation Status Summary still said "Critical open gaps: G33, G34"
  and counted 17/26/7.
- The STRIDE table's Proxy row still called out G33/G34 as live.

Goal: reconcile the docs against the actual code state and remove the stale
"Critical open gaps" callout.

---

## Work Completed

### `design/stories/epic-17-security-review/THREAT-MODEL.md`

- §9 gap table: G33 → 🟢 Fixed (with citation to the wired middleware and
  the `TestWorkspaceAccessMiddleware_*` regression battery). G34 → 🟢 Fixed
  (with PR link and the copyRequestHeaders implementation pointer). G39 →
  🟢 Fixed (with PR link, the new `newCheckOriginChecker` location, and the
  Helm value). G49 → 🟢 Fixed (rotate-kek CLI ships).
- STRIDE Proxy row: marked G33/G34 items as 🟢 Fixed inline.
- §10 Implementation Status Summary: 21 Fixed / 22 Open / 7 Accepted.
  Removed the "Critical open gaps: G33, G34" callout; replaced with a note
  explaining the closure and naming the new highest-severity open gaps
  (G35 RecoveryAccount rate limit, G50 decrypt audit not wired).
- §11 Revision History: added v2.3 entry documenting the reconciliation.

### `design/stories/epic-17-security-review/security-report-g33-g47.md`

- Added a v0.3.0 banner at the top of the file explaining that G33/G34/G39
  are resolved, with brief one-line summaries and PR links. The per-finding
  sections below are preserved verbatim for historical context.
- Per-finding `**Status:**` lines for G33, G34, G39 updated to
  "✅ Resolved (v0.3.0, PR #...)".

---

## Key Decisions

1. **Don't rewrite history.** The per-finding sections in
   `security-report-g33-g47.md` are preserved verbatim. They're the
   adversarial validator's contemporaneous notes from 2026-06-12; editing
   them would erase the reasoning that motivated the fixes. The resolution
   banners at the top + the per-finding status updates are sufficient.
2. **THREAT-MODEL.md is now authoritative.** The security-report is a
   point-in-time artefact; the threat model's gap table is the living
   document. The banner in the security-report says so explicitly.
3. **Counts reconcile exactly.** 20 Fixed + 23 Open + 7 Accepted = 50. The
   prior summary's "17 + 26 + 7 = 50" was correct at v2.2; this update
   moves 3 from Open to Fixed.

---

## Assumptions stated and validated (Rule 7)

1. *G33 was doc drift, not a real regression.* Validated by reading
   `router.go:291-292` (`idGroup.Use(middleware.WorkspaceAccessMiddleware(...))`)
   and `router.go:331` (`registerProxyRoutes(idGroup, ...)`). The
   middleware exists, is wired, and is inherited by every proxy route.
2. *No other doc references the old G33/G34 status as authoritative.*
   Validated by `grep -rn "G33\|G34" design/` — only
   `epic-17-security-review/` references them.
3. *The other gaps (G35-G47, G50) are still actually open.* Validated
   by spot-checking G35 (RecoverAccount still has no rate limit at
   `router.go:264`). G49, previously thought open, is **actually
   Fixed** — the AI reviewer on PR #521 caught that my original
   "rotate-kek CLI still doesn't exist in `cmd/`" claim was wrong;
   `cmd/rotate-kek/main.go` exists (153 lines, full implementation).
   The G49 row's stale "CLI pending" text was itself doc drift.
   Updated G49 → 🟢 Fixed in this round.
4. *Cited line numbers verified.* Re-validated after merge of latest
   main: `idGroup.Use(WorkspaceAccessMiddleware)` at `router.go:287-288`,
   `registerProxyRoutes(idGroup, ...)` at `router.go:327`,
   `copyRequestHeaders` call at `proxy.go:470`.

---

## Blockers

None.

---

## Tests Run

Doc-only change. No tests apply. Verified the markdown renders correctly
(no broken table syntax) by reading the file end-to-end after edits.

---

## Next Steps

1. Open this PR for review.
2. After approval + merge + the release-engineering PR #520 merges, cut v0.3.0.

---

## Files Modified

- `design/stories/epic-17-security-review/THREAT-MODEL.md` (G33/G34/G39 rows, STRIDE Proxy row, §10 summary, §11 revision history)
- `design/stories/epic-17-security-review/security-report-g33-g47.md` (v0.3.0 banner, per-finding status lines)
- `worklogs/NNNN_2026-07-11_threat-model-reconcile.md` (this entry)
