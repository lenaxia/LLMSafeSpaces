# Worklog: #820 Phase 2 — credential-plane closure design (0060)

**Date:** 2026-09-21
**Session:** The structural-removal design doc per the owner's "completely and securely" directive; branch `design/remove-provider-keys-from-pod` (HOLD-class design PR, no implementation)
**Status:** Complete

---

## Objective

Produce the design/ doc that answers the credential plane as a whole — explicit disposition of #820/#823/#825 each with fix-or-documented-residual, an egress-channel threat model (gh/API/git, not just file reads), and the #1500-precedent adversarial proof bar.

---

## Work Completed

- Read the full #820 thread (7 comments): the 2026-08-14 verification (4-file inventory, two corrections, option-3 rejection), the staged-path assessment, the 2026-08-27 decision record (D1 relay-through default / D2 per-request router resolve, no plaintext cache / D3 agentd non-decrypt-capable), the 2026-09-15 triage, and the orchestrator's Phase-1/Phase-2 plan comment. Siblings #823 (control token; P2; three fix options) and #825 (policy-only defense) read in full.
- Discovered design 0058 (relay-only key delivery) already landed 2026-09-15 with the corrected surface inventory (S1–S3 post-US-70.5), the full C1 design, and its own threat model — the epic-68 README the decision record promised became 0058/Epic 72. My doc ADOPTS it as Part A rather than duplicating.
- Verified the current code state for the remaining plane members: the control-token surfaces (PasswordPath bootstrap file; opencodeChildEnv; the spawn-env PULL delivering OPENCODE_SERVER_PASSWORD as an env value), the admin-token dual delivery (env form at pod_builder.go:118, file form AGENTD_ADMIN_TOKEN_FILE already existing), the C4 user-owned surfaces (git-credentials, ssh, secrets-env 0640 CROSS_UID), and C5's non-surface status (platform-API material never reaches uid 1000 — verified by surface audit).
- Read the enforcement-tiers WIP (feat/opencode-permission-tiers @ e0f7ef23): the deny/ask/allow tier map, credential-name denies, resolved-target denies — and its wire finding that the prior soft gating was inert. Part D builds on it with the inversion: structural defense, policy as alarm.
- **Wrote `design/0060_2026-09-21_credential-plane-closure.md`:** the five-class inventory (C1 provider keys → 0058; C2 control token → file/env surface removal via sidecar-mode + FD-pipe spawn delivery, with the operator-session residual named honestly and its seam = 0055 end-state / upstream session-ACL ask; C3 admin token → file-only delivery hygiene; C4 user-owned → presence by principal-design, exfil bounded by #821; C5 verified non-surface); the per-channel egress threat model (provider endpoints, gh/API, git, relay-prompt channel, :4096 self-auth, harness re-reads); the credential-plane sweep as the single closing artifact (canaries at the sources; read probes incl. /proc environ+fd, config re-read, batch, spawn-PULL observer; exfil probes with C4-success-reported-as-residual semantics; mode assertions; **red-state mutation pins** — the #1509 lesson applied: a green sweep without a demonstrated red state proves nothing); sequencing (tiers → Epic 72 → B2/C → B1 → sweep); Rule-7 assumptions A1–A7 (A3/A6 flagged as implementer spikes with accepted fallbacks); rejected alternatives (chmod-only, in-pod decrypt, in-pod RBAC, third uid, deleting C4); four open owner questions (Q1–Q4, each with a recommendation).

---

## Key Decisions

- **Adopt, don't restate, 0058** — the provider-key half is designed, decision-bound, and current; this doc's value is the plane whole.
- **The B3 honesty line:** opencode has no session ACL, so *any* valid token is operator-equivalent; B1/B2 narrow acquisition but the impersonation impact is a documented residual with a named seam — not a pretended fix.
- **The #825 inversion:** tiers as alarm around a structural defense, never the secrecy mechanism — the load-bearing claim the original issue's framing got backwards.
- **C4 reported-not-failed in the sweep:** the git-PAT exfil probes succeeding in allowlist-off mode is #821's signal, not this design's failure — conflation there would make the sweep lie.

### Assumptions stated and validated (Rule 7)

The doc's §8 table (A1–A7) is the canonical list; two are explicitly spike-first with accepted fallbacks (A3 FD-read at boot; A6 cross-container FD passing).

---

## Blockers

None. Four owner questions outstanding (§10) — all non-blocking for the design, all with recommendations.

---

## Tests Run

None — design-doc lane (no code). The doc's §6 *specifies* the test artifact (the sweep + its mutation pins) as Epic-72/exit work.

---

## Next Steps

- Design PR (HOLD per convention), iterate to APPROVED, notify the orchestrator; worker 5 implements from it after #1510/#1506.
- Q1–Q4 to the orchestrator with the design (the #1465-vehicle pattern).

---

## Files Modified

- `design/0060_2026-09-21_credential-plane-closure.md` (new)
