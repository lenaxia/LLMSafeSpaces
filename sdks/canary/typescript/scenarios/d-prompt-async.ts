// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// D-PROMPT-ASYNC canary — TypeScript SDK

import http from 'http';
import { LLMSafeSpaces } from '../../../typescript/src/index.js';
import { Runner, Config, configFromEnv, nodeFetch, rawDo, waitActive, ensureSessionWithRetry, sleep } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  if (!cfg.llmApiKey) { r.ok('prompt-async: skipped (no LLM API key)'); return; }
  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: cfg.apiKey, timeout: 120000, fetch: nodeFetch as any });
  let wsId: string | null = null;
  try {
    const [ok, ws] = await r.assertNoError(
      () => c.workspaces.create({ name: 'canary-ts-prompt-async', runtime: 'base', storageSize: '1Gi' }),
      'create: no error');
    if (!ok || !ws) return;
    wsId = ws.id;

    const phase = await waitActive(c, wsId);
    r.assert(phase === 'Active', 'reach-active', `got "${phase}"`);
    if (phase !== 'Active') return;

    const sess = await ensureSessionWithRetry(c, wsId, 5);
    r.ok('ensure-session: no error');
    const sid = sess.sessionId;

    // P1: prompt_async → 202 immediately
    await r.assertNoError(
      () => c.sessions.sendPromptAsync(wsId!, sid, 'Reply with the word: ASYNC-OK'),
      'prompt-async: 202 immediate');

    // P2+P3: SSE session.idle
    const idle = await waitForSessionIdle(cfg, wsId!, sid, 90000);
    r.assert(idle, 'sse: received session.idle', 'no session.idle within 90s');

    // P4: history
    const [ok2, hist] = await r.assertNoError(
      () => c.sessions.getHistory(wsId!, sid), 'history-after-async: no error');
    if (ok2 && hist) r.assert(hist.length >= 1, 'history-after-async: ≥1 entry', String(hist.length));

    // N1: malformed session ID
    const [s] = await rawDo('POST',
      `${cfg.apiUrl}/api/v1/workspaces/${wsId}/sessions/..%2Fetc/prompt`,
      cfg.apiKey, Buffer.from('{"parts":[{"type":"text","text":"ping"}]}'));
    r.assert(s === 400, `malformed-session-id: 400 (got ${s})`);

  } finally {
    if (wsId) { try { await c.workspaces.delete(wsId); } catch {} }
  }
}

function waitForSessionIdle(cfg: Config, wsId: string, sessionId: string, timeoutMs: number): Promise<boolean> {
  return new Promise(resolve => {
    const url = new URL(`${cfg.apiUrl}/api/v1/workspaces/${wsId}/events`);
    const deadline = Date.now() + timeoutMs;
    let resolved = false;
    const timer = setTimeout(() => { if (!resolved) { resolved = true; resolve(false); } }, timeoutMs);

    const req = http.get({
      hostname: url.hostname, port: url.port || 80, path: url.pathname,
      headers: { Authorization: `Bearer ${cfg.apiKey}`, Accept: 'text/event-stream' },
    }, (res) => {
      let buf = '';
      res.on('data', (chunk: Buffer) => {
        if (Date.now() > deadline) { req.destroy(); return; }
        buf += chunk.toString();
        const lines = buf.split('\n');
        buf = lines.pop() ?? '';
        for (const line of lines) {
          if (!line.startsWith('data: ')) continue;
          try {
            const evt = JSON.parse(line.slice(6));
            if (evt.type === 'session.status' && evt.status === 'idle' &&
                (evt.session_id === sessionId || !evt.session_id)) {
              clearTimeout(timer);
              if (!resolved) { resolved = true; resolve(true); }
              req.destroy();
            }
          } catch {}
        }
      });
      res.on('end', () => { if (!resolved) { resolved = true; resolve(false); } });
      res.on('error', () => { if (!resolved) { resolved = true; resolve(false); } });
    });
    req.on('error', () => { if (!resolved) { resolved = true; resolve(false); } });
  });
}

async function main() {
  const r = new Runner('prompt-async');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
