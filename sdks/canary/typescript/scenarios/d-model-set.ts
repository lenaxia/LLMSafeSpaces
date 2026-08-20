// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// D-MODEL-SET canary — TypeScript SDK

import { LLMSafeSpaces } from '../../../typescript/src/index.js';
import { Runner, Config, configFromEnv, nodeFetch, waitActive, ensureSessionWithRetry } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  if (!cfg.llmApiKey || !cfg.llmModel) {
    r.ok('model-set: skipped (no LLM API key or model)');
    return;
  }
  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: cfg.apiKey, timeout: 120000, fetch: nodeFetch as any });
  let wsId: string | null = null;
  try {
    const [ok, ws] = await r.assertNoError(
      () => c.workspaces.create({ name: 'canary-ts-model-set', runtime: 'base', storageSize: '1Gi' }),
      'create: no error');
    if (!ok || !ws) return;
    wsId = ws.id;

    const phase = await waitActive(c, wsId);
    r.assert(phase === 'Active', 'reach-active', `got "${phase}"`);
    if (phase !== 'Active') return;

    await r.assertNoError(
      () => c.workspaces.setModel(wsId!, cfg.llmModel), 'set-model: no error');

    const [ok2, models] = await r.assertNoError(
      () => c.workspaces.getModels(wsId!), 'get-models-after-set: no error');
    if (ok2 && models) {
      r.assert(models.currentModel === cfg.llmModel, 'models: currentModel updated',
        `${models.currentModel} vs ${cfg.llmModel}`);
      const selected = (models.models as any[])?.find((m: any) => m.selected);
      r.assert(selected?.id === cfg.llmModel, 'models: selected matches',
        `${selected?.id} vs ${cfg.llmModel}`);
    }

    const sess = await ensureSessionWithRetry(c, wsId!, 5);
    r.ok('ensure-session: no error');
    const [ok3, msg] = await r.assertNoError(
      () => c.sessions.sendMessage(wsId!, sess.sessionId, 'Reply with exactly: MODEL-SET-OK'),
      'send-message: no error');
    if (ok3 && msg) {
      r.assert((msg.text ?? (msg.parts ?? []).filter(p => p.type === 'text').map(p => p.text ?? '').join('')).length > 0, 'send-message: non-empty content');
    }

    await r.assertError(
      () => c.workspaces.setModel(wsId!, ''), 'set-empty-model: error');

    await r.assertError(
      () => c.workspaces.setModel('00000000-0000-0000-0000-000000000000', cfg.llmModel),
      'set-model-nonexistent-ws: error');

    const [ok4] = await r.assertNoError(
      () => c.workspaces.setModel(wsId!, cfg.badModel).catch(async (e: any) => {
        if (e?.status === 400 || e?.message) {
          const ws = await c.workspaces.get(wsId!);
          r.assert(ws.phase === 'Active', 'bad-model: ws still Active after bad model',
            ws.phase);
          throw e;
        }
        throw e;
      }),
      'set-bad-model: handled');
    if (!ok4) {
      const ws2 = await c.workspaces.get(wsId!);
      r.assert(ws2.phase === 'Active', 'bad-model-fallback: ws still Active', ws2.phase);
    }

  } finally {
    if (wsId) { try { await c.workspaces.delete(wsId); } catch {} }
  }
}

async function main() {
  const r = new Runner('model-set');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
