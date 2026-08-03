# Image Factory — Agent Handoff (2026-08-03)

> Status: **v0.8.2 deployed to prod (talos-ops-prod), fully wired, but builds fail at dispatch.**
> This doc supersedes the prior handoff notes. The previous root-cause hypothesis
> ("binary not built from v0.8.2 tag") is **WRONG** and was disproven. The real
> cause is below — verified live against the cluster and the GitHub API.

---

## TL;DR — the blocker, the real cause, the fix

`POST /api/v1/image-factory/configs` returns **`503 "failed to dispatch build"`**.

**Root cause (VERIFIED):** The GitHub App `llmsafespaces-builder` (App ID `4470040`)
has repository permission **`actions: read`** but **NOT `actions: write`**. The
`workflow_dispatch` POST therefore returns
`403 "Resource not accessible by integration"`. That error is **swallowed** by the
handler (`imagefactory_create.go:230-233`) and surfaced only as the generic 503 —
which is why it looked like a wiring/version problem.

**Evidence (live, 2026-08-03):**
- Minted the App JWT from the actual secret → installation `150863674` → installation
  token `201` → `GET .../workflows/image-build.yml` → `200` (active). Auth is fine.
- `POST .../actions/workflows/image-build.yml/dispatches` (same token, realistic
  dockerfile payload) → **`403 {"message":"Resource not accessible by integration"}`**.
- Installation permissions record returned by the API:
  ```json
  { "actions": "read", "contents": "read", "metadata": "read", "packages": "write" }
  ```
- Timing is consistent: pod's 503 took **636 ms** ≈ install-GET (~150 ms) +
  token-POST (~150 ms) + 403 dispatch (~264 ms).

**The fix (no code change, no redeploy needed):**
1. GitHub → App settings for `llmsafespaces-builder` → **Repository permissions →
   Actions → change "Read-only" to "Read and write"**.
2. Save. If the org requires re-approval for elevated permissions, accept the
   permission update on the installation (Settings → Applications, or the org's
   integration settings).
3. Retry `POST /configs` from the UI. It should return `201` immediately. (The
   build then runs in GH Actions and calls back.)

**Code hygiene fix (strongly recommended, do it now):** the handler must log the
dispatch error instead of discarding it. See "Fix B" below — this is why diagnosis
took hours instead of seconds.

---

## Verified cluster state (all confirmed, not assumed)

| Check | Result |
|---|---|
| API image tag | `ghcr.io/lenaxia/llmsafespaces/api:0.8.2` (pod actually running it) |
| ConfigMap `ghDispatcher` | Present: `owner=lenaxia repo=LLMSafeSpaces workflowId=image-build.yml ref=main` |
| Env vars on api container | `LLMSAFESPACES_IMAGE_FACTORY_APP_ID` + `_APP_PRIVATE_KEY` mounted from secret `image-factory-app-credentials` ✓ |
| Secret `image-factory-app-credentials` | Has `app-id` (decodes to `4470040`) + `app-private-key` (valid 2048-bit RSA PEM, 1679 B) ✓ |
| `applyImageFactoryEnvOverrides` | Reads the two env vars into `cfg.ImageFactory.GHDispatcher.AppID/PrivateKey` (`config.go:409-417`) ✓ |
| Dispatcher wiring | `app.go:711-719` constructs `NewGHActionsDispatcher` when AppID+PrivateKey non-empty. Handler returns "image builds are not configured" only if `dispatcher==nil`; we see "failed to dispatch build" instead → **dispatcher is non-nil and `Dispatch()` actually ran** ✓ |
| Repo name `LLMSafeSpaces` (plural) | Correct — canonical (HTTP 200). The singular `LLMSafeSpace` 301-redirects to repo id `943462740`; the git remote still uses the old singular name and works via GitHub's redirect. **The ConfigMap plural value is right.** |
| Network egress (cluster → GitHub) | DNS + HTTPS to `api.github.com` work from a debug pod in-namespace (returns 401 for unauth, i.e. reachable). NetworkPolicies only target `component=workspace` + `relay-router`, NOT the api pod ✓ |
| Workflow file on `main` | `.github/workflows/image-build.yml` exists, state `active`, has `on: workflow_dispatch` with all 8 inputs ✓ |
| GitHub App installation | Installed on `lenaxia` (install `150863674`), token mint works, workflow is readable ✓ |

