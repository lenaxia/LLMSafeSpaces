# Worklog: salvage — #1759 engine deny-grammar documentation on the merged floor

**Date:** 2026-10-20
**Session:** Salvage PR B of the closed #1764 (Refs #1759): the live-observed engine deny grammar recorded as residual 3 on #1762's landed permissiontiers.go, including the orchestrator-mandated children-deny experiment and the attribution correction from the salvage-A mutation experiment.
**Status:** Complete

---

## Objective

Document the engine facts the #1759 investigation proved — so the next engineer at the external_directory seam inherits them instead of re-deriving them — without changing any shipped behavior (docs-only floor change plus one model matrix row).

---

## Work Completed

- **Residual 3** on the landed permissiontiers.go: the grammar matrix (only "X/*" patterns fire denies; exact and terminal-star keys load but never match for deny, fire for allow), consequence (a) bare credential names are intent/model rules (live: bare-name listing executes under the root allow; contents stay children-glob denied, pinned live), (b) a single file under an allowed glob has no live-effective deny shape — the workspace auth.json exposure is the config allow's own trade, WITH the correction that the closed #1764 asserted an unobserved engine auth-store self-allow and the "cannot protect" conclusion derived from it, (c) appended engine self-allows (observed shapes: tool-output dir, "/tmp/opencode/*") beat config under findLast, (d) the children-deny experiment, upstream asks (i)-(iii) with (iii) reworded to "document which paths the engine self-allows".
- **The mandated experiment** (run from the salvage-A tree, verdict-only logging): floor + "/workspace/.local/opencode/*" deny, bash `cat /workspace/.local/opencode/auth.json | wc -c` → **denied=true, rule quoted** — the children deny BITES. Outcome (a) of the ruling: a live-effective protective shape exists; floor adoption is the owner's decision (scope trade: also denies opencode.db-class session state to agent tools), explicitly not taken in this docs PR.
- **Model matrix row** (grammar edge): "/workspace/.local/opencode/auth.json.bak" → allow — documents the model's honest verdict for unprotected file siblings; catches a future bogus file-level deny that would silently change it.
- **Attribution correction recorded** (Rule 11 Phase 2): the closed #1764 asserted an unobserved engine auth-store self-allow (its finding 2 listed the XDG auth store among appended self-allows; no rule dump ever showed one) and derived from it the conclusion that a children deny cannot protect auth.json — falsified jointly by the dump-absence record and by the (d) experiment (which ran the harness's synthetic XDG_DATA_HOME: it proves the children deny bites for the probed path).

## Key Decisions

- Docs-only: the children-deny key is proven live but NOT added — per the orchestrator's ruling the trade-off is the owner's, not a docs PR's.
- The note attaches to #1762's landed shapes (their floor has no workspace twin; residual 1's project-root claim is attributed as THEIR governing claim for the default pod, with the foreign-project-root governance separately proven).

## Blockers

None.

## Tests Run

- `go test -count=1 -run TestPermissionTier ./pkg/agent/opencode/` → ok (the new matrix row green on the landed model).
- The mandated experiment: 1/1 leg PASS on the pinned 1.18.15 (denied=true, ruleQuoted=true), verdict-only logging, probe file never logged.

## Next Steps

- PR review loop to APPROVED; packet to the orchestrator. PRs #1768 (prompt) and #1770 (harness/legs) review loops run in parallel.

## Files Modified

- pkg/agent/opencode/permissiontiers.go — residual 3 (engine deny grammar + experiment + corrected attribution)
- pkg/agent/opencode/permissiontiers_test.go — grammar-edge matrix row
- worklogs/1149_2026-10-20_salvage-engine-deny-grammar-note.md — this worklog
