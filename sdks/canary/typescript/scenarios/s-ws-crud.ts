// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// S-WS-CRUD canary — TypeScript SDK

import { LLMSafeSpaces, NotFoundError } from '../../../typescript/src/index.js';
import { Runner, Config, configFromEnv, nodeFetch, rawDo, hasField } from '../canary.js';

async function run(run: Runner, cfg: Config): Promise<void> {
  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: cfg.apiKey, timeout: 20000, fetch: nodeFetch as any });
  let wsId: string | null = null;

  try {
    // P1: Create
    const [ok, ws] = await run.assertNoError(
      () => c.workspaces.create({ name: 'canary-ts-crud', runtime: 'base', storageSize: '1Gi' }),
      'create: no error',
    );
    if (!ok || !ws) return;
    wsId = ws.id;
    run.assert(ws.name === 'canary-ts-crud', 'create: name', ws.name);
    run.assert(ws.runtime === 'base', 'create: runtime');
    run.assert(ws.storageSize === '1Gi', 'create: storageSize');

    // P2: Get
    const [ok2, got] = await run.assertNoError(() => c.workspaces.get(wsId!), 'get: no error');
    if (ok2 && got) run.assert(got.id === wsId, 'get: id matches');

    // P3+P4: List + pagination
    const [ok3, list] = await run.assertNoError(() => c.workspaces.list(), 'list: no error');
    if (ok3 && list) {
      run.assert(list.items.some(i => i.id === wsId), 'list: workspace present');
      run.assert(list.pagination !== undefined, 'list: pagination present');
    }
    const [ok4, page] = await run.assertNoError(() => c.workspaces.list(1, 0), 'list-limit1: no error');
    if (ok4 && page) run.assert(page.items.length <= 1, 'list-limit1: ≤1 item', String(page.items.length));

    // P6: Rename
    await run.assertNoError(() => c.workspaces.rename(wsId!, 'canary-ts-renamed'), 'rename: no error');
    const renamed = await c.workspaces.get(wsId!);
    run.assert(renamed.name === 'canary-ts-renamed', 'rename: name updated', renamed.name);

    // P7: Delete
    await run.assertNoError(() => c.workspaces.delete(wsId!), 'delete: no error');
    wsId = null;

  } finally {
    if (wsId) { try { await c.workspaces.delete(wsId); } catch {} }
  }

  // N1: Get nonexistent → NotFoundError
  await run.assertError(() => c.workspaces.get('00000000-0000-0000-0000-000000000000'), 'get-nonexistent: error');

  // N5: Storage too large
  await run.assertError(
    () => c.workspaces.create({ name: 'oversized', runtime: 'base', storageSize: '9999Gi' }),
    'create-oversized-storage: error',
  );

  // N6: Invalid storage format
  await run.assertError(
    () => c.workspaces.create({ name: 'badsize', runtime: 'base', storageSize: 'invalid' }),
    'create-invalid-storage-format: error',
  );
}

async function main() {
  const r = new Runner('ws-crud');
  const cfg = configFromEnv();
  await run(r, cfg);
  r.print();
  process.exit(r.exitCode());
}

main().catch(e => { console.error(e); process.exit(1); });
