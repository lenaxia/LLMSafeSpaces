// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// S-APIKEY canary — TypeScript SDK

import { LLMSafeSpaces } from '../../../typescript/src/index.js';
import { Runner, Config, configFromEnv, nodeFetch } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: cfg.apiKey, timeout: 20000, fetch: nodeFetch as any });
  let keyId: string | null = null;
  try {
    const [ok, key] = await r.assertNoError(() => c.auth.createApiKey('canary-ts-key'), 'create-key: no error');
    if (!ok || !key) return;
    r.assert(key.name === 'canary-ts-key', 'create-key: name');
    r.assert(key.key?.startsWith('lsp_') ?? false, 'create-key: starts with lsp_', key.key);
    r.assert(key.active === true, 'create-key: active=true');
    keyId = key.id;

    // List — key present, full key absent
    const [ok2, keys] = await r.assertNoError(() => c.auth.listApiKeys(), 'list-keys: no error');
    if (ok2 && keys) {
      const found = (keys as any[]).find((k: any) => k.id === keyId);
      r.assert(!!found, 'list-keys: key present');
      if (found) r.assert(!found.key, 'list-keys: full key absent');
    }

    // New key authenticates
    const nc = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: key.key, timeout: 10000, fetch: nodeFetch as any });
    await r.assertNoError(() => nc.auth.me(), 'new-key: authenticates');

    // Delete
    await r.assertNoError(() => c.auth.deleteApiKey(keyId!), 'delete-key: no error');
    keyId = null;

    // Absent after delete
    const [ok3, keys2] = await r.assertNoError(() => c.auth.listApiKeys(), 'list-after-delete: no error');
    if (ok3 && keys2) r.assert(!(keys2 as any[]).some((k: any) => k.id === key.id), 'list-after-delete: absent');

    // Deleted key rejected
    await r.assertError(() => nc.auth.me(), 'deleted-key: AuthError');

  } finally {
    if (keyId) { try { await c.auth.deleteApiKey(keyId); } catch {} }
  }
  await r.assertError(() => c.auth.deleteApiKey('00000000-0000-0000-0000-000000000099'), 'delete-nonexistent: error');
}

async function main() {
  const r = new Runner('apikey');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
