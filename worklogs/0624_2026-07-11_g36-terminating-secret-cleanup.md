# Worklog: G36 — Workspace secrets cleaned on deletion

**Date:** 2026-07-11
**Session:** Address threat-model gap G36 (High) — `handleTerminating` deleted only `workspace-pw-*`; `workspace-creds-*` persisted indefinitely after workspace deletion.
**Status:** Complete

---

## Objective

Close G36 from `design/stories/epic-17-security-review/THREAT-MODEL.md`. The workspace controller's graceful-termination path (`controller/internal/workspace/phase_terminating.go:handleTerminating`) deleted the pod, PVC, and `workspace-pw-*` Secret, but NOT `workspace-creds-*`. The latter persisted indefinitely — a credential leak (the Secret carries per-workspace credential material) and a quota cost.

The threat-model row referenced `deleteEphemeralSecretsSecret` which does not exist by that name. The actual primitive is `cleanupFailedWorkspaceSecrets` (`secrets.go:33`), already used in `recovery.go` (3 call sites) for the Failed-phase path. This PR wires it into the graceful-termination path too.

---

## Work Completed

### Implementation

- **`controller/internal/workspace/phase_terminating.go`** — added a single call to `r.cleanupFailedWorkspaceSecrets(ctx, workspace)` after the existing explicit password-secret delete. The explicit delete is retained for clarity (easy to grep, primary path); `cleanupFailedWorkspaceSecrets` is the catch-all that also catches `workspace-creds-*` and any future secret names added to its list. Defense in depth: `workspace-pw-*` is deleted twice (idempotent — Get-then-Delete with continue-on-NotFound).

### Tests

Two new tests in `controller/internal/workspace/phase_terminating_test.go`:

- `TestHandleTerminating_G36_DeletesCredentialsSecret` — G36 core regression. Seeds both `workspace-pw-*` and `workspace-creds-*` Secrets, calls `Reconcile`, asserts both are deleted.
- `TestHandleTerminating_G36_DoesNotDeleteOtherWorkspaceSecrets` — confirms cleanup is workspace-scoped: another workspace's `workspace-creds-*` Secret survives.

### Documentation

- **`CHANGELOG.md`** — entry under `[Unreleased] → Security`.
- **`design/stories/epic-17-security-review/THREAT-MODEL.md`** — G36 row flipped 🔴 → 🟢 Fixed. Threat-model row's stale reference to `deleteEphemeralSecretsSecret` corrected to `cleanupFailedWorkspaceSecrets`. STRIDE `Controller` row updated. Counts: 25 Fixed / 18 Open → 26 Fixed / 17 Open. Revision 2.8 added.

---

## Key Decisions

1. **Call `cleanupFailedWorkspaceSecrets` rather than duplicate its logic.** The primitive already exists, already deletes both `workspace-creds-*` and `workspace-pw-*`, already handles the "Get-then-Delete with continue-on-NotFound" idempotency pattern, and is already tested. Inventing a new helper would duplicate all of that.

2. **Keep the explicit password-secret delete AND call `cleanupFailedWorkspaceSecrets`.** Defense in depth: the explicit delete is the primary (propagates errors), the catch-all is the safety net (logs errors only). The explicit delete is also easier to grep when investigating incidents. The double-delete is idempotent.

3. **Order: `cleanupFailedWorkspaceSecrets` AFTER the explicit password-secret delete, BEFORE the status update.** Mirrors the existing structure. If I'd put it before, the explicit delete would become redundant; if after the status update, a status-update failure would skip the cleanup.

4. **Best-effort, not propagating.** `cleanupFailedWorkspaceSecrets` logs failures but doesn't return them. The workspace is already being torn down; the finalizer must still release even if the Secret API is briefly unavailable. Matches the pattern in `recovery.go:31,60,112` where the same primitive is used for the Failed-phase path.

5. **`handleDeletion` inherits the fix automatically.** It calls `handleTerminating` at `phase_terminating.go:90`. No separate wiring needed.

---

## Assumptions (Rule 7) — stated and validated

