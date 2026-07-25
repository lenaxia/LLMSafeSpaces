# Worklog: Frontend major-version bumps — vite 5→8, vitest 2→3, react-router 6→7

**Date:** 2026-07-25
**Session:** Clear the moderate-severity npm CVEs (esbuild dev-server CORS, react-router open-redirect) that Trivy was flagging on every PR, by bumping the frontend toolchain to current major versions.
**Status:** Complete

---

## Objective

Trivy's `--severity HIGH,CRITICAL` config was not flagging the moderate CVEs, but they showed up in `npm audit` and were called out as follow-up work in PR #597. The user requested all remaining issues be fixed, including these major-version bumps. This PR clears the moderate CVEs and gets `npm audit` as clean as possible given current upstream advisory state.

---

## Work Completed

### Bumps

| Package | Before | After | Why |
|---|---|---|---|
| `vite` | `^5.4.21` | `^8.1.5` | Clears esbuild dev-server CORS (GHSA-67mh-4wv8-2f99), vite server.fs.deny bypass (CVE-2026-53571) |
| `@vitejs/plugin-react` | `^4.7.0` | `^5.2.0` | Required for vite 8 peer compat (v4 caps at vite 7) |
| `vitest` | `^2.1.9` | `^3.2.7` | vitest 2 depends on vulnerable vite ≤6.4.2; v3 uses vite 8 internally |
| `@vitest/coverage-v8` | `^2.1.9` | `^3.2.7` | Must track vitest major |
| `react-router-dom` | `^6.30.3` | `^7.18.1` | Clears open-redirect advisories (GHSA-wrjc-x8rr-h8h6, GHSA-337j-9hxr-rhxg) and the same-origin redirect advisory (GHSA-2j2x-hqr9-3h42) |

`vite-plugin-pwa@^1.3.0` was already compatible with vite 8 (no change needed).

### Migration notes

- **vite 5→8**: three major versions jumped, but the app's `vite.config.ts` uses only stable, long-standing APIs (`defineConfig`, `react()`, `tailwindcss()`, `VitePWA()`). No config changes required. Build passes; bundle sizes unchanged within noise.
- **vitest 2→3**: no API changes consumed — the test files use standard `describe/it/expect`, `vi.mock`, jsdom environment. All 127 test files / 1437 tests pass.
- **react-router 6→7**: the app already used `createBrowserRouter` (the v6.4+ data-router API), which is the v7 recommended pattern. v7 makes the v6 future flags (`v7_startTransition`, `v7_relativeSplatPath`) the default. **One code change required** — see "returnTo redirect fix" below.

### returnTo redirect fix (react-router v7 startTransition race)

v7 wraps router state updates in `React.startTransition` by default (the `v7_startTransition` future flag became the default). This exposed a race in the login/register `returnTo` redirect:

1. User submits login form → `onSubmit` calls `await login(email, password)`.
2. Inside `login`, `setUser(res.user)` dispatches a state update.
3. The state update triggers `GuestOnly` to render `<Navigate to="/chat" replace />` (because `user` is now non-null).
4. The line after `await login()` — `if (returnTo) navigate(returnTo)` — runs, but the `GuestOnly` redirect has already committed in the startTransition batch.

Result: the user lands on `/chat` instead of the `returnTo` target (e.g., `/settings`). The e2e test `return-to.spec.ts:100` ("login with return_to navigates back to target") caught this.

**Fix**: replaced `navigate(returnTo)` with `window.location.href = returnTo` in both `LoginPage.tsx` and `RegisterPage.tsx`. A full-page navigation is deterministic — the browser navigates before React re-renders. The page reloads at the target URL with the now-valid auth cookie, and `RequireAuth` lets it through.

