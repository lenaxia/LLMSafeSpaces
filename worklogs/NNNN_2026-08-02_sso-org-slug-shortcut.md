# Worklog: SSO Org-Slug Shortcut + Redirect-Based Error Handling

**Date:** 2026-08-02
**Session:** Add /sso/:orgSlug frontend shortcut and fix backend Start handler error UX
**Status:** Complete

---

## Objective

Add a short, shareable URL (`/sso/:orgSlug`) so users can log into a specific org's SSO without typing the full API path. Fix the backend Start handler to redirect to the frontend on all error paths instead of returning raw JSON.

---

## Work Completed

### /sso/:orgSlug frontend route

- New `SSOStartPage.tsx` — reads `:orgSlug` from URL, shows a spinner, redirects to backend `GET /api/v1/auth/sso/:orgSlug/start` via `window.location.href`
- Registered in `router.tsx` as a standalone public route (same pattern as `/invitations/:token`)
- Covers the gap for orgs like Authelia where email domains are inconsistent and domain-based login-page discovery doesn't route users

### Backend Start handler: redirect instead of JSON

- Changed all error branches in `Start` (`org_sso.go:235`) to `c.Redirect(302, frontendRedirectWithError(err))` instead of `c.JSON(404/500)`
- Added `not_configured` token mapped from `ErrSSONotConfigured` in `errorReason()`
- Removed redundant double-logging on unset RedirectBaseURL (resolveCallbackURL already logs)
- Matches the existing Callback handler pattern — every SSO entry point now gets clean error UX

### Frontend error messages

- Added `not_configured` case to LoginPage SSO error banner: "This organisation does not have single sign-on configured."

### Tests

- `SSOStartPage.test.tsx` — 4 component tests (redirect, slug pass-through, spinner, missing-slug fallback)
- `sso-start.spec.ts` — Playwright e2e (happy: redirects to IdP; unhappy: shows not_configured error on login page)
- `LoginPage.test.tsx` — added unit test for `?sso=not_configured` message
- `org_sso_test.go` — updated 2 existing tests + added generic-error redirect test (3 total for Start handler)

---

## Key Decisions

- **Redirect over JSON for Start errors**: The Start endpoint is a browser-facing redirect endpoint, not a JSON API. Returning JSON for errors showed raw text in the browser. The Callback handler already used the redirect pattern — we aligned Start with it.
- **No frontend slug validation**: The backend is the source of truth for slug validity. Client-side validation would duplicate rules and is unnecessary for a single redirect.
- **Standalone route, not behind GuestOnly**: `/sso/:orgSlug` is a pure redirect — it doesn't render UI beyond a spinner. No auth gate needed, consistent with `/invitations/:token`.

---

## Blockers

None.

---

## Tests Run

- `go test -race ./internal/handlers/... -run "SSO|Login"` — all pass
- `npx vitest run src/pages/SSOStartPage.test.tsx` — 4 pass (Node 18; CI uses compatible version)
- `npx eslint` on all changed files — clean

---

## Next Steps

- Merge PR #623 after reviewer approval
- Consider adding an org-directory page for discovering orgs by name (deferred — the /sso/:orgSlug shortcut handles the immediate need)

---

## Files Modified

- `frontend/src/pages/SSOStartPage.tsx` (new)
- `frontend/src/pages/SSOStartPage.test.tsx` (new)
- `frontend/src/router.tsx`
- `frontend/src/pages/LoginPage.tsx`
- `frontend/src/pages/LoginPage.test.tsx`
- `frontend/tests/e2e/sso-start.spec.ts` (new)
- `api/internal/handlers/org_sso.go`
- `api/internal/handlers/org_sso_test.go`
