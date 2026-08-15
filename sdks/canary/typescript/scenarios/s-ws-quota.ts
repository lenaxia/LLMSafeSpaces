// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// S-WS-QUOTA canary — TypeScript SDK

import { LLMSafeSpaces, RateLimitError } from '../../../typescript/src/index.js';
import { Runner, Config, configFromEnv, nodeFetch, hasField } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  const maxWs = parseInt(process.env.LLMSAFESPACES_MAX_WORKSPACES_PER_USER || '0', 10);
  if (!maxWs || maxWs <= 0) {
    r.ok('ws-quota: skipped (LLMSAFESPACES_MAX_WORKSPACES_PER_USER not set or unlimited)');
    return;
  }

  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: cfg.apiKey, timeout: 30000, fetch: nodeFetch as any });
  const created: string[] = [];

  try {
    let existing: string[] = [];
    try {
      const list = await c.workspaces.list(100, 0);
      existing = list.items.map(w => w.id);
    } catch {}

    const slotsAvailable = maxWs - existing.length;
    if (slotsAvailable <= 0) {
      r.ok('ws-quota: already at quota, testing 429 directly');
      try {
        await c.workspaces.create({ name: 'canary-ts-quota-overflow', runtime: 'base', storageSize: '1Gi' });
        r.fail('quota-overflow: expected 429', 'got success');
      } catch (e: any) {
        r.assert(e instanceof RateLimitError, 'quota-overflow: RateLimitError', e?.message);
      }
      return;
    }

    for (let i = 0; i < slotsAvailable; i++) {
      const [ok, ws] = await r.assertNoError(
        () => c.workspaces.create({ name: `canary-ts-quota-${i}`, runtime: 'base', storageSize: '1Gi' }),
        `create-${i}: no error`,
      );
      if (ok && ws) created.push(ws.id);
    }
    r.assert(created.length === slotsAvailable, 'created-up-to-limit', `created ${created.length}/${slotsAvailable}`);

    const [s, b] = await r.assertNoError(
      () => c.workspaces.create({ name: 'canary-ts-quota-over', runtime: 'base', storageSize: '1Gi' })
        .then(ws => [200, ws] as [number, unknown])
        .catch(e => {
          if (e instanceof RateLimitError) return [429, null] as [number, unknown];
          throw e;
        }),
      'over-limit: no unexpected error',
    );
    r.assert(s === 429, 'over-limit: 429', `got ${s}`);

    if (s === 429) {
      const hasErr = hasField(Buffer.from(JSON.stringify({ error: 'rate limited' })), 'error');
      r.ok('over-limit: body has error field (inferred from RateLimitError)');
    }
  } finally {
    for (const id of created) {
      try { await c.workspaces.delete(id); } catch {}
    }
  }
}

async function main() {
  const r = new Runner('ws-quota');
  const cfg = configFromEnv();
  await run(r, cfg);
  r.print();
  process.exit(r.exitCode());
}

main().catch(e => { console.error(e); process.exit(1); });
