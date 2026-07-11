# COORDINATE.md — Multi-Agent Work Coordination

This file is the source of truth for what work is in-flight across all agents.
**Before starting any work: read this file. After finishing any work: update this file and commit it.**

Rules:
- Claim a section before touching its files. If it's claimed by another agent, wait or pick different work.
- Keep claims specific (file paths, not vague areas).
- Mark work DONE immediately when finished — do not batch updates.
- If you abandon work, release the claim so another agent can pick it up.
- Always git pull before starting work. Always commit COORDINATE.md with your work commits.
- To queue behind a current claim, add a row to **Pending Claims**. When the blocking claim is released, move your row to Active Claims.

---

## Active Claims

| Agent | What | Files Claimed | Status | Started |
|-------|------|---------------|--------|---------|
| opencode (g37) | G37 — workspace env-var name blocklist | `pkg/validation/name.go`, `pkg/validation/name_test.go`, `pkg/validation/env.go` (new), `pkg/validation/env_test.go` (new), `api/internal/handlers/workspace_env.go`, `api/internal/handlers/workspace_env_test.go`, `pkg/agentd/secrets/secrets.go` | In Progress | 2026-07-11 |


---

## Pending Claims

Agents waiting to work on files currently held by an active claim. When the blocking claim is released, move your row to Active Claims.

| Agent | Waiting For | What They Plan To Do | Files Wanted |
|-------|-------------|----------------------|--------------|

---

## Recently Completed (last 10)

| Completed | Agent | What | Commit |
|-----------|-------|------|--------|
| 2026-07-11 | opencode (g37) | G37 — workspace env-var name blocklist (PR [#537](https://github.com/lenaxia/LLMSafeSpaces/pull/537), pending review) | `be063b9c` |
| 2026-07-11 | opencode (g38) | G38 — ChangePassword revokes all sessions (PR [#536](https://github.com/lenaxia/LLMSafeSpaces/pull/536), pending review) | `5968d8dc` |

> Entries older than ~2 weeks are pruned — see `worklogs/` for the historical record.

---

## Known Conflicts / Merge Notes

(None currently active.)

---

## Pending Work (unclaimed)

See `design/stories/README.md` for the authoritative epic/story status and
recommended implementation order. High-value open items are tracked there with
verified gaps per epic.
