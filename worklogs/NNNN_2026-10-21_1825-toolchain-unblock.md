# Worklog: #1825 — health-probe rate-limit exemption + the builder GOTOOLCHAIN carry (fleet unblock slice)

**Date:** 2026-10-21
**Session:** continuation worker (orchestrator dispatch: fold into #1825, single lane, no hotfix branch). Rebuilt in /workspace/llmsafespaces on the security-bumps-v2 branch rebased onto post-#1819 main (the pre-rebuild history lives on backup-1825-pre-rebase; its SHAs are cited as history, not as this diff).
**Status:** Complete

---

## Objective

Fix PR #1825's two red legs — the SDK canaries' `FAIL /livez: not rate-limited` and the pkg/secrets integration 32s fast-fail — inside the branch, and carry the fleet-unblock builder story (GOTOOLCHAIN=auto) so one merge unblocks canyon's CI, harbor's #1820, and #1826.

---

## Work Completed

- REBUILT ONTO POST-#1819 MAIN (5dcade32): the branch's earlier carries (go.mod 1.26.10, vite 8.1.6, the workflow go-version pins, the #1819 digest bumps) are ALL on main now — the rebuild drops every duplicate hunk; the net diff is exactly the unique content below.
- The five service builders set `ENV GOTOOLCHAIN=auto` — the ONLY builder delta in the net diff. #1819 (merged) re-pinned the five digests to 1.26.10-resolving images; auto composes ON TOP: the digest remains the pinned base, auto bridges any future minor gap between a digest's minor and go.mod's requirement.
- Diagnosed the canary failure: /livez + /readyz were never in the limiter's ExemptPaths — they passed only by refill-boundary luck after the canary's login burst; operationally a 429 on a k8s liveness probe restarts healthy pods. Added both to DefaultRouterConfig's ExemptPaths (router.go:328).
- Red-first pin `TestRouter_HealthProbesNeverRateLimited`: production wiring; (1,1) global limiter; an exhaustion CONTROL leg (429 observed on an ordinary route from the same IP — never assumed) before /livez is asserted exactly 200 and /readyz its typed 503 (stubbed pings = unreachable; the r1 panic class is structurally impossible with stubs). Verified RED pre-fix, GREEN post-fix; the r3 reviewer independently re-verified red/green by reverting the exemption at head.
- Review iterations r1→r3 (against the pre-rebuild heads): the false-green `!= 429` shape replaced with the discriminating exact-status + control form; the stale comments corrected (the remaining "unstubbed" mentions describe the pre-stub panic behavior, not the current mocks); the refill rationale states the mechanism (rate = limit → 1 token/second; DefaultWindow unused by the strategy; exhaustion is empirical); the CHANGELOG `(nothing yet)` placeholder removed — claimed in r3 but actually landed in r4 (the r3-era removal was a silent no-op; owned on the PR thread).

---

## Key Decisions

- `GOTOOLCHAIN=auto` kept alongside #1819's 1.26.10 digests: the digest is the base-image anchor; auto's toolchain fetch (module proxy/GOSUMDB) only engages when go.mod's minor exceeds the digest's — defense in depth, not a de-pinning (the r2-era "de-pin" note described the pre-#1819 world).
- /readyz unthrottled is intended (same k8s probe class as /livez); its unauthenticated backend pings are a load-amplification surface only where the ingress is publicly reachable.
- /health (the legacy /livez alias) stays limited: not a chart probe target; recorded as a deliberate follow-up decision for external deployments.
- The probe pin lives in `router_1652_rate_limit_test.go` (the existing rate-limit wiring pin file) rather than a new file — noted as a style preference by r3; kept to minimize churn in a hotfix slice.

---

## Blockers

None.

---

## Tests Run

- `go test ./api/internal/server/ ./api/internal/middleware/ -count=1` green.
- The probe pin `-race` green at head (also independently reproduced red/green by the r3 reviewer).
- golangci `--new-from-merge-base` 0 issues (pre-rebuild head; the rebuild introduces no Go changes beyond what was linted). CI: govulncheck, Trivy, Secrets Integration, and the SDK canaries green at the pre-rebuild head; the rebuild re-runs them.

---

## Next Steps

- Merge #1825 (the fleet chokepoint: canyon's CI, harbor's APPROVED #1820, #1826's legs).
- Hermetic follow-up consideration: GOTOOLCHAIN=auto + #1819's digests now coexist — if the digests are the sole desired anchor, auto can be dropped in a later housekeeping pass (it adds proxy egress as a build dependency).
- Decide the `/health` alias's limiter class deliberately.
- mutation.yml's `GOTOOLCHAIN: local` + `continue-on-error: true` combination remains a latent silent-green risk (observed by the r3 skeptical pass; unchanged here).

---

## Files Modified

`api/internal/server/router.go`, `api/internal/server/router_1652_rate_limit_test.go`, `api/Dockerfile`, `cmd/relay-proxy/Dockerfile`, `cmd/relay-router/Dockerfile`, `cmd/workspace-agentd/Dockerfile`, `controller/Dockerfile`, `CHANGELOG.md`, this worklog. (Exactly the nine-file net diff vs post-#1819 main — dedupe verified by content, not filename.)
