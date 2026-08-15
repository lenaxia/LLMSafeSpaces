// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// S-RATE-LIMIT canary — TypeScript SDK

import { Runner, Config, configFromEnv, rawDo, hasErrorField } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  const loginUrl = `${cfg.apiUrl}/api/v1/auth/login`;
  const body = Buffer.from(JSON.stringify({ email: 'rate-limit-test@llmsafespaces.test', password: 'wrong-password' }));

  const [s0] = await rawDo('POST', loginUrl, '', body);
  r.assert(s0 === 401, 'first-login: 401 (not 429)', `got ${s0}`);

  let got429 = false;
  for (let i = 0; i < 8; i++) {
    const [s, b] = await rawDo('POST', loginUrl, '', body);
    if (s === 429) {
      got429 = true;
      r.assert(hasErrorField(b), '429: error field is a string');
      break;
    }
  }
  r.assert(got429, 'rate-limit: got 429 within burst', 'no 429 after 8 rapid attempts');

  for (const path of ['/readyz', '/livez']) {
    let blocked = false;
    for (let i = 0; i < 10; i++) {
      const [s] = await rawDo('GET', cfg.apiUrl + path);
      if (s === 429) { blocked = true; break; }
    }
    r.assert(!blocked, `${path}: not rate-limited`);
  }
}

async function main() {
  const r = new Runner('rate-limit');
  const cfg = configFromEnv();
  await run(r, cfg);
  r.print();
  process.exit(r.exitCode());
}

main().catch(e => { console.error(e); process.exit(1); });
