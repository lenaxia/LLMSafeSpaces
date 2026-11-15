# U-4 — `models.json` `!command` execution context (pi v1.5.2)

Question: cwd/env of the spawned command, failure surfacing, and the precise
scope of "runs at request time and is not cached".

## Sources (fetched at tag v1.5.2)

- `packages/coding-agent/src/core/resolve-config-value.ts` (287 lines — the whole mechanism)
- `packages/coding-agent/src/core/provider-composer.ts` (`composeApiKeyAuth.resolve`, ~line 466-480)
- `packages/coding-agent/src/core/model-runtime.ts:242-266, 680` (request-time call site)
- `packages/coding-agent/docs/models.md` ("Commands in models.json run at request time and are not cached")

## Findings

**Mechanism** (`resolve-config-value.ts`): a value starting with `!` is the
command (`config.slice(1)`); `$NAME`/`${NAME}` are env templates; `$$`/`$!`
escape literals. Execution:

```ts
function executeWithDefaultShell(command: string): string | undefined {
    const output = execSync(command, {
        encoding: "utf-8",
        timeout: 10000,
        stdio: ["ignore", "pipe", "ignore"],   // stdout only; stderr discarded
    });
    return output.trim() || undefined;
}
```

- **cwd**: the pi process's cwd (no override) — `/workspace` for platform children.
- **env**: the pi process's full inherited environment (no scrubbing); the
  per-call `env` map (auth-context env) takes precedence per-name for `${VAR}`.
- **failure**: non-zero exit, timeout (10 s), or empty stdout → `undefined`;
  through `resolveConfigValueOrThrow` it becomes
  `Failed to resolve API key for provider "<id>" from shell command: <the command>`
  — the error NAMES the command (operators see what ran; the command string
  is config, not a secret — the resolved value never appears).
- **Caching — the doc quote is precise**: a process-lifetime
  `commandResultCache` exists, but models.json apiKeys and headers resolve
  via `resolveConfigValueOrThrow` → **`resolveConfigValueUncached`** — a
  fresh `execSync` on every resolution. The cached path serves `auth.json`
  `!command` keys (providers.md: "runs the command when the key is first
  needed and caches its standard output for the process lifetime").

**Call site — per model request**: `composeApiKeyAuth`'s `resolve` calls
`resolveConfigValueOrThrow(rawKey, ...)`; `ModelRuntime` invokes `getAuth(model,…)`
immediately before building each `requestModel` (model-runtime.ts:680 —
"request-time authentication", model-registry.ts:128). Scope: **every
stream/classify/images call** — i.e. per model request, not per prompt and
not per process.

## Ruling input (feeds design 0073 §3 config stance)

- The relay-token refresh plan is VALIDATED at code level: `apiKey:
  "!cat /sandbox-runtime/rt/relay-token"` re-executes per model request —
  the platform's resync machinery refreshes the file, no harness restart.
- Caveat recorded: `execSync` is synchronous — each resolution blocks the
  child's event loop up to the 10 s timeout. With per-session children the
  blast radius is that session only; a `cat` is sub-millisecond. Acceptable.
- `${RELAY_TOKEN}` env interpolation is the zero-exec alternative (also
  per-request resolution, no shell).
