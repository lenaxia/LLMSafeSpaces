# Worklog: First-session harness picker — the frontend's only harness interaction (owner ruling 2026-11-16)

**Date:** 2026-11-16
**Session:** story tsk-vibrant-geyser-masks (epic "User-facing harness switching", re-scoped by owner ruling 2026-11-16 — supersedes the switch-control stub entirely)
**Status:** Complete — implementation + pins; PR iterating with AI reviewer

---

## Objective

Deliver the re-scoped frontend story: no switch UI exists or ever will (one harness per workspace, immutable). The frontend's only harness interaction is the FIRST-SESSION PICKER — a selection modal on the first session of an unbound workspace, binding harness + session atomically via the bind-once API's sessions/new body. Plus the post-binding read-only badge, the unbound-empty hint, and the pinned absence of every switch affordance.

## Design decisions (and why)

| # | Decision | Rationale |
|---|---|---|
| F1 | The picker gates on `workspace.harness == null` alone — no session-count check | The bind-once API (D4/D5 of tsk-visioned-hurdle-leaves) guarantees an unbound workspace has NO pod and can never have had a session (bind happens BEFORE session create). "First session on an unbound workspace" ≡ "any session creation while unbound". No cache peeking, no races. |
| F2 | Offer list = `harnessOptions` verbatim, degrading to `["opencode"]` | The API union-enforces opencode in harnessOptions; the same degrade the server applies on settings failure (D8) is mirrored client-side for listings that predate the field. |
| F3 | Single-option auto-bind: `options.length <= 1` → call sessions/new with the one harness, no modal | The charter: "no decision to make". The submit is identical; only the modal is skipped. |
| F4 | Submit rides the EXISTING `ensureSession` (POST sessions/new) with `{harness}` in the body | The API story made that body the atomic bind+create (the picker's submit, D5/D6). No second call, no PUT /harness from the frontend — one entrance, one wire. |
| F5 | Cancel creates nothing; a failed create keeps the workspace unbound in cache until `["workspaces"]` invalidation | The API keeps the binding server-side on a post-bind failure (convergent by design); invalidating on BOTH mutation outcomes keeps the sidebar honest — on retry the bound badge replaces the picker instead of re-offering a decided choice. |
| F6 | The "New chat" trigger now renders for unbound workspaces too | Unbound workspaces park in Creating (no pod until bind — API D4) and the Plus button was Active-only; the trigger IS the bind's entrance, so `(isActive \|\| harnessUnbound)`. |
| F7 | Badge = a `<span>`, never a control; the settings row is label + badge + note | The pin: zero switch affordances anywhere. The drawer test asserts no button/combobox/switch/radio and no interactive element inside the harness row. |
| F8 | a11y per #2021: Radix Dialog (focus trap, Escape, aria-modal), roving-tabindex radiogroup (arrows/Home/End move selection, focus-visible ring), polite live region announcing the selection | The house discipline; Radix gives the trap natively, the radiogroup implements the roving pattern BoardPage's columns use. |

## What was NOT built (the re-scope's point)

No settings switch control, no recreate flow, no confirm dialog, no "disabled when unswitchable", no enablement editing. The stale stub criteria describing those are superseded; their pins are the ABSENCE assertions (drawer test) instead.

## Verification

- `HarnessPickerDialog.test.tsx` (7): options render with name/blurb; opencode preselected; submit carries the selected harness (default as-is, changed choice); arrow-key move + aria-live announce; roving tabindex; submitting/error states; cancel submits nothing.
- `Sidebar.harness.test.tsx` (6): picker opens on first session of unbound multi-option ws + submit binds via ensureSession("ws-1", {harness:"pi"}); bound ws never opens the picker and the call carries no harness (exactly-once); single-option silent bind with no modal; unbound-empty hint renders (and only for unbound); bound header badge (name + title); unbound renders no badge.
- `WorkspaceSettingsDrawer.harness.test.tsx` (3): bound badge + permanence note; unbound not-chosen-yet note; ZERO switch affordances (no button/combobox/switch/radio named harness; no interactive element in the row).
- Gates: full frontend suite, tsc --noEmit, eslint, make fmt-check (gofmt — N/A for this diff, run green on the merge head).

## Coordination

Stacked on `origin/feat/harness-bind-once` (the API story, done on the board, orchestrator's merge pending): worktree branch merges that branch in so the picker builds against `harnessOptions` + first-session binding, which origin/main does not carry yet. When bind-once lands, this PR's diff collapses to the frontend delta. Never self-merge — the orchestrator merges.
