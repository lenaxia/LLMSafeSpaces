# Worklog: M3 — the posture gate workflow (design 0061 §5)

**Date:** 2026-09-23
**Session:** Design 0061 implementation, M3 lane (the gate itself): `.github/workflows/posture-gate.yml` + `local/posture_gate_workflow_test.go` structural pins. Merge sequenced AFTER M1 (the gate's assertion 2 consumes M1's armed line) and after the #1546 fixes lane (worklog 1056) — the gate is red on main until both land, by design.
**Status:** Complete (authoring; merge held for sequencing)

---

## Objective

One CI job that cold-installs the chart under its OWN mandated default posture on kind and asserts the posture is livable: the four §5 assertions in order (all-Ready, the armed line, zero forbidden, the commit-stamp envelope), on every PR touching the posture surface (§11 ruling-3 breadth: `helm/**` + `controller/**` + `api/**`).

## Work Completed

- **`.github/workflows/posture-gate.yml`** — the e2e-nightly bootstrap reused verbatim (kind v0.32.0, helm, the `lss-e2e-registry` digest-pin registry, the 2-node `kind-cluster-nightly.yaml` topology, the full image-build chain with `COMMIT_SHA="${{ github.sha }}"` stamps, cert-manager, the credentials Secret, test Postgres/Redis), plus the router image built from tree under a local ref (the drill-shape precedent — kind cannot pull the ghcr default; the override is environmental image plumbing, not posture).
- **The install** carries ONLY environmental overrides (image repos/tags/pullPolicy, delivery pins, `mcp.enabled=false` (issue #28 — no image exists), test DB/Redis, logging verbosity). Every posture lever — `rbac.scope`, `relayOnlyKeyDelivery.enabled`, `agentdSidecar.enabled`, `allowRelayRouterEgress` — reaches helm UNTOUCHED at its shipped default. If the shipped defaults cannot go all-Ready, the gate is red. That is the point.
- **The four assertions, in order, as separate unconditional steps**:
  1. `rollout status deployment --all` in BOTH rendered namespaces ($NS + `llm-relay`) — the #1546 Defect-1 catch (a CrashLooping router at first cold install).
  2. The controller log must contain `relay-only key delivery enabled` — M1's boot-time armed contract, asserted cluster-side.
  3. Zero `forbidden` lines in any pod log, any container, both rendered namespaces — the generic silent-RBAC-starvation catch (Defect 2's class and the refresher class).
  4. The running controller's own `starting controller … commit=<sha>` stamp must equal the `${{ github.sha }}` this run stamped the image with — the wrong-artifact class (tag overwrite / push mix-up / stale mirror). The §5 r1 envelope is stated honestly in the step comment: wrong-bits-with-right-stamp stays owner-side; the gate does not claim it.
- **`local/posture_gate_workflow_test.go`** — six structural pins (red-first against the absent file, green on the workflow):
  - (a) triggers: `workflow_dispatch` + pull_request paths EXACTLY the ruling-3 three-path breadth;
  - (b) the four assertion steps present, each carrying its literal, in §5 order, with no extra `Assert N` steps;
  - (c) the install command: every environmental override present, every posture lever BANNED as a substring (`rbac.scope=`, `relayOnlyKeyDelivery.enabled=`, `agentdSidecar.enabled=`, `allowRelayRouterEgress=`) — a gate that pins the posture off is testing an override and fails the pin;
  - (d) the provenance basis is ONE sha: the controller build stamp literal and the assertion-4 comparison literal are the same `${{ github.sha }}` expression — stamp/assert drift is a test failure before it is a forever-red or silently-passing gate;
  - (e) the nightly bootstrap contributions (topology config, registry, cert-manager, postgres-redis manifest, credentials Secret) are present verbatim;
  - (f) failure semantics: the assertions carry NO `if:` (a failed cold install IS a red gate — the #1541 arming exists for evidence lanes, not for the thing under test), and the failure-dump/teardown steps keep the crash-loud culture.

## Key Decisions

1. **Unconditional assertions.** The nightly's cancel-guard arming protects EVIDENCE lanes from unrelated row failures; here the install is the thing under test — a failed `helm --wait` already fails the job, and conditioning the assertions would only manufacture skip-paths around red gates.
2. **`--all` on rollout status across both rendered namespaces**, not a hand-list of deployments — a new Deployment the chart starts rendering is covered without remembering to extend a list.
3. **The provenance channel is the running binary's own startup line** (`pkg/version.CommitSHA`, the same ldflags channel the release stamps) — not a registry label lookup through the node's containerd, which would re-derive the same attestation with more moving parts on the kind path.
4. **Router from tree, local ref** (the drill-shape precedent): deterministic on PR runs, no external image dependency, and the router binary is this tree's code — the gate exercising it is coverage, not drift.
5. **`mcp.enabled=false` is environmental, not posture**: the mcp image does not exist (issue #28); a cold install would ImagePullBackOff on infrastructure that cannot ship. Recorded in the install comment and in pin (c)'s allowed list.

## Red-first record

All six pins RED against the absent workflow file (verified), GREEN on the authored workflow. Bash `-n` syntax-checked every run block; no step name carries the ` #` YAML-truncation risk (the nightly's pinned gotcha).

## Blockers

None. Merge sequencing is the orchestrator's: AFTER M1 (armed line exists to assert) and after the #1546 fixes (the all-Ready assertion would legitimately fail on main without them).

## Tests Run

- `go test ./local/ -run TestPostureGate -count=1` — 6/6 PASS.
- `go test ./local/ -count=1` — full package green (25.6s).
- `go vet ./local/` — clean; `gofmt` — clean.
