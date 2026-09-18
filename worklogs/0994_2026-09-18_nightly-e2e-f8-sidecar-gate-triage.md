# Worklog: Nightly e2e F8 silent-skip + Epic 68 sidecar-gate misdetection (issue #1456)

**Date:** 2026-09-18
**Session:** Redo of the #1456 fix from the triage spec (the announced branch `feat/issue-1456-nightly-sidecar-gate-f8-lookup` / dd0a3308 was never pushed to origin and never PR'd — re-verified; this session re-implements it TDD-first on `fix/issue-1456-nightly-sidecar-gate-f8-lookup`)
**Status:** Complete — PR open, iterating review

---

## Objective

Unblock the nightly full-system e2e (`e2e-nightly.yml`), which has failed before all downstream rows (automation R1–R8, us-70, #1342 suites) since the attachment step's introduction, by fixing the two defects the #1456 triage proved:

1. `local/us-68-attachments-e2e.sh` sidecar gate probes only `.spec.containers[*].name`; since #980 the agentd sidecar is a NATIVE sidecar (init container, `restartPolicy: Always` — `controller/internal/workspace/agentd_sidecar.go:294,222`), so sidecar-mode nightly pods (`agentdSidecar.enabled=true`) were misread as single-container → row E2 ran → hit the sidecar's read-only `/workspace` (`agentd_sidecar.go:198`) → agentd 5xx → API 502.
2. The F8 step's Service discovery used `kubectl get svc -l app=valkey`, but the valkey Service in `local/postgres-redis.yaml` carries NO metadata labels (the label is on the Deployment/pod-template and in the Service selector) — F8 silently SKIPped (exit 0) on every run in history. The reported "F8 NetworkPolicy regression" never existed; the NetworkPolicy is correct (chart `TestF8_ValkeyPolicyAllowsMigrateJob` guards it; re-rendered below).

Plus the owner-endorsed #1459 withdrawal hardening deltas 1 and 2 (delta 3 dropped, concrete reason below).

---

## Work Completed

### Core fix (commit 311970fc)

- **Pin tests first (red)** — new `local/us68_attachments_script_test.go`:
  - Structural: gate jsonpath must cover BOTH `{.spec.containers[*].name} {.spec.initContainers[*].name}`; the containers-only probe form must be gone.
  - Executable: the script's REAL gate block extracted and run with a fake `kc` — a native-sidecar pod (init container list containing `agentd`) takes the sidecar path (D1 clean-fail assert + LOUD `SKIPPED E2/E10/E11` + exit 0); a single-container pod (init containers, no `agentd`) falls through to the rows; a non-5xx sidecar upload dies (D1 assertion live).
  - Structural F8 pins: by-name lookup (`get svc valkey`), anti-pin on `-l app=valkey` (step-scoped, absolute — even a comment quoting it fails, which caught my own first comment draft), SKIP-on-absent guard, why-comment fragment.
  - Executable F8: the REAL step script extracted from the YAML (`run: |` body, 10-space indent stripped, `${{ env.NS }}` rendered exactly as GitHub does) and run against a fake `kubectl` + stubbed `sleep` on PATH, with every invocation traced. Red run reproduced the production bug empirically: against a live `svc valkey`, the pre-fix step printed `SKIP: valkey Service not found` (label lookup matched nothing → fake kubectl rejected it → empty IP → silent skip).
- **Fixes**: gate renamed `SIDECAR_CONTAINERS` → `CONTAINER_NAMES`, probes both lists, same `*"agentd"*` substring decision; F8 looks up `svc valkey` by name with the why-comment, SKIP guard retained.

### Hardening delta 2 — diagnostics before delete (commit ea93d426)

Pre-fix, the step deleted the probe pod before any diagnostics existed (describe would have described a corpse). Failing verdict now dumps `describe pod` + the probe's events (field-selector scoped) BEFORE the delete; green leg still deletes. Pinned by trace-order assertions (describe/events indices < delete index) + the reachable leg still cleaning up.

### Hardening delta 1 — three-way verdict (commit 8c44d319)

Classification on RESULT emptiness, phase re-fetched AFTER the logs read (wait-loop sample can be stale):
- `REACHABLE` → OK, exit 0.
- verdict without REACHABLE (`BLOCKED`) → hard FAIL + datastore NetworkPolicy dump (`get networkpolicy -o yaml`), exit 1.
- empty logs (probe never ran to completion — pull/scheduling/API flake) → WARN + describe/events, exit 0 — a no-verdict leg no longer holds the ten downstream rows hostage while a real verdict still hard-fails. A terminal phase with empty logs also lands on the WARN leg (pinned) — unverified ≠ blocked.
- Wait-loop break converted to the `if/then` form (repo-codified set -e discipline per the us70 pin; this loop executes for the first time under this fix since the step always SKIPped before).

### Delta 3 — REDISCLI_AUTH — DROPPED, concrete reason

The delta targets the withdrawn branch's own probe, which used `redis-cli -a "$REDIS_PW"`. This workflow's probe is a credential-less busybox `nc -z` TCP connect test: `REDIS_PW`/`redis-cli`/`valkey-cli`/`REDISCLI` appear NOWHERE in `e2e-nightly.yml` (grep-verified, exit 1). Retrofitting a credential into a credential-less probe in order to hide it would increase secret surface — the opposite of the delta's intent. (The valkey Deployment's own readiness probe already uses in-pod `REDIS_PASSWORD` env, untouched.)

