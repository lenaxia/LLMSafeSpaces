# U-3 — `get_entries since` cost on very large sessions (pi v1.5.2)

Question: is the `since` cursor an early-exit scan or a full parse per call?

## Sources (fetched at tag v1.5.2)

- `packages/coding-agent/src/modes/rpc/rpc-mode.ts:657-668` (command handler)
- `packages/coding-agent/src/core/session-manager.ts:1521-1537` (`getEntries`)
- `packages/coding-agent/docs/rpc-commands.md` § get_entries

## Findings

```ts
// rpc-mode.ts:657-668
case "get_entries": {
    const sessionManager = session.sessionManager;
    let entries = sessionManager.getEntries();          // in-memory array copy
    if (command.since !== undefined) {
        const sinceIndex = entries.findIndex((e) => e.id === command.since);
        if (sinceIndex === -1) {
            return error(id, "get_entries", `Entry not found: ${command.since}`);
        }
        entries = entries.slice(sinceIndex + 1);        // tail slice
    }
    return success(id, "get_entries", { entries, leafId: sessionManager.getLeafId() });
}
```

- The session file is parsed ONCE at open into `fileEntries`; `getEntries()`
  returns a shallow copy of the in-memory array. The cursor is
  `findIndex` (O(n) compare over ids) + `slice` — **no file re-read, no
  re-parse per call**; the wire carries only the delta.
- An unknown cursor id is a typed error (`Entry not found: <id>`) — not an
  empty page — so a stale/corrupt cursor is loud, and because entry ids are
  persisted in the file, a client restart's last-seen id resolves fine
  ("even across client restarts", rpc-commands.md).
- `get_entries` includes pre-compaction history and abandoned branches
  (documented) — the cursor cannot break across compaction.

## Ruling input (feeds design 0073 §2.1 cost model)

- Per-call CPU is O(n) id-compare + O(delta) response — cheap even at
  10^5 entries.
- The REAL large-session cost is resident memory: the whole session lives
  in RAM per child process. That is bounded by the per-session policy
  (~79 MiB idle + session size per ACTIVE session only; unattached
  sessions cost zero process and are list-rendered by the session-dir scan).
