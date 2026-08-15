// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// S-EMAIL-RESET canary — tests email endpoints through the real HTTP boundary

import { Runner, Config, configFromEnv, rawDo, containsLeakedInternals } from '../canary.js';

async function run(run: Runner, cfg: Config): Promise<void> {
  const base = cfg.apiUrl.replace(/\/$/, '') + '/api/v1';
  const unique = Date.now();
  const email = `canary-email-${unique}@llmsafespaces.test`;
  const password = 'canary-email-pwd-123456';

  // P1: Register
  const [regStatus] = await rawDo('POST', base + '/auth/register', '', Buffer.from(JSON.stringify({ username: `canaryemail${unique}`, email, password })));
  run.assert(regStatus === 201 || regStatus === 409, `register: 201 or 409 (got ${regStatus})`, '');

  // P2: Login
  // Retry on 429: sibling scenarios' logins share the per-IP login bucket
  // (10/min); this scenario's contract is the 200-vs-403 verification
  // semantics, not rate limiting (Python-twin parity).
  let loginStatus = 0;
  for (let attempt = 0; attempt < 7; attempt++) {
    [loginStatus] = await rawDo('POST', base + '/auth/login', '', Buffer.from(JSON.stringify({ email, password })));
    if (loginStatus !== 429) break;
    await new Promise((res) => setTimeout(res, 10_000));
  }
  if (loginStatus === 200) {
    run.ok('login: 200 (noop mode — auto-verified)');
  } else if (loginStatus === 403) {
    run.ok('login: 403 (SES mode — unverified)');
  } else {
    run.fail('login: unexpected status', `got ${loginStatus}`);
  }

  // P3: Password-reset request → 202
  const [resetStatus] = await rawDo('POST', base + '/auth/password-reset/request', '', Buffer.from(JSON.stringify({ email })));
  run.assert(resetStatus === 202, `password-reset-request: 202 (got ${resetStatus})`, '');

  // P4: Password-reset request unknown → 202
  const [unknownStatus] = await rawDo('POST', base + '/auth/password-reset/request', '', Buffer.from(JSON.stringify({ email: 'nonexistent-canary@llmsafespaces.test' })));
  run.assert(unknownStatus === 202, `password-reset-request-unknown: 202 (got ${unknownStatus})`, '');

  // P5: Password-reset confirm bogus → 404
  const [confirmStatus] = await rawDo('POST', base + '/auth/password-reset/confirm', '', Buffer.from(JSON.stringify({ token: 'canary-bogus', newPassword: 'canary-new-pwd-123456' })));
  run.assert(confirmStatus === 404, `password-reset-confirm-bogus: 404 (got ${confirmStatus})`, '');

  // P6: Verify-email bogus → 404
  const [verifyStatus] = await rawDo('POST', base + '/auth/verify-email', '', Buffer.from(JSON.stringify({ token: 'canary-bogus' })));
  run.assert(verifyStatus === 404, `verify-email-bogus: 404 (got ${verifyStatus})`, '');

  // P7: Verify-email resend → 202
  const [resendStatus] = await rawDo('POST', base + '/auth/verify-email/resend', '', Buffer.from(JSON.stringify({ email })));
  run.assert(resendStatus === 202, `verify-email-resend: 202 (got ${resendStatus})`, '');

  // P8: Resend unknown → 202
  const [resendUnknownStatus] = await rawDo('POST', base + '/auth/verify-email/resend', '', Buffer.from(JSON.stringify({ email: 'ghost-canary@nonexistent.invalid' })));
  run.assert(resendUnknownStatus === 202, `verify-email-resend-unknown: 202 (got ${resendUnknownStatus})`, '');

  // P9: No leaked internals in error responses
  const [, leakResp] = await rawDo('POST', base + '/auth/password-reset/confirm', '', Buffer.from(JSON.stringify({ token: 'x', newPassword: 'canary-valid-pwd' })));
  run.assert(!containsLeakedInternals(leakResp.toString()), 'password-reset-confirm: no leaked internals', '');
}

async function main() {
  const r = new Runner('email-reset', 'typescript-sdk');
  const cfg = configFromEnv();
  await run(r, cfg);
  const res = r.print();
  if (res.failed > 0) process.exit(1);
}

main().catch((e) => { console.error(e); process.exit(1); });
