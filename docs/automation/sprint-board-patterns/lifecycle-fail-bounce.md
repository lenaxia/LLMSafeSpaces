# Lifecycle: the fail-bounce — findings arrive, remediation iterates on the same PR
<!-- pattern: slug=lifecycle-fail-bounce; doc=board; use-case=mechanics; nodes=none; wiring=none -->
Description: What happens when the reviewer records a fail verdict — the bounce re-arms the story, findings ride the new charter, and remediation iterates on the SAME pull request rather than starting over.

A fail verdict is a steering event, not a demolition. The work already
pushed stays; the findings tell the next pass exactly what to change.

## 1. The verdict that bounces

The reviewer records:

```sh kind=shell
board_record_verdict {claimToken: "<reviewer's>", verdict: "fail",
                      reviewer: "<reviewer identity>",
                      findings: [
                        {severity: "blocker",
                         pointer: "acceptance criterion 2 — the malformed-token test never asserts the 401 body",
                         criterionIndex: 1},
                        {severity: "minor",
                         pointer: "commit message lacks the conventional prefix"}
                      ]}
```

The story bounces `in_review → in_progress`: the row re-arms a fresh claim
and the engine feeds it to a worker (often a fresh session — the bounce is
how findings reach someone with full context budget).

## 2. The remediation charter carries the findings

The next claimant's charter is the ORIGINAL description plus the reviewer's
findings embedded verbatim. Read them first — they are the diff between
"what was asked" and "what was delivered":

- A `blocker` finding maps to an acceptance criterion (`criterionIndex`) —
  remediation is not done until that criterion actually holds.
- A `major` finding is real work that missed the criteria's letter — fix it
  and consider whether the story's criteria should have caught it.
- A `minor` finding rides along — cheap to satisfy in the same pass.

## 3. Iterate on the SAME branch and pull request

Do NOT open a second pull request. The evidence chain, the review history,
and the reviewer's bearings all live on the first one:

1. `board_read` the story — confirm the findings and which criteria the
   reviewer left unmet.
2. Continue on the existing branch: new commits addressing each finding,
   conventional commit messages saying so (`fix: address review finding —
   assert the 401 body's error code`).
3. `make fmt-check`, push. The pull request updates in place — that IS the
   iteration; the story's `evidenceUrl` does not change.
4. Re-verify the criteria yourself before handing back — the reviewer will
   re-check every one, including the ones that passed last time.

## 4. Hand back

```sh kind=shell
board_move_stage {toStage: "in_review", claimToken: "<your token>",
                  evidenceUrl: "<SAME pull request URL>"}
```

The same or another reviewer claims the row, re-checks, and records the
next verdict. Repeat until APPROVED — each bounce narrows the gap; a
bounce is never a reason to abandon the branch (that IS restarting, the
thing the bounce exists to avoid).

## When the findings are wrong

If a finding misreads the work, do not litigate in code: satisfy the
criterion the finding points at in the cheapest correct way, and note the
disagreement in the pull request where the reviewer will read it. If a
criterion itself is wrong, that is a story PATCH (`board_update_task`) —
rare, and visible in the audit timeline.
