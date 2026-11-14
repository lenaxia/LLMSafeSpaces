# U-7 — persona/role registry (pi v1.5.2)

Question: can pi named sub-agents/personas be declared per-config (the
platform's board persona / role-ref model), or is materialization
prompt-level only?

## Sources (fetched at tag v1.5.2)

- `packages/coding-agent/docs/settings.md` — the FULL settings key surface
  (Model/thinking, Interaction, Tools, Sessions, Compaction, Terminal, …)
- `packages/coding-agent/docs/extensions.md` (3,367 lines; built-in
  extension table + config surface)
- `packages/coding-agent/docs/packages.md` (project/user packages)

## Findings

- `settings.md` contains **no persona, agent, or named-role key** of any
  kind — no registry exists in user, agent-dir, or project settings.
- The only sub-agent surface in the tree is the built-in **`subagent/`
  extension** (extensions.md:3346: "Spawn sub-agents — `registerTool`,
  `exec`") — i.e. sub-agents are an EXTENSION that spawns processes, not a
  declarative config construct.
- Prompt-shaping config surfaces: `SYSTEM.md` (replace) / `APPEND_SYSTEM.md`
  (append) under the agent dir, project `.pi/SYSTEM.md` (trust-gated), and
  the `--append-system-prompt <text|path>` CLI flag (cli.md:226-233).

## Ruling input (feeds design 0073 §2.2 consequences)

Confirmed: prompt-level materialization is the ONLY mechanism at v1.5.2.
The platform's persona model (board role-refs, stage personas) renders to
pi as APPEND_SYSTEM.md content via the S3 configwriter's prompt sources;
pi workspaces advertise no named-agent capability. Re-check the extensions
surface on version bumps — an upstream registry would be an adoption
opportunity, not a break (the adapter never assumed one).
