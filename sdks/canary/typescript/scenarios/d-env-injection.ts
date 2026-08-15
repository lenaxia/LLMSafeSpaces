// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// D-ENV-INJECTION canary — TypeScript SDK

import { LLMSafeSpaces } from '../../../typescript/src/index.js';
import { Runner, Config, configFromEnv, nodeFetch, waitActive, ensureSessionWithRetry, sleep } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  if (!cfg.llmApiKey) { r.ok('env-injection: skipped (no LLM API key)'); return; }
  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: cfg.apiKey, timeout: 120000, fetch: nodeFetch as any });
  let wsId: string | null = null;
  try {
    const [ok, ws] = await r.assertNoError(
      () => c.workspaces.create({ name: 'canary-ts-env-inject', runtime: 'base', storageSize: '1Gi' }),
      'create: no error');
    if (!ok || !ws) return;
    wsId = ws.id;

    const phase = await waitActive(c, wsId);
    r.assert(phase === 'Active', 'reach-active', `got "${phase}"`);
    if (phase !== 'Active') return;

    await r.assertNoError(
      () => c.workspaces.setEnv(wsId!, { CANARY_INJECT: 'canary-xyz' }),
      'set-env: no error');

    const [okE, envData] = await r.assertNoError(
      () => c.workspaces.getEnv(wsId!), 'get-env: no error');
    if (okE && envData) {
      r.assert((envData as any).vars?.includes?.('CANARY_INJECT') ||
        Array.isArray(envData.vars) && envData.vars.some((v: any) =>
          typeof v === 'string' && v.includes('CANARY_INJECT') ||
          typeof v === 'object' && v.name === 'CANARY_INJECT'),
        'get-env: CANARY_INJECT present');
    }

    const sess = await ensureSessionWithRetry(c, wsId, 5);
    r.ok('ensure-session: no error');

    const [ok2, msg] = await r.assertNoError(
      () => c.sessions.sendMessage(wsId!, sess.sessionId,
        'Run: python3 -c \'import os; print(os.environ.get("CANARY_INJECT", "NOTFOUND"))\''),
      'send-check-env: no error');
    if (ok2 && msg) {
      r.assert(msg.content.includes('canary-xyz'), 'env-present: agent sees canary-xyz',
        msg.content.substring(0, 200));
    }

    await r.assertNoError(
      () => c.workspaces.deleteEnv(wsId!, 'CANARY_INJECT'), 'delete-env: no error');
    await r.assertNoError(
      () => c.workspaces.reloadSecrets(wsId!), 'reload-secrets: no error');

    await sleep(3000);

    const [ok3, msg2] = await r.assertNoError(
      () => c.sessions.sendMessage(wsId!, sess.sessionId,
        'Run: python3 -c \'import os; print(os.environ.get("CANARY_INJECT", "NOTFOUND"))\''),
      'send-check-env-gone: no error');
    if (ok3 && msg2) {
      r.assert(msg2.content.includes('NOTFOUND'), 'env-gone: agent sees NOTFOUND',
        msg2.content.substring(0, 200));
    }

  } finally {
    if (wsId) { try { await c.workspaces.delete(wsId); } catch {} }
  }
}

async function main() {
  const r = new Runner('env-injection');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