| # | Assumption | Validation |
|---|---|---|
| 1 | G36 still open in the codebase | Verified: `phase_terminating.go:40-46` deletes only `workspace-pw-*`; `workspace-creds-*` not deleted anywhere in the termination path. |
| 2 | Threat-model reference to `deleteEphemeralSecretsSecret` is stale | Verified: `grep -rn "deleteEphemeralSecretsSecret" controller/` returned no hits. The actual primitive is `cleanupFailedWorkspaceSecrets`. |
| 3 | `cleanupFailedWorkspaceSecrets` is the right primitive | Verified at `secrets.go:33-48`: deletes both `workspace-creds-*` and `workspace-pw-*`, idempotent, best-effort. Already called from `recovery.go:31,60,112` for the Failed-phase path. |
| 4 | `workspace-secrets-*` no longer exists | Verified at `secrets.go:31`: comment "Epic 35: workspace-secrets-* is no longer created (secretless injection), so it is no longer in the cleanup list." |
| 5 | Idempotent — safe to call even if secrets are absent | Verified by `TestCleanupFailedWorkspaceSecrets_IdempotentWhenAlreadyGone` (existing test) and the Get-then-Delete logic at `secrets.go:39-46`. |
| 6 | `handleDeletion` calls `handleTerminating` | Verified at `phase_terminating.go:90`. |
| 7 | Suspension does NOT trigger handleTerminating | Verified: `phase_suspend.go` deletes only the pod; the workspace CRD survives suspension. `handleTerminating` runs only on actual deletion. |

---

## Adversarial Self-Review (Rule 11)

### Phase 1 — finding candidates

1. Idempotency of cleanupFailedWorkspaceSecrets when secrets are absent.
2. Best-effort vs propagating errors — inconsistent with the explicit password-secret delete above it.
3. Order: cleanupFailedWorkspaceSecrets after explicit password delete — defense in depth or redundant?
4. Does `handleDeletion` (line 84) inherit the fix?
5. Does this affect workspace suspension (a non-terminating deletion of the pod)?

### Phase 2 — validation

| # | Real? | Disposition |
|---|---|---|
| 1 | False alarm — verified by existing idempotency test |
| 2 | Acceptable — different policies for primary (propagating) vs catch-all (best-effort) is intentional; mixing wouldn't improve correctness |
| 3 | False alarm — defense in depth; the explicit delete is the primary, the catch-all is the safety net |
| 4 | Real positive — `handleDeletion` calls `handleTerminating`, so the fix covers both deletion paths |
| 5 | False alarm — suspension is a separate code path (`phase_suspend.go`) that does not call `handleTerminating` |

### Phase 3 — remediation

Zero real findings in the new code.

---

## Blockers

None.

---

## Tests Run

```bash
# Targeted G36 tests
go test -count=1 -timeout 25s -v -run 'TestHandleTerminating_G36' ./controller/internal/workspace/...
# → 2/2 PASS

# All handleTerminating tests (regression check after wiring change)
go test -count=1 -timeout 25s -v -run 'TestHandleTerminating' ./controller/internal/workspace/...
# → 5/5 PASS (3 existing + 2 new)

# Full controller workspace package
go test -count=1 -timeout 50s ./controller/internal/workspace/...
# → PASS

# Full repository test suite
go test -timeout 240s -short ./...
# → 67 packages ok, 0 FAIL

# Build + vet
go build ./...    # exit 0
go vet ./...      # exit 0

# Lint
golangci-lint run --timeout=4m ./controller/internal/workspace/...
# → 0 issues

# Format
gofmt -l <changed files>      # clean
goimports -l <changed files>  # clean
```

---

## Next Steps

1. **Merge this PR**, then move to G28 (workspace bind handler no-op for first-time delivery — the most invasive remaining gap).
2. After G28, the remaining open gaps are: G4 (mTLS), G6 (per-endpoint rate limit on secrets), G9 (opencode binary checksums), G13 (lockout DoS), G21 (sandbox-cfg password mode), G29 (path-traversal API-side mirror), G30 (DNS exfil), G40 (agentd user-port auth), G41-G47 (medium/low), G50 (decrypt audit not wired). The High-severity list will be exhausted after G28.

---

## Files Modified

- `controller/internal/workspace/phase_terminating.go` — call `cleanupFailedWorkspaceSecrets` after explicit password-secret delete
- `controller/internal/workspace/phase_terminating_test.go` — 2 new G36 tests
- `CHANGELOG.md` — entry under `[Unreleased] → Security`
- `design/stories/epic-17-security-review/THREAT-MODEL.md` — G36 row flipped 🟢; STRIDE + counts + revision 2.8
- `worklogs/0624_2026-07-11_g36-terminating-secret-cleanup.md` — this file
