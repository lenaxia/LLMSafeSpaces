// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// S-SECRET-AUDIT canary — TypeScript SDK

import { LLMSafeSpaces } from '../../../typescript/src/index.js';
import { jwtLogin, Runner, Config, configFromEnv, nodeFetch, rawDo } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: await jwtLogin(cfg), timeout: 15000, fetch: nodeFetch as any });
  let sid: string | null = null;
  try {
    const [ok, s] = await r.assertNoError(
      () => c.secrets.create({ name: 'canary-ts-audit', type: 'env-secret', value: 'v', metadata: { var_name: 'CANARY_TS_VAR' } }),
      'create-for-audit: no error');
    if (ok && s) sid = s.id;

    const [ok2, log] = await r.assertNoError(() => c.secrets.getAuditLog(), 'get-audit: no error');
    if (ok2 && log) {
      r.assert(Array.isArray(log.entries), 'get-audit: entries array');
      if (sid) {
        const found = (log.entries as any[]).find((e: any) => e.secretId === sid);
        r.assert(!!found, 'get-audit: entry for created secret present');
        if (found) {
          r.assert(typeof found.action === 'string' && found.action !== '', 'audit-entry: action field');
          r.assert(typeof found.userId === 'string' && found.userId !== '', 'audit-entry: userId field');
        }
      }
    }
  } finally {
    if (sid) { try { await c.secrets.delete(sid); } catch {} }
  }

  const [s] = await rawDo('GET', cfg.apiUrl + '/api/v1/secrets/audit');
  r.assert(s === 401, `audit-no-auth: 401 (got ${s})`);
}

async function main() {
  const r = new Runner('secret-audit');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
