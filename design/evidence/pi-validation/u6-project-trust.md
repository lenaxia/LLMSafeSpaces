# U-6 — project trust when `/workspace` contains `.pi/` (user-controlled PVC)

Question: does RPC mode auto-trust, prompt, or deny project-local config, and
can a headless runner pin the decision?

## Sources (fetched at tag v1.5.2)

- `packages/coding-agent/docs/security.md` (§ Understand project trust, § Project trust without an interactive prompt)
- `packages/coding-agent/src/core/project-trust.ts` (`resolveProjectTrusted`)
- `packages/coding-agent/src/core/trust-manager.ts` (resource detection, trust.json)

## Findings

Protected resources (security.md): `.pi/settings.json`, `.pi/mcp.json`,
`.pi/extensions|skills|prompts|themes`, `.pi/SYSTEM.md|APPEND_SYSTEM.md`,
`.agents/skills` — and **a bare `.pi/` directory does not require trust**.
Context files (`AGENTS.md`, `CLAUDE.md`, …) load regardless of trust.

Decision ladder (`resolveProjectTrusted`, project-trust.ts):

```
1. trustOverride            (--approve / --no-approve CLI)   — wins
2. project_trust extension handler
3. saved decision            (~/.pi/agent/trust.json)
4. defaultProjectTrust setting (default "ask")
5. "ask" + no UI  →  return false                            — RPC resolves DENY
```

security.md states it verbatim: "Print, JSON, and RPC modes cannot show the
built-in trust prompt. … `defaultProjectTrust: \"ask\"` or `\"never\"` skips
them. Use `--approve` or `--no-approve` when an automated run needs an
explicit one-time decision."

So on a user-controlled PVC: RPC children SKIP protected `.pi/` resources by
default — user-supplied extensions/MCP/system-prompt files never load into
the harness process. The one pre-trust lookup (project `sessionDir` setting,
security.md) is moot for the platform: the supervisor passes `--session-dir`
explicitly, which overrides the setting.

## Ruling input (feeds the S7 hardening)

The default outcome is safe, but it rests on the no-UI fallback inside
`resolveProjectTrusted` — a pi change to that fallback would silently flip
platform posture. S7 therefore pins it explicitly: `piChildCmd` now passes
`--no-approve` on every spawn (cmd/workspace-agentd/pi_rpc.go, argv pinned by
`TestPiChildCmdArgv`). Defense-in-depth: the outcome is headless-pinned, not
fallback-dependent.
