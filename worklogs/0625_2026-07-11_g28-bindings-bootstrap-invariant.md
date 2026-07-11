# Worklog: G28 — Reclassified as Accepted (architecture changed in Epic 35)

**Date:** 2026-07-11
**Session:** Address threat-model gap G28 (originally High, Open). Investigation revealed the gap is stale — Epic 35 (secretless injection) replaced the architecture the row described. Reclassified as Accepted with an invariant regression test.
**Status:** Complete

---

## Objective

Investigate and close G28 from `design/stories/epic-17-security-review/THREAT-MODEL.md`. The original row claimed: "`PUT /api/v1/workspaces/<id>/bindings` returns 204 but K8s Secret is never created. Investigate `pushSecretsToAgent` silent skip when bindings added to a freshly-created workspace."

The investigation found the row is **stale**. Epic 35 (secretless injection) removed the durable K8s Secret path entirely (`EnsureSecretsManifest` is gone). The architecture now:
1. `SetBindings` persists bindings to PostgreSQL `user_secret_bindings` inside an advisory-locked transaction (`pg_secret_store.go:301`).
2. The live HTTP push via `agentpush.Service.Push` is best-effort — `ErrNoRunningPod` is documented at `agentpush.go:70-75` as an accepted, transient state.
3. The init container fetches credentials at boot via `/internal/v1/pod-bootstrap`, which calls `GetBindings` to resolve what to inject.

The "no-op for first-time delivery" is the **intended behavior** in the new architecture: bindings are durable in PostgreSQL, and first-time delivery happens at pod boot.

---

## Work Completed

### Investigation (Rules 7, 8)

Verified each link in the new architecture:

1. **`SetBindings` persists to PostgreSQL.** Confirmed at `pkg/secrets/pg_secret_store.go:301-332` — `BEGIN tx → advisory_xact_lock → DELETE existing → INSERT new → COMMIT`. Durable.
2. **Bootstrap reads bindings at pod boot.** Confirmed at `api/internal/handlers/pod_bootstrap.go` and `pkg/secrets/secret_service.go:GetBindings` (reads `user_secret_bindings`).
3. **`ErrNoRunningPod` is documented as accepted.** Confirmed at `api/internal/services/agentpush/agentpush.go:70-75`: "Not a hard failure for the push flow: user-initiated callers surface 409, the pod-recreation auto-push logs at info and increments the 'no_pod' metric outcome."
4. **The handler's `pushSecretsToAgent` correctly handles `ErrNoRunningPod`.** Confirmed at `api/internal/handlers/secrets.go:442-446`: logs at info "reload-secrets skipped: no running pod" and returns success.

### Test

- **`pkg/secrets/secret_service_test.go`** — added `TestSecretService_G28_BindingsSurviveNoPodState`. Locks the persistence invariant: after `SetBindings` commits, `GetBindings` returns the same set — proving that a binding created while no pod is running will be visible to the bootstrap endpoint at the next pod boot. Uses one LLM-provider secret + one env-secret to cover both materialization paths.

### Documentation

- **`design/stories/epic-17-security-review/THREAT-MODEL.md`** — G28 row reclassified 🔴 Open → 🟡 Accepted with full rationale (Epic 35 architecture change, persistence path, bootstrap path, ErrNoRunningPod acceptance). Moved from Open list to Accepted list. Counts: 26 Fixed / 17 Open / 7 Accepted → 26 Fixed / 16 Open / 8 Accepted. Revision 2.9 added.
- **`CHANGELOG.md`** — entry under `[Unreleased] → Security` documenting the reclassification and the rationale.

---

## Key Decisions

1. **Reclassify as Accepted, not Fixed.** The threat-model "Fixed" classification means "remediated with regression test that prevents reintroduction." The new invariant test prevents reintroduction of the persistence gap, but the underlying behavior (live push is best-effort) is intentional, not a fix. **Accepted** is more accurate — the design consciously defers first-time delivery to pod boot.

2. **Add an invariant test even though the classification is Accepted.** The test locks the architectural invariant that the reclassification relies on (bindings survive the no-pod window). If a future refactor silently breaks the persistence guarantee, this test catches it before the reclassification becomes wrong.

3. **Don't add an e2e test that exercises the full bootstrap flow with a binding created before pod boot.** That would require envtest + fake agentd + fake K8s client — significant test infrastructure for a reclassification PR. The unit-level invariant test plus the existing `TestPodBootstrap_ValidToken_ReturnsSecrets` cover the two halves of the contract independently.

4. **Don't mark as Fixed just to make the count look better.** Misclassifying would hide the genuine architectural decision (defer to pod boot) from future readers.

