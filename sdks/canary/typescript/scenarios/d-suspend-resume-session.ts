// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// D-SUSPEND-RESUME-SESSION canary — TypeScript SDK

import { LLMSafeSpaces } from '../../../typescript/src/index.js';
import { Runner, Config, configFromEnv, nodeFetch, waitActive, waitPhase, ensureSessionWithRetry } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  if (!cfg.llmApiKey) { r.ok('suspend-resume-session: skipped (no LLM API key)'); return; }
  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: cfg.apiKey, timeout: 120000, fetch: nodeFetch as any });
  let wsId: string | null = null;
  try {
    const [ok, ws] = await r.assertNoError(
      () => c.workspaces.create({ name: 'canary-ts-sr-sess', runtime: 'base', storageSize: '1Gi' }),
      'create-ws: no error');
    if (!ok || !ws) return;
    wsId = ws.id;

    const phase = await waitActive(c, wsId);
    r.assert(phase === 'Active', 'reach-active', `got "${phase}"`);
    if (phase !== 'Active') return;

    const sess = await ensureSessionWithRetry(c, wsId, 5);
    r.ok('ensure-session: no error');
    const sid = sess.sessionId;

    const [ok2, msg1] = await r.assertNoError(
      () => c.sessions.sendMessage(wsId!, sid, 'Reply with exactly: BEFORE'), 'send-before: no error');
    if (ok2 && msg1) r.assert(msg1.content.length > 0, 'send-before: non-empty');

    const [ok3, hist1] = await r.assertNoError(
      () => c.sessions.getHistory(wsId!, sid), 'history-before-suspend: no error');
    if (ok3 && hist1) r.assert(hist1.length >= 1, 'history-before-suspend: ≥1 entry', String(hist1.length));

    await r.assertNoError(() => c.workspaces.suspend(wsId!), 'suspend: no error');
    const sp = await waitPhase(c, wsId, 'Suspended', 60000);
    r.assert(sp === 'Suspended', 'suspend: phase=Suspended', `got "${sp}"`);

    await r.assertNoError(() => c.workspaces.activate(wsId!), 'activate: no error');
    const rp = await waitActive(c, wsId);
    r.assert(rp === 'Active', 'activate: phase=Active', `got "${rp}"`);

    const sess2 = await ensureSessionWithRetry(c, wsId, 8);
    r.ok('ensure-session-post-resume: no error');

    const [ok4, msg2] = await r.assertNoError(
      () => c.sessions.sendMessage(wsId!, sess2.sessionId, 'Reply with exactly: AFTER'), 'send-after: no error');
    if (ok4 && msg2) r.assert(msg2.content.length > 0, 'send-after: non-empty');

    // P8: The BEFORE message must still be retrievable on the ORIGINAL session ID.
    // This is the actual persistence test. After resume the pod is the same
    // (PVC survives), so opencode's session store should still have the BEFORE entry.
    const [ok5, histOriginal] = await r.assertNoError(
      () => c.sessions.getHistory(wsId!, sid),
      'history-original-session-after-resume: no error');
    if (ok5 && histOriginal !== null) {
      r.assert((histOriginal as any[]).length >= 1,
        'history-original-session-after-resume: BEFORE message persisted',
        `got ${(histOriginal as any[]).length} entries — history was wiped by suspend/resume`);
    }

    // Also verify the new session has its AFTER message
    const [ok6, hist2] = await r.assertNoError(
      () => c.sessions.getHistory(wsId!, sess2.sessionId), 'history-new-session-after-resume: no error');
    if (ok6 && hist2 !== null) r.assert((hist2 as any[]).length >= 1, 'history-new-session: ≥1 entry');

  } finally {
    if (wsId) { try { await c.workspaces.delete(wsId); } catch {} }
  }
}

async function main() {
  const r = new Runner('suspend-resume-session');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
