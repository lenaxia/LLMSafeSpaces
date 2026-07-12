# Epic 60: Remove Cloudflare Worker Inference Relay

**Status:** Approved (operator decision 2026-07-12)
**Priority:** High
**Supersedes:** Epic 26 (Client-Proxied Inference — CF Worker architecture)
**Depends On:** Epic 42 (self-hosted InferenceRelay fleet, the surviving path)
**Motivation:** Zen (opencode.ai/zen/v1) now blocks all Cloudflare Worker egress IPs. The CF Worker relay at `relay.safespaces.dev` is unreachable from any workspace pod. The chart default `inferenceRelayURL: https://relay.safespaces.dev` shipped in `helm/values.yaml` actively breaks fresh installs (#474): the auto-generated cluster secret never matches what's deployed at the Worker, so every workspace free-tier request returns 403, opencode interprets that as a credential failure, and the agent restarts itself in a loop.

---

## Problem Statement

### Current State

The platform ships **two** inference relay paths for free-tier Zen models, selected by the configured relay URL:

1. **CF Worker relay (Epic 26, shipped 2026-06-05).** A 37-line Cloudflare Worker at `workers/inference-relay/` deployed to `relay.safespaces.dev`. Workspace pods embed the per-cluster `inferenceRelaySecret` as a path segment (`https://relay.safespaces.dev/<secret>/responses`); the Worker proxies to `https://opencode.ai/zen/v1`. Secret distribution: the chart's `relay-secret-sync` Helm Hook Job pushes the cluster's random-generated secret to the Worker via the Cloudflare API at install/upgrade time.

2. **Self-hosted InferenceRelay fleet (Epic 42, shipped).** A `controller-runtime` reconciler that provisions relay VMs across AWS/OCI/GCP via cloud-init, health-checks them, and rotates them on 429 storms. The router (`cmd/relay-router/`) distributes workspace traffic across healthy VMs with per-VM token auth. Opt-in via `controller.inferenceRelay.enabled: true`.

The CF Worker was the simpler path: one Worker, no VMs, no cloud spend. It was the chart default.

### Why it has to go

Zen (opencode.ai/zen/v1) now blocks all Cloudflare Worker egress IPs. The Worker is unreachable. Workspace requests return 403/timeout from every Worker POP, regardless of secret validity. The "simple path" is now a no path.

The chart's `inferenceRelayURL: https://relay.safespaces.dev` default compounds the problem (#474): fresh installs that don't override it get HTTP 403 on every free-tier call, surfaced in the UI as "Agent is restarting (credential change, OOM, or crash)" — a misleading error that hides the relay-auth root cause.

### What does NOT change

- **The self-hosted InferenceRelay fleet (Epic 42) stays.** It uses per-VM token auth over HTTP to relay VMs we control, not the CF Worker. It is the recommended path for operators who want IP diversity on free-tier Zen access.
- **The "direct-to-Zen" mode stays.** When `inferenceRelayURL: ""` (the new default), workspace pods call `https://opencode.ai/zen/v1` directly using opencode's built-in `public` anonymous key. This is the right default for homelab and small-team deploys that don't need IP rotation. The code path is already implemented and tested at every layer (`pod_builder.go:208`, `workspace-agentd/pre_boot_relay.go`, `api/internal/app/app.go:443-446`).
- **The relay-config subsystem stays.** That subsystem (`cmd/workspace-agentd/relay_injector.go`, `agent_config_writer.go`, etc.) is URL-agnostic: it builds the `opencode-relay` provider block in `agent-config.json` from whatever URL the controller injects. It works identically for Worker, fleet, or any future relay. Only the Worker-specific comments and example URLs need rewording.
- **Cloudflare Turnstile CAPTCHA stays.** Turnstile is a browser widget that calls Cloudflare's siteverify API from the user's browser; it is not a Worker and is not affected by Zen's Worker IP block. (Confirmed: shipped 2026-07-04, worklog 0595.)

---

## Design

### Shape of the removal

One atomic PR. Splitting would leave broken intermediate states (chart referencing deleted files, controller flag with no chart wiring, etc.).

**Delete entirely:**
- `workers/inference-relay/` — 7 files, the Worker itself
- `helm/templates/relay-secret-sync-job.yaml` — the Helm Hook Job that pushed the cluster secret to the Worker
- `tests/epic26/` — 2 files, the Go integration/contract tests simulating Worker behavior

**Chart values (`helm/values.yaml`):**
- Drop the top-level `inferenceRelayURL` value (default was `https://relay.safespaces.dev`; new behavior: empty).
- Drop the top-level `inferenceRelaySecret` value (was auto-generated; no consumer remains).
- Drop the top-level `cloudflare:` block (`apiToken`, `accountId`, `workerName`) — its sole purpose was pushing the secret to the Worker.

**Chart templates:**
- `controller-deployment.yaml`: collapse the `{{- if .Values.controller.inferenceRelay.enabled }}...{{- else }}{{- with .Values.inferenceRelayURL }}...{{- end }}{{- end }}` branching. The `else` branch (Worker path, G47 plaintext-secret guard, `INFERENCE_RELAY_SECRET` env block) is removed entirely. The `if` branch (fleet) is unchanged. With neither set, no `--inference-relay-url` flag renders.
- `api-deployment.yaml`: remove the `{{- with .Values.inferenceRelayURL }}` block that injects `LLMSAFESPACES_SERVER_INFERENCERELAYURL` env var.
- `secret.yaml`: remove the `relaySecret` resolution logic + the `inference-relay-secret` and `cloudflare-api-token` `stringData` entries. The Secret shrinks back to holding only the credentials it actually needs.

**Cluster values (`values-cluster.yaml`):**
- Drop the `inferenceRelayURL: https://relay.safespaces.dev` line.

**Go code (comment/flag rewording only; no behavior change):**
- `controller/main.go:79-86`: the `--inference-relay-url` and `--inference-relay-secret` flags keep working (the self-hosted fleet still uses `--inference-relay-url`). Help strings reworded from "Cloudflare Worker URL for free-tier inference relay" to "Self-hosted relay URL (InferenceRelay fleet) for free-tier inference relay." `--inference-relay-secret` is removed: no remaining consumer uses path-segment secrets (the fleet uses per-VM tokens).
- `controller/internal/workspace/reconciler.go:31-42`: reword struct field doc comments from "Cloudflare Worker URL" to "Self-hosted relay URL." Drop `InferenceRelaySecret` field entirely — it has no consumer post-removal.
- `controller/internal/workspace/pod_builder.go:203-216`: reword the comment block. The URL-embedding logic stays (the fleet still needs the URL injected); the path-segment secret embedding is removed (no fleet consumer).
- `cmd/workspace-agentd/{main.go,relay_injector.go,pre_boot_relay.go}`: reword file-header and inline comments from "CF Worker relay" to "self-hosted relay (InferenceRelay fleet)" or just "relay." Logic stays.
- `api/internal/config/config.go:34-36`: keep `Server.InferenceRelayURL` field (the API still needs to know whether a relay is configured to drive `ModelsHandler.SetRelayActive`). Reword the doc comment.

**Go test fixtures (URLs only; logic stays):**
- `controller/internal/workspace/relay_injection_test.go`: change `https://relay.safespaces.dev` to a neutral `https://relay.example.test/`. Reword the file-header comment.
- `cmd/workspace-agentd/{relay_injector_test.go,secrets_test.go,agent_config_writer_test.go,agent_config_writer_schema_test.go}`: same URL substitutions.

**Chart tests (`helm/chart_test.go`):**
- Delete `TestControllerArgs_PreservesCFWorkerURLWhenFleetDisabled` — asserts the Worker-path branch renders; that branch is gone.
- Delete `TestControllerArgs_G47_NoPlaintextRelaySecretFallback` and `TestControllerArgs_G47_EnvVarPathStillWorks` — both exercise the G47 mechanism that guarded the Worker-secret env interpolation; with the Worker gone, G47 is moot.
- Keep `TestControllerArgs_RoutesWorkspacesThroughRouterWhenFleetEnabled` (fleet wins over a stale `inferenceRelayURL` when both are set) — still meaningful as a regression guard, but the test's setup changes: with `inferenceRelayURL` no longer a chart value, the test simulates the legacy state by passing it via `--set` to verify the chart's `if` branch still wins.
- Add `TestControllerArgs_NoRelayURLByDefault` — asserts that with no chart overrides, no `--inference-relay-url` flag renders at all (the new default state).

**Documentation:**
- `README.md`: drop the "two interchangeable deployments" framing. State that the CF Worker relay is removed (Epic 60) and the self-hosted InferenceRelay fleet is the relay option; the default is direct-to-Zen.
- `README-LLM.md`: same. Update repo layout, "Inference Relay Fleet" section, "Relay Config Subsystem" section's auth-model note.
- `docs/operator/inference-relay.md`: rewrite the page. Drop "Option 1: CF Worker relay." Document the two remaining options: direct-to-Zen (default) and self-hosted fleet.
- `docs/operator/runbook.md`: drop the "Rotating the inference relay secret" section's CF Worker subsection.
- `docs/operator/installation.md`: drop the "Inference relay secret" row from the generated-credentials table.
- `docs/operator/troubleshooting.md`: drop the `inferenceRelayURL` row from the relay-misconfiguration diagnosis, or reword to point at the fleet.
- `docs/reference/helm-values.md`: drop the `inferenceRelayURL`, `inferenceRelaySecret`, and `cloudflare.*` rows.
- `docs/reference/cli.md`: drop the `--inference-relay-secret` flag row.
- `docs/api/rest.md`: reword the footer referencing the CF Worker.
- `design/stories/epic-26-client-proxied-inference/README.md`: mark Status as `⛔ Superseded by Epic 60`. Add supersession banner.
- `design/stories/epic-42-multi-cloud-inference-relay/README.md`: update the "Migration from Epic 26" section — the migration is now mandatory (the Worker is dead), not optional. Drop the "Keep CF Worker code for historical reference" line.
- `design/stories/README.md`: flip Epic 26 status to `⛔ Superseded`; add Epic 60 row as `✅ Complete`.
- `design/stories/epic-52-test-coverage/US-52.9-inference-relay-worker-tests.md`: supersede the story (its target is deleted).
- `design/stories/epic-52-test-coverage/README.md`: update the assumption row referencing the Worker's testability.

**Worklogs:** historical record. Not edited. The PR description notes they contain now-stale references to the Worker; this is the project's standard practice (`worklogs/0507_2026-06-22_worklog-sentinel-naming-scheme.md` documents the append-only discipline).

### `inferenceRelayURL` default after removal

`""` (empty string). This activates direct-to-Zen mode:

- The chart renders no `--inference-relay-url` flag, no `INFERENCE_RELAY_SECRET` env var, no `LLMSAFESPACES_SERVER_INFERENCERELAYURL` env var, no `relay-secret-sync` Job.
- The controller's reconciler leaves `InferenceRelayURL=""`, so `pod_builder.go:208` skips `INFERENCE_RELAY_BASEURL` injection.
- `workspace-agentd` sees no `INFERENCE_RELAY_BASEURL`; `maybeStartRelayInjector` and `applyRelayConfigPreBoot` both no-op with outcome `skipped_no_relay_url`.
- The API's `ModelsHandler.SetRelayActive` is never called; `relayActive` stays `false`; no `opencode -> opencode-relay` providerID remap occurs; opencode calls `https://opencode.ai/zen/v1` directly using the built-in `public` free-tier key.

This is exactly the mode #474 calls for. No binary behavior change is required — the empty-URL path was already implemented and tested.

### Backwards compatibility

**Operators currently running the CF Worker relay** (i.e. those with a non-empty `inferenceRelayURL` in their cluster values): they need a one-time migration. Since the Worker is already unreachable, there is no in-flight traffic to preserve. The migration is:

1. Remove `inferenceRelayURL` and `inferenceRelaySecret` from cluster values.
2. `helm upgrade` — chart renders without the Worker plumbing.
3. Workspace pods recreate (next suspend/activate cycle, or manual restart); they come up in direct-to-Zen mode and free-tier inference resumes immediately.

**Operators who want IP rotation on free-tier Zen access** (the original motivation for the Worker): they should enable the self-hosted InferenceRelay fleet (`controller.inferenceRelay.enabled: true`). That path is unaffected by this PR.

**Operators who set `cloudflare.apiToken` etc.**: those values become ignored. Document this in the upgrade notes.

### What this PR explicitly does NOT do

- Does not remove the `InferenceRelay` CRD, the `controller/internal/relay/` reconciler, `cmd/relay-router/`, `cmd/relay-proxy/`, or the `api/internal/handlers/relay_admin.go` admin wizard. Those are the self-hosted fleet, not the Worker.
- Does not remove the relay-config subsystem inside `workspace-agentd`. That subsystem is URL-agnostic and serves the fleet path identically.
- Does not remove the `--inference-relay-url` controller flag. The fleet still uses it (set to the in-cluster router FQDN).
- Does not remove Turnstile or any other Cloudflare integration. Turnstile is not a Worker.

---

## Threat-model implications

The CF Worker relay's exposure (free-tier Zen access only, no paid credentials, no user data) is documented as an accepted trade-off in the Epic 42 design doc. Removing it does not change the threat model — there is no remaining attack surface to model. The self-hosted fleet's threat model is already documented in `design/stories/epic-42-multi-cloud-inference-relay/README.md`.

The threat-model row `G9 (opencode binary not checksum-verified at build) — Accepted` is unrelated. No THREAT-MODEL.md update required.

---

## Acceptance Criteria

1. `workers/inference-relay/` directory no longer exists.
2. `helm/templates/relay-secret-sync-job.yaml` no longer exists.
3. `tests/epic26/` directory no longer exists.
4. `helm/values.yaml` has no top-level `inferenceRelayURL`, `inferenceRelaySecret`, or `cloudflare:` block.
5. `helm template` with default values produces no `--inference-relay-url` flag and no `INFERENCE_RELAY_SECRET` env var.
6. `helm template` with `controller.inferenceRelay.enabled: true` still renders the fleet wiring (unchanged).
7. All `helm/chart_test.go` tests pass. The new `TestControllerArgs_NoRelayURLByDefault` passes. The deleted Worker-specific tests are gone.
8. `go build ./...` and `go test -race ./...` pass.
9. `golangci-lint` and `gofmt` are clean.
10. No Go file references "Cloudflare Worker" or `relay.safespaces.dev` outside of worklogs (historical) and the Epic 26 supersession banner (intentional reference).
11. `docs/operator/inference-relay.md` no longer mentions the CF Worker as a deployable option.
12. `design/stories/README.md` lists Epic 26 as `⛔ Superseded` and Epic 60 as `✅ Complete`.
13. Closes #474.