---

## Assumptions (Rule 7) — stated and validated

| # | Assumption | Validation |
|---|---|---|
| 1 | G28 stale — Epic 35 removed the durable K8s Secret path | Verified at `secrets.go:414-418` comment: "Epic 35: the durable K8s Secret path (EnsureSecretsManifest) has been removed — secretless injection means the init container fetches credentials directly from the API at boot." |
| 2 | `SetBindings` persists to PostgreSQL | Verified at `pg_secret_store.go:301-332`. |
| 3 | Bootstrap reads bindings at pod boot | Verified at `pod_bootstrap.go` + `secret_service.go:GetBindings`. |
| 4 | `ErrNoRunningPod` is the intentional fallback | Verified at `agentpush.go:70-75` documentation. |
| 5 | Correct classification is Accepted, not Fixed | Validated by reading the threat-model legend at line 319: 🟡 Accepted = "risk accepted with documented rationale and compensating controls." Matches the architectural decision. |
| 6 | The invariant test belongs in `pkg/secrets/secret_service_test.go` | Verified — the test exercises `SecretService.SetBindings` → `SecretService.GetBindings` round-trip, which is the package's primary contract. |

---

## Adversarial Self-Review (Rule 11)

### Phase 1 — finding candidates

1. Am I sure G28 is stale, not real? (Sanity-check the architecture claim.)
2. Is "Accepted" the right classification, or should it be "Fixed"?
3. Should the threat model mention the Epic 35 architectural change explicitly?
4. Should I add an e2e test that exercises the full bootstrap flow?
5. The `bindResult` is unused (`_ = bindResult`). Is that a smell?

### Phase 2 — validation

| # | Real? | Disposition |
|---|---|---|
| 1 | False alarm — verified at unit level (persistence + bootstrap read paths) |
| 2 | Real classification decision — Accepted is correct (intentional design, not a fix) |
| 3 | Real — threat-model row needs to explain Epic 35 (addressed in the row update) |
| 4 | Acceptable — out of scope for a reclassification PR; existing tests cover the two halves independently |
| 5 | Acceptable — explicit unused-variable acknowledgement; the production handler uses the result for `MarkCredentialChanged`, the test only needs the err check |

### Phase 3 — remediation

No code findings. Threat-model row update addresses #3.

---

## Blockers

None.

---

## Tests Run

```bash
# Targeted G28 invariant test
go test -count=1 -timeout 25s -v -run 'TestSecretService_G28_BindingsSurviveNoPodState' ./pkg/secrets/...
# → PASS (0.32s)

# Full pkg/secrets package regression
go test -count=1 -timeout 60s ./pkg/secrets/...
# → PASS

# Full repository test suite
go test -timeout 240s -short ./...
# → 67 packages ok, 0 FAIL

# Build + vet
go build ./...    # exit 0
go vet ./...      # exit 0

# Lint
golangci-lint run --timeout=4m ./pkg/secrets/...
# → 0 issues

# Format
gofmt -l <changed files>      # clean
goimports -l <changed files>  # clean
```

---

## Next Steps

1. **Merge this PR.** All 6 originally-targeted High gaps (G38, G37, G35, G25, G36, G28) are now closed (5 Fixed + 1 Accepted). Threat model counts: 26 Fixed / 16 Open / 8 Accepted.
2. **Remaining open gaps** (lower severity): G4 (no mTLS API↔pod), G6 (per-endpoint secrets rate limit — partially addressed by G35's PerRouteRateLimitMiddleware, just needs the route added), G9 (opencode binary checksums), G13 (account-lockout DoS), G21 (sandbox-cfg password mode), G29 (path-traversal API-side mirror), G30 (DNS exfil), G40 (agentd user-port auth), G41-G47 (medium/low), G50 (decrypt audit not wired).
3. **Suggested next-PR order:** G41 is now a one-line addition (just add `/api/v1/secrets/:id/reveal` to `PerRouteRateLimitConfig.Routes` in `DefaultRouterConfig` — same shape as G35). G29 is a small validation mirror. G40 is the highest-impact remaining Medium.

---

## Files Modified

- `pkg/secrets/secret_service_test.go` — new test `TestSecretService_G28_BindingsSurviveNoPodState`
- `CHANGELOG.md` — entry under `[Unreleased] → Security`
- `design/stories/epic-17-security-review/THREAT-MODEL.md` — G28 row reclassified 🔴 → 🟡; moved Open → Accepted list; counts updated; revision 2.9
- `worklogs/0625_2026-07-11_g28-bindings-bootstrap-invariant.md` — this file
