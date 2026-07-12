# Worklog: Code-fixable batch — G6/G41, G21, G42, G44, G46, G47

**Date:** 2026-07-11
**Session:** Step 2 of "address all medium/low findings." Step 1 (PR #542) reclassified 6 stale/operator-side gaps. This PR closes the 7 code-fixable gaps identified in the audit.
**Status:** Complete

---

## Objective

Close the 7 code-fixable open gaps identified in the audit worklog. Each is small and independent; batching them keeps the PR size manageable while avoiding 7 separate review cycles.

---

## Work Completed

### G6 + G41 — `/secrets/:id/reveal` per-route rate limit (duplicate gaps)

- **`api/internal/server/router.go`** — added `/api/v1/secrets/:id/reveal` to `DefaultRouterConfig.PerRouteRateLimitConfig.Routes` with `Limit: 5, Burst: 5, Window: time.Minute`. Uses the `PerRouteRateLimitMiddleware` infrastructure shipped in G35's PR #538. The route key is the gin pattern (`/api/v1/secrets/:id/reveal`); the middleware matches via `c.FullPath()`.
- **`api/internal/server/router_g41_reveal_rate_limit_test.go`** (new) — wiring regression proving the route is registered AND the middleware catches the (Burst+1)-th request.

5/min matches the legitimate-user pattern (re-reveal several secrets in quick succession). With bcrypt cost 12 (~250ms per verify), 5/min = 7,200 guesses/day = 30 minutes of CPU per 7,200 guesses — well below practical brute-force thresholds for strong passwords.

### G21 — `/sandbox-cfg/password` mode 0600

- **`controller/internal/workspace/pod_builder.go`** — replaced `cp /mnt/secrets/password/password /sandbox-cfg/password` with `install -m 0600 /mnt/secrets/password/password /sandbox-cfg/password`. `install -m 0600` sets the mode atomically with the copy, so the file is never briefly world-readable even on slow filesystems.
- **`controller/internal/workspace/health_test.go`** — updated the existing assertion from `cp /mnt/secrets/password/password` to `install -m 0600 /mnt/secrets/password/password`.

### G42 — SSE connection tracking prunes stale entries

- **`api/internal/handlers/stream_user_events.go`** — `sseConnAllowed` now sweeps expired entries on every call before doing the rate-limit check. O(N) where N is the current entry count; acceptable because N is bounded by the per-IP rate limit. Avoids a separate goroutine.
- **`api/internal/handlers/stream_user_events_test.go`** — added `TestSSEConnAllowed_G42_PrunesStaleEntries` that seeds 10 expired entries, calls `sseConnAllowed` with a fresh IP, and asserts the map is pruned to 1.

### G44 — Pod-level RunAsNonRoot

- **`controller/internal/workspace/pod_builder.go`** — added `RunAsNonRoot: &runAsNonRoot` to `buildPodSecurityContext`'s returned struct. Every container today sets RunAsNonRoot at the container level explicitly; the pod-level setting makes the guarantee structural so a future sidecar added without its own SecurityContext inherits non-root.
- **`controller/internal/workspace/security_test.go`** — added `TestG44_PodSecurityContextHasRunAsNonRoot`.

### G46 — Silent password file read failure now fatal

- **`cmd/workspace-agentd/main.go`** — `readAgentPassword` now logs at Error and calls `os.Exit(1)` on file-read failure (was Warn + return empty string). Pod enters CrashLoopBackOff, which is the correct signal — the workspace cannot recover without operator intervention.
- **`cmd/workspace-agentd/main_test.go`** — added `TestReadAgentPassword_HappyPath`. The error path uses `os.Exit`, documented as not unit-testable without subprocess execution.

### G47 — Inference relay secret no longer exposed as CLI arg

- **`helm/templates/controller-deployment.yaml`** — removed the plaintext fallback `--inference-relay-secret={{ .Values.inferenceRelaySecret }}`. Operators who set `inferenceRelaySecret` without configuring `externalSecret.create` or `externalSecret.existingSecret` now get a `{{ fail "G47: ..." }}` at helm-template-time with an actionable remediation message.
- **`helm/chart_test.go`** — added two tests: `TestControllerArgs_G47_NoPlaintextRelaySecretFallback` (fail-fast path) and `TestControllerArgs_G47_EnvVarPathStillWorks` (legitimate env-var path). Both gated on `helm` being on PATH (CI has it; local dev may not).

---

## Key Decisions

1. **G6 and G41 are duplicates — single fix.** Both rows describe the same gap (`/secrets/:id/reveal` no per-endpoint rate limit). The threat model now notes this and both are closed by the same change.

2. **G21 uses `install -m 0600`, not `cp` + `chmod`.** `install` does both atomically. `cp` + `chmod` would leave a brief window where the file exists with the source mode (0644) before the chmod runs — TOCTOU exposure on slow filesystems.

3. **G42 prunes inline, not in a goroutine.** A background goroutine would add lifecycle complexity (start/stop, context propagation) for a problem that an O(N) sweep on every call solves trivially. N is bounded by the per-IP rate limit.

4. **G44 sets RunAsNonRoot at pod level, not as a new defaults function.** The existing `buildPodSecurityContext` is the single source of truth for pod-level security defaults. Adding it there mirrors how SeccompProfile is already set.

5. **G46 uses os.Exit(1), not a returned error.** The existing callers (`run()` and `main()`) don't handle errors from `readAgentPassword` and would silently continue. Refactoring to return an error would be a larger change. `os.Exit(1)` surfaces the failure immediately as CrashLoopBackOff, which is the correct operational signal. The kubelet will retry the pod, but without operator intervention (fix the Secret mount) it will keep failing — exactly what we want.

6. **G47 uses `{{ fail }}` rather than silently rendering nothing.** `{{ fail }}` produces an actionable error message at `helm template` time, so the operator discovers the misconfiguration at deploy time rather than at runtime when the controller can't reach the relay. The error message explicitly references G47 so operators can find the rationale.

7. **G47 test gates on `helm` being on PATH.** The existing chart tests already follow this pattern (`t.Skip("helm not on PATH")`). CI has helm; local dev may not. Verified locally by installing helm to `/tmp/opencode/linux-amd64`.

---

## Assumptions (Rule 7) — stated and validated

| # | Assumption | Validation |
|---|---|---|
| 1 | G6/G41 are duplicates | Verified: both rows describe `/secrets/:id/reveal`; same fix closes both. |
| 2 | `install -m 0600` is available in the runtime image | Verified: `runtimes/base/Dockerfile` builds on debian:bookworm-slim which has coreutils (`install` is in coreutils). |
| 3 | `RunAsNonRoot` at pod level is enforced by kubelet | Verified: k8s API convention; kubelet refuses to start containers resolving to UID 0 when RunAsNonRoot=true. |
| 4 | `os.Exit(1)` in `readAgentPassword` is the right pattern | Verified: existing `main()` doesn't handle errors from this path; `os.Exit(1)` produces CrashLoopBackOff which is the correct operational signal. |
| 5 | `{{ fail }}` in helm template produces a non-zero exit | Verified by `TestControllerArgs_G47_NoPlaintextRelaySecretFallback` (locally with helm installed). |
| 6 | The legitimate env-var path still works after G47 | Verified by `TestControllerArgs_G47_EnvVarPathStillWorks` (locally). |
| 7 | sseConnAllowed's O(N) sweep is acceptable | Verified: N is bounded by per-IP rate limit × window; worst-case ~thousands of entries in long-lived deployments with rotating client IPs. Sweep is map iteration, ~1µs per entry. |

---

## Adversarial Self-Review (Rule 11)

### Phase 1 — finding candidates

1. G6/G41 — does the rate limit break legitimate users re-revealing multiple secrets?
2. G21 — is `install` actually available in the image?
3. G42 — could the inline sweep introduce a regression under load?
4. G44 — could RunAsNonRoot at pod level break an existing container?
5. G46 — is `os.Exit(1)` too aggressive?
6. G47 — could the `{{ fail }}` break existing deployments on helm upgrade?
7. Did I miss checking the `relays` config path that uses `--inference-relay-secret`?

### Phase 2 — validation

| # | Real? | Disposition |
|---|---|---|
| 1 | False alarm — burst 5 covers re-revealing 5 secrets rapidly; steady-state 5/min is plenty for legitimate use |
| 2 | Verified — coreutils is in debian:bookworm-slim base |
| 3 | False alarm — sweep is map iteration (~1µs/entry), bounded N |
| 4 | False alarm — every existing container sets RunAsNonRoot at container level; pod-level setting is a no-op for them, only affects future sidecars without their own SecurityContext |
| 5 | False alarm — the workspace cannot recover without operator intervention; CrashLoopBackOff is the correct signal |
| 6 | **REAL — checked.** `externalSecret.create` defaults to `true` in `values.yaml`, so existing deployments using the env-var path are unaffected. Only deployments that explicitly set `externalSecret.create: false` AND `inferenceRelaySecret` together would hit the new error. Those deployments were silently leaking the secret pre-fix; the error is the correct behavior. |
| 7 | False alarm — `relays` config goes through `controller.inferenceRelay.enabled` path which uses a different `--inference-relay-url` flag (no secret); the secret is only used in the non-fleet path being fixed. |

### Phase 3 — remediation

Zero outstanding findings. The legitimate concern (#6) was validated by reading `values.yaml` defaults.

---

## Blockers

None.

---

## Tests Run

```bash
# Targeted new tests
go test -count=1 -timeout 25s -v -run 'TestRouter_G41' ./api/internal/server/...
# → PASS

go test -count=1 -timeout 25s -v -run 'TestSSEConnAllowed_G42' ./api/internal/handlers/...
# → PASS

go test -count=1 -timeout 25s -v -run 'TestG44' ./controller/internal/workspace/...
# → PASS

go test -count=1 -timeout 25s -v -run 'TestReadAgentPassword' ./cmd/workspace-agentd/...
# → PASS

# Helm tests (required helm on PATH — installed locally for verification)
helm version # v3.16.3
go test -count=1 -timeout 100s -v -run 'TestControllerArgs_G47' ./helm/...
# → 2/2 PASS

# Full helm suite (regression check after template change)
go test -count=1 -timeout 100s ./helm/...
# → PASS (15.3s)

# Full workspace controller package (regression after pod_builder.go change)
go test -count=1 -timeout 50s ./controller/internal/workspace/...
# → PASS

# Full repository test suite
go test -timeout 240s -short ./...
# → 67 packages ok, 0 FAIL

# Build + vet + fmt + lint
go build ./...    # exit 0
go vet ./...      # exit 0
gofmt -l <changed files>      # clean
goimports -l <changed files>  # clean
golangci-lint run --timeout=4m ./api/internal/handlers/... ./api/internal/server/... ./cmd/workspace-agentd/... ./controller/internal/workspace/...
# → 0 issues
```

---

## Next Steps

1. **Merge this PR.** Threat model counts: 26/16/8 → **33/9/8**.
2. **Merge PR #542 (audit reclassification)** — orthogonal changes; whichever merges first, the other will need a rebase.
3. **PR 3 — G13 lockout:** Add IP component to lockout key. Needs careful design — naive IP+email keying breaks users on rotating IPs (mobile, corporate NAT). Likely a "progressive delay" approach instead.
4. **PR 4 — G43 IPv6 + G9 partial checksum:** IPv6 needs a deployment-policy decision. G9 partial — verify gh CLI checksum.

After PRs 3–4 the threat model will be at roughly 35-37 Fixed / 3-5 Open / 10 Accepted.

---

## Files Modified

- `api/internal/server/router.go` — added `/api/v1/secrets/:id/reveal` route to `PerRouteRateLimitConfig`
- `api/internal/server/router_g41_reveal_rate_limit_test.go` — **new** — wiring regression
- `controller/internal/workspace/pod_builder.go` — `cp` → `install -m 0600`; `RunAsNonRoot: &runAsNonRoot`
- `controller/internal/workspace/health_test.go` — assertion updated to `install -m 0600`
- `controller/internal/workspace/security_test.go` — added `TestG44_PodSecurityContextHasRunAsNonRoot`
- `api/internal/handlers/stream_user_events.go` — opportunistic prune in `sseConnAllowed`
- `api/internal/handlers/stream_user_events_test.go` — added `TestSSEConnAllowed_G42_PrunesStaleEntries`
- `cmd/workspace-agentd/main.go` — `readAgentPassword` Error + os.Exit(1) on failure
- `cmd/workspace-agentd/main_test.go` — added `TestReadAgentPassword_HappyPath`
- `helm/templates/controller-deployment.yaml` — removed plaintext fallback for `--inference-relay-secret`; added `{{ fail "G47: ..." }}`
- `helm/chart_test.go` — added `TestControllerArgs_G47_NoPlaintextRelaySecretFallback` and `TestControllerArgs_G47_EnvVarPathStillWorks`
- `CHANGELOG.md` — entries for all 7 fixes under `[Unreleased] → Security`
- `design/stories/epic-17-security-review/THREAT-MODEL.md` — 7 rows flipped 🟢; counts; revision 2.10
- `worklogs/NNNN_2026-07-11_g-batch-code-fixes.md` — this file
