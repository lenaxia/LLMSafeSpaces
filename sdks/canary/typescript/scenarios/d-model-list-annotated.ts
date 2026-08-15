// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// D-MODEL-LIST-ANNOTATED canary — TypeScript SDK

import { LLMSafeSpaces } from '../../../typescript/src/index.js';
import { Runner, Config, configFromEnv, nodeFetch, waitActive } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  if (!cfg.llmApiKey) { r.ok('model-list-annotated: skipped (no LLM API key)'); return; }
  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: cfg.apiKey, timeout: 60000, fetch: nodeFetch as any });
  let wsId: string | null = null;
  try {
    const [ok, ws] = await r.assertNoError(
      () => c.workspaces.create({ name: 'canary-ts-model-list', runtime: 'base', storageSize: '1Gi' }),
      'create: no error');
    if (!ok || !ws) return;
    wsId = ws.id;

    const phase = await waitActive(c, wsId);
    r.assert(phase === 'Active', 'reach-active', `got "${phase}"`);
    if (phase !== 'Active') return;

    const [ok2, result] = await r.assertNoError(
      () => c.workspaces.getModels(wsId!), 'get-models: no error');
    if (ok2 && result) {
      r.assert(typeof result.currentModel === 'string', 'models: currentModel is string',
        typeof result.currentModel);
      r.assert(Array.isArray(result.models) && result.models.length > 0,
        'models: non-empty array', String(result.models?.length));

      if (Array.isArray(result.models)) {
        for (const m of result.models as any[]) {
          r.assert(typeof m.id === 'string', `model: has id`);
          r.assert(typeof m.name === 'string', `model: has name`);
          r.assert(typeof m.tier === 'string', `model: has tier`);
          r.assert(typeof m.selected === 'boolean', `model: has selected`);
        }

        const selected = (result.models as any[]).filter((m: any) => m.selected);
        r.assert(selected.length === 1, 'models: exactly one selected',
          `found ${selected.length}`);
        if (selected.length === 1) {
          r.assert(selected[0].id === result.currentModel,
            'models: selected id === currentModel',
            `${selected[0].id} vs ${result.currentModel}`);
        }
      }
    }

    await r.assertError(
      () => c.workspaces.getModels('00000000-0000-0000-0000-000000000000'),
      'get-models-nonexistent: error');

  } finally {
    if (wsId) { try { await c.workspaces.delete(wsId); } catch {} }
  }
}

async function main() {
  const r = new Runner('model-list-annotated');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
