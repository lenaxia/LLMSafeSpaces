// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// S-CRED-CRUD canary — TypeScript SDK

import { LLMSafeSpaces } from '../../../typescript/src/index.js';
import { jwtLogin, Runner, Config, configFromEnv, nodeFetch } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: await jwtLogin(cfg), timeout: 20000, fetch: nodeFetch as any });
  const credValue = JSON.stringify({ kind: cfg.llmProvider, slug: "canary-ts-cred", apiKey: 'sk-canary-placeholder-00000000' });
  let credId: string | null = null;
  try {
    const [ok, cred] = await r.assertNoError(
      () => c.secrets.create({ name: 'canary-ts-llm-cred', type: 'llm-provider', value: credValue }),
      'create-cred: no error');
    if (!ok || !cred) return;
    r.assert(cred.type === 'llm-provider', 'create-cred: type=llm-provider', cred.type);
    credId = cred.id;

    const [ok2, list] = await r.assertNoError(() => c.secrets.list(), 'list-creds: no error');
    if (ok2 && list) r.assert((list as any[]).some((s: any) => s.id === credId), 'list-creds: present');

    await r.assertNoError(() => c.secrets.delete(credId!), 'delete-cred: no error');
    credId = null;

    const [ok3, list2] = await r.assertNoError(() => c.secrets.list(), 'list-after-delete: no error');
    if (ok3 && list2) r.assert(!(list2 as any[]).some((s: any) => s.id === cred.id), 'list-after-delete: absent');

  } finally {
    if (credId) { try { await c.secrets.delete(credId); } catch {} }
  }

  await r.assertError(() => c.secrets.delete('00000000-0000-0000-0000-000000000097'), 'delete-nonexistent: error');
  await r.assertError(
    () => c.secrets.create({ name: 'canary-ts-bad-cred', type: 'llm-provider', value: 'not-valid-json' }),
    'create-malformed-cred: error');
}

async function main() {
  const r = new Runner('cred-crud');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
