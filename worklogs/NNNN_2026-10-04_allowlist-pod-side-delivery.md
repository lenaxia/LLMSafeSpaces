# Worklog: #1575 r2 — the allowlist meets the live catalog pod-side (delivery-field redesign)

**Date:** 2026-10-04
**Session:** Address the #1621 r1 REQUEST_CHANGES verdict — the reviewer proved pd.Models is ALWAYS empty at applyModelAllowlist (every blob write path marshals kind/slug/apiKey/baseURL only), so the r1 intersection fix never reached the production shape; redesign to deliver the allowlist for pod-side post-enrichment filtering + the issue's boundary-validation ask
**Status:** Complete

---

## Objective

Make a provider model literally named "default" actually deliverable at the PRODUCTION blob shape (TheKaoCloud repro), and move mis-form protection to the credential create/update boundary with a real error.

---

## Work Completed

### The r1 verdict, restated

- Core finding: `pd.Models` is always empty server-side — `encryptCredentialData` (all five write paths) marshals only Kind/Slug/APIKey/BaseURL. The live catalog is fetched pod-side (`model_enricher.go`) AFTER delivery, and the fetch is suppressed precisely when synthesis produced a non-empty list. My r1 test hand-crafted a blob WITH Models — a state production never creates; output was byte-identical pre/post r1 at the real repro.
- Adjacent bug surfaced by the review: allowlist `["default"]`-only → empty synthesis → enricher fetches the FULL unfiltered catalog — the allowlist silently bypassed.

### The chosen design: pod-side allowlist (the verdict's alternative option)

