// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// D-ACTIVATE-EVICTION canary — TypeScript SDK

import { LLMSafeSpaces } from '../../../typescript/src/index.js';
import { Runner, Config, configFromEnv, nodeFetch, waitActive, waitPhase } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  const maxActive = parseInt(process.env.LLMSAFESPACES_MAX_ACTIVE_WORKSPACES_PER_USER || '3', 10);
  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: cfg.apiKey, timeout: 60000, fetch: nodeFetch as any });
  const wsIds: string[] = [];

  try {
    for (let i = 0; i < maxActive; i++) {
      const [ok, ws] = await r.assertNoError(
        () => c.workspaces.create({ name: `canary-ts-evict-${i}`, runtime: 'base', storageSize: '1Gi' }),
        `create-${i}: no error`);
      if (ok && ws) wsIds.push(ws.id);
    }
    r.assert(wsIds.length === maxActive, 'created-all', `${wsIds.length}/${maxActive}`);

    for (const id of wsIds) {
      const phase = await waitActive(c, id);
      r.assert(phase === 'Active', `active-${id.slice(0, 8)}`, `got "${phase}"`);
    }

    await r.assertNoError(() => c.workspaces.suspend(wsIds[0]), 'suspend-first: no error');
    const sp = await waitPhase(c, wsIds[0], 'Suspended', 60000);
    r.assert(sp === 'Suspended', 'suspend-first: phase=Suspended', `got "${sp}"`);

    const [ok2, result] = await r.assertNoError(
      () => c.workspaces.activate(wsIds[0]),
      'activate-eviction: no error');
    if (ok2 && result) {
      r.assert(typeof result.resumed === 'string' && result.resumed.length > 0,
        'eviction: has resumed field', JSON.stringify(result));
      r.assert(typeof result.suspended === 'string' && result.suspended.length > 0,
        'eviction: has suspended field', JSON.stringify(result));
    }

    const rp = await waitActive(c, wsIds[0]);
    r.assert(rp === 'Active', 'evicted-ws-active', `got "${rp}"`);

  } finally {
    for (const id of wsIds) {
      try { await c.workspaces.delete(id); } catch {}
    }
  }
}

async function main() {
  const r = new Runner('activate-eviction');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
