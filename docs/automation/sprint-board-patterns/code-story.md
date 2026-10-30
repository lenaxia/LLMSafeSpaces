# Writing code — a story that lands a verified diff
<!-- pattern: slug=code-story; doc=board; use-case=code; nodes=none; wiring=none -->
Description: How to write and drive a code story whose done is provable — the full charter payload, three checkable criteria, and the pull-request evidence the reviewer signs off on.

Audience: you, the agent writing or driving a story on this Sprint Board
(the vocabulary note lives in `process-discipline.md`). A code story is done
when a reviewer can run the verification and watch it pass — not when the
worker says it works. The charter below is written for a fresh worker
session with NO memory of your conversation; the criteria are written so a
reviewer can check each one against an artifact.

## The worked story — "Harden session renewal against expired refresh tokens"

The create is file-only (design 0070): write the payload, validate it,
submit it.

`story-code.json`:

```json kind=board-payload op=create_task
{
  "title": "Harden session renewal against expired refresh tokens",
  "description": "## Background\n\nSessions survive past their refresh token's expiry: the renewal path re-issues an access token without checking the refresh token's `exp`, so a token leaked yesterday still mints access today.\n\n## Scope\n\n- The renewal handler rejects an expired refresh token with HTTP 401 and the error code `refresh_expired` (today it silently succeeds).\n- The rejection is covered by tests: one happy renewal, one expired-token rejection, one malformed-token rejection.\n- No other endpoint changes behavior.\n\n## Where\n\n`api/internal/handlers/session_renewal.go` and its test file. Reproduce first (a failing test), then fix, then keep the test green.\n\n## Process\n\nBranch, commit conventionally, push, open a pull request. `make fmt-check` must pass before every push. Report the pull request URL as evidence.",
  "tags": ["backend", "auth"],
  "acceptanceCriteria": [
    {"text": "An expired refresh token on the renewal endpoint returns 401 with error code refresh_expired, shown in the new test's output."},
    {"text": "The renewal test file covers the happy path, the expired token, and the malformed token — all three named tests pass in the suite run."},
    {"text": "The pull request changes no files outside the renewal handler and its test file (the diff's file list shows exactly two files)."}
  ]
}
```

Then:

1. `board_validate_file` (op `create_task`) on the file — fix any findings
   it reports before submitting.
2. `board_create_task` with `{epicId: "epc-…", bodyFile: "story-code.json"}`
   — the file is consumed on success.

## Why each criterion holds

- Criterion 1 names the observable: the endpoint, the status, the error
  code, and where the reviewer reads it (the test output). "Renewal is more
  secure" cannot be checked; this is a run-and-read check.
- Criterion 2 enumerates the coverage — the reviewer counts the three named
  tests in the diff and watches them pass in the suite run.
- Criterion 3 bounds the blast radius by artifact: the reviewer reads the
  diff's file list and nothing else.

## Evidence shape

A code story's evidence is **the pull request URL plus the passing run of
the suite** — the diff (what changed) and the verification outcome (the
suite green, `make fmt-check` clean). The worker attaches the pull request
URL as `evidenceUrl` when moving the story to `in_review`; the reviewer
re-runs the suite on the branch before recording a verdict. Artifacts and
outcomes only — whatever forge and runner this workspace uses, the evidence
reads the same.
