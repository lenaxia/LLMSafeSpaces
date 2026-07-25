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
- **react-router 6→7**: the app already used `createBrowserRouter` (the v6.4+ data-router API), which is the v7 recommended pattern. v7 makes the v6 future flags (`v7_startTransition`, `v7_relativeSplatPath`) the default — the warnings disappeared post-bump. No code changes required.

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
cd frontend && npx vitest run            # 127 files, 1437 pass / 1 skip
cd frontend && npm audit                 # 2 remaining (both documented non-exploitable)
```

---

## Files Modified

- `frontend/package.json` — 5 version bumps.
- `frontend/package-lock.json` — regenerated.
- `.trivyignore` — removed 2 obsolete entries (esbuild, vite), added 2 new entries (brace-expansion, react-router RSC) with documented rationale + expiration.
- `worklogs/0652_2026-07-25_frontend-major-bumps.md` — this worklog.
