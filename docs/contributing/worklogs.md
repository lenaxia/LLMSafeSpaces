# Worklogs

Worklogs are **mandatory**. They are the institutional memory of this project. Every meaningful session produces a worklog entry. This is not optional.

LLMSafeSpaces is built with significant LLM assistance. Each agent session starts with a fresh context window — there is no carried-over memory between sessions except what is written down. Worklogs are how the project remembers what was done, why, what was decided, what's blocked, and what comes next.

## Why worklogs exist

- **Institutional memory for an LLM-built project.** The next session (human or AI) has no memory of this one. The worklog is the bridge.
- **Auditability.** Every change is traceable to a session, a decision, and a rationale.
- **Onboarding.** A new contributor (human or AI) can read recent worklogs to understand the current state of an area without re-reading the entire history.
- **Assumption tracking.** [Rule 7](rules.md#rule-7-assumptions-state-then-validate) requires assumptions to be stated and validated. The worklog is where they're recorded.
- **Review evidence.** [Rule 11](rules.md#rule-11-adversarial-self-review) requires adversarial findings and their validation to be documented. The worklog is where they live.

## When to write a worklog

Write a worklog entry after **any** of the following:

- Completing a user story or part of one
- Making an architectural decision
- Discovering a bug or unexpected behavior
- Completing a design document
- Running into a blocker
- Starting or finishing a feature branch
- Any session longer than 30 minutes of work

!!! tip "If in doubt, write the worklog"
    The cost of writing a worklog you didn't strictly need is a few minutes. The cost of *not* writing one you did need is a lost session that the next contributor has to reconstruct from git history. Always err on the side of writing.

## File naming

```
NNNN_YYYY-MM-DD_short-description.md
```

- `NNNN` is a **literal sentinel placeholder** — do not pick a number yourself.
- Date is the actual date the work was done (`YYYY-MM-DD`).
- Description is lowercase, hyphen-separated, 3–6 words.
- The pre-commit hook blocks new worklogs that don't match the `NNNN_` prefix.

Examples:

```
NNNN_2026-05-01_initial-project-setup.md
NNNN_2026-05-02_api-service-foundation.md
NNNN_2026-05-03_controller-tdd-sandbox.md
```

### Why sentinels (not manual numbering)

After merge, a post-merge bot renames the file with the real sequential number and comments the assigned number on the merged PR:

```
0545_2026-05-01_initial-project-setup.md
0546_2026-05-02_api-service-foundation.md
0547_2026-05-03_controller-tdd-sandbox.md
```

Picking numbers manually races under concurrent PRs: two branches both observe `max=543`, both pick `544`, and collide on merge. The sentinel scheme eliminates the race — the bot assigns numbers atomically at merge time, serialized by GitHub's sequential merge-commit ordering.

## Format

Every worklog entry must follow this exact structure:

```markdown
# Worklog: <Short Title>

**Date:** YYYY-MM-DD
**Session:** <brief description of what this session was about>
**Status:** Complete | In Progress | Blocked

---

## Objective

What was the goal of this session?

---

## Work Completed

<Per-session entries — one ### subsection per logical unit of work>

---

## Key Decisions

List any decisions made and the rationale behind them. If a decision was
made without enough information, note that and flag it for follow-up.

---

## Blockers

List anything that is blocking progress. Include what information or action
is needed to unblock. If none, write "None."

---

## Tests Run

List test commands run and their outcomes. If no tests were run, explain why.

---

## Next Steps

What should the next session start with? Be specific enough that a fresh
context can pick up immediately without re-reading everything.

---

## Files Modified

List every file created or modified in this session.
```

### Section guidance

| Section | What goes here | Common mistake |
|---------|----------------|----------------|
| **Objective** | The goal of the session, in one or two sentences | Vague goals ("work on controller") |
| **Work Completed** | One `###` subsection per logical unit of work, naming functions/files/line numbers | Bullet points with no specificity |
| **Key Decisions** | Decisions + rationale; flag decisions made without enough info | Listing decisions without the *why* |
| **Blockers** | What's blocking, and what's needed to unblock | Silently skipping the entry when blocked |
| **Tests Run** | Exact commands + outcomes (pass/fail); "none" with a reason if no tests | "Tests passed" with no commands |
| **Next Steps** | Actionable, specific enough for a fresh context to pick up | "Continue implementation" (not actionable) |
| **Files Modified** | Every file created or modified | Omitting files (makes session audit hard) |

## Discipline rules

1. **Write it before ending the session** — not the next day. Memory degrades fast.
2. **Be specific** — vague entries like "worked on controller" are useless. Name the functions, the decisions, the line numbers if relevant.
3. **Document decisions with rationale** — not just what was decided, but *why*. Future sessions will need to understand the reasoning, not just the outcome.
4. **Record blockers immediately** — if you are blocked, write it down. Do not silently skip the entry.
5. **List every file touched** — this makes it trivial to audit what changed in a session.
6. **Next steps must be actionable** — "continue implementation" is not actionable. "Implement `CreateSandbox()` in `pkg/secrets/secret_service.go` and write tests first per TDD" is actionable.
7. **Never retroactively rewrite a worklog** — worklogs are append-only history. If something was wrong, note the correction in the next entry.

!!! warning "Worklogs are append-only"
    Never rewrite a past worklog to "fix" it. If a decision was wrong, or a finding turned out to be a false alarm, or an assumption was disproved, write the correction in the **next** worklog entry. Rewriting history destroys the audit trail that makes worklogs valuable.

## Worklogs and the rules

Worklogs are where several engineering rules leave their evidence:

- **[Rule 7 (Assumptions)](rules.md#rule-7-assumptions-state-then-validate)** — assumptions are stated and their validation results recorded in the worklog.
- **[Rule 11 (Adversarial Self-Review)](rules.md#rule-11-adversarial-self-review)** — adversarial findings, their validation, and false-alarm rationales go in the worklog.
- **[Rule 0 (TDD)](rules.md#rule-0-test-driven-development-tdd)** — the exact test commands and their outcomes are recorded under "Tests Run."

A worklog that omits assumptions, review findings, or test evidence is incomplete.

## Example

```markdown
# Worklog: Add workspace refresh-compute endpoint

**Date:** 2026-06-15
**Session:** Implement POST /workspaces/:id/refresh-compute (re-sync resource defaults + rebuild pod)
**Status:** Complete

---

## Objective

Add an endpoint that re-syncs a workspace's resource defaults (CPU, memory,
security level, storage class) with the platform's current configuration and
bumps spec.restartGeneration so the controller rebuilds the pod with the
latest runtime image version.

## Work Completed

### API
- Added `RefreshWorkspaceCompute` to `WorkspaceService`
  (`api/internal/services/workspace/workspace_service.go:412`)
- Registered `POST /:id/refresh-compute` on `idGroup`
  (`api/internal/server/router.go:1057`)
- Returns `{restartGeneration: <int>}` (202 Accepted)

### Controller
- No change — the controller already observes `spec.restartGeneration`
  bumps and rebuilds the pod.

## Key Decisions

- **Return restartGeneration, not a full workspace object.** The caller
  needs to know the new generation to detect when the rebuild completes.
  Assumption: restartGeneration is monotonic — verified via
  `controller/internal/workspace/pod_builder.go:88`.

## Blockers

None.

## Tests Run

- `go test -timeout 90s -race ./api/internal/services/workspace/...` — PASS
- `go test -timeout 90s -race ./api/internal/server/...` — PASS
- `./local/test.sh` (e2e) — PASS (9/9)

## Next Steps

- Frontend: wire a "Refresh compute" button on the workspace detail page.
- Consider surfacing the restartGeneration in the workspace status response
  so the frontend can poll for rebuild completion.

## Files Modified

- api/internal/services/workspace/workspace_service.go
- api/internal/server/router.go
- api/internal/services/workspace/workspace_service_test.go
- api/internal/server/router_workspace_test.go
- pkg/types/types.go (RefreshWorkspaceResult)
```

## Next

- [Engineering Rules](rules.md) — the rules worklogs support (0, 7, 11)
- [Contributing overview](index.md) — where worklogs fit in the PR workflow
- [Development Workflow](development.md) — the sessions worklogs document
