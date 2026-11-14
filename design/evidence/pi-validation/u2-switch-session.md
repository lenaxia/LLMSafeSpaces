# U-2 — switch_session while streaming (pi v1.5.2 source read)

Question: does RPC mode refuse, block, or destroy an in-flight turn when
`switch_session` arrives mid-stream?

## Sources (fetched at tag v1.5.2)

- `packages/coding-agent/src/modes/rpc/rpc-mode.ts` (846 lines)
- `packages/coding-agent/src/core/agent-session-runtime.ts` (460 lines)
- `packages/coding-agent/docs/rpc-commands.md` § switch_session

## Findings

The RPC command handler has NO busy guard — it forwards straight to the
runtime host:

```ts
// rpc-mode.ts:624-629
case "switch_session": {
    const result = await runtimeHost.switchSession(command.sessionPath);
    ...
    return success(id, "switch_session", result);
}
```

The runtime host does NOT block on the streaming turn — it **aborts it,
persists the aborted turn to the OUTGOING session file, then swaps**:

```ts
// agent-session-runtime.ts:167-178
private async teardownCurrent(reason, targetSessionFile?) {
    // Settle any active response first so the aborted turn (including tool
    // results) is persisted to the outgoing session before it is replaced.
    await this.session.abort();
    await emitSessionShutdownEvent(...);   // session_shutdown {reason, targetSessionFile}
    this.beforeSessionInvalidate?.();
    this.session.dispose();
}

// agent-session-runtime.ts:196-224  (switchSession)
async switchSession(sessionPath, options?) {
    const beforeResult = await this.emitBeforeSwitch("resume", sessionPath);
    if (beforeResult.cancelled) return beforeResult;      // extension veto
    ...
    await this.teardownCurrent("resume", sessionManager.getSessionFile());  // ABORT + swap
    ...
}
```

The only refusal surface is the `session_before_switch` extension event —
rpc-commands.md: "Can be canceled by a `session_before_switch` extension
event handler." Response `data.cancelled` distinguishes veto from swap.

## Ruling input (feeds design 0073 §2.1)

- pi-level `switch_session` mid-stream = **destructive abort-and-swap**
  (the aborted turn IS persisted — content survives, the turn does not).
- The scoping report's "a busy session blocks switching" (§1.4(b)) is
  imprecise: nothing in v1.5.2 refuses the switch; the cost is the aborted
  turn of whatever session was active. A multi-session pod where platform
  session B's prompt kills platform session A's in-flight turn is
  unacceptable — which is exactly why the platform-side single-process
  policy refuses while busy (`errPiBusySwitch`, pi_policy.go) and why
  per-session is the default: the destructive semantics are unreachable
  through the supervisor.
