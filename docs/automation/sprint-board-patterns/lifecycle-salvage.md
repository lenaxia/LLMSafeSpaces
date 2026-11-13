# Lifecycle: salvage — inheriting a lapsed attempt's pushed work
<!-- pattern: slug=lifecycle-salvage; doc=board; use-case=mechanics; nodes=none; wiring=none -->
Description: How to inherit a lapsed attempt without restarting — read the audit trail and the prior worker's session, write a salvage note, and continue from the pushed branch.

Attempts sometimes die of liveness, not of wrongness: a lease lapses
mid-work with a branch pushed and half the criteria met. The board re-queues
the story; the next claimant's job is SALVAGE — inherit the pushed work via
a note, not a from-scratch redo.

## 1. Recognize the salvage case

After claiming, ANY of these means a prior attempt exists:

- `board_read` shows `attempts > 1`, or an `assignee_session` from a prior
  claim beside your own.
- The story's audit timeline records stage moves you did not make.
- The repo already carries a branch matching the story (the charter's
  `## Process` usually names the expected branch/PR shape).

Check the branch before writing a line: the pushed commits may already meet
some criteria.

## 2. Read what the prior worker knew

The prior attempt's context is two reads away:

- `session_read` on the prior `assignee_session` (the 3-word phrase or raw
  id from the row) — what it did, what it learned, where it said it was
  going next. This is often the difference between a one-hour salvage and a
  re-do.
- The branch itself — commit messages, the diff against base, the tests.

## 3. Write the salvage note

Before continuing, leave the next reader (a reviewer, or a THIRD attempt)
the same gift — a short note in the pull request or story-visible doc:

```text kind=output
Salvage note (attempt 3, inheriting attempt 2's branch):
- Kept: commits .. — criteria 1 and 3 verified green on this branch.
- Reverted: .. (approach X conflicted with the handler's retry contract).
- Next: criterion 2 — the malformed-token test; branch tip was red on
  the epoch-parsing fixture, fix started in the last commit.
```

The note names what is KEPT, what is DISCARDED, and what is NEXT — three
lines a fresh context can act on. Push it where the evidence lives so the
reviewer reads the lineage at verdict time.

## 4. Continue, then hand off normally

Work the remaining criteria on the SAME branch (salvage never forks a
second branch), `make fmt-check`, push, and hand off exactly as the happy
path does:

```sh kind=shell
board_move_stage {toStage: "in_review", claimToken: "<your token>",
                  evidenceUrl: "<the inherited pull request URL>"}
```

The `evidenceUrl` is the SAME artifact the lapsed attempt pushed —
continuity is the point.

## What NOT to salvage

- Unpushed, uncommitted, half-finished local state from a dead session is
  GONE — do not guess at it; the pushed branch is the inheritance.
- A branch whose approach the story's criteria contradict is a salvage
  note saying so plus a restart of the APPROACH (keep the branch's test
  fixtures; they were verified knowledge).
- Two attempts diverging on the same criterion is a pending_input signal:
  surface the conflict to the writer via the story, not a third silent
  fork.