---

## Key Decisions

1. **Keep the `nc -z` probe** (no redis-cli PING upgrade): F8's question is TCP reachability under the NetworkPolicy, which is what the policy gates; the triage's expected output (`OK: migrate-labeled pod reached Valkey:6379 (F8)`) matches this probe. Any busybox/nc flag issue would surface as a loud BLOCKED hard-fail with diagnostics, not a silent skip.
2. **Substring gate on combined container names is unambiguous**: verified against pod_builder.go:87 (`workspace`) and platform_init.go (`platform-init`, `platform-bootstrap`, `platform-materialize`, `platform-dirs`, plus `workspace-setup`/`credential-setup`) — no container name other than the sidecar contains `agentd`. Documented in the script comment.
3. **Absolute anti-pin on `-l app=valkey`** (fails even inside comments): the pin caught my own first comment draft quoting the selector — evidence it works; copy-paste reuse of the dead selector cannot return.
4. **Test harness quoting bug found and fixed during self-review**: `t.TempDir()` paths embed the subtest name (may contain `(`); unquoted `FAKE_TRACE=<path>` concatenation was a latent bash parse error — fixed with `shQuote`; suite now stable across `-count=5`.
5. **Assumptions stated and validated (Rule 7)** — see Blockers/Tests for evidence:
   - The valkey Service carries no metadata labels → verified by reading `local/postgres-redis.yaml:156-166`.
   - The agentd sidecar is an init container named `agentd` → `agentd_sidecar.go:41` (`agentdSidecarContainerName = "agentd"`), appended to `pod.Spec.InitContainers` (`agentd_sidecar.go:294`).
   - The rendered nightly policy admits the probe's labels → helm render below.
   - GitHub renders `${{ env.NS }}` before the step script runs → standard Actions semantics; the test performs the same substitution.

---

## Blockers

None. Runtime arbitration happens on the next nightly (06:00 UTC 2026-09-19): F8 runs for the first time in history; the attachment step should print the sidecar clean-fail + `SKIPPED E2/E10/E11`. Failures in newly-unblocked downstream rows (automation R1–R8 etc.) would be new coverage surfacing, not regressions of this fix — per the triage.

---

## Tests Run

- `go test -timeout 120s ./local/ -run 'TestUS68'` — RED pre-fix (all new pins), GREEN post-fix.
- `go test -timeout 300s ./local/` (full package) — **ok** (35.4s first full run; 7.3s warm).
- `go test -timeout 300s ./local/ -run 'TestUS68' -count=5` — **ok** (flakiness check after the quoting fix).
- `go test -timeout 300s ./helm/ -run 'TestF8|TestG26'` — all 8 **PASS** (after installing helm v3.16.4 to /tmp/opencode/bin; they SKIP without it).
- `bash -n local/us-68-attachments-e2e.sh` — clean (also pinned by `TestUS68AttachmentsScript_BashSyntax`).
- YAML validity of `e2e-nightly.yml` — `python3 -c yaml.safe_load` → VALID.
- Nightly-args render proof (helm v3.16.4, `--kube-version 1.35.0`, release `llmsafespaces`, ns `llmsafespaces`, synthetic delivery digests, `agentdSidecar.enabled=true`, `rbac.scope=cluster`, `mcp.enabled=false`): `llmsafespaces-valkey-ingress` (targets `app: valkey`) includes an ingress rule `from: podSelector.matchLabels {app.kubernetes.io/component: migrate, app.kubernetes.io/instance: llmsafespaces}` port 6379 — exactly the probe pod's labels. The policy admits the probe; with the by-name lookup F8 will actually verify it.
- No cluster runtime validation possible in this workspace (no docker/kind) — stated in the PR.

---

## Next Steps

- Iterate the adversarial PR review until APPROVED; orchestrator adjudicates merge (target: before 06:00 UTC nightly).
- Watch the next nightly: expect `OK: migrate-labeled pod reached Valkey:6379 (F8)` (first real F8 run) and the attachment step's sidecar clean-fail + skip; triage any newly-surfaced downstream failures on their own merits.

---

## Files Modified

- `local/us-68-attachments-e2e.sh` — sidecar gate probes both container lists; comment; rename to `CONTAINER_NAMES`.
- `.github/workflows/e2e-nightly.yml` — F8 by-name Service lookup + why-comment; three-way verdict; diagnostics-before-delete; if/then loop form.
- `local/us68_attachments_script_test.go` — NEW: structural + executable pins (fake kc / fake kubectl with invocation trace).
- `worklogs/0994_2026-09-18_nightly-e2e-f8-sidecar-gate-triage.md` — this worklog.
