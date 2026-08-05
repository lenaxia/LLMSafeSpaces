# Worklog 0683 — Close deferred-wiring regression gaps (admin audit + resolveWorkspaceQuota nil-guard)

**Date:** 2026-08-04
**Scope:** Close the two regression-test gaps flagged in the post-merge audit
of the MCP deferred-wiring fix (PR #622) and the org-tab fix (PR #650).

## Summary

After merging #622 (nil-orgChecker production 500) and #650 (org-tab routing
+ policy toggle), an audit found two gaps where a bug we'd identified and
fixed had **no regression test** pinning the invariant:

1. **Admin audit nil-wiring (#622)** — `adminMcpHandler.SetAudit(pgOrgStore)`
   was nil-wired at construction (same init-ordering bug as the user handler).
   The fix moved `SetAudit` to the deferred-wiring block, but no test proved
   platform-admin MCP CRUD events actually reach the logger after the wiring.
   A regression would silently drop all admin MCP audit events again.

2. **`resolveWorkspaceQuota` nil-guard (#622 review, deferred)** — the bot
   review of #622 flagged `resolveWorkspaceQuota` (`mcp_servers.go:707`) as
   the same nil-orgChecker bug class as `UserCreate`, with a narrower window
   (org-owned-workspace bind path). The deferred wiring makes it unreachable
   today, but there was no guard and no test. This PR adds both.

## Fixes

- **`mcp_servers.go:resolveWorkspaceQuota`** — added
  `if h.orgChecker == nil { return types.DefaultMaxMcpServersPerWorkspace }`
  before the `GetOrgPolicies` deref. Fail-safe default, mirrors the
  `UserCreate` nil-guard.
- **`mcp_servers_test.go`** — `stubMCPStore` gained a `wsOrgID` field so the
  org-owned-workspace quota path is exercisable (default empty = personal
  workspace = early-return, preserving existing test behavior).

## Regression tests

- `TestAdminCreate_DeferredAudit_LogsEvent` — constructs the admin handler
  with no audit, calls `SetAudit(stub)`, invokes `AdminCreate`, asserts
  exactly one `LogAuditEvent` call with `domain=admin`,
  `action=mcp_server.create`. Pre-fix (nil-wired SetAudit) → 0 calls.
- `TestBind_NilOrgChecker_OrgOwnedWorkspace_DoesNotPanic` — constructs a
  user handler with nil orgChecker + a store that returns a non-empty
  orgID (org-owned workspace), invokes `Bind`. Pre-fix (no nil-guard) →
  nil deref panic on `GetOrgPolicies`. Post-fix → default quota, bind
  proceeds, no 500.

## Validation

- `go test -race -short ./api/internal/handlers/` — ok (full package).
- Both new tests pass under `-race`.
