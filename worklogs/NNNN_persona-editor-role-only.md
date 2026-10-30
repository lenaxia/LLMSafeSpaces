# Worklog: the persona editor is ROLE-ONLY (owner ruling 2026-10-29, round 2 — tsk-pallid-pivot-wiggles)

**Session:** The v0.44.8 persona editor's empty-state presentation steered
toward the deprecated shape: a project with an ABSENT persona override map
(overrides `{}`, `stagePersonas: {}`, sources all code_default) rendered the
override state with legacy-shaped cards — per stage the role Select defaulted
to "No role (legacy name + instructions)" (PERSONA_NO_ROLE) and the free-form
name input + instructions textarea rendered as the primary affordance. This
lane removes every authoring surface except the role select, per the owner's
round-2 ruling.

**Owner rulings (2026-10-29, verbatim):**

- Round 1: "We don't need backwards compatibility because we never created
  any personas."
- Round 2 (stronger, supersedes): "We can remove any editor feature for the
  DX. It's not role-first, it's role-ONLY."

## Scope fence (the failed first attempt's lesson)

The previous attempt on this task rewrote the BACKEND too
(`pkg/board/config.go` decode removal, `sdks/openapi.yaml`, agentd schema) —
explicitly out of bounds: the charter says **Backend untouched** (stored
`{name, instructions}` / `{roleId, instructions}` shapes still decode and
ride wire; the default-reviewer composition and the set-path RoleID
validation unchanged; no migration, no wire change). That worktree state was
discarded wholesale (`git checkout -- .` + removing the stray untracked seed
test); this attempt is frontend-only:

```
 frontend/src/components/board/BoardConfigPanels.tsx        (the editor)
 frontend/src/components/board/BoardConfigPanels.test.tsx   (the re-pin)
 frontend/src/components/ui/Select.tsx                      (placeholder seam)
```

## Mechanism

**One authoring surface.** `PersonaEditor`'s explicit per-stage row is now
exactly: the role `<Select>` + Remove, wrapped in a `<label>` with the stage
name (the file's established select pattern — label click forwards to the
Radix trigger, which is also how the tests drive it). Deleted from the DOM:

- the `PERSONA_NO_ROLE` option ("No role (legacy name + instructions)") —
  gone, not deprecated; `roleOptions` starts at the first role;
- the free-form name `Input` (the legacy lane's `-name` testid);
- the instructions-override `<textarea>` (the `-instructions` testid) —
  instructions are not an editor surface anymore; the backend's
  default-reviewer composition and the stored-shape decode are the only
  writers/readers of them;
- the per-row role note (`-role-note`), replaced by ONE section-level
  `role-only-note` hint ("the role's model, tools, and system prompt are the
  single source of truth. A row with no role picked is not saved.").

**The picker.** "Add persona" now drops `{}` into the map — the added row IS
a role picker (Radix placeholder "Pick a role…" via a new optional
`placeholder` prop on the `ui/Select` wrapper; Radix shows it for
`value === ""` — `shouldShowPlaceholder`, verified in the installed source).
A stored legacy `{name, instructions}` entry renders the same unpicked
picker: there is no legacy editing lane anymore (round 1: no personas were
ever created).

**The drop-on-save rule.** `persistablePersonaMap` (both tiers' save bodies)
drops every entry with no `roleId` — added-but-unpicked rows AND stored
legacy shapes. Role-bearing entries ride VERBATIM (name/instructions fields
included — the wire shape is untouched; only the editor stopped authoring
them), so the round-1/round-2 wipe-protection pins (REPLACE never wipes a
stored map) hold for every persistable persona. The set-ness rule is
unchanged: no `stagePersonas` key unless explicit; `{}` remains the visible
opt-out.

**Reviewer import.** `reviewerImport` keeps its roleId-first arm (the
one-click suppression-warning restore adds the reviewer ROLE persona, select
shows it); its no-reviewer-role fallback still returns the legacy shape for
display continuity, but it carries no roleId and therefore does not persist
— coherent under role-only.

**Backend untouched.** `frontend/src/api/board.ts` `BoardPersonaConfig`
({roleId?, name?, instructions?}) is unchanged — it mirrors the wire, and
the wire did not move. No Go, no openapi, no migration, no MCP.

## Test re-pin (BoardConfigPanels.test.tsx)

- The legacy-input pins are gone: "renders editable name+instructions rows"
  → "role selects ONLY" (asserts `querySelectorAll("input, textarea")` is
  empty on the whole persona section — `expectNoTextInputs`), "legacy
  name-only persona stays editable in the role-less lane" → renders as an
  unpicked picker and never persists.
- New pins: "Add persona opens the role picker directly" (placeholder row,
  no text inputs, pick → save carries `{roleId, name}` only); "an added row
  left unpicked is NOT persisted" (sends `stagePersonas: {}`); "a stored
  LEGACY entry … is dropped on save" (both tiers — the project-tier leg
  dirties reviewColumn so the save fires).
- The wipe-protection pins (user round-2, project round-1) re-pinned on
  role-bearing stored maps — verbatim carry INCLUDING a stored instructions
  override (the wire shape is untouched).
- The suppression-warning import pin now asserts the import is the reviewer
  ROLE (select label), NO name input / instructions textarea render, and the
  save still carries `roleId` + the mirrored charter prose in the stored
  shape (the editor never shows it — round-2's exact seam).
- The slug-reference pin re-pinned: select-only row (no textareas, no
  per-row note), re-pick saves the new reference.

## Verification

- `npx vitest run src/components/board/BoardConfigPanels.test.tsx` — 28/28.
- Full frontend suite — 224 files, 2571/2571 green.
- `npm run typecheck` (tsc) clean; `npm run lint` 0 errors (7 pre-existing
  warnings: 6 generated-abi eslint-disable directives + 1 BoardPage
  react-hooks dep warning, all present on main; none in the touched files).
- `git status` — exactly the three frontend files (backend/wire untouched).

## Process

Frontend-only worktree off origin/main (979a071d2), conventional commit,
CHANGELOG [Unreleased], this worklog, COORDINATE.md row. PR iterates with
the GitHub AI reviewer until APPROVED — never self-merge.
