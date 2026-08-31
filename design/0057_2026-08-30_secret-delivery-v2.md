# 0057 — Secret Delivery v2: pull-based, terminal-verified credential delivery

**Status:** Accepted (2026-08-30) — epic #1158; normative content lives in that issue (five laws, invariants I1–I15, R1–R3, assumptions A1–A5, AC-1–AC-17, story map, gap register W1–W16)
**Date:** 2026-08-30
**Epic:** #1158 · **Folder:** `design/stories/epic-70-secret-delivery-v2/`
**Supersedes:** design 0052 Phases 2–4 + Phase-1 remainder (Phase-1 precursor #1114 remains shipped history)
**Depends on:** design 0051 (uid separation, §D1 credential), Epic 50/58 (KEK), #852 (session-aware restart)
**Composes with:** Epic 69 / design 0055 (4097 §D1 seam), design 0053 (S3 default-flip gated on US-70.1)

---

## Problem (verified live, 2026-08-30)

1. **Delivery is verified only to materialization — never to the agent process.** Sidecar pods deliver + materialize the batch correctly, then deterministically lose env-class secrets at every boot: the boot-time push dials a control socket in a container that cannot have started yet under native-sidecar startup gating (`connect: connection refused`, every boot; 3/6 fleet pods; suspend/resume re-breaks). Landed in US-4a (#1015).
2. Three batch builders, five DEK sources, five delivery paths disagree silently (design 0052 §3; the 2026-08-28 v0.25.1 incident).
3. Self-heal rests on a heuristic (`UserCredsPresent` + `secretautopush`) that is slow and error-looping.

## Design principles (the five laws)

1. **Pull, never push.** The consumer fetches at the moment of need; notify only shrinks latency.
2. **One builder, one truth.** Identical inputs → identical canonical bytes → identical revision.
3. **Verify at the point of consumption.** "Delivered" = what the agent spawned with / what files exist.
4. **Level-triggered, not edge-triggered.** Lost events cost seconds, never correctness.
5. **Loud or absent.** Only "owner has no secrets bound" is quiet; every other non-delivery carries a machine-readable reason.

## Key elements (normative detail in #1158)

- **R1 — two-tier revisions** (`manifest_rev = (seq, hash)` + `batch_hash`): US-70.2. Covers the **file manifest** too (see R2b).
- **R2 — supervisor spawn path: bounded wait + last-good-delta cache.** At every spawn the supervisor pulls the current delta from the sidecar user mux (4097, §D1 carve-out credential) with a bounded wait; on failure it spawns with the last-good delta from memory + `degraded:spawn_env_unavailable`; first boot with a dead sidecar spawns platform-env-only, loudly. Never-block-boot extends to never-block-spawn. **US-70.1 (landed 2026-08-30; cluster e2e + surface completion via #1164, 2026-08-31):** pull endpoint `/v1/spawn-env`, `spawned_rev` measured at the env the child actually spawned with, degrade codes plumbed healthz → CRD `secretsDelivery`, standing cross-uid matrix.
- **R2b — file-class ownership flip (#1165, added 2026-08-31).** Credential files are **written by the uid that consumes them** — ownership by construction, not post-hoc. The sidecar *stages* canonical bytes + a typed manifest `{target path, mode contract, bytes}`; the uid-1000 supervisor pulls (same §D1 seam, same `preSpawn`/reload hook) and writes into its own space. `reset()` becomes **manifest-scoped** (deletes exactly the manifest's entries — never directories), which also fixes the reload-wipe of uid-1000-owned files in `~/.ssh` (`known_hosts`, user keys). Per-type mode contracts (`id_*`→0600, ssh config→not-group-writable, `git-credentials`→0600, `secret-file`→user-settable default 0600) live in one table in `pkg/agentd/secrets`. Terminal verification: `files_rev`, self-computed over the files actually written, surfaced via the existing statusz → `secretsDelivery` path (file-class oracle for R1/US-70.3). Platform ssh `config` carries `Include ~/.ssh/config.d/*` so user fragments survive reloads.
- **R3 — standing cross-uid credential matrix:** every boot-path credential/file crossing (writer uid, reader uid, mode, outcome) enumerated via the CROSS_UID_FILES machinery; new crossings are added to the matrix, never fixed ad hoc. A2 validated with US-70.1. **Schema growth (#1165):** rows gain **owner-uid** and **consumer-constraint** fields — readability alone was insufficient to predict ssh's ownership refusal; rows added for every file class (ssh, `git-credentials`, `secret-file`) plus the reset blast-radius rule.

## Story map

| Story | Scope | State |
|---|---|---|
| US-70.0 | delivery test harness (fault injection, suspend/resume, gVisor) | open |
| US-70.1 | spawn-time env pull (R2) + `spawned_rev` + degrade codes + A2 + R3 matrix | **landed** (cluster e2e + surface completion via #1164) |
| #1165 fix | file-class ownership flip (R2b) + manifest-scoped reset + `files_rev` + R3 schema growth | **landed 2026-08-31** |
| US-70.2 | one builder + two-tier revisions (R1, covers the file manifest) + conditional pull endpoint | open |
| US-70.3 | notify-pull + reconcile loop + revocation + `secrets_resync`; consumes `files_rev` | open |
| US-70.4 | login-independent re-wrap reconciler | open |
| US-70.5 | demolition (incl. `pushInitialSpawnEnv` deletion) | blocked by 70.3+70.4 **and the #1165 fix** |

## Sequencing gates

US-70.1 lands **before** design 0053 S3's default flip (W16); reconcile (70.3) + re-wrap (70.4) land before demolition (70.5) removes the current self-heal. The **#1165 fix (R2b) lands before US-70.5** — legacy self-heal paths are not demolished while a live credential class (file-class in sidecar mode) is broken — and its manifest folds into US-70.2's revision model; nothing from the fix is discarded by later stories.
