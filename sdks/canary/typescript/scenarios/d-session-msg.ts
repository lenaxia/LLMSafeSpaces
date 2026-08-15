// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// D-SESSION-MSG canary — TypeScript SDK

import { LLMSafeSpaces } from '../../../typescript/src/index.js';
import { Runner, Config, configFromEnv, nodeFetch, waitActive, ensureSessionWithRetry } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  if (!cfg.llmApiKey) { r.ok('session-msg: skipped (no LLM API key)'); return; }
  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: cfg.apiKey, timeout: 120000, fetch: nodeFetch as any });
  let wsId: string | null = null;
  try {
    const [ok, ws] = await r.assertNoError(
      () => c.workspaces.create({ name: 'canary-ts-sess-msg', runtime: 'base', storageSize: '1Gi' }),
      'create: no error');
    if (!ok || !ws) return;
    wsId = ws.id;

    const phase = await waitActive(c, wsId);
    r.assert(phase === 'Active', 'reach-active', `got "${phase}"`);
    if (phase !== 'Active') return;

    const sess = await ensureSessionWithRetry(c, wsId, 5);
    r.ok('ensure-session: no error');

    const [ok2, msg] = await r.assertNoError(
      () => c.sessions.sendMessage(wsId!, sess.sessionId, 'Reply with exactly: PONG'),
      'send-message: no error');
    if (ok2 && msg) {
      r.assert(msg.content.length > 0, 'send-message: non-empty content');
    }

    // lastActivityAt updated
    const [ok3, st] = await r.assertNoError(() => c.workspaces.getStatus(wsId!), 'get-status-after-msg: no error');
    if (ok3 && st) r.assert((st as any).lastActivityAt != null, 'status: lastActivityAt non-nil');

    // N1: nonexistent session
    await r.assertError(() => c.sessions.sendMessage(wsId!, 'ses_nonexistent000000', 'ping'),
      'send-nonexistent-session: error');

  } finally {
    if (wsId) { try { await c.workspaces.delete(wsId); } catch {} }
  }
}

async function main() {
  const r = new Runner('session-msg');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
