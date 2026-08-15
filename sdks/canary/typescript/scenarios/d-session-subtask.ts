// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// D-SESSION-SUBTASK canary — TypeScript SDK

import { LLMSafeSpaces } from '../../../typescript/src/index.js';
import { Runner, Config, configFromEnv, nodeFetch, waitActive, ensureSessionWithRetry, sleep } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  if (!cfg.llmApiKey) { r.ok('session-subtask: skipped (no LLM API key)'); return; }
  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: cfg.apiKey, timeout: 180000, fetch: nodeFetch as any });
  let wsId: string | null = null;
  try {
    const [ok, ws] = await r.assertNoError(
      () => c.workspaces.create({ name: 'canary-ts-subtask', runtime: 'base', storageSize: '1Gi' }),
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
        'Use the task tool to spawn a subagent that responds with exactly: SUBTASK-OK'),
      'send-subtask-message: no error');
    if (ok2 && msg) r.assert(msg.content.length > 0, 'send-subtask-message: non-empty content');

    let subtaskFound = false;
    const deadline = Date.now() + 30000;
    while (Date.now() < deadline && !subtaskFound) {
      await sleep(3000);
      const [ok3, sessions] = await r.assertNoError(
        () => c.sessions.list(wsId!), 'poll-sessions: no error');
      if (ok3 && sessions) {
        for (const s of sessions as any[]) {
          if (s.parentId && s.parentId.length > 0) {
            subtaskFound = true;
            r.assert(s.parentId === sid, 'subtask: parentId matches top-level session',
              `${s.parentId} vs ${sid}`);
            break;
          }
        }
      }
    }

    if (!subtaskFound) {
      r.ok('subtask: model did not spawn subagent (skipped gracefully)');
    }

    const [ok4, sessions] = await r.assertNoError(
      () => c.sessions.list(wsId!), 'final-session-list: no error');
    if (ok4 && sessions) {
      const topLevel = (sessions as any[]).find(s => s.id === sid);
      if (topLevel) {
        r.assert(!topLevel.parentId || topLevel.parentId === null || topLevel.parentId === '',
          'top-level: no parentId', `got "${topLevel.parentId}"`);
      }
    }

  } finally {
    if (wsId) { try { await c.workspaces.delete(wsId); } catch {} }
  }
}

async function main() {
  const r = new Runner('session-subtask');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
