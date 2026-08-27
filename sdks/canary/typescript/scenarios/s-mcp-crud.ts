// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// S-MCP-CRUD canary (#1046) — user-scope MCP server CRUD round-trip.

import { LLMSafeSpaces } from '../../../typescript/src/index.js';
import { jwtLogin, Runner, Config, configFromEnv, nodeFetch } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: await jwtLogin(cfg), timeout: 20000, fetch: nodeFetch as any });
  let srvId: string | null = null;
  try {
    const [ok, srv] = await r.assertNoError(
      () => c.mcpServers.create({ name: 'canary-ts-mcp', transport: 'http', url: 'https://mcp.example.com/mcp' }),
      'create-mcp: no error');
    if (!ok || !srv) return;
    r.assert(!!srv.id, 'create-mcp: id non-empty', srv.id);
    srvId = srv.id;

    const [ok2, got] = await r.assertNoError(() => c.mcpServers.get(srvId!), 'get-mcp: no error');
    if (ok2 && got) r.assert(got.name === 'canary-ts-mcp', 'get-mcp: name', got.name);

    const [ok3, list] = await r.assertNoError(() => c.mcpServers.list(), 'list-mcp: no error');
    if (ok3 && list) r.assert(list.some((s) => s.id === srvId), 'list-mcp: present', '');

    const [ok4, updated] = await r.assertNoError(
      () => c.mcpServers.update(srvId!, { name: 'canary-ts-mcp-renamed' }),
      'update-mcp: no error');
    if (ok4 && updated) r.assert(updated.name === 'canary-ts-mcp-renamed', 'update-mcp: renamed', updated.name);

    const [ok5] = await r.assertNoError(() => c.mcpServers.createAutoApply(srvId!, 'user'), 'auto-apply-create: no error');
    if (ok5) {
      const [ok6, rules] = await r.assertNoError(() => c.mcpServers.listAutoApply(srvId!), 'auto-apply-list: no error');
      if (ok6 && rules) r.assert(rules.some((x) => x.targetType === 'user'), 'auto-apply-list: user rule', '');
    }

    const [ok7] = await r.assertNoError(() => c.mcpServers.delete(srvId!), 'delete-mcp: no error');
    if (ok7) {
      srvId = null;
      await r.assertError(() => c.mcpServers.get(srv!.id), 'get-after-delete: error');
    }
  } finally {
    if (srvId) { try { await c.mcpServers.delete(srvId); } catch {} }
  }

  await r.assertError(() => c.mcpServers.get('00000000-0000-0000-0000-000000000098'), 'get-nonexistent-mcp: error');
  await r.assertError(
    () => c.mcpServers.create({ transport: 'http' } as any),
    'create-malformed-mcp: error');
}

async function main() {
  const r = new Runner('mcp-crud');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
