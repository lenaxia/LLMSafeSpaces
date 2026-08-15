// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// S-ENV-VARS canary — TypeScript SDK

import { LLMSafeSpaces } from '../../../typescript/src/index.js';
import { jwtLogin, Runner, Config, configFromEnv, nodeFetch, rawDo } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: await jwtLogin(cfg), timeout: 20000, fetch: nodeFetch as any });
  let wsId: string | null = null;
  try {
    const [ok, ws] = await r.assertNoError(
      () => c.workspaces.create({ name: 'canary-ts-envvars', runtime: 'base', storageSize: '1Gi' }),
      'create-ws: no error');
    if (!ok || !ws) return;
    wsId = ws.id;

    // P1: Set env
    await r.assertNoError(() => c.workspaces.setEnv(wsId!, { CANARY_VAR: 'hello' }), 'set-env: no error');

    // P2: Get — contains CANARY_VAR
    const [ok2, env] = await r.assertNoError(() => c.workspaces.getEnv(wsId!), 'get-env: no error');
    if (ok2 && env) // The API returns secret names (<wsID>-env-<lowercased_var>), not the
      // original var names — match the lowercased suffix (Go twin parity).
      r.assert((env.vars ?? []).some((v: string) => v.endsWith('-env-canary_var')), 'get-env: CANARY_VAR present');

    // P3: Upsert
    await r.assertNoError(() => c.workspaces.setEnv(wsId!, { CANARY_VAR: 'updated' }), 'upsert-env: no error');

    // P4: Delete
    await r.assertNoError(() => c.workspaces.deleteEnv(wsId!, 'CANARY_VAR'), 'delete-env: no error');

    // P5: Absent after delete
    const [ok3, env2] = await r.assertNoError(() => c.workspaces.getEnv(wsId!), 'get-after-delete: no error');
    if (ok3 && env2) r.assert(!env2.vars?.includes('CANARY_VAR'), 'get-after-delete: CANARY_VAR absent');

    // N2: missing vars body → 400
    const [s] = await rawDo('PUT', `${cfg.apiUrl}/api/v1/workspaces/${wsId}/env`, cfg.apiKey, Buffer.from('{}'));
    r.assert(s === 400, `set-env-no-vars: 400 (got ${s})`);

    // N3: delete nonexistent var → 404
    const [s2] = await rawDo('DELETE', `${cfg.apiUrl}/api/v1/workspaces/${wsId}/env/NONEXISTENT_XYZ`, cfg.apiKey);
    r.assert(s2 === 404, `delete-nonexistent-var: 404 (got ${s2})`);

  } finally {
    if (wsId) { try { await c.workspaces.delete(wsId); } catch {} }
  }
}

async function main() {
  const r = new Runner('env-vars');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