**Rationale (recorded for the PR):** the fetch machinery + 24h cache already live pod-side (the architecture deliberately enriches in the pod — the enricher's own header documents it); a server-side catalog fetch would put API→operator-endpoint egress on the batch-build path with no cache infrastructure — a new subsystem to fix a filtering-order bug. Delivering the allowlist (non-secret, operator-configured metadata) and filtering after the live fetch is the smaller, architecture-faithful change.

- **`pkg/secrets/types.go`** — `LLMProviderData` gains delivery-only fields (`ModelAllowlist`, `ModelContextLimits`, `ModelOutputLimits`; additive JSON, older pods ignore them — mixed-fleet safe).
- **`pkg/secrets/injection.go` `applyModelAllowlist`** — custom endpoints (`BaseURL != ""`): attach the allowlist + limits, leave Models EMPTY so the enricher fetch fires; no synthesis, no filtering (nothing vouches yet). First-party (no BaseURL): synthesis unchanged (opencode merges its built-in catalog config-side; no pod-side fetch exists) including the ""/"default" mis-form-artifact skip; the blob-stored-catalog intersection (defensive shape) keeps the r1 exact-allowlist semantics.
- **`cmd/workspace-agentd/model_enricher.go`** — `applyDeliveredAllowlist` filters the FETCHED (or cache-read — cache stores raw) list against the attached allowlist, merging per-model limits with the server's precedence (explicit entry value wins). NO pod-side synthesis: a live catalog matching nothing allowed is correctly empty; fetch failure keeps the existing degrade. This also fixes the adjacent bug: a default-only allowlist now restricts the fetched catalog to exactly "default".
- **Ask 2 — `secrets.ValidateModelAllowlist`** (credential_identity.go): rejects empty/whitespace-affected ids with a field error. "default" NOT rejected (real catalog id). Wired into all five write paths: admin create+update, user create, org create+update (the user surface has no update handler).
- **`controller/internal/workspace/staging.go` `relayDesiredSet`** — token scope prefers the attached `ModelAllowlist` when Models is empty (deriving from Models would mint an UNRESTRICTED relay token for exactly the credentials whose allowlist the platform enforces); first-party keeps deriving from synthesized Models.

### Tests (production shapes; all behavioral pins verified RED against the r1 tree first)

- Server: `CustomEndpointAllowlistDeliveredNotFiltered` (THE repro shape — blob sans Models, BaseURL, allowlist incl. "default" → Models empty + allowlist/limits attached), `CustomEndpointDefaultOnlyAllowlistAttached` (adjacent-bug input), `AllowlistIntersectionIsCatalogExact` (r1 pin reframed to blob-stored catalog), worklog-0272 ContextLimits pin updated to the delivery contract (the 0272 flow is unchanged, one hop later — pod-side merge pinned in the enricher tests).
- Enricher: `AppliesDeliveredAllowlistPostFetch` (default delivered iff served + limits merged), `AllowlistRestrictsFetchedCatalog` (adjacent bug fixed at the pod), `AllowlistWithStaleIDsFetchedListWins` (no pod-side synthesis), `CacheServesRawFilterAppliesAfter` (allowlist change re-filters without refetch).
- Boundary: validator table + create-400 pins on admin/user/org + admin update-400 + "default" accepted (acceptance and delivery no longer disagree).
- Staging: `TestRelayDesiredSet_PrefersAttachedAllowlist`.

### First-party boundary (documented, unchanged)

A first-party key's allowlist still rides synthesis only; an empty synthesis (allowlist ""/"default"-only on first-party) leaves the config-side opencode catalog merge unfiltered — pre-existing, out of the issue's repro shape, and unfixable without opencode-side allowlist support (noted in the PR).

---

## Key Decisions

- Pod-side filtering over server-side catalog fetch (rationale above).
- Delivery-only fields on LLMProviderData rather than a wrapper type: the materializer path unmarshals LLMProviderData throughout; additive fields are mixed-fleet safe; the blob never carries them (doc comment marks them delivery-only).

### Assumptions stated and validated (Rule 7)

1. The enricher transform is the single catalog-population point for custom endpoints (both materialize and reload call sites use it) — verified: `secrets.go:662,1160`.
2. The cache stores the RAW list — verified: `fetchOrCacheModels` marshals `models` pre-filter; filter applies after cache read (pinned).
3. Relay mint scope must not widen — verified + pinned (`TestRelayDesiredSet_PrefersAttachedAllowlist`).
4. No other consumer of `ResolveLLMProviders` reads `pd.Models` expecting the filtered set — checked: the controller staging is the only consumer (fixed); MCP/model-picker surfaces read the pod's opencode, not this.

---

## Blockers

None. (Two turn-deaths mid-`go test` during verification — runs repeated green after resume; the wt-1565 worktree directory was wiped by a pod restart but the branch + red-test commit survived in the shared .git and the worktree was re-added.)

---

## Tests Run

- RED (r1 tree): the two server delivery pins (synthesized `[glm-5.1]` missing `default`; nothing attached), the four enricher pins, staging pin, validator/handler 400s.
- GREEN (final tree): `TestCredentialPrecedence_|TestValidateModelAllowlist` ok; full `./pkg/secrets/` ok (12.9s); full `./controller/internal/workspace/` ok (68.2s); `TestAdminProviderCredentials|TestUserProviderCredentials|TestOrgCredentials` ok; `TestEnrichProviderModels|TestFetchModels|TestWorkflowExecute_|TestUserTimezone_` ok; `TestStaging|TestRelayDesiredSet|TestMixedFleet` ok; gofmt clean; go vet clean on touched packages.

---

## Next Steps

- Push to fix/1575-model-default (PR #1621) for r2 review.
- #1565: implement the strict-decode fix on the red-pinned branch (fix/1565-agentd-parse-boundary, commit 97fbcb19).
- #1623: sidebar gesture zone (Option A) — after #1565.

---

## Files Modified

- pkg/secrets/types.go, injection.go, credential_identity.go, credential_identity_test.go, credential_precedence_test.go
- cmd/workspace-agentd/model_enricher.go, model_enricher_test.go
- api/internal/handlers/admin_provider_credentials.go(+test), user_provider_credentials.go(+test), org_credentials.go(+test)
- controller/internal/workspace/staging.go, staging_test.go
- worklogs/NNNN_2026-10-04_allowlist-pod-side-delivery.md (this worklog)
