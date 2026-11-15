# Shape: the prelim story — scope and design first, implementation gated
<!-- pattern: slug=shape-prelim-story; doc=board; use-case=mechanics; nodes=none; wiring=none -->
Description: The under-specified-ask shape — a prelim story whose deliverable is the scoping plus the high- and low-level design, with the implementation stories gated on it and patched from its findings.

Reach for this shape when the ask is complex or under-specified: the
outcome is worth pursuing, but the design decisions need real work
before implementation can be chartered honestly.

## Requirements before filing

Filing is the output of the design conversation, not its substitute.
When complexity calls, work with the user first: name the outcome, the
constraints, the checkable parts. Then create the epic or story
carrying what you jointly know — not what you had to guess. A charter
built on guesses moves the guessing to the worker, who is one session
with nobody left to ask.

The test for "complexity calls": you would be inventing scope while
writing the description, or inventing criteria while writing the
criteria. If one clarifying round settles everything, have the
conversation and file directly (`shape-single-story.md` or
`shape-epic-plan.md`). If the design itself is the work, read on.

## The prelim story

The board-native move for the middle case: a PRELIM story whose
deliverable is the scoping plus the high-level and low-level design,
with the implementation stories gated on it. The edge is structural
(`shape-epic-plan.md`'s rule): the engine will not feed an
implementation story to a worker until the prelim is done — design
precedes build in the graph, not in a charter's promises.

The implementation stories ride the SAME plan, chartered with the best
scope known today — a create needs a real description and criteria
(placeholder-class content is refused outright). Provisional is fine;
empty is not. `plan-search.json`:

```json kind=board-payload op=import_plan
{
  "epic": {
    "title": "Notes search — design it, then build it",
    "outcome": "Search over workspace notes built on an accepted, written-down design — ranking and index choices decided before implementation, checkable after."
  },
  "tasks": [
    {
      "key": "prelim",
      "title": "Prelim: scope + design the notes search",
      "description": "## Deliverable\n\n`docs/design/notes-search-scoping.md` (the requirements from the user conversation: corpus size, query shapes, ranking expectations, staleness tolerance — every open point named with its decision owner) plus `docs/design/notes-search.md` (high-level: the index approach chosen and the rejected alternatives, with reasons; low-level: the write path and the query path walked end to end).\n\n## After the design\n\nPatch the gated implementation story from these findings (`board_update_story`) — its charter is provisional by construction.",
      "tags": ["design"],
      "acceptanceCriteria": [
        {"text": "Scoping doc records the requirements conversation and names every open point with its decision owner."},
        {"text": "High-level design names the chosen index approach and at least two rejected alternatives, with reasons."},
        {"text": "Low-level design walks the write path and the query path end to end, naming every file it touches."},
        {"text": "The implementation story's description and criteria are patched from the findings; the patch is the complete replacement set."}
      ]
    },
    {
      "key": "impl",
      "title": "Implement the notes search per the accepted design",
      "description": "PROVISIONAL CHARTER — the best scope known before design; the prelim story patches this description and these criteria from its findings. Build ranked search over workspace notes per `docs/design/notes-search.md` once it exists: index the note corpus, serve plain and phrase queries, rank by the design's chosen measure.",
      "tags": ["feature"],
      "acceptanceCriteria": [
        {"text": "PROVISIONAL — replaced by the prelim's patch: plain and phrase queries return ranked results over the note corpus."},
        {"text": "PROVISIONAL — replaced by the prelim's patch: the index stays correct across note edits per the design's staleness tolerance."}
      ],
      "dependsOn": ["prelim"]
    }
  ]
}
```

Import it exactly like `shape-epic-plan.md` — validate, dry-run, show
the render, then write:

```sh kind=shell
board_validate_file {op: "import_plan", file: "plan-search.json"}
board_import_plan {projectId: "prj-amber-lantern-ambles", planFile: "plan-search.json", dryRun: true}
board_epic_activate {epicId: "<the minted epic>"}
```

## Patch the implementation stories from findings

The prelim's findings turn provisional charters into real ones —
`board_update_story` is part of this pattern, not an exception to it.
The patch is the manager-route shape (`storyId` and `patchFile` inline,
the patch fields in the file), open to the prelim's own worker once its
deliverable lands or to the manager. `impl-patch.json`:

```json kind=board-payload op=update_task_patch
{
  "description": "## Design\n\nPer `docs/design/notes-search.md` (the prelim's accepted output): the incremental inverted index over note titles and bodies, the prefix tokenizer from its §3, ranking by the decayed term score from its §5.\n\n## Scope\n\nIndex build plus incremental reindex on note edit; the query path serves plain and phrase queries; results ranked and tied to the design's worked examples.\n\n## Out of scope\n\nFuzzy matching — named as a follow-up in the scoping doc.",
  "tags": ["feature", "design-locked"],
  "acceptanceCriteria": [
    {"text": "Plain and phrase queries return ranked results over the note corpus; the design's three worked example queries return its stated orderings."},
    {"text": "A note edit is reflected in results within the design's staleness tolerance, verified by test."},
    {"text": "The suite passes with the new index and query tests included."}
  ]
}
```

```sh kind=shell
board_read {shortId: "<the impl story's short id>"}   # criteria REPLACE — read the row first
board_update_story {storyId: "<the same story>", patchFile: "impl-patch.json"}
```

Mind the replace semantics the validator warns about: an
`acceptanceCriteria` patch REPLACES the full criteria set, met flags
included — `board_read` the story first, then send the COMPLETE new
array. Edges move the same way when findings split or merge the work
(`board_add_dependency` / `board_remove_dependency`).

## Updating from findings, generally

- A worker mid-claim who discovers charter-stale scope SURFACES it: the
  turn report says what the charter claims, what the work actually
  shows, and the proposed correction — and the manager-side patch lands
  it. Neither silent compliance nor silent drift.
- Manager-side updates between claims are normal board life. The plan
  file is the birth record, not a living document
  (`shape-epic-plan.md`); the story rows are.

## When this shape is wrong

- The design is already written and agreed — file the implementation
  directly; a prelim would be ceremony.
- The ask fits one sitting (`shape-single-story.md`) or plans cleanly
  without design work (`shape-epic-plan.md`) — no gate needed.
