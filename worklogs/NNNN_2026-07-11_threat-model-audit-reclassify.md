# Worklog: Threat model audit — stale reclassification + docs

**Date:** 2026-07-11
**Session:** Address remaining medium/low threat-model gaps. Step 1: audit each one against current code to separate real from stale. Step 2 (this PR): reclassify stale rows, accept operator-side gaps, reconcile docs.
**Status:** Complete

---

## Objective

The user asked to "address all medium and low findings" and to "first determine if they are actually correct and real findings." This PR is the audit + reclassification step — no production code changes. Subsequent PRs will close the code-fixable real gaps.

---

## Audit results

Each remaining open gap was audited against the current codebase:

### Stale (row status did not match reality) — Fixed

| Gap | Row claim | Reality (verified) |
|---|---|---|
| **G29** | "API accepts `mount_path = '../../etc/passwd'` with HTTP 201" | `validateMountPath` exists at `pkg/secrets/secret_service.go:582-608`, called from line 563 BEFORE secret creation. Rejects empty, absolute, base dir, and any path whose `filepath.Rel` resolves outside the base. Wraps `ErrInvalidMetadata` for 400 mapping. |
| **G45** | "`entrypoint-opencode.sh:8-10` sources file that is never created" | US-35.7 moved the env-secret source path to `/sandbox-runtime/secrets-env` (line 9-10 of the current entrypoint). The legacy `/sandbox-cfg/env` source no longer exists. |
| **G50** | "`NewAuditedProvider` has zero call sites anywhere" | US-50.12 wired it at three production sites in `api/internal/app/app.go`: `app.go:408` (providerCredsProv), `app.go:409` (orgCredsProv), `app.go:624` (apiKeyProv). Every Decrypt on those providers logs to `secret_audit_log`. |

### Real (operator-side infrastructure dependencies) — Accepted

| Gap | Why accepted |
|---|---|
| **G4** (mTLS) | Requires service mesh (Linkerd/Istio) or per-workspace cert infrastructure. Outside the scope of threat-model-gap fixes. Compensating controls documented. Operator runbook: deploy Linkerd/Istio in `inject` mode. |
| **G30** (DNS exfil) | Standard `NetworkPolicy` cannot restrict DNS by domain. Requires Cilium FQDN, Calico GlobalNetworkPolicy, or a custom filtering resolver. Operator infrastructure decision. |
| **G40** (agentd user-port auth) | NetworkPolicy is the documented trust boundary. `requireBearerToken` would be defense-in-depth that existing controls make redundant for the documented deployment topologies. |

### Real (code-fixable) — Open, will be addressed in subsequent PRs

| Gap | Severity | Fix shape |
|---|---|---|
| **G6/G41** (duplicate) | Medium | 1-line route add to the `PerRouteRateLimitMiddleware` config shipped in PR #538 |
| **G9** (opencode binary checksums) | Medium | opencode upstream doesn't publish checksums; partial fix possible (verify `gh` checksum, which IS published) |
| **G13** (lockout keyed on email only) | Medium | Add IP component to lockout key, careful design to avoid breaking legitimate users |
| **G21** (`/sandbox-cfg/password` mode 0644) | Medium | Replace `cp` with `install -m 0600` in credScript |
| **G42** (SSE connection tracking unbounded) | Medium | Periodic cleanup of stale entries |
| **G43** (IPv6 egress unrestricted) | Medium | Add IPv6 rules or document IPv4-only assumption |
| **G44** (pod-level SecurityContext missing RunAsNonRoot) | Low | One-line addition to `buildPodSecurityContext` |
| **G46** (silent password file read failure) | Low | Log at Error + non-zero exit (file:line in threat model was stale; actual location is `cmd/workspace-agentd/main.go:134-140`) |
| **G47** (relay secret as CLI arg fallback) | Low | Remove the literal-value fallback at `controller-deployment.yaml:108` |

---

## Work Completed

### Documentation changes (no production code)

- **`design/stories/epic-17-security-review/THREAT-MODEL.md`** — 6 rows updated:
  - G29, G45, G50 → 🟢 Fixed (stale rows; validator/wiring already existed)
  - G4, G30, G40 → 🟡 Accepted (operator-side infrastructure dependencies; compensating controls documented)
  - STRIDE `Proxy`, `Sandbox Pod`, `Database`, `Workspace Network` rows updated
  - Highest-severity-open callout updated (G33/G34/G35/G50 closure history + new highest: G13, G21)
  - Open gaps list and Accepted risks list reconciled
  - Counts: 26 Fixed / 16 Open / 8 Accepted → **29 Fixed / 10 Open / 11 Accepted**
  - Revision 3.0 added

- **`CHANGELOG.md`** — new `### Threat model reconciliation` section under `[Unreleased]` documenting all 6 reclassifications with rationale, file:line evidence, and operator runbooks where applicable.

### No production code changes

This PR is docs-only. The 3 stale-Fixed reclassifications point at validators/wiring that already exist in the codebase. The 3 operator-side Accepted reclassifications are infrastructure decisions, not code bugs. The 9 real code-fixable gaps will be addressed in subsequent PRs.

---

## Key Decisions

1. **Audit before fixing.** The user explicitly asked to "first determine if they are actually correct and real findings." 3 of the 15 remaining open gaps turned out to be stale (G29, G45, G50). Reclassifying them first keeps the threat model honest and prevents wasted work.

