# 0061 — Relay-only delivery hardening: four mechanisms, no more

**Status:** Proposed (2026-09-23) — design stage (HOLD), holds for review; implementation stories follow owner approval.
**Date:** 2026-09-23
**Issues:** #1548 (the split-brain incident — umbrella) · #1546 (the posture defects) — both read in full, evidence chains below.
**Elaborates:** design 0058 (relay-only key delivery), the US-72.5 flip (#1534), the scrub/sweep (#1537), and the #1548 provenance-parity PR (`feat/issue-1548-relay-staging-provenance-parity`: `verifyRelayStagingParity` + the release-smoke binary markers).
**Binding rulings:** the owner/orchestrator convergence recorded in this doc's §2 (four mechanisms, two trimming passes) and §9 (the rejected-alternatives list with reasons). This design does not re-litigate them.

---

## 1. The problem, in one page

Two incidents landed within a day of the US-72.5 default flip, and together they define the gap:

**#1548 — the split-brain (2026-09-23 04:53Z).** The API ran relay-only token emission while the controller's staging pass had never executed — no enable line, no router traffic, zero envelope Secrets. `relay_batch.go`'s fail-closed property held *correctly* (every batch for a bound workspace delivered zero llm entries, `relay_staging_not_ready`), which means the failure mode of a half-armed relay-only system is **silent zero-credential delivery**: the exact harm the design exists to prevent, inverted. Root cause resolved to **provenance drift** (the tagged image's bits were not built from the stamped tree — the stamp is an injected build-arg attesting intent, not the built tree; no merged commit can produce the observed double flag-contradiction). The producer was absent; the consumer's fail-closed logic had nothing to be closed about.

**#1546 — the posture defects.** Under the chart's own mandated posture (`rbac.scope=namespace` forced by the relay-only guard), the release was DOA: the router's keypair bootstrap cannot `create` through a resourceNames-scoped rule (the apiserver ignores `resourceNames` for create — the chart documents this for agentd pins and then contradicts it here); the pins informer needs `list/watch` the pins Role never granted; the free-models refresher runs unconditionally in the binary while its write RBAC is values-gated; and `inferenceRelay` (a cluster-scoped CRD watch) cannot coexist with the namespace posture at all. The least-privilege posture the flip mandates had **never actually been exercised** with delivery overlays enabled — every prior install ran the cluster-scope grant that masked all four.

**The common shape:** every one of these failures is *quiet at the exact moment it happens*. The inert staging binary booted green. The forbidden RBAC lines buried in logs while pods CrashLooped or watches silently failed. The values drift (Epic-42 wiring dropped mid-rollback) shipped because nothing asserts the assembled posture. The four mechanisms below make each failure loud at its own moment of occurrence — and nothing else. This is deliberately a small document: the incident taught that the missing thing was not machinery but **fewer, load-bearing assertions**.

## 2. The binding convergence (owner + orchestrator, two trimming passes)

Four mechanisms. No fifth. The trimmed candidates are recorded in §9 with their rejection reasons — they are **ruled out**, not deferred.

| # | Mechanism | Catches | Loud at |
|---|---|---|---|
| M1 | Crash-loud arming | the absent/inert staging producer (#1548) | controller boot |
| M2 | Fail-open fallback + `relay_fallback_deliveries_total` | staging not ready at batch time (migration) | the exact batch that would have been degraded |
| M3 | Posture gate (one CI job) | the shipped-default posture being broken (#1546); wiring drift + wrong-artifact-for-tag (provenance's two detectable classes — §5.4's envelope) | every default-posture change, pre-merge |
| M4 | `CredentialsStaged=False` CRD condition | per-workspace user visibility of batch degrade | the workspace object |

## 3. M1 — Crash-loud arming

**Goal:** a controller deployed with `--relay-only-key-delivery=true` that cannot reach **armed** state within a bounded startup window exits with a distinct code. The silent inert binary becomes structurally undeployable.

**Semantics.**

- **Armed state** is defined as the conjunction that already exists in `SetupRelayStaging` (`controller/internal/controller/controller.go:120-167`): flags valid → staging config constructed → startup guard green (`ValidateRelayStagingStartup`: router reachable, keypair Secret present, RBAC functional) → **the `relay-only key delivery enabled` line emitted**. The enable line IS the armed assertion — it is not a log among logs; it is the observable proof the producer exists. M1 does not add a second notion of armed; it makes the existing one un-skippable.
- **Distinct exit code: 85** — extending the fail-closed doctrine's ladder (81/82 agentd verify, 83/84 opencode verify; `constants.go:47-53`). Today `main.go:355-357` exits **1** on the staging error — indistinguishable from any other boot failure. Exit 85 makes the CrashLoop's reason one `kubectl describe pod` away: `RelayStagingNotArmed` as the named condition-class the controller-side detection (`agentd_overlay.go:147` precedent) and any runbook grep key on.
- **Bounded window:** the startup guard's existing 30s timeout (controller.go:159) is the window. No new timer machinery.
- **Relationship to the parity PR:** `verifyRelayStagingParity` (flag↔config self-assert) catches the *constructed-wrong* class; M1 catches the *cannot-arm* class. Both end in exit 85 (parity failures take the code too — one code, one meaning: "deployed relay-only but not armed").
- **The enable line becomes the CI-observable contract** consumed by M3 (the gate asserts it on the kind cluster) — boot-time truth, not test-only truth.

**Tests (TDD, red-first):** enabled + unreachable router → exit 85 within the window (not 1, not a hang); enabled + arming green → line emitted, exit 0; parity mismatch → 85; flag-off → zero behavior change (nil config path untouched, byte-identical).

**Scope guard:** M1 does NOT gate on the API's emission env — cross-pod coupling is posture surface (§9, rejected D1-derivation). The controller arms itself, loudly, alone.

## 4. M2 — Fail-open fallback + counter (migration mode)

**Goal:** during migration, "staging not ready at batch-build time" delivers the **pre-flip raw-key batch** instead of a zero-credential batch, and increments a counter at the exact moment it does. The counter is the stall detector — full stop.

**Semantics (specified precisely, per the ruling).**

- **"Not ready" at batch time** means exactly one thing, read at the batch-builder's decision point (`pkg/secrets/relay_batch.go`, where `relay_staging_not_ready` is currently emitted): **the controller-staged handoff Secret for this workspace, carrying a token for a currently-bound llm-provider credential, is absent or expired at the moment the builder assembles the batch.** Not heartbeat-derived, not probed, not stale-cached — the presence check the builder already performs, inverted into delivery. Absent-handoff ⇒ raw-key entry for that provider (the pre-flip delivery path, unchanged bytes); present ⇒ token entry (the flip path). Per-workspace, per-provider: readiness is a property of the credential's staging state, not a global mode.
- **Mode flag:** `relayOnlyKeyDelivery.fallbackMode: migration|strict` (values.yaml + the builder's env; default **migration** at merge). Strict = the fallback is disabled ⇒ the current fail-closed zero-entry behavior (which remains the correct steady-state posture — it is what #1537's sweep asserts).
- **Counter:** `relay_fallback_deliveries_total` on the **API** (the process that builds batches and already owns the degrade reason strings), labels: `{workspace, provider_slug}`. PromQL alert (rendered in `prometheus-rules.yaml`, the `llmsafespaces` group): `increase(relay_fallback_deliveries_total[10m]) > 0` — any firing means migration is still live somewhere; the alert text names the runbook section.
- **Audit:** each fallback also writes the existing audit row with a distinct action (`relay_fallback_delivery`) — the per-workspace trail M4's condition links to.
- **Why fail-open is safe HERE, uniquely:** the fallback restores the pre-flip delivery — raw keys in pod files. That is the S1/S2/S3 surface design 0058 spent an epic removing, so the fallback trades confidentiality back for **availability** deliberately and *observably*: every instance is counted, alerted, and audited. The migration-mode default is the honest posture for a fleet whose staging producer just proved it can be silently absent; strict mode is earned, not assumed.
- **Strict-mode flip criteria (the runbook paragraph + one bool):** flip `fallbackMode: strict` when `relay_fallback_deliveries_total` has read zero for **7 consecutive days** fleet-wide AND the M3 gate is green on the current chart. Rollback is the bool back to migration (no re-flip machinery — the same lever shape as `relayOnlyKeyDelivery.api.enabled`).

**Tests (TDD):** builder table (absent handoff → raw-key entry + counter+audit; expired → same; present → token entry, no counter); strict mode → zero-entry fail-closed preserved (pin); counter label shape; alert rule renders + fires in the unit harness.

## 5. M3 — The posture gate (one CI job)

**Goal:** cold-install the chart **under its own mandated default posture** on kind, wait all-Ready, and assert the posture is actually livable. This subsumes the flip drill's mechanics, the provenance assertion, and the flip-evidence gate — one job, every default-posture change.

**The install (the shipped state, nothing more):** `helm install` with the chart's OWN defaults — `rbac.scope=namespace` (the relay-only guard's mandate), `relayOnlyKeyDelivery.enabled=true` (the flipped default), delivery pins set (`controller.agentdDelivery.*` / `controller.opencodeDelivery.*` — the posture #1546's Defect 2 exercised), `mcp.enabled=false` (issue #28), test DB/Redis — and **every other RBAC-requiring default in its shipped state**. If the shipped defaults cannot go all-Ready, the gate is red. That is the point.

**The assertions (four, in order):**
1. **All-Ready**: every Deployment in every rendered namespace reports ready within the window (this alone kills #1546's Defect 1 — the CrashLooping router — at the first cold install).
2. **The armed line**: the controller pod's log contains `relay-only key delivery enabled` (M1's boot-time contract, asserted cluster-side).
3. **Zero forbidden**: no `forbidden` line in any pod log in any namespace (kills Defects 2/3 and the design-gap flood — silent RBAC starvation becomes a red gate).
4. **Provenance — envelope stated honestly (r1)**: the running controller image's commit label equals the tag's commit. The label channel is stamped by the SAME release run from the SAME `github.sha` as the ldflags build-arg — identical attestation class, so a wrong-bits build carries the right label and this assertion CANNOT catch the stale-cache/wrong-tree drift that shipped 0.34.7 (§1's own trust model). What it catches: **running something other than this tag's pushed artifact** (a tag overwrite, a push mix-up, a stale mirror — the #1548 runbook's digest-check class). The **wiring-drift class** (a binary lacking the staging code) is caught by assertion 2 — the armed line is behavioral: a drifted binary without the wiring cannot emit it, and the gate goes red. Residual, named: a wrong-bits build that happens to contain the wiring but differs elsewhere passes both — that residue is the behavioral-canary question (#1548 runbook check 2) and stays owner-side until a stronger provenance channel (signing) exists; the gate does not claim it.

**Carries the #1546 fixes as its first catch (implementation, in this order):**
- **Defect 1**: split the router keypair rule — unscoped `create` on secrets in `llm-relay` + name-scoped `update` for the two keypair Secrets (the router is the llm-relay Secrets' key owner with get/list/watch already; unscoped create adds no meaningful surface — exactly the #863/#890 agentd-pins precedent the chart already documents).
- **Defect 2**: the pins Role gains `get/list/watch` on configmaps (release namespace) — the informer's actual requirement.
- **Defect 3 — re-derived (r1), dropped as a shipped defect**: the refresher is flag-gated in every merged tree (`controller/main.go:456`, unchanged since `c2a8c3c2`), and the chart renders the flag AND its RBAC from the SAME values key — consistent by construction. #1546's observed "unconditional run" was the drifted binary, not a shipped desync (the #1548 RCA this design cites established exactly this; the r0 draft contradicted its own citation — caught by review). **No unconditional grant lands** — that would widen the shipped RBAC posture (configmap writes even with `freeModelsRefresher.enabled=false`) to fix a class correct provenance eliminates. What ships instead: (a) the gate's assertion 3 (zero forbidden) covers refresher-RBAC starvation generically, exactly as it covers Defect 2's class; (b) a render-pin that the flag and its RBAC remain key-coupled (the template's same-values-key structure asserted — the desync class becomes a test failure, not a posture question).
- **Design gap**: an always-created, relay-only-safe `inferencerelays` ClusterRole (CRD watch only, no Secrets) under namespace scope — see §8.

**Home:** a new `.github/workflows/posture-gate.yml`, `workflow_dispatch` + `pull_request` paths on `helm/**` + `controller/**` (the posture surface), reusing the e2e-nightly kind bootstrap steps verbatim (the established image-build + kind + helm sequence). The **contribution rule** (§9's replacement for a flip-manifest system): *a PR flipping any multi-component default must include posture-gate evidence covering the new default posture* — the gate is that evidence, mechanically.

**Tests:** the workflow's own shape pinned (`local/posture_gate_workflow_test.go`: the four assertions present, the default-posture install command pinned, the helm/controller path triggers); each shipped #1546 fix pinned by a helm-render test (the split keypair rule, the pins verbs, the always-created ClusterRole) plus the refresher flag↔RBAC key-coupling render-pin (§5's Defect-3 replacement).

## 6. M4 — `CredentialsStaged=False` CRD condition

**Goal:** a degraded batch surfaces on the Workspace CRD — visible to the UI and alertable — not only in API bootstrap logs.

**Semantics.** The API already computes the degrade (`relay_staging_not_ready`, `relay_fallback_delivery` post-M2) at batch-build time. M4 writes it to the Workspace status as the **existing** `CredentialsStaged` condition set to `False` with the degrade reason (and back to `True` on the next clean batch) — via the same status-path the conditions already ride. No new condition type, no new surface: the US-72.3 condition, fed by the batch outcome.

**Honest scope marker (per the ruling):** this is the one **preference item** — the M2 counter already alerts; M4 adds user-facing visibility ("why does my workspace have no models" answered on the object). If the review wants it trimmed, the design stands on M1–M3.

**Tests:** degrade → `CredentialsStaged=False/relay_staging_not_ready` on the CRD; clean batch → back to `True`; M2 fallback → `False/relay_fallback_delivery` (the migration visible per-workspace).

## 7. Non-goals

- **Not** re-litigating 0058's envelope/token/resolve design — the mechanisms harden its delivery, they do not alter it.
- **Not** touching the router's resolve path, KMS posture, or the §4.3 no-read guarantee (the #1546 fixes are RBAC *shape* corrections inside the same guarantee; the unscoped-create split was pre-approved by the key-owner argument).
- **Not** the ops-repo values discipline (§8.2 — stated, out of repo scope).
- **Not** a general RBAC audit — only the four #1546 items the gate would have caught.

## 8. Dispositions (recorded, not solved here)

**8.1 The #1546 design gap (inferenceRelay coexistence).** Namespace-scope installs need the `inferencerelays` watch for the fleet reconciler to coexist with relay-only. The fix ships WITH M3 (an always-created ClusterRole: `get/list/watch` on `inferencerelays` only — a CRD read, no Secrets, no §4.3 conflict; created unconditionally so the posture never depends on the fleet being enabled). Production's interim (`inferenceRelay.enabled=false`) is documented in the runbook as the pre-gate workaround.

**8.3 The #1546 operational note (helm rollback cascades the chart-created llm-relay Namespace and any out-of-band RBAC patches with it).** Mooted by this design: the cascade's payload was the out-of-band patches operators needed because the chart lacked the grants — with the grants in-chart (M3's fixes), there is nothing out-of-band to lose, and a rollback/retry cycle is just a namespace re-create away from a clean bootstrap. Recorded so the mooting is explicit, not implicit.

**8.4 The #1541 nightly drill lane (AC5's clause).** The posture gate SUBSUMES the drill lane's evidence role (M3 asserts, pre-merge and on every posture change, what the nightly drill asserted post-hoc) — the "#1541 unblocked or the flip reverted" precondition is therefore MOOTED-BY-GATE for future flips, recorded here. The nightly lane itself remains useful as soak (the #1456 wiring lane's scope); it is no longer the flip-evidence gate. If the owner prefers the lane unblocked anyway, that is an owner-side call, not a design dependency.

**8.2 The Epic-42 values drift (#1548's secondary regression).** Release 508 dropped `inferenceRelayURL`/`inferenceRelay.enabled` during the 506-fail → 507-rollback → 508-retry sequence — reconstructed values, not reviewed ones. **Recommendation (out of repo scope, stated for the owner's ops repo):** the wiring lives in ONE reviewed values file; rollback/retry sequences re-apply that file verbatim and never reconstruct values ad hoc. Recorded as a talos-ops-prod note; the repo-side half is M3's contribution rule (posture-affecting changes carry evidence, and values reconstruction is a posture change).

## 9. Rejected alternatives (binding, with reasons)

| Candidate | Rejected because |
|---|---|
| **API-side heartbeat derivation / auto-follow** (the API probing controller health and deriving emission state) | Speculative automation for a twice-yearly migration; adds cross-namespace coupling that is itself posture surface. M1's boot-time assertion + M2's counter detect the same condition at the moment of harm, with zero new trust paths. |
| **A staleness heartbeat/gauge** (controller writes liveness; API compares age) | Subsumed by the fallback counter — which fires at the exact moment harm would begin (a degraded batch), not on a timer's opinion about freshness. The owner's ruling: detectors ride on work that already happens. |
| **A separate digest↔commit CI job** | Folded into M3's assertion 4 — a second job would duplicate the gate's cluster for one grep. |
| **A flip-manifest CI system** (a machine-checked manifest of which components a flip touches) | A contribution-rule sentence (§5) carries the same enforcement at a fraction of the surface: flips of multi-component defaults must include posture-gate evidence. The gate IS the manifest's check, executed. |

## 10. Acceptance criteria (the design's, for the implementation stories)

- [ ] M1: enabled-but-unarmable controller exits 85 within the 30s window; the enable line is the armed contract; parity failures take the same code; flag-off byte-identical.
- [ ] M2: the builder table (absent/expired handoff → raw-key + counter + audit; present → token); strict mode preserves the fail-closed zero-entry behavior; the alert renders and fires; the migration-mode default lands with the flip criteria in the runbook.
- [ ] M3: the gate cold-installs the shipped defaults all-Ready and asserts the four checks; the three shipped #1546 fixes land first and are each render-pinned (Defect 3 re-derived: no fix ships, the key-coupling pin does); the workflow shape is pinned; the contribution rule lands in README-LLM.
- [ ] M2/M4 e2e (owns #1548 AC4, closes AC1's first-handoff clause): the migration scenario on the kind cluster — credentials bound pre-flip stage on the first post-flip Creating/Active reconcile, the batch carries TOKENS, the first `workspace-relay-*` handoff appears, and zero `relay_staging_not_ready`/`relay_fallback_delivery` degrades fire (the not-ready arm exercised separately by killing the staging Secret: the raw-key fallback delivers, the counter increments, `CredentialsStaged=False/relay_fallback_delivery` lands on the CRD). The literal AC4 text ("no relay_staging_not_ready") is SUPERSEDED by M2 — in migration mode the not-ready outcome is a counted fallback delivery, not a degrade; that supersession is recorded HERE, not left implicit.
- [ ] M4: degrade/clean transitions on `CredentialsStaged`; the fallback reason is per-workspace visible.
- [ ] #1548's disposition map: AC1 (root cause + enable line + first handoff) ← M1 + the parity PR + the M2/M4 e2e story's first-handoff assertion; AC2 ← M2/M4 (the heartbeat class rejected, §9); AC3 ← M4; AC4 ← the M2/M4 e2e story (literal text superseded by M2, recorded above); AC5 ← §8.4 (mooted-by-gate); AC6 ← §8.2 (owner-side); AC7 ← this worklog. The cluster-side digest checks remain owner-side per the #1548 thread.
- [ ] Worklog per repo rules; design doc registered in README-LLM.

## 11. Open questions (for review)

1. **M2's handoff-absence read:** the builder reads the workspace's handoff Secret presence directly (a Secrets LIST in the release namespace per batch build — the API's existing client) vs. the controller pushing readiness into the batch manifest. The design leans **direct read** (one source of truth, no new push path); reviewer confirm.
2. **M1's exit code:** 85 extends the ladder cleanly — confirm no collision in the runbook's grep surface.
3. **M3's trigger breadth:** `helm/**` + `controller/**` only, or also `api/**` (the builder is posture-relevant post-M2)? The design leans adding `api/**`.
