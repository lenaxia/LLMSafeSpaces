// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// S-SECRET-CRUD canary — TypeScript SDK

import { LLMSafeSpaces } from '../../src/index.js';
import { Runner, Config, configFromEnv, nodeFetch, rawDo, hasField } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: cfg.apiKey, timeout: 20000, fetch: nodeFetch as any });
  let secretId: string | null = null;

  try {
    // P1: Create
    const [ok, s] = await r.assertNoError(
      () => c.secrets.create({ name: 'canary-ts-secret', type: 'env-secret', value: 'canary-val', metadata: { var_name: 'CANARY_TS_VAR' } }),
      'create: no error');
    if (!ok || !s) return;
    r.assert(s.id !== '', 'create: id non-empty');
    r.assert(s.name === 'canary-ts-secret', 'create: name');
    r.assert(s.type === 'env-secret', 'create: type');
    secretId = s.id;

    // P2: List
    const [ok2, list] = await r.assertNoError(() => c.secrets.list(), 'list: no error');
    if (ok2 && list) r.assert((list as any[]).some((x: any) => x.id === secretId), 'list: secret present');

    // P3: Get (no value field)
    const [ok3, got] = await r.assertNoError(() => c.secrets.get(secretId!), 'get: no error');
    if (ok3 && got) {
      r.assert(got.name === 'canary-ts-secret', 'get: name');
      const [, b] = await rawDo('GET', `${cfg.apiUrl}/api/v1/secrets/${secretId}`, cfg.apiKey);
      r.assert(!hasField(b, 'value'), 'get-raw: no value field');
    }

    // P4: Update
    await r.assertNoError(() => c.secrets.update(secretId!, 'updated-val'), 'update: no error');

    // P5: Delete
    await r.assertNoError(() => c.secrets.delete(secretId!), 'delete: no error');
    secretId = null;

    // P6: Re-create with same name → succeeds
    const [ok4, s2] = await r.assertNoError(
      () => c.secrets.create({ name: 'canary-ts-secret', type: 'env-secret', value: 'v2', metadata: { var_name: 'CANARY_TS_VAR' } }),
      're-create-after-delete: no error');
    if (ok4 && s2) { await c.secrets.delete(s2.id).catch(() => {}); }

  } finally {
    if (secretId) { try { await c.secrets.delete(secretId); } catch {} }
  }

  // N1: Get nonexistent
  await r.assertError(() => c.secrets.get('00000000-0000-0000-0000-000000000099'), 'get-nonexistent: error');

  // N4: Duplicate name
  let dup1Id: string | null = null;
  try {
    const dup = await c.secrets.create({ name: 'canary-ts-dup', type: 'env-secret', value: 'v1', metadata: { var_name: 'CANARY_TS_VAR' } });
    dup1Id = dup.id;
    await r.assertError(
      () => c.secrets.create({ name: 'canary-ts-dup', type: 'env-secret', value: 'v2', metadata: { var_name: 'CANARY_TS_VAR' } }),
      'create-duplicate: ConflictError');
  } finally {
    if (dup1Id) { try { await c.secrets.delete(dup1Id); } catch {} }
  }
}

async function main() {
  const r = new Runner('secret-crud');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
