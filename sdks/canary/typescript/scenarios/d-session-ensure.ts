// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// D-SESSION-ENSURE canary — TypeScript SDK

import { LLMSafeSpaces } from '../../../typescript/src/index.js';
import { Runner, Config, configFromEnv, nodeFetch, waitActive, waitPhase, ensureSessionWithRetry, rawDo } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: cfg.apiKey, timeout: 60000, fetch: nodeFetch as any });
  let wsId: string | null = null;
  try {
    const [ok, ws] = await r.assertNoError(
      () => c.workspaces.create({ name: 'canary-ts-sess-ensure', runtime: 'base', storageSize: '1Gi' }),
      'create: no error');
    if (!ok || !ws) return;
    wsId = ws.id;

    const phase = await waitActive(c, wsId);
    r.assert(phase === 'Active', 'reach-active', `got "${phase}"`);
    if (phase !== 'Active') return;

    // Ensure on Active → resumed=false
    const sess = await ensureSessionWithRetry(c, wsId, 5);
    r.ok('ensure-active: no error');
    r.assert(sess?.sessionId !== '', 'ensure-active: sessionId present');
    r.assert(sess?.resumed === false, 'ensure-active: resumed=false');
    const sid = sess.sessionId;

    // Suspend then ensure → auto-resume
    await r.assertNoError(() => c.workspaces.suspend(wsId!), 'suspend: no error');
    await waitPhase(c, wsId, 'Suspended', 60000);
    const sess2 = await ensureSessionWithRetry(c, wsId, 10);
    r.ok('ensure-suspended: no error (auto-resume)');
    r.assert(sess2?.resumed === true, 'ensure-suspended: resumed=true');
    r.assert(sess2?.workspacePhase === 'Active', 'ensure-suspended: workspacePhase=Active', sess2?.workspacePhase);

    // List sessions
    const [ok2, lst] = await r.assertNoError(() => c.sessions.list(wsId!), 'list-sessions: no error');
    if (ok2 && lst) r.assert(Array.isArray(lst), 'list-sessions: array');

    // Active sessions
    const [ok3, active] = await r.assertNoError(() => c.sessions.getActive(wsId!), 'active-sessions: no error');
    if (ok3 && active) r.assert(active.maxActive > 0, 'active-sessions: maxActive > 0', String(active.maxActive));

    // Rename
    await r.assertNoError(() => c.sessions.rename(wsId!, sid, 'canary-ts-title'), 'rename-session: no error');

    // GET individual session
    const [ok4, sessObj] = await r.assertNoError(() => c.sessions.get(wsId!, sid), 'get-session: no error');
    if (ok4 && sessObj) r.assert('id' in sessObj, 'get-session: id present');

    // Abort
    await r.assertNoError(() => c.sessions.abort(wsId!, sid), 'abort: no error');

    // N1: nonexistent workspace
    await r.assertError(() => c.sessions.ensure('00000000-0000-0000-0000-000000000000'),
      'ensure-nonexistent-ws: error');

    // Path traversal
    const [s] = await rawDo('GET',
      `${cfg.apiUrl}/api/v1/workspaces/${wsId}/sessions/..%2Fetc/message`, cfg.apiKey);
    r.assert(s === 400 || s === 404, 'path-traversal: 400 or 404', String(s));

  } finally {
    if (wsId) { try { await c.workspaces.delete(wsId); } catch {} }
  }
}

async function main() {
  const r = new Runner('session-ensure');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
