// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// D-CRED-BIND canary — TypeScript SDK

import { LLMSafeSpaces } from '../../../typescript/src/index.js';
import { jwtLogin, Runner, Config, configFromEnv, nodeFetch, waitActive, waitPhase } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: await jwtLogin(cfg), timeout: 60000, fetch: nodeFetch as any });
  let wsId: string | null = null, credId: string | null = null;
  try {
    const [ok, ws] = await r.assertNoError(
      () => c.workspaces.create({ name: 'canary-ts-cred-bind', runtime: 'base', storageSize: '1Gi' }),
      'create-ws: no error');
    if (!ok || !ws) return;
    wsId = ws.id;

    const phase = await waitActive(c, wsId);
    r.assert(phase === 'Active', 'reach-active', `got "${phase}"`);
    if (phase !== 'Active') return;

    const credValue = JSON.stringify({ kind: cfg.llmProvider, slug: "canary-ts-bind", apiKey: 'sk-canary-placeholder' });
    const [ok2, cred] = await r.assertNoError(
      () => c.secrets.create({ name: 'canary-ts-cred-bind-s', type: 'llm-provider', value: credValue }),
      'create-cred: no error');
    if (!ok2 || !cred) return;
    credId = cred.id;

    await r.assertNoError(() => c.workspaces.setBindings(wsId!, [credId!]), 'bind-cred: no error');

    const [ok3, b] = await r.assertNoError(() => c.workspaces.getBindings(wsId!), 'get-bindings: no error');
    if (ok3 && b) r.assert(b.bindings.some(x => x.id === credId), 'get-bindings: cred present');

    const [ok4, result] = await r.assertNoError(() => c.workspaces.reloadSecrets(wsId!), 'reload-secrets: no error');
    if (ok4 && result) r.assert(result.reloaded >= 1, 'reload-secrets: reloaded ≥ 1', String(result.reloaded));

    const [ok5, st] = await r.assertNoError(() => c.workspaces.getStatus(wsId!), 'status-after-reload: no error');
    if (ok5 && st) r.assert((st as any).credentialState?.available === true, 'status: credentialState.available=true');

    await r.assertNoError(() => c.workspaces.setBindings(wsId!, []), 'unbind: no error');
    const [ok6, er] = await r.assertNoError(() => c.workspaces.reloadSecrets(wsId!), 'reload-after-unbind: no error');
    if (ok6 && er) r.assert(er.reloaded === 0, 'reload-after-unbind: reloaded=0', String(er.reloaded));

    // Reload on suspended → error
    await r.assertNoError(() => c.workspaces.suspend(wsId!), 'suspend: no error');
    await waitPhase(c, wsId, 'Suspended', 60000);
    await r.assertError(() => c.workspaces.reloadSecrets(wsId!), 'reload-suspended: error (no running pod)');

  } finally {
    if (credId) { try { await c.secrets.delete(credId); } catch {} }
    if (wsId) { try { await c.workspaces.delete(wsId); } catch {} }
  }
}

async function main() {
  const r = new Runner('cred-bind');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
