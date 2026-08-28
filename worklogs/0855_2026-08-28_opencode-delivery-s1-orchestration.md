# Worklog: S1 opencode overlay delivery — orchestrator session

**Date:** 2026-08-28
**Session:** Implemented S1 of design 0053 / issue #1116 (opencodeDelivery — controller/helm wiring + supervisor verify/spawn) via the multi-agent workflow: 2 parallel implementation delegations (controller+helm; agentd supervisor), 1 skeptical validation round (0 BLOCKER, 2 MAJOR, 5 MINOR, 4 NIT), 1 remediation delegation (all real findings fixed, several mutation-proven), 1 re-validation round (all PROVEN FIXED, zero real findings).
**Status:** Complete (S1 code; CI/artifact half deferred — see Next Steps)

---

## Objective

Deliver the first story of platform overlay delivery: an opt-in, digest-pinned opencode image volume (`controller.opencodeDelivery`) mirroring #863 agentdDelivery — controller resolves per-arch binary sha256 pins from OCI index annotations (cached in `llmsafespaces-opencode-pins` ConfigMap), wires a read-only `/opencode` volume + env pins into workspace pods, the supervisor verifies the binary hash before first spawn and spawns from the overlay path; exit 83/84 surface as `WorkspaceConditionOpencodeVerified` conditions/events/metrics. Inert when unset.

---

## Work Completed

### 1. Parallel implementation delegations
- **Controller/helm** (worklog: `NNNN_2026-08-28_opencode-overlay-controller-helm.md`): generalized pin resolver (`overlay_pins.go`, second consumer justified per Rule 12) with byte-identical agentd behavior; `opencode_pins.go` + `opencode_overlay.go` (wiring, 83/84 detection, idempotency-per-episode, phase hooks); main.go flags + startup resolution + sentinel exit; conditions/reasons; `llmsafespaces_workspace_opencode_verify_failures_total` metric + critical alert; helm values/guards/RBAC (scoped get/update + unscoped create); 44 tests.
- **agentd supervisor** (worklog: `NNNN_2026-08-28_opencode-overlay-supervisor-verify.md`): single shared spawn seam (`opencodeSpawnBaseFactory`) used by both `--supervise` and `supervise-opencode`; verify-before-spawn exits 83 (verify failed) / 84 (overlay missing) with termination-log messages; marker-unset behavior pinned byte-identical to today; SetSpawnEnv adapter carries the resolved factory (caught a real regression where post-push restarts would have fallen back to PATH lookup); 26 tests.

### 2. Validation loop
- Round 1 findings all triaged real (except NIT-2 os.Exit/log.Sync — accepted, precedent-consistent) and fixed with regression tests, including: seam contract cross-check test (mutation-proven on 6 drift scenarios), controller-half worklog, values.yaml accuracy caveat, constant-usage fixes, single-attribution comment, rollout-down gate test, TestMain temp cleanup, filteredEnviron hardening.
- Pre-existing envtest breakage (`TestEnvtestPlatformInit_*`, "no kind registered for v1.Workspace") root-caused and fixed as setup-only changes (scheme registration via `testScheme(t)`, unique UIDs, container image, `dyn.Status().Update`) — assertions unchanged/strengthened; 5/5 envtest tests pass against a real API server.

---

## Key Decisions

- **Generalize the pins resolver now** — the second consumer (opencode) is funded, which is exactly README Rule 12's abstraction trigger; agentd instance kept byte-identical.
- **New exit codes 83/84 + separate condition/metric/alert** — 81/82 are agentd's; disjoint codes make cross-artifact failure attribution structurally impossible (dual-pinned by tests).
- **Seam contract mechanically pinned** — source-parsing contract test (precedent: `TestAgentdAnnotationKeys_MatchCIWorkflow`) so the two packages cannot drift silently.
- **Workspace-container-only mount** — the supervisor that spawns opencode is PID 1 of the workspace container; sidecar needs no mount (test-pinned).

---

## Blockers

None.

---

## Tests Run

- `go build ./...` — ok
- `go test -count=1 -timeout 600s ./controller/... ./helm/... ./pkg/apis/...` — ok
- `go test -count=1 -timeout 600s ./cmd/workspace-agentd/...` — ok (~152s)
- `go vet ./...` + `go vet -tags envtest ./controller/...` — clean
- `gofmt -l` on all changed packages — empty
- `-race` on new suites — ok
- envtest (platform-init ×2, agentd-pins ×3, real API server) — 5/5 pass

---

## Next Steps

- **S1 remainder (follow-up delegation):** standalone opencode artifact (`runtimes/opencode/Dockerfile`, FROM scratch, binary at `/usr/local/bin/opencode`), `build-opencode`/`merge-opencode` jobs in ci.yml + release.yml with per-arch sha256 OCI index annotations (`dev.llmsafespaces/opencode.sha256-*`), printed values block; extend/parallel the CI-annotation guard test. Land after PR #1118 (ci.yml) merges to avoid conflicts.
- Prune the time-boxed caveat in `helm/values.yaml` opencodeDelivery comment when that lands.
- Consider adding `controller/internal/workspace/platform_init_envtest_test.go` paths to the envtest workflow triggers if not already covered (the two fixed tests were dead in CI — they run in this PR's CI because we touch `controller/internal/workspace/**`).
- S2 (redact subcommand fold), S3 (base strip + mandatory pins), S4 (factory), S5 (kind suite + flip) per design 0053 §7.

---

## Files Modified

See the two delegation worklogs for full lists (25 modified + 13 new files across `controller/`, `cmd/workspace-agentd/`, `helm/`, `pkg/apis/`, `worklogs/`). This session additionally touched: this worklog only.
