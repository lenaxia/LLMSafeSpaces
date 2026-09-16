# Worklog: Workspace egress destination allowlist (#821, Epic 67)

**Date:** 2026-09-16
**Session:** Implement issue #821 — replace the IP-deny-list-only workspace egress posture with an operator-selectable destination allowlist (staged), via chart values + NetworkPolicy template + tests + docs.
**Status:** Complete

---

## Objective

Issue #821 (P1): workspace egress filtering is a deny-list (0.0.0.0/0 minus RFC1918/CGNAT/private), so arbitrary public IPs are reachable from inside a sandbox — the exfiltration sink for Epic 67 SEC-1/SEC-3/SEC-6. Ship the issue's option 1 (explicit destination allowlist enforced via NetworkPolicy egress) with a staged default, plus the option-2 data-plane/tooling split.

---

## Work Completed

### Values + template (the mechanism)

- `helm/values.yaml`: new `networkPolicy.workspaceEgress.mode` (`public` default | `allowlist`) and `networkPolicy.workspaceEgress.allowlist.{llmCIDRs, tooling.{enabled,cidrs}}`. Updated the section header + `allowedEgressCIDRs` comments to describe both postures.
- `helm/templates/workspace-network-policy.yaml`:
  - mode parsing with `fail` on unknown values (a typo cannot silently fall back to the permissive posture);
  - `allowlist` mode does NOT render the general public-internet rule — egress limited to DNS, in-namespace API relay path, optional relay-router, `extraEgressCIDRs` escape hatch, and the two CIDR groups;
  - group rules are port-restricted to TCP 443/80 and rendered as plain ipBlock (no `except:`);
  - `public` mode unchanged except: the shared `blockedEgressCIDRs` except-list is now attached ONLY to the `0.0.0.0/0` catch-all (fixes a pre-existing API-invalid render for narrow custom `allowedEgressCIDRs`, found while designing this change — previously a narrow entry + shared except list produced a NetworkPolicy the API server rejects, which on fresh install means NO egress policy at all).

### Tests (TDD — written first, red → green)

New `helm/networkpolicy_egress_allowlist_test.go`, 9 tests:
`TestEgress_Allowlist_DefaultModeIsPublic`, `...ModeDropsPublicInternetRule`, `...RendersGroupsOnHTTPSPortsOnly`, `...ToolingSplit`, `...KeepsDNSAPIAndRouterRules`, `...GroupRulesCarryNoExcept`, `...ExtraEgressCIDRsEscapeHatch`, `...InvalidModeFailsRender`, `TestEgress_PublicMode_NarrowAllowedCIDRRendersWithoutExcept`.

### Docs

- `docs/operator/networking.md`: new "Destination allowlist mode (#821)" section (rule table, staged-default rationale, public-CIDR constraint, dev-preview caveat); rewrote the stale "Per-workspace egress" paragraph (claimed a nonexistent `securityPolicy.network` field; actual field is `spec.networkAccess.egress`, controller-resolved to /32s).
- `docs/reference/helm-values.md`: rows for `mode` + both groups; `allowedEgressCIDRs` row updated.
- `docs/user/dev-preview.md`: note that allowlist-mode clusters must allowlist tunnel-provider CIDRs.
- `README-LLM.md` §Tenant isolation: network-isolation row now mentions the opt-in destination-allowlist mode.

---

## Key Decisions

1. **Default posture stays `public` (staged rollout); `allowlist` is opt-in.** Validated (live DNS, 2026-09-16): every default-surface destination is CDN-fronted — opencode.ai → Cloudflare (172.65.x), pypi.org/files.pythonhosted.org → Fastly (151.101.x), registry.npmjs.org/nodejs.org → Cloudflare (104.16.x), github.com → 140.82.x, go.dev → Google. Chart-shipped default CIDRs would either rot (pinned /32s) or re-admit the channel (whole-CDN ranges include free attacker-usable hosting: GitHub Pages/gists, Cloudflare-proxied sites). Flipping the default would also break upgrading deployments whose agents install arbitrary packages — which is the product. The values structure is the staging vehicle; the strict data-plane endgame is #820's relay-only path (in-cluster selector egress, no public IPs), for which `allowlist` mode with empty groups is already the correct posture.
2. **Mechanism = structured values → ipBlock rules; no faked FQDN support.** Plain NetworkPolicy cannot match FQDNs (threat-model A3, "well-known K8s limitation"). Honesty requirements: values docs state the limitation, point at `dig +short` for deploy-time resolution, at Cilium CNP (with `workspaceEgress.enabled=false`) for strict FQDN, and at the existing per-workspace `spec.networkAccess.egress` widening (controller-side FQDN→/32 at reconcile; composes by NetPol union).
3. **Group rules carry no `except:` and are TCP 443/80 only.** Kubernetes rejects ipBlock `except` entries that are not subnets of the `cidr`; the shared blockedEgressCIDRs (private) ranges are not subnets of narrow public CIDRs, so attaching them makes the whole object API-invalid → fresh installs end up with no egress policy (fail-open). Port restriction to 443/80 matches what the default surface needs. `extraEgressCIDRs` keeps its all-ports, no-subtraction semantics as the documented internal-destination escape hatch.
4. **Data-plane/tooling split via `allowlist.tooling.enabled`** — `false` yields the LLM/relay-only posture the issue's option 2 describes; the groups map 1:1 to the split so #820's llm-relay namespace slots in later without redesign.
5. **No per-workspace CRD API added** — the issue offers it as an option, not a requirement, and `spec.networkAccess.egress` already provides per-workspace widening. Right-sized per Rule 4.

---

## Assumptions (stated + validated per Rule 7)

