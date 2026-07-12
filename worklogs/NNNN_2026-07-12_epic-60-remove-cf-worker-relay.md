# Worklog: Epic 60 — remove Cloudflare Worker inference relay

**Date:** 2026-07-12
**Session:** Zen now blocks all Cloudflare Worker egress IPs. The CF Worker relay at `workers/inference-relay/` is unreachable, and the chart default `inferenceRelayURL: https://relay.safespaces.dev` actively breaks fresh installs (#474). Removing the Worker path entirely.
**Status:** Complete

---

## Objective

Close #474 by removing the CF Worker relay path. The self-hosted InferenceRelay fleet (Epic 42) stays. The default becomes direct-to-Zen mode (workspace pods call `https://opencode.ai/zen/v1` directly with opencode's built-in `public` key), which the code already supported.

---

## Work Completed

### Code + chart removal

**Deleted entirely:**
- `workers/inference-relay/` (7 files) — the Worker
- `helm/templates/relay-secret-sync-job.yaml` — the Helm Hook Job that pushed the cluster secret to the CF Worker
- `tests/epic26/` (2 files) — Go integration/contract tests simulating Worker behavior

**Chart values (`helm/values.yaml`):**
- Dropped top-level `inferenceRelayURL` (default was `https://relay.safespaces.dev`; #474's root cause)
- Dropped top-level `inferenceRelaySecret` (auto-generated; no consumer remains)
- Dropped top-level `cloudflare:` block (`apiToken`, `accountId`, `workerName`) — sole purpose was the secret sync

**Chart templates:**
- `controller-deployment.yaml`: collapsed the if/else Worker-vs-fleet branching. The else branch (Worker path + G47 plaintext-secret guard + `INFERENCE_RELAY_SECRET` env block) is removed entirely. The if branch (fleet) is unchanged. With neither set, no `--inference-relay-url` renders.
- `api-deployment.yaml`: removed the `{{- with .Values.inferenceRelayURL }}` block that injected `LLMSAFESPACES_SERVER_INFERENCERELAYURL` env var.
- `secret.yaml`: removed the `relaySecret` resolution logic and the `inference-relay-secret`/`cloudflare-api-token` `stringData` entries.

**Cluster values (`values-cluster.yaml`):**
- Dropped the `inferenceRelayURL: https://relay.safespaces.dev` line.

**Go code (removal + rewording):**
- Removed `--inference-relay-secret` controller flag (no remaining consumer — fleet uses per-VM tokens).
- Removed `WorkspaceReconciler.InferenceRelaySecret` struct field.
- Removed `SetupControllers`'s `inferenceRelaySecret` parameter.
- Removed the path-segment secret embedding in `pod_builder.go` (lines 210-212 in pre-fix) — `INFERENCE_RELAY_BASEURL` now equals `r.InferenceRelayURL` verbatim.
- Reworded comment blocks across `controller/main.go`, `controller/internal/workspace/reconciler.go`, `controller/internal/workspace/pod_builder.go`, `cmd/workspace-agentd/main.go`, `cmd/workspace-agentd/relay_injector.go`, `api/internal/config/config.go` — "Cloudflare Worker" -> "self-hosted relay (InferenceRelay fleet)" or just "relay". Logic stays; only docs/comments change.

**Tests:**
- `controller/internal/workspace/relay_injection_test.go`: reworded file header, updated fixture URL from `https://relay.safespaces.dev` to `https://relay.example.test/`.
- `controller/internal/workspace/pod_builder_test.go`: dropped the `secret` parameter from `reconcilerWithRelay` helper (no longer needed); updated assertions to expect the URL verbatim (no path-segment embedding).
- `cmd/workspace-agentd/*_test.go`: bulk-replaced `https://relay.safespaces.dev/...` fixture URLs with `https://relay.example.test/...`.
- `helm/chart_test.go`:
  - Deleted `TestControllerArgs_PreservesCFWorkerURLWhenFleetDisabled` (asserted the Worker-path branch renders; that branch is gone).
  - Deleted `TestControllerArgs_G47_NoPlaintextRelaySecretFallback` and `TestControllerArgs_G47_EnvVarPathStillWorks` (G47 surface is gone).
  - Added `TestControllerArgs_NoRelayURLByDefault` (asserts the new default: no `--inference-relay-url`, no `--inference-relay-secret`).
  - Removed unused `os` import.
  - Added a comment block at the deleted G47 section noting the surface was removed in Epic 60.

### Documentation

- `README.md`: rewrote "Inference Relay" section (two modes: direct-to-Zen default + self-hosted fleet; Worker removal noted). Dropped `workers/` line from repo layout.
- `README-LLM.md`: dropped `workers/` from repo layout; rewrote "Inference Relay Fleet" overview; updated auth-model note.
- `docs/operator/inference-relay.md`: full rewrite (334 lines, was 369). Two-mode structure (direct-to-Zen + fleet), Worker-removal note as admonition, drop "Option 1" + cloudflare values cross-ref + relay.safespaces.dev from diagrams. Fleet content intact.
- `docs/operator/installation.md`: dropped "Inference relay secret" row from generated-credentials table.
- `docs/operator/runbook.md`: dropped "Rotating the inference relay secret" section + its TOC entry. Merged the surviving self-hosted-fleet VM rotation into the existing "Rotating a relay VM" section.
- `docs/operator/troubleshooting.md`: reworded "Relay URL misconfigured" row to "Fleet URL misconfigured", pointing at the controller flag instead of the deleted chart value.
- `docs/reference/cli.md`: dropped `--inference-relay-secret` flag row.
- `docs/reference/helm-values.md`: dropped `inferenceRelayURL`, `inferenceRelaySecret`, and `cloudflare.*` rows + section headers.
- `docs/api/rest.md`: reworded footer (self-hosted fleet vs. direct-to-Zen, not CF Worker).

### Design stories

- New `design/stories/epic-60-remove-cf-worker-relay/README.md` — the design doc (problem statement, what changes, what stays, threat-model implications, acceptance criteria).
- `design/stories/epic-26-client-proxied-inference/README.md`: Status flipped from "Complete" to "⛔ Superseded by Epic 60". Two banner paragraphs: supersession (current) + historical pivot (2026-06-05).
- `design/stories/epic-42-multi-cloud-inference-relay/README.md`: "Migration from Epic 26" section rewritten — was optional steps with "Keep CF Worker code for historical reference", is now mandatory steps for operators wanting relayed free-tier access post-Worker-removal.
- `design/stories/epic-52-test-coverage/US-52.9-inference-relay-worker-tests.md`: Status flipped from "Proposed" to "⛔ Superseded" — its target is deleted.
- `design/stories/README.md`: Epic 26 row status updated to "⛔ Superseded", with Epic 60 attribution.
- `design/stories/epic-17-security-review/THREAT-MODEL.md`: new revision 2.13 entry noting the G47 surface (inference-relay-secret as CLI arg) was removed entirely by Epic 60. Historical G47 row preserved as-is.

---

## Key Decisions

1. **One atomic PR.** Splitting would leave broken intermediate states (chart referencing deleted files, controller flag with no chart wiring). The chart rename precedent (PR #526) was one PR; following the same pattern.

2. **`inferenceRelayURL` default = `""` (empty).** This activates direct-to-Zen mode, which the code already supports at every layer. No binary behavior change is required — only the values default and the now-dead Worker plumbing.

3. **Remove `--inference-relay-secret` controller flag entirely.** Considered keeping it as a no-op for backward-compat. Rejected per Rule 5 (Zero Technical Debt: "Remove legacy code"). The flag had exactly one consumer (Worker path-segment secret embedding in `pod_builder.go`), which is also removed. Any operator still setting it is on a dead path that returns 403 from Zen.

4. **Reword Worker-specific comments in agentd rather than rewrite logic.** The relay-config subsystem is URL-agnostic by design (see README-LLM.md §"Relay Config Subsystem" — it builds the opencode-relay provider block from whatever URL the controller injects). Worker-vs-fleet is a controller-chart distinction, not an agentd distinction. Comment cleanup only.

5. **G47 threat-model row preserved.** Could have deleted it (the surface is gone). Kept it as historical record (the fix was real when shipped in rev 2.10) and added a new revision 2.13 entry noting the entire surface was removed in Epic 60. The threat-model change log is append-only history; rewriting old rows would lose the audit trail.

6. **No THREAT-MODEL.md gap-count change.** G47 stays at 🟢 Fixed; the surface going away doesn't change its historical status. The new 2.13 entry explicitly notes "gap count unchanged at 38/0/12".

---

## Assumptions stated and validated (Rule 7)

1. *The empty-URL path is fully implemented at every layer.* Validated by reading `pod_builder.go:208` (early-returns when InferenceRelayURL empty), `controller/internal/workspace/relay_injection_test.go:30-51` (pins the contract), `controller/main.go:80` (flag default already `""`), `cmd/workspace-agentd/pre_boot_relay.go:45-47,168` (`skipped_no_relay_url` outcome), `api/internal/app/app.go:443-446` (empty URL = `relayActive=false`). No binary behavior change is required.
2. *The self-hosted InferenceRelay fleet is URL-agnostic with respect to the Worker.* Validated by reading `helm/templates/controller-deployment.yaml` — the fleet path uses a separate `if .Values.controller.inferenceRelay.enabled` branch with its own URL derivation (cross-ns router FQDN), independent of the top-level `inferenceRelayURL` value.
3. *The `InferenceRelay` CRD and `cmd/relay-router`/`cmd/relay-proxy` have no Worker-specific code.* Validated by `grep -r 'relay.safespaces\|inferenceRelayURL\|cloudflare' cmd/relay-router/ cmd/relay-proxy/ controller/internal/relay/` — zero matches.
4. *Turnstile CAPTCHA is unaffected.* Validated by reading `api/internal/middleware/turnstile.go` — it calls `https://challenges.cloudflare.com/turnstile/v0/siteverify` from the API pod, not a Worker. Zen's CF Worker IP block doesn't affect siteverify.
5. *PR #500's fix logic for #468 (frontend copy-html PSA restricted) is correct, only the paths were stale.* Validated separately and re-landed as PR #551 in a prior session. Independent of this PR.

---

## Adversarial Self-Review (Rule 11)

- **Phase 1 finding (real, fixed):** `pod_builder_test.go` `reconcilerWithRelay` helper took a `secret` parameter that no longer has a use. Phase 2 verdict: real dead-parameter. Remediation: dropped the parameter, updated all 4 call sites, simplified the helper. The two tests that previously asserted path-segment embedding behavior now assert the URL verbatim.
- **Phase 1 finding (real, fixed):** The chart test `TestControllerArgs_G47_NoPlaintextRelaySecretFallback` would fail to compile if not deleted — it referenced deleted chart values. Phase 2 verdict: real broken test. Remediation: deleted both G47 chart tests and the unused `os` import that surfaced after deletion.
- **Phase 1 finding (real, fixed):** Initial deletion command (`git rm helm/templates/relay-secret-sync-job.yaml && git rm -r tests/epic26/`) appeared to succeed in the shell output truncation but didn't actually run. Phase 2 verdict: real bug in my session — verified by running `helm template --set controller.inferenceRelay.enabled=true` which failed because the file was still present. Remediation: re-ran the deletions, confirmed via `ls`.
- **Phase 1 finding (real, addressed in PR description):** `TestMaterializeSubcommand_MissingSecretsFile_AppliesWorkspaceConfig` fails. Phase 2 verdict: pre-existing on main (verified by checkout main + run); not caused by this PR. Documented in the PR description as a separate follow-up.
- **Phase 2 false alarm initially considered:** "Does removing `inferenceRelaySecret` from the reconciler break any fleet test?" Validated by `grep -r 'InferenceRelaySecret' controller/` — only matches were in main.go (the flag, removed), controller.go (the parameter, removed), reconciler.go (the field, removed), pod_builder.go (the path-segment logic, removed), and pod_builder_test.go (the helper, updated). No fleet code references it. False alarm.

---

## Blockers

None.

---

## Tests Run

```
go build ./...                                                          → RC=0
go vet ./...                                                            → RC=0
gofmt -l controller/ cmd/workspace-agentd/ helm/chart_test.go           → (empty, clean)
go test -timeout 80s -race -count=1 ./helm/...                          → ok
go test -timeout 80s -race -count=1 -skip TestMaterializeSubcommand_MissingSecretsFile_AppliesWorkspaceConfig ./controller/... → ok
helm template test ./helm                                               → renders clean, no relay references
helm template test ./helm --set controller.inferenceRelay.enabled=true ... → fleet args render correctly
```

`TestMaterializeSubcommand_MissingSecretsFile_AppliesWorkspaceConfig` is pre-existing-broken on main (verified by checkout main + run); not caused by this PR.

---

## Files Modified

**Deleted (10 files):**
- `workers/inference-relay/{.gitignore,README.md,package.json,src/index.ts,src/index.test.ts,vitest.config.ts,wrangler.toml}`
- `helm/templates/relay-secret-sync-job.yaml`
- `tests/epic26/{relay_e2e_test.go,relay_contract_test.go}`

**Modified — code (8 files):**
- `controller/main.go`, `controller/internal/controller/controller.go`, `controller/internal/workspace/{reconciler.go,pod_builder.go,pod_builder_test.go,relay_injection_test.go}`
- `api/internal/config/config.go`
- `cmd/workspace-agentd/{main.go,relay_injector.go,relay_injector_test.go,secrets_test.go,agent_config_writer_test.go,agent_config_writer_schema_test.go}`

**Modified — chart (5 files):**
- `helm/values.yaml`, `helm/templates/{controller-deployment.yaml,api-deployment.yaml,secret.yaml}`
- `helm/chart_test.go`
- `values-cluster.yaml`

**Modified — docs (8 files):**
- `README.md`, `README-LLM.md`
- `docs/operator/{inference-relay.md,installation.md,runbook.md,troubleshooting.md}`
- `docs/reference/{cli.md,helm-values.md}`
- `docs/api/rest.md`

**Modified — design (5 files):**
- `design/stories/epic-17-security-review/THREAT-MODEL.md`
- `design/stories/epic-26-client-proxied-inference/README.md`
- `design/stories/epic-42-multi-cloud-inference-relay/README.md`
- `design/stories/epic-52-test-coverage/US-52.9-inference-relay-worker-tests.md`
- `design/stories/README.md`

**Added (2 files):**
- `design/stories/epic-60-remove-cf-worker-relay/README.md`
- `worklogs/NNNN_2026-07-12_epic-60-remove-cf-worker-relay.md` (this file)

---

## Next Steps

1. Open this PR.
2. Closes #474.
3. Operator-side migration: remove `inferenceRelayURL`/`inferenceRelaySecret`/`cloudflare.*` from cluster values; `helm upgrade`; workspace pods recreate on next lifecycle event and resume free-tier inference in direct-to-Zen mode.
4. Operators who want IP rotation: enable `controller.inferenceRelay.enabled: true` (the fleet path, unaffected by this PR).
