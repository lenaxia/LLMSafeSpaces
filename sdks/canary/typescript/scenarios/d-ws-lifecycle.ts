// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// D-WS-LIFECYCLE canary — TypeScript SDK

import { LLMSafeSpaces } from '../../../typescript/src/index.js';
import { Runner, Config, configFromEnv, nodeFetch, waitActive, waitPhase } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: cfg.apiKey, timeout: 60000, fetch: nodeFetch as any });
  let wsId: string | null = null;
  try {
    const [ok, ws] = await r.assertNoError(
      () => c.workspaces.create({ name: 'canary-ts-lifecycle', runtime: 'base', storageSize: '1Gi' }),
      'create: no error');
    if (!ok || !ws) return;
    wsId = ws.id;

    const phase = await waitActive(c, wsId);
    r.assert(phase === 'Active', 'reach-active', `got "${phase}"`);
    if (phase !== 'Active') return;

    // Status fields on Active
    const [ok2, st] = await r.assertNoError(() => c.workspaces.getStatus(wsId!), 'get-status-active: no error');
    if (ok2 && st) {
      r.assert((st as any).imageTag !== '', 'status-active: imageTag non-empty', (st as any).imageTag);
      r.assert((st as any).agentHealth?.agentVersion !== '', 'status-active: agentVersion non-empty');
      r.assert(Array.isArray((st as any).conditions) && (st as any).conditions.length > 0, 'status-active: conditions non-empty');
      r.assert((st as any).agentHealth?.status === 'Healthy', 'status-active: agentHealth=Healthy');
      r.assert(((st as any).diskTotalBytes ?? 0) > 0, 'status-active: diskTotalBytes > 0');
    }

    // Suspend
    await r.assertNoError(() => c.workspaces.suspend(wsId!), 'suspend: no error');
    const sp = await waitPhase(c, wsId, 'Suspended', 60000);
    r.assert(sp === 'Suspended', 'suspend: phase=Suspended', `got "${sp}"`);

    // Double-suspend → conflict
    await r.assertError(() => c.workspaces.suspend(wsId!), 'double-suspend: ConflictError');

    // Activate
    await r.assertNoError(() => c.workspaces.activate(wsId!), 'activate: no error');
    const rp = await waitActive(c, wsId);
    r.assert(rp === 'Active', 'activate: phase=Active', `got "${rp}"`);

    // Activate already-Active → idempotent
    await r.assertNoError(() => c.workspaces.activate(wsId!), 'activate-already-active: no error');

    // Restart
    await r.assertNoError(() => c.workspaces.restart(wsId!), 'restart: no error');
    const rp2 = await waitActive(c, wsId);
    r.assert(rp2 === 'Active', 'restart: returns to Active', `got "${rp2}"`);

  } finally {
    if (wsId) { try { await c.workspaces.delete(wsId); } catch {} }
  }
}

async function main() {
  const r = new Runner('ws-lifecycle');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
