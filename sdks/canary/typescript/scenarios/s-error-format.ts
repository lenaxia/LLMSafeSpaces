// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// S-ERROR-FORMAT canary — TypeScript SDK

import { LLMSafeSpaces } from '../../../typescript/src/index.js';
import { Runner, Config, configFromEnv, nodeFetch, rawDo, hasErrorField, hasField, containsLeakedInternals } from '../canary.js';

async function run(run: Runner, cfg: Config): Promise<void> {
  const base = `${cfg.apiUrl}/api/v1`;

  // P1: 401 no auth
  const [s1, b1] = await rawDo('GET', `${base}/auth/me`, '');
  run.assert(s1 === 401, '401-no-auth: status', `got ${s1}`);
  run.assert(hasErrorField(b1), '401-no-auth: error field');
  assertErrorIsString(run, b1, '401-no-auth: error is string');

  // P2: 404 nonexistent workspace
  const [s2, b2] = await rawDo('GET', `${base}/workspaces/00000000-0000-0000-0000-000000000000`, cfg.apiKey);
  run.assert(s2 === 404, '404-nonexistent: status', `got ${s2}`);
  run.assert(hasErrorField(b2), '404-nonexistent: error field');

  // P3: 400 empty register
  const [s3, b3] = await rawDo('POST', `${base}/auth/register`, '', Buffer.from('{}'));
  run.assert(s3 === 400, '400-empty-register: status', `got ${s3}`);
  run.assert(hasErrorField(b3), '400-empty-register: error field');

  // P4: 400 rename with missing name
  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: cfg.apiKey, timeout: 15000, fetch: nodeFetch as any });
  let wsId: string | null = null;
  try {
    const ws = await c.workspaces.create({ name: 'canary-ts-errfmt', runtime: 'base', storageSize: '1Gi' });
    wsId = ws.id;
    const [s4, b4] = await rawDo('PUT', `${base}/workspaces/${wsId}`, cfg.apiKey, Buffer.from('{}'));
    run.assert(s4 === 400, '400-rename-empty: status', `got ${s4}`);
    run.assert(hasErrorField(b4), '400-rename-empty: error field');
    assertErrorIsString(run, b4, '400-rename-empty: error is string');

    // P7: proxy 503 workspace-not-ready
    const [s7, b7] = await rawDo('POST',
      `${base}/workspaces/${wsId}/sessions/canary-session-id/message`,
      cfg.apiKey,
      Buffer.from('{"content":"ping","parts":[{"type":"text","text":"ping"}]}'));
    if (s7 === 503) {
      run.assert(hasField(b7, 'phase'), '503-not-ready: phase field');
      run.assert(hasField(b7, 'retryAfter'), '503-not-ready: retryAfter field');
      run.assert(hasErrorField(b7), '503-not-ready: error field');
    } else {
      run.assert(s7 >= 400, 'proxy-error: 4xx/5xx', `got ${s7}`);
    }
  } finally {
    if (wsId) { try { await c.workspaces.delete(wsId); } catch {} }
  }

  // P8: path traversal blocked
  const [s8] = await rawDo('GET', `${base}/workspaces/test-ws/sessions/..%2F..%2Fetc%2Fpasswd/message`, cfg.apiKey);
  run.assert(s8 === 400 || s8 === 404, 'path-traversal: 400 or 404', `got ${s8}`);

  // P5+P6: No leaked internals
  for (const body of [b1, b2, b3]) {
    run.assert(!containsLeakedInternals(body), 'no-leaked-internals');
  }

  // P9: Success has no error field
  const [s9, b9] = await rawDo('GET', `${cfg.apiUrl}/livez`, '');
  run.assert(s9 === 200, 'success-no-error: livez 200');
  run.assert(!hasField(b9, 'error'), 'success-no-error: no error field');
}

function assertErrorIsString(run: Runner, body: Buffer, label: string): void {
  try {
    const obj = JSON.parse(body.toString());
    run.assert(typeof obj?.error === 'string', label, `error field type: ${typeof obj?.error}`);
  } catch {
    run.fail(label, 'not valid JSON');
  }
}

async function main() {
  const r = new Runner('error-format');
  const cfg = configFromEnv();
  await run(r, cfg);
  r.print();
  process.exit(r.exitCode());
}

main().catch(e => { console.error(e); process.exit(1); });
