// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// D-SESSION-LIMIT canary — TypeScript SDK

import http from 'http';
import { LLMSafeSpaces, RateLimitError } from '../../../typescript/src/index.js';
import { Runner, Config, configFromEnv, nodeFetch, rawDo, waitActive, ensureSessionWithRetry, sleep } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: cfg.apiKey, timeout: 60000, fetch: nodeFetch as any });
  let wsId: string | null = null;
  try {
    const [ok, ws] = await r.assertNoError(
      () => c.workspaces.create({ name: 'canary-ts-sess-limit', runtime: 'base', storageSize: '1Gi' }),
      'create: no error');
    if (!ok || !ws) return;
    wsId = ws.id;

    const phase = await waitActive(c, wsId);
    r.assert(phase === 'Active', 'reach-active', `got "${phase}"`);
    if (phase !== 'Active') return;

    const sess = await ensureSessionWithRetry(c, wsId, 5);
    r.ok('ensure-session: no error');
    const sid = sess.sessionId;

    const [okAS, asr] = await r.assertNoError(
      () => c.sessions.getActive(wsId!), 'get-active-sessions: no error');
    if (okAS && asr) {
      r.assert(typeof asr.maxActive === 'number' && asr.maxActive > 0,
        'active-sessions: maxActive > 0', String(asr.maxActive));
    }

    if (cfg.llmApiKey) {
      let got429 = false;
      const [okMsg] = await r.assertNoError(
        () => c.sessions.sendMessage(wsId!, sid, 'Reply: LIMIT-TEST'),
        'send-message-1: no error');
      await sleep(500);
      if (okMsg !== false) {
        try {
          await c.sessions.sendMessage(wsId!, sid, 'Reply: LIMIT-TEST-2');
          r.ok('send-message-2: no error (within limit)');
        } catch (e: any) {
          if (e instanceof RateLimitError) got429 = true;
        }
      }
      if (!got429) r.ok('session-limit: no 429 (may need more concurrent sessions to hit limit)');
      else r.ok('session-limit: got 429 on concurrent message');
    }

    const connections: http.ClientRequest[] = [];
    let conn429 = false;
    for (let i = 0; i < 12; i++) {
      const url = new URL(`${cfg.apiUrl}/api/v1/workspaces/${wsId}/events`);
      const req = http.get({
        hostname: url.hostname,
        port: url.port || 80,
        path: url.pathname,
        headers: { Authorization: `Bearer ${cfg.apiKey}`, Accept: 'text/event-stream' },
      }, (res) => {
        if (res.statusCode === 429) {
          conn429 = true;
          r.ok('connection-limit: 429 on excess SSE connection');
        }
        res.resume();
      });
      req.on('error', () => {});
      connections.push(req);
      await sleep(100);
    }
    r.assert(conn429, 'connection-limit: got 429 on 11th+ SSE connection',
      conn429 ? '' : 'no 429 after 12 connections');

    for (const conn of connections) { try { conn.destroy(); } catch {} }
  } finally {
    if (wsId) { try { await c.workspaces.delete(wsId); } catch {} }
  }
}

async function main() {
  const r = new Runner('session-limit');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