**Net:** every preconditions is satisfied except the App's Actions permission level.

---

## The two fixes

### Fix A — grant Actions:Write to the App (unblocks immediately)

- URL: https://github.com/settings/apps/llmsafespaces-builder (or the org's app
  settings) → **General → Repository permissions → Actions = Read and write**.
- `packages: write` is already granted (needed for the workflow to push images to
  `ghcr.io/lenaxia/llmsafespaces-images/ws`). `contents: read` is enough for the
  dispatch. **Only `actions` needs to go read→write.**
- No redeploy. The dispatcher caches the installation token for 50 min
  (`imagefactory_dispatcher.go:157`); a fresh token after the permission change
  will carry `actions: write`. To be safe, restart the api pod (or wait 50 min) so
  the cached token refreshes.
- Verify: retry the create-and-build from the UI; expect `201`, then a `building`
  config, then a callback flipping it to `ready` (or `rejected` with a buildx log).

### Fix B — stop swallowing the dispatch error (APPLIED in code, pending deploy)

**Status: code change done on `main` (uncommitted in working tree as of
2026-08-03); needs commit + a new tag + Flux reconcile to reach prod.**

The root cause was diagnosable only by reproducing the GitHub API flow
out-of-band because `imagefactory_create.go` discarded the dispatch error.
The fix surfaces it via the handler's logger (following the package's
`SetLogger(l pkginterfaces.LoggerInterface)` + `if h.logger != nil` convention).

Changes (all verified: `go build`, `go vet`, `gofmt -l`, image-factory tests
green incl. a new `TestIF_CreateConfig_DispatchFailureLogsError` regression):

- `api/internal/handlers/imagefactory.go` — added `logger` field +
  `SetLogger(l pkginterfaces.LoggerInterface)`; imported
  `pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"`.
- `api/internal/handlers/imagefactory_create.go` (dispatch-failure path ~L230):
  ```go
  if err != nil {
      if h.logger != nil {
          h.logger.Error("image-factory: build dispatch failed", err,
              "hash", hash, "baseName", baseName, "baseVersion", baseVersion)
      }
      c.JSON(http.StatusServiceUnavailable, gin.H{"error": "failed to dispatch build"})
      return
  }
  ```
  Nil-guarded so existing tests that don't wire a logger still pass.
- `api/internal/app/app.go` — calls `imageFactoryHandler.SetLogger(log)`
  right after construction.
- `api/internal/handlers/imagefactory_create_test.go` — added
  `captureIFLogger` + `TestIF_CreateConfig_DispatchFailureLogsError`
  (asserts the wrapped error reaches the logger), and refactored
  `newIFRouterWithDispatcher` to delegate to a shared `newIFRouterForHandler`.

> With this deployed, a repeat of the 403 would have logged
> `image-factory: build dispatch failed error="gh dispatch: unexpected status
> 403: {\"message\":\"Resource not accessible by integration\"...}"`
> — making the missing permission obvious in seconds.

---

## What's built and merged (v0.8.0 → v0.8.2)

**Design**
- `design/0046_2026-08-01_image-factory.md` — 28 decisions
- `design/0047_2026-08-02_image-factory-contracts.md` — types, interfaces, test plan

**Pure logic — `api/internal/imagefactory/`**
- `types.go`, `selection.go`, `resolved_values.go`, `dockerfile.go` (single renderer)
- `seed.go` + `catalog.seed.yaml` — embedded catalog, **30 extensions in 3 groups**
  (Language Packs, System Packages, Files). Seeded on startup.
