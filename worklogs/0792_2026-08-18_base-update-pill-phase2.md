# Worklog: base-update pill phase 2 — refresh flow + launch-picker pill

**Date:** 2026-08-18

**PR:** #931

**Issue:** #928

**Session:** continuation of the v1 work merged in #930

**Status:** phase 2 complete pending review; #928 closes with this PR

---

## Objective

Deliver the action half of the base-update pill: the one-click refresh
flow (prefill → diff review → save new hash; original untouched) and
the launch-picker staleness signal.

---

## Context

#930 shipped the read-side signal (updatesAvailable on config reads,
Settings pill, launch-picker pill). The refresh ACTION and the
launch-picker pill were deferred as phase 2; review round 1 of this PR
found the save step broken (below) — the flow looked complete but
failed at its terminal step against the real API.

---

## Work Completed

- Refresh flow (WorkspaceImagesTab): "Refresh to <target>" button on
  stale expanded cards; prefills the create form — name de-conflicted
  as `<original> (<base> <version>)` (scoped name uniqueness would 500
  the save otherwise), exact extension selection, base pre-targeted at
  migration-default or same-name latest.
- Amber banner states the contract: same extensions, new base, NEW
  config on save, original untouched and launchable; migration variant
  adds the ruling-29 Debian-suite caveat. Cancel restores the default
  base (not the pre-targeted one — review round 2 C5) and empties the
  form. Save toast distinguishes refresh from plain create and names
  the update target.
- Launch picker (NewWorkspaceSplitButton): ↻ pill on stale ready
  configs; launching stale configs remains fully allowed.
- Unsupported-on-base hint (review round 2 C4): when the current
  selection contains extensions whose supportedBases exclude the target
  base, the form says so before save (the API would 422 per-extension
  at save time otherwise).
- Docs: docs/operator/image-factory.md — base lifecycle for operators
  (publish, move default, extension-coverage prerequisite, user-visible
  matrix, failure modes). mkdocs nav entry.

---

## Key Decisions

- **De-conflicted default name instead of a conflict-mapped 422**: the
  prefill suggests `name (base version)`; the user may edit. Simpler
  than teaching the create path about refresh-origin conflicts, and the
  common case never hits the unique constraint.
- **Form-as-diff instead of a separate diff-preview screen**: the
  prefilled form shows extensions unchanged and the new base in the
  select; the banner carries the semantics. #928's "diff preview" scope
  item is satisfied by this (issue text updated to say so) rather than
  a bespoke before/after modal — less surface, same information.
- **Cancel restores the default base**, not the pre-refresh selection:
  leaving the migration base pre-targeted would silently aim the next
  manual create at it.

---

## Assumption validated (Rule 7)

- Scoped name uniqueness constraint exists (migration 000013
  partial unique indexes) — validated by review round 2's empirical
  500 reproduction; the de-conflicted name exists BECAUSE the
  assumption was proven, not guessed.
- Extension supportedBases is enforced at save (ResolveSelection) —
  surfaced upfront in the form per the same review.

---

## Tests Run

Commands and outcomes (node_modules installed this session; the
previous "sandbox lacks node_modules" claim was stale):

- `npx vitest run src/components/settings/WorkspaceImagesTab.test.tsx`
  → 23/23 pass (9 refresh-flow: prefill/de-conflicted name, cancel,
  fresh-no-button, save-success via mockToast, save-failure keeps
  prefill, version_bump variant no-caveat, retired-base toast,
  cancel-restores-default-base, unsupported-on-base hint)
- `npx vitest run src/components/workspace/NewWorkspaceSplitButton.test.tsx`
  → pass (incl. launch-picker pill test)
- `npx vitest run` (full) → 1681/1681 pass (round-3 addendum runs;
  1678 at the round-2 commit — numbers are per-run, see addenda)
- `tsc --noEmit` → clean

Integration/e2e levels: NOT added this round — handler/store paths are
unchanged by this PR (frontend-only); the real-API save semantics that
broke are pinned by existing handler tests and the de-conflicted-name
component test. Noted as a gap if reviewers require a Playwright leg.

---

## Blockers

None.

---

## Next Steps

- #928 closes with this PR.
- Future: SDK surface includes updatesAvailable from day one (reworded
  AC); Playwright e2e for the refresh flow if the suite grows
  image-factory coverage.

---

## Review round 3 addendum

- R2 (retired extensions): prefill auto-drops selection IDs absent from
  the catalog (retired), reports via toast; fully-retired selection
  aborts BEFORE any state mutation (no half-landed banner). The
  unsupported hint also flags absent-from-catalog IDs (manual path).
- R3 (repeat refresh): suggested name deduped against the in-hand
  configs list with numeric suffix — second refresh of the same
  original no longer deterministically 500s on the unique constraint.
- Operator doc's three factual errors corrected: 500 (not 409) on
  scoped-name collision with the 4xx mapping noted as backend debt;
  POST-upsert (no PUT) for isDefault; multiple defaults resolve
  highest-sorted (not first). Added retired-extension and
  repeat-refresh failure-mode entries.
- E2E: tests/e2e/image-factory-refresh.spec.ts — 3 legs (happy
  save-and-toast with original intact; save-500 retains prefill;
  retired-base race errors loudly). Playwright browser can't launch in
  this sandbox (missing system libs); CI's runner executes the suite —
  the specs follow the established platform-image-factory mock pattern.
- Component tests for R2/R3 added (auto-drop+report, fully-retired
  abort, name dedup): file 26/26, full suite 1681/1681, tsc clean.

---

## Files Modified

frontend/src/components/settings/WorkspaceImagesTab{.tsx,.test.tsx};
frontend/src/components/workspace/NewWorkspaceSplitButton{.tsx,.test.tsx};
docs/operator/image-factory.md; mkdocs.yml.

---

## Review round 5 addendum

- N3: successful refresh-save now restores the default base (mirror of
  the cancel fix; test drives the select trixie→bookworm through
  refresh→save).
- N5: the hint's ext.retired direction corrected (retired entries are
  flagged, not suppressed); the retired-drop toast stays "success"
  severity (the provider's only kinds are success/error; the message
  text carries the informational tone).
- N1/N2: the move-default runbook now states what the API actually
  does — the upsert never clears other defaults (two-step move
  procedure) and the boot seed re-applies its isDefault on every API
  restart (seed-PR as the intended flow until the backend grows an
  operator-preserving mode, recorded as debt). Fresh-path error message
  named. File 27/27.
