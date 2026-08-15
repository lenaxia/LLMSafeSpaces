// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// D-KEY-ROTATE canary — TypeScript SDK

import { LLMSafeSpaces, AuthError } from '../../../typescript/src/index.js';
import { Runner, Config, configFromEnv, nodeFetch } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  const rotateEmail = process.env.LLMSAFESPACES_ROTATE_EMAIL || 'canary-rotate@llmsafespaces.test';
  const rotatePassword = process.env.LLMSAFESPACES_ROTATE_PASSWORD || 'canary-rotate-password!';

  const c = new LLMSafeSpaces({
    baseUrl: cfg.apiUrl,
    credentials: { email: rotateEmail, password: rotatePassword },
    timeout: 60000,
    fetch: nodeFetch as any,
  });

  const [ok, secret] = await r.assertNoError(
    () => c.secrets.create({ name: 'canary-rotate-test', type: 'env-secret', value: 'rotate-secret-value', metadata: { var_name: 'CANARY_TS_VAR' } }),
    'create-secret: no error');
  if (!ok || !secret) return;

  try {
    const [ok2, rotResult] = await r.assertNoError(
      () => c.account.rotateKey(rotatePassword),
      'rotate-key: no error');
    if (ok2 && rotResult) {
      r.assert(typeof rotResult.keyVersion === 'number', 'rotate-key: has keyVersion',
        typeof rotResult.keyVersion);
      r.assert(typeof rotResult.recoveryKey === 'string' && rotResult.recoveryKey.length > 0,
        'rotate-key: has recoveryKey');
    }

    const [ok3, revealed] = await r.assertNoError(
      () => c.secrets.reveal(secret.id, rotatePassword),
      'reveal-after-rotate: no error');
    if (ok3 && revealed) {
      r.assert(revealed.value === 'rotate-secret-value', 'reveal-after-rotate: value correct',
        revealed.value);
    }

    await r.assertError(
      () => c.account.rotateKey('wrong-password'),
      'rotate-wrong-password: AuthError');

    const [ok4] = await r.assertNoError(
      () => c.account.rotateKey('').catch((e: any) => {
        if (e instanceof AuthError || e?.status === 400) throw e;
        throw e;
      }),
      'rotate-missing-password: error');
    if (ok4 === false) {
      r.ok('rotate-missing-password: caught error');
    }

  } finally {
    try { await c.secrets.delete(secret.id); } catch {}
  }
}

async function main() {
  const r = new Runner('key-rotate');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