- `HashSelection`, `ResolveSelection`, `RenderDockerfile`, `ValidateResolved`, `SeedCatalog`

**DB — `api/internal/services/database/imagefactory.go`**
- Migration `000013` — 6 tables: `platform_config`, `bases`, `extensions`,
  `known_failures`, `configs`, `builds`
- 30+ methods incl. atomic txns: `CreateConfigAndBuild`, `TransitionBuildSucceeded`,
  `TransitionBuildFailed`

**Handlers — `api/internal/handlers/imagefactory*.go`**
- Consumer: `GET /catalog`, `GET /configs`, `GET /configs/:hash`,
  `POST /configs` (coalescing + **dispatch-before-commit**)
- Callback: `POST /internal/image-factory/builds/:id/callback`
  (ConstantTimeCompare token, idempotent, atomic transitions)
- Admin (`imagefactory_admin.go`, behind AdminGuard): `GET/POST/DELETE` for bases,
  extensions, known-failures, platform-config
- LLM explainer (`imagefactory_explainer.go`) — OpenAI-compatible, graceful degrade
- Dispatcher (`imagefactory_dispatcher.go`) — GitHub App JWT → installation token
  (cached 50 min, mutex-protected) → workflow_dispatch

**Workflow — `.github/workflows/image-build.yml`**
- API renders the Dockerfile; workflow builds via raw buildx (`set -euo pipefail`),
  pushes to `ghcr.io/lenaxia/llmsafespaces-images/ws`, callbacks with real buildx
  log tail on failure. Repo secrets `APP_ID` + `APP_PRIVATE_KEY` set for
  `tj-actions/github-app-token` inside the workflow.

**Frontend — `frontend/src/components/settings/WorkspaceImagesTab.tsx`**
- Settings page: grouped extensions, status pills (building/ready/rejected),
  create-and-build form. API client `frontend/src/api/imageFactory.ts`.

**Helm — `helm/values.yaml` + templates**
- `imageFactory.*` values; `ghDispatcher.appCredentials.secretName` wires App ID +
  private key via `secretKeyRef` into the api deployment's env.

**Tests:** ~118 Go test funcs (unit, sqlmock, postgres integration, e2e
round-trips, dispatcher auth flow). Frontend vitest suite exists.

---

## What's NOT built yet (next agent's backlog)

1. **Org/platform admin image-config publishing** — no UI for org owners to publish
   named configs or set `assemble` / `published_only` policy; the org-scoped config
   management endpoints don't exist yet.
2. **Launch-dialog integration** — the workspace "+ create" flow doesn't reference
   image-factory configs. Design wants a dropdown with status pills
   (ready / building / greyed-out rejected).
3. **On-read status recovery** — if a callback is lost, a config stays `building`
   forever. The old `deriveBuildStatus` was removed as dead code; needs a real
   GH Actions status resolver (poll `actions/runs/{gh_run_id}`).
