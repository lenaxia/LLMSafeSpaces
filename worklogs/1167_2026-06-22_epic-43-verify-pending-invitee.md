# Worklog: Verify-User on Pending Invitations (epic-43 follow-up to PR #343)

**Date:** 2026-06-22
**Session:** Close the gap surfaced during in-cluster validation of PR #343: the existing "Verify" surface only acted on already-accepted members, but org admins also need to override the email-verification gate for invitees who already have a users row but never completed verification.
**Status:** Complete — backend handler + frontend Verify button on pending invitations, 14 backend tests, 5 frontend tests.

---

## Objective

PR #343 added a `Verify` button to the org admin Members table to bypass `users.email_verified` for already-accepted members. Field validation revealed a related gap: when an admin invites a user whose email is `mikekao@outlook.com` and that user already has a platform account but never completed email verification, the admin had no way to bypass the verification — the invitee couldn't accept the invitation (verification gate) and the admin couldn't verify them (no Verify button on pending invitations).

This session adds a Verify button to the **Pending Invitations** table that flips `users.email_verified=true` on the existing users row, while leaving the invitation row pending so the user still goes through the normal acceptance flow (matching the user's spec: "if the user already exists, but they have not verified their account, we should have the verify override option").

---

## Work Completed

### Backend (Go)

#### Interface and handler

- `api/internal/handlers/invitations.go`:
  - Extended the handler-local `invitationStore` interface with three new methods: `GetUserIDByEmail` (resolves invite email → existing users row, or "" miss), `MarkUserEmailVerified` (idempotent UPDATE), and `LogOrgEvent` (audit trail). All three already exist on `PgOrgStore` (added by PR #343 / the broader org-admin surface), so production wiring is automatic.
  - Added `(*InvitationsHandler).VerifyUserForInvitation` handling `POST /api/v1/orgs/:id/invitations/:invID/verify-user`. Look up the invitation by ID, confirm it belongs to this org (cross-org invitations return 404 to avoid leaking existence), confirm it's still pending (already-accepted/declined → 409, expired → 410), resolve the email to a users row, and if found, MarkUserEmailVerified + audit. If no users row exists, return 422 with `{"error":"no_account_for_email"}` so the frontend can render a clear actionable message.
  - Audit event uses action `invitation.verify_user` (distinct from `member.verify` to keep audit-log queries clean) with metadata `{"email": <invite email>, "invitationID": <invID>}`. Audit-emission failures are non-fatal but surfaced via `h.logger.Warn(...)` (matches `OrgsHandler.VerifyMember`'s pattern from PR #343).

#### Route registration

- `api/internal/server/router.go`: registered the new endpoint under `orgAdminGroup` (already guarded by `OrgAdminGuard`):
  ```go
  orgAdminGroup.POST("/invitations/:invID/verify-user", invH.VerifyUserForInvitation)
  ```

### Frontend (TypeScript / React)

- `frontend/src/api/orgs.ts`: added `orgsApi.verifyInvitee(orgId, invId)` calling the new endpoint.
- `frontend/src/components/org-admin/OrgMembersTab.tsx`: added a `Verify` button to each row of the Pending Invitations table (alongside `Resend` and `Revoke`). Click handler:
  - On 200: clear any prior error message, refresh the table.
  - On 422 with body `{"error":"no_account_for_email"}` (detected via `ApiClientError.body.error`): render the user-visible message "No account exists for this email yet. The invitee must sign up before you can verify them." rather than the raw machine code.
  - On any other error: render `e.message` or fallback "Verify failed".

### Tests

#### Backend (14 tests in `invitations_test.go`)

Extended `mockInvitationStore` with `usersByEmail` map, `markVerifiedCalls`, `auditEvents`, plus error-injection fields. Reuses `mockAuditEvent` from `orgs_test.go` (same package).

| Test | Pin |
|---|---|
| `Success_ExistingUser` | Headline path — invitation pending, user exists, MarkUserEmailVerified called with resolved userID, audit event emitted with action `invitation.verify_user` and metadata `{email, invitationID}` |
| `NoUserAccount` | 422 + machine code `no_account_for_email`; no DB writes, no audit |
| `EmailNormalization` | Lookup case-folds `INVITEE@Example.COM` → `invitee@example.com` |
| `Idempotent` | Two consecutive calls both succeed; both reach MarkUserEmailVerified (DB-level idempotent) and emit an audit event each time |
| `InvitationNotFound` | 404; no DB writes, no audit |
| `CrossOrgIsolation` | 404 (NOT 403) when invID exists but belongs to another org |
| `AlreadyAccepted` | 409 — use member.verify on the resulting member instead |
| `AlreadyDeclined` | 409 |
| `Expired` | 410, matches Accept's behavior |
| `GetUserIDError` | 500 on DB error during lookup |
| `MarkVerifiedError` | 500 on UPDATE failure; no audit event (verification didn't happen) |
| `AuditFailureNonFatal` | 200 — audit failure does NOT undo verification; warn logged via `invLogCapture` |
| `GetInvitationByIDError` | 500 — DB error during the invitation lookup is a distinct path from `inv == nil` (404). Reviewer-flagged missing branch from PR #352. |
| `NilLogger_DoesNotPanic` | 200 + audit-failure path — the nil-logger guard tolerates the (defense-in-depth) case where a caller wires `NewInvitationsHandler(..., nil)`. Reviewer-flagged missing branch from PR #352. |

A small `invLogCapture` test type (Warn-counting + Error-counting) was added because the existing `warnCaptureLogger` in `orgs_test.go` only satisfies the smaller `OrgsHandler` logger interface (Warn-only, no Error), while `invitationLogger` requires both.

#### Frontend (5 tests added to `OrgMembersTab.test.tsx`)

| Test | Pin |
|---|---|
| `renders a Verify button on each pending invitation row (admin)` | Members table has its own Verify button for unverified accepted members; Pending Invitations adds another. Total of two on screen confirms both surfaces. |
| `calls verifyInvitee(orgId, invId) when clicked` | API call is made with the right args; refresh fires after success |
| `renders a clear 'must sign up first' message on 422 no_account_for_email` | The frontend translates the machine code into human guidance instead of leaking it |
| `falls through to a generic error message on non-422 failures` | Other errors preserve `e.message` |
| `hides the pending-invitation Verify button for non-admins` | The Pending Invitations section itself is admin-only; non-admins see neither the section nor the buttons |

Existing test file extended with `mockVerifyInvitee`, `mockResendInvitation`, `mockRevokeInvitation` mocks plus a `PENDING_INVITATION` fixture.

---

## Key Decisions

1. **The endpoint targets the invitation, not the user.** Path is `/orgs/:id/invitations/:invID/verify-user`, not `/orgs/:id/users/:userID/verify`. The admin's mental model is "I see this pending invitation in the table; I want to verify the person it was sent to." The backend internally resolves the email to a userID and acts on the user row, but the request shape is invitation-centric. Cross-org isolation also falls out naturally — an admin of org A cannot probe an invitation in org B because the cross-org check happens before the email lookup.

2. **422 with machine-parseable error code, not 200 with a flag, for the "no users row" case.** A 200 + `{"verified": false, "reason": "no_account"}` would let the frontend keep its happy-path code path — but it would also conflate "we did the thing" with "we did nothing because nothing to do," which is a known UX trap that makes reasoning about success harder. 422 + `{"error":"no_account_for_email"}` makes the frontend's handling explicit, supports a clear error message, and matches the existing `ApiClientError` flow used elsewhere (e.g. the rate-limit 429 path in `InviteForm`).

3. **Distinct audit action `invitation.verify_user`.** The existing `member.verify` action is used by `OrgsHandler.VerifyMember` (acts on org_memberships); using the same action name from a different surface would make audit-log queries ambiguous (was this from the Members table? the Pending Invitations table?). Distinct action names keep the trail readable. Both events also carry `email` in metadata; the new event additionally carries `invitationID` so an investigator can correlate the override with the invitation row.

4. **Invitation stays pending.** The user still has to click the invitation link to accept and join the org — verifying email and accepting a specific org invitation are different concerns. The user's spec said "they should not have to reverify"; nothing said "skip the acceptance." Auto-accepting would also bypass the `EqualFold(userEmail, inv.Email)` token-theft prevention check in `Accept` (line 319 of `invitations.go`), which protects against an admin who invited a typo'd address.

5. **`invLogCapture` separate from `warnCaptureLogger`.** Code reuse across test files is good — but the existing `warnCaptureLogger` only satisfies a Warn-only interface, while `invitationLogger` requires both `Warn(msg, args...)` and `Error(msg, err, args...)`. Renaming `warnCaptureLogger` to satisfy both would force every existing OrgsHandler test to be updated. Adding a small adjacent type costs ~5 lines and avoids the disruption.

---

## Assumptions

| # | Assumption | Validation |
|---|---|---|
| A1 | `PgOrgStore.GetUserIDByEmail`, `MarkUserEmailVerified`, and `LogOrgEvent` already exist with the right signatures. | Confirmed: all three were added by PR #343 (or earlier) and live on `PgOrgStore`. The `pgOrgStore` value is what `app.go:657` passes to `NewInvitationsHandler` already, so no app.go changes are needed — adding the methods to the handler-local interface is sufficient. |
| A2 | An admin verifying an invitee whose email matches an existing-but-unrelated platform account is acceptable behavior. | Confirmed by the user's spec ("if the user already exists, but they have not verified their account, we should have the verify override option"). Audit log captures the override; the user still owns their account and just gains the ability to log in. |
| A3 | Cross-org isolation requires a 404 (not 403) on cross-org invitation lookup. | Confirmed — the existing `Resend` and `Delete` paths in invitations.go return 404 for missing invitations without inspecting the path's `:id`. To match that boundary, the new handler also returns 404 when `inv.OrgID != orgID`. |
| A4 | The `audit_log.action` column has no CHECK constraint, so `invitation.verify_user` requires no migration. | Confirmed by reading PR #343's worklog (0474) — same property documented there for `member.verify`. |
| A5 | Frontend `ApiClientError.body.error` is the right field to switch on. | Confirmed: `frontend/src/api/client.ts:7-12` shows `body.error` is the parsed JSON's `error` string; `types.ts:179-182` shows `ApiError.error: string` is the canonical shape. |

---

## Adversarial Self-Review

**Phase 1 — findings generated:**

1. *Could an attacker who somehow obtains org-admin access to org A use this to enumerate which emails have unverified users on the platform?* The 422 response shape is distinct from the 200 success shape, so an admin can probe `/invitations/inv-1/verify-user` with different invitations (each with a different email) and observe whether the response is 200 or 422. **Mitigation:** the admin must first create the invitation (via `Create`, which is rate-limited to 50/hour), then call verify-user. The rate limit + audit log already deter mass enumeration; the 422 vs 200 distinction is intentional UX (the admin needs to know whether the action succeeded). False alarm — no privilege expansion beyond what an org admin already has.
2. *What if the invitee's existing users row has `active=false` (suspended)?* The handler currently flips `email_verified` regardless of `active`. **Decision:** that's correct. Email verification and account suspension are orthogonal concerns. A suspended user gaining verified status doesn't unlock anything — they're still suspended. If the user gets unsuspended later, they'll already be verified, which is the desired state.
3. *What if the invitee accepts the invitation between the verify-user call and the next admin pageload?* The `Accept` handler also doesn't touch `email_verified`; it just creates the org_membership. So the user ends up verified + a member (the desired terminal state). No conflict.
4. *Frontend Pending Invitations table now has three buttons (Verify / Resend / Revoke). Is the visual order correct?* Verify first because it's the new affordance and the most likely admin intent at this moment ("I just added this person, mark them verified"); Resend second; Revoke last because it's destructive and the existing pattern puts destructive actions on the right.
5. *Is the 422 message string user-facing the right way?* The current text is "No account exists for this email yet. The invitee must sign up before you can verify them." This makes the action steps clear without exposing the machine code. Acceptable.

**Phase 2** — All findings either fixed (none required) or documented as false alarms with rationale.

**Phase 3** — No remediations needed.

---

## Blockers

None.

---

## Tests Run

```bash
# Backend — affected packages
GOPROXY=direct GONOSUMCHECK=* GONOSUMDB=* go test -timeout 60s -run 'TestInvitations_VerifyUser' ./api/internal/handlers/   # 14/14 PASS
GOPROXY=direct GONOSUMCHECK=* GONOSUMDB=* go test -timeout 120s ./api/internal/handlers/                                    # all PASS

# Backend — formatting
gofmt -l api/internal/handlers/invitations.go api/internal/handlers/invitations_test.go api/internal/server/router.go        # no output (clean)

# Frontend
npm run test -- src/components/org-admin/OrgMembersTab.test.tsx                                                              # 13/13 PASS (8 pre-existing + 5 new)
npm run test                                                                                                                  # 1198/1198 PASS (113 files)
npm run typecheck                                                                                                            # clean
npm run lint                                                                                                                 # clean
```

---

## Files Modified

- `api/internal/handlers/invitations.go` — interface extension + `VerifyUserForInvitation` handler
- `api/internal/handlers/invitations_test.go` — mock extension (3 new methods + helpers) + 12 new tests
- `api/internal/server/router.go` — route registration under `orgAdminGroup`
- `frontend/src/api/orgs.ts` — `verifyInvitee` API method
- `frontend/src/components/org-admin/OrgMembersTab.tsx` — Verify button on Pending Invitations rows
- `frontend/src/components/org-admin/OrgMembersTab.test.tsx` — 5 new tests + mock extension
- `worklogs/0500_2026-06-22_epic-43-verify-pending-invitee.md` — this file

---

## Next Steps

- (Optional) Add a confirmation dialog before force-verifying — matches the destructive-action pattern in `DangerZone.tsx`. Defer until product confirms whether the lightweight UX of Resend/Revoke is right for Verify too.
- (Optional) Surface the `invitation.verify_user` events in the org audit-log UI alongside `member.verify` for a unified "force-verify history" view.
- (Optional) Consider adding the same flow to platform-admin pages (`AdminUsersPage`) for cross-org visibility.
