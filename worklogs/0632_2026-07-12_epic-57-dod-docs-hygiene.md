# Worklog: Epic 57 DoD docs hygiene + threat-model refresh

**Date:** 2026-07-12
**Session:** Close three half-finished Epic 57 Definition-of-Done items left behind when the epic's stories shipped.
**Status:** Complete

---

## Objective

Epic 57 (RCE resistance hardening) shipped all three stories (US-57.1 AWS KMS, US-57.2 migrate-kek CLI, US-57.3 GCP KMS), but three DoD items were left incomplete:

- **DoD #6:** `pkg/secrets/README.md` provider table was updated with AWS KMS / GCP KMS rows, but the threat-model matrix still had the stale "External (planned)" column instead of provider-specific KMS columns.
- **DoD #7:** `design/stories/epic-17-security-review/THREAT-MODEL.md` attack tree [2.4] still said "Residual — KMS/Vault deferred" instead of reflecting that KMS is shipped.
- **DoD #8:** `design/stories/epic-50-master-kek-hardening/README.md` "Deferred — External Providers (H3)" section still said H3 was deferred instead of resolved by Epic 57.

All three are docs-only work but they cause real downstream confusion: an operator reading the threat model today would conclude KMS is unbuilt and plan around the wrong posture.

---

## Work Completed

### `pkg/secrets/README.md`

- Opening paragraph: "Two local implementations ship today; an external provider (Vault / OpenBao Transit) is planned" → "Two local + two cloud-KMS implementations ship today; OpenBao remains a possible community contribution."
- Threat-model matrix: replaced the single "External (planned)" column with separate "AWS KMS" and "GCP KMS" columns. Each row updated with provider-specific reasoning (file-mounted IAM identity vs. raw key material; CloudTrail vs. Cloud Audit Logs; etc.).
- "Key takeaway" paragraph: replaced the generic "An external (Transit) provider adds exfiltration-limitation and audit" with provider-specific language about the HSM boundary and the dual-sourced audit story under KMS.
- "Choosing a provider" section: added the AWS and GCP production rows, including the per-purpose KMS key ARNs and the pointer to the new `helm/KEK-MIGRATION.md` runbook.

### `design/stories/epic-17-security-review/THREAT-MODEL.md`

- Attack tree [2.4] (Read master KEK from API process memory) reclassified from "Residual / KMS deferred" to 🟡 Partial, with the Epic 57 detail inline (cloud-KMS provider keeps key material in the HSM; RCE-window bounded; CloudTrail/Cloud Audit Logs independently log every decrypt).
- Revision history: added entry 2.11 noting the reclassification and cross-referencing `pkg/secrets/README.md` and `epic-50` H3.

### `design/stories/epic-50-master-kek-hardening/README.md`

- H3 row in the findings table: status flipped from "Deferred" to "Done (Epic 57)".
- "Deferred — External Providers (H3)" section renamed to "H3 — External Providers (resolved by Epic 57)", with Epic 57's cloud-KMS-not-Vault decision rationale appended. Historical Epic 50 reasoning preserved verbatim below the new resolution note (for traceability — the historical reasoning is what shaped the interface that Epic 57 consumed).

---

## Key Decisions

1. **Preserve historical reasoning in place, don't rewrite.** Epic 50's deferral rationale documents why the `RootKeyProvider` interface was shaped the way it was — that shaped what Epic 57 could build. Rewriting it to look like the deferral was wrong would erase the trail. Instead, the section is re-framed as "here was the deferral; here is how it was resolved; here is why the resolution differs from the original recommendation."

2. **Two KMS columns, not one.** AWS KMS and GCP KMS have meaningfully different operational properties (file-mounted AWS static credentials vs. file-mounted SA JSON; CloudTrail vs. Cloud Audit Logs; multi-region replica shape). Collapsing them into one "Cloud KMS" column would hide the differences an operator needs to reason about.

3. **🟡 Partial, not 🟢 Mitigated, on attack tree [2.4].** The in-process-abuse-during-live-RCE property is genuinely unchanged by KMS — an attacker with live RCE can still call `Decrypt` as the application does. The improvement is exfiltration-limitation + audit, not prevention. Marking it 🟢 would oversell what KMS delivers and contradict the honest framing in `pkg/secrets/README.md` §"Threat model".

---

## Assumptions stated and validated (Rule 7)

1. *The provider table in `pkg/secrets/README.md` was already updated for Epic 57.* Validated by reading the table at lines 17-23 — AWSKMSProvider and GPCKMSProvider rows were present with the Epic 57 attribution.
2. *Epic 57 DoD items #6/#7/#8 were genuinely incomplete.* Validated by grep for "External (planned)" (1 hit, the stale matrix), grep for "Residual" in THREAT-MODEL.md (the [2.4] row), and reading the H3 section in epic-50.
3. *AWS KMS and GCP KMS have meaningfully different matrix-row content.* Validated by reading `pkg/secrets/kms_aws_provider.go` (file-mounted credentials per D2) and `kms_gcp_provider.go` (SA JSON per D1, plus CRC32C integrity verification specific to GCP).
4. *No code change needed for these three items.* Validated — all three are docs-only. Code shipped with US-57.1/57.3.

---

## Adversarial Self-Review (Rule 11)

- **Phase 1 findings:** The "Choosing a provider" section now references `helm/KEK-MIGRATION.md` — that file didn't exist yet. Phase 2 validation surfaced this as a real finding (forward reference to a not-yet-created file). Phase 3 remediation: create the runbook as part of T2 in the same session (done — see worklog 0631).
- **Phase 2 false alarm initially considered:** "Did I overstate CloudTrail's audit independence?" Reviewed: CloudTrail logs the KMS API call (which key, which principal) regardless of whether the application's audit log fires. The two are genuinely independent observability surfaces. Not a real finding.

---

## Blockers

None.

---

## Tests Run

```
go test -timeout 60s ./pkg/secrets/ -run "TestCompositeProvider|TestStaticKeyProvider" -count=1 → ok
```

(Smoke test only — this worklog is docs-only; no behavior change to test. The adjacent code/test work is in worklog 0631.)

---

## Files Modified

- `pkg/secrets/README.md`
- `design/stories/epic-17-security-review/THREAT-MODEL.md`
- `design/stories/epic-50-master-kek-hardening/README.md`

---

## Next Steps

1. Open this PR.
2. Cross-link from Epic 57's README to confirm DoD #6/#7/#8 are now satisfied (the README does not track per-DoD status today; this is a separate doc-structure improvement).
