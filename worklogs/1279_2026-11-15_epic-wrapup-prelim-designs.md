# Worklog NNNN — Epic wrap-up review: high/low-level designs (prelim story)

- **Date:** 2026-11-15
- **Story:** tsk-florid-teapot-pearls (PRELIM: scope + high/low-level designs for epic
  wrap-up review) — board project `prj-orient-mint-stencils`, epic `bb456235`
  ("Epic wrap-up review (holistic, delta-scoped, role-configurable)")
- **Artifacts:** `reports/specs/epic-wrapup-review-high-level-design.md`,
  `reports/specs/epic-wrapup-review-low-level-design.md` (this PR)

## What this did

The prelim story for the wrap-up epic: produced the designs BEFORE the implementation
stories run, verified every seam against real code (`de99d5361`), then patched the four
implementation charters from the findings.

## Seam verification (all anchors checked against main at de99d5361)

- Epic auto-complete: `epicCompletionCheck` pkg/board/store.go:449-463; five call sites
  (stage.go:516 doneCascade, review.go:141, service.go:866 epicLifecycle, reconciler.go:1308
  drift-done, reconciler.go:1517 rc4Net) — confirmed SINGLE seam, one-function
  interception design.
- Minting: `CreateTask` service.go:886-1020 (replay-first guards, `mintUnique` phrase ids,
  in-tx deps). Wrap-up mint mirrors the shape, bypasses owner actor + node cap.
- Claim machinery: `ClaimNext` engine.go:76; personaClaimSQL/personaClaimMintTx
  persona.go:264/276; in_review entry mint stage.go:225-233; reconciler re-claim pass
  reconciler.go:~940-1045; `spawnFor` engine.go:675 — wrap-up rows ride ALL of it, with
  persona resolution + dispatch carve-outs designed, not a parallel ladder.
- Config plumbing: config.go keys/struct/validate/validatePersonaRoleRefs (:48, :196, :337,
  :670, :785-853); key-CHECK growth pattern migrations 000042→000049.
- Persona seeding: builtin_roles.go + board_role_directory.go:101/144 + app.go:1678-1698 —
  the wrapup-reviewer seeded role is a sibling of the built-in reviewer.
- Charter: boardCharterPrompt (api/internal/app/board_spawner.go:47-108) needs NO wrap-up
  branch — the wrap-up charter is the minted story description (frozen on the row).

## Design decisions worth remembering

- D1 interception at the single completion seam; epic row FOR UPDATE serializes drains.
- D2 self-exclusion via `tasks.kind`, lifecycle via explicit epic columns, mint guard
  `wrapup_task_id IS NULL`.
- D3 wrap-up claims are ordinary claims (lease ladder unchanged); carve-outs only in
  dispatch/rearm/persona-resolution.
- D4 watermark advances at CLOSE to the row's MINT time (mid-review completions fall in
  the next delta; single-transaction atomicity).
- D5 engine renders a window, never runs git; the reviewer computes the bounded diff.
- D6 explicit close verb `board_record_wrapup_verdict` (pass/gaps); GAPS holds the epic
  open through the ordinary drain predicate — zero new holding machinery.
- D7 cap = 3 consecutive closes; ack is a manager verb that resets + re-drains.
- Open questions (non-blocking, defaults pinned): cap value, ack-resets-to-zero,
  node-cap exemption for engine mints.

## Review trail (PR #2057)

- Round 0 (head 7c037361): CHANGES_REQUESTED — one blocker (the PASS close path re-ran
  the mint seam, whose enabled branch list has no completion path → empty-delta mint
  loop to the cap; the epic could never complete) + advisory anchor drift. Fixed on
  8b2e0bdd5: §2 note (the disabled branch is the seam's ONLY completion path), §4 direct
  guarded completion UPDATE in the verdict transaction, cap does not gate PASS (a PASS
  is the loop's terminus), §10 born-red pins, most anchors corrected.
- Round 1 (head 8b2e0bdd5): blocker verified FIXED; new one-line gap (the r1 fix's
  wrapup_epic_complete event missing from §8's vocabulary + the high-level Events row)
  + residual anchors. Fixed on e56e1f6fe: the full six-event vocabulary in both docs;
  criteriaAllMet store.go:487 re-applied (an earlier edit reported success without
  persisting), epicCompletionCheck range tails 449-463/452-463, the worklog/high-level
  :101 repeats.

## Follow-on

The four implementation stories were patched from these designs (per-story change
summaries in the prelim story's completion report); cross-story dependency edges added:
engine←config-persona, watermark←engine, gap-recursion←(engine, watermark).
