// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// S-AUTH canary — TypeScript SDK

import { LLMSafeSpaces, AuthError } from '../../../typescript/src/index.js';
import { Runner, Config, configFromEnv, nodeFetch } from '../canary.js';

async function run(run: Runner, cfg: Config): Promise<void> {
  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: cfg.apiKey, timeout: 20000, fetch: nodeFetch as any });

  // P1: valid API key
  const [ok, me] = await run.assertNoError(() => c.auth.me(), 'valid-key: auth.me no error');
  if (ok && me) {
    run.assert(typeof me.id === 'string' && me.id.length > 0, 'valid-key: user.id present');
    run.assert(typeof me.email === 'string', 'valid-key: user.email present');
    run.assert(typeof me.role === 'string', 'valid-key: user.role present');
    run.assert(me.active === true, 'valid-key: user.active=true');
  }

  // P2+P3: JWT login
  if (cfg.email && cfg.password) {
    const jwtC = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, credentials: { email: cfg.email, password: cfg.password }, timeout: 20000, fetch: nodeFetch as any });
    const [ok2, me2] = await run.assertNoError(() => jwtC.auth.me(), 'jwt-login: auth.me no error');
    if (ok2 && me2) run.assert(me2.active === true, 'jwt-login: user.active=true');
  }

  // N1–N3: bad credentials
  for (const [name, key] of [
    ['invalid-key', 'lsp_invalid_canary_key_000000000000'],
    ['empty-key', ''],
    ['malformed-key', 'not-an-lsp-key'],
  ]) {
    const bad = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: key as string, timeout: 10000, fetch: nodeFetch as any });
    await run.assertError(() => bad.auth.me(), `${name}: AuthError`);
  }
}

async function main() {
  const r = new Runner('auth');
  const cfg = configFromEnv();
  await run(r, cfg);
  const res = r.print();
  process.exit(r.exitCode());
}

main().catch(e => { console.error(e); process.exit(1); });