4. **In-workflow retry** (design #12) — deferred; currently reports first result.
5. **Rate limits / GC** (design #20, #24) — deferred.

---

## Key files

| Area | Path |
|---|---|
| Design | `design/0046_*`, `design/0047_*` |
| Pure logic | `api/internal/imagefactory/` (`types.go`, `selection.go`, `resolved_values.go`, `dockerfile.go`, `seed.go`, `catalog.seed.yaml`) |
| DB store | `api/internal/services/database/imagefactory.go` |
| Consumer handlers | `api/internal/handlers/imagefactory.go`, `imagefactory_create.go`, `imagefactory_callback.go` |
| Admin handler | `api/internal/handlers/imagefactory_admin.go` |
| **Dispatcher** | `api/internal/handlers/imagefactory_dispatcher.go` |
| LLM explainer | `api/internal/handlers/imagefactory_explainer.go` |
| E2E tests | `api/internal/handlers/imagefactory_e2e_test.go` |
| Workflow | `.github/workflows/image-build.yml` |
| Frontend | `frontend/src/components/settings/WorkspaceImagesTab.tsx`, `frontend/src/api/imageFactory.ts` |
| Config | `api/internal/config/config.go` (`ImageFactory` struct ~L239, `applyImageFactoryEnvOverrides` ~L409) |
| Wiring | `api/internal/app/app.go` (~L690-730) |
| Migration | `api/migrations/000013_image_factory.up.sql` |
| **Helm (prod)** | `~/workspace/talos-ops-prod/kubernetes/apps/llmsafespaces/llmsafespaces/app/helm-release.yaml` (`imageFactory.ghDispatcher` ~L257) |
| SOPS secret (prod) | `~/workspace/talos-ops-prod/kubernetes/apps/llmsafespaces/llmsafespaces/app/secret.sops.yaml` (`app-id`, `app-private-key`) |

---

## PRs merged

| PR | Scope | Tag |
|---|---|---|
| #616 | S1-S3: design, migration, pure logic, DB store, read-only endpoints | v0.8.0 |
| #619 | S4+S5: POST /configs, coalescing, dispatch-before-commit, callback, atomic transitions | v0.8.0 |
| #624 | S6: LLM failure explainer | v0.7.2 |
| #628 | S7: admin portal endpoints | v0.7.2 |
| #629 | S8: GH Actions image-build workflow | v0.7.2 |
| #631 | S9+S10: e2e tests + frontend | v0.8.0 |
| #634 | Production: catalog seed, real dispatcher, Helm wiring | v0.8.0 |
| #637 | GitHub App authentication (replaces PAT) | v0.8.1 |
| #638 | Catalog redesign (30 extensions, 3 groups) | v0.8.2 |

`main` HEAD at writing: `704471b7 release: v0.8.2 — null fix + catalog redesign`
(includes `319a43bb fix(image-factory): prevent null knownFailures crash`).

---

## How to verify the fix (after granting Actions:Write)

1. Restart the api pod so the cached installation token refreshes:
   `kubectl -n llmsafespaces rollout restart deploy/llmsafespaces-api`
2. Tail logs filtered for the dispatch path:
   `kubectl -n llmsafespaces logs -f deploy/llmsafespaces-api -c api | grep -iE "image-factory|dispatch"`
3. From the UI (Settings → Workspace Images), submit a small selection (e.g.
   `bookworm` + `python-3.12`). Expect `201`, a `building` config.
4. Watch the GH Actions run; on success the config flips to `ready` and the image
   appears at `ghcr.io/lenaxia/llmsafespaces-images/ws:s-<hash>`.

## Quick reproduction of the failure (if the fix doesn't hold)

The dispatch flow is fully reproducible out-of-band with the App key (no redeploy):

```python
import jwt, requests, time
app_id, key = "4470040", open("key.pem").read().strip()
H = {"Accept":"application/vnd.github+json","X-GitHub-Api-Version":"2022-11-28"}
now=int(time.time())
t=jwt.encode({"iat":now-60,"exp":now+600,"iss":app_id},key,algorithm="RS256")
inst=requests.get("https://api.github.com/repos/lenaxia/LLMSafeSpaces/installation",
      headers={**H,"Authorization":f"Bearer {t}"}).json()["id"]
it=requests.post(f"https://api.github.com/app/installations/{inst}/access_tokens",
      headers={**H,"Authorization":f"Bearer {t}"}).json()["token"]
r=requests.post("https://api.github.com/repos/lenaxia/LLMSafeSpaces/actions/workflows/image-build.yml/dispatches",
      headers={**H,"Authorization":f"Bearer {it}","Content-Type":"application/json"},
      json={"ref":"main","inputs":{...all 8 inputs...}})
print(r.status_code, r.text)   # 403 = still missing Actions:Write ; 201 = fixed
```
A `403 "Resource not accessible by integration"` on this call = the App still lacks
`actions: write`. A `201` = fixed.
