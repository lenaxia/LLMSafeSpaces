// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// D-SESSION-GET canary — TypeScript SDK

import { LLMSafeSpaces } from '../../../typescript/src/index.js';
import { Runner, Config, configFromEnv, nodeFetch, rawDo, waitActive, ensureSessionWithRetry } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: cfg.apiKey, timeout: 60000, fetch: nodeFetch as any });
  let wsId: string | null = null;
  try {
    const [ok, ws] = await r.assertNoError(
      () => c.workspaces.create({ name: 'canary-ts-sess-get', runtime: 'base', storageSize: '1Gi' }),
      'create: no error');
    if (!ok || !ws) return;
    wsId = ws.id;

    const phase = await waitActive(c, wsId);
    r.assert(phase === 'Active', 'reach-active', `got "${phase}"`);
    if (phase !== 'Active') return;

    const sess = await ensureSessionWithRetry(c, wsId, 5);
    r.ok('ensure-session: no error');
    const sid = sess.sessionId;

    const [ok2, sessData] = await r.assertNoError(
      () => c.sessions.get(wsId!, sid), 'get-session: no error');
    if (ok2 && sessData) {
      r.assert((sessData as any).id === sid, 'get-session: id matches', `${(sessData as any).id} vs ${sid}`);
      r.assert(typeof (sessData as any).title === 'string', 'get-session: has title field');
    }

    await r.assertNoError(
      () => c.sessions.rename(wsId!, sid, 'canary-renamed'), 'rename-session: no error');

    const [ok3, sessData2] = await r.assertNoError(
      () => c.sessions.get(wsId!, sid), 'get-session-after-rename: no error');
    if (ok3 && sessData2) {
      r.assert((sessData2 as any).title === 'canary-renamed', 'get-session: title updated',
        `got "${(sessData2 as any).title}"`);
    }

    await r.assertError(
      () => c.sessions.get(wsId!, 'ses_nonexistent000000'),
      'get-nonexistent-session: error');

    const [s] = await rawDo('GET',
      `${cfg.apiUrl}/api/v1/workspaces/${wsId}/sessions/..%2F..%2Fetc%2Fpasswd`,
      cfg.apiKey);
    r.assert(s === 400, 'path-traversal: 400', `got ${s}`);

  } finally {
    if (wsId) { try { await c.workspaces.delete(wsId); } catch {} }
  }
}

async function main() {
  const r = new Runner('session-get');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