| # | Assumption | Validation |
|---|---|---|
| 1 | Native NetworkPolicy has no FQDN matching | `design/0027` A3 ("✅ Validated"); `docs/operator/networking.md` CNI table |
| 2 | K8s API rejects `except` entries not contained in `cidr` | Kubernetes networking validation (well-established); rendered artifacts assert the design consequence (`TestEgress_Allowlist_GroupRulesCarryNoExcept`, `TestEgress_PublicMode_NarrowAllowedCIDRRendersWithoutExcept`) |
| 3 | Default-surface destinations are CDN-fronted/rotating | Live resolution 2026-09-16 (results in Decision 1) |
| 4 | kind CI cluster does not enforce NetworkPolicy (kindnet) → live enforcement not testable there; structural chart tests are this repo's NP verification level | `local/us-70-faults-e2e.sh:341` comment; all prior G16 NP work verified via `helm/chart_test.go` |
| 5 | Per-workspace FQDN widening exists and composes by union | `controller/internal/workspace/network_policy.go` (no changes needed) |
| 6 | Selector-based rules (DNS/API/router) are posture-independent | Pinned by `TestEgress_Allowlist_KeepsDNSAPIAndRouterRules` |

---

## Blockers

None. Live-cluster enforcement validation of `allowlist` mode is deferred with rationale (assumption 4); the kind suite (kindnet) cannot exercise NetworkPolicy enforcement at all.

---

## Tests Run

- `go test -timeout 300s ./helm/ -run 'TestEgress_' -v` — 9/9 pass (was 6 fail / 3 pass before implementation).
- `go test -timeout 900s ./helm/` — full chart suite pass (18.2s).
- `go build ./...` — pass.
- `make lint` — pass.
- Full `make test` run — pass (all packages; helm tests require helm on PATH, installed to /tmp for the session).

---

## Next Steps

- Operator rollout: pin destination CIDR sets per deployment and flip `mode: allowlist`; the empty-groups posture is #820-ready.
- When #820 lands: add the llm-relay namespace/router selector rule to the allowlist-mode rule set (values-driven, no redesign).
- Consider a default-mode flip (allowlist-by-default) only after a registry-mirror story exists.

---

## Files Modified

- `helm/values.yaml`
- `helm/templates/workspace-network-policy.yaml`
- `helm/networkpolicy_egress_allowlist_test.go` (new)
- `docs/operator/networking.md`
- `docs/reference/helm-values.md`
- `docs/user/dev-preview.md`
- `README-LLM.md`

---

## Iteration 2 — AI review findings (2026-09-16, PR #1386 round 1: CHANGES_REQUESTED)

### Findings + dispositions (Rule 11 Phase 2)

1. **Deferred scope untracked while `Closes #821` auto-closes** — REAL. Filed follow-ups: **#1387** (egress-audit / tier 3) and **#1388** (default-posture decision, reserved for the owner per the #821 design comment). PR body updated to reference them.
2. **Silent-widening footgun: private CIDRs in groups render unconditionally** — REAL. Added `llmsafespaces.isPrivateIPv4CIDR` render-time guard (lexical IPv4 octet check vs the blockedEgressCIDRs defaults) failing the render for private entries in `llmCIDRs`/`tooling.cidrs` and narrow private `allowedEgressCIDRs` (restores the loud failure the pre-#821 API-rejection accidentally provided). 3 new tests incl. the over-match guard (172.15/16, 100.63/16, 100.128/16 render fine).
3. **networking.md hardcoded 8080 for the API-relay port** — REAL (doc nit). Now `.Values.api.service.port (8080 by default)`.
4. **`allowlist: null` degrade unpinned** — REAL (missing test). `TestEgress_Allowlist_NullAllowlistDegradesStrictest` added.
5. **Flake-fix commit undisclosed in PR body** — REAL (process omission). Disclosed in the iteration-2 section with root cause + verification.

### Tests added this iteration

`TestEgress_Allowlist_PrivateGroupCIDRFailsRender` (+3 subtests), `TestEgress_PublicMode_PrivateNarrowAllowedCIDRFailsRender`, `TestEgress_Allowlist_PublicGroupCIDRsStillRender`, `TestEgress_Allowlist_NullAllowlistDegradesStrictest`. Suite now 14 egress tests; full `./helm/` suite + `make helm-render` pass.

### Files modified this iteration

`helm/templates/_helpers.tpl`, `helm/templates/workspace-network-policy.yaml`, `helm/networkpolicy_egress_allowlist_test.go`, `helm/values.yaml`, `docs/operator/networking.md`, PR #1386 body; issues #1387 + #1388 filed.

---

## Iteration 3 — AI review round 2 findings (2026-09-16, PR #1386)

1. **`allowlist.tooling: null` nil-pointer crash** — REAL (reproduced red first: `TestEgress_Allowlist_NullToolingDegradesStrictest` failed with the reviewer's exact error, then fixed). Helm null-deletes the key so `$allowlist.tooling.enabled` crashed. Fix: `$tooling := $allowlist.tooling | default dict`; tooling:null now degrades fail-closed (no tooling rules, llm group intact), mirroring the shallow `allowlist: null` pin.
2. **"Enforced at render time" overstated the guard's scope** — REAL (doc precision). values.yaml / networking.md / _helpers.tpl wording now states: well-formed private dotted-quads fail the render; malformed spellings/hostnames pass the render but are rejected loudly at apply time by ipBlock validation; aggregate CIDRs spanning private space (0.0.0.0/1) are the residual silent case (contrived); extraEgressCIDRs unguarded by design.
3. **#1388 decision frame should name the per-workspace open-egress dimension** (minor, issue text) — done: appended the option-3 per-workspace dimension to #1388's body.

Tests: +1 (`TestEgress_Allowlist_NullToolingDegradesStrictest`); 15 egress tests total; full ./helm/ suite + lint + helm-render pass.
