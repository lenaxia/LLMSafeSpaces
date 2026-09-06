# Worklog: fleet-wide "Model unavailable" — the auth-store write gap (#1296)

**Date:** 2026-09-06
**Session:** The user's fresh workspace showed no agent responses; every turn died "ModelUnavailableError" for every provider. Root-caused live on production; two stacked bugs, both in the auth-store path.
**Status:** Complete

---

## Objective

Make a fresh workspace's provider credentials actually work.

## Root cause (live-proven chain)

1. **opencode registers providers from its auth store (auth.json), not from agent-config** — a config apiKey alone does not make a model available to the session runner.
2. **The relay injector wrote auth.json 0640 uid-2000** — the #1119 fix made it readable, but opencode (uid 1000) must also WRITE it (its auth subsystem persists at boot and PUT /auth); PermissionDenied → 500 → provider init fails → the registry comes up EMPTY → "Model unavailable" for EVERY provider (relay included). Live-proven: PUT /auth 500 with "PermissionDenied: Failed to write auth data"; with the file made uid-1000-writable, the same PUT returned 200.
3. **Nothing ever wrote non-relay credentials to the store at boot**: the bootstrap materialize only flushed providers to agent-config; the reload path's StageCredentials PUTs to a live opencode (never up at bootstrap); the injector writes only the relay entry. Thekaocloud never landed in any fresh pod's store.
4. Intermittency explained: pods where opencode won the write race (created the file itself, uid-1000-owned) worked forever; injector-won pods wedged forever (mode persists on the PVC). The 863 pod: wedged since 2026-08-29 (the 0600 class). NOT a 0.27.x regression.

## The fix

1. auth.json lands **0660** (shared-gid read+write) — the injector's writes AND the bootstrap merge; the mode is umask-immune (chmod on a unique CreateTemp temp BEFORE the plaintext write; the fixed-name-temp variant inherited crashed runs' modes) and the rename resolves the symlink (resolvable OR dangling-first-boot — renaming onto a dangling link puts plaintext on the PVC, the r5 regression the fixture caught).
2. The bootstrap materialize **merges staged providers into auth.json** — payload parity with the live PUT /auth ({key, type, metadata.baseURL?}); preserves existing entries; the reserved `opencode` slug is skipped with an observable stderr line; failure is NON-FATAL (log + continue, the pre-boot relay doctrine — a CrashLoop wedges harder).

## Why no test caught it (the user's question)

The pool verifies env/file secrets (SD_FIRST, ~/.ssh) — never a provider-credential turn through a real opencode boot. The #1119 pin asserted READ readability (0640) — the exact assertion that made the WRITE gap invisible. The new pins assert 0660 (both halves) and the bootstrap merge.

## Key Decisions

1. 0660 not 0600+chown: the uid split (2000 writes, 1000 reads+writes) is bridged by the shared gid — the existing T2 exception, plus w.
2. Merge not replace at bootstrap: opencode's own live writes and the relay entry must survive.
3. Fail materialize (exit 3) on store-write failure: a partial-credential boot silent-wedges worse than a CrashLoop.

### Review r1 hardening

- Payload parity with the live PUT /auth path: metadata.baseURL when the provider carries one (the divergence was unpinned).
- The reserved "opencode" slug is skipped (a user provider under that literal would trip shouldSkipRelay's personal-key detection) — pinned.
- Failure doctrine reconciled: the merge fails NON-fatal (matches pre-boot relay's applied_auth_failed; a CrashLoop wedges harder than a degraded boot).
- A corrupt store at boot is surfaced on stderr, not silently replaced.
- The unreachable no-key skip case removed from tests (applyLLMProvider + Validate guarantee both non-empty).
- WIRING PIN: TestRunMaterialize_WritesProvidersToAuthStore drives the real runMaterializeCommand end-to-end — deleting the merge call fails it (the r1 finding: the behavioral fix shipped unpinned).

## Tests Run

- Full cmd/workspace-agentd + sessionstate suites green.
- New: TestUpdateAuthJSONForRelay_CrossUIDMode (0660 create + legacy repair), TestWriteStagedProvidersToAuthStore (merge preserves, baseURL parity, reserved slug, mode 0660), TestRunMaterialize_WritesProvidersToAuthStore (the wiring pin).

## Files Modified

- cmd/workspace-agentd/relay_injector.go — 0640 → 0660 (write + repair).
- cmd/workspace-agentd/secrets.go — writeStagedProvidersToAuthStore at bootstrap.
- Tests as above.
