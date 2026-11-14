# U-9 — `get_state` when a model call hangs mid-stream (pi v1.5.2)

Question: does `isStreaming` stay true through a hang; is there any stdout
keepalive; is `get_state` itself responsive?

## Sources (fetched at tag v1.5.2)

- `packages/agent/src/agent.ts` (state machine: `isStreaming` set/cleared)
- `packages/agent/src/agent-loop.ts` (959 lines — the streaming loop)
- `packages/coding-agent/src/core/agent-session.ts:1544-1551`
- `packages/coding-agent/src/modes/rpc/rpc-mode.ts:466-480` (get_state handler)

## Findings

`isStreaming` is the agent-run active flag — set true at run start, cleared
on completion/error:

```ts
// agent.ts (MutableAgentState)
this._state.isStreaming = true;    // run start
this._state.isStreaming = false;   // run end / error / abort
// agent-session.ts:1544
get isStreaming(): boolean { return this._isAgentRunActive; }
```

A hung model call (network stall awaiting fetch data) never ends the run →
**`isStreaming` stays true for the hang's duration** — the truth is
available; what is absent is any liveness HEARTBEAT:

- `grep -i 'heartbeat|keepalive|timeout|stall|watchdog'` over
  `agent-loop.ts` + `stream-fn.ts`: **zero hits**. No inactivity timer
  exists in the streaming path; `message_update` records are emitted only
  on real provider deltas. A hung stream = **stdout silence**.
- `get_state` latency during a hang: the command handler reads in-memory
  state (`rpc-mode.ts:473: isStreaming: session.isStreaming`) — the event
  loop is free during an async network stall, so `get_state` answers
  immediately with `isStreaming: true`.
- The one bounded exception: a SYNCHRONOUS block inside the child (e.g. the
  `!command` credential `execSync`, U-4 — 10 s ceiling) stalls `get_state`
  itself for up to that bound.

## Ruling input (feeds design 0073 §2.1 + the S2 health contract)

Health derivation MUST be supervisor-side timeout logic — which is exactly
what S2 shipped: the supervisor's health pass polls `get_state` per child
(responsive during hangs, `isStreaming` = the busy truth), surfaces
`OldestBusySeconds` for upstream escalation (D6: escalate, never
auto-kill), and restarts ONLY on mechanical wedge evidence (stdin not
draining). No platform change needed; the absence of a pi-native
keepalive is the documented reason the supervisor owns the clock.
