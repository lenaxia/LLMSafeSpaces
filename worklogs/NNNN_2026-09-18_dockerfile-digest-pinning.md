# Worklog: Dockerfile base-image digest pinning (#1330)

**Date:** 2026-09-18
**Session:** Digest-pin every Dockerfile base image (issue #1330), add red-first pin-enforcement repolint test, extend renovate digest grouping, make README-LLM's posture claim true.
**Status:** Complete

---

## Objective

Issue #1330: unprefixed Dockerfile `FROM` lines resolve against docker.io; during the 2026-09-11 Docker Hub auth-endpoint outage 11 CI jobs failed (`failed to fetch oauth token ... 500`), and tag-only pins mean a moved tag = silently different base. README-LLM.md already *claimed* "Debian bookworm-slim (digest-pinned)" — false at the time. Fix: pin every registry FROM to `tag@sha256:<manifest-list digest>`, enforce with a repolint test so it can't regress, and keep pins maintainable via renovate.

---

## Work Completed

### Inventory (sweep, not just the issue list)

`rg`-equivalent sweep (`grep -rn '^FROM ' --include='Dockerfile*'`) over all 8 Dockerfiles found **13 unpinned registry FROMs** — the issue's 9 plus **4 × `gcr.io/distroless/static:nonroot`** (api/controller/relay-proxy/relay-router delivery stages) that the issue hadn't listed but that fail the same "every FROM carries @sha256:" bar. `FROM scratch` (agentd:52, opencode:79) correctly exempt — no registry round-trip. No `*.Dockerfile`/build-stage files exist elsewhere; no Dockerfiles under any `testdata/` (api/internal/imagefactory has no testdata dir at all — nothing byte-locked to exclude).

### Digest resolution (docker unavailable in sandbox → registry HTTP API)

Per-tag manifest-LIST digest via anonymous token + manifest GET, reading the `Docker-Content-Digest` response header. Resolution commands (recorded verbatim):

```sh
repo=library/golang; tag=1.26   # (repeat per image; nginxinc/... for the namespaced one)
token=$(curl -sfS "https://auth.docker.io/token?service=registry.docker.io&scope=repository:${repo}:pull" | jq -r .token)
curl -sfS -o /dev/null -D - -H "Authorization: Bearer ${token}" \
  -H "Accept: application/vnd.oci.image.index.v1+json" \
  -H "Accept: application/vnd.docker.distribution.manifest.list.v2+json" \
  "https://registry-1.docker.io/v2/${repo}/manifests/${tag}" | tr -d '\r' | grep -i docker-content-digest
# gcr.io variant: anonymous token from https://gcr.io/v2/token?scope=repository:distroless/static:pull,
# then GET https://gcr.io/v2/distroless/static/manifests/nonroot
```

Evidence all are list/index pins: response `Content-Type: application/vnd.oci.image.index.v1+json` (verified for golang:1.26, nginxinc/nginx-unprivileged:1.27-alpine, and gcr.io/distroless/static) — the multi-arch (amd64+arm64) buildx matrix resolves both platforms against a list digest; a single-arch manifest digest would 404/fail on the other arch.

| Image | Digest (2026-09-18) | Used in |
|---|---|---|
| `golang:1.26` | `sha256:3c3e25a4da13fd0478eed2df1eb35a0e667094a7124d3993a6a1d30f71c17e79` | api:13, controller:13, relay-proxy:11, relay-router:11 |
| `golang:1.26-bookworm` | `sha256:9fdc884aacc3bec89b20ffc69f4bb369c78210e3e4f600387b5128b12c199f81` | workspace-agentd:29 |
| `debian:bookworm-slim` | `sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171` | runtimes/base:2, runtimes/opencode:47 |
| `node:22-bookworm-slim` | `sha256:83f487e0a63425e5b4d146fb5e5be574bcbe1b7b843d3ebafdd95eaf7767a7e5` | frontend:1 |
| `nginxinc/nginx-unprivileged:1.27-alpine` | `sha256:65e3e85dbaed8ba248841d9d58a899b6197106c23cb0ff1a132b7bfe0547e4c0` | frontend:20 |
| `gcr.io/distroless/static:nonroot` | `sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3` | api:47, controller:48, relay-proxy:39, relay-router:39 |

Post-pin re-verification: re-queried `debian` and `distroless` **by digest** (GET .../manifests/sha256:...) → HTTP 200 both.

### Enforcement test (red-first, TDD)

New `pkg/repolint/dockerfile_digest_pin_test.go` — `TestDockerfiles_BaseImagesDigestPinned` walks every `Dockerfile*` (same walk as `TestDockerfiles_NoTargetArchDefault`; skips node_modules/vendor/.git), parses each FROM (flag-tolerant, comment-skipping), exempts `scratch` + stage-local aliases (two-pass: collect `AS <name>` first), and requires `@sha256:` + exactly 64 lowercase hex (catches truncated pastes). Committed RED first (13 findings, exactly the inventory above), then pins, then green.

### renovate disposition

`renovate.json` already extends **`docker:pinDigests`** — that preset is what pins and keeps updating Dockerfile digests, so coverage existed. Gap found: the only dockerfile-manager packageRule grouped `patch` updates; digest-pin refresh PRs (`updateType: digest`) would arrive ungrouped/ungoverned. Extended that rule in-place (existing style): `matchUpdateTypes: ["patch", "digest"]` + description updated. Deliberately did **not** add automerge for docker digests (unlike github-actions digests): base-image changes alter release-gated artifacts (runtime images are content-versioned CalVer; frontend has a zero-HIGH/CRITICAL Trivy gate) — human review stays.

### README-LLM posture line

Line 787 technology-stack row edited minimally: "Debian bookworm-slim (digest-pinned; every Dockerfile base is, enforced by `pkg/repolint` — #1330)" — the previously false claim is now true and points at the enforcement test.

### Review r1 on PR #1447 (github-actions REQUEST_CHANGES) — all findings addressed in r2

- **Finding 1 (hard gate) — `# syntax=docker/dockerfile:*` directives are a second docker.io dependency.** 6 Dockerfiles (api, controller, relay-proxy, relay-router `:1.7`; workspace-agentd, runtimes/opencode `:1`) carried the directive, forcing a docker/dockerfile frontend-image fetch from docker.io on every fresh CI runner — the exact outage class of the issue. **Disposition: REMOVED**, following the prior attempt documented in the issue thread (2026-09-11, verified by full buildx 0.37 builds of agentd + relay-router). Independently re-verified before removal: targeted grep over all 8 Dockerfiles finds zero post-builtin-frontend features (no heredocs, `RUN --mount`, `COPY --parents`, `ADD --checksum`, `--network`); the only frontend features used are `COPY --chmod/--chown` and `--platform=$BUILDPLATFORM`, supported by the builtin frontend in CI's buildx. Chose removal over pinning (`# syntax=...@sha256:...`): removal eliminates the registry fetch entirely, where a pinned directive would still fetch from docker.io (digest pulls still need auth.docker.io tokens). Enforcement extended: any future `# syntax=` directive referencing a registry image must be digest-pinned (pin-if-present, same bar as FROM) — red-first committed (6 findings), then directives removed → green. The PR's Build jobs (amd64+arm64 per image) are the empirical re-verification that the builtin frontend suffices.
- **Finding 2 (hard gate) — thin tests.** Extracted the lint pass into pure `lintDockerfileContent(content) []string`; added `TestLintDockerfileContent` — 18 table-driven cases covering: bare/digest-pinned tags, `--platform` flags, scratch (incl. mixed case), stage-alias exemption (incl. case-insensitivity; not exercised by any live Dockerfile) and unknown-alias findings, lowercase `from`, truncated/uppercase-hex/sha512 digests, digest-without-tag, bare-digest ref, `${VAR}` base, comment/blank lines, and the three syntax-directive variants. The full exemption path now runs under test, not just helpers.
- **Finding 3 (minor) — walker gap + premature DONE.** Walker now also matches `*.Dockerfile`-suffixed files; digest-without-tag is its own finding (keep-the-tag policy from the issue). COORDINATE.md row moved from DONE to an honest "In review — r2 pushed" state.

### Review r2 on PR #1447 (REQUEST_CHANGES) — closed in r3

r2 confirmed: r1 hard gates closed, issue #1330 **fully addressed** (all digests independently re-resolved by both review passes and match; red-first history reproduced by execution; directive removal verified byte-level). One NEW gating finding + two minors, all fixed in r3 (fd3e5d6f red-first cases, then matcher fix):

- **Gating — directive-spelling bypass.** BuildKit's directive parser (frontend/dockerfile/parser/directives.go: `CutPrefix "#"` → `TrimLeftFunc IsSpace` → `^([a-zA-Z][a-zA-Z0-9]*)\s*=\s*(.+?)\s*$`) honors `#syntax=`, `#<tab>syntax=`, and `=`-padded spellings; the r2 check only matched the literal `# syntax=` prefix — a future unspaced directive would silently bypass the ratchet. Fixed with `buildkitSyntaxDirective()` implementing BuildKit's exact grammar (lowercase `syntax` name only — `#SYNTAX=` is NOT build-honored and stays a plain comment). 5 new directive-spelling cases + 2 port-vs-tag cases (25 total in `TestLintDockerfileContent`), committed red-first (RED at HEAD on exactly the 4 finding-encoding cases).
- **Minor — port mistaken for tag.** `pinnedImageRef` used `strings.Contains(name, ":")`; `registry:5000/img@sha256:…` over-accepted. Now the LAST path segment must carry the tag; port-only refs are findings.
- **Minor — over-broad PR-body claim.** "CI Build jobs are the empirical proof for all 6 images" was wrong: PR CI builds only the frontend image (other build jobs are `event_name != 'pull_request'`-gated). PR body corrected to scope the claim: frontend pins proven by PR CI builds; the other five images proven by two independent live registry resolutions + the issue-thread prior attempt's full builds; their first in-CI build proof lands post-merge.
- **Correction (append-only rule):** the "Files Modified" entry below saying COORDINATE.md went "claim → DONE" describes the r1-era commit only; COORDINATE.md's row itself was corrected to "In review" in r2 and tracks review state from here on.

CI note (r2 run): "Frontend (unit + typecheck + e2e)" and "Test (full suite, race detector)" went red once on unrelated flakes — Composer-attachments Playwright spec (146 passed/1 failed/1 flaky; zero frontend source in this diff) and pre-existing `TestRelayRearm_PreKillDeferredCheck_SkipsRelayKill` agentd timing flake (COORDINATE #1312 class). `gh run rerun --failed` → full CI green; the r2 reviewer independently attributed both to flakes, not this PR.

### Review r3 on PR #1447 (REQUEST_CHANGES) — closed in r4

r3 verified all r2 findings closed (digests re-resolved a third time, byte-exact; red-first chain re-executed; 0 directives repo-wide; merge-safety vs main re-validated — the skeptical pass's "would revert #1446/#1442" blocker was refuted as a two-dot-diff artifact, `merge-tree` 0 conflicts). One new gating finding, fixed in r4:

- **Gating — `#SYNTAX=` IS build-honored.** BuildKit's directive parser lowercases the captured key (`k := strings.ToLower(...)` in frontend/dockerfile/parser/directives.go, verified by the reviewer on master + v0.12.0) before matching `syntax` — so `#SYNTAX=docker/dockerfile:1.7` fetches the frontend from docker.io, while the r3 matcher treated it as a plain comment AND a table case enshrined that wrong expectation. **Correction (append-only): the r3-closure note above claiming "#SYNTAX= stays a plain comment (BuildKit matches lowercase)" was WRONG.** Fixed: key compared case-insensitively (`strings.EqualFold`), the case flipped to want:1 (committed red-first at 93dd18e2), comments corrected, and a lock-in case added for the deliberate all-lines directive superset (BuildKit stops directive parsing at the first instruction; the lint scans every line — over-enforcement in the safe direction).
- Non-gating, noted: branch behind main by a few commits — merge-tree clean; orchestrator merges, current-CI-signal refresh optional.

---

## Key Decisions

- **List/index digests, not per-arch**: pins must keep CI's buildx amd64+arm64 matrix working on both platforms; verified via Accept+Content-Type round-trip (see above).
- **Pin the sweep-found distroless images too**: issue listed 9; the bar is "every FROM". gcr.io isn't Docker Hub, but the supply-chain argument (tag move = silent base change) is identical.
- **Scratch and stage-local aliases exempt** (the only exemptions): scratch has no registry fetch; `FROM builder` is intra-file. No allowlist mechanism added — a future `${VAR}` base or new image without a digest should fail loudly and be pin-exempted deliberately, not silently.
- **Tags kept in front of `@sha256:`** for readability, per issue requirement.
- **Grouped digest bumps in renovate instead of automerge**: see renovate disposition above.
- **Syntax directives removed, not pinned** (r2): removal eliminates the docker.io frontend fetch entirely; a pinned directive would still fetch from docker.io (digest pulls still need auth.docker.io tokens) — the availability dependency the issue was filed for. Pin-if-present enforcement keeps future re-introductions honest.
- **Not addressing the ghcr.io mirroring half of the issue's ask**: mirroring bases into ghcr.io is an infra/registry change (credentials, cache hygiene) out of scope for a Dockerfile+enforcement PR; digest-pinning alone removes the tag-move class. Noted as follow-up.

---

## Blockers

None.

---

## Tests Run

- `go test ./pkg/repolint/ -run TestDockerfiles_BaseImagesDigestPinned -count=1` → RED pre-pin (13 findings), GREEN post-pin.
- r2: `-run 'TestLintDockerfileContent|TestDockerfiles_BaseImagesDigestPinned'` → RED on exactly the 6 syntax directives (after test hardening commit 3b566259), GREEN after directive removal; all 18 `TestLintDockerfileContent` cases pass at both points.
- r2: full `go test ./pkg/repolint/ ./local/ -count=1` → ok post-removal.
- Targeted feature grep over all 8 Dockerfiles (heredocs / `--mount` / `--parents` / `--checksum` / `--network`) → zero hits (evidence for directive removal).
- `go test ./pkg/repolint/ -count=1` → ok (full package: no regressions to arch/CA-bundle pins).
- `go test ./local/ -count=1` → ok (runtime-dockerfile retry pins unaffected).
- `go build ./...` → exit 0 (no Go changes; test-only + Dockerfile/JSON/MD edits).
- `jq empty renovate.json` → valid JSON.
- Registry re-query by digest (debian, distroless) → 200.
- No shell scripts touched → `bash -n` n/a.
- gitleaks: not installed in sandbox (hook skipped it); diff contains only public image digests — nothing secret-shaped. Pre-commit did **not** block, so `--no-verify` was not needed on any commit.

---

## Next Steps

- If Docker Hub flakiness recurs pre-pins-merge, consider the ghcr.io mirror half of #1330's ask as a separate infra task.
- Renovate will open the first "docker base images" digest-bump PR after merge; expect it to update golang/debian/node/nginx/distroless digests — review it like any base bump.

---

## Files Modified

- `api/Dockerfile` (2 FROMs pinned)
- `controller/Dockerfile` (2)
- `cmd/relay-proxy/Dockerfile` (2)
- `cmd/relay-router/Dockerfile` (2)
- `cmd/workspace-agentd/Dockerfile` (1)
- `frontend/Dockerfile` (2)
- `runtimes/base/Dockerfile` (1)
- `runtimes/opencode/Dockerfile` (1)
- `pkg/repolint/dockerfile_digest_pin_test.go` (new)
- `renovate.json` (docker base-images rule: +digest updateType)
- `README-LLM.md` (line 787 posture row)
- `worklogs/NNNN_2026-09-18_dockerfile-digest-pinning.md` (this file)
- `COORDINATE.md` (claim → DONE)
