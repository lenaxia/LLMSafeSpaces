# Worklog: #1575 r3 — e2e legs, the latent-shape pin, the org-update pin, and the mixed-fleet correction

**Date:** 2026-10-06
**Session:** Address the #1621 r3 verdict — four requirements: e2e on the real-binary materialize harness, an explicit mixed-fleet decision (the r2 worklog's "mixed-fleet safe" claim was a Rule 7 violation), a pin on the latent Models+BaseURL+allowlist shape, the org-update 400 pin
**Status:** Complete

---

## Objective

Close the four r3 gates without regressing the approved r2 shape.

---

## Work Completed

### 1. E2E legs (real materialize binary, fake /models upstream)

- `TestE2E_ModelEnricher_DeliveredAllowlistFiltersFetchedCatalog` (happy): entry carries `modelAllowlist:["default","glm-5.1"]` + per-model limits; live catalog `[default, glm-5.1, bge-m3]`; agent-config.json ends with exactly `default` (limits block merged) + `glm-5.1`; `bge-m3` filtered. The full chain — batch entry → materialize subprocess → enrich fetch → filter → FormatOpenCodeConfig → the file opencode reads.
- `TestE2E_ModelEnricher_AllowlistMatchingNothingYieldsNoModels` (unhappy): stale allowlist over a live catalog → provider present (options block intact), NO models — never synthesis, never the unfiltered fetched list. Degrade visible, not silent.

### 2. Mixed-fleet decision: ROLLOUT-ORDER NOTE (chosen over a transition gate) — and the r2 worklog correction

**The regression, stated plainly (the r2 worklog's "mixed-fleet safe" claim was inaccurate — this entry corrects it; the r2 file stands append-only):** a pre-#1575 agentd receiving the new delivery shape (Models empty + unknown `modelAllowlist` fields) fetches the FULL unfiltered catalog — allowlist ENFORCEMENT for custom-endpoint credentials lapses until the pod runs the new binary. Backwards-compatible (nothing breaks) ≠ enforcement-safe: during the window, allowlisted credentials show their entire upstream catalog.

**Why a note, not a gate:** the batch is built unilaterally (bootstrap, live push, resync pull); the pod's contract version is negotiated only on the bootstrap path, and threading a pod-version signal into every build (including live pushes to unknown-version pods) is disproportionate to a transition window the release train already bounds — the chart rolls the agentd delivery overlay WITH the API (one appVersion bump), so the window is only the workspace pods' rotation lag, and it fails OPEN ON CURATION ONLY (same key, same provider; not a credential boundary; the relay-token scope path is controller-side and already fixed).

**Operator note (in the PR body):** roll workspace delivery images with the API upgrade (the standard chart upgrade does); suspend/resume alone does NOT close the window (the old binary ignores the new fields regardless of re-materialization) — pod recreation on the new image does.

### 3. The latent-shape pin + closure (not just a pin)

`Models`+`BaseURL`+allowlist in one blob would have passed UNFILTERED under r2 (custom branch returned early; a non-empty delivered Models list SUPPRESSES the pod-side fetch, so nothing downstream would filter). Closed in `applyModelAllowlist`: the stored-catalog intersection moved BEFORE the branch split — a stored list vouches for its IDs, so it is intersected server-side (default included) AND the allowlist still rides the entry. Pinned: `TestCredentialPrecedence_CustomEndpointStoredCatalogIsIntersected`. Net function shape: single intersection, then custom-attach or first-party-synthesis — no duplicated loop.

### 4. Org-update 400 pin

`TestOrgCredentials_Update_EmptyAllowlistIDRejected` (the r2 wiring existed; the pin was missing).

---

## Key Decisions

- Rollout-order note over a version-gated delivery shape (rationale above; recorded in the PR body for the reviewer's scrutiny).
- Latent shape CLOSED (server-side intersection) rather than merely pinned — a pin without the closure would document a hole.

---

## Blockers

None.

---

## Tests Run

- `TestE2E_ModelEnricher_DeliveredAllowlistFiltersFetchedCatalog|TestE2E_ModelEnricher_AllowlistMatchingNothingYieldsNoModels` — PASS (real binary, 4.3s family).
- `TestCredentialPrecedence_` full family ok; `TestOrgCredentials_Update_EmptyAllowlistIDRejected` PASS; `TestOrgCredentials_` family ok.
- Full sweeps re-run before push (see PR).

---

## Next Steps

- Push r3 to PR #1621; notify the orchestrator.
- #1565: fix committed (24e490bf) on fix/1565-agentd-parse-boundary — push + PR next (held until #1621 r3 pushed, per lane sequencing).
- #1623: sidebar gesture zone.

---

## Files Modified

- pkg/secrets/injection.go (intersection hoisted; latent shape closed)
- pkg/secrets/credential_precedence_test.go (CustomEndpointStoredCatalogIsIntersected)
- cmd/workspace-agentd/model_enricher_e2e_test.go (two e2e legs)
- api/internal/handlers/org_credentials_test.go (update 400 pin)
- worklogs/1093_2026-10-06_allowlist-r3-gates.md (this worklog)
