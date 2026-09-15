# Worklog: Design 0058 — relay-only key delivery (issue #820)

**Date:** 2026-09-15
**Session:** Design-only elaboration of the owner's 2026-08-27 decision record on #820 into design doc 0058 + Epic 72 story folder; exposure surfaces re-verified on main; PR opened for AI review (holds — design PRs never auto-merge).
**Status:** Complete (design stage; implementation tracked in Epic 72 stories)

---

## Objective

Turn the fixed decision record (D1 relay-through default, D2 per-request resolve / no long-lived plaintext cache, D3 agentd non-decrypt-capable) into the authoritative design document and epic story folder, consistent with current architecture, landed via an approved design PR. No implementation.

---

## Work Completed

### Exposure surfaces re-verified on main @ 17a2f288 (Rule 7 — designs carry assumptions too)

The Aug-14 verification comment's four-surface list is stale in one place: US-70.5 (#1224, 2026-09-01) demolished `/sandbox-runtime/last-reload-secrets.json` (survives only as a repolint deleted-symbol guard, `pkg/repolint/deleted_symbols.go:48`). Corrected inventory (design 0058 §1.2): S1 `agent-config.json` (`pkg/agent/opencode/format.go:71`, writer `configwriter.go:652`, mode 0640 — agent-readable both modes); S2 `auth.json` (boot merge `cmd/workspace-agentd/secrets.go:601-612` chmod 0660 at `secrets.go:1495-1498`; reload PUT `/auth` `pkg/agent/opencode/client.go:191,261,276`); S3 the batch file — single-container: `/sandbox-cfg/secrets.json` (`bootstrap.go:86,173`, uid 1000 per `pod_builder.go:605`) AND `/sandbox-runtime/rt/secrets.json` (US-70.3 B1 relocation `pod_builder.go:331-343`, resync write `resync_secrets.go:177`) — both uid-1000-readable in the default single-container mode (`agentdSidecar.enabled: false`, `helm/values.yaml:376-377`); sidecar mode: uid-2000-written 0600 (`agentd_sidecar.go:99,189`), NOT agent-readable. Non-surfaces verified: `secrets-env` never carries llm-provider material (`pkg/agentd/secrets/secrets.go:653-654,703`).

### Design decisions made beyond the owner's three (flagged in PR body)

1. **Stage 0 EXCLUDED** — 0400 is a no-op in single-container mode (same uid) and breaks opencode's read in sidecar mode (0640+shared-gid is load-bearing); deny rules don't gate arbitrary code. §5 of the design.
2. **Epic 68 → 72** — the decision record's draft folder collides with shipped `epic-68-chat-file-attachments`; renumber note in the epic README (the 67→68 renumber precedent).
3. **Token minted by the router** (`POST /internal/v1/tokens`), controller obtains and stages it — single signing-key holder; renewal rides US-70.2/70.3 manifest-tier conditional pull (no new delivery path, K6).
4. **Batch-builder rewrite is API-side** (one-builder invariant, US-70.2); agentd needs zero seam changes — its story (US-72.4) is liveness/degrade-codes/sweep hooks.
5. **Separate BYO router Deployment in `llm-relay`**, not the Epic 42 fleet router and not fleet VMs (customer keys stay inside the operator trust boundary); leans separate-Deployment, settled in US-72.2's worklog.
6. **Full provider coverage assumed** (not the withdrawn draft's `baseURL != ""` restriction) — surfaced as open item Q1 for the owner.

### Integration claims validated

`AgentConfigWriter` seam passes provider bytes verbatim (`pkg/agent/agentconfig.go:85-147`); `upstreamAuth` router-side injection precedent (`cmd/relay-router/proxy.go:40-56`, `helm` `controller.inferenceRelay.upstreamAuth.keySecret`); per-binding `ModelAllowlist` already exists and is enforced batch-side (`pkg/secrets/credential_store.go:50`, `injection.go:531+`); KMS provider precedents (`kms_aws_provider.go`, `kms_gcp_provider.go`, `RootKeyProvider` at `root_key.go:35`); #1078 = deploy-drain grace (cap-not-delay) + in-flight surfacing, pattern applied to the 2-replica router; `shouldSkipRelay` (`relay_injector.go:98-131`) is free-tier-Zen-only, orthogonal to BYO relay-through; NetworkPolicy carve-out pattern (`relay-router-networkpolicy.yaml` namespaceSelector); condition-types precedent (`workspace_types.go:264-294`, `SecretsDelivery` at `:439`).

### Files

- `design/0058_2026-09-15_relay-only-key-delivery.md` — the design doc (house style per 0055/0057: status header, depends/composes, numbered sections, threat model, assumptions table, rejected alternatives, open items)
- `design/stories/epic-72-relay-only-key-delivery/README.md` — epic folder (renumber note, story map US-72.0–.6 with scope/AC/test-plan/sizing/deps, cross-cutting invariants K1–K6)
- this worklog

---

## Key Decisions

See "Design decisions made beyond the owner's three" above. All six are elaborations within the decision record's authority, surfaced in the PR body; #1 and #6 are explicitly flagged as questions for the owner (delegated Stage 0 disposition; first-party provider coverage).

---

## Blockers

None.

---

## Tests Run

None — design-only change (no code paths touched; `make lint`/repolint run via pre-commit hook on commit). Exposure-surface verification was read-only source inspection at main @ 17a2f288.

---

## Next Steps

1. AI review loop on the design PR until APPROVE (holds; never auto-merges).
2. On approval + owner `/merge`: file Epic 72 story issues (US-72.0 first — #910 re-arm), then implementation per the story map.
3. Owner answers open items Q1–Q3 (design 0058 §10) before US-72.3 lands defaults.

---

## Files Modified

- `design/0058_2026-09-15_relay-only-key-delivery.md` (new)
- `design/stories/epic-72-relay-only-key-delivery/README.md` (new)
- `worklogs/NNNN_2026-09-15_relay-only-key-delivery-design.md` (new, sentinel)
