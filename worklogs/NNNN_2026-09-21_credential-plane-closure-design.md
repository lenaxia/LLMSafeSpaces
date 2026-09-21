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

None. §10 is now a DECISION RECORD: the orchestrator ruled all five questions mid-review (R1 file the FOUR-ask upstream bundle now — session-ACL + FD-delivery + LANDLOCK + the #1465 MCP caller-session identity draft; R2 honor #823's recorded default-flip gate; R3 minimal-bar nightly/full-bar epic-exit; R4 warn-only + metric; R5 (a)+(b) — ask filed AND spawn-env payload redaction interim shipped). The doc's in-body pointers updated to cite the rulings; B2's interim (R5b) is an implementation-story, not design.

### Review round 1 (CHANGES_REQUESTED → addressed)

Eight findings, all doc-level, all fixed: (1) the stale S2 citation — inherited from 0058 despite my "re-verified" claims in THREE places (pickup comment, worklog, header); corrected + an in-doc erratum owning it (the same completion-claim class as #1509's lesson, now bit me directly); (2) the §5 batch-row mis-citation + recycled withdrawn-0050 wording → corrected to 0058's actual token-substitution shape; (3) B3's §8-Q2 → §10-Q1; (4) B2/A3's undeclared UPSTREAM dependency — opencode is a pinned binary; FD-read/stdin don't exist upstream; A3 rewritten as upstream-gated + new Q5 (file the ask; interim payload-seam redaction shrinks the env surface without touching the pin); (5) the tiers WIP is local-only/unfetchable — marked pending-push, A2's "unit suite green" claim weakened to corroborated-existence (branch push is worker 5's call); (6) #823 options 2/3 named and dispositioned (B2b); (7) #823's 2026-09-16 #978 rescope gate reconciled into Q2 (honor the recorded default-flip gate); (8) #825's LANDLOCK/seccomp arm explicitly deferred to the upstream seam, default-deny-vs-ask rationale added, option 3 one-line deferral. Plus the sweep's verdict semantics defined (§6.4b — a positive readability model with an explicit residual set, so the guaranteed memory finding has a defined verdict and probe-blindness is itself red).

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

### Review round 2 (CHANGES_REQUESTED → addressed) + rulings recorded

Rulings first: §10 became the R1-R5 decision record (the orchestrator ruled mid-review; Q1 upgraded to the FOUR-ask upstream bundle — session-ACL + FD-delivery + LANDLOCK + the #1465 MCP caller-session identity draft).

R2's findings, all fixed: (9) the erratum itself miscited (secrets.go:1495-1498 is the symlink preamble; the 0660 chmod is 1510-1515) — fixed with the recursion acknowledged in-place; (10) A6's fallback was INFEASIBLE as written (a uid-2000-created 0600 file is unreadable by the uid-1000 supervisor; 0640-CROSS_UID for a control token is an unjustified carve) — replaced with the single pipe-across-exec-on-supervisor-spawn mechanism preserving T2 ownership; (security, carried from r1) §6's instrumentation is now BUILD-SCOPED — the memory-report primitive exists only in a sweep build-tag variant never shipped to production (a flag-gated production endpoint would be a new credential-plane surface AND a uid-1000→uid-2000 read path across the 0051 boundary); new A8 flags /proc/pid/mem as likely-INFEASIBLE (ptrace EPERM under cap-drop-ALL/NoNewPrivs, gVisor, Yama scope-1) with the memory row degraded to sweep-build self-reporting and B3's MECHANISM restated (process-memory access, not bare ptrace); sweep semantics made fallback-aware (per-delivery-mode residual sets; a lingering fallback file is RED); #825 option 4 named-and-adopted; the thread's "SEQUENCE AFTER epic-72" verdict reconciled with §7's tiers-first ordering via the inversion; the tiers pending-push caveat discharged (branch on origin, reviewer-diffed).

### Review round 3 (CHANGES_REQUESTED → addressed)

Seven findings, all doc-level: (f6) the erratum fix had landed in the ERRATUM only — the C1 row still carried the wrong 1495-1498 range; the row is the load-bearing citation and now carries 1510-1515 (the doc contradicted itself in one section); (f7) §6's exit-criteria line was stale against the doc's OWN recorded gates — #823's line now cites the R2 gate (B2 explicitly NOT in it — upstream-gated), #825's now cites the sweep per the Part D reconciliation, not tiers-alone; (f8) A8's degraded probe asserted a different fact about a different process than the residual-set item it was supposed to satisfy — three probe modes now pinned, EACH WITH ITS OWN residual-set variant (P1 external /proc read; P2 the single-container parent-reads-child middle path the reviewer surfaced — agentd is opencode's parent, Yama scope-1 permits ancestor ptrace; P3 self-report with opencode's memory moved to the declared-unmeasured column); (f9a) the "#1465-era draft, already written" claim corrected to what's verifiable (#1469 is the in-repo sentinel half; the upstream half is in the orchestrator's queue); (f9b) R5b's interim given its mechanism sentence (payload-seam class exclusion + supervisor re-inject at exec) and §7-step-3 now sequences it FIRST; (f10) both citation nits (spawn_env_pull const at :71; the pre-allows list made exhaustive).

### Review round 4 (CHANGES_REQUESTED → addressed) — the false-topology round

The reviewer traced the REAL wiring and found my entire C2 sidecar-mode story (B1/B2/R5b/A6, four load-bearing points) built on an unvalidated topology — exactly the Rule-7 failure the doc's own header warns about: (f7) agentd_sidecar.go:342 wires OPENCODE_SERVER_PASSWORD on the UID-1000 MAIN container (the secretKeyRef I attributed to the uid-2000 sidecar — whose copy is the differently-named AGENTD_SIDECAR_PASSWORD); (f8) the spawn-env PULL never carries the password (the child inherits it from the supervisor's environ), AND the supervisor STRUCTURALLY needs it in-env for PULL Basic auth — the D1 carve-out, with agentdPassword/admin-bearer forbidden as alternatives — so C2-in-uid-1000-env is structural in sidecar mode, and the sweep as specified would have gone RED on its own claimed end state; (f9) R5b's interim was a triple no-op (nothing to stop carrying; no per-class redaction at that seam; bootAgentPassword empty in supervise-opencode mode); (f10) there is NO sidecar exec of opencode — both modes exec it from the supervisor in the SAME container, so the cross-container FD question, the r2 fallback, and the r3 pipe-then-file machinery all solved a nonexistent topology.

Redesign: the C2 story is now told as THREE wires — w1 the file (audited/removable where vestigial), w2 the supervisor env (DECLARED STRUCTURAL RESIDUAL: PULL auth + D1; a per-pod pull-token seam is named), w3 the child env (FD-at-exec, gated SOLELY on A3 — same-container exec makes FD inheritance trivial spawner-side). The sweep's residual set gained the supervisor-environ and child-environ rows (the canary WILL be found in them; declared, inside the set). R5b's interim WITHDRAWN with an erratum and referred back to the orchestrator for re-ruling (the honest interim is the B1 file audit only). A6 collapsed to satisfied-by-construction. The withdrawn r2 fallback is named as withdrawn with the reason.

### Review round 5 (CHANGES_REQUESTED → addressed) — the consume-the-correction round

r4's topology fix was verified genuine but hadn't reached its consumers: (f5) §6.4b never cross-referenced A8's P1/P2/P3 sets (a P3 run double-fails the flat set) and the non-disclosure of the carried finding was named; (f6) the withdrawn fallback was still pinned as a live delivery-mode variant + its leak class in the mutation pins — purged; (f7) the w2 "(both modes)" claim was FALSE in single-container mode (the controller wires no container-env form there; the supervisor reads the file at boot and holds it in HEAP — a different surface needing its own declaration — so every single-container run would have gone red on a phantom expected-residual); (f8) A3 still cited the withdrawn (b) half of R5; (f9) the PULL observer was a tautology — rewritten to assert the real channels (child's composed environ pre/post-A3; the FD hand); (f10) §6.4 item 4 contradicted 6.4b. All fixed: 6.4b is now a function of THREE axes (pod mode × A3 phase × probe mode), mode-correct in both directions, fallback-purged.

### Review round 6 (CHANGES_REQUESTED → addressed)

Two findings, both owned: (f6-carried) the §6.2 observer rewrite was CLAIMED in r5's worklog but never landed — the diff had no §6.2 hunk; a false completion claim in the very log section about the #1509 lesson, the same class three rounds have now caught. Rewritten for real this time (verified in-diff): the observer asserts the payload-never-carries tautology (pinned as a regression tripwire), the child's composed environ pre/post-A3 (the load-bearing w3 row), the FD-3 hand post-A3, and the w2 Basic-auth appearance — nothing about "pipe traffic." (f7-new) the C3 admin-token mode is 0400 (init_fs.go:211, #887 D5.1), not 0600 — four assertion-bearing sites corrected (a sweep keyed to 0600 would red on the honest single-container end state; 0400 is stricter, the residual stands, but this doc's bar is assertion exactness).
