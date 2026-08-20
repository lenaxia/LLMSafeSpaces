// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// D-SESSION-TITLE canary — TypeScript SDK

import { LLMSafeSpaces } from '../../../typescript/src/index.js';
import { Runner, Config, configFromEnv, nodeFetch, waitActive, ensureSessionWithRetry, sleep } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  if (!cfg.llmApiKey) { r.ok('session-title: skipped (no LLM API key)'); return; }
  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: cfg.apiKey, timeout: 120000, fetch: nodeFetch as any });
  let wsId: string | null = null;
  try {
    const [ok, ws] = await r.assertNoError(
      () => c.workspaces.create({ name: 'canary-ts-sess-title', runtime: 'base', storageSize: '1Gi' }),
      'create: no error');
    if (!ok || !ws) return;
    wsId = ws.id;

    const phase = await waitActive(c, wsId);
    r.assert(phase === 'Active', 'reach-active', `got "${phase}"`);
    if (phase !== 'Active') return;

    const sess = await ensureSessionWithRetry(c, wsId, 5);
    r.ok('ensure-session: no error');
    const sid = sess.sessionId;

    const [ok2, msg] = await r.assertNoError(
      () => c.sessions.sendMessage(wsId!, sid,
        'Explain in one sentence why the sky is blue during the day but orange at sunset.'),
      'send-message: no error');
    if (ok2 && msg) r.assert((msg.text ?? (msg.parts ?? []).filter(p => p.type === 'text').map(p => p.text ?? '').join('')).length > 0, 'send-message: non-empty content');

    let titleFound = false;
    const deadline = Date.now() + 20000;
    while (Date.now() < deadline && !titleFound) {
      await sleep(2000);
      const [ok3, sessions] = await r.assertNoError(
        () => c.sessions.list(wsId!), 'poll-sessions: no error');
      if (ok3 && sessions) {
        const target = sessions.find((s: any) => s.id === sid);
        if (target && target.title && target.title.length > 0) {
          titleFound = true;
          r.ok('session-title: auto-generated title present');
        }
      }
    }
    if (!titleFound) {
      r.fail('session-title: auto-generated title present', 'no title within 20s');
    }

    const [ok4, sessGet] = await r.assertNoError(
      () => c.sessions.get(wsId!, sid), 'get-session-title: no error');
    if (ok4 && sessGet) {
      r.assert(typeof (sessGet as any).title === 'string', 'get-session: has title field');
    }
  } finally {
    if (wsId) { try { await c.workspaces.delete(wsId); } catch {} }
  }
}

async function main() {
  const r = new Runner('session-title');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
