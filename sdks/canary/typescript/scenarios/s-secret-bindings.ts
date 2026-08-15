// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// S-SECRET-BINDINGS canary — TypeScript SDK

import { LLMSafeSpaces } from '../../../typescript/src/index.js';
import { jwtLogin, Runner, Config, configFromEnv, nodeFetch } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: await jwtLogin(cfg), timeout: 20000, fetch: nodeFetch as any });
  let wsId: string | null = null;
  let sid: string | null = null;
  try {
    const [ok, ws] = await r.assertNoError(
      () => c.workspaces.create({ name: 'canary-ts-bindings', runtime: 'base', storageSize: '1Gi' }),
      'create-ws: no error');
    if (!ok || !ws) return;
    wsId = ws.id;

    const [ok2, s] = await r.assertNoError(
      () => c.secrets.create({ name: 'canary-ts-bind-s', type: 'env-secret', value: 'v', metadata: { var_name: 'CANARY_TS_VAR' } }),
      'create-secret: no error');
    if (!ok2 || !s) return;
    sid = s.id;

    // P1: Bind
    await r.assertNoError(() => c.workspaces.setBindings(wsId!, [sid!]), 'set-bindings: no error');

    // P2: Get — contains secret
    const [ok3, b] = await r.assertNoError(() => c.workspaces.getBindings(wsId!), 'get-bindings: no error');
    if (ok3 && b) r.assert(b.bindings.some(x => x.secretId === sid), 'get-bindings: secret present');

    // P3: Idempotent re-bind
    await r.assertNoError(() => c.workspaces.setBindings(wsId!, [sid!]), 'rebind-same: idempotent');
    const [ok4, b2] = await r.assertNoError(() => c.workspaces.getBindings(wsId!), 'get-bindings-after-rebind: no error');
    if (ok4 && b2) {
      const count = b2.bindings.filter(x => x.secretId === sid).length;
      r.assert(count === 1, 'rebind-same: exactly 1 entry', String(count));
    }

    // P4+P5: Clear
    await r.assertNoError(() => c.workspaces.setBindings(wsId!, []), 'clear-bindings: no error');
    const [ok5, empty] = await r.assertNoError(() => c.workspaces.getBindings(wsId!), 'get-empty: no error');
    if (ok5 && empty) r.assert(empty.bindings.length === 0, 'clear-bindings: empty', String(empty.bindings.length));

    // P6: secret bindings
    await r.assertNoError(() => c.secrets.getBindingsForSecret(sid!), 'get-secret-bindings: no error');

  } finally {
    if (sid) { try { await c.secrets.delete(sid); } catch {} }
    if (wsId) { try { await c.workspaces.delete(wsId); } catch {} }
  }

  await r.assertError(() => c.workspaces.setBindings('00000000-0000-0000-0000-000000000000', []),
    'bind-nonexistent-ws: error');
}

async function main() {
  const r = new Runner('secret-bindings');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
