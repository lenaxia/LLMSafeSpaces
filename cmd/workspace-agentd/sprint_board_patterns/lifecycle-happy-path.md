# Lifecycle: the happy path — charter to verified done
<!-- pattern: slug=lifecycle-happy-path; doc=board; use-case=mechanics; nodes=none; wiring=none -->
Description: The full walk of a story that goes right — create, claim, work, in_review with evidence, reviewer APPROVED, done — with the exact tool moves at each stage.

This is the reference lifecycle every other lifecycle file varies on. The
board arbitrates every stage move server-side; your job at each stage is one
or two tool calls, never side-channels.

## 1. The story exists (the writer)

Write the payload file, `board_validate_file` (op `create_task`), then
`board_create_task` `{epicId, bodyFile}`. The description seeds the worker
charter; the criteria define done. The epic must be active (an inactive epic
never feeds stories to workers — `board_epic_activate` is the publish
lever).

## 2. The claim (the engine, not you)

A fresh worker session claims the story FIFO when it is ready (backlog,
unblocked, unpaused). Nobody assigns workers. The claimed worker receives a
charter envelope: the short id, the description, the criteria, and a CLAIM
TOKEN — the in-flight guard that authorizes this worker's writes on this
story.

## 3. The work (the worker)

Do the work the charter describes, following the process discipline
(`process-discipline.md`): branch, commits, `make fmt-check` before every
push, push, open the pull request / produce the artifact. If a BOARD LEASE
NUDGE message arrives, call `board_heartbeat` once with your latest progress
stamp, then keep working.

## 4. The handoff (the worker)

When the work is done:

```sh kind=shell
board_move_stage {toStage: "in_review", claimToken: "<from your charter>",
                  evidenceUrl: "<the artifact: pull request URL, report path, dataset path>"}
```

The `evidenceUrl` is the artifact link for this transition — its expected
shape per work type is in `process-discipline.md`. Attach it and stop
working: review owns the story now.

## 5. The verdict (the reviewer)

A reviewer persona claims the in_review row, checks EVERY acceptance
criterion against the evidence, and records the structured verdict:

```sh kind=shell
board_record_verdict {claimToken: "<the reviewer's>", verdict: "approve",
                      reviewer: "<reviewer identity>", evidenceUrl: "<the verified artifact>"}
```

APPROVE requires the story's evidence URL and ALL criteria met — the Done
guard applies regardless. An APPROVED verdict settles the story to done.

## 6. Done

The story reads `done` with every criterion met and the evidence attached.
The audit timeline (readable from the board UI) carries the whole chain:
create, claim, stage moves, verdict.

## The failure modes that are NOT failures

- **A 409 `claim_token` on any write** means the claim is no longer yours —
  STOP, re-read the story with `board_read`, and do not retry the write.
- **A nudge** is not a scolding — heartbeat once and continue (see step 3).
- **`in_review → in_progress` without findings you can see** means a bounce
  happened; read the story — the findings ride the new charter
  (`lifecycle-fail-bounce.md`).
