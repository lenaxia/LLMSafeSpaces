# Worklog: the #1546 posture RBAC fixes (design 0061 §5, the posture gate's first catch)

**Date:** 2026-09-23
**Session:** Design 0061 implementation PR1 (the recorded order's first item): the three shipped #1546 fixes + the flag↔RBAC key-coupling render pin, each render-pinned. GO per the orchestrator after the design merged (#1549, squash 2077cdd7).
**Status:** Complete

---

## Objective

Make the chart's own mandated posture (namespace scope + relay-only + delivery pins) livable: the split keypair rule (Defect 1), the pins informer grant (Defect 2), the always-created relay-safe inferencerelays ClusterRole (the design gap), and the key-coupling pin (Defect 3's replacement — the desync class becomes a test failure).

## Work Completed

- **Defect 1** (`helm/templates/llm-relay-rbac.yaml`): the keypair rule SPLIT — unscoped `create` on secrets + name-scoped `update` for exactly the two keypair Secrets. The comment names the apiserver semantics (resourceNames ignored for create — dead RBAC that CrashLooped the router) and the key-owner argument for why unscoped create adds no meaningful surface (the #863/#890 precedent the chart already documents).
- **Defect 2** (`helm/templates/rbac.yaml`): ONE shared unscoped `get/list/watch`-on-configmaps rule under the either-pin conditional, with the comment naming the informer semantics (the first CACHED Get starts a reflector that LISTs; resourceNames-scoped list is as dead as scoped create) and the release-namespace scoping (the leader-election Role).
- **The design gap** (`helm/templates/rbac.yaml`): the always-created `…-controller-relay-safe-crd-watch` ClusterRole + binding — `inferencerelays` get/list/watch ONLY (apiGroup `llmsafespaces.dev`, the CRD's actual group — caught my own wrong first guess `relay.llmsafespaces.dev` by checking the CRD manifest), rendered under NAMESPACE scope whenever `inferenceRelay.enabled` — the fleet coexists with the mandated posture; no Secrets, no §4.3 conflict.
- **The key-coupling pin** (the Defect-3 replacement, `helm/posture_rbac_fixes_test.go`): for BOTH values of `controller.freeModelsRefresher.enabled`, the configmap write-grant and the pod arg correlate — the flag/RBAC desync class is now a test failure, not a posture question.

## Red-first record (all three defect pins)

- `TestRelayRouterKeypairRuleSplit` — RED against the unsplit rule (the dead create+resourceNames combination failed the no-dead-letter assertion); GREEN after the split.
- `TestPinsRoleGrantsInformerListWatch` — RED once the render pinned the refresher OFF (the first version passed VACUOUSLY: the refresher defaults enabled and its grant satisfied the predicate — caught by the debug dump, fixed by rendering `freeModelsRefresher.enabled=false` + `inferenceRelay.enabled=false` so ONLY the pins grants are in play).
- `TestInferenceRelayNamespaceScopeClusterRole` — RED (the ClusterRole did not exist); GREEN after.
- `TestFreeModelsRefresherFlagRBACKeyCoupling` — GREEN against the current templates BY DESIGN (it is a PIN, not a fix — the coupling already holds; the predicate needed the configmaps scoping after the leases rule (get list watch create update patch delete on LEASES) false-matched the first version).

## Key Decisions

1. The informer grant is a shared either-pin rule (one rule, not two per-pin duplicates — the requirement is the informer's, not the pin's).
2. The ClusterRole carries ONLY the CRD watch — the design's relay-only-safe boundary; the cluster block keeps its broader version for cluster installs.
3. The coupling test asserts correlation in BOTH directions (grant⇔arg) across both settings — the desync has no direction to hide in.

## Blockers

None.

## Tests Run

- `go test ./helm/` — FULL package green with real helm (31.9s; includes the four new pins + all prior suites).

## Next Steps

1. PR1 review rounds; then the recorded order continues: M1 crash-loud arming (exit 85).

## Files Modified

- `helm/templates/llm-relay-rbac.yaml` (Defect 1)
- `helm/templates/rbac.yaml` (Defect 2 + the design-gap ClusterRole)
- `helm/posture_rbac_fixes_test.go` (new: the four render pins)
