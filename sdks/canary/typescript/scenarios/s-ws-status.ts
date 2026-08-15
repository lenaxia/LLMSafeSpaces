// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// S-WS-STATUS canary — TypeScript SDK

import { LLMSafeSpaces } from '../../../typescript/src/index.js';
import { Runner, Config, configFromEnv, nodeFetch, rawDo, hasField } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: cfg.apiKey, timeout: 20000, fetch: nodeFetch as any });
  let wsId: string | null = null;
  try {
    const [ok, ws] = await r.assertNoError(
      () => c.workspaces.create({ name: 'canary-ts-wsstatus', runtime: 'base', storageSize: '1Gi' }),
      'create: no error');
    if (!ok || !ws) return;
    wsId = ws.id;

    const [ok2, st] = await r.assertNoError(() => c.workspaces.getStatus(wsId!), 'get-status: no error');
    if (ok2 && st) {
      // Phase may be empty pre-reconcile (canary CI runs a controller-less
      // cluster) — same relaxation as the Go/Python twins.
      r.assert('phase' in st && typeof st.phase === 'string', 'status: phase present (may be empty pre-reconcile)');
      r.assert(typeof st.activeSessions === 'number' && st.activeSessions >= 0, 'status: activeSessions ≥ 0');
      r.assert('credentialState' in st, 'status: credentialState present');
      r.assert('agentHealth' in st, 'status: agentHealth present');
      r.assert(typeof (st as any).agentHealth?.status === 'string', 'status: agentHealth.status is string');
    }

    // Raw — no error field on success
    const [s, b] = await rawDo('GET', `${cfg.apiUrl}/api/v1/workspaces/${wsId}/status`, cfg.apiKey);
    r.assert(s === 200, `status-raw: 200 (got ${s})`);
    r.assert(!hasField(b, 'error'), 'status-raw: no error field');

  } finally {
    if (wsId) { try { await c.workspaces.delete(wsId); } catch {} }
  }

  await r.assertError(() => c.workspaces.getStatus('00000000-0000-0000-0000-000000000000'),
    'status-nonexistent: error');
}

async function main() {
  const r = new Runner('ws-status');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
