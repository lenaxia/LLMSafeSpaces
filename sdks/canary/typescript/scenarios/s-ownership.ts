// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// S-OWNERSHIP canary — TypeScript SDK

import { LLMSafeSpaces } from '../../../typescript/src/index.js';
import { jwtLogin, Runner, Config, configFromEnv, nodeFetch, rawDo } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  if (!cfg.apiKeyUser2) { r.ok('ownership: skipped (no API_KEY_USER2)'); return; }

  const c1 = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: await jwtLogin(cfg), timeout: 20000, fetch: nodeFetch as any });
  const c2 = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: cfg.apiKeyUser2, timeout: 20000, fetch: nodeFetch as any });
  let ws1Id: string | null = null, ws2Id: string | null = null, s1Id: string | null = null;
  try {
    const [ok, ws1] = await r.assertNoError(
      () => c1.workspaces.create({ name: 'canary-ts-own-u1', runtime: 'base', storageSize: '1Gi' }),
      'user1-create-ws');
    if (!ok || !ws1) return;
    ws1Id = ws1.id;

    const [ok2, s1] = await r.assertNoError(
      () => c1.secrets.create({ name: 'canary-ts-own-s1', type: 'env-secret', value: 'v', metadata: { var_name: 'CANARY_TS_VAR' } }),
      'user1-create-secret');
    if (ok2 && s1) s1Id = s1.id;

    const [ok3, ws2] = await r.assertNoError(
      () => c2.workspaces.create({ name: 'canary-ts-own-u2', runtime: 'base', storageSize: '1Gi' }),
      'user2-create-ws');
    if (!ok3 || !ws2) return;
    ws2Id = ws2.id;

    await r.assertNoError(() => c1.workspaces.get(ws1Id!), 'user1-get-own');
    await r.assertNoError(() => c2.workspaces.get(ws2Id!), 'user2-get-own');

    const [ok4, l1] = await r.assertNoError(() => c1.workspaces.list(), 'user1-list');
    if (ok4 && l1) {
      r.assert(l1.items.some(i => i.id === ws1Id), 'user1-list: W1 present');
      r.assert(!l1.items.some(i => i.id === ws2Id), 'user1-list: W2 absent');
    }
    const [ok5, l2] = await r.assertNoError(() => c2.workspaces.list(), 'user2-list');
    if (ok5 && l2) {
      r.assert(!l2.items.some(i => i.id === ws1Id), 'user2-list: W1 absent');
      r.assert(l2.items.some(i => i.id === ws2Id), 'user2-list: W2 present');
    }

    // Validated: workspace routes return 403 (ForbiddenError), not 404.
    // The bindings route goes through the secrets handler which maps
    // cross-user access to 404 (ErrWorkspaceNotOwned).
    await r.assertError(() => c2.workspaces.get(ws1Id!), 'user2-get-user1-ws: 403 Forbidden');
    await r.assertError(() => c2.workspaces.delete(ws1Id!), 'user2-delete-user1-ws: error');
    await r.assertError(() => c2.workspaces.getStatus(ws1Id!), 'user2-status-user1-ws: 403');
    if (s1Id) await r.assertError(() => c2.secrets.get(s1Id!), 'user2-get-user1-secret: error');
    await r.assertError(() => c2.sessions.ensure(ws1Id!), 'user2-ensure-session-user1: error');

    const [s] = await rawDo('GET', `${cfg.apiUrl}/api/v1/workspaces/${ws1Id}/bindings`, cfg.apiKeyUser2);
    // N6: 403 (ownership middleware denies before the secrets handler) or 404 — Go twin contract.
    r.assert(s === 403 || s === 404, `user2-bindings-user1: 403/404 (access denied, got ${s})`);

  } finally {
    if (s1Id) { try { await c1.secrets.delete(s1Id); } catch {} }
    if (ws1Id) { try { await c1.workspaces.delete(ws1Id); } catch {} }
    if (ws2Id) { try { await c2.workspaces.delete(ws2Id); } catch {} }
  }
}

async function main() {
  const r = new Runner('ownership');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
