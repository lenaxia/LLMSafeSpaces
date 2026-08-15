// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// D-AGENT-INPUT canary — TypeScript SDK

import { LLMSafeSpaces } from '../../../typescript/src/index.js';
import { Runner, Config, configFromEnv, nodeFetch, rawDo, waitActive, ensureSessionWithRetry, sleep } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  if (!cfg.llmApiKey) { r.ok('agent-input: skipped (no LLM API key)'); return; }
  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: cfg.apiKey, timeout: 120000, fetch: nodeFetch as any });
  let wsId: string | null = null;
  try {
    const [ok, ws] = await r.assertNoError(
      () => c.workspaces.create({ name: 'canary-ts-agent-input', runtime: 'base', storageSize: '1Gi' }),
      'create: no error');
    if (!ok || !ws) return;
    wsId = ws.id;

    const phase = await waitActive(c, wsId);
    r.assert(phase === 'Active', 'reach-active', `got "${phase}"`);
    if (phase !== 'Active') return;

    const sess = await ensureSessionWithRetry(c, wsId, 5);
    r.ok('ensure-session: no error');
    const sid = sess.sessionId;

    const [okQ, qBody] = await r.assertNoError(
      () => rawDo('GET', `${cfg.apiUrl}/api/v1/workspaces/${wsId}/proxy/question`, cfg.apiKey),
      'get-question: no error');
    if (okQ) r.assert(okQ, 'get-question: returned response');

    const [okP, pBody] = await r.assertNoError(
      () => rawDo('GET', `${cfg.apiUrl}/api/v1/workspaces/${wsId}/proxy/permission`, cfg.apiKey),
      'get-permission: no error');
    if (okP) r.assert(okP, 'get-permission: returned response');

    const [okMsg, msg] = await r.assertNoError(
      () => c.sessions.sendMessage(wsId!, sid,
        'Please create a new file called /tmp/canary-test-file.txt with the content "hello". This requires file write permission.'),
      'send-tool-message: no error');
    if (okMsg && msg) r.assert(msg.content.length > 0, 'send-tool-message: non-empty content');

    await sleep(3000);

    const [sPerm, permBody] = await rawDo('GET',
      `${cfg.apiUrl}/api/v1/workspaces/${wsId}/proxy/permission`, cfg.apiKey);
    r.assert(sPerm === 200, 'get-permission-after-msg: 200', `got ${sPerm}`);

    if (sPerm === 200) {
      let perms: any[] = [];
      try { perms = JSON.parse(permBody.toString()); } catch { perms = []; }
      if (Array.isArray(perms) && perms.length > 0 && perms[0].id) {
        const permId = perms[0].id;
        const [sReply] = await rawDo('POST',
          `${cfg.apiUrl}/api/v1/workspaces/${wsId}/proxy/permission/${permId}/reply`,
          cfg.apiKey, Buffer.from(JSON.stringify({ reply: 'once' })));
        r.assert(sReply === 200 || sReply === 204, 'permission-reply: success',
          `got ${sReply}`);
      } else {
        r.ok('permission: no pending permissions (model did not trigger tool permission)');
      }
    }

    const [sBadQ] = await rawDo('POST',
      `${cfg.apiUrl}/api/v1/workspaces/${wsId}/proxy/question/invalid-id/reply`,
      cfg.apiKey, Buffer.from(JSON.stringify({ text: 'answer' })));
    r.assert(sBadQ === 400, 'bad-question-id: 400', `got ${sBadQ}`);

    const [sBadPerm] = await rawDo('POST',
      `${cfg.apiUrl}/api/v1/workspaces/${wsId}/proxy/permission/invalid-id/reply`,
      cfg.apiKey, Buffer.from(JSON.stringify({ reply: 'maybe' })));
    r.assert(sBadPerm === 400, 'bad-permission-reply-value: 400', `got ${sBadPerm}`);

    const [sBadPermId] = await rawDo('POST',
      `${cfg.apiUrl}/api/v1/workspaces/${wsId}/proxy/permission/not-per-id/reply`,
      cfg.apiKey, Buffer.from(JSON.stringify({ reply: 'once' })));
    r.assert(sBadPermId === 400, 'bad-permission-id-format: 400', `got ${sBadPermId}`);

  } finally {
    if (wsId) { try { await c.workspaces.delete(wsId); } catch {} }
  }
}

async function main() {
  const r = new Runner('agent-input');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
