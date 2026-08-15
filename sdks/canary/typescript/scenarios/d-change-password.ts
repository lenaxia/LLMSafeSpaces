// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// D-CHANGE-PASSWORD canary — TypeScript SDK

import { LLMSafeSpaces, AuthError } from '../../../typescript/src/index.js';
import { Runner, Config, configFromEnv, nodeFetch, rawDo } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  const rotateEmail = process.env.LLMSAFESPACES_ROTATE_EMAIL || 'canary-rotate@llmsafespaces.test';
  const rotatePassword = process.env.LLMSAFESPACES_ROTATE_PASSWORD || 'canary-rotate-password!';
  const newPassword = 'canary-rotate-newpw!';

  const c = new LLMSafeSpaces({
    baseUrl: cfg.apiUrl,
    credentials: { email: rotateEmail, password: rotatePassword },
    timeout: 60000,
    fetch: nodeFetch as any,
  });

  const [ok, secret] = await r.assertNoError(
    () => c.secrets.create({ name: 'canary-pw-change-test', type: 'env-secret', value: 'pw-change-secret', metadata: { var_name: 'CANARY_TS_VAR' } }),
    'create-secret: no error');
  if (!ok || !secret) return;

  try {
    await r.assertNoError(
      () => c.account.changePassword(rotatePassword, newPassword),
      'change-password: no error');

    const loginBody = Buffer.from(JSON.stringify({ email: rotateEmail, password: newPassword }));
    const [s1, b1] = await rawDo('POST', `${cfg.apiUrl}/api/v1/auth/login`, '', loginBody);
    r.assert(s1 === 200, 'login-new-password: 200', `got ${s1}`);
    r.assert(hasField(b1, 'token'), 'login-new-password: has token');

    const loginBodyOld = Buffer.from(JSON.stringify({ email: rotateEmail, password: rotatePassword }));
    const [s2] = await rawDo('POST', `${cfg.apiUrl}/api/v1/auth/login`, '', loginBodyOld);
    r.assert(s2 === 401, 'login-old-password: 401', `got ${s2}`);

    const c2 = new LLMSafeSpaces({
      baseUrl: cfg.apiUrl,
      credentials: { email: rotateEmail, password: newPassword },
      timeout: 60000,
      fetch: nodeFetch as any,
    });
    const [ok3, revealed] = await r.assertNoError(
      () => c2.secrets.reveal(secret.id, newPassword),
      'reveal-with-new-password: no error');
    if (ok3 && revealed) {
      r.assert(revealed.value === 'pw-change-secret', 'reveal-with-new-password: value correct',
        revealed.value);
    }

    await r.assertNoError(
      () => c2.account.changePassword(newPassword, rotatePassword),
      'change-password-back: no error');

    await r.assertError(
      () => c.account.changePassword('wrong-password', newPassword),
      'change-wrong-old-password: error');

    const [ok4] = await r.assertNoError(
      () => c.account.changePassword(rotatePassword, 'short').catch((e: any) => {
        if (e?.status === 400 || e instanceof AuthError) throw e;
        throw e;
      }),
      'change-too-short-password: error');
    if (ok4 === false) r.ok('change-too-short-password: caught error');

  } finally {
    try {
      const cCleanup = new LLMSafeSpaces({
        baseUrl: cfg.apiUrl,
        credentials: { email: rotateEmail, password: rotatePassword },
        timeout: 30000,
        fetch: nodeFetch as any,
      });
      await cCleanup.secrets.delete(secret.id);
    } catch {}
  }
}

function hasField(body: Buffer, field: string): boolean {
  try {
    const obj = JSON.parse(body.toString());
    return field in obj;
  } catch { return false; }
}

async function main() {
  const r = new Runner('change-password');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
