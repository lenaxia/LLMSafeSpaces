// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// S-LOGOUT canary — TypeScript SDK

import { LLMSafeSpaces } from '../../../typescript/src/index.js';
import { Runner, Config, configFromEnv, nodeFetch, rawDo } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  if (!cfg.email || !cfg.password) { r.ok('logout: skipped (no email/password)'); return; }

  // P1: Login → JWT
  const [s, b] = await rawDo('POST', cfg.apiUrl + '/api/v1/auth/login', '',
    Buffer.from(JSON.stringify({ email: cfg.email, password: cfg.password })));
  if (!r.assert(s === 200, `login: 200 (got ${s})`, b.toString().substring(0, 200))) return;
  const token = JSON.parse(b.toString()).token as string;
  r.assert(token !== '', 'login: token non-empty');

  // P2: JWT works pre-logout
  const jwtC = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: token, timeout: 10000, fetch: nodeFetch as any });
  await r.assertNoError(() => jwtC.auth.me(), 'pre-logout: auth.me succeeds');

  // P3: Logout
  const [s2] = await rawDo('POST', cfg.apiUrl + '/api/v1/auth/logout', token, Buffer.from(''));
  r.assert(s2 === 204, `logout: 204 (got ${s2})`);

  // P4: Same JWT rejected
  await r.assertError(() => jwtC.auth.me(), 'post-logout: JWT rejected');

  // P5: Idempotent
  const [s3] = await rawDo('POST', cfg.apiUrl + '/api/v1/auth/logout', token, Buffer.from(''));
  r.assert(s3 === 204, 'logout-idempotent: 204');

  // N1+N2: API key still valid
  const kc = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: cfg.apiKey, timeout: 10000, fetch: nodeFetch as any });
  await r.assertNoError(() => kc.auth.me(), 'api-key: still valid');
}

async function main() {
  const r = new Runner('logout');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
