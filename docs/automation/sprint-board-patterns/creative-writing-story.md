# Creative writing — a story whose review is an editorial read
<!-- pattern: slug=creative-writing-story; doc=board; use-case=creative-writing; nodes=none; wiring=none -->
Description: How to write a creative-writing story with checkable craft constraints — outline beats hit, voice consistent with prior chapters, the planned hook landed.

Creative work is checkable when the criteria name the CRAFT constraints, not
the quality. "Make it gripping" cannot be verified; "the chapter ends on the
planned hook" can. Quality lives inside the constraints — the review pass is
an editorial read against them.

## The worked story — "Draft chapter 7 (target ~2,500 words)"

`story-chapter7.json`:

```json kind=board-payload op=create_task
{
  "title": "Draft chapter 7 — 'The lockkeeper's ledger' (target ~2,500 words)",
  "description": "## Where the series stands\n\nChapters 1–6 live in `book/manuscript/` (chapter-1.md … chapter-6.md); the series outline is `book/outline.md`. Chapter 6 ended with Mara holding the forged ledger, undecided whether to burn it.\n\n## This chapter's beats (from the outline, §7)\n\n1. Mara crosses the estuary at first light, ledger hidden in the bait crate.\n2. The lockkeeper recognizes his own handwriting in a forged entry — and says nothing.\n3. Mara spends the ledger's last page to buy passage, and understands too late what she paid.\n4. Hook: the ferryman's manifest carries her real name.\n\n## Voice and continuity\n\nRead chapters 5 and 6 before drafting: the narration stays close-third on Mara, present tense, sentences shortening when she is afraid. The lockkeeper speaks in ledgers and counts (established chapter 3).\n\n## Deliverable\n\n`book/manuscript/chapter-7.md`, ~2,500 words (2,200–2,800 acceptable), beat structure preserved.",
  "tags": ["book", "draft"],
  "acceptanceCriteria": [
    {"text": "The draft hits all four outline beats in order, each findable by a reader skimming for it (beat 4 — the manifest with Mara's real name — is the chapter's last scene)."},
    {"text": "The voice check passes: close-third present tense on Mara throughout, no point-of-view breaks, and the lockkeeper's dialogue keeps the ledgers-and-counts register from chapter 3."},
    {"text": "The word count lands in the 2,200–2,800 band, stated in the draft's header line."}
  ]
}
```

Validate with `board_validate_file` (op `create_task`), then submit with
`board_create_task` `{epicId, bodyFile}`.

## Why each criterion holds

- Criterion 1 checks STRUCTURE, not taste: four beats, in order, findable.
  A reviewer who has read the outline can verify this objectively.
- Criterion 2 checks VOICE as a pair of concrete signals (person/tense and
  an established dialogue register) — checkable against chapters 3, 5, and 6
  which the charter names.
- Criterion 3 checks LENGTH, a number. "~2,500 words" alone would leave the
  reviewer guessing; the band plus the header line makes it a read-and-count
  check.

## Evidence shape

A creative-writing story's evidence is **the draft document** — the
manuscript file itself. The worker attaches its path/URL as `evidenceUrl`
when moving the story to `in_review`; the review pass is an editorial read:
the reviewer checks each criterion against the draft and the named prior
chapters, then records the verdict. Editorial FEEDBACK beyond the criteria
rides the verdict's findings as minor items; only criterion violations
block.
