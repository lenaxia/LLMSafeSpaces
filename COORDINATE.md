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
| opencode (g-audit) | Reclassify stale gaps (G29/G45/G50→Fixed; G4/G30/G40→Accepted) + docs reconciliation | `design/stories/epic-17-security-review/THREAT-MODEL.md`, `CHANGELOG.md`, `README-LLM.md` | In Progress | 2026-07-11 |
| opencode (g28) | G28 — reclassify as Accepted (architecture changed in Epic 35) + invariant test | `design/stories/epic-17-security-review/THREAT-MODEL.md`, `pkg/secrets/secret_service_test.go` | In Progress | 2026-07-11 |
| opencode (g36) | G36 — workspace secrets cleanup on deletion | `controller/internal/workspace/phase_terminating.go`, `controller/internal/workspace/phase_terminating_test.go` | In Progress | 2026-07-11 |
| opencode (g25) | G25 — secret value field logged unredacted | `api/internal/middleware/logging.go`, `api/internal/middleware/tests/logging_test.go`, `api/internal/server/router.go` | In Progress | 2026-07-11 |


---

## Pending Claims

Agents waiting to work on files currently held by an active claim. When the blocking claim is released, move your row to Active Claims.

| Agent | Waiting For | What They Plan To Do | Files Wanted |
|-------|-------------|----------------------|--------------|

---

## Recently Completed (last 10)

| Completed | Agent | What | Commit |
|-----------|-------|------|--------|
| 2026-07-11 | opencode (g28) | G28 — reclassify as Accepted + invariant test (PR [#541](https://github.com/lenaxia/LLMSafeSpaces/pull/541), pending review) | `7518ecf1` |
| 2026-07-11 | opencode (g36) | G36 — workspace secrets cleanup on deletion (PR [#540](https://github.com/lenaxia/LLMSafeSpaces/pull/540), merged) | `f3043835` |
| 2026-07-11 | opencode (g25) | G25 — secret value field logging (PR [#539](https://github.com/lenaxia/LLMSafeSpaces/pull/539), merged) | `4370c44b` |
| 2026-07-11 | opencode (g35) | G35 — /account/recover per-route rate limit (PR [#538](https://github.com/lenaxia/LLMSafeSpaces/pull/538), merged) | `6fddeecd` |
| 2026-07-11 | opencode (g37) | G37 — workspace env-var name blocklist (PR [#537](https://github.com/lenaxia/LLMSafeSpaces/pull/537), merged) | `be063b9c` |
| 2026-07-11 | opencode (g38) | G38 — ChangePassword revokes all sessions (PR [#536](https://github.com/lenaxia/LLMSafeSpaces/pull/536), merged) | `5968d8dc` |

> Entries older than ~2 weeks are pruned — see `worklogs/` for the historical record.

---

## Known Conflicts / Merge Notes

(None currently active.)

---

## Pending Work (unclaimed)

See `design/stories/README.md` for the authoritative epic/story status and
recommended implementation order. High-value open items are tracked there with
verified gaps per epic.
