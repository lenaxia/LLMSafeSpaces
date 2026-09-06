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

1. auth.json lands **0660** (shared-gid read+write) — injector writes and the legacy-mode repair both.
2. The bootstrap materialize **merges staged providers into auth.json** (same {key,type} shape as opencode's own PUT /auth; preserves existing entries; 0660; MkdirAll the store dir).

## Why no test caught it (the user's question)

The pool verifies env/file secrets (SD_FIRST, ~/.ssh) — never a provider-credential turn through a real opencode boot. The #1119 pin asserted READ readability (0640) — the exact assertion that made the WRITE gap invisible. The new pins assert 0660 (both halves) and the bootstrap merge.

## Key Decisions

1. 0660 not 0600+chown: the uid split (2000 writes, 1000 reads+writes) is bridged by the shared gid — the existing T2 exception, plus w.
2. Merge not replace at bootstrap: opencode's own live writes and the relay entry must survive.
3. Fail materialize (exit 3) on store-write failure: a partial-credential boot silent-wedges worse than a CrashLoop.

## Tests Run

- Full cmd/workspace-agentd + sessionstate suites green.
- New: TestUpdateAuthJSONForRelay_CrossUIDMode (0660 create + legacy repair), TestWriteStagedProvidersToAuthStore (merge preserves, mode 0660, no-key skip).

## Files Modified

- cmd/workspace-agentd/relay_injector.go — 0640 → 0660 (write + repair).
- cmd/workspace-agentd/secrets.go — writeStagedProvidersToAuthStore at bootstrap.
- Tests as above.