**Tests updated**:
- `LoginPage.test.tsx` and `RegisterPage.test.tsx`: the `returnTo` happy-path tests now spy on `window.location.href` setter (via `Object.defineProperty(window, "location", ...)`) instead of the mocked `useNavigate`. The mock is installed AFTER render (so the component's `useEffect` can read `return_to` from real search params) but BEFORE the submit click.
- Added unhappy-path tests: `does NOT redirect to return_to when login fails` (LoginPage) and `does NOT redirect to return_to when register fails` (RegisterPage) — verify that a rejected auth promise does not trigger `window.location.href`.
- Removed dead `useNavigate` / `mockNavigate` from both test files (no longer used after the component change).

### `.trivyignore` updates

**Removed** (fixes now included in the bumped versions):
- `GHSA-gv7w-rqvm-qjhr` (esbuild Deno integrity) — vite 8 uses esbuild 0.28+ which has the fix.
- `CVE-2026-53571` (vite server.fs.deny Windows bypass) — fixed in vite 8.0.16+.

**Added** (new advisories that surfaced post-bump, both documented as non-exploitable):
- `GHSA-mh99-v99m-4gvg` (brace-expansion OOM DoS) — transitive dev-only dep (minimatch → brace-expansion → test-exclude, pulled by vitest/coverage-v8). Not shipped in production (frontend Dockerfile runs `npm ci --omit=dev`). Fix requires vitest 4.x.
- `GHSA-qwww-vcr4-c8h2` (react-router RSC CSRF bypass) — affects RSC mode with server actions only. This app uses client-side `createBrowserRouter` (no SSR, no server actions). No patched release exists yet.

---

## Tests Run

```
cd frontend && npx tsc --noEmit          # clean
cd frontend && npx vite build            # ✓ built in 1.97s
cd frontend && npx vitest run            # 127 files, 1437 pass / 1 skip (after returnTo fix + new unhappy-path test)
cd frontend && npm audit                 # 2 remaining (both documented non-exploitable)
# CI validates: Frontend (unit + typecheck + e2e) — includes the Playwright return-to.spec.ts e2e
```

---

## Key Decisions

1. **Jump vite 5→8 directly (not 5→6→7→8).** The app's `vite.config.ts` uses only stable APIs that didn't change across those majors. Incremental bumps would have tripled the testing effort for zero additional safety. All intermediate breaking changes (CJS Node API removal in v6, default SSR externalization in v7, rolldown integration in v8) don't affect this config.

2. **`react-router-dom@7` not `react-router@7`.** v7 merged `react-router-dom` re-exports into `react-router`, but `react-router-dom` remains the correct package for apps using DOM APIs (`createBrowserRouter`). The merged package is `react-router` (framework mode). Keeping `react-router-dom` avoids touching imports.

3. **Document non-exploitable advisories rather than blocking on them.** `brace-expansion` is dev-only (vitest transitive); `react-router RSC CSRF` requires a server runtime this app doesn't run. Both have 2026-10-21 review dates so they don't linger.

---

## Blockers

None.

---

## Next Steps

1. After merge, the frontend Dockerfile build will use vite 8 / vitest 3 / react-router 7 — verify the production image build (`make build-frontend`) succeeds in the release pipeline.
2. Re-evaluate the 2 `.trivyignore` entries at 2026-10-21: check if vitest 4 is stable (clears brace-expansion) and if react-router has shipped a patched release (clears RSC CSRF).
3. Optional: the `@types/node@^24` and `typescript@~6.0.2` versions are already current — no further TypeScript-side bumps needed.

---

## Files Modified

- `frontend/package.json` — 5 version bumps.
- `frontend/package-lock.json` — regenerated.
- `frontend/src/pages/LoginPage.tsx` — `navigate(returnTo)` → `window.location.href = returnTo` (v7 startTransition race fix); removed `useNavigate` import.
- `frontend/src/pages/RegisterPage.tsx` — same fix as LoginPage.
- `frontend/src/pages/LoginPage.test.tsx` — updated returnTo test to spy on `window.location.href`; added unhappy-path test (login failure does not redirect); removed dead `useNavigate`/`mockNavigate`.
- `frontend/src/pages/RegisterPage.test.tsx` — same test updates as LoginPage.
- `.trivyignore` — removed 2 obsolete entries (esbuild, vite), added 2 new entries (brace-expansion, react-router RSC) with documented rationale + expiration.
- `worklogs/NNNN_2026-07-25_frontend-major-bumps.md` — this worklog.
