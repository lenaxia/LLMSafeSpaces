// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// S-SECRET-REVEAL canary — TypeScript SDK

import { LLMSafeSpaces } from '../../../typescript/src/index.js';
import { jwtLogin, Runner, Config, configFromEnv, nodeFetch, rawDo, hasField } from '../canary.js';

const SECRET_VALUE = 'canary-ts-reveal-test-val-xyz';

async function run(r: Runner, cfg: Config): Promise<void> {
  if (!cfg.password) { r.ok('reveal: skipped (no password)'); return; }
  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: await jwtLogin(cfg), timeout: 15000, fetch: nodeFetch as any });
  let sid: string | null = null;
  try {
    const [ok, s] = await r.assertNoError(
      () => c.secrets.create({ name: 'canary-ts-reveal', type: 'env-secret', value: SECRET_VALUE, metadata: { var_name: 'CANARY_TS_VAR' } }),
      'create: no error');
    if (!ok || !s) return;
    sid = s.id;

    const [ok2, result] = await r.assertNoError(() => c.secrets.reveal(sid!, cfg.password), 'reveal-correct: no error');
    if (ok2 && result) r.assert(result.value === SECRET_VALUE, 'reveal: value matches', result.value);

    const [, b] = await rawDo('GET', `${cfg.apiUrl}/api/v1/secrets/${sid}`, cfg.apiKey);
    r.assert(!hasField(b, 'value'), 'get: no value field');

    const [s1] = await rawDo('POST', `${cfg.apiUrl}/api/v1/secrets/${sid}/reveal`, cfg.apiKey, Buffer.from('{}'));
    r.assert(s1 === 400, `reveal-no-password: 400 (got ${s1})`);

    const [s2] = await rawDo('POST', `${cfg.apiUrl}/api/v1/secrets/${sid}/reveal`, cfg.apiKey,
      Buffer.from(JSON.stringify({ password: 'definitely-wrong-xyz' })));
    r.assert(s2 === 403, `reveal-wrong-password: 403 (got ${s2})`);

  } finally {
    if (sid) { try { await c.secrets.delete(sid); } catch {} }
  }
}

async function main() {
  const r = new Runner('secret-reveal');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