2. **Stale-Fixed rows cite the existing validator/wiring.** Each row was rewritten to explain WHAT already exists in the code (file:line), so a future auditor can re-verify without re-investigating. Not just "Fixed" but "Fixed — here's the proof."

3. **Operator-side Accepted rows include a runbook.** "Reclassify to Fixed when..." gives future maintainers a clear exit criterion. G4 → "when the chart ships a service-mesh reference implementation." G30 → "when the chart ships Cilium and Calico reference policies." G40 → "if a deployment topology emerges where NetworkPolicy is insufficient."

4. **3 PRs, not 1.** Mixing stale reclassification, operator-side acceptance, AND code fixes in one PR would be too large to review. PR 1 (this) is docs-only. PR 2 is code-fixable lows + mediums (G6/G41, G21, G42, G44, G46, G47). PR 3 is G13 (lockout IP+email — slightly more design work).

5. **REPORT.md is historical, not edited.** `design/stories/epic-17-security-review/REPORT.md` is the pentest report as it stood at the time. Editing it would falsify history. THREAT-MODEL.md is the live document.

---

## Assumptions (Rule 7) — stated and validated

| # | Assumption | Validation |
|---|---|---|
| 1 | G29 stale | Verified: `validateMountPath` at `secret_service.go:582`, called from line 563. |
| 2 | G45 stale | Verified: `entrypoint-opencode.sh` sources `/sandbox-runtime/secrets-env` (line 9-10), not the legacy `/sandbox-cfg/env`. |
| 3 | G50 stale | Verified: `NewAuditedProvider` called at `app.go:408,409,624`. |
| 4 | G4 is operator-side | Verified: `proxy.go` uses plain HTTP; fix requires service mesh or per-workspace cert. |
| 5 | G30 is operator-side | Verified: `workspace-network-policy.yaml` is standard `NetworkPolicy` (no FQDN support). |
| 6 | G40 is operator-side | Verified: `agent_reload.go:23-26` documents the NetworkPolicy trust boundary explicitly. |
| 7 | G28 stale → Accepted (last PR) was correct | Verified: bindings persist to PG, bootstrap reads from PG, ErrNoRunningPod documented. (Already reclassified in PR #541.) |

---

## Adversarial Self-Review (Rule 11)

### Phase 1 — finding candidates

1. Am I sure the 3 stale-Fixed rows are actually fixed (not partially)?
2. Are the 3 Accepted gaps really acceptable, or am I just deferring hard work?
3. Should REPORT.md be updated to reflect the new classifications?
4. Did I miss any stale rows in the audit?
5. Are the threat-model count changes internally consistent (sum = 50)?

### Phase 2 — validation

| # | Real? | Disposition |
|---|---|---|
| 1 | False alarm — verified each validator exists at the cited file:line and is called from production code paths |
| 2 | Real decision — each Accepted gap has a documented operator runbook and compensating controls. This is the threat-model's 🟡 Accepted semantic ("risk accepted with documented rationale and compensating controls"). |
| 3 | False alarm — REPORT.md is historical (pentest report); editing would falsify history. THREAT-MODEL.md is the live document. |
| 4 | False alarm — every remaining open gap was audited against current code; the 9 left are genuinely open |
| 5 | Verified: 29 + 10 + 11 = 50 ✓ |

### Phase 3 — remediation

No real findings. All decisions validated.

---

## Blockers

None.

---

## Tests Run

```bash
# Docs-only PR — no test changes, but ran the full suite to confirm no
# accidental breakage from the doc edits (e.g. yaml/markdown lint).
go test -timeout 240s -short ./...
# → 67 packages ok, 0 FAIL

# Build + vet
go build ./...    # exit 0
go vet ./...      # exit 0

# Repolint (catches threat-model drift against actual code state)
make repolint
# → all checks passed

# Format
gofmt -l <changed files>      # clean (no Go files changed)
```

---

## Next Steps

1. **Merge this PR.**
2. **PR 2 — Code-fixable lows + mediums:** G6/G41 (1-line route add), G21 (`install -m 0600`), G42 (SSE pruning), G44 (pod RunAsNonRoot), G46 (Error + exit), G47 (remove plaintext fallback). Batch — each is small and independent.
3. **PR 3 — G13 lockout:** Add IP component to lockout key. Needs careful design — naive IP+email keying breaks users on rotating IPs (mobile, corporate NAT). Likely a "progressive delay" approach instead.
4. **PR 4 — G43 IPv6 + G9 partial checksum:** IPv6 needs a deployment-policy decision (support vs. IPv4-only). G9 partial — verify gh CLI checksum (it publishes `.sha256`); opencode upstream still doesn't publish checksums so that part remains documented.

After PRs 2–4 the threat model will be at roughly 35 Fixed / 5 Open / 10 Accepted — with the remaining opens being genuine design decisions (e.g. G13 progressive-delay design) rather than stale or trivially-fixable rows.

---

## Files Modified

- `design/stories/epic-17-security-review/THREAT-MODEL.md` — 6 rows updated; STRIDE rows; counts; callout; revision 3.0
- `CHANGELOG.md` — new `### Threat model reconciliation` section under `[Unreleased]`
- `worklogs/NNNN_2026-07-11_threat-model-audit-reclassify.md` — this file
